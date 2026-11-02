package persistent

import (
	"context"
	"os"
	"testing"
)

// Pin for round20/bugB bug 4: generation lease lock files
// (leases/generation-<id>.lock) were created on every OpenSnapshotLease and
// never deleted anywhere in the codebase — every generation ever leased left
// a lock file in the store root forever, accumulating without bound. When a
// generation is physically retired (segments gone, no shared lease), its lock
// file is dead weight and must go with it; lease files of live (referenced)
// generations are untouched.
func TestRetiredGenerationLeaseLockFileIsRemoved(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStore(root, Config{KeepGenerations: 2})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	commitPinGeneration(t, s, []byte("gen1"))
	lease, err := s.OpenSnapshotLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := lease.Snapshot().ID; got != 1 {
		t.Fatalf("leased generation %d, want 1", got)
	}
	if _, err := os.Stat(generationLockPath(root, 1)); err != nil {
		t.Fatalf("leased generation must have a lock file: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}

	// gen4's commit pushes gen1 out of the KeepGenerations=2 history window
	// and physically deletes its segments — the lock file must follow.
	commitPinGeneration(t, s, []byte("gen2"))
	commitPinGeneration(t, s, []byte("gen3"))
	commitPinGeneration(t, s, []byte("gen4"))

	if _, err := os.Stat(generationLockPath(root, 1)); !os.IsNotExist(err) {
		t.Fatalf("retired generation 1 lease lock file must be removed, stat err: %v", err)
	}

	// Lease files of live generations are untouched: the current generation
	// can still be leased after the retirement sweep.
	lease2, err := s.OpenSnapshotLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	current := lease2.Snapshot().ID
	if _, err := os.Stat(generationLockPath(root, current)); err != nil {
		t.Fatalf("current generation %d lease lock file must exist: %v", current, err)
	}
	if err := lease2.Close(); err != nil {
		t.Fatal(err)
	}
}
