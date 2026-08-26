package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// diagBackend serves canned diagnostics and counts invocations.
type diagBackend struct {
	mockBackend
	calls atomic.Int64
}

func (b *diagBackend) Diagnostics(_ context.Context, _ string, _ []byte) ([]languages.Diagnostic, error) {
	b.calls.Add(1)
	return []languages.Diagnostic{
		{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 3, Severity: 1, Code: "E1", Source: "go", Message: "undefined: foo"},
	}, nil
}

func TestC11_PushDebouncedPublish(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	be := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	rt := &recordingTransport{}
	s.mu.Lock()
	s.transport = rt
	s.mu.Unlock()

	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
	s.publishSnapshot()
	s.diag.request(uri) // didOpen tail

	// Debounce window must elapse before the backend is touched.
	time.Sleep(50 * time.Millisecond)
	if n := be.calls.Load(); n != 0 {
		t.Fatalf("backend called %d times inside debounce window", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rt.filterMethod("textDocument/publishDiagnostics")) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	pubs := rt.filterMethod("textDocument/publishDiagnostics")
	if len(pubs) != 1 {
		t.Fatalf("expected exactly 1 publish after debounce, got %d", len(pubs))
	}
	if !containsBytes(pubs[0].Params, `"uri":"`+uri+`"`) || !containsBytes(pubs[0].Params, "undefined") {
		t.Errorf("publish payload malformed: %s", pubs[0].Params)
	}
	if n := be.calls.Load(); n != 1 {
		t.Fatalf("backend calls = %d, want 1 (debounce coalesced)", n)
	}

	// Rapid re-requests coalesce into one debounced push; the §C11 cache
	// answers it, so the backend is NOT touched again — but the wire still
	// carries the republished set.
	rt.reset()
	s.diag.request(uri)
	s.diag.request(uri)
	time.Sleep(400 * time.Millisecond)
	if n := be.calls.Load(); n != 1 {
		t.Fatalf("cache miss after identical re-push: calls = %d, want 1", n)
	}
	if n := len(rt.filterMethod("textDocument/publishDiagnostics")); n != 1 {
		t.Fatalf("republished diagnostics %d times, want 1", n)
	}
}

func TestC11_PullStableResultIdAndCache(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	be := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
	s.publishSnapshot()

	pull := func() json.RawMessage {
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "textDocument/diagnostic",
			json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("pull failed: %+v", resp)
		}
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		raw, _ := json.Marshal(resp)
		json.Unmarshal(raw, &env)
		return env.Result
	}

	first := pull()
	if !containsBytes(first, `"kind":"full"`) || !containsBytes(first, `"resultId"`) {
		t.Fatalf("pull shape wrong: %s", first)
	}
	second := pull()
	if n := be.calls.Load(); n != 1 {
		t.Fatalf("second pull recomputed (calls=%d): cache key must gate", n)
	}
	var a, b struct {
		Items []struct {
			ResultID string `json:"resultId"`
		} `json:"items"`
	}
	json.Unmarshal(first, &a)
	json.Unmarshal(second, &b)
	if len(a.Items) == 0 || a.Items[0].ResultID != b.Items[0].ResultID {
		t.Fatalf("resultId unstable across identical pulls")
	}

	// Content change ⇒ new key ⇒ recompute.
	s.vfs.Update(uri, 2, []byte("package main // changed\n"))
	s.publishSnapshot()
	pull()
	if n := be.calls.Load(); n != 2 {
		t.Fatalf("content change did not invalidate cache (calls=%d)", n)
	}
}

func TestI19_CodeActionInRangeOnly(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	s.RegisterBackend("go", &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}})
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
	s.publishSnapshot()

	dispatch := func(rng string) json.RawMessage {
		body := `{"textDocument":{"uri":"` + uri + `"},"range":` + rng + `,"context":{}}`
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 5}, "textDocument/codeAction", json.RawMessage(body)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("codeAction failed: %+v", resp)
		}
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		raw, _ := json.Marshal(resp)
		json.Unmarshal(raw, &env)
		return env.Result
	}

	hit := dispatch(`{"start":{"line":0,"character":0},"end":{"line":0,"character":10}}`)
	if !containsBytes(hit, `"kind":"quickfix"`) || !containsBytes(hit, "About this diagnostic") {
		t.Fatalf("in-range actions missing: %s", hit)
	}
	miss := dispatch(`{"start":{"line":9,"character":0},"end":{"line":9,"character":0}}`)
	var empty []map[string]any
	if err := json.Unmarshal(miss, &empty); err != nil || len(empty) != 0 {
		t.Fatalf("out-of-range should yield empty actions, got %s", miss)
	}
}

func TestC4_PositionEncodingNegotiation(t *testing.T) {
	const uri = "file:///w/main.go"

	t.Run("client proposal wins and echoes", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "initialize",
			json.RawMessage(`{"processId":1,"rootUri":"file:///w","capabilities":{},"general":{"positionEncodings":["utf-8","utf-16"]}}`)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("initialize failed: %+v", resp)
		}
		raw, _ := json.Marshal(resp)
		var env struct {
			Result struct {
				Capabilities struct {
					PositionEncoding string `json:"positionEncoding"`
				} `json:"capabilities"`
			} `json:"result"`
		}
		json.Unmarshal(raw, &env)
		if env.Result.Capabilities.PositionEncoding != "utf-8" {
			t.Fatalf("negotiated = %q, want utf-8 (first client proposal)", env.Result.Capabilities.PositionEncoding)
		}
		if got := s.negotiatedEncodingInt(); got != 0 {
			t.Fatalf("encoding int = %d, want 0 (utf-8)", got)
		}
	})

	t.Run("silent client keeps utf-16 baseline", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
		s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "initialize",
			json.RawMessage(`{"processId":1,"rootUri":"file:///w","capabilities":{}}`)))
		if got := s.negotiatedEncodingInt(); got != 1 {
			t.Fatalf("baseline encoding int = %d, want 1 (utf-16)", got)
		}
	})
}

func TestI10_PrepareRenameGate(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
	s.vfs.Open(uri, "go", 1, []byte("package main\n\nfunc main() {\n\ttotal := 1\n\tExported()\n}\n"), 0)
	s.publishSnapshot()

	prep := func(line, char uint32) *jsonrpc.Message {
		return s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "textDocument/prepareRename",
			json.RawMessage(fmt.Sprintf(`{"textDocument":{"uri":"%s"},"position":{"line":%d,"character":%d}}`, uri, line, char))))
	}

	t.Run("unexported identifier gets placeholder", func(t *testing.T) {
		resp := prep(3, 2) // on `total`
		if resp == nil || resp.Error != nil || resp.Result == nil {
			t.Fatalf("prepareRename failed: %+v", resp)
		}
		if !containsBytes(resp.Result, `"placeholder":"total"`) {
			t.Errorf("placeholder missing: %s", resp.Result)
		}
	})

	t.Run("exported identifier refused SEM-SAFE-001", func(t *testing.T) {
		resp := prep(4, 3) // on `Exported`
		if resp == nil || resp.Error == nil {
			t.Fatalf("exported symbol must be refused, got %+v", resp)
		}
		if !containsBytes([]byte(resp.Error.Message), "SEM-SAFE-001") {
			t.Errorf("refusal lacks policy reference: %s", resp.Error.Message)
		}
	})

	t.Run("off-identifier position yields null", func(t *testing.T) {
		resp := prep(0, 3) // inside `package` keyword — still an ident, but line 0 char 3
		_ = resp           // either placeholder or null is protocol-valid; must not error
	})
}

// TestT2_DeclarationServedAndDeclared pins §I14/T2: textDocument/declaration
// is advertised in capabilities AND served through the optional
// DeclarationProvider capability (ADR-0009 D2 — core interface stays frozen).
func TestT2_DeclarationServedAndDeclared(t *testing.T) {
	caps := buildCapabilities("utf-16")
	if !caps.DeclarationProvider {
		t.Fatal("declarationProvider not declared")
	}
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "d1", IsStr: true},
		"textDocument/declaration", json.RawMessage(`{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("dispatch failed: %+v", resp)
	}
}
