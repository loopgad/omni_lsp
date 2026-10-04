// Package upstream provides a small test-only LSP stdio client for direct
// semantic comparisons against pinned language servers.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

const stderrCap = 64 * 1024

// Stderr returns the bounded diagnostic tail, including provider startup errors.
func (s *Session) Stderr() string {
	return s.stderr.String()
}

const notificationHistoryLimit = 256

// Notification is a bounded, immutable snapshot of a server-to-client LSP
// notification received by Session.
type Notification struct {
	Sequence uint64
	Method   string
	Params   json.RawMessage
}

type Session struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	codec     *jsonrpc.Codec
	writeMu   sync.Mutex
	seq       atomic.Int64
	responses chan *jsonrpc.Message
	readErr   chan error
	stderr    cappedBuffer
	notifMu   sync.Mutex
	notifSeq  uint64
	notifs    []Notification
}

// NotificationCursor returns the sequence number of the latest captured
// notification. Use it before sending a request or document notification to
// wait only for later events.
func (s *Session) NotificationCursor() uint64 {
	s.notifMu.Lock()
	defer s.notifMu.Unlock()
	return s.notifSeq
}

// NotificationsSince returns captured notifications newer than cursor. The
// bool reports whether the bounded history has already dropped any events
// after cursor.
func (s *Session) NotificationsSince(cursor uint64) ([]Notification, bool) {
	notifications, _, overflow := s.NotificationsSinceAndCursor(cursor)
	return notifications, overflow
}

// NotificationsSinceAndCursor returns one atomic snapshot of notifications
// newer than cursor and the latest sequence covered by that snapshot. A
// notification arriving after the snapshot has a greater sequence and remains
// visible to the next call.
func (s *Session) NotificationsSinceAndCursor(cursor uint64) ([]Notification, uint64, bool) {
	s.notifMu.Lock()
	defer s.notifMu.Unlock()
	if len(s.notifs) > 0 && cursor < s.notifs[0].Sequence-1 {
		return nil, s.notifSeq, true
	}
	result := make([]Notification, 0, len(s.notifs))
	for _, notification := range s.notifs {
		if notification.Sequence <= cursor {
			continue
		}
		copy := notification
		copy.Params = append(json.RawMessage(nil), notification.Params...)
		result = append(result, copy)
	}
	return result, s.notifSeq, false
}

func (s *Session) captureNotification(message *jsonrpc.Message) {
	if message == nil || !message.IsNotification() {
		return
	}
	s.notifMu.Lock()
	defer s.notifMu.Unlock()
	s.notifSeq++
	notification := Notification{
		Sequence: s.notifSeq,
		Method:   message.Method,
		Params:   append(json.RawMessage(nil), message.Params...),
	}
	if len(s.notifs) == notificationHistoryLimit {
		copy(s.notifs, s.notifs[1:])
		s.notifs = s.notifs[:notificationHistoryLimit-1]
	}
	s.notifs = append(s.notifs, notification)
}

// Start starts a pinned upstream server in the given workspace.
func Start(binary string, args []string, workspace string, extraEnv []string) (*Session, error) {
	cmd := exec.Command(binary, args...)
	cmd.Dir = workspace
	cmd.Env = mergeEnv(os.Environ(), extraEnv)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("upstream stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("upstream stdout: %w", err)
	}
	s := &Session{cmd: cmd, stdin: stdin, codec: jsonrpc.NewCodec(), responses: make(chan *jsonrpc.Message, 64), readErr: make(chan error, 1)}
	cmd.Stderr = &s.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start upstream %s: %w", binary, err)
	}
	go s.readLoop(stdout)
	return s, nil
}

func (s *Session) Initialize(ctx context.Context, workspace string) error {
	return s.InitializeWithCapabilities(ctx, workspace, map[string]any{
		"window":  map[string]any{"workDoneProgress": true},
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
	})
}

// InitializeWithCapabilities performs initialize/initialized with explicit
// client capabilities. A nil capability map is encoded as an empty object.
// This lets parity tests match a pinned server's production child client.
func (s *Session) InitializeWithCapabilities(ctx context.Context, workspace string, capabilities map[string]any) error {
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	rootURI := uri.FromPath(workspace).String()
	params := map[string]any{
		"processId": os.Getpid(), "rootUri": rootURI,
		"workspaceFolders": []map[string]string{{"uri": rootURI, "name": "acceptance"}},
		"capabilities":     capabilities,
	}
	if _, err := s.RequestContext(ctx, "initialize", params); err != nil {
		return fmt.Errorf("upstream initialize: %w (stderr: %s)", err, s.stderr.String())
	}
	return s.Notify("initialized", map[string]any{})
}

func (s *Session) Notify(method string, params any) error {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return s.write(&jsonrpc.Message{JSONRPC: jsonrpc.Version, Method: method, Params: paramsJSON})
}

func (s *Session) RequestContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	id := s.seq.Add(1)
	requestID := jsonrpc.RequestID{Num: id}
	if err := s.write(jsonrpc.NewRequest(requestID, method, paramsJSON)); err != nil {
		return nil, err
	}
	for {
		select {
		case response := <-s.responses:
			if response.ID == nil || !response.ID.Equals(requestID) {
				continue
			}
			if response.Error != nil {
				return nil, response.Error
			}
			return append(json.RawMessage(nil), response.Result...), nil
		case err := <-s.readErr:
			return nil, fmt.Errorf("read upstream response: %w (stderr: %s)", err, s.stderr.String())
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (s *Session) Close() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = s.RequestContext(ctx, "shutdown", map[string]any{})
	cancel()
	_ = s.Notify("exit", map[string]any{})
	_ = s.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		_ = s.cmd.Process.Kill()
		return <-done
	}
}

func (s *Session) write(message *jsonrpc.Message) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.codec.WriteMessage(s.stdin, message)
}

func (s *Session) readLoop(stdout io.Reader) {
	for {
		message, err := s.codec.ReadMessage(stdout)
		if err != nil {
			s.readErr <- err
			return
		}
		if message.IsRequest() {
			result := json.RawMessage("null")
			switch message.Method {
			case "workspace/configuration":
				result = json.RawMessage("[]")
			case "workspace/workspaceFolders":
				result = json.RawMessage("[]")
			}
			if err := s.write(jsonrpc.NewResponse(*message.ID, result)); err != nil {
				s.readErr <- err
				return
			}
			continue
		}
		if message.IsResponse() {
			select {
			case s.responses <- message:
			default:
				// The only expected overflow is an abandoned timed-out request.
			}
			continue
		}
		if message.IsNotification() {
			s.captureNotification(message)
		}
	}
}

func mergeEnv(base, overrides []string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	keys := make([]string, 0, len(base)+len(overrides))
	set := func(item string) {
		for i := 0; i < len(item); i++ {
			if item[i] == '=' {
				key := item[:i]
				if _, ok := values[key]; !ok {
					keys = append(keys, key)
				}
				values[key] = item[i+1:]
				return
			}
		}
	}
	for _, item := range base {
		set(item)
	}
	for _, item := range overrides {
		set(item)
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+values[key])
	}
	return out
}

type cappedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLength := len(p)
	if len(p) > stderrCap {
		p = p[len(p)-stderrCap:]
	}
	b.data = append(b.data, p...)
	if len(b.data) > stderrCap {
		b.data = append([]byte(nil), b.data[len(b.data)-stderrCap:]...)
	}
	return originalLength, nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
