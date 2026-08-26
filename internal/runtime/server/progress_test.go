package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// TestC8_ProgressNotifications pins §C8 work-done reporting: a request that
// carries a workDoneToken yields begin+end notifications on the wire; a
// request without one stays silent.
func TestC8_ProgressNotifications(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)

	var notifications []*jsonrpc.Message
	s.mu.Lock()
	// Capture outbound traffic by wrapping the transport send path.
	orig := s.send
	if orig == nil {
		s.mu.Unlock()
		t.Fatal("send nil")
	}
	s.mu.Unlock()

	// The server has no transport until Run; drive the notification helpers
	// directly and inspect what they emit through a recording transport.
	rt := &recordingTransport{}
	s.mu.Lock()
	s.transport = rt
	s.mu.Unlock()

	t.Run("token present emits begin and end", func(t *testing.T) {
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "textDocument/references",
			json.RawMessage(`{"textDocument":{"uri":"`+uri+`"},"position":{"line":0,"character":0},"context":{"includeDeclaration":false},"workDoneToken":"tok-42"}`)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("references failed: %+v", resp)
		}
		progress := rt.filterMethod("$/progress")
		if len(progress) < 2 {
			t.Fatalf("expected >=2 $/progress notifications, got %d", len(progress))
		}
		first, last := progress[0], progress[len(progress)-1]
		if !jsonContains(first, `"kind":"begin"`) || !jsonContains(first, `"title":"Finding references"`) || !jsonContains(first, `"token":"tok-42"`) {
			t.Errorf("begin malformed: %s", first.Params)
		}
		if !jsonContains(last, `"kind":"end"`) {
			t.Errorf("end malformed: %s", last.Params)
		}
	})

	t.Run("no token stays silent", func(t *testing.T) {
		rt.reset()
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 2}, "workspace/symbol",
			json.RawMessage(`{"query":"x"}`)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("workspaceSymbol failed: %+v", resp)
		}
		if n := len(rt.filterMethod("$/progress")); n != 0 {
			t.Errorf("unexpected progress notifications without token: %d", n)
		}
	})
	_ = notifications
	_ = orig
}

// recordingTransport captures outbound messages (async notification writers
// race the test reader, so the log is mutex-guarded).
type recordingTransport struct {
	mu   sync.Mutex
	msgs []*jsonrpc.Message
}

func (r *recordingTransport) Read(_ context.Context) (*jsonrpc.Message, error) { return nil, nil }
func (r *recordingTransport) Write(_ context.Context, m *jsonrpc.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
	return nil
}
func (r *recordingTransport) Close() error          { return nil }
func (r *recordingTransport) Done() <-chan struct{} { return make(chan struct{}) }

func (r *recordingTransport) filterMethod(m string) []*jsonrpc.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*jsonrpc.Message
	for _, x := range r.msgs {
		if x.Method == m {
			out = append(out, x)
		}
	}
	return out
}

func (r *recordingTransport) reset() { r.mu.Lock(); defer r.mu.Unlock(); r.msgs = nil }

func jsonContains(m *jsonrpc.Message, sub string) bool {
	return m != nil && len(m.Params) > 0 && containsBytes(m.Params, sub)
}

func containsBytes(b []byte, sub string) bool {
	return len(sub) > 0 && len(b) >= len(sub) && indexOf(b, []byte(sub)) >= 0
}

func indexOf(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// incompleteBackend declares heuristic completion (§I9).
type incompleteBackend struct{ mockBackend }

func (b *incompleteBackend) CompletionIsIncomplete() bool { return true }

// TestI9_CompletionIsIncompleteNegotiation pins the §I9 projection: bridges
// declaring heuristic completion get IsIncomplete=true on the wire; others
// stay false.
func TestI9_CompletionIsIncompleteNegotiation(t *testing.T) {
	const uri = "file:///w/main.go"

	t.Run("declaring backend projects true", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &incompleteBackend{mockBackend{langID: "go", exts: []string{".go"}}})
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		raw := dispatchCompletion(t, s, uri)
		if !containsBytes(raw, `"isIncomplete":true`) {
			t.Errorf("expected isIncomplete true, got %s", raw)
		}
	})

	t.Run("plain backend stays false", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		raw := dispatchCompletion(t, s, uri)
		if containsBytes(raw, `"isIncomplete":true`) {
			t.Errorf("expected isIncomplete false, got %s", raw)
		}
	})
}

func dispatchCompletion(t *testing.T, s *Server, uri string) json.RawMessage {
	t.Helper()
	resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 9}, "textDocument/completion",
		json.RawMessage(`{"textDocument":{"uri":"`+uri+`"},"position":{"line":0,"character":0}}`)))
	if resp == nil || resp.Error != nil {
		t.Fatalf("completion failed: %+v", resp)
	}
	return resp.Result
}
