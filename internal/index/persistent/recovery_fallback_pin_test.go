package persistent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// commitPinGeneration commits one generation with the given segment payloads.
func commitPinGeneration(t *testing.T, s *FileStore, payloads ...[]byte) {
	t.Helper()
	build, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range payloads {
		if _, err := build.WriteSegment(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := build.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// rootSegmentFilesFor lists the segment files of genID currently in the root.
func rootSegmentFilesFor(t *testing.T, root string, genID uint64) []SegmentID {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "seg-" + strconv.FormatUint(genID, 10) + "-"
	var out []SegmentID
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".bin") {
			continue
		}
		out = append(out, SegmentID(strings.TrimSuffix(strings.TrimPrefix(name, "seg-"), ".bin")))
	}
	return out
}

// Pin for round20/bugB bug 5: recover's fallback loop treats a transient
// segment I/O failure exactly like an integrity failure — it pops the
// candidate from the in-memory history — and the orphan sweep right after
// derives "referenced" from that same list, so the healthy sibling segments
// of the hidden candidate were deleted as orphans within the very same open.
// The on-disk history still listed the candidate, so the next open
// reconsidered a generation whose segments were already gone and hid it
// permanently: the recovery safety net silently destroyed a healthy
// generation. A transient failure must instead keep the candidate in the
// in-memory history (hidden for this open's promotion, still referenced for
// the sweep, reconsidered by the next recover), mirroring
// openSnapshotWithWriterLock's fail-loudly policy for the current generation.
//
// Layout note: the corrupted manifest must belong to a generation that is
// no longer the newest, so the two-segment generation under test is a
// history CANDIDATE (recover's fallback list), not the corrupt current.
func TestRecoveryFallbackKeepsTransientCandidateForNextOpen(t *testing.T) {
	root := t.TempDir()
	s1, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	commitPinGeneration(t, s1, []byte("gen1 payload"))
	commitPinGeneration(t, s1, []byte("gen2 payload one"), []byte("gen2 payload two"))
	commitPinGeneration(t, s1, []byte("gen3 payload")) // newest = corrupt current

	seg2 := rootSegmentFilesFor(t, root, 2)
	if len(seg2) != 2 {
		t.Fatalf("gen2 must have two segments, got %v", seg2)
	}

	// Corrupt the current manifest so recover falls back through history
	// (the corrupt current is quarantined; gen2 is the newest candidate).
	if err := os.WriteFile(filepath.Join(root, fileManifest), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Transient filesystem failure on one gen2 segment: swap it for a
	// directory (the portable stand-in for an AV lock / sharing violation;
	// os.ReadFile then fails with a non-IsNotExist filesystem error, i.e.
	// the errSegmentIO class).
	heal := swapSegmentWithDir(t, root, seg2[1])

	// open#1: gen2 is the newest history candidate and fails transiently. It
	// must stay a referenced history candidate (its healthy sibling must
	// survive the orphan sweep) while gen1 is still promoted for this open.
	s2, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s2.current == nil || s2.current.ID != 1 {
		t.Fatalf("open#1 must promote gen1 through the fallback, got %+v", s2.current)
	}
	if _, err := os.Stat(filepath.Join(root, segFileName(seg2[0]))); err != nil {
		t.Fatalf("healthy sibling of the transiently-failed candidate was destroyed by the orphan sweep: %v", err)
	}

	// The transient failure clears: the candidate must be reconsidered and
	// promoted by the next recover, with both segments intact.
	healSegmentFromDir(t, root, seg2[1], heal)
	s3, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s3.current == nil || s3.current.ID != 2 {
		t.Fatalf("open#2 must reconsider and promote gen2, got %+v", s3.current)
	}
	view, err := s3.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range seg2 {
		if _, err := view.ReadSegment(id); err != nil {
			t.Fatalf("promoted gen2 segment %s must read back: %v", id, err)
		}
	}
}
