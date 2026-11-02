package persistent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Pin for round20/bugB bug 3: pruneHistoryGeneration — Compact's closing leg
// (compact.go) — acquired the cross-process writer lock with
// context.Background(), ignoring the caller's cancellation: a cancelled
// Compact could wait forever behind another process's reindex or long
// Commit, the same defect NewFileStore's unbounded construction had
// (复证圈 2 审计发现 2, fixed there by NewFileStoreContext). The caller's
// context must gate the lock acquisition.
func TestPruneHistoryGenerationHonoursCancellation(t *testing.T) {
	s := newStore(t)
	commitPinGeneration(t, s, []byte("one"))
	commitPinGeneration(t, s, []byte("two"))

	// Hold the writer lock the way another process's reindex would.
	holder, err := acquireWriterLock(context.Background(), s.root)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.pruneHistoryGeneration(ctx, 1) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("prune must surface the caller's cancellation, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pruneHistoryGeneration ignored a cancelled context while the writer lock was busy")
	}

	// And the normal path still works once the lock is free.
	holder.release()
	if err := s.pruneHistoryGeneration(context.Background(), 1); err != nil {
		t.Fatalf("prune with a live context must succeed: %v", err)
	}
}
