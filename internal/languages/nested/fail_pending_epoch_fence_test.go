package nested

// round20 第 4 圈：4eeea22 只栅栏了 hung-worker 派发的击杀半边（killProcess）。
// cancelFn 半边调 failPending 整表交换 pending map，无任何 epoch 检查——判定
// 与投递之间完成一个完整重启周期（唯一 epoch 增量点 Attach）时，替换 epoch
// 的 pending 请求被误标「<name> worker hung; requests failed」，真实响应因
// id 已出表被丢弃，而栅栏后的 kill 正确 no-op：健康子进程存活、无重启兜底、
// 错误语义与事实不符。修复：failPendingIfEpoch 与 killProcess 同一栅栏语义。
//
// 竞态窗口 µs 级，端到端不可确定性复现（与 kill_epoch_fence_test 同判）；本
// 测试直接钉住栅栏的两个面。红由变异实验证明：短路 epoch 比对后，陈旧投递
// 会击中替换 epoch 的 pending 表，本测试转红。

import (
	"os"
	"strings"
	"testing"
)

func TestFailPendingRespectsEpochFence(t *testing.T) {
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	stdout1, stdout1W, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe 1: %v", err)
	}
	stdin1, stdin1W, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe 1: %v", err)
	}
	conn.Attach(nil, stdin1W, stdout1W) // epoch → 1
	defer func() {
		_ = stdout1.Close()
		_ = stdout1W.Close()
		_ = stdin1.Close()
	}()
	_ = conn.RegisterPending(7) // epoch-1 请求（重启周期会被 Attach 正常清算）

	// 完整重启周期：替换子进程（epoch → 2），替换 epoch 上挂起新请求。
	stdout2, stdout2W, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe 2: %v", err)
	}
	stdin2, stdin2W, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe 2: %v", err)
	}
	defer func() {
		_ = stdout2.Close()
		_ = stdout2W.Close()
		_ = stdin2.Close()
	}()
	conn.Attach(nil, stdin2W, stdout2W)
	if got := conn.readerEpoch.Load(); got != 2 {
		t.Fatalf("setup: readerEpoch after restart = %d, want 2", got)
	}
	ch8 := conn.RegisterPending(8)

	// 陈旧 epoch（判定所属的 1）投递：必须放过替换 epoch 的 pending 表。
	const hung = "fake-lsp worker hung; requests failed"
	conn.failPendingIfEpoch(1, hung)
	select {
	case m := <-ch8:
		t.Fatalf("a stale-epoch cancel half stamped the replacement's pending request as hung: %+v", m)
	default:
	}

	// 当前 epoch 投递：必须整表清算并送达 hung 错误。
	conn.failPendingIfEpoch(2, hung)
	select {
	case m := <-ch8:
		if m.Error == nil || !strings.Contains(m.Error.Message, "worker hung") {
			t.Fatalf("current-epoch fail delivered %+v, want the worker-hung InternalError", m)
		}
	default:
		t.Fatal("current-epoch fail left the pending request unanswered")
	}
}
