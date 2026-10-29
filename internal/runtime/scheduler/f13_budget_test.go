// Package scheduler — F13 memory budget manager tests.
package scheduler

import (
	"testing"
)

func TestBudgetManagerInitialState(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	if bm.State() != "normal" {
		t.Errorf("state = %q, want normal", bm.State())
	}
	if bm.Pressure() != 0 {
		t.Errorf("pressure = %f, want 0", bm.Pressure())
	}
}

func TestBudgetManagerRecordAndRelease(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	bm.SetLimit(CatSnapshots, 1000)
	bm.RecordUsage(CatSnapshots, 500)
	want := 0.5
	got := bm.CategoryPressure(CatSnapshots)
	if got < want-0.01 || got > want+0.01 {
		t.Errorf("category pressure = %f, want ~%f", got, want)
	}
	bm.ReleaseUsage(CatSnapshots, 500)
	got = bm.CategoryPressure(CatSnapshots)
	if got != 0 {
		t.Errorf("pressure after release = %f, want 0", got)
	}
}

func TestBudgetManagerThresholds(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	// Set ALL category limits to a small value so totalLimit is meaningful
	for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
		bm.SetLimit(cat, 1000)
	}
	bm.RecordUsage(CatSnapshots, 750)
	// totalUsed=750, totalLimit=8000 -> pressure=9.4% (normal)
	// CategoryPressure(CatSnapshots) = 750/1000 = 75% (evict threshold)
	if bm.CategoryPressure(CatSnapshots) < 0.74 || bm.CategoryPressure(CatSnapshots) > 0.76 {
		t.Errorf("snap pressure = %f", bm.CategoryPressure(CatSnapshots))
	}
	bm.RecordUsage(CatSnapshots, 200)
	// CategoryPressure = 950/1000 = 95% -> throttle
	if bm.CategoryPressure(CatSnapshots) < 0.94 || bm.CategoryPressure(CatSnapshots) > 0.96 {
		t.Errorf("snap pressure after += 200 = %f", bm.CategoryPressure(CatSnapshots))
	}
}

func TestBudgetManagerCanAllocate(t *testing.T) {
	// REPLACED (deliberate semantic cut): this test used to pin rejection
	// deferred to 100% pressure ("should allow 50 bytes at 90% total
	// pressure"), which contradicted goal.md §F13 — the >= 90% band is
	// "reject expensive work". CanAllocate now refuses any allocation that
	// would land total pressure at or above the High threshold (0.90 by
	// default); the band mapping itself is locked by
	// TestBudgetManagerPressureLadder.
	bm := NewBudgetManager(DefaultBudgetThresholds())
	for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
		bm.SetLimit(cat, 1000)
	}
	// totalUsed=6400, totalLimit=8000 -> 80%: inside the throttle band, still
	// admits allocations that stay below the 90% reject line.
	bm.RecordUsage(CatSnapshots, 6400)
	if !bm.CanAllocate(719) {
		t.Error("should admit an allocation landing at 88.9% (< High 0.90)")
	}
	if bm.CanAllocate(800) {
		t.Error("should reject an allocation landing exactly at the 90% reject line")
	}
	// Cross the line: at 90% total pressure nothing more is admitted.
	bm.RecordUsage(CatQueryCache, 800)
	if bm.CanAllocate(1) {
		t.Error("should reject any allocation at >= 90% total pressure")
	}
}

// TestBudgetManagerPressureLadder locks the goal.md §F13 band mapping:
//
//	< 70%   normal
//	70-80%  evict cold caches
//	80-90%  pause/throttle background work
//	>= 90%  reject expensive work / emergency eviction
//	100%    budget exhausted (critical)
//
// Before this ladder was enforced, State() shifted every band one tier up
// (80% -> evict, 90% -> throttle, rejection only at 100%) and the Soft(0.70)
// threshold was consumed by no branch at all.
func TestBudgetManagerPressureLadder(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
		bm.SetLimit(cat, 1000)
	}
	cases := []struct {
		total int64  // totalUsed out of totalLimit=8000
		state string // goal.md §F13 band
	}{
		{0, "normal"},
		{5520, "normal"},   // 69.0%
		{5600, "evict"},    // 70.0% exactly — the Soft tier must fire
		{6399, "evict"},    // 79.99%
		{6400, "throttle"}, // 80.0% exactly
		{7199, "throttle"}, // 89.99%
		{7200, "reject"},   // 90.0% exactly — the High tier rejects
		{7999, "reject"},   // 99.99%
		{8000, "critical"}, // 100% — exhausted
	}
	for _, tc := range cases {
		bm2 := NewBudgetManager(DefaultBudgetThresholds())
		for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
			bm2.SetLimit(cat, 1000)
		}
		fillUsage(bm2, tc.total)
		if got := bm2.State(); got != tc.state {
			t.Errorf("pressure %d/8000: State() = %q, want %q", tc.total, got, tc.state)
		}
	}
}

// fillUsage records total bytes spread across categories in 1000-byte chunks
// so no single category limit interferes with the total-pressure ladder.
func fillUsage(bm *BudgetManager, total int64) {
	for cat := CatDocumentText; cat <= CatRPCBuffers && total > 0; cat++ {
		n := total
		if n > 1000 {
			n = 1000
		}
		bm.RecordUsage(cat, n)
		total -= n
	}
}

func TestBudgetManagerCriticalState(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
		bm.SetLimit(cat, 1000)
	}
	// Push total pressure to critical (>=100%)
	bm.RecordUsage(CatSnapshots, 1000)
	bm.RecordUsage(CatQueryCache, 1000)
	bm.RecordUsage(CatSyntaxTrees, 1000)
	bm.RecordUsage(CatLineIndex, 1000)
	bm.RecordUsage(CatDocumentText, 1000)
	bm.RecordUsage(CatDynamicIndex, 1000)
	bm.RecordUsage(CatBackendRSS, 1000)
	bm.RecordUsage(CatRPCBuffers, 1000)
	if bm.State() != "critical" {
		t.Errorf("state = %q, want critical", bm.State())
	}
}

func TestBudgetManagerUsageSnapshot(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	bm.RecordUsage(CatQueryCache, 100)
	snap := bm.UsageSnapshot()
	if snap[CatQueryCache] != 100 {
		t.Errorf("snapshot[CatQueryCache] = %d, want 100", snap[CatQueryCache])
	}
}

func TestBudgetManagerDefaultThresholds(t *testing.T) {
	th := DefaultBudgetThresholds()
	if th.Soft != 0.70 {
		t.Errorf("soft = %f, want 0.70", th.Soft)
	}
	if th.Medium != 0.80 {
		t.Errorf("medium = %f, want 0.80", th.Medium)
	}
	if th.High != 0.90 {
		t.Errorf("high = %f, want 0.90", th.High)
	}
	if th.Critical != 1.0 {
		t.Errorf("critical = %f, want 1.0", th.Critical)
	}
}

func TestBudgetManagerZeroRelease(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	bm.SetLimit(CatSnapshots, 1000)
	bm.RecordUsage(CatSnapshots, 100)
	bm.ReleaseUsage(CatSnapshots, 9999)
	if bm.CategoryPressure(CatSnapshots) < 0 {
		t.Error("pressure should not go negative")
	}
}

func TestBudgetManagerMultipleCategories(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
		bm.SetLimit(cat, 1000)
	}
	bm.RecordUsage(CatSnapshots, 500)
	bm.RecordUsage(CatQueryCache, 750)
	if bm.CategoryPressure(CatSnapshots) < 0.49 || bm.CategoryPressure(CatSnapshots) > 0.51 {
		t.Errorf("snap pressure = %f", bm.CategoryPressure(CatSnapshots))
	}
	if bm.CategoryPressure(CatQueryCache) < 0.74 || bm.CategoryPressure(CatQueryCache) > 0.76 {
		t.Errorf("query pressure = %f", bm.CategoryPressure(CatQueryCache))
	}
}

// TestBudgetManagerTotalBudgetDrivesLadder pins the DEF-BUDWIRE hook: an
// explicit SetTotalBudget overrides the sum-of-category-limits denominator
// for Pressure, State and CanAllocate, while per-category limits keep
// bounding CategoryPressure. This is what lets the server's index wiring run
// the §F13 ladder against a configured memory budget instead of the
// constructor's 1 GiB-per-category defaults (which could never bind).
func TestBudgetManagerTotalBudgetDrivesLadder(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	bm.SetTotalBudget(1000)
	bm.RecordUsage(CatSnapshots, 800)
	if p := bm.Pressure(); p < 0.79 || p > 0.81 {
		t.Errorf("pressure against explicit total = %f, want 0.8", p)
	}
	// 800/1000 = 80% -> throttle band on the §F13 ladder.
	if got := bm.State(); got != "throttle" {
		t.Errorf("state at 80%% of explicit total = %q, want throttle", got)
	}
	// CanAllocate refuses any allocation landing at or above High (90%).
	if bm.CanAllocate(100) {
		t.Error("CanAllocate(100) would land at 90%% of the total; want refusal")
	}
	if !bm.CanAllocate(99) {
		t.Error("CanAllocate(99) stays below 90%% of the total; want admission")
	}
	// Category limits still bound their own categories: CatSnapshots keeps
	// its 1 GiB constructor default, so category pressure is ~0.
	if p := bm.CategoryPressure(CatSnapshots); p > 0.001 {
		t.Errorf("category pressure = %f, want ~0 (explicit total must not touch it)", p)
	}

	// Zero restores the historical denominator: the sum of category limits.
	bm.SetTotalBudget(0)
	if p := bm.Pressure(); p >= 0.001 {
		t.Errorf("pressure after clearing the total = %f, want ~0 (sum of 1 GiB limits)", p)
	}
	if !bm.CanAllocate(1 << 20) {
		t.Error("CanAllocate against 1 GiB-sum limits must admit 1 MiB")
	}
}

// TestBudgetManagerTotalBudgetDefaultUnchanged pins that managers built
// without SetTotalBudget keep the sum-of-limits semantics the existing
// ladder tests (TestBudgetManagerPressureLadder) depend on.
func TestBudgetManagerTotalBudgetDefaultUnchanged(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
		bm.SetLimit(cat, 1000)
	}
	bm.RecordUsage(CatSnapshots, 900)
	if p := bm.Pressure(); p < 0.1115 || p > 0.1135 {
		t.Errorf("pressure = %f, want 900/8000 = 0.1125? got wrong denominator", p)
	}
	if got := bm.State(); got != "normal" {
		t.Errorf("state = %q, want normal (900/8000 = 11.25%%)", got)
	}
}
