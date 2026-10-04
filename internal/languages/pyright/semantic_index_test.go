package pyright

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
)

type semanticTestView struct {
	id      model.Identity
	files   map[string][]model.File
	content map[string][]byte
}

type borrowedSemanticTestView struct {
	*semanticTestView
	rootPath string
}

func (v borrowedSemanticTestView) Materialize(ctx context.Context, rootURI, _ string) (model.MaterializedView, error) {
	return v.semanticTestView.Materialize(ctx, rootURI, v.rootPath)
}

func (v *semanticTestView) Identity() model.Identity { return v.id }

func (v *semanticTestView) Walk(ctx context.Context, root string, visit func(model.File) error) error {
	files := append([]model.File(nil), v.files[root]...)
	sort.Slice(files, func(i, j int) bool { return files[i].URI < files[j].URI })
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func (v *semanticTestView) Read(ctx context.Context, uri string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, ok := v.content[uri]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (v *semanticTestView) Materialize(ctx context.Context, rootURI, destination string) (model.MaterializedView, error) {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return nil, err
	}
	root, err := url.Parse(rootURI)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]string, len(v.files[rootURI]))
	for _, file := range v.files[rootURI] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		parsed, err := url.Parse(file.URI)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(filepath.FromSlash(root.Path), filepath.FromSlash(parsed.Path))
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("file outside root: %s", file.URI)
		}
		localPath := filepath.Join(destination, relative)
		if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(localPath, v.content[file.URI], 0o600); err != nil {
			return nil, err
		}
		paths[file.URI] = localPath
	}
	return semanticTestMaterialized{rootURI: rootURI, rootPath: destination, paths: paths}, nil
}

type semanticTestMaterialized struct {
	rootURI, rootPath string
	paths             map[string]string
}

func (v semanticTestMaterialized) RootURI() string  { return v.rootURI }
func (v semanticTestMaterialized) RootPath() string { return v.rootPath }
func (v semanticTestMaterialized) Close() error     { return nil }
func (v semanticTestMaterialized) PathForURI(uri string) (string, error) {
	path, ok := v.paths[uri]
	if !ok {
		return "", os.ErrNotExist
	}
	return path, nil
}

type semanticTestRunner struct {
	batches        []PyrightSemanticBatch
	batchesByScope map[string][]PyrightSemanticBatch
	coverage       []model.Coverage
	verifyErr      error
	exportErr      error
	cancel         context.CancelFunc
	projectScopes  map[string][]model.Scope
	verifyCall     int
	exportCall     int
}

func (r *semanticTestRunner) VerifyTools(_ context.Context, request PyrightSemanticRequest) ([]model.ToolIdentity, error) {
	r.verifyCall++
	if r.verifyErr != nil {
		return nil, r.verifyErr
	}
	if request.Materialized == nil || request.Materialized.RootPath() == "" {
		return nil, errors.New("missing materialized view")
	}
	return append([]model.ToolIdentity(nil), request.Tools...), nil
}

func (r *semanticTestRunner) Export(ctx context.Context, request PyrightSemanticRequest, emit func(PyrightSemanticBatch) error) (PyrightSemanticResult, error) {
	r.exportCall++
	if r.projectScopes == nil {
		r.projectScopes = make(map[string][]model.Scope)
	}
	r.projectScopes[request.Scope.ID] = append([]model.Scope(nil), request.ProjectScopes...)
	if r.exportErr != nil {
		return PyrightSemanticResult{}, r.exportErr
	}
	batches := r.batches
	if scoped, ok := r.batchesByScope[request.Scope.ID]; ok {
		batches = scoped
	}
	for _, batch := range batches {
		if err := emit(batch); err != nil {
			return PyrightSemanticResult{}, err
		}
	}
	if r.cancel != nil {
		r.cancel()
		if err := ctx.Err(); err != nil {
			return PyrightSemanticResult{}, err
		}
	}
	coverage := append([]model.Coverage(nil), r.coverage...)
	for i := range coverage {
		coverage[i].ScopeID = request.Scope.ID
	}
	return PyrightSemanticResult{Coverage: coverage}, nil
}

type semanticTestSink struct {
	symbols     []model.Symbol
	occurrences []model.Occurrence
	edges       []model.Edge
	maxBatch    int
}

func (s *semanticTestSink) WriteSymbols(_ context.Context, values []model.Symbol) error {
	s.maxBatch = max(s.maxBatch, len(values))
	s.symbols = append(s.symbols, values...)
	return nil
}
func (s *semanticTestSink) WriteOccurrences(_ context.Context, values []model.Occurrence) error {
	s.maxBatch = max(s.maxBatch, len(values))
	s.occurrences = append(s.occurrences, values...)
	return nil
}
func (s *semanticTestSink) WriteEdges(_ context.Context, values []model.Edge) error {
	s.maxBatch = max(s.maxBatch, len(values))
	s.edges = append(s.edges, values...)
	return nil
}

func TestSemanticIndexPyrightCompilerFixturePreservesFactsAndSnapshotLocations(t *testing.T) {
	root := "file:///repo/python"
	baseURI := root + "/pkg/base.py"
	useURI := root + "/pkg/use.py"
	generatedURI := root + "/generated/api.py"
	baseContent := []byte("# café\r\nclass Base:\r\n    pass\r\n")
	useContent := []byte("from pkg.base import Base\r\nclass Child(Base):\r\n    def call(self, x: Base):\r\n        return x.run()\r\n")
	generatedContent := []byte("class G(Base):\r\n    pass\r\n")
	baseFile := semanticFile(baseURI, "python", baseContent)
	useFile := semanticFile(useURI, "python", useContent)
	generatedFile := semanticFile(generatedURI, "python", generatedContent)
	generatedFile.Generated = true
	generatedFile.SourceURI = useURI
	generatedFile.SourceMap = []model.SourceMapSpan{{
		Generated: model.Position{StartLine: 0, StartChar: 8, EndLine: 0, EndChar: 12},
		SourceURI: useURI,
		Source:    model.Position{StartLine: 1, StartChar: 12, EndLine: 1, EndChar: 16},
	}}
	view := &semanticTestView{
		id:    model.Identity{Workspace: "py-fixture", DiskDigest: "sha256:fixture", SnapshotRev: 17},
		files: map[string][]model.File{root: {baseFile, useFile, generatedFile}},
		content: map[string][]byte{
			baseURI: baseContent, useURI: useContent, generatedURI: generatedContent,
		},
	}
	scope, provenance := pythonScope(root, "pkg", view.id, semanticTools(t))
	coverage := completeCoverage(scope.ID)
	runner := &semanticTestRunner{
		coverage: coverage,
		batches: []PyrightSemanticBatch{{
			Symbols: []model.Symbol{
				{ID: "pyright/1.1.414/Base", Name: "Base", Kind: "class"},
				{ID: "pyright/1.1.414/Child", Name: "Child", Kind: "class"},
				{ID: "pyright/1.1.414/call", Name: "call", Kind: "method"},
				{ID: "pyright/1.1.414/run", Name: "run", Kind: "method"},
				{ID: "pyright/1.1.414/pkg", Name: "pkg.base", Kind: "module"},
			},
			Occurrences: []model.Occurrence{
				{SymbolID: "pyright/1.1.414/Base", URI: baseURI, Range: model.Position{StartLine: 1, StartChar: 6, EndLine: 1, EndChar: 10}, Role: "definition"},
				{SymbolID: "pyright/1.1.414/Base", URI: useURI, Range: model.Position{StartLine: 0, StartChar: 21, EndLine: 0, EndChar: 25}, Role: "reference"},
				{SymbolID: "pyright/1.1.414/Child", URI: useURI, Range: model.Position{StartLine: 1, StartChar: 6, EndLine: 1, EndChar: 11}, Role: "definition"},
				{SymbolID: "pyright/1.1.414/Base", URI: generatedURI, Range: model.Position{StartLine: 0, StartChar: 8, EndLine: 0, EndChar: 12}, Role: "reference"},
			},
			Edges: []model.Edge{
				{From: "pyright/1.1.414/Child", To: "pyright/1.1.414/Base", ScopeID: scope.ID, Kind: model.EdgeImplementation, SourceURI: useURI, Range: model.Position{StartLine: 1, StartChar: 12, EndLine: 1, EndChar: 16}},
				{From: "pyright/1.1.414/Child", To: "pyright/1.1.414/Base", Kind: model.EdgeTypeRelation, SourceURI: useURI, Range: model.Position{StartLine: 1, StartChar: 12, EndLine: 1, EndChar: 16}},
				{From: "pyright/1.1.414/call", To: "pyright/1.1.414/run", Kind: model.EdgeCall, SourceURI: useURI, Range: model.Position{StartLine: 3, StartChar: 13, EndLine: 3, EndChar: 16}},
				{From: "pyright/1.1.414/call", To: "pyright/1.1.414/Base", Kind: model.EdgeImport, SourceURI: useURI, Range: model.Position{StartLine: 0, StartChar: 21, EndLine: 0, EndChar: 25}},
				{From: "pyright/1.1.414/call", To: "pyright/1.1.414/pkg", Kind: model.EdgeModule, SourceURI: useURI, Range: model.Position{StartLine: 0, StartChar: 5, EndLine: 0, EndChar: 13}},
			},
		}},
	}
	sink := &semanticTestSink{}
	got, err := testSemanticProvider(runner, provenance.Tools).ExportIndex(context.Background(), model.Request{
		View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, sink)
	if err != nil {
		t.Fatalf("ExportIndex: %v", err)
	}
	if err := model.ValidateReport(model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}, got); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	if runner.verifyCall != 1 || runner.exportCall != 1 || sink.maxBatch > semanticIndexBatchLimit {
		t.Fatalf("runner calls=(%d,%d), max batch=%d", runner.verifyCall, runner.exportCall, sink.maxBatch)
	}
	if len(sink.symbols) != 5 || len(sink.occurrences) != 4 || len(sink.edges) != 5 {
		t.Fatalf("fact counts symbols/occurrences/edges = %d/%d/%d", len(sink.symbols), len(sink.occurrences), len(sink.edges))
	}
	for _, symbol := range sink.symbols {
		if symbol.ScopeID != scope.ID || !strings.HasPrefix(string(symbol.ID), "pyright/1.1.414/") {
			t.Errorf("symbol semantic identity was not preserved: %+v", symbol)
		}
	}
	var mapped, crossFile bool
	for _, occurrence := range sink.occurrences {
		if occurrence.URI == useURI && occurrence.Range == (model.Position{StartLine: 1, StartChar: 12, EndLine: 1, EndChar: 16}) && occurrence.SourceHash == useFile.SHA256 {
			mapped = true
		}
		if occurrence.URI == useURI && occurrence.Range.StartLine == 0 && occurrence.Role == "reference" {
			crossFile = true
		}
		if occurrence.BuildContext != scope.BuildContext || occurrence.ScopeID != scope.ID {
			t.Errorf("occurrence lost context: %+v", occurrence)
		}
	}
	if !mapped || !crossFile {
		t.Errorf("generated source-map or cross-file location was not preserved: mapped=%v crossFile=%v", mapped, crossFile)
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference, model.FactImplementation, model.FactTypeRelation, model.FactCall, model.FactImport, model.FactModule} {
		if coverageState(got.Coverage, scope.ID, fact) != model.Complete {
			t.Errorf("coverage for %s = %s, want runner-attested completeness", fact, coverageState(got.Coverage, scope.ID, fact))
		}
	}
	if coverageState(got.Coverage, scope.ID, model.FactGenerated) != model.IncompleteKnownSubset {
		t.Errorf("generated-source coverage = %s, want incomplete without exhaustive source-map attestation", coverageState(got.Coverage, scope.ID, model.FactGenerated))
	}
	if coverageState(got.Coverage, scope.ID, model.FactInclude) != model.Unavailable {
		t.Errorf("include coverage = %s, want unavailable", coverageState(got.Coverage, scope.ID, model.FactInclude))
	}
}

func TestSemanticIndexPyrightMultipleContextsAndIncompleteConfig(t *testing.T) {
	view, scopes, provenances := pythonMultiScopeFixture(t)
	runner := &semanticTestRunner{
		coverage: completeCoverage(scopes[0].ID),
		batchesByScope: map[string][]PyrightSemanticBatch{
			scopes[0].ID: {{Symbols: []model.Symbol{{ID: "pyright/1.1.414/project-a entry", Name: "entry", Kind: "function"}}}},
			scopes[1].ID: {{Symbols: []model.Symbol{{ID: "pyright/1.1.414/project-b entry", Name: "entry", Kind: "function"}}}},
		},
	}
	sink := &semanticTestSink{}
	request := model.Request{View: view, Scopes: scopes, Provenance: provenances}
	got, err := testSemanticProvider(runner, provenances[scopes[0].ID].Tools).ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("ExportIndex: %v", err)
	}
	if len(sink.symbols) != 2 || sink.symbols[0].ID == sink.symbols[1].ID || sink.symbols[0].ScopeID == sink.symbols[1].ScopeID {
		t.Fatalf("same raw symbol was not isolated by project context: %+v", sink.symbols)
	}
	if len(got.UsedTools[scopes[0].ID]) != 2 || len(got.UsedTools[scopes[1].ID]) != 2 {
		t.Fatalf("used tools not recorded per context: %+v", got.UsedTools)
	}

	incomplete := scopes[0]
	incomplete.Build.Options = map[string]string{"pythonVersion": "3.12"}
	badScope, badProvenance := pythonScope("file:///repo/incomplete", "broken", view.id, provenances[scopes[0].ID].Tools)
	badScope.Build.Options = incomplete.Build.Options
	badProvenance.Scope = badScope
	badScope.BuildContext = model.ComputeBuildContextID(badScope, badProvenance.Extractor, badProvenance.ExtractorVer, badProvenance.Toolchain, badProvenance.Tools)
	badProvenance.Scope = badScope
	badView := &semanticTestView{id: view.id, files: map[string][]model.File{badScope.RootURI: {}}, content: map[string][]byte{}}
	badRequest := model.Request{View: badView, Scopes: []model.Scope{badScope}, Provenance: map[string]model.Provenance{badScope.ID: badProvenance}}
	badReport, err := testSemanticProvider(runner, badProvenance.Tools).ExportIndex(context.Background(), badRequest, &semanticTestSink{})
	if err != nil {
		t.Fatalf("incomplete config should be reported as unavailable: %v", err)
	}
	if runner.verifyCall != 2 || runner.exportCall != 2 {
		t.Fatalf("incomplete configuration invoked tools: verify=%d export=%d", runner.verifyCall, runner.exportCall)
	}
	for _, fact := range model.RequiredFactKinds {
		if coverageState(badReport.Coverage, badScope.ID, fact) != model.Unknown {
			t.Errorf("incomplete configuration coverage %s = %s, want unknown", fact, coverageState(badReport.Coverage, badScope.ID, fact))
		}
	}
}

func TestSemanticIndexPyrightPreservesCrossProjectSemanticIDs(t *testing.T) {
	view, scopes, provenances := pythonMultiScopeFixture(t)
	sourceID := identity.SymbolID("pyright/1.1.414/project-a/entry@" + string(scopes[0].BuildContext))
	targetID := identity.SymbolID("pyright/1.1.414/project-b/base@" + string(scopes[1].BuildContext))
	sourceURI := scopes[0].RootURI + "/main.py"
	runner := &semanticTestRunner{
		coverage: completeCoverage(scopes[0].ID),
		batchesByScope: map[string][]PyrightSemanticBatch{
			scopes[0].ID: {{
				Symbols:     []model.Symbol{{ID: sourceID, Name: "entry", Kind: "function"}},
				Occurrences: []model.Occurrence{{SymbolID: targetID, URI: sourceURI, Range: model.Position{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 5}, Role: "reference"}},
				Edges:       []model.Edge{{From: sourceID, To: targetID, Kind: model.EdgeImport, SourceURI: sourceURI, Range: model.Position{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 5}}},
			}},
			scopes[1].ID: {{Symbols: []model.Symbol{{ID: targetID, Name: "base", Kind: "function"}}}},
		},
	}
	sink := &semanticTestSink{}
	request := model.Request{View: view, Scopes: scopes, Provenance: provenances}
	got, err := testSemanticProvider(runner, provenances[scopes[0].ID].Tools).ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("ExportIndex: %v", err)
	}
	if len(got.Coverage) != len(scopes)*len(model.RequiredFactKinds) {
		t.Fatalf("coverage matrix size = %d", len(got.Coverage))
	}
	if len(sink.edges) != 1 || sink.edges[0].ScopeID != scopes[0].ID || sink.edges[0].From != sourceID || sink.edges[0].To != targetID {
		t.Fatalf("cross-project edge semantic IDs were rewritten: %+v", sink.edges)
	}
	if len(sink.occurrences) != 1 || sink.occurrences[0].SymbolID != targetID || sink.occurrences[0].ScopeID != scopes[0].ID {
		t.Fatalf("cross-project occurrence target was rewritten: %+v", sink.occurrences)
	}
	if len(sink.symbols) != 2 || sink.symbols[1].ID != targetID || sink.symbols[1].ScopeID != scopes[1].ID {
		t.Fatalf("target declaration does not match edge target: %+v", sink.symbols)
	}
	projectScopes := runner.projectScopes[scopes[0].ID]
	if len(projectScopes) != 2 || projectScopes[1].BuildContext != scopes[1].BuildContext {
		t.Fatalf("runner did not receive target project context: %+v", projectScopes)
	}
}

func TestSemanticIndexPyrightCancellationStopsExport(t *testing.T) {
	view, scope, provenance := pythonOneFileFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	runner := &semanticTestRunner{coverage: completeCoverage(scope.ID), cancel: cancel}
	_, err := testSemanticProvider(runner, provenance.Tools).ExportIndex(ctx, model.Request{
		View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, &semanticTestSink{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ExportIndex error = %v, want context.Canceled", err)
	}
}

func TestSemanticIndexPyrightRejectsFloatingCompilerVersion(t *testing.T) {
	tools := semanticTools(t)
	tools[0].Version = "latest"
	root, uri := "file:///repo/floating", "file:///repo/floating/main.py"
	content := []byte("def main(): return 1\r\n")
	view := &semanticTestView{
		id:    model.Identity{Workspace: "py-floating", DiskDigest: "sha256:floating", SnapshotRev: 3},
		files: map[string][]model.File{root: {semanticFile(uri, "python", content)}}, content: map[string][]byte{uri: content},
	}
	scope, provenance := pythonScope(root, "floating", view.id, tools)
	runner := &semanticTestRunner{}
	got, err := testSemanticProvider(runner, tools).ExportIndex(context.Background(), model.Request{
		View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, &semanticTestSink{})
	if err != nil {
		t.Fatalf("floating tool config should fail closed as unavailable: %v", err)
	}
	if runner.verifyCall != 0 || runner.exportCall != 0 {
		t.Fatalf("floating version invoked tool: verify=%d export=%d", runner.verifyCall, runner.exportCall)
	}
	for _, fact := range model.RequiredFactKinds {
		if coverageState(got.Coverage, scope.ID, fact) != model.Unavailable {
			t.Errorf("coverage %s = %s, want unavailable", fact, coverageState(got.Coverage, scope.ID, fact))
		}
	}
}

func TestSemanticIndexPyrightRecordsVerifiedToolsOnExportFailure(t *testing.T) {
	view, scope, provenance := pythonOneFileFixture(t)
	wantErr := errors.New("export failed")
	runner := &semanticTestRunner{exportErr: wantErr}
	got, err := testSemanticProvider(runner, provenance.Tools).ExportIndex(context.Background(), model.Request{
		View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, &semanticTestSink{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("ExportIndex error = %v, want %v", err, wantErr)
	}
	if len(got.UsedTools[scope.ID]) != len(provenance.Tools) {
		t.Fatalf("verified tools lost after export error: %+v", got.UsedTools)
	}
}

func TestSemanticIndexPyrightAcceptsBorrowedReadOnlyMaterializedView(t *testing.T) {
	source, scope, provenance := pythonOneFileFixture(t)
	rootPath := t.TempDir()
	view := borrowedSemanticTestView{semanticTestView: source, rootPath: rootPath}
	runner := &semanticTestRunner{coverage: completeCoverage(scope.ID)}
	_, err := testSemanticProvider(runner, provenance.Tools).ExportIndex(context.Background(), model.Request{
		View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, &semanticTestSink{})
	if err != nil {
		t.Fatalf("ExportIndex with borrowed materialized view: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "main.py")); err != nil {
		t.Fatalf("provider removed borrowed RootPath: %v", err)
	}
}

func TestSemanticIndexRequestBuilderPinsMultiplePythonProjects(t *testing.T) {
	view, scopes, _ := pythonMultiScopeFixture(t)
	tools := semanticTools(t)
	provider := NewSemanticIndexProvider(SemanticIndexConfig{
		Tools: tools,
		BuildScopes: func(_ context.Context, _ model.WorkspaceView, _ string) ([]model.Scope, error) {
			return scopes, nil
		},
	})
	request, err := provider.BuildIndexRequest(context.Background(), view, "file:///repo")
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	if len(request.Scopes) != 2 {
		t.Fatalf("scopes = %d, want 2", len(request.Scopes))
	}
	for _, scope := range request.Scopes {
		provenance := request.Provenance[scope.ID]
		if !reflect.DeepEqual(provenance.Scope, scope) || len(provenance.Tools) != 2 ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			t.Errorf("request did not pin project context/tool identities: scope=%+v provenance=%+v", scope, provenance)
		}
	}
	if request.Scopes[0].BuildContext == request.Scopes[1].BuildContext {
		t.Fatal("different Python project roots shared one build context")
	}
}

func TestSemanticIndexRequestBuilderAllowsMissingToolPinsToFailClosed(t *testing.T) {
	view, scopes, _ := pythonMultiScopeFixture(t)
	provider := NewSemanticIndexProvider(SemanticIndexConfig{
		BuildScopes: func(_ context.Context, _ model.WorkspaceView, _ string) ([]model.Scope, error) {
			return scopes, nil
		},
	})
	request, err := provider.BuildIndexRequest(context.Background(), view, "file:///repo")
	if err != nil {
		t.Fatalf("BuildIndexRequest without tool pins: %v", err)
	}
	runner := &semanticTestRunner{}
	provider = NewSemanticIndexProvider(SemanticIndexConfig{Runner: runner})
	got, err := provider.ExportIndex(context.Background(), request, &semanticTestSink{})
	if err != nil {
		t.Fatalf("ExportIndex without tool pins: %v", err)
	}
	if runner.verifyCall != 0 || runner.exportCall != 0 || len(got.UsedTools) != 0 {
		t.Fatalf("missing tool pins reached runner or were recorded: verify=%d export=%d used=%v", runner.verifyCall, runner.exportCall, got.UsedTools)
	}
	for _, scope := range request.Scopes {
		for _, fact := range model.RequiredFactKinds {
			if coverageState(got.Coverage, scope.ID, fact) != model.Unavailable {
				t.Errorf("scope %s coverage %s = %s, want unavailable", scope.ID, fact, coverageState(got.Coverage, scope.ID, fact))
			}
		}
	}
}

func TestRebuildVerifiedPlannerRequestUsesCurrentScopesWithoutRunningTools(t *testing.T) {
	view, root, attestations := pythonRebuildFixture(t)
	current := *view
	current.id.SnapshotRev++
	request, err := RebuildVerifiedPlannerRequest(context.Background(), &current, root, attestations)
	if err != nil {
		t.Fatalf("RebuildVerifiedPlannerRequest: %v", err)
	}
	if len(request.Scopes) != 1 || len(request.Provenance) != 1 {
		t.Fatalf("rebuilt request = %d scopes, %d provenances; want one each", len(request.Scopes), len(request.Provenance))
	}
	if request.Provenance[request.Scopes[0].ID].Identity != current.Identity() {
		t.Fatalf("rebuilt provenance identity = %+v, want current view identity %+v", request.Provenance[request.Scopes[0].ID].Identity, current.Identity())
	}
}

func TestRebuildVerifiedPlannerRequestRejectsSourceConfigAndToolDrift(t *testing.T) {
	t.Run("source identity", func(t *testing.T) {
		view, root, attestations := pythonRebuildFixture(t)
		changed := *view
		changed.id.DiskDigest = "sha256:changed-source"
		if _, err := RebuildVerifiedPlannerRequest(context.Background(), &changed, root, attestations); err == nil {
			t.Fatal("source identity drift was accepted")
		}
	})

	t.Run("current config closure", func(t *testing.T) {
		view, root, attestations := pythonRebuildFixture(t)
		changed := *view
		changed.content = make(map[string][]byte, len(view.content))
		for uri, content := range view.content {
			changed.content[uri] = append([]byte(nil), content...)
		}
		configURI := root + "/pyrightconfig.json"
		changed.content[configURI] = []byte(`{"pythonVersion":"3.11","extraPaths":["src"]}`)
		changed.files = map[string][]model.File{root: append([]model.File(nil), view.files[root]...)}
		for i := range changed.files[root] {
			if changed.files[root][i].URI == configURI {
				changed.files[root][i] = semanticFile(configURI, "json", changed.content[configURI])
			}
		}
		if _, err := RebuildVerifiedPlannerRequest(context.Background(), &changed, root, attestations); err == nil {
			t.Fatal("current config closure drift was accepted")
		}
	})

	t.Run("pinned tool bytes", func(t *testing.T) {
		view, root, attestations := pythonRebuildFixture(t)
		if err := os.WriteFile(attestations[0].Tools[0].Path, []byte("changed tool bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := RebuildVerifiedPlannerRequest(context.Background(), view, root, attestations); err == nil {
			t.Fatal("pinned tool hash drift was accepted")
		}
	})
}

func TestPyrightIncludeExcludePatternsAreBuildIdentityAndInvalidateAttestation(t *testing.T) {
	root := "file:///repo/include-exclude"
	configURI, sourceURI := root+"/pyrightconfig.json", root+"/src/main.py"
	config := []byte(`{"include":["src/**/*.py"],"exclude":["src/generated/**"]}`)
	source := []byte("def main() -> int:\n    return 1\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "py-include-exclude", DiskDigest: "sha256:include-exclude", SnapshotRev: 1},
		files: map[string][]model.File{root: {
			semanticFile(configURI, "json", config), semanticFile(sourceURI, "python", source),
		}},
		content: map[string][]byte{configURI: config, sourceURI: source},
	}
	tools := semanticTools(t)
	provider := NewSemanticIndexProvider(SemanticIndexConfig{
		BuildScopes: func(ctx context.Context, view model.WorkspaceView, rootURI string) ([]model.Scope, error) {
			return DiscoverPythonScopes(ctx, view, rootURI, "")
		},
		Tools: tools,
	})
	initial, err := provider.BuildIndexRequest(context.Background(), view, root)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	if len(initial.Scopes) != 1 {
		t.Fatalf("discovered %d scopes, want 1", len(initial.Scopes))
	}
	scope := initial.Scopes[0]
	if scope.Build.Options["pyrightInclude"] != `["src/**/*.py"]` || scope.Build.Options["pyrightExclude"] != `["src/generated/**"]` {
		t.Fatalf("scope did not retain Pyright file specs: %#v", scope.Build.Options)
	}

	changedConfig := []byte(`{"include":["src/main.py"],"exclude":["src/generated/**","vendor"]}`)
	changed := *view
	changed.content = make(map[string][]byte, len(view.content))
	for uri, body := range view.content {
		changed.content[uri] = append([]byte(nil), body...)
	}
	changed.content[configURI] = changedConfig
	changed.files = map[string][]model.File{root: append([]model.File(nil), view.files[root]...)}
	for i := range changed.files[root] {
		if changed.files[root][i].URI == configURI {
			changed.files[root][i] = semanticFile(configURI, "json", changedConfig)
		}
	}
	updated, err := provider.BuildIndexRequest(context.Background(), &changed, root)
	if err != nil {
		t.Fatalf("BuildIndexRequest after file-spec update: %v", err)
	}
	if initial.Scopes[0].BuildContext == updated.Scopes[0].BuildContext {
		t.Fatal("include/exclude change did not change the semantic build context")
	}
	attestation := initial.Provenance[initial.Scopes[0].ID]
	if _, err := RebuildVerifiedPlannerRequest(context.Background(), &changed, root, []model.Provenance{attestation}); err == nil {
		t.Fatal("saved semantic attestation survived include/exclude config drift")
	}
}

func pythonRebuildFixture(t *testing.T) (*semanticTestView, string, []model.Provenance) {
	t.Helper()
	root := "file:///repo/rebuild-python"
	configURI, sourceURI := root+"/pyrightconfig.json", root+"/main.py"
	config := []byte(`{"pythonVersion":"3.12","extraPaths":["src"]}`)
	source := []byte("def main():\n    return 1\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "py-rebuild", DiskDigest: "sha256:py-rebuild", SnapshotRev: 9},
		files: map[string][]model.File{root: {
			semanticFile(configURI, "json", config), semanticFile(sourceURI, "python", source),
		}},
		content: map[string][]byte{configURI: config, sourceURI: source},
	}
	provider := NewSemanticIndexProvider(SemanticIndexConfig{
		BuildScopes: func(ctx context.Context, view model.WorkspaceView, rootURI string) ([]model.Scope, error) {
			return DiscoverPythonScopes(ctx, view, rootURI, "")
		},
		Tools: semanticTools(t),
	})
	request, err := provider.BuildIndexRequest(context.Background(), view, root)
	if err != nil {
		t.Fatalf("build attested fixture: %v", err)
	}
	attestations := make([]model.Provenance, 0, len(request.Provenance))
	for _, scope := range request.Scopes {
		attestations = append(attestations, request.Provenance[scope.ID])
	}
	return view, root, attestations
}

func pythonOneFileFixture(t *testing.T) (*semanticTestView, model.Scope, model.Provenance) {
	t.Helper()
	root, uri := "file:///repo/one", "file:///repo/one/main.py"
	content := []byte("def main():\r\n    return 1\r\n")
	view := &semanticTestView{id: model.Identity{Workspace: "py-one", DiskDigest: "sha256:one", SnapshotRev: 1},
		files: map[string][]model.File{root: {semanticFile(uri, "python", content)}}, content: map[string][]byte{uri: content}}
	scope, provenance := pythonScope(root, "one", view.id, semanticTools(t))
	return view, scope, provenance
}

func pythonMultiScopeFixture(t *testing.T) (*semanticTestView, []model.Scope, map[string]model.Provenance) {
	t.Helper()
	rootA, rootB := "file:///repo/a", "file:///repo/b"
	uriA, uriB := rootA+"/main.py", rootB+"/main.py"
	contentA, contentB := []byte("def entry(): return 1\r\n"), []byte("def entry(): return 2\r\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "py-multi", DiskDigest: "sha256:multi", SnapshotRev: 5},
		files: map[string][]model.File{
			rootA: {semanticFile(uriA, "python", contentA)},
			rootB: {semanticFile(uriB, "python", contentB)},
		}, content: map[string][]byte{uriA: contentA, uriB: contentB},
	}
	tools := semanticTools(t)
	scopeA, provenanceA := pythonScope(rootA, "project-a", view.id, tools)
	scopeB, provenanceB := pythonScope(rootB, "project-b", view.id, tools)
	provenance := map[string]model.Provenance{scopeA.ID: provenanceA, scopeB.ID: provenanceB}
	return view, []model.Scope{scopeA, scopeB}, provenance
}

func pythonScope(root, id string, viewIdentity model.Identity, tools []model.ToolIdentity) (model.Scope, model.Provenance) {
	scope := model.Scope{
		ID: id, Language: "python", RootURI: root,
		Build: model.BuildInputs{
			Environment:  map[string]string{"pythonInterpreter": "C:/Python312/python.exe"},
			IncludePaths: []string{"stubs", "src"},
			Options: map[string]string{
				"pyrightConfig": "pyrightconfig.json", "pyrightConfigDigest": "sha256:" + strings.Repeat("a", 64),
				"pythonVersion": "3.12", "stubPath": "none",
			},
		},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: viewIdentity, Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend: identity.BackendID{Language: scope.Language, Name: "pyright-semantic-index"}, Toolchain: "pyright/1.1.414+node/24.14.0",
		Tools: append([]model.ToolIdentity(nil), tools...),
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	return scope, provenance
}

func semanticTools(t *testing.T) []model.ToolIdentity {
	t.Helper()
	dir := t.TempDir()
	tools := []model.ToolIdentity{
		{Name: semanticIndexCompilerName, Version: semanticIndexCompilerVersion, Path: filepath.Join(dir, "pyright.bin")},
		{Name: semanticIndexExporterName, Version: semanticIndexExtractorVersion, Path: filepath.Join(dir, "pyright-exporter.bin")},
	}
	for i := range tools {
		content := []byte("pinned tool " + tools[i].Name + "\n")
		if err := os.WriteFile(tools[i].Path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(content)
		tools[i].SHA256 = hex.EncodeToString(hash[:])
	}
	return tools
}

func testSemanticProvider(runner PyrightSemanticRunner, tools []model.ToolIdentity) *SemanticIndexProvider {
	return NewSemanticIndexProvider(SemanticIndexConfig{Runner: runner, Tools: tools})
}

func semanticFile(uri, language string, content []byte) model.File {
	hash := sha256.Sum256(content)
	return model.File{URI: uri, LanguageID: language, Size: int64(len(content)), SHA256: identity.ContentHash("sha256:" + hex.EncodeToString(hash[:]))}
}

func completeCoverage(scopeID string) []model.Coverage {
	coverage := make([]model.Coverage, 0, len(model.RequiredFactKinds))
	for _, fact := range model.RequiredFactKinds {
		coverage = append(coverage, model.Coverage{ScopeID: scopeID, Fact: fact, State: model.Complete})
	}
	return coverage
}

func coverageState(coverage []model.Coverage, scope string, fact model.FactKind) model.Completeness {
	for _, item := range coverage {
		if item.ScopeID == scope && item.Fact == fact {
			return item.State
		}
	}
	return ""
}
