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
	e := a
	e.SnapshotInstance = 2
	if a.String() == e.String() {
		t.Fatal("different immutable snapshot instances must not collide")
	}
	f := a
	f.IndexGeneration = 3
	if a.String() == f.String() {
		t.Fatal("different persistent index generations must not collide")
	}
	d := a
	d.BackendEpoch = 2
	if a.String() == d.String() {
		t.Fatal("different backend epochs must not collide")
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

// A canceled leader cannot turn a retryable request failure into stable
// negative knowledge shared by later callers on the same snapshot.
func TestJ4_CanceledComputeIsNotMemoized(t *testing.T) {
	for _, failed := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(failed.Error(), func(t *testing.T) {
			e := NewEngine(1)
			k := key("hover", "canceled", 1)
			calls := 0
			_, err := e.Query(context.Background(), k, nil,
				func(context.Context, Bindings) (any, DepSet, error) {
					calls++
					return nil, nil, fmt.Errorf("backend: %w", failed)
				})
			if !errors.Is(err, failed) {
				t.Fatalf("first query: got %v, want %v", err, failed)
			}
			res, err := e.Query(context.Background(), k, nil,
				func(context.Context, Bindings) (any, DepSet, error) {
					calls++
					return "recovered", nil, nil
				})
			if err != nil || res.Value != "recovered" || calls != 2 {
				t.Fatalf("retry: value=%v err=%v calls=%d", res.Value, err, calls)
			}
		})
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

func TestJ6_LeaderCancelKeepsSharedComputeForRemainingWaiter(t *testing.T) {
	e := NewEngine(0)
	k := key("sf", "leader-cancel", 1)
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	release := make(chan struct{})
	started := make(chan struct{})
	sharedCanceled := make(chan struct{})
	fn := func(ctx context.Context, _ Bindings) (any, DepSet, error) {
		close(started)
		select {
		case <-release:
			return "shared result", nil, nil
		case <-ctx.Done():
			close(sharedCanceled)
			return nil, nil, ctx.Err()
		}
	}

	type outcome struct {
		result Result
		err    error
	}
	leaderDone := make(chan outcome, 1)
	go func() {
		res, err := e.Query(leaderCtx, k, nil, fn)
		leaderDone <- outcome{res, err}
	}()
	<-started
	waiterDone := make(chan outcome, 1)
	go func() {
		res, err := e.Query(context.Background(), k, nil, fn)
		waiterDone <- outcome{res, err}
	}()
	waitForQueryWaiters(t, e, k.String(), 2)

	cancelLeader()
	select {
	case got := <-leaderDone:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("leader err = %v, want context.Canceled", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled leader did not return independently")
	}

	close(release)
	select {
	case got := <-waiterDone:
		if got.err != nil || got.result.Value != "shared result" {
			t.Fatalf("remaining waiter = (%v, %v), want (shared result, nil)", got.result.Value, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("remaining waiter did not receive the shared result")
	}
	select {
	case <-sharedCanceled:
		t.Fatal("leader cancellation canceled shared work while another waiter remained")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestJ6_LastWaiterCancelStopsSharedCompute(t *testing.T) {
	e := NewEngine(0)
	k := key("sf", "all-cancel", 1)
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	started := make(chan struct{})
	sharedCanceled := make(chan struct{})
	fn := func(ctx context.Context, _ Bindings) (any, DepSet, error) {
		close(started)
		<-ctx.Done()
		close(sharedCanceled)
		return nil, nil, ctx.Err()
	}
	done1 := make(chan error, 1)
	go func() { _, err := e.Query(ctx1, k, nil, fn); done1 <- err }()
	<-started
	done2 := make(chan error, 1)
	go func() { _, err := e.Query(ctx2, k, nil, fn); done2 <- err }()
	waitForQueryWaiters(t, e, k.String(), 2)
	cancel1()
	cancel2()
	for i, done := range []chan error{done1, done2} {
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("waiter %d err = %v, want context.Canceled", i, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("waiter %d remained blocked", i)
		}
	}
	select {
	case <-sharedCanceled:
	case <-time.After(time.Second):
		t.Fatal("shared compute context was not canceled after its last waiter left")
	}
}

func TestJ6_LastWaiterCancelAllowsFreshSameKey(t *testing.T) {
	e := NewEngine(0)
	k := key("sf", "fresh-after-cancel", 1)
	firstStarted := make(chan struct{})
	sharedCanceled := make(chan struct{})
	releaseOldCompute := make(chan struct{})
	var releaseOldOnce sync.Once
	releaseOld := func() { releaseOldOnce.Do(func() { close(releaseOldCompute) }) }
	defer releaseOld()
	oldFn := func(ctx context.Context, _ Bindings) (any, DepSet, error) {
		close(firstStarted)
		<-ctx.Done()
		close(sharedCanceled)
		<-releaseOldCompute // Keep the abandoned computation in flight deliberately.
		return nil, nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan error, 1)
	go func() {
		_, err := e.Query(ctx, k, nil, oldFn)
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first computation did not start")
	}

	e.lock()
	oldCall := e.inflight[k.String()]
	e.unlock()
	if oldCall == nil {
		t.Fatal("first computation was not registered")
	}

	cancel()
	select {
	case err := <-firstDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first waiter err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter did not return")
	}
	select {
	case <-sharedCanceled:
	case <-time.After(time.Second):
		t.Fatal("last waiter did not cancel shared computation")
	}

	freshStarted := make(chan struct{})
	freshDone := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := e.Query(context.Background(), k, nil,
			func(context.Context, Bindings) (any, DepSet, error) {
				close(freshStarted)
				return "fresh", nil, nil
			})
		freshDone <- struct {
			result Result
			err    error
		}{result, err}
	}()

	var fresh struct {
		result Result
		err    error
	}
	select {
	case fresh = <-freshDone:
	case <-time.After(time.Second):
		releaseOld()
		<-oldCall.done
		<-freshDone
		t.Fatal("fresh same-key request joined canceled computation and did not complete")
	}
	select {
	case <-freshStarted:
	default:
		t.Fatal("fresh request completed without running its fresh computation")
	}
	if fresh.err != nil || fresh.result.Value != "fresh" {
		t.Fatalf("fresh request = (%v, %v), want (fresh, nil)", fresh.result.Value, fresh.err)
	}

	releaseOld()
	select {
	case <-oldCall.done:
	case <-time.After(time.Second):
		t.Fatal("abandoned computation did not finish cleanup")
	}
	// The detached old computation must not delete or overwrite the newer memo.
	var recomputed atomic.Bool
	got, err := e.Query(context.Background(), k, nil,
		func(context.Context, Bindings) (any, DepSet, error) {
			recomputed.Store(true)
			return "wrong", nil, nil
		})
	if err != nil || got.Value != "fresh" || recomputed.Load() {
		t.Fatalf("memo after old compute cleanup = (%v, %v), recomputed=%v; want fresh cached result", got.Value, err, recomputed.Load())
	}
}

func TestJ6_QueryLockWaitHonorsCancellation(t *testing.T) {
	e := NewEngine(0)
	e.lock()
	defer e.unlock()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &queryObservedContext{Context: base, observed: make(chan struct{}), observeAt: 2}
	done := make(chan error, 1)
	go func() {
		_, err := e.Query(ctx, key("lock", "wait", 1), nil,
			func(context.Context, Bindings) (any, DepSet, error) { return "ran", nil, nil })
		done <- err
	}()
	select {
	case <-ctx.observed:
	case <-time.After(time.Second):
		t.Fatal("query never reached the lock wait")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Query err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("query remained blocked on the engine lock after cancellation")
	}
}

func TestJ6_PanicCompletesInflightWaitersAndAllowsRetry(t *testing.T) {
	e := NewEngine(0)
	k := key("sf", "panic", 1)
	started := make(chan struct{})
	release := make(chan struct{})
	fn := func(context.Context, Bindings) (any, DepSet, error) {
		close(started)
		<-release
		panic("compute boom")
	}
	leaderPanic := make(chan any, 1)
	go func() {
		defer func() { leaderPanic <- recover() }()
		_, _ = e.Query(context.Background(), k, nil, fn)
	}()
	<-started
	waiterDone := make(chan error, 1)
	go func() {
		_, err := e.Query(context.Background(), k, nil, fn)
		waiterDone <- err
	}()
	waitForQueryWaiters(t, e, k.String(), 2)
	close(release)
	select {
	case got := <-leaderPanic:
		if got != "compute boom" {
			t.Fatalf("leader panic = %v, want original panic", got)
		}
	case <-time.After(time.Second):
		t.Fatal("leader did not observe compute panic after cleanup")
	}
	select {
	case err := <-waiterDone:
		if err == nil || !strings.Contains(err.Error(), "compute panicked") {
			t.Fatalf("waiter err = %v, want a panic error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("panic left an in-flight waiter stranded")
	}
	res, err := e.Query(context.Background(), k, nil,
		func(context.Context, Bindings) (any, DepSet, error) { return "retry", nil, nil })
	if err != nil || res.Value != "retry" {
		t.Fatalf("retry after panic = (%v, %v), want (retry, nil)", res.Value, err)
	}
}

func waitForQueryWaiters(t *testing.T, e *Engine, key string, want int64) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		e.lock()
		call := e.inflight[key]
		got := int64(0)
		if call != nil {
			got = call.waiters.Load()
		}
		e.unlock()
		if got == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("in-flight waiters = %d, want %d", got, want)
		case <-ticker.C:
		}
	}
}

type queryObservedContext struct {
	context.Context
	observed  chan struct{}
	observeAt int32
	calls     atomic.Int32
	once      sync.Once
}

func (c *queryObservedContext) Err() error {
	if c.calls.Add(1) == c.observeAt {
		c.once.Do(func() { close(c.observed) })
	}
	return c.Context.Err()
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
	// Raise the expectation while compute is parked.
	e.InvalidateSnapshot(5)
	close(block)
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

// TestJ4_TransientErrorCarriesComputedValue pins the contract §B6 recovery
// depends on: when a compute fails transiently AFTER producing a value, the
// value travels back with the error so the caller can project an honest
// Unknown/Unavailable envelope without re-invoking the backend. The failed
// entry stays uncached (transient retry, see TestJ4_StateTransitions).
func TestJ4_TransientErrorCarriesComputedValue(t *testing.T) {
	e := NewEngine(0)
	k := key("k", "tv", 1)
	calls := 0
	r, err := e.Query(context.Background(), k, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			calls++
			return "envelope", nil, Transient(errors.New("upstream refused"))
		})
	if !isTransient(err) {
		t.Fatalf("want transient error, got %v", err)
	}
	if r.Value != "envelope" {
		t.Fatalf("transient result value = %v, want computed envelope carried with err", r.Value)
	}
	// The failed entry must not be memoized: the next query recomputes.
	r2, err2 := e.Query(context.Background(), k, nil,
		func(ctx context.Context, b Bindings) (any, DepSet, error) {
			calls++
			return "recovered", nil, nil
		})
	if err2 != nil || r2.Value != "recovered" || calls != 2 {
		t.Fatalf("retry: calls=%d value=%v err=%v", calls, r2.Value, err2)
	}
}

// TestJ4_StableErrorCarriesValueOnCacheHit pins the same contract for a
// non-transient failure. A stable failure is memoized, so the second call
// returns from cache rather than from the leader's call.res. Both paths must
// agree: dropping the value on the cache-hit path made two calls of the same
// failing query disagree on Result.Value and left the evidence ring empty on
// the second observation, so handlers could not record why they refused.
func TestJ4_StableErrorCarriesValueOnCacheHit(t *testing.T) {
	e := NewEngine(0)
	k := key("k", "sv", 1)
	calls := 0
	compute := func(ctx context.Context, b Bindings) (any, DepSet, error) {
		calls++
		return "envelope", nil, errors.New("invalid request")
	}
	r, err := e.Query(context.Background(), k, nil, compute)
	if err == nil {
		t.Fatal("want stable failure, got nil")
	}
	if isTransient(err) {
		t.Fatalf("want non-transient error, got %v", err)
	}
	if r.Value != "envelope" {
		t.Fatalf("first call value = %v, want computed envelope carried with err", r.Value)
	}

	r2, err2 := e.Query(context.Background(), k, nil, compute)
	if err2 == nil || isTransient(err2) {
		t.Fatalf("cache hit: err = %v, want the same stable failure", err2)
	}
	if r2.Value != "envelope" {
		t.Fatalf("cache hit value = %v, want the same envelope as the first call", r2.Value)
	}
	if calls != 1 {
		t.Errorf("stable failure recomputed: %d calls, want 1 (memoized)", calls)
	}
}
