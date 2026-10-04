// 性能基准套件（goal.md §S17 基准元数据 + §S18 交互 SLO）。
//
// 复用 test/golden 的 harness 模式：全新 Server + 确定性无工具链 fake backend，
// 不依赖磁盘工具链。与 golden 不同，计时统一走 CallMethod 门面——它与 LSP 传输
// 层共用同一 dispatcher 路径（C2 之外的 handler→快照源→后端→JSON 投影完全一致，
// 见 server.go §C0），且同步返回、零 goroutine 跳数。golden 的 5ms 轮询式
// waitResponse 会淹没微秒级被测对象，故基准不采用传输层驱动。
//
// textDocument/completion 的 handler 存在（internal/runtime/server/handlers.go
// handleCompletion，已在 registerHandlers 注册），因此无需 §S18 documented
// exception，completion 基准正常纳入。
package perf

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/protocol/lsp"
	"github.com/omnilsp/omni/internal/runtime/server"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

// --- 固定语料 -------------------------------------------------------------------

// buildCorpus 确定性生成 ~200 行合成 Go 语料，内嵌于本文件、零磁盘依赖。
// 结构：头 3 行 + 39 个函数 × 5 行 = 198 行。
func buildCorpus() string {
	var b strings.Builder
	b.WriteString("// OmniLSP 性能基准合成语料（确定性生成，勿手改）\n")
	b.WriteString("package perf\n\n")
	for i := 0; i < 39; i++ {
		fmt.Fprintf(&b, "func sym%03d(x int) int {\n\ty := x*%d + %d\n\treturn y + sym%03d(y)\n}\n\n",
			i, i, i+1, (i+1)%39)
	}
	return b.String()
}

var corpus = buildCorpus()

const corpusNewlines = 198

const (
	docURI    = "file:///w/main.go"
	hoverLine = 28 // 第 5 个函数签名行："func sym005(x int) int {"
	hoverChar = 7  // 位于符号名 sym005 内部
	editLine  = 4  // func sym000 首行 body："\ty := x*0 + 1"
	editFrom  = 6  // 该行字符 6 是 'x'
	editTo    = 7
	warmupOps = 32 // 每次计时的预热点数（§S17 warm state）
)

// --- 确定性 fake backend（模式取自 test/golden/golden_test.go）--------------------

type fakeBackend struct{}

func (fakeBackend) LanguageID() string       { return "go" }
func (fakeBackend) FileExtensions() []string { return []string{".go"} }

var perfEvidence = []identity.Evidence{{
	Kind:      identity.EvidenceSemantic,
	Assurance: identity.AssuranceIndexedExact,
	Snapshot:  identity.SnapshotID{Workspace: "w", Revision: 1},
	Backend:   identity.BackendID{Language: "go", Name: "perf-fake"},
	SourceHash: identity.ContentHash(
		"sha256:0000000000000000000000000000000000000000000000000000000000000000"),
}}

func (f fakeBackend) Completion(_ context.Context, _ languages.CompletionRequest) ([]languages.CompletionItem, error) {
	items := make([]languages.CompletionItem, 64)
	for i := range items {
		items[i] = languages.CompletionItem{Label: fmt.Sprintf("cand%03d", i), Kind: i%8 + 1}
	}
	return items, nil
}

func (f fakeBackend) Diagnostics(_ context.Context, _ string, _ []byte) ([]languages.Diagnostic, error) {
	return nil, nil
}

func (f fakeBackend) DocumentSymbols(_ context.Context, _ languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	return nil, nil
}

func (f fakeBackend) WorkspaceSymbols(_ context.Context, _ languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	return nil, nil
}

func (f fakeBackend) SemanticTokens(_ context.Context, _ string, _ []byte) ([]languages.SemanticToken, error) {
	return nil, nil
}

func (f fakeBackend) Close() error { return nil }

func (f fakeBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value: &languages.HoverResult{
			Contents: "func sym005(x int) int",
			Range:    &languages.Range{StartLine: hoverLine, StartCharacter: 5, EndLine: hoverLine, EndCharacter: 11},
			Evidence: languages.EvidenceL2,
		},
		Evidence:     perfEvidence,
		Completeness: identity.Complete,
	}, nil
}

func (f fakeBackend) Definition(_ context.Context, _ languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{
		Status: identity.ResultExact,
		Value: []languages.Location{{
			URI:   docURI,
			Range: languages.Range{StartLine: 3, StartCharacter: 5, EndLine: 3, EndCharacter: 11},
		}},
		Evidence:     perfEvidence,
		Completeness: identity.Complete,
	}, nil
}

func (f fakeBackend) References(_ context.Context, req languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	locs := []languages.Location{{
		URI:   docURI,
		Range: languages.Range{StartLine: 4, StartCharacter: 8, EndLine: 4, EndCharacter: 9},
	}}
	if req.IncludeDecl {
		locs = append(locs, languages.Location{
			URI:   docURI,
			Range: languages.Range{StartLine: 3, StartCharacter: 5, EndLine: 3, EndCharacter: 11},
		})
	}
	return identity.SemanticResult[[]languages.Location]{Value: locs, Evidence: perfEvidence, Completeness: identity.Complete}, nil
}

func (f fakeBackend) Rename(_ context.Context, _ languages.RenameRequest) (identity.SemanticResult[languages.ValidatedEdit], error) {
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status: identity.ResultExact,
		Value: languages.ValidatedEdit{
			Complete: false,
			Edits:    []languages.TextEdit{{URI: docURI, NewText: "renamed"}},
		},
		InternalDiagnostics: []string{"reference set completeness unproven"},
	}, nil
}

// --- harness（复用 golden 的「新 Server + fake backend」模式）----------------------

type harness struct {
	srv *server.Server
	ctx context.Context
	ver int64 // 与 didOpen 的 Version: 1 对齐，edit 前自增
}

func newHarness() *harness {
	srv := server.New(server.DefaultConfig())
	srv.RegisterBackend("go", fakeBackend{})
	h := &harness{srv: srv, ctx: context.Background(), ver: 1}
	// Facade mutating methods are lifecycle-gated (C2): initialize first.
	if _, err := h.call("initialize", map[string]any{"processId": 1, "rootUri": "file:///w"}); err != nil {
		panic("initialize failed: " + err.Error())
	}
	if _, err := h.call("initialized", map[string]any{}); err != nil {
		panic("initialized failed: " + err.Error())
	}
	if _, err := h.call("textDocument/didOpen", server.DidOpenTextDocumentParams{
		TextDocument: lsp.TextDocumentItem{URI: docURI, LanguageID: "go", Version: 1, Text: corpus},
	}); err != nil {
		panic("didOpen failed: " + err.Error())
	}
	return h
}

func (h *harness) call(method string, params any) (json.RawMessage, error) {
	return h.srv.CallMethod(h.ctx, method, params)
}

func ensure(res json.RawMessage, err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if len(res) == 0 || string(res) == "null" {
		return fmt.Errorf("%s: 结果为空，基准失效", what)
	}
	return nil
}

func (h *harness) hover() error {
	res, err := h.call("textDocument/hover", server.HoverParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: docURI},
		Position:     lsp.Position{Line: hoverLine, Character: hoverChar},
	})
	return ensure(res, err, "hover")
}

func (h *harness) definition() error {
	res, err := h.call("textDocument/definition", server.DefinitionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: docURI},
		Position:     lsp.Position{Line: hoverLine, Character: hoverChar},
	})
	return ensure(res, err, "definition")
}

func (h *harness) completion() error {
	res, err := h.call("textDocument/completion", server.CompletionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: docURI},
		Position:     lsp.Position{Line: hoverLine, Character: hoverChar},
	})
	return ensure(res, err, "completion")
}

// edit 发送一次 didChange；CallMethod 返回时新快照已发布可服务，
// 因此该调用耗时即「编辑后语法更新到快照可服务」的延迟（§S18）。
func (h *harness) edit() error {
	h.ver++
	_, err := h.call("textDocument/didChange", server.DidChangeTextDocumentParams{
		TextDocument: lsp.VersionedTextDocumentIdentifier{URI: docURI, Version: h.ver},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{{
			Range: &lsp.Range{
				Start: lsp.Position{Line: editLine, Character: editFrom},
				End:   lsp.Position{Line: editLine, Character: editTo},
			},
			Text: "q", // 等长替换单字符，位置永久有效
		}},
	})
	return err
}

// --- 基准（§S18 四项操作，ms/op 经 b.ReportMetric 上报）----------------------------

func benchOp(b *testing.B, op func() error) {
	for i := 0; i < warmupOps; i++ {
		if err := op(); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := op(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(b.Elapsed().Seconds()*1000/float64(b.N), "ms/op")
}

func BenchmarkHotHover(b *testing.B) { benchOp(b, newHarness().hover) }

func BenchmarkHotDefinition(b *testing.B) { benchOp(b, newHarness().definition) }

// completion handler 存在（见文件头注释），无 §S18 documented exception。
func BenchmarkCompletionFirstResult(b *testing.B) { benchOp(b, newHarness().completion) }

func BenchmarkSyntaxUpdateAfterEdit(b *testing.B) { benchOp(b, newHarness().edit) }

// --- §S17 元数据 -----------------------------------------------------------------

// BenchmarkMetadata 承载 §S17 要求的全部十项元数据。
type BenchmarkMetadata struct {
	Hardware        string
	OSFilesystem    string
	ProductBuild    string
	BackendVersions string
	Toolchains      string
	CorpusRevision  string
	ColdWarmState   string
	CacheState      string
	WorkerCount     string
	MemoryLimit     string
}

// s17Keys 按 goal.md §S17 原文顺序列出十个元数据键。
var s17Keys = [10]string{
	"hardware", "OS/filesystem", "product build/commit", "backend versions",
	"toolchains", "repository/corpus revision", "cold/warm state", "cache state",
	"worker count", "memory limit",
}

func collectBenchmarkMetadata() BenchmarkMetadata {
	cpu := os.Getenv("PROCESSOR_IDENTIFIER") // ponytail: env 探测 CPU 型号，跨平台精确型号需 cgo/syscall 时再加
	if cpu == "" {
		cpu = "unknown-cpu(" + runtime.GOARCH + ")"
	}
	// ponytail: 文件系统类型未探测（Windows 卷信息需 syscall）；标 unavailable，
	// 需要时用 GetVolumeInformationW 补上。
	m := BenchmarkMetadata{
		Hardware:     fmt.Sprintf("%s; %d logical cores", cpu, runtime.NumCPU()),
		OSFilesystem: fmt.Sprintf("%s/%s; fs=unavailable", runtime.GOOS, runtime.GOARCH),
		CorpusRevision: fmt.Sprintf("sha256:%x (%d lines, 内嵌合成语料)",
			sha256.Sum256([]byte(corpus)), corpusNewlines),
		BackendVersions: "perf-fake-go v1 (确定性无工具链测试后端)",
		ColdWarmState:   fmt.Sprintf("warm (每基准先预热 %d 次)", warmupOps),
		CacheState:      "进程内已预热；确定性无状态 fake 后端，缓存不适用",
		WorkerCount:     fmt.Sprintf("GOMAXPROCS=%d", runtime.GOMAXPROCS(0)),
	}
	if lim := debug.SetMemoryLimit(-1); lim >= math.MaxInt64/2 {
		m.MemoryLimit = "unlimited (GOMEMLIMIT 未设置)"
	} else {
		m.MemoryLimit = fmt.Sprintf("%.0f MiB", float64(lim)/(1<<20))
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, dirty string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "+dirty"
				}
			}
		}
		if rev != "" {
			m.ProductBuild = rev + dirty
		}
	}
	if m.ProductBuild == "" {
		m.ProductBuild = "dev" // vcs 信息缺失（如 go run / 无 git 环境）
	}
	m.Toolchains = probeToolchains()
	return m
}

// probeToolchains 探测 PATH 上的常见语言工具链；全部失败标 "unavailable"。
func probeToolchains() string {
	var found []string
	for _, name := range []string{"go", "gopls", "rustc", "cargo", "node"} {
		if p, err := exec.LookPath(name); err == nil {
			found = append(found, name+"("+p+")")
		}
	}
	if len(found) == 0 {
		return "unavailable"
	}
	return strings.Join(found, ", ")
}

// RenderBanner 输出 §S17 全部十项元数据的对齐横幅。
func (m BenchmarkMetadata) RenderBanner() string {
	vals := [10]string{
		m.Hardware, m.OSFilesystem, m.ProductBuild, m.BackendVersions,
		m.Toolchains, m.CorpusRevision, m.ColdWarmState, m.CacheState,
		m.WorkerCount, m.MemoryLimit,
	}
	var b strings.Builder
	b.WriteString("==== OmniLSP 基准元数据（goal.md §S17） ====\n")
	for i, k := range s17Keys {
		fmt.Fprintf(&b, "%-28s: %s\n", k, vals[i])
	}
	return b.String()
}

// --- 测试 -----------------------------------------------------------------------

// TestS17_MetadataPresent 断言横幅包含 §S17 全部十个键。
func TestS17_MetadataPresent(t *testing.T) {
	if got := strings.Count(corpus, "\n"); got != corpusNewlines {
		t.Fatalf("语料行数漂移：got %d, want %d", got, corpusNewlines)
	}
	banner := collectBenchmarkMetadata().RenderBanner()
	for _, k := range s17Keys {
		if !strings.Contains(banner, k) {
			t.Errorf("横幅缺少 §S17 键 %q", k)
		}
	}
	t.Logf("\n%s", banner)
}

// percentile 返回升序样本的分位值（最近秩统计量，不做插值）。
func percentile(sortedAsc []time.Duration, q float64) time.Duration {
	n := len(sortedAsc)
	idx := int(math.Ceil(q*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sortedAsc[idx]
}

// batchOps 是每个分位样本包含的操作次数。单次操作在微秒级，而 Windows 计时器
// 粒度约 0.5ms（实测 time.Since 对 µs 级忙循环返回 0），逐次采样会全部塌缩为
// 零值；每样本批量执行再取均值才能跨过多个时钟刻度。
// ponytail: 固定批量 256；需要真分布而非均值近似时换 QPC/平台高精度时钟。
const batchOps = 256

// TestS18_FacadeOverhead measures batch-mean quantiles for the synchronous
// facade with a fake backend. Passing only bounds facade overhead; it does
// not establish end-to-end S18 SLOs on a representative Tier S corpus.
func TestS18_FacadeOverhead(t *testing.T) {
	const sloIters = 250 // ≥200（§S18 样本量要求）
	cases := []struct {
		name                            string
		p50Target, p95Target, p99Target time.Duration
		op                              func(*harness) error
	}{
		{"hot hover", 20 * time.Millisecond, 75 * time.Millisecond, 150 * time.Millisecond, (*harness).hover},
		{"hot definition", 25 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond, (*harness).definition},
		{"completion first usable", 40 * time.Millisecond, 120 * time.Millisecond, 250 * time.Millisecond, (*harness).completion},
		{"syntax update after edit", 15 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, (*harness).edit},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			run := func() error { return tc.op(h) }
			for i := 0; i < warmupOps; i++ {
				if err := run(); err != nil {
					t.Fatalf("预热失败: %v", err)
				}
			}
			samples := make([]time.Duration, sloIters)
			for i := range samples {
				start := time.Now()
				for j := 0; j < batchOps; j++ {
					if err := run(); err != nil {
						t.Fatalf("第 %d 批第 %d 次迭代失败: %v", i, j, err)
					}
				}
				samples[i] = time.Since(start) / batchOps
			}
			sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
			p50 := percentile(samples, 0.50)
			p95 := percentile(samples, 0.95)
			p99 := percentile(samples, 0.99)
			t.Logf("%s: P50=%v/op (目标 ≤%v)  P95=%v/op (目标 ≤%v)  P99=%v/op (目标 ≤%v)  [每样本 %d 次均值]",
				tc.name, p50, tc.p50Target, p95, tc.p95Target, p99, tc.p99Target, batchOps)
			if p50 > tc.p50Target {
				t.Errorf("%s P50 %v 超出 §S18 目标 %v", tc.name, p50, tc.p50Target)
			}
			if p95 > tc.p95Target {
				t.Errorf("%s P95 %v 超出 §S18 目标 %v", tc.name, p95, tc.p95Target)
			}
			if p99 > tc.p99Target {
				t.Errorf("%s P99 %v 超出 §S18 目标 %v", tc.name, p99, tc.p99Target)
			}
		})
	}
}

// --- §S19: real Go backend reference scaling ---------------------------------

// TestS19_ReferencesScalingCurve exercises the actual Go reference walk over
// valid sources with a growing number of results. It measures warm query cost
// and verifies that a canceled request returns a canceled terminal outcome.
func TestS19_ReferencesScalingCurve(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	sizes := []int{200, 800, 3200}
	perOp := make([]time.Duration, 0, len(sizes))
	for _, count := range sizes {
		be, req := newGoReferencesFixture(t, count)
		ctx := context.Background()
		check := func() {
			res, err := be.References(ctx, req)
			if err != nil || len(res.Value) < count {
				t.Fatalf("references@%d: %d locations, status=%v, err=%v", count, len(res.Value), res.Status, err)
			}
		}
		check() // warm the package loader
		var total time.Duration
		const samples = 10
		for i := 0; i < samples; i++ {
			start := time.Now()
			check()
			total += time.Since(start)
		}
		perOp = append(perOp, total/samples)
	}
	for i := 1; i < len(perOp); i++ {
		// Fourfold growth may be noisy on shared CI hosts; flag clear blowup.
		if perOp[i-1] > 0 && perOp[i] > perOp[i-1]*20 {
			t.Errorf("superlinear blowup: %v -> %v at size %d->%d",
				perOp[i-1], perOp[i], sizes[i-1], sizes[i])
		}
	}
	t.Logf("Go references scaling: %v/op @%d, %v/op @%d, %v/op @%d references",
		perOp[0], sizes[0], perOp[1], sizes[1], perOp[2], sizes[2])

	be, req := newGoReferencesFixture(t, 800)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := be.References(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("canceled references returned %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("cancelled references took %v; cancellation must be honored promptly", d)
	}
}

func newGoReferencesFixture(t *testing.T, count int) (*golang.Backend, languages.ReferencesRequest) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module perf\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("package perf\nvar target int\nfunc use() {\n")
	for i := 0; i < count; i++ {
		b.WriteString("_ = target\n")
	}
	b.WriteString("}\n")
	content := []byte(b.String())
	path := filepath.Join(dir, "references.go")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	be := golang.New(dir)
	t.Cleanup(func() { _ = be.Close() })
	return be, languages.ReferencesRequest{
		URI: uri.FromPath(path).String(), Content: content,
		Line: 1, Column: 5, SnapshotRev: 1, IncludeDecl: true,
	}
}
