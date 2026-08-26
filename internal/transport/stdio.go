// Package transport provides transport-layer implementations for JSON-RPC.
//
// Invariants:
//  1. Transport is purely framing + I/O — no semantic logic.
//  2. Transport must be safe to close from any goroutine.
//  3. A closed transport returns errors on all subsequent operations.
//  4. Context cancellation must be respected for reads and writes.
//  5. Exactly one reader goroutine per transport instance (F15).
package transport

import (
	"bufio"
	"context"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// Transport is the interface for JSON-RPC message transport.
type Transport interface {
	// Read reads the next message. Blocks until a message is available or context is done.
	Read(ctx context.Context) (*jsonrpc.Message, error)
	// Write writes a message. Must be safe for concurrent calls.
	Write(ctx context.Context, msg *jsonrpc.Message) error
	// Close closes the transport. Must be idempotent.
	Close() error
	// Done returns a channel that is closed when the transport is shut down.
	Done() <-chan struct{}
}

// StdioTransport implements Transport over stdin/stdout.
// This is the standard LSP transport.
//
// Uses a single reader goroutine (F15 compliance) with a buffered channel.
//
// Backpressure policy (F6): msgCh buffers 32 inbound messages — sized for a
// typical LSP client's concurrent-request window. If the consumer falls
// behind and the buffer fills, readLoop drops the arriving message, reports
// io.ErrShortBuffer through Read/errCh, and exits: the transport actively
// disconnects instead of continuing to run with silent holes in the stream.
// Rationale: LSP requires exactly one terminal response per request, so
// running on with half-delivered traffic would strand requests forever;
// a clean disconnect lets the client retry the whole session.
type StdioTransport struct {
	stdin  io.Reader
	stdout io.Writer
	codec  *jsonrpc.Codec

	msgCh     chan *jsonrpc.Message
	errCh     chan error
	closed    atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
	writeMu   sync.Mutex
}

// NewStdioTransport creates a new stdio transport.
func NewStdioTransport(stdin io.Reader, stdout io.Writer) *StdioTransport {
	t := &StdioTransport{
		// bufio absorbs the codec's header scanning so it does not become one
		// syscall per byte on real streams.
		stdin:  bufio.NewReaderSize(stdin, 64*1024),
		stdout: stdout,
		codec:  jsonrpc.NewCodec(),
		msgCh:  make(chan *jsonrpc.Message, 32),
		errCh:  make(chan error, 1),
		done:   make(chan struct{}),
	}
	go t.readLoop()
	return t
}

// NewStdioTransportFromOS creates a stdio transport using os.Stdin/Stdout.
func NewStdioTransportFromOS() *StdioTransport {
	return NewStdioTransport(os.Stdin, os.Stdout)
}

// readLoop is the single goroutine responsible for reading messages (F15).
func (t *StdioTransport) readLoop() {
	defer close(t.done)
	for {
		msg, err := t.codec.ReadMessage(t.stdin)
		if err != nil {
			select {
			case t.errCh <- err:
			default:
			}
			return
		}
		select {
		case t.msgCh <- msg:
		default:
			// Buffer full: drop + terminate — Backpressure policy (F6) above.
			select {
			case t.errCh <- io.ErrShortBuffer:
			default:
			}
			return
		}
	}
}

func (t *StdioTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	if t.closed.Load() {
		return nil, io.ErrClosedPipe
	}

	select {
	case msg := <-t.msgCh:
		return msg, nil
	case err := <-t.errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		// readLoop sends to errCh before closing done; drain it so a
		// truncated stream surfaces its error instead of a fake clean EOF
		// when both channels become ready in the same select race.
		select {
		case err := <-t.errCh:
			return nil, err
		default:
			return nil, io.EOF
		}
	}
}

func (t *StdioTransport) Write(ctx context.Context, msg *jsonrpc.Message) error {
	if t.closed.Load() {
		return io.ErrClosedPipe
	}

	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	// Write is non-cancelable once started to avoid partial messages.
	return t.codec.WriteMessage(t.stdout, msg)
}

func (t *StdioTransport) Close() error {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		// readLoop will close(t.done) when it exits, which unblocks Read().
	})
	return nil
}

func (t *StdioTransport) Done() <-chan struct{} {
	return t.done
}

// TCPTransport implements Transport over a TCP connection.
//
// Backpressure policy (F6): same as StdioTransport — when msgCh (32 entries,
// a typical LSP concurrent-request window) fills up, readLoop drops the
// arriving message, reports io.ErrShortBuffer through Read/errCh, and
// terminates: active disconnect rather than silently dropping messages while
// the transport keeps running. LSP requires exactly one terminal response per
// request; a dropped request can never answer, so disconnecting lets the
// client retry the session as a whole.
type TCPTransport struct {
	conn      net.Conn
	readBuf   *bufio.Reader // wraps conn; owned by readLoop
	codec     *jsonrpc.Codec
	closed    atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
	writeMu   sync.Mutex
	msgCh     chan *jsonrpc.Message
	errCh     chan error
}

// NewTCPTransport creates a transport over an existing net.Conn.
func NewTCPTransport(conn net.Conn) *TCPTransport {
	t := &TCPTransport{
		conn: conn,
		// Same rationale as stdio: absorb the codec's header scanning so it
		// does not become one syscall per byte on a real socket.
		codec: jsonrpc.NewCodec(),
		done:  make(chan struct{}),
		msgCh: make(chan *jsonrpc.Message, 32),
		errCh: make(chan error, 1),
	}
	t.readBuf = bufio.NewReaderSize(conn, 64*1024)
	go t.readLoop()
	return t
}

func (t *TCPTransport) readLoop() {
	defer close(t.done)
	for {
		msg, err := t.codec.ReadMessage(t.readBuf)
		if err != nil {
			select {
			case t.errCh <- err:
			default:
			}
			return
		}
		select {
		case t.msgCh <- msg:
		default:
			// Buffer full: drop + terminate — Backpressure policy (F6) above.
			select {
			case t.errCh <- io.ErrShortBuffer:
			default:
			}
			return
		}
	}
}

func (t *TCPTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	if t.closed.Load() {
		return nil, io.ErrClosedPipe
	}

	select {
	case msg := <-t.msgCh:
		return msg, nil
	case err := <-t.errCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.done:
		// readLoop sends to errCh before closing done; drain it so a
		// truncated stream surfaces its error instead of a fake clean EOF
		// when both channels become ready in the same select race.
		select {
		case err := <-t.errCh:
			return nil, err
		default:
			return nil, io.EOF
		}
	}
}

func (t *TCPTransport) Write(ctx context.Context, msg *jsonrpc.Message) error {
	if t.closed.Load() {
		return io.ErrClosedPipe
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.codec.WriteMessage(t.conn, msg)
}

func (t *TCPTransport) Close() error {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		_ = t.conn.Close()
	})
	return nil
}

func (t *TCPTransport) Done() <-chan struct{} {
	return t.done
}
