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
	"time"
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
		jerr := json.Unmarshal(mb, &gen)
		switch {
		case jerr == nil && validGeneration(&gen):
			s.current = &gen
		case jerr != nil:
			if qerr := s.quarantineCorruptManifest("undecodable"); qerr != nil {
				return qerr
			}
		default:
			if qerr := s.quarantineCorruptManifest("invalid-generation"); qerr != nil {
				return qerr
			}
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
		// Integrity failures corrupt a candidate for good; a transient
		// I/O failure (errSegmentIO) only hides the candidate for this
		// open — the retry budget below absorbs AV locks and sharing
		// violations, and the on-disk history is left untouched so the
		// next recover reconsiders it (nothing is quarantined here).
		for len(s.history) > 0 {
			cand := s.history[0]
			view := GenerationView{Generation: *cand, dir: s.root}
			ok := true
			for _, seg := range cand.Segments {
				if _, err := view.readSegmentWithIORetry(seg.ID); err != nil {
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

// quarantineCorruptManifest moves an undecodable or invalid manifest.json
// into the quarantine directory under a manifest-corrupt-<reason>-<timestamp>
// name, so a failed publish pointer leaves forensic residue (§L8: integrity
// failures are quarantined for diagnostics — the same treatment corrupt
// segments get) instead of vanishing with a silent remove. The evidence files
// are deliberately not recycled by recycleQuarantine (it only prunes
// seg-*.bin): one small file per corruption event, bounded by how often
// corruption happens.
//
// When the move is blocked (file occupying the quarantine path, AV lock,
// sharing violation) the error is propagated: recover then fails and the next
// open retries, instead of the store silently continuing with the only
// diagnostic destroyed. Caller holds the writer lock (recover contract).
func (s *FileStore) quarantineCorruptManifest(reason string) error {
	if err := os.MkdirAll(s.quarantineDir(), 0o700); err != nil {
		return fmt.Errorf("persistent: quarantine corrupt manifest: %w", err)
	}
	name := fmt.Sprintf("manifest-corrupt-%s-%s.json", reason, time.Now().UTC().Format("20060102T150405.000000000"))
	dst := filepath.Join(s.quarantineDir(), name)
	if err := os.Rename(s.manifestPath(), dst); err != nil {
		return fmt.Errorf("persistent: quarantine corrupt manifest: %w", err)
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
		// validSegmentID already guaranteed the prefix before the only '-'
		// parses as a canonical uint64 (format.go), so the error is ignored.
		separator := strings.IndexByte(string(id), '-')
		genID, _ := strconv.ParseUint(string(id[:separator]), 10, 64)
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
	s.recycleQuarantine()
}

// quarantineBudgetDivisor bounds quarantined forensic files at
// DiskBudgetBytes/quarantineBudgetDivisor so repeated corruption cannot
// monotonically exhaust storage (§L17) — quarantine no longer counts against
// the live write budget (build.go), so this cap is its only bound.
const quarantineBudgetDivisor = 4

// recycleQuarantine deletes quarantined segment files beyond the quarantine
// capacity cap, oldest generation first, and drops the recycled IDs from the
// in-memory quarantine list. Quarantine is forensic only — no code path reads
// quarantined files back — so dropping the oldest evidence is safe. Caller
// must hold the writer lock (cleanupOrphanSegments contract). With
// DiskBudgetBytes = 0 (unlimited) no cap applies.
func (s *FileStore) recycleQuarantine() {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := s.cfg.DiskBudgetBytes / quarantineBudgetDivisor
	if limit <= 0 {
		return
	}
	entries, err := os.ReadDir(s.quarantineDir())
	if err != nil {
		return
	}
	type quarantinedFile struct {
		id   SegmentID
		gen  uint64
		size int64
	}
	var files []quarantinedFile
	var total int64
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "seg-") || !strings.HasSuffix(name, ".bin") {
			continue
		}
		id := SegmentID(strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".bin"))
		if !validSegmentID(id) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		// validSegmentID already guaranteed the prefix before the only '-'
		// parses as a canonical uint64 (format.go), so the error is ignored.
		separator := strings.IndexByte(string(id), '-')
		genID, _ := strconv.ParseUint(string(id[:separator]), 10, 64)
		files = append(files, quarantinedFile{id: id, gen: genID, size: info.Size()})
		total += info.Size()
	}
	if total <= limit {
		return
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].gen != files[j].gen {
			return files[i].gen < files[j].gen
		}
		return files[i].id < files[j].id
	})
	pruned := make(map[SegmentID]bool)
	for _, f := range files {
		if total <= limit {
			break
		}
		if err := os.Remove(filepath.Join(s.quarantineDir(), segFileName(f.id))); err != nil && !os.IsNotExist(err) {
			continue
		}
		total -= f.size
		pruned[f.id] = true
	}
	if len(pruned) == 0 {
		return
	}
	kept := s.quarantined[:0]
	for _, id := range s.quarantined {
		if !pruned[id] {
			kept = append(kept, id)
		}
	}
	s.quarantined = kept
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

// Compact moved to compact.go: structure-bounded compaction (DEF-COMPACT)
// re-partitions batch envelopes instead of merging payloads into one
// physical segment, which the live semantic format cannot read back.

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
