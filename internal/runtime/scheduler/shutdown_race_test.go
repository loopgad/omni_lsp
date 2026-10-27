package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

type schedulerPickBarrierContext struct {
	context.Context
	entered chan struct{}
	release <-chan struct{}
	calls   atomic.Int32
}

func (c *schedulerPickBarrierContext) Done() <-chan struct{} {
	// pickRequest first probes cancellation with a nonblocking select, then
	// calls Done again while evaluating its blocking wait select. Hold that
	// second call so the test can make notification and shutdown readiness
	// deterministic before the worker enters the wait.
	if c.calls.Add(1) == 2 {
		close(c.entered)
		select {
		case <-c.release:
		case <-time.After(time.Second):
		}
	}
	return c.Context.Done()
}

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
					select {
					case <-req.Result: // terminal outcome guaranteed
					case <-time.After(time.Second):
						t.Error("admitted request did not receive a terminal outcome")
						return
					}
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

// TestScheduler_ShutdownLinearizesAfterQueueSenders verifies that Shutdown
// cannot publish the closed state while a queue sender holds the shared lock
// spanning its closed check and send.
func TestScheduler_ShutdownLinearizesAfterQueueSenders(t *testing.T) {
	s := New(DefaultConfig())
	s.queueMu.RLock()
	readLocked := true
	shutdownDone := make(chan struct{})
	defer func() {
		if readLocked {
			s.queueMu.RUnlock()
		}
		select {
		case <-shutdownDone:
		case <-time.After(time.Second):
			t.Error("Shutdown did not finish after releasing the queue read lock")
		}
	}()

	go func() {
		s.Shutdown()
		close(shutdownDone)
	}()

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for s.queueMu.TryRLock() {
		s.queueMu.RUnlock()
		select {
		case <-shutdownDone:
			t.Fatal("Shutdown returned while a queue reader still held the lock")
		case <-deadline.C:
			t.Fatal("Shutdown did not reach the queue write lock")
		case <-ticker.C:
		}
	}

	if s.closed.Load() {
		t.Fatal("Shutdown marked scheduler closed before acquiring the queue write lock")
	}
	s.queueMu.RUnlock()
	readLocked = false

	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after queue senders released the lock")
	}
	if !s.closed.Load() {
		t.Error("Shutdown completed without marking the scheduler closed")
	}
}

// TestScheduler_ShutdownDrainsIdleWorkerQueue blocks an idle worker after its
// queue scan but before its wait select. Submit then makes both the shutdown
// context and notify ready, exercising the ambiguous select wakeup safely.
func TestScheduler_ShutdownDrainsIdleWorkerQueue(t *testing.T) {
	for _, consumeNotify := range []bool{false, true} {
		name := "both-wakeups-ready"
		if consumeNotify {
			name = "shutdown-selected"
		}
		t.Run(name, func(t *testing.T) {
			s := New(Config{MaxConcurrent: 1, MaxQueueSize: 2, MaxInFlight: 2})
			baseCtx, cancel := context.WithCancel(context.Background())
			s.cancel = cancel
			entered := make(chan struct{})
			releasePicker := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releasePicker) }) }
			workerCtx := &schedulerPickBarrierContext{
				Context: baseCtx,
				entered: entered,
				release: releasePicker,
			}
			workerDone := make(chan struct{})
			go func() {
				s.worker(workerCtx)
				close(workerDone)
			}()
			defer func() {
				release()
				s.Shutdown()
				select {
				case <-workerDone:
				case <-time.After(time.Second):
					t.Error("worker did not exit after shutdown")
				}
				cancel()
			}()

			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("idle worker did not reach its wait select")
			}

			req := &Request{RequestID: "idle-shutdown-queued", Priority: PriorityCompletion}
			if got := s.Submit(req); got != Admitted {
				t.Fatalf("Submit = %v, want Admitted", got)
			}
			if consumeNotify {
				select {
				case <-s.notify:
				case <-time.After(time.Second):
					t.Fatal("Submit did not signal the idle worker")
				}
			}

			shutdownDone := make(chan struct{})
			go func() {
				s.Shutdown()
				close(shutdownDone)
			}()
			select {
			case <-shutdownDone:
			case <-time.After(time.Second):
				t.Fatal("Shutdown did not complete")
			}
			release()

			select {
			case res := <-req.Result:
				if res.Err == nil {
					t.Error("queued request completed without a shutdown error")
				}
			case <-time.After(time.Second):
				t.Fatal("accepted queued request did not receive a terminal result")
			}
			select {
			case <-workerDone:
			case <-time.After(time.Second):
				t.Fatal("idle worker did not drain and exit")
			}
		})
	}
}

// TestScheduler_ShutdownDrainsMixedPriorityQueue guarantees that requests
// accepted at different priorities reach a terminal result when shutdown
// races with a worker finishing its current request.
func TestScheduler_ShutdownDrainsMixedPriorityQueue(t *testing.T) {
	s := New(Config{
		MaxConcurrent: 1,
		MaxQueueSize:  4,
		MaxInFlight:   4,
	})
	s.Start(context.Background())

	releaseBlocker := make(chan struct{})
	blockerStarted := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBlocker) }) }
	blocker := &Request{
		RequestID: "shutdown-blocker",
		Priority:  PriorityMaintenance,
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			close(blockerStarted)
			<-releaseBlocker
			return "blocker", nil
		},
	}
	if got := s.Submit(blocker); got != Admitted {
		t.Fatalf("submit blocker = %v, want Admitted", got)
	}
	defer release()
	select {
	case <-blockerStarted:
	case <-time.After(time.Second):
		t.Fatal("worker did not start blocker")
	}

	queued := []*Request{
		{
			RequestID: "shutdown-high",
			Priority:  PriorityCompletion,
		},
		{
			RequestID: "shutdown-low",
			Priority:  PriorityIndexing,
		},
	}
	for _, req := range queued {
		if got := s.Submit(req); got != Admitted {
			t.Fatalf("submit %s = %v, want Admitted", req.RequestID, got)
		}
	}

	s.Shutdown()
	release()

	for _, req := range append([]*Request{blocker}, queued...) {
		select {
		case <-req.Result:
		case <-time.After(time.Second):
			t.Errorf("request %s did not receive a terminal result after shutdown", req.RequestID)
		}
	}
}

// TestScheduler_BoostAfterShutdownIsNoOp replaces the pre-fix pin
// (TestScheduler_BoostNeverDropsRequests), which asserted that a post-
// Shutdown aging pass terminates an extracted stale request with a
// rejection. That pin captured the pseudo-rejection path the audit flags
// (F3): with the aging fence, a pass can no longer overlap the closed state
// at all, so post-shutdown boostStaleRequests is a no-op — it must not panic
// on the closed lanes and must not write a rejection behind the drain's
// back. The parked request's terminal outcome belongs to the draining worker
// (covered by the Shutdown drain tests in this file), never to aging.
func TestScheduler_BoostAfterShutdownIsNoOp(t *testing.T) {
	s := New(DefaultConfig())
	req := &Request{
		RequestID:  "boost-after-shutdown",
		Priority:   PriorityMaintenance,
		CostClass:  DefaultCostClass,               // as Submit would resolve it; aging runs per §F8 class lane
		EnqueuedAt: time.Now().Add(-1 * time.Hour), // definitely stale
		Result:     make(chan Result, 1),
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			return nil, nil
		},
	}
	// Park the request in its queue without a worker consuming it. Workers
	// are NOT started here: a running worker would race boostStaleRequests
	// for the same channel and the test would be flaky by construction.
	if !s.trySend(req.CostClass, int(req.Priority), req) {
		t.Fatal("setup: could not enqueue request")
	}
	s.Shutdown() // closed=true, lanes closed

	// Post-shutdown aging pass: fenced no-op.
	s.boostStaleRequests(context.Background())

	select {
	case res := <-req.Result:
		t.Errorf("post-shutdown aging touched the parked request: %+v", res)
	default:
	}
}

// TestScheduler_DrainScanFencesInFlightAgingPass pins the fence mechanism
// deterministically (audit finding 2): a request that an aging pass holds
// between lanes while the worker performs its final draining scan must not
// be stranded. The worker must wait for the pending pass and pick the
// re-queued request up, delivering a terminal result.
func TestScheduler_DrainScanFencesInFlightAgingPass(t *testing.T) {
	s := New(Config{MaxConcurrent: 1, MaxQueueSize: 4, PriorityAgingMs: 5000})
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	req := &Request{
		RequestID:  "fence-drain",
		Priority:   PriorityMaintenance,
		CostClass:  DefaultCostClass,
		EnqueuedAt: time.Now(),
		Result:     make(chan Result, 1), // trySend parking skips Submit's channel setup
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			return nil, nil
		},
	}
	// Park without notify so the worker stays idle until cancellation.
	if !s.trySend(req.CostClass, int(req.Priority), req) {
		t.Fatal("setup: could not enqueue request")
	}

	// Simulate an aging pass in flight: registered, holding the fence, and
	// holding the extracted request between lanes.
	s.agingPending.Add(1)
	s.agingMu.Lock()
	extracted := <-s.queues[DefaultCostClass][int(PriorityMaintenance)]
	if extracted != req {
		t.Fatal("setup: extracted the wrong request")
	}

	cancel() // the worker must not finish its drain while the pass is pending

	// Give the worker time to (wrongly, pre-fix) finish its drain and exit.
	time.Sleep(50 * time.Millisecond)

	// Complete the simulated pass: closed is still false, so the requeue
	// succeeds — the request is back in a lane after the fence is released.
	if !s.trySend(req.CostClass, int(req.Priority), req) {
		s.agingMu.Unlock()
		s.agingPending.Add(-1)
		t.Fatal("setup: could not requeue the extracted request")
	}
	s.agingMu.Unlock()
	s.agingPending.Add(-1)

	select {
	case res := <-req.Result:
		if res.Err == nil {
			t.Error("re-queued request completed without a cancellation error")
		}
	case <-time.After(time.Second):
		t.Fatal("request re-queued by a pending aging pass was stranded: the draining worker exited without it")
	}
}
