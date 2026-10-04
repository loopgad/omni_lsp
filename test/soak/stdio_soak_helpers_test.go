//go:build soak

package soak

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
