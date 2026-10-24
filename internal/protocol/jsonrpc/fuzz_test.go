package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// FuzzJSONRPC exercises JSON-RPC framing and message decoding
// with adversarial payloads. Invariant: never panic.
func FuzzJSONRPC(f *testing.F) {
	for _, seed := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"test","params":null}`,
		`{"jsonrpc":"2.0","id":"abc","method":"textDocument/hover","params":{"textDocument":{"uri":"file:///x"}}}`,
		`{"jsonrpc":"2.0","method":"initialized"}`,
		`{"jsonrpc":"2.0","id":1,"result":null}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		c := NewCodec()

		var buf bytes.Buffer
		w := io.LimitReader(bytes.NewReader(data), 64*1024)
		_, _ = io.Copy(&buf, w)

		for {
			msg, err := c.ReadMessage(&buf)
			if err != nil {
				break
			}
			if msg == nil {
				break
			}
			_ = msg.IsRequest()
			_ = msg.IsNotification()
			_ = msg.IsResponse()
			_ = msg.IsError()
			_, _ = json.Marshal(msg)
			if buf.Len() == 0 {
				break
			}
		}

		adversarial := []string{
			"Content-Length: " + string(data[:min(len(data), 80)]) + "\r\n\r\n",
			"Content-Length: -1\r\n\r\n",
			"Content-Length: 99999999\r\n\r\nvery short body",
			"\x00\x00\x00\x00\r\n\r\n",
			strings.Repeat(`{"a":`, 500) + "\r\n\r\n",
		}
		for _, v := range adversarial {
			var b bytes.Buffer
			_, _ = b.WriteString(v)
			_, _ = c.ReadMessage(&b)
		}
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// FuzzPosition encodes adversarial positions into JSON-RPC params
// and validates decoding never panics.
func FuzzPosition(f *testing.F) {
	texts := [][]byte{
		[]byte("hello world"),
		[]byte("\u4f60\u597d\u4e16\u754c"),
		[]byte("a\xf0\x9f\x98\x80b"),
		[]byte("line1\nline2\n"),
		[]byte(""),
		[]byte("x"),
	}
	for _, t := range texts {
		for _, enc := range []string{"utf-8", "utf-16", "utf-32"} {
			f.Add(string(t), enc)
		}
	}

	f.Fuzz(func(t *testing.T, text string, enc string) {
		// Test JSON-RPC decode of position params. Every payload is driven
		// through the real wire path: frame it as a Content-Length body and
		// read it back through the codec. The oracle is a direct
		// json.Unmarshal into a Message — the framed path must agree with
		// it: a body the oracle rejects must come back as a typed
		// ParseError CodecError, a body it accepts must decode into a
		// Message whose kind predicates match its decoded shape. Never a
		// panic on either side.
		//
		// The oracle cannot be json.Valid: the fuzzer controls text/enc and
		// can inject quotes, so a body may be legal JSON yet carry a
		// mistyped known field (e.g. "jsonrpc":123) — legal JSON, illegal
		// Message, still a ParseError.
		payloads := []string{
			`{"line":0,"character":0}`,
			fmt.Sprintf(`{"line":%d,"character":%d}`, 1<<20, 1<<20),
			`{"line":-1,"character":0}`,
			`{"line":"oops","character":1}`,
			`{"line":0}`,
			`{"character":0}`,
			`{}`,
			`not json`,
			fmt.Sprintf(`{"text":"%s","encoding":"%s"}`, text, enc),
			`\x00\x01\x02`,
			"\x00\x00\x00\x00\x00",
		}
		c := NewCodec()
		for _, p := range payloads {
			var oracle Message
			malformed := json.Unmarshal([]byte(p), &oracle) != nil

			msg, err := c.ReadMessage(strings.NewReader(frame(p)))

			if malformed {
				if err == nil {
					t.Fatalf("payload %q: malformed body must be rejected, got message %+v", p, msg)
				}
				if msg != nil {
					t.Fatalf("payload %q: malformed body must not yield a message: %+v", p, msg)
				}
				var codecErr *CodecError
				if !errors.As(err, &codecErr) {
					t.Fatalf("payload %q: error = %v (%T), want *CodecError", p, err, err)
				}
				if codecErr.Code != ParseError {
					t.Fatalf("payload %q: code = %d, want ParseError (%d)", p, codecErr.Code, ParseError)
				}
				continue
			}

			if err != nil {
				t.Fatalf("payload %q: well-formed body must decode, got error: %v", p, err)
			}
			if msg == nil {
				t.Fatalf("payload %q: well-formed body decoded to nil message", p)
			}
			hasM := msg.methodPresent || msg.Method != ""
			if got, want := msg.IsRequest(), msg.ID != nil && hasM; got != want {
				t.Errorf("payload %q: IsRequest = %v, want %v", p, got, want)
			}
			if got, want := msg.IsNotification(), msg.ID == nil && hasM; got != want {
				t.Errorf("payload %q: IsNotification = %v, want %v", p, got, want)
			}
			if got, want := msg.IsResponse(), msg.ID != nil && !hasM; got != want {
				t.Errorf("payload %q: IsResponse = %v, want %v", p, got, want)
			}
			if got, want := msg.IsError(), msg.ID != nil && !hasM && msg.Error != nil; got != want {
				t.Errorf("payload %q: IsError = %v, want %v", p, got, want)
			}
		}
	})
}

// TestY2_UnknownParamsFieldsTolerated pins §A7 field-level tolerance: params
// objects carrying keys the server has never heard of must decode cleanly —
// unknown fields are the forward-compat contract, not an error.
func TestY2_UnknownParamsFieldsTolerated(t *testing.T) {
	msg := []byte(`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"processId":1,"rootUri":"file:///w","capabilities":{"future":{"xrayVision":true}},"someFutureTopLevel":"whatever","workspaceFolders":[]}}`)
	var m Message
	if err := json.Unmarshal(msg, &m); err != nil {
		t.Fatalf("unknown fields must not break decode: %v", err)
	}
	if m.Method != "initialize" {
		t.Fatalf("method = %q", m.Method)
	}
	var p struct {
		ProcessID int `json:"processId"`
	}
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatalf("params with unknown fields must unmarshal: %v", err)
	}
	if p.ProcessID != 1 {
		t.Fatalf("known field lost: %d", p.ProcessID)
	}
}
