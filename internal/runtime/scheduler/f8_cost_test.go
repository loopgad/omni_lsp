package scheduler

// Guards for goal.md §F8 wiring, in the fail-closed spirit of the PERF-2 /
// PERF-3 gate guards (internal/conformance): the default must be safe even
// when a caller forgets to classify, and the per-class caps must genuinely
// bind execution concurrency — not merely exist in config.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// TestF8_UnclassifiedRequestFallsIntoDefaultClass pins the fail-closed
// default: the zero value of CostClass is CostUnset, and Submit must resolve
// it to DefaultCostClass before admission. If the zero value ever leaked into
// the pool as CostTiny (the pre-wiring zero value and the widest lane), an
// unclassified flood could occupy every worker; if it stayed CostUnset, the
// request would sit in a lane no worker scans and never run.
func TestF8_UnclassifiedRequestFallsIntoDefaultClass(t *testing.T) {
	s := New(DefaultConfig())
	s.Start(context.Background())
	defer s.Shutdown()

	req := &Request{
		RequestID:  "f8-unclassified",
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
			return "done", nil
		},
	} // CostClass deliberately left at its zero value
	if res := s.Submit(req); res != Admitted {
		t.Fatalf("Submit = %v, want Admitted", res)
	}
	select {
	case <-req.Result:
	case <-time.After(2 * time.Second):
		t.Fatal("unclassified request never completed")
	}
	if req.CostClass != DefaultCostClass {
		t.Fatalf("unclassified request resolved to %v, want DefaultCostClass (%v): zero value must not leak into the pool", req.CostClass, DefaultCostClass)
	}
	if req.CostClass == CostUnset || req.CostClass == CostTiny {
		t.Fatalf("resolved class %v is unsafe: not the fail-closed default", req.CostClass)
	}

	// Explicit classification survives admission untouched: the default is a
	// fallback, not an override.
	for _, c := range []CostClass{CostTiny, CostSmall, CostMedium, CostLarge, CostVeryLarge} {
		explicit := &Request{
			RequestID:  identity.RequestID("f8-explicit-" + c.String()),
			CostClass:  c,
			Priority:   PriorityCompletion,
			EnqueuedAt: time.Now(),
			Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
				return nil, nil
			},
		}
		if res := s.Submit(explicit); res != Admitted {
			t.Fatalf("Submit explicit %v = %v, want Admitted", c, res)
		}
		select {
		case <-explicit.Result:
		case <-time.After(2 * time.Second):
			t.Fatalf("explicit %v request never completed", c)
		}
		if explicit.CostClass != c {
			t.Fatalf("explicit class %v was rewritten to %v", c, explicit.CostClass)
		}
	}
}

// TestF8_ClassifyCostMethodFamilies pins the dispatch-point mapping: method
// families land in the §F8 class matching their documented cost, and unknown
// methods fall to the fail-closed default rather than a wide lane.
func TestF8_ClassifyCostMethodFamilies(t *testing.T) {
	cases := []struct {
		method string
		want   CostClass
	}{
		{"textDocument/hover", CostTiny},
		{"textDocument/completion", CostTiny},
		{"textDocument/signatureHelp", CostTiny},
		{"omnilsp/status", CostTiny},
		{"textDocument/definition", CostSmall},
		{"textDocument/documentSymbol", CostSmall},
		{"textDocument/diagnostic", CostSmall},
		{"textDocument/rename", CostMedium},
		{"textDocument/semanticTokens/full", CostMedium},
		{"textDocument/references", CostLarge},
		{"workspace/symbol", CostLarge},
		{"omnilsp/reindex", CostVeryLarge},
		{"textDocument/unknownFutureMethod", DefaultCostClass},
		{"", DefaultCostClass},
	}
	for _, tc := range cases {
		if got := ClassifyCost(tc.method); got != tc.want {
			t.Errorf("ClassifyCost(%q) = %v, want %v", tc.method, got, tc.want)
		}
	}
}

// TestF8_PerClassCapActuallyLimitsConcurrency guards both halves of §F8's
// "Large work MUST have stricter concurrency caps":
//
//  1. On the default (production) path — cmd/omnilsp never sets ClassCaps —
//     the derived caps are genuinely stratified: the interactive lanes span
//     the pool, while DefaultCostClass/Large/VeryLarge are held below it.
//  2. At runtime, a class at its cap cannot exceed it no matter how many
//     requests queue, and workers never block on class capacity: a Tiny
//     request stays live while Large lanes saturate (F7).
func TestF8_PerClassCapActuallyLimitsConcurrency(t *testing.T) {
	// (1) Derived caps on the default config are wired into the built pool
	// and stratified strictly by cost.
	s0 := New(DefaultConfig())
	if got := cap(s0.classTokens[CostTiny]); got != DefaultConfig().MaxConcurrent {
		t.Errorf("derived Tiny cap = %d, want the full pool (%d)", got, DefaultConfig().MaxConcurrent)
	}
	if got := cap(s0.classTokens[DefaultCostClass]); got >= DefaultConfig().MaxConcurrent {
		t.Errorf("derived DefaultCostClass cap = %d, want < pool (%d): an unclassified flood must not be able to occupy every worker", got, DefaultConfig().MaxConcurrent)
	}
	largeCap := cap(s0.classTokens[CostLarge])
	veryLargeCap := cap(s0.classTokens[CostVeryLarge])
	if largeCap >= DefaultConfig().MaxConcurrent {
		t.Errorf("derived Large cap = %d, want < pool (%d): §F8 MUST", largeCap, DefaultConfig().MaxConcurrent)
	}
	if veryLargeCap >= largeCap {
		t.Errorf("derived VeryLarge cap = %d, want < Large cap (%d)", veryLargeCap, largeCap)
	}

	// (2) Runtime enforcement: six long-running Large requests with a cap of
	// two. Exactly two may execute concurrently; the rest stay queued.
	cfg := DefaultConfig()
	cfg.ClassCaps = map[CostClass]int{CostLarge: 2}
	s := New(cfg)
	s.Start(context.Background())
	defer s.Shutdown()

	var inFlight, maxObserved atomic.Int64
	started := make(chan struct{}, 6)
	release := make(chan struct{})
	largeExec := func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
		cur := inFlight.Add(1)
		for {
			m := maxObserved.Load()
			if cur <= m || maxObserved.CompareAndSwap(m, cur) {
				break
			}
		}
		started <- struct{}{}
		<-release
		inFlight.Add(-1)
		return nil, nil
	}
	larges := make([]*Request, 6)
	for i := range larges {
		larges[i] = &Request{
			RequestID:  identity.RequestID("f8-large-" + string(rune('a'+i))),
			CostClass:  CostLarge,
			Priority:   PriorityReferences,
			EnqueuedAt: time.Now(),
			Execute:    largeExec,
		}
		if res := s.Submit(larges[i]); res != Admitted {
			t.Fatalf("Submit large %d = %v, want Admitted", i, res)
		}
	}

	// Wait for the two admitted executions, then give any over-admission a
	// fair window to show itself.
	<-started
	<-started
	time.Sleep(150 * time.Millisecond)
	if got := maxObserved.Load(); got != 2 {
		t.Fatalf("Large class ran %d requests concurrently, want exactly the cap 2: per-class cap is not enforced", got)
	}

	// Liveness cross-check (F7): workers must not block on class capacity —
	// a Tiny request completes while the Large lane is saturated.
	tiny := &Request{
		RequestID:  "f8-tiny-during-saturation",
		CostClass:  CostTiny,
		Priority:   PriorityCompletion,
		EnqueuedAt: time.Now(),
		Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
			return "tiny", nil
		},
	}
	if res := s.Submit(tiny); res != Admitted {
		t.Fatalf("Submit tiny = %v, want Admitted", res)
	}
	select {
	case res := <-tiny.Result:
		if res.Err != nil {
			t.Fatalf("tiny request failed while large at cap: %v", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Tiny request starved while Large lane sat at its cap: workers are blocking on class capacity instead of picking eligible work")
	}

	close(release)
	for i, req := range larges {
		select {
		case res := <-req.Result:
			if res.Err != nil {
				t.Fatalf("large %d terminated with error: %v", i, res.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("large %d never completed after release: staged/capped work stranded", i)
		}
	}
	if got := maxObserved.Load(); got != 2 {
		t.Fatalf("Large class peaked at %d concurrent executions, want exactly 2", got)
	}
}
