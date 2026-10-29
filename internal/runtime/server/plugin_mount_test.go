package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/plugin"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// §O0-O5 落差1+3 守卫与端到端测试（DEF-PLUGMOUNT 最小垂直切片）：
//   - 零值 PluginsDir = 零行为变化（X10 红线：默认关）；
//   - 信任链失败不 spawn（Verify → Install 拒绝）；
//   - 无 diagnostics.emit Grant 的 emit 被拒（default deny 实证）；
//   - emit 全链路：握手 → Call → 插件通知 → 订阅 → 能力门 → 协调器去抖
//     推送（带宿主强制来源标记，不绕过去抖）；
//   - emit 后崩溃：已注入诊断不回滚、RecordCrash 接线、崩溃循环达阈值隔离。

// attachRecordingTransport 在 Run 之外接上出站捕获（progress_test 同模式）。
func attachRecordingTransport(t *testing.T, s *Server) *recordingTransport {
	t.Helper()
	rt := &recordingTransport{}
	s.mu.Lock()
	s.transport = rt
	s.mu.Unlock()
	return rt
}

// waitUntil 以至多 4s 的窗口轮询 cond（推送路径含 150ms 去抖）。
func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

// mountedProc 返回装配启动的插件进程句柄。
func mountedProc(t *testing.T, s *Server, id string) *plugin.Process {
	t.Helper()
	s.pluginMu.Lock()
	defer s.pluginMu.Unlock()
	return s.pluginProcs[id]
}

// TestPluginsMount_ZeroValueIsNoOp 钉住 X10 红线的默认关语义：未配置
// PluginsDir 时装配层完全不激活——无 Manager、无子进程、无计数漂移，
// dispatcher 行为照旧。
func TestPluginsMount_ZeroValueIsNoOp(t *testing.T) {
	s := New(DefaultConfig())
	if s.pluginMgr != nil {
		t.Fatal("零值 PluginsDir 不得创建 plugin.Manager（默认关被破坏）")
	}
	if len(s.pluginProcs) != 0 {
		t.Fatalf("零值 PluginsDir 不得启动任何插件进程，got %d", len(s.pluginProcs))
	}
	if n := s.pluginNotifies.Load(); n != 0 {
		t.Errorf("pluginNotifies = %d, want 0", n)
	}
	rt := attachRecordingTransport(t, s)
	resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 1}, "omnilsp/status", json.RawMessage(`{}`)))
	if resp == nil || resp.Error != nil {
		t.Fatalf("omnilsp/status failed: %+v", resp)
	}
	if n := len(rt.filterMethod("textDocument/publishDiagnostics")); n != 0 {
		t.Errorf("无插件时不得有任何诊断推送，got %d", n)
	}
}

// TestPluginsMount_EmptyDirMountsNothing：目录存在但无插件 → 装配空转。
func TestPluginsMount_EmptyDirMountsNothing(t *testing.T) {
	s := New(Config{PluginsDir: t.TempDir()})
	if s.pluginMgr == nil {
		t.Fatal("显式 PluginsDir 应创建 Manager")
	}
	if len(s.pluginProcs) != 0 {
		t.Fatalf("空目录不得有插件进程，got %d", len(s.pluginProcs))
	}
	s.ClosePlugins() // 空表关闭必须无害
}

// TestPluginsMount_TrustChainFailureNeverSpawns：entrypoint 与声明 checksum
// 不符（磁盘调包）→ Verify 拒绝 → Install 拒绝 → 永不 spawn（N14 执行时刻
// 复核经装配路径依然生效）。
func TestPluginsMount_TrustChainFailureNeverSpawns(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")

	root := t.TempDir()
	writePluginDir(t, root, "tampered", []plugin.Capability{plugin.CapDiagnosticsEmit}, func(binPath string) {
		f, err := os.OpenFile(binPath, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatalf("open for tamper: %v", err)
		}
		if _, err := f.WriteString("tampered-bytes"); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		f.Close()
	})

	s := New(Config{PluginsDir: root})
	defer s.ClosePlugins()
	if len(s.pluginProcs) != 0 {
		t.Fatalf("信任链失败目录不得 spawn 插件进程，got %d", len(s.pluginProcs))
	}
	if _, err := s.pluginMgr.State("tampered"); err == nil {
		t.Fatal("未通过 Verify 的插件不得进入 Manager 注册表（N14）")
	}
	if n := s.pluginNotifies.Load(); n != 0 {
		t.Errorf("未运行插件不得产生通知，got %d", n)
	}
}

// TestEmit_DefaultDenyWithoutGrant：manifest 未声明 diagnostics.emit 的插件
// 敌意 emit（helper 不关心授予什么）——宿主 Grant.Check 必须拒绝：
// 无推送、拒绝计数 +1（default deny 实证点，server.go onPluginNotify）。
func TestEmit_DefaultDenyWithoutGrant(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")

	root := t.TempDir()
	writePluginDir(t, root, "no-grant", []plugin.Capability{plugin.CapDocumentRead}, nil)

	s := New(Config{PluginsDir: root})
	defer s.ClosePlugins()
	rt := attachRecordingTransport(t, s)

	proc := mountedProc(t, s, "no-grant")
	if proc == nil {
		t.Fatal("document.read 插件应正常装配运行")
	}
	// 插件能力声明缺 diagnostics.emit，Launch 前置门放行（声明⊆授予），
	// 但宿主 emit 门必须拒绝这条敌意通知。
	if _, err := proc.Call("emit/now", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// 等待拒绝计数落定（onPluginNotify 的 notifies++ 与 rejected++ 之间存在
	// 指令窗口，不能在第一个计数上采样）。
	waitUntil(t, func() bool { return s.pluginEmitRejected.Load() == 1 }, "无 Grant 的 emit 未被拒绝并计数")
	time.Sleep(300 * time.Millisecond) // 越过 diagDebounce 窗口
	if pushes := rt.filterMethod("textDocument/publishDiagnostics"); len(pushes) != 0 {
		t.Fatalf("default deny 下不得推送插件诊断: %+v", pushes)
	}
}

// TestEmit_EndToEndWithDebounce 全链路：真实子进程插件握手 → 宿主 Call →
// 插件 diagnostics/emit 通知 → readLoop 订阅投递 → Grant 门 → 协调器
// 去抖（emit 后立即无推送）→ publishDiagnostics 推送，diagnostics[].source
// 为宿主强制的 "plugin:<id>"。
func TestEmit_EndToEndWithDebounce(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")
	t.Setenv("GO_PLUGIN_EMIT_MESSAGE", "e2e-marker")

	root := t.TempDir()
	writePluginDir(t, root, "diag-e2e", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	s := New(Config{PluginsDir: root})
	defer s.ClosePlugins()
	rt := attachRecordingTransport(t, s)

	proc := mountedProc(t, s, "diag-e2e")
	if proc == nil {
		t.Fatal("diagnostics.emit 插件应正常装配运行")
	}
	if st, err := s.pluginMgr.State("diag-e2e"); err != nil || st != plugin.StateRunning {
		t.Fatalf("装配后状态 = %v (err %v), want running", st, err)
	}

	if _, err := proc.Call("emit/now", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if n := s.pluginEmitRejected.Load(); n != 0 {
		t.Fatalf("有 Grant 的 emit 不得被拒，pluginEmitRejected = %d", n)
	}
	// 去抖实证：emit 已注入但推送必须等待 diagDebounce——直连 notifyClient
	// 的绕行实现会在这里立即出帧。
	time.Sleep(50 * time.Millisecond)
	if pushes := rt.filterMethod("textDocument/publishDiagnostics"); len(pushes) != 0 {
		t.Fatalf("推送绕过了去抖（150ms 内出帧）: %+v", pushes)
	}

	waitUntil(t, func() bool {
		return len(rt.filterMethod("textDocument/publishDiagnostics")) > 0
	}, "去抖窗口后 publishDiagnostics 未到达客户端")

	for _, m := range rt.filterMethod("textDocument/publishDiagnostics") {
		body := string(m.Params)
		if !strings.Contains(body, "e2e-marker") {
			t.Errorf("推送缺少插件消息: %s", body)
		}
		if !strings.Contains(body, `"source":"plugin:diag-e2e"`) {
			t.Errorf("推送缺少宿主强制的插件来源标记: %s", body)
		}
		if !strings.Contains(body, "file:///p/main.go") {
			t.Errorf("推送 uri 缺失: %s", body)
		}
	}
}

// TestEmit_CrashDuringEmitContained：插件 emit 后立即崩溃——(1) 已注入
// 宿主的诊断照常推送（协调器与进程生命解耦）；(2) OnExit → RecordCrash
// 接线生效（State → Failed）；(3) 连续崩溃达 MaxConsecutivePluginCrashes
// 后 Enable 拒绝（ErrQuarantined，崩溃循环隔离）。
func TestEmit_CrashDuringEmitContained(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit-die")

	root := t.TempDir()
	writePluginDir(t, root, "crasher", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	s := New(Config{PluginsDir: root})
	defer s.ClosePlugins()
	rt := attachRecordingTransport(t, s)

	proc := mountedProc(t, s, "crasher")
	if proc == nil {
		t.Fatal("插件应正常装配运行")
	}

	exited := make(chan error, 1)
	go func() {
		_, err := proc.Call("emit/now", nil)
		exited <- err
	}()
	select {
	case <-exited: // err 可为 nil（pong 竞速胜出）或 EOF 错误；唤醒即合格
	case <-time.After(5 * time.Second):
		t.Fatal("崩溃插件的 Call 未被唤醒（EOF 路径失效）")
	}

	waitUntil(t, func() bool {
		st, err := s.pluginMgr.State("crasher")
		return err == nil && st == plugin.StateFailed
	}, "进程崩溃未触发 RecordCrash（State 未迁移到 failed）")

	waitUntil(t, func() bool {
		for _, msg := range rt.filterMethod("textDocument/publishDiagnostics") {
			if strings.Contains(string(msg.Params), "boom") {
				return true
			}
		}
		return false
	}, "emit 后崩溃：已注入的诊断必须照常推送（不回滚）")

	// 崩溃循环隔离（§O4）：崩溃计数经 Enable 重启会归零（Manager 既有钉住
	// 语义「全新启动，连续计数归零」），而装配最小切片无自动重启——宿主侧
	// 的崩溃循环即同一插件连续多次异常退出事件进入 pluginOnExit 接线。
	// 此处沿接线（而非直调 mgr.RecordCrash）补足至阈值，断言隔离生效。
	mgr := s.pluginMgr
	for i := 1; i <= plugin.MaxConsecutivePluginCrashes-1; i++ {
		s.pluginOnExit("crasher", errors.New("exit status 7"))
	}
	if _, err := mgr.Enable("crasher"); !errors.Is(err, plugin.ErrQuarantined) {
		t.Fatalf("崩溃循环达阈值后 Enable err = %v, want ErrQuarantined", err)
	}
	st, err := mgr.State("crasher")
	if err != nil || st != plugin.StateQuarantined {
		t.Fatalf("state = %v (err %v), want quarantined", st, err)
	}
}
