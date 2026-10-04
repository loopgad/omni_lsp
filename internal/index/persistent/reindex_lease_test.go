package persistent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReindexLeaseSerializesBuildsWithoutHoldingWriterLock(t *testing.T) {
	store := newStore(t)
	first, err := store.AcquireReindexLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	// A separate rebuild waits for the whole-build lease and honors cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if second, err := store.AcquireReindexLease(ctx); !errors.Is(err, context.DeadlineExceeded) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("concurrent rebuild lease = %v, want deadline exceeded", err)
	}

	// Segment mutations still use the independent short-lived writer lock.
	build, err := store.BeginBuild(context.Background())
	if err != nil {
		t.Fatalf("build while holding reindex lease: %v", err)
	}
	if _, err := build.WriteSegment([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := build.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := store.AcquireReindexLease(context.Background())
	if err != nil {
		t.Fatalf("reindex lease remained held after close: %v", err)
	}
	_ = third.Close()
}
