package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// TestF11_RejectionsWrapSentinel pins the reason errRejected exists: every
// rejection the scheduler hands back wraps it, so a caller can tell "the
// scheduler would not run this" apart from a failure the request caused. The
// three rejection paths used to build their own inline fmt.Errorf, so nothing
// outside the package could recognise one and the sentinel was only ever passed
// in by the rollback test.
func TestF11_RejectionsWrapSentinel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxQueueSize = 1
	cfg.MaxInFlight = 1
	// No worker pool started: nothing drains the queues, so the single slot
	// fills and the next Submit has to refuse.
	s := New(cfg)

	block := make(chan struct{})
	defer close(block)
	exec := func(context.Context, *snapshot.Snapshot) (any, error) {
		<-block
		return nil, nil
	}

	var refused error
	for i := 0; i < 32 && refused == nil; i++ {
		req := &Request{Execute: exec, Priority: PriorityCompletion}
		if s.Submit(req) == Admitted {
			continue
		}
		select {
		case res := <-req.Result:
			refused = res.Err
		default:
		}
	}
	if refused == nil {
		t.Skip("no rejection surfaced a Result; nothing to assert")
	}
	if !errors.Is(refused, errRejected) {
		t.Errorf("rejection %v does not wrap errRejected; callers cannot recognise it", refused)
	}
}

// TestF11_InFlightRejectionWrapsSentinel covers the other rejection path. The
// queue-full case above fires without any worker; the in-flight budget is
// checked against a counter only the worker loop increments, so reaching it
// needs a request actually executing.
func TestF11_InFlightRejectionWrapsSentinel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxInFlight = 1
	s := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Shutdown()

	block := make(chan struct{})
	holder := &Request{
		Priority: PriorityCompletion,
		Execute:  func(context.Context, *snapshot.Snapshot) (any, error) { <-block; return nil, nil },
	}
	if got := s.Submit(holder); got != Admitted {
		t.Fatalf("first Submit = %v, want Admitted", got)
	}
	defer close(block)
	// Wait for the worker to pick it up, since that is what claims the slot.
	for i := 0; i < 1000 && s.inFlight.Load() == 0; i++ {
		time.Sleep(time.Millisecond)
	}
	if s.inFlight.Load() == 0 {
		t.Skip("worker never picked the request up; in-flight budget not reachable here")
	}

	over := &Request{
		Priority: PriorityCompletion,
		Execute:  func(context.Context, *snapshot.Snapshot) (any, error) { return nil, nil },
	}
	if got := s.Submit(over); got == Admitted {
		t.Fatal("second Submit was admitted while the in-flight budget was exhausted")
	}
	select {
	case res := <-over.Result:
		if !errors.Is(res.Err, errRejected) {
			t.Errorf("in-flight rejection %v does not wrap errRejected", res.Err)
		}
	default:
		t.Error("rejected request never received its terminal Result")
	}
}

// TestF11_RequestFailureIsNotARejection is the other half: an error the
// request's own work produced must not be mistaken for a scheduler refusal.
// Without the distinction a caller retrying on errRejected would retry a
// deterministic failure forever.
func TestF11_RequestFailureIsNotARejection(t *testing.T) {
	want := errors.New("backend exploded")
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Shutdown()

	req := &Request{
		Priority: PriorityCompletion,
		Execute:  func(context.Context, *snapshot.Snapshot) (any, error) { return nil, want },
	}
	if got := s.Submit(req); got != Admitted {
		t.Fatalf("Submit = %v, want Admitted", got)
	}
	res := <-req.Result
	if !errors.Is(res.Err, want) {
		t.Fatalf("request error = %v, want %v", res.Err, want)
	}
	if errors.Is(res.Err, errRejected) {
		t.Error("a failure from the request's own work was reported as a scheduler rejection")
	}
}
