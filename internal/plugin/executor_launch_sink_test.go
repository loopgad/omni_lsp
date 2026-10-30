package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 第 18 轮治理开口 4 守卫：Launch 返回与 SetNotifySink 注册之间存在订阅
// 窗口——窗口内到达的插件通知按有界有损语义丢弃并计入 DroppedNotifies。
// 修复形态是「SetNotifySink 前移」：LaunchWithSink 在 readLoop/pump 启动前
// 注册订阅回调，注册窗口从协议时序上整体移除；SetNotifySink 仍用于事后
// 替换/复位（无订阅者丢弃的既有语义由 executor_notify_test.go 钉住不动）。

// launchTestBinaryWithSink 是 launchTestBinary 的 Launch 时刻订阅变体：
// entrypoint/checksum 按测试二进制实算，sink 经 LaunchWithSink 在宿主
// readLoop/pump 启动前注册。
func launchTestBinaryWithSink(t *testing.T, m Manifest, g Grant, onExit func(error), trust func() error, sink func(string, json.RawMessage)) (*Process, error) {
	t.Helper()
	m.Entrypoint = filepath.Base(os.Args[0])
	raw, err := os.ReadFile(os.Args[0])
	if err != nil {
		return nil, fmt.Errorf("读取测试二进制: %w", err)
	}
	sum := sha256.Sum256(raw)
	m.Checksum = "sha256:" + hex.EncodeToString(sum[:])
	return LaunchWithSink(context.Background(), filepath.Dir(os.Args[0]), m, g, onExit, trust, sink)
}

// TestNotify_ImmediateEmitWithoutSinkDrops 先钉住旧契约的现实（修复前后都
// 必须成立）：无订阅者时，注册窗口内到达的通知被丢弃且计数——丢弃可见
// 不静默。这是开口 4 的病灶实证，不是修复断言。
func TestNotify_ImmediateEmitWithoutSinkDrops(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "emit-immediate")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p, err := launchTestBinary(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	// 不注册 sink，让通知落在无订阅者窗口里。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && p.DroppedNotifies() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if n := p.DroppedNotifies(); n != 1 {
		t.Fatalf("DroppedNotifies = %d, want 1（无订阅者的注册窗口丢弃必须可见）", n)
	}
}

// TestNotify_SinkAtLaunchClosesRegistrationWindow 是开口 4 的修复守卫：
// LaunchWithSink 在 readLoop/pump 启动前注册订阅回调——emit-immediate 的
// 通知帧在 Launch 返回前就已写入管道，仍必须送达订阅者且零丢弃。修复前
// 本测试无法编译（LaunchWithSink 不存在），红阶段以编译失败登记。
func TestNotify_SinkAtLaunchClosesRegistrationWindow(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "emit-immediate")

	type got struct {
		method string
		params json.RawMessage
	}
	var mu sync.Mutex
	var events []got

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p, err := launchTestBinaryWithSink(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow,
		func(method string, params json.RawMessage) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, got{method: method, params: params})
		})
	if err != nil {
		t.Fatalf("LaunchWithSink: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(events) == 1
	}, "Launch 时刻注册的订阅回调未收到握手期通知")

	mu.Lock()
	defer mu.Unlock()
	if events[0].method != "diagnostics/emit" {
		t.Errorf("method = %q, want %q", events[0].method, "diagnostics/emit")
	}
	var params struct {
		URI string `json:"uri"`
	}
	if err := json.Unmarshal(events[0].params, &params); err != nil {
		t.Fatalf("params unparsable: %v (%s)", err, events[0].params)
	}
	if params.URI != "file:///p/immediate.go" {
		t.Errorf("uri = %q, want file:///p/immediate.go", params.URI)
	}
	if n := p.DroppedNotifies(); n != 0 {
		t.Errorf("DroppedNotifies = %d, want 0（Launch 时刻订阅不得丢弃注册窗口通知）", n)
	}
}
