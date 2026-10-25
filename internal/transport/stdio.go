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
//
// Batch frames (JSON-RPC 2.0 §6, per the 2026-10-25 ruling in
// docs/audit-longterm-spec.md 终局裁决第 1 条) are expanded here: a valid
// element is delivered as usual, an element that fails to decode is answered
// with its single error response on the outbound stream, and the frame never
// terminates the session. Only framing-level faults — a frame that cannot be
// parsed at all, a broken header, an oversize body — stay per-stream fatal.
func (t *StdioTransport) readLoop() {
	defer close(t.done)
	for {
		fr, err := t.codec.ReadFrame(t.stdin)
		if err != nil {
			select {
			case t.errCh <- err:
			default:
			}
			return
		}
		if fr.Msg != nil {
			if !t.deliver(fr.Msg) {
				return
			}
			continue
		}
		for _, el := range fr.Elements {
			if el.Reply != nil {
				// §6 element isolation: answer this element only.
				if err := t.writeMessage(el.Reply); err != nil {
					select {
					case t.errCh <- err:
					default:
					}
					return
				}
				continue
			}
			if !t.deliver(el.Msg) {
				return
			}
		}
	}
}

// deliver pushes one message into msgCh, applying the F6 backpressure policy
// (drop + terminal error) when the buffer is full. Reports false when the
// read loop must exit.
func (t *StdioTransport) deliver(msg *jsonrpc.Message) bool {
	select {
	case t.msgCh <- msg:
		return true
	default:
		// Buffer full: drop + terminate — Backpressure policy (F6) above.
		select {
		case t.errCh <- io.ErrShortBuffer:
		default:
		}
		return false
	}
}

// writeMessage writes one message to the outbound stream under writeMu, so a
// batch element's error response cannot interleave with a concurrent Write
// and corrupt the framing.
func (t *StdioTransport) writeMessage(msg *jsonrpc.Message) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.codec.WriteMessage(t.stdout, msg)
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

	// Write is non-cancelable once started to avoid partial messages.
	return t.writeMessage(msg)
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
		fr, err := t.codec.ReadFrame(t.readBuf)
		if err != nil {
			select {
			case t.errCh <- err:
			default:
			}
			return
		}
		if fr.Msg != nil {
			if !t.deliver(fr.Msg) {
				return
			}
			continue
		}
		// Same §6 element isolation as stdio: one semantics across transports.
		for _, el := range fr.Elements {
			if el.Reply != nil {
				if err := t.writeMessage(el.Reply); err != nil {
					select {
					case t.errCh <- err:
					default:
					}
					return
				}
				continue
			}
			if !t.deliver(el.Msg) {
				return
			}
		}
	}
}

// deliver pushes one message into msgCh, applying the F6 backpressure policy
// (drop + terminal error) when the buffer is full. Reports false when the
// read loop must exit.
func (t *TCPTransport) deliver(msg *jsonrpc.Message) bool {
	select {
	case t.msgCh <- msg:
		return true
	default:
		// Buffer full: drop + terminate — Backpressure policy (F6) above.
		select {
		case t.errCh <- io.ErrShortBuffer:
		default:
		}
		return false
	}
}

// writeMessage writes one message to the connection under writeMu, so a
// batch element's error response cannot interleave with a concurrent Write.
func (t *TCPTransport) writeMessage(msg *jsonrpc.Message) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	return t.codec.WriteMessage(t.conn, msg)
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
	return t.writeMessage(msg)
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
