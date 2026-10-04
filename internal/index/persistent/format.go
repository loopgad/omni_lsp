// Package persistent implements the transactional on-disk index store
// (goal.md §L4-L8, §L16-L17): immutable segments, atomic generation
// publication, checksum verification, quarantine of corrupt data with
// fallback to the last good generation, and a disk budget.
//
// Owned mutable state: mu guards generation pointer + history + quarantine
// list; build sessions own their staging directories exclusively. A durable
// sequence file reserves generation IDs before extraction starts.
//
// Concurrency model: writer.lock serializes reservation, recovery,
// publication, quarantine, and compaction across FileStore instances/processes.
// Build sessions release it between segment writes so extraction can proceed
// concurrently. Generation leases use shared per-generation locks to prevent
// compaction from deleting files a reader still uses.
//
// Invariants:
//  1. IDX-TXN-001: readers observe exactly one committed generation — never
//     a mix of N and N+1 segments.
//  2. TXN-002: a half-written segment is never discoverable through the
//     committed manifest (staging lives outside it until fsync+rename).
//  3. TXN-004: any byte-level corruption quarantines the whole offending
//     generation; partially decodable data is not trusted.
//  4. L8: recovery always leaves either a verified older generation or a
//     typed error — the process survives.
package persistent

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	dirFormat   = "v1"
	dirLeases   = "leases"
	segMagic    = "OMNI"
	schemaVer   = uint32(1)
	segHeaderSz = 4 + 4 + 8 + 8 + 4 // magic + ver + gen + len + crc32
	historyK    = 3                 // generations kept for fallback (L8)

	fileManifest  = "manifest.json"
	fileTmp       = "manifest.json.tmp"
	fileHistory   = "history.json"
	fileGenSeq    = "generation.seq"
	dirQuarantine = "quarantine"
)

// SegmentID names an immutable segment file inside a generation directory.
type SegmentID string

func segFileName(id SegmentID) string { return fmt.Sprintf("seg-%s.bin", id) }

// validSegmentID accepts only IDs emitted by fileBuild. Manifest contents are
// untrusted, so validate before turning an ID into a filesystem path.
func validSegmentID(id SegmentID) bool {
	value := string(id)
	separator := strings.IndexByte(value, '-')
	if separator <= 0 || separator == len(value)-1 || separator != strings.LastIndexByte(value, '-') {
		return false
	}
	genID, err := strconv.ParseUint(value[:separator], 10, 64)
	if err != nil || genID == 0 || strconv.FormatUint(genID, 10) != value[:separator] {
		return false
	}
	random := value[separator+1:]
	if len(random) != 8 {
		return false
	}
	decoded, err := hex.DecodeString(random)
	return err == nil && len(decoded) > 0 && hex.EncodeToString(decoded) == random
}

func validGeneration(gen *Generation) bool {
	if gen == nil || gen.ID == 0 || len(gen.Segments) == 0 {
		return false
	}
	seen := make(map[SegmentID]bool, len(gen.Segments))
	for _, seg := range gen.Segments {
		if !validSegmentID(seg.ID) || seg.Len < segHeaderSz || seen[seg.ID] {
			return false
		}
		seen[seg.ID] = true
		idGen, _ := strconv.ParseUint(strings.SplitN(string(seg.ID), "-", 2)[0], 10, 64)
		if idGen != gen.ID {
			return false
		}
	}
	return true
}

// ensureLayout creates the store root layout on first open.
func ensureLayout(root string) error {
	for _, d := range []string{"", dirQuarantine, dirLeases} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return fmt.Errorf("persistent: layout %s: %w", d, err)
		}
	}
	return nil
}
