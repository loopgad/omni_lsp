package plugin

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestShutdown_ResponsesStillServed pins the middle leg of the shutdown
// three-state semantics (LSP §C2: shutdown stops new semantic work while the
// process keeps serving): after a successful plugin/shutdown round-trip and
// before exit, Call must still be served — the host must not refuse requests
// in the lcShutdown state.
func TestShutdown_ResponsesStillServed(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "graceful-shutdown")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p := spawnHelper(t, "graceful-shutdown", m, NewGrant(CapDiagnosticsEmit))

	if err := p.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// shutdown 已应答、exit 未发生：插件仍须响应。
	res, err := p.Call("probe/alive", map[string]any{"uri": "file:///alive.go"})
	if err != nil {
		t.Fatalf("shutdown 后 Call 被拒绝: %v", err)
	}
	var got struct {
		Pong struct {
			URI string `json:"uri"`
		} `json:"pong"`
	}
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatalf("result unparsable: %v (%s)", err, res)
	}
	if got.Pong.URI != "file:///alive.go" {
		t.Fatalf("params mangled after shutdown: %+v", got.Pong)
	}
	// 重复 shutdown 必须被拒。
	if err := p.Shutdown(); !errors.Is(err, ErrAlreadyShutdown) {
		t.Fatalf("second Shutdown err = %v, want ErrAlreadyShutdown", err)
	}
}

// TestExit_BeforeShutdownIsNonCompliant pins the ordering rule: Exit() without
// a prior successful Shutdown() is a non-compliant exit (LSP: exit must follow
// shutdown). The host refuses the transition and — proven by the child-side
// audit — writes nothing to the pipe: the child saw neither plugin/shutdown
// nor plugin/exit.
func TestExit_BeforeShutdownIsNonCompliant(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "graceful-shutdown")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p := spawnHelper(t, "graceful-shutdown", m, NewGrant(CapDiagnosticsEmit))

	if err := p.Exit(); !errors.Is(err, ErrNonCompliantExit) {
		t.Fatalf("err = %v, want ErrNonCompliantExit", err)
	}

	// 宿主拒绝时零发送：插件侧审计确认两个停机帧都没到达。
	audit, err := p.Call("audit/lifecycle", nil)
	if err != nil {
		t.Fatalf("audit Call: %v", err)
	}
	var st struct {
		SawShutdown bool `json:"sawShutdown"`
		SawExit     bool `json:"sawExit"`
	}
	if err := json.Unmarshal(audit, &st); err != nil {
		t.Fatalf("audit unparsable: %v (%s)", err, audit)
	}
	if st.SawShutdown || st.SawExit {
		t.Fatalf("不合规 Exit 泄漏了停机帧: sawShutdown=%v sawExit=%v", st.SawShutdown, st.SawExit)
	}
}

// TestExit_HungPluginIsKilled pins the bounded-exit contract (audit batch 2,
// §O4「Crash loops MUST be contained」): a plugin that receives plugin/exit but
// never terminates must not hold the host's Manager in StateRunning forever —
// Exit() waits at most exitGracePeriod, then kills the process. The Wait
// goroutine then fires OnExit with a non-nil error, so RecordCrash still
// counts the kill (crash accounting closes for the hung class too), and the
// process lifecycle reaches its terminal state.
func TestExit_HungPluginIsKilled(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "graceful-shutdown") // 记录 exit 但永不退出

	old := exitGracePeriod
	exitGracePeriod = 300 * time.Millisecond
	t.Cleanup(func() { exitGracePeriod = old })

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	exits := make(chan error, 1)
	p, err := launchTestBinary(t, m, NewGrant(CapDiagnosticsEmit), func(err error) { exits <- err })
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	if err := p.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	exitErr := p.Exit()
	if exitErr == nil {
		t.Fatal("挂死插件的 Exit 必须报告有界等待超时并强制终止，got nil")
	}
	select {
	case werr := <-exits:
		if werr == nil {
			t.Fatal("强制终止后 OnExit 必须以非 nil 错误触发（RecordCrash 记账闭合），got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kill 后 OnExit 未触发（进程未被收割）")
	}
	// 终态：强杀之后一切 Call 被拒。
	if _, err := p.Call("probe/after-kill", nil); !errors.Is(err, ErrExited) {
		t.Fatalf("err = %v, want ErrExited", err)
	}
}

// TestShutdownExit_NormalLifecycle pins the full compliant lifecycle:
// Shutdown() round-trips (the child records sawShutdown before it ever sees
// plugin/exit), Exit() delivers plugin/exit, the child terminates cleanly
// (OnExit fires with nil), and the process is terminal — every further Call
// is refused with ErrExited.
func TestShutdownExit_NormalLifecycle(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "shutdown-exit")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	exits := make(chan error, 1)
	p, err := launchTestBinary(t, m, NewGrant(CapDiagnosticsEmit), func(err error) { exits <- err })
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	if err := p.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// 停机握手已落地：插件记录了 shutdown，且此刻尚未收到 exit。
	audit, err := p.Call("audit/lifecycle", nil)
	if err != nil {
		t.Fatalf("audit Call: %v", err)
	}
	var st struct {
		SawShutdown bool `json:"sawShutdown"`
		SawExit     bool `json:"sawExit"`
	}
	if err := json.Unmarshal(audit, &st); err != nil {
		t.Fatalf("audit unparsable: %v (%s)", err, audit)
	}
	if !st.SawShutdown || st.SawExit {
		t.Fatalf("shutdown 次序错乱: sawShutdown=%v sawExit=%v", st.SawShutdown, st.SawExit)
	}

	if err := p.Exit(); err != nil {
		t.Fatalf("Exit: %v", err)
	}
	// 插件收到 exit 后干净退出：OnExit 恰好一次、参数为 nil。
	select {
	case werr := <-exits:
		if werr != nil {
			t.Fatalf("干净退出应无错误: %v", werr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exit 通知后插件未退出")
	}

	// 终态：exit 之后一切 Call 被拒。
	if _, err := p.Call("probe/after-exit", nil); !errors.Is(err, ErrExited) {
		t.Fatalf("err = %v, want ErrExited", err)
	}
}
