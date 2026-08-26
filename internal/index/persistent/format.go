// Package persistent implements the transactional on-disk index store
// (goal.md §L4-L8, §L16-L17): immutable segments, atomic generation
// publication, checksum verification, quarantine of corrupt data with
// fallback to the last good generation, and a disk budget.
//
// Owned mutable state: mu guards generation pointer + history + quarantine
// list; build sessions own their staging directories exclusively.
//
// Concurrency model: single-writer (BeginBuild/Publish/Quarantine/Compact
// serialized by mu); readers clone the committed manifest under RLock and
// read segment files immutably afterwards.
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
	"fmt"
	"os"
	"path/filepath"
)

const (
	dirFormat   = "v1"
	segMagic    = "OMNI"
	schemaVer   = uint32(1)
	segHeaderSz = 4 + 4 + 8 + 8 + 4 // magic + ver + gen + len + crc32
	historyK    = 3                 // generations kept for fallback (L8)

	fileManifest  = "manifest.json"
	fileTmp       = "manifest.json.tmp"
	fileHistory   = "history.json"
	dirQuarantine = "quarantine"
)

// SegmentID names an immutable segment file inside a generation directory.
type SegmentID string

func segFileName(id SegmentID) string { return fmt.Sprintf("seg-%s.bin", id) }

// ensureLayout creates the store root layout on first open.
func ensureLayout(root string) error {
	for _, d := range []string{"", dirQuarantine} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return fmt.Errorf("persistent: layout %s: %w", d, err)
		}
	}
	return nil
}
