package server

import (
	"encoding/json"
	"fmt"
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
