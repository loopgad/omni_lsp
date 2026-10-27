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
	"time"
)

// ErrDiskBudgetExceeded reports a write refused because the store directory
// reached its configured budget (§L17: refuse, never fill the disk).
var ErrDiskBudgetExceeded = errors.New("persistent: disk budget exceeded")

// ErrNoGeneration reports that no verified generation could be recovered.
var ErrNoGeneration = errors.New("persistent: no usable generation")

// errSegmentIO marks a ReadSegment failure caused by the filesystem itself
// rather than by segment content: sharing violations, AV locks, permission
// errors, transient device failures. Content-integrity failures (missing
// manifest-referenced file, decode, checksum, length) do not carry it, so
// recovery can restrict quarantine to real corruption (L7/L8 only sanction
// isolation for integrity failures).
var errSegmentIO = errors.New("persistent: transient segment I/O")

// isSegmentIOError reports whether err is a filesystem-level read failure
// rather than a content-integrity failure.
func isSegmentIOError(err error) bool { return errors.Is(err, errSegmentIO) }

// Segment I/O retry budget: transient Windows faults (AV scan, sharing
// violation) usually clear within milliseconds.
const (
	segIORetryAttempts = 3
	segIORetryDelay    = 5 * time.Millisecond
)

// readSegmentWithIORetry re-reads one segment with a small bounded backoff
// while the failure is transient I/O. Integrity failures return immediately.
// The last error is returned once the retry budget is spent.
func (v GenerationView) readSegmentWithIORetry(id SegmentID) ([]byte, error) {
	var payload []byte
	var err error
	for attempt := 0; attempt < segIORetryAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(segIORetryDelay)
		}
		payload, err = v.ReadSegment(id)
		if err == nil || !isSegmentIOError(err) {
			return payload, err
		}
	}
	return payload, err
}

var (
	ErrBuildAborted   = errors.New("persistent: build aborted")
	ErrBuildCommitted = errors.New("persistent: build already committed")
)

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
		if os.IsNotExist(err) {
			// The manifest references a segment that is gone: that is
			// generation corruption (integrity), not transient I/O.
			return nil, fmt.Errorf("persistent: read %s: %w", id, err)
		}
		// Everything else (sharing violation, permission, device error)
		// is filesystem-level and potentially transient (L7).
		return nil, fmt.Errorf("persistent: read %s: %w: %w", id, errSegmentIO, err)
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

// BuildSession stages segments for the next generation (TXN-002).
type BuildSession interface {
	// GenerationID is assigned durably when the session is reserved. It is
	// published only if Commit succeeds.
	GenerationID() uint64
	WriteSegment(payload []byte) (SegmentID, error)
	Commit(ctx context.Context) error
	Abort() error
}

// Config controls FileStore behaviour.
type Config struct {
	DiskBudgetBytes int64 // 0 = unlimited
	KeepGenerations int   // fallback depth; <=0 → historyK
}

// FileStore is the §L5 storage boundary's file-backed implementation (v1 ADR
// choice: immutable per-generation segment files + atomic manifest rename).
// The store owns durability; callers own content semantics.
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
	s.cleanupOrphanSegments()
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
	s.cleanupOrphanSegments()
	return s.openSnapshotWithWriterLock(ctx)
}

// OpenSnapshotLease opens and pins the current verified generation. The
// returned lease prevents compaction in this or another process from deleting
// its segment files until Close is called.
func (s *FileStore) OpenSnapshotLease(ctx context.Context) (*GenerationLease, error) {
	writerLock, err := acquireWriterLock(ctx, s.root)
	if err != nil {
		return nil, err
	}
	defer writerLock.release()
	s.mu.Lock()
	err = s.recover()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.cleanupOrphanSegments()
	view, err := s.openSnapshotWithWriterLock(ctx)
	if err != nil {
		return nil, err
	}
	leaseLock, err := acquireGenerationLock(ctx, s.root, view.ID, false, true)
	if err != nil {
		return nil, err
	}
	return &GenerationLease{view: view, lock: leaseLock, store: s}, nil
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
		var ioErr error
		for _, seg := range cur.Segments {
			if _, err := view.readSegmentWithIORetry(seg.ID); err != nil {
				if isSegmentIOError(err) {
					ioErr = err
				} else {
					bad = append(bad, seg.ID) // integrity failure only (L7)
				}
			}
		}
		if ioErr != nil {
			// Transient I/O survived the retry budget (whether or not
			// integrity failures sit alongside it). Quarantining or dropping
			// a possibly healthy generation is not sanctioned (L7/L8 isolate
			// integrity failures only): fail the open loudly and leave the
			// store untouched — the next open retries.
			return GenerationView{}, ioErr
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
	defer writerLock.release()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.recover(); err != nil {
		return nil, err
	}
	seqID, exists, err := readGenerationSequence(s.root)
	if err != nil {
		return nil, err
	}
	if exists && seqID > s.nextGenID {
		s.nextGenID = seqID
	}
	if s.nextGenID == ^uint64(0) {
		return nil, fmt.Errorf("persistent: generation ID space exhausted")
	}
	genID := s.nextGenID + 1
	stage := filepath.Join(s.root, fmt.Sprintf("staging-%d", genID))
	if err := os.Mkdir(stage, 0o755); err != nil {
		return nil, fmt.Errorf("persistent: staging: %w", err)
	}
	if err := writeGenerationSequence(s.root, genID); err != nil {
		_ = os.RemoveAll(stage)
		return nil, fmt.Errorf("persistent: reserve generation %d: %w", genID, err)
	}
	s.nextGenID = genID
	return &fileBuild{store: s, genID: genID, stageDir: stage}, nil
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
