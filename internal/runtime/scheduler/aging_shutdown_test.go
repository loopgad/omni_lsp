package scheduler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// runAgingShutdownRound drives one aging/Shutdown collision (audit 复证圈 1,
// findings 1/2/3/5): a slow request occupies the only worker, a full stale
// lane churns under tight-loop aging passes, then the context is cancelled
// and Shutdown closes the lanes while the blocker still runs. The blocker is
// only released after Shutdown so the draining worker scans lanes that aging
// passes were concurrently extracting from. Every admitted request must reach
// exactly one terminal outcome and no pass may panic on a drained closed lane.
func runAgingShutdownRound(t *testing.T, round int) {
	t.Helper()
	// PriorityAgingMs is huge on purpose: the parked requests must stay NOT
	// stale for the whole round so the extract→put-back branch keeps the high
	// lanes populated through the shutdown close (the pseudo-rejection and
	// nil-receive window) instead of cascading to the scan floor. The field
	// is consumed in milliseconds (round20/bugC unit fix) — 10^10 reads as
	// ~115 days, far beyond this round.
	s := New(Config{MaxConcurrent: 1, MaxQueueSize: 256, MaxInFlight: 256, PriorityAgingMs: 10_000_000_000})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	blockerStarted := make(chan struct{})
	releaseBlocker := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBlocker) }) }
	defer release()
	blocker := &Request{
		RequestID: identity.RequestID(fmt.Sprintf("aging-stress-%d-blocker", round)),
		Priority:  PriorityMaintenance,
		CostClass: CostLarge,
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			close(blockerStarted)
			<-releaseBlocker
			return "blocker", nil
		},
	}
	if got := s.Submit(blocker); got != Admitted {
		t.Fatalf("round %d: submit blocker = %v, want Admitted", round, got)
	}
	select {
	case <-blockerStarted:
	case <-time.After(time.Second):
		t.Fatal("worker did not start the blocker")
	}

	const total = 200
	reqs := make([]*Request, total)
	for i := range reqs {
		reqs[i] = &Request{
			RequestID:  identity.RequestID(fmt.Sprintf("aging-stress-%d-%d", round, i)),
			Priority:   PriorityMaintenance,
			CostClass:  CostLarge,
			EnqueuedAt: time.Now(), // not stale: aging extracts and puts back in place
			Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
				return nil, nil
			},
		}
		if got := s.Submit(reqs[i]); got != Admitted {
			t.Fatalf("round %d: submit %d = %v, want Admitted", round, i, got)
		}
	}

	// Two stale requests in a small separate lane: the stale branch extracts
	// them for a boost, and the lane empties quickly under the concurrent
	// drain — the tight window where a hammer's len>0 check is followed by a
	// receive on the just-drained closed lane (the nil *Request deref).
	for i := 0; i < 2; i++ {
		stale := &Request{
			RequestID:  identity.RequestID(fmt.Sprintf("aging-stress-%d-stale-%d", round, i)),
			Priority:   PriorityMaintenance,
			CostClass:  CostVeryLarge,
			EnqueuedAt: time.Now().Add(-time.Hour),
			Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
				return nil, nil
			},
		}
		if got := s.Submit(stale); got != Admitted {
			t.Fatalf("round %d: submit stale %d = %v, want Admitted", round, i, got)
		}
	}

	// Tight-loop aging passes from several goroutines, exactly the pressure
	// shape that exposed the nil receive on a drained closed lane.
	stop := make(chan struct{})
	var hammerWG sync.WaitGroup
	for i := 0; i < 8; i++ {
		hammerWG.Add(1)
		go func() {
			defer hammerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s.boostStaleRequests(ctx)
			}
		}()
	}

	time.Sleep(30 * time.Millisecond)
	cancel()
	s.Shutdown() // lanes close while the blocker still occupies the worker
	release()    // the worker now drains every queued request to a terminal state
	close(stop)
	hammerWG.Wait()

	// The cancellation is a drain signal, not a discard: every admitted
	// request must reach exactly one terminal outcome even though aging
	// passes were interleaving with the final draining scan and with the
	// lane close.
	deadline := time.After(5 * time.Second)
	results := make([]Result, total)
	for i, req := range reqs {
		select {
		case res := <-req.Result:
			results[i] = res
		case <-deadline:
			st := s.Stats()
			t.Fatalf("round %d: request %d stranded without a terminal result: stats=+%v queued=%d", round, i, st, s.QueueLen())
		}
	}
	for i, res := range results {
		if res.Err != nil && strings.Contains(res.Err.Error(), "priority aging") {
			t.Errorf("round %d: request %d was pseudo-rejected by priority aging: %v", round, i, res.Err)
		}
	}
}

// TestScheduler_AgingShutdownStressNoPanicNoStrand runs the collision round
// several times; pre-fix it reproduced both the agingLoop nil-pointer panic
// and stranded/pseudo-rejected queued requests.
func TestScheduler_AgingShutdownStressNoPanicNoStrand(t *testing.T) {
	for round := 0; round < 3; round++ {
		runAgingShutdownRound(t, round)
	}
}
