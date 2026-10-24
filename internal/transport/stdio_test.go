package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
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

// TestBatchFrameIsPerStreamFatal pins the CURRENT end-to-end behaviour of a
// JSON-RPC batch frame on the stdio transport — the chain §3.1 of
// docs/audit-longterm-spec.md calls out:
//
//	array body frame → Codec.ReadMessage decodes into a single *Message
//	  (codec.go ReadMessage, json.Unmarshal on a struct) → *CodecError{ParseError}
//	  → StdioTransport.readLoop treats ANY ReadMessage error as fatal
//	  → errCh + close(done) → the whole connection terminates.
//
// JSON-RPC 2.0 §5.1 wants an unparseable message answered with a per-message
// ParseError while the stream lives on; here per-message recovery has
// degraded into per-stream fatal, so one legal batch frame kills the session
// and anything framed after it is never read.
//
// 语义未拍板（§7.5，docs/audit-longterm-spec.md §7 第 5 条「JSON-RPC 批量的
// 语义」）：拒绝并保持连接 vs 实现批量；本测试钉的是现状，拍板后必须随选定
// 语义改写。
func TestBatchFrameIsPerStreamFatal(t *testing.T) {
	frame := func(body string) string {
		return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	}
	// Three frames on one in-memory stream: a good message, a legal JSON-RPC
	// batch (array body — legal per JSON-RPC 2.0, but unrepresentable for a
	// decoder that targets a single *Message), then another good message.
	var stream strings.Builder
	stream.WriteString(frame(`{"jsonrpc":"2.0","id":1,"method":"before/batch"}`))
	stream.WriteString(frame(`[{"jsonrpc":"2.0","id":2,"method":"batch/a"},{"jsonrpc":"2.0","id":3,"method":"batch/b"}]`))
	stream.WriteString(frame(`{"jsonrpc":"2.0","id":7,"method":"after/batch"}`))

	tr := NewStdioTransport(strings.NewReader(stream.String()), &bytes.Buffer{})
	defer tr.Close()

	// Drain until the terminal error. The buffered good frame and the error
	// may arrive in either order (Read selects among ready cases), so the
	// loop just keeps reading; two deliveries can never happen: the batch
	// itself (it never becomes a Message) and the frame after it (readLoop
	// is dead before reaching it).
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
			t.Fatal("Read delivered a nil message: the batch frame must surface as a terminal error, never as a nil delivery")
		}
		if msg.Method == "after/batch" {
			t.Fatal("message framed after the batch was delivered: the current chain is per-stream fatal and must not read past the batch")
		}
	}

	// The terminal error is the codec's typed ParseError, surfaced through
	// errCh — not a bare io.EOF: readLoop sends the ReadMessage error before
	// closing done, and Read's done-branch drains errCh.
	var codecErr *jsonrpc.CodecError
	if !errors.As(gotErr, &codecErr) {
		t.Fatalf("terminal error = %v (%T), want *jsonrpc.CodecError", gotErr, gotErr)
	}
	if codecErr.Code != jsonrpc.ParseError {
		t.Errorf("code = %d, want ParseError (%d)", codecErr.Code, jsonrpc.ParseError)
	}

	// readLoop exited: the connection is down, exactly the per-stream
	// fatal degradation §3.1 describes.
	select {
	case <-tr.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done() did not close after the batch frame: transport survived a per-message fault")
	}

	// And it stays down: a later Read errors (EOF once errCh is drained)
	// instead of reviving to deliver the stranded after/batch frame.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if msg, err := tr.Read(ctx); err == nil {
		t.Fatalf("Read after termination delivered %+v; the stream must stay dead", msg)
	}
}

// TestTransportInterface verifies both transports satisfy the Transport interface.
var _ Transport = (*StdioTransport)(nil)
var _ Transport = (*TCPTransport)(nil)
