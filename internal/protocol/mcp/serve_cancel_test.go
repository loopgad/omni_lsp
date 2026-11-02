package mcp

// MCP stdio 主循环的关闭路径钉住（round20/bugC）。修复前 Serve 只在两次
// Scan 之间检查 ctx.Err()，而 bufio.Scanner.Scan 阻塞在 stdin 上不响应
// ctx：`--transport mcp` 下按 Ctrl+C（signal.NotifyContext）后 ctx 已取消，
// serveMCP 仍阻塞在 Scan 里，进程不退出、CloseSession 不执行，直到客户端
// 关闭 stdin 或再来一行输入。与 LSP stdio 路径（srv.Run 可被取消）相比，
// MCP 分支失去了唯一一个信号触发的正常关闭路径。

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestServeReturnsOnContextCancelWhileBlockedOnInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw, err := os.Pipe() // 无输入：Scan 将阻塞在 stdin 上
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pw.Close()

	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, pr, io.Discard, fakeCore{}) }()

	// 让 Serve 进入阻塞中的 Scan。
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after ctx cancellation while blocked reading stdin — the signal-triggered shutdown path is unreachable (CloseSession never runs)")
	}

	// Serve 已返回；io.Reader 没有取消原语，读 goroutine 要等 EOF 才退出，
	// 关闭写端释放它（这是修复方案的已知边界，不是泄漏回归）。
	_ = pr.Close()
}

func TestServeStillProcessesThenReturnsOnCancelWithPendingLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pw.Close()
	defer pr.Close()

	if _, err := pw.WriteString(`{"jsonrpc":"2.0","id":7,"method":"ping"}` + "\n"); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(ctx, pr, io.Discard, fakeCore{}) }()

	// 取消后 Serve 返回 context.Canceled（可能抢在处理已就绪的行之前——
	// select 双就绪是随机序），但绝不能再阻塞等待新输入。
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}
