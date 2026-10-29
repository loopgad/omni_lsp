package server

import (
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/plugin"
)

// 复证圈 1 审计发现 5/7 的守卫：
//   - 发现 5：ClosePlugins 之后的 launchReadyPlugins 不得再把插件推进到
//     Launch/注册——closing 置位后 pending 丢弃、已 Launch 的结果按「已关闭」
//     处理（kill + Disable），否则进程与管道泄漏到宿主退出。
//   - 发现 7：插件对 plugin/shutdown 挂起应答（存活不响应）时，closePlugins
//     不得只 continue 跳过 Exit——Shutdown 超时失败的存活进程必须补一次
//     有界 kill（32f5afd 为 Exit 腿修掉的同级故障在 Shutdown 腿重开）。

// TestClosePlugins_LaunchAfterCloseIsDiscarded：先关闭、后起飞的尝试必须被
// 丢弃（不 Launch、不注册、Disable 回 verified）。
func TestClosePlugins_LaunchAfterCloseIsDiscarded(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")
	t.Setenv("OMNILSP_TRUST", "trusted")

	root := t.TempDir()
	dir, m := writePluginDir(t, root, "late-launch", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	s.InitializeWorkspace(t.TempDir()) // 策略就绪（此时无 pending，零 Launch）
	t.Cleanup(func() {
		// 红/绿两相通用的兜底清理：若有存活子进程，走握手拆除。
		if proc := mountedProc(t, s, "late-launch"); proc != nil {
			if err := proc.Shutdown(); err == nil {
				_ = proc.Exit()
			}
		}
	})

	// 白盒注入一条「关闭后才就绪」的待命插件（模拟装配与关闭交错的产物）。
	s.pluginMu.Lock()
	s.pluginPending = append(s.pluginPending, pluginPendingLaunch{
		discovery: plugin.Discovery{Dir: dir, Manifest: m},
		grant:     plugin.NewGrant(plugin.CapDiagnosticsEmit),
	})
	s.pluginMu.Unlock()

	s.ClosePlugins()       // 先关闭
	s.launchReadyPlugins() // 关闭后再起飞：必须被丢弃

	if proc := mountedProc(t, s, "late-launch"); proc != nil {
		t.Fatal("ClosePlugins 之后 launchReadyPlugins 仍 Launch/注册了插件——该进程不会被任何路径 shutdown/exit（进程与管道泄漏到宿主退出）")
	}
	st, err := s.pluginMgr.State("late-launch")
	if err != nil || st != plugin.StateVerified {
		t.Fatalf("关闭后起飞的插件应被 Disable 回 verified, got %v (err %v)", st, err)
	}
}

// TestClosePlugins_ShutdownHungPluginIsKilled：对 shutdown 挂起应答的存活
// 插件，closePlugins 必须在有界时间内完成拆除（kill 兜底 → Wait → OnExit →
// pluginOnExit 从 pluginProcs 删除）。旧实现 Shutdown 失败即 continue——
// Exit（唯一带 kill 的路径）被跳过，进程与两根 stdio 管道泄漏为孤儿。
// 注：插件对 shutdown 无应答时 Call 需等满 callTimeout（5s），本测试整体
// 约 5-10s，属既有协议超时的一半（先 Shutdown 后可能 Exit 的拆除窗口）。
func TestClosePlugins_ShutdownHungPluginIsKilled(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "hold-shutdown")
	t.Setenv("OMNILSP_TRUST", "trusted")

	root := t.TempDir()
	writePluginDir(t, root, "hung-shutdown", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()
	s.InitializeWorkspace(t.TempDir())
	waitUntil(t, func() bool {
		return mountedProc(t, s, "hung-shutdown") != nil
	}, "hold-shutdown 插件未被装配起飞")

	s.ClosePlugins() // Shutdown 挂起应答 → 超时失败 → 必须 kill 兜底并收割
	waitUntil(t, func() bool {
		return mountedProc(t, s, "hung-shutdown") == nil
	}, "shutdown 挂起应答的存活插件未被有界拆除（closePlugins 该路径下进程永不终止、OnExit/RecordCrash 不闭合）")
}
