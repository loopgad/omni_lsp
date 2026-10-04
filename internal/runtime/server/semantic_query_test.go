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
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

type fixtureSemanticProvider struct {
	occurrencesBeforeSymbols bool
	symbolCount              int
	buildEnvironment         map[string]string
	definitionColumn         uint32
	declarationOnly          bool
	graphNode                bool
}

func TestWorkspaceSymbolsWithoutVerifiedSourceDoesNotClaimEmptyResult(t *testing.T) {
	s := New(DefaultConfig())
	response := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 90}, "workspace/symbol", json.RawMessage(`{"query":"target"}`)))
	if response == nil || response.Error == nil {
		t.Fatalf("unavailable workspace symbols returned a successful empty result: %+v", response)
	}
	records := s.recentEvidence()
	if len(records) != 1 || records[0].Status != identity.ResultUnavailable || records[0].Completeness != identity.CompletenessUnknown {
		t.Fatalf("unavailable workspace symbol evidence = %+v", records)
	}
}

func (p fixtureSemanticProvider) BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	scope := model.Scope{ID: "fixture-go", Language: "go", RootURI: rootURI}
	scope.Build.Environment = p.buildEnvironment
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion,
		Identity:      view.Identity(),
		Scope:         scope,
		Extractor:     "runtime-test",
		ExtractorVer:  "1",
		Backend:       identity.BackendID{Language: "go", Name: "fixture"},
		Toolchain:     "fixture-toolchain",
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, nil)
	provenance.Scope = scope
	return model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}, nil
}

func (p fixtureSemanticProvider) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	scope := request.Scopes[0]
	var source model.File
	if err := request.View.Walk(ctx, scope.RootURI, func(file model.File) error { source = file; return nil }); err != nil {
		return model.Report{}, err
	}
	count := p.symbolCount
	if count <= 0 {
		count = 1
	}
	symbols := make([]model.Symbol, 0, count)
	occurrences := make([]model.Occurrence, 0, count)
	column := p.definitionColumn
	if column == 0 {
		column = 5
	}
	for i := 0; i < count; i++ {
		name := "PersistentTarget"
		if count > 1 {
			name = fmt.Sprintf("PersistentTarget%05d", i)
		}
		id := "go fixture " + name
		kind, role := "Function", "definition"
		if p.declarationOnly {
			kind, role = "Interface", "declaration"
		}
		symbols = append(symbols, model.Symbol{ID: identity.SymbolID(id), ScopeID: scope.ID, Name: name, Kind: kind})
		occurrences = append(occurrences, model.Occurrence{
			SymbolID: identity.SymbolID(id), ScopeID: scope.ID, URI: source.URI,
			Range: model.Position{StartLine: uint32(i + 1), StartChar: column, EndLine: uint32(i + 1), EndChar: column + uint32(len(name))},
			Role:  role, SourceHash: source.SHA256, BuildContext: scope.BuildContext,
		})
	}
	if p.graphNode {
		symbols = append(symbols, model.Symbol{ID: "fixture-file-node", ScopeID: scope.ID, Name: "PersistentTarget module", Kind: "module_graph_node"})
	}
	if p.occurrencesBeforeSymbols {
		if err := sink.WriteOccurrences(ctx, occurrences); err != nil {
			return model.Report{}, err
		}
		if err := sink.WriteSymbols(ctx, symbols); err != nil {
			return model.Report{}, err
		}
	} else {
		if err := sink.WriteSymbols(ctx, symbols); err != nil {
			return model.Report{}, err
		}
		if err := sink.WriteOccurrences(ctx, occurrences); err != nil {
			return model.Report{}, err
		}
	}
	report := model.Report{Identity: request.View.Identity(), UsedTools: map[string][]model.ToolIdentity{scope.ID: {}}}
	for _, fact := range model.RequiredFactKinds {
		state, reason := model.Unknown, "fixture does not emit this fact family"
		if fact == model.FactSymbol || fact == model.FactDefinition || p.declarationOnly && fact == model.FactDeclaration {
			state, reason = model.Complete, ""
		}
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: state, Reason: reason})
	}
	return report, nil
}

func newSemanticQueryFixture(tb testing.TB, symbolCount int) (*Server, string, uint64) {
	tb.Helper()
	tb.Setenv("OMNILSP_TRUST", "trusted")
	root := tb.TempDir()
	file := filepath.Join(root, "symbols.go")
	var source strings.Builder
	source.WriteString("package sample\n")
	count := symbolCount
	if count <= 0 {
		count = 1
	}
	for i := 0; i < count; i++ {
		name := "PersistentTarget"
		if count > 1 {
			name = fmt.Sprintf("PersistentTarget%05d", i)
		}
		fmt.Fprintf(&source, "func %s() {}\n", name)
	}
	if err := os.WriteFile(file, []byte(source.String()), 0o644); err != nil {
		tb.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.IndexDir = filepath.Join(tb.TempDir(), "semantic-index")
	s := New(cfg)
	s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}, wsymResult: []languages.WorkspaceSymbol{{Name: "LiveTarget", Kind: languages.SymbolFunction, URI: uri.FromPath(file).String()}}})
	provider := fixtureSemanticProvider{occurrencesBeforeSymbols: true, symbolCount: symbolCount}
	s.RegisterSemanticIndexProvider("go", provider, provider)
	params, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
	if err != nil {
		tb.Fatal(err)
	}
	init := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", params))
	if init == nil || init.Error != nil {
		tb.Fatalf("initialize: %+v", init)
	}
	if response := indexRequest(s, context.Background(), "omnilsp/reindex"); response == nil || response.Error != nil {
		tb.Fatalf("semantic reindex: %+v", response)
	}
	return s, file, s.currentRevision()
}

func TestSemanticReindexStatsAndPersistentWorkspaceSymbols(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	root := t.TempDir()
	file := filepath.Join(root, "symbols.go")
	if err := os.WriteFile(file, []byte("package sample\nfunc PersistentTarget() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.IndexDir = filepath.Join(t.TempDir(), "semantic-index")
	s := New(cfg)
	backend := &mockBackend{langID: "go", exts: []string{".go"}, wsymResult: []languages.WorkspaceSymbol{{Name: "LiveTarget", Kind: languages.SymbolFunction, URI: uri.FromPath(file).String()}}}
	s.RegisterBackend("go", backend)
	provider := fixtureSemanticProvider{occurrencesBeforeSymbols: true, buildEnvironment: map[string]string{"semanticImportRoot": "fixture-project"}}
	s.RegisterSemanticIndexProvider("go", provider, provider)
	params, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
	if err != nil {
		t.Fatal(err)
	}
	init := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", params))
	if init == nil || init.Error != nil {
		t.Fatalf("initialize: %+v", init)
	}
	if response := indexRequest(s, context.Background(), "omnilsp/reindex"); response == nil || response.Error != nil {
		t.Fatalf("semantic reindex: %+v", response)
	}
	stats := indexStats(t, s)
	if !stats.Fresh || stats.Generation != 1 || stats.SemanticStatus != "disk_fresh" || len(stats.SemanticCoverage) != len(model.RequiredFactKinds) || len(stats.Segments) < 3 {
		t.Fatalf("semantic index stats = %+v", stats)
	}

	request := func(id int64, query string) []lspWorkspaceSymbolResult {
		t.Helper()
		params, err := json.Marshal(map[string]string{"query": query})
		if err != nil {
			t.Fatal(err)
		}
		message := jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, "workspace/symbol", params)
		response := s.dispatcher.Dispatch(context.Background(), message)
		if response == nil || response.Error != nil {
			t.Fatalf("workspace/symbol: %+v", response)
		}
		var result []lspWorkspaceSymbolResult
		if err := json.Unmarshal(response.Result, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	got := request(2, "Persistent")
	if len(got) != 1 || got[0].Name != "PersistentTarget" || got[0].Location.Range.Start.Line != 1 {
		t.Fatalf("persistent symbol result = %+v", got)
	}
	if err := os.WriteFile(file, []byte("package sample\nfunc ChangedOnDisk() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = request(3, "Live")
	if len(got) != 1 || got[0].Name != "LiveTarget" {
		t.Fatalf("disk-stale live fallback result = %+v", got)
	}

	// This fixture cannot attest the changed source's Go semantics. A dirty
	// workspace must refuse rather than return disk-only live symbols.
	s.vfs.Open(uri.FromPath(file).String(), "go", 1, []byte("package sample\nfunc UnsavedTarget() {}\n"), 0)
	s.publishSnapshot()
	response := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 4}, "workspace/symbol", json.RawMessage(`{"query":"Persistent"}`)))
	if response == nil || response.Error == nil || response.Error.Code != jsonrpc.ContentModified || len(response.Result) != 0 {
		t.Fatalf("unprovable unsaved overlay did not refuse stale symbols: %+v", response)
	}
}

func BenchmarkPersistentWorkspaceSymbols(b *testing.B) {
	s, _, revision := newSemanticQueryFixture(b, 1)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if symbols, used := s.persistentWorkspaceSymbols(ctx, "Persistent", revision); !used || len(symbols) != 1 {
			b.Fatalf("persistent symbols = %+v, used=%v", symbols, used)
		}
	}
}

func TestPersistentWorkspaceSymbolsAcceptsOnlyDiskEquivalentOpenDocument(t *testing.T) {
	s, file, _ := newSemanticQueryFixture(t, 1)
	documentURI := uri.FromPath(file).String()
	diskContent, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	s.vfs.Open(documentURI, "go", 1, diskContent, 0)
	s.publishSnapshot()
	if symbols, used := s.persistentWorkspaceSymbols(context.Background(), "Persistent", s.currentRevision()); !used || len(symbols) != 1 {
		t.Fatalf("disk-equivalent open document = %+v, used=%v", symbols, used)
	}

	s.vfs.Update(documentURI, 2, []byte("package sample\nfunc UnsavedTarget() {}\n"))
	s.publishSnapshot()
	if symbols, used := s.persistentWorkspaceSymbols(context.Background(), "Persistent", s.currentRevision()); used {
		t.Fatalf("unsaved document returned stale persistent symbols: %+v", symbols)
	}

	s.vfs.Close(documentURI)
	s.publishSnapshot()
	if symbols, used := s.persistentWorkspaceSymbols(context.Background(), "Persistent", s.currentRevision()); !used || len(symbols) != 1 {
		t.Fatalf("closed document = %+v, used=%v", symbols, used)
	}
}

func TestPersistentWorkspaceSymbolsIncludeDeclarationsAndKeepGraphNodesSeparate(t *testing.T) {
	s, file, revision := newSemanticQueryFixture(t, 1)
	if err := os.WriteFile(file, []byte("package sample\ntype PersistentTarget interface{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	provider := fixtureSemanticProvider{declarationOnly: true, graphNode: true}
	s.RegisterSemanticIndexProvider("go", provider, provider)
	if response := indexRequest(s, context.Background(), "omnilsp/reindex"); response == nil || response.Error != nil {
		t.Fatalf("reindex declaration fixture: %+v", response)
	}
	symbols, used := s.persistentWorkspaceSymbols(context.Background(), "PersistentTarget", revision)
	if !used || len(symbols) != 1 || symbols[0].Name != "PersistentTarget" || symbols[0].Kind != languages.SymbolInterface || symbols[0].StartLine != 1 || symbols[0].StartCol != 5 {
		t.Fatalf("persistent declaration symbols = %+v, used=%v", symbols, used)
	}
}

func TestPersistentWorkspaceSymbolsLimit(t *testing.T) {
	s, _, revision := newSemanticQueryFixture(t, 1000)
	symbols, used := s.persistentWorkspaceSymbols(context.Background(), "", revision)
	if !used || len(symbols) != maxPersistentWorkspaceSymbols {
		t.Fatalf("empty-query symbols = %d, used=%v; want %d", len(symbols), used, maxPersistentWorkspaceSymbols)
	}
	if symbols[0].Name != "PersistentTarget00000" || symbols[len(symbols)-1].Name != "PersistentTarget00099" {
		t.Fatalf("empty-query top symbols = first %q, last %q; want 00000..00099", symbols[0].Name, symbols[len(symbols)-1].Name)
	}
}

func BenchmarkPersistentWorkspaceSymbolsLargeUnmatched(b *testing.B) {
	s, _, revision := newSemanticQueryFixture(b, 5000)
	benchmarkPersistentWorkspaceSymbols(b, s, revision, "NoSuchWorkspaceSymbol", 0)
}

func BenchmarkPersistentWorkspaceSymbolsLargeEmpty(b *testing.B) {
	s, _, revision := newSemanticQueryFixture(b, 5000)
	benchmarkPersistentWorkspaceSymbols(b, s, revision, "", maxPersistentWorkspaceSymbols)
}

func benchmarkPersistentWorkspaceSymbols(b *testing.B, s *Server, revision uint64, query string, want int) {
	b.Helper()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		symbols, used := s.persistentWorkspaceSymbols(ctx, query, revision)
		if !used || len(symbols) != want {
			b.Fatalf("query %q symbols = %d, used=%v; want %d", query, len(symbols), used, want)
		}
	}
}

func TestToolHashesByPathDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.exe")
	provenance := map[string]model.Provenance{
		"go": {Tools: []model.ToolIdentity{
			{Path: path, SHA256: "A"},
			{Path: path, SHA256: "a"},
		}},
		"typescript": {Tools: []model.ToolIdentity{
			{Path: path, SHA256: "B"},
		}},
	}
	got := toolHashesByPath(provenance)
	if len(got) != 1 || len(got[path]) != 2 {
		t.Fatalf("tool hashes by path = %+v, want one path with two distinct hashes", got)
	}
	for _, want := range []string{"A", "B"} {
		found := false
		for _, actual := range got[path] {
			if strings.EqualFold(actual, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tool hashes by path missing %q: %+v", want, got[path])
		}
	}
}

func TestContextReaderHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := contextReader{ctx: ctx, reader: bytes.NewReader([]byte("tool"))}
	_, err := reader.Read(make([]byte, 4))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("context reader error = %v, want context.Canceled", err)
	}
}

type lspWorkspaceSymbolResult struct {
	Name     string `json:"name"`
	Location struct {
		Range struct {
			Start struct {
				Line uint32 `json:"line"`
			} `json:"start"`
		} `json:"range"`
	} `json:"location"`
}
