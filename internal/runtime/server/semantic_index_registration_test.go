package server

// Explicit semantic index registration guards (behavior-authorization round).
//
// Round 18 established that the ccls bridge binds its exporter capability
// implicitly through RegisterBackend's interface probe while the dirty
// workspace/symbol guard keeps consulting only explicit registrations. This
// round authorizes the explicit cpp/c and js-family registrations in
// cmd/omnilsp/main.go — which flips dirty cpp workspace/symbol from the live
// backend to fail-closed — and these tests pin the resulting key-table
// semantics:
//
//  1. Alias/dialect keys (c, javascript, typescriptreact, javascriptreact)
//     share one exporter capability with their canonical key (cpp,
//     typescript). The enumerated binding list must keep reporting the
//     canonical language so downstream exact-language consumers (SCIP/LSIF
//     import selection, reindex labels) stay unchanged.
//  2. Every registered document-language key must resolve the shared
//     capability, so dirty document overlays can engage for the whole family.
//  3. Dirty cpp workspace/symbol fails closed once the capability is
//     explicitly registered (the authorized behavior change), while clean
//     workspaces and dirty c documents keep answering from the live backend.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// TestRegisterSemanticIndexProviderAliasKeyKeepsCanonicalCppFamily pins the
// cpp family: registering the "c" alias explicitly must not relabel the
// enumerated binding away from the canonical "cpp" key, and both keys must
// resolve the same capability.
func TestRegisterSemanticIndexProviderAliasKeyKeepsCanonicalCppFamily(t *testing.T) {
	s := New(DefaultConfig())
	capability := semanticRegistrationBackend{mockBackend: &mockBackend{langID: "cpp"}}
	s.RegisterBackend("cpp", capability)
	s.RegisterSemanticIndexProvider("cpp", capability, capability)
	s.RegisterSemanticIndexProvider("c", capability, capability)

	bindings := s.semanticIndexBindings()
	if len(bindings) != 1 || bindings[0].language != "cpp" {
		t.Fatalf("alias registration relabeled the cpp family binding: %#v", bindings)
	}
	if _, ok := s.semanticIndexBindingForLanguage("cpp"); !ok {
		t.Fatal("canonical cpp key no longer resolves its binding after alias registration")
	}
	binding, ok := s.semanticIndexBindingForLanguage("c")
	if !ok {
		t.Fatal("alias key c does not resolve the cpp family capability")
	}
	canonical, canonicalOK := s.semanticIndexBindingForLanguage("cpp")
	if !canonicalOK {
		t.Fatal("canonical cpp key no longer resolves its binding after alias registration")
	}
	if binding != canonical {
		t.Fatal("alias key c resolved a different capability than the canonical cpp key")
	}
	if !s.hasExplicitSemanticIndex("cpp") || !s.hasExplicitSemanticIndex("c") {
		t.Fatal("explicit registration must mark both family keys explicit")
	}
}

// TestRegisterSemanticIndexProviderJavaScriptFamilyKeepsTypescriptCanonical
// pins the js family: the four TS/JS dialect keys share the pinned TypeScript
// capability, the enumerated binding keeps the canonical "typescript" label,
// and every dialect key resolves it.
func TestRegisterSemanticIndexProviderJavaScriptFamilyKeepsTypescriptCanonical(t *testing.T) {
	s := New(DefaultConfig())
	capability := semanticRegistrationBackend{mockBackend: &mockBackend{langID: "typescript"}}
	s.RegisterSemanticIndexProvider("typescript", capability, capability)
	s.RegisterSemanticIndexProvider("javascript", capability, capability)
	s.RegisterSemanticIndexProvider("typescriptreact", capability, capability)
	s.RegisterSemanticIndexProvider("javascriptreact", capability, capability)

	bindings := s.semanticIndexBindings()
	if len(bindings) != 1 || bindings[0].language != "typescript" {
		t.Fatalf("js-family registration relabeled the typescript binding: %#v", bindings)
	}
	for _, language := range []string{"typescript", "javascript", "typescriptreact", "javascriptreact"} {
		if _, ok := s.semanticIndexBindingForLanguage(language); !ok {
			t.Fatalf("language %q does not resolve the typescript capability", language)
		}
	}
	if !s.hasExplicitSemanticIndex("typescript") {
		t.Fatal("typescript must stay explicitly registered for the dirty workspace/symbol guard")
	}
}

// countedCppPlannerBackend counts planner invocations so the fail-closed pin
// can distinguish "the registered capability actually planned and refused"
// from "the binding lookup failed before any export was attempted".
type countedCppPlannerBackend struct {
	*overlayWorkspaceCountingBackend
	planCalls int
}

func (b *countedCppPlannerBackend) BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	b.planCalls++
	return b.overlayWorkspaceCountingBackend.BuildIndexRequest(ctx, view, rootURI)
}

// newCppRegistrationFixture builds the production ccls shape hermetically: a
// backend implementing both semantic index interfaces registered under the
// canonical "cpp" key and the "c" alias, one dirty (or clean) main.cpp/main.c
// document, and an empty persistent store. toolMissing controls whether the
// fake planner can restate its plan (missing compile database analogue).
func newCppRegistrationFixture(t *testing.T, fileExtension, declaredLanguage string, dirty bool, toolMissing bool) (*Server, *countedCppPlannerBackend, string) {
	t.Helper()
	root := t.TempDir()
	diskSource := []byte("int keep_old(void) { return 0; }\n")
	openSource := []byte("int renamed_target(void) { return 1; }\n")
	if !dirty {
		openSource = diskSource
	}
	filePath := filepath.Join(root, "main."+fileExtension)
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	toolPath := filepath.Join(t.TempDir(), "fake-cpp-tool")
	if toolMissing {
		// A missing tool file makes the fake planner fail exactly like the
		// production ccls bridge without a compile_commands.json.
	} else if err := os.WriteFile(toolPath, []byte("tool-v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	liveBackend := &countedCppPlannerBackend{overlayWorkspaceCountingBackend: &overlayWorkspaceCountingBackend{overlayLanguageFakeBackend: &overlayLanguageFakeBackend{
		mockBackend: mockBackend{
			langID: "cpp", exts: []string{"." + fileExtension},
			wsymResult: []languages.WorkspaceSymbol{{Name: "LiveCppTarget", URI: fileURI, Kind: languages.SymbolFunction}},
		},
		language: "cpp", token: "renamed_target", toolPath: toolPath,
	}}}
	s := New(DefaultConfig())
	s.workspaceID = identity.WorkspaceID(rootURI)
	s.idx = &indexService{
		root: root, workspaceID: identity.WorkspaceID(rootURI),
		dir: filepath.Join(root, ".index"),
	}
	s.overlayFacts = newGoSnapshotSemanticFactsCache()
	s.RegisterBackend("cpp", liveBackend)
	s.RegisterBackend("c", liveBackend)
	s.RegisterSemanticIndexProvider("cpp", liveBackend, liveBackend)
	s.RegisterSemanticIndexProvider("c", liveBackend, liveBackend)
	s.vfs.Open(fileURI, declaredLanguage, 1, openSource, vfs.SourceEditor)
	revision := s.vfs.Revision()
	captured := snapshot.New(string(s.workspaceID), revision, map[string]snapshot.DocumentSnapshot{
		fileURI: {URI: fileURI, LanguageID: declaredLanguage, Version: 1, Content: openSource},
	})
	s.snapMgr.Publish(captured)
	installEmptyPersistentWorkspaceStore(t, s)
	return s, liveBackend, fileURI
}

func dispatchWorkspaceSymbols(t *testing.T, s *Server, id string) *jsonrpc.Message {
	t.Helper()
	request := jsonrpc.NewRequest(jsonrpc.RequestID{Str: id, IsStr: true}, "workspace/symbol", json.RawMessage(`{"query":""}`))
	response := s.Dispatcher().Dispatch(context.Background(), request)
	if response == nil {
		t.Fatal("workspace symbol response is nil")
	}
	return response
}

// TestExplicitCppSemanticIndexFailsDirtyWorkspaceSymbolsClosed pins the
// authorized behavior change: with the capability explicitly registered, a
// dirty cpp workspace/symbol whose exporter cannot prove a complete snapshot
// answer must fail the request closed (content_modified) instead of serving
// stale disk symbols from the live backend. The planner must actually be
// attempted once — an unknown answer from a lost binding lookup is not this
// contract.
func TestExplicitCppSemanticIndexFailsDirtyWorkspaceSymbolsClosed(t *testing.T) {
	s, backend, _ := newCppRegistrationFixture(t, "cpp", "cpp", true, true)
	response := dispatchWorkspaceSymbols(t, s, "explicit-capability-dirty-fail-closed")
	if response.Error == nil {
		t.Fatalf("dirty cpp workspace/symbol answered %+v, want a content_modified failure", response.Result)
	}
	if response.Error.Code != jsonrpc.ContentModified {
		t.Fatalf("dirty cpp workspace/symbol error code = %d, want content_modified (%d)", response.Error.Code, jsonrpc.ContentModified)
	}
	if backend.planCalls != 1 {
		t.Fatalf("registered cpp planner invocations = %d, want 1 while dirty and fail-closed", backend.planCalls)
	}
	if got := backend.workspaceCalls.Load(); got != 0 {
		t.Fatalf("live backend workspace symbols calls = %d, want 0 while dirty and fail-closed", got)
	}
}

// TestExplicitCppSemanticIndexKeepsLiveBackendForCleanWorkspace pins the X10
// face: explicit registration must not disturb the clean path — a workspace
// without dirty documents keeps answering workspace/symbol from the live
// backend.
func TestExplicitCppSemanticIndexKeepsLiveBackendForCleanWorkspace(t *testing.T) {
	s, backend, fileURI := newCppRegistrationFixture(t, "cpp", "cpp", false, false)
	response := dispatchWorkspaceSymbols(t, s, "explicit-capability-clean-live")
	if response.Error != nil {
		t.Fatalf("clean workspace/symbol failed: %+v", *response.Error)
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
		t.Fatalf("live backend symbols missing from clean response: %+v", symbols)
	}
	if backend.planCalls != 0 {
		t.Fatalf("registered cpp planner invocations = %d, want 0 on the clean path", backend.planCalls)
	}
	if got := backend.workspaceCalls.Load(); got != 1 {
		t.Fatalf("live backend workspace symbols calls = %d, want 1 on the clean path", got)
	}
}

// TestExplicitCppSemanticIndexKeepsLiveBackendForDirtyCDocuments pins the
// alias face: a dirty .c document is attributed to the "c" document language,
// and the workspace/symbol guard consults the canonical backend language
// ("cpp"). The dirty cpp overlay therefore does not engage for it and the
// request keeps answering from the live backend instead of failing closed.
func TestExplicitCppSemanticIndexKeepsLiveBackendForDirtyCDocuments(t *testing.T) {
	s, backend, fileURI := newCppRegistrationFixture(t, "c", "c", true, true)
	response := dispatchWorkspaceSymbols(t, s, "explicit-capability-dirty-c-live")
	if response.Error != nil {
		t.Fatalf("dirty c workspace/symbol failed closed: %+v", *response.Error)
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
		t.Fatalf("live backend symbols missing from dirty c response: %+v", symbols)
	}
	if backend.planCalls != 0 {
		t.Fatalf("registered cpp planner invocations = %d, want 0 while dirty on a c document", backend.planCalls)
	}
	if got := backend.workspaceCalls.Load(); got != 1 {
		t.Fatalf("live backend workspace symbols calls = %d, want 1 while dirty on a c document", got)
	}
}
