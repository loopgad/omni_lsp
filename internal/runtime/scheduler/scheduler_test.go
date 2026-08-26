package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

func TestSchedulerBasic(t *testing.T) {
	s := New(DefaultConfig())
	ctx := context.Background()
	s.Start(ctx)
	defer s.Shutdown()

	req := &Request{
		RequestID:  "test-1",
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			return "result", nil
		},
	}

	result := s.Submit(req)
	if result != Admitted {
		t.Fatalf("Submit = %v, want Admitted", result)
	}
	t.Logf("queue len after submit: %d, in-flight: %d", s.QueueLen(), s.Stats().InFlight)

	select {
	case res := <-req.Result:
		t.Logf("got result: value=%v err=%v", res.Value, res.Err)
		if res.Err != nil {
			t.Fatalf("Execute error: %v", res.Err)
		}
		if res.Value != "result" {
			t.Errorf("Value = %v, want result", res.Value)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for result. stats: %+v", s.Stats())
	}
}

func TestSchedulerPriorityOrder(t *testing.T) {
	s := New(Config{
		MaxConcurrent: 1,
		MaxQueueSize:  10,
		MaxInFlight:   10,
	})
	ctx := context.Background()
	s.Start(ctx)
	defer s.Shutdown()

	// Submit a long-running low-priority request to occupy the single worker.
	low := &Request{
		RequestID:  "low",
		Priority:   PriorityIndexing,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			time.Sleep(100 * time.Millisecond)
			return "low", nil
		},
	}
	s.Submit(low)
	time.Sleep(10 * time.Millisecond) // let low start executing

	// Submit a high-priority request; it should execute after low finishes
	// (single worker, but high priority is picked first when low completes).
	high := &Request{
		RequestID:  "high",
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			return "high", nil
		},
	}
	result := s.Submit(high)
	if result != Admitted {
		t.Fatalf("Submit high = %v, want Admitted", result)
	}

	select {
	case res := <-high.Result:
		if res.Err != nil {
			t.Fatalf("High priority error: %v", res.Err)
		}
		if res.Value != "high" {
			t.Errorf("Value = %v, want high", res.Value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for high-priority result")
	}
}

func TestSchedulerShutdown(t *testing.T) {
	s := New(DefaultConfig())
	ctx := context.Background()
	s.Start(ctx)

	s.Shutdown()

	result := s.Submit(&Request{
		RequestID:  "after-shutdown",
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute:    func(ctx context.Context, snap *snapshot.Snapshot) (any, error) { return nil, nil },
	})
	if result != RejectedCancelled {
		t.Errorf("submit after shutdown = %v, want RejectedCancelled", result)
	}
}

func TestSchedulerQueueLen(t *testing.T) {
	s := New(DefaultConfig())
	if s.QueueLen() != 0 {
		t.Errorf("initial queue length = %d, want 0", s.QueueLen())
	}
}

func TestSchedulerAdmissionRejection(t *testing.T) {
	done := make(chan struct{})
	s := New(Config{
		MaxConcurrent: 1,
		MaxQueueSize:  2,
		MaxInFlight:   1,
	})
	ctx := context.Background()
	s.Start(ctx)
	defer s.Shutdown()

	// Submit first request -  it occupies the single in-flight slot.
	req0 := &Request{
		RequestID:  identity.RequestID("req-0"),
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			<-done // block until test signals
			return nil, nil
		},
	}
	if s.Submit(req0) != Admitted {
		t.Fatal("submit 0 should be admitted")
	}
	time.Sleep(50 * time.Millisecond) // let worker pick it up

	// Queue now has 0 items, in-flight = 1 (full). Submit 2 more.
	for i := 1; i <= 2; i++ {
		req := &Request{
			RequestID:  identity.RequestID(fmt.Sprintf("req-%d", i)),
			Priority:   PriorityCompletion,
			EnqueuedAt: time.Now(),
			Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
				return nil, nil
			},
		}
		result := s.Submit(req)
		if result != RejectedQueueFull {
			t.Errorf("submit %d = %v, want RejectedQueueFull", i, result)
		}
	}
	close(done) // unblock the first request so Shutdown can complete
}

func TestSchedulerContextCancellation(t *testing.T) {
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	s.Start(ctx)

	// Cancel the scheduler context AND shutdown.
	cancel()
	s.Shutdown()
	time.Sleep(50 * time.Millisecond)

	// Submit should be rejected because scheduler is closed.
	req := &Request{
		RequestID:  "cancelled",
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute:    func(ctx context.Context, snap *snapshot.Snapshot) (any, error) { return nil, nil },
	}
	result := s.Submit(req)
	if result != RejectedCancelled {
		t.Errorf("submit after cancel = %v, want RejectedCancelled", result)
	}
}

func TestSchedulerStats(t *testing.T) {
	s := New(Config{
		MaxConcurrent: 2,
		MaxQueueSize:  10,
		MaxInFlight:   10,
	})
	ctx := context.Background()
	s.Start(ctx)
	defer s.Shutdown()

	initial := s.Stats()
	if initial.TotalEnqueued != 0 {
		t.Errorf("initial TotalEnqueued = %d, want 0", initial.TotalEnqueued)
	}

	req := &Request{
		RequestID:  "stats-test",
		Priority:   PriorityHover,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			return "ok", nil
		},
	}
	s.Submit(req)
	<-req.Result

	stats := s.Stats()
	if stats.TotalExecuted != 1 {
		t.Errorf("TotalExecuted = %d, want 1", stats.TotalExecuted)
	}
}

func TestSchedulerMultiplePriorities(t *testing.T) {
	s := New(Config{
		MaxConcurrent: 1,
		MaxQueueSize:  20,
		MaxInFlight:   20,
	})
	ctx := context.Background()
	s.Start(ctx)
	defer s.Shutdown()

	// Occupy the worker with a long P4 request.
	blocker := &Request{
		RequestID:  "blocker",
		Priority:   PriorityIndexing,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			time.Sleep(200 * time.Millisecond)
			return nil, nil
		},
	}
	s.Submit(blocker)
	time.Sleep(10 * time.Millisecond)

	// P0 and P2 requests should both be admitted.
	for _, prio := range []Priority{PriorityCompletion, PriorityDiagnostics} {
		req := &Request{
			RequestID:  identity.RequestID(prio.String()),
			Priority:   prio,
			EnqueuedAt: time.Now(),
			Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
				return prio.String(), nil
			},
		}
		if s.Submit(req) != Admitted {
			t.Errorf("submit %v should be admitted", prio)
		}
		go func(r *Request) {
			res := <-r.Result
			if res.Err != nil {
				t.Errorf("request %s error: %v", r.RequestID, res.Err)
			}
		}(req)
	}

	// Wait for all pending requests to complete.
	// Use a timeout based on the blocker duration + margin.
	time.Sleep(300 * time.Millisecond)

	// Verify no errors in results by checking stats.
	stats := s.Stats()
	if stats.TotalExecuted < 3 {
		t.Errorf("expected at least 3 executions, got %d", stats.TotalExecuted)
	}
}
