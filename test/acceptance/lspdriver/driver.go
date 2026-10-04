// Package lspdriver runs a real OmniLSP process over its stdio LSP transport.
// It is shared by process-level language and performance acceptance tests.
package lspdriver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

const (
	requestTimeout = 30 * time.Second
	eventLimit     = 4096
	stderrLimit    = 64 * 1024
)

// Session owns one child server process and its stdio protocol streams.
type Session struct {
	cmd       *exec.Cmd
	workspace string
	stdin     io.WriteCloser
	codec     *jsonrpc.Codec
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[int64]chan *jsonrpc.Message
	events    []*jsonrpc.Message
	eventBase uint64
	eventSeq  uint64
	stderr    cappedBuffer
	readErr   chan error
	eventWake chan struct{}
	done      chan struct{}
	exitErr   error
	seq       atomic.Int64
	closed    atomic.Bool
}

// Start starts binary serve in workspace. Extra environment entries override
// matching variables in the inherited environment.
func Start(t testing.TB, binary, workspace string, extraEnv []string) *Session {
	return StartWithProcessHook(t, binary, workspace, extraEnv, nil)
}

// StartWithProcessHook invokes hook with the new server PID immediately after
// process creation and before any LSP bytes are read or sent. It lets
// platform-specific tests attach the candidate to a Job Object or equivalent
// resource monitor before initialization can launch backend children.
func StartWithProcessHook(t testing.TB, binary, workspace string, extraEnv []string, hook func(pid int) error) *Session {
	t.Helper()
	absBin, err := filepath.Abs(binary)
	if err != nil {
		t.Fatalf("absolute server binary: %v", err)
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		t.Fatalf("absolute workspace: %v", err)
	}
	cmd := exec.Command(absBin, "serve", "--transport", "stdio", "--workspace", absWorkspace)
	cmd.Env = mergeEnv(os.Environ(), extraEnv)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("server stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("server stdout: %v", err)
	}
	s := &Session{
		cmd: cmd, workspace: absWorkspace, stdin: stdin, codec: jsonrpc.NewCodec(),
		pending: make(map[int64]chan *jsonrpc.Message), readErr: make(chan error, 1),
		eventWake: make(chan struct{}, 1), done: make(chan struct{}),
	}
	cmd.Stderr = &s.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start OmniLSP: %v", err)
	}
	go s.waitLoop()
	t.Cleanup(func() { s.killIfRunning() })
	if hook != nil {
		if err := hook(cmd.Process.Pid); err != nil {
			s.killIfRunning()
			t.Fatalf("attach server process hook: %v", err)
		}
	}
	go s.readLoop(stdout)
	return s
}

// Initialize performs the LSP lifecycle handshake with UTF-16 positions.
func (s *Session) Initialize(t testing.TB) {
	t.Helper()
	rootURI := uri.FromPath(s.workspace).String()
	result := s.Request(t, "initialize", map[string]any{
		"processId": os.Getpid(), "rootUri": rootURI,
		"workspaceFolders": []map[string]string{{"uri": rootURI, "name": filepath.Base(s.workspace)}},
		"capabilities": map[string]any{
			"general": map[string]any{"positionEncodings": []string{"utf-16"}},
			"workspace": map[string]any{
				"applyEdit": true, "workspaceFolders": true, "configuration": true,
				"didChangeConfiguration": map[string]bool{"dynamicRegistration": true},
			},
			"textDocument": map[string]any{
				"completion":         map[string]any{"completionItem": map[string]any{"snippetSupport": false}},
				"publishDiagnostics": map[string]any{"relatedInformation": true, "versionSupport": true},
				"rename":             map[string]any{"prepareSupport": true},
			},
		},
	})
	var initialized struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &initialized); err != nil || initialized.Capabilities == nil {
		t.Fatalf("invalid initialize result: %s (%v)", result, err)
	}
	s.Notify(t, "initialized", map[string]any{})
}

// Notify sends a JSON-RPC notification.
func (s *Session) Notify(t testing.TB, method string, params any) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal %s params: %v", method, err)
	}
	if err := s.write(&jsonrpc.Message{JSONRPC: jsonrpc.Version, Method: method, Params: raw}); err != nil {
		t.Fatalf("send %s: %v", method, err)
	}
}

// Request sends a request and waits up to the default acceptance timeout.
// JSON-RPC errors fail the test; use RequestContext when asserting errors.
func (s *Session) Request(t testing.TB, method string, params any) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	result, err := s.RequestContext(ctx, method, params)
	if err != nil {
		t.Fatalf("%s failed: %v; stderr: %s", method, err, s.stderr.String())
	}
	return result
}

// RequestContext sends a request and returns either its JSON result or the
// JSON-RPC error. Context cancellation does not kill the child process.
func (s *Session) RequestContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	_, result, err := s.RequestIDContext(ctx, method, params)
	return result, err
}

// RequestIDContext is like RequestContext and also returns the outgoing JSON-RPC
// id, allowing cancellation tests to correlate the terminal response.
func (s *Session) RequestIDContext(ctx context.Context, method string, params any) (int64, json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal %s params: %w", method, err)
	}
	id := s.seq.Add(1)
	ch := make(chan *jsonrpc.Message, 1)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()
	if err := s.write(jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, method, raw)); err != nil {
		return id, nil, fmt.Errorf("send %s: %w", method, err)
	}
	select {
	case response := <-ch:
		if response.Error != nil {
			return id, response.Result, response.Error
		}
		return id, response.Result, nil
	case <-ctx.Done():
		cancelParams, _ := json.Marshal(map[string]any{"id": id})
		if err := s.write(&jsonrpc.Message{JSONRPC: jsonrpc.Version, Method: "$/cancelRequest", Params: cancelParams}); err != nil {
			return id, nil, fmt.Errorf("send cancellation for request %d: %w", id, err)
		}
		return id, nil, ctx.Err()
	case <-s.done:
		return id, nil, fmt.Errorf("server exited: %v; stderr: %s", s.exitErr, s.stderr.String())
	case err := <-s.readErr:
		return id, nil, fmt.Errorf("read server response: %w; stderr: %s", err, s.stderr.String())
	}
}

// Events returns a snapshot of server notifications and server-initiated
// requests received so far, including diagnostics and progress events.
func (s *Session) Events() []*jsonrpc.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*jsonrpc.Message(nil), s.events...)
}

// EventCursor returns the sequence number that will be assigned to the next
// recorded server event. Pair it with EventsSince to observe notifications
// without missing events when the bounded event history wraps.
func (s *Session) EventCursor() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.eventSeq
}

// EventsSince returns retained events at or after cursor, the next cursor, and
// whether the requested history has already been evicted from the bounded
// event buffer. The snapshot and next cursor are read atomically.
func (s *Session) EventsSince(cursor uint64) ([]*jsonrpc.Message, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cursor < s.eventBase || cursor > s.eventSeq {
		return nil, s.eventSeq, true
	}
	start := int(cursor - s.eventBase)
	return append([]*jsonrpc.Message(nil), s.events[start:]...), s.eventSeq, false
}

// PID returns the operating-system process id of the managed server.
func (s *Session) PID() int {
	if s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// WaitForResponse waits for the terminal server response to an outgoing
// request. It is useful for proving cancellation completion after the caller's
// request context has already expired.
func (s *Session) WaitForResponse(ctx context.Context, id int64) (*jsonrpc.Message, error) {
	for {
		s.mu.Lock()
		for i := len(s.events) - 1; i >= 0; i-- {
			msg := s.events[i]
			if msg.IsResponse() && msg.ID != nil && !msg.ID.IsStr && msg.ID.Num == id {
				s.mu.Unlock()
				return msg, nil
			}
		}
		s.mu.Unlock()
		select {
		case <-s.eventWake:
		case <-s.done:
			return nil, fmt.Errorf("server exited before response %d: %v", id, s.exitErr)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Close performs a graceful LSP shutdown, closes the pipes, and waits for the
// process to exit. Cleanup kills the process if graceful shutdown fails.
func (s *Session) Close(t testing.TB) {
	t.Helper()
	if s.closed.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := s.RequestContext(ctx, "shutdown", map[string]any{})
	cancel()
	if err != nil {
		t.Errorf("graceful shutdown request: %v; stderr: %s", err, s.stderr.String())
		s.killIfRunning()
		return
	}
	if err := s.write(&jsonrpc.Message{JSONRPC: jsonrpc.Version, Method: "exit", Params: json.RawMessage("{}")}); err != nil {
		t.Errorf("send exit: %v; stderr: %s", err, s.stderr.String())
		s.killIfRunning()
		return
	}
	_ = s.stdin.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-s.done:
		if s.exitErr != nil && !errors.Is(s.exitErr, os.ErrProcessDone) {
			t.Errorf("server exit: %v; stderr: %s", s.exitErr, s.stderr.String())
		}
		s.closed.Store(true)
	case <-ctx.Done():
		t.Errorf("server did not exit after shutdown; stderr: %s", s.stderr.String())
		s.killIfRunning()
	}
}

func (s *Session) write(msg *jsonrpc.Message) error {
	if s.closed.Load() {
		return os.ErrClosed
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.codec.WriteMessage(s.stdin, msg)
}

func (s *Session) readLoop(r io.Reader) {
	for {
		msg, err := s.codec.ReadMessage(r)
		if err != nil {
			select {
			case s.readErr <- err:
			default:
			}
			return
		}
		if msg.IsResponse() {
			s.mu.Lock()
			s.appendEventLocked(msg)
			if msg.ID != nil && !msg.ID.IsStr {
				if ch := s.pending[msg.ID.Num]; ch != nil {
					select {
					case ch <- msg:
					default:
					}
				}
			}
			s.mu.Unlock()
			continue
		}
		s.mu.Lock()
		s.appendEventLocked(msg)
		s.mu.Unlock()
		if msg.IsRequest() {
			if err := s.respondToServerRequest(msg); err != nil {
				select {
				case s.readErr <- err:
				default:
				}
				return
			}
		}
	}
}

func (s *Session) appendEventLocked(msg *jsonrpc.Message) {
	if len(s.events) == eventLimit {
		copy(s.events, s.events[1:])
		s.events = s.events[:eventLimit-1]
		s.eventBase++
	}
	s.events = append(s.events, msg)
	s.eventSeq++
	select {
	case s.eventWake <- struct{}{}:
	default:
	}
}

func (s *Session) respondToServerRequest(msg *jsonrpc.Message) error {
	var result json.RawMessage = json.RawMessage("null")
	switch msg.Method {
	case "workspace/configuration":
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return fmt.Errorf("decode workspace/configuration: %w", err)
		}
		values := make([]json.RawMessage, len(params.Items))
		for i := range values {
			values[i] = json.RawMessage("null")
		}
		var err error
		result, err = json.Marshal(values)
		if err != nil {
			return err
		}
	case "workspace/workspaceFolders":
		folders, err := json.Marshal([]map[string]string{{
			"uri": uri.FromPath(s.workspace).String(), "name": filepath.Base(s.workspace),
		}})
		if err != nil {
			return err
		}
		result = folders
	}
	return s.write(&jsonrpc.Message{JSONRPC: jsonrpc.Version, ID: msg.ID, Result: result})
}

func (s *Session) killIfRunning() {
	if s.closed.Load() {
		return
	}
	select {
	case <-s.done:
	default:
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		<-s.done
	}
	s.closed.Store(true)
}

func (s *Session) waitLoop() {
	s.exitErr = s.cmd.Wait()
	close(s.done)
}

func mergeEnv(base, overrides []string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	for _, entry := range append(append([]string(nil), base...), overrides...) {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(key)] = key + "=" + value
		}
	}
	result := make([]string, 0, len(values))
	for _, entry := range values {
		result = append(result, entry)
	}
	return result
}

type cappedBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(p) >= stderrLimit {
		b.b = append(b.b[:0], p[len(p)-stderrLimit:]...)
	} else {
		if excess := len(b.b) + len(p) - stderrLimit; excess > 0 {
			copy(b.b, b.b[excess:])
			b.b = b.b[:len(b.b)-excess]
		}
		b.b = append(b.b, p...)
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.b))
}
