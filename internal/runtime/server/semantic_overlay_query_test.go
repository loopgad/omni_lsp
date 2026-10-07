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

func TestHandleWorkspaceSymbolsUsesDirtyOverlayForEveryBoundLanguage(t *testing.T) {
	setGoOverlayTestEnvironment(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module overlay.workspace.test\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	goDisk := []byte("package overlaytest\n\nfunc target() {}\n\nfunc use() { target() }\n")
	goOpen := []byte("// unsaved line\npackage overlaytest\n\nfunc /*😀*/ renamed() {}\n\nfunc use() { renamed() }\n")
	goPath := filepath.Join(root, "main.go")
	if err := os.WriteFile(goPath, goDisk, 0o600); err != nil {
		t.Fatal(err)
	}
	pythonDisk := []byte("def keep_old_helper():\n    pass\n")
	pythonOpen := []byte("def renamed_helper():\n    pass\n\n\ndef caller():\n    renamed_helper()\n")
	pythonPath := filepath.Join(root, "main.py")
	if err := os.WriteFile(pythonPath, pythonDisk, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	goURI := uri.FromPath(goPath).Canonical()
	pythonURI := uri.FromPath(pythonPath).Canonical()
	toolPath := filepath.Join(t.TempDir(), "fake-python-tool")
	if err := os.WriteFile(toolPath, []byte("tool-v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	goBackend := &countingGoOverlayBackend{Backend: golang.New(root)}
	pythonBackend := &overlayWorkspaceCountingBackend{overlayLanguageFakeBackend: &overlayLanguageFakeBackend{
		mockBackend: mockBackend{langID: "python", wsymResult: []languages.WorkspaceSymbol{{Name: "PythonKeep", URI: "file:///workspace.py", Kind: languages.SymbolFunction}}},
		language:    "python", token: "renamed_helper", toolPath: toolPath,
	}}
	s := New(DefaultConfig())
	s.workspaceID = identity.WorkspaceID(rootURI)
	s.idx = &indexService{
		root: root, workspaceID: identity.WorkspaceID(rootURI),
		dir: filepath.Join(root, ".index"),
	}
	s.overlayFacts = newGoSnapshotSemanticFactsCache()
	s.RegisterBackend("go", goBackend)
	s.RegisterSemanticIndexProvider("go", goBackend, goBackend)
	s.RegisterBackend("python", pythonBackend)
	s.RegisterSemanticIndexProvider("python", pythonBackend, pythonBackend)
	s.vfs.Open(goURI, "go", 1, goOpen, vfs.SourceEditor)
	s.vfs.Open(pythonURI, "python", 1, pythonOpen, vfs.SourceEditor)
	captured := snapshot.New(string(s.workspaceID), s.vfs.Revision(), map[string]snapshot.DocumentSnapshot{
		goURI:     {URI: goURI, LanguageID: "go", Version: 1, Content: goOpen},
		pythonURI: {URI: pythonURI, LanguageID: "python", Version: 1, Content: pythonOpen},
	})
	s.snapMgr.Publish(captured)
	s.positionEncoding = "utf-8"
	installEmptyPersistentWorkspaceStore(t, s)
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
	var foundRenamed, foundOld, foundPythonOverlay, foundPythonLive bool
	var renamedStart uint32
	for _, symbol := range symbols {
		switch symbol.Name {
		case "renamed":
			foundRenamed = symbol.Location.URI == goURI
			renamedStart = symbol.Location.Range.Start.Character
		case "target":
			foundOld = true
		case "renamed_helper":
			foundPythonOverlay = symbol.Location.URI == pythonURI
		case "PythonKeep":
			foundPythonLive = true
		}
	}
	if !foundRenamed || foundOld || !foundPythonOverlay || foundPythonLive {
		t.Fatalf("merged dirty workspace symbols: renamed=%v old=%v pythonOverlay=%v pythonLive=%v values=%+v", foundRenamed, foundOld, foundPythonOverlay, foundPythonLive, symbols)
	}
	if renamedStart != 14 {
		t.Fatalf("UTF-8 workspace symbol start character = %d, want 14", renamedStart)
	}
	if got := goBackend.workspaceCalls.Load(); got != 0 {
		t.Fatalf("live Go workspace symbols calls = %d, want 0 for exact dirty overlay", got)
	}
	if got := pythonBackend.workspaceCalls.Load(); got != 0 {
		t.Fatalf("live Python workspace symbols calls = %d, want 0 for exact dirty overlay", got)
	}
	if got := pythonBackend.overlayLanguageFakeBackend.calls.Load(); got != 1 {
		t.Fatalf("Python overlay exports = %d, want exactly one dirty snapshot export", got)
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

func TestHandleWorkspaceSymbolsRejectsUnprovableDirtyOverlay(t *testing.T) {
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
	if response == nil || response.Error == nil || !strings.Contains(response.Error.Message, "current go workspace symbols could not be verified") {
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

// A backend without semantic index capabilities has no overlay to prove a
// dirty snapshot with: its language must keep answering from the live backend
// instead of failing the whole request as content-modified.
func TestHandleWorkspaceSymbolsWithoutBindingKeepsLiveBackendWhenDirty(t *testing.T) {
	root := t.TempDir()
	diskSource := []byte("int keep_old(void) { return 0; }\n")
	openSource := []byte("int renamed_target(void) { return 1; }\n")
	filePath := filepath.Join(root, "main.c")
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	liveBackend := &workspaceCountingBackend{mockBackend: mockBackend{
		langID: "c", exts: []string{".c"},
		wsymResult: []languages.WorkspaceSymbol{{Name: "LiveCTarget", URI: fileURI, Kind: languages.SymbolFunction}},
	}}
	var asBackend languages.Backend = liveBackend
	if _, implementsPlanner := asBackend.(languages.SemanticIndexRequestBuilder); implementsPlanner {
		t.Fatal("fixture backend must not implement semantic index capabilities")
	}
	s := newOverlayQueryTestServer(root, rootURI, fileURI, "c", openSource, liveBackend)
	installEmptyPersistentWorkspaceStore(t, s)
	request := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "no-binding-workspace-symbols", IsStr: true}, "workspace/symbol", json.RawMessage(`{"query":""}`))
	response := s.Dispatcher().Dispatch(context.Background(), request)
	if response == nil || response.Error != nil {
		if response == nil {
			t.Fatal("workspace symbol response is nil")
		}
		t.Fatalf("dirty workspace symbols without a semantic binding returned error %+v, want the live backend answer", *response.Error)
	}
	var symbols []struct {
		Name     string `json:"name"`
		Location struct {
			URI string `json:"uri"`
		} `json:"location"`
	}
	if err := json.Unmarshal(response.Result, &symbols); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, symbol := range symbols {
		if symbol.Name == "LiveCTarget" && symbol.Location.URI == fileURI {
			found = true
		}
	}
	if !found {
		t.Fatalf("live backend symbols missing from dirty no-binding response: %+v", symbols)
	}
	if liveBackend.calls != 1 {
		t.Fatalf("live backend workspace symbols calls = %d, want 1 while dirty and unbound", liveBackend.calls)
	}
}

// A backend that implements the semantic index interfaces but was never
// registered explicitly through RegisterSemanticIndexProvider has no trusted
// dirty overlay: its language must keep answering from the live backend
// instead of failing the whole request as content-modified. This mirrors the
// production ccls shape — the bridge implements both interfaces, but without
// a compile_commands.json its planner cannot prove a dirty snapshot, so the
// request must not fail as content-modified.
func TestHandleWorkspaceSymbolsWithoutExplicitSemanticIndexKeepsLiveBackendWhenDirty(t *testing.T) {
	root := t.TempDir()
	diskSource := []byte("int keep_old(void) { return 0; }\n")
	openSource := []byte("int renamed_target(void) { return 1; }\n")
	filePath := filepath.Join(root, "main.cpp")
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	toolPath := filepath.Join(t.TempDir(), "fake-cpp-tool")
	if err := os.WriteFile(toolPath, []byte("tool-v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	liveBackend := &overlayWorkspaceCountingBackend{overlayLanguageFakeBackend: &overlayLanguageFakeBackend{
		mockBackend: mockBackend{
			langID: "cpp", exts: []string{".cpp"},
			wsymResult: []languages.WorkspaceSymbol{{Name: "LiveCppTarget", URI: fileURI, Kind: languages.SymbolFunction}},
		},
		language: "cpp", token: "renamed_target", toolPath: toolPath,
	}}
	var asBackend languages.Backend = liveBackend
	if _, implementsProvider := asBackend.(languages.SemanticIndexProvider); !implementsProvider {
		t.Fatal("fixture backend must implement the semantic index provider")
	}
	if _, implementsPlanner := asBackend.(languages.SemanticIndexRequestBuilder); !implementsPlanner {
		t.Fatal("fixture backend must implement the semantic index request builder")
	}
	s := New(DefaultConfig())
	s.workspaceID = identity.WorkspaceID(rootURI)
	s.idx = &indexService{
		root: root, workspaceID: identity.WorkspaceID(rootURI),
		dir: filepath.Join(root, ".index"),
	}
	s.overlayFacts = newGoSnapshotSemanticFactsCache()
	s.RegisterBackend("cpp", liveBackend)
	if s.hasExplicitSemanticIndex("cpp") {
		t.Fatal("RegisterBackend alone must not register an explicit semantic index capability")
	}
	s.vfs.Open(fileURI, "cpp", 1, openSource, vfs.SourceEditor)
	revision := s.vfs.Revision()
	captured := snapshot.New(string(s.workspaceID), revision, map[string]snapshot.DocumentSnapshot{
		fileURI: {URI: fileURI, LanguageID: "cpp", Version: 1, Content: openSource},
	})
	s.snapMgr.Publish(captured)
	installEmptyPersistentWorkspaceStore(t, s)
	request := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "unregistered-capability-workspace-symbols", IsStr: true}, "workspace/symbol", json.RawMessage(`{"query":""}`))
	response := s.Dispatcher().Dispatch(context.Background(), request)
	if response == nil || response.Error != nil {
		if response == nil {
			t.Fatal("workspace symbol response is nil")
		}
		t.Fatalf("dirty workspace symbols with an unregistered semantic capability returned error %+v, want the live backend answer", *response.Error)
	}
	var symbols []struct {
		Name     string `json:"name"`
		Location struct {
			URI string `json:"uri"`
		} `json:"location"`
	}
	if err := json.Unmarshal(response.Result, &symbols); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, symbol := range symbols {
		if symbol.Name == "LiveCppTarget" && symbol.Location.URI == fileURI {
			found = true
		}
	}
	if !found {
		t.Fatalf("live backend symbols missing from dirty unregistered-capability response: %+v", symbols)
	}
	if got := liveBackend.workspaceCalls.Load(); got != 1 {
		t.Fatalf("live backend workspace symbols calls = %d, want 1 while dirty with only a backend-level capability", got)
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
	if _, err := cache.GetOrExport(ctx, s, view, backend, backend, "go"); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if err := os.WriteFile(toolPath, []byte("tool-v2"), 0o700); err != nil {
		t.Fatal(err)
	}
	backend.planTag = "second"
	if _, err := cache.GetOrExport(ctx, s, view, backend, backend, "go"); err != nil {
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

// overlayWorkspaceCountingBackend counts live workspace symbol queries for a
// fake non-Go semantic index binding while promoting its BuildIndexRequest and
// ExportIndex capabilities.
type overlayWorkspaceCountingBackend struct {
	*overlayLanguageFakeBackend
	workspaceCalls atomic.Int32
}

func (b *overlayWorkspaceCountingBackend) WorkspaceSymbols(ctx context.Context, request languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	b.workspaceCalls.Add(1)
	return b.overlayLanguageFakeBackend.WorkspaceSymbols(ctx, request)
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
	return newOverlayQueryTestServer(root, rootURI, fileURI, "go", content, backend)
}

func newOverlayQueryTestServer(
	root, rootURI, fileURI, language string,
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
	s.RegisterBackend(language, backend)
	// The dirty workspace/symbol guard trusts only explicitly registered
	// semantic index capabilities, so the overlay fixtures register their own
	// provider/planner explicitly instead of relying on interface duck-typing.
	if provider, ok := backend.(languages.SemanticIndexProvider); ok {
		if planner, ok := backend.(languages.SemanticIndexRequestBuilder); ok {
			s.RegisterSemanticIndexProvider(language, provider, planner)
		}
	}
	s.vfs.Open(fileURI, language, 1, content, vfs.SourceEditor)
	revision := s.vfs.Revision()
	captured := snapshot.New(string(s.workspaceID), revision, map[string]snapshot.DocumentSnapshot{
		fileURI: {URI: fileURI, LanguageID: language, Version: 1, Content: content},
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

// overlayFactsCacheLanguages counts the cached fact sets per language so tests
// can assert that language participates in the cache key.
func overlayFactsCacheLanguages(cache *goSnapshotSemanticFactsCache) map[string]int {
	byLanguage := make(map[string]int)
	if cache == nil {
		return byLanguage
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for _, element := range cache.entries {
		entry := element.Value.(*goOverlayFactsEntry)
		byLanguage[entry.facts.language]++
	}
	return byLanguage
}

// overlayLanguageFakeBackend is a hermetic semantic index capability for one
// non-Go language. It plans a single workspace scope for its language and
// exports a definition and a reference occurrence for token in the captured
// snapshot source, mirroring the compiler-resolved contract without invoking
// any external toolchain.
type overlayLanguageFakeBackend struct {
	mockBackend
	language string
	token    string
	toolPath string
	calls    atomic.Int32
}

func (b *overlayLanguageFakeBackend) BuildIndexRequest(_ context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	toolBytes, err := os.ReadFile(b.toolPath)
	if err != nil {
		return model.Request{}, err
	}
	toolHash := strings.TrimPrefix(string(semanticOverlayContentHash(toolBytes)), "sha256:")
	tool := model.ToolIdentity{Name: b.language + "-tool", Path: b.toolPath, Version: "test", SHA256: toolHash}
	scope := model.Scope{
		ID: b.language + ":overlay", Language: b.language, RootURI: rootURI,
		Build: model.BuildInputs{Options: map[string]string{"language": b.language}},
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, b.language+"-extractor", "1", b.language+"-toolchain", []model.ToolIdentity{tool})
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.Identity(), Scope: scope,
		Extractor: b.language + "-extractor", ExtractorVer: "1",
		Backend:   identity.BackendID{Language: b.language, Name: "test-" + b.language},
		Toolchain: b.language + "-toolchain", Tools: []model.ToolIdentity{tool},
	}
	return model.Request{
		WorkspaceRootURI: rootURI, View: view, Scopes: []model.Scope{scope},
		Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, nil
}

func (b *overlayLanguageFakeBackend) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	b.calls.Add(1)
	var sources []model.File
	if err := request.View.Walk(ctx, request.WorkspaceRootURI, func(file model.File) error {
		if file.LanguageID == b.language {
			sources = append(sources, file)
		}
		return nil
	}); err != nil {
		return model.Report{}, err
	}
	scope := request.Scopes[0]
	var source model.File
	var content []byte
	for _, candidate := range sources {
		candidateContent, err := readGoSnapshotViewFile(ctx, request.View.(*goSnapshotSemanticView), candidate)
		if err != nil {
			return model.Report{}, err
		}
		if strings.Contains(string(candidateContent), b.token) {
			source, content = candidate, candidateContent
			break
		}
	}
	if source.URI == "" {
		return model.Report{}, fmt.Errorf("test plan found no %s source containing %q", b.language, b.token)
	}
	occurrences, err := overlayFakeTokenOccurrences(b.language, b.token, content, source, scope)
	if err != nil {
		return model.Report{}, err
	}
	if err := sink.WriteSymbols(ctx, []model.Symbol{{ID: identity.SymbolID(b.language + "-symbol"), ScopeID: scope.ID, Name: b.token, Kind: "function"}}); err != nil {
		return model.Report{}, err
	}
	if err := sink.WriteOccurrences(ctx, occurrences); err != nil {
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

func overlayFakeTokenOccurrences(language, token string, content []byte, source model.File, scope model.Scope) ([]model.Occurrence, error) {
	first := strings.Index(string(content), token)
	last := strings.LastIndex(string(content), token)
	if first < 0 || last <= first {
		return nil, fmt.Errorf("test %s source omitted a definition and reference of %q", language, token)
	}
	index := position.NewIndex(content, position.UTF16)
	occurrence := func(offset int, role string) (model.Occurrence, error) {
		start, err := index.OffsetToPosition(content, uint32(offset))
		if err != nil {
			return model.Occurrence{}, err
		}
		end, err := index.OffsetToPosition(content, uint32(offset+len(token)))
		if err != nil {
			return model.Occurrence{}, err
		}
		return model.Occurrence{
			SymbolID: identity.SymbolID(language + "-symbol"), ScopeID: scope.ID, URI: source.URI,
			Range: model.Position{StartLine: start.Line, StartChar: start.Col, EndLine: end.Line, EndChar: end.Col},
			Role:  role, SourceHash: source.SHA256, BuildContext: scope.BuildContext,
		}, nil
	}
	definition, err := occurrence(first, "definition")
	if err != nil {
		return nil, err
	}
	reference, err := occurrence(last, "reference")
	if err != nil {
		return nil, err
	}
	return []model.Occurrence{definition, reference}, nil
}

// runOverlayLanguageLocationsTest proves the dirty snapshot overlay answers
// definition/reference queries for one non-Go language from the exact editor
// buffer, with compiler-grade evidence and per-language cache facts.
func runOverlayLanguageLocationsTest(t *testing.T, language, fileName string, diskSource, openSource []byte, token string) {
	t.Helper()
	toolPath := filepath.Join(t.TempDir(), "fake-"+language+"-tool")
	if err := os.WriteFile(toolPath, []byte("tool-v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filePath := filepath.Join(root, fileName)
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	backend := &overlayLanguageFakeBackend{
		mockBackend: mockBackend{langID: language},
		language:    language, token: token, toolPath: toolPath,
	}
	s := newOverlayQueryTestServer(root, rootURI, fileURI, language, openSource, backend)
	captured := s.snapMgr.Current()
	ctx := withSnapshot(context.Background(), captured)

	queryOffset := strings.LastIndex(string(openSource), token)
	line, character := testPositionAtOffset(t, openSource, queryOffset, position.UTF16)
	definition, used := s.goSnapshotSemanticLocations(ctx, fileURI, line, character, int(position.UTF16), captured.ID().Revision, persistentDefinition, false)
	if !used {
		t.Fatalf("%s overlay definition was not used for the dirty snapshot", language)
	}
	if definition.Status != identity.ResultExact || definition.Completeness != identity.Complete || len(definition.Value) != 1 {
		t.Fatalf("%s overlay definition result = %#v", language, definition)
	}
	defOffset := strings.Index(string(openSource), token)
	wantLine, wantStart := testPositionAtOffset(t, openSource, defOffset, position.UTF16)
	_, wantEnd := testPositionAtOffset(t, openSource, defOffset+len(token), position.UTF16)
	if got := definition.Value[0]; got.URI != fileURI || got.Range.StartLine != wantLine || got.Range.StartCharacter != wantStart ||
		got.Range.EndLine != wantLine || got.Range.EndCharacter != wantEnd {
		t.Fatalf("%s overlay definition location = %#v, want %d:%d-%d:%d", language, got, wantLine, wantStart, wantLine, wantEnd)
	}
	if len(definition.Evidence) != 1 || definition.Evidence[0].Kind != identity.EvidenceCompiler ||
		definition.Evidence[0].Assurance != identity.AssuranceCompilerResolved || definition.Evidence[0].IndexGen != 0 ||
		definition.Evidence[0].SourceHash != semanticOverlayContentHash(openSource) ||
		definition.Evidence[0].Backend.Language != language ||
		definition.Evidence[0].DetailCode != language+"-snapshot-overlay-definition" {
		t.Fatalf("%s overlay definition evidence = %#v", language, definition.Evidence)
	}

	references, used := s.goSnapshotSemanticLocations(ctx, fileURI, line, character, int(position.UTF16), captured.ID().Revision, persistentReferences, true)
	if !used || references.Status != identity.ResultExact || references.Completeness != identity.Complete || len(references.Value) != 2 {
		t.Fatalf("%s overlay references result = %#v used=%v", language, references, used)
	}
	wantOffsets := []int{defOffset, queryOffset}
	for i, offset := range wantOffsets {
		wantLine, wantStart := testPositionAtOffset(t, openSource, offset, position.UTF16)
		_, wantEnd := testPositionAtOffset(t, openSource, offset+len(token), position.UTF16)
		got := references.Value[i]
		if got.URI != fileURI || got.Range.StartLine != wantLine || got.Range.StartCharacter != wantStart ||
			got.Range.EndLine != wantLine || got.Range.EndCharacter != wantEnd {
			t.Fatalf("%s overlay reference location %d = %#v, want %d:%d-%d:%d", language, i, got, wantLine, wantStart, wantLine, wantEnd)
		}
	}
	if got := overlayFactsCacheLanguages(s.overlayFacts); len(got) != 1 || got[language] != 1 {
		t.Fatalf("%s overlay facts cache languages = %v, want exactly one %s fact set", language, got, language)
	}
}

func TestRustSnapshotOverlayLocationsUseDirtySnapshot(t *testing.T) {
	runOverlayLanguageLocationsTest(t, "rust", "main.rs",
		[]byte("fn keep_old() {}\n"),
		[]byte("fn renamed_target() {}\n\nfn caller() { renamed_target(); }\n"),
		"renamed_target")
}

func TestCppSnapshotOverlayLocationsUseDirtySnapshot(t *testing.T) {
	runOverlayLanguageLocationsTest(t, "cpp", "main.cpp",
		[]byte("int keep_old() { return 0; }\n"),
		[]byte("int renamed_target() { return 1; }\n\nint caller() { return renamed_target(); }\n"),
		"renamed_target")
}

func TestTypescriptSnapshotOverlayLocationsUseDirtySnapshot(t *testing.T) {
	runOverlayLanguageLocationsTest(t, "typescript", "main.ts",
		[]byte("function keepOld() {}\n"),
		[]byte("function renamedTarget() {}\n\nfunction caller() { renamedTarget(); }\n"),
		"renamedTarget")
}

// runOverlayLanguageWorkspaceSymbolsTest proves the dirty snapshot overlay
// answers workspace/symbol for one non-Go language from the exact editor
// buffer, with compiler-grade evidence, and that the live backend is never
// consulted while the overlay proves the answer.
func runOverlayLanguageWorkspaceSymbolsTest(t *testing.T, language, fileName string, diskSource, openSource []byte, token string) {
	t.Helper()
	toolPath := filepath.Join(t.TempDir(), "fake-"+language+"-tool")
	if err := os.WriteFile(toolPath, []byte("tool-v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	filePath := filepath.Join(root, fileName)
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	backend := &overlayWorkspaceCountingBackend{overlayLanguageFakeBackend: &overlayLanguageFakeBackend{
		mockBackend: mockBackend{langID: language, wsymResult: []languages.WorkspaceSymbol{
			{Name: "LiveOnly" + language, URI: "file:///live/" + language, Kind: languages.SymbolFunction},
		}},
		language: language, token: token, toolPath: toolPath,
	}}
	s := newOverlayQueryTestServer(root, rootURI, fileURI, language, openSource, backend)
	installEmptyPersistentWorkspaceStore(t, s)
	var evidence []identity.Evidence
	s.SetSemanticResponseObserver(func(_ jsonrpc.RequestID, values []identity.Evidence) { evidence = append(evidence, values...) })
	request := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "dirty-" + language + "-workspace-symbols", IsStr: true}, "workspace/symbol", json.RawMessage(`{"query":""}`))
	response := s.Dispatcher().Dispatch(context.Background(), request)
	if response == nil || response.Error != nil {
		if response == nil {
			t.Fatalf("%s workspace symbol response is nil", language)
		}
		t.Fatalf("%s workspace symbol response error = %+v", language, *response.Error)
	}
	var symbols []struct {
		Name     string `json:"name"`
		Location struct {
			URI   string `json:"uri"`
			Range struct {
				Start struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"start"`
			} `json:"range"`
		} `json:"location"`
	}
	if err := json.Unmarshal(response.Result, &symbols); err != nil {
		t.Fatal(err)
	}
	defOffset := strings.Index(string(openSource), token)
	wantLine, wantCharacter := testPositionAtOffset(t, openSource, defOffset, position.UTF16)
	overlaySymbol, liveLeaked := false, false
	for _, symbol := range symbols {
		switch symbol.Name {
		case token:
			overlaySymbol = symbol.Location.URI == fileURI &&
				symbol.Location.Range.Start.Line == wantLine && symbol.Location.Range.Start.Character == wantCharacter
		case "LiveOnly" + language:
			liveLeaked = true
		}
	}
	if !overlaySymbol || liveLeaked {
		t.Fatalf("%s dirty workspace symbols: overlaySymbol=%v liveLeaked=%v values=%+v", language, overlaySymbol, liveLeaked, symbols)
	}
	if got := backend.workspaceCalls.Load(); got != 0 {
		t.Fatalf("live %s workspace symbols calls = %d, want 0 for exact dirty overlay", language, got)
	}
	overlayEvidence := 0
	for _, item := range evidence {
		if item.Kind == identity.EvidenceCompiler && item.DetailCode == language+"-snapshot-overlay-workspace-symbols" {
			overlayEvidence++
		}
	}
	if overlayEvidence == 0 {
		t.Fatalf("%s dirty workspace result omitted overlay evidence: %+v", language, evidence)
	}
}

func TestRustSnapshotOverlayWorkspaceSymbolsUseDirtySnapshot(t *testing.T) {
	runOverlayLanguageWorkspaceSymbolsTest(t, "rust", "main.rs",
		[]byte("fn keep_old() {}\n"),
		[]byte("fn renamed_target() {}\n\nfn caller() { renamed_target(); }\n"),
		"renamed_target")
}

func TestCppSnapshotOverlayWorkspaceSymbolsUseDirtySnapshot(t *testing.T) {
	runOverlayLanguageWorkspaceSymbolsTest(t, "cpp", "main.cpp",
		[]byte("int keep_old() { return 0; }\n"),
		[]byte("int renamed_target() { return 1; }\n\nint caller() { return renamed_target(); }\n"),
		"renamed_target")
}

func TestTypescriptSnapshotOverlayWorkspaceSymbolsUseDirtySnapshot(t *testing.T) {
	runOverlayLanguageWorkspaceSymbolsTest(t, "typescript", "main.ts",
		[]byte("function keepOld() {}\n"),
		[]byte("function renamedTarget() {}\n\nfunction caller() { renamedTarget(); }\n"),
		"renamedTarget")
}

func TestSnapshotOverlayFactsCacheSeparatesLanguages(t *testing.T) {
	root := t.TempDir()
	rootURI := uri.FromPath(root).Canonical()
	rustPath := filepath.Join(root, "main.rs")
	tsPath := filepath.Join(root, "main.ts")
	rustDisk := []byte("fn keep_old() {}\n")
	tsDisk := []byte("function keepOld() {}\n")
	rustOpen := []byte("fn renamed_target() {}\n\nfn caller() { renamed_target(); }\n")
	tsOpen := []byte("function renamedTarget() {}\n\nfunction caller() { renamedTarget(); }\n")
	if err := os.WriteFile(rustPath, rustDisk, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tsPath, tsDisk, 0o600); err != nil {
		t.Fatal(err)
	}
	rustURI := uri.FromPath(rustPath).Canonical()
	tsURI := uri.FromPath(tsPath).Canonical()
	fake := func(language, token string, source []byte) *overlayLanguageFakeBackend {
		toolPath := filepath.Join(t.TempDir(), "fake-"+language+"-tool")
		if err := os.WriteFile(toolPath, []byte("tool-v1"), 0o700); err != nil {
			t.Fatal(err)
		}
		return &overlayLanguageFakeBackend{
			mockBackend: mockBackend{langID: language},
			language:    language, token: token, toolPath: toolPath,
		}
	}
	rustBackend := fake("rust", "renamed_target", rustOpen)
	tsBackend := fake("typescript", "renamedTarget", tsOpen)

	s := New(DefaultConfig())
	s.workspaceID = identity.WorkspaceID(rootURI)
	s.idx = &indexService{
		root: root, workspaceID: identity.WorkspaceID(rootURI),
		dir: filepath.Join(root, ".index"),
	}
	s.overlayFacts = newGoSnapshotSemanticFactsCache()
	s.RegisterBackend("rust", rustBackend)
	s.RegisterBackend("typescript", tsBackend)
	s.vfs.Open(rustURI, "rust", 1, rustOpen, vfs.SourceEditor)
	s.vfs.Open(tsURI, "typescript", 1, tsOpen, vfs.SourceEditor)
	revision := s.vfs.Revision()
	captured := snapshot.New(string(s.workspaceID), revision, map[string]snapshot.DocumentSnapshot{
		rustURI: {URI: rustURI, LanguageID: "rust", Version: 1, Content: rustOpen},
		tsURI:   {URI: tsURI, LanguageID: "typescript", Version: 1, Content: tsOpen},
	})
	s.snapMgr.Publish(captured)
	ctx := withSnapshot(context.Background(), captured)

	query := func(fileURI string, source []byte, token string) {
		t.Helper()
		line, character := testPositionAtOffset(t, source, strings.LastIndex(string(source), token), position.UTF16)
		result, used := s.goSnapshotSemanticLocations(ctx, fileURI, line, character, int(position.UTF16), captured.ID().Revision, persistentDefinition, false)
		if !used || result.Status != identity.ResultExact || len(result.Value) != 1 || result.Value[0].URI != fileURI {
			t.Fatalf("%s overlay definition result = %#v used=%v", fileURI, result, used)
		}
	}
	query(rustURI, rustOpen, "renamed_target")
	query(tsURI, tsOpen, "renamedTarget")
	if rustBackend.calls.Load() != 1 || tsBackend.calls.Load() != 1 {
		t.Fatalf("per-language exports = rust %d typescript %d, want one each", rustBackend.calls.Load(), tsBackend.calls.Load())
	}
	got := overlayFactsCacheLanguages(s.overlayFacts)
	if len(got) != 2 || got["rust"] != 1 || got["typescript"] != 1 {
		t.Fatalf("overlay facts cache languages = %v, want one fact set per language", got)
	}
}

func TestSnapshotOverlayDirtyScreenStaysBoundedPerLanguage(t *testing.T) {
	const rustFiles = overlayDirtyScreenMaxPerLanguage + 1
	token := "renamed_target"
	dirtyDisk := []byte("fn keep_old() {}\n")
	dirtyOpen := []byte("fn " + token + "() {}\n\nfn caller() { " + token + "(); }\n")
	cleanSource := []byte("fn clean() {}\n")

	newServer := func(dirtyFirst bool) (*Server, string, string, *snapshot.Snapshot) {
		t.Helper()
		toolPath := filepath.Join(t.TempDir(), "fake-rust-tool")
		if err := os.WriteFile(toolPath, []byte("tool-v1"), 0o700); err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		rootURI := uri.FromPath(root).Canonical()
		open := make(map[string]snapshot.DocumentSnapshot, rustFiles)
		s := New(DefaultConfig())
		s.workspaceID = identity.WorkspaceID(rootURI)
		s.idx = &indexService{
			root: root, workspaceID: identity.WorkspaceID(rootURI),
			dir: filepath.Join(root, ".index"),
		}
		s.overlayFacts = newGoSnapshotSemanticFactsCache()
		s.RegisterBackend("rust", &overlayLanguageFakeBackend{
			mockBackend: mockBackend{langID: "rust"},
			language:    "rust", token: token, toolPath: toolPath,
		})
		dirtyURI := ""
		for i := 0; i < rustFiles; i++ {
			name := fmt.Sprintf("main%03d.rs", i)
			path := filepath.Join(root, name)
			disk, editor := cleanSource, cleanSource
			if i == 0 && dirtyFirst {
				disk, editor = dirtyDisk, dirtyOpen
			}
			if err := os.WriteFile(path, disk, 0o600); err != nil {
				t.Fatal(err)
			}
			fileURI := uri.FromPath(path).Canonical()
			if i == 0 {
				dirtyURI = fileURI
			}
			s.vfs.Open(fileURI, "rust", 1, editor, vfs.SourceEditor)
			open[fileURI] = snapshot.DocumentSnapshot{URI: fileURI, LanguageID: "rust", Version: 1, Content: editor}
		}
		captured := snapshot.New(string(s.workspaceID), s.vfs.Revision(), open)
		s.snapMgr.Publish(captured)
		return s, rootURI, dirtyURI, captured
	}

	// More per-language open documents than the screen budget: the pre-screen
	// stays bounded and the request falls back to the live backend instead of
	// scanning without bound, even though every document is clean.
	s, _, dirtyURI, captured := newServer(false)
	ctx := withSnapshot(context.Background(), captured)
	if _, used := s.goSnapshotSemanticLocations(ctx, dirtyURI, 0, 0, int(position.UTF16), captured.ID().Revision, persistentDefinition, false); used {
		t.Fatal("overlay used although the dirty screen exceeded its per-language budget without finding a change")
	}

	// One dirty document inside the budget is still attributed, and the
	// remaining over-budget documents neither disable the overlay nor unbound
	// the screen.
	s, _, dirtyURI, captured = newServer(true)
	ctx = withSnapshot(context.Background(), captured)
	line, character := testPositionAtOffset(t, dirtyOpen, strings.LastIndex(string(dirtyOpen), token), position.UTF16)
	if _, used := s.goSnapshotSemanticLocations(ctx, dirtyURI, line, character, int(position.UTF16), captured.ID().Revision, persistentDefinition, false); !used {
		t.Fatal("dirty overlay was not used although the dirty document sits inside the screen budget")
	}
}
