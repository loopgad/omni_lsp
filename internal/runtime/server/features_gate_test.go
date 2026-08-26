package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// sigBackend embeds the plain mock and adds only SignatureHelper — proving
// the optional-interface negotiation: one capability present, others refused.
type sigBackend struct {
	mockBackend
}

func (b *sigBackend) SignatureHelp(_ context.Context, req languages.SignatureHelpRequest) (identity.SemanticResult[*languages.SignatureHelpResult], error) {
	return identity.SemanticResult[*languages.SignatureHelpResult]{
		Status: identity.ResultPartial,
		Value: &languages.SignatureHelpResult{Signatures: []languages.SignatureInformation{
			{Label: "demo(a int, b int)", Parameters: []string{"a int", "b int"}, ActiveParameter: int(req.Column) % 2},
		}},
	}, nil
}

// TestI16_SignatureHelpNegotiatedAndUnsupported pins §I16 capability
// negotiation end to end.
func TestI16_SignatureHelpNegotiatedAndUnsupported(t *testing.T) {
	const uri = "file:///w/main.go"

	t.Run("capable backend answers", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &sigBackend{mockBackend{langID: "go", exts: []string{".go"}}})
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "textDocument/signatureHelp",
			json.RawMessage(`{"textDocument":{"uri":"`+uri+`"},"position":{"line":0,"character":0}}`)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("signatureHelp failed: %+v", resp)
		}
		var out struct {
			Result struct {
				Signatures []struct {
					Label string `json:"label"`
				} `json:"signatures"`
			} `json:"result"`
		}
		raw, _ := json.Marshal(resp)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Result.Signatures) != 1 || out.Result.Signatures[0].Label == "" {
			t.Errorf("unexpected signatures payload: %s", raw)
		}
	})

	t.Run("incapable backend refuses cleanly", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		for _, m := range []string{"textDocument/signatureHelp", "textDocument/formatting", "textDocument/inlayHint"} {
			resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
				jsonrpc.RequestID{Num: 2}, m,
				json.RawMessage(`{"textDocument":{"uri":"`+uri+`"},"position":{"line":0,"character":0}}`)))
			if resp == nil || resp.Error == nil {
				t.Errorf("%s: expected not-supported refusal, got %+v", m, resp)
				continue
			}
			if resp.Error.Code != jsonrpc.MethodNotFound {
				t.Errorf("%s: code = %d, want MethodNotFound", m, resp.Error.Code)
			}
		}
	})
}

// fmtBackend exercises §I20 with a real full-document edit.
type fmtBackend struct{ mockBackend }

func (b *fmtBackend) Formatting(_ context.Context, req languages.FormattingRequest) ([]languages.TextEdit, error) {
	return []languages.TextEdit{{URI: req.URI, StartLine: 0, StartChar: 0, EndLine: 1, EndChar: 0, NewText: "package main\n"}}, nil
}

func TestI20_FormattingFullDocumentEdit(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	s.RegisterBackend("go", &fmtBackend{mockBackend{langID: "go", exts: []string{".go"}}})
	s.vfs.Open(uri, "go", 1, []byte("package   main\n"), 0)
	resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 3}, "textDocument/formatting",
		json.RawMessage(`{"textDocument":{"uri":"`+uri+`"},"options":{"tabSize":4,"insertSpaces":true}}`)))
	if resp == nil || resp.Error != nil {
		t.Fatalf("formatting failed: %+v", resp)
	}
	var edits []map[string]any
	raw, _ := json.Marshal(resp)
	var env struct {
		Result json.RawMessage `json:"result"`
	}
	json.Unmarshal(raw, &env)
	if err := json.Unmarshal(env.Result, &edits); err != nil {
		t.Fatalf("edits shape: %v (%s)", err, env.Result)
	}
	if len(edits) != 1 || edits[0]["newText"] == "" {
		t.Errorf("expected single full-document edit, got %s", env.Result)
	}
}
