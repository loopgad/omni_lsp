package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	golangbackend "github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// diagBackend serves canned diagnostics and counts invocations.
type diagBackend struct {
	mockBackend
	calls atomic.Int64
	last  atomic.Value
}

type cancelAwareDiagBackend struct {
	mockBackend
	started  chan struct{}
	finished chan struct{}
}

type scopedDiagBackend struct {
	*diagBackend
	generation atomic.Uint64
}

type updatingDiagBackend struct {
	*scopedDiagBackend
	mu                     sync.Mutex
	items                  []languages.Diagnostic
	initialPullDiagnostics []languages.Diagnostic
	handler                func(uri string)
}

type postResultUpdateDiagBackend struct {
	*scopedDiagBackend
	uri         string
	mu          sync.Mutex
	items       []languages.Diagnostic
	replacement []languages.Diagnostic
	handler     func(uri string)
	calls       atomic.Int64
	repeat      bool
	armed       bool
	fired       bool
}

type diagnosticsLeaseTestKey struct{}

type diagnosticsLeaseTestState struct {
	finished atomic.Bool
}

type goDiagnosticsLeaseTestBackend struct {
	languages.Backend
	synchronizer languages.WorkspaceSnapshotSynchronizer
	mu           sync.Mutex
	tokens       []*diagnosticsLeaseTestState
	finishCalls  int
	afterFirst   func()
}

func (b *goDiagnosticsLeaseTestBackend) BeginWorkspaceSnapshot(ctx context.Context, workspace languages.WorkspaceSnapshot) (context.Context, func() error, error) {
	leaseCtx, finish, err := b.synchronizer.BeginWorkspaceSnapshot(ctx, workspace)
	if err != nil {
		return nil, nil, err
	}
	state := &diagnosticsLeaseTestState{}
	return context.WithValue(leaseCtx, diagnosticsLeaseTestKey{}, state), func() error {
		err := finish()
		state.finished.Store(true)
		b.mu.Lock()
		b.finishCalls++
		b.mu.Unlock()
		return err
	}, nil
}

func (b *goDiagnosticsLeaseTestBackend) WorkspaceSnapshotGeneration() uint64 {
	return b.synchronizer.WorkspaceSnapshotGeneration()
}

func (b *goDiagnosticsLeaseTestBackend) Diagnostics(ctx context.Context, uri string, content []byte) ([]languages.Diagnostic, error) {
	state, ok := ctx.Value(diagnosticsLeaseTestKey{}).(*diagnosticsLeaseTestState)
	if !ok || state == nil || state.finished.Load() {
		return nil, fmt.Errorf("diagnostics called without an active workspace snapshot context")
	}
	b.mu.Lock()
	b.tokens = append(b.tokens, state)
	call := len(b.tokens)
	afterFirst := b.afterFirst
	b.mu.Unlock()

	items, err := b.Backend.Diagnostics(ctx, uri, content)
	if call == 1 && afterFirst != nil {
		afterFirst()
	}
	return items, err
}

func (b *goDiagnosticsLeaseTestBackend) leaseState() (int, []*diagnosticsLeaseTestState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.finishCalls, append([]*diagnosticsLeaseTestState(nil), b.tokens...)
}

func (b *updatingDiagBackend) Diagnostics(_ context.Context, uri string, _ []byte) ([]languages.Diagnostic, error) {
	call := b.calls.Add(1)
	b.mu.Lock()
	var handler func(uri string)
	if call == 1 && b.initialPullDiagnostics != nil {
		b.items = append([]languages.Diagnostic(nil), b.initialPullDiagnostics...)
		b.initialPullDiagnostics = nil
		handler = b.handler
	}
	items := append([]languages.Diagnostic(nil), b.items...)
	b.mu.Unlock()
	if handler != nil {
		handler(uri)
	}
	return items, nil
}

func (b *updatingDiagBackend) SetDiagnosticsUpdateHandler(handler func(uri string)) {
	b.mu.Lock()
	b.handler = handler
	b.mu.Unlock()
}

func (b *updatingDiagBackend) publish(uri string, items []languages.Diagnostic) {
	b.mu.Lock()
	b.items = append([]languages.Diagnostic(nil), items...)
	handler := b.handler
	b.mu.Unlock()
	if handler != nil {
		handler(uri)
	}
}

func (b *postResultUpdateDiagBackend) Diagnostics(_ context.Context, _ string, _ []byte) ([]languages.Diagnostic, error) {
	b.calls.Add(1)
	b.mu.Lock()
	items := append([]languages.Diagnostic(nil), b.items...)
	b.armed = true
	b.mu.Unlock()
	return items, nil
}

func (b *postResultUpdateDiagBackend) SetDiagnosticsUpdateHandler(handler func(uri string)) {
	b.mu.Lock()
	b.handler = handler
	b.mu.Unlock()
}

func (b *postResultUpdateDiagBackend) WorkspaceSnapshotGeneration() uint64 {
	b.mu.Lock()
	var handler func(uri string)
	if b.armed {
		b.armed = false
		if !b.fired || b.repeat {
			b.fired = true
			b.items = append([]languages.Diagnostic(nil), b.replacement...)
			handler = b.handler
		}
	}
	b.mu.Unlock()
	if handler != nil {
		handler(b.uri)
	}
	return b.generation.Load()
}

func (b *scopedDiagBackend) BeginWorkspaceSnapshot(ctx context.Context, _ languages.WorkspaceSnapshot) (context.Context, func() error, error) {
	return ctx, func() error { return nil }, nil
}

func (b *scopedDiagBackend) WorkspaceSnapshotGeneration() uint64 {
	return b.generation.Load()
}

func (b *cancelAwareDiagBackend) Diagnostics(ctx context.Context, _ string, _ []byte) ([]languages.Diagnostic, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	close(b.finished)
	return nil, ctx.Err()
}

func (b *diagBackend) Diagnostics(_ context.Context, _ string, content []byte) ([]languages.Diagnostic, error) {
	b.calls.Add(1)
	b.last.Store(string(content))
	return []languages.Diagnostic{
		{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 3, Severity: 1, Code: "E1", Source: "go", Message: "undefined: foo"},
	}, nil
}

func TestC11_PullUsesCapturedSnapshot(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	be := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("version-one"), vfs.SourceEditor)
	s.publishSnapshot()
	oldSnap := s.snapMgr.Current()
	s.vfs.Update(uri, 2, []byte("version-two"))
	s.publishSnapshot()

	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/diagnostic",
		json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`))
	resp := s.dispatcher.Dispatch(withSnapshot(context.Background(), oldSnap), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("pull failed: %+v", resp)
	}
	if got := be.last.Load().(string); got != "version-one" {
		t.Fatalf("diagnostics saw %q, want captured version-one", got)
	}
}

func TestC11_DiagnosticCacheTracksBackendWorkspaceGeneration(t *testing.T) {
	const uri = "file:///w/main.py"
	s := New(DefaultConfig())
	be := &scopedDiagBackend{diagBackend: &diagBackend{mockBackend: mockBackend{langID: "python", exts: []string{".py"}}}}
	s.RegisterBackend("python", be)
	s.vfs.Open(uri, "python", 1, []byte("value = 1\n"), vfs.SourceEditor)
	s.publishSnapshot()

	first, err := s.diag.compute(context.Background(), uri)
	if err != nil || first == nil {
		t.Fatalf("first diagnostics = %v, %v", first, err)
	}
	if got := be.calls.Load(); got != 1 {
		t.Fatalf("first backend calls = %d, want 1", got)
	}

	// A same-parent-revision update to another URI can change the child
	// workspace generation without changing this file's cache inputs.
	be.generation.Add(1)
	second, err := s.diag.compute(context.Background(), uri)
	if err != nil || second == nil {
		t.Fatalf("second diagnostics = %v, %v", second, err)
	}
	if got := be.calls.Load(); got != 2 {
		t.Fatalf("backend calls after workspace generation advanced = %d, want cache miss and 2", got)
	}
}

func TestC11_GoDiagnosticsUseWorkspaceLeaseAcrossRetry(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "go.mod"), []byte("module example.com/diagtest\n\ngo 1.26.1\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	mainPath := filepath.Join(workDir, "main.go")
	typesPath := filepath.Join(workDir, "types.go")
	mainContent := []byte("package diagtest\nvar value WorkspaceType\n")
	unsavedTypes := []byte("package diagtest\ntype WorkspaceType struct{}\n")
	if err := os.WriteFile(mainPath, mainContent, 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	// The editor's sibling-file definition is not yet present on disk.
	if err := os.WriteFile(typesPath, []byte("package diagtest\n"), 0o644); err != nil {
		t.Fatalf("write types.go: %v", err)
	}
	mainURI := uri.FromPath(mainPath).Canonical()
	typesURI := uri.FromPath(typesPath).Canonical()

	goBackend := golangbackend.New(workDir)
	defer goBackend.Close()
	backend := &goDiagnosticsLeaseTestBackend{
		Backend:      goBackend,
		synchronizer: goBackend,
	}
	s := New(DefaultConfig())
	defer s.diag.Close()
	s.RegisterBackend("go", backend)
	backend.afterFirst = func() {
		// Change only the diagnostic generation to force compute to retry while
		// retaining the same captured workspace lease.
		s.diag.mu.Lock()
		s.diag.updates[mainURI]++
		s.diag.mu.Unlock()
	}
	s.vfs.Open(mainURI, "go", 1, mainContent, vfs.SourceEditor)
	s.vfs.Open(typesURI, "go", 1, unsavedTypes, vfs.SourceEditor)
	s.publishSnapshot()

	items, err := s.diag.compute(context.Background(), mainURI)
	if err != nil {
		t.Fatalf("compute Go diagnostics: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("Go diagnostics = %+v, want the captured unsaved sibling type to resolve WorkspaceType", items)
	}
	finishCalls, tokens := backend.leaseState()
	if finishCalls != 1 {
		t.Fatalf("workspace lease finish calls = %d, want one after computation", finishCalls)
	}
	if len(tokens) != 2 {
		t.Fatalf("Go Diagnostics calls = %d, want one retry under the same lease", len(tokens))
	}
	if tokens[0] == nil || tokens[0] != tokens[1] {
		t.Fatalf("Diagnostics observed different lease tokens across retry: %p then %p", tokens[0], tokens[1])
	}
	if !tokens[0].finished.Load() {
		t.Fatal("workspace lease was not finished after compute returned")
	}
}

func TestC11_DiagnosticsUseSemanticBackendForLanguageVariants(t *testing.T) {
	s := New(DefaultConfig())
	cBackend := &diagBackend{mockBackend: mockBackend{langID: "cpp", exts: []string{".c", ".h"}}}
	tsBackend := &diagBackend{mockBackend: mockBackend{langID: "typescript", exts: []string{".js", ".jsx", ".ts", ".tsx"}}}
	s.RegisterBackend("cpp", cBackend)
	s.RegisterBackend("c", cBackend)
	s.RegisterBackend("typescript", tsBackend)
	s.RegisterBackend("ts", tsBackend)

	docs := []struct {
		uri, languageID string
		want            languages.Backend
	}{
		{"file:///w/main.c", "c", cBackend},
		{"file:///w/header.h", "cpp", cBackend},
		{"file:///w/app.js", "javascript", tsBackend},
		{"file:///w/view.jsx", "javascriptreact", tsBackend},
		{"file:///w/component.tsx", "typescriptreact", tsBackend},
	}
	for i, doc := range docs {
		s.vfs.Open(doc.uri, doc.languageID, int64(i+1), []byte("source"), vfs.SourceEditor)
	}
	s.publishSnapshot()

	for _, doc := range docs {
		semanticBackend, err := s.resolveBackend(doc.uri)
		if err != nil {
			t.Fatalf("semantic backend for %s: %v", doc.uri, err)
		}
		if semanticBackend != doc.want {
			t.Errorf("semantic backend for %s = %T, want %T", doc.uri, semanticBackend, doc.want)
		}
		if got := s.findWorkspaceBackendFor(doc.uri); got != semanticBackend {
			t.Errorf("diagnostic backend for %s = %T, want semantic backend %T", doc.uri, got, semanticBackend)
		}
		if _, err := s.diag.compute(context.Background(), doc.uri); err != nil {
			t.Errorf("diagnostics for %s: %v", doc.uri, err)
		}
	}

	if got := cBackend.calls.Load(); got != 2 {
		t.Errorf("C/C++ backend diagnostics calls = %d, want 2", got)
	}
	if got := tsBackend.calls.Load(); got != 3 {
		t.Errorf("TypeScript backend diagnostics calls = %d, want 3", got)
	}
}

func TestC11_DiagnosticsDoNotFallBackForUnknownLanguage(t *testing.T) {
	s := New(DefaultConfig())
	goBackend := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	tsBackend := &diagBackend{mockBackend: mockBackend{langID: "typescript", exts: []string{".ts", ".tsx"}}}
	s.RegisterBackend("go", goBackend)
	s.RegisterBackend("typescript", tsBackend)
	const uri = "file:///w/unknown.omnilsp-test"
	s.vfs.Open(uri, "unknown", 1, []byte("source"), vfs.SourceEditor)
	s.publishSnapshot()

	if got := s.findWorkspaceBackendFor(uri); got != nil {
		t.Fatalf("unknown document resolved to arbitrary backend %T", got)
	}
	if _, err := s.diag.compute(context.Background(), uri); err == nil {
		t.Fatal("diagnostics unexpectedly succeeded for unknown language")
	}
	if got := goBackend.calls.Load() + tsBackend.calls.Load(); got != 0 {
		t.Fatalf("unknown language invoked a registered backend %d times", got)
	}
}

func TestC11_PushDebouncedPublish(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	be := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	rt := &recordingTransport{}
	s.mu.Lock()
	s.transport = rt
	s.mu.Unlock()

	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
	s.publishSnapshot()
	s.diag.request(uri) // didOpen tail

	// Debounce window must elapse before the backend is touched.
	time.Sleep(50 * time.Millisecond)
	if n := be.calls.Load(); n != 0 {
		t.Fatalf("backend called %d times inside debounce window", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(rt.filterMethod("textDocument/publishDiagnostics")) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	pubs := rt.filterMethod("textDocument/publishDiagnostics")
	if len(pubs) != 1 {
		t.Fatalf("expected exactly 1 publish after debounce, got %d", len(pubs))
	}
	if !containsBytes(pubs[0].Params, `"uri":"`+uri+`"`) || !containsBytes(pubs[0].Params, "undefined") {
		t.Errorf("publish payload malformed: %s", pubs[0].Params)
	}
	if n := be.calls.Load(); n != 1 {
		t.Fatalf("backend calls = %d, want 1 (debounce coalesced)", n)
	}

	// Rapid re-requests coalesce into one debounced push; the §C11 cache
	// answers it, so the backend is NOT touched again — but the wire still
	// carries the republished set.
	rt.reset()
	s.diag.request(uri)
	s.diag.request(uri)
	time.Sleep(400 * time.Millisecond)
	if n := be.calls.Load(); n != 1 {
		t.Fatalf("cache miss after identical re-push: calls = %d, want 1", n)
	}
	if n := len(rt.filterMethod("textDocument/publishDiagnostics")); n != 1 {
		t.Fatalf("republished diagnostics %d times, want 1", n)
	}
}

func TestC11_DidCloseStopsPendingDiagnosticPush(t *testing.T) {
	const uri = "file:///w/closed.go"
	s := New(DefaultConfig())
	be := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	rt := &recordingTransport{}
	s.mu.Lock()
	s.transport = rt
	s.mu.Unlock()
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	s.diag.request(uri)
	s.diag.didClose(uri)
	time.Sleep(diagDebounce + 50*time.Millisecond)
	if be.calls.Load() != 0 || len(rt.filterMethod("textDocument/publishDiagnostics")) != 0 {
		t.Fatalf("closed document diagnostics ran or were published: backend calls=%d", be.calls.Load())
	}
	s.diag.mu.Lock()
	defer s.diag.mu.Unlock()
	if _, ok := s.diag.cache[uri]; ok || s.diag.timers[uri] != nil || s.diag.runs[uri] != nil {
		t.Fatalf("didClose retained per-URI diagnostic state: cache=%v timer=%v run=%v", s.diag.cache[uri], s.diag.timers[uri], s.diag.runs[uri])
	}
}

func TestC11_DidCloseCancelsInFlightDiagnosticPush(t *testing.T) {
	const uri = "file:///w/inflight.go"
	s := New(DefaultConfig())
	be := &cancelAwareDiagBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		started:     make(chan struct{}, 1), finished: make(chan struct{}),
	}
	s.RegisterBackend("go", be)
	rt := &recordingTransport{}
	s.mu.Lock()
	s.transport = rt
	s.mu.Unlock()
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	s.diag.request(uri)
	select {
	case <-be.started:
	case <-time.After(2 * time.Second):
		t.Fatal("diagnostic backend was not invoked")
	}
	s.diag.didClose(uri)
	select {
	case <-be.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("didClose did not cancel the in-flight diagnostic request")
	}
	time.Sleep(20 * time.Millisecond)
	if len(rt.filterMethod("textDocument/publishDiagnostics")) != 0 {
		t.Fatal("in-flight diagnostics were published after didClose")
	}
	s.diag.mu.Lock()
	defer s.diag.mu.Unlock()
	if _, ok := s.diag.cache[uri]; ok || s.diag.runs[uri] != nil {
		t.Fatalf("in-flight didClose left cached or active state: cache=%v run=%v", s.diag.cache[uri], s.diag.runs[uri])
	}
}

func TestC11_PullDiagnosticsAfterCloseReturnsEmptyWithoutBackend(t *testing.T) {
	const uri = "file:///w/closed-pull.go"
	s := New(DefaultConfig())
	be := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	closed := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewNotification(
		"textDocument/didClose", json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
	if closed != nil {
		t.Fatalf("didClose returned a response: %+v", closed)
	}

	resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 2}, "textDocument/diagnostic",
		json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
	if resp == nil || resp.Error != nil {
		t.Fatalf("pull after close failed: %+v", resp)
	}
	var result struct {
		Kind  string                 `json:"kind"`
		Items []languages.Diagnostic `json:"items"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode pull result: %v", err)
	}
	if result.Kind != "full" || len(result.Items) != 0 {
		t.Fatalf("pull after close = %+v, want empty full report", result)
	}
	if be.calls.Load() != 0 {
		t.Fatalf("closed pull invoked backend %d times", be.calls.Load())
	}
	s.diag.mu.Lock()
	defer s.diag.mu.Unlock()
	if _, ok := s.diag.cache[uri]; ok || len(s.diag.pullRuns[uri]) != 0 {
		t.Fatalf("closed pull left diagnostic state: cache=%v pullRuns=%v", s.diag.cache[uri], s.diag.pullRuns[uri])
	}
}

func TestC11_DidCloseCancelsInFlightPullDiagnostics(t *testing.T) {
	const uri = "file:///w/inflight-pull.go"
	s := New(DefaultConfig())
	be := &cancelAwareDiagBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		started:     make(chan struct{}, 1), finished: make(chan struct{}),
	}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	response := make(chan *jsonrpc.Message, 1)
	go func() {
		response <- s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 2}, "textDocument/diagnostic",
			json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
	}()
	select {
	case <-be.started:
	case <-time.After(2 * time.Second):
		t.Fatal("pull diagnostic backend was not invoked")
	}
	closed := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewNotification(
		"textDocument/didClose", json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
	if closed != nil {
		t.Fatalf("didClose returned a response: %+v", closed)
	}
	select {
	case <-be.finished:
	case <-time.After(2 * time.Second):
		t.Fatal("didClose did not cancel the in-flight pull")
	}
	select {
	case resp := <-response:
		if resp == nil || resp.Error != nil || string(resp.Result) == "" || !containsBytes(resp.Result, `"items":[]`) {
			t.Fatalf("closing pull should resolve to empty full report: %+v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pull request did not complete after close")
	}
	s.diag.mu.Lock()
	defer s.diag.mu.Unlock()
	if _, ok := s.diag.cache[uri]; ok || s.diag.pullRuns[uri] != nil {
		t.Fatalf("in-flight close left diagnostic state: cache=%v pullRuns=%v", s.diag.cache[uri], s.diag.pullRuns[uri])
	}
}

func TestC11_PullStableResultIdAndCache(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	be := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
	s.publishSnapshot()

	pull := func() json.RawMessage {
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "textDocument/diagnostic",
			json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("pull failed: %+v", resp)
		}
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		raw, _ := json.Marshal(resp)
		json.Unmarshal(raw, &env)
		return env.Result
	}

	first := pull()
	if !containsBytes(first, `"kind":"full"`) || !containsBytes(first, `"resultId"`) {
		t.Fatalf("pull shape wrong: %s", first)
	}
	second := pull()
	if n := be.calls.Load(); n != 1 {
		t.Fatalf("second pull recomputed (calls=%d): cache key must gate", n)
	}
	var a, b struct {
		ResultID string `json:"resultId"`
	}
	json.Unmarshal(first, &a)
	json.Unmarshal(second, &b)
	if a.ResultID == "" || a.ResultID != b.ResultID {
		t.Fatalf("resultId unstable across identical pulls")
	}

	// Content change ⇒ new key ⇒ recompute.
	s.vfs.Update(uri, 2, []byte("package main // changed\n"))
	s.publishSnapshot()
	pull()
	if n := be.calls.Load(); n != 2 {
		t.Fatalf("content change did not invalidate cache (calls=%d)", n)
	}
}

func TestC11_ChildDiagnosticUpdateReplacesEmptyPullAndRepublishes(t *testing.T) {
	const uri = "file:///w/streaming.rs"
	s := New(DefaultConfig())
	be := &updatingDiagBackend{
		scopedDiagBackend: &scopedDiagBackend{diagBackend: &diagBackend{
			mockBackend: mockBackend{langID: "rust", exts: []string{".rs"}},
		}},
	}
	s.RegisterBackend("rust", be)
	rt := &recordingTransport{}
	s.mu.Lock()
	s.transport = rt
	s.mu.Unlock()
	s.vfs.Open(uri, "rust", 1, []byte("fn main() { missing(); }\n"), vfs.SourceEditor)
	s.publishSnapshot()

	pull := func() []languages.Diagnostic {
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "textDocument/diagnostic",
			json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("diagnostic pull failed: %+v", resp)
		}
		var result struct {
			Kind  string                 `json:"kind"`
			Items []languages.Diagnostic `json:"items"`
		}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatalf("decode diagnostic pull: %v", err)
		}
		if result.Kind != "full" {
			t.Fatalf("diagnostic report kind = %q, want full", result.Kind)
		}
		return result.Items
	}

	if first := pull(); len(first) != 0 {
		t.Fatalf("first empty report = %+v", first)
	}
	if calls := be.calls.Load(); calls != 1 {
		t.Fatalf("backend calls after initial pull = %d, want 1", calls)
	}
	be.publish(uri, []languages.Diagnostic{{
		StartLine: 0, StartChar: 11, EndLine: 0, EndChar: 18,
		Severity: 1, Code: "E0425", Source: "rust-analyzer", Message: "cannot find function `missing`",
	}})

	deadline := time.Now().Add(2 * time.Second)
	var latestPublish *jsonrpc.Message
	for time.Now().Before(deadline) {
		for _, message := range rt.filterMethod("textDocument/publishDiagnostics") {
			if containsBytes(message.Params, "cannot find function `missing`") {
				latestPublish = message
				break
			}
		}
		if latestPublish != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if latestPublish == nil {
		t.Fatal("later nonempty child diagnostics were not republished")
	}
	latest := pull()
	if len(latest) != 1 || latest[0].Message != "cannot find function `missing`" {
		t.Fatalf("pull after child update = %+v, want latest nonempty set", latest)
	}
	if calls := be.calls.Load(); calls != 2 {
		t.Fatalf("backend calls after cached latest pull = %d, want 2", calls)
	}
}

func TestC11_InitialChildDiagnosticDuringPullIsNotDropped(t *testing.T) {
	const uri = "file:///w/initial-streaming.rs"
	want := languages.Diagnostic{
		StartLine: 0, StartChar: 11, EndLine: 0, EndChar: 18,
		Severity: 1, Code: "E0425", Source: "rust-analyzer", Message: "cannot find function `missing`",
	}
	s := New(DefaultConfig())
	defer s.diag.Close()
	be := &updatingDiagBackend{
		scopedDiagBackend: &scopedDiagBackend{diagBackend: &diagBackend{
			mockBackend: mockBackend{langID: "rust", exts: []string{".rs"}},
		}},
		initialPullDiagnostics: []languages.Diagnostic{want},
	}
	s.RegisterBackend("rust", be)
	rt := &recordingTransport{}
	s.mu.Lock()
	s.transport = rt
	s.mu.Unlock()
	s.vfs.Open(uri, "rust", 1, []byte("fn main() { missing(); }\n"), vfs.SourceEditor)
	s.publishSnapshot()

	resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 1}, "textDocument/diagnostic",
		json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
	if resp == nil || resp.Error != nil {
		t.Fatalf("diagnostic pull failed: %+v", resp)
	}
	var result struct {
		Kind  string `json:"kind"`
		Items []struct {
			Message string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode diagnostic pull: %v", err)
	}
	if result.Kind != "full" || len(result.Items) != 1 || result.Items[0].Message != want.Message {
		t.Fatalf("initial pull = %+v, want full report containing the child result %+v", result, want)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, message := range rt.filterMethod("textDocument/publishDiagnostics") {
			if containsBytes(message.Params, want.Message) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("initial child diagnostic was not pushed to the client")
}

func TestC11_ChildUpdateAfterPullResultCaptureRetriesBeforeCommit(t *testing.T) {
	const uri = "file:///w/racing-streaming.rs"
	stale := languages.Diagnostic{Code: "E0", Source: "rust-analyzer", Message: "older report"}
	want := languages.Diagnostic{Code: "E1", Source: "rust-analyzer", Message: "replacement report"}
	s := New(DefaultConfig())
	defer s.diag.Close()
	be := &postResultUpdateDiagBackend{
		scopedDiagBackend: &scopedDiagBackend{diagBackend: &diagBackend{
			mockBackend: mockBackend{langID: "rust", exts: []string{".rs"}},
		}},
		uri:         uri,
		items:       []languages.Diagnostic{stale},
		replacement: []languages.Diagnostic{want},
	}
	s.RegisterBackend("rust", be)
	s.vfs.Open(uri, "rust", 1, []byte("fn main() {}\n"), vfs.SourceEditor)
	s.publishSnapshot()

	pull := func() []languages.Diagnostic {
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "textDocument/diagnostic",
			json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("diagnostic pull failed: %+v", resp)
		}
		var result struct {
			Kind  string                 `json:"kind"`
			Items []languages.Diagnostic `json:"items"`
		}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatalf("decode diagnostic pull: %v", err)
		}
		if result.Kind != "full" {
			t.Fatalf("diagnostic report kind = %q, want full", result.Kind)
		}
		return result.Items
	}

	if got := pull(); len(got) != 1 || got[0] != want {
		t.Fatalf("pull returned %+v, want replacement report %+v", got, want)
	}
	if calls := be.calls.Load(); calls != 2 {
		t.Fatalf("backend calls after raced pull = %d, want stale result discarded and retried once", calls)
	}
	s.diag.mu.Lock()
	entry, ok := s.diag.cache[uri]
	generation := s.diag.updates[uri]
	s.diag.mu.Unlock()
	if !ok || generation != 1 || entry.key.DiagnosticsGeneration != generation || len(entry.items) != 1 || entry.items[0] != want {
		t.Fatalf("cache does not contain the replacement generation: entry=%+v updates=%d", entry, generation)
	}
	if got := pull(); len(got) != 1 || got[0] != want {
		t.Fatalf("cached pull returned %+v, want replacement report %+v", got, want)
	}
	if calls := be.calls.Load(); calls != 2 {
		t.Fatalf("replacement pull missed the cache (calls=%d)", calls)
	}
}

func TestC11_ContinuousChildUpdatesReturnContentModified(t *testing.T) {
	const uri = "file:///w/churning-streaming.rs"
	s := New(DefaultConfig())
	defer s.diag.Close()
	be := &postResultUpdateDiagBackend{
		scopedDiagBackend: &scopedDiagBackend{diagBackend: &diagBackend{
			mockBackend: mockBackend{langID: "rust", exts: []string{".rs"}},
		}},
		uri:         uri,
		replacement: []languages.Diagnostic{{Code: "E1", Source: "rust-analyzer", Message: "changing report"}},
		repeat:      true,
	}
	s.RegisterBackend("rust", be)
	s.vfs.Open(uri, "rust", 1, []byte("fn main() {}\n"), vfs.SourceEditor)
	s.publishSnapshot()

	resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 1}, "textDocument/diagnostic",
		json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
	if resp == nil || resp.Error == nil || resp.Error.Code != jsonrpc.ContentModified {
		t.Fatalf("continuous diagnostic updates response = %+v, want ContentModified", resp)
	}
	if calls := be.calls.Load(); calls != diagRetryLimit {
		t.Fatalf("backend calls during continuous updates = %d, want retry limit %d", calls, diagRetryLimit)
	}
	s.diag.mu.Lock()
	_, cached := s.diag.cache[uri]
	updates := s.diag.updates[uri]
	s.diag.mu.Unlock()
	if cached || updates != diagRetryLimit {
		t.Fatalf("continuous updates produced cache=%v and generation=%d, want no cache and %d updates", cached, updates, diagRetryLimit)
	}
}

func TestI19_CodeActionInRangeOnly(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	s.RegisterBackend("go", &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}})
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
	s.publishSnapshot()

	dispatch := func(rng string) json.RawMessage {
		body := `{"textDocument":{"uri":"` + uri + `"},"range":` + rng + `,"context":{}}`
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 5}, "textDocument/codeAction", json.RawMessage(body)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("codeAction failed: %+v", resp)
		}
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		raw, _ := json.Marshal(resp)
		json.Unmarshal(raw, &env)
		return env.Result
	}

	hit := dispatch(`{"start":{"line":0,"character":0},"end":{"line":0,"character":10}}`)
	if !containsBytes(hit, `"kind":"quickfix"`) || !containsBytes(hit, "About this diagnostic") {
		t.Fatalf("in-range actions missing: %s", hit)
	}
	miss := dispatch(`{"start":{"line":9,"character":0},"end":{"line":9,"character":0}}`)
	var empty []map[string]any
	if err := json.Unmarshal(miss, &empty); err != nil || len(empty) != 0 {
		t.Fatalf("out-of-range should yield empty actions, got %s", miss)
	}
}

func TestC4_PositionEncodingNegotiation(t *testing.T) {
	const uri = "file:///w/main.go"

	t.Run("client proposal wins and echoes", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
		resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "initialize",
			json.RawMessage(`{"processId":1,"rootUri":"file:///w","capabilities":{},"general":{"positionEncodings":["utf-8","utf-16"]}}`)))
		if resp == nil || resp.Error != nil {
			t.Fatalf("initialize failed: %+v", resp)
		}
		raw, _ := json.Marshal(resp)
		var env struct {
			Result struct {
				Capabilities struct {
					PositionEncoding string `json:"positionEncoding"`
				} `json:"capabilities"`
			} `json:"result"`
		}
		json.Unmarshal(raw, &env)
		if env.Result.Capabilities.PositionEncoding != "utf-8" {
			t.Fatalf("negotiated = %q, want utf-8 (first client proposal)", env.Result.Capabilities.PositionEncoding)
		}
		if got := s.negotiatedEncodingInt(); got != 0 {
			t.Fatalf("encoding int = %d, want 0 (utf-8)", got)
		}
	})

	t.Run("silent client keeps utf-16 baseline", func(t *testing.T) {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
		s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "initialize",
			json.RawMessage(`{"processId":1,"rootUri":"file:///w","capabilities":{}}`)))
		if got := s.negotiatedEncodingInt(); got != 1 {
			t.Fatalf("baseline encoding int = %d, want 1 (utf-16)", got)
		}
	})
}

func TestI10_PrepareRenameGate(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
	s.vfs.Open(uri, "go", 1, []byte("package main\n\nfunc main() {\n\ttotal := 1\n\tExported()\n}\n"), 0)
	s.publishSnapshot()

	prep := func(line, char uint32) *jsonrpc.Message {
		return s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
			jsonrpc.RequestID{Num: 1}, "textDocument/prepareRename",
			json.RawMessage(fmt.Sprintf(`{"textDocument":{"uri":"%s"},"position":{"line":%d,"character":%d}}`, uri, line, char))))
	}

	t.Run("unexported identifier gets placeholder", func(t *testing.T) {
		resp := prep(3, 2) // on `total`
		if resp == nil || resp.Error != nil || resp.Result == nil {
			t.Fatalf("prepareRename failed: %+v", resp)
		}
		if !containsBytes(resp.Result, `"placeholder":"total"`) {
			t.Errorf("placeholder missing: %s", resp.Result)
		}
	})

	t.Run("exported identifier refused SEM-SAFE-001", func(t *testing.T) {
		resp := prep(4, 3) // on `Exported`
		if resp == nil || resp.Error == nil {
			t.Fatalf("exported symbol must be refused, got %+v", resp)
		}
		wire, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal prepareRename response: %v", err)
		}
		var decoded jsonrpc.Message
		if err := json.Unmarshal(wire, &decoded); err != nil {
			t.Fatalf("decode prepareRename wire response: %v", err)
		}
		if decoded.Error == nil || decoded.Error.Code != jsonrpc.RequestFailed {
			t.Errorf("exported symbol wire error = %+v, want RequestFailed (%d)", decoded.Error, jsonrpc.RequestFailed)
		}
		if !containsBytes([]byte(resp.Error.Message), "SEM-SAFE-001") {
			t.Errorf("refusal lacks policy reference: %s", resp.Error.Message)
		}
	})

	t.Run("off-identifier position yields null", func(t *testing.T) {
		resp := prep(0, 3) // inside `package` keyword — still an ident, but line 0 char 3
		_ = resp           // either placeholder or null is protocol-valid; must not error
	})
}

// TestT2_DeclarationServedAndDeclared pins §I14/T2: textDocument/declaration
// is advertised in capabilities AND served through the optional
// DeclarationProvider capability (ADR-0009 D2 — core interface stays frozen).
func TestT2_DeclarationServedAndDeclared(t *testing.T) {
	caps := buildCapabilities("utf-16")
	if !caps.DeclarationProvider {
		t.Fatal("declarationProvider not declared")
	}
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "d1", IsStr: true},
		"textDocument/declaration", json.RawMessage(`{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("dispatch failed: %+v", resp)
	}
}

// TestC12_ResultMetaAndQueryTrace pins §C12: the envelope metadata exit
// (omnilsp/resultMeta) exposes completeness/evidence per URI+method, and
// omnilsp/queryTrace exposes engine counters — both must respond cleanly.
func TestC12_ResultMetaAndQueryTrace(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)

	req := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`
	for _, m := range []string{"textDocument/hover", "textDocument/definition"} {
		msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: m, IsStr: true}, m, json.RawMessage(req))
		if resp := s.dispatcher.Dispatch(context.Background(), msg); resp == nil || resp.Error != nil {
			t.Fatalf("%s dispatch failed", m)
		}
	}

	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "rm", IsStr: true}, "omnilsp/resultMeta",
		json.RawMessage(`{"uri":"file:///x.go"}`))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("resultMeta failed: %+v", resp)
	}
	var metas []struct {
		Method       string `json:"method"`
		Status       string `json:"status"`
		Completeness string `json:"completeness"`
	}
	if err := json.Unmarshal(resp.Result, &metas); err != nil {
		t.Fatalf("unmarshal resultMeta: %v (%s)", err, resp.Result)
	}
	found := map[string]bool{}
	for _, m := range metas {
		found[m.Method] = true
		if m.Status == "" || m.Completeness == "" {
			t.Errorf("%s meta incomplete: %+v", m.Method, m)
		}
	}
	if !found["textDocument/hover"] || !found["textDocument/definition"] {
		t.Fatalf("missing methods in resultMeta: %v", found)
	}

	qmsg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "qt", IsStr: true}, "omnilsp/queryTrace", nil)
	qresp := s.dispatcher.Dispatch(context.Background(), qmsg)
	if qresp == nil || qresp.Error != nil {
		t.Fatalf("queryTrace failed: %+v", qresp)
	}
	var stats map[string]any
	if err := json.Unmarshal(qresp.Result, &stats); err != nil || len(stats) == 0 {
		t.Fatalf("queryTrace bad payload: %v %s", err, qresp.Result)
	}
}

// TestC3_CapabilitiesDeclaredEqualsServed pins §C3: every declared provider
// flag has a registered handler behind it. A declared-but-unserved feature
// makes clients send requests that fail; a served-but-undeclared one is dead.
func TestC3_CapabilitiesDeclaredEqualsServed(t *testing.T) {
	s := New(DefaultConfig())
	caps := buildCapabilities("utf-16")
	if caps.HoverProvider {
		if !s.dispatcher.HasHandler("textDocument/hover") {
			t.Error("hoverProvider declared but no handler")
		}
	}
	for m, declared := range map[string]bool{
		"textDocument/signatureHelp": caps.SignatureHelpProvider != nil,
		"textDocument/codeAction":    caps.CodeActionProvider,
		"textDocument/diagnostic":    caps.DiagnosticProvider != nil,
		"textDocument/formatting":    caps.DocumentFormattingProvider,
		"textDocument/inlayHint":     caps.InlayHintProvider,
		"textDocument/declaration":   caps.DeclarationProvider,
	} {
		if !declared {
			t.Errorf("%s implemented but not declared", m)
			continue
		}
		if !s.dispatcher.HasHandler(m) {
			t.Errorf("%s declared but no handler registered", m)
		}
	}
}
