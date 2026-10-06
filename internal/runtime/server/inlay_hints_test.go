package server

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// hintBackend is the only backend in this package that implements
// InlayHintProvider, which is why the inlay hint success path had no coverage:
// TestC3_CapabilitiesDeclaredEqualsServed only asks whether the handler is
// registered, and the gate test only exercises the incapable branch. Nothing
// ever asked a capable backend for hints.
type hintBackend struct {
	mockBackend
	hints  []languages.InlayHint
	err    error
	gotReq languages.InlayHintRequest
	calls  int
}

func (b *hintBackend) InlayHints(_ context.Context, req languages.InlayHintRequest) ([]languages.InlayHint, error) {
	b.calls++
	b.gotReq = req
	return b.hints, b.err
}

func dispatchInlayHints(t *testing.T, s *Server, uri string, startLine, endLine uint32) *jsonrpc.Message {
	t.Helper()
	params := `{"textDocument":{"uri":"` + uri + `"},"range":{"start":{"line":` +
		strconv.FormatUint(uint64(startLine), 10) + `,"character":0},"end":{"line":` +
		strconv.FormatUint(uint64(endLine), 10) + `,"character":0}}}`
	return s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 7}, "textDocument/inlayHint", json.RawMessage(params)))
}

// newHintServer wires a hint-capable backend over one open document.
func newHintServer(t *testing.T, be *hintBackend, src string) (*Server, string) {
	t.Helper()
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte(src), vfs.SourceEditor)
	s.publishSnapshot()
	return s, uri
}

type hintPayload struct {
	Position struct {
		Line      uint32 `json:"line"`
		Character uint32 `json:"character"`
	} `json:"position"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

// TestQ4_InlayHintSuccessPath covers the branch that turns provider hints into
// LSP JSON. It also pins the one rename in the mapping -- the backend calls the
// field Column and the wire calls it character -- which nothing else would
// notice if it were dropped.
func TestQ4_InlayHintSuccessPath(t *testing.T) {
	be := &hintBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		hints: []languages.InlayHint{
			{Line: 3, Column: 7, Label: "name: string", Kind: "parameter"},
			{Line: 4, Column: 2, Label: "int"},
		},
	}
	s, uri := newHintServer(t, be, "package main\n\nfunc f(name string) int { return 0 }\n")

	resp := dispatchInlayHints(t, s, uri, 3, 4)
	if resp == nil || resp.Error != nil {
		t.Fatalf("inlayHint = %+v, want success", resp)
	}
	var got []hintPayload
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode result %s: %v", resp.Result, err)
	}
	if len(got) != 2 {
		t.Fatalf("hint count = %d, want 2", len(got))
	}
	if got[0].Position.Line != 3 || got[0].Position.Character != 7 || got[0].Label != "name: string" || got[0].Kind != "parameter" {
		t.Errorf("hint[0] = %+v, want line 3 character 7 label %q kind %q",
			got[0], "name: string", "parameter")
	}
	// An empty kind must be absent from the wire form. Check the raw bytes:
	// round-tripping through a decode struct that lacks omitempty would add the
	// key back and make this pass whatever the handler emitted.
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(resp.Result, &raw); err != nil {
		t.Fatalf("decode result %s: %v", resp.Result, err)
	}
	if _, present := raw[1]["kind"]; present {
		t.Errorf("hint[1] = %s, want no kind key when the backend reports none", resp.Result)
	}
	if _, present := raw[0]["kind"]; !present {
		t.Errorf("hint[0] = %s, want the kind it reported", resp.Result)
	}
}

// TestQ4_InlayHintRequestCarriesRangeAndDocument covers what the handler hands
// the provider: the range's lines, the document bytes, and the revision. Only
// the lines are passed -- the characters are dropped, since the provider's
// request is line-granular.
func TestQ4_InlayHintRequestCarriesRangeAndDocument(t *testing.T) {
	const src = "package main\n\nfunc f(name string) int { return 0 }\n"
	be := &hintBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s, uri := newHintServer(t, be, src)

	resp := dispatchInlayHints(t, s, uri, 2, 3)
	if resp == nil || resp.Error != nil {
		t.Fatalf("inlayHint = %+v, want success", resp)
	}
	if be.gotReq.URI != uri {
		t.Errorf("request URI = %q, want %q", be.gotReq.URI, uri)
	}
	if string(be.gotReq.Content) != src {
		t.Errorf("request content = %q, want the open document", be.gotReq.Content)
	}
	if be.gotReq.StartLine != 2 || be.gotReq.EndLine != 3 {
		t.Errorf("request range = [%d,%d], want [2,3]", be.gotReq.StartLine, be.gotReq.EndLine)
	}
	if be.gotReq.SnapshotRev == 0 {
		t.Error("request snapshot revision is 0; the handler should pass the current one")
	}
}

// TestQ4_InlayHintEmptyAndErrorResults covers the two non-happy shapes: a
// provider with nothing to say, and one that fails. The empty case has to
// serialise as [] rather than null, because a client indexing the result would
// otherwise see a null where it expects an array.
func TestQ4_InlayHintEmptyAndErrorResults(t *testing.T) {
	t.Run("no hints yields an empty array", func(t *testing.T) {
		be := &hintBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
		s, uri := newHintServer(t, be, "package main\n")
		resp := dispatchInlayHints(t, s, uri, 0, 1)
		if resp == nil || resp.Error != nil {
			t.Fatalf("inlayHint = %+v, want success", resp)
		}
		if got := string(resp.Result); got != "[]" {
			t.Errorf("result = %s, want [] (a client would see null and cannot index it)", got)
		}
	})

	t.Run("provider error reaches the client", func(t *testing.T) {
		be := &hintBackend{
			mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
			err:         ierrors.New(ierrors.ErrParse, "go", "cannot infer types"),
		}
		s, uri := newHintServer(t, be, "package main\n")
		resp := dispatchInlayHints(t, s, uri, 0, 1)
		if resp == nil || resp.Error == nil {
			t.Fatalf("inlayHint = %+v, want an error", resp)
		}
	})
}
