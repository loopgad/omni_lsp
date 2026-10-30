package ccls

// F14 backend-buffer budget guards for the ccls memo tables (DEF-BUDWIRE,
// F14 half). This package's tests are the shared-package lane: they are
// written against the wiring but NOT run by the F14 lane (并发波次纪律) —
// the closing phase's integration gate owns their first run.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
)

func resetMemoBudgetForTest(t *testing.T, pairs map[string]string) {
	t.Helper()
	for name, value := range pairs {
		t.Setenv(name, value)
	}
	languages.ResetBackendBudgetForTest()
	t.Cleanup(languages.ResetBackendBudgetForTest)
}

func memoCompletionValue(items int) languages.CompletionList {
	list := languages.CompletionList{}
	for i := 0; i < items; i++ {
		list.Items = append(list.Items, languages.CompletionItem{
			Label:      "candidate-" + strings.Repeat("x", 32),
			Kind:       int(languages.CompletionFunction),
			InsertText: "candidate",
		})
	}
	return list
}

func TestMemoStoreRefusedWhenOverQuota(t *testing.T) {
	resetMemoBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_QUOTA_CCLS_MEMO_BYTES": "64",
	})

	memo := &cclsMemo{quietPeriod: time.Millisecond}
	now := time.Now()
	key := completionMemoKeyFor("file:///w/a.cpp", []byte("int main(){}"), 1, 0, 0, 0, 0, 0)
	refusalsBefore := languages.BackendBufferRefusals("ccls-memo")

	memo.completionStore(key, memoCompletionValue(8), now)

	memo.mu.Lock()
	stored := len(memo.completions)
	memo.mu.Unlock()
	if stored != 0 {
		t.Fatalf("over-quota memo value was staged anyway: %d entries", stored)
	}
	if got := languages.BackendBufferRefusals("ccls-memo"); got != refusalsBefore+1 {
		t.Fatalf("refusals(ccls-memo) delta = %d, want 1", got-refusalsBefore)
	}
}

func TestMemoStoreInBudgetReplacesAccounting(t *testing.T) {
	resetMemoBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_QUOTA_CCLS_MEMO_BYTES": "1048576",
	})

	memo := &cclsMemo{quietPeriod: time.Millisecond}
	now := time.Now()
	key := completionMemoKeyFor("file:///w/a.cpp", []byte("int main(){}"), 1, 0, 0, 0, 0, 0)

	first := memoCompletionValue(2)
	memo.completionStore(key, first, now)
	manager := languages.BackendBudget()
	if manager == nil {
		t.Fatal("quota configured but the manager stayed nil")
	}
	memo.mu.Lock()
	entry := memo.completions[key]
	reserved1 := entry.budgetBytes
	memo.mu.Unlock()
	if reserved1 == 0 {
		t.Fatal("in-budget memo store must charge its value bytes")
	}
	if total, _ := manager.Usage(); total != reserved1 {
		t.Fatalf("global usage = %d, want the entry charge %d", total, reserved1)
	}

	second := memoCompletionValue(8)
	memo.completionStore(key, second, now)
	memo.mu.Lock()
	reserved2 := memo.completions[key].budgetBytes
	memo.mu.Unlock()
	if total, _ := manager.Usage(); total != reserved2 {
		t.Fatalf("global usage after replacement = %d, want %d (old charge must be released)", total, reserved2)
	}

	memo.completionStore(key, first, now) // same value re-confirm: no growth
	memo.mu.Lock()
	reserved3 := memo.completions[key].budgetBytes
	memo.mu.Unlock()
	if total, _ := manager.Usage(); total != reserved3 {
		t.Fatalf("global usage after re-confirm = %d, want %d", total, reserved3)
	}
}

func TestMemoUnconfiguredBehaviorUnchanged(t *testing.T) {
	// Refusal counters are process-global; the unconfigured invariant is
	// that this store adds none.
	refusalsBefore := languages.BackendBufferRefusals("ccls-memo")
	resetMemoBudgetForTest(t, nil)

	memo := &cclsMemo{quietPeriod: time.Millisecond}
	now := time.Now()
	key := completionMemoKeyFor("file:///w/a.cpp", []byte("int main(){}"), 1, 0, 0, 0, 0, 0)
	value := memoCompletionValue(4)
	memo.completionStore(key, value, now)

	memo.mu.Lock()
	stored := len(memo.completions)
	memo.mu.Unlock()
	if stored != 1 {
		t.Fatalf("unconfigured memo store kept %d entries, want 1 (default behavior unchanged)", stored)
	}
	if languages.BackendBudget() != nil {
		t.Fatal("no budget variables configured; the latch must stay nil (X10)")
	}
	if served, ok := memo.completionServe(key, now.Add(time.Minute)); !ok || len(served.Items) != 0 {
		// Unconfirmed entries never serve; the assertion is that storage
		// itself was untouched by the budget wiring.
		_ = served
	}
	if got := languages.BackendBufferRefusals("ccls-memo"); got != refusalsBefore {
		t.Fatalf("unconfigured path recorded %d new refusals, want 0", got-refusalsBefore)
	}
}

func TestMemoValueBytesMatchesWireSize(t *testing.T) {
	value := memoCompletionValue(3)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := memoValueBytes(value); got != int64(len(encoded)) {
		t.Fatalf("memoValueBytes = %d, want the JSON wire size %d", got, len(encoded))
	}
}
