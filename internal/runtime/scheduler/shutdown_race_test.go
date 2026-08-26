package scheduler

import (
	"context"

	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"sync"
	"testing"
	"time"
)

// TestScheduler_ShutdownNoPanicOnConcurrentSubmit locks the shutdown-safety
// invariant: Submit / agingLoop racing Shutdown must never panic on
// send-to-closed-queue, and every admitted request keeps a terminal outcome.
func TestScheduler_ShutdownNoPanicOnConcurrentSubmit(t *testing.T) {
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				req := &Request{
					RequestID: "shutdown-race",
					Priority:  PriorityCompletion,
					Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
						return nil, nil
					},
				}
				switch s.Submit(req) {
				case Admitted:
					<-req.Result // terminal outcome guaranteed
				case RejectedQueueFull:
					// Contract: only Admitted results are readable.
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	s.Shutdown()
	close(stop)
	wg.Wait()

	// Post-shutdown submissions are cleanly rejected.
	if got := s.Submit(&Request{RequestID: "late", Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) { return nil, nil }}); got == Admitted {
		t.Error("Submit after Shutdown returned Admitted")
	}
}

// TestScheduler_BoostNeverDropsRequests locks the anti-vanishing fix: when
// queues refuse re-queueing (here: post-Shutdown closed state), the stale
// request's Result receives an immediate rejection instead of silence.
func TestScheduler_BoostNeverDropsRequests(t *testing.T) {
	s := New(DefaultConfig())
	if s.config.PriorityAgingMs <= 0 {
		s.config.PriorityAgingMs = 10
	}
	req := &Request{
		RequestID:  "boost-drop",
		Priority:   PriorityMaintenance,
		EnqueuedAt: time.Now().Add(-1 * time.Hour), // definitely stale
		Result:     make(chan Result, 1),
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			return nil, nil
		},
	}
	// Park the request in its queue without a worker consuming it. Workers
	// are NOT started here: a running worker would race boostStaleRequests
	// for the same channel and the test would be flaky by construction.
	if !s.trySend(int(req.Priority), req) {
		t.Fatal("setup: could not enqueue request")
	}
	s.Shutdown() // closed=true → every trySend now refuses

	// Post-shutdown aging pass: the popped stale request can go nowhere.
	s.boostStaleRequests()

	select {
	case res := <-req.Result:
		if res.Err == nil {
			t.Error("request terminated with neither value nor error")
		}
	default:
		t.Error("stale request has no terminal outcome after boostStaleRequests")
	}
}
