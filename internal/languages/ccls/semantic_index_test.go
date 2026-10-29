package ccls

// Typed-coverage evidence for the C/C++ semantic index capability
// (DEF-IDXWIRE). The server binds this backend's exporter implicitly through
// RegisterBackend's capability probe (internal/runtime/server/server.go binds
// any backend implementing both SemanticIndexProvider and
// SemanticIndexRequestBuilder), and Backend.BuildIndexRequest/ExportIndex are
// the exact surfaces that binding calls. These tests drive the real libclang
// extractor end to end against the pinned LLVM toolchain and assert typed
// facts plus per-fact coverage honesty, so the registered capability is proven
// on the seven-language coverage table rather than assumed from the
// interface assertion.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

// pinnedLLVM22ClangPlus mirrors internal/index/extractors/clang's pinned
// toolchain discovery so this pack proves the same machine-supported path the
// extractor tests run.
func pinnedLLVM22ClangPlus(t *testing.T) string {
	t.Helper()
	path := os.Getenv("OMNILSP_LLVM22_HOME")
	if path == "" {
		path = `D:\Program Files\LLVM-22\clang+llvm-22.1.5-x86_64-pc-windows-msvc`
	}
	compiler := filepath.Join(path, "bin", "clang++.exe")
	if runtime.GOOS != "windows" {
		t.Skip("ccls semantic index fixtures pin the Windows LLVM 22.1.5 layout")
	}
	if _, err := os.Stat(compiler); err != nil {
		t.Skipf("pinned LLVM 22.1.5 is not installed: %v", err)
	}
	return compiler
}

// indexCoverageView is an immutable WorkspaceView + WorkspaceMaterializer over
// a captured fixture tree, mirroring the borrowed-view lifecycle the server
// hands to external extractors.
type indexCoverageView struct {
	rootURI      string
	logicalRoot  string
	snapshotRoot string
	files        map[string]model.File
	data         map[string][]byte
	materialized int
	closed       int
}

func newIndexCoverageView(t *testing.T, logicalRoot, snapshotRoot string, data map[string][]byte) *indexCoverageView {
	t.Helper()
	rootURI := workspaceuri.FromPath(logicalRoot).Canonical()
	view := &indexCoverageView{
		rootURI:      rootURI,
		logicalRoot:  logicalRoot,
		snapshotRoot: snapshotRoot,
		files:        make(map[string]model.File),
		data:         make(map[string][]byte),
	}
	for relative, contents := range data {
		fileURI := workspaceuri.FromPath(filepath.Join(logicalRoot, filepath.FromSlash(relative))).Canonical()
		body := append([]byte(nil), contents...)
		sum := sha256.Sum256(body)
		view.data[fileURI] = body
		view.files[fileURI] = model.File{
			URI:        fileURI,
			LanguageID: "cpp",
			Size:       int64(len(body)),
			SHA256:     identity.ContentHash("sha256:" + hex.EncodeToString(sum[:])),
		}
		destination := filepath.Join(snapshotRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, body, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	return view
}

func (v *indexCoverageView) uriFor(relative string) string {
	return workspaceuri.FromPath(filepath.Join(v.logicalRoot, filepath.FromSlash(relative))).Canonical()
}

func (v *indexCoverageView) Identity() model.Identity {
	return model.Identity{Workspace: "ccls-index-coverage", DiskDigest: "sha256:ccls-index-coverage", SnapshotRev: 1}
}

func (v *indexCoverageView) Walk(ctx context.Context, rootURI string, visit func(model.File) error) error {
	if rootURI != v.rootURI {
		return fmt.Errorf("unexpected walk root %q", rootURI)
	}
	for _, file := range v.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func (v *indexCoverageView) Read(ctx context.Context, fileURI string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := v.data[fileURI]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func (v *indexCoverageView) Materialize(ctx context.Context, rootURI, destination string) (model.MaterializedView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if rootURI != v.rootURI {
		return nil, errors.New("materialize root does not match the captured view")
	}
	v.materialized++
	return &indexCoverageMaterializedView{owner: v, rootURI: rootURI}, nil
}

type indexCoverageMaterializedView struct {
	owner    *indexCoverageView
	rootURI  string
	closedOK bool
}

func (v *indexCoverageMaterializedView) RootURI() string  { return v.rootURI }
func (v *indexCoverageMaterializedView) RootPath() string { return v.owner.snapshotRoot }

func (v *indexCoverageMaterializedView) PathForURI(target string) (string, error) {
	file, ok := v.owner.files[target]
	if !ok {
		return "", os.ErrNotExist
	}
	parsed, err := workspaceuri.Parse(file.URI)
	if err != nil {
		return "", err
	}
	logicalPath, err := parsed.Path()
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(v.owner.logicalRoot, logicalPath)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", os.ErrNotExist
	}
	return filepath.Join(v.owner.snapshotRoot, rel), nil
}

func (v *indexCoverageMaterializedView) Close() error {
	v.closedOK = true
	v.owner.closed++
	return nil
}

type indexCoverageSink struct {
	symbols     []model.Symbol
	occurrences []model.Occurrence
	edges       []model.Edge
}

func (s *indexCoverageSink) WriteSymbols(_ context.Context, values []model.Symbol) error {
	s.symbols = append(s.symbols, values...)
	return nil
}

func (s *indexCoverageSink) WriteOccurrences(_ context.Context, values []model.Occurrence) error {
	s.occurrences = append(s.occurrences, values...)
	return nil
}

func (s *indexCoverageSink) WriteEdges(_ context.Context, values []model.Edge) error {
	s.edges = append(s.edges, values...)
	return nil
}

// cclsIndexCoverageFixture builds one compile-database workspace with two
// translation units sharing api.hpp: derived-class overrides produce
// implementation and type-relation edges, the run() body produces references
// and call edges, and the two distinct -DCTX values force two compile
// contexts so the planner contract is exercised across scopes.
func cclsIndexCoverageFixture(t *testing.T, compiler string) (model.Request, *indexCoverageView) {
	t.Helper()
	logicalRoot := t.TempDir()
	snapshotRoot := t.TempDir()
	files := map[string][]byte{
		"api.hpp": []byte(`#pragma once
struct Base { virtual int value(int) const = 0; };
struct Widget : Base {
  int value(int) const override;
  int score(int) const;
};
`),
		"api.cpp": []byte(`#include "api.hpp"
int Widget::value(int x) const { return x + 1; }
int Widget::score(int x) const { return x + 2; }
`),
		"main.cpp": []byte(`#include "api.hpp"
int run(Widget& w) { return w.value(1) + w.score(2); }
`),
	}
	args := func(source string, define string) []string {
		return []string{compiler, "-std=c++17", "-I", logicalRoot, "-DCTX=" + define, "-c",
			filepath.Join(logicalRoot, source), "-o", filepath.Join(logicalRoot, "obj", strings.TrimSuffix(source, ".cpp")+".obj")}
	}
	db, err := json.Marshal([]map[string]any{
		{"directory": logicalRoot, "file": filepath.Join(logicalRoot, "api.cpp"), "arguments": args("api.cpp", "1")},
		{"directory": logicalRoot, "file": filepath.Join(logicalRoot, "main.cpp"), "arguments": args("main.cpp", "2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	files["compile_commands.json"] = db
	view := newIndexCoverageView(t, logicalRoot, snapshotRoot, files)
	backend := newCclsProtocolBackendOnly(t)
	request, err := backend.BuildIndexRequest(context.Background(), view, view.rootURI)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	return request, view
}

// newCclsProtocolBackendOnly constructs a Backend whose nested connection is
// the fake bridge from bridge_test.go, so the index surface can be driven
// without spawning a real clangd: BuildIndexRequest/ExportIndex never consult
// the bridge, they only need a live Backend identity epoch.
func newCclsProtocolBackendOnly(t *testing.T) *Backend {
	t.Helper()
	backend, _, _ := newCclsProtocolBackend(t)
	return backend
}

func coverageForScope(report model.Report, scopeID string) map[model.FactKind]model.Coverage {
	states := make(map[model.FactKind]model.Coverage)
	for _, item := range report.Coverage {
		if item.ScopeID == scopeID {
			states[item.Fact] = item
		}
	}
	return states
}

func TestCclsBackendBuildIndexRequestPinsCompileContextsAndTools(t *testing.T) {
	compiler := pinnedLLVM22ClangPlus(t)
	request, _ := cclsIndexCoverageFixture(t, compiler)
	if len(request.Scopes) != 2 {
		t.Fatalf("compile database derived %d scopes, want one per distinct compile context", len(request.Scopes))
	}
	for _, scope := range request.Scopes {
		provenance, ok := request.Provenance[scope.ID]
		if !ok {
			t.Fatalf("scope %q has no provenance", scope.ID)
		}
		if provenance.Backend.Language != "cpp" || provenance.Backend.Name != "ccls" {
			t.Fatalf("scope %q binds backend %+v, want the canonical cpp/ccls identity", scope.ID, provenance.Backend)
		}
		if len(provenance.Tools) != 2 {
			t.Fatalf("scope %q pinned %d tools, want compiler and libclang: %+v", scope.ID, len(provenance.Tools), provenance.Tools)
		}
		for _, tool := range provenance.Tools {
			if tool.Version != "22.1.5" {
				t.Errorf("scope %q pinned %s version %q, want the requested LLVM 22.1.5", scope.ID, tool.Name, tool.Version)
			}
			if !filepath.IsAbs(tool.Path) || len(tool.SHA256) != 64 {
				t.Errorf("scope %q pinned %s without an absolute path and content digest: %+v", scope.ID, tool.Name, tool)
			}
		}
		if scope.BuildContext == "" {
			t.Errorf("scope %q derived no build context identity", scope.ID)
		}
	}
}

func TestCclsBackendExportIndexEmitsTypedFactsAndHonestCoverage(t *testing.T) {
	compiler := pinnedLLVM22ClangPlus(t)
	request, view := cclsIndexCoverageFixture(t, compiler)
	backend := newCclsProtocolBackendOnly(t)
	sink := &indexCoverageSink{}
	report, err := backend.ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("ExportIndex: %v", err)
	}
	if report.Identity != view.Identity() {
		t.Fatalf("report identity %+v does not match the captured view %+v", report.Identity, view.Identity())
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}

	valueIDs := make(map[identity.SymbolID]bool)
	scoreIDs := make(map[identity.SymbolID]bool)
	baseID := identity.SymbolID("")
	widgetID := identity.SymbolID("")
	for _, symbol := range sink.symbols {
		switch symbol.Name {
		case "value":
			valueIDs[symbol.ID] = true
		case "score":
			scoreIDs[symbol.ID] = true
		case "Base":
			baseID = symbol.ID
		case "Widget":
			widgetID = symbol.ID
		}
	}
	if len(valueIDs) != 2 || len(scoreIDs) != 1 || baseID == "" || widgetID == "" {
		t.Fatalf("exported symbols are missing the Base/Widget hierarchy and its override: value=%d score=%d Base=%q Widget=%q",
			len(valueIDs), len(scoreIDs), baseID, widgetID)
	}

	widgetValueDecl, widgetValueDef, baseValueDecl := identity.SymbolID(""), identity.SymbolID(""), identity.SymbolID("")
	mainValueReference, mainScoreReference := false, false
	for _, occurrence := range sink.occurrences {
		switch {
		case occurrence.URI == view.uriFor("api.hpp") && occurrence.Role == "declaration" && valueIDs[occurrence.SymbolID]:
			if occurrence.Range.StartLine == 1 {
				baseValueDecl = occurrence.SymbolID
			}
			if occurrence.Range.StartLine == 3 {
				widgetValueDecl = occurrence.SymbolID
			}
		case occurrence.URI == view.uriFor("api.cpp") && occurrence.Role == "definition" && valueIDs[occurrence.SymbolID]:
			widgetValueDef = occurrence.SymbolID
		case occurrence.URI == view.uriFor("main.cpp") && occurrence.Role == "reference" && valueIDs[occurrence.SymbolID]:
			mainValueReference = true
		case occurrence.URI == view.uriFor("main.cpp") && occurrence.Role == "reference" && scoreIDs[occurrence.SymbolID]:
			mainScoreReference = true
		}
	}
	if baseValueDecl == "" || widgetValueDecl == "" || baseValueDecl == widgetValueDecl {
		t.Fatalf("override declarations did not retain distinct Base and Widget identities: Base=%q Widget=%q", baseValueDecl, widgetValueDecl)
	}
	if widgetValueDef == "" || widgetValueDef != widgetValueDecl {
		t.Fatalf("Widget::value definition in api.cpp did not resolve to its header declaration: def=%q decl=%q", widgetValueDef, widgetValueDecl)
	}
	if !mainValueReference || !mainScoreReference {
		t.Fatalf("main.cpp member calls did not resolve as cross-TU references: value=%v score=%v", mainValueReference, mainScoreReference)
	}

	implementationEdge, typeRelationEdge, includeEdge := false, false, false
	callEdgeFromMain := false
	for _, edge := range sink.edges {
		switch edge.Kind {
		case model.EdgeImplementation:
			if edge.SourceURI == view.uriFor("api.hpp") && edge.From == widgetValueDecl && edge.To == baseValueDecl {
				implementationEdge = true
			}
		case model.EdgeTypeRelation:
			if edge.From == widgetID && edge.To == baseID {
				typeRelationEdge = true
			}
		case model.EdgeCall:
			if edge.SourceURI == view.uriFor("main.cpp") && (valueIDs[edge.To] || scoreIDs[edge.To]) {
				callEdgeFromMain = true
			}
		case model.EdgeInclude:
			if edge.SourceURI == view.uriFor("main.cpp") {
				includeEdge = true
			}
		}
	}
	if !implementationEdge {
		t.Fatalf("Widget::value override did not export an implementation edge to Base::value: edges=%+v", sink.edges)
	}
	if !typeRelationEdge {
		t.Fatalf("Widget did not export a type-relation edge to Base: edges=%+v", sink.edges)
	}
	if !callEdgeFromMain {
		t.Fatalf("run() member calls did not export call edges from main.cpp: edges=%+v", sink.edges)
	}
	if !includeEdge {
		t.Fatalf("main.cpp include of api.hpp did not export an include edge: edges=%+v", sink.edges)
	}

	for _, scope := range request.Scopes {
		states := coverageForScope(report, scope.ID)
		if len(states) != len(model.RequiredFactKinds) {
			t.Fatalf("scope %q reported coverage for %d fact kinds, want all %d", scope.ID, len(states), len(model.RequiredFactKinds))
		}
		for _, fact := range []model.FactKind{
			model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference,
			model.FactImplementation, model.FactCall, model.FactInclude, model.FactGenerated,
		} {
			item, ok := states[fact]
			if !ok {
				t.Fatalf("scope %q omitted %s coverage", scope.ID, fact)
			}
			if item.State != model.Complete {
				t.Errorf("scope %q coverage for %s = %s (%s), want complete for the clean fixture", scope.ID, fact, item.State, item.Reason)
			}
		}
		// Class-base relations export as typed edges, but the helper also
		// walks parameter and return types it cannot always anchor, so honest
		// type-relation coverage stays a known subset even for a clean
		// fixture (extractor.go incomplete_type_relation handling).
		typeRelation, ok := states[model.FactTypeRelation]
		if !ok {
			t.Fatalf("scope %q omitted type_relation coverage", scope.ID)
		}
		if typeRelation.State != model.IncompleteKnownSubset {
			t.Errorf("scope %q coverage for type_relation = %s, want incomplete_known_subset", scope.ID, typeRelation.State)
		}
		for _, fact := range []model.FactKind{model.FactImport, model.FactModule} {
			item, ok := states[fact]
			if !ok {
				t.Fatalf("scope %q omitted %s coverage", scope.ID, fact)
			}
			if item.State != model.Unknown {
				t.Errorf("scope %q coverage for %s = %s, want unknown (libclang cannot attest module identity)", scope.ID, fact, item.State)
			}
			if !strings.Contains(item.Reason, "libclang") {
				t.Errorf("scope %q coverage for %s carries an unattested reason %q", scope.ID, fact, item.Reason)
			}
		}
	}
	if view.materialized == 0 || view.closed == 0 {
		t.Fatalf("borrowed view lifecycle: materialize=%d close=%d, want at least one of each", view.materialized, view.closed)
	}
}
