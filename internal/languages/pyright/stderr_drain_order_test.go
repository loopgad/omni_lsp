package pyright

// round20 第 3 圈：runScript 的解码循环以 stdout EOF 退出后先 cmd.Wait() 再
// <-stderrDone。os/exec 的 StdoutPipe/StderrPipe 契约明言「Wait before all
// reads from the pipe have completed 是错误的」——Wait 在进程退出后关闭
// stderr 读端，未排空的字节随之丢失，错误信息里刻意格式化的 stderr detail
// （waitErr != nil 分支）被截断或清空（读错误被 value, _ := 丢弃）。修复：
// 先 <-stderrDone（stdout 解码循环已因 EOF 退出，stdout 侧无死锁面），再
// cmd.Wait()。
//
// 同进程「写完立即退出」的丢失窗口在实测中不可触发（写→退出间的进程拆除
// 间隔始终够捕获 goroutine 排空，含单 P 收紧），故本测试用**确定性构造**：
// 假 node（测试二进制自拷贝，TestMain 模式分支）派生一个继承 stderr 的孙
// 进程（cmd /c ping 1s 后向 stderr 写唯一尾标），随即关闭自身 stdout 并以
// exit 7 退出——父侧解码循环立即见 EOF，旧顺序的 Wait 在 ~50ms 内关闭
// stderr 读端，孙进程 1s 后的写落在已关闭的读端上，尾标必然丢失；新顺序
// 的 <-stderrDone 会等到孙进程写端关闭，尾标必然在列。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain 让同一测试二进制可以假扮 exporter：派生延迟写 stderr 的孙进程、
// 关闭自身 stdout、50ms 后 exit 7——保证父侧的 EOF→Wait→关读端全部先于孙
// 进程的写入完成。注意：Windows 上孙进程必须显式接 Stderr——不设时它拿到
// 的是控制台句柄而非本进程被重定向的管道（缺 STARTF_USESTDHANDLES）。
// 模式 "deadline"（round20 第 4 圈）把孙进程寿命拉到 ~2s，用于钉住「stderr
// 排空必须受 caller ctx 约束」。
func TestMain(m *testing.M) {
	if mode := os.Getenv("OMNILSP_FAKE_PYRIGHT_EXPORTER"); mode != "" {
		ping := "-n 2" // ≈1s：尾标晚于父侧 Wait 关读端，早于合理 ctx
		if mode == "deadline" {
			ping = "-n 3" // ≈2s：远超 500ms 的 caller ctx
		}
		grandchild := exec.Command("cmd", "/c", fmt.Sprintf("ping %s 127.0.0.1 >nul 2>&1 & echo ESTAIL-PYRIGHT-42B7D1 1>&2", ping))
		grandchild.Stderr = os.Stderr
		gerr := grandchild.Start()
		if f, ferr := os.Create(os.Getenv("OMNILSP_FAKE_PYRIGHT_DEBUG")); ferr == nil {
			fmt.Fprintf(f, "grandchild start err: %v\n", gerr)
			_ = f.Close()
		}
		_ = os.Stdout.Close() // 父侧解码循环立即见 EOF
		time.Sleep(50 * time.Millisecond)
		os.Exit(7)
	}
	os.Exit(m.Run())
}

func TestRunScriptDrainsStderrBeforeWait(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("fake exporter staged as a copied Windows executable with a cmd/ping grandchild")
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	data, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	fakeNode := filepath.Join(t.TempDir(), "fake-node.exe")
	if err := os.WriteFile(fakeNode, data, 0o755); err != nil {
		t.Fatalf("stage fake node: %v", err)
	}
	t.Setenv("OMNILSP_FAKE_PYRIGHT_EXPORTER", "1")
	t.Setenv("OMNILSP_FAKE_PYRIGHT_DEBUG", filepath.Join(t.TempDir(), "gc-debug.txt"))

	r := NewDirectPyrightRunner(fakeNode)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	for i := 0; i < 4; i++ {
		_, err := r.runScript(ctx, fakeNode, directPyrightRequest{}, func(PyrightSemanticBatch) error { return nil })
		if err == nil {
			t.Fatalf("iteration %d: expected the fake exporter failure", i)
		}
		if !strings.Contains(err.Error(), "ESTAIL-PYRIGHT-42B7D1") {
			t.Fatalf("iteration %d: stderr detail lost (Wait ran before the stderr capture drained — the grandchild's late write landed on an already-closed read end): %v", i, err)
		}
	}
}

// TestRunScriptReturnsOnCallerDeadlineDespiteLiveDescendant 钉住 round20 第 4
// 圈修复：stderr EOF 要求所有写端关闭，句柄继承的孙进程可以在 exporter 退出
// 后无限期推迟它，而 CommandContext 只管直接子进程（早已自退）——裸
// <-stderrDone 因此不受 caller ctx 约束（ask 实测：ctx=2s，孙进程 ~11s，返回
// 11.26s）。修复后等待必须以 caller ctx 为界：ctx 先到即关闭父侧读端打断捕
// 获 goroutine，未排空尾巴随取消一并放弃（旧行为同场景也丢尾但能返回）。
func TestRunScriptReturnsOnCallerDeadlineDespiteLiveDescendant(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("fake exporter staged as a copied Windows executable with a cmd/ping grandchild")
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	data, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	fakeNode := filepath.Join(t.TempDir(), "fake-node.exe")
	if err := os.WriteFile(fakeNode, data, 0o755); err != nil {
		t.Fatalf("stage fake node: %v", err)
	}
	t.Setenv("OMNILSP_FAKE_PYRIGHT_EXPORTER", "deadline") // 孙进程存活 ~2s

	r := NewDirectPyrightRunner(fakeNode)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err = r.runScript(ctx, fakeNode, directPyrightRequest{}, func(PyrightSemanticBatch) error { return nil })
	elapsed := time.Since(started)
	if err == nil {
		t.Fatalf("runScript returned nil after ctx cancellation")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runScript error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 1200*time.Millisecond {
		t.Fatalf("runScript returned after %v with a 500ms ctx: the stderr drain is not bounded by the caller's deadline (the grandchild's pipe tenure governs the return instead)", elapsed)
	}
}
