package server

// Pins for round 23 OPT-2: CurrentSemanticIndexIdentity memoizes the verified
// generation identity per (index service, generation) so the per-response
// semantic observer stops re-deriving it (full-tree capture, tool re-hashing,
// whole-store segment digest) on every evidence-indexed response. A new
// generation (reindex) must miss the memo; errors must never poison it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

// replayIdentityFixtureProvider is fixtureSemanticProvider with a real pinned
// tool file, so CurrentSemanticIndexIdentity can reach its success path
// (ValidateReport requires complete tool identity for attested facts).
type replayIdentityFixtureProvider struct {
	toolPath string
	toolHash string
}

func (p replayIdentityFixtureProvider) tool() model.ToolIdentity {
	return model.ToolIdentity{Name: "fixture-tool", Path: p.toolPath, Version: "1", SHA256: p.toolHash}
}

func (p replayIdentityFixtureProvider) BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	scope := model.Scope{ID: "fixture-go", Language: "go", RootURI: rootURI}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion,
		Identity:      view.Identity(),
		Scope:         scope,
		Extractor:     "runtime-test",
		ExtractorVer:  "1",
		Backend:       identity.BackendID{Language: "go", Name: "fixture"},
		Toolchain:     "fixture-toolchain",
		Tools:         []model.ToolIdentity{p.tool()},
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	return model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}, nil
}

func (p replayIdentityFixtureProvider) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	scope := request.Scopes[0]
	var source model.File
	if err := request.View.Walk(ctx, scope.RootURI, func(file model.File) error { source = file; return nil }); err != nil {
		return model.Report{}, err
	}
	id := identity.SymbolID("go fixture PersistentTarget")
	symbol := model.Symbol{ID: id, ScopeID: scope.ID, Name: "PersistentTarget", Kind: "Function"}
	occurrence := model.Occurrence{
		SymbolID: id, ScopeID: scope.ID, URI: source.URI,
		Range: model.Position{StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 21},
		Role:  "definition", SourceHash: source.SHA256, BuildContext: scope.BuildContext,
	}
	if err := sink.WriteSymbols(ctx, []model.Symbol{symbol}); err != nil {
		return model.Report{}, err
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{occurrence}); err != nil {
		return model.Report{}, err
	}
	report := model.Report{
		Identity:  request.View.Identity(),
		UsedTools: map[string][]model.ToolIdentity{scope.ID: {p.tool()}},
	}
	for _, fact := range model.RequiredFactKinds {
		state, reason := model.Unknown, "fixture does not emit this fact family"
		if fact == model.FactSymbol || fact == model.FactDefinition {
			state, reason = model.Complete, ""
		}
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: state, Reason: reason})
	}
	return report, nil
}

func newReplayIdentityFixture(t *testing.T) (*Server, string) {
	t.Helper()
	t.Setenv("OMNILSP_TRUST", "trusted")
	root := t.TempDir()
	file := filepath.Join(root, "symbols.go")
	if err := os.WriteFile(file, []byte("package sample\nfunc PersistentTarget() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolPath := filepath.Join(t.TempDir(), "fixture-tool.bin")
	if err := os.WriteFile(toolPath, []byte(strings.Repeat("t", 1<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(strings.Repeat("t", 1<<20)))
	provider := replayIdentityFixtureProvider{toolPath: toolPath, toolHash: hex.EncodeToString(sum[:])}
	cfg := DefaultConfig()
	cfg.IndexDir = filepath.Join(t.TempDir(), "semantic-index")
	s := New(cfg)
	s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
	s.RegisterSemanticIndexProvider("go", provider, provider)
	initParams, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
	if err != nil {
		t.Fatal(err)
	}
	init := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 71}, "initialize", initParams))
	if init == nil || init.Error != nil {
		t.Fatalf("initialize: %+v", init)
	}
	if response := indexRequest(s, context.Background(), "omnilsp/reindex"); response == nil || response.Error != nil {
		t.Fatalf("semantic reindex: %+v", response)
	}
	return s, toolPath
}

func TestCurrentSemanticIndexIdentityMemoizesPerGeneration(t *testing.T) {
	ctx := context.Background()
	s, toolPath := newReplayIdentityFixture(t)

	first, err := s.CurrentSemanticIndexIdentity(ctx)
	if err != nil {
		t.Fatalf("first identity: %v", err)
	}
	if first.Generation == 0 || first.IndexContentDigest == "" || len(first.BuildContexts) != 1 || len(first.Tools) != 1 {
		t.Fatalf("identity incomplete: %+v", first)
	}

	// Same generation: the memo must serve the identity without re-deriving
	// it — even after the pinned tool file changes on disk, because one
	// generation's identity is invariant for that generation's lifetime
	// (the responses bound to it were produced by the generation, not by the
	// current tool bytes).
	if err := os.WriteFile(toolPath, []byte(strings.Repeat("u", 1<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := s.CurrentSemanticIndexIdentity(ctx)
	if err != nil {
		t.Fatalf("memoized identity: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("memoized identity differs from the verified one: %+v vs %+v", second, first)
	}

	// New generation: the memo must miss and re-derive. The tool file is
	// restored first — a re-export pins the tool identity again and the full
	// path must re-verify it against disk.
	if err := os.WriteFile(toolPath, []byte(strings.Repeat("t", 1<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	if response := indexRequest(s, ctx, "omnilsp/reindex"); response == nil || response.Error != nil {
		t.Fatalf("second reindex: %+v", response)
	}
	third, err := s.CurrentSemanticIndexIdentity(ctx)
	if err != nil {
		t.Fatalf("identity after reindex: %v", err)
	}
	if third.Generation == first.Generation {
		t.Fatal("reindex did not advance the generation — memo invalidation untestable")
	}
	if cached, ok := s.cachedSemanticIndexIdentity(s.idx, third.Generation); !ok || cached.Generation != third.Generation {
		t.Fatalf("new generation was not memoized: ok=%v cached=%+v", ok, cached)
	}

	// A fresh server (new index service) must not inherit the memo.
	other, _ := newReplayIdentityFixture(t)
	fresh, err := other.CurrentSemanticIndexIdentity(ctx)
	if err != nil {
		t.Fatalf("fresh server identity: %v", err)
	}
	if fresh.Generation != first.Generation {
		t.Fatalf("fresh store generation = %d, want %d", fresh.Generation, first.Generation)
	}
}

func TestCurrentSemanticIndexIdentityNeverCachesErrors(t *testing.T) {
	// No pinned tool identity: the full path refuses. The refusal must stay
	// retryable — an erroring call must not memoize a poisoned identity.
	s, _, _ := newSemanticQueryFixture(t, 1)
	for i := 0; i < 2; i++ {
		if _, err := s.CurrentSemanticIndexIdentity(context.Background()); err == nil || !strings.Contains(err.Error(), "tool identity") {
			t.Fatalf("call %d without pinned tools: want refusal, got %v", i, err)
		}
	}
	if s.semanticIdentityMemo.gen != 0 {
		t.Fatalf("failed identity was memoized: gen=%d", s.semanticIdentityMemo.gen)
	}
}
