package plugin

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// §O0-O5 落差2 守卫测试：插件→宿主通知帧（无 id、带 method）不再是 readLoop
// 的静默丢弃物，而是经有界通道送达宿主订阅回调（SetNotifySink）；慢消费者
// 与无订阅者都按有界有损语义丢弃并计入 DroppedNotifies——丢弃可见不静默，
// 且绝不拖死唯一的响应读取循环。

// waitFor 以至多 2s 的间隔轮询 cond，超时即失败（通知投递是异步的）。
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestNotify_SinkReceivesPluginNotification 钉住订阅回调投递：插件发出的
// diagnostics/emit 通知帧必须到达 SetNotifySink 注册的宿主回调，method 与
// params 原样送达；Call 与通知共用一条 stdout 而互不干扰（响应照常路由）。
func TestNotify_SinkReceivesPluginNotification(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "emit-notify")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p := spawnHelper(t, "emit-notify", m, NewGrant(CapDiagnosticsEmit))

	type got struct {
		method string
		params json.RawMessage
	}
	var mu sync.Mutex
	var events []got
	p.SetNotifySink(func(method string, params json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, got{method: method, params: params})
	})

	res, err := p.Call("probe/emit", nil)
	if err != nil {
		t.Fatalf("Call after notify frame: %v", err)
	}
	if res == nil {
		t.Fatal("expected pong result, got nil")
	}

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) == 1
	}, "订阅回调从未收到插件通知")

	mu.Lock()
	defer mu.Unlock()
	ev := events[0]
	if ev.method != "diagnostics/emit" {
		t.Errorf("method = %q, want %q", ev.method, "diagnostics/emit")
	}
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(ev.params, &params); err != nil {
		t.Fatalf("params unparsable: %v (%s)", err, ev.params)
	}
	if params.URI != "file:///p/main.go" {
		t.Errorf("uri = %q, want file:///p/main.go", params.URI)
	}
	if n := p.DroppedNotifies(); n != 0 {
		t.Errorf("DroppedNotifies = %d, want 0（正常投递不得计数丢弃）", n)
	}
}

// TestNotify_NoSinkDropsAndCounts 钉住无订阅者的默认态：通知被丢弃且计数
// 登记——丢弃可见，绝不静默（这不是旧行为的静默 continue）。
func TestNotify_NoSinkDropsAndCounts(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "emit-notify")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p := spawnHelper(t, "emit-notify", m, NewGrant(CapDiagnosticsEmit))

	if _, err := p.Call("probe/emit", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	waitFor(t, func() bool { return p.DroppedNotifies() == 1 },
		"无订阅者时通知应被丢弃且 DroppedNotifies 计到 1")
}

// TestNotify_BoundedDropSlowConsumer 钉住有界投递的核心承诺：sink 阻塞
// （慢消费者）时——(1) readLoop 不被拖死，响应照常路由（Call 返回）；
// (2) 溢出通知被丢弃并计数；(3) 丢弃之后连接继续可用。
func TestNotify_BoundedDropSlowConsumer(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "notify-flood")
	t.Setenv("GO_PLUGIN_FLOOD", "200")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p := spawnHelper(t, "notify-flood", m, NewGrant(CapDiagnosticsEmit))

	gate := make(chan struct{})
	var received int
	var mu sync.Mutex
	p.SetNotifySink(func(method string, params json.RawMessage) {
		mu.Lock()
		received++
		mu.Unlock()
		<-gate // 慢消费者：第一条之后 pump 整体阻塞
	})

	done := make(chan error, 1)
	go func() {
		_, err := p.Call("probe/flood", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Call must survive notify flood: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("readLoop 被慢消费者拖死：Call 未返回")
	}

	mu.Lock()
	first := received
	mu.Unlock()
	dropped := p.DroppedNotifies()
	if dropped == 0 {
		t.Error("通道饱和后应有丢弃计数，got 0")
	}
	if first > pluginNotifyBuffer+1 {
		t.Errorf("阻塞的 sink 不应收到超过缓冲容量的通知：got %d", first)
	}
	if dropped >= 200 {
		t.Errorf("丢弃 %d 条意味着响应帧也被当通知丢了，投递逻辑失真", dropped)
	}

	// 放行慢消费者：pump 恢复，缓冲余量送达，连接继续可用。
	close(gate)
	if _, err := p.Call("probe/after-flood", nil); err != nil {
		t.Fatalf("second Call after drain: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if received > pluginNotifyBuffer+1 {
		t.Errorf("总计送达 %d 条超过有界上界 %d", received, pluginNotifyBuffer+1)
	}
}
