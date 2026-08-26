package scheduler

import (
	"errors"
	"sync"
)

// errRejected is delivered to waiters when their leader never executes
// (admission rollback). Distinct so tests and callers can recognize it.
var errRejected = errors.New("scheduler: leader rejected before execution")

// InFlightTracker provides single-flight joining for shared computations
// (F11/J6): concurrent requests with the same CoalesceKey share one
// execution instead of duplicating work. The first submitter becomes the
// leader and is queued normally; later submitters receive the leader's
// result when it is delivered.
//
// Waiter-side cancellation is NOT handled here: waiters select on their own
// Request.wakeCh (closed by Cancel), so a cancelled waiter stops waiting
// without disturbing the leader or other waiters.
type InFlightTracker struct {
	mu       sync.Mutex
	inFlight map[string]*sharedCall
}

// sharedCall is one in-flight computation and its result broadcast.
type sharedCall struct {
	done  chan struct{} // closed exactly once when the result is set
	value any
	err   error
}

func NewInFlightTracker() *InFlightTracker {
	return &InFlightTracker{inFlight: make(map[string]*sharedCall)}
}

// acquire joins the in-flight call for key, or registers the caller as its
// leader. Returns (call, isLeader). Leaders MUST eventually call Deliver —
// including on failure paths (queue rejection, cancellation) — or waiters
// hang forever.
func (t *InFlightTracker) acquire(key string) (*sharedCall, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if sc, ok := t.inFlight[key]; ok {
		return sc, false
	}
	sc := &sharedCall{done: make(chan struct{})}
	t.inFlight[key] = sc
	return sc, true
}

// Deliver completes the leader's call and broadcasts value/err to all
// waiters. Unknown keys are ignored.
func (t *InFlightTracker) Deliver(coalesceKey string, value any, err error) {
	t.mu.Lock()
	sc := t.inFlight[coalesceKey]
	delete(t.inFlight, coalesceKey)
	t.mu.Unlock()
	if sc == nil {
		return
	}
	sc.value, sc.err = value, err
	close(sc.done)
}
