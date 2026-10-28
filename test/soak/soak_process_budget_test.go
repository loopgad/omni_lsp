package soak

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Guards the audit finding "the 1h soak's -test.timeout=90m is a bare
// literal": the literal predates the 300s request budget and sits below the
// stage's legal worst-case wall clock (one periodic reindex per minute row,
// each up to requestBudget + 2*soakPollPeriod, so 60 legal reindexes alone
// consume ~5.2h). A timeout kill calls os.Exit without running the deferred
// final report, and no final report voids the continuous stage — fail-closed,
// but structurally unreachable. acceptance.ps1 must derive the soak
// -test.timeout from the process budget the frozen test binary reports
// (TestSoakRealStdioProcessBudgetHelper); no duration literal may live in the
// script (16th-round rule: budget derivation, not constants).

// soakProcessBudgetTestRoot resolves the repository root without leaning on
// the soak-tagged test files, so these guards run in the untagged default
// suite next to the pure gate checks.
func soakProcessBudgetTestRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve soak process budget test source path")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(source), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestSoakTimeoutIsDerivedNotLiteral(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(soakProcessBudgetTestRoot(t), "scripts", "acceptance.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	// `-test.timeout=` (dot) is unique to the soak stage; the other stages
	// invoke `go test` with `-timeout=` and are not this finding's subject.
	if strings.Contains(script, "-test.timeout=90m") {
		t.Fatal("acceptance.ps1 still pins the soak stage to a bare -test.timeout=90m literal; derive it from the stage's process budget")
	}
	if !strings.Contains(script, "-test.timeout=$soakProcessTimeout") {
		t.Fatal("the soak invocation must pass -test.timeout from the helper-reported process budget ($soakProcessTimeout)")
	}
}

// TestSoakRealStdioProcessBudgetHelper is the only sanctioned channel for
// acceptance.ps1 to obtain the soak stage's -test.timeout: it prints the
// derived process budget for the env-pinned SOAK_DURATION so the single
// source of truth stays next to soakMaintenanceBudget. Skipped in normal
// runs; writes no checks and touches no server.
func TestSoakRealStdioProcessBudgetHelper(t *testing.T) {
	if os.Getenv("OMNILSP_SOAK_PROCESS_BUDGET_HELPER") != "1" {
		t.Skip("acceptance helper: reports soakProcessBudget(SOAK_DURATION) for the -test.timeout derivation")
	}
	duration, err := parseStdioSoakDuration(os.Getenv("SOAK_DURATION"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "soak process budget helper: %v\n", err)
		os.Exit(2)
	}
	budget := soakProcessBudget(duration)
	fmt.Printf("soak-process-budget=%s\nsoak-process-budget-seconds=%d\n", budget, int64(budget.Seconds()))
}

// TestSoakProcessBudgetCoversLegalWorstCase pins the derivation against its
// own components for every legal tier: the budget must at least cover the
// active cadence, every minute row's periodic reindex, the single candidate
// restart, and a minimal startup/finalization sequence — so a future edit
// cannot silently drop a term and re-create a structurally unreachable gate.
// The env-widening half pins that a widened OMNILSP_SOAK_REQUEST_BUDGET grows
// the budget instead of silently shrinking the headroom the script pinned at
// 300s.
func TestSoakProcessBudgetCoversLegalWorstCase(t *testing.T) {
	for _, requestBudget := range []string{"", "90s"} {
		t.Run("request-budget="+requestBudget, func(t *testing.T) {
			t.Setenv(stdioRequestBudgetEnv, requestBudget)
			for _, duration := range []time.Duration{30 * time.Second, 10 * time.Minute, time.Hour} {
				budget := soakProcessBudget(duration)
				ticks := duration/time.Minute + 1
				floor := duration +
					ticks*soakMaintenanceBudget("periodic-reindex") +
					soakMaintenanceBudget("candidate-restart") +
					time.Duration(soakInitialQueryCount)*stdioRequestBudget() +
					stdioColdStartAllowance(90*time.Second)
				if budget < floor {
					t.Fatalf("soakProcessBudget(%s) = %s, below its own legal components (%s)", duration, budget, floor)
				}
				if budget <= duration {
					t.Fatalf("soakProcessBudget(%s) = %s leaves no room for maintenance", duration, budget)
				}
			}
		})
	}
	// The acceptance configuration pins OMNILSP_SOAK_REQUEST_BUDGET=300s;
	// that is the widening under which the finding measured a single periodic
	// reindex at 167s on a slow host. The continuous tier's budget must cover
	// the active hour plus that measured accumulation (60 × 167s ≈ 2.8h) —
	// a budget below it would re-create the structurally unreachable 1h gate
	// the bare 90m literal produced.
	t.Run("request-budget=300s", func(t *testing.T) {
		t.Setenv(stdioRequestBudgetEnv, "300s")
		hour := soakProcessBudget(time.Hour)
		if want := time.Hour + 60*167*time.Second; hour < want {
			t.Fatalf("soakProcessBudget(1h) = %s, below the active hour plus the measured legal reindex accumulation (%s)", hour, want)
		}
	})
}
