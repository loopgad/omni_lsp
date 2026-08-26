package scheduler

// Tests for goal.md §C7/§F11: cancellation must reach queued and running work,
// shared or finished requests must be unaffected.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

func newTestSched(maxConcurrent int) (*Scheduler, context.CancelFunc) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = maxConcurrent
	s := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)
	return s, cancel
}

// TestC7_CancelWhileQueuedSkipsExecution verifies a cancelled request that has
// not started is never executed (exactly one terminal outcome).
func TestC7_CancelWhileQueuedSkipsExecution(t *testing.T) {
	s, stop := newTestSched(1)
	defer stop()

	release := make(chan struct{})
	blockerStarted := make(chan struct{})
	blocker := &Request{
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
			close(blockerStarted)
			<-release
			return nil, nil
		},
	}
	if res := s.Submit(blocker); res != Admitted {
		t.Fatalf("blocker admission: %v", res)
	}
	<-blockerStarted

	victimExecuted := atomic.Bool{}
	victim := &Request{
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
			victimExecuted.Store(true)
			return nil, nil
		},
	}
	if res := s.Submit(victim); res != Admitted {
		t.Fatalf("victim admission: %v", res)
	}
	victim.Cancel()
	close(release)

	select {
	case res := <-victim.Result:
		if res.Err == nil {
			t.Error("cancelled queued request must terminate with an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no terminal outcome for cancelled queued request")
	}
	if victimExecuted.Load() {
		t.Error("cancelled queued request must not execute")
	}
}

// TestC7_CancelWhileRunningStopsWork verifies context cancellation propagates.
func TestC7_CancelWhileRunningStopsWork(t *testing.T) {
	s, stop := newTestSched(1)
	defer stop()

	started := make(chan struct{})
	req := &Request{
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
			close(started)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Second):
				return "finished", nil
			}
		},
	}
	if res := s.Submit(req); res != Admitted {
		t.Fatalf("admission: %v", res)
	}
	<-started
	req.Cancel()

	select {
	case res := <-req.Result:
		if res.Err != context.Canceled {
			t.Errorf("Err = %v, want context.Canceled", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("running request ignored cancellation")
	}
}

// TestF11_CancelOneWaiterDoesNotAffectFinishedResult verifies Cancel after
// completion does not corrupt the delivered result.
func TestF11_CancelAfterCompletionKeepsResult(t *testing.T) {
	s, stop := newTestSched(1)
	defer stop()

	req := &Request{
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
			return "value", nil
		},
	}
	if res := s.Submit(req); res != Admitted {
		t.Fatalf("admission: %v", res)
	}
	res := <-req.Result
	req.Cancel() // late cancel
	if res.Err != nil || res.Value != "value" {
		t.Errorf("completed result corrupted by late cancel: %+v", res)
	}
}
