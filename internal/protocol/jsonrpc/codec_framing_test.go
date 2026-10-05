package jsonrpc

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// frame builds a Content-Length frame around body verbatim, so a test can hand
// the codec exactly the bytes a hostile or broken peer would send.
func frame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

// TestCodecRejectsMalformedBodies locks the decode failure path. Every branch
// here used to be unreachable from a test, which left §S10's parse-error
// contract unenforced: nothing checked that a broken body yields the typed
// ParseError a client is entitled to answer, rather than an opaque string.
func TestCodecRejectsMalformedBodies(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"not json at all", frame("{not js")},
		{"truncated object", frame(`{"jsonrpc":`)},
		// A JSON-RPC batch is a legal request frame that this decoder cannot
		// represent: ReadMessage decodes into a single *Message, so an array
		// body fails. Asserting the typed error documents the shape; it does
		// not make the behaviour right. transport treats any ReadMessage error
		// as fatal, so one batch frame currently drops the connection instead of
		// getting a per-message ParseError as JSON-RPC 2.0 §5.1 requires. See
		// docs/audit-longterm-spec.md.
		{"json-rpc batch array", frame(`[{"jsonrpc":"2.0","id":1,"method":"x"}]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewCodec().ReadMessage(strings.NewReader(tc.raw))
			var codecErr *CodecError
			if !errors.As(err, &codecErr) {
				t.Fatalf("error = %v (%T), want *CodecError", err, err)
			}
			if codecErr.Code != ParseError {
				t.Errorf("code = %d, want ParseError (%d)", codecErr.Code, ParseError)
			}
		})
	}
}

// TestCodecRejectsBrokenFraming covers the header paths. Each one has to fail
// loudly: a frame the codec cannot size must not be reported as an empty
// message, because the caller reads a nil *Message as end of stream.
func TestCodecRejectsBrokenFraming(t *testing.T) {
	// The size cap is raised for this table so a declared length that is merely
	// wrong is not caught by it first: the point of the body case is that a
	// short body is distinguished from an oversized one.
	const sizeCap = 4096
	for _, tc := range []struct {
		name    string
		raw     string
		wantMsg string
	}{
		{
			name:    "zero length is not a message",
			raw:     "Content-Length: 0\r\n\r\n",
			wantMsg: "invalid Content-Length",
		},
		{
			name: "absent length",
			// readHeader seeds contentLength at -1, so a frame with no
			// Content-Length at all lands on the missing-header check rather
			// than being read as an empty message. The name is accurate.
			raw:     "Content-Type: application/vscode-jsonrpc\r\n\r\n",
			wantMsg: "missing Content-Length header",
		},
		{
			name:    "negative length",
			raw:     "Content-Length: -1\r\n\r\n",
			wantMsg: "missing Content-Length header",
		},
		{
			name:    "non numeric length",
			raw:     "Content-Length: abc\r\n\r\n",
			wantMsg: "invalid Content-Length value",
		},
		{
			name:    "oversized declared length",
			raw:     fmt.Sprintf("Content-Length: %d\r\n\r\n{}", sizeCap+1),
			wantMsg: "message too large",
		},
		{
			name:    "body shorter than declared",
			raw:     "Content-Length: 40\r\n\r\n{\"a\":1}",
			wantMsg: "failed to read message body",
		},
		{
			name:    "header never terminates",
			raw:     "X-Pad: " + strings.Repeat("a", 1100) + "\r\n\r\n",
			wantMsg: "header too large",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Codec{MaxMessageSize: sizeCap}
			msg, err := c.ReadMessage(strings.NewReader(tc.raw))
			if err == nil {
				t.Fatalf("read a message %+v from a frame that cannot be sized", msg)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantMsg)
			}
			if msg != nil {
				t.Errorf("message = %+v, want nil alongside the error", msg)
			}
		})
	}
}

// TestCodecAcceptsToleratedFraming pins the two shapes the reader deliberately
// bends toward rather than rejecting. A client that omits the CR is broken but
// harmless; refusing its frame would cost a session over a byte. A header line
// with no colon is likewise skipped, since the length is all the reader needs
// and the line carries nothing it would otherwise choke on.
func TestCodecAcceptsToleratedFraming(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"lf only header terminator", "Content-Length: 2\n\n{}"},
		{"header line without a colon", "X-Nonsense\nContent-Length: 2\r\n\r\n{}"},
		{"header key case insensitive", "content-length: 2\r\n\r\n{}"},
		{"extra headers ignored", "Content-Type: application/vscode-jsonrpc\r\nX-Trace: 1\r\nContent-Length: 2\r\n\r\n{}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := NewCodec().ReadMessage(strings.NewReader(tc.raw))
			if err != nil {
				t.Fatalf("ReadMessage: %v", err)
			}
			if msg == nil {
				t.Fatal("nil message")
			}
		})
	}
}

// A zero-length frame is a framing fault, not a malformed body: readHeader
// returns 0 and ReadMessage refuses before any JSON is parsed. It lives in the
// framing test above rather than here, where it would not produce a ParseError.
