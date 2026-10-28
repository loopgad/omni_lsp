package transport

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// frameFraming mirrors the LSP Content-Length framing for test streams.
func frameFraming(body string) string {
	return "Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestStdioReadFrameBatchCapIsPerStreamFatal pins the transport-side wiring of
// the batch element cap (audit circle 2, finding 1 + 4): a frame over the cap
// is a framing fault — Read surfaces a ParseError and the outbound stream
// carries ZERO per-element replies. Pre-fix the same frame made readLoop
// write one InvalidRequest reply per element (108x output amplification on a
// 512KiB body) and keep the session alive.
func TestStdioReadFrameBatchCapIsPerStreamFatal(t *testing.T) {
	body := "[" + strings.TrimSuffix(strings.Repeat(`1,`, jsonrpc.DefaultMaxBatchElements+200), `,`) + `]`
	var out bytes.Buffer
	tr := NewStdioTransport(strings.NewReader(frameFraming(body)), &out)
	defer tr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := tr.Read(ctx)
	if err == nil {
		t.Fatalf("oversize batch delivered %+v, want a per-stream ParseError", msg)
	}
	var codecErr *jsonrpc.CodecError
	if !errors.As(err, &codecErr) || codecErr.Code != jsonrpc.ParseError {
		t.Fatalf("error = %v (%T), want *jsonrpc.CodecError{ParseError}", err, err)
	}
	if out.Len() != 0 {
		t.Fatalf("oversize batch wrote %d outbound reply bytes, want none", out.Len())
	}
}
