package persistent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// swapSegmentWithDir replaces a segment file with a directory at the same
// path. os.ReadFile then fails with a filesystem-level error that is not
// IsNotExist on every supported platform (Windows: ERROR_INVALID_FUNCTION,
// POSIX: EISDIR) — the portable stand-in for an AV lock or sharing violation.
func swapSegmentWithDir(t *testing.T, root string, id SegmentID) []byte {
	t.Helper()
	p := filepath.Join(root, segFileName(id))
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return raw
}

func healSegmentFromDir(t *testing.T, root string, id SegmentID, raw []byte) {
	t.Helper()
	p := filepath.Join(root, segFileName(id))
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func manifestGenerationID(t *testing.T, root string) (uint64, bool) {
	t.Helper()
	mb, err := os.ReadFile(filepath.Join(root, fileManifest))
	if os.IsNotExist(err) {
		return 0, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var gen Generation
	if err := json.Unmarshal(mb, &gen); err != nil {
		t.Fatalf("manifest unparseable: %v", err)
	}
	return gen.ID, true
}

func inQuarantine(s *FileStore, id SegmentID) bool {
	for _, q := range s.Quarantined() {
		if q == id {
			return true
		}
	}
	return false
}

// TestL7_TransientReadErrorDoesNotQuarantineOrFallBack: a filesystem-level
// read failure must never quarantine a possibly healthy generation nor drop
// the manifest pointer (L7/L8 sanction isolation for integrity failures
// only). After the transient fault heals, the same generation must be served
// again — no silent fallback to an older one.
func TestL7_TransientReadErrorDoesNotQuarantineOrFallBack(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "good-generation")
	publishOne(t, s, "current-generation")

	view, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	currentID := view.ID
	currentSeg := view.Segments[0].ID
	wantPayload, _ := view.ReadSegment(currentSeg)

	raw := swapSegmentWithDir(t, s.root, currentSeg)

	_, err = s.OpenSnapshot(ctx)
	if err == nil {
		t.Fatal("persistent I/O error during open must fail the open, not silently degrade")
	}
	if !isSegmentIOError(err) {
		t.Fatalf("open error %v is not classified as transient segment I/O", err)
	}
	if q := s.Quarantined(); len(q) != 0 {
		t.Fatalf("I/O error quarantined segments %v — only integrity failures may quarantine (L7)", q)
	}
	if genID, ok := manifestGenerationID(t, s.root); !ok || genID != currentID {
		t.Fatalf("manifest pointer dropped or moved by I/O error: id=%d present=%t, want %d", genID, ok, currentID)
	}

	// The transient fault clears: the very same generation comes back.
	healSegmentFromDir(t, s.root, currentSeg, raw)
	view2, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("open after fault healed: %v", err)
	}
	if view2.ID != currentID {
		t.Fatalf("after heal opened generation %d, want the untouched generation %d", view2.ID, currentID)
	}
	got, err := view2.ReadSegment(currentSeg)
	if err != nil || string(got) != string(wantPayload) {
		t.Fatalf("after heal payload = %q, %v; want %q", got, err, wantPayload)
	}
}

// TestL7_MissingReferencedSegmentIsIntegrityAndQuarantines pins the other
// half of the classification: a manifest-referenced segment file that is gone
// is corruption, and must still quarantine and fall back.
func TestL7_MissingReferencedSegmentIsIntegrityAndQuarantines(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "good-generation")
	publishOne(t, s, "to-be-missing")

	view, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	missingSeg := view.Segments[0].ID
	if err := os.Remove(filepath.Join(s.root, segFileName(missingSeg))); err != nil {
		t.Fatal(err)
	}

	view2, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("missing referenced segment must quarantine-and-fall-back, got: %v", err)
	}
	if !inQuarantine(s, missingSeg) {
		t.Errorf("missing segment %s not recorded in quarantine list: %v", missingSeg, s.Quarantined())
	}
	if view2.ID >= view.ID {
		t.Errorf("did not fall back to an older generation: opened %d after corrupting %d", view2.ID, view.ID)
	}
	if _, ok := manifestGenerationID(t, s.root); ok {
		t.Errorf("manifest still points at the quarantined generation")
	}
}

// TestRecoverFallbackSkipsIOErrorCandidateNonDestructively: when the newest
// history candidate cannot be read due to transient I/O, recover skips it for
// this open but leaves the on-disk history untouched, so the candidate is
// reconsidered (and wins again) once the fault clears.
func TestRecoverFallbackSkipsIOErrorCandidateNonDestructively(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "oldest")
	publishOne(t, s, "middle")
	publishOne(t, s, "newest")

	view, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newestID := view.ID

	// Force the recovery fallback path: manifest gone, history intact.
	// The manifest holds the newest generation; the history chain below it
	// starts at the newest fallback candidate.
	if err := os.Remove(s.manifestPath()); err != nil {
		t.Fatal(err)
	}
	hb, err := os.ReadFile(s.historyPath())
	if err != nil {
		t.Fatal(err)
	}
	var hist []*Generation
	if err := json.Unmarshal(hb, &hist); err != nil {
		t.Fatal(err)
	}
	if len(hist) < 2 || hist[0].ID != newestID-1 || hist[1].ID != newestID-2 {
		t.Fatalf("history = %v, want [%d %d]", hist, newestID-1, newestID-2)
	}
	candidateID := hist[0].ID
	olderID := hist[1].ID
	candidateSeg := hist[0].Segments[0].ID
	raw := swapSegmentWithDir(t, s.root, candidateSeg)

	view2, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("fallback past an I/O-erroring candidate failed: %v", err)
	}
	if view2.ID != olderID {
		t.Fatalf("opened generation %d; expected the next candidate %d after skipping %d", view2.ID, olderID, candidateID)
	}
	if inQuarantine(s, candidateSeg) {
		t.Errorf("recover quarantined the I/O-erroring candidate — recover never quarantines")
	}
	// On-disk history still carries the candidate.
	hb2, err := os.ReadFile(s.historyPath())
	if err != nil {
		t.Fatal(err)
	}
	var hist2 []*Generation
	if err := json.Unmarshal(hb2, &hist2); err != nil {
		t.Fatal(err)
	}
	if len(hist2) == 0 || hist2[0].ID != candidateID {
		t.Fatalf("on-disk history lost the skipped candidate: head %v, want %d", hist2, candidateID)
	}

	// Fault clears: the skipped candidate is reconsidered and served again.
	healSegmentFromDir(t, s.root, candidateSeg, raw)
	view3, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("open after fault healed: %v", err)
	}
	if view3.ID != candidateID {
		t.Fatalf("after heal opened generation %d, want %d (skipped candidate must be reconsidered)", view3.ID, candidateID)
	}
}
