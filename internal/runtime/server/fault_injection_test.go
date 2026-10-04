package server

// Fault-injection tests for goal.md §S13 (transport/frame faults),
// §S15/F7 (fairness under load), and §S4 (cancel-race permutations).
//
// Injection points (mapping notes):
//   - S13 half-frame: transport.StdioTransport accepts any io.Reader, so an
//     io.Pipe carrying a truncated Content-Length frame stands in for a real
//     client connection dying mid-write.
//   - S13 backend crash: a fake backend owns an io.Pipe standing in for its
//     child-process channel; "crashing" = closing that pipe mid-request.
//   - Fairness: driven through Server.scheduleMessage so the production
//     classifier (classifyPriority) assigns priorities, not hand-built ones.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/transport"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// TestS13_HalfFrameDisconnect injects a partial JSON-RPC frame (declared
// Content-Length exceeds the bytes actually written) and then kills the input
// stream, per goal.md §S13.
//
// Expected: the server reports the read error and Run returns cleanly (single
// -connection model: Run owns one connection; a broken stream ends that Run),
// no panic, the process stays live, and no goroutines leak.
func TestS13_HalfFrameDisconnect(t *testing.T) {
	base := runtime.NumGoroutine()

	srv := New(DefaultConfig())
	pr, pw := io.Pipe()
	tr := transport.NewStdioTransport(pr, &bytes.Buffer{})
	defer tr.Close()

	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(context.Background(), tr) }()

	// Header promises far more body bytes than we deliver, then the stream
	// dies mid-frame (peer crash / truncated write).
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)*8)
	if _, err := pw.Write(append([]byte(header), body...)); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	pw.CloseWithError(io.ErrUnexpectedEOF)

	select {
	case err := <-runErr:
		if err == nil {
			t.Error("Run returned nil on truncated frame; want reported error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after half-frame disconnect")
	}

	// Process liveness: the dispatcher (independent of the dead transport)
	// must still serve a fresh initialize handshake.
	resp := srv.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 42}, "initialize",
		json.RawMessage(`{"processId":1,"rootUri":"file:///ws"}`)))
	if resp == nil || resp.Error != nil {
		t.Errorf("server unresponsive after disconnect: %+v", resp)
	}

	// Goroutine leak check (±2 tolerance per spec): transport readLoop,
	// scheduler workers and the Run goroutine must all wind down.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base+2 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: before=%d after=%d (tolerance +2)", base, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLSP_GracefulExitClosesRunWithoutTransportFailure(t *testing.T) {
	var inbound, outbound bytes.Buffer
	codec := jsonrpc.NewCodec()
	messages := []*jsonrpc.Message{
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", json.RawMessage(`{"processId":1,"rootUri":"file:///ws"}`)),
		jsonrpc.NewNotification("initialized", json.RawMessage(`{}`)),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 2}, "shutdown", json.RawMessage(`{}`)),
		jsonrpc.NewNotification("exit", json.RawMessage(`{}`)),
	}
	for _, msg := range messages {
		if err := codec.WriteMessage(&inbound, msg); err != nil {
			t.Fatalf("encode lifecycle message %s: %v", msg.Method, err)
		}
	}

	srv := New(DefaultConfig())
	pr, pw := io.Pipe()
	defer pw.Close()
	tr := transport.NewStdioTransport(pr, &outbound)
	writeDone := make(chan error, 1)
	go func() {
		_, err := pw.Write(inbound.Bytes())
		writeDone <- err
	}()
	if err := srv.Run(context.Background(), tr); err != nil {
		t.Fatalf("Run returned an error after the LSP exit notification: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write lifecycle messages: %v", err)
	}
	_ = pw.Close()
	select {
	case <-tr.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stdio read loop did not stop after closing its input")
	}
	if srv.State() != StateExited {
		t.Fatalf("server state after graceful exit = %s, want exited", srv.State())
	}
}

// TestS13_P0FloodDoesNotStarveP4 verifies forward progress of background
// work under a sustained flood of interactive requests (goal.md §S15/F7).
//
// Mapping note: the flood uses textDocument/completion, classified P0
// (PriorityCompletion). goal.md's "P4 background" class maps through the
// production classifier to the lowest served class: no LSP method maps to
// scheduler PriorityIndexing today, so background-class work is represented
// by textDocument/documentSymbol, which classifies to PriorityMaintenance
// (P5) — the class strict priority would starve first. (workspace/symbol was
// NOT used: its handler takes s.mu.RLock around findWorkspaceBackend, which
// RLocks again — recursive read-lock that deadlocks against a concurrent
// writer; see report. documentSymbol exercises a real backend dispatch with
// single-level locking.)
//
// Expected: within the observation window, background work still completes
// at least N times despite hundreds of P0 submissions (aging must prevent
// starvation). Product code is untouched; if this fails, starvation is real.
func TestS13_P0FloodDoesNotStarveP4(t *testing.T) {
	cfg := DefaultConfig()
	// Tight aging so starvation relief is observable inside the window
	// (agingLoop ticks at max(PriorityAgingMs/2, 100ms)).
	cfg.Scheduler.PriorityAgingMs = 50
	cfg.Scheduler.MaxConcurrent = 2 // maximize contention pressure
	srv := New(cfg)
	ft := attachTransport(srv)
	stop := startScheduler(srv)
	defer stop()

	srv.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
	srv.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	srv.publishSnapshot()

	// Lifecycle handshake so semantic work is admitted.
	srv.scheduleMessage(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 1}, "initialize", json.RawMessage(`{}`)))
	waitForState(t, srv, StateInitializing)
	srv.scheduleMessage(context.Background(), jsonrpc.NewNotification("initialized", nil))
	waitForState(t, srv, StateRunning)

	const (
		flooders       = 4
		window         = 2 * time.Second
		bgInterval     = 25 * time.Millisecond
		minBgCompleted = 5
		bgIDBase       = 1 << 20
	)

	completionParams := json.RawMessage(`{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`)

	var wg sync.WaitGroup
	floodStop := make(chan struct{})
	for f := 0; f < flooders; f++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := int64(1); ; id++ {
				select {
				case <-floodStop:
					return
				default:
				}
				srv.scheduleMessage(context.Background(), jsonrpc.NewRequest(
					jsonrpc.RequestID{Num: id}, "textDocument/completion", completionParams))
				time.Sleep(100 * time.Microsecond)
			}
		}()
	}

	var bgMu sync.Mutex
	var bgIDs []int64
	bgDone := make(chan struct{})
	go func() {
		defer close(bgDone)
		tick := time.NewTicker(bgInterval)
		defer tick.Stop()
		for n := 0; ; n++ {
			id := int64(bgIDBase + n)
			bgMu.Lock()
			bgIDs = append(bgIDs, id)
			bgMu.Unlock()
			srv.scheduleMessage(context.Background(), jsonrpc.NewRequest(
				jsonrpc.RequestID{Num: id}, "textDocument/documentSymbol",
				json.RawMessage(`{"textDocument":{"uri":"file:///x.go"}}`)))
			select {
			case <-floodStop:
				return
			case <-tick.C:
			}
		}
	}()

	time.Sleep(window)
	close(floodStop)
	wg.Wait()
	<-bgDone
	// Grace: let in-flight work land its responses.
	time.Sleep(500 * time.Millisecond)

	resps := ft.responses()
	completed := make(map[int64]bool)
	for _, m := range resps {
		if m.ID != nil && m.ID.Num >= bgIDBase {
			completed[m.ID.Num] = true
		}
	}
	bgMu.Lock()
	submitted := append([]int64(nil), bgIDs...)
	bgMu.Unlock()

	if len(completed) < minBgCompleted {
		st := srv.scheduler.Stats()
		t.Errorf("background work starved: %d/%d completed (want >= %d); scheduler stats: enq=%d exec=%d rej=%d q=%v",
			len(completed), len(submitted), minBgCompleted,
			st.TotalEnqueued, st.TotalExecuted, st.TotalRejected, st.QueueLengths)
	}
}

// crashHoverBackend simulates a language backend whose analysis process dies
// mid-request: the owned pipe (stand-in for its IPC channel) is closed and an
// error surfaces, racing with the client's $/cancelRequest.
type crashHoverBackend struct {
	mockBackend

	mu      sync.Mutex
	started chan struct{}
	crash   chan struct{}
}

func (b *crashHoverBackend) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.started = make(chan struct{})
	b.crash = make(chan struct{})
}

func (b *crashHoverBackend) enterHandler() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.started
}

func (b *crashHoverBackend) doCrash() {
	b.mu.Lock()
	defer b.mu.Unlock()
	close(b.crash)
}

func (b *crashHoverBackend) Hover(ctx context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.mu.Lock()
	started, crash := b.started, b.crash
	b.mu.Unlock()
	close(started)

	select {
	case <-ctx.Done():
		// Cancellation won the race.
		return identity.SemanticResult[*languages.HoverResult]{}, ctx.Err()
	case <-crash:
		// Backend "process" dies: IPC pipe torn down mid-flight.
		pr, pw := io.Pipe()
		_ = pw.CloseWithError(errors.New("backend process died"))
		_, _ = pr.Read(make([]byte, 1))
		return identity.SemanticResult[*languages.HoverResult]{},
			errors.New("backend crashed mid-request")
	}
}

// TestS4_CancelRaceWithCrash replays the §S4 cancel-race permutation against
// a §S13 crashing backend: cancel lands while the backend is mid-request and
// the backend dies at the same moment. Each permutation must end in exactly
// one terminal response for the request, never silence, a double response,
// panic, or a goroutine leak.
func TestS4_CancelRaceWithCrash(t *testing.T) {
	base := runtime.NumGoroutine()

	srv := New(DefaultConfig())
	ft := attachTransport(srv)
	stop := startScheduler(srv)
	defer stop()

	be := &crashHoverBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
	}
	srv.RegisterBackend("go", be)

	srv.scheduleMessage(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 1}, "initialize", json.RawMessage(`{}`)))
	waitForState(t, srv, StateInitializing)
	srv.scheduleMessage(context.Background(), jsonrpc.NewNotification("initialized", nil))
	waitForState(t, srv, StateRunning)

	const iterations = 24
	hoverParams := func(i int) json.RawMessage {
		// Distinct position per iteration => distinct F11 coalesce keys, so
		// iterations cannot join each other's execution.
		return json.RawMessage(fmt.Sprintf(
			`{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":%d}}`, i))
	}
	inflightGone := func(id int64) bool {
		key := requestIDKey(jsonrpc.RequestID{Num: id})
		srv.mu.RLock()
		defer srv.mu.RUnlock()
		_, ok := srv.inflight[key]
		return !ok
	}

	for i := 0; i < iterations; i++ {
		id := int64(100 + i)
		be.arm()

		srv.scheduleMessage(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: id}, "textDocument/hover", hoverParams(i)))

		select { // wait until the request is executing in the backend
		case <-be.enterHandler():
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: backend never entered handler", i)
		}

		// Permute arrival order: even iterations cancel first, odd ones
		// crash first; every 4th delays the cancel slightly.
		cancelMsg := jsonrpc.NewNotification("$/cancelRequest",
			json.RawMessage(fmt.Sprintf(`{"id":%d}`, id)))
		switch i % 4 {
		case 0:
			srv.scheduleMessage(context.Background(), cancelMsg)
			be.doCrash()
		case 1:
			be.doCrash()
			srv.scheduleMessage(context.Background(), cancelMsg)
		case 2:
			var once sync.Once
			go func() { once.Do(func() { time.Sleep(time.Millisecond); be.doCrash() }) }()
			srv.scheduleMessage(context.Background(), cancelMsg)
			once.Do(func() {})
		default:
			be.doCrash()
			time.Sleep(time.Millisecond)
			srv.scheduleMessage(context.Background(), cancelMsg)
		}

		// The in-flight entry is removed before the response is written, so
		// wait for the correlated terminal response itself.
		deadline := time.Now().Add(2 * time.Second)
		responded := false
		for !responded && time.Now().Before(deadline) {
			for _, m := range ft.responses() {
				if m.ID != nil && m.ID.Num == id {
					responded = true
					break
				}
			}
			if !responded {
				time.Sleep(2 * time.Millisecond)
			}
		}
		if !responded || !inflightGone(id) {
			t.Fatalf("iter %d: request did not produce its correlated terminal response and clear in-flight state (responded=%t)", i, responded)
		}
	}

	// Settle, then audit: exactly one response per request ID anywhere on
	// the wire (silence and double-response are both forbidden).
	time.Sleep(300 * time.Millisecond)
	counts := make(map[int64]int)
	for _, m := range ft.responses() {
		if m.ID != nil && m.ID.Num >= 100 && m.ID.Num < 100+iterations {
			counts[m.ID.Num]++
		}
	}
	for id := int64(100); id < 100+iterations; id++ {
		if n := counts[id]; n != 1 {
			t.Errorf("request id %d produced %d responses; want exactly one terminal response", id, n)
		}
	}

	// Wind down the scheduler pool explicitly before the leak audit
	// (Shutdown is idempotent; the deferred stop below stays safe).
	stop()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base+5 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: before=%d after=%d (tolerance +5)", base, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
