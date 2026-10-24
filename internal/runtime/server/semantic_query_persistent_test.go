package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/protocol/lsp"
	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

type persistentLocationsFixture struct {
	referenceCoverage model.Completeness
	badReferenceRange bool
	buildMarker       string
	plannerCalls      atomic.Int32
	exportCalls       atomic.Int32
}

func (p *persistentLocationsFixture) BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	p.plannerCalls.Add(1)
	marker := p.buildMarker
	if marker == "" {
		marker = "persistent-query"
	}
	scope := model.Scope{ID: "query-go", Language: "go", RootURI: rootURI, Build: model.BuildInputs{Options: map[string]string{"fixture": marker}}}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion,
		Identity:      view.Identity(),
		Scope:         scope,
		Extractor:     "persistent-query-fixture",
		ExtractorVer:  "1",
		Backend:       identity.BackendID{Language: "go", Name: "fixture"},
		Toolchain:     "fixture-toolchain",
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, nil)
	provenance.Scope = scope
	return model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}, nil
}

func (p *persistentLocationsFixture) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	p.exportCalls.Add(1)
	scope := request.Scopes[0]
	files := make(map[string]model.File)
	if err := request.View.Walk(ctx, scope.RootURI, func(file model.File) error {
		files[path.Base(file.URI)] = file
		return nil
	}); err != nil {
		return model.Report{}, err
	}
	main, mainOK := files["main.go"]
	target, targetOK := files["target.go"]
	if !mainOK || !targetOK {
		return model.Report{}, fmt.Errorf("fixture sources not found: main=%t target=%t", mainOK, targetOK)
	}
	mainContent, err := readFixtureFile(ctx, request.View, main.URI)
	if err != nil {
		return model.Report{}, err
	}
	targetContent, err := readFixtureFile(ctx, request.View, target.URI)
	if err != nil {
		return model.Report{}, err
	}
	name := "PersistentTarget"
	mainStart := strings.Index(string(mainContent), name)
	targetStart := strings.Index(string(targetContent), name)
	if mainStart < 0 || targetStart < 0 {
		return model.Report{}, fmt.Errorf("fixture symbol missing from source")
	}
	mainLine := uint32(bytesBefore(mainContent, mainStart, '\n'))
	targetLine := uint32(bytesBefore(targetContent, targetStart, '\n'))
	mainColumn := uint32(mainStart - lastByteBefore(mainContent, mainStart, '\n') - 1)
	targetColumn := uint32(targetStart - lastByteBefore(targetContent, targetStart, '\n') - 1)
	definitionEnd := targetColumn + uint32(len(name))
	referenceEnd := mainColumn + uint32(len(name))
	if p.badReferenceRange {
		referenceEnd = 10000
	}
	symbolID := identity.SymbolID("go fixture PersistentTarget")
	if err := sink.WriteSymbols(ctx, []model.Symbol{{ID: symbolID, ScopeID: scope.ID, Name: name, Kind: "Function"}}); err != nil {
		return model.Report{}, err
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{
		{SymbolID: symbolID, ScopeID: scope.ID, URI: target.URI,
			Range: model.Position{StartLine: targetLine, StartChar: targetColumn, EndLine: targetLine, EndChar: definitionEnd},
			Role:  "definition", SourceHash: target.SHA256, BuildContext: scope.BuildContext},
		{SymbolID: symbolID, ScopeID: scope.ID, URI: main.URI,
			Range: model.Position{StartLine: mainLine, StartChar: mainColumn, EndLine: mainLine, EndChar: referenceEnd},
			Role:  "reference", SourceHash: main.SHA256, BuildContext: scope.BuildContext},
	}); err != nil {
		return model.Report{}, err
	}
	coverage := make([]model.Coverage, 0, len(model.RequiredFactKinds))
	for _, fact := range model.RequiredFactKinds {
		state, reason := model.Unknown, "fixture does not emit this fact family"
		switch fact {
		case model.FactSymbol, model.FactDefinition, model.FactDeclaration:
			state, reason = model.Complete, ""
		case model.FactReference:
			state = p.referenceCoverage
			if state == model.Complete {
				reason = ""
			} else {
				reason = "fixture reference coverage is incomplete"
			}
		}
		coverage = append(coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: state, Reason: reason})
	}
	return model.Report{Identity: request.View.Identity(), Coverage: coverage, UsedTools: map[string][]model.ToolIdentity{scope.ID: {}}}, nil
}

func readFixtureFile(ctx context.Context, view model.WorkspaceView, fileURI string) ([]byte, error) {
	file, err := view.Read(ctx, fileURI)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

func bytesBefore(content []byte, offset int, target byte) int {
	return strings.Count(string(content[:offset]), string(target))
}

func lastByteBefore(content []byte, offset int, target byte) int {
	for i := offset - 1; i >= 0; i-- {
		if content[i] == target {
			return i
		}
	}
	return -1
}

type persistentLocationsBackend struct {
	mockBackend
	definitionCalls atomic.Int32
	referenceCalls  atomic.Int32
}

func (b *persistentLocationsBackend) Definition(context.Context, languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.definitionCalls.Add(1)
	return identity.SemanticResult[[]languages.Location]{Status: identity.ResultExact, Value: []languages.Location{{URI: "file:///live/definition.go"}}}, nil
}

func (b *persistentLocationsBackend) References(context.Context, languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.referenceCalls.Add(1)
	return identity.SemanticResult[[]languages.Location]{Status: identity.ResultExact, Value: []languages.Location{{URI: "file:///live/reference.go"}}}, nil
}

func newPersistentLocationsServer(tb testing.TB, fixture *persistentLocationsFixture) (*Server, string, string, uint32, uint32) {
	return newPersistentLocationsServerWithDirs(tb, fixture, tb.TempDir(), filepath.Join(tb.TempDir(), "semantic-index"), true)
}

// newPersistentLocationsServerWithDirs builds the persistent-locations fixture
// on an explicit workspace root and index directory. With reindex=false it
// models a fresh server process that only reopens an existing store: it never
// commits a generation itself.
func newPersistentLocationsServerWithDirs(tb testing.TB, fixture *persistentLocationsFixture, root, indexDir string, reindex bool) (*Server, string, string, uint32, uint32) {
	tb.Helper()
	tb.Setenv("OMNILSP_TRUST", "trusted")
	mainPath := filepath.Join(root, "main.go")
	targetPath := filepath.Join(root, "target.go")
	mainContent := []byte("package sample\nfunc Use() { PersistentTarget() }\n")
	targetContent := []byte("package sample\nfunc PersistentTarget() {}\n")
	if err := os.WriteFile(mainPath, mainContent, 0o644); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(targetPath, targetContent, 0o644); err != nil {
		tb.Fatal(err)
	}
	mainURI, targetURI := uri.FromPath(mainPath).String(), uri.FromPath(targetPath).String()
	mainLineStart := strings.Index(string(mainContent), "\n") + 1
	column := uint32(strings.Index(string(mainContent[mainLineStart:]), "PersistentTarget") + 2)
	cfg := DefaultConfig()
	cfg.IndexDir = indexDir
	s := New(cfg)
	backend := &persistentLocationsBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", backend)
	s.RegisterSemanticIndexProvider("go", fixture, fixture)
	params, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
	if err != nil {
		tb.Fatal(err)
	}
	response := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 71}, "initialize", params))
	if response == nil || response.Error != nil {
		tb.Fatalf("initialize: %+v", response)
	}
	if reindex {
		if response := indexRequest(s, context.Background(), "omnilsp/reindex"); response == nil || response.Error != nil {
			tb.Fatalf("semantic reindex: %+v", response)
		}
	}
	return s, mainURI, targetURI, 1, column
}

func callPersistentLocationRequest(t *testing.T, s *Server, method, documentURI string, line, column uint32, includeDeclaration bool, id int64) []lsp.Location {
	t.Helper()
	params := map[string]any{
		"textDocument": map[string]string{"uri": documentURI},
		"position":     map[string]uint32{"line": line, "character": column},
	}
	if method == "textDocument/references" {
		params["context"] = map[string]bool{"includeDeclaration": includeDeclaration}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	response := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, method, raw))
	if response == nil || response.Error != nil {
		t.Fatalf("%s response: %+v", method, response)
	}
	var locations []lsp.Location
	if err := json.Unmarshal(response.Result, &locations); err != nil {
		t.Fatalf("decode %s result %s: %v", method, response.Result, err)
	}
	return locations
}

func TestPersistentDefinitionAndReferencesUseCommittedSemanticRecords(t *testing.T) {
	fixture := &persistentLocationsFixture{referenceCoverage: model.Complete}
	s, mainURI, targetURI, line, column := newPersistentLocationsServer(t, fixture)
	backend := s.languages["go"].(*persistentLocationsBackend)

	definitions := callPersistentLocationRequest(t, s, "textDocument/definition", mainURI, line, column, false, 72)
	if len(definitions) != 1 || definitions[0].URI != targetURI || definitions[0].Range.Start.Line != 1 {
		t.Fatalf("definition = %+v, want committed cross-file target %s:1", definitions, targetURI)
	}
	references := callPersistentLocationRequest(t, s, "textDocument/references", mainURI, line, column, false, 73)
	if len(references) != 1 || references[0].URI != mainURI || references[0].Range.Start.Line != 1 {
		t.Fatalf("references without declaration = %+v, want the cross-file reference", references)
	}
	withDeclaration := callPersistentLocationRequest(t, s, "textDocument/references", mainURI, line, column, true, 74)
	if len(withDeclaration) != 2 || withDeclaration[0].URI != mainURI || withDeclaration[1].URI != targetURI {
		t.Fatalf("references with declaration = %+v, want reference and definition in stable URI order", withDeclaration)
	}
	if backend.definitionCalls.Load() != 0 || backend.referenceCalls.Load() != 0 {
		t.Fatalf("backend calls after persistent hits = definition %d, references %d", backend.definitionCalls.Load(), backend.referenceCalls.Load())
	}
	if fixture.exportCalls.Load() != 1 {
		t.Fatalf("query re-exported semantic facts %d times; want only the committed generation build", fixture.exportCalls.Load())
	}

	var definitionEvidence *identity.Evidence
	for _, record := range s.recentEvidence() {
		if record.Method == "textDocument/definition" && len(record.Ev) == 1 {
			ev := record.Ev[0]
			definitionEvidence = &ev
		}
	}
	if definitionEvidence == nil || definitionEvidence.Kind != identity.EvidenceIndex ||
		definitionEvidence.Assurance != identity.AssuranceIndexedExact || definitionEvidence.IndexGen == 0 ||
		definitionEvidence.BuildContext == "" || definitionEvidence.SourceHash == "" {
		t.Fatalf("persistent definition evidence = %+v, want exact index evidence with generation/context/source", definitionEvidence)
	}
}

func TestPersistentLocationsDoNotRequireLiveBackend(t *testing.T) {
	fixture := &persistentLocationsFixture{referenceCoverage: model.Complete}
	s, mainURI, targetURI, line, column := newPersistentLocationsServer(t, fixture)
	s.mu.Lock()
	s.languages = make(map[string]languages.Backend)
	s.mu.Unlock()
	definitions := callPersistentLocationRequest(t, s, "textDocument/definition", mainURI, line, column, false, 91)
	if len(definitions) != 1 || definitions[0].URI != targetURI {
		t.Fatalf("offline definitions = %+v", definitions)
	}
	references := callPersistentLocationRequest(t, s, "textDocument/references", mainURI, line, column, false, 92)
	if len(references) != 1 || references[0].URI != mainURI {
		t.Fatalf("offline references = %+v", references)
	}
	definitionEnvelope, err := s.DefinitionEnvelope(context.Background(), mainURI, line, column)
	if err != nil {
		t.Fatalf("offline typed definition: %v", err)
	}
	if definitionEnvelope.Status != identity.ResultExact || len(definitionEnvelope.Value) != 1 || definitionEnvelope.Value[0].URI != targetURI {
		t.Fatalf("offline typed definition = %+v, want exact committed target %s", definitionEnvelope, targetURI)
	}
	referenceEnvelope, err := s.ReferencesEnvelope(context.Background(), mainURI, line, column, false)
	if err != nil {
		t.Fatalf("offline typed references: %v", err)
	}
	if referenceEnvelope.Status != identity.ResultExact || len(referenceEnvelope.Value) != 1 || referenceEnvelope.Value[0].URI != mainURI {
		t.Fatalf("offline typed references = %+v, want exact committed source reference", referenceEnvelope)
	}
	for name, result := range map[string]identity.SemanticResult[[]languages.Location]{
		"definition": definitionEnvelope,
		"references": referenceEnvelope,
	} {
		if len(result.Evidence) == 0 || result.Evidence[0].Kind != identity.EvidenceIndex || result.Evidence[0].Assurance != identity.AssuranceIndexedExact {
			t.Errorf("offline typed %s evidence = %+v, want exact committed index evidence", name, result.Evidence)
		}
	}
	if fixture.exportCalls.Load() != 1 {
		t.Fatalf("offline LSP/facade query rebuilt index: exports=%d", fixture.exportCalls.Load())
	}
}

func TestPersistentLocationsAllowDiskEquivalentOpenOverlay(t *testing.T) {
	fixture := &persistentLocationsFixture{referenceCoverage: model.Complete}
	s, mainURI, targetURI, line, column := newPersistentLocationsServer(t, fixture)
	backend := s.languages["go"].(*persistentLocationsBackend)

	parsed, err := uri.Parse(mainURI)
	if err != nil {
		t.Fatal(err)
	}
	mainPath, err := parsed.Path()
	if err != nil {
		t.Fatal(err)
	}
	diskContent, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	s.vfs.Open(mainURI, "go", 37, diskContent, 0)
	s.publishSnapshot()

	definitions := callPersistentLocationRequest(t, s, "textDocument/definition", mainURI, line, column, false, 81)
	if len(definitions) != 1 || definitions[0].URI != targetURI {
		t.Fatalf("definition with disk-equivalent open overlay = %+v, want committed cross-file definition", definitions)
	}
	references := callPersistentLocationRequest(t, s, "textDocument/references", mainURI, line, column, false, 82)
	if len(references) != 1 || references[0].URI != mainURI {
		t.Fatalf("references with disk-equivalent open overlay = %+v, want committed reference", references)
	}
	if backend.definitionCalls.Load() != 0 || backend.referenceCalls.Load() != 0 {
		t.Fatalf("disk-equivalent overlay unexpectedly fell back: definition calls=%d reference calls=%d", backend.definitionCalls.Load(), backend.referenceCalls.Load())
	}
	if fixture.exportCalls.Load() != 1 {
		t.Fatalf("persistent query re-exported facts %d times; want only the committed generation build", fixture.exportCalls.Load())
	}
}

func TestSemanticOverlayIdentityBindsVersionAndContent(t *testing.T) {
	fixture := &persistentLocationsFixture{referenceCoverage: model.Complete}
	s, mainURI, _, _, _ := newPersistentLocationsServer(t, fixture)
	parsed, err := uri.Parse(mainURI)
	if err != nil {
		t.Fatal(err)
	}
	mainPath, err := parsed.Path()
	if err != nil {
		t.Fatal(err)
	}
	diskContent, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	s.vfs.Open(mainURI, "go", 1, diskContent, 0)
	s.publishSnapshot()
	firstRevision := s.currentRevision()
	first, ok := captureSemanticOverlayIdentity(s, context.Background(), firstRevision)
	if !ok || len(first.Documents) != 1 || first.Documents[0].Version != 1 {
		t.Fatalf("first overlay identity = %+v, ok=%t", first, ok)
	}

	s.vfs.Update(mainURI, 2, diskContent)
	s.publishSnapshot()
	secondRevision := s.currentRevision()
	second, ok := captureSemanticOverlayIdentity(s, context.Background(), secondRevision)
	if !ok || len(second.Documents) != 1 || second.Documents[0].Version != 2 {
		t.Fatalf("second overlay identity = %+v, ok=%t", second, ok)
	}
	if first.Digest == second.Digest {
		t.Fatalf("overlay digest did not bind the document version: first=%+v second=%+v", first, second)
	}
	if semanticOverlayStillCurrent(s, context.Background(), firstRevision, first) {
		t.Fatal("stale overlay identity remained current after the VFS version advanced")
	}
	if second.Documents[0].Content != first.Documents[0].Content {
		t.Fatalf("same-content version update changed content identity: first=%+v second=%+v", first, second)
	}

	changed := append([]byte(nil), diskContent...)
	changed = append(changed, []byte("// unsaved\n")...)
	s.vfs.Update(mainURI, 3, changed)
	s.publishSnapshot()
	third, ok := captureSemanticOverlayIdentity(s, context.Background(), s.currentRevision())
	if !ok || third.Documents[0].Content == second.Documents[0].Content {
		t.Fatalf("content edit was not bound in overlay identity: %+v, ok=%t", third, ok)
	}
}

func TestPersistentDefinitionFallsBackForInvalidPosition(t *testing.T) {
	t.Run("malformed indexed range", func(t *testing.T) {
		fixture := &persistentLocationsFixture{referenceCoverage: model.Complete, badReferenceRange: true}
		s, mainURI, _, line, column := newPersistentLocationsServer(t, fixture)
		backend := s.languages["go"].(*persistentLocationsBackend)
		locations := callPersistentLocationRequest(t, s, "textDocument/definition", mainURI, line, column, false, 75)
		if backend.definitionCalls.Load() != 1 || len(locations) != 1 || locations[0].URI != "file:///live/definition.go" {
			t.Fatalf("invalid persisted occurrence did not use live fallback: calls=%d locations=%+v", backend.definitionCalls.Load(), locations)
		}
	})
	t.Run("cursor misses semantic occurrence", func(t *testing.T) {
		fixture := &persistentLocationsFixture{referenceCoverage: model.Complete}
		s, mainURI, _, line, column := newPersistentLocationsServer(t, fixture)
		backend := s.languages["go"].(*persistentLocationsBackend)
		locations := callPersistentLocationRequest(t, s, "textDocument/definition", mainURI, line, column+40, false, 80)
		if backend.definitionCalls.Load() != 1 || len(locations) != 1 || locations[0].URI != "file:///live/definition.go" {
			t.Fatalf("unmatched cursor did not use live fallback: calls=%d locations=%+v", backend.definitionCalls.Load(), locations)
		}
	})
}

func TestPersistentReferencesFallBackForIncompleteCoverage(t *testing.T) {
	fixture := &persistentLocationsFixture{referenceCoverage: model.IncompleteKnownSubset}
	s, mainURI, _, line, column := newPersistentLocationsServer(t, fixture)
	backend := s.languages["go"].(*persistentLocationsBackend)
	locations := callPersistentLocationRequest(t, s, "textDocument/references", mainURI, line, column, false, 76)
	if backend.referenceCalls.Load() != 1 || len(locations) != 1 || locations[0].URI != "file:///live/reference.go" {
		t.Fatalf("incomplete persisted coverage did not use live fallback: calls=%d locations=%+v", backend.referenceCalls.Load(), locations)
	}
}

func TestPersistentLocationsFallBackForOpenUnsavedDocument(t *testing.T) {
	fixture := &persistentLocationsFixture{referenceCoverage: model.Complete}
	s, mainURI, _, line, column := newPersistentLocationsServer(t, fixture)
	backend := s.languages["go"].(*persistentLocationsBackend)
	s.vfs.Open(mainURI, "go", 1, []byte("package sample\nfunc Unsaved() {}\n"), 0)
	s.publishSnapshot()
	locations := callPersistentLocationRequest(t, s, "textDocument/definition", mainURI, line, column, false, 77)
	if backend.definitionCalls.Load() != 1 || len(locations) != 1 || locations[0].URI != "file:///live/definition.go" {
		t.Fatalf("open unsaved document did not use live fallback: calls=%d locations=%+v", backend.definitionCalls.Load(), locations)
	}
}

func TestPersistentLocationsFallBackWhenDiskOrBuildContextChanges(t *testing.T) {
	t.Run("disk digest", func(t *testing.T) {
		fixture := &persistentLocationsFixture{referenceCoverage: model.Complete}
		s, mainURI, targetURI, line, column := newPersistentLocationsServer(t, fixture)
		backend := s.languages["go"].(*persistentLocationsBackend)
		parsed, err := uri.Parse(targetURI)
		if err != nil {
			t.Fatal(err)
		}
		targetPath, err := parsed.Path()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(targetPath, []byte("package sample\nfunc ChangedTarget() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		locations := callPersistentLocationRequest(t, s, "textDocument/definition", mainURI, line, column, false, 78)
		if backend.definitionCalls.Load() != 1 || len(locations) != 1 || locations[0].URI != "file:///live/definition.go" {
			t.Fatalf("changed disk digest did not use live fallback: calls=%d locations=%+v", backend.definitionCalls.Load(), locations)
		}
	})

	t.Run("build context", func(t *testing.T) {
		fixture := &persistentLocationsFixture{referenceCoverage: model.Complete}
		s, mainURI, _, line, column := newPersistentLocationsServer(t, fixture)
		backend := s.languages["go"].(*persistentLocationsBackend)
		fixture.buildMarker = "changed-after-index"
		locations := callPersistentLocationRequest(t, s, "textDocument/definition", mainURI, line, column, false, 79)
		if backend.definitionCalls.Load() != 1 || len(locations) != 1 || locations[0].URI != "file:///live/definition.go" {
			t.Fatalf("changed build context did not use live fallback: calls=%d locations=%+v", backend.definitionCalls.Load(), locations)
		}
	})
}

func TestPersistedOccurrencePositionsConvertFromUTF16(t *testing.T) {
	content := []byte("😀PersistentTarget")
	for _, test := range []struct {
		encoding int
		start    uint32
		end      uint32
	}{
		{encoding: 0, start: 4, end: 20},
		{encoding: 1, start: 2, end: 18},
		{encoding: 2, start: 1, end: 17},
	} {
		start, ok := persistedPositionInEncoding(content, 0, 2, position.Encoding(test.encoding))
		if !ok || start.Col != test.start {
			t.Errorf("encoding %d start = %+v, ok=%t; want column %d", test.encoding, start, ok, test.start)
		}
		end, ok := persistedPositionInEncoding(content, 0, 18, position.Encoding(test.encoding))
		if !ok || end.Col != test.end {
			t.Errorf("encoding %d end = %+v, ok=%t; want column %d", test.encoding, end, ok, test.end)
		}
	}
}

// gomodSemanticFixture is a hermetic semantic binding for the no-go.mod
// degradation tests: it plans one deterministic workspace scope and exports
// one complete definition fact from the immutable view. With failNoProject set
// it models what the real Go planner reports in a workspace without go.mod or
// go.work instead of planning.
type gomodSemanticFixture struct {
	language      string
	scopeID       string
	failNoProject bool
}

func (f *gomodSemanticFixture) BuildIndexRequest(_ context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	if f.failNoProject {
		return model.Request{}, fmt.Errorf("Go semantic index: no go.mod or go.work project was found in the immutable scope")
	}
	scope := model.Scope{ID: f.scopeID, Language: f.language, RootURI: rootURI}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion,
		Identity:      view.Identity(),
		Scope:         scope,
		Extractor:     f.language + "-gomod-fixture",
		ExtractorVer:  "1",
		Backend:       identity.BackendID{Language: f.language, Name: f.language + "-gomod-fixture"},
		Toolchain:     f.language + "-gomod-toolchain",
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, nil)
	provenance.Scope = scope
	return model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}, nil
}

func (f *gomodSemanticFixture) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	scope := request.Scopes[0]
	var source model.File
	if err := request.View.Walk(ctx, scope.RootURI, func(file model.File) error {
		if file.LanguageID == f.language {
			source = file
		}
		return nil
	}); err != nil {
		return model.Report{}, err
	}
	if source.URI == "" {
		return model.Report{}, fmt.Errorf("gomod fixture found no %s source", f.language)
	}
	name := "PersistentTarget"
	symbolID := identity.SymbolID(f.language + " gomod fixture " + name)
	if err := sink.WriteSymbols(ctx, []model.Symbol{{ID: symbolID, ScopeID: scope.ID, Name: name, Kind: "Function"}}); err != nil {
		return model.Report{}, err
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{{
		SymbolID: symbolID, ScopeID: scope.ID, URI: source.URI,
		Range: model.Position{StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 5 + uint32(len(name))},
		Role:  "definition", SourceHash: source.SHA256, BuildContext: scope.BuildContext,
	}}); err != nil {
		return model.Report{}, err
	}
	report := model.Report{Identity: request.View.Identity(), UsedTools: map[string][]model.ToolIdentity{scope.ID: {}}}
	for _, fact := range model.RequiredFactKinds {
		state, reason := model.Unknown, "gomod fixture does not emit this fact family"
		if fact == model.FactSymbol || fact == model.FactDefinition {
			state, reason = model.Complete, ""
		}
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: state, Reason: reason})
	}
	return report, nil
}

// A Go binding registered in a workspace without go.mod must not poison the
// whole committed generation: its failing planner contributes no request this
// round and the remaining language still restates every saved scope, so SCIP
// export succeeds.
func TestSemanticSCIPExportToleratesGoPlannerFailureWithoutGoMod(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.py"), []byte("def persistent_target():\n    pass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.IndexDir = t.TempDir()
	srv := New(cfg)
	srv.InitializeWorkspace(workspace)
	python := &gomodSemanticFixture{language: "python", scopeID: "py:gomod-overlay"}
	srv.RegisterSemanticIndexProvider("python", python, python)
	if response := indexRequest(srv, context.Background(), "omnilsp/reindex"); response == nil || response.Error != nil {
		t.Fatalf("python reindex: %+v", response)
	}
	goBinding := &gomodSemanticFixture{language: "go", scopeID: "go:gomod-stub", failNoProject: true}
	srv.RegisterSemanticIndexProvider("go", goBinding, goBinding)
	data, err := srv.ExportSemanticSCIP(context.Background(), "py:gomod-overlay")
	if err != nil {
		t.Fatalf("SCIP export with a failing Go planner = %v, want the python generation exported", err)
	}
	if len(data) == 0 {
		t.Fatal("SCIP export produced no data")
	}
}

// A generation containing a Go scope must stay unexportable once the Go
// planner can no longer restate that scope: the per-binding degradation only
// covers languages contributing nothing this round, never a saved scope.
func TestSemanticSCIPExportStillRejectsUnreproducibleGoScope(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	workspace := t.TempDir()
	files := map[string]string{
		"go.mod":  "module gomod.fixture\n\ngo 1.22\n",
		"main.go": "package fixture\n\nfunc PersistentTarget() {}\n",
		"main.py": "def persistent_helper():\n    pass\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultConfig()
	cfg.IndexDir = t.TempDir()
	srv := New(cfg)
	srv.InitializeWorkspace(workspace)
	goBinding := &gomodSemanticFixture{language: "go", scopeID: "go:gomod-stub"}
	python := &gomodSemanticFixture{language: "python", scopeID: "py:gomod-overlay"}
	srv.RegisterSemanticIndexProvider("go", goBinding, goBinding)
	srv.RegisterSemanticIndexProvider("python", python, python)
	if response := indexRequest(srv, context.Background(), "omnilsp/reindex"); response == nil || response.Error != nil {
		t.Fatalf("two-language reindex: %+v", response)
	}
	if _, err := srv.ExportSemanticSCIP(context.Background(), "py:gomod-overlay"); err != nil {
		t.Fatalf("export before the Go planner failed: %v", err)
	}
	goBinding.failNoProject = true
	if _, err := srv.ExportSemanticSCIP(context.Background(), "py:gomod-overlay"); err == nil || !strings.Contains(err.Error(), "generation is stale") {
		t.Fatalf("export after the Go scope became unreproducible = %v, want a stale-generation rejection", err)
	}
}

// persistentRestartSymbolResult decodes workspace/symbol entries including the
// location URI the persistent generation reported.
type persistentRestartSymbolResult struct {
	Name     string `json:"name"`
	Location struct {
		URI   string `json:"uri"`
		Range struct {
			Start struct {
				Line uint32 `json:"line"`
			} `json:"start"`
		} `json:"range"`
	} `json:"location"`
}

func dispatchWorkspaceSymbolQuery(t *testing.T, s *Server, id int64, query string) []persistentRestartSymbolResult {
	t.Helper()
	params, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		t.Fatal(err)
	}
	response := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, "workspace/symbol", params))
	if response == nil || response.Error != nil {
		t.Fatalf("workspace/symbol: %+v", response)
	}
	var result []persistentRestartSymbolResult
	if err := json.Unmarshal(response.Result, &result); err != nil {
		t.Fatalf("decode workspace/symbol result %s: %v", response.Result, err)
	}
	return result
}

// TestTypedSemanticGenerationServesFreshQueriesAfterServerRestart closes the
// X5-7 loop across processes: a typed semantic generation committed by one
// server process is reopened from disk by a fresh server process — no
// reindex, no re-export — and serves workspace-symbol, definition and
// reference queries with the committed generation's evidence. Once the disk
// tree diverges from the generation's digest, the restarted process must
// refuse the stale generation and fall back to the live backend.
func TestTypedSemanticGenerationServesFreshQueriesAfterServerRestart(t *testing.T) {
	fixture := &persistentLocationsFixture{referenceCoverage: model.Complete}
	root := t.TempDir()
	indexDir := filepath.Join(t.TempDir(), "semantic-index")
	s, mainURI, targetURI, line, column := newPersistentLocationsServerWithDirs(t, fixture, root, indexDir, true)
	firstGeneration := indexStats(t, s).Generation

	// A second server process over the same workspace and index directory,
	// which never builds a generation itself.
	restarted, restartMainURI, restartTargetURI, restartLine, restartColumn := newPersistentLocationsServerWithDirs(t, fixture, root, indexDir, false)
	backend := restarted.languages["go"].(*persistentLocationsBackend)
	if restartMainURI != mainURI || restartTargetURI != targetURI || restartLine != line || restartColumn != column {
		t.Fatalf("restart fixture identities diverged: %s/%s/%d/%d", restartMainURI, restartTargetURI, restartLine, restartColumn)
	}
	if stats := indexStats(t, restarted); stats.Generation != firstGeneration {
		t.Fatalf("restarted process sees generation %d, want the committed %d", stats.Generation, firstGeneration)
	}

	definitions := callPersistentLocationRequest(t, restarted, "textDocument/definition", mainURI, line, column, false, 81)
	if len(definitions) != 1 || definitions[0].URI != targetURI || definitions[0].Range.Start.Line != 1 {
		t.Fatalf("restarted definition = %+v, want the committed cross-file target %s:1", definitions, targetURI)
	}
	references := callPersistentLocationRequest(t, restarted, "textDocument/references", mainURI, line, column, true, 82)
	if len(references) != 2 || references[0].URI != mainURI || references[1].URI != targetURI {
		t.Fatalf("restarted references = %+v, want reference and committed definition in stable order", references)
	}
	envelope, err := restarted.DefinitionEnvelope(context.Background(), mainURI, line, column)
	if err != nil || envelope.Status != identity.ResultExact || len(envelope.Value) != 1 || envelope.Value[0].URI != targetURI {
		t.Fatalf("restarted typed definition = %+v, err=%v, want the committed target", envelope, err)
	}
	if len(envelope.Evidence) != 1 || envelope.Evidence[0].Kind != identity.EvidenceIndex ||
		envelope.Evidence[0].Assurance != identity.AssuranceIndexedExact ||
		envelope.Evidence[0].IndexGen != identity.IndexGeneration(firstGeneration) {
		t.Fatalf("restarted definition evidence = %+v, want indexed-exact evidence for generation %d", envelope.Evidence, firstGeneration)
	}
	symbols := dispatchWorkspaceSymbolQuery(t, restarted, 83, "Persistent")
	if len(symbols) != 1 || symbols[0].Name != "PersistentTarget" || symbols[0].Location.URI != targetURI {
		t.Fatalf("restarted workspace symbols = %+v, want the committed PersistentTarget at %s", symbols, targetURI)
	}
	if backend.definitionCalls.Load() != 0 || backend.referenceCalls.Load() != 0 {
		t.Fatalf("restarted process fell back to the live backend: definition=%d references=%d", backend.definitionCalls.Load(), backend.referenceCalls.Load())
	}
	if fixture.exportCalls.Load() != 1 {
		t.Fatalf("restarted process re-exported the committed generation %d times; want only the original build", fixture.exportCalls.Load())
	}

	// The reopened store must refuse the committed generation once the disk
	// tree diverges from its digest: adding a file changes the digest while
	// every recorded source hash still matches, so the digest gates are the
	// only defense between the stale generation and this query.
	if err := os.WriteFile(filepath.Join(root, "restart_probe.go"), []byte("package sample\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := callPersistentLocationRequest(t, restarted, "textDocument/definition", mainURI, line, column, false, 84)
	if backend.definitionCalls.Load() != 1 || len(fallback) != 1 || fallback[0].URI != "file:///live/definition.go" {
		t.Fatalf("restarted process served a generation whose disk digest no longer matches: calls=%d result=%+v", backend.definitionCalls.Load(), fallback)
	}
	if stale, used := restarted.persistentWorkspaceSymbols(context.Background(), "Persistent", restarted.currentRevision()); used {
		t.Fatalf("stale generation served workspace symbols after the disk tree changed: %+v", stale)
	}
}

// TestPersistentQueriesServeOnlyTheCurrentGenerationAfterRecommit pins
// generation isolation on the query path: after a recommit publishes
// generation 2, definitions and references must come from generation 2's
// records (with its evidence), and a cursor position that only generation 1
// indexed must never be served from the superseded generation.
func TestPersistentQueriesServeOnlyTheCurrentGenerationAfterRecommit(t *testing.T) {
	fixture := &persistentLocationsFixture{referenceCoverage: model.Complete}
	s, mainURI, targetURI, line, column := newPersistentLocationsServer(t, fixture)
	backend := s.languages["go"].(*persistentLocationsBackend)
	ctx := context.Background()

	first, err := s.DefinitionEnvelope(ctx, mainURI, line, column)
	if err != nil || len(first.Value) != 1 || first.Value[0].URI != targetURI || first.Value[0].Range.StartLine != 1 {
		t.Fatalf("generation-1 definition = %+v, err=%v, want the committed target on line 1", first, err)
	}
	if len(first.Evidence) != 1 || first.Evidence[0].IndexGen != 1 {
		t.Fatalf("generation-1 evidence = %+v, want IndexGen 1", first.Evidence)
	}

	// Move the committed definition to a new line on disk and republish.
	parsed, err := uri.Parse(targetURI)
	if err != nil {
		t.Fatal(err)
	}
	targetPath, err := parsed.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte("package sample\n\nfunc PersistentTarget() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if response := indexRequest(s, ctx, "omnilsp/reindex"); response == nil || response.Error != nil {
		t.Fatalf("generation-2 reindex: %+v", response)
	}
	if stats := indexStats(t, s); stats.Generation != 2 {
		t.Fatalf("generation after recommit = %d, want 2", stats.Generation)
	}

	second, err := s.DefinitionEnvelope(ctx, mainURI, line, column)
	if err != nil {
		t.Fatalf("generation-2 definition: %v", err)
	}
	if backend.definitionCalls.Load() != 0 {
		t.Fatalf("current-generation definition fell back to the live backend: %+v", second.Value)
	}
	if len(second.Value) != 1 || second.Value[0].URI != targetURI || second.Value[0].Range.StartLine != 2 {
		t.Fatalf("definition after recommit = %+v, want the generation-2 location on line 2", second.Value)
	}
	if len(second.Evidence) != 1 || second.Evidence[0].IndexGen != 2 {
		t.Fatalf("definition evidence after recommit = %+v, want IndexGen 2", second.Evidence)
	}

	// A position that only generation 1 indexed (the old definition line in
	// target.go) must not be answered from the superseded generation.
	stale := callPersistentLocationRequest(t, s, "textDocument/definition", targetURI, 1, 5, false, 91)
	if backend.definitionCalls.Load() != 1 || len(stale) != 1 || stale[0].URI != "file:///live/definition.go" {
		t.Fatalf("superseded generation-1 position was served: calls=%d result=%+v", backend.definitionCalls.Load(), stale)
	}
}
