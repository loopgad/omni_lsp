package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestRunProbeHelperProcess 不是真测试：它是「挂死的工具链探测」替身，由
// TestRunProbeKillsHungToolWithinDeadline 以 -test.run 精确选中后重新执行。
// 子进程凭自身 os.Args 里的 -test.run 旗标识别替身模式——不走环境变量，免得
// 父进程跑全量套件时同名测试也进入睡眠。
func TestRunProbeHelperProcess(t *testing.T) {
	for _, arg := range os.Args {
		if arg == "-test.run=TestRunProbeHelperProcess$" {
			time.Sleep(60 * time.Second) // 挂死工具替身
			return
		}
	}
	t.Skip("not in helper-process mode")
}

// TestRunProbeKillsHungToolWithinDeadline 钉住探测硬上限（审计批 1，X10 兜底）：
// rustup 探测/版本/sysroot 三类子进程原先裸 exec.Command(...).Output() 无
// context 无超时，rustup 挂起可无限阻塞 TCP accept 循环。runProbe 必须在
// deadline 处杀掉挂死进程并返回可判定的错误，绝不等待子进程自愈。
func TestRunProbeKillsHungToolWithinDeadline(t *testing.T) {
	start := time.Now()
	_, err := runProbe(context.Background(), 300*time.Millisecond,
		os.Args[0], "-test.run=TestRunProbeHelperProcess$", "-test.count=1")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("hung probe must fail, not block until the helper exits on its own")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("runProbe returned after %s; the deadline must kill the hung tool long before the helper's 60s sleep ends", elapsed)
	}
}
