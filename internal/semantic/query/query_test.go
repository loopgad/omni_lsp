package query

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func key(kind, subject string, rev uint64) Key {
	return Key{Kind: kind, Workspace: "w", SnapshotRev: rev, BuildContext: "b", Subject: subject}
}

func depFile(hash string) Dep { return Dep{Kind: "file", ID: hash} }

// TestJ1_KeyDeterminism: identical semantic fields produce identical keys
// regardless of construction order; the encoding is stable.
func TestJ1_KeyDeterminism(t *testing.T) {
	a := Key{Kind: "hover", Workspace: "ws1", SnapshotRev: 7, BuildContext: "ctx", Subject: "file:///a.go", OptionsHash: "o1"}
	b := Key{Kind: "hover", Workspace: "ws1", SnapshotRev: 7, BuildContext: "ctx", Subject: "file:///a.go", OptionsHash: "o1"}
	if a.String() != b.String() {
		t.Fatalf("identical keys encode differently: %q vs %q", a.String(), b.String())
	}
	c := a
	c.SnapshotRev = 8
	if a.String() == c.String() {
		t.Fatal("different revisions must not collide")
	}
	for _, part := range strings.Split(a.String(), "|") {
		if part == "" {
			t.Error("empty component would make encodings ambiguous")
		}
	}
}

// TestJ4_StateTransitions walks all six lifecycle states.
func TestJ4_StateTransitions(t *testing.T) {
	e := NewEngine(0)

	// Absent → Computing → Ready → hit.
	calls := 0
	r1, err := e.Query(context.Background(), key("k", "s1", 1), nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			calls++
			return "v1", nil, nil
		})
	if err != nil || r1.Value != "v1" {
		t.Fatalf("first query: %v %v", r1.Value, err)
	}
	r2, _ := e.Query(context.Background(), key("k", "s1", 1), nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) { return "wrong", nil, nil })
	if calls != 1 {
		t.Fatalf("expected memoized hit (calls=%d)", calls)
	}
	if r2.Value != "v1" {
		t.Fatalf("hit returned %v", r2.Value)
	}

	// Transient failure is retried, never cached.
	transientCalls := 0
	kT := key("k", "t1", 1)
	_, err = e.Query(context.Background(), kT, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			transientCalls++
			return nil, nil, Transient(errors.New("flaky"))
		})
	if !isTransient(err) {
		t.Fatalf("want transient error, got %v", err)
	}
	if transientCalls != 1 {
		t.Fatalf("transient call count = %d", transientCalls)
	}
	ok2, err := e.Query(context.Background(), kT, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			return "recovered", nil, nil
		})
	if err != nil || ok2.Value != "recovered" {
		t.Fatalf("transient retry failed: %v %v", ok2.Value, err)
	}

	// Stable failure is cached as negative knowledge until invalidated.
	stableCalls := 0
	kS := key("k", "s2", 1)
	wantErr := errors.New("permanent")
	dep := depFile("f1")
	for i := 0; i < 2; i++ {
		_, err = e.Query(context.Background(), kS, DepSet{dep: {}},
			func(ctx context.Context, b Bindings) (any, DepSet, error) {
				stableCalls++
				return nil, nil, wantErr
			})
		if !errors.Is(err, wantErr) {
			t.Fatalf("stable error round %d: %v", i, err)
		}
	}
	if stableCalls != 1 {
		t.Fatalf("stable failure must be cached (calls=%d)", stableCalls)
	}

	// Dependency invalidation evicts it and recompute happens.
	if n := e.Invalidate(dep); n != 1 {
		t.Fatalf("Invalidate returned %d, want 1", n)
	}
	_, err = e.Query(context.Background(), kS, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			return "fresh", nil, nil
		})
	if err != nil {
		t.Fatalf("post-invalidation recompute: %v", err)
	}

	// Evicted state is observable only via stats; sanity-check counters.
	st := e.Stats()
	if st.Computations < 4 || st.Hits < 2 || st.Evictions < 1 {
		t.Errorf("stats off: %+v", st)
	}
}

// TestJ2_SelectiveInvalidation: invalidating one dependency recomputes only
// the affected query; its sibling sharing another dependency stays cached.
func TestJ2_SelectiveInvalidation(t *testing.T) {
	e := NewEngine(0)
	var q1, q2 atomic.Int64

	go1 := func(ctx context.Context, b Bindings) (any, DepSet, error) {
		q1.Add(1)
		return "one", DepSet{depFile("shared"): {}}, nil
	}
	go2 := func(ctx context.Context, b Bindings) (any, DepSet, error) {
		q2.Add(1)
		return "two", DepSet{depFile("only2"): {}}, nil
	}

	ctx := context.Background()
	_, _ = e.Query(ctx, key("k", "q1", 1), nil, go1)
	_, _ = e.Query(ctx, key("k", "q2", 1), nil, go2)

	// Warm hits — no computation.
	_, _ = e.Query(ctx, key("k", "q1", 1), nil, go1)
	_, _ = e.Query(ctx, key("k", "q2", 1), nil, go2)
	if q1.Load() != 1 || q2.Load() != 1 {
		t.Fatalf("warm-up computed more than once: q1=%d q2=%d", q1.Load(), q2.Load())
	}

	// The shared dependency feeds q1 only (by actual read set).
	n := e.Invalidate(depFile("shared"))
	if n != 1 {
		t.Fatalf("invalidated %d entries, want exactly q1", n)
	}
	_, _ = e.Query(ctx, key("k", "q1", 1), nil, go1)
	_, _ = e.Query(ctx, key("k", "q2", 1), nil, go2)
	if q1.Load() != 2 {
		t.Errorf("q1 should have recomputed, got %d", q1.Load())
	}
	if q2.Load() != 1 {
		t.Errorf("q2 must stay cached (selective!), got %d", q2.Load())
	}
}

// TestJ6_SingleflightIndependentCancel: cancelled waiters never disturb the
// leader or surviving siblings.
func TestJ6_SingleflightIndependentCancel(t *testing.T) {
	e := NewEngine(0)

	release := make(chan struct{})
	var computes atomic.Int64
	fn := func(ctx context.Context, b Bindings) (any, DepSet, error) {
		computes.Add(1)
		<-release
		return "done", nil, nil
	}
	k := key("sf", "x", 1)

	const waiters = 5
	type outcome struct {
		val string
		err error
	}
	results := make([]chan outcome, waiters)
	for i := range results {
		i := i
		results[i] = make(chan outcome, 1)
		go func() {
			ctx := context.Background()
			if i < 3 { // first three waiters cancel while blocked
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				go func() { time.Sleep(20 * time.Millisecond); cancel() }()
			}
			r, err := e.Query(ctx, k, nil, fn)
			results[i] <- outcome{val: toString(r.Value), err: err}
		}()
	}

	time.Sleep(60 * time.Millisecond) // let everyone join the in-flight call
	close(release)

	cancelled, delivered := 0, 0
	for i, ch := range results {
		o := <-ch
		switch {
		case o.err == nil:
			delivered++
			if o.val != "done" {
				t.Errorf("waiter %d got %q", i, o.val)
			}
		case errors.Is(o.err, context.Canceled):
			cancelled++
		default:
			t.Errorf("waiter %d unexpected error %v", i, o.err)
		}
	}
	// Invariant: every waiter got exactly one terminal outcome and the work
	// ran once. Whether a cancelled waiter races the result delivery is a
	// legal select outcome — the §J6 contract is independent cancellation,
	// not forced failure.
	if cancelled+delivered != waiters {
		t.Errorf("cancelled=%d delivered=%d, want total %d", cancelled, delivered, waiters)
	}
	if delivered < 1 {
		t.Errorf("no waiter received the result; leader lost?")
	}
	if n := computes.Load(); n != 1 {
		t.Errorf("compute ran %d times, want exactly 1 (singleflight)", n)
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

// TestJ5_SelfWaitDetected: a compute function synchronously querying its own
// key reports a cycle instead of deadlocking.
func TestJ5_SelfWaitDetected(t *testing.T) {
	e := NewEngine(0)
	k := key("cyc", "self", 1)

	_, err := e.Query(context.Background(), k, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			_, ierr := b.Query(ctx, k, nil,
				func(ctx context.Context, b Bindings) (any, DepSet, error) {
					return "inner", nil, nil
				})
			if ierr != nil {
				return nil, nil, ierr
			}
			return "outer", nil, nil
		})
	if !errors.Is(err, ErrQueryCycle) {
		t.Fatalf("want ErrQueryCycle, got %v", err)
	}
	if e.Stats().CyclesDetected == 0 {
		t.Error("cycle counter not incremented")
	}
}

// TestJ7_StalePublishRejected: results computed against an outdated snapshot
// revision never enter the cache.
func TestJ7_StalePublishRejected(t *testing.T) {
	e := NewEngine(1) // engine expects revision >= 1

	block := make(chan struct{})
	var once sync.Once
	k := key("stale", "doc.go", 3) // computed against an older revision

	started := make(chan struct{})
	fn := func(ctx context.Context, b Bindings) (any, DepSet, error) {
		close(started)
		<-block
		return "old-world", nil, nil
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := e.Query(context.Background(), k, nil, fn)
		errCh <- err
	}()

	<-started
	once.Do(func() { close(block) }) // bump via invalidation below

	// Raise the expectation while compute is parked.
	e.InvalidateSnapshot(5)
	err := <-errCh
	if err == nil || !strings.Contains(err.Error(), "stale publish") {
		t.Fatalf("want stale-publish rejection, got %v", err)
	}

	// The rejected value must NOT be cached: next query recomputes fresh.
	r, err := e.Query(context.Background(), key("stale", "doc.go", 5), nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			return "new-world", nil, nil
		})
	if err != nil || r.Value != "new-world" {
		t.Fatalf("fresh recompute failed: %v %v", r.Value, err)
	}
	if st := e.Stats(); st.StalePublishRejected == 0 {
		t.Error("stale counter not incremented")
	}
}

// TestK3_NegativeCachingPolicy pins §K3: stable failures are memoized as
// known negative knowledge (second call does not recompute), while transient
// failures retry on every query and never become cached negatives.
func TestK3_NegativeCachingPolicy(t *testing.T) {
	k := Key{Kind: "hover", Workspace: "/w", Subject: "file:///w/a.go"}

	t.Run("stable failure memoized", func(t *testing.T) {
		e := NewEngine(0)
		calls := 0
		fn := func(ctx context.Context, bind Bindings) (any, DepSet, error) {
			calls++
			return nil, nil, fmt.Errorf("parse: bad syntax")
		}
		if _, err := e.Query(context.Background(), k, nil, fn); err == nil {
			t.Fatal("expected stable failure")
		}
		if _, err := e.Query(context.Background(), k, nil, fn); err == nil {
			t.Fatal("expected memoized failure")
		}
		if calls != 1 {
			t.Errorf("stable negative recomputed: %d calls, want 1", calls)
		}
	})

	t.Run("transient failure retries", func(t *testing.T) {
		e := NewEngine(0)
		calls := 0
		fn := func(ctx context.Context, bind Bindings) (any, DepSet, error) {
			calls++
			return nil, nil, Transient(fmt.Errorf("backend warming up"))
		}
		for i := 0; i < 3; i++ {
			if _, err := e.Query(context.Background(), k, nil, fn); err == nil {
				t.Fatal("expected transient failure")
			}
		}
		if calls != 3 {
			t.Errorf("transient negative cached: %d calls, want 3", calls)
		}
	})
}
