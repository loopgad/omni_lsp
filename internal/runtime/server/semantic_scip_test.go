package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

type scipFixtureProvider struct{}

func (scipFixtureProvider) ExportIndex(context.Context, model.Request, model.Sink) (model.Report, error) {
	return model.Report{}, os.ErrInvalid
}

type lossySCIPFixtureProvider struct{}

func (lossySCIPFixtureProvider) BuildIndexRequest(_ context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	scope := model.Scope{ID: "go:module:lossy-fixture", Language: "go", RootURI: rootURI, Build: model.BuildInputs{Options: map[string]string{}}}
	scope.BuildContext = model.ComputeBuildContextID(scope, "lossy-scip-fixture", "1", "go-test-toolchain", nil)
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.Identity(), Scope: scope,
		Extractor: "lossy-scip-fixture", ExtractorVer: "1", Toolchain: "go-test-toolchain",
		Backend: identity.BackendID{Language: "go", Name: "lossy-scip-fixture"},
	}
	return model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}, nil
}

func (lossySCIPFixtureProvider) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	files := make(map[string]model.File)
	if err := request.View.Walk(ctx, request.Scopes[0].RootURI, func(file model.File) error {
		files[file.URI] = file
		return nil
	}); err != nil {
		return model.Report{}, err
	}
	var source, manifest model.File
	for _, file := range files {
		switch {
		case strings.HasSuffix(file.URI, "/main.go"):
			source = file
		case strings.HasSuffix(file.URI, "/go.mod"):
			manifest = file
		}
	}
	if source.URI == "" || manifest.URI == "" {
		return model.Report{}, fmt.Errorf("fixture source files missing")
	}
	scope := request.Scopes[0]
	foo := identity.SymbolID("go repo main#Foo.")
	module := identity.SymbolID("go:module:fixture")
	if err := sink.WriteSymbols(ctx, []model.Symbol{
		{ID: foo, ScopeID: scope.ID, Name: "Foo", Kind: "function"},
		{ID: module, ScopeID: scope.ID, Name: "fixture", Kind: "module"},
	}); err != nil {
		return model.Report{}, err
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{{
		SymbolID: foo, ScopeID: scope.ID, URI: source.URI,
		Range: model.Position{StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 8},
		Role:  "definition", SourceHash: source.SHA256, BuildContext: scope.BuildContext,
	}}); err != nil {
		return model.Report{}, err
	}
	if err := sink.WriteEdges(ctx, []model.Edge{{
		From: foo, To: module, ScopeID: scope.ID, Kind: model.EdgeModule,
		SourceURI: manifest.URI, SourceHash: manifest.SHA256, BuildContext: scope.BuildContext,
	}}); err != nil {
		return model.Report{}, err
	}
	report := model.Report{Identity: request.View.Identity(), UsedTools: map[string][]model.ToolIdentity{scope.ID: {}}}
	for _, fact := range model.RequiredFactKinds {
		report.Coverage = append(report.Coverage, model.Coverage{
			ScopeID: scope.ID, Fact: fact, State: model.IncompleteKnownSubset,
			Reason: "test provider emits only a minimal fixture subset",
		})
	}
	return report, nil
}

func (scipFixtureProvider) BuildIndexRequest(_ context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	scope := model.Scope{ID: "go:module:fixture", Language: "go", RootURI: rootURI, Build: model.BuildInputs{Options: map[string]string{}}}
	scope.BuildContext = model.ComputeBuildContextID(scope, "test-scip-import", "1", "test-toolchain", nil)
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.Identity(), Scope: scope,
		Extractor: "test-scip-import", ExtractorVer: "1", Toolchain: "test-toolchain",
		Backend: identity.BackendID{Language: "go", Name: "test-scip-import"},
	}
	return model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}, nil
}

func TestSemanticSCIPImportExportUsesOneVerifiedScopeAndPreservesStore(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	storeDir := t.TempDir()
	cfg := DefaultConfig()
	cfg.IndexDir = storeDir
	srv := New(cfg)
	provider := scipFixtureProvider{}
	srv.RegisterSemanticIndexProvider("go", provider, provider)
	srv.InitializeWorkspace(workspace)

	rootURI := uri.FromPath(workspace).String()
	input, err := proto.Marshal(&scip.Index{
		Metadata: &scip.Metadata{ProjectRoot: rootURI, TextDocumentEncoding: scip.TextEncoding_UTF16},
		Documents: []*scip.Document{{
			RelativePath: "main.go", Language: "go",
			Occurrences: []*scip.Occurrence{{Symbol: "scip-go gomod fixture main/Foo.", SymbolRoles: int32(scip.SymbolRole_Definition), Range: []int32{1, 5, 1, 8}}},
			Symbols:     []*scip.SymbolInformation{{Symbol: "scip-go gomod fixture main/Foo.", DisplayName: "Foo", Kind: scip.SymbolInformation_Function}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	imported, err := srv.ImportSemanticSCIP(context.Background(), "go", "go:module:fixture", input)
	if err != nil {
		t.Fatalf("ImportSemanticSCIP: %v", err)
	}
	if imported.Generation == 0 || len(imported.Coverage) != len(model.RequiredFactKinds) {
		t.Fatalf("import report does not identify a fully described generation: %+v", imported)
	}
	for _, coverage := range imported.Coverage {
		if coverage.State == model.Complete {
			t.Fatalf("SCIP import overstated %s coverage: %+v", coverage.Fact, coverage)
		}
	}

	output, err := srv.ExportSemanticSCIP(context.Background(), "go:module:fixture")
	if err != nil {
		t.Fatalf("ExportSemanticSCIP: %v", err)
	}
	var exported scip.Index
	if err := proto.Unmarshal(output, &exported); err != nil {
		t.Fatalf("decode exported SCIP: %v", err)
	}
	if len(exported.GetDocuments()) != 1 || exported.GetDocuments()[0].GetRelativePath() != "main.go" {
		t.Fatalf("unexpected scoped export: %+v", exported.GetDocuments())
	}

	if _, err := srv.ImportSemanticSCIP(context.Background(), "go", "go:module:fixture", input); err == nil || !strings.Contains(err.Error(), "refuses to replace existing generation") {
		t.Fatalf("second import did not preserve the existing generation: %v", err)
	}
	statsJSON, err := srv.handleIndexStats(context.Background(), &jsonrpc.Message{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(statsJSON), `"generation":`+fmt.Sprint(imported.Generation)) {
		t.Fatalf("current index generation differs from successful import: %s", statsJSON)
	}
}

func TestSemanticSCIPLossyExportIsExplicitAndSelfDescribing(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module fixture.local/test\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.IndexDir = t.TempDir()
	srv := New(cfg)
	provider := lossySCIPFixtureProvider{}
	srv.RegisterSemanticIndexProvider("go", provider, provider)
	srv.InitializeWorkspace(workspace)

	if response := indexRequest(srv, context.Background(), "omnilsp/reindex"); response == nil || response.Error != nil {
		t.Fatalf("reindex fixture: %+v", response)
	}
	scopeID := "go:module:lossy-fixture"
	if _, err := srv.ExportSemanticSCIP(context.Background(), scopeID); err == nil || !strings.Contains(err.Error(), `cannot represent semantic edge kind "module"`) {
		t.Fatalf("strict export error = %v, want rejection of the module edge", err)
	}
	data, losses, err := srv.ExportSemanticSCIPWithLosses(context.Background(), scopeID)
	if err != nil {
		t.Fatalf("explicit lossy export: %v", err)
	}
	if len(losses) != 1 || losses[0].Kind != model.EdgeModule || losses[0].Count != 1 {
		t.Fatalf("loss summary = %+v, want one omitted module edge", losses)
	}
	var exported scip.Index
	if err := proto.Unmarshal(data, &exported); err != nil {
		t.Fatalf("decode SCIP export: %v", err)
	}
	if len(exported.GetDocuments()) == 0 || len(exported.GetDocuments()[0].GetOccurrences()) == 0 {
		t.Fatalf("lossy export dropped representable occurrence facts: %+v", exported.GetDocuments())
	}
	wantMarker := "omnilsp.lossy-edge.module=1"
	foundMarker := false
	for _, arg := range exported.GetMetadata().GetToolInfo().GetArguments() {
		foundMarker = foundMarker || arg == wantMarker
	}
	if !foundMarker {
		t.Fatalf("SCIP metadata omitted loss marker %q: %+v", wantMarker, exported.GetMetadata().GetToolInfo())
	}
}
