// Golden snapshot tests (goal.md S0/S6/T7).
//
// 每个场景驱动一个全新 Server + 确定性 fake backend，通过内存管道收发 LSP
// 消息，收集全部响应帧并规范化后与 testdata/<scenario>.json 快照比对。
//
// S6 纪律：更新 golden 是显式行为（OMNISP_UPDATE_GOLDEN=1），禁止静默更新；
// 规范化只抹平协议格式差异（id / 时间戳 / tmp 路径 / 时变计数器），不掩盖
// 语义变化。
package golden

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/server"
)

// --- 确定性 fake backend -----------------------------------------------------

// fakeBackend 返回完全硬编码的结果，保证快照逐字节可复现。
type fakeBackend struct{}

func newFakeBackend() *fakeBackend { return &fakeBackend{} }

func (f *fakeBackend) LanguageID() string       { return "rust" }
func (f *fakeBackend) FileExtensions() []string { return []string{".rs"} }

func (f *fakeBackend) Completion(_ context.Context, _ languages.CompletionRequest) ([]languages.CompletionItem, error) {
	return nil, nil
}

func (f *fakeBackend) Diagnostics(_ context.Context, _ string, _ []byte) ([]languages.Diagnostic, error) {
	return nil, nil
}

func (f *fakeBackend) DocumentSymbols(_ context.Context, _ languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	return nil, nil
}

func (f *fakeBackend) WorkspaceSymbols(_ context.Context, _ languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	return nil, nil
}

func (f *fakeBackend) SemanticTokens(_ context.Context, _ string, _ []byte) ([]languages.SemanticToken, error) {
	return nil, nil
}

func (f *fakeBackend) Close() error { return nil }

// fixedEvidence 是所有语义结果共用的确定性证据记录（供 omnilsp/explain 使用）。
var fixedEvidence = []identity.Evidence{{
	Kind:      identity.EvidenceSemantic,
	Assurance: identity.AssuranceIndexedExact,
	Snapshot:  identity.SnapshotID{Workspace: "w", Revision: 1},
	Backend:   identity.BackendID{Language: "rust", Name: "golden-fake"},
	SourceHash: identity.ContentHash(
		"sha256:0000000000000000000000000000000000000000000000000000000000000000"),
}}

func (f *fakeBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value: &languages.HoverResult{
			Contents: "fn demo(x: i32) -> i32",
			Range:    &languages.Range{StartLine: 2, StartCharacter: 3, EndLine: 2, EndCharacter: 7},
			Evidence: languages.EvidenceL2,
		},
		Evidence:     fixedEvidence,
		Completeness: identity.Complete,
	}, nil
}

func (f *fakeBackend) Definition(_ context.Context, _ languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{
		Status: identity.ResultExact,
		Value: []languages.Location{{
			URI:   "file:///w/demo.rs",
			Range: languages.Range{StartLine: 3, StartCharacter: 7, EndLine: 3, EndCharacter: 11},
		}},
		Evidence:     fixedEvidence,
		Completeness: identity.Complete,
	}, nil
}

func (f *fakeBackend) References(_ context.Context, req languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	locs := []languages.Location{{
		URI:   "file:///w/demo.rs",
		Range: languages.Range{StartLine: 4, StartCharacter: 4, EndLine: 4, EndCharacter: 5},
	}}
	if req.IncludeDecl {
		locs = append(locs, languages.Location{
			URI:   "file:///w/demo.rs",
			Range: languages.Range{StartLine: 2, StartCharacter: 3, EndLine: 2, EndCharacter: 7},
		})
	}
	return identity.SemanticResult[[]languages.Location]{
		Value:        locs,
		Evidence:     fixedEvidence,
		Completeness: identity.Complete,
	}, nil
}

// Rename 刻意返回未证明完整性的结果（SEM-SAFE-001 fail-closed 路径）。
func (f *fakeBackend) Rename(_ context.Context, _ languages.RenameRequest) (identity.SemanticResult[languages.ValidatedEdit], error) {
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status: identity.ResultExact,
		Value: languages.ValidatedEdit{
			Complete: false,
			Edits:    []languages.TextEdit{{URI: "file:///w/demo.rs", NewText: "renamed"}},
		},
		InternalDiagnostics: []string{"reference set completeness unproven"},
	}, nil
}

// --- 内存管道 transport -------------------------------------------------------

type pipeTransport struct {
	mu     sync.Mutex
	writes []*jsonrpc.Message
	in     chan *jsonrpc.Message
	done   chan struct{}
	once   sync.Once
}

func newPipeTransport() *pipeTransport {
	return &pipeTransport{in: make(chan *jsonrpc.Message, 32), done: make(chan struct{})}
}

func (p *pipeTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	select {
	case m := <-p.in:
		return m, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.done:
		return nil, errors.New("transport closed")
	}
}

func (p *pipeTransport) Write(_ context.Context, msg *jsonrpc.Message) error {
	p.mu.Lock()
	p.writes = append(p.writes, msg)
	n := len(p.writes)
	p.mu.Unlock()
	_ = n
	return nil
}

func (p *pipeTransport) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.writes)
}

func (p *pipeTransport) snapshot() []*jsonrpc.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*jsonrpc.Message(nil), p.writes...)
}

func (p *pipeTransport) waitResponse(id jsonrpc.RequestID, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range p.snapshot() {
			if m.ID != nil && m.ID.Equals(id) && m.Method == "" {
				return true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func (p *pipeTransport) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *pipeTransport) Done() <-chan struct{} { return p.done }

// --- 场景定义 -----------------------------------------------------------------

const (
	docURI  = "file:///w/demo.rs"
	docText = "module demo\n\nfn demo(x: i32) -> i32 {\n    x + 1\n}\n"
)

type scenario struct {
	name string
	// normalizeShape 为 true 时把响应中所有数字叶子归一为 "<n>"（用于
	// omnilsp/status、omnilsp/explain 等计数时变的响应；golden 的价值在
	// 形状稳定而非数据丰富）。
	normalizeShape bool
	msgs           []string // 原始 LSP JSON-RPC 消息（客户端视角）
}

func handshake(msgs ...string) []string {
	pre := []string{
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"processId":1,"rootUri":"file:///w"}}`,
		`{"jsonrpc":"2.0","method":"initialized","params":{}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"` + docURI + `","languageId":"rust","version":1,"text":` + quoteJSON(docText) + `}}}`,
	}
	return append(pre, msgs...)
}

func hoverReq(id int) string {
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"textDocument/hover","params":{"textDocument":{"uri":"` + docURI + `"},"position":{"line":2,"character":5}}}`
}

func posParams(id int, method string) string {
	return `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"` + method + `","params":{"textDocument":{"uri":"` + docURI + `"},"position":{"line":2,"character":5},"context":{"includeDeclaration":true}}}`
}

func scenarios() []scenario {
	return []scenario{
		{
			name: "initialize",
			msgs: []string{
				`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"processId":1,"rootUri":"file:///w"}}`,
			},
		},
		{
			name: "hover",
			msgs: handshake(hoverReq(7)),
		},
		{
			name: "definition",
			msgs: handshake(posParams(7, "textDocument/definition")),
		},
		{
			name: "references",
			msgs: handshake(posParams(7, "textDocument/references")),
		},
		{
			name: "rename-refusal",
			msgs: handshake(`{"jsonrpc":"2.0","id":7,"method":"textDocument/rename","params":{"textDocument":{"uri":"` + docURI + `"},"position":{"line":2,"character":5},"newName":"renamed"}}`),
		},
		{
			name:           "omnilsp-status",
			normalizeShape: true,
			msgs:           handshake(`{"jsonrpc":"2.0","id":8,"method":"omnilsp/status"}`),
		},
		{
			name:           "omnilsp-explain",
			normalizeShape: true,
			msgs:           handshake(hoverReq(7), `{"jsonrpc":"2.0","id":9,"method":"omnilsp/explain","params":{"uri":"`+docURI+`"}}`),
		},

		// §S9 edit-sequence scenarios: multi-step sessions whose value is the
		// interaction ORDER (edit→query→cancel→lifecycle), frozen as goldens.

		{
			name: "seq-edit-query-cycle",
			msgs: handshake(
				posParams(7, "textDocument/references"),
				// didChange bumps the snapshot; the next query must reflect it.
				`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"`+docURI+`","version":2},"contentChanges":[{"text":`+quoteJSON(docText+`
fn extra() {}
`)+`}]}}`,
				posParams(8, "textDocument/references"),
				hoverReq(9),
			),
		},
		{
			name: "seq-cancel-inflight",
			msgs: handshake(
				posParams(7, "textDocument/references"),
				`{"jsonrpc":"2.0","method":"$/cancelRequest","params":{"id":7}}`,
				posParams(8, "textDocument/definition"),
			),
		},
		{
			name: "seq-diagnostics-pull",
			msgs: handshake(
				`{"jsonrpc":"2.0","id":7,"method":"textDocument/diagnostic","params":{"textDocument":{"uri":"`+docURI+`"}}}`,
				posParams(8, "textDocument/hover"),
			),
		},
		{
			name: "seq-codeaction-and-signature",
			msgs: handshake(
				`{"jsonrpc":"2.0","id":7,"method":"textDocument/codeAction","params":{"textDocument":{"uri":"`+docURI+`"},"range":{"start":{"line":2,"character":4},"end":{"line":2,"character":5}},"context":{}}}`,
				`{"jsonrpc":"2.0","id":8,"method":"textDocument/signatureHelp","params":{"textDocument":{"uri":"`+docURI+`"},"position":{"line":2,"character":7}}}`,
			),
		},
	}
}

// --- 执行器 -------------------------------------------------------------------

// runScenario 起新 Server + fake backend，顺序发送 msgs（每个请求等到自己的
// 响应再发下一条，消除调度乱序），最后经 500ms 静默窗口收尾兜底帧。
func runScenario(t *testing.T, sc scenario) [][]byte {
	t.Helper()

	srv := server.New(server.DefaultConfig())
	srv.RegisterBackend("rust", newFakeBackend())
	pt := newPipeTransport()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run(ctx, pt) }()

	for _, raw := range sc.msgs {
		var msg jsonrpc.Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("scenario %s: bad message %s: %v", sc.name, raw, err)
		}
		select {
		case pt.in <- &msg:
		case <-time.After(5 * time.Second):
			t.Fatalf("scenario %s: server read loop stalled", sc.name)
		}
		if msg.ID != nil && !msg.ID.IsNull {
			if !pt.waitResponse(*msg.ID, 5*time.Second) {
				t.Fatalf("scenario %s: no response for request %s", sc.name, msg.Method)
			}
		} else {
			time.Sleep(20 * time.Millisecond) // 通知类消息留出 inline 处理时间
		}
	}

	// 静默超时收尾：500ms 无新帧即认为本场景帧已到齐。
	const quiet = 500 * time.Millisecond
	last := pt.count()
	stable := time.Duration(0)
	for stable < quiet {
		time.Sleep(25 * time.Millisecond)
		stable += 25 * time.Millisecond
		if n := pt.count(); n != last {
			last = n
			stable = 0
		}
	}
	cancel()
	select {
	case <-runErr:
	default:
	}

	frames := make([][]byte, 0, pt.count())
	for _, m := range pt.snapshot() {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		frames = append(frames, b)
	}
	if len(frames) == 0 {
		t.Fatalf("scenario %s produced no frames", sc.name)
	}
	return frames
}

// --- 规范化（S6：只归一协议格式差异）-------------------------------------------

// normalizeFrames 把帧数组规范化成 canonical JSON 文本：
//   - 对象键递归排序（Go map marshal 天然按键排序）
//   - 顶层 "id" → "<id>"（数字 id 随场景变化）
//   - "jsonrpc":"2.0" 保留
//   - 时变值占位：时间戳 → "<ts>"，sourceHash → "<hash>"，
//     snapshot/snapshotRev → "<rev>"
//   - 绝对路径中的临时目录前缀 → "<tmp>"
//   - shape 模式额外把所有数字叶子 → "<n>"（计数器/队列长度等时点采样）
//
// 纯函数：同一输入恒得同一输出（TestGoldenNormalizeIdempotent 保证）。
func normalizeFrames(frames [][]byte, shape bool, tmpDir string) (string, error) {
	out := make([]any, 0, len(frames))
	for _, f := range frames {
		var v any
		if err := json.Unmarshal(f, &v); err != nil {
			return "", fmt.Errorf("normalize: bad frame: %w", err)
		}
		out = append(out, normalizeValue(v, shape, tmpDir))
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // 占位符 "<id>" 保持原样，golden 可读
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func normalizeValue(v any, shape bool, tmpDir string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			switch k {
			case "id":
				out[k] = "<id>"
			case "at":
				out[k] = "<ts>"
			case "sourceHash":
				out[k] = "<hash>"
			case "snapshot", "snapshotRev":
				out[k] = "<rev>"
			default:
				out[k] = normalizeValue(val, shape, tmpDir)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalizeValue(e, shape, tmpDir)
		}
		return out
	case string:
		s := strings.ReplaceAll(x, tmpDir, "<tmp>")
		s = strings.ReplaceAll(s, filepath.ToSlash(tmpDir), "<tmp>")
		if _, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return "<ts>"
		}
		return s
	case float64:
		if shape {
			return "<n>"
		}
		return x
	default:
		return v // bool / null
	}
}

// firstDiff 返回两段文本的第一处差异行对，便于直接定位漂移帧。
func firstDiff(want, got string) string {
	wl := strings.Split(want, "\n")
	gl := strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var w, g string
		if i < len(wl) {
			w = wl[i]
		}
		if i < len(gl) {
			g = gl[i]
		}
		if w != g {
			return fmt.Sprintf("line %d\n  golden: %s\n  actual: %s", i+1, w, g)
		}
	}
	return "no textual diff"
}

// --- 测试 ---------------------------------------------------------------------

// TestGoldenScenarios 是主比对入口：缺文件 FAIL 并提示更新命令；不一致 FAIL
// 并打印首个 diff 行；OMNISP_UPDATE_GOLDEN=1 时写入并 PASS（S6 显式更新）。
func TestGoldenScenarios(t *testing.T) {
	tmpDir := t.TempDir()
	update := os.Getenv("OMNISP_UPDATE_GOLDEN") == "1"

	for _, sc := range scenarios() {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			got, err := normalizeFrames(runScenario(t, sc), sc.normalizeShape, tmpDir)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", sc.name+".json")

			if update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				// goal.md S6: No unexplained “golden update” to hide regressions.
				// 更新只能由显式环境变量触发，且必须伴随代码评审说明原因。
				if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("updated golden %s", path)
				return
			}

			want, err := os.ReadFile(path)
			if errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("golden file missing: %s\ncreate it with:\n  OMNISP_UPDATE_GOLDEN=1 go test -count=1 ./test/golden/", path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != got+"\n" {
				t.Fatalf("golden mismatch in %s\nfirst diff: %s", path, firstDiff(string(want), got+"\n"))
			}
		})
	}
}

// TestGoldenNormalizeIdempotent 自检规范化纯度：同一输入两次归一相等；
// 不同 id / 时间戳 / 计数值归一到相同输出（防 golden 腐化）。
func TestGoldenNormalizeIdempotent(t *testing.T) {
	tmpDir := t.TempDir()
	frame := []byte(`{"jsonrpc":"2.0","id":42,"result":{"state":"running","totalEnq":7,"nested":{"snapshotRev":3,"at":"2026-08-23T10:00:00.123456789Z","path":"` + filepath.ToSlash(tmpDir) + `/x"}}}`)

	a, err := normalizeFrames([][]byte{frame}, true, tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := normalizeFrames([][]byte{frame}, true, tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("normalize not deterministic:\na=%s\nb=%s", a, b)
	}
	if !strings.Contains(a, `"id": "<id>"`) || strings.Contains(a, "42") {
		t.Errorf("numeric id not normalized: %s", a)
	}
	if strings.Contains(a, tmpDir) || strings.Contains(a, "2026-08-23") {
		t.Errorf("tmp path or timestamp leaked: %s", a)
	}

	// 不同 id / 不同计数的同类帧必须归一到相同文本。
	frame2 := []byte(`{"jsonrpc":"2.0","id":"req-x","result":{"state":"running","totalEnq":99,"nested":{"snapshotRev":9,"at":"1999-01-01T00:00:00Z","path":"` + filepath.ToSlash(tmpDir) + `/x"}}}`)
	c, err := normalizeFrames([][]byte{frame}, true, tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	d, err := normalizeFrames([][]byte{frame2}, true, tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if c != d {
		t.Fatalf("different ids/counters should converge:\nc=%s\nd=%s", c, d)
	}
}

// --- 小工具 --------------------------------------------------------------------

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
