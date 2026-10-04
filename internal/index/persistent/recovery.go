package persistent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// encodeSegment frames a payload: magic | schemaVer | genID | len | crc32.
// All framing fields are treated as untrusted on decode (L7).
func encodeSegment(genID uint64, payload []byte) []byte {
	out := make([]byte, segHeaderSz+len(payload))
	copy(out[0:4], segMagic)
	binary.BigEndian.PutUint32(out[4:8], schemaVer)
	binary.BigEndian.PutUint64(out[8:16], genID)
	binary.BigEndian.PutUint64(out[16:24], uint64(len(payload)))
	binary.BigEndian.PutUint32(out[24:28], crc32.ChecksumIEEE(payload))
	copy(out[segHeaderSz:], payload)
	return out
}

// decodeSegment validates the frame and returns the verified payload. Any
// length that does not match the actual file size is corruption (L7).
func decodeSegment(raw []byte) ([]byte, error) {
	if len(raw) < segHeaderSz {
		return nil, fmt.Errorf("persistent: segment shorter than header (%d)", len(raw))
	}
	if string(raw[0:4]) != segMagic {
		return nil, fmt.Errorf("persistent: bad magic %q", raw[0:4])
	}
	if v := binary.BigEndian.Uint32(raw[4:8]); v != schemaVer {
		return nil, fmt.Errorf("persistent: schema version %d, want %d", v, schemaVer)
	}
	_ = binary.BigEndian.Uint64(raw[8:16]) // genID informational
	n := binary.BigEndian.Uint64(raw[16:24])
	want := binary.BigEndian.Uint32(raw[24:28])
	payload := raw[segHeaderSz:]
	if uint64(len(payload)) != n {
		return nil, fmt.Errorf("persistent: declared %d bytes, file holds %d", n, len(payload))
	}
	if got := crc32.ChecksumIEEE(payload); got != want {
		return nil, fmt.Errorf("persistent: checksum mismatch (want %08x got %08x)", want, got)
	}
	return payload, nil
}

// recover loads the newest verifiable generation at open time. A corrupt
// current manifest falls back through history entries; everything failing
// verification leaves the store empty-but-alive with ErrNoGeneration (L8).
func (s *FileStore) recover() error {
	s.current = nil
	s.history = nil
	s.quarantined = s.readQuarantined()

	mb, err := os.ReadFile(s.manifestPath())
	if err == nil {
		var gen Generation
		if jerr := json.Unmarshal(mb, &gen); jerr == nil && validGeneration(&gen) {
			s.current = &gen
		} else {
			_ = os.Remove(s.manifestPath())
		}
	}

	hb, err := os.ReadFile(s.historyPath())
	if err == nil {
		var hist []*Generation
		if json.Unmarshal(hb, &hist) == nil {
			seen := make(map[uint64]bool, len(hist))
			for _, gen := range hist {
				if !validGeneration(gen) || (s.current != nil && gen.ID == s.current.ID) || seen[gen.ID] {
					continue
				}
				seen[gen.ID] = true
				s.history = append(s.history, gen)
			}
			sort.Slice(s.history, func(i, j int) bool { return s.history[i].ID > s.history[j].ID })
		}
	}

	if s.current == nil {
		// Fall back through history until one generation verifies fully.
		for len(s.history) > 0 {
			cand := s.history[0]
			view := GenerationView{Generation: *cand, dir: s.root}
			ok := true
			for _, seg := range cand.Segments {
				if _, err := view.ReadSegment(seg.ID); err != nil {
					ok = false
					break
				}
			}
			s.history = s.history[1:]
			if ok {
				s.current = cand
				break
			}
		}
	}
	if s.current != nil && s.current.ID > s.nextGenID {
		s.nextGenID = s.current.ID
	}
	for _, gen := range s.history {
		if gen.ID > s.nextGenID {
			s.nextGenID = gen.ID
		}
	}
	if seq, exists, err := readGenerationSequence(s.root); err == nil && exists && seq > s.nextGenID {
		s.nextGenID = seq
	}
	if staged, err := stagingHighWater(s.root); err == nil && staged > s.nextGenID {
		s.nextGenID = staged
	}
	return nil
}

func readGenerationSequence(root string) (uint64, bool, error) {
	path := filepath.Join(root, fileGenSeq)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	value := strings.TrimSpace(string(raw))
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil || id == 0 || strconv.FormatUint(id, 10) != value {
		return 0, true, fmt.Errorf("persistent: invalid generation sequence %q", value)
	}
	return id, true, nil
}

func writeGenerationSequence(root string, id uint64) error {
	return writeFileAtomic(filepath.Join(root, fileGenSeq), []byte(strconv.FormatUint(id, 10)+"\n"))
}

func stagingHighWater(root string) (uint64, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	var high uint64
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "staging-") {
			continue
		}
		text := strings.TrimPrefix(entry.Name(), "staging-")
		id, err := strconv.ParseUint(text, 10, 64)
		if err == nil && id > high && strconv.FormatUint(id, 10) == text {
			high = id
		}
	}
	return high, nil
}

func (s *FileStore) cleanupOrphanSegments() {
	s.mu.Lock()
	referenced := make(map[SegmentID]bool)
	if s.current != nil {
		for _, seg := range s.current.Segments {
			referenced[seg.ID] = true
		}
	}
	for _, gen := range s.history {
		for _, seg := range gen.Segments {
			referenced[seg.ID] = true
		}
	}
	s.mu.Unlock()

	entries, err := os.ReadDir(s.root)
	if err != nil {
		return
	}
	retired := make(map[uint64]*Generation)
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "seg-") || !strings.HasSuffix(entry.Name(), ".bin") {
			continue
		}
		id := SegmentID(strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "seg-"), ".bin"))
		if referenced[id] || !validSegmentID(id) {
			continue
		}
		separator := strings.IndexByte(string(id), '-')
		genID, err := strconv.ParseUint(string(id[:separator]), 10, 64)
		if err != nil {
			continue
		}
		gen := retired[genID]
		if gen == nil {
			gen = &Generation{ID: genID}
			retired[genID] = gen
		}
		gen.Segments = append(gen.Segments, SegmentRef{ID: id})
	}
	for _, gen := range retired {
		_ = s.removeGenerationSegments(gen)
	}
}

func (s *FileStore) readQuarantined() []SegmentID {
	entries, err := os.ReadDir(s.quarantineDir())
	if err != nil {
		return nil
	}
	var out []SegmentID
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".bin") {
			continue
		}
		id := SegmentID(strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".bin"))
		if validSegmentID(id) {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// mergeFraming is the length-prefixed layout used to pack many logical
// payloads into one physical segment during compaction.
func mergeAppend(dst []byte, payloads ...[]byte) []byte {
	for _, p := range payloads {
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(len(p)))
		dst = append(dst, l[:]...)
		dst = append(dst, p...)
	}
	return dst
}

// SplitMerged unpacks a compacted segment produced by mergeAppend.
func SplitMerged(merged []byte) [][]byte {
	var out [][]byte
	for len(merged) >= 8 {
		n := binary.BigEndian.Uint64(merged[:8])
		merged = merged[8:]
		if uint64(len(merged)) < n {
			return out // defensive: truncated tail (L7)
		}
		out = append(out, merged[:n])
		merged = merged[n:]
	}
	return out
}

// Compact merges a pinned snapshot into one fresh segment. It holds the
// cross-process writer lock only while opening the snapshot, reserving the new
// generation, publishing it, and pruning history.
func (s *FileStore) Compact(ctx context.Context) error {
	writerLock, err := acquireWriterLock(ctx, s.root)
	if err != nil {
		return err
	}
	defer writerLock.release()
	s.mu.Lock()
	err = s.recover()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.cleanupOrphanSegments()
	view, err := s.openSnapshotWithWriterLock(ctx)
	if err != nil {
		return err
	}
	leaseLock, err := acquireGenerationLock(ctx, s.root, view.ID, false, true)
	if err != nil {
		return err
	}
	lease := &GenerationLease{view: view, lock: leaseLock, store: s}
	build, err := s.beginBuildWithWriterLock(ctx, writerLock)
	if err != nil {
		_ = lease.Close()
		return err
	}
	defer lease.Close()
	view = lease.Snapshot()
	var merged []byte
	for _, seg := range view.Segments {
		payload, err := view.ReadSegment(seg.ID)
		if err != nil {
			build.Abort()
			return err
		}
		merged = mergeAppend(merged, payload)
	}
	if _, err := build.WriteSegment(merged); err != nil {
		_ = build.Abort()
		return err
	}
	if err := build.Commit(ctx); err != nil {
		_ = build.Abort()
		return err
	}
	_ = lease.Close()
	// The previous generation is retained in history for fallback. Retire it
	// from that chain durably before deleting its segments; older fallbacks stay.
	return s.pruneHistoryGeneration(view.ID)
}

func (s *FileStore) pruneHistoryGeneration(genID uint64) error {
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
	if s.current != nil && s.current.ID == genID {
		return fmt.Errorf("persistent: refusing to prune current generation %d", genID)
	}
	kept := make([]*Generation, 0, len(s.history))
	var retired *Generation
	for _, gen := range s.history {
		if gen.ID == genID {
			retired = gen
		} else {
			kept = append(kept, gen)
		}
	}
	if retired == nil {
		return nil
	}
	if err := writeHistoryAtomic(s.historyPath(), kept); err != nil {
		return fmt.Errorf("persistent: prune recovery history: %w", err)
	}
	s.history = kept
	return s.removeGenerationSegments(retired)
}
