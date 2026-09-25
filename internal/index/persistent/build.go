package persistent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
)

// fileBuild stages the next generation; nothing it writes is discoverable
// through the manifest until Commit's atomic pointer swap (TXN-002).
type fileBuild struct {
	store      *FileStore
	genID      uint64
	stageDir   string
	segments   []SegmentRef
	aborted    bool
	writerLock *writerLock
}

// WriteSegment encodes, fsyncs and records one immutable segment. Budget is
// enforced before touching disk (§L17).
func (b *fileBuild) WriteSegment(payload []byte) (SegmentID, error) {
	if b.aborted {
		return "", fmt.Errorf("persistent: build aborted")
	}
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

// Commit publishes the staged generation: move segments into the store root,
// write manifest.tmp, fsync, then atomically rename over the pointer (L4).
func (b *fileBuild) Commit(ctx context.Context) error {
	defer b.writerLock.release()
	return b.commit(ctx)
}

func (b *fileBuild) commit(ctx context.Context) error {
	if b.aborted {
		return fmt.Errorf("persistent: build aborted")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	gen := Generation{ID: b.genID, Segments: b.segments}

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
	}

	mb, err := json.MarshalIndent(gen, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.store.tmpManifestPath()
	if err := writeFileSync(tmp, mb); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Windows: os.Rename cannot clobber; the remove window is the one non-
	// atomic step — recovery tolerates a missing pointer (falls back to
	// history), so this never exposes a half-published generation.
	_ = os.Remove(b.store.manifestPath())
	if err := os.Rename(tmp, b.store.manifestPath()); err != nil {
		return fmt.Errorf("persistent: publish manifest: %w", err)
	}

	b.store.mu.Lock()
	if b.store.current != nil {
		b.store.history = append([]*Generation{b.store.current}, b.store.history...)
		if len(b.store.history) > b.store.cfg.KeepGenerations {
			old := b.store.history[len(b.store.history)-1]
			for _, seg := range old.Segments {
				_ = os.Remove(filepath.Join(b.store.root, segFileName(seg.ID)))
			}
			b.store.history = b.store.history[:len(b.store.history)-1]
		}
	}
	b.store.current = &gen
	b.store.nextGenID = b.genID
	syncHistory(b.store.historyPath(), b.store.history)
	b.store.mu.Unlock()

	return os.RemoveAll(b.stageDir)
}

// Abort discards staging; half-written data stays undiscoverable (TXN-002).
func (b *fileBuild) Abort() error {
	b.aborted = true
	err := os.RemoveAll(b.stageDir)
	b.writerLock.release()
	return err
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

// syncHistory persists the fallback chain best-effort; loss only narrows
// recovery options, never correctness.
func syncHistory(path string, hist []*Generation) {
	if len(hist) == 0 {
		_ = writeFileSync(path, []byte("[]"))
		return
	}
	b, err := json.Marshal(hist)
	if err != nil {
		return
	}
	_ = writeFileSync(path, b)
}
