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
