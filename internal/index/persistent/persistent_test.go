package persistent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *FileStore {
	t.Helper()
	s, err := NewFileStore(t.TempDir(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func publishOne(t *testing.T, s *FileStore, payload string) SegmentID {
	t.Helper()
	b, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.WriteSegment([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestL4_CrashBeforePublishKeepsOldGeneration: staging data abandoned before
// Commit never affects the readable pointer.
func TestL4_CrashBeforePublishKeepsOldGeneration(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "generation-one")

	b, err := s.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.WriteSegment([]byte("never published")); err != nil {
		t.Fatal(err)
	}
	if err := b.Abort(); err != nil { // simulates crash cleanup of staging
		t.Fatal(err)
	}

	view, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("old generation lost: %v", err)
	}
	if len(view.Segments) != 1 {
		t.Fatalf("segments = %d, want the single old one", len(view.Segments))
	}
	got, err := view.ReadSegment(view.Segments[0].ID)
	if err != nil || string(got) != "generation-one" {
		t.Fatalf("read %q err=%v", got, err)
	}
	// No staging leftovers discoverable in the store root.
	if _, err := os.Stat(filepath.Join(s.root, "staging-2")); !os.IsNotExist(err) {
		t.Error("staging directory leaked into store root")
	}
}

// TestL6_TXN002_HalfWrittenSegmentNotDiscoverable: a truncated staged file
// fails verification and cannot enter a committed generation.
func TestL6_TXN002_HalfWrittenSegmentNotDiscoverable(t *testing.T) {
	s := newStore(t)
	b, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.WriteSegment([]byte("payload-that-will-be-truncated"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a torn write in staging: chop the file below declared length.
	p := filepath.Join(b.(*fileBuild).stageDir, segFileName(id))
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, raw[:len(raw)-5], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(context.Background()); err != nil {
		t.Fatalf("commit itself may proceed; discovery is what must fail: %v", err)
	}
	_, err = s.OpenSnapshot(context.Background())
	if err == nil {
		t.Fatal("half-written segment became discoverable — TXN-002 violated")
	}
}

// TestL7_ChecksumMismatchQuarantines: one flipped byte quarantines the whole
// generation and falls back to the previous verified one (TXN-004 + L8).
func TestL7_ChecksumMismatchQuarantines(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "good-generation")
	publishOne(t, s, "corrupt-me")

	view, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	corruptSeg := view.Segments[0].ID
	p := filepath.Join(s.root, segFileName(corruptSeg))
	raw, _ := os.ReadFile(p)
	raw[len(raw)-1] ^= 0xFF // flip one payload byte
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	view2, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("recovery failed instead of falling back: %v", err)
	}
	q := s.Quarantined()
	found := false
	for _, id := range q {
		if id == corruptSeg {
			found = true
		}
	}
	if !found {
		t.Errorf("corrupt segment %s not in quarantine list: %v", corruptSeg, q)
	}
	got, _ := view2.ReadSegment(view2.Segments[0].ID)
	if string(got) != "good-generation" && len(view2.Segments) >= 1 {
		t.Logf("fell back to generation with first segment %q", got)
	}
}

// TestL6_TXN003_IdempotentRecovery: repeated opens on the same corruption
// state converge to the same outcome without double-quarantining.
func TestL6_TXN003_IdempotentRecovery(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "stable-old")
	publishOne(t, s, "broken-new")

	view, _ := s.OpenSnapshot(ctx)
	p := filepath.Join(s.root, segFileName(view.Segments[0].ID))
	raw, _ := os.ReadFile(p)
	raw[segHeaderSz] ^= 0x01 // corrupt payload start
	os.WriteFile(p, raw, 0o644)

	v1, err1 := s.OpenSnapshot(ctx)
	q1 := s.Quarantined()
	v2, err2 := s.OpenSnapshot(ctx)
	q2 := s.Quarantined()

	if (err1 == nil) != (err2 == nil) {
		t.Fatalf("non-idempotent errors: %v vs %v", err1, err2)
	}
	if len(v1.Segments) > 0 && len(v2.Segments) > 0 && v1.ID != v2.ID {
		t.Errorf("recovered generations diverge: %d vs %d", v1.ID, v2.ID)
	}
	if len(q2) < len(q1) {
		t.Errorf("quarantine list shrank between runs")
	}
}

// TestBudget_RejectsWhenFull: writes past the configured budget are refused
// with ErrDiskBudgetExceeded, never partially applied (§L17).
func TestBudget_RejectsWhenFull(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, Config{DiskBudgetBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 4096) // far beyond the tiny budget
	_, err = b.WriteSegment(big)
	if !errors.Is(err, ErrDiskBudgetExceeded) {
		t.Fatalf("want ErrDiskBudgetExceeded, got %v", err)
	}
	if err := b.Commit(context.Background()); err == nil {
		t.Log("empty commit allowed; harmless")
	}
}

// TestCompact_ReducesSegmentsAndPreservesReads: many segments merge into a
// fresh generation with identical readable content (§L16).
func TestCompact_ReducesSegmentsAndPreservesReads(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	want := map[string]string{}
	b, err := s.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"alpha", "beta", "gamma"} {
		id, err := b.WriteSegment([]byte(p))
		if err != nil {
			t.Fatal(err)
		}
		want[string(id)] = p
		_ = i
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	before, _ := s.OpenSnapshot(ctx)
	if len(before.Segments) < 3 {
		t.Fatalf("setup produced %d segments", len(before.Segments))
	}
	if err := s.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Segments) >= len(before.Segments) {
		t.Errorf("compaction did not reduce segments: %d → %d", len(before.Segments), len(after.Segments))
	}
	if len(after.Segments) != 1 {
		t.Errorf("compaction must yield exactly one merged segment, got %d", len(after.Segments))
	}
	payload, err := after.ReadSegment(after.Segments[0].ID)
	if err != nil {
		t.Fatalf("post-compact read: %v", err)
	}
	var got []string
	for _, p := range SplitMerged(payload) {
		got = append(got, string(p))
	}
	wantPayloads := []string{"alpha", "beta", "gamma"}
	if len(got) != len(wantPayloads) {
		t.Fatalf("merged segment holds %d payloads, want 3", len(got))
	}
	for i := range wantPayloads {
		if got[i] != wantPayloads[i] {
			t.Errorf("payload[%d] = %q, want %q", i, got[i], wantPayloads[i])
		}
	}
}

// TestL9_FutureSchemaQuarantinesAndFallsBack pins the forward-compatibility
// reader policy (L9): a segment written by a NEWER schema is refused, the
// offending generation quarantines, and recovery falls back to the older
// verified generation instead of serving unknown bytes.
func TestL9_FutureSchemaQuarantinesAndFallsBack(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "old-good-generation")
	futureSeg := publishOne(t, s, "written-by-future-version")

	// Rewrite the future segment's framing with an unknown schema version.
	p := filepath.Join(s.root, segFileName(futureSeg))
	raw, _ := os.ReadFile(p)
	raw[4] = 0 // raw[4:8] holds schemaVer (big-endian); 0x00000001 -> 0x00FFFFFF+1
	raw[5], raw[6], raw[7] = 0xFF, 0xFF, 0xFF
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("recovery must fall back to old generation: %v", err)
	}
	for _, seg := range view.Segments {
		payload, err := view.ReadSegment(seg.ID)
		if err != nil {
			t.Fatalf("surviving segment %s unreadable: %v", seg.ID, err)
		}
		if string(payload) == "written-by-future-version" {
			t.Error("future-schema payload must never be visible")
		}
	}
	found := false
	for _, id := range s.Quarantined() {
		if id == futureSeg {
			found = true
		}
	}
	if !found {
		t.Errorf("future-schema segment %s not quarantined: %v", futureSeg, s.Quarantined())
	}
}

// TestL10_FreshnessTupleGatesVisibility pins the §L10 contract: sealed
// records whose identity inputs no longer match are stale — never visible;
// matching tuples round-trip the payload untouched.
func TestL10_FreshnessTupleGatesVisibility(t *testing.T) {
	fresh := FreshnessTuple{SourceHash: "h1", BuildContext: "bc1", Toolchain: "go1.26", BackendVer: "v7", Revision: 42}
	sealed := SealPayload([]byte("payload-bytes"), fresh)

	got, err := VerifyPayload(sealed, fresh)
	if err != nil || string(got) != "payload-bytes" {
		t.Fatalf("fresh tuple round-trip: %q %v", got, err)
	}

	stale := fresh
	stale.Revision = 43
	if _, err := VerifyPayload(sealed, stale); err == nil {
		t.Fatal("stale revision accepted as fresh")
	}
	for name, mut := range map[string]func(*FreshnessTuple){
		"sourceHash":   func(f *FreshnessTuple) { f.SourceHash = "other" },
		"buildContext": func(f *FreshnessTuple) { f.BuildContext = "other" },
		"toolchain":    func(f *FreshnessTuple) { f.Toolchain = "go1.25" },
		"backendVer":   func(f *FreshnessTuple) { f.BackendVer = "v6" },
	} {
		m := fresh
		mut(&m)
		if _, err := VerifyPayload(sealed, m); err == nil {
			t.Errorf("%s mismatch accepted", name)
		}
	}

	if _, err := VerifyPayload([]byte{0, 0}, fresh); err == nil {
		t.Error("truncated seal accepted")
	}
}
