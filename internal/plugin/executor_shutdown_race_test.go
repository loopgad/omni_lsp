package plugin

import (
	"errors"
	"testing"
	"time"
)

// 复证圈 1 审计发现 3 的守卫：Shutdown 握手失败后的回滚必须是条件迁移——
// 仅当仍处于 lcShutdown 时回滚 lcActive；并发 Exit 已置位的 lcExited 终态
// 不可回退（否则状态机与已发线字节脱钩：后续 Exit 误报 ErrNonCompliantExit、
// lcExited 的 Call 拒绝门重开）。
//
// 确定性交错构造：helper 模式 hold-shutdown-exit 对 plugin/shutdown 不应答
// （Call 挂起等待），主测试确认 Call 已注册后并发 Exit——plugin/exit 使子进程
// 退出，readLoop 的 EOF 唤醒挂起的 Call，Shutdown 的回滚必然发生在
// Exit 置位 lcExited 之后（回滚的触发链严格后于置位）。
func TestShutdown_RollbackNeverOverwritesExitedState(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "hold-shutdown-exit")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p, err := launchTestBinary(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- p.Shutdown() }()

	// 白盒等待：Shutdown 的 Call 已进入 pending 表（同包测试可见）。
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		registered := len(p.pending) == 1
		p.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shutdown Call 未在 5s 内注册进 pending 表")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 并发 Exit：置 lcExited 并送达 plugin/exit（helper 收到后干净退出）。
	if err := p.Exit(); err != nil {
		t.Fatalf("Exit: %v", err)
	}

	select {
	case err := <-shutdownDone:
		if err == nil {
			t.Fatal("插件已退出（EOF）的 shutdown 握手必须失败")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown 未在子进程退出后返回")
	}

	p.mu.Lock()
	lifecycle := p.lifecycle
	p.mu.Unlock()
	if lifecycle != lcExited {
		t.Fatalf("Shutdown 失败回滚覆写了并发 Exit 已置位的 lcExited，lifecycle=%d（want %d lcExited）——状态机与已发线字节脱钩", lifecycle, lcExited)
	}
	// 终态语义闭合：lcExited 之后一切 Call 被拒。
	if _, err := p.Call("probe/after-race", nil); !errors.Is(err, ErrExited) {
		t.Fatalf("err = %v, want ErrExited（lcExited 的 Call 拒绝门被回滚重开）", err)
	}
}
