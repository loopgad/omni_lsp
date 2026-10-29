package plugin

import (
	"strings"
	"testing"
	"time"
)

// 复证圈 1 审计发现 4/8 的守卫。

// TestCall_StdinWriteIsBoundedWhenPluginStopsReading（发现 4）：子进程存活但
// 停止读 stdin 时，宿主→插件的写帧必须有时间界——Call 的 5s 上限只约束响应
// 等待（select 在写完成之后才启动），写阶段必须有独立的超时。
func TestCall_StdinWriteIsBoundedWhenPluginStopsReading(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "stop-reading")

	old := writeStuckTimeout
	writeStuckTimeout = 300 * time.Millisecond
	t.Cleanup(func() { writeStuckTimeout = old })

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapIndexQuery}
	p, err := launchTestBinary(t, m, NewGrant(CapIndexQuery), nil, trustAllow)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() }) // 解除可能仍卡在写上的内部 goroutine

	done := make(chan error, 1)
	go func() {
		_, err := p.Call("index/query", map[string]any{"q": strings.Repeat("x", 1<<20)}) // 1MiB：管道缓冲必然写满
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("停止读 stdin 的插件必须使 Call 以错误返回（而非挂死）")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("宿主→插件 stdin 写帧无界阻塞：Call 的超时 select 不含写阶段，挂死插件整体绕过 kill 界")
	}
}

// TestExit_WriteStuckFallsBackToKill（发现 4 的 kill 兜底腿）：Exit 的通知写
// 被卡死的写（writeMu 被永远阻塞的写持有）阻塞时，必须在写超时后落入 kill
// 路径——有界拆除覆盖全部退出路径，OnExit/RecordCrash 记账闭合。
func TestExit_WriteStuckFallsBackToKill(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "stop-reading")

	old := writeStuckTimeout
	writeStuckTimeout = 300 * time.Millisecond
	t.Cleanup(func() { writeStuckTimeout = old })
	oldGrace := exitGracePeriod
	exitGracePeriod = 1 * time.Second // kill 后收割等待的上限（注入短窗口）
	t.Cleanup(func() { exitGracePeriod = oldGrace })

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapIndexQuery}
	exits := make(chan error, 1)
	p, err := launchTestBinary(t, m, NewGrant(CapIndexQuery), func(err error) { exits <- err }, trustAllow)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	// 白盒推到 lcShutdown：放行 Exit 的不合规门（本测试只针对写路径）。
	p.mu.Lock()
	p.lifecycle = lcShutdown
	p.mu.Unlock()

	// 先把写路径卡死：一次必然阻塞的写（内部 goroutine 持 writeMu 永远写不完）。
	stuck := make(chan error, 1)
	go func() {
		_, err := p.Call("index/query", map[string]any{"q": strings.Repeat("x", 1<<20)})
		stuck <- err
	}()
	time.Sleep(2 * writeStuckTimeout) // 越过写超时：Call 已报错返回，但内部写 goroutine 仍持 writeMu 卡死

	exitDone := make(chan error, 1)
	go func() { exitDone <- p.Exit() }()
	select {
	case err := <-exitDone:
		if err == nil {
			t.Fatal("exit 通知写阻塞必须以错误返回（kill 兜底可观测）")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Exit 的通知写被卡死写阻塞后无界挂起：Notify 无界写 + grace timer 在 Notify 之后布防，kill 界被整体绕过")
	}
	select {
	case werr := <-exits:
		if werr == nil {
			t.Fatal("kill 兜底后 OnExit 必须以非 nil 错误触发（RecordCrash 记账闭合）")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kill 兜底未执行：进程未被终止、OnExit 未触发")
	}
}

// TestCall_MarshalFailureLeavesNoPendingEntry（发现 8）：Call 在 json.Marshal
// 失败时不得残留已注册的 pending 表项——写失败与超时两条错误路径都做了
// delete，唯独此路径遗漏；每次失败永久占用一个自增 id 与一个 entry。
func TestCall_MarshalFailureLeavesNoPendingEntry(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "stop-reading") // 模式无关：marshal 失败发生在任何写之前

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapIndexQuery}
	p, err := launchTestBinary(t, m, NewGrant(CapIndexQuery), nil, trustAllow)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	if _, err := p.Call("index/query", map[string]any{"ch": make(chan int)}); err == nil {
		t.Fatal("不可序列化 params 必须报错")
	}
	p.mu.Lock()
	leaked := len(p.pending)
	p.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("marshal 失败后 pending 残留 %d 项（每次失败永久占用一个自增 id 与一个 entry，直到 readLoop 终止才释放）", leaked)
	}
}
