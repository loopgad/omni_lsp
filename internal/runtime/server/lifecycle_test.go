package server

// Tests for goal.md §C2 (LSP lifecycle state machine) and §C7
// ($/cancelRequest mapping) enforced at the admission boundary.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/scheduler"
)

type cancellationResponseBackend struct {
	mockBackend
	entered chan struct{}
}

func (b *cancellationResponseBackend) Hover(ctx context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	close(b.entered)
	<-ctx.Done()
	return identity.SemanticResult[*languages.HoverResult]{}, ctx.Err()
}

// fakeTransport captures written responses without a real stream.
type fakeTransport struct {
	mu     sync.Mutex
	writes []*jsonrpc.Message
	done   chan struct{}
}

type blockedResponseWriteTransport struct {
	*fakeTransport
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
}

func newBlockedResponseWriteTransport() *blockedResponseWriteTransport {
	return &blockedResponseWriteTransport{
		fakeTransport: newFakeTransport(),
		entered:       make(chan struct{}),
		release:       make(chan struct{}),
	}
}

func (t *blockedResponseWriteTransport) Write(ctx context.Context, msg *jsonrpc.Message) error {
	t.enteredOnce.Do(func() { close(t.entered) })
	select {
	case <-t.release:
		return t.fakeTransport.Write(ctx, msg)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *blockedResponseWriteTransport) releaseWrite() {
	t.releaseOnce.Do(func() { close(t.release) })
}

type panicResponseWriteTransport struct{ *fakeTransport }

func (t *panicResponseWriteTransport) Write(context.Context, *jsonrpc.Message) error {
	panic("synthetic response write panic")
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

type lifecycleSequenceTransport struct {
	messages []*jsonrpc.Message
	next     int
	mu       sync.Mutex
	writes   []*jsonrpc.Message
	done     chan struct{}
	closeOne sync.Once
}

func newLifecycleSequenceTransport(messages ...*jsonrpc.Message) *lifecycleSequenceTransport {
	return &lifecycleSequenceTransport{messages: messages, done: make(chan struct{})}
}

func (t *lifecycleSequenceTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	if t.next < len(t.messages) {
		message := t.messages[t.next]
		t.next++
		return message, nil
	}
	select {
	case <-t.done:
		return nil, io.ErrClosedPipe
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *lifecycleSequenceTransport) Write(_ context.Context, message *jsonrpc.Message) error {
	t.mu.Lock()
	t.writes = append(t.writes, message)
	t.mu.Unlock()
	return nil
}

func (t *lifecycleSequenceTransport) Close() error {
	t.closeOne.Do(func() { close(t.done) })
	return nil
}

func (t *lifecycleSequenceTransport) Done() <-chan struct{} { return t.done }

func (t *lifecycleSequenceTransport) writesSnapshot() []*jsonrpc.Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*jsonrpc.Message(nil), t.writes...)
}

type shutdownLifecycleBackend struct {
	mockBackend
	closeMu      sync.Mutex
	closeCalls   int
	closeStarted chan struct{}
	releaseClose <-chan struct{}
	closeErr     error
}

func (b *shutdownLifecycleBackend) Close() error {
	b.closeMu.Lock()
	b.closeCalls++
	b.closeMu.Unlock()
	if b.closeStarted != nil {
		select {
		case b.closeStarted <- struct{}{}:
		default:
		}
	}
	if b.releaseClose != nil {
		<-b.releaseClose
	}
	return b.closeErr
}

func (b *shutdownLifecycleBackend) closeCount() int {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	return b.closeCalls
}

func lifecycleSequence() []*jsonrpc.Message {
	return []*jsonrpc.Message{
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", json.RawMessage(`{"rootUri":"file:///ws"}`)),
		jsonrpc.NewNotification("initialized", nil),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 2}, "shutdown", nil),
		jsonrpc.NewNotification("exit", nil),
	}
}

func responseForID(messages []*jsonrpc.Message, id int64) *jsonrpc.Message {
	for _, message := range messages {
		if message.ID != nil && !message.ID.IsStr && message.ID.Num == id {
			return message
		}
	}
	return nil
}

func TestRunShutdownClosesUniqueBackendsBeforeResponse(t *testing.T) {
	releaseClose := make(chan struct{})
	backend := &shutdownLifecycleBackend{
		mockBackend:  mockBackend{langID: "go", exts: []string{".go"}},
		closeStarted: make(chan struct{}, 1),
		releaseClose: releaseClose,
	}
	s := New(DefaultConfig())
	s.RegisterBackend("go", backend)
	s.RegisterBackend("golang", backend)
	tr := newLifecycleSequenceTransport(lifecycleSequence()...)
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(context.Background(), tr) }()

	select {
	case <-backend.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not start closing the registered backend")
	}
	if responseForID(tr.writesSnapshot(), 2) != nil {
		t.Fatal("shutdown response was written before backend Close completed")
	}
	if initializeResponse := responseForID(tr.writesSnapshot(), 1); initializeResponse == nil || initializeResponse.Error != nil {
		t.Fatalf("initialize response = %+v, want successful response before shutdown", initializeResponse)
	}

	close(releaseClose)
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not finish after backend Close completed")
	}
	if got := backend.closeCount(); got != 1 {
		t.Fatalf("backend Close calls = %d, want one despite alias registration", got)
	}
	shutdownResponse := responseForID(tr.writesSnapshot(), 2)
	if shutdownResponse == nil || shutdownResponse.Error != nil {
		t.Fatalf("shutdown response = %+v, want success after Close", shutdownResponse)
	}
}

func TestRunShutdownCloseFailureIsReturnedAndBlocksSuccessResponse(t *testing.T) {
	closeErr := errors.New("pyright child close failed")
	backend := &shutdownLifecycleBackend{
		mockBackend: mockBackend{langID: "pyright", exts: []string{".py"}},
		closeErr:    closeErr,
	}
	s := New(DefaultConfig())
	s.RegisterBackend("python", backend)
	s.RegisterBackend("pyright", backend)
	tr := newLifecycleSequenceTransport(lifecycleSequence()...)
	err := s.Run(context.Background(), tr)
	if !errors.Is(err, closeErr) {
		t.Fatalf("Run error = %v, want wrapped backend close error %v", err, closeErr)
	}
	if got := backend.closeCount(); got != 1 {
		t.Fatalf("backend Close calls = %d, want one despite alias registration", got)
	}
	shutdownResponse := responseForID(tr.writesSnapshot(), 2)
	if shutdownResponse == nil || shutdownResponse.Error == nil || shutdownResponse.Error.Code != jsonrpc.InternalError || !strings.Contains(shutdownResponse.Error.Message, closeErr.Error()) {
		t.Fatalf("shutdown response = %+v, want an error reporting backend Close failure", shutdownResponse)
	}
}

func TestRunReturnsBackendShutdownFailure(t *testing.T) {
	s := New(DefaultConfig())
	s.RegisterBackend("go", &mockBackend{langID: "go", closeErr: context.DeadlineExceeded})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.Run(ctx, newFakeTransport())
	if err == nil || !strings.Contains(err.Error(), "close go backend") || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("Run error = %v; want backend shutdown error propagated", err)
	}
}

func TestDrainInflightWaitsForResponseWrite(t *testing.T) {
	s := New(DefaultConfig())
	tr := newBlockedResponseWriteTransport()
	s.mu.Lock()
	s.transport = tr
	s.mu.Unlock()

	const reqID = "blocked-response-write"
	req := &scheduler.Request{Result: make(chan scheduler.Result, 1)}
	req.Result <- scheduler.Result{Value: jsonrpc.NewResponse(jsonrpc.RequestID{Num: 7}, json.RawMessage(`{}`))}
	s.mu.Lock()
	s.inflight[reqID] = req
	s.mu.Unlock()
	resultDone := make(chan struct{})
	go func() {
		s.drainResult(req, reqID)
		close(resultDone)
	}()
	defer tr.releaseWrite()

	select {
	case <-tr.entered:
	case <-time.After(time.Second):
		t.Fatal("drainResult did not reach the blocked response write")
	}
	if err := s.drainInflight(20 * time.Millisecond); err == nil {
		t.Fatal("drainInflight succeeded while the response Write was still blocked")
	}
	s.mu.RLock()
	_, stillInflight := s.inflight[reqID]
	s.mu.RUnlock()
	if !stillInflight {
		t.Fatal("request was unregistered before response Write completed")
	}

	tr.releaseWrite()
	select {
	case <-resultDone:
	case <-time.After(time.Second):
		t.Fatal("drainResult did not finish after response Write was released")
	}
	if err := s.drainInflight(time.Second); err != nil {
		t.Fatalf("drainInflight after response Write completed: %v", err)
	}
}

func TestDrainResultUnregistersInflightAfterWritePanic(t *testing.T) {
	s := New(DefaultConfig())
	s.mu.Lock()
	s.transport = &panicResponseWriteTransport{fakeTransport: newFakeTransport()}
	s.mu.Unlock()

	const reqID = "panic-response-write"
	req := &scheduler.Request{Result: make(chan scheduler.Result, 1)}
	req.Result <- scheduler.Result{Value: jsonrpc.NewResponse(jsonrpc.RequestID{Num: 8}, json.RawMessage(`{}`))}
	s.mu.Lock()
	s.inflight[reqID] = req
	s.mu.Unlock()
	resultDone := make(chan struct{})
	go func() {
		s.drainResult(req, reqID)
		close(resultDone)
	}()
	select {
	case <-resultDone:
	case <-time.After(time.Second):
		t.Fatal("drainResult did not recover from the response Write panic")
	}
	if err := s.drainInflight(time.Second); err != nil {
		t.Fatalf("in-flight request remained after response Write panic: %v", err)
	}
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

func TestC7_CancelledRequestGetsRequestCancelledResponse(t *testing.T) {
	s := New(DefaultConfig())
	ft := attachTransport(s)
	backend := &cancellationResponseBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		entered:     make(chan struct{}),
	}
	s.RegisterBackend("go", backend)
	stop := startScheduler(s)
	defer stop()

	s.scheduleMessage(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", json.RawMessage(`{"rootUri":"file:///ws"}`)))
	waitForState(t, s, StateInitializing)
	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("initialized", nil))
	waitForState(t, s, StateRunning)
	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("textDocument/didOpen", json.RawMessage(`{"textDocument":{"uri":"file:///ws/main.go","languageId":"go","version":1,"text":"package main\nfunc Foo() {}\n"}}`)))
	waitFor(t, func() bool { return s.vfs.Get("file:///ws/main.go") != nil })

	const requestID = "cancel-me"
	s.scheduleMessage(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Str: requestID, IsStr: true}, "textDocument/hover",
		json.RawMessage(`{"textDocument":{"uri":"file:///ws/main.go"},"position":{"line":1,"character":6}}`),
	))
	select {
	case <-backend.entered:
	case <-time.After(time.Second):
		t.Fatal("hover backend did not start")
	}
	s.scheduleMessage(context.Background(), jsonrpc.NewNotification("$/cancelRequest", json.RawMessage(`{"id":"cancel-me"}`)))

	var terminal *jsonrpc.Message
	waitFor(t, func() bool {
		for _, msg := range ft.responses() {
			if msg.ID != nil && msg.ID.IsStr && msg.ID.Str == requestID {
				terminal = msg
				return true
			}
		}
		return false
	})
	if got := respCode(terminal); got != jsonrpc.RequestCancelled {
		t.Fatalf("cancelled request error code = %d, want RequestCancelled (%d): %+v", got, jsonrpc.RequestCancelled, terminal)
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
