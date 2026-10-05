package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
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

	// The server has no transport until Run, so outbound traffic cannot be
	// captured by wrapping the send path -- there is nothing to wrap until the
	// loop that will call it exists. Attach a recording transport instead and
	// read the notifications off it.
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

	// LSP 3.17 defines ProgressToken as integer | string. A numeric token used
	// to be dropped, so a client that sent one waited forever for the
	// $/progress pair it had explicitly requested.
	for _, tc := range []struct {
		name     string
		token    string
		wantEcho string
	}{
		{"string token", `"str-token"`, `"token":"str-token"`},
		{"integer token", `4242`, `"token":4242`},
	} {
		t.Run(tc.name+" reaches the client verbatim", func(t *testing.T) {
			rt.reset()
			resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
				jsonrpc.RequestID{Num: 3}, "workspace/symbol",
				json.RawMessage(`{"query":"x","workDoneToken":`+tc.token+`}`)))
			if resp == nil || resp.Error != nil {
				t.Fatalf("workspaceSymbol failed: %+v", resp)
			}
			progress := rt.filterMethod("$/progress")
			if len(progress) < 2 {
				t.Fatalf("expected >=2 $/progress notifications for token %s, got %d",
					tc.token, len(progress))
			}
			// The token must come back byte-identical, not stringified: a client
			// matches the notification against the value it sent.
			for i, msg := range progress {
				if !jsonContains(msg, tc.wantEcho) {
					t.Errorf("notification %d carries the wrong token: %s", i, msg.Params)
				}
			}
		})
	}

	t.Run("an explicit null token stays silent", func(t *testing.T) {
		rt.reset()
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 4}, "workspace/symbol",
			json.RawMessage(`{"query":"x","workDoneToken":null}`)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("workspaceSymbol failed: %+v", resp)
		}
		if n := len(rt.filterMethod("$/progress")); n != 0 {
			t.Errorf("progress reported for a null token: %d notifications", n)
		}
	})
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

type completionListBackend struct {
	mockBackend
	result languages.CompletionList
}

func (b *completionListBackend) CompletionList(context.Context, languages.CompletionRequest) (languages.CompletionList, error) {
	return b.result, nil
}

type parentRequestIDBackend struct {
	mockBackend
	completionID     json.RawMessage
	documentSymbolID json.RawMessage
}

func (b *parentRequestIDBackend) CompletionList(_ context.Context, req languages.CompletionRequest) (languages.CompletionList, error) {
	b.completionID = append(json.RawMessage(nil), req.ParentRequestID...)
	return languages.CompletionList{}, nil
}

func (b *parentRequestIDBackend) DocumentSymbols(_ context.Context, req languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	b.documentSymbolID = append(json.RawMessage(nil), req.ParentRequestID...)
	return nil, nil
}

func TestCompletionAndDocumentSymbolPreserveParentRequestID(t *testing.T) {
	const uri = "file:///w/main.cpp"
	s := New(DefaultConfig())
	backend := &parentRequestIDBackend{mockBackend: mockBackend{langID: "cpp", exts: []string{".cpp"}}}
	s.RegisterBackend("cpp", backend)
	s.vfs.Open(uri, "cpp", 1, []byte("int main() {}\n"), 0)

	cases := []struct {
		method string
		id     jsonrpc.RequestID
		params string
		got    *json.RawMessage
	}{
		{
			method: "textDocument/completion",
			id:     jsonrpc.RequestID{Num: 41},
			params: `{"textDocument":{"uri":"` + uri + `"},"position":{"line":0,"character":0}}`,
			got:    &backend.completionID,
		},
		{
			method: "textDocument/documentSymbol",
			id:     jsonrpc.RequestID{Str: "syntax-42", IsStr: true},
			params: `{"textDocument":{"uri":"` + uri + `"}}`,
			got:    &backend.documentSymbolID,
		},
	}
	for _, tc := range cases {
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(tc.id, tc.method, json.RawMessage(tc.params)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("Dispatch(%s) = %+v", tc.method, resp)
		}
		want, err := tc.id.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(*tc.got) != string(want) {
			t.Errorf("%s parent request ID = %s, want %s", tc.method, *tc.got, want)
		}
	}
}

func TestCompletionListProviderPreservesRequestMetadata(t *testing.T) {
	const uri = "file:///w/main.go"
	for _, incomplete := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "incomplete"}[incomplete], func(t *testing.T) {
			s := New(DefaultConfig())
			s.RegisterBackend("go", &completionListBackend{
				mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
				result: languages.CompletionList{
					IsIncomplete: incomplete,
					Items: []languages.CompletionItem{{
						Label: "symbol", Kind: int(languages.CompletionFunction), Detail: "func symbol()",
						Documentation: "**symbol**", InsertText: "symbol($0)", SortText: "01", FilterText: "sym",
					}},
				},
			})
			s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
			raw := dispatchCompletion(t, s, uri)
			var got struct {
				IsIncomplete bool `json:"isIncomplete"`
				Items        []struct {
					Label         string `json:"label"`
					Documentation string `json:"documentation"`
					InsertText    string `json:"insertText"`
					SortText      string `json:"sortText"`
					FilterText    string `json:"filterText"`
				} `json:"items"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("decode completion response: %v", err)
			}
			if got.IsIncomplete != incomplete || len(got.Items) != 1 || got.Items[0].Label != "symbol" ||
				got.Items[0].Documentation != "**symbol**" || got.Items[0].InsertText != "symbol($0)" ||
				got.Items[0].SortText != "01" || got.Items[0].FilterText != "sym" {
				t.Fatalf("completion response lost list or item metadata: %+v", got)
			}
		})
	}
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
