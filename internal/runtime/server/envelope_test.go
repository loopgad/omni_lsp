package server

// Tests for goal.md §B5 envelope projection and §B4 evidence propagation.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// envelopeBackend lets tests drive every §B5 status through the server.
type envelopeBackend struct {
	mockBackend
	hoverStatus identity.ResultStatus
}

func (m *envelopeBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	return identity.SemanticResult[*languages.HoverResult]{
		Status:              m.hoverStatus,
		Value:               nil,
		Evidence:            []identity.Evidence{{Kind: identity.EvidenceSyntax}},
		InternalDiagnostics: []string{"driven-by-test"},
	}, nil
}

// TestB5_UnknownHoverProjectsNull verifies read-only queries project
// Unknown/Unavailable as protocol null — absence is not fabrication (§Q3).
func TestB5_UnknownHoverProjectsNull(t *testing.T) {
	for _, st := range []identity.ResultStatus{identity.ResultUnknown, identity.ResultUnavailable} {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &envelopeBackend{
			mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
			hoverStatus: st,
		})
		s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
		params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`
		msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/hover", json.RawMessage(params))
		resp := s.dispatcher.Dispatch(context.Background(), msg)
		if resp == nil || resp.Error != nil {
			t.Fatalf("status %v: unexpected error %v", st, resp.Error)
		}
		if string(resp.Result) != "null" {
			t.Errorf("status %v: result = %s, want null", st, resp.Result)
		}
	}
}

// TestE7_BuildContextFlowsIntoRequests verifies the server fills the build
// context from an optional backend provider into backend requests.
func TestE7_BuildContextFlowsIntoRequests(t *testing.T) {
	var seen identity.BuildContextID
	inner := &contextCapturingMock{seen: &seen, bc: "go:sha256:testctx"}
	s := New(DefaultConfig())
	s.RegisterBackend("go", inner)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/hover", json.RawMessage(params))
	s.dispatcher.Dispatch(context.Background(), msg)
	if seen != "go:sha256:testctx" {
		t.Errorf("request BuildContext = %q, want go:sha256:testctx", seen)
	}
}

type contextCapturingMock struct {
	mockBackend
	seen  *identity.BuildContextID
	bc    identity.BuildContextID
	epoch uint64
}

func (m *contextCapturingMock) BuildContextID() identity.BuildContextID { return m.bc }
func (m *contextCapturingMock) SupervisorEpoch() uint64                 { return m.epoch }

func (m *contextCapturingMock) Hover(_ context.Context, req languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	*m.seen = req.BuildContext
	return identity.SemanticResult[*languages.HoverResult]{
		Status:   identity.ResultExact,
		Value:    &languages.HoverResult{Contents: "ok"},
		Evidence: []identity.Evidence{{BuildContext: req.BuildContext, BackendEpoch: identity.BackendEpoch(m.epoch)}},
	}, nil
}

// TestI25_ExplainReportsEvidenceChain verifies the evidence ring feeds
// omnilsp/explain with the real §B4 records from semantic handlers.
func TestI25_ExplainReportsEvidenceChain(t *testing.T) {
	s := New(DefaultConfig())
	be := &contextCapturingMock{bc: "go:sha256:testctx", seen: new(identity.BuildContextID), epoch: 7}
	be.hoverResult = &languages.HoverResult{Contents: "sig"}
	s.RegisterBackend("go", be)

	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()

	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/hover", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(withSnapshot(context.Background(), s.snapMgr.Current()), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("hover failed: %v", resp.Error)
	}

	explainMsg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 2}, "omnilsp/explain",
		json.RawMessage(`{"uri":"file:///x.go"}`))
	expResp := s.dispatcher.Dispatch(context.Background(), explainMsg)
	if expResp == nil || expResp.Error != nil {
		t.Fatalf("explain failed: %v", expResp.Error)
	}
	var out struct {
		Snapshot    uint64 `json:"snapshot"`
		SyncRejects int64  `json:"syncRejects"`
		Evidence    []struct {
			Method       string `json:"method"`
			Kind         string `json:"kind"`
			Assurance    string `json:"assurance"`
			BuildContext string `json:"buildContext"`
			BackendEpoch uint64 `json:"backendEpoch"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(expResp.Result, &out); err != nil {
		t.Fatalf("unmarshal explain: %v", err)
	}
	if len(out.Evidence) == 0 {
		t.Fatal("explain returned no evidence for a served hover")
	}
	e := out.Evidence[0]
	if e.Method != "textDocument/hover" {
		t.Errorf("method = %q", e.Method)
	}
	if e.BuildContext != "go:sha256:testctx" {
		t.Errorf("buildContext = %q, want go:sha256:testctx", e.BuildContext)
	}
	if e.BackendEpoch != 7 {
		t.Errorf("backendEpoch = %d, want the producing backend's epoch 7", e.BackendEpoch)
	}
}

func TestEvidenceQueriesAcceptEquivalentDocumentURIs(t *testing.T) {
	s := New(DefaultConfig())
	s.recordEvidence(context.Background(), "textDocument/hover", "file:///C:/workspace/a%20b.go", identity.ResultExact, identity.Complete,
		[]identity.Evidence{{Kind: identity.EvidenceIndex}}, nil)
	for _, method := range []string{"omnilsp/explain", "omnilsp/resultMeta"} {
		params, err := json.Marshal(map[string]string{"uri": "file:///c:/workspace/a b.go"})
		if err != nil {
			t.Fatal(err)
		}
		msg := &jsonrpc.Message{Method: method, Params: params}
		var raw json.RawMessage
		if method == "omnilsp/explain" {
			raw, err = s.handleOmnilspExplain(context.Background(), msg)
		} else {
			raw, err = s.handleOmnilspResultMeta(context.Background(), msg)
		}
		if err != nil {
			t.Fatal(err)
		}
		if method == "omnilsp/explain" {
			var result struct {
				Evidence []json.RawMessage `json:"evidence"`
			}
			if err := json.Unmarshal(raw, &result); err != nil || len(result.Evidence) != 1 {
				t.Fatalf("%s alias evidence missing: %s err=%v", method, raw, err)
			}
		} else {
			var entries []json.RawMessage
			if err := json.Unmarshal(raw, &entries); err != nil || len(entries) != 1 {
				t.Fatalf("%s alias evidence missing: %s err=%v", method, raw, err)
			}
		}
	}
}
