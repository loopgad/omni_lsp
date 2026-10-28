package persistent

import (
	"context"
	"fmt"
	"testing"
)

// TestCommitReadsStoreStateOnlyUnderStoreMutex pins the batch-4 audit finding:
// commitWithWriterLock used to re-read store.history/store.current without
// store.mu to assemble the next recovery history, relying solely on the
// implicit "every mutator holds the cross-process writer lock" convention.
// This test runs a store.mu-protected writer of those very fields
// concurrently (recover() mutates them exactly this way under store.mu) and
// requires the commit path to stay silent under -race: an unsynchronized read
// of store state on the commit side is reported as a data race.
func TestCommitReadsStoreStateOnlyUnderStoreMutex(t *testing.T) {
	s := newStore(t)
	publishOne(t, s, "committed-payload")
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.mu.Lock()
			// Write the current values back under store.mu: a no-op for the
			// store's content, but a genuine store.mu-protected write that
			// -race must not see racing with the commit path.
			current := s.current
			s.current = current
			history := s.history
			s.history = history
			s.mu.Unlock()
		}
	}()
	for i := 0; i < 8; i++ {
		build, err := s.BeginBuild(context.Background())
		if err != nil {
			close(stop)
			t.Fatal(err)
		}
		if _, err := build.WriteSegment([]byte(fmt.Sprintf("gen-%d", i))); err != nil {
			close(stop)
			t.Fatal(err)
		}
		if err := build.Commit(context.Background()); err != nil {
			close(stop)
			t.Fatal(err)
		}
	}
	close(stop)
	<-writerDone
	view, err := s.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatalf("open snapshot after concurrent commits: %v", err)
	}
	if len(view.Segments) != 1 {
		t.Fatalf("store lost its committed segment layout: %+v", view.Generation)
	}
	if _, err := view.ReadSegment(view.Segments[0].ID); err != nil {
		t.Fatalf("committed segment became unreadable: %v", err)
	}
}
