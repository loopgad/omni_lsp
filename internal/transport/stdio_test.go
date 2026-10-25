package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// TestStdioTransportLifecycle tests the full lifecycle of StdioTransport.
func TestStdioTransportLifecycle(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()

	tcp := NewStdioTransport(reader, writer)
	defer tcp.Close()

	if tcp.Done() == nil {
		t.Fatal("Done() returned nil")
	}

	// Write a message
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "test", nil)
	if err := tcp.Write(context.Background(), msg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Read it back
	resp, err := tcp.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil message")
	}
	if resp.Method != "test" {
		t.Errorf("expected method 'test', got %q", resp.Method)
	}
}

// TestStdioTransportClosed returns errors after close.
func TestStdioTransportClosed(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()

	tcp := NewStdioTransport(reader, writer)
	tcp.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := tcp.Read(ctx); err == nil {
		t.Error("expected error after close")
	}
	if err := tcp.Write(ctx, nil); err == nil {
		t.Error("expected error on write after close")
	}
}

// TestStdioTransportContextCancellation tests ctx cancellation during Read.
func TestStdioTransportContextCancellation(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()

	tcp := NewStdioTransport(reader, writer)
	defer tcp.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := tcp.Read(ctx)
	if err == nil {
		t.Error("expected error on cancelled context")
	}
}

// TestStdioTransportReaderEOF tests behavior when reader returns EOF.
func TestStdioTransportReaderEOF(t *testing.T) {
	empty := strings.NewReader("")
	tcp := NewStdioTransport(empty, &bytes.Buffer{})
	defer tcp.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := tcp.Read(ctx)
	if err == nil {
		t.Error("expected error when reader is empty")
	}
}

// TestStdioTransportDuplicateClose is idempotent.
func TestStdioTransportDuplicateClose(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()

	tcp := NewStdioTransport(reader, writer)
	if err := tcp.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tcp.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestTCPTransportLifecycle tests TCP transport with a loopback connection.
func TestTCPTransportLifecycle(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("skip: %v", err)
	}
	defer listener.Close()

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		serverConnCh <- conn
	}()

	clientConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clientConn.Close()

	serverConn := <-serverConnCh
	defer serverConn.Close()

	serverTCP := NewTCPTransport(serverConn)
	clientTCP := NewTCPTransport(clientConn)
	defer serverTCP.Close()
	defer clientTCP.Close()

	// Server sends a message
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "ping", nil)
	if err := serverTCP.Write(context.Background(), msg); err != nil {
		t.Fatalf("server Write: %v", err)
	}

	// Client reads it
	resp, err := clientTCP.Read(context.Background())
	if err != nil {
		t.Fatalf("client Read: %v", err)
	}
	if resp == nil || resp.Method != "ping" {
		t.Error("expected ping message")
	}
}

// TestTCPTransportClosed returns errors after close.
func TestTCPTransportClosed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("skip: %v", err)
	}
	defer listener.Close()

	serverConnCh := make(chan net.Conn, 1)
	go func() {
		conn, _ := listener.Accept()
		serverConnCh <- conn
	}()

	clientConn, err := net.Dial("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("skip: dial failed")
	}
	defer clientConn.Close()

	serverConn := <-serverConnCh
	defer serverConn.Close()

	tcp := NewTCPTransport(serverConn)
	tcp.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := tcp.Read(ctx); err == nil {
		t.Error("expected error after close")
	}
	if err := tcp.Write(ctx, nil); err == nil {
		t.Error("expected error on write after close")
	}
}

// TestS13_BufferFullDropsAndCloses locks the documented F6 backpressure
// policy: when msgCh is full the transport drops the arriving message,
// surfaces io.ErrShortBuffer as a terminal error, and disconnects (Done
// closes). It never keeps running with a silent hole in the stream, and the
// dropped message never produces a response.
func TestS13_BufferFullDropsAndCloses(t *testing.T) {
	const bufCap = 32 // must match NewStdioTransport's msgCh capacity
	const victimID = int64(bufCap + 1)

	var stream bytes.Buffer
	frame := func(id int64) []byte {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"s13/probe"}`, id)
		return []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body))
	}
	for id := int64(1); id <= victimID; id++ {
		stream.Write(frame(id)) // ids 1..32 fill msgCh; #33 is the overflow victim
	}

	tr := NewStdioTransport(bytes.NewReader(stream.Bytes()), &bytes.Buffer{})
	defer tr.Close()

	// No consumer exists yet, so readLoop drains the in-memory stream
	// uncontended: ids 1..32 fill msgCh, id 33 hits the full buffer and must
	// be dropped with a terminal error. Done closing proves readLoop took the
	// overflow exit (it cannot return without either dropping #33 or EOF,
	// and the in-memory stream never yields EOF mid-frame sequence).
	select {
	case <-tr.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("transport did not terminate after buffer overflow")
	}

	// Drain: buffered messages and the terminal error may arrive in any order
	// (Read selects among ready cases), but every delivery must be one of the
	// buffered messages — the victim must never come through.
	delivered := 0
	sawShortBuffer := false
	for !sawShortBuffer && delivered < bufCap {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		msg, err := tr.Read(ctx)
		cancel()
		switch {
		case err == nil:
			if msg.ID != nil && msg.ID.Num == victimID {
				t.Fatalf("dropped message id %d was delivered", victimID)
			}
			delivered++
		case errors.Is(err, io.ErrShortBuffer):
			sawShortBuffer = true
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if !sawShortBuffer {
		t.Fatal("expected terminal io.ErrShortBuffer after buffer overflow")
	}

	select {
	case <-tr.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("transport did not reach Done after overflow")
	}
}

// lockedBuffer is an io.Writer the test can read after the readLoop has
// written to it, without racing the transport's reader goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// parseFramed decodes a stream of Content-Length frames into messages.
func parseFramed(t *testing.T, raw []byte) []*jsonrpc.Message {
	t.Helper()
	var msgs []*jsonrpc.Message
	for len(raw) > 0 {
		const sep = "\r\n\r\n"
		i := bytes.Index(raw, []byte(sep))
		if i < 0 {
			t.Fatalf("framed stream has no header terminator: %q", raw)
		}
		const prefix = "Content-Length: "
		hdr := string(raw[:i])
		j := strings.Index(hdr, prefix)
		if j < 0 {
			t.Fatalf("frame header missing Content-Length: %q", hdr)
		}
		rest := hdr[j+len(prefix):]
		if k := strings.Index(rest, "\r\n"); k >= 0 {
			rest = rest[:k]
		}
		n, err := strconv.Atoi(strings.TrimSpace(rest))
		if err != nil {
			t.Fatalf("bad Content-Length in %q: %v", hdr, err)
		}
		body := raw[i+len(sep):]
		if len(body) < n {
			t.Fatalf("frame body truncated: declared %d, have %d", n, len(body))
		}
		var m jsonrpc.Message
		if err := json.Unmarshal(body[:n], &m); err != nil {
			t.Fatalf("framed body %q does not decode: %v", body[:n], err)
		}
		msgs = append(msgs, &m)
		raw = body[n:]
	}
	return msgs
}

// TestBatchElementsAnsweredIndividually replaces TestBatchFrameIsPerStreamFatal
// (deleted in this commit) per the 2026-10-25 ruling (docs/audit-longterm-spec.md
// 终局裁决第 1 条): framing integrity and element validity are two layers. A
// batch frame is a legal frame — its ARRAY body parses, so the frame is whole
// — and §6 isolates its elements: a valid element is delivered as usual, an
// element that fails to decode is answered with exactly one error response
// (InvalidRequest; the element's id echoed when detectable, null otherwise),
// and the stream carries on. The old pin asserted the pre-ruling chain
// (array body → Codec.ParseError → readLoop fatal → dead session); its
// terminal assertions survive — narrowed to the framing-fault class — in
// TestUnparseableFrameRemainsPerStreamFatal below.
func TestBatchElementsAnsweredIndividually(t *testing.T) {
	frame := func(body string) string {
		return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	}
	// One batch frame, four elements in wire order: a valid request, a bare
	// number (valid JSON, no message shape, no detectable id), an element
	// whose method field is mistyped (fails to decode, id 5 IS detectable),
	// and a valid notification. Then a frame after the batch: reaching it at
	// all proves the stream survived.
	//
	// The frames ride an io.Pipe that is never closed: Read's select races
	// buffered messages against a terminal error, so a finite stream cannot
	// prove survival deterministically (the old pin's comment said the same
	// of its own drain loop). A still-open stream parks readLoop on the next
	// header and makes every assertion below order-deterministic.
	pr, pw := io.Pipe()
	go func() {
		pw.Write([]byte(frame(`[{"jsonrpc":"2.0","id":2,"method":"batch/a"},1,{"id":5,"method":123},{"jsonrpc":"2.0","method":"batch/n"}]`)))
		pw.Write([]byte(frame(`{"jsonrpc":"2.0","id":7,"method":"after/batch"}`)))
		// No close: the stream stays open for the assertions below.
	}()

	out := &lockedBuffer{}
	tr := NewStdioTransport(pr, out)
	defer func() {
		tr.Close()
		pw.Close() // release readLoop, parked on the next header
	}()

	// The two valid elements and the after-frame are delivered, in wire
	// order. msgCh is FIFO, so the sequence is deterministic.
	var got []string
	for len(got) < 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		msg, err := tr.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Read #%d: %v — the stream must survive a batch frame and its invalid elements", len(got)+1, err)
		}
		if msg == nil {
			t.Fatalf("Read #%d delivered nil", len(got)+1)
		}
		got = append(got, msg.Method)
	}
	want := []string{"batch/a", "batch/n", "after/batch"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("deliveries = %v, want %v (valid elements in wire order, after-frame reachable)", got, want)
		}
	}

	// The session is alive, not limping: Done stays open.
	select {
	case <-tr.Done():
		t.Fatal("stream terminated by a batch frame: element isolation regressed to per-stream fatal")
	case <-time.After(200 * time.Millisecond):
	}

	// Exactly one error response per invalid element, in wire order, written
	// to the outbound stream: InvalidRequest, id null for the bare number and
	// id 5 for the mistyped element whose id is still detectable. By the time
	// the after-frame was delivered both replies were already on the wire
	// (readLoop writes them before pushing later elements), so reading the
	// buffer now is race-free through lockedBuffer.
	replies := parseFramed(t, out.Bytes())
	if len(replies) != 2 {
		t.Fatalf("outbound stream carries %d messages, want exactly the 2 per-element error responses: %+v", len(replies), replies)
	}
	if replies[0].Error == nil || replies[0].Error.Code != jsonrpc.InvalidRequest {
		t.Errorf("reply 1 = %+v, want InvalidRequest error response for element `1`", replies[0])
	}
	if id := replies[0].ID; id != nil && !id.IsNull {
		t.Errorf("reply 1 id = %+v, want null (no id detectable in `1`; wire null decodes to a nil pointer)", id)
	}
	if replies[1].Error == nil || replies[1].Error.Code != jsonrpc.InvalidRequest {
		t.Errorf("reply 2 = %+v, want InvalidRequest error response for {\"id\":5,\"method\":123}", replies[1])
	}
	if replies[1].ID == nil || !replies[1].ID.Equals(jsonrpc.RequestID{Num: 5}) {
		t.Errorf("reply 2 id = %+v, want 5 (id detectable despite the decode failure)", replies[1].ID)
	}
}

// TestUnparseableFrameRemainsPerStreamFatal is the counter-example the ruling
// requires alongside TestBatchElementsAnsweredIndividually: the OLD pin
// (TestBatchFrameIsPerStreamFatal, deleted in this commit) asserted that a
// batch frame kills the stream. The ruling narrows that to the framing-fault
// class — a frame that cannot be parsed AT ALL is a framing-integrity breach
// and stays per-stream fatal — while element validity inside a parseable
// array never touches the session. Both subtests keep the old pin's terminal
// assertions (typed ParseError, Done closes, stream stays dead, later frames
// stranded) on bodies the ruling keeps fatal.
func TestUnparseableFrameRemainsPerStreamFatal(t *testing.T) {
	frame := func(body string) string {
		return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	}
	for _, tc := range []struct {
		name string
		bad  string
	}{
		// Non-array garbage: the framing layer never yields a message.
		{"body is not json", `{not js`},
		// The ruling's cut line: a body that STARTS as an array but does not
		// parse is still an unparseable frame — element isolation only
		// applies once the array itself parsed. Element faults are isolated,
		// framing faults are not.
		{"array with a syntax error inside", `[{"jsonrpc":"2.0","id":1,"method":"x"}, oops]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stream strings.Builder
			stream.WriteString(frame(`{"jsonrpc":"2.0","id":1,"method":"before/bad"}`))
			stream.WriteString(frame(tc.bad))
			stream.WriteString(frame(`{"jsonrpc":"2.0","id":7,"method":"after/bad"}`))

			tr := NewStdioTransport(strings.NewReader(stream.String()), &bytes.Buffer{})
			defer tr.Close()

			// Drain until the terminal error; the buffered good frame and the
			// error may arrive in either order. The after/bad frame can never
			// be delivered: readLoop is dead before reaching it.
			var gotErr error
			for gotErr == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				msg, err := tr.Read(ctx)
				cancel()
				if err != nil {
					gotErr = err
					break
				}
				if msg == nil {
					t.Fatal("Read delivered a nil message")
				}
				if msg.Method == "after/bad" {
					t.Fatal("message framed after the unparseable frame was delivered: framing faults must stay per-stream fatal")
				}
			}

			var codecErr *jsonrpc.CodecError
			if !errors.As(gotErr, &codecErr) {
				t.Fatalf("terminal error = %v (%T), want *jsonrpc.CodecError", gotErr, gotErr)
			}
			if codecErr.Code != jsonrpc.ParseError {
				t.Errorf("code = %d, want ParseError (%d)", codecErr.Code, jsonrpc.ParseError)
			}

			select {
			case <-tr.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("Done() did not close after an unparseable frame")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if msg, err := tr.Read(ctx); err == nil {
				t.Fatalf("Read after termination delivered %+v; the stream must stay dead", msg)
			}
		})
	}
}

// TestTCPBatchElementsAnsweredIndividually covers the TCPTransport readLoop
// connection point — the second loop switched to ReadFrame in the batch
// ruling (docs/audit-longterm-spec.md 终局裁决第 1 条). Same semantics as
// stdio: valid elements delivered, invalid elements answered with one
// InvalidRequest response each on the connection, stream alive afterwards.
// The eighth pinning round deliberately added no TCP case; this one guards
// the implementation.
func TestTCPBatchElementsAnsweredIndividually(t *testing.T) {
	frame := func(body string) string {
		return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	}
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	tr := NewTCPTransport(server)
	defer tr.Close()

	// Full-duplex pipe: the writer goroutine feeds frames; the reader
	// goroutine collects whatever the transport writes back (the two
	// per-element error responses). Without the concurrent reader the
	// transport's first reply write would deadlock against this test's
	// second frame write.
	var replies []byte
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 4096)
		for {
			n, err := client.Read(buf)
			replies = append(replies, buf[:n]...)
			if err != nil {
				return
			}
		}
	}()
	go func() {
		client.Write([]byte(frame(`[{"jsonrpc":"2.0","id":2,"method":"batch/a"},9]`)))
		client.Write([]byte(frame(`{"jsonrpc":"2.0","id":7,"method":"after/batch"}`)))
		// No close: the transport parks on the next header, keeping Done
		// open and every delivery deterministic.
	}()

	var got []string
	for len(got) < 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		msg, err := tr.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("Read #%d: %v — the stream must survive a batch frame", len(got)+1, err)
		}
		got = append(got, msg.Method)
	}
	if got[0] != "batch/a" || got[1] != "after/batch" {
		t.Fatalf("deliveries = %v, want [batch/a after/batch]", got)
	}
	select {
	case <-tr.Done():
		t.Fatal("TCP stream terminated by a batch frame")
	case <-time.After(200 * time.Millisecond):
	}

	// Close the client end explicitly BEFORE waiting on the reader
	// goroutine: readDone only completes when client.Read errors, and a
	// defer-run close would deadlock the wait it is supposed to release.
	client.Close()
	<-readDone
	parsed := parseFramed(t, replies)
	if len(parsed) != 1 {
		t.Fatalf("connection carries %d replies, want exactly the 1 per-element error response: %+v", len(parsed), parsed)
	}
	if parsed[0].Error == nil || parsed[0].Error.Code != jsonrpc.InvalidRequest {
		t.Errorf("reply = %+v, want InvalidRequest for element `9`", parsed[0])
	}
	if id := parsed[0].ID; id != nil && !id.IsNull {
		t.Errorf("reply id = %+v, want null (nothing detectable in `9`)", id)
	}
}

// TestTransportInterface verifies both transports satisfy the Transport interface.
var _ Transport = (*StdioTransport)(nil)
var _ Transport = (*TCPTransport)(nil)
