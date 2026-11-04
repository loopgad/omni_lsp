package persistent

// Pins for round 23 OPT-4 (store half):
//   1. a view opened through OpenSnapshotLease retains its CRC-verified
//      payloads, so reads after open serve from memory and never touch disk
//      again — even if a segment file is corrupted after the lease is taken;
//      a view constructed WITHOUT the verification pass keeps the fail-closed
//      disk read (corruption detected);
//   2. the orphan sweep is throttled by the dirty flag: a store that opened
//      with nothing dirty does NOT sweep, and the sweep runs again once
//      something dirty (a commit) happens.

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

	// Plant an orphan segment file now, with the dirty flag clear.
	orphan := encodeSegment(999, []byte("orphan bytes"))
	orphanID := SegmentID("999-deadbeef")
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
