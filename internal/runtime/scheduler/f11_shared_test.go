package scheduler

// Tests for F11 single-flight joining: tracker semantics plus end-to-end
// Submit integration (identical CoalesceKey shares one execution; cancelled
// waiters wake without disturbing the leader).

import (
	"context"
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
		tr.Deliver("k", "payload", nil)
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
	tr.Deliver("missing", "x", nil) // must not panic
}

func TestInFlightDeliverCleansUpForNewLeader(t *testing.T) {
	tr := NewInFlightTracker()
	tr.acquire("k")
	tr.Deliver("k", 1, nil)
	sc2, leader := tr.acquire("k")
	if !leader {
		t.Fatal("after Deliver the next submitter must become a new leader")
	}
	tr.Deliver("k", 2, nil)
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

	s.deliverRollback(&Request{CoalesceKey: "k|full"}, errRejected)

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
