package server

import (
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// 复证圈 1 审计发现 1/2 的守卫：插件 emit 车道必须与后端 compute 车道对齐
// 同一关闭契约——
//   - 文档关闭后迟到的 inject 被拒（不复填充、计入可见拒绝计数）；
//   - 去抖上线前复查文档仍打开（publishPluginDiagnostics 关闭态复查）；
//   - didCloseDocument 对 pluginOrder 做与 d.order 相同的原地过滤
//     （close/reopen 循环不得为同一 uri 累积重复槽位——FIFO 驱逐据 order
//     长度删 map，重复槽位会误删存活 uri 的插件诊断集）。
//
// 发现 1 的语义变更与旧钉住测试的切割：TestEmit_EndToEndWithDebounce 与
// TestEmit_CrashDuringEmitContained 原本对「从未 didOpen 的 uri」emit 并期望
// 推送——那是无守卫旧语义；两测试已改为先 vfs.Open（见 plugin_mount_test.go
// 的更新），与本守卫的关闭契约一致。

var auditPluginDiagItems = []languages.Diagnostic{{
	StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 3,
	Severity: 1, Code: "E1", Source: "plugin:p1", Message: "boom",
}}

// TestPluginEmit_LateInjectAfterCloseIsDropped：文档关闭后迟到的插件 emit
// 必须被拒——对齐 compute 的 errDiagnosticDocumentClosed 守卫
// （diagnostics.go:417/493/508），且拒绝计入可见计数。
func TestPluginEmit_LateInjectAfterCloseIsDropped(t *testing.T) {
	s := New(DefaultConfig())
	attachRecordingTransport(t, s)
	uri := "file:///p/main.go"
	s.vfs.Open(uri, "go", 1, []byte("package main"), vfs.SourceEditor)

	s.diag.injectPluginDiagnostics("p1", uri, auditPluginDiagItems) // 打开态：正常注入
	d := s.diag
	d.mu.Lock()
	_, injected := d.pluginItems[uri]
	d.mu.Unlock()
	if !injected {
		t.Fatal("打开态注入应进入插件诊断表（基线）")
	}

	s.diag.didCloseDocument(uri, func() { s.vfs.Close(uri) })

	s.diag.injectPluginDiagnostics("p1", uri, auditPluginDiagItems) // 迟到 inject
	d.mu.Lock()
	_, repopulated := d.pluginItems[uri]
	rejected := s.pluginEmitRejected.Load()
	d.mu.Unlock()
	if repopulated {
		t.Fatal("didCloseDocument 后的迟到插件 emit 复填充了插件诊断表（违反本车道关闭契约，对端 compute 有守卫而插件车道没有）")
	}
	if rejected == 0 {
		t.Fatal("关闭后的迟到 emit 未计入可见拒绝计数（丢弃不可见）")
	}
}

// TestPluginEmit_PublishAbandonedWhenDocumentClosed：去抖回调取出的插件集在
// 上线前必须于 d.mu 内复查文档仍打开——已关闭文档不得再收到插件诊断推送
// （对齐 publishComputed 的上线前双复查纪律）。
func TestPluginEmit_PublishAbandonedWhenDocumentClosed(t *testing.T) {
	s := New(DefaultConfig())
	rt := attachRecordingTransport(t, s)
	uri := "file:///p/main.go"
	s.vfs.Open(uri, "go", 1, []byte("package main"), vfs.SourceEditor)
	sets := map[string]pluginDiagSet{"p1": {items: auditPluginDiagItems}}

	s.diag.publishPluginDiagnostics(uri, sets) // 打开态：照常推送（基线）
	if n := len(rt.filterMethod("textDocument/publishDiagnostics")); n == 0 {
		t.Fatal("打开态 publishPluginDiagnostics 应推送（基线）")
	}
	rt.reset()

	s.diag.didCloseDocument(uri, func() { s.vfs.Close(uri) })
	s.diag.publishPluginDiagnostics(uri, sets) // 关闭态：必须放弃发布
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if pushes := rt.filterMethod("textDocument/publishDiagnostics"); len(pushes) > 0 {
			t.Fatalf("已关闭文档收到了插件诊断推送: %+v", pushes)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPluginOrder_FilteredOnDocumentClose：didCloseDocument 清 pluginItems/
// pluginTimers 的同时必须过滤 pluginOrder（同函数对后端 d.order 做了过滤）；
// injectPluginDiagnostics 的 seen 判定须与 order 一致——close/reopen 循环后
// 同一 uri 在 pluginOrder 的槽位数必须保持 ≤1。
func TestPluginOrder_FilteredOnDocumentClose(t *testing.T) {
	s := New(DefaultConfig())
	attachRecordingTransport(t, s)
	uri := "file:///p/main.go"
	d := s.diag
	for cycle := 0; cycle < 3; cycle++ {
		s.vfs.Open(uri, "go", 1, []byte("package main"), vfs.SourceEditor)
		s.diag.injectPluginDiagnostics("p1", uri, auditPluginDiagItems)
		s.diag.didCloseDocument(uri, func() { s.vfs.Close(uri) })
		d.mu.Lock()
		slots, tables := len(d.pluginOrder), len(d.pluginItems)
		d.mu.Unlock()
		if slots != 0 || tables != 0 {
			t.Fatalf("cycle %d: close 后 pluginOrder=%d pluginItems=%d——pluginOrder 未随关闭过滤（重复槽位累积，FIFO 驱逐会误删存活 uri 的插件诊断集）", cycle, slots, tables)
		}
	}
	// 关闭后再打开并注入：order 恰好一个槽位，与 pluginItems 键集一致。
	s.vfs.Open(uri, "go", 1, []byte("package main"), vfs.SourceEditor)
	s.diag.injectPluginDiagnostics("p1", uri, auditPluginDiagItems)
	d.mu.Lock()
	slots, tables := len(d.pluginOrder), len(d.pluginItems)
	d.mu.Unlock()
	if slots != 1 || tables != 1 {
		t.Fatalf("reopen 注入后 pluginOrder=%d pluginItems=%d，want 1/1（order 与键集一致）", slots, tables)
	}
}
