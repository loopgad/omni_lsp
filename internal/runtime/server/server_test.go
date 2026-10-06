package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/scheduler"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// mockBackend implements languages.Backend for testing.
type mockBackend struct {
	langID      string
	exts        []string
	hoverResult *languages.HoverResult
	compResult  []languages.CompletionItem
	defResult   []languages.Location
	symResult   []languages.DocumentSymbol
	renResult   languages.ValidatedEdit
	semtResult  []languages.SemanticToken
	wsymResult  []languages.WorkspaceSymbol
	closeErr    error
}

func (m *mockBackend) LanguageID() string { return m.langID }

// The fixture backend reads only immutable request content, never disk.
func (m *mockBackend) SemanticInputFingerprint(context.Context, *snapshot.Snapshot, string) (string, error) {
	return "immutable-request-fixture", nil
}
func (m *mockBackend) FileExtensions() []string { return m.exts }
func (m *mockBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact, Value: m.hoverResult,
	}, nil
}
func (m *mockBackend) Completion(_ context.Context, _ languages.CompletionRequest) ([]languages.CompletionItem, error) {
	return m.compResult, nil
}

// Declaration opts the mock into the optional capability so declaration
// tests exercise the served path, not just the clean refusal.
func (m *mockBackend) Declaration(ctx context.Context, req languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{Status: identity.ResultExact}, nil
}

func (m *mockBackend) Definition(_ context.Context, _ languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{
		Status: identity.ResultExact, Value: m.defResult,
	}, nil
}
func (m *mockBackend) References(_ context.Context, _ languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{
		Status: identity.ResultExact,
	}, nil
}
func (m *mockBackend) DocumentSymbols(_ context.Context, _ languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	return m.symResult, nil
}
func (m *mockBackend) WorkspaceSymbols(_ context.Context, _ languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	return m.wsymResult, nil
}
func (m *mockBackend) Diagnostics(_ context.Context, _ string, _ []byte) ([]languages.Diagnostic, error) {
	return nil, nil
}
func (m *mockBackend) SemanticTokens(_ context.Context, _ string, _ []byte) ([]languages.SemanticToken, error) {
	return m.semtResult, nil
}
func (m *mockBackend) Rename(_ context.Context, _ languages.RenameRequest) (identity.SemanticResult[languages.ValidatedEdit], error) {
	return identity.SemanticResult[languages.ValidatedEdit]{Status: identity.ResultExact, Value: m.renResult}, nil
}
func (m *mockBackend) Close() error { return m.closeErr }

type didCloseTrackingBackend struct {
	mockBackend
	server   *Server
	called   bool
	uri      string
	revision uint64
	wasOpen  bool
}

func (b *didCloseTrackingBackend) DidCloseDocument(uri string, revision uint64) error {
	b.called = true
	b.uri = uri
	b.revision = revision
	b.wasOpen = b.server.vfs.Get(uri) != nil
	return nil
}

type didSaveTrackingBackend struct {
	mockBackend
	server               *Server
	called               bool
	uri                  string
	content              string
	revision             uint64
	wasDirty             bool
	snapshotRevisionSeen uint64
}

func (b *didSaveTrackingBackend) DidSaveDocument(uri string, content []byte, revision uint64) error {
	b.called = true
	b.uri = uri
	b.content = string(content)
	b.revision = revision
	if file := b.server.vfs.Get(uri); file != nil {
		b.wasDirty = file.Dirty
	}
	if snapshot := b.server.snapMgr.Current(); snapshot != nil {
		b.snapshotRevisionSeen = uint64(snapshot.ID().Revision)
	}
	return nil
}

func TestNewServer(t *testing.T) {
	s := New(DefaultConfig())
	if s == nil {
		t.Fatal("New returned nil")
	}
	if s.State() != StateUninitialized {
		t.Errorf("State = %d, want StateUninitialized", s.State())
	}
	if s.scheduler == nil {
		t.Error("scheduler is nil")
	}
	if s.snapMgr == nil {
		t.Error("snapMgr is nil")
	}
	if s.vfs == nil {
		t.Error("vfs is nil")
	}
}

func TestRegisterBackend(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}}
	s.RegisterBackend("go", be)
	if len(s.languages) != 1 {
		t.Errorf("languages len = %d, want 1", len(s.languages))
	}
	if s.languages["go"] != be {
		t.Error("backend not registered")
	}
}

func TestClassifyPriority(t *testing.T) {
	s := New(DefaultConfig())
	cases := []struct {
		method string
		prio   int
	}{
		{"textDocument/hover", int(scheduler.PriorityHover)},
		{"textDocument/completion", int(scheduler.PriorityCompletion)},
		{"textDocument/definition", int(scheduler.PriorityDefinition)},
		{"textDocument/references", int(scheduler.PriorityReferences)},
		{"textDocument/rename", int(scheduler.PriorityReferences)},
		{"textDocument/diagnostic", int(scheduler.PriorityDiagnostics)},
		{"textDocument/codeAction", int(scheduler.PriorityDiagnostics)},
		{"textDocument/semanticTokens/full", int(scheduler.PrioritySemanticTokens)},
		{"$/progress", int(scheduler.PriorityMaintenance)},
		{"unknown/method", int(scheduler.PriorityMaintenance)},
	}
	for _, c := range cases {
		msg := &jsonrpc.Message{Method: c.method}
		got := s.classifyPriority(msg)
		if got != scheduler.Priority(c.prio) {
			t.Errorf("method=%s priority=%d, want %d", c.method, got, c.prio)
		}
	}
}

func TestHandleInitialize(t *testing.T) {
	s := New(DefaultConfig())
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "init-1", IsStr: true}, "initialize",
		json.RawMessage(`{"processId":123,"rootUri":"file:///tmp/ws"}`))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected success, got err=%v", resp.Error)
	}
	var result InitializeResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !result.Capabilities.HoverProvider {
		t.Error("HoverProvider should be true")
	}
	if !result.Capabilities.DefinitionProvider {
		t.Error("DefinitionProvider should be true")
	}
	if got := result.Capabilities.DiagnosticProvider; got == nil {
		t.Error("diagnosticProvider options should be present")
	} else {
		if !got.InterFileDependencies {
			t.Error("diagnosticProvider.interFileDependencies should be true for workspace-aware backends")
		}
		if got.WorkspaceDiagnostics {
			t.Error("diagnosticProvider.workspaceDiagnostics should be false without workspace/diagnostic handler")
		}
	}
	if result.Capabilities.TextDocumentSync == nil || !result.Capabilities.TextDocumentSync.OpenClose {
		t.Error("OpenClose should be true")
	}
}

func TestHandleDidOpen(t *testing.T) {
	s := New(DefaultConfig())
	params := `{"textDocument":{"uri":"file:///tmp/main.go","languageId":"go","version":1,"text":"package main\n\nfunc foo() {}\n"}}`
	msg := jsonrpc.NewNotification("textDocument/didOpen", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp != nil {
		t.Fatalf("expected nil response for notification")
	}
	f := s.vfs.Get("file:///tmp/main.go")
	if f == nil {
		t.Fatal("file not in VFS after didOpen")
	}
	if f.LanguageID != "go" {
		t.Errorf("LanguageID = %q, want go", f.LanguageID)
	}
}

func TestHandleDidChange(t *testing.T) {
	s := New(DefaultConfig())
	s.vfs.Open("file:///tmp/main.go", "go", 1, []byte("package main\n"), vfs.SourceDisk)
	s.publishSnapshot()
	params := `{"textDocument":{"uri":"file:///tmp/main.go","version":2},"contentChanges":[{"text":"package main\n\nfunc foo() {}\n"}]}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "dc", IsStr: true}, "textDocument/didChange", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected success")
	}
	f := s.vfs.Get("file:///tmp/main.go")
	if f == nil {
		t.Fatal("file missing from VFS")
	}
	if f.Version != 2 {
		t.Errorf("version = %d, want 2", f.Version)
	}
	if !f.Dirty {
		t.Error("file should be dirty after change")
	}
}

func TestHandleDidClose(t *testing.T) {
	s := New(DefaultConfig())
	const uri = "file:///tmp/a.go"
	be := &didCloseTrackingBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}, server: s}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceDisk)
	s.publishSnapshot()
	closeRevision := s.vfs.Revision() + 1
	params := `{"textDocument":{"uri":"` + uri + `"}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "dc", IsStr: true}, "textDocument/didClose", json.RawMessage(params))
	if _, err := s.handleDidClose(context.Background(), msg); err != nil {
		t.Fatalf("handleDidClose returned error: %v", err)
	}
	if s.vfs.Get(uri) != nil {
		t.Error("file should be removed from VFS after close")
	}
	if !be.called || be.uri != uri || be.revision != closeRevision || !be.wasOpen {
		t.Fatalf("optional backend close hook = called:%v uri:%q rev:%d open:%v; want URI/post-close revision and open state", be.called, be.uri, be.revision, be.wasOpen)
	}
	if got := s.snapMgr.Current().ID().Revision; got != closeRevision {
		t.Fatalf("published close revision = %d, want backend barrier revision %d", got, closeRevision)
	}
}

func TestHandleDidSave(t *testing.T) {
	s := New(DefaultConfig())
	defer s.diag.Close()
	const uri = "file:///tmp/a.go"
	const content = "package main\n"
	be := &didSaveTrackingBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		server:      s,
	}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte(content), vfs.SourceEditor)
	s.publishSnapshot()
	params := `{"textDocument":{"uri":"` + uri + `"}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "ds", IsStr: true}, "textDocument/didSave", json.RawMessage(params))
	s.dispatcher.Dispatch(context.Background(), msg)
	f := s.vfs.Get(uri)
	if f == nil {
		t.Fatal("file missing after save")
	}
	if f.Dirty {
		t.Error("file should not be dirty after save")
	}
	wantRevision := s.vfs.Revision()
	if !be.called || be.uri != uri || be.content != content || be.revision != wantRevision || be.wasDirty || be.snapshotRevisionSeen != wantRevision {
		t.Fatalf("save hook = called:%v uri:%q content:%q revision:%d dirty:%v snapshot revision:%d; want saved URI/content and published clean snapshot at revision %d", be.called, be.uri, be.content, be.revision, be.wasDirty, be.snapshotRevisionSeen, wantRevision)
	}
}

func TestResolveBackend(t *testing.T) {
	s := New(DefaultConfig())
	s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
	s.RegisterBackend("ts", &mockBackend{langID: "ts", exts: []string{".ts"}})
	cases := []struct {
		uri, wantID string
		wantErr     bool
	}{
		{"file:///foo/bar.go", "go", false},
		{"file:///foo/bar.ts", "ts", false},
		{"file:///foo/bar.py", "", true},
	}
	for _, c := range cases {
		be, err := s.resolveBackend(c.uri)
		if c.wantErr {
			if err == nil {
				t.Errorf("uri=%s want error, got nil", c.uri)
			}
			continue
		}
		if err != nil {
			t.Errorf("uri=%s unexpected error: %v", c.uri, err)
			continue
		}
		if be == nil || be.LanguageID() != c.wantID {
			t.Errorf("uri=%s got=%v want=%s", c.uri, be, c.wantID)
		}
	}
}

func TestDispatchSemanticRequestNoBackend(t *testing.T) {
	s := New(DefaultConfig())
	_, err := s.dispatchSemanticRequest(context.Background(), nil, "file:///foo.py", 0, 0, func(_ context.Context, be languages.Backend, src []byte, _ uint64, _ identity.BuildContextID) (json.RawMessage, error) {
		return nil, nil
	})
	if err == nil {
		t.Error("expected error for unknown URI")
	}
	if !errors.IsKind(err, errors.ErrBackendUnavailable) {
		t.Errorf("error kind mismatch")
	}
}

func TestHandleHover(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}, hoverResult: &languages.HoverResult{Contents: "func foo() int", Evidence: languages.EvidenceL3}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":1,"character":5}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "hv", IsStr: true}, "textDocument/hover", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected success, got err=%v", resp.Error)
	}
	var hover struct {
		Contents struct{ Value string } `json:"contents"`
	}
	if err := json.Unmarshal(resp.Result, &hover); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if hover.Contents.Value != "func foo() int" {
		t.Errorf("hover contents = %q, want %q", hover.Contents.Value, "func foo() int")
	}
}

func TestHandleCompletion(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}, compResult: []languages.CompletionItem{{Label: "foo", Kind: int(languages.CompletionFunction), Detail: "func foo() int", Evidence: languages.EvidenceL3}}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "cp", IsStr: true}, "textDocument/completion", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected success")
	}
	var list struct {
		Items []struct {
			Label string `json:"label"`
		} `json:"items"`
	}
	if err := json.Unmarshal(resp.Result, &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(list.Items) != 1 || list.Items[0].Label != "foo" {
		t.Errorf("items = %v, want [{foo}]", list.Items)
	}
}

func TestHandleDefinition(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}, defResult: []languages.Location{{URI: "file:///x.go", Range: languages.Range{StartLine: 0, StartCharacter: 5, EndLine: 0, EndCharacter: 8}}}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "df", IsStr: true}, "textDocument/definition", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected success")
	}
	var locs []struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(resp.Result, &locs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(locs) != 1 || locs[0].URI != "file:///x.go" {
		t.Errorf("locations = %v, want [{file:///x.go}]", locs)
	}
}

func TestHandleRename(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}, renResult: languages.ValidatedEdit{
		Complete: true,
		Edits:    []languages.TextEdit{{URI: "file:///x.go", StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 3, NewText: "bar"}},
	}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0},"newName":"bar"}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "rn", IsStr: true}, "textDocument/rename", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected success")
	}
	var edit struct {
		Changes map[string][]struct {
			NewText string `json:"newText"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(resp.Result, &edit); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(edit.Changes) != 1 {
		t.Errorf("changes len = %d, want 1", len(edit.Changes))
	}
}

func TestHandleRenameRejectsInvalidWorkspaceEdit(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}, renResult: languages.ValidatedEdit{
		Complete: true,
		Edits: []languages.TextEdit{
			{URI: "file:///x.go", StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 4, NewText: "first"},
			{URI: "file:///x.go", StartLine: 0, StartChar: 3, EndLine: 0, EndChar: 5, NewText: "second"},
		},
	}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	resp := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Str: "rn-invalid", IsStr: true}, "textDocument/rename",
		json.RawMessage(`{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0},"newName":"bar"}`)))
	if resp == nil || resp.Error == nil {
		t.Fatalf("overlapping WorkspaceEdit was not rejected: %+v", resp)
	}
	if resp.Error.Code != jsonrpc.RequestFailed || !contains(resp.Error.Message, "WorkspaceEdit validation") {
		t.Fatalf("invalid WorkspaceEdit refusal = %+v", resp.Error)
	}
}

func TestHandleDocumentSymbol(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}, symResult: []languages.DocumentSymbol{{Name: "foo", Kind: languages.SymbolFunction, StartLine: 0, StartCharacter: 0, EndLine: 0, EndCharacter: 12, SelectionLine: 0, SelectionCharacter: 5, SelectionEndLine: 0, SelectionEndCharacter: 7, SelectionRangeSet: true}}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///x.go"}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "ds", IsStr: true}, "textDocument/documentSymbol", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected success")
	}
	var syms []struct {
		Name           string `json:"name"`
		SelectionRange struct {
			Start struct{ Line, Character uint32 } `json:"start"`
			End   struct{ Line, Character uint32 } `json:"end"`
		} `json:"selectionRange"`
	}
	if err := json.Unmarshal(resp.Result, &syms); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(syms) != 1 || syms[0].Name != "foo" {
		t.Errorf("symbols = %v, want [{foo}]", syms)
	}
	if len(syms) == 1 && (syms[0].SelectionRange.Start.Character != 5 || syms[0].SelectionRange.End.Character != 7) {
		t.Errorf("selection range = %+v, want exact stored end 0:7", syms[0].SelectionRange)
	}
}

func TestHandleWorkspaceSymbol(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}, wsymResult: []languages.WorkspaceSymbol{{Name: "Bar", Kind: languages.SymbolFunction, URI: "file:///x.go", StartLine: 2, StartCol: 5}}}
	s.RegisterBackend("go", be)
	params := `{"query":"bar"}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "ws", IsStr: true}, "workspace/symbol", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected success")
	}
	var syms []struct {
		Name     string `json:"name"`
		Location struct {
			URI string `json:"uri"`
		} `json:"location"`
	}
	if err := json.Unmarshal(resp.Result, &syms); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(syms) != 1 || syms[0].Name != "Bar" {
		t.Errorf("workspace symbols = %v, want [{Bar}]", syms)
	}
}

func TestHandleSemanticTokens(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}, semtResult: []languages.SemanticToken{{DeltaLine: 0, DeltaStart: 5, Length: 3, TokenType: uint32(languages.TokFunction)}}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///x.go"}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "st", IsStr: true}, "textDocument/semanticTokens/full", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected success")
	}
	// §I21 wire shape lock: strict decode — the response MUST be
	// {"data":[uint x5 per token]} and nothing else. json.Unmarshal is
	// case-insensitive and would silently accept the old object-array bug.
	if !json.Valid(resp.Result) {
		t.Fatalf("invalid json: %s", resp.Result)
	}
	var wire struct {
		Data []int `json:"data"`
	}
	if err := json.Unmarshal(resp.Result, &wire); err != nil {
		t.Fatalf("flat data array expected, got %s: %v", resp.Result, err)
	}
	if string(resp.Result[0]) != `{` {
		t.Fatalf("response must be a single object with only \"data\", got %s", resp.Result)
	}
	want := []int{0, 5, 3, int(languages.TokFunction), 0}
	if len(wire.Data) != len(want) {
		t.Fatalf("data = %v, want %v", wire.Data, want)
	}
	for i := range want {
		if wire.Data[i] != want[i] {
			t.Fatalf("data[%d] = %d, want %d", i, wire.Data[i], want[i])
		}
	}
}

// TestI21_LegendMatchesEmittedTypes pins §I21: the legend advertised in
// capabilities must interpret the exact TokenType values backends emit —
// a mismatch colors every token as the wrong kind client-side.
func TestI21_LegendMatchesEmittedTypes(t *testing.T) {
	n := len(languages.SemanticTokenTypes)
	if n != int(languages.TokComment)+1 {
		t.Fatalf("legend has %d entries but emitter enum tops at %d", n, languages.TokComment)
	}
	caps := buildCapabilities("utf-16")
	st := caps.SemanticTokensProvider
	if st == nil {
		t.Fatal("semanticTokensProvider not declared")
	}
	if st.Incremental {
		t.Error("Incremental declared without a delta handler (§C3 honesty)")
	}
	if len(st.Legend.TokenTypes) != n {
		t.Fatalf("capabilities legend %d != canonical %d", len(st.Legend.TokenTypes), n)
	}
}

func TestHandleShutdown(t *testing.T) {
	s := New(DefaultConfig())
	// C2: shutdown requires the initialize handshake first.
	initMsg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "i", IsStr: true}, "initialize",
		json.RawMessage(`{"processId":1,"rootUri":"file:///tmp/ws"}`))
	if resp := s.dispatcher.Dispatch(context.Background(), initMsg); resp == nil || resp.Error != nil {
		t.Fatalf("initialize failed: %v", resp.Error)
	}
	s.dispatcher.Dispatch(context.Background(), jsonrpc.NewNotification("initialized", nil))
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "sh", IsStr: true}, "shutdown", json.RawMessage("{}"))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil {
		t.Fatal("expected response")
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
	if s.State() != StateShuttingDown {
		t.Errorf("State = %s, want shutting_down", s.State())
	}
}

func TestHandleInitialized(t *testing.T) {
	s := New(DefaultConfig())
	msg := jsonrpc.NewNotification("initialized", nil)
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp != nil {
		t.Errorf("notification should return nil, got %+v", resp)
	}
}

func TestHandleExit(t *testing.T) {
	s := New(DefaultConfig())
	msg := jsonrpc.NewNotification("exit", nil)
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp != nil {
		t.Errorf("notification should return nil, got %+v", resp)
	}
}

func TestUnknownMethod(t *testing.T) {
	s := New(DefaultConfig())
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "unknown", IsStr: true}, "nonexistent/method", json.RawMessage("{}"))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil || resp.Error == nil {
		t.Fatal("expected error response for unknown method")
	}
	if resp.Error.Code != jsonrpc.MethodNotFound {
		t.Errorf("error code = %d, want MethodNotFound", resp.Error.Code)
	}
}

func TestPublishSnapshot(t *testing.T) {
	s := New(DefaultConfig())
	s.workspaceID = identity.WorkspaceID("ws1")
	s.vfs.Open("file:///a.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	snap := s.snapMgr.Current()
	if snap == nil {
		t.Fatal("snapshot not published")
	}
	if snap.ID().Revision != 1 {
		t.Errorf("revision = %d, want 1", snap.ID().Revision)
	}
}

func TestWorkspaceSymbolNilBackend(t *testing.T) {
	s := New(DefaultConfig())
	params := `{"query":"foo"}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "ws", IsStr: true}, "workspace/symbol", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil {
		t.Fatal("expected response")
	}
}

func TestCaptureSnapshot(t *testing.T) {
	s := New(DefaultConfig())
	s.workspaceID = identity.WorkspaceID("ws1")
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	snap := s.snapMgr.Current()
	if snap == nil {
		t.Fatal("snapshot not published")
	}
	if snap.ID().WorkspaceID != "ws1" {
		t.Errorf("workspace = %q, want ws1", snap.ID().WorkspaceID)
	}
}

func TestLastChangeWinsCoalescing(t *testing.T) {
	// F12: last-change-wins coalescing for didChange.
	s := New(DefaultConfig())
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceDisk)
	s.publishSnapshot()
	params := `{"textDocument":{"uri":"file:///x.go","version":5},"contentChanges":[{"text":"first\n"},{"text":"second\n"},{"text":"final\n"}]}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "dc", IsStr: true}, "textDocument/didChange", json.RawMessage(params))
	s.dispatcher.Dispatch(context.Background(), msg)
	f := s.vfs.Get("file:///x.go")
	if f == nil {
		t.Fatal("file missing")
	}
	want := "final\n"
	if string(f.Content) != want {
		t.Errorf("content = %q, want %q (last-change-wins)", string(f.Content), want)
	}
}

func TestDidChangeNoChanges(t *testing.T) {
	s := New(DefaultConfig())
	params := `{"textDocument":{"uri":"file:///x.go","version":1},"contentChanges":[]}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "dc", IsStr: true}, "textDocument/didChange", json.RawMessage(params))
	_ = s.dispatcher.Dispatch(context.Background(), msg)
}

func TestHoverNullResult(t *testing.T) {
	// SEM-003: null result should not panic.
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "hv", IsStr: true}, "textDocument/hover", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	// dispatch returns *Message, no error
	if resp == nil {
		t.Fatal("expected response")
	}
}

func TestRenameNoEdits(t *testing.T) {
	// SEM-SAFE-001: a proven-complete but empty edit set projects as null.
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}, renResult: languages.ValidatedEdit{Complete: true}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0},"newName":"bar"}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "rn", IsStr: true}, "textDocument/rename", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("expected success, got err=%v", resp.Error)
	}
}

// TestSEM_SAFE_001_RenameFailsClosed verifies that an unproven rename is
// refused with a typed error instead of being projected as a WorkspaceEdit.
func TestSEM_SAFE_001_RenameFailsClosed(t *testing.T) {
	s := New(DefaultConfig())
	// Backend returns Exact status but Complete=false: no proof, no publish.
	be := &mockBackend{langID: "go", exts: []string{".go"}, renResult: languages.ValidatedEdit{
		Complete: false,
		Edits:    []languages.TextEdit{{URI: "file:///x.go", NewText: "evil"}},
	}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0},"newName":"bar"}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "rn", IsStr: true}, "textDocument/rename", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	if resp == nil {
		t.Fatal("expected error response")
	}
	if resp.Error == nil {
		t.Fatal("unproven rename must be refused, got success")
	}
	if resp.Error.Code != jsonrpc.RequestFailed {
		t.Errorf("error code = %d, want RequestFailed", resp.Error.Code)
	}
}

func TestDispatchSemanticRequestNilSourceFallback(t *testing.T) {
	s := New(DefaultConfig())
	be := &mockBackend{langID: "go", exts: []string{".go"}}
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	s.vfs.Close("file:///x.go")
	called := false
	_, _ = s.dispatchSemanticRequest(context.Background(), nil, "file:///x.go", 0, 0,
		func(_ context.Context, be languages.Backend, src []byte, _ uint64, _ identity.BuildContextID) (json.RawMessage, error) {
			called = true
			return json.RawMessage(`{"ok":true}`), nil
		})
	if !called {
		t.Error("project function was not called")
	}
}

func TestPriorityOrdering(t *testing.T) {
	// F3: verify priority ordering is monotonic (higher = more urgent).
	s := New(DefaultConfig())
	prios := []string{
		"textDocument/completion",
		"textDocument/hover",
		"textDocument/definition",
		"textDocument/references",
		"textDocument/diagnostic",
		"textDocument/semanticTokens/full",
		"/progress",
	}
	prev := scheduler.Priority(0)
	for _, method := range prios {
		got := s.classifyPriority(&jsonrpc.Message{Method: method})
		if got < prev {
			t.Errorf("priority for %s (%d) < previous (%d)", method, got, prev)
		}
		prev = got
	}
}

func TestHandleDidOpenInvalidParams(t *testing.T) {
	s := New(DefaultConfig())
	msg := jsonrpc.NewNotification("textDocument/didOpen", json.RawMessage("not json"))
	_ = s.dispatcher.Dispatch(context.Background(), msg)
	// Dispatcher does not return error for invalid notification params
}

func TestServerStateUninitialized(t *testing.T) {
	s := New(DefaultConfig())
	if s.State() != StateUninitialized {
		t.Errorf("State = %d, want StateUninitialized", s.State())
	}
}

func BenchmarkResolveBackend(b *testing.B) {
	s := New(DefaultConfig())
	s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
	s.RegisterBackend("ts", &mockBackend{langID: "ts", exts: []string{".ts"}})
	uri := "file:///project/main.go"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.resolveBackend(uri)
	}
}
