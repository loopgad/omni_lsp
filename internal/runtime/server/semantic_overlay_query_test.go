package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/languages"
	golang "github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

func TestGoSnapshotSemanticLocationsUsesDirtySnapshotAndNegotiatedEncoding(t *testing.T) {
	setGoOverlayTestEnvironment(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module overlay.query.test\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diskSource := []byte("package overlaytest\n\nfunc target() {}\n\nfunc use() { target() }\n")
	openSource := []byte("// 😀 unsaved line\npackage overlaytest\n\nfunc renamed() {}\n\nfunc use() { _ = \"😀\"; renamed() }\n")
	filePath := filepath.Join(root, "main.go")
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	s := newGoOverlayQueryTestServer(root, rootURI, fileURI, openSource, golang.New(root))
	captured := s.snapMgr.Current()
	ctx := withSnapshot(context.Background(), captured)

	for _, encoding := range []position.Encoding{position.UTF8, position.UTF16, position.UTF32} {
		queryOffset := strings.LastIndex(string(openSource), "renamed")
		line, character := testPositionAtOffset(t, openSource, queryOffset, encoding)
		result, used := s.goSnapshotSemanticLocations(ctx, fileURI, line, character, int(encoding), captured.ID().Revision, persistentReferences, true)
		if !used {
			t.Fatalf("overlay references were not used with %s positions", encoding)
		}
		if result.Status != identity.ResultExact || result.Completeness != identity.Complete || len(result.Value) != 2 {
			t.Fatalf("overlay result with %s = %#v", encoding, result)
		}
		if len(result.Evidence) != 1 || result.Evidence[0].Kind != identity.EvidenceCompiler ||
			result.Evidence[0].Assurance != identity.AssuranceCompilerResolved || result.Evidence[0].IndexGen != 0 ||
			result.Evidence[0].SourceHash != semanticOverlayContentHash(openSource) {
			t.Fatalf("overlay evidence with %s = %#v", encoding, result.Evidence)
		}
		wantOffsets := []int{strings.Index(string(openSource), "renamed"), strings.LastIndex(string(openSource), "renamed")}
		for i, offset := range wantOffsets {
			wantLine, wantStart := testPositionAtOffset(t, openSource, offset, encoding)
			_, wantEnd := testPositionAtOffset(t, openSource, offset+len("renamed"), encoding)
			got := result.Value[i]
			if got.URI != fileURI || got.Range.StartLine != wantLine || got.Range.StartCharacter != wantStart ||
				got.Range.EndLine != wantLine || got.Range.EndCharacter != wantEnd {
				t.Fatalf("location %d with %s = %#v, want %d:%d-%d:%d", i, encoding, got, wantLine, wantStart, wantLine, wantEnd)
			}
		}
	}
	if got := overlaySymbolIDFromFactsCache(s.overlayFacts, "target"); got {
		t.Fatal("query cache retained a definition renamed away in the captured editor buffer")
	}
}

func TestGoSnapshotSemanticLocationsFallsBackForIncompleteScopeAndStaleSnapshot(t *testing.T) {
	setGoOverlayTestEnvironment(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module overlay.incomplete.test\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := []byte("package overlaytest\n\nfunc target() {}\n\nfunc use() { target() }\n")
	filePath := filepath.Join(root, "main.go")
	if err := os.WriteFile(filePath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	backend := &incompleteGoOverlayBackend{Backend: golang.New(root)}
	s := newGoOverlayQueryTestServer(root, rootURI, fileURI, source, backend)
	captured := s.snapMgr.Current()
	ctx := withSnapshot(context.Background(), captured)
	line, character := testPositionAtOffset(t, source, strings.Index(string(source), "target()"), position.UTF16)
	if _, used := s.goSnapshotSemanticLocations(ctx, fileURI, line, character, int(position.UTF16), captured.ID().Revision, persistentDefinition, false); used {
		t.Fatal("query served results from a scope with incomplete definition coverage")
	}

	s.vfs.Update(fileURI, 2, []byte("package overlaytest\n\nfunc newer() {}\n"))
	if _, used := s.goSnapshotSemanticLocations(ctx, fileURI, line, character, int(position.UTF16), captured.ID().Revision, persistentDefinition, false); used {
		t.Fatal("query served facts after the captured snapshot became stale")
	}
}

func TestHandleDefinitionUsesDirtyGoOverlayBeforeLiveBackend(t *testing.T) {
	setGoOverlayTestEnvironment(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module overlay.dispatch.test\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diskSource := []byte("package overlaytest\n\nfunc target() {}\n\nfunc use() { target() }\n")
	openSource := []byte("// unsaved line\npackage overlaytest\n\nfunc renamed() {}\n\nfunc use() { renamed() }\n")
	filePath := filepath.Join(root, "main.go")
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	backend := &countingGoOverlayBackend{Backend: golang.New(root)}
	s := newGoOverlayQueryTestServer(root, rootURI, fileURI, openSource, backend)
	line, character := testPositionAtOffset(t, openSource, strings.LastIndex(string(openSource), "renamed"), position.UTF16)
	params := fmt.Sprintf(`{"textDocument":{"uri":%q},"position":{"line":%d,"character":%d}}`, fileURI, line, character)
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "dirty-definition", IsStr: true}, "textDocument/definition", json.RawMessage(params))
	response := s.Dispatcher().Dispatch(context.Background(), msg)
	if response == nil || response.Error != nil {
		encoded, _ := json.Marshal(response)
		t.Fatalf("definition response = %s", encoded)
	}
	if got := backend.definitionCalls.Load(); got != 0 {
		t.Fatalf("live backend definition calls = %d, want 0 for a complete dirty snapshot overlay", got)
	}
	var locations []struct {
		URI   string `json:"uri"`
		Range struct {
			Start struct {
				Line      uint32 `json:"line"`
				Character uint32 `json:"character"`
			} `json:"start"`
		} `json:"range"`
	}
	if err := json.Unmarshal(response.Result, &locations); err != nil {
		t.Fatal(err)
	}
	if len(locations) != 1 || locations[0].URI != fileURI || locations[0].Range.Start.Line != 3 || locations[0].Range.Start.Character != 5 {
		t.Fatalf("dirty definition locations = %#v, want shifted renamed declaration at %s:3:5", locations, fileURI)
	}
}

func TestHandleWorkspaceSymbolsUsesDirtyGoOverlayAndKeepsOtherLanguages(t *testing.T) {
	setGoOverlayTestEnvironment(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module overlay.workspace.test\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diskSource := []byte("package overlaytest\n\nfunc target() {}\n\nfunc use() { target() }\n")
	openSource := []byte("// unsaved line\npackage overlaytest\n\nfunc /*😀*/ renamed() {}\n\nfunc use() { renamed() }\n")
	filePath := filepath.Join(root, "main.go")
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	goBackend := &countingGoOverlayBackend{Backend: golang.New(root)}
	s := newGoOverlayQueryTestServer(root, rootURI, fileURI, openSource, goBackend)
	s.positionEncoding = "utf-8"
	installEmptyPersistentWorkspaceStore(t, s)
	pythonSymbol := languages.WorkspaceSymbol{Name: "PythonKeep", URI: "file:///workspace.py", Kind: languages.SymbolFunction}
	pythonBackend := &workspaceCountingBackend{mockBackend: mockBackend{
		langID: "python", wsymResult: []languages.WorkspaceSymbol{pythonSymbol},
	}}
	s.RegisterBackend("python", pythonBackend)
	var evidence []identity.Evidence
	s.SetSemanticResponseObserver(func(_ jsonrpc.RequestID, values []identity.Evidence) { evidence = append(evidence, values...) })
	request := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "dirty-workspace-symbols", IsStr: true}, "workspace/symbol", json.RawMessage(`{"query":""}`))
	response := s.Dispatcher().Dispatch(context.Background(), request)
	if response == nil || response.Error != nil {
		if response == nil {
			t.Fatal("workspace symbol response is nil")
		}
		t.Fatalf("workspace symbol response error = %+v", *response.Error)
	}
	var symbols []struct {
		Name     string `json:"name"`
		Location struct {
			URI   string `json:"uri"`
			Range struct {
				Start struct {
					Character uint32 `json:"character"`
				} `json:"start"`
			} `json:"range"`
		} `json:"location"`
	}
	if err := json.Unmarshal(response.Result, &symbols); err != nil {
		t.Fatal(err)
	}
	var foundRenamed, foundOld, foundPython bool
	var renamedStart uint32
	for _, symbol := range symbols {
		switch symbol.Name {
		case "renamed":
			foundRenamed = symbol.Location.URI == fileURI
			renamedStart = symbol.Location.Range.Start.Character
		case "target":
			foundOld = true
		case "PythonKeep":
			foundPython = symbol.Location.URI == pythonSymbol.URI
		}
	}
	if !foundRenamed || foundOld || !foundPython {
		t.Fatalf("merged dirty workspace symbols: renamed=%v old=%v python=%v values=%+v", foundRenamed, foundOld, foundPython, symbols)
	}
	if renamedStart != 14 {
		t.Fatalf("UTF-8 workspace symbol start character = %d, want 14", renamedStart)
	}
	if got := goBackend.workspaceCalls.Load(); got != 0 {
		t.Fatalf("live Go workspace symbols calls = %d, want 0 for exact dirty overlay", got)
	}
	if pythonBackend.calls != 1 {
		t.Fatalf("non-Go backend calls = %d, want 1", pythonBackend.calls)
	}
	if len(evidence) == 0 {
		t.Fatal("dirty Go workspace result omitted compiler evidence")
	}
	for _, item := range evidence {
		if item.Kind == identity.EvidenceCompiler && item.IndexGen != 0 {
			t.Fatalf("dirty overlay evidence claims persistent generation: %+v", item)
		}
	}
}

func TestHandleWorkspaceSymbolsRejectsUnprovableDirtyGoOverlay(t *testing.T) {
	setGoOverlayTestEnvironment(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module overlay.workspace.incomplete.test\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diskSource := []byte("package overlaytest\n\nfunc target() {}\n")
	openSource := []byte("// unsaved line\npackage overlaytest\n\nfunc renamed() {}\n")
	filePath := filepath.Join(root, "main.go")
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	goBackend := &incompleteGoOverlayBackend{Backend: golang.New(root)}
	s := newGoOverlayQueryTestServer(root, rootURI, fileURI, openSource, goBackend)
	installEmptyPersistentWorkspaceStore(t, s)
	pythonBackend := &workspaceCountingBackend{mockBackend: mockBackend{langID: "python"}}
	s.RegisterBackend("python", pythonBackend)
	request := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "unprovable-workspace-symbols", IsStr: true}, "workspace/symbol", json.RawMessage(`{"query":""}`))
	response := s.Dispatcher().Dispatch(context.Background(), request)
	if response == nil || response.Error == nil || !strings.Contains(response.Error.Message, "current Go workspace symbols could not be verified") {
		if response == nil {
			t.Fatal("unprovable dirty Go workspace response is nil")
		}
		t.Fatalf("unprovable dirty Go workspace error = %+v, want content-modified error", response.Error)
	}
	if got := goBackend.workspaceCalls.Load(); got != 0 {
		t.Fatalf("live Go workspace symbols calls = %d, want 0 after dirty overlay proof failed", got)
	}
	if pythonBackend.calls != 1 {
		t.Fatalf("non-Go backend calls = %d, want 1 before rejecting Go overlay", pythonBackend.calls)
	}
}

func TestGoSnapshotSemanticLocationsRejectDiskChangeDuringExport(t *testing.T) {
	setGoOverlayTestEnvironment(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module overlay.closedchange.test\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diskSource := []byte("package overlaytest\n\nfunc target() {}\n\nfunc use() { target() }\n")
	openSource := []byte("// unsaved line\npackage overlaytest\n\nfunc renamed() {}\n\nfunc use() { renamed() }\n")
	filePath := filepath.Join(root, "main.go")
	dependencyPath := filepath.Join(root, "dependency.go")
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	originalDependency := []byte("package overlaytest\n\nvar untouched = 1\n")
	if err := os.WriteFile(dependencyPath, originalDependency, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	backend := &mutatingGoOverlayBackend{Backend: golang.New(root), mutate: func() error {
		return os.WriteFile(dependencyPath, []byte("package overlaytest\n\nvar untouched = 2\n"), 0o600)
	}}
	s := newGoOverlayQueryTestServer(root, rootURI, fileURI, openSource, backend)
	captured := s.snapMgr.Current()
	ctx := withSnapshot(context.Background(), captured)
	line, character := testPositionAtOffset(t, openSource, strings.LastIndex(string(openSource), "renamed"), position.UTF16)
	if _, used := s.goSnapshotSemanticLocations(ctx, fileURI, line, character, int(position.UTF16), captured.ID().Revision, persistentReferences, true); used {
		t.Fatal("dirty Go result survived a closed dependency disk change during export")
	}
	if got, err := os.ReadFile(dependencyPath); err != nil || bytes.Equal(got, originalDependency) {
		t.Fatalf("test dependency mutation did not happen: content=%q err=%v", got, err)
	}
}

func TestGoSnapshotSemanticFactsCacheCancellationCleansFlight(t *testing.T) {
	cache := newGoSnapshotSemanticFactsCache()
	key := goOverlayFactsKey{workspace: "workspace", diskDigest: "sha256:disk", overlayDigest: "sha256:overlay", snapshotInstance: 1, revision: 1, builder: 1, provider: 2}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := cache.getOrLoad(ctx, key, func() (*goSnapshotSemanticFacts, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}, func() bool { return true })
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cache load error = %v, want context canceled", err)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.flights) != 0 || len(cache.entries) != 0 || cache.used != 0 {
		t.Fatalf("cache retained canceled work: flights=%d entries=%d bytes=%d", len(cache.flights), len(cache.entries), cache.used)
	}
}

func TestGoSnapshotSemanticFactsCacheReplansSameCapabilities(t *testing.T) {
	setGoOverlayTestEnvironment(t)
	root := t.TempDir()
	filePath := filepath.Join(root, "main.go")
	diskSource := []byte("package overlaytest\n\nfunc target() {}\n")
	openSource := []byte("package overlaytest\n\nfunc renamed() {}\n")
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	toolPath := filepath.Join(t.TempDir(), "fake-go-tool")
	if err := os.WriteFile(toolPath, []byte("tool-v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	s := New(DefaultConfig())
	s.workspaceID = identity.WorkspaceID(rootURI)
	s.vfs.Open(fileURI, "go", 1, openSource, vfs.SourceEditor)
	revision := s.vfs.Revision()
	captured := snapshot.New(string(s.workspaceID), revision, map[string]snapshot.DocumentSnapshot{
		fileURI: {URI: fileURI, LanguageID: "go", Version: 1, Content: openSource},
	})
	s.snapMgr.Publish(captured)
	ctx := withSnapshot(context.Background(), captured)
	base, err := captureSemanticView(ctx, root, s.workspaceID, revision, "")
	if err != nil {
		t.Fatal(err)
	}
	view, err := captureGoSnapshotSemanticView(s, ctx, base)
	if err != nil {
		_ = base.Close()
		t.Fatal(err)
	}
	defer view.Close()
	backend := &mutableOverlayPlanBackend{toolPath: toolPath, planTag: "first"}
	cache := newGoSnapshotSemanticFactsCache()
	if _, err := cache.GetOrExport(ctx, s, view, backend, backend); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if err := os.WriteFile(toolPath, []byte("tool-v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend.planTag = "second"
	if _, err := cache.GetOrExport(ctx, s, view, backend, backend); err != nil {
		t.Fatalf("export after planner/tool input changed: %v", err)
	}
	if got := backend.exportCalls.Load(); got != 2 {
		t.Fatalf("provider exports = %d, want 2 after same capability pointers produced a new plan", got)
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if len(cache.entries) != 2 {
		t.Fatalf("cached plan entries = %d, want distinct old and new plan identities", len(cache.entries))
	}
}

type incompleteGoOverlayBackend struct {
	*golang.Backend
	workspaceCalls atomic.Int32
}

type countingGoOverlayBackend struct {
	*golang.Backend
	definitionCalls atomic.Int32
	workspaceCalls  atomic.Int32
}

type mutatingGoOverlayBackend struct {
	*golang.Backend
	mutate func() error
}

type mutableOverlayPlanBackend struct {
	toolPath    string
	planTag     string
	exportCalls atomic.Int32
}

func (b *mutableOverlayPlanBackend) BuildIndexRequest(_ context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	toolBytes, err := os.ReadFile(b.toolPath)
	if err != nil {
		return model.Request{}, err
	}
	toolHash := strings.TrimPrefix(string(semanticOverlayContentHash(toolBytes)), "sha256:")
	tool := model.ToolIdentity{Name: "test-tool", Path: b.toolPath, Version: "test", SHA256: toolHash}
	scope := model.Scope{
		ID: "go:test", Language: "go", RootURI: rootURI,
		Build: model.BuildInputs{Options: map[string]string{"plan": b.planTag}},
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, "test-extractor", "1", "test-toolchain", []model.ToolIdentity{tool})
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.Identity(), Scope: scope,
		Extractor: "test-extractor", ExtractorVer: "1", Backend: identity.BackendID{Language: "go", Name: "test"},
		Toolchain: "test-toolchain", Tools: []model.ToolIdentity{tool},
	}
	return model.Request{
		WorkspaceRootURI: rootURI, View: view, Scopes: []model.Scope{scope},
		Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, nil
}

func (b *mutableOverlayPlanBackend) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	b.exportCalls.Add(1)
	var source model.File
	if err := request.View.Walk(ctx, request.WorkspaceRootURI, func(file model.File) error {
		if file.LanguageID == "go" && strings.HasSuffix(file.URI, ".go") {
			source = file
		}
		return nil
	}); err != nil {
		return model.Report{}, err
	}
	if source.URI == "" {
		return model.Report{}, errors.New("test plan found no Go source")
	}
	scope := request.Scopes[0]
	content, err := readGoSnapshotViewFile(ctx, request.View.(*goSnapshotSemanticView), source)
	if err != nil {
		return model.Report{}, err
	}
	nameOffset := strings.Index(string(content), "renamed")
	if nameOffset < 0 {
		return model.Report{}, errors.New("test source omitted renamed identifier")
	}
	start, err := position.NewIndex(content, position.UTF16).OffsetToPosition(content, uint32(nameOffset))
	if err != nil {
		return model.Report{}, err
	}
	end, err := position.NewIndex(content, position.UTF16).OffsetToPosition(content, uint32(nameOffset+len("renamed")))
	if err != nil {
		return model.Report{}, err
	}
	if err := sink.WriteSymbols(ctx, []model.Symbol{{ID: "test-symbol", ScopeID: scope.ID, Name: "renamed", Kind: "function"}}); err != nil {
		return model.Report{}, err
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{{
		SymbolID: "test-symbol", ScopeID: scope.ID, URI: source.URI,
		Range: model.Position{StartLine: start.Line, StartChar: start.Col, EndLine: end.Line, EndChar: end.Col},
		Role:  "definition", SourceHash: source.SHA256, BuildContext: scope.BuildContext,
	}}); err != nil {
		return model.Report{}, err
	}
	coverage := make([]model.Coverage, 0, len(model.RequiredFactKinds))
	for _, fact := range model.RequiredFactKinds {
		coverage = append(coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: model.Complete})
	}
	tool := request.Provenance[scope.ID].Tools[0]
	return model.Report{
		Identity: request.View.Identity(), Coverage: coverage,
		UsedTools: map[string][]model.ToolIdentity{scope.ID: {tool}},
	}, nil
}

func (b *countingGoOverlayBackend) Definition(ctx context.Context, request languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.definitionCalls.Add(1)
	return b.Backend.Definition(ctx, request)
}

func (b *countingGoOverlayBackend) WorkspaceSymbols(ctx context.Context, request languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	b.workspaceCalls.Add(1)
	return b.Backend.WorkspaceSymbols(ctx, request)
}

func (b *incompleteGoOverlayBackend) WorkspaceSymbols(ctx context.Context, request languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	b.workspaceCalls.Add(1)
	return b.Backend.WorkspaceSymbols(ctx, request)
}

func (b *incompleteGoOverlayBackend) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	report, err := b.Backend.ExportIndex(ctx, request, sink)
	if err != nil {
		return report, err
	}
	for i := range report.Coverage {
		if report.Coverage[i].Fact == model.FactDefinition {
			report.Coverage[i].State = model.IncompleteKnownSubset
			report.Coverage[i].Reason = "test: incomplete definition scope"
		}
	}
	return report, nil
}

func (b *mutatingGoOverlayBackend) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	report, err := b.Backend.ExportIndex(ctx, request, sink)
	if err == nil && b.mutate != nil {
		err = b.mutate()
	}
	return report, err
}

func newGoOverlayQueryTestServer(
	root, rootURI, fileURI string,
	content []byte,
	backend languages.Backend,
) *Server {
	s := New(DefaultConfig())
	s.workspaceID = identity.WorkspaceID(rootURI)
	s.idx = &indexService{
		root: root, workspaceID: identity.WorkspaceID(rootURI),
		dir: filepath.Join(root, ".index"),
	}
	s.overlayFacts = newGoSnapshotSemanticFactsCache()
	s.RegisterBackend("go", backend)
	s.vfs.Open(fileURI, "go", 1, content, vfs.SourceEditor)
	revision := s.vfs.Revision()
	captured := snapshot.New(string(s.workspaceID), revision, map[string]snapshot.DocumentSnapshot{
		fileURI: {URI: fileURI, LanguageID: "go", Version: 1, Content: content},
	})
	s.snapMgr.Publish(captured)
	return s
}

func installEmptyPersistentWorkspaceStore(t *testing.T, s *Server) {
	t.Helper()
	store, err := persistent.NewFileStore(s.idx.dir, persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.idx.store = store
}

func setGoOverlayTestEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOENV", "off")
	t.Setenv("GOTOOLCHAIN", "local")
}

func testPositionAtOffset(t *testing.T, content []byte, offset int, encoding position.Encoding) (uint32, uint32) {
	t.Helper()
	if offset < 0 || offset > len(content) {
		t.Fatalf("test byte offset %d outside content length %d", offset, len(content))
	}
	pos, err := position.NewIndex(content, encoding).OffsetToPosition(content, uint32(offset))
	if err != nil {
		t.Fatal(err)
	}
	return pos.Line, pos.Col
}

func overlaySymbolIDFromFactsCache(cache *goSnapshotSemanticFactsCache, name string) bool {
	if cache == nil {
		return false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, element := range cache.entries {
		facts := element.Value.(*goOverlayFactsEntry).facts
		for _, symbol := range facts.symbols {
			if symbol.Name == name {
				return true
			}
		}
	}
	return false
}
