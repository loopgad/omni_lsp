package server

// Tests for goal.md §C2 (LSP lifecycle state machine) and §C7
// ($/cancelRequest mapping) enforced at the admission boundary.

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/scheduler"
)

// fakeTransport captures written responses without a real stream.
type fakeTransport struct {
	mu     sync.Mutex
	writes []*jsonrpc.Message
	done   chan struct{}
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{done: make(chan struct{})}
}

func (f *fakeTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeTransport) Write(_ context.Context, msg *jsonrpc.Message) error {
	f.mu.Lock()
	f.writes = append(f.writes, msg)
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) Close() error {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}

func (f *fakeTransport) Done() <-chan struct{} { return f.done }

func (f *fakeTransport) responses() []*jsonrpc.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*jsonrpc.Message(nil), f.writes...)
}

func attachTransport(s *Server) *fakeTransport {
	ft := newFakeTransport()
	s.mu.Lock()
	s.transport = ft
	s.mu.Unlock()
	return ft
}

// startScheduler launches the server's scheduler for direct scheduleMessage
// use in tests; callers get a cleanup func.
func startScheduler(s *Server) func() {
	ctx, cancel := context.WithCancel(context.Background())
	s.scheduler.Start(ctx)
	return cancel
}

// TestC2_RejectsWorkBeforeInitialize verifies only initialization-safe
// messages are accepted before initialize completes.
func TestC2_RejectsWorkBeforeInitialize(t *testing.T) {
	s := New(DefaultConfig())
	ft := attachTransport(s)

	params := `{"textDocument":{"uri":"file:///x.go","languageId":"go","version":1,"text":"package main\n"}}`
	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("textDocument/didOpen", json.RawMessage(params)))

	if s.vfs.Get("file:///x.go") != nil {
		t.Error("didOpen must not mutate VFS before initialize")
	}
	resps := ft.responses()
	if len(resps) != 0 {
		t.Logf("notification rejection is silent: %d responses", len(resps))
	}

	// Requests receive an explicit InvalidRequest response.
	hoverParams := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`
	s.scheduleMessage(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 1}, "textDocument/hover", json.RawMessage(hoverParams)))
	resps = ft.responses()
	if len(resps) != 1 || respCode(resps[0]) != jsonrpc.InvalidRequest {
		t.Errorf("want single InvalidRequest response, got %+v", resps)
	}
}

// TestC2_FullLifecycleSequence walks New->Initializing->Running->ShuttingDown.
func TestC2_FullLifecycleSequence(t *testing.T) {
	s := New(DefaultConfig())
	attachTransport(s)
	stop := startScheduler(s)
	defer stop()

	initMsg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize",
		json.RawMessage(`{"processId":1,"rootUri":"file:///ws"}`))
	s.scheduleMessage(context.Background(), initMsg)
	waitForState(t, s, StateInitializing)

	// Semantic work during Initializing is still gated off.
	params := `{"textDocument":{"uri":"file:///x.go","languageId":"go","version":1,"text":"package main\n"}}`
	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("textDocument/didOpen", json.RawMessage(params)))
	if s.vfs.Get("file:///x.go") != nil {
		t.Error("didOpen must not apply during Initializing")
	}

	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("initialized", nil))
	waitForState(t, s, StateRunning)

	// Now semantic work is admitted: didOpen reaches VFS.
	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("textDocument/didOpen", json.RawMessage(params)))
	waitFor(t, func() bool { return s.vfs.Get("file:///x.go") != nil })

	// shutdown -> ShuttingDown; further work gated.
	s.scheduleMessage(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 2}, "shutdown", nil))
	waitForState(t, s, StateShuttingDown)

	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("textDocument/didChange",
		json.RawMessage(`{"textDocument":{"uri":"file:///x.go","version":2},"contentChanges":[{"text":"y"}]}`)))
	time.Sleep(50 * time.Millisecond)
	if got := string(s.vfs.Content("file:///x.go")); got != "package main\n" {
		t.Errorf("didChange applied after shutdown: content = %q", got)
	}
}

// TestC2_ExitTerminatesConnection verifies exit closes the transport from any state.
func TestC2_ExitTerminatesConnection(t *testing.T) {
	s := New(DefaultConfig())
	ft := attachTransport(s)
	stop := startScheduler(s)
	defer stop()
	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("exit", nil))
	select {
	case <-ft.Done():
	case <-time.After(time.Second):
		t.Fatal("exit did not close transport")
	}
	if s.State() != StateExited {
		t.Errorf("State = %s, want exited", s.State())
	}
}

// TestC7_CancelUnknownIDIgnored verifies robustness of the cancel handler.
func TestC7_CancelUnknownIDIgnored(t *testing.T) {
	s := New(DefaultConfig())
	msg := jsonrpc.NewNotification("$/cancelRequest", json.RawMessage(`{"id":424242}`))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	if resp != nil {
		t.Error("cancel notification must not produce a response")
	}
}

// TestC7_CancelRequestMapsToScheduledRequest verifies the in-flight lookup.
func TestC7_CancelRequestMapsToScheduledRequest(t *testing.T) {
	s := New(DefaultConfig())
	req := &scheduler.Request{}
	s.mu.Lock()
	s.inflight[requestIDKey(jsonrpc.RequestID{Num: 7})] = req
	s.mu.Unlock()

	msg := jsonrpc.NewNotification("$/cancelRequest", json.RawMessage(`{"id":7}`))
	s.dispatcher.Dispatch(context.Background(), msg)
	if !req.Cancelled() {
		t.Error("$/cancelRequest did not reach the scheduled request")
	}
}

// waitForState polls until the server reaches the wanted state.
func waitForState(t *testing.T, s *Server, want State) {
	t.Helper()
	waitFor(t, func() bool { return s.State() == want })
}

// waitFor polls cond up to 2 seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func respCode(m *jsonrpc.Message) int {
	if m == nil || m.Error == nil {
		return 0
	}
	return m.Error.Code
}
