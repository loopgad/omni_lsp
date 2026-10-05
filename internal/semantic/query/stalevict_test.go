package query

import (
	"context"
	"errors"
	"testing"
)

// TestJ7_StaleEntryEvictedOnReadPath covers the read-side freshness gate
// (core.go:52-59): an entry whose snapshot revision fell behind the engine's
// expectation is dropped and its dep links unwired before the switch below
// ever sees it, so the query recomputes against the current revision instead
// of serving a value computed for an older world.
//
// Nothing reached this branch before: TestJ7 only exercises the write-side
// gate (a revision moving *while* a compute is in flight), and every other
// test keeps expectedRev and snapshotRev in step.
func TestJ7_StaleEntryEvictedOnReadPath(t *testing.T) {
	e := NewEngine(5) // engine now demands revision >= 5
	// Revision 0 is the only way to seed a memo while the engine already
	// expects a higher one: core.go:171 exempts rev==0 from the write-side
	// gate, so the entry lands and only the read-side gate can catch it.
	k := key("read-stale", "doc.go", 0)

	calls := 0
	compute := func(ctx context.Context, b Bindings) (any, DepSet, error) {
		calls++
		return "seeded", nil, nil
	}
	if _, err := e.Query(context.Background(), k, nil, compute); err != nil {
		t.Fatalf("seed revision-0 query: %v", err)
	}
	if calls != 1 {
		t.Fatalf("seed called compute %d times, want 1", calls)
	}

	// Do NOT raise the expectation here. InvalidateSnapshot evicts every entry
	// with snapshotRev <= rev (core.go:250), which would mask the read-side gate
	// entirely; expectedRev is already 5 from NewEngine(5), so the very next
	// query is the only thing that can evict this entry.
	r, err := e.Query(context.Background(), k, nil, compute)
	if err != nil {
		t.Fatalf("revision-5 query: %v", err)
	}
	if calls != 2 {
		t.Errorf("stale entry served from cache: compute called %d times, want 2", calls)
	}
	if r.Value != "seeded" {
		t.Errorf("value = %v, want the recomputed seeded", r.Value)
	}
}

// TestJ7_EvictionUnwiresDepLinks pins that dropping a stale entry also drops
// its reverse dep links. Leaving them behind makes a later Invalidate report
// hits for a key that no longer exists, which is exactly the kind of phantom
// bookkeeping that turns a correct invalidation into a silent no-op.
func TestJ7_EvictionUnwiresDepLinks(t *testing.T) {
	e := NewEngine(5)
	k := key("dep-stale", "doc.go", 0) // rev 0 passes the write-side gate

	// Seed the memo while depending on dep-a.
	if _, err := e.Query(context.Background(), k, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			return "seeded", DepSet{depFile("dep-a"): {}}, nil
		}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n := e.Invalidate(depFile("dep-a")); n != 1 {
		t.Fatalf("seed wiring: Invalidate(dep-a) = %d, want 1", n)
	}
	// Re-seed, since Invalidate just dropped it.
	if _, err := e.Query(context.Background(), k, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			return "seeded", DepSet{depFile("dep-a"): {}}, nil
		}); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	// The read-side gate now sweeps this entry and recomputes it against a
	// compute that declares no deps. If the sweep calls removeKeyFromDeps
	// (core.go:55) the reverse index is emptied; if it only deleted the entry,
	// the dead key would stay wired to dep-a forever and every later real
	// invalidation of dep-a would chase a key that no longer exists.
	if _, err := e.Query(context.Background(), k, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			return "rescanned", nil, nil
		}); err != nil {
		t.Fatalf("post-eviction query: %v", err)
	}
	if keys := e.depIndex[depFile("dep-a")]; len(keys) != 0 {
		t.Errorf("dep-a still wired to %d key(s) after the entry was swept: %v", len(keys), keys)
	}
}

// TestK3_StableErrorsAreErrorsIs pins that both exported sentinels survive
// wrapping. engine_wiring.go distinguishes Unknown/Unavailable envelopes from
// real failures with errors.Is, so a stable failure that arrives as a bare
// sentinel (rather than an *errors.Error carrying it) would be misclassified.
func TestK3_StableErrorsAreErrorsIs(t *testing.T) {
	e := NewEngine(1)
	k := key("wrap", "doc.go", 1)

	// A stable failure from the compute fn is cached and replayed verbatim,
	// which is the only way errors.Is can be checked on the hit path.
	sentinel := errors.New("backend says no")
	if _, err := e.Query(context.Background(), k, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			return "envelope", nil, sentinel
		}); err == nil {
		t.Fatal("seed: want a stable failure")
	}

	r, err := e.Query(context.Background(), k, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			t.Error("cached stable failure must not recompute")
			return "other", nil, nil
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("cache hit err = %v, want errors.Is match on the original sentinel", err)
	}
	if r.Value != "envelope" {
		t.Errorf("cache hit value = %v, want the envelope from the failing call", r.Value)
	}
}
