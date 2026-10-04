// Package query implements the incremental query engine (goal.md §J0-J8):
// deterministic memoized queries with content-addressed dependency tracking,
// a six-state lifecycle, singleflight joining, and cycle detection.
//
// Owned mutable state: entries map (mu-protected), inflight table,
// depIndex (dep → dependent keys), stats counters (atomics).
//
// Concurrency model: one gate guards the entry/inflight tables; compute
// functions run OUTSIDE the lock; waiters park on per-query channels.
//
// Invariants:
//  1. A published result's snapshot revision matched the expectation at
//     publish time — stale results never enter the cache (§J7).
//  2. A compute function synchronously re-querying its own key is detected
//     and reported as ErrQueryCycle, never deadlocked (§J5).
//  3. Milestone-deferred dependencies are content-addressed identities
//     (hash/generation/epoch), never live pointers (§J3).
package query

import "fmt"

// Key identifies a memoizable query (§J1). Semantic identity only:
// timestamps and request IDs MUST NOT appear in any field.
type Key struct {
	Kind             string // e.g. "hover", "references", "index.symbol"
	Workspace        string
	SnapshotRev      uint64
	SnapshotInstance uint64 // process-local immutable snapshot identity; guards revision aliases
	IndexGeneration  uint64 // committed persistent semantic generation (zero means none)
	BuildContext     string
	BackendEpoch     uint64 // lifecycle generation of an external backend; in-process is zero
	Subject          string // URI, symbol ID, …
	OptionsHash      string // hash over options that affect the result
}

func (k Key) String() string {
	return fmt.Sprintf("%s|%s|%d:%d|g%d|%s|%d|%s|%s",
		k.Kind, k.Workspace, k.SnapshotRev, k.SnapshotInstance, k.IndexGeneration, k.BuildContext, k.BackendEpoch, k.Subject, k.OptionsHash)
}

// Dep is one content-addressed dependency identity (§J3).
type Dep struct {
	Kind string // "file", "indexGen", "backendEpoch", "snapshot"
	ID   string // content hash or generation number as string
}

func (d Dep) String() string { return d.Kind + ":" + d.ID }

// DepSet is the set of dependencies a query read while computing.
type DepSet map[Dep]struct{}

// Union returns a new set with the entries of both receivers.
func (a DepSet) Union(b DepSet) DepSet {
	out := make(DepSet, len(a)+len(b))
	for d := range a {
		out[d] = struct{}{}
	}
	for d := range b {
		out[d] = struct{}{}
	}
	return out
}

// Has reports membership.
func (a DepSet) Has(d Dep) bool { _, ok := a[d]; return ok }
