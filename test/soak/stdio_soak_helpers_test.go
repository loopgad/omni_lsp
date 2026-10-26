package soak

// The form checks in this file are deliberately untagged per the 2026-10-25
// ruling 3: they exercise the pure gate predicates in stdio_gates.go with no
// live server, and must run in the default suite.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

func TestRequestReindexWithRetryRetriesOnlyExactBusyResponse(t *testing.T) {
	busy := &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "reindex already running"}
	calls := 0
	got, err := requestReindexWithRetry(context.Background(), func(context.Context) (json.RawMessage, error) {
		calls++
		if calls == 1 {
			return nil, busy
		}
		return json.RawMessage(`{"generation":2}`), nil
	})
	if err != nil {
		t.Fatalf("requestReindexWithRetry() error = %v", err)
	}
	if calls != 2 || string(got) != `{"generation":2}` {
		t.Fatalf("requestReindexWithRetry() calls=%d result=%s, want 2 calls and successful response", calls, got)
	}

	drift := &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "workspace files changed during reindex: changed soak.go"}
	calls = 0
	_, err = requestReindexWithRetry(context.Background(), func(context.Context) (json.RawMessage, error) {
		calls++
		return nil, drift
	})
	var gotResponse *jsonrpc.ResponseError
	if !errors.As(err, &gotResponse) || gotResponse != drift || calls != 1 {
		t.Fatalf("workspace drift retry result = %v after %d calls, want original error after one call", err, calls)
	}

	for _, nonBusy := range []*jsonrpc.ResponseError{
		{Code: jsonrpc.InternalError, Message: "reindex already running"},
		{Code: jsonrpc.RequestFailed, Message: "reindex already running after workspace drift"},
	} {
		calls = 0
		_, err = requestReindexWithRetry(context.Background(), func(context.Context) (json.RawMessage, error) {
			calls++
			return nil, nonBusy
		})
		if err != nonBusy || calls != 1 {
			t.Fatalf("non-exact busy response %v retried or changed: err=%v calls=%d", nonBusy, err, calls)
		}
	}
}

func TestRequestReindexWithRetryHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	_, err := requestReindexWithRetry(ctx, func(context.Context) (json.RawMessage, error) {
		calls++
		return nil, &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "reindex already running"}
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("deadline result = %v after %d calls, want DeadlineExceeded after one attempt", err, calls)
	}
}

func TestStdioGenerationAndFinalProgressGates(t *testing.T) {
	if err := validateReindexGeneration(4, 5, 100); err != nil {
		t.Fatalf("advanced generation rejected: %v", err)
	}
	if err := validateReindexGeneration(4, 4, 100); err == nil {
		t.Fatal("explicit reindex accepted a generation that did not advance")
	}
	if err := validateReindexGeneration(5, 4, 100); err == nil {
		t.Fatal("explicit reindex accepted a regressed generation")
	}
	if err := validateReindexGeneration(4, 5, 99); err == nil {
		t.Fatal("explicit reindex accepted an undersized inventory")
	}
	if err := validatePersistentIndexRecovery(7, 7, true, true); err != nil {
		t.Fatalf("unchanged fresh persistent generation rejected after restart: %v", err)
	}
	if err := validatePersistentIndexRecovery(8, 7, true, true); err == nil {
		t.Fatal("restart accepted a different persistent generation")
	}
	if err := validatePersistentIndexRecovery(7, 7, true, false); err == nil {
		t.Fatal("restart accepted a stale persistent generation")
	}
	if err := validateFinalStdioProgress(7, 7, 5, 5, true, true); err != nil {
		t.Fatalf("last committed generation should pass without an artificial increment: %v", err)
	}
	if err := validateFinalStdioProgress(6, 7, 5, 5, true, true); err == nil {
		t.Fatal("final gate accepted a generation older than the last committed generation")
	}
	if err := validateFinalStdioProgress(7, 7, 5, 5, false, true); err == nil {
		t.Fatal("final gate accepted a stale generation")
	}
	if err := validateFinalStdioProgress(7, 7, 5, 5, false, false); err == nil {
		t.Fatal("final gate accepted a disabled or stale index")
	}
}

func TestStdioSoakDurationGateUsesOnlyConfiguredStages(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{value: "30s", want: 30 * time.Second},
		{value: "10m", want: 10 * time.Minute},
		{value: "1h", want: time.Hour},
	} {
		got, err := parseStdioSoakDuration(test.value)
		if err != nil || got != test.want {
			t.Errorf("parseStdioSoakDuration(%q) = %s, %v; want %s", test.value, got, err, test.want)
		}
	}
	for _, value := range []string{"55m", "24h", "invalid"} {
		if _, err := parseStdioSoakDuration(value); err == nil {
			t.Errorf("parseStdioSoakDuration(%q) accepted an unsupported duration", value)
		}
	}
}

func TestStdioIndexContentGateRequiresFullCrossFileReferences(t *testing.T) {
	const docURI = "file:///workspace/soak.go"
	text := stdioGoSource()
	declaration := positionOf(text, "soakTarget", 1)
	use := positionOf(text, "soakTarget", 2)
	locations := make([]stdioLocation, stdioGeneratedFiles+2)
	locations[0] = stdioLocation{URI: docURI, Range: stdioRange{Start: stdioPosition{Line: uint32(number(declaration["line"])), Character: uint32(number(declaration["character"]))}}}
	locations[1] = stdioLocation{URI: docURI, Range: stdioRange{Start: stdioPosition{Line: uint32(number(use["line"])), Character: uint32(number(use["character"]))}}}
	for i := 2; i < len(locations); i++ {
		locations[i] = stdioLocation{URI: "file:///workspace/corpus.go"}
	}

	result, err := json.Marshal(locations)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStdioIndexContent(result, docURI, text); err != nil {
		t.Fatalf("complete cross-file references rejected: %v", err)
	}

	truncated, err := json.Marshal(locations[:len(locations)-1])
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStdioIndexContent(truncated, docURI, text); err == nil {
		t.Fatal("index content with a missing corpus reference was accepted")
	}

	missingDeclaration := append([]stdioLocation(nil), locations...)
	missingDeclaration[0].Range.Start.Character++
	missingDeclarationRaw, err := json.Marshal(missingDeclaration)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateStdioIndexContent(missingDeclarationRaw, docURI, text); err == nil {
		t.Fatal("index content without the exact declaration location was accepted")
	}
}

func TestMinuteWindowGapContinuityThresholds(t *testing.T) {
	// A healthy first minute row lands ~65s after loop start with ~60s of
	// active time; the 70s ceiling tolerates tick scheduling only.
	if !minuteWindowGapContinuous(65*time.Second, time.Minute, 0) {
		t.Fatal("healthy first minute row rejected")
	}
	if !minuteWindowGapContinuous(minuteCadenceBudget(), minuteCadenceBudget(), 0) {
		t.Fatal("inclusive cadence ceiling rejected")
	}
	// A recorded maintenance pause explains the excess; the raw active gap
	// still has to contain a full minute of soak time.
	if !minuteWindowGapContinuous(3*time.Minute, 3*time.Minute, 2*time.Minute) {
		t.Fatal("bounded maintenance pause rejected")
	}
	for name, gap := range map[string]time.Duration{
		"zero wall gap":     0,
		"wall gap over 70s": minuteCadenceBudget() + time.Second,
		"negative wall gap": -time.Second,
	} {
		if minuteWindowGapContinuous(gap, time.Minute, 0) {
			t.Errorf("%s accepted", name)
		}
	}
	for name, gap := range map[string]time.Duration{
		"active gap under a minute": 59 * time.Second,
		"active gap over 70s":       minuteCadenceBudget() + time.Second,
	} {
		if minuteWindowGapContinuous(65*time.Second, gap, 0) {
			t.Errorf("%s accepted", name)
		}
	}
	for name, tc := range map[string]struct{ wall, active, pause time.Duration }{
		"pause exceeds wall gap":   {2 * time.Minute, 3 * time.Minute, 2*time.Minute + time.Second},
		"pause exceeds active gap": {3 * time.Minute, 2 * time.Minute, 2*time.Minute + time.Second},
		"negative pause":           {3 * time.Minute, 3 * time.Minute, -time.Second},
	} {
		if minuteWindowGapContinuous(tc.wall, tc.active, tc.pause) {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestSoakMaintenanceBudgetsAreDerived(t *testing.T) {
	t.Setenv(stdioRequestBudgetEnv, "")
	if got, want := soakMaintenanceBudget("periodic-reindex"), 30*time.Second+2*soakPollPeriod; got != want {
		t.Fatalf("periodic reindex budget = %s, want %s", got, want)
	}
	if got, want := soakMaintenanceBudget("candidate-restart"), stdioColdStartAllowance(90*time.Second)+soakSnapshotOpenBudget+soakSnapshotReopenBudget+soakProgressLifecycleBudget+2*soakPollPeriod; got != want {
		t.Fatalf("candidate restart budget = %s, want %s", got, want)
	}
	if got := soakMaintenanceBudget("unknown"); got != 0 {
		t.Fatalf("unknown budget = %s, want 0", got)
	}
	t.Setenv(stdioRequestBudgetEnv, "90s")
	if got, want := soakMaintenanceBudget("periodic-reindex"), 90*time.Second+2*soakPollPeriod; got != want {
		t.Fatalf("widened periodic reindex budget = %s, want %s", got, want)
	}
	if got, want := soakMaintenanceBudget("candidate-restart"), stdioColdStartAllowance(90*time.Second)+soakSnapshotOpenBudget+soakSnapshotReopenBudget+soakProgressLifecycleBudget+2*soakPollPeriod; got != want {
		t.Fatalf("widened candidate restart budget = %s, want %s", got, want)
	}
}

func TestSoakRustStabilityProbeBudgetIsRequestBudget(t *testing.T) {
	t.Setenv(stdioRequestBudgetEnv, "")
	if got, want := soakRustStabilityProbeBudget(), 30*time.Second; got != want {
		t.Fatalf("default stability probe budget = %s, want %s", got, want)
	}
	t.Setenv(stdioRequestBudgetEnv, "45s")
	if got, want := soakRustStabilityProbeBudget(), 45*time.Second; got != want {
		t.Fatalf("widened stability probe budget = %s, want %s", got, want)
	}
}

func TestSupervisedRustAnalyzerPIDSelectsActualLeaf(t *testing.T) {
	sample := processTreeSample{Members: []processTreeMember{
		{PID: 10, Role: "candidate", ImagePath: `C:\omnilsp.exe`},
		{PID: 20, ParentPID: 10, Role: "backend", ImagePath: `C:\Users\test\.cargo\bin\rust-analyzer.exe`},
		{PID: 21, ParentPID: 20, Role: "backend", ImagePath: `C:\Users\test\.rustup\toolchains\stable\bin\rust-analyzer.exe`},
	}}
	got, err := supervisedRustAnalyzerPID(sample, 10, `C:\Users\test\.cargo\bin\rust-analyzer.exe`)
	if err != nil || got != 21 {
		t.Fatalf("supervisedRustAnalyzerPID() = %d, %v; want actual leaf PID 21", got, err)
	}

	sample.Members = append(sample.Members, processTreeMember{PID: 22, ParentPID: 10, Role: "backend", ImagePath: `C:\another\rust-analyzer.exe`})
	if _, err := supervisedRustAnalyzerPID(sample, 10, `C:\Users\test\.cargo\bin\rust-analyzer.exe`); err == nil || !strings.Contains(err.Error(), "one supervised rust-analyzer leaf") {
		t.Fatalf("ambiguous supervised backend identity error = %v", err)
	}
}

func TestExactSoakToolVersionRejectsSuffixAndPrefixMatches(t *testing.T) {
	tests := []struct {
		id, output, expected string
		want                 bool
	}{
		{"go", "go version go1.26.1 windows/amd64", "go1.26.1", true},
		{"go", "go version go1.26.10 windows/amd64", "go1.26.1", false},
		{"node", "v24.14.0", "v24.14.0", true},
		{"node", "v24.14.0-extra", "v24.14.0", false},
		{"python", "Python 3.13.13", "3.13.13", true},
		{"clangd", "clangd version 22.1.5", "22.1.5", true},
		{"clangd", "clangd version 22.1.50", "22.1.5", false},
		{"rust-analyzer", "rust-analyzer 1.97.0 (2d8144b7 2026-07-07)", "1.97.0 (2d8144b7 2026-07-07)", true},
		{"rust-analyzer", "rust-analyzer 1.97.0-extra (2d8144b7 2026-07-07)", "1.97.0 (2d8144b7 2026-07-07)", false},
	}
	for _, test := range tests {
		t.Run(test.id+"/"+test.output, func(t *testing.T) {
			if got := exactSoakToolVersion(test.id, test.output, test.expected); got != test.want {
				t.Fatalf("exactSoakToolVersion(%q, %q, %q) = %t, want %t", test.id, test.output, test.expected, got, test.want)
			}
		})
	}
}

func TestMakeStdioQueriesKeepSymbolPositionsIndependent(t *testing.T) {
	text := stdioGoSource()
	queries := makeStdioQueries("file:///soak.go", text)
	base := positionOf(text, "soakTarget", 2)
	wantOffsets := map[string]int{
		"textDocument/definition":    0,
		"textDocument/references":    0,
		"textDocument/completion":    5,
		"textDocument/prepareRename": len("soakTarget"),
	}
	for _, query := range queries {
		offset, ok := wantOffsets[query.method]
		if !ok {
			continue
		}
		position := query.params["position"].(map[string]any)
		if got, want := number(position["character"]), number(base["character"])+offset; got != want {
			t.Errorf("%s character = %d, want %d", query.method, got, want)
		}
	}
}

func TestProgressLifecycleMustEndBeforeTerminalResponse(t *testing.T) {
	progress := func(kind string) *jsonrpc.Message {
		params, err := json.Marshal(map[string]any{"token": "test-token", "value": map[string]any{"kind": kind}})
		if err != nil {
			t.Fatal(err)
		}
		return jsonrpc.NewNotification("$/progress", params)
	}
	response := jsonrpc.NewResponse(jsonrpc.RequestID{Num: 1}, json.RawMessage(`[]`))
	valid := []*jsonrpc.Message{progress("begin"), progress("report"), progress("end"), response}
	if _, err := progressLifecycleBeforeResponse(valid, "test-token"); err != nil {
		t.Fatalf("ordered lifecycle rejected: %v", err)
	}
	lateEnd := []*jsonrpc.Message{progress("begin"), response, progress("end")}
	if _, err := progressLifecycleBeforeResponse(lateEnd, "test-token"); err == nil {
		t.Fatal("lifecycle ending after terminal response was accepted")
	}
}

func TestSoak_RequestBudgetDefaultIs30s(t *testing.T) {
	t.Setenv(stdioRequestBudgetEnv, "")
	if got := stdioRequestBudget(); got != 30*time.Second {
		t.Fatalf("stdioRequestBudget() with %s unset = %s, want 30s", stdioRequestBudgetEnv, got)
	}
	t.Setenv(stdioRequestBudgetEnv, "90s")
	if got := stdioRequestBudget(); got != 90*time.Second {
		t.Fatalf("stdioRequestBudget() with %s=90s = %s, want 90s", stdioRequestBudgetEnv, got)
	}
	for _, invalid := range []string{"nope", "0s", "-5s"} {
		t.Setenv(stdioRequestBudgetEnv, invalid)
		if got := stdioRequestBudget(); got != 30*time.Second {
			t.Fatalf("stdioRequestBudget() with %s=%q = %s, want the 30s default", stdioRequestBudgetEnv, invalid, got)
		}
	}
}
