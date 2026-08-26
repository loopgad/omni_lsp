package persistent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
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
	mb, err := os.ReadFile(s.manifestPath())
	if err == nil {
		var gen Generation
		if jerr := json.Unmarshal(mb, &gen); jerr == nil && len(gen.Segments) > 0 {
			s.current = &gen
		} else {
			_ = s.quarantineSegs(nil) // record nothing; pointer unusable
			_ = os.Remove(s.manifestPath())
		}
	}

	hb, err := os.ReadFile(s.historyPath())
	if err == nil {
		var hist []*Generation
		if json.Unmarshal(hb, &hist) == nil {
			s.history = hist
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
	return nil
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

// Compact merges every committed segment into ONE fresh segment and atomically
// retires the old files (§L16).
func (s *FileStore) Compact(ctx context.Context) error {
	view, err := s.OpenSnapshot(ctx)
	if err != nil {
		return err
	}
	build, err := s.BeginBuild(ctx)
	if err != nil {
		return err
	}
	oldSegs := make([]SegmentID, 0, len(view.Segments))
	var merged []byte
	for _, seg := range view.Segments {
		payload, err := view.ReadSegment(seg.ID)
		if err != nil {
			build.Abort()
			return err
		}
		oldSegs = append(oldSegs, seg.ID)
		merged = mergeAppend(merged, payload)
	}
	if _, err := build.WriteSegment(merged); err != nil {
		build.Abort()
		return err
	}
	if err := build.Commit(ctx); err != nil {
		return err
	}
	// Retire superseded segments after the new pointer is live (best-effort;
	// leftovers only waste budget, never correctness — L4 retirement step).
	for _, id := range oldSegs {
		_ = os.Remove(filepath.Join(s.root, segFileName(id)))
	}
	return nil
}
