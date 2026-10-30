package server

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/plugin"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// 审计批 3 发现 1 守卫（插件通知订阅窗口：grant 登记滞后于 sink）：开口 4 把
// sink 提前到 LaunchWithSink 时刻（readLoop/pump 启动前），但宿主授权表
// pluginGrants 仍按旧时序在 Launch 返回 + MarkRunning 之后登记——插件收到
// plugin/hello 后立即 emit 的通知经 pump 直达 onPluginNotify，在 grant 查表
// 门被拒（pluginEmitRejected）。server.go「注册窗口从协议时序上整体移除」的
// 注释与实际不符：窗口内通知到不了诊断车道，只是丢弃科目从 DroppedNotifies
// 换成 pluginEmitRejected。修复形态：grant 不再查表，而是随 sink 闭包与被
// Launch 的进程同源抵达（LaunchWithSink 的实参 grant 直接进 onPluginNotify
// 的能力门）——「已接 sink 但未登记」态在存活路径上不可达，窗口按构造闭合。

// TestEmit_NotifyGateAdmitsHandshakeEmit 是缝上守卫（红先于修复）：「sink 已
// 活、宿主授权登记未落」的到达时序下，emit 必须照常注入诊断协调器，而不是
// 在授权门被拒。旧实现的 grant 门查 pluginGrants 表，本测试在未播种任何登记
// 的 Server 上直调 onPluginNotify，把握手窗口钉成确定性失败（旧代码：拒绝计
// 数 +1、pluginItems 无注入 → 红）。
//
// 语义切割注记：修复后 grant 作显式实参随 sink 闭包抵达，「未登记」态不复
// 存在——本测试的调用形状随签名更换（grant 入参），旧的白盒查表播种法
// （plugin_emit_budget_test.go 的 pluginGrants 播种）随切割一并替换。
func TestEmit_NotifyGateAdmitsHandshakeEmit(t *testing.T) {
	s := New(DefaultConfig())
	attachRecordingTransport(t, s)
	uri := "file:///p/main.go"
	s.vfs.Open(uri, "go", 1, []byte("package main"), vfs.SourceEditor)

	params, err := json.Marshal(map[string]any{
		"uri": uri,
		"diagnostics": []map[string]any{{
			"startLine": 0, "startChar": 0, "endLine": 0, "endChar": 3,
			"severity": 1, "code": "E1", "message": "handshake-marker",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 关键：不向任何宿主侧登记表播种（修复后该表已不存在）——grant 是随 sink
	// 闭包抵达的显式实参，即被 Launch 进程所持的同一张授予表。修复前本测试
	// 以旧签名（无 grant 入参、查 pluginGrants 表）运行并确定性红：未登记的
	// emit 被拒（pluginEmitRejected=1、pluginItems 无注入）。红态输出存档：
	//   --- FAIL: TestEmit_NotifyGateAdmitsHandshakeEmit
	//       plugin_handshake_emit_test.go:53: 握手窗口内的 emit 在授权门被拒
	//       （pluginEmitRejected=1）——grant 登记滞后于 sink 的窗口仍在
	s.onPluginNotify("p1", plugin.NewGrant(plugin.CapDiagnosticsEmit), "diagnostics/emit", params)

	if n := s.pluginEmitRejected.Load(); n != 0 {
		t.Fatalf("握手窗口内的 emit 在授权门被拒（pluginEmitRejected=%d）——grant 登记滞后于 sink 的窗口仍在", n)
	}
	s.diag.mu.Lock()
	_, injected := s.diag.pluginItems[uri]
	s.diag.mu.Unlock()
	if !injected {
		t.Fatal("握手窗口内的 emit 未注入诊断协调器（握手期通知到不了诊断车道）")
	}
}

// TestEmit_HelloEmitReachesClient 是全链路守卫：真实子进程插件在收到
// plugin/hello 的当下立即 diagnostics/emit（helper 的 hello-emit 模式）——
// 通知在 LaunchWithSink 返回前后、宿主记账尚未完成的握手窗口内抵达宿主。
// 修复后该 emit 必须照常 admitted（能力门按随 sink 同源的 grant 判定），
// 经去抖上线为带宿主强制来源标记的 publishDiagnostics，且插件EmitRejected
// 为零。此测试是端到端回归绊线；确定性红态证明见
// TestEmit_NotifyGateAdmitsHandshakeEmit（缝上直调，不依赖进程竞速）。
func TestEmit_HelloEmitReachesClient(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "hello-emit")
	t.Setenv("GO_PLUGIN_EMIT_MESSAGE", "hello-marker")
	t.Setenv("OMNILSP_TRUST", "trusted")

	root := t.TempDir()
	writePluginDir(t, root, "hello-emit", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()
	rt := attachRecordingTransport(t, s)
	s.vfs.Open("file:///p/main.go", "go", 1, []byte("package main"), vfs.SourceEditor)

	// InitializeWorkspace → initIndex → launchReadyPlugins：宿主写 plugin/hello，
	// 插件收到即 emit（协议时序上先于 Launch 返回后的任何宿主记账）。
	s.InitializeWorkspace(t.TempDir())

	waitUntil(t, func() bool {
		for _, m := range rt.filterMethod("textDocument/publishDiagnostics") {
			if strings.Contains(string(m.Params), "hello-marker") {
				return true
			}
		}
		return false
	}, "握手期 emit 未上线（hello-emit 插件的通知丢失）")

	for _, m := range rt.filterMethod("textDocument/publishDiagnostics") {
		body := string(m.Params)
		if !strings.Contains(body, `"source":"plugin:hello-emit"`) {
			t.Errorf("握手期 emit 推送缺少宿主强制的插件来源标记: %s", body)
		}
	}
	// 等过一次去抖窗口再采样拒绝计数，排除「计数在途」的假绿。
	time.Sleep(diagDebounce + 100*time.Millisecond)
	if n := s.pluginEmitRejected.Load(); n != 0 {
		t.Fatalf("握手期 emit 被拒计数（pluginEmitRejected=%d）——订阅窗口仍在丢弃通知", n)
	}
}
