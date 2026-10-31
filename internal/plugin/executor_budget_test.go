package plugin

import (
	"encoding/json"
	"io"
	"os"
	"strings"
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

// captureStderr 在闭包执行期间重定向进程 stderr 并返回其输出（顺序测试内
// 使用；并发写者不在捕获窗口内）。
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	if err := w.Close(); err != nil {
		t.Fatalf("关闭捕获写端: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读取捕获输出: %v", err)
	}
	_ = r.Close()
	return string(out)
}

// TestNotifyBudget_SubFrameQuotaAnnouncesOnStderr（复证圈 1 sweep 发现 6）：
// Reserve 的拒绝条件是 used+n>quota，配额 Q < MaxPluginFrameSize(4MiB) 时，
// 负载落在 (Q, 4MiB] 的协议合法通知帧在缓冲完全空闲时也被永久拒绝（每插件
// 进程一个 Manager，终进程生命周期）——而该拒绝只落入 BudgetDroppedNotifies
// 计数，生产侧无任何读取点。与 languages 车道 BackendBufferCap 把配额映射为
// 协议层帧上限收紧不同（插件 readFrame 无 skip-drain 机制，帧上限拒绝在
// readLoop 是连接级 fatal，会连坐多路复用响应——见 executor_budget.go 的取
// 舍登记），本车道选择把「大帧将按配额拒绝」向 stderr 显式声明（与
// ParsePositiveBytes 的非法值宣告纪律同构）：配置了会静默拒大帧的配额，操
// 作者必须被告知。对照：配额 ≥ 单帧上限时不宣告。
func TestNotifyBudget_SubFrameQuotaAnnouncesOnStderr(t *testing.T) {
	t.Setenv(pluginNotifyBudgetEnv, "1024") // < MaxPluginFrameSize（4MiB）

	out := captureStderr(t, func() {
		if m := notifyBudgetFromEnv(); m == nil {
			t.Fatal("合法正值必须构造 Manager")
		}
	})
	if !strings.Contains(out, pluginNotifyBudgetEnv) || !strings.Contains(out, "低于") {
		t.Fatalf("配额 %d < 单帧上限时必须向 stderr 声明大帧拒绝语义，got %q（旧实现静默）",
			1024, out)
	}

	// 对照：配额 ≥ 单帧上限 → 通知帧不会被配额拒，无需宣告。
	t.Setenv(pluginNotifyBudgetEnv, "16777216") // 16MiB > 4MiB
	out = captureStderr(t, func() {
		if m := notifyBudgetFromEnv(); m == nil {
			t.Fatal("合法正值必须构造 Manager")
		}
	})
	if strings.Contains(out, "低于") {
		t.Fatalf("配额 ≥ 单帧上限时不得宣告，got %q", out)
	}
}

// TestNotifyBudget_FloodSlowSinkEnqueueFailureReleases（复证圈 1 sweep 发现
// 5）：钉住 readLoop 的「Reserve 成功但入队失败（通道饱和）即时 Release」
// 路径（executor.go readLoop 的 select default 分支）——这是 readLoop 与
// pumpNotifies 两个 goroutine 对同一 Manager 并发 Release 不相交事件的唯一
// 交错点，此前在整个测试套件下零覆盖（notify-flood 不设预算、设预算的测试
// 全部单通知 emit-notify）。
//
// 确定性构造：配额 ≥ 全部 200 帧驻留字节（零配额拒绝），sink 在第一条后
// 阻塞 pump——通道（64 槽）必然饱和，其后 200-64=136 帧只能走 default 分支。
// 断言记账不变量：三科目守恒（送达 + 通道丢弃 + 配额拒绝 = 发送总数）且
// 稳定后 Manager 全局用量回零——default 分支的即时 Release 若缺失，136 帧
// 的驻留字节将永久滞留 used/total，Usage() 不回零（守卫即变异敏感）。
func TestNotifyBudget_FloodSlowSinkEnqueueFailureReleases(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "notify-flood")
	t.Setenv("GO_PLUGIN_FLOOD", "200")
	// 5MiB > 单帧上限（不触发 sub-frame stderr 宣告）且 ≫ 200 帧 × ~27 字节
	// ≈ 5.4KiB：配额全程不绑定，default 分支是唯一的饱和出路。
	t.Setenv("OMNILSP_PLUGIN_NOTIFY_BUDGET_BYTES", "5242880")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}

	gate := make(chan struct{})
	var mu sync.Mutex
	var delivered int
	p, err := launchTestBinaryWithSink(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow,
		func(string, json.RawMessage) {
			mu.Lock()
			delivered++
			mu.Unlock()
			<-gate // 慢消费者：第一条之后 pump 整体阻塞，通道必然饱和
		})
	if err != nil {
		t.Fatalf("LaunchWithSink: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	// readLoop 不被饱和/丢弃路径拖死：pong 照常路由（帧序上 pong 在全部
	// 通知帧之后——Call 返回即 200 帧的 readLoop 记账已定）。
	if _, err := p.Call("probe/flood", nil); err != nil {
		t.Fatalf("Call must survive notify flood under budget: %v", err)
	}

	// 饱和期四科目守恒：pump 被闸门阻塞后不再消费——Call 返回时每帧必然
	// 已落入「已送达 / 通道丢弃 / 配额拒绝 / 通道内驻留（len(notifyCh)）」
	// 之一（readLoop 已读完 pong，其后不再有帧）。readLoop 终态下该等式
	// 竞速无关。
	mu.Lock()
	got := delivered
	mu.Unlock()
	if got+int(p.DroppedNotifies())+int(p.BudgetDroppedNotifies())+len(p.notifyCh) != 200 {
		t.Fatalf("饱和期守恒破坏：delivered=%d dropped=%d budgetDropped=%d channel=%d，want 合计 200",
			got, p.DroppedNotifies(), p.BudgetDroppedNotifies(), len(p.notifyCh))
	}
	if n := p.BudgetDroppedNotifies(); n != 0 {
		t.Fatalf("配额全程不绑定，不得有配额拒绝：BudgetDroppedNotifies=%d", n)
	}
	if p.DroppedNotifies() == 0 {
		t.Fatal("通道饱和后必须有 default 分支丢弃——本测试的目标路径未被执行")
	}

	// 放行慢消费者：pump 排空通道余量（每帧收货 Release），连接继续可用。
	// 注：pump 取走第 1 帧阻塞在 sink 期间 readLoop 仍可回填通道，入队总量
	// 在 64~65 之间摆动——精确科目配比不钉，钉守恒与回零。
	close(gate)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered+int(p.DroppedNotifies())+int(p.BudgetDroppedNotifies()) == 200
	}, "闸门放行后全部 200 帧必须收敛到三个科目（通道排空）")
	mu.Lock()
	defer mu.Unlock()
	if delivered+int(p.DroppedNotifies())+int(p.BudgetDroppedNotifies()) != 200 {
		t.Fatalf("排空后守恒破坏：delivered=%d dropped=%d budgetDropped=%d，want 合计 200",
			delivered, p.DroppedNotifies(), p.BudgetDroppedNotifies())
	}
	// 记账闭环主断言：每笔 Reserve 恰好一次 Release（default 分支即时或
	// pump 收货）——全局用量必须回零。default 分支 Release 缺失即此处红。
	if total, _ := p.notifyBudget.Usage(); total != 0 {
		t.Fatalf("稳定后 manager.Usage() = %d，want 0（default 分支或 pump 的 Release 记账泄漏）", total)
	}
}

// TestNotifyBudget_FloodRefusalAccountingConserves（复证圈 1 sweep 发现 5 的
// 拒绝面）：配额设为 64 帧驻留总量的一半（~832 字节 ≈ 30 帧记账），sink 在
// 第一条后阻塞 pump——配额（≈30 帧）先于通道容量（64 槽）绑定，readLoop
// 的其余帧确定性落入 Reserve 拒绝分支（TestNotifyBudget_QuotaRefusesWhen
// Exceeded 的单通知构造升级为 flood 构造）。断言：拒绝确定性发生、四科目
// 守恒（送达 + 通道丢弃 + 配额拒绝 + 通道驻留 = 发送总数）、放行后守恒且
// 稳定后 Usage() 回零（每笔成功 Reserve 恰好一次 Release——pump 侧合账）。
func TestNotifyBudget_FloodRefusalAccountingConserves(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "notify-flood")
	t.Setenv("GO_PLUGIN_FLOOD", "200")
	t.Setenv("OMNILSP_PLUGIN_NOTIFY_BUDGET_BYTES", "832")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}

	gate := make(chan struct{})
	var mu sync.Mutex
	var delivered int
	p, err := launchTestBinaryWithSink(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow,
		func(string, json.RawMessage) {
			mu.Lock()
			delivered++
			mu.Unlock()
			<-gate
		})
	if err != nil {
		t.Fatalf("LaunchWithSink: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	if _, err := p.Call("probe/flood", nil); err != nil {
		t.Fatalf("Call must survive notify flood under budget: %v", err)
	}

	// 配额（~832B ≈ 30 帧）先于通道容量（64 槽）绑定：拒绝必须确定性发生。
	if n := p.BudgetDroppedNotifies(); n == 0 {
		t.Fatalf("配额 < 64 帧驻留总量时 flood 必须触发整帧拒绝，BudgetDroppedNotifies=0")
	}
	mu.Lock()
	got := delivered
	mu.Unlock()
	if got+int(p.DroppedNotifies())+int(p.BudgetDroppedNotifies())+len(p.notifyCh) != 200 {
		t.Fatalf("饱和期守恒破坏：delivered=%d dropped=%d budgetDropped=%d channel=%d，want 合计 200",
			got, p.DroppedNotifies(), p.BudgetDroppedNotifies(), len(p.notifyCh))
	}

	// 放行：pump 排空通道内余量（配额内的帧全部送达），连接继续可用。
	close(gate)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return delivered+int(p.DroppedNotifies())+int(p.BudgetDroppedNotifies()) == 200
	}, "闸门放行后全部 200 帧必须收敛到三个科目（通道排空）")
	mu.Lock()
	defer mu.Unlock()
	if delivered+int(p.DroppedNotifies())+int(p.BudgetDroppedNotifies()) != 200 {
		t.Fatalf("排空后守恒破坏：delivered=%d dropped=%d budgetDropped=%d，want 合计 200",
			delivered, p.DroppedNotifies(), p.BudgetDroppedNotifies())
	}
	t.Logf("科目配比：delivered=%d dropped=%d budgetDropped=%d",
		delivered, p.DroppedNotifies(), p.BudgetDroppedNotifies())
	if total, _ := p.notifyBudget.Usage(); total != 0 {
		t.Fatalf("稳定后 manager.Usage() = %d，want 0（Refuse/pump/default 三路径的记账闭环泄漏）", total)
	}
}
