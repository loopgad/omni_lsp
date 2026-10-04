package scheduler

// Tests for F11 single-flight joining: tracker semantics plus end-to-end
// Submit integration (identical CoalesceKey shares one execution; cancelled
// waiters wake without disturbing the leader).

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

func TestInFlightAcquireLeaderThenJoiner(t *testing.T) {
	tr := NewInFlightTracker()
	sc1, leader1 := tr.acquire("k")
	if !leader1 {
		t.Fatal("first acquire must be leader")
	}
	sc2, leader2 := tr.acquire("k")
	if leader2 {
		t.Fatal("second acquire must join")
	}
	if sc1 != sc2 {
		t.Fatal("joiner must receive the leader's sharedCall")
	}
}

func TestInFlightDeliverBroadcastsToWaiters(t *testing.T) {
	tr := NewInFlightTracker()
	sc, _ := tr.acquire("k")

	go func() {
		time.Sleep(5 * time.Millisecond)
		tr.Deliver(sc, "payload", nil)
	}()

	select {
	case <-sc.done:
		if sc.value != "payload" || sc.err != nil {
			t.Fatalf("broadcast = (%v, %v), want (payload, nil)", sc.value, sc.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never woken by Deliver")
	}
}

func TestInFlightDeliverUnknownKeyNoOp(t *testing.T) {
	tr := NewInFlightTracker()
	tr.Deliver(nil, "x", nil) // must not panic
}

func TestInFlightDeliverCleansUpForNewLeader(t *testing.T) {
	tr := NewInFlightTracker()
	sc1, _ := tr.acquire("k")
	tr.Deliver(sc1, 1, nil)
	sc2, leader := tr.acquire("k")
	if !leader {
		t.Fatal("after Deliver the next submitter must become a new leader")
	}
	tr.Deliver(sc2, 2, nil)
	<-sc2.done
}

func blockingReq(key string, release <-chan struct{}, execs *int32) *Request {
	return &Request{
		Priority:    PriorityHover,
		CoalesceKey: key,
		Result:      make(chan Result, 1),
		Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
			atomic.AddInt32(execs, 1)
			<-release
			return "shared", nil
		},
	}
}

// TestF11_JoinSuccessPath proves identical coalescable requests share ONE
// execution: leader runs once, joiner receives the same result.
func TestF11_JoinSuccessPath(t *testing.T) {
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Shutdown()

	release := make(chan struct{})
	var execs int32

	leader := blockingReq("hover|same", release, &execs)
	if res := s.Submit(leader); res != Admitted {
		t.Fatalf("leader submit: %v", res)
	}

	waiter := blockingReq("hover|same", release, &execs)
	if res := s.Submit(waiter); res != Admitted {
		t.Fatalf("joiner submit: %v", res)
	}

	close(release)

	for _, r := range []*Request{leader, waiter} {
		select {
		case got := <-r.Result:
			if got.Err != nil || got.Value != "shared" {
				t.Fatalf("result = (%v, %v), want (shared, nil)", got.Value, got.Err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("request never completed")
		}
	}
	if n := atomic.LoadInt32(&execs); n != 1 {
		t.Fatalf("executions = %d, want exactly 1", n)
	}
	if j := s.Stats().TotalJoined; j != 1 {
		t.Fatalf("TotalJoined = %d, want 1", j)
	}
}

// TestF11_WaiterCancellationWakesWithoutDisturbingLeader proves a waiter
// cancelled mid-wait gets Canceled immediately while the leader still runs
// and a remaining waiter still receives the real result.
func TestF11_WaiterCancellationWakesWithoutDisturbingLeader(t *testing.T) {
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Shutdown()

	release := make(chan struct{})
	var execs int32

	leader := blockingReq("hover|same", release, &execs)
	if res := s.Submit(leader); res != Admitted {
		t.Fatalf("leader submit: %v", res)
	}

	cancelled := blockingReq("hover|same", release, &execs)
	if res := s.Submit(cancelled); res != Admitted {
		t.Fatalf("cancelled-waiter submit: %v", res)
	}
	cancelled.Cancel() // must wake its waiter goroutine at once

	select {
	case got := <-cancelled.Result:
		if got.Err != context.Canceled {
			t.Fatalf("cancelled waiter err = %v, want context.Canceled", got.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter never woken — F11 cancellation broken")
	}

	// A second live waiter still joins and will get the shared result.
	live := blockingReq("hover|same", release, &execs)
	if res := s.Submit(live); res != Admitted {
		t.Fatalf("live-waiter submit: %v", res)
	}

	close(release)

	for _, r := range []*Request{leader, live} {
		select {
		case got := <-r.Result:
			if got.Err != nil || got.Value != "shared" {
				t.Fatalf("result = (%v, %v), want (shared, nil)", got.Value, got.Err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("request never completed after release")
		}
	}
	if n := atomic.LoadInt32(&execs); n != 1 {
		t.Fatalf("executions = %d, want exactly 1 (cancel must not disturb leader)", n)
	}
}

func TestF11_LeaderCancellationKeepsSharedWorkForLiveWaiter(t *testing.T) {
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Shutdown()

	release := make(chan struct{})
	started := make(chan struct{})
	sharedCanceled := make(chan struct{})
	leader := &Request{
		Priority:    PriorityHover,
		CoalesceKey: "hover|leader-cancel",
		Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
			close(started)
			select {
			case <-release:
				return "shared", nil
			case <-ctx.Done():
				close(sharedCanceled)
				return nil, ctx.Err()
			}
		},
	}
	if got := s.Submit(leader); got != Admitted {
		t.Fatalf("leader admission = %v", got)
	}
	<-started
	waiter := blockingReq("hover|leader-cancel", release, new(int32))
	if got := s.Submit(waiter); got != Admitted {
		t.Fatalf("waiter admission = %v", got)
	}
	leader.Cancel()
	select {
	case got := <-leader.Result:
		if got.Err != context.Canceled {
			t.Fatalf("leader result = %+v, want cancellation", got)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled leader did not receive an independent terminal result")
	}
	close(release)
	select {
	case got := <-waiter.Result:
		if got.Err != nil || got.Value != "shared" {
			t.Fatalf("remaining waiter result = %+v, want shared success", got)
		}
	case <-time.After(time.Second):
		t.Fatal("live waiter did not receive shared result")
	}
	select {
	case <-sharedCanceled:
		t.Fatal("leader cancellation canceled shared work needed by a waiter")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestF11_LastWaiterCancellationCancelsSharedWork(t *testing.T) {
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Shutdown()

	started := make(chan struct{})
	sharedCanceled := make(chan struct{})
	leader := &Request{
		Priority:    PriorityHover,
		CoalesceKey: "hover|all-cancel",
		Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
			close(started)
			<-ctx.Done()
			close(sharedCanceled)
			return nil, ctx.Err()
		},
	}
	if got := s.Submit(leader); got != Admitted {
		t.Fatalf("leader admission = %v", got)
	}
	<-started
	waiter := blockingReq("hover|all-cancel", make(chan struct{}), new(int32))
	if got := s.Submit(waiter); got != Admitted {
		t.Fatalf("waiter admission = %v", got)
	}
	leader.Cancel()
	waiter.Cancel()
	for _, req := range []*Request{leader, waiter} {
		select {
		case got := <-req.Result:
			if !errors.Is(got.Err, context.Canceled) {
				t.Errorf("request result = %+v, want cancellation", got)
			}
		case <-time.After(time.Second):
			t.Fatal("canceled shared request remained blocked")
		}
	}
	select {
	case <-sharedCanceled:
	case <-time.After(time.Second):
		t.Fatal("shared execution context stayed live after its last waiter canceled")
	}
}

func TestF11_WaiterDeadlineDoesNotCancelLeaderWork(t *testing.T) {
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Shutdown()

	release := make(chan struct{})
	started := make(chan struct{})
	sharedCanceled := make(chan struct{})
	leader := &Request{
		Priority:    PriorityHover,
		CoalesceKey: "hover|waiter-deadline",
		Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
			close(started)
			select {
			case <-release:
				return "shared", nil
			case <-ctx.Done():
				close(sharedCanceled)
				return nil, ctx.Err()
			}
		},
	}
	if got := s.Submit(leader); got != Admitted {
		t.Fatalf("leader admission = %v", got)
	}
	<-started
	waiter := &Request{
		Priority:    PriorityHover,
		CoalesceKey: "hover|waiter-deadline",
		Deadline:    time.Now().Add(30 * time.Millisecond),
		Execute:     leader.Execute,
	}
	if got := s.Submit(waiter); got != Admitted {
		t.Fatalf("waiter admission = %v", got)
	}
	select {
	case got := <-waiter.Result:
		if !errors.Is(got.Err, context.DeadlineExceeded) {
			t.Fatalf("waiter result = %+v, want deadline exceeded", got)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter deadline was not enforced independently")
	}
	select {
	case <-sharedCanceled:
		t.Fatal("waiter deadline canceled work still needed by the leader")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case got := <-leader.Result:
		if got.Err != nil || got.Value != "shared" {
			t.Fatalf("leader result = %+v, want shared success", got)
		}
	case <-time.After(time.Second):
		t.Fatal("leader did not receive shared result")
	}
}

func TestF11_PanicCompletesWaitersAndWorkerSurvives(t *testing.T) {
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Shutdown()

	started := make(chan struct{})
	release := make(chan struct{})
	leader := &Request{
		Priority:    PriorityHover,
		CoalesceKey: "hover|panic",
		Execute: func(context.Context, *snapshot.Snapshot) (any, error) {
			close(started)
			<-release
			panic("scheduler boom")
		},
	}
	if got := s.Submit(leader); got != Admitted {
		t.Fatalf("leader admission = %v", got)
	}
	<-started
	waiter := blockingReq("hover|panic", release, new(int32))
	if got := s.Submit(waiter); got != Admitted {
		t.Fatalf("waiter admission = %v", got)
	}
	close(release)
	for _, req := range []*Request{leader, waiter} {
		select {
		case got := <-req.Result:
			if got.Err == nil || !strings.Contains(got.Err.Error(), "panicked") {
				t.Errorf("request result = %+v, want panic error", got)
			}
		case <-time.After(time.Second):
			t.Fatal("panic stranded a shared waiter")
		}
	}
	next := &Request{Priority: PriorityCompletion, Execute: func(context.Context, *snapshot.Snapshot) (any, error) { return "alive", nil }}
	if got := s.Submit(next); got != Admitted {
		t.Fatalf("post-panic request admission = %v", got)
	}
	select {
	case got := <-next.Result:
		if got.Err != nil || got.Value != "alive" {
			t.Fatalf("worker after panic = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not survive a panicking request")
	}
}

// TestF11_LeaderRejectedRollsBack proves a leader whose admission fails
// delivers the error to its waiters instead of hanging them forever.
// (End-to-end queue overflow cannot reliably stage — workers drain queues
// instantly — so the rollback contract is verified at the mechanism level.)
func TestF11_LeaderRejectedRollsBack(t *testing.T) {
	s := New(DefaultConfig())

	sc, leader := s.tracker.acquire("k|full")
	if !leader {
		t.Fatal("expected to register as leader")
	}
	joiner, isLeader := s.tracker.acquire("k|full")
	if isLeader || joiner != sc {
		t.Fatalf("second acquire must join the same call")
	}

	s.deliverRollback(&Request{CoalesceKey: "k|full", sharedCall: sc}, errRejected)

	select {
	case <-sc.done:
		if sc.err != errRejected {
			t.Fatalf("waiter err = %v, want %v", sc.err, errRejected)
		}
	default:
		t.Fatal("abandoned leader must release waiters immediately")
	}
	// After rollback the key must be free for a fresh leader.
	if _, isLeader := s.tracker.acquire("k|full"); !isLeader {
		t.Fatal("key must be re-acquirable after rollback")
	}
}
