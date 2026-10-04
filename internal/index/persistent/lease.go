package persistent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// GenerationLease pins one immutable generation's files until Close.
// Snapshot returns a copy of the pinned generation metadata. Keep the lease
// open until all reads through that snapshot have finished.
type GenerationLease struct {
	view  GenerationView
	lock  *writerLock
	store *FileStore
	once  sync.Once
}

// ReindexLease serializes whole rebuild attempts that target the same store.
// It is a separate OS lock from writer.lock, so extraction can run without
// holding the short-lived manifest/segment mutation lock.
type ReindexLease struct {
	lock *writerLock
	once sync.Once
}

func (l *ReindexLease) Close() error {
	if l != nil {
		l.once.Do(func() { l.lock.release() })
	}
	return nil
}

// AcquireReindexLease prevents duplicate rebuilds from different server
// processes sharing this store. The OS releases the lock if its owner exits.
func (s *FileStore) AcquireReindexLease(ctx context.Context) (*ReindexLease, error) {
	lock, err := acquireFileLock(ctx, filepath.Join(s.root, "reindex.lock"), "reindex")
	if err != nil {
		return nil, err
	}
	return &ReindexLease{lock: lock}, nil
}

func (l *GenerationLease) Snapshot() GenerationView {
	if l == nil {
		return GenerationView{}
	}
	view := l.view
	view.Segments = append([]SegmentRef(nil), l.view.Segments...)
	return view
}

func (l *GenerationLease) Close() error {
	if l != nil {
		var closed bool
		l.once.Do(func() {
			if l.lock != nil {
				l.lock.release()
			}
			closed = true
		})
		if closed && l.store != nil {
			l.store.removeIfRetired(l.view.Generation)
		}
	}
	return nil
}

func generationLockPath(root string, id uint64) string {
	return filepath.Join(root, dirLeases, fmt.Sprintf("generation-%d.lock", id))
}

func acquireGenerationLock(ctx context.Context, root string, id uint64, exclusive, wait bool) (*writerLock, error) {
	f, err := os.OpenFile(generationLockPath(root, id), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("persistent: open generation %d lease: %w", id, err)
	}
	for {
		unlock, lockErr := tryGenerationLock(f, exclusive)
		if lockErr == nil {
			return &writerLock{file: f, unlock: unlock}, nil
		}
		if !errorsIsLockBusy(lockErr) {
			_ = f.Close()
			return nil, fmt.Errorf("persistent: lock generation %d: %w", id, lockErr)
		}
		if !wait {
			_ = f.Close()
			return nil, errWriterLockBusy
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func errorsIsLockBusy(err error) bool { return err == errWriterLockBusy }

// removeGenerationSegments deletes files only when no reader in any process
// holds a shared generation lease. A busy generation is left for later
// garbage collection.
func (s *FileStore) removeGenerationSegments(gen *Generation) error {
	if gen == nil {
		return nil
	}
	lock, err := acquireGenerationLock(context.Background(), s.root, gen.ID, true, false)
	if err != nil {
		if errorsIsLockBusy(err) {
			return nil
		}
		return err
	}
	defer lock.release()
	for _, seg := range gen.Segments {
		if err := os.Remove(filepath.Join(s.root, segFileName(seg.ID))); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("persistent: retire generation %d segment %s: %w", gen.ID, seg.ID, err)
		}
	}
	return nil
}

func (s *FileStore) removeIfRetired(gen Generation) {
	writerLock, err := tryAcquireWriterLock(s.root)
	if err != nil {
		return
	}
	defer writerLock.release()

	s.mu.Lock()
	if err := s.recover(); err != nil {
		s.mu.Unlock()
		return
	}
	referenced := s.current != nil && s.current.ID == gen.ID
	for _, history := range s.history {
		if history.ID == gen.ID {
			referenced = true
			break
		}
	}
	s.mu.Unlock()
	if !referenced {
		_ = s.removeGenerationSegments(&gen)
	}
}
