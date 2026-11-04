package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/plugin"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/vfs"
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
	s.pluginMu.Lock()
	derr := s.pluginDiscoverErr
	s.pluginMu.Unlock()
	if derr != "" {
		t.Fatalf("可用目录不得记录发现失败，got %q", derr)
	}
	s.ClosePlugins() // 空表关闭必须无害
}

// TestPluginsMount_UnreadableDirIsObservable（审计批 2）：PluginsDir 指向
// 不存在的目录时，装配点不得把它伪装成「无插件」——目录级失败必须留下
// 可观测状态（pluginDiscoverErr 记录原因，对齐 disableIndex 的 idxReason
// 约定；stderr 一条由装配点落盘，此处断言状态半边）。
func TestPluginsMount_UnreadableDirIsObservable(t *testing.T) {
	s := New(Config{PluginsDir: filepath.Join(t.TempDir(), "does-not-exist")})
	defer s.ClosePlugins()
	if s.pluginMgr == nil {
		t.Fatal("显式 PluginsDir 应创建 Manager")
	}
	s.pluginMu.Lock()
	derr := s.pluginDiscoverErr
	s.pluginMu.Unlock()
	if derr == "" {
		t.Fatal("不可读 PluginsDir 必须记录目录级失败原因（pluginDiscoverErr 为空）")
	}
	if len(s.pluginProcs) != 0 {
		t.Fatalf("发现失败的装配不得有插件进程，got %d", len(s.pluginProcs))
	}
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

// TestPluginsMount_LaunchDeferredUntilTrustPolicy（审计批 2 守卫）：装配期
// （New，信任策略尚未建立）绝不 Launch——「nil 即放行」窗口已移除，插件停留
// 在 enabled 待命；工作区信任策略就绪（initialize → initIndex）后才起飞，
// 默认本地启动 = trusted → 放行。
func TestPluginsMount_LaunchDeferredUntilTrustPolicy(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")
	t.Setenv("OMNILSP_TRUST", "trusted")

	root := t.TempDir()
	writePluginDir(t, root, "deferred", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()

	if proc := mountedProc(t, s, "deferred"); proc != nil {
		t.Fatal("信任策略建立前不得 Launch（装配期放行窗口未移除）")
	}
	if st, err := s.pluginMgr.State("deferred"); err != nil || st != plugin.StateEnabled {
		t.Fatalf("装配后应停留 enabled 待命, got %v (err %v)", st, err)
	}
	if n := s.pluginNotifies.Load(); n != 0 {
		t.Errorf("待命插件不得产生通知，got %d", n)
	}

	// 策略就绪（initialize 路径）→ 待命插件起飞 → running。
	s.InitializeWorkspace(t.TempDir())
	waitUntil(t, func() bool {
		return mountedProc(t, s, "deferred") != nil
	}, "信任策略就绪后插件未被 Launch")
	if st, err := s.pluginMgr.State("deferred"); err != nil || st != plugin.StateRunning {
		t.Fatalf("策略就绪后状态 = %v (err %v), want running", st, err)
	}
}

// TestPluginsMount_UntrustedWorkspaceRefusesLoad（审计批 2 守卫）：OMNILSP_TRUST
// 显式收紧为 untrusted 时，plugin.load 在 Launch 必经前置被拒——插件不得起飞、
// 不持有 Enabled 授权（Disable 回 verified）。旧的「装配期 nil 即放行 + 后续
// Enable 补足」约定不再存在。
func TestPluginsMount_UntrustedWorkspaceRefusesLoad(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")
	t.Setenv("OMNILSP_TRUST", "untrusted")

	root := t.TempDir()
	writePluginDir(t, root, "untrusted-plugin", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()

	s.InitializeWorkspace(t.TempDir()) // 策略就绪：untrusted → Gate(CapPluginLoad) 拒
	waitUntil(t, func() bool {
		st, err := s.pluginMgr.State("untrusted-plugin")
		return err == nil && st == plugin.StateVerified
	}, "untrusted 工作区的插件未被信任门拒绝（应 Disable 回 verified）")
	if proc := mountedProc(t, s, "untrusted-plugin"); proc != nil {
		t.Fatal("untrusted 工作区不得有插件进程")
	}
	if n := s.pluginNotifies.Load(); n != 0 {
		t.Errorf("被拒插件不得产生通知，got %d", n)
	}
}

// TestEmit_DefaultDenyWithoutGrant：manifest 未声明 diagnostics.emit 的插件
// 敌意 emit（helper 不关心授予什么）——宿主 Grant.Check 必须拒绝：
// 无推送、拒绝计数 +1（default deny 实证点，server.go onPluginNotify）。
func TestEmit_DefaultDenyWithoutGrant(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")
	t.Setenv("OMNILSP_TRUST", "trusted")

	root := t.TempDir()
	writePluginDir(t, root, "no-grant", []plugin.Capability{plugin.CapDocumentRead}, nil)

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()
	s.InitializeWorkspace(t.TempDir()) // 审计批 2：策略就绪后才 Launch
	rt := attachRecordingTransport(t, s)

	// Round-23 OPT-3: launch 是后台化的——等待起飞完成再断言。
	waitUntil(t, func() bool { return mountedProc(t, s, "no-grant") != nil }, "document.read 插件未被 Launch")
	proc := mountedProc(t, s, "no-grant")
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
	t.Setenv("OMNILSP_TRUST", "trusted")

	root := t.TempDir()
	writePluginDir(t, root, "diag-e2e", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()
	s.InitializeWorkspace(t.TempDir()) // 审计批 2：策略就绪后才 Launch
	rt := attachRecordingTransport(t, s)

	// 复证圈 1 发现 1 的语义切割：插件 emit 车道与 compute 车道对齐关闭契约
	// 后，emit 的目标文档必须处于打开态——旧的「对从未 didOpen 的 uri emit
	// 并期望推送」是无守卫时代的钉法，随守卫落地一并替换。
	s.vfs.Open("file:///p/main.go", "go", 1, []byte("package main"), vfs.SourceEditor)

	// Round-23 OPT-3: launch 是后台化的——等待起飞完成再断言。
	waitUntil(t, func() bool { return mountedProc(t, s, "diag-e2e") != nil }, "diagnostics.emit 插件未被 Launch")
	proc := mountedProc(t, s, "diag-e2e")
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
	t.Setenv("OMNILSP_TRUST", "trusted")

	root := t.TempDir()
	writePluginDir(t, root, "crasher", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()
	s.InitializeWorkspace(t.TempDir()) // 审计批 2：策略就绪后才 Launch
	rt := attachRecordingTransport(t, s)

	// 复证圈 1 发现 1 的语义切割：emit 目标文档须为打开态（见
	// TestEmit_EndToEndWithDebounce 同款注记）。
	s.vfs.Open("file:///p/main.go", "go", 1, []byte("package main"), vfs.SourceEditor)

	// Round-23 OPT-3: launch 是后台化的——等待起飞完成再断言。
	waitUntil(t, func() bool { return mountedProc(t, s, "crasher") != nil }, "crasher 插件未被 Launch")
	proc := mountedProc(t, s, "crasher")

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

// TestPluginsMount_HighPrivilegeCapsRequireOptIn（审计批·注册变更越权覆盖面）：
// 装配点对三项高权能力（process.execute/filesystem.write/network.access）执行
// 第二道授予门——授予策略（DefaultGrantPolicy）默认剔除，仅 OMNILSP_PLUGIN_CAPS_ALLOW
// 显式列名者放行；有效声明经同一策略过滤后进入 pending，使 Launch 前置门
// 「声明 ⊆ 授予」按构造成立，低权能力不受影响。旧的「Enable 即按声明授予」
// 使「进入 PluginsDir 且过 checksum」等同自授全部高权能力，随本守卫切割。
func TestPluginsMount_HighPrivilegeCapsRequireOptIn(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")
	t.Setenv("OMNILSP_TRUST", "trusted")
	allCaps := []plugin.Capability{plugin.CapDiagnosticsEmit, plugin.CapProcessExecute, plugin.CapFilesystemWrite, plugin.CapNetworkAccess}

	// 装配快照：pending 首条（信任策略就绪前不 Launch，pending 稳定）。
	snapshot := func(s *Server) (plugin.Grant, []plugin.Capability) {
		s.pluginMu.Lock()
		defer s.pluginMu.Unlock()
		if len(s.pluginPending) != 1 {
			return plugin.Grant{}, nil
		}
		return s.pluginPending[0].grant, s.pluginPending[0].discovery.Manifest.Capabilities
	}
	grantHas := func(t *testing.T, g plugin.Grant, c plugin.Capability, want bool) {
		t.Helper()
		err := g.Check(c)
		if want && err != nil {
			t.Errorf("授予表缺 %s: %v", c, err)
		}
		if !want && err == nil {
			t.Errorf("授予表不应含 %s（第二道门未生效）", c)
		}
	}

	t.Run("default-withholds-high-privilege", func(t *testing.T) {
		root := t.TempDir()
		writePluginDir(t, root, "highpriv", allCaps, nil)
		s := New(Config{PluginsDir: root})
		defer s.ClosePlugins()
		grant, eff := snapshot(s)
		if eff == nil {
			t.Fatal("高权声明插件应正常装配待命（低权车道不受第二道门影响）")
		}
		grantHas(t, grant, plugin.CapDiagnosticsEmit, true)
		grantHas(t, grant, plugin.CapProcessExecute, false)
		grantHas(t, grant, plugin.CapFilesystemWrite, false)
		grantHas(t, grant, plugin.CapNetworkAccess, false)
		// 有效声明与授予表同源：Launch 前置门（声明 ⊆ 授予）不被第二道门破坏。
		if len(eff) != 1 || eff[0] != plugin.CapDiagnosticsEmit {
			t.Fatalf("有效声明 = %v, want [diagnostics.emit]", eff)
		}
	})

	t.Run("explicit-opt-in-restores-named-cap", func(t *testing.T) {
		t.Setenv("OMNILSP_PLUGIN_CAPS_ALLOW", "process.execute")
		root := t.TempDir()
		writePluginDir(t, root, "highpriv", allCaps, nil)
		s := New(Config{PluginsDir: root})
		defer s.ClosePlugins()
		grant, eff := snapshot(s)
		if eff == nil {
			t.Fatal("opt-in 后插件应正常装配待命")
		}
		grantHas(t, grant, plugin.CapProcessExecute, true)
		grantHas(t, grant, plugin.CapFilesystemWrite, false)
		grantHas(t, grant, plugin.CapNetworkAccess, false)
		grantHas(t, grant, plugin.CapDiagnosticsEmit, true)
		if len(eff) != 2 || eff[0] != plugin.CapDiagnosticsEmit || eff[1] != plugin.CapProcessExecute {
			t.Fatalf("有效声明 = %v, want [diagnostics.emit process.execute]", eff)
		}
	})
}

// TestEmit_LanguageScopeEnforced（审计批·合并策略来源伪造面）：manifest.languages
// （§O3 必填）此前在 emit 路径零执行——声明 languages:[go] 的插件可对任意
// 打开文档 emit 诊断。守卫：对可判定语言的越界 emit（[go] 插件对 python 文档）
// 与能力门同待遇——拒绝并计入 pluginEmitRejected，无推送；同声明语言的文档
// emit 照常放行（正控，证明门不误伤）。
func TestEmit_LanguageScopeEnforced(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")
	t.Setenv("OMNILSP_TRUST", "trusted")

	t.Run("out-of-scope-rejected", func(t *testing.T) {
		t.Setenv("GO_PLUGIN_EMIT_URI", "file:///p/main.py")
		root := t.TempDir()
		writePluginDir(t, root, "langscope", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)
		s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
		defer s.ClosePlugins()
		s.InitializeWorkspace(t.TempDir())
		rt := attachRecordingTransport(t, s)
		s.vfs.Open("file:///p/main.py", "python", 1, []byte("print(1)"), vfs.SourceEditor)

		// Round-23 OPT-3: launch 是后台化的——等待起飞完成再断言。
		waitUntil(t, func() bool { return mountedProc(t, s, "langscope") != nil }, "languages:[go] 插件未被 Launch")
		proc := mountedProc(t, s, "langscope")
		if _, err := proc.Call("emit/now", nil); err != nil {
			t.Fatalf("Call: %v", err)
		}
		waitUntil(t, func() bool { return s.pluginEmitRejected.Load() >= 1 },
			"[go] 插件对 python 文档的越界 emit 未被拒绝计数")
		time.Sleep(300 * time.Millisecond) // 越过 diagDebounce 窗口
		if pushes := rt.filterMethod("textDocument/publishDiagnostics"); len(pushes) != 0 {
			t.Fatalf("越界 emit 不得推送诊断: %+v", pushes)
		}
	})

	t.Run("in-scope-accepted", func(t *testing.T) {
		root := t.TempDir()
		writePluginDir(t, root, "langscope", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)
		s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
		defer s.ClosePlugins()
		s.InitializeWorkspace(t.TempDir())
		rt := attachRecordingTransport(t, s)
		s.vfs.Open("file:///p/main.go", "go", 1, []byte("package main"), vfs.SourceEditor)

		// Round-23 OPT-3: launch 是后台化的——等待起飞完成再断言。
		waitUntil(t, func() bool { return mountedProc(t, s, "langscope") != nil }, "languages:[go] 插件未被 Launch")
		proc := mountedProc(t, s, "langscope")
		if _, err := proc.Call("emit/now", nil); err != nil {
			t.Fatalf("Call: %v", err)
		}
		time.Sleep(300 * time.Millisecond) // 越过 diagDebounce 窗口
		if n := s.pluginEmitRejected.Load(); n != 0 {
			t.Fatalf("声明语言内的 emit 不得被拒，pluginEmitRejected = %d", n)
		}
		waitUntil(t, func() bool {
			return len(rt.filterMethod("textDocument/publishDiagnostics")) > 0
		}, "声明语言内的 emit 未推送（正控失败）")
	})
}
