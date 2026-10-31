package plugin

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// 第 18 轮治理开口 5 守卫：插件→宿主通知缓冲区的 §F14 配额（与
// internal/languages 的 F14 后端缓冲接线同构——env 显式开关、默认关
// （X10：未配置即零行为变化）、fail-closed 拒绝可见计数不静默）。
//
// 预算面：OMNILSP_PLUGIN_NOTIFY_BUDGET_BYTES（正整数字节）。设置后，每个
// 插件进程驻留在通知缓冲通道内的字节总量受配额约束：超配额的通知帧在
// readLoop 入队前整帧拒绝（丢弃可见，BudgetDroppedNotifies 计数），绝不
// 部分采纳。未设置/非法值：配额关闭，行为与接线前逐字节一致。

// waitForBudgetDrop 以至多 2s 窗口轮询配额拒绝计数。
func waitForBudgetDrop(t *testing.T, p *Process, want uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.BudgetDroppedNotifies() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("BudgetDroppedNotifies = %d, want >= %d（超配额通知必须在入队前整帧拒绝并计数）",
		p.BudgetDroppedNotifies(), want)
}

// TestNotifyBudget_OffByDefaultDelivers 钉住默认关（X10 红线）：不设置
// OMNILSP_PLUGIN_NOTIFY_BUDGET_BYTES 时通知照常投递、零配额拒绝——与接线前
// 的行为逐字节一致。
func TestNotifyBudget_OffByDefaultDelivers(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "emit-notify")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}

	var mu sync.Mutex
	var delivered int
	p, err := launchTestBinaryWithSink(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow,
		func(string, json.RawMessage) {
			mu.Lock()
			delivered++
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("LaunchWithSink: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	if _, err := p.Call("probe/emit", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered == 1
	}, "默认关（未配置预算）下通知必须照常投递")
	if n := p.BudgetDroppedNotifies(); n != 0 {
		t.Errorf("BudgetDroppedNotifies = %d, want 0（默认关不得产生配额拒绝）", n)
	}
	if n := p.DroppedNotifies(); n != 0 {
		t.Errorf("DroppedNotifies = %d, want 0（配额关闭时不得误记通道丢弃）", n)
	}
}

// TestNotifyBudget_QuotaRefusesWhenExceeded 是 fail-closed 守卫：配额 1 字节
// 时任何带 params 的通知帧都无法入队——整帧拒绝并计入 BudgetDroppedNotifies，
// 订阅者不得收到该通知；连接与其余请求不受牵连（Call 照常返回）。
func TestNotifyBudget_QuotaRefusesWhenExceeded(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "emit-notify")
	t.Setenv("OMNILSP_PLUGIN_NOTIFY_BUDGET_BYTES", "1")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}

	var mu sync.Mutex
	var delivered int
	p, err := launchTestBinaryWithSink(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow,
		func(string, json.RawMessage) {
			mu.Lock()
			delivered++
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("LaunchWithSink: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	// Call 必须照常返回：配额只约束通知缓冲，不牵连响应多路复用。
	if _, err := p.Call("probe/emit", nil); err != nil {
		t.Fatalf("Call must survive quota refusal: %v", err)
	}

	waitForBudgetDrop(t, p, 1)
	time.Sleep(200 * time.Millisecond) // 越过投递竞速窗口后再断言未送达
	mu.Lock()
	n := delivered
	mu.Unlock()
	if n != 0 {
		t.Fatalf("超配额通知被投递了 %d 次，want 0（fail-closed 被破坏）", n)
	}
	if other := p.DroppedNotifies(); other != 0 {
		t.Errorf("DroppedNotifies = %d, want 0（配额拒绝必须记 BudgetDroppedNotifies，不得混入通道饱和计数）", other)
	}
}

// TestNotifyBudget_WithinQuotaDelivers 钉住配额内的正常路径不受影响：
// 预算远大于单帧时通知照常投递、零拒绝。
func TestNotifyBudget_WithinQuotaDelivers(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "emit-notify")
	t.Setenv("OMNILSP_PLUGIN_NOTIFY_BUDGET_BYTES", "1048576")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}

	var mu sync.Mutex
	var delivered int
	p, err := launchTestBinaryWithSink(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow,
		func(string, json.RawMessage) {
			mu.Lock()
			delivered++
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("LaunchWithSink: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	if _, err := p.Call("probe/emit", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered == 1
	}, "配额内的通知必须照常投递")
	if n := p.BudgetDroppedNotifies(); n != 0 {
		t.Errorf("BudgetDroppedNotifies = %d, want 0（配额内不得拒绝）", n)
	}
}

// TestNotifyBudget_InvalidValueKeepsOff 钉住非法值的 positiveIntFromEnv 约定：
// 非法配置不猜测（配额保持关闭），通知照常投递——与 F13/F14 两条车线的
// 非法值处理同构。
func TestNotifyBudget_InvalidValueKeepsOff(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "emit-notify")
	t.Setenv("OMNILSP_PLUGIN_NOTIFY_BUDGET_BYTES", "not-a-number")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}

	var mu sync.Mutex
	var delivered int
	p, err := launchTestBinaryWithSink(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow,
		func(string, json.RawMessage) {
			mu.Lock()
			delivered++
			mu.Unlock()
		})
	if err != nil {
		t.Fatalf("LaunchWithSink: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	if _, err := p.Call("probe/emit", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered == 1
	}, "非法预算值必须保持配额关闭（通知照常投递）")
	if n := p.BudgetDroppedNotifies(); n != 0 {
		t.Errorf("BudgetDroppedNotifies = %d, want 0（非法值保持关闭）", n)
	}
}
