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
	return errors.As(err, &t)
}

// Query resolves k: cached hit, in-flight join (singleflight §J6), or
// compute. declared deps are unioned with those the function reports.
func (e *Engine) Query(ctx context.Context, k Key, declared DepSet, fn ComputeFn) (Result, error) {
	return e.queryOnce(ctx, k, declared, fn, false)
}

func (e *Engine) queryOnce(ctx context.Context, k Key, declared DepSet, fn ComputeFn, recursion bool) (Result, error) {
	key := k.String()

	if !recursion && chainHas(ctx, key) {
		e.mu.Lock()
		e.cyclesFound++
		e.mu.Unlock()
		return Result{}, fmt.Errorf("%w: %s", ErrQueryCycle, key)
	}

	for {
		e.mu.Lock()
		if en := e.entries[key]; en != nil {
			switch en.state {
			case Ready:
				e.hits++
				res := Result{Value: en.value, Evidence: en.evidence, SafetyClass: en.safety}
				e.mu.Unlock()
				return res, nil
			case FailedStable:
				e.hits++
				err := en.err
				e.mu.Unlock()
				return Result{}, err
			}
		}
		if call := e.inflight[key]; call != nil {
			// Singleflight join (§J6): park until the leader finishes.
			e.mu.Unlock()
			select {
			case <-call.done:
				if call.err != nil {
					return Result{}, call.err
				}
				return call.res, nil
			case <-ctx.Done():
				// Independent waiter cancellation: leader and siblings are
				// unaffected; this waiter simply gives up (§J6).
				return Result{}, ctx.Err()
			}
		}

		// We are the leader: register in-flight and compute outside the lock.
		e.misses++
		e.computations++
		call := &inflightCall{done: make(chan struct{})}
		e.inflight[key] = call
		e.mu.Unlock()

		val, gotDeps, err := func() (any, DepSet, error) {
			defer func() {
				if r := recover(); r != nil {
					panic(r) // compute panics propagate; F16 boundary is upstream
				}
			}()
			return fn(withKey(ctx, key), Bindings{e: e})
		}()

		e.mu.Lock()
		delete(e.inflight, key)

		// §J7 freshness gate: revision moved while computing → discard.
		if k.SnapshotRev != 0 && e.expectedRev > k.SnapshotRev {
			e.stalePublishRejected++
			err = fmt.Errorf("query: stale publish for %s (computed@%d want>=%d): %w", key, k.SnapshotRev, e.expectedRev, ErrStalePublish)
			call.err = err
			close(call.done)
			e.mu.Unlock()
			return Result{}, err
		}

		deps := gotDeps.Union(declared)
		en := &entry{deps: deps}
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
		call.res = Result{Value: val}
		call.err = err
		close(call.done)
		e.mu.Unlock()

		if err != nil {
			return Result{}, err
		}
		return call.res, nil
	}
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
	e.mu.Lock()
	defer e.mu.Unlock()
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
	e.mu.Lock()
	defer e.mu.Unlock()
	drop := fmt.Sprintf("|%d|", rev)
	n := 0
	for k, en := range e.entries {
		if en.state != Ready && en.state != FailedStable {
			continue
		}
		if containsSub(k, drop) {
			e.removeKeyFromDeps(k, en.deps)
			delete(e.entries, k)
			n++
			e.evictions++
		}
	}
	if rev > e.expectedRev {
		e.expectedRev = rev
	}
	return n
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
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
}

// ErrStalePublish marks a result computed against a superseded revision
// (§J7). Callers that legitimately serve captured snapshots (INV-SNAPSHOT-002)
// can detect it and answer directly instead of treating it as a failure.
var ErrStalePublish = errors.New("stale revision")
