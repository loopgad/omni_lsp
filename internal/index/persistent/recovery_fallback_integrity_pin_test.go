package persistent

import (
	"os"
	"path/filepath"
	"testing"
)

// corruptPayloadByte flips the last payload byte of a committed segment file,
// turning it into a content-integrity failure (checksum mismatch) — not a
// transient I/O failure — for the next recover.
func corruptPayloadByte(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= segHeaderSz {
		t.Fatalf("segment %s too small to corrupt its payload", path)
	}
	raw[len(raw)-1] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Pin for verifierB-r23b entry 1: recover's fallback loop hid an
// integrity-failed history candidate from the in-memory history, and the
// orphan sweep that runs right after derives "referenced" from that same list
// — so the whole candidate generation (the corrupt segment AND its healthy
// siblings) plus the segments of the previous current generation (whose
// manifest pointer was corrupt and quarantined) were retired and physically
// deleted with no quarantine and no segment-level forensic residue,
// contradicting IDX-TXN-004 ("Corrupt data MUST be quarantined") and the
// transient-failure handling the loop already had (round-20 bug 5 kept only
// the transient branch). The integrity branch must behave like the transient
// branch for the sweep: keep the candidate referenced, quarantine the corrupt
// segment itself (aligned with the current-generation path), and never let
// the sweep retire generations newer than every pointer the store still
// knows — a corrupt manifest must not destroy the healthy data it pointed at.
func TestRecoveryFallbackIntegrityCandidateKeptAndQuarantined(t *testing.T) {
	root := t.TempDir()
	s1, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	commitPinGeneration(t, s1, []byte("gen1 payload one"), []byte("gen1 payload two"))
	commitPinGeneration(t, s1, []byte("gen2 payload")) // newest = current behind the manifest

	seg1 := rootSegmentFilesFor(t, root, 1)
	if len(seg1) != 2 {
		t.Fatalf("gen1 must have two segments, got %v", seg1)
	}
	seg2 := rootSegmentFilesFor(t, root, 2)
	if len(seg2) != 1 {
		t.Fatalf("gen2 must have one segment, got %v", seg2)
	}

	// Corrupt the manifest (the gen2 pointer is quarantined with it), then
	// flip one payload byte in one gen1 segment so the fallback candidate
	// fails integrity — not transient I/O.
	if err := os.WriteFile(filepath.Join(root, fileManifest), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	corruptPayloadByte(t, filepath.Join(root, segFileName(seg1[0])))

	s2, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatalf("corrupt manifest + corrupt history candidate must degrade, not fail the open: %v", err)
	}
	if s2.current != nil {
		t.Fatalf("no generation is usable; current = %+v", s2.current)
	}

	// The corrupt segment must leave forensic residue: moved into quarantine
	// and listed exactly once (TXN-004, same treatment as the current
	// generation's integrity failures).
	if got := s2.Quarantined(); len(got) != 1 || got[0] != seg1[0] {
		t.Fatalf("corrupt history-candidate segment must be quarantined exactly once, got %v (want [%s])", got, seg1[0])
	}
	if _, err := os.Stat(filepath.Join(root, dirQuarantine, segFileName(seg1[0]))); err != nil {
		t.Fatalf("corrupt segment must be moved into quarantine as forensic residue: %v", err)
	}

	// The healthy sibling stays referenced through the kept candidate, and the
	// previous current generation's segment must survive: its generation is
	// newer than every pointer the store still knows, so the sweep must not
	// retire it.
	for _, id := range []SegmentID{seg1[1], seg2[0]} {
		if _, err := os.Stat(filepath.Join(root, segFileName(id))); err != nil {
			t.Fatalf("segment %s was destroyed by the orphan sweep: %v", id, err)
		}
	}
	if len(s2.history) != 1 || s2.history[0].ID != 1 {
		t.Fatalf("integrity-failed candidate must stay in the in-memory history, got %+v", s2.history)
	}

	// Idempotent reopen: the same degraded state, no duplicate quarantine, no
	// deferred destruction of the preserved segments.
	s3, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s3.Quarantined(); len(got) != 1 || got[0] != seg1[0] {
		t.Fatalf("quarantine list must not duplicate across opens, got %v", got)
	}
	if s3.current != nil {
		t.Fatalf("reopen must not resurrect the corrupt candidate, current = %+v", s3.current)
	}
	for _, id := range []SegmentID{seg1[1], seg2[0]} {
		if _, err := os.Stat(filepath.Join(root, segFileName(id))); err != nil {
			t.Fatalf("segment %s disappeared on reopen: %v", id, err)
		}
	}
}
