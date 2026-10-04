package pyright

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
)

func TestDirectPyrightRunnerExportsCrossFileFactsFromPinnedAnalyzer(t *testing.T) {
	_, request, view := directCompilerFixture(t, `from .base import Base

class Child(Base):
    pass

def invoke(value: Child) -> int:
    return value.ping()
`)
	provider := NewPinnedSemanticIndexProvider(request.Tools)
	indexRequest, err := provider.BuildIndexRequest(context.Background(), view, request.Scope.RootURI)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	sink := &semanticTestSink{}
	result, err := provider.ExportIndex(context.Background(), indexRequest, sink)
	if err != nil {
		t.Fatalf("ExportIndex: %v", err)
	}
	baseID, childID, invokeID, pingID := "", "", "", ""
	for _, symbol := range sink.symbols {
		switch symbol.Name {
		case "Base":
			baseID = string(symbol.ID)
		case "Child":
			childID = string(symbol.ID)
		case "invoke":
			invokeID = string(symbol.ID)
		case "ping":
			pingID = string(symbol.ID)
		}
	}
	if baseID == "" || childID == "" || invokeID == "" || pingID == "" {
		t.Fatalf("Pyright did not emit declarations across the project: symbols=%+v", sink.symbols)
	}
	if !hasSemanticEdge(sink.edges, childID, baseID, model.EdgeImplementation) {
		t.Errorf("cross-file class base edge missing: %+v", sink.edges)
	}
	if !hasSemanticEdge(sink.edges, invokeID, pingID, model.EdgeCall) {
		t.Errorf("cross-file method call edge missing: %+v", sink.edges)
	}
	if !hasSemanticOccurrenceAt(sink.occurrences, baseID, "file:///repo/direct/pkg/use.py", model.Position{StartLine: 0, StartChar: 18, EndLine: 0, EndChar: 22}, "reference") {
		t.Errorf("cross-file import reference missing: %+v", sink.occurrences)
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference, model.FactCall} {
		if coverageState(result.Coverage, indexRequest.Scopes[0].ID, fact) != model.Complete {
			t.Errorf("coverage for %s = %s, want complete for static fixture", fact, coverageState(result.Coverage, indexRequest.Scopes[0].ID, fact))
		}
	}
	for _, fact := range []model.FactKind{model.FactImplementation, model.FactTypeRelation, model.FactImport, model.FactModule} {
		if coverageState(result.Coverage, indexRequest.Scopes[0].ID, fact) != model.IncompleteKnownSubset {
			t.Errorf("coverage for %s = %s, want incomplete_known_subset", fact, coverageState(result.Coverage, indexRequest.Scopes[0].ID, fact))
		}
	}
}

func TestDirectPyrightRunnerUsesPinnedIncludeRootsAndImportedExcludedSources(t *testing.T) {
	_, semanticRequest, view := directCompilerProjectFixture(t, map[string][]byte{
		"pyrightconfig.json":    []byte(`{"typeCheckingMode":"strict","include":["src/**/*.py"],"exclude":["hidden"]}`),
		"src/main.py":           []byte("from hidden.target import selected\n\ndef run() -> int:\n    return selected()\n"),
		"src/unselected.py":     []byte("def unselected() -> int:\n    return 0\n"),
		"hidden/__init__.py":    []byte(""),
		"hidden/target.py":      []byte("def selected() -> int:\n    return 1\n"),
		"hidden/unused.py":      []byte("def unused_hidden() -> int:\n    return 2\n"),
		"tests/not_included.py": []byte("def not_included() -> int:\n    return 3\n"),
	})
	provider := NewPinnedSemanticIndexProvider(semanticRequest.Tools)
	request, err := provider.BuildIndexRequest(context.Background(), view, semanticRequest.Scope.RootURI)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	sink := &semanticTestSink{}
	report, err := provider.ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("ExportIndex with include/exclude config: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	selectedID, runID, unselectedID := "", "", ""
	for _, symbol := range sink.symbols {
		switch symbol.Name {
		case "selected":
			selectedID = string(symbol.ID)
		case "run":
			runID = string(symbol.ID)
		case "unselected":
			unselectedID = string(symbol.ID)
		case "unused_hidden", "not_included":
			t.Errorf("Pyright exported a non-root, non-dependency declaration %q", symbol.Name)
		}
	}
	if selectedID == "" || runID == "" || unselectedID == "" {
		t.Fatalf("Pyright omitted selected root or imported excluded dependency declarations: %+v", sink.symbols)
	}
	if !hasSemanticOccurrenceAt(sink.occurrences, selectedID, "file:///repo/direct/hidden/target.py", model.Position{StartLine: 0, StartChar: 4, EndLine: 0, EndChar: 12}, "definition") {
		t.Errorf("imported excluded dependency definition was not retained: %+v", sink.occurrences)
	}
	if !hasSemanticOccurrenceAt(sink.occurrences, selectedID, "file:///repo/direct/src/main.py", model.Position{StartLine: 0, StartChar: 26, EndLine: 0, EndChar: 34}, "reference") {
		t.Errorf("reference to imported excluded dependency was not retained: %+v", sink.occurrences)
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference} {
		if got := coverageState(report.Coverage, request.Scopes[0].ID, fact); got != model.Complete {
			t.Errorf("coverage for %s = %s, want complete for the selected roots and captured import closure", fact, got)
		}
	}
}

func TestDirectPyrightRunnerFailsClosedForUnmatchedIncludeSpec(t *testing.T) {
	runner, request, _ := directCompilerProjectFixture(t, map[string][]byte{
		"pyrightconfig.json": []byte(`{"include":["missing-source"]}`),
		"src/main.py":        []byte("def main() -> int:\n    return 1\n"),
	})
	_, err := runner.Export(context.Background(), request, func(PyrightSemanticBatch) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "include configuration did not resolve") {
		t.Fatalf("Export error = %v, want a fail-closed unmatched include diagnostic", err)
	}
}

func TestDirectPyrightRunnerPreservesMultiDeclarationBindingIdentity(t *testing.T) {
	_, semanticRequest, view := directCompilerFixture(t, `def choose(condition: bool):
    if condition:
        value = 1
    else:
        value = "other"
    return value
`)
	provider := NewPinnedSemanticIndexProvider(semanticRequest.Tools)
	request, err := provider.BuildIndexRequest(context.Background(), view, semanticRequest.Scope.RootURI)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	sink := &semanticTestSink{}
	report, err := provider.ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("ExportIndex: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	var valueID identity.SymbolID
	for _, symbol := range sink.symbols {
		if symbol.Name != "value" {
			continue
		}
		if valueID != "" {
			t.Fatalf("Pyright's one multi-declaration binding was split into multiple semantic symbols: %+v", sink.symbols)
		}
		valueID = symbol.ID
	}
	if valueID == "" {
		t.Fatalf("Pyright emitted no semantic symbol for the branch-bound name: %+v", sink.symbols)
	}
	useURI := semanticRequest.Files[2].URI
	for _, occurrence := range []struct {
		position model.Position
		role     string
	}{
		{model.Position{StartLine: 2, StartChar: 8, EndLine: 2, EndChar: 13}, "definition"},
		{model.Position{StartLine: 4, StartChar: 8, EndLine: 4, EndChar: 13}, "definition"},
		{model.Position{StartLine: 5, StartChar: 11, EndLine: 5, EndChar: 16}, "reference"},
	} {
		if !hasSemanticOccurrenceAt(sink.occurrences, string(valueID), useURI, occurrence.position, occurrence.role) {
			t.Errorf("Pyright binding occurrence %+v was not linked to semantic symbol %q: %+v", occurrence, valueID, sink.occurrences)
		}
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference} {
		if got := coverageState(report.Coverage, request.Scopes[0].ID, fact); got != model.Complete {
			t.Errorf("coverage for %s = %s, want complete for statically resolved multi-declaration binding", fact, got)
		}
	}
	moduleGraphNodes := 0
	for _, symbol := range sink.symbols {
		if symbol.Kind == "module" {
			t.Errorf("module graph node %q is exposed as a workspace symbol", symbol.Name)
		}
		if symbol.Kind == "module_graph_node" {
			moduleGraphNodes++
		}
	}
	if moduleGraphNodes == 0 {
		t.Fatalf("Pyright emitted no module graph nodes for the indexed source files: %+v", sink.symbols)
	}
}

func TestDirectPyrightRunnerSeparatesAttributeAndLocalSameNameBindings(t *testing.T) {
	_, semanticRequest, view := directCompilerFixture(t, `from .base import Base

def invoke(value: Base, ping: int) -> int:
    return value.ping() + ping
`)
	provider := NewPinnedSemanticIndexProvider(semanticRequest.Tools)
	request, err := provider.BuildIndexRequest(context.Background(), view, semanticRequest.Scope.RootURI)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	sink := &semanticTestSink{}
	if _, err := provider.ExportIndex(context.Background(), request, sink); err != nil {
		t.Fatalf("ExportIndex: %v", err)
	}
	baseURI := "file:///repo/direct/pkg/base.py"
	useURI := "file:///repo/direct/pkg/use.py"
	// These definition occurrences are emitted from Pyright's own class and
	// function symbol tables, so each use is checked against an independent
	// compiler binding rather than against a spelling-derived expectation.
	methodID := occurrenceSymbolAt(t, sink.occurrences, baseURI, model.Position{StartLine: 1, StartChar: 8, EndLine: 1, EndChar: 12}, "definition")
	localID := occurrenceSymbolAt(t, sink.occurrences, useURI, model.Position{StartLine: 2, StartChar: 24, EndLine: 2, EndChar: 33}, "definition")
	if methodID == "" || localID == "" || methodID == localID {
		t.Fatalf("pinned Pyright declarations did not identify independent method/local bindings: method=%q local=%q occurrences=%+v", methodID, localID, sink.occurrences)
	}
	if got := occurrenceSymbolAt(t, sink.occurrences, useURI, model.Position{StartLine: 3, StartChar: 17, EndLine: 3, EndChar: 21}, "reference"); got != methodID {
		t.Errorf("Pyright attribute reference resolved to %q, want method binding %q", got, methodID)
	}
	if got := occurrenceSymbolAt(t, sink.occurrences, useURI, model.Position{StartLine: 3, StartChar: 26, EndLine: 3, EndChar: 30}, "reference"); got != localID {
		t.Errorf("Pyright lexical reference resolved to %q, want local binding %q", got, localID)
	}
	invokeID := occurrenceSymbolAt(t, sink.occurrences, useURI, model.Position{StartLine: 2, StartChar: 4, EndLine: 2, EndChar: 10}, "definition")
	if invokeID == "" || !hasSemanticEdge(sink.edges, string(invokeID), string(methodID), model.EdgeCall) {
		t.Errorf("Pyright member call is not linked to its method declaration %q: %+v", methodID, sink.edges)
	}
}

func occurrenceSymbolAt(t *testing.T, occurrences []model.Occurrence, uri string, position model.Position, role string) identity.SymbolID {
	t.Helper()
	for _, occurrence := range occurrences {
		if occurrence.URI == uri && occurrence.Range == position && occurrence.Role == role {
			return occurrence.SymbolID
		}
	}
	return ""
}

func TestDirectPyrightRunnerLinksNestedProjectTargetsToTheirOwningBuildContext(t *testing.T) {
	_, directRequest, _ := directCompilerFixture(t, "pass\n")
	tools := directRequest.Tools
	workspaceRoot := "file:///repo/python-projects"
	childRoot := workspaceRoot + "/child"
	mainURI := workspaceRoot + "/main.py"
	baseURI := childRoot + "/base.py"
	parentConfigURI := workspaceRoot + "/pyrightconfig.json"
	childConfigURI := childRoot + "/pyrightconfig.json"
	parentConfig := []byte(`{"typeCheckingMode":"strict"}`)
	childConfig := []byte(`{"typeCheckingMode":"strict"}`)
	main := []byte("from child.base import Base\n\ndef use(value: Base) -> int:\n    return value.ping()\n")
	base := []byte("class Base:\n    def ping(self) -> int:\n        return 1\n")
	workspaceFiles := []model.File{
		semanticFile(parentConfigURI, "json", parentConfig),
		semanticFile(mainURI, "python", main),
		semanticFile(childConfigURI, "json", childConfig),
		semanticFile(baseURI, "python", base),
	}
	childFiles := []model.File{
		semanticFile(childConfigURI, "json", childConfig),
		semanticFile(baseURI, "python", base),
	}
	view := &semanticTestView{
		id:    model.Identity{Workspace: "py-projects", DiskDigest: identity.ContentHash("sha256:" + strings.Repeat("b", 64)), SnapshotRev: 9},
		files: map[string][]model.File{workspaceRoot: workspaceFiles, childRoot: childFiles},
		content: map[string][]byte{
			parentConfigURI: parentConfig, childConfigURI: childConfig,
			mainURI: main, baseURI: base,
		},
	}
	provider := NewPinnedSemanticIndexProvider(tools)
	request, err := provider.BuildIndexRequest(context.Background(), view, workspaceRoot)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	if len(request.Scopes) != 2 {
		t.Fatalf("discovered Python scopes = %d, want parent and nested project: %+v", len(request.Scopes), request.Scopes)
	}
	var parentScope, childScope model.Scope
	for _, scope := range request.Scopes {
		if scope.RootURI == workspaceRoot {
			parentScope = scope
		}
		if scope.RootURI == childRoot {
			childScope = scope
		}
	}
	if parentScope.ID == "" || childScope.ID == "" || parentScope.BuildContext == childScope.BuildContext {
		t.Fatalf("parent and child Python build contexts are not distinct: parent=%+v child=%+v", parentScope, childScope)
	}
	sink := &semanticTestSink{}
	report, err := provider.ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("ExportIndex with pinned Pyright: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	var baseID identity.SymbolID
	for _, symbol := range sink.symbols {
		if symbol.ScopeID == childScope.ID && symbol.Name == "Base" {
			baseID = symbol.ID
		}
	}
	if baseID == "" {
		t.Fatalf("pinned Pyright did not emit the nested project's Base declaration: %+v", sink.symbols)
	}
	var importedTarget bool
	for _, occurrence := range sink.occurrences {
		if occurrence.ScopeID == parentScope.ID && occurrence.URI == mainURI && occurrence.Role == "reference" && occurrence.SymbolID == baseID {
			importedTarget = true
		}
	}
	if !importedTarget {
		t.Fatalf("real Pyright target occurrence did not link to the child project's semantic ID %q: %+v", baseID, sink.occurrences)
	}
	if !strings.Contains(string(baseID), string(childScope.BuildContext)) || strings.Contains(string(baseID), string(parentScope.BuildContext)) {
		t.Fatalf("target semantic ID does not carry its owning build context: %q", baseID)
	}
	var linkedCall bool
	for _, edge := range sink.edges {
		if edge.ScopeID == parentScope.ID && edge.Kind == model.EdgeCall {
			linkedCall = true
		}
	}
	if !linkedCall {
		t.Fatalf("pinned Pyright emitted no non-empty call fact for the importing project: %+v", sink.edges)
	}
}

func TestDirectPyrightRunnerMarksDynamicProjectIncomplete(t *testing.T) {
	runner, request, _ := directCompilerFixture(t, `from .base import Base

def invoke(name: str) -> object:
    return getattr(Base, name)()
`)
	result, err := runner.Export(context.Background(), request, func(PyrightSemanticBatch) error { return nil })
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference, model.FactImplementation, model.FactTypeRelation, model.FactCall, model.FactImport, model.FactModule} {
		if coverageState(result.Coverage, request.Scope.ID, fact) != model.IncompleteKnownSubset {
			t.Errorf("coverage for %s = %s, want incomplete_known_subset", fact, coverageState(result.Coverage, request.Scope.ID, fact))
		}
	}
}

func TestDirectPyrightRunnerKeepsUnanchoredAnyReferenceIncomplete(t *testing.T) {
	runner, request, _ := directCompilerFixture(t, `from typing import Any

def unchecked(value: Any) -> Any:
    return value
`)
	result, err := runner.Export(context.Background(), request, func(PyrightSemanticBatch) error { return nil })
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := coverageState(result.Coverage, request.Scope.ID, model.FactReference); got != model.IncompleteKnownSubset {
		t.Errorf("coverage for %s = %s, want incomplete_known_subset while Any is unanchored to the immutable source closure", model.FactReference, got)
	}
}

func TestDirectPyrightRunnerDoesNotTreatBuiltinNamedMemberAsBuiltin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		source   string
		wantRef  model.Completeness
		wantCall model.Completeness
	}{
		{
			name: "unresolved member named len",
			source: `from .base import Base

def invoke(box: Base) -> int:
    return box.len()
`,
			wantRef:  model.IncompleteKnownSubset,
			wantCall: model.IncompleteKnownSubset,
		},
		{
			name: "unqualified len builtin",
			source: `def count(values: list[int]) -> int:
    return len(values)
`,
			wantRef:  model.Complete,
			wantCall: model.Complete,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, request, _ := directCompilerFixture(t, tc.source)
			result, err := runner.Export(context.Background(), request, func(PyrightSemanticBatch) error { return nil })
			if err != nil {
				t.Fatalf("Export: %v", err)
			}
			if got := coverageState(result.Coverage, request.Scope.ID, model.FactReference); got != tc.wantRef {
				t.Errorf("reference coverage = %s, want %s", got, tc.wantRef)
			}
			if got := coverageState(result.Coverage, request.Scope.ID, model.FactCall); got != tc.wantCall {
				t.Errorf("call coverage = %s, want %s", got, tc.wantCall)
			}
		})
	}
}

func directCompilerFixture(t *testing.T, useSource string) (*DirectPyrightRunner, PyrightSemanticRequest, *semanticTestView) {
	t.Helper()
	return directCompilerProjectFixture(t, map[string][]byte{
		"pyrightconfig.json": []byte(`{"typeCheckingMode":"strict"}`),
		"pkg/base.py":        []byte("class Base:\n    def ping(self) -> int:\n        return 1\n"),
		"pkg/use.py":         []byte(useSource),
	})
}

func directCompilerProjectFixture(t *testing.T, contents map[string][]byte) (*DirectPyrightRunner, PyrightSemanticRequest, *semanticTestView) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "project")
	if len(contents["pyrightconfig.json"]) == 0 {
		t.Fatal("direct Pyright fixture requires pyrightconfig.json")
	}
	rootURI := "file:///repo/direct"
	paths := make([]string, 0, len(contents))
	for name := range contents {
		paths = append(paths, filepath.ToSlash(name))
	}
	sort.Slice(paths, func(i, j int) bool {
		if paths[i] == "pyrightconfig.json" {
			return paths[j] != "pyrightconfig.json"
		}
		if paths[j] == "pyrightconfig.json" {
			return false
		}
		return paths[i] < paths[j]
	})
	files := make([]model.File, 0, len(paths))
	content := make(map[string][]byte, len(paths))
	for _, name := range paths {
		localPath := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
			t.Fatal(err)
		}
		body := append([]byte(nil), contents[name]...)
		writeFixtureFile(t, localPath, body)
		language := "python"
		if strings.EqualFold(filepath.Ext(name), ".json") {
			language = "json"
		}
		fileURI := rootURI + "/" + name
		files = append(files, semanticFile(fileURI, language, body))
		content[fileURI] = body
	}
	configBytes := contents["pyrightconfig.json"]
	view := &semanticTestView{
		id:    model.Identity{Workspace: identity.WorkspaceID("direct-pyright"), DiskDigest: identity.ContentHash("sha256:" + strings.Repeat("a", 64)), SnapshotRev: 1},
		files: map[string][]model.File{rootURI: files}, content: content,
	}
	materialized, err := view.Materialize(context.Background(), rootURI, root)
	if err != nil {
		t.Fatal(err)
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	nodePath, err = filepath.Abs(nodePath)
	if err != nil {
		t.Fatal(err)
	}
	_, sourceFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
	pyrightRoot := filepath.Join(repoRoot, "test", "acceptance", "tools", "node_modules", "pyright")
	internalPath := filepath.Join(pyrightRoot, "dist", "pyright-internal.js")
	vendorPath := filepath.Join(pyrightRoot, "dist", "vendor.js")
	for _, toolPath := range []string{internalPath, vendorPath} {
		if _, err := os.Stat(toolPath); err != nil {
			t.Skipf("pinned Pyright tool bundle is not installed: %v", err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	tools := []model.ToolIdentity{
		{Name: semanticIndexRuntimeName, Version: semanticIndexRuntimeVersion, Path: nodePath},
		{Name: semanticIndexCompilerName, Version: semanticIndexCompilerVersion, Path: internalPath, SHA256: pyrightInternalSHA256},
		{Name: semanticIndexVendorName, Version: semanticIndexCompilerVersion, Path: vendorPath, SHA256: pyrightVendorSHA256},
		{Name: semanticIndexExporterName, Version: semanticIndexExtractorVersion, Path: executable},
	}
	for i := range tools {
		if tools[i].SHA256 != "" {
			continue
		}
		tools[i].SHA256, err = fileSHA256(tools[i].Path)
		if err != nil {
			t.Fatal(err)
		}
	}
	_, parsedOptions, extraPaths := parseSupportedPyrightConfig(configBytes)
	options := map[string]string{
		"pyrightConfig":       "pyrightconfig.json",
		"pyrightConfigDigest": "sha256:" + digestBytes(configBytes),
		"stubPath":            "none",
		"pythonVersion":       "3.13",
	}
	for key, value := range parsedOptions {
		options[key] = value
	}
	pythonPath, err := exec.LookPath("python")
	if err != nil {
		pythonPath, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python interpreter is not installed")
	}
	pythonPath, err = filepath.Abs(pythonPath)
	if err != nil {
		t.Fatal(err)
	}
	pythonVersionOutput, err := exec.Command(pythonPath, "--version").CombinedOutput()
	if err != nil {
		t.Skipf("Python interpreter cannot be probed: %v", err)
	}
	versionFields := strings.Fields(strings.TrimSpace(string(pythonVersionOutput)))
	if len(versionFields) < 2 {
		t.Fatalf("unexpected Python version output %q", pythonVersionOutput)
	}
	pythonTool := model.ToolIdentity{Name: "python", Version: versionFields[len(versionFields)-1], Path: pythonPath}
	pythonTool.SHA256, err = fileSHA256(pythonPath)
	if err != nil {
		t.Fatal(err)
	}
	tools = append(tools, pythonTool)
	scope := model.Scope{ID: "direct-pyright", Language: langID, RootURI: rootURI,
		Build: model.BuildInputs{
			Environment:  map[string]string{"pythonInterpreter": pythonPath},
			IncludePaths: append([]string{"."}, extraPaths...), Options: options,
		}}
	scope.BuildContext = model.ComputeBuildContextID(scope, semanticIndexExtractor, semanticIndexExtractorVersion, semanticIndexToolchain, tools)
	runner := NewDirectPyrightRunner(nodePath)
	request := PyrightSemanticRequest{
		Scope: scope, Files: files, Materialized: materialized, Tools: tools,
	}
	return runner, request, view
}

func hasSemanticEdge(edges []model.Edge, from, to string, kind model.EdgeKind) bool {
	for _, edge := range edges {
		if string(edge.From) == from && string(edge.To) == to && edge.Kind == kind {
			return true
		}
	}
	return false
}

func hasSemanticOccurrenceAt(occurrences []model.Occurrence, symbol, uri string, position model.Position, role string) bool {
	for _, occurrence := range occurrences {
		if string(occurrence.SymbolID) == symbol && occurrence.URI == uri && occurrence.Range == position && occurrence.Role == role {
			return true
		}
	}
	return false
}

func writeFixtureFile(t *testing.T, name string, content []byte) {
	t.Helper()
	if err := os.WriteFile(name, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func digestBytes(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func fileSHA256(name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}
