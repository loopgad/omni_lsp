package nested

// round20 第 3 圈：killProcess 是唯一无 epoch 栅栏的终止路径（对照
// retireProcessEpoch 的 current 检查）。watchdog 对 epoch E 做出 hung 判定后
// 在新 goroutine 里执行击杀；判定与击杀之间完成一个完整重启周期（子进程
// EOF→handleProcessExit→MarkUnhealthy→retryAfter→Start→Attach）时，无栅栏
// 的击杀会误杀刚替换上的健康子进程并 failPending 其 pending 表。修复：击杀
// 携带判定时的 epoch，c.mu 临界段内比对，不一致即放弃。
//
// 竞态窗口是 µs 级，无法确定性复现整条 watchdog 链；本测试直接钉住栅栏本
// 身的两个面：陈旧 epoch 击杀必须放过当前子进程，当前 epoch 击杀必须生效
// （探测方式：向 conn 持有的 stdin 写端写入——已关闭则写报错）。红由变异
// 实验证明：把 killProcess 的 epoch 比对短路（`_ = epoch`）后本测试转红。

import (
	"os"
	"testing"
)

func TestKillProcessRespectsEpochFence(t *testing.T) {
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir()})
	stdoutA, stdoutAW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe A: %v", err)
	}
	stdinA, stdinAW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe A: %v", err)
	}
	conn.Attach(nil, stdinAW, stdoutAW) // epoch → 1
	if got := conn.readerEpoch.Load(); got != 1 {
		t.Fatalf("setup: readerEpoch = %d, want 1", got)
	}

	// 当前 epoch 击杀：必须拆掉 A 的管道（stdin 写端不可再写）。
	conn.killProcess(1)
	if _, err := stdinAW.Write([]byte("x")); err == nil {
		t.Fatal("killProcess(current epoch) left the child's stdin writable (pipes untouched)")
	}
	_ = stdoutAW.Close()
	_ = stdoutA.Close()
	_ = stdinA.Close()

	// 完整重启周期：替换子进程（epoch → 2，新管道 B）。
	stdoutB, stdoutBW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe B: %v", err)
	}
	stdinB, stdinBW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe B: %v", err)
	}
	defer func() {
		_ = stdoutB.Close()
		_ = stdoutBW.Close()
		_ = stdinB.Close()
	}()
	conn.Attach(nil, stdinBW, stdoutBW)
	if got := conn.readerEpoch.Load(); got != 2 {
		t.Fatalf("setup: readerEpoch after restart = %d, want 2", got)
	}

	// 陈旧 epoch（判定所属的 1）击杀：必须放过健康替换子进程——B 的 stdin
	// 仍可写（24 字节小帧落在管道缓冲内，无需排空）。
	conn.killProcess(1)
	if _, err := stdinBW.Write([]byte("Content-Length: 2\r\n\r\n{}")); err != nil {
		t.Fatalf("a stale-epoch kill murdered the healthy replacement child's stdin: %v", err)
	}

	// 当前 epoch 击杀：必须生效。
	conn.killProcess(2)
	if _, err := stdinBW.Write([]byte("x")); err == nil {
		t.Fatal("killProcess(current epoch) left the replacement child's stdin writable (pipes untouched)")
	}
}
