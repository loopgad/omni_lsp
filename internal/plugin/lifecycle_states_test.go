package plugin

// §O4 状态机可达性守卫。审计发现 StateFailed 与 StateRemoved 是死态：
// RecordCrash 直接从任意状态跳到 Quarantined（StateFailed 无写入点），
// Manager 方法表没有 Remove（StateRemoved 无迁移方法）。本文件钉住修复后
// 的语义：
//   - RecordCrash 未达阈值迁移 StateFailed，达阈值升 StateQuarantined；
//   - Enable 接受 Failed → Enabled（崩溃后的重启路径，成功即清零连续计数）；
//   - Remove 把任何非运行态迁入 StateRemoved 并清空授权与崩溃计数，
//     Running 必须先 Disable，未知 id 报错，重复 Remove 幂等。
// 任一状态重新变成不可达（或迁移语义漂移），本文件必须红。

import (
	"errors"
	"strings"
	"testing"
)

func TestO4_RecordCrashMarksFailedBeforeThreshold(t *testing.T) {
	id := "demo.plugin"
	mgr := NewManager()
	if err := mgr.Install(Discovery{Dir: "d", Manifest: newValidManifest([]byte("x"))}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// 首次崩溃：Verified → Failed（修复前直接留在 Verified，StateFailed 不可达）。
	mgr.RecordCrash(id)
	if st, err := mgr.State(id); err != nil || st != StateFailed {
		t.Fatalf("首次崩溃后 state = %v (err %v), want failed", st, err)
	}

	// 未达阈值的后续崩溃停留在 Failed（首次已计 1 次，再计 3 次共 4 < 5）。
	for i := 0; i < MaxConsecutivePluginCrashes-2; i++ {
		mgr.RecordCrash(id)
	}
	if st, err := mgr.State(id); err != nil || st != StateFailed {
		t.Fatalf("未达阈值时 state = %v (err %v), want failed", st, err)
	}

	// Failed 是可重启态：Enable 接受 Failed → Enabled（否则崩溃一次即死锁，
	// 只能靠 Disable 绕行）。成功启用清零连续计数。
	if _, err := mgr.Enable(id); err != nil {
		t.Fatalf("Enable from failed: %v", err)
	}
	if st, err := mgr.State(id); err != nil || st != StateEnabled {
		t.Fatalf("Enable 后 state = %v (err %v), want enabled", st, err)
	}

	// 达到阈值：升 Quarantined，Enable 一律拒绝。
	for i := 0; i < MaxConsecutivePluginCrashes; i++ {
		mgr.RecordCrash(id)
	}
	st, err := mgr.State(id)
	if err != nil || st != StateQuarantined {
		t.Fatalf("达阈值后 state = %v (err %v), want quarantined", st, err)
	}
	if _, err := mgr.Enable(id); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("期望 ErrQuarantined，实际 %v", err)
	}
}

// Disable 的 switch 早已声明 Failed 可禁用——RecordCrash 迁移出的 Failed
// 必须能走通 Failed → Verified 回退，否则该分支也是死代码。
func TestO4_FailedDisableFallsBackToVerified(t *testing.T) {
	id := "demo.plugin"
	mgr := NewManager()
	if err := mgr.Install(Discovery{Dir: "d", Manifest: newValidManifest([]byte("x"))}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	mgr.RecordCrash(id)
	if st, _ := mgr.State(id); st != StateFailed {
		t.Fatalf("state = %v, want failed", st)
	}
	if err := mgr.Disable(id); err != nil {
		t.Fatalf("Disable from failed: %v", err)
	}
	if st, err := mgr.State(id); err != nil || st != StateVerified {
		t.Fatalf("Disable 后 state = %v (err %v), want verified", st, err)
	}
}

func TestO4_RemoveMovesToRemovedAndClearsGrant(t *testing.T) {
	id := "demo.plugin"
	mgr := NewManager()
	if err := mgr.Install(Discovery{Dir: "d", Manifest: newValidManifest([]byte("x"))}); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Verified → Removed。
	if err := mgr.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if st, err := mgr.State(id); err != nil || st != StateRemoved {
		t.Fatalf("Remove 后 state = %v (err %v), want removed", st, err)
	}
	// Removed 是终态：不可再启用。
	if _, err := mgr.Enable(id); err == nil || errors.Is(err, ErrQuarantined) {
		t.Fatalf("removed 后 Enable 应拒绝且非 ErrQuarantined，实际 %v", err)
	}
	// 幂等。
	if err := mgr.Remove(id); err != nil {
		t.Fatalf("重复 Remove 应幂等: %v", err)
	}

	// Running 必须先 Disable：宿主仍持有子进程。
	mgr2 := NewManager()
	if err := mgr2.Install(Discovery{Dir: "d", Manifest: newValidManifest([]byte("y"))}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := mgr2.Enable(id); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := mgr2.MarkRunning(id); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	err := mgr2.Remove(id)
	if err == nil || !strings.Contains(err.Error(), "运行") {
		t.Fatalf("Remove(running) 应拒绝并提示先禁用，实际 %v", err)
	}
	if st, _ := mgr2.State(id); st != StateRunning {
		t.Fatalf("被拒绝的 Remove 不得改变状态，实际 %v", st)
	}

	// 未知 id 报错，而不是静默落入 Removed。
	if err := (NewManager()).Remove("nope.plugin"); err == nil {
		t.Fatal("Remove(未知 id) 应报错")
	}
}

func TestO4_RemoveClearsGrantAndCrashCount(t *testing.T) {
	id := "demo.plugin"
	mgr := NewManager()
	if err := mgr.Install(Discovery{Dir: "d", Manifest: newValidManifest([]byte("x"))}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := mgr.Enable(id); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	mgr.RecordCrash(id)
	mgr.mu.Lock()
	_, hadGrant := mgr.grants[id]
	crashes := mgr.crashes[id]
	mgr.mu.Unlock()
	if !hadGrant || crashes != 1 {
		t.Fatalf("前置状态不成立: grant=%v crashes=%d", hadGrant, crashes)
	}
	if err := mgr.Remove(id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	mgr.mu.Lock()
	_, hadGrant = mgr.grants[id]
	crashes = mgr.crashes[id]
	mgr.mu.Unlock()
	if hadGrant || crashes != 0 {
		t.Fatalf("Remove 未清空授权/崩溃计数: grant=%v crashes=%d", hadGrant, crashes)
	}
}
