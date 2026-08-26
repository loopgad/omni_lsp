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
	bm := NewBudgetManager(DefaultBudgetThresholds())
	for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
		bm.SetLimit(cat, 1000)
	}
	// Fill all categories to near-critical total pressure
	bm.RecordUsage(CatSnapshots, 900)
	bm.RecordUsage(CatQueryCache, 900)
	bm.RecordUsage(CatSyntaxTrees, 900)
	bm.RecordUsage(CatLineIndex, 900)
	bm.RecordUsage(CatDocumentText, 900)
	bm.RecordUsage(CatDynamicIndex, 900)
	bm.RecordUsage(CatBackendRSS, 900)
	bm.RecordUsage(CatRPCBuffers, 900)
	// totalUsed=7200, totalLimit=8000 -> 90% -> can still allocate small amount
	if !bm.CanAllocate(50) {
		t.Error("should allow 50 bytes at 90% total pressure")
	}
	// 7200+300=7500/8000=93.75% < 100% -> still allows
	// Need to push above critical: use larger alloc
	if bm.CanAllocate(1000) {
		t.Error("should reject 1000 bytes at 90% total pressure")
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
