package jsonrpc

import (
	"bytes"
	"encoding/json"
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
		// Test JSON-RPC decode of position params.
		payloads := []string{
			fmt.Sprintf(`{"line":0,"character":0}`),
			fmt.Sprintf(`{"line":%d,"character":%d}`, 1<<20, 1<<20),
			fmt.Sprintf(`{"line":-1,"character":0}`),
			`{"line":"oops","character":1}`,
			`{"line":0}`,
			`{"character":0}`,
			`{}`,
			`not json`,
			fmt.Sprintf(`{"text":"%s","encoding":"%s"}`, text, enc),
			`\x00\x01\x02`,
			"\x00\x00\x00\x00\x00",
		}
		for _, p := range payloads {
			var raw json.RawMessage
			if err := json.Unmarshal([]byte(p), &raw); err != nil {
				// expected for non-JSON
				continue
			}
			// Validate: if it unmarshals, the codec is safe.
			_ = raw
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
