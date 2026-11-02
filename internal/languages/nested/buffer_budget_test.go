package nested

// F14 backend-buffer budget guards for the nested stdio bridge
// (DEF-BUDWIRE, F14 half). The budget is latched from the environment by
// internal/languages; every test here pins its own environment and resets
// the latch so the default-off invariant stays observable per test.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// resetBudgetForTest pins budget variables for this test and resets the
// process-wide latch both before and after.
func resetBudgetForTest(t *testing.T, pairs map[string]string) {
	t.Helper()
	for name, value := range pairs {
		t.Setenv(name, value)
	}
	languages.ResetBackendBudgetForTest()
	t.Cleanup(languages.ResetBackendBudgetForTest)
}

// newBudgetTestConn builds a conn with one open document whose state is
// reachable directly, without spawning a real worker. readerEpoch stays at
// its initial value, so receiveDiagnostics(epoch) matches.
func newBudgetTestConn(t *testing.T, name string) (*Conn, *documentState, string) {
	t.Helper()
	conn := New(Config{Name: name, Lang: "test", WorkDir: t.TempDir(), RequestTimeout: time.Second})
	const uri = "file:///workspace/main.go"
	conn.mu.Lock()
	state := &documentState{
		langID:           "test",
		open:             true,
		epoch:            conn.readerEpoch.Load(),
		version:          1,
		diagnosticsReady: make(chan struct{}),
	}
	conn.storeDocumentLocked(uri, state)
	conn.mu.Unlock()
	t.Cleanup(func() { _ = conn.Close() })
	return conn, state, uri
}

func diagnosticsPayload(items int) json.RawMessage {
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < items; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"range":{"start":{"line":%d,"character":0},"end":{"line":%d,"character":5}},"message":"diagnostic message %d","source":"test"}`, i, i, i)
	}
	b.WriteString("]")
	return json.RawMessage(b.String())
}

func publishRaw(uri string, version int32, diagnostics json.RawMessage) json.RawMessage {
	raw, err := json.Marshal(publishDiagnosticsParams{URI: uri, Version: &version, Diagnostics: diagnostics})
	if err != nil {
		panic(err)
	}
	return raw
}

func TestDiagnosticsBudgetRefusalDropsPublicationAndCounts(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES": "16",
	})

	conn, state, uri := newBudgetTestConn(t, "fake-lsp")

	// Refusal counters are process-global and persist across -count
	// iterations; this test pins ITS OWN contribution (exactly one counted
	// refusal), following the delta convention of
	// TestDiagnosticsUnconfiguredBehaviorUnchanged below.
	refusalsBefore := languages.BackendBufferRefusals("fake-lsp")

	// A publication whose wire staging exceeds the 16-byte budget is
	// refused: the previously stored diagnostics (none) stay, the refusal is
	// counted, nothing panics.
	conn.receiveDiagnostics(conn.readerEpoch.Load(), publishRaw(uri, 1, diagnosticsPayload(3)))

	if len(state.diagnostics) != 0 {
		t.Fatalf("over-quota publication was staged anyway: %d diagnostics", len(state.diagnostics))
	}
	if state.diagnosticsReceived {
		t.Fatal("over-quota publication must not mark diagnostics received")
	}
	if got := languages.BackendBufferRefusals("fake-lsp"); got != refusalsBefore+1 {
		t.Fatalf("refusals(fake-lsp) delta = %d, want exactly 1 (fail-closed must be countable, not silent)", got-refusalsBefore)
	}
}

func TestDiagnosticsBudgetReplacementSwapsAccounting(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES": "1048576",
	})

	conn, state, uri := newBudgetTestConn(t, "fake-lsp")
	first := diagnosticsPayload(3)
	conn.receiveDiagnostics(conn.readerEpoch.Load(), publishRaw(uri, 1, first))
	if len(state.diagnostics) == 0 {
		t.Fatal("in-budget publication must be stored")
	}
	if state.budgetReserved != int64(len(first)) {
		t.Fatalf("budgetReserved = %d, want the staged wire size %d", state.budgetReserved, len(first))
	}
	manager := languages.BackendBudget()
	if manager == nil {
		t.Fatal("budget configured but manager stayed nil")
	}
	total1, _ := manager.Usage()
	if total1 != int64(len(first)) {
		t.Fatalf("global usage after first publication = %d, want %d", total1, len(first))
	}

	second := diagnosticsPayload(8)
	conn.receiveDiagnostics(conn.readerEpoch.Load(), publishRaw(uri, 1, second))
	if len(state.diagnostics) != 8 {
		t.Fatalf("replacement publication stored %d diagnostics, want 8", len(state.diagnostics))
	}
	if state.budgetReserved != int64(len(second)) {
		t.Fatalf("budgetReserved after replacement = %d, want %d", state.budgetReserved, len(second))
	}
	total2, _ := manager.Usage()
	if total2 != int64(len(second)) {
		t.Fatalf("global usage after replacement = %d, want %d (old reservation must be released, not accumulated)", total2, len(second))
	}
}

func TestDiagnosticsBudgetCloseDocumentReleases(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES": "1048576",
	})

	conn, state, uri := newBudgetTestConn(t, "fake-lsp")
	conn.receiveDiagnostics(conn.readerEpoch.Load(), publishRaw(uri, 1, diagnosticsPayload(3)))
	manager := languages.BackendBudget()
	if manager == nil {
		t.Fatal("budget configured but manager stayed nil")
	}
	if total, _ := manager.Usage(); total == 0 {
		t.Fatal("publication must hold a reservation before close")
	}

	// CloseDocument's trailing didClose write fails on this pipe-less conn
	// (never started); the §F14 release happens before that write, which is
	// what this test pins.
	_ = conn.CloseDocument(uri, 1)
	if state.budgetReserved != 0 {
		t.Fatalf("budgetReserved after close = %d, want 0", state.budgetReserved)
	}
	if total, _ := manager.Usage(); total != 0 {
		t.Fatalf("global usage after close = %d, want 0 (closing a document must release its staging bytes)", total)
	}
}

func TestDiagnosticsUnconfiguredBehaviorUnchanged(t *testing.T) {
	// Refusal counters are process-global and earlier tests may have
	// incremented this owner's; the unconfigured invariant is that THIS
	// publication adds none.
	refusalsBefore := languages.BackendBufferRefusals("fake-lsp")
	resetBudgetForTest(t, nil)

	conn, state, uri := newBudgetTestConn(t, "fake-lsp")
	if languages.BackendBudget() != nil {
		t.Fatal("no budget variables configured; the latch must stay nil (X10)")
	}
	conn.receiveDiagnostics(conn.readerEpoch.Load(), publishRaw(uri, 1, diagnosticsPayload(3)))
	if len(state.diagnostics) != 3 {
		t.Fatalf("unconfigured path stored %d diagnostics, want 3 (default behavior unchanged)", len(state.diagnostics))
	}
	if got := languages.BackendBufferRefusals("fake-lsp"); got != refusalsBefore {
		t.Fatalf("unconfigured path recorded %d new refusals, want 0", got-refusalsBefore)
	}
}

// TestReadLoopFrameCapSkipsOversizedFrameKeepsStreamAlive 替换了旧守卫
// TestReadLoopFrameCapRefusesOversizedFrames（其断言 spawns >= 2，钉住的正是
// 审计批 2 发现 1 的缺陷行为：超限帧杀死整条读循环 → 重启 → 子进程重发同一
// 帧 → 再死，crashes 在 MarkReady 清零下 0↔1 摆动，隔离永不触发，且该文件
// 诊断永不送达）。新语义（审计批 2 发现 1 修复，fixSketch (b)）：配额收紧的
// 帧上限拒绝的是单帧分配，不是整条流——超限帧按 Content-Length 排空丢弃、
// 计数，读循环继续；后续合法帧照常处理。旧语义钉住测试随本替换整体切割，
// 无并存。
func TestReadLoopFrameCapSkipsOversizedFrameKeepsStreamAlive(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_QUOTA_FAKE_LSP_BYTES": "4096",
	})

	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), Sup: fastSup(), CaptureDiagnostics: true})
	const uri = "file:///workspace/main.go"
	conn.mu.Lock()
	state := &documentState{
		langID:           "test",
		open:             true,
		epoch:            conn.readerEpoch.Load(),
		version:          1,
		diagnosticsReady: make(chan struct{}),
	}
	conn.storeDocumentLocked(uri, state)
	conn.mu.Unlock()

	var spawns atomic.Int32
	// io.Pipe returns (reader, writer): the test writes frames into the
	// writer, the conn's readLoop consumes the reader.
	childReader, childWriter := io.Pipe()
	conn.cfg.Start = func(c *Conn) error {
		spawns.Add(1)
		c.Attach(nil, nopWriteCloser{}, childReader)
		c.MarkReady()
		return nil
	}
	if err := conn.StartSupervised(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Attach 每次把 readerEpoch +1 并把预存文档整体作废（open=false、ready
	// 通道关闭，invalidateDocumentsForRestartLocked）——生产语义里文档在
	// attach 之后经 didOpen 打开。此处按 attach 后状态重开文档，否则
	// receiveDiagnostics 按 open/epoch 失配丢弃本次发布。
	conn.mu.Lock()
	state.epoch = conn.readerEpoch.Load()
	state.open = true
	state.diagnosticsClosed = false
	state.diagnosticsReady = make(chan struct{})
	conn.mu.Unlock()

	// 一个 8192 字节帧（配额 4096）后跟一个合法 publishDiagnostics 帧，单次
	// 写入保序：超限帧必须被排空丢弃（计数），随后的合法帧必须照常入账——
	// 读循环断链或后续帧丢失任一发生都算回归。合法帧 body 是完整 JSON-RPC
	// 通知（带 method），publishRaw 只是其 params。
	oversized := fmt.Sprintf("Content-Length: 8192\r\n\r\n%s", strings.Repeat("x", 8192))
	valid := publishRaw(uri, 1, diagnosticsPayload(1))
	notification, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/publishDiagnostics",
		"params":  json.RawMessage(valid),
	})
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	validFrame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(notification), string(notification))
	if _, err := io.WriteString(childWriter, oversized+validFrame); err != nil {
		t.Fatalf("write frames: %v", err)
	}

	// 合法帧的诊断必须送达（流仍可用）；有界窗口等待。诊断切片由
	// receiveDiagnostics 在 c.mu 内整体替换（diagnostics.go:843），此处与
	// readLoop goroutine 并发，读取必须持同一把锁，否则构成数据竞争
	//（race 门禁实测捕获：本测试裸读 vs receiveDiagnostics 写）。
	diagnosticsLen := func() int {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		return len(state.diagnostics)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if diagnosticsLen() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := diagnosticsLen(); got != 1 {
		t.Fatalf("frame after the oversized one was not processed: %d diagnostics, want 1 (stream must stay usable)", got)
	}
	if got := languages.BackendBufferRefusals("fake-lsp"); got < 1 {
		t.Fatalf("refusals(fake-lsp) = %d, want >= 1: the skipped frame must be a counted refusal (visible, not silent)", got)
	}
	// 读循环不得因超限帧断链：无第二次 spawn（缺陷行为是退出+重启）。
	if time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond) // 给（不该发生的）重启留出显形窗口
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("spawns = %d, want 1: a skipped oversized frame must not retire the read loop (the old behavior respawned forever)", got)
	}
	_ = childWriter.Close()
	_ = conn.Close()
}

func TestReadLoopFrameCapUnconfiguredKeepsDefault(t *testing.T) {
	resetBudgetForTest(t, nil)

	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	codec := conn.newReadCodec()
	if codec.MaxMessageSize != jsonrpc.DefaultMaxMessageSize {
		t.Fatalf("unconfigured codec cap = %d, want the codec default %d (§C1 cap unchanged)", codec.MaxMessageSize, jsonrpc.DefaultMaxMessageSize)
	}
	// The configured side: a below-default owner quota tightens the cap.
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_QUOTA_FAKE_LSP_BYTES": "256",
	})
	conn2 := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	if got := conn2.newReadCodec().MaxMessageSize; got != 256 {
		t.Fatalf("configured codec cap = %d, want the owner quota 256", got)
	}
}

// TestReadLoopDefaultCapRefusalCountedUnderGlobalBudget 钉住默认 §C1 帧帽
// 拒绝路径的计数（round20/bugC）。配了全局预算但 owner 无配额（或配额 ≥
// 默认帽）时 BackendBufferCap 返回 0，SkipOversizedFrames 不启用，超限帧走
// codec 默认帽的 fatal 拒绝——该拒绝必须被计数，且计数不得依赖 codec 错误
// 文案的稳定：修复前 readLoop 用 strings.HasPrefix(err.Error(), "jsonrpc:
// message too large") 判定，协议包自己的文案 pin（contains 断言）会在文案
// 改动的同一次提交里被机械更新，而本路径在 nested 侧零守卫——计数静默归
// 零、全程无测试失败。本测试即是缺失的守卫。
func TestReadLoopDefaultCapRefusalCountedUnderGlobalBudget(t *testing.T) {
	resetBudgetForTest(t, map[string]string{
		"OMNILSP_BACKEND_BUDGET_BYTES": "1048576", // 仅全局预算：owner 无配额
	})

	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), RequestTimeout: time.Second})
	codec := conn.newReadCodec()
	if codec.MaxMessageSize != jsonrpc.DefaultMaxMessageSize || codec.SkipOversizedFrames {
		t.Fatalf("a global budget alone must leave the §C1 default cap and fatal posture: cap=%d skip=%v",
			codec.MaxMessageSize, codec.SkipOversizedFrames)
	}

	// 拒绝计数是进程全局的，跨 -count 迭代累积；按既有约定钉增量。
	refusalsBefore := languages.BackendBufferRefusals("fake-lsp")

	// io.Pipe：只写帧头。默认帽在分配 body 之前就拒绝，body 无需存在。
	childReader, childWriter := io.Pipe()
	conn.Attach(nil, nopWriteCloser{}, childReader)
	oversized := fmt.Sprintf("Content-Length: %d\r\n\r\n", jsonrpc.DefaultMaxMessageSize+1)
	if _, err := io.WriteString(childWriter, oversized); err != nil {
		t.Fatalf("write oversized header: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && languages.BackendBufferRefusals("fake-lsp") == refusalsBefore {
		time.Sleep(5 * time.Millisecond)
	}
	if got := languages.BackendBufferRefusals("fake-lsp"); got != refusalsBefore+1 {
		t.Fatalf("refusals(fake-lsp) delta = %d, want 1: the §C1 default-cap refusal must be countable without string-matching the codec message", got-refusalsBefore)
	}
	_ = childWriter.Close()
	_ = conn.Close()
}
