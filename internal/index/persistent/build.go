package persistent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sync"
)

// fileBuild stages the next generation; nothing it writes is discoverable
// through the manifest until Commit's atomic pointer swap (TXN-002).
type fileBuild struct {
	mu       sync.Mutex
	store    *FileStore
	genID    uint64
	stageDir string
	segments []SegmentRef
	state    buildState
	result   error
}

type buildState uint8

const (
	buildOpen buildState = iota
	buildCommitted
	buildAborted
)

// WriteSegment encodes, fsyncs and records one immutable segment. Budget is
// enforced before touching disk (§L17).
func (b *fileBuild) WriteSegment(payload []byte) (SegmentID, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != buildOpen {
		return "", b.closedError()
	}
	writerLock, err := acquireWriterLock(context.Background(), b.store.root)
	if err != nil {
		return "", err
	}
	defer writerLock.release()
	b.store.mu.Lock()
	budget := b.store.cfg.DiskBudgetBytes
	b.store.mu.Unlock()

	var used int64
	_ = filepath.Walk(b.store.root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			used += fi.Size()
		}
		return nil
	})
	if budget > 0 && used+int64(len(payload))+int64(segHeaderSz)+16 > budget {
		return "", ErrDiskBudgetExceeded
	}

	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	id := SegmentID(fmt.Sprintf("%d-%x", b.genID, rnd))

	raw := encodeSegment(b.genID, payload)
	dst := filepath.Join(b.stageDir, segFileName(id))
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("persistent: create %s: %w", id, err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return "", fmt.Errorf("persistent: write %s: %w", id, err)
	}
	if err := f.Sync(); err != nil { // L4: durable before discoverable
		f.Close()
		return "", fmt.Errorf("persistent: fsync %s: %w", id, err)
	}
	if err := f.Close(); err != nil {
		return "", err
	}

	b.segments = append(b.segments, SegmentRef{ID: id, Len: int64(len(raw)), CRC: crc32.ChecksumIEEE(payload)})
	return id, nil
}

func (b *fileBuild) GenerationID() uint64 { return b.genID }

// Commit publishes the staged generation: move segments into the store root,
// write manifest.tmp, persist recovery history, then atomically replace the
// manifest (L4). The build mutex makes cancellation/commit/abort terminal.
// Cancellation observed before the final context check aborts; a successful
// manifest replacement wins if cancellation happens after that check.
func (b *fileBuild) Commit(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case buildCommitted:
		return nil
	case buildAborted:
		if b.result != nil {
			return b.result
		}
		return ErrBuildAborted
	}
	writerLock, err := acquireWriterLock(ctx, b.store.root)
	if err != nil {
		return b.finishAbort(err)
	}
	defer writerLock.release()
	if err := b.commitWithWriterLock(ctx); err != nil {
		return b.finishAbort(err)
	}
	b.state = buildCommitted
	b.result = nil
	return nil
}

func (b *fileBuild) commitWithWriterLock(ctx context.Context) (retErr error) {
	b.store.mu.Lock()
	if err := b.store.recover(); err != nil {
		b.store.mu.Unlock()
		return err
	}
	var previous *Generation
	if b.store.current != nil {
		copy := *b.store.current
		copy.Segments = append([]SegmentRef(nil), copy.Segments...)
		previous = &copy
	}
	oldHistory := append([]*Generation(nil), b.store.history...)
	b.store.mu.Unlock()
	rollbackHistory := append([]*Generation(nil), oldHistory...)
	if previous != nil {
		rollbackHistory = append([]*Generation{previous}, rollbackHistory...)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if previous != nil && b.genID <= previous.ID {
		return fmt.Errorf("persistent: generation %d superseded by committed generation %d", b.genID, previous.ID)
	}
	if len(b.segments) == 0 {
		return fmt.Errorf("persistent: cannot commit generation %d without segments", b.genID)
	}

	gen := Generation{ID: b.genID, Segments: b.segments}
	promoted := make([]SegmentID, 0, len(b.segments))
	manifestTmpTouched := false
	published := false
	historyUpdated := false
	defer func() {
		if published {
			return
		}
		var cleanupErrs []error
		if historyUpdated {
			if err := writeHistoryAtomic(b.store.historyPath(), rollbackHistory); err != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("restore recovery history: %w", err))
			}
		}
		for _, id := range promoted {
			if err := os.Remove(filepath.Join(b.store.root, segFileName(id))); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("remove uncommitted segment %s: %w", id, err))
			}
		}
		if manifestTmpTouched {
			if err := os.Remove(b.store.tmpManifestPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("remove uncommitted manifest: %w", err))
			}
		}
		if err := os.RemoveAll(b.stageDir); err != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("remove staging directory: %w", err))
		}
		if err := errors.Join(cleanupErrs...); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("persistent: rollback uncommitted generation %d: %w", b.genID, err))
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}

	// Stage → root segment placement.
	for _, seg := range b.segments {
		if err := ctx.Err(); err != nil {
			return err
		}
		src := filepath.Join(b.stageDir, segFileName(seg.ID))
		dst := filepath.Join(b.store.root, segFileName(seg.ID))
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("persistent: promote %s: %w", seg.ID, err)
		}
		promoted = append(promoted, seg.ID)
	}
	if err := os.RemoveAll(b.stageDir); err != nil {
		return fmt.Errorf("persistent: remove staging directory: %w", err)
	}

	mb, err := json.MarshalIndent(gen, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.store.tmpManifestPath()
	manifestTmpTouched = true
	if err := writeFileSync(tmp, mb); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Save the current generation in recovery history before atomically
	// replacing the manifest, so recovery retains a fallback after publication.
	nextHistory := append([]*Generation(nil), b.store.history...)
	if b.store.current != nil {
		nextHistory = append([]*Generation{b.store.current}, nextHistory...)
	}
	var retired []*Generation
	if len(nextHistory) > b.store.cfg.KeepGenerations {
		retired = nextHistory[b.store.cfg.KeepGenerations:]
		nextHistory = nextHistory[:b.store.cfg.KeepGenerations]
	}
	if err := syncHistory(b.store.historyPath(), nextHistory); err != nil {
		return fmt.Errorf("persistent: stage recovery history: %w", err)
	}
	historyUpdated = true
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := replaceFile(tmp, b.store.manifestPath()); err != nil {
		return fmt.Errorf("persistent: publish manifest: %w", err)
	}
	published = true

	b.store.mu.Lock()
	b.store.history = nextHistory
	b.store.current = &gen
	b.store.mu.Unlock()
	for _, old := range retired {
		_ = b.store.removeGenerationSegments(old)
	}

	return retErr
}

// Abort discards staging; half-written data stays undiscoverable (TXN-002).
func (b *fileBuild) Abort() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == buildCommitted {
		return ErrBuildCommitted
	}
	if b.state == buildAborted {
		return nil
	}
	b.state = buildAborted
	b.result = ErrBuildAborted
	if err := os.RemoveAll(b.stageDir); err != nil {
		b.result = errors.Join(b.result, err)
		return err
	}
	return nil
}

func (b *fileBuild) finishAbort(err error) error {
	b.state = buildAborted
	b.result = err
	if cleanupErr := os.RemoveAll(b.stageDir); cleanupErr != nil {
		b.result = errors.Join(b.result, fmt.Errorf("persistent: remove staging directory: %w", cleanupErr))
	}
	return b.result
}

func (b *fileBuild) closedError() error {
	switch b.state {
	case buildCommitted:
		return ErrBuildCommitted
	case buildAborted:
		return ErrBuildAborted
	default:
		return nil
	}
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("persistent: open %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncHistory persists the fallback chain before the manifest is replaced.
func syncHistory(path string, hist []*Generation) error {
	return writeHistoryAtomic(path, hist)
}

func writeHistoryAtomic(path string, hist []*Generation) error {
	var data []byte
	if len(hist) == 0 {
		data = []byte("[]")
	} else {
		var err error
		data, err = json.Marshal(hist)
		if err != nil {
			return err
		}
	}
	return writeFileAtomic(path, data)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := replaceFile(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("persistent: replace %s: %w", filepath.Base(path), err)
	}
	return nil
}
