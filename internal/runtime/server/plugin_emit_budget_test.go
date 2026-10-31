package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/plugin"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// 复证圈 2 审计发现 3 的守卫：插件 diagnostics/emit 的内容面必须有 per-emit
// 条数预算。旧实现只按帧解析（单帧由 maxPluginFrameSize=4MiB 兜底），每
// uri×每插件的诊断集整集驻留 pluginItems，FIFO 只限 uri 数（512）不限每
// uri 内容，无预算接车。超限 emit 整帧拒绝并计入 pluginEmitRejected。
func TestPluginEmit_OversizedFrameIsRejectedAndCounted(t *testing.T) {
	s := New(DefaultConfig())
	attachRecordingTransport(t, s)
	uri := "file:///p/main.go"
	s.vfs.Open(uri, "go", 1, []byte("package main"), vfs.SourceEditor)
	// 白盒直授 grant：让 emit 越过 Grant 门（default deny），使条数门成为
	// 本测试唯一的拒绝来源。（审计批 3 发现 1：grant 改随 sink 闭包作显式
	// 实参抵达 onPluginNotify，旧的白盒 pluginGrants 播种随查表门一并切割。）
	grant := plugin.NewGrant(plugin.CapDiagnosticsEmit)

	// 组一帧超过条数预算的 emit（1024 上限 + 1）。
	diags := make([]map[string]any, 0, maxPluginEmitDiagnostics+1)
	for i := 0; i < maxPluginEmitDiagnostics+1; i++ {
		diags = append(diags, map[string]any{
			"startLine": i, "startChar": 0, "endLine": i, "endChar": 1,
			"severity": 1, "code": fmt.Sprintf("E%d", i), "message": "boom",
		})
	}
	params, err := json.Marshal(map[string]any{"uri": uri, "diagnostics": diags})
	if err != nil {
		t.Fatal(err)
	}

	s.onPluginNotify("p1", grant, "diagnostics/emit", params)

	if n := s.pluginEmitRejected.Load(); n != 1 {
		t.Fatalf("超预算 emit 未被拒绝计数，pluginEmitRejected=%d（旧实现无条数预算，全集驻留）", n)
	}
	s.diag.mu.Lock()
	_, injected := s.diag.pluginItems[uri]
	s.diag.mu.Unlock()
	if injected {
		t.Fatal("超预算 emit 的诊断集不得进入插件诊断表")
	}

	// 预算内的 emit 照常注入（语义不回退）。
	okDiags := diags[:8]
	params, err = json.Marshal(map[string]any{"uri": uri, "diagnostics": okDiags})
	if err != nil {
		t.Fatal(err)
	}
	s.onPluginNotify("p1", grant, "diagnostics/emit", params)
	s.diag.mu.Lock()
	_, injected = s.diag.pluginItems[uri]
	s.diag.mu.Unlock()
	if !injected {
		t.Fatal("预算内的 emit 必须照常注入")
	}
}

// TestPluginEmit_OverByteBudgetResidentSetRejectedAndCounted（复证圈 1 sweep
// 发现 1）：条数上限（maxPluginEmitDiagnostics=1024）不约束单条体积——旧实现
// 每 (uri, plugin) 驻留一整帧解析后 emit 集，驻留总量 = 打开文档数 × 插件数 ×
// ~4MiB 级结构（diagCacheLimit=512 的 FIFO 只按 uri 计数），GiB 量级可达且
// 无任何字节记账。修复后：注入前按 projectDiagnostics 序列化长度设帧级字节
// 预算（maxPluginEmitFrameBytes），超限整帧拒绝并计入 pluginEmitRejected。
// 本测试的 emit 条数恰在条数预算内（=1024），仅字节超限——钉住字节面独立
// 于条数面生效。
func TestPluginEmit_OverByteBudgetResidentSetRejectedAndCounted(t *testing.T) {
	s := New(DefaultConfig())
	attachRecordingTransport(t, s)
	uri := "file:///p/big.go"
	s.vfs.Open(uri, "go", 1, []byte("package main"), vfs.SourceEditor)
	grant := plugin.NewGrant(plugin.CapDiagnosticsEmit)

	// 1024 条 × ~5KiB message：序列化驻留 ≈ 5.4MiB > maxPluginEmitFrameBytes
	// （4MiB），条数 = maxPluginEmitDiagnostics 不超条数预算。
	const msgLen = 5 << 10
	big := strings.Repeat("m", msgLen)
	diags := make([]map[string]any, 0, maxPluginEmitDiagnostics)
	for i := 0; i < maxPluginEmitDiagnostics; i++ {
		diags = append(diags, map[string]any{
			"startLine": i, "startChar": 0, "endLine": i, "endChar": 1,
			"severity": 1, "code": fmt.Sprintf("E%d", i), "message": big,
		})
	}
	params, err := json.Marshal(map[string]any{"uri": uri, "diagnostics": diags})
	if err != nil {
		t.Fatal(err)
	}

	s.onPluginNotify("p1", grant, "diagnostics/emit", params)

	if n := s.pluginEmitRejected.Load(); n != 1 {
		t.Fatalf("超字节预算的 emit 未被拒绝计数，pluginEmitRejected=%d（旧实现无字节记账，整集驻留）", n)
	}
	s.diag.mu.Lock()
	_, injected := s.diag.pluginItems[uri]
	s.diag.mu.Unlock()
	if injected {
		t.Fatal("超字节预算 emit 的诊断集不得进入插件诊断表")
	}

	// 字节预算内的 emit 照常注入（语义不回退）。
	params, err = json.Marshal(map[string]any{"uri": uri, "diagnostics": diags[:8]})
	if err != nil {
		t.Fatal(err)
	}
	s.onPluginNotify("p1", grant, "diagnostics/emit", params)
	s.diag.mu.Lock()
	_, injected = s.diag.pluginItems[uri]
	s.diag.mu.Unlock()
	if !injected {
		t.Fatal("字节预算内的 emit 必须照常注入")
	}
}

// TestPluginEmitFrameBytesMirrorsPluginFrameCap（复证圈 3 sweep 发现 5）：
// maxPluginEmitFrameBytes 注释里的「与 maxPluginFrameSize 同锚」此前只是
// 跨包注释，无任何测试或编译期手段钉住——未来调大插件单帧上限时 emit 驻
// 留预算静默留在 4MiB，协议新接受的合法 emit 帧被默认开启的字节门整帧拒绝
// 而无报警。比照 TestMaxCodecDefaultFrameCapMirror 先例把「同锚」升格为被
// 执法的跨包不变量（plugin 侧导出 MaxPluginFrameSize）。
func TestPluginEmitFrameBytesMirrorsPluginFrameCap(t *testing.T) {
	if int64(maxPluginEmitFrameBytes) != int64(plugin.MaxPluginFrameSize) {
		t.Fatalf("maxPluginEmitFrameBytes = %d, want plugin.MaxPluginFrameSize %d（跨包同锚漂移：emit 驻留预算与插件通道单帧上限不同步）",
			int64(maxPluginEmitFrameBytes), int64(plugin.MaxPluginFrameSize))
	}
}
