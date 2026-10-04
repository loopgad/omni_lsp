package interop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

type scipModelView struct {
	id   model.Identity
	file model.File
	text string
}

func (v scipModelView) Identity() model.Identity { return v.id }
func (v scipModelView) Walk(ctx context.Context, _ string, visit func(model.File) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return visit(v.file)
}
func (v scipModelView) Read(ctx context.Context, uri string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uri != v.file.URI {
		return nil, io.EOF
	}
	return io.NopCloser(strings.NewReader(v.text)), nil
}

type scipModelSink struct {
	symbols     []model.Symbol
	occurrences []model.Occurrence
	edges       []model.Edge
}

type scipRoundTripView struct {
	id       model.Identity
	files    map[string]model.File
	contents map[string]string
}

func (v scipRoundTripView) Identity() model.Identity { return v.id }
func (v scipRoundTripView) Walk(ctx context.Context, _ string, visit func(model.File) error) error {
	uris := make([]string, 0, len(v.files))
	for uri := range v.files {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	for _, uri := range uris {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(v.files[uri]); err != nil {
			return err
		}
	}
	return nil
}
func (v scipRoundTripView) Read(ctx context.Context, uri string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, ok := v.contents[uri]
	if !ok {
		return nil, io.EOF
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (s *scipModelSink) WriteSymbols(_ context.Context, values []model.Symbol) error {
	s.symbols = append(s.symbols, values...)
	return nil
}
func (s *scipModelSink) WriteOccurrences(_ context.Context, values []model.Occurrence) error {
	s.occurrences = append(s.occurrences, values...)
	return nil
}
func (s *scipModelSink) WriteEdges(_ context.Context, values []model.Edge) error {
	s.edges = append(s.edges, values...)
	return nil
}

func TestImportSCIPToModelNormalizesPositionsAndDeclaresLosses(t *testing.T) {
	view := scipModelView{
		id:   model.Identity{Workspace: "file:///repo", DiskDigest: "sha256:workspace", SnapshotRev: 4},
		file: model.File{URI: "file:///repo/main.go", LanguageID: "go", Size: int64(len("package main\n😀foo\n")), SHA256: "sha256:file"},
		text: "package main\n😀foo\n",
	}
	tools := []model.ToolIdentity{{Name: "rust-analyzer", Path: "C:/tools/rust-analyzer.exe", Version: "1.0", SHA256: strings.Repeat("a", 64)}}
	scope := model.Scope{ID: "go:module:repo", Language: "go", RootURI: "file:///repo", Build: model.BuildInputs{Environment: map[string]string{"GOOS": "windows"}}}
	scope.BuildContext = model.ComputeBuildContextID(scope, "scip-export", "1", "go1.26/windows-amd64", tools)
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: "scip-export", ExtractorVer: "1", Toolchain: "go1.26/windows-amd64", Tools: tools,
		Backend: identity.BackendID{Language: "go", Name: "test"},
	}
	request := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	index := &scip.Index{
		Metadata: &scip.Metadata{ProjectRoot: scope.RootURI, TextDocumentEncoding: scip.TextEncoding_UTF8},
		Documents: []*scip.Document{{
			Language: "go", RelativePath: "main.go",
			Symbols: []*scip.SymbolInformation{
				{Symbol: "go repo main#foo.", DisplayName: "foo", Kind: scip.SymbolInformation_Function,
					Relationships: []*scip.Relationship{{Symbol: "go repo main#Interface#", IsImplementation: true}}},
			},
			Occurrences: []*scip.Occurrence{{
				Symbol: "go repo main#foo.", SymbolRoles: int32(scip.SymbolRole_Definition),
				Range: []int32{1, 4, 7},
			}},
		}},
	}
	data, err := proto.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	sink := &scipModelSink{}
	report, err := ImportSCIPToModel(context.Background(), data, request, sink)
	if err != nil {
		t.Fatalf("ImportSCIPToModel: %v", err)
	}
	if len(sink.occurrences) != 1 || sink.occurrences[0].Range.StartChar != 2 || sink.occurrences[0].Range.EndChar != 5 {
		t.Fatalf("UTF-8 range was not normalized to UTF-16: %+v", sink.occurrences)
	}
	if len(sink.edges) != 1 || sink.edges[0].Kind != model.EdgeImplementation {
		t.Fatalf("implementation relation not imported: %+v", sink.edges)
	}
	if report.UsedTools[scope.ID][0] != tools[0] {
		t.Fatalf("used tool identity lost: %+v", report.UsedTools)
	}
	for _, coverage := range report.Coverage {
		if coverage.Fact == model.FactCall && coverage.State != model.Unknown {
			t.Fatalf("SCIP call coverage overstated: %+v", coverage)
		}
	}
}

func TestImportSCIPToModelRejectsInvalidUTF16LocationsAndLanguage(t *testing.T) {
	makeIndex := func(language string, sourceRange []int32) *scip.Index {
		return &scip.Index{
			Metadata: &scip.Metadata{ProjectRoot: "file:///repo", TextDocumentEncoding: scip.TextEncoding_UTF16},
			Documents: []*scip.Document{{
				Language: language, RelativePath: "main.go",
				Occurrences: []*scip.Occurrence{{Symbol: "go repo main#Foo.", Range: sourceRange}},
			}},
		}
	}
	testCases := []struct {
		name     string
		language string
		prepare  func(model.Request) model.Request
		rangeVal []int32
	}{
		{name: "out of bounds column", language: "go", rangeVal: []int32{0, 100, 100}},
		{name: "wrong document language", language: "rust", rangeVal: []int32{0, 0, 0}},
		{
			name: "surrogate pair split", language: "go", rangeVal: []int32{0, 1, 2},
			prepare: func(req model.Request) model.Request {
				view := req.View.(scipRoundTripView)
				uri := "file:///repo/main.go"
				view.contents[uri] = "😀x\n"
				hash := sha256.Sum256([]byte(view.contents[uri]))
				contentHash := identity.ContentHash("sha256:" + hex.EncodeToString(hash[:]))
				file := view.files[uri]
				file.Size, file.SHA256 = int64(len(view.contents[uri])), contentHash
				view.files[uri] = file
				req.View = view
				return req
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := scipRoundTripRequest(t)
			if tc.prepare != nil {
				req = tc.prepare(req)
			}
			data, err := proto.Marshal(makeIndex(tc.language, tc.rangeVal))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ImportSCIPToModel(context.Background(), data, req, &scipModelSink{}); !errors.Is(err, errInvalidSCIP) {
				t.Fatalf("invalid SCIP input error = %v, want fail-closed semantic index error", err)
			}
		})
	}
}

func TestImportSCIPToModelAcceptsUTF16UnicodeCRLFLocations(t *testing.T) {
	req, _ := scipRoundTripRequest(t)
	view := req.View.(scipRoundTripView)
	uri := "file:///repo/main.go"
	view.contents[uri] = "😀\r\nx\n"
	hash := sha256.Sum256([]byte(view.contents[uri]))
	contentHash := identity.ContentHash("sha256:" + hex.EncodeToString(hash[:]))
	file := view.files[uri]
	file.Size, file.SHA256 = int64(len(view.contents[uri])), contentHash
	view.files[uri] = file
	req.View = view
	index := &scip.Index{
		Metadata: &scip.Metadata{ProjectRoot: "file:///repo", TextDocumentEncoding: scip.TextEncoding_UTF16},
		Documents: []*scip.Document{{
			Language: "go", RelativePath: "main.go",
			Occurrences: []*scip.Occurrence{
				{Symbol: "go repo main#Foo.", Range: []int32{0, 2, 2}},
				{Symbol: "go repo main#Foo.", Range: []int32{1, 0, 1}},
			},
		}},
	}
	data, err := proto.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	sink := &scipModelSink{}
	if _, err := ImportSCIPToModel(context.Background(), data, req, sink); err != nil {
		t.Fatalf("valid UTF-16 CRLF locations rejected: %v", err)
	}
	if len(sink.occurrences) != 2 || sink.occurrences[0].Range.StartLine != 0 || sink.occurrences[0].Range.StartChar != 2 ||
		sink.occurrences[1].Range.StartLine != 1 || sink.occurrences[1].Range.EndChar != 1 {
		t.Fatalf("UTF-16 CRLF locations were not preserved: %+v", sink.occurrences)
	}
}

func TestImportSCIPToModelAcceptsBareCRLineBreaks(t *testing.T) {
	for _, encoding := range []scip.TextEncoding{scip.TextEncoding_UTF8, scip.TextEncoding_UTF16} {
		t.Run(encoding.String(), func(t *testing.T) {
			req, _ := scipRoundTripRequest(t)
			view := req.View.(scipRoundTripView)
			uri := "file:///repo/main.go"
			view.contents[uri] = "a\rb"
			hash := sha256.Sum256([]byte(view.contents[uri]))
			contentHash := identity.ContentHash("sha256:" + hex.EncodeToString(hash[:]))
			file := view.files[uri]
			file.Size, file.SHA256 = int64(len(view.contents[uri])), contentHash
			view.files[uri] = file
			req.View = view
			index := &scip.Index{
				Metadata: &scip.Metadata{ProjectRoot: "file:///repo", TextDocumentEncoding: encoding},
				Documents: []*scip.Document{{
					Language: "go", RelativePath: "main.go",
					Occurrences: []*scip.Occurrence{{Symbol: "go repo main#Foo.", Range: []int32{1, 0, 1}}},
				}},
			}
			data, err := proto.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			sink := &scipModelSink{}
			if _, err := ImportSCIPToModel(context.Background(), data, req, sink); err != nil {
				t.Fatalf("bare-CR line break rejected: %v", err)
			}
			if len(sink.occurrences) != 1 || sink.occurrences[0].Range.StartLine != 1 || sink.occurrences[0].Range.StartChar != 0 || sink.occurrences[0].Range.EndChar != 1 {
				t.Fatalf("bare-CR position was not preserved: %+v", sink.occurrences)
			}
		})
	}
}

func TestImportSCIPToModelRejectsUnsafeDocumentPath(t *testing.T) {
	if _, err := scipDocumentURI("file:///repo", "../outside.go"); err == nil {
		t.Fatal("unsafe relative path was accepted")
	}
	if _, err := scipDocumentURI("file:///repo", `..\outside.go`); err == nil {
		t.Fatal("Windows traversal path was accepted")
	}
	got, err := scipDocumentURI("file:///repo", `src\nested\main.go`)
	if err != nil || got != "file:///repo/src/nested/main.go" {
		t.Fatalf("safe Windows relative path = %q, %v", got, err)
	}
	if _, err := scipDocumentURI("file:///repo", strings.Repeat("a", maxSCIPRelativePathBytes+1)); err == nil {
		t.Fatal("oversized SCIP relative path was accepted")
	}
	if _, err := modelURIToSCIPPath("file:///repo", "file:///repo/"+strings.Repeat("a", maxSCIPRelativePathBytes+1)); err == nil {
		t.Fatal("oversized persistent source path was accepted")
	}
	if _, err := modelURIToSCIPPath("file:///repo", "file:///repo/%2e%2e/out.go"); err == nil {
		t.Fatal("escaped traversal path was accepted")
	}
}

func TestPersistentSemanticSCIPModelRoundTripAndDeclaredLoss(t *testing.T) {
	ctx := context.Background()
	req, hashes := scipRoundTripRequest(t)
	scope := req.Scopes[0]
	foo := identity.SymbolID("go repo main#Foo.")
	iface := identity.SymbolID("go repo main#IFoo#")
	occurrences := []model.Occurrence{
		{SymbolID: foo, ScopeID: scope.ID, URI: "file:///repo/main.go", Range: model.Position{StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 8}, Role: "definition", SourceHash: hashes["file:///repo/main.go"], BuildContext: scope.BuildContext},
		{SymbolID: foo, ScopeID: scope.ID, URI: "file:///repo/refs.go", Range: model.Position{StartLine: 1, StartChar: 3, EndLine: 1, EndChar: 6}, Role: "reference", SourceHash: hashes["file:///repo/refs.go"], BuildContext: scope.BuildContext},
	}
	edges := []model.Edge{{
		From: foo, To: iface, ScopeID: scope.ID, Kind: model.EdgeImplementation,
		SourceURI: "file:///repo/refs.go", Range: model.Position{StartLine: 4, StartChar: 2, EndLine: 4, EndChar: 8},
		SourceHash: hashes["file:///repo/refs.go"], BuildContext: scope.BuildContext,
	}}
	firstStore := persistSCIPRoundTripGeneration(t, req,
		[]model.Symbol{{ID: foo, ScopeID: scope.ID, Name: "Foo", Kind: "function", Signature: "func Foo()"}, {ID: iface, ScopeID: scope.ID, Name: "IFoo", Kind: "interface"}},
		occurrences, edges,
	)
	firstView, err := firstStore.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	firstReader, err := semantic.OpenReader(ctx, firstView)
	if err != nil {
		t.Fatal(err)
	}
	defer firstReader.Close()
	data, err := ExportSCIPFromSemantic(ctx, firstReader)
	if err != nil {
		t.Fatalf("ExportSCIPFromSemantic: %v", err)
	}
	scopedData, err := ExportSCIPSemanticScope(ctx, firstReader, scope.ID)
	if err != nil {
		t.Fatalf("ExportSCIPSemanticScope: %v", err)
	}
	if !bytes.Equal(data, scopedData) {
		t.Fatal("single-scope export differs from explicit scope export")
	}
	if _, err := ExportSCIPSemanticScope(ctx, firstReader, "missing-scope"); err == nil {
		t.Fatal("export accepted a scope that is absent from the verified generation")
	}
	var index scip.Index
	if err := proto.Unmarshal(data, &index); err != nil {
		t.Fatalf("decode exported SCIP: %v", err)
	}
	if proto.Size(&index) != len(data) || len(data) > maxSCIPStreamBytes {
		t.Fatalf("SCIP stream size is not exact and bounded: encoded=%d size=%d", len(data), proto.Size(&index))
	}
	if index.GetMetadata().GetProjectRoot() != scope.RootURI || index.GetMetadata().GetTextDocumentEncoding() != scip.TextEncoding_UTF16 {
		t.Fatalf("SCIP metadata lost source root or encoding: %+v", index.GetMetadata())
	}
	if !hasSCIPArgument(index.GetMetadata().GetToolInfo().GetArguments(), scipRepositoryArgPrefix+req.View.Identity().Repository) ||
		!hasSCIPArgument(index.GetMetadata().GetToolInfo().GetArguments(), commitArgPrefix+req.View.Identity().Revision) {
		t.Fatalf("SCIP metadata lost repository/revision identity: %+v", index.GetMetadata().GetToolInfo())
	}
	if len(index.GetDocuments()) != 2 || len(index.GetExternalSymbols()) != 1 {
		t.Fatalf("unexpected SCIP document/external symbol counts: docs=%d external=%d", len(index.GetDocuments()), len(index.GetExternalSymbols()))
	}
	var sawDefinition, sawReference, sawSignature, sawRelationship bool
	for _, doc := range index.GetDocuments() {
		for _, occurrence := range doc.GetOccurrences() {
			sawDefinition = sawDefinition || occurrence.GetSymbolRoles()&int32(scip.SymbolRole_Definition) != 0
			sawReference = sawReference || occurrence.GetSymbolRoles()&int32(scip.SymbolRole_Definition) == 0
		}
		for _, info := range doc.GetSymbols() {
			if info.GetSymbol() == string(foo) && info.GetSignatureDocumentation().GetText() == "func Foo()" && info.GetSignatureDocumentation().GetLanguage() == scope.Language {
				sawSignature = true
			}
			for _, relationship := range info.GetRelationships() {
				if info.GetSymbol() == string(foo) && relationship.GetSymbol() == string(iface) && relationship.GetIsImplementation() {
					sawRelationship = true
				}
			}
		}
	}
	if !sawDefinition || !sawReference || !sawSignature || !sawRelationship {
		t.Fatalf("SCIP round-trip facts missing: definition=%t reference=%t signature=%t relationship=%t", sawDefinition, sawReference, sawSignature, sawRelationship)
	}

	secondStore, secondSink, secondBuild := newSCIPRoundTripSink(t, req)
	importedReport, err := ImportSCIPToModel(ctx, data, req, secondSink)
	if err != nil {
		_ = secondBuild.Abort()
		t.Fatalf("ImportSCIPToModel: %v", err)
	}
	if err := secondSink.Finalize(ctx, importedReport); err != nil {
		_ = secondBuild.Abort()
		t.Fatalf("finalize imported generation: %v", err)
	}
	if err := secondBuild.Commit(ctx); err != nil {
		t.Fatalf("commit imported generation: %v", err)
	}
	secondView, err := secondStore.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secondReader, err := semantic.OpenReader(ctx, secondView)
	if err != nil {
		t.Fatal(err)
	}
	defer secondReader.Close()
	gotSymbols := map[identity.SymbolID]model.Symbol{}
	var gotOccurrences []model.Occurrence
	var gotEdges []model.Edge
	for {
		batch, err := secondReader.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch batch.Kind {
		case semantic.BatchSymbols:
			for _, symbol := range batch.Symbols {
				gotSymbols[symbol.ID] = symbol
			}
		case semantic.BatchOccurrences:
			gotOccurrences = append(gotOccurrences, batch.Occurrences...)
		case semantic.BatchEdges:
			gotEdges = append(gotEdges, batch.Edges...)
		}
	}
	if gotSymbols[foo].Name != "Foo" || gotSymbols[foo].Kind != "function" || gotSymbols[foo].Signature != "func Foo()" ||
		gotSymbols[iface].Kind != "interface" {
		t.Fatalf("symbol fields did not round-trip: %+v", gotSymbols)
	}
	if len(gotOccurrences) != 2 {
		t.Fatalf("occurrences or snapshot-bound source hashes were lost: %+v", gotOccurrences)
	}
	for _, occurrence := range gotOccurrences {
		if hashes[occurrence.URI] != occurrence.SourceHash || occurrence.BuildContext != scope.BuildContext {
			t.Fatalf("occurrence source binding changed: %+v", occurrence)
		}
	}
	if len(gotEdges) != 1 || gotEdges[0].Kind != model.EdgeImplementation || gotEdges[0].SourceURI != "file:///repo/refs.go" {
		t.Fatalf("relationship kind/source document did not round-trip: %+v", gotEdges)
	}
	if gotEdges[0].Range != (model.Position{}) {
		t.Fatalf("SCIP unexpectedly preserved an edge source range: %+v", gotEdges[0].Range)
	}
	if gotOccurrences[0].Range != occurrences[0].Range || gotOccurrences[1].Range != occurrences[1].Range {
		t.Fatalf("UTF-16 occurrence positions changed: got=%+v want=%+v", gotOccurrences, occurrences)
	}
	if !strings.Contains(strings.Join(LossyNotes(), "\n"), "SCIP 关系没有来源范围") {
		t.Fatal("the edge source range loss is not declared in LossyNotes")
	}
	if secondReader.Metadata().Identity.SnapshotRev != 0 || len(secondReader.Metadata().Provenance) != 1 {
		t.Fatalf("persistent source identity/provenance was not reattached: %+v", secondReader.Metadata())
	}
}

func TestSCIPExportBudgetArithmeticAndCancellation(t *testing.T) {
	if got, err := reserveSCIPBytes(0, maxSCIPStreamBytes); err != nil || got != maxSCIPStreamBytes {
		t.Fatalf("exact 64 MiB reservation = %d, %v", got, err)
	}
	if _, err := reserveSCIPBytes(maxSCIPStreamBytes, 1); err == nil {
		t.Fatal("reservation over 64 MiB was accepted")
	}
	if _, err := reserveSCIPBytes(-1, 1); err == nil {
		t.Fatal("negative current reservation was accepted")
	}
	if _, err := reserveSCIPBytes(0, -1); err == nil {
		t.Fatal("negative size reservation was accepted")
	}

	req, _ := scipRoundTripRequest(t)
	store := persistSCIPRoundTripGeneration(t, req, nil, nil, nil)
	view, err := store.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := semantic.OpenReader(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExportSCIPFromSemantic(ctx, reader); err != context.Canceled {
		t.Fatalf("cancelled export error = %v, want context.Canceled", err)
	}
}

func TestSCIPExportRejectsUnrepresentableEdges(t *testing.T) {
	req, hashes := scipRoundTripRequest(t)
	scope := req.Scopes[0]
	foo := identity.SymbolID("go repo main#Foo.")
	iface := identity.SymbolID("go repo main#IFoo#")
	store := persistSCIPRoundTripGeneration(t, req,
		[]model.Symbol{{ID: foo, ScopeID: scope.ID, Name: "Foo", Kind: "function"}, {ID: iface, ScopeID: scope.ID, Name: "IFoo", Kind: "interface"}},
		nil,
		[]model.Edge{{From: foo, To: iface, ScopeID: scope.ID, Kind: model.EdgeCall, SourceURI: "file:///repo/refs.go", SourceHash: hashes["file:///repo/refs.go"], BuildContext: scope.BuildContext}},
	)
	view, err := store.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := semantic.OpenReader(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := ExportSCIPFromSemantic(context.Background(), reader); !errors.Is(err, errInvalidSCIP) {
		t.Fatalf("unsupported call edge export error = %v, want fail-closed SCIP error", err)
	}
}

func TestSCIPLossyExportRequiresOptInAndDeclaresOmittedEdgeCounts(t *testing.T) {
	req, hashes := scipRoundTripRequest(t)
	scope := req.Scopes[0]
	foo := identity.SymbolID("go repo main#Foo.")
	iface := identity.SymbolID("go repo main#IFoo#")
	edges := []model.Edge{{
		From: foo, To: iface, ScopeID: scope.ID, Kind: model.EdgeImplementation,
		SourceURI: "file:///repo/refs.go", SourceHash: hashes["file:///repo/refs.go"], BuildContext: scope.BuildContext,
	}}
	callEdges := 0
	for _, kind := range []model.EdgeKind{
		model.EdgeCall, model.EdgeCall, model.EdgeImport, model.EdgeInclude, model.EdgeModule, model.EdgeGenerated,
	} {
		from, to := foo, iface
		if kind == model.EdgeCall && callEdges > 0 {
			from, to = iface, foo
		}
		if kind == model.EdgeCall {
			callEdges++
		}
		edges = append(edges, model.Edge{
			From: from, To: to, ScopeID: scope.ID, Kind: kind,
			SourceURI: "file:///repo/refs.go", SourceHash: hashes["file:///repo/refs.go"], BuildContext: scope.BuildContext,
		})
	}
	store := persistSCIPRoundTripGeneration(t, req,
		[]model.Symbol{{ID: foo, ScopeID: scope.ID, Name: "Foo", Kind: "function"}, {ID: iface, ScopeID: scope.ID, Name: "IFoo", Kind: "interface"}},
		[]model.Occurrence{{SymbolID: foo, ScopeID: scope.ID, URI: "file:///repo/main.go", Range: model.Position{StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 8}, Role: "definition", SourceHash: hashes["file:///repo/main.go"], BuildContext: scope.BuildContext}},
		edges,
	)
	view, err := store.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := semantic.OpenReader(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if _, err := ExportSCIPSemanticScope(context.Background(), reader, scope.ID); !errors.Is(err, errInvalidSCIP) || !strings.Contains(err.Error(), `cannot represent semantic edge kind "call"`) {
		t.Fatalf("strict export error = %v, want refusal of the unsupported call edge", err)
	}

	data, losses, err := ExportSCIPSemanticScopeWithLosses(context.Background(), reader, scope.ID)
	if err != nil {
		t.Fatalf("explicit lossy export: %v", err)
	}
	wantLosses := []SemanticSCIPExportLoss{
		{Kind: model.EdgeCall, Count: 2},
		{Kind: model.EdgeGenerated, Count: 1},
		{Kind: model.EdgeImport, Count: 1},
		{Kind: model.EdgeInclude, Count: 1},
		{Kind: model.EdgeModule, Count: 1},
	}
	if !reflect.DeepEqual(losses, wantLosses) {
		t.Fatalf("loss summary = %+v, want %+v", losses, wantLosses)
	}
	var exported scip.Index
	if err := proto.Unmarshal(data, &exported); err != nil {
		t.Fatalf("decode lossy export: %v", err)
	}
	if len(exported.GetDocuments()) == 0 || len(exported.GetDocuments()[0].GetOccurrences()) == 0 {
		t.Fatalf("lossy export dropped representable source facts: %+v", exported.GetDocuments())
	}
	wantMarkers := []string{
		"omnilsp.lossy-edge.call=2",
		"omnilsp.lossy-edge.generated_source=1",
		"omnilsp.lossy-edge.import=1",
		"omnilsp.lossy-edge.include=1",
		"omnilsp.lossy-edge.module=1",
	}
	for _, marker := range wantMarkers {
		if !hasSCIPArgument(exported.GetMetadata().GetToolInfo().GetArguments(), marker) {
			t.Errorf("lossy SCIP artifact omitted self-description %q: %v", marker, exported.GetMetadata().GetToolInfo().GetArguments())
		}
	}
	relationships := 0
	for _, doc := range exported.GetDocuments() {
		for _, symbol := range doc.GetSymbols() {
			for _, relationship := range symbol.GetRelationships() {
				relationships++
				if !relationship.GetIsImplementation() && !relationship.GetIsTypeDefinition() {
					t.Errorf("lossy export emitted an unsupported SCIP relationship: %+v", relationship)
				}
			}
		}
	}
	if relationships != 1 {
		t.Fatalf("representable implementation relationship count = %d, want 1", relationships)
	}
	again, repeatedLosses, err := ExportSCIPSemanticScopeWithLosses(context.Background(), reader, scope.ID)
	if err != nil {
		t.Fatalf("repeat explicit lossy export: %v", err)
	}
	if !bytes.Equal(data, again) || !reflect.DeepEqual(losses, repeatedLosses) {
		t.Fatal("lossy export or loss ordering is not deterministic")
	}
	if !strings.Contains(strings.Join(LossyNotes(), "\n"), "在 ToolInfo.Arguments 记录各类数量") {
		t.Fatal("explicit SCIP edge omission is not described in LossyNotes")
	}
}

func TestValidateSCIPSourceIdentityRejectsMismatch(t *testing.T) {
	current := model.Identity{Repository: "https://code.example/omnilsp", Revision: "abc123"}
	metadata := &scip.Metadata{ProjectRoot: "file:///repo", ToolInfo: &scip.ToolInfo{Arguments: []string{
		scipRepositoryArgPrefix + string(current.Repository), commitArgPrefix + current.Revision,
	}}}
	if err := validateSCIPSourceIdentity(metadata, current, "file:///repo"); err != nil {
		t.Fatalf("matching SCIP identity rejected: %v", err)
	}
	for name, candidate := range map[string]*scip.Metadata{
		"repository":           {ProjectRoot: "file:///repo", ToolInfo: &scip.ToolInfo{Arguments: []string{scipRepositoryArgPrefix + "https://attacker.invalid/repo"}}},
		"revision":             {ProjectRoot: "file:///repo", ToolInfo: &scip.ToolInfo{Arguments: []string{commitArgPrefix + "different"}}},
		"project root":         {ProjectRoot: "https://attacker.invalid/repo", ToolInfo: &scip.ToolInfo{}},
		"missing project root": {ToolInfo: &scip.ToolInfo{Arguments: []string{scipRepositoryArgPrefix + string(current.Repository)}}},
		"conflicting repository arguments": {ProjectRoot: "file:///repo", ToolInfo: &scip.ToolInfo{Arguments: []string{
			scipRepositoryArgPrefix + string(current.Repository), scipRepositoryArgPrefix + "https://attacker.invalid/repo",
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSCIPSourceIdentity(candidate, current, "file:///repo"); err == nil {
				t.Fatal("mismatched source identity was accepted")
			}
		})
	}
	if err := validateSCIPSourceIdentity(&scip.Metadata{ProjectRoot: "file:///unrelated"}, model.Identity{}, "file:///repo"); err == nil {
		t.Fatal("unverified workspace project root was accepted without repository identity")
	}
}

func scipRoundTripRequest(t *testing.T) (model.Request, map[string]identity.ContentHash) {
	t.Helper()
	contents := map[string]string{
		"file:///repo/main.go": "package main\nfunc Foo() {}\n",
		"file:///repo/refs.go": "package main\n// Foo\n",
	}
	hashes := make(map[string]identity.ContentHash, len(contents))
	files := make(map[string]model.File, len(contents))
	for uri, content := range contents {
		hash := sha256.Sum256([]byte(content))
		contentHash := identity.ContentHash("sha256:" + hex.EncodeToString(hash[:]))
		hashes[uri] = contentHash
		files[uri] = model.File{URI: uri, LanguageID: "go", Size: int64(len(content)), SHA256: contentHash}
	}
	diskHash := sha256.Sum256([]byte("scip round-trip workspace"))
	id := model.Identity{
		Workspace: "workspace-scip-round-trip", DiskDigest: identity.ContentHash("sha256:" + hex.EncodeToString(diskHash[:])),
		Repository: "https://code.example/omnilsp", Revision: "0123456789abcdef", SnapshotRev: 17,
	}
	view := scipRoundTripView{id: id, files: files, contents: contents}
	scope := model.Scope{ID: "go:module:repo", Language: "go", RootURI: "file:///repo", Build: model.BuildInputs{Options: map[string]string{}}}
	scope.BuildContext = model.ComputeBuildContextID(scope, "scip-importer", "1", "go-test-toolchain", nil)
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: id, Scope: scope,
		Extractor: "scip-importer", ExtractorVer: "1", Toolchain: "go-test-toolchain",
		Backend: identity.BackendID{Language: scope.Language, Name: "scip"},
	}
	return model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}, hashes
}

func newSCIPRoundTripSink(t *testing.T, req model.Request) (*persistent.FileStore, *semantic.Sink, persistent.BuildSession) {
	t.Helper()
	store, err := persistent.NewFileStore(t.TempDir(), persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	build, err := store.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sink, err := semantic.NewSink(context.Background(), build, req)
	if err != nil {
		_ = build.Abort()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sink.Close()
		_ = build.Abort()
	})
	return store, sink, build
}

func persistSCIPRoundTripGeneration(t *testing.T, req model.Request, symbols []model.Symbol, occurrences []model.Occurrence, edges []model.Edge) *persistent.FileStore {
	t.Helper()
	store, sink, build := newSCIPRoundTripSink(t, req)
	ctx := context.Background()
	if err := sink.WriteSymbols(ctx, symbols); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteOccurrences(ctx, occurrences); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteEdges(ctx, edges); err != nil {
		t.Fatal(err)
	}
	coverage := make([]model.Coverage, 0, len(model.RequiredFactKinds))
	for _, fact := range model.RequiredFactKinds {
		coverage = append(coverage, model.Coverage{ScopeID: req.Scopes[0].ID, Fact: fact, State: model.IncompleteKnownSubset, Reason: "round-trip fixture does not assert completeness"})
	}
	report := model.Report{Identity: req.View.Identity(), Coverage: coverage, UsedTools: map[string][]model.ToolIdentity{req.Scopes[0].ID: {}}}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatalf("finalize persistent semantic fixture: %v", err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatalf("commit persistent semantic fixture: %v", err)
	}
	return store
}

func hasSCIPArgument(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestImportSCIPToModelHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ImportSCIPToModel(ctx, []byte{1}, model.Request{}, &scipModelSink{}); err == nil {
		t.Fatal("cancelled import unexpectedly succeeded")
	}
}

func TestUTF8RangesAcceptOnlyEOFLineAfterTrailingNewline(t *testing.T) {
	ranges := []model.Position{{StartLine: 0, StartChar: 0, EndLine: 1, EndChar: 0}}
	converted, err := utf8RangesToUTF16(context.Background(), strings.NewReader("package p\n"), ranges)
	if err != nil || len(converted) != 1 || converted[0].EndLine != 1 || converted[0].EndChar != 0 {
		t.Fatalf("trailing-newline EOF range = %+v, %v", converted, err)
	}
	if _, err := utf8RangesToUTF16(context.Background(), strings.NewReader("package p"), ranges); err == nil {
		t.Fatal("one-past-EOF range without a trailing newline was accepted")
	}
	empty, err := utf8RangesToUTF16(context.Background(), strings.NewReader(""), []model.Position{{EndLine: 0, EndChar: 0}})
	if err != nil || len(empty) != 1 {
		t.Fatalf("empty-file EOF range = %+v, %v", empty, err)
	}
}
