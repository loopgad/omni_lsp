package scheduler

import (
	"context"
	"errors"
	"sync"
)

// errRejected is delivered to waiters when their leader never executes
// (admission rollback). The budget and queue rejections wrap it too, so a
// caller can tell "the scheduler would not run this" apart from a failure the
// request itself caused, with errors.Is.
var errRejected = errors.New("scheduler: leader rejected before execution")

// InFlightTracker provides single-flight joining for shared computations
// (F11/J6): concurrent requests with the same CoalesceKey share one
// execution instead of duplicating work. The first submitter becomes the
// leader and is queued normally; later submitters receive the leader's
// result when it is delivered.
//
// Each admitted request holds one waiter reference. Releasing an individual
// reference leaves the shared context alive; releasing the last reference
// removes the key and cancels the shared execution.
type InFlightTracker struct {
	mu       sync.Mutex
	inFlight map[string]*sharedCall
}

// sharedCall is one in-flight computation and its result broadcast.
type sharedCall struct {
	key       string
	done      chan struct{} // closed exactly once when the result is set
	value     any
	err       error
	waiters   int
	cancel    context.CancelFunc
	completed bool
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
		sc.waiters++
		return sc, false
	}
	sc := &sharedCall{key: key, done: make(chan struct{}), waiters: 1}
	t.inFlight[key] = sc
	return sc, true
}

// release drops one request's interest. The shared execution is canceled and
// removed from admission only when its final waiter leaves.
func (t *InFlightTracker) release(sc *sharedCall) {
	if sc == nil {
		return
	}
	t.mu.Lock()
	if sc.waiters > 0 {
		sc.waiters--
	}
	var cancel context.CancelFunc
	if sc.waiters == 0 && !sc.completed {
		if t.inFlight[sc.key] == sc {
			delete(t.inFlight, sc.key)
		}
		cancel = sc.cancel
	}
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (t *InFlightTracker) setCancel(sc *sharedCall, cancel context.CancelFunc) {
	t.mu.Lock()
	if sc.completed {
		t.mu.Unlock()
		cancel()
		return
	}
	sc.cancel = cancel
	shouldCancel := sc.waiters == 0
	t.mu.Unlock()
	if shouldCancel {
		cancel()
	}
}

func (t *InFlightTracker) waiterCount(sc *sharedCall) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return sc.waiters
}

// Deliver completes the leader's call and broadcasts value/err to all
// waiters. Unknown keys are ignored.
func (t *InFlightTracker) Deliver(sc *sharedCall, value any, err error) {
	if sc == nil {
		return
	}
	t.mu.Lock()
	if sc.completed {
		t.mu.Unlock()
		return
	}
	if t.inFlight[sc.key] == sc {
		delete(t.inFlight, sc.key)
	}
	sc.value, sc.err, sc.completed = value, err, true
	close(sc.done)
	t.mu.Unlock()
}
