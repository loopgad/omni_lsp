package interop

import (
	"bytes"
	"context"
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

// lsifRoundTripFixture 构造无边的语义生成：foo 在 main.go 定义、refs.go 引用。
// 符号记录携带显示名/kind/signature——LSIF 子集不编码这些字段（LossyNotes）。
func lsifRoundTripFixture(t *testing.T) (model.Request, map[string]identity.ContentHash, *persistent.FileStore) {
	t.Helper()
	req, hashes := scipRoundTripRequest(t)
	scope := req.Scopes[0]
	foo := identity.SymbolID("go repo main#Foo.")
	iface := identity.SymbolID("go repo main#IFoo#")
	occurrences := []model.Occurrence{
		{SymbolID: foo, ScopeID: scope.ID, URI: "file:///repo/main.go", Range: model.Position{StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 8}, Role: "definition", SourceHash: hashes["file:///repo/main.go"], BuildContext: scope.BuildContext},
		{SymbolID: foo, ScopeID: scope.ID, URI: "file:///repo/refs.go", Range: model.Position{StartLine: 1, StartChar: 3, EndLine: 1, EndChar: 6}, Role: "reference", SourceHash: hashes["file:///repo/refs.go"], BuildContext: scope.BuildContext},
	}
	store := persistSCIPRoundTripGeneration(t, req,
		[]model.Symbol{{ID: foo, ScopeID: scope.ID, Name: "Foo", Kind: "function", Signature: "func Foo()"}, {ID: iface, ScopeID: scope.ID, Name: "IFoo", Kind: "interface"}},
		occurrences, nil,
	)
	return req, hashes, store
}

// openSemanticReader 便捷封装：打开已提交生成的校验 Reader。
func openSemanticReader(t *testing.T, store *persistent.FileStore) *semantic.Reader {
	t.Helper()
	view, err := store.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := semantic.OpenReader(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	return reader
}

// TestLSIFSemanticExportPreservesProvenanceAndRepeats 钉住 Semantic→LSIF 导出桥：
// provenance（projectRoot/commitId/repository/tool 版本）进入 metaData 且可经
// Canonical 导入恢复；同一生成两次导出逐位一致；取消的 ctx 返回取消错误。
func TestLSIFSemanticExportPreservesProvenanceAndRepeats(t *testing.T) {
	ctx := context.Background()
	req, _, store := lsifRoundTripFixture(t)
	reader := openSemanticReader(t, store)

	first, err := ExportLSIFFromSemantic(ctx, reader)
	if err != nil {
		t.Fatalf("ExportLSIFFromSemantic: %v", err)
	}
	second, err := ExportLSIFFromSemantic(ctx, reader)
	if err != nil {
		t.Fatalf("repeat ExportLSIFFromSemantic: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("LSIF semantic export is not bitwise deterministic across repeats")
	}
	scoped, err := ExportLSIFSemanticScope(ctx, reader, req.Scopes[0].ID)
	if err != nil {
		t.Fatalf("ExportLSIFSemanticScope: %v", err)
	}
	if !bytes.Equal(first, scoped) {
		t.Fatal("single-scope LSIF export differs from explicit scope export")
	}
	if _, err := ExportLSIFSemanticScope(ctx, reader, "missing-scope"); err == nil {
		t.Fatal("export accepted a scope that is absent from the verified generation")
	}

	for _, marker := range []string{
		`"projectRoot":"file:///repo"`,
		`"commitId":"0123456789abcdef"`,
		`"name":"omnilsp"`,
		`"version":"1"`,
		`"args":["omnilsp.repository=https://code.example/omnilsp"]`,
		`"positionEncoding":"utf-16"`,
	} {
		if !strings.Contains(string(first), marker) {
			t.Errorf("LSIF export lost provenance marker %s:\n%s", marker, first)
		}
	}

	canonical, err := ImportLSIF(first)
	if err != nil {
		t.Fatalf("ImportLSIF: %v", err)
	}
	wantIdentity := req.View.Identity()
	if canonical.Metadata.RepoIdentity != wantIdentity.Repository {
		t.Errorf("repository identity lost: got %q want %q", canonical.Metadata.RepoIdentity, wantIdentity.Repository)
	}
	if canonical.Metadata.CommitID != wantIdentity.Revision {
		t.Errorf("revision identity lost: got %q want %q", canonical.Metadata.CommitID, wantIdentity.Revision)
	}
	if canonical.Metadata.ToolVersion != "1" {
		t.Errorf("tool version lost: got %q", canonical.Metadata.ToolVersion)
	}
	if canonical.Metadata.Provenance != lsifProvenance || canonical.Metadata.FreshnessLimit == "" {
		t.Errorf("import provenance/freshness not marked: %+v", canonical.Metadata)
	}
	if len(canonical.Documents) != 2 {
		t.Fatalf("documents: got %d want 2", len(canonical.Documents))
	}
	symbol := string(identity.SymbolID("go repo main#Foo."))
	type occKey struct {
		path, symbol string
		startLine    int32
		defined      bool
	}
	got := map[occKey]bool{}
	for _, doc := range canonical.Documents {
		for _, occ := range doc.Occurrences {
			got[occKey{doc.Path, occ.SymbolName, occ.Range.StartLine, occ.Role == RoleDefinition}] = true
		}
	}
	for _, want := range []occKey{
		{"repo/main.go", symbol, 1, true},
		{"repo/refs.go", symbol, 1, false},
	} {
		if !got[want] {
			t.Errorf("occurrence missing after canonical import: %+v (got %v)", want, got)
		}
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := ExportLSIFFromSemantic(cancelled, reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled export error = %v, want context.Canceled", err)
	}
}

// TestPersistentSemanticLSIFModelRoundTripBitwiseDeterministic 是三步往返确定性测试：
// ① 语义生成 → ExportLSIFFromSemantic → L1；② L1 → ImportLSIFToModel → 提交为
// 新语义生成；③ 新生成再导出 → L2。断言 L1 与 L2 逐位相等（bitwise），
// 且 coverage 如实声明 LSIF 无法编码的 fact family。
func TestPersistentSemanticLSIFModelRoundTripBitwiseDeterministic(t *testing.T) {
	ctx := context.Background()
	req, hashes, storeA := lsifRoundTripFixture(t)
	scope := req.Scopes[0]
	readerA := openSemanticReader(t, storeA)

	step1, err := ExportLSIFFromSemantic(ctx, readerA)
	if err != nil {
		t.Fatalf("step1 ExportLSIFFromSemantic: %v", err)
	}

	storeB, sinkB, buildB := newSCIPRoundTripSink(t, req)
	report, err := ImportLSIFToModel(ctx, step1, req, sinkB)
	if err != nil {
		_ = buildB.Abort()
		t.Fatalf("step2 ImportLSIFToModel: %v", err)
	}
	if err := sinkB.Finalize(ctx, report); err != nil {
		_ = buildB.Abort()
		t.Fatalf("finalize imported generation: %v", err)
	}
	if err := buildB.Commit(ctx); err != nil {
		t.Fatalf("commit imported generation: %v", err)
	}
	states := map[model.FactKind]model.Completeness{}
	reasons := map[model.FactKind]string{}
	for _, coverage := range report.Coverage {
		states[coverage.Fact] = coverage.State
		reasons[coverage.Fact] = coverage.Reason
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactDefinition, model.FactReference} {
		if states[fact] != model.IncompleteKnownSubset {
			t.Errorf("coverage %q = %q, want incomplete_known_subset", fact, states[fact])
		}
	}
	for _, fact := range []model.FactKind{model.FactImplementation, model.FactTypeRelation, model.FactCall, model.FactImport, model.FactInclude, model.FactModule, model.FactGenerated, model.FactDeclaration} {
		if states[fact] != model.Unknown || reasons[fact] == "" {
			t.Errorf("coverage %q = %q (reason %q), want unknown with a reason", fact, states[fact], reasons[fact])
		}
	}
	if report.Identity != req.View.Identity() {
		t.Fatalf("report identity mismatch: %+v", report.Identity)
	}

	readerB := openSemanticReader(t, storeB)
	step3, err := ExportLSIFFromSemantic(ctx, readerB)
	if err != nil {
		t.Fatalf("step3 ExportLSIFFromSemantic: %v", err)
	}
	if !bytes.Equal(step1, step3) {
		t.Fatalf("three-step round trip is not bitwise deterministic:\nstep1:\n%s\nstep3:\n%s", step1, step3)
	}

	var gotOccurrences []model.Occurrence
	gotSymbols := map[identity.SymbolID]model.Symbol{}
	if err := readerB.Rewind(); err != nil {
		t.Fatal(err)
	}
	for {
		batch, err := readerB.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch batch.Kind {
		case semantic.BatchSymbols:
			for _, s := range batch.Symbols {
				gotSymbols[s.ID] = s
			}
		case semantic.BatchOccurrences:
			gotOccurrences = append(gotOccurrences, batch.Occurrences...)
		case semantic.BatchEdges:
			t.Fatalf("LSIF import must not fabricate edges: %+v", batch.Edges)
		}
	}
	foo := identity.SymbolID("go repo main#Foo.")
	if len(gotOccurrences) != 2 {
		t.Fatalf("occurrences lost: %+v", gotOccurrences)
	}
	sort.Slice(gotOccurrences, func(i, j int) bool { return gotOccurrences[i].URI < gotOccurrences[j].URI })
	if gotOccurrences[0].URI != "file:///repo/main.go" || gotOccurrences[0].Role != "definition" ||
		gotOccurrences[0].Range != (model.Position{StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 8}) {
		t.Errorf("definition occurrence changed: %+v", gotOccurrences[0])
	}
	if gotOccurrences[1].URI != "file:///repo/refs.go" || gotOccurrences[1].Role != "reference" ||
		gotOccurrences[1].Range != (model.Position{StartLine: 1, StartChar: 3, EndLine: 1, EndChar: 6}) {
		t.Errorf("reference occurrence changed: %+v", gotOccurrences[1])
	}
	for _, occurrence := range gotOccurrences {
		if occurrence.SymbolID != foo || occurrence.ScopeID != scope.ID ||
			occurrence.SourceHash != hashes[occurrence.URI] || occurrence.BuildContext != scope.BuildContext {
			t.Errorf("occurrence identity binding changed: %+v", occurrence)
		}
	}
	if len(gotSymbols) != 1 || gotSymbols[foo].Name != "" || gotSymbols[foo].Kind != "" || gotSymbols[foo].Signature != "" {
		t.Fatalf("imported symbol records must be bare identities (declared loss): %+v", gotSymbols)
	}
	notes := strings.Join(LossyNotes(), "\n")
	for _, marker := range []string{"显示名/kind/signature 不进 LSIF", "omnilsp.lossy-edge."} {
		if !strings.Contains(notes, marker) {
			t.Errorf("LossyNotes does not declare the LSIF loss %q", marker)
		}
	}
}

// TestLSIFSemanticExportRefusesEdgesAndLossyDeclaresCounts：严格导出拒绝一切
// 语义边（LSIF 子集无编码位）；显式有损导出省略并在 toolInfo.args 记录计数。
func TestLSIFSemanticExportRefusesEdgesAndLossyDeclaresCounts(t *testing.T) {
	ctx := context.Background()
	req, hashes := scipRoundTripRequest(t)
	scope := req.Scopes[0]
	foo := identity.SymbolID("go repo main#Foo.")
	iface := identity.SymbolID("go repo main#IFoo#")
	store := persistSCIPRoundTripGeneration(t, req,
		[]model.Symbol{{ID: foo, ScopeID: scope.ID, Name: "Foo", Kind: "function"}, {ID: iface, ScopeID: scope.ID, Name: "IFoo", Kind: "interface"}},
		[]model.Occurrence{{SymbolID: foo, ScopeID: scope.ID, URI: "file:///repo/main.go", Range: model.Position{StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 8}, Role: "definition", SourceHash: hashes["file:///repo/main.go"], BuildContext: scope.BuildContext}},
		[]model.Edge{{From: foo, To: iface, ScopeID: scope.ID, Kind: model.EdgeImplementation, SourceURI: "file:///repo/refs.go", SourceHash: hashes["file:///repo/refs.go"], BuildContext: scope.BuildContext}},
	)
	reader := openSemanticReader(t, store)

	if _, err := ExportLSIFFromSemantic(ctx, reader); !errors.Is(err, errInvalidLSIF) || !strings.Contains(err.Error(), `cannot represent semantic edge kind "implementation"`) {
		t.Fatalf("strict export error = %v, want refusal of the unrepresentable edge", err)
	}

	data, losses, err := ExportLSIFFromSemanticWithLosses(ctx, reader)
	if err != nil {
		t.Fatalf("explicit lossy export: %v", err)
	}
	wantLosses := []SemanticLSIFExportLoss{{Kind: model.EdgeImplementation, Count: 1}}
	if !reflect.DeepEqual(losses, wantLosses) {
		t.Fatalf("loss summary = %+v, want %+v", losses, wantLosses)
	}
	if !strings.Contains(string(data), `"omnilsp.lossy-edge.implementation=1"`) {
		t.Fatalf("lossy LSIF artifact omitted its self-description:\n%s", data)
	}
	again, repeatedLosses, err := ExportLSIFFromSemanticWithLosses(ctx, reader)
	if err != nil {
		t.Fatalf("repeat lossy export: %v", err)
	}
	if !bytes.Equal(data, again) || !reflect.DeepEqual(losses, repeatedLosses) {
		t.Fatal("lossy export or loss ordering is not deterministic")
	}
}

// TestConvertSCIPToLSIFMatchesDirectSemanticExport：同一无边的已验证生成，
// 经 SCIP 转码的 LSIF 与 Reader 直出的 LSIF 必须逐位一致。
func TestConvertSCIPToLSIFMatchesDirectSemanticExport(t *testing.T) {
	ctx := context.Background()
	_, _, store := lsifRoundTripFixture(t)
	reader := openSemanticReader(t, store)

	direct, err := ExportLSIFFromSemantic(ctx, reader)
	if err != nil {
		t.Fatalf("ExportLSIFFromSemantic: %v", err)
	}
	scipData, err := ExportSCIPFromSemantic(ctx, reader)
	if err != nil {
		t.Fatalf("ExportSCIPFromSemantic: %v", err)
	}
	viaSCIP, err := ConvertSCIPToLSIF(scipData)
	if err != nil {
		t.Fatalf("ConvertSCIPToLSIF: %v", err)
	}
	if !bytes.Equal(direct, viaSCIP) {
		t.Fatalf("SCIP-transcoded LSIF differs from the direct semantic export:\ndirect:\n%s\nviaSCIP:\n%s", direct, viaSCIP)
	}
	if _, err := ConvertSCIPToLSIF(nil); err == nil {
		t.Fatal("empty SCIP input was accepted")
	}
	if _, err := ConvertSCIPToLSIF([]byte{0xff}); err == nil {
		t.Fatal("undecodable SCIP input was accepted")
	}
}

// TestConvertLSIFToSCIPFeedsModelImport：LSIF→SCIP 转码（CLI lsif 导入路径）
// 产出 UTF-16 声明的 SCIP 流，身份扩展转录为 ToolInfo.Arguments，并可直接
// 喂给已验证的 SCIP 模型导入；完整链路 LSIF→SCIP→模型→SCIP→LSIF 逐位还原 L1。
func TestConvertLSIFToSCIPFeedsModelImport(t *testing.T) {
	ctx := context.Background()
	req, hashes, store := lsifRoundTripFixture(t)
	reader := openSemanticReader(t, store)
	lsifData, err := ExportLSIFFromSemantic(ctx, reader)
	if err != nil {
		t.Fatalf("ExportLSIFFromSemantic: %v", err)
	}

	converted, err := ConvertLSIFToSCIP(lsifData)
	if err != nil {
		t.Fatalf("ConvertLSIFToSCIP: %v", err)
	}
	if _, err := ConvertLSIFToSCIP(nil); err == nil {
		t.Fatal("empty LSIF input was accepted")
	}
	var index scip.Index
	if err := proto.Unmarshal(converted, &index); err != nil {
		t.Fatalf("decode converted SCIP: %v", err)
	}
	if index.GetMetadata().GetProjectRoot() != "file:///repo" || index.GetMetadata().GetTextDocumentEncoding() != scip.TextEncoding_UTF16 {
		t.Fatalf("converted SCIP metadata lost root/encoding: %+v", index.GetMetadata())
	}
	if !hasSCIPArgument(index.GetMetadata().GetToolInfo().GetArguments(), scipRepositoryArgPrefix+string(req.View.Identity().Repository)) ||
		!hasSCIPArgument(index.GetMetadata().GetToolInfo().GetArguments(), commitArgPrefix+req.View.Identity().Revision) {
		t.Fatalf("converted SCIP lost identity arguments: %+v", index.GetMetadata().GetToolInfo())
	}

	storeC, sinkC, buildC := newSCIPRoundTripSink(t, req)
	report, err := ImportSCIPToModel(ctx, converted, req, sinkC)
	if err != nil {
		_ = buildC.Abort()
		t.Fatalf("ImportSCIPToModel on converted stream: %v", err)
	}
	if err := sinkC.Finalize(ctx, report); err != nil {
		_ = buildC.Abort()
		t.Fatalf("finalize: %v", err)
	}
	if err := buildC.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	readerC := openSemanticReader(t, storeC)
	var gotOccurrences []model.Occurrence
	for {
		batch, err := readerC.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		gotOccurrences = append(gotOccurrences, batch.Occurrences...)
	}
	if len(gotOccurrences) != 2 {
		t.Fatalf("occurrences lost through LSIF→SCIP→model: %+v", gotOccurrences)
	}
	sort.Slice(gotOccurrences, func(i, j int) bool { return gotOccurrences[i].URI < gotOccurrences[j].URI })
	if gotOccurrences[0].Role != "definition" || gotOccurrences[1].Role != "reference" ||
		gotOccurrences[0].Range.StartChar != 5 || gotOccurrences[1].Range.StartChar != 3 {
		t.Fatalf("positions/roles changed through transcode: %+v", gotOccurrences)
	}
	for _, occurrence := range gotOccurrences {
		if occurrence.SourceHash != hashes[occurrence.URI] {
			t.Fatalf("source binding lost: %+v", occurrence)
		}
	}

	scipAgain, err := ExportSCIPFromSemantic(ctx, readerC)
	if err != nil {
		t.Fatalf("re-export SCIP: %v", err)
	}
	lsifAgain, err := ConvertSCIPToLSIF(scipAgain)
	if err != nil {
		t.Fatalf("re-transcode LSIF: %v", err)
	}
	if !bytes.Equal(lsifData, lsifAgain) {
		t.Fatalf("LSIF→SCIP→model→SCIP→LSIF is not bitwise deterministic:\nfirst:\n%s\nagain:\n%s", lsifData, lsifAgain)
	}
}

// TestLSIFImportReadsToolInfoExtensions 钉住 Canonical ImportLSIF 补全：
// metaData.toolInfo 的版本与 omnilsp.repository=/omnilsp.commit= 扩展被读取；
// projectRoot/commitId 仍是缺省来源。
func TestLSIFImportReadsToolInfoExtensions(t *testing.T) {
	lines := []string{
		`{"id":1,"label":"metaData","version":"0.5.0","positionEncoding":"utf-16","projectRoot":"file:///repo","commitId":"abc1234def","toolInfo":{"name":"omnilsp","version":"v9.8","args":["omnilsp.repository=github.com/omnilsp/omni"]}}`,
		`{"id":2,"label":"document","uri":"file:///a.go","languageId":"go"}`,
		`{"id":3,"label":"range","start":{"line":0,"character":0},"end":{"line":0,"character":3}}`,
		`{"id":4,"label":"resultSet","symbol":"go demo A."}`,
		`{"id":5,"label":"referenceResult"}`,
		`{"id":6,"label":"contains","outV":2,"inVs":[3]}`,
		`{"id":7,"label":"textDocument/references","outV":4,"inV":5}`,
		`{"id":8,"label":"next","outV":3,"inV":4}`,
		`{"id":9,"label":"item","outV":5,"inV":3,"property":"definitions"}`,
	}
	out, err := ImportLSIF([]byte(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatalf("ImportLSIF: %v", err)
	}
	if out.Metadata.RepoIdentity != "github.com/omnilsp/omni" {
		t.Errorf("repository extension ignored: got %q", out.Metadata.RepoIdentity)
	}
	if out.Metadata.CommitID != "abc1234def" {
		t.Errorf("commitId field ignored: got %q", out.Metadata.CommitID)
	}
	if out.Metadata.ToolVersion != "v9.8" {
		t.Errorf("toolInfo.version ignored: got %q", out.Metadata.ToolVersion)
	}

	// 无 toolInfo 时回落 projectRoot/commitId（既有行为），commitId 缺失时
	// 回落 omnilsp.commit= 参数。
	fallback := []string{
		`{"id":1,"label":"metaData","version":"0.5.0","positionEncoding":"utf-16","projectRoot":"legacy/repo","toolInfo":{"name":"omnilsp","args":["omnilsp.commit=feed0001"]}}`,
		`{"id":2,"label":"document","uri":"file:///a.go","languageId":"go"}`,
	}
	out, err = ImportLSIF([]byte(strings.Join(fallback, "\n") + "\n"))
	if err != nil {
		t.Fatalf("ImportLSIF fallback: %v", err)
	}
	if out.Metadata.RepoIdentity != "legacy/repo" || out.Metadata.CommitID != "feed0001" {
		t.Errorf("fallback identity = %+v", out.Metadata)
	}
}
