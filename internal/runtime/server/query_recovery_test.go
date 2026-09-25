package server

// B6/§J4 regression: a backend that answers failure as an in-band Unknown
// or Unavailable envelope (err == nil) must not have that envelope pinned in
// the §J memo engine for the rest of the snapshot. The first request may
// return Unknown/Unavailable (honest per §A3), but once the backend recovers,
// the very next identical request MUST reach the backend again and return
// the fresh exact result.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// flakyRefBackend fails references with an in-band envelope (nil Go error,
// the TS-bridge convention) for the first N calls, then serves an exact hit.
type flakyRefBackend struct {
	mockBackend
	calls      atomic.Int64
	failFor    int64
	failStatus identity.ResultStatus
	langID     string
}

func (f *flakyRefBackend) References(_ context.Context, _ languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	n := f.calls.Add(1)
	if n <= f.failFor {
		return identity.SemanticResult[[]languages.Location]{
			Status:              f.failStatus,
			Completeness:        identity.CompletenessUnknown,
			InternalDiagnostics: []string{"bridge request failed: simulated"},
		}, nil
	}
	return identity.SemanticResult[[]languages.Location]{
		Status: identity.ResultExact,
		Value: []languages.Location{{
			URI: "file:///x/lib.ts",
			Range: languages.Range{
				StartLine: 0, StartCharacter: 0, EndLine: 0, EndCharacter: 3,
			},
		}},
	}, nil
}

// runRefRecovery drives three identical references requests against one
// snapshot: fail → recover → memoized-success, asserting the backend call
// count at each step (failure never memoized; success always memoized).
func runRefRecovery(t *testing.T, failStatus identity.ResultStatus) *flakyRefBackend {
	t.Helper()
	be := &flakyRefBackend{
		mockBackend: mockBackend{langID: "ts", exts: []string{".ts"}},
		failFor:     1,
		failStatus:  failStatus,
	}
	s := New(DefaultConfig())
	s.RegisterBackend("typescript", be)
	s.vfs.Open("file:///x/main.ts", "typescript", 1, []byte("let x = 1;\n"), vfs.SourceEditor)
	s.publishSnapshot()

	call := func() *jsonrpc.Message {
		params := `{"textDocument":{"uri":"file:///x/main.ts"},"position":{"line":0,"character":4},"context":{"includeDeclaration":true}}`
		return jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/references", json.RawMessage(params))
	}

	// First request: backend fails → empty projection (honest per §A3).
	resp1 := s.dispatcher.Dispatch(withSnapshot(context.Background(), s.snapMgr.Current()), call())
	if resp1 == nil || resp1.Error != nil {
		t.Fatalf("first references failed: %v", resp1.Error)
	}
	if string(resp1.Result) != "[]" {
		t.Fatalf("first result = %s, want []", resp1.Result)
	}

	// Second request: backend has recovered; the memo engine must NOT replay
	// the cached failure envelope — the backend must be consulted again.
	resp2 := s.dispatcher.Dispatch(withSnapshot(context.Background(), s.snapMgr.Current()), call())
	if resp2 == nil || resp2.Error != nil {
		t.Fatalf("second references failed: %v", resp2.Error)
	}
	var got []json.RawMessage
	if err := json.Unmarshal(resp2.Result, &got); err != nil {
		t.Fatalf("second result %s: %v", resp2.Result, err)
	}
	if len(got) != 1 {
		t.Fatalf("second result = %s, want the recovered exact location", resp2.Result)
	}
	if n := be.calls.Load(); n != 2 {
		t.Fatalf("backend calls = %d, want 2 (failure must not be memoized)", n)
	}

	// Third request: success is memoizable — same snapshot, no third call.
	resp3 := s.dispatcher.Dispatch(withSnapshot(context.Background(), s.snapMgr.Current()), call())
	if resp3 == nil || resp3.Error != nil {
		t.Fatalf("third references failed: %v", resp3.Error)
	}
	if n := be.calls.Load(); n != 2 {
		t.Fatalf("backend calls after success = %d, want 2 (success must stay memoized)", n)
	}
	return be
}

func TestB6_UnknownEnvelopeNotMemoizedAcrossRecovery(t *testing.T) {
	runRefRecovery(t, identity.ResultUnknown)
}

func TestB6_UnavailableEnvelopeNotMemoizedAcrossRecovery(t *testing.T) {
	runRefRecovery(t, identity.ResultUnavailable)
}
