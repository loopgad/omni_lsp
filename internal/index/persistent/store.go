package persistent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ErrDiskBudgetExceeded reports a write refused because the store directory
// reached its configured budget (§L17: refuse, never fill the disk).
var ErrDiskBudgetExceeded = errors.New("persistent: disk budget exceeded")

// ErrNoGeneration reports that no verified generation could be recovered.
var ErrNoGeneration = errors.New("persistent: no usable generation")

// SegmentRef locates one segment inside the committed generation.
type SegmentRef struct {
	ID  SegmentID `json:"id"`
	Len int64     `json:"len"`
	CRC uint32    `json:"crc"`
}

// Generation is one committed snapshot of index content (L4).
type Generation struct {
	ID       uint64       `json:"generation"`
	Segments []SegmentRef `json:"segments"`
}

// GenerationView is what readers get from OpenSnapshot: immutable segments
// addressable by ID.
type GenerationView struct {
	Generation
	dir string // generation payload directory (root for v1 layout)
}

// ReadSegment returns the verified payload bytes of a segment. The decoder
// treats all stored lengths as untrusted input (L7).
func (v GenerationView) ReadSegment(id SegmentID) ([]byte, error) {
	if !validSegmentID(id) {
		return nil, fmt.Errorf("persistent: invalid segment ID %q", id)
	}
	var ref *SegmentRef
	for i := range v.Segments {
		if v.Segments[i].ID == id {
			ref = &v.Segments[i]
			break
		}
	}
	if ref == nil {
		return nil, fmt.Errorf("persistent: segment %s is not in generation %d", id, v.ID)
	}
	raw, err := os.ReadFile(filepath.Join(v.dir, segFileName(id)))
	if err != nil {
		return nil, fmt.Errorf("persistent: read %s: %w", id, err)
	}
	if int64(len(raw)) != ref.Len {
		return nil, fmt.Errorf("persistent: segment %s length %d, manifest says %d", id, len(raw), ref.Len)
	}
	if len(raw) < segHeaderSz || binary.BigEndian.Uint64(raw[8:16]) != v.ID {
		return nil, fmt.Errorf("persistent: segment %s does not belong to generation %d", id, v.ID)
	}
	payload, err := decodeSegment(raw)
	if err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(payload) != ref.CRC {
		return nil, fmt.Errorf("persistent: segment %s checksum differs from manifest", id)
	}
	return payload, nil
}

// IndexStore is the §L5 storage boundary. Implementations own durability;
// callers own content semantics.
type IndexStore interface {
	OpenSnapshot(ctx context.Context) (GenerationView, error)
	BeginBuild(ctx context.Context) (BuildSession, error)
	Quarantine(seg SegmentID) error
	Quarantined() []SegmentID
	Compact(ctx context.Context) error
}

// BuildSession stages segments for the next generation (TXN-002).
type BuildSession interface {
	WriteSegment(payload []byte) (SegmentID, error)
	Commit(ctx context.Context) error
	Abort() error
}

// Config controls FileStore behaviour.
type Config struct {
	DiskBudgetBytes int64 // 0 = unlimited
	KeepGenerations int   // fallback depth; <=0 → historyK
}

// FileStore is the file-backed IndexStore implementation (v1 ADR choice:
// immutable per-generation segment files + atomic manifest rename).
type FileStore struct {
	root string
	cfg  Config

	mu          sync.Mutex
	current     *Generation   // committed pointer (TXN-001 anchor)
	history     []*Generation // newest first, capped at KeepGenerations
	quarantined []SegmentID
	nextGenID   uint64
}

// NewFileStore opens (creating if needed) a store at root and recovers the
// newest verifiable generation (§L8).
func NewFileStore(root string, cfg Config) (*FileStore, error) {
	if err := ensureLayout(root); err != nil {
		return nil, err
	}
	writerLock, err := acquireWriterLock(context.Background(), root)
	if err != nil {
		return nil, err
	}
	defer writerLock.release()
	if cfg.KeepGenerations <= 0 {
		cfg.KeepGenerations = historyK
	}
	s := &FileStore{root: root, cfg: cfg}
	if err := s.recover(); err != nil && !errors.Is(err, ErrNoGeneration) {
		return nil, err
	}
	return s, nil
}

// manifestPath / historyPath are root-level pointers (atomic rename target).
func (s *FileStore) manifestPath() string { return filepath.Join(s.root, fileManifest) }
func (s *FileStore) tmpManifestPath() string {
	return filepath.Join(s.root, fileTmp)
}
func (s *FileStore) historyPath() string { return filepath.Join(s.root, fileHistory) }
func (s *FileStore) quarantineDir() string {
	return filepath.Join(s.root, dirQuarantine)
}

// OpenSnapshot returns the current verified generation. The per-store writer
// lock protects recovery and quarantine across processes; any invalid segment
// quarantines its whole generation and falls back to the newest survivor (L7/L8).
func (s *FileStore) OpenSnapshot(ctx context.Context) (GenerationView, error) {
	writerLock, err := acquireWriterLock(ctx, s.root)
	if err != nil {
		return GenerationView{}, err
	}
	defer writerLock.release()
	s.mu.Lock()
	err = s.recover()
	s.mu.Unlock()
	if err != nil {
		return GenerationView{}, err
	}
	return s.openSnapshotWithWriterLock(ctx)
}

func (s *FileStore) openSnapshotWithWriterLock(ctx context.Context) (GenerationView, error) {
	for attempt := 0; attempt < 8; attempt++ { // bounded fallback chain
		if err := ctx.Err(); err != nil {
			return GenerationView{}, err
		}
		s.mu.Lock()
		cur := s.current
		s.mu.Unlock()
		if cur == nil {
			return GenerationView{}, ErrNoGeneration
		}

		view := GenerationView{Generation: *cur, dir: s.root}
		var bad []SegmentID
		for _, seg := range cur.Segments {
			if _, err := view.ReadSegment(seg.ID); err != nil {
				bad = append(bad, seg.ID)
			}
		}
		if len(bad) == 0 {
			return view, nil
		}

		genID := cur.ID
		s.mu.Lock()
		if err := s.quarantineSegsLocked(bad); err != nil {
			s.mu.Unlock()
			return GenerationView{}, err
		}
		if s.current != nil && s.current.ID == genID {
			s.dropCurrentLocked() // TXN-004: whole-generation distrust
		}
		s.mu.Unlock()
	}
	return GenerationView{}, fmt.Errorf("persistent: recovery exceeded fallback depth %d", historyK)
}

// BeginBuild starts staging a new generation in an isolated directory.
func (s *FileStore) BeginBuild(ctx context.Context) (BuildSession, error) {
	writerLock, err := acquireWriterLock(ctx, s.root)
	if err != nil {
		return nil, err
	}
	return s.beginBuildWithWriterLock(ctx, writerLock)
}

func (s *FileStore) beginBuildWithWriterLock(ctx context.Context, writerLock *writerLock) (BuildSession, error) {
	if err := ctx.Err(); err != nil {
		writerLock.release()
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recover(); err != nil {
		writerLock.release()
		return nil, err
	}
	s.nextGenID++
	genID := s.nextGenID
	stage := filepath.Join(s.root, fmt.Sprintf("staging-%d", genID))
	if err := os.MkdirAll(stage, 0o755); err != nil {
		writerLock.release()
		return nil, fmt.Errorf("persistent: staging: %w", err)
	}
	return &fileBuild{store: s, genID: genID, stageDir: stage, writerLock: writerLock}, nil
}

// Quarantine moves suspect segments out of the readable path with evidence
// preserved (TXN-004). If the current generation loses any member it is
// dropped wholesale and the newest surviving history entry takes over.
func (s *FileStore) Quarantine(seg SegmentID) error {
	writerLock, err := acquireWriterLock(context.Background(), s.root)
	if err != nil {
		return err
	}
	defer writerLock.release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recover(); err != nil {
		return err
	}
	return s.quarantineSegsLocked([]SegmentID{seg})
}

func (s *FileStore) quarantineSegsLocked(segs []SegmentID) error {
	for _, seg := range segs {
		if !validSegmentID(seg) {
			return fmt.Errorf("persistent: invalid segment ID %q", seg)
		}
		src := filepath.Join(s.root, segFileName(seg))
		dst := filepath.Join(s.quarantineDir(), segFileName(seg))
		if _, err := os.Stat(src); err == nil {
			if rmErr := os.Remove(dst); rmErr != nil && !os.IsNotExist(rmErr) {
				return rmErr
			}
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("persistent: quarantine %s: %w", seg, err)
			}
		}
		s.quarantined = append(s.quarantined, seg)
	}
	// If any quarantined segment belonged to the current generation, drop it.
	if s.current != nil {
		bad := map[SegmentID]bool{}
		for _, q := range segs {
			bad[q] = true
		}
		for _, seg := range s.current.Segments {
			if bad[seg.ID] {
				s.dropCurrentLocked()
				break
			}
		}
	}
	return nil
}

// QuarantineGeneration drops an entire generation by ID (TXN-004 whole-trust
// rule) and falls back to the newest surviving history entry.
func (s *FileStore) QuarantineGeneration(genID uint64, segs []SegmentID) error {
	writerLock, err := acquireWriterLock(context.Background(), s.root)
	if err != nil {
		return err
	}
	defer writerLock.release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recover(); err != nil {
		return err
	}
	if s.current == nil || s.current.ID != genID {
		return nil
	}
	if err := s.quarantineSegsLocked(segs); err != nil {
		return err
	}
	if s.current != nil && s.current.ID == genID {
		s.dropCurrentLocked()
	}
	return nil
}

// dropCurrentLocked advances to the newest surviving history generation.
// Caller must hold mu.
func (s *FileStore) dropCurrentLocked() {
	if len(s.history) > 0 {
		s.current = s.history[0]
		s.history = s.history[1:]
	} else {
		s.current = nil
	}
	_ = os.Remove(s.manifestPath()) // pointer gone; recovery starts clean
}

// Quarantined lists segments moved out of service.
func (s *FileStore) Quarantined() []SegmentID {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SegmentID, len(s.quarantined))
	copy(out, s.quarantined)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
