package replay

// Golden round-trip: record a live session against a server, replay the
// "in" stream into a fresh identical server, and require the produced "out"
// stream to match the recording message-for-message (§P9 deterministic
// replay; volatile fields stripped by normalization).

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/server"
)

// stubBackend answers deterministically — no toolchain required.
type stubBackend struct{}

func (s *stubBackend) LanguageID() string       { return "go" }
func (s *stubBackend) FileExtensions() []string { return []string{".go"} }
func (s *stubBackend) Close() error             { return nil }

func (s *stubBackend) Completion(_ context.Context, _ languages.CompletionRequest) ([]languages.CompletionItem, error) {
	return nil, nil
}

func (s *stubBackend) Hover(_ context.Context, req languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: "stub hover"},
	}, nil
}

func (s *stubBackend) Definition(_ context.Context, _ languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{Status: identity.ResultExact, Value: []languages.Location{}}, nil
}

func (s *stubBackend) References(_ context.Context, _ languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{Status: identity.ResultExact, Value: []languages.Location{}}, nil
}

func (s *stubBackend) DocumentSymbols(_ context.Context, _ languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	return nil, nil
}

func (s *stubBackend) WorkspaceSymbols(_ context.Context, _ languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	return nil, nil
}

func (s *stubBackend) SemanticTokens(_ context.Context, _ string, _ []byte) ([]languages.SemanticToken, error) {
	return nil, nil
}

func (s *stubBackend) Rename(_ context.Context, _ languages.RenameRequest) (identity.SemanticResult[languages.ValidatedEdit], error) {
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status: identity.ResultUnavailable,
	}, nil
}

func (s *stubBackend) Diagnostics(_ context.Context, _ string, _ []byte) ([]languages.Diagnostic, error) {
	return nil, nil
}

// feedTransport is a bidirectional fake: Read serves scripted messages,
// Write records everything the server produces.
type feedTransport struct {
	in      chan *jsonrpc.Message
	writesM sync.Mutex
	writes  []*jsonrpc.Message
	done    chan struct{}
	closed  chan struct{}
}

func newFeedTransport(in ...*jsonrpc.Message) *feedTransport {
	f := &feedTransport{
		in:     make(chan *jsonrpc.Message, len(in)),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	for _, m := range in {
		f.in <- m
	}
	close(f.in) // EOF after the script drains
	return f
}

func (f *feedTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	select {
	case msg, ok := <-f.in:
		if !ok {
			// Script drained: hold Run open until the test closes or cancels.
			select {
			case <-f.closed:
				return nil, context.Canceled
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *feedTransport) Write(_ context.Context, msg *jsonrpc.Message) error {
	f.writesM.Lock()
	f.writes = append(f.writes, msg)
	f.writesM.Unlock()
	return nil
}

func (f *feedTransport) Close() error { close(f.closed); return nil }

// Done satisfies transport.Transport: closed on Close, ending Run.
func (f *feedTransport) Done() <-chan struct{} { return f.closed }

func (f *feedTransport) written() int {
	f.writesM.Lock()
	defer f.writesM.Unlock()
	return len(f.writes)
}

func scriptedIn() []*jsonrpc.Message {
	initParams := json.RawMessage(`{"processId":null,"rootUri":"file:///w","capabilities":{}}`)
	openParams := json.RawMessage(`{"textDocument":{"uri":"file:///w/a.go","languageId":"go","version":1,"text":"package a"}}`)
	hoverParams := json.RawMessage(`{"textDocument":{"uri":"file:///w/a.go"},"position":{"line":0,"character":0}}`)
	shutdownParams := json.RawMessage(`null`)
	return []*jsonrpc.Message{
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", initParams),
		jsonrpc.NewNotification("initialized", json.RawMessage(`{}`)),
		jsonrpc.NewNotification("textDocument/didOpen", openParams),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 2}, "textDocument/hover", hoverParams),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 3}, "shutdown", shutdownParams),
	}
}

const wantResponses = 3 // initialize + hover + shutdown

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

func TestP9_RecordReplayRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// --- Phase 1: record a session against a live server. ---
	live := server.New(server.DefaultConfig())
	live.RegisterBackend("go", &stubBackend{})
	script := scriptedIn()
	ft := newFeedTransport(script...)
	rec, err := NewRecorder(ft, dir+"/session.jsonl", Meta{
		ConfigHash: ConfigHash(map[string]string{"transport": "stdio"}),
		Toolchain:  map[string]string{"go": "stub-1"},
	}, live.SnapshotRevision)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}

	sessionDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { sessionDone <- live.Run(ctx, rec) }()

	waitFor(t, func() bool { return ft.written() >= wantResponses })
	cancel()
	select {
	case <-sessionDone:
	case <-time.After(3 * time.Second):
		t.Fatal("live run did not stop")
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("recorder close: %v", err)
	}

	// --- Phase 2: load and replay into a fresh server. ---
	sess, err := LoadSession(dir + "/session.jsonl")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if sess.Meta.ConfigHash == "" || len(sess.Entries) == 0 {
		t.Fatal("session meta/entries missing")
	}

	fresh := server.New(server.DefaultConfig())
	fresh.RegisterBackend("go", &stubBackend{})
	player := NewPlayer()
	for _, e := range sess.Entries {
		if e.Dir != "in" {
			continue
		}
		var msg jsonrpc.Message
		if err := json.Unmarshal(e.Payload, &msg); err != nil {
			t.Fatalf("entry %d unparsable: %v", e.Seq, err)
		}
		player.Feed(&msg)
	}
	go player.EndFeed()

	replayCtx, replayCancel := context.WithCancel(context.Background())
	runErr := fresh.Run(replayCtx, player.Transport())
	replayCancel()
	if runErr != nil && runErr != context.Canceled {
		t.Fatalf("replay run: %v", runErr)
	}

	if err := player.CompareOut(sess.Entries); err != nil {
		t.Fatal(err)
	}
}

// TestRecorder_WriteFailureSurfaces: a recording write failure used to be
// swallowed by `_ = r.log(...)`. It must now be retained and exposed via
// Err() while the live session keeps flowing untouched (pass-through intact,
// later writes still attempted, first error never overwritten).
func TestRecorder_WriteFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	// Payload larger than the bufio.Writer buffer forces an actual write to
	// the (closed) file instead of lingering in the buffer.
	bulk := json.RawMessage(`{"pad":"` + strings.Repeat("x", 16*1024) + `"}`)
	ft := newFeedTransport(jsonrpc.NewRequest(jsonrpc.RequestID{Num: 7}, "textDocument/hover", bulk))
	rec, err := NewRecorder(ft, dir+"/session.jsonl", Meta{}, nil)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	defer rec.Close()

	// Sabotage the sink: close the file under the recorder so every real disk
	// write fails from here on.
	if err := rec.f.Close(); err != nil {
		t.Fatalf("sabotage close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	gotMsg, readErr := rec.Read(ctx)
	if gotMsg == nil || readErr != nil {
		t.Fatalf("Read pass-through broken: msg=%v err=%v", gotMsg, readErr)
	}
	if rec.Err() == nil {
		t.Fatal("Err() is nil after failed recording write")
	}
	first := rec.Err()

	if err := rec.Write(ctx, gotMsg); err != nil {
		t.Fatalf("Write pass-through broken: %v", err)
	}
	if ft.written() != 1 {
		t.Fatalf("inner transport writes = %d, want 1", ft.written())
	}

	// Later failures must still be attempted but must not overwrite the first.
	rec.log("out", raw(gotMsg))
	if rec.Err() != first {
		t.Fatal("first recorded error was overwritten by a later failure")
	}
}
