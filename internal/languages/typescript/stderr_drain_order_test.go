package typescript

// round20 第 3 圈（pyright direct_compiler.go 同款问题的 TypeScript 侧）：
// runScript 的解码循环以 stdout EOF 退出后先 cmd.Wait() 再 <-stderrDone，
// Wait 按 os/exec 契约关闭 stderr 读端，未排空字节（含句柄继承的孙进程在
// 退出后的写入）随之丢失，错误 detail 被截断。修复：先 <-stderrDone 再
// Wait。构造细节（孙进程必须显式接 Stderr 等）见 pyright 包同名测试。

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
// 关闭自身 stdout、50ms 后 exit 7。模式 "deadline"（round20 第 4 圈）把孙进
// 程寿命拉到 ~2s，用于钉住「stderr 排空必须受 caller ctx 约束」。
func TestMain(m *testing.M) {
	if mode := os.Getenv("OMNILSP_FAKE_TSC_EXPORTER"); mode != "" {
		ping := "-n 2" // ≈1s：尾标晚于父侧 Wait 关读端，早于合理 ctx
		if mode == "deadline" {
			ping = "-n 3" // ≈2s：远超 500ms 的 caller ctx
		}
		grandchild := exec.Command("cmd", "/c", fmt.Sprintf("ping %s 127.0.0.1 >nul 2>&1 & echo ESTAIL-TSC-9C7A3F 1>&2", ping))
		grandchild.Stderr = os.Stderr
		gerr := grandchild.Start()
		if f, ferr := os.Create(os.Getenv("OMNILSP_FAKE_TSC_DEBUG")); ferr == nil {
			fmt.Fprintf(f, "grandchild start err: %v\n", gerr)
			_ = f.Close()
		}
		_ = os.Stdout.Close() // 父侧解码循环立即见 EOF
		time.Sleep(50 * time.Millisecond)
		os.Exit(7)
	}
	os.Exit(m.Run())
}

func TestCompilerRunScriptDrainsStderrBeforeWait(t *testing.T) {
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
	t.Setenv("OMNILSP_FAKE_TSC_EXPORTER", "1")
	t.Setenv("OMNILSP_FAKE_TSC_DEBUG", filepath.Join(t.TempDir(), "gc-debug.txt"))

	r := NewDirectCompilerRunner(fakeNode)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	for i := 0; i < 4; i++ {
		_, err := r.runScript(ctx, fakeNode, directCompilerRequest{}, func(SCIPBatch) error { return nil })
		if err == nil {
			t.Fatalf("iteration %d: expected the fake exporter failure", i)
		}
		if !strings.Contains(err.Error(), "ESTAIL-TSC-9C7A3F") {
			t.Fatalf("iteration %d: stderr detail lost (Wait ran before the stderr capture drained — the grandchild's late write landed on an already-closed read end): %v", i, err)
		}
	}
}

// TestCompilerRunScriptReturnsOnCallerDeadlineDespiteLiveDescendant 钉住
// round20 第 4 圈修复（pyright 同款）：stderr EOF 要求所有写端关闭，句柄继
// 承的孙进程可以在 exporter 退出后无限期推迟它，而 CommandContext 只管直接
// 子进程（早已自退）——裸 <-stderrDone 因此不受 caller ctx 约束。修复后等
// 待以 caller ctx 为界：ctx 先到即关闭父侧读端打断捕获 goroutine，未排空尾
// 巴随取消一并放弃。
func TestCompilerRunScriptReturnsOnCallerDeadlineDespiteLiveDescendant(t *testing.T) {
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
	t.Setenv("OMNILSP_FAKE_TSC_EXPORTER", "deadline") // 孙进程存活 ~2s

	r := NewDirectCompilerRunner(fakeNode)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err = r.runScript(ctx, fakeNode, directCompilerRequest{}, func(SCIPBatch) error { return nil })
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
