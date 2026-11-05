package persistent

// Pins for round 23 OPT-4 (store half):
//   1. a view opened through OpenSnapshotLease retains its CRC-verified
//      payloads, so reads after open serve from memory and never touch disk
//      again — even if a segment file is corrupted after the lease is taken;
//      a view constructed WITHOUT the verification pass keeps the fail-closed
//      disk read (corruption detected);
//   2. the orphan sweep is throttled by the dirty flag: a store that opened
//      with nothing dirty does NOT sweep, and the sweep runs again once
//      something dirty (a commit) happens;
//   3. when the sweep DOES run, it never retires a root segment whose
//      generation is newer than every pointer the store still knows
//      (merged round-23 optB semantics, ebe8725): such a segment belongs to
//      a generation whose pointer was lost — most commonly the current
//      generation behind a manifest recover just quarantined as corrupt —
//      and its bytes may be entirely healthy. The pre-merge throttle pin
//      planted its orphan above every pointer precisely because such an
//      orphan used to be the canonical sweepable case; the merge made the
//      two semantics collide, so the throttle pin now plants its orphan
//      inside the sweepable range and pin 3 guards the protection itself.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func opt4PublishOne(t *testing.T, s *FileStore, payload string) SegmentID {
	t.Helper()
	build, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id, err := build.WriteSegment([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := build.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLeaseViewServesVerifiedPayloadsWithoutRereadingDisk(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	id := opt4PublishOne(t, s, "opt4 verified payload bytes")

	lease, err := s.OpenSnapshotLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	view := lease.Snapshot()
	original, err := view.ReadSegment(id)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}

	// Corrupt the on-disk segment file AFTER the lease was taken: the
	// lease-pinned view must serve the retained verified payload and must
	// not notice (no re-read on the read path).
	segPath := filepath.Join(root, segFileName(id))
	if err := os.WriteFile(segPath, []byte("corrupted after open"), 0o600); err != nil {
		t.Fatal(err)
	}
	reread, err := view.ReadSegment(id)
	if err != nil {
		t.Fatalf("read after on-disk corruption: %v (lease views must serve retained payloads)", err)
	}
	if string(reread) != string(original) {
		t.Fatal("lease view payload changed after on-disk corruption — cache miss")
	}

	// The uncached view (no verification pass) keeps the fail-closed disk
	// read: the corrupted file must be detected.
	uncached := GenerationView{Generation: view.Generation, dir: root}
	if _, err := uncached.ReadSegment(id); err == nil {
		t.Fatal("uncached view must fail-closed on the corrupted segment file")
	}
}

func TestOrphanSweepThrottledByDirtyFlag(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	// Construction swept once (seeded dirty) and cleared the flag. Commit a
	// generation — the commit itself marks dirty — then open once more: this
	// open sweeps (consuming the commit's mark) and leaves the flag clear.
	_ = opt4PublishOne(t, s, "committed generation")
	if _, err := s.OpenSnapshot(ctx); err != nil {
		t.Fatal(err)
	}

	// Plant an orphan segment file now, with the dirty flag clear. The fake
	// generation must stay within the sweepable range: after the round-23
	// optB merge (ebe8725) a root segment whose generation is newer than
	// every pointer the store still knows is preserved as possibly-healthy
	// data whose pointer was lost, so the throttle pin uses an unreferenced
	// sibling of the already-committed generation 1 instead.
	orphan := encodeSegment(1, []byte("orphan bytes"))
	orphanID := SegmentID("1-deadbeef")
	orphanPath := filepath.Join(root, segFileName(orphanID))
	if err := os.WriteFile(orphanPath, orphan, 0o600); err != nil {
		t.Fatal(err)
	}

	// Nothing dirty since the last sweep: the sweep is throttled away and the
	// orphan survives the open.
	if _, err := s.OpenSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("orphan swept although nothing was dirty (throttle must not sweep unmarked opens): %v", err)
	}

	// A commit marks dirty: the NEXT open sweeps and reclaims the orphan.
	_ = opt4PublishOne(t, s, "second committed generation")
	if _, err := s.OpenSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan not reclaimed after a dirty commit re-enabled the sweep: %v", err)
	}
}

// Pin 3 of the merged round-23 semantics (optB ebe8725): when the sweep does
// run, it must never retire a root segment whose generation is newer than
// every pointer the store still knows (current manifest, recovery history).
// A committed generation is pointed at by either the manifest or
// history.json, so a root segment above every known pointer belongs to a
// generation whose pointer was lost — most commonly the current generation
// behind a manifest this recover just quarantined as corrupt — and its bytes
// may be entirely healthy; destroying them would corrupt-on-recovery data
// the store served moments ago (IDX-TXN-004). The residue stays bounded: the
// reservation window moves past the lost generation once later pointers are
// published, and from then on the segment is ordinary sweepable orphan bytes
// again.
func TestOrphanSweepKeepsRootSegmentsAboveEveryPointer(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	_ = opt4PublishOne(t, s, "committed generation") // gen 1

	// Plant a root segment of a generation no pointer names (999 > 1) — the
	// exact shape the pre-merge throttle pin treated as the canonical orphan.
	lost := encodeSegment(999, []byte("possibly-healthy bytes of a lost generation"))
	lostID := SegmentID("999-deadbeef")
	lostPath := filepath.Join(root, segFileName(lostID))
	if err := os.WriteFile(lostPath, lost, 0o600); err != nil {
		t.Fatal(err)
	}

	// A commit marks dirty, so the NEXT open performs a real sweep — and even
	// that dirty-enabled sweep must leave the above-pointer segment alone.
	_ = opt4PublishOne(t, s, "second committed generation") // gen 2
	if _, err := s.OpenSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lostPath); err != nil {
		t.Fatalf("root segment above every known pointer was retired by the dirty-enabled sweep: %v", err)
	}

	// Bounded residue: crash-orphaned staging-1000 raises the reservation
	// window on the next recover, so a commit publishes generation 1001 and
	// the lost generation 999 falls below every pointer that now exists —
	// the next dirty-enabled sweep must reclaim it.
	stagingLost := filepath.Join(root, "staging-1000")
	if err := os.MkdirAll(stagingLost, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingLost, "seg-1000-cafebeef.bin"), encodeSegment(1000, []byte("staged bytes")), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	_ = opt4PublishOne(t, s2, "third committed generation") // gen 1001 (staging high water seeded the reservation)
	if _, err := s2.OpenSnapshot(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lostPath); !os.IsNotExist(err) {
		t.Fatalf("orphan of a lost generation must become sweepable once a newer pointer exists: %v", err)
	}
}
