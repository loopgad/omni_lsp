package query

import (
	"context"
	"fmt"
	"sync"
)

// Engine is the incremental query engine (§J0).
//
// Owned mutable state: entries, inflight, depIndex (mu-protected);
// stats counters (atomic); expectedRev (set under mu by InvalidateSnapshot).
//
// Concurrency model: one Mutex guards tables; compute functions run outside
// the lock; waiters park on per-inflight-call channels. Cycle detection uses
// an explicit call chain carried in the Context, not goroutine identity.
// maxQueryEntries bounds the memo table (§K0: every cache is bounded). FIFO
// eviction matches the pkgCache pattern; LRU only if hit rates demand it.
const maxQueryEntries = 4096

type Engine struct {
	mu       sync.Mutex
	entries  map[string]*entry
	inflight map[string]*inflightCall
	depIndex map[Dep]map[string]struct{}
	fifo     []string // insertion order for bounded eviction

	expectedRev uint64

	hits, misses         uint64
	computations         uint64
	cyclesFound          uint64
	evictions            uint64
	stalePublishRejected uint64
}

func NewEngine(expectedSnapshotRev uint64) *Engine {
	return &Engine{
		entries:     map[string]*entry{},
		inflight:    map[string]*inflightCall{},
		depIndex:    map[Dep]map[string]struct{}{},
		expectedRev: expectedSnapshotRev,
	}
}

// Stats is a point-in-time counter snapshot.
type Stats struct {
	Hits                 uint64
	Misses               uint64
	Computations         uint64
	CyclesDetected       uint64
	Evictions            uint64
	StalePublishRejected uint64
}

func (e *Engine) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return Stats{
		Hits: e.hits, Misses: e.misses,
		Computations: e.computations, CyclesDetected: e.cyclesFound,
		Evictions: e.evictions, StalePublishRejected: e.stalePublishRejected,
	}
}

func (e *Engine) removeKeyFromDeps(key string, deps DepSet) {
	for d := range deps {
		keys := e.depIndex[d]
		if keys == nil {
			continue
		}
		delete(keys, key)
		if len(keys) == 0 {
			delete(e.depIndex, d)
		}
	}
}

type entry struct {
	state       State
	snapshotRev uint64
	value       any
	err         error
	deps        DepSet
	evidence    string
	safety      int
}

type inflightCall struct {
	done chan struct{}
	res  Result
	err  error
}

// callChainKey carries the active query-key stack in the Context so nested
// self-waits are detectable without goroutine identity hacks (§J5).
type callChainKey struct{}

func withKey(ctx context.Context, k string) context.Context {
	parent, _ := ctx.Value(callChainKey{}).([]string)
	stack := make([]string, len(parent)+1)
	copy(stack, parent)
	stack[len(parent)] = k
	return context.WithValue(ctx, callChainKey{}, stack)
}

func chainHas(ctx context.Context, k string) bool {
	stack, _ := ctx.Value(callChainKey{}).([]string)
	for _, s := range stack {
		if s == k {
			return true
		}
	}
	return false
}

// Bindings lets a compute function recurse through the engine so cycles and
// dependency edges stay centrally tracked.
type Bindings struct {
	e *Engine
}

// Query resolves another query from inside a compute function.
func (b Bindings) Query(ctx context.Context, k Key, deps DepSet, fn ComputeFn) (any, error) {
	r, err := b.e.Query(ctx, k, deps, fn)
	return r.Value, err
}

// RecursionAllowed deliberately re-enters the same key for language-level
// semantic cycles (recursive types etc.); depth bounds runaway recursion.
func (b Bindings) RecursionAllowed(ctx context.Context, k Key, deps DepSet, fn ComputeFn, depth int) (any, error) {
	if depth <= 0 || depth > maxRecursionDepth {
		return nil, fmt.Errorf("query: recursion depth %d out of bounds", depth)
	}
	return b.e.queryOnce(ctx, k, deps, fn, true)
}
