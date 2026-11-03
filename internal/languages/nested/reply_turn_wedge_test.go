package nested

// round20 第 3 圈：礼貌回写停桩在满管道上并持有共享写回合后，后续用户请求
// 的 writeFrameBounded 等回合超时（LockContext 直接返回错误）并不 retire——
// 「writer-timeout retire」这条注释声称的释放途径恰好不可达，连接永久楔死：
// 每个请求烧满 RequestTimeout 后失败，无自愈（§G5 watchdog 对「持续产 stdout
// 但不排 stdin」的子进程沉默，readLoop 每消息刷新 lastActivity）。修复：等
// 回合前先快照管道+epoch，回合超时路径对该确切 epoch 执行 retire（retire 只
// 需 c.mu，不需写回合；关闭捕获的 stdin 释放停桩回写，写者经 defer 归还回
// 合），监督器随后重启子进程。

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

func TestStuckReplyTurnIsRetiredByWriterTimeout(t *testing.T) {
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), RequestTimeout: 200 * time.Millisecond})
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	conn.Attach(nil, stdinW, stdoutR)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = stdoutW.Close()
		_ = stdinR.Close()
	})

	// 灌满 stdin：后台写 8MB，缓冲填满后永久阻塞（子进程停止排空）。
	filler := make(chan error, 1)
	go func() {
		_, err := stdinW.Write(make([]byte, 8<<20))
		filler <- err
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-filler:
		t.Fatalf("setup: 8MB stdin write completed unexpectedly (%v) — pipe buffer larger than assumed", err)
	default:
	}

	// 子进程 client-bound 请求 → 礼貌回写拿到空闲写回合后停桩在满管道上。
	childReq := `{"jsonrpc":"2.0","id":1,"method":"workspace/configuration","params":{"items":[]}}`
	if _, err := io.WriteString(stdoutW, lspFrame(childReq)); err != nil {
		t.Fatalf("write child request: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // 回写已停桩并持有写回合

	// 回合被停桩回写占住后，用户请求必须触发自愈（对确切的 epoch retire），
	// 而不是烧满超时后原样失败且连接永久楔死。
	for i := 0; i < 2; i++ {
		started := time.Now()
		if _, err := conn.SendRequest(context.Background(), "textDocument/hover", map[string]any{}); err == nil {
			t.Fatalf("request %d: expected failure against a wedged child", i)
		} else {
			t.Logf("request %d failed in %v: %v", i, time.Since(started), err)
		}
	}
	conn.mu.Lock()
	stdin := conn.stdin
	conn.mu.Unlock()
	if stdin != nil {
		t.Fatalf("the write turn was held beyond RequestTimeout by a parked reply and the child was never retired across two timed-out requests — the connection is wedged: every later request burns the full timeout with no escape (G5 stays silent since stdout traffic keeps refreshing lastActivity)")
	}
}
