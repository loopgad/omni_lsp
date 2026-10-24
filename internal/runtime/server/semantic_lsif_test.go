package server

// X52 server 原生 LSIF API 的往返钉子：ImportSemanticLSIF → ExportSemanticLSIF
// 与 SCIP 原生路径共享同一套空库/信任/新鲜度守卫，仅在 interop 转换步骤分岔。
// 两段式往返（L1 导入→导出 E1→再导入→导出 E2）必须逐位一致，且 LSIF 导入的
// coverage 如实声明格式无法证明的 fact family。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// lsifServerFixture 在给定 workspace 上组装一个带 go:module:fixture 语义
// 提供者的服务器（独立空索引目录；与 semantic_scip_test.go 的
// scipFixtureProvider 同一形态）。往返两段共用同一 workspace，保证 metaData
// 的 projectRoot 身份对第二台服务器仍然成立。
func lsifServerFixture(t *testing.T, workspace string) *Server {
	t.Helper()
	cfg := DefaultConfig()
	cfg.IndexDir = t.TempDir()
	srv := New(cfg)
	provider := scipFixtureProvider{}
	srv.RegisterSemanticIndexProvider("go", provider, provider)
	srv.InitializeWorkspace(workspace)
	return srv
}

// lsifFixtureStream 产出与 emitLSIF 相同顶点/边次序的最小 LSIF 0.5.x 流：
// metaData/document/range/contains/resultSet/referenceResult/
// textDocument/references/next/item(definitions)。
func lsifFixtureStream(t *testing.T, rootURI, docURI string) []byte {
	t.Helper()
	lines := []string{
		fmt.Sprintf(`{"id":1,"label":"metaData","version":"0.5.0","positionEncoding":"utf-16","projectRoot":%q,"toolInfo":{"name":"omnilsp","version":"1"}}`, rootURI),
		fmt.Sprintf(`{"id":2,"label":"document","uri":%q,"languageId":"go"}`, docURI),
		`{"id":3,"label":"range","start":{"line":1,"character":5},"end":{"line":1,"character":8}}`,
		`{"id":4,"label":"resultSet","symbol":"go repo main#Foo."}`,
		`{"id":5,"label":"referenceResult"}`,
		`{"id":6,"label":"contains","outV":2,"inVs":[3]}`,
		`{"id":7,"label":"textDocument/references","outV":4,"inV":5}`,
		`{"id":8,"label":"next","outV":3,"inV":4}`,
		`{"id":9,"label":"item","outV":5,"inV":3,"property":"definitions"}`,
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func TestSemanticLSIFImportExportRoundTripIsNativeAndDeterministic(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	ctx := context.Background()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\nfunc Foo() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(workspace).Canonical()
	docURI := uri.FromPath(filepath.Join(workspace, "main.go")).Canonical()
	first := lsifServerFixture(t, workspace)
	lsifIn := lsifFixtureStream(t, rootURI, docURI)

	if _, err := first.ImportSemanticLSIF(ctx, "go", "go:module:fixture", nil); err == nil || !strings.Contains(err.Error(), "between 1 and") {
		t.Fatalf("empty LSIF import error = %v, want the bounded-input refusal", err)
	}
	imported, err := first.ImportSemanticLSIF(ctx, "go", "go:module:fixture", lsifIn)
	if err != nil {
		t.Fatalf("ImportSemanticLSIF: %v", err)
	}
	if imported.Generation == 0 || imported.ScopeID != "go:module:fixture" {
		t.Fatalf("import report does not identify the created generation: %+v", imported)
	}
	for _, coverage := range imported.Coverage {
		if coverage.State == model.Complete {
			t.Fatalf("LSIF import overstated %s coverage: %+v", coverage.Fact, coverage)
		}
	}

	exported1, err := first.ExportSemanticLSIF(ctx, "go:module:fixture")
	if err != nil {
		t.Fatalf("ExportSemanticLSIF: %v", err)
	}
	for _, marker := range []string{
		`"projectRoot":"` + rootURI + `"`,
		`"symbol":"go repo main#Foo."`,
		`"start":{"line":1,"character":5},"end":{"line":1,"character":8}`,
		`"property":"definitions"`,
	} {
		if !strings.Contains(string(exported1), marker) {
			t.Fatalf("LSIF export lost marker %s:\n%s", marker, exported1)
		}
	}
	if strings.Contains(string(exported1), "omnilsp.lossy-edge.") {
		t.Fatalf("strict export must not carry loss markers:\n%s", exported1)
	}

	// 第二段：E1 再导入第二个空库服务器（同一 workspace、新索引目录）→
	// 再导出 E2，必须逐位一致。
	second := lsifServerFixture(t, workspace)
	reimported, err := second.ImportSemanticLSIF(ctx, "go", "go:module:fixture", exported1)
	if err != nil {
		t.Fatalf("second ImportSemanticLSIF: %v", err)
	}
	if reimported.Generation == 0 {
		t.Fatalf("reimport report does not identify a generation: %+v", reimported)
	}
	exported2, err := second.ExportSemanticLSIF(ctx, "go:module:fixture")
	if err != nil {
		t.Fatalf("second ExportSemanticLSIF: %v", err)
	}
	if string(exported1) != string(exported2) {
		t.Fatalf("server LSIF round trip is not bitwise deterministic:\nfirst:\n%s\nsecond:\n%s", exported1, exported2)
	}

	// 守卫面：既有生成不可被替换；缺失 scope 不可导出；打开文档时不可导出。
	if _, err := first.ImportSemanticLSIF(ctx, "go", "go:module:fixture", lsifIn); err == nil || !strings.Contains(err.Error(), "refuses to replace existing generation") {
		t.Fatalf("second import did not preserve the existing generation: %v", err)
	}
	if _, err := first.ExportSemanticLSIF(ctx, "go:module:missing"); err == nil || !strings.Contains(err.Error(), "not present in the generation") {
		t.Fatalf("missing scope export error = %v, want an interop scope refusal", err)
	}
	first.vfs.Open(docURI, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	if _, err := first.ExportSemanticLSIF(ctx, "go:module:fixture"); err == nil || !strings.Contains(err.Error(), "requires closed documents") {
		t.Fatalf("export with open documents error = %v, want the overlay-mixing refusal", err)
	}
}

func TestSemanticLSIFExportLossTrackMirrorsSCIP(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	ctx := context.Background()
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
	if _, err := srv.ExportSemanticLSIF(ctx, scopeID); err == nil || !strings.Contains(err.Error(), `cannot represent semantic edge kind "module"`) {
		t.Fatalf("strict LSIF export error = %v, want rejection of the module edge", err)
	}
	data, losses, err := srv.ExportSemanticLSIFWithLosses(context.Background(), scopeID)
	if err != nil {
		t.Fatalf("explicit lossy LSIF export: %v", err)
	}
	if len(losses) != 1 || losses[0].Kind != model.EdgeModule || losses[0].Count != 1 {
		t.Fatalf("loss summary = %+v, want one omitted module edge", losses)
	}
	if !strings.Contains(string(data), `"omnilsp.lossy-edge.module=1"`) {
		t.Fatalf("lossy LSIF artifact omitted its self-description:\n%s", data)
	}
}
