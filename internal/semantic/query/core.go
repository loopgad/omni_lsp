package query

import (
	"context"
	"errors"
	"fmt"
)

// ErrQueryCycle reports a memoized future synchronously waiting on itself
// (§J5: a bug in the caller, never a legal wait).
var ErrQueryCycle = errors.New("query: cycle detected (self-wait)")

// TransientError marks an error retryable: not cached as stable negative
// knowledge (§J4).
type TransientError struct{ Err error }

func (t *TransientError) Error() string { return "query: transient: " + t.Err.Error() }
func (t *TransientError) Unwrap() error { return t.Err }

// Transient wraps err so the engine retries instead of caching failure.
func Transient(err error) error { return &TransientError{Err: err} }

func isTransient(err error) bool {
	var t *TransientError
	return errors.As(err, &t) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// Query resolves k: cached hit, in-flight join (singleflight §J6), or
// compute. declared deps are unioned with those the function reports.
func (e *Engine) Query(ctx context.Context, k Key, declared DepSet, fn ComputeFn) (Result, error) {
	return e.queryOnce(ctx, k, declared, fn, false)
}

func (e *Engine) queryOnce(ctx context.Context, k Key, declared DepSet, fn ComputeFn, recursion bool) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	key := k.String()

	if !recursion && chainHas(ctx, key) {
		if err := e.lockContext(ctx); err != nil {
			return Result{}, err
		}
		e.cyclesFound++
		e.unlock()
		return Result{}, fmt.Errorf("%w: %s", ErrQueryCycle, key)
	}

	for {
		if err := e.lockContext(ctx); err != nil {
			return Result{}, err
		}
		if en := e.entries[key]; en != nil {
			if en.snapshotRev < e.expectedRev {
				e.removeKeyFromDeps(key, en.deps)
				delete(e.entries, key)
				en = nil
			}
		}
		if en := e.entries[key]; en != nil {
			switch en.state {
			case Ready:
				e.hits++
				res := Result{Value: en.value, Evidence: en.evidence, SafetyClass: en.safety}
				e.unlock()
				return res, nil
			case FailedStable:
				e.hits++
				err := en.err
				e.unlock()
				return Result{}, err
			}
		}
		if call := e.inflight[key]; call != nil {
			// Each caller owns one reference. The shared context is canceled
			// only when its last waiter leaves (§F11/J6).
			call.waiters.Add(1)
			e.unlock()
			return e.waitForCall(ctx, key, call, false)
		}

		// Register shared work outside the request's cancellation lifetime.
		// The request still waits independently; other waiters can keep the
		// compute alive if this caller cancels.
		e.misses++
		e.computations++
		sharedCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call := &inflightCall{done: make(chan struct{}), ctx: sharedCtx, cancel: cancel}
		call.waiters.Store(1)
		e.inflight[key] = call
		e.unlock()
		go e.compute(key, k, declared, fn, call)
		return e.waitForCall(ctx, key, call, true)
	}
}

func (e *Engine) waitForCall(ctx context.Context, key string, call *inflightCall, leader bool) (Result, error) {
	if err := ctx.Err(); err != nil {
		e.releaseWaiter(key, call)
		return Result{}, err
	}
	select {
	case <-call.done:
		if leader && call.panicValue != nil {
			panic(call.panicValue) // preserve the leader's panic boundary after cleanup
		}
		return call.res, call.err
	case <-ctx.Done():
		e.releaseWaiter(key, call)
		return Result{}, ctx.Err()
	}
}

func (e *Engine) releaseWaiter(key string, call *inflightCall) {
	e.lock()
	lastWaiter := call.waiters.Add(-1) == 0
	if lastWaiter && e.inflight[key] == call {
		// Remove canceled work before allowing a new same-key caller to join.
		// The pointer check prevents a late waiter from removing a replacement.
		delete(e.inflight, key)
	}
	e.unlock()
	if lastWaiter {
		call.cancel()
	}
}

// compute runs outside the table lock so callers can cancel independently.
// It completes its call even if the last waiter already detached it, and only
// publishes when it still owns the key's in-flight slot.
func (e *Engine) compute(key string, k Key, declared DepSet, fn ComputeFn, call *inflightCall) {
	var val any
	var gotDeps DepSet
	var err error
	var panicValue any
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicValue = r
				err = fmt.Errorf("query: compute panicked (%T)", r)
			}
		}()
		val, gotDeps, err = fn(withKey(call.ctx, key), Bindings{e: e})
	}()
	if panicValue != nil {
		val = nil
		gotDeps = nil
	} else if canceled := call.ctx.Err(); canceled != nil {
		// Work abandoned by its last waiter must not publish a stable result.
		val, gotDeps, err = nil, nil, canceled
	}

	e.lock()
	if panicValue == nil {
		if canceled := call.ctx.Err(); canceled != nil {
			val, gotDeps, err = nil, nil, canceled
		}
	}
	current := e.inflight[key] == call
	if current {
		delete(e.inflight, key)
	}

	if current {
		// §J7 freshness gate: revision moved while computing → discard.
		if k.SnapshotRev != 0 && e.expectedRev > k.SnapshotRev {
			e.stalePublishRejected++
			err = fmt.Errorf("query: stale publish for %s (computed@%d want>=%d): %w", key, k.SnapshotRev, e.expectedRev, ErrStalePublish)
			val = nil
			gotDeps = nil
		}

		if panicValue == nil && !errors.Is(err, ErrStalePublish) {
			deps := gotDeps.Union(declared)
			en := &entry{deps: deps, snapshotRev: k.SnapshotRev}
			if err != nil {
				if isTransient(err) {
					delete(e.entries, key) // retryable: drop entirely
				} else {
					en.state = FailedStable
					en.err = err
					e.entries[key] = en
					e.indexDeps(key, deps)
				}
			} else {
				en.state = Ready
				en.value = val
				e.entries[key] = en
				e.indexDeps(key, deps)
			}
			e.evictBounded(key)
		}
	}

	call.res = Result{Value: val}
	call.err = err
	call.panicValue = panicValue
	call.cancel()
	close(call.done)
	e.unlock()
}

// indexDeps records dep → dependent-key edges for selective invalidation.
func (e *Engine) indexDeps(key string, deps DepSet) {
	for d := range deps {
		if e.depIndex[d] == nil {
			e.depIndex[d] = map[string]struct{}{}
		}
		e.depIndex[d][key] = struct{}{}
	}
}

// Invalidate drops every cached entry depending on dep (§J2-J3).
func (e *Engine) Invalidate(dep Dep) int {
	e.lock()
	defer e.unlock()
	keys, ok := e.depIndex[dep]
	if !ok {
		return 0
	}
	n := 0
	for k := range keys {
		if en, ok := e.entries[k]; ok && (en.state == Ready || en.state == FailedStable) {
			e.removeKeyFromDeps(k, en.deps)
			delete(e.entries, k)
			n++
			e.evictions++
		}
	}
	delete(e.depIndex, dep)
	return n
}

// InvalidateSnapshot drops all entries bound to revisions older than rev and
// raises the freshness expectation for publishes in flight (§J7).
func (e *Engine) InvalidateSnapshot(rev uint64) int {
	e.lock()
	defer e.unlock()
	n := 0
	for k, en := range e.entries {
		if en.state != Ready && en.state != FailedStable {
			continue
		}
		if en.snapshotRev <= rev {
			e.removeKeyFromDeps(k, en.deps)
			delete(e.entries, k)
			n++
			e.evictions++
		}
	}
	if rev > e.expectedRev {
		e.expectedRev = rev
	}
	e.compactFIFO()
	return n
}

// evictBounded keeps the memo table within maxQueryEntries (§K0), evicting
// oldest-inserted entries first. Caller holds mu. The just-inserted key stays.
func (e *Engine) evictBounded(justInserted string) {
	for len(e.entries) > maxQueryEntries && len(e.fifo) > 0 {
		oldest := e.fifo[0]
		e.fifo = e.fifo[1:]
		if oldest == justInserted {
			continue
		}
		if en, ok := e.entries[oldest]; ok && (en.state == Ready || en.state == FailedStable) {
			e.removeKeyFromDeps(oldest, en.deps)
			delete(e.entries, oldest)
			e.evictions++
		}
	}
	e.fifo = append(e.fifo, justInserted)
	if len(e.fifo) > maxQueryEntries*2 {
		e.compactFIFO()
	}
}

func (e *Engine) compactFIFO() {
	compact := e.fifo[:0]
	seen := make(map[string]struct{}, len(e.entries))
	for _, key := range e.fifo {
		if _, ok := e.entries[key]; !ok {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		compact = append(compact, key)
	}
	e.fifo = compact
}

// ErrStalePublish marks a result computed against a superseded revision
// (§J7). Callers that legitimately serve captured snapshots (INV-SNAPSHOT-002)
// can detect it and answer directly instead of treating it as a failure.
var ErrStalePublish = errors.New("stale revision")
