package nested

// 子进程→客户端请求的钉住测试（round20/bugC）。nested 桥的 readLoop 修复前
// 从不检查 msg.IsResponse()：带数字 id 且带 method 的子进程请求（rust-analyzer
// 的 workspace/configuration、clangd/TSLS 的 client/registerCapability、
// window/workDoneProgress/create）被当成对我们请求的响应投递——
//   (a) id 与在途请求撞车时，SendRequest 拿到 (nil, nil)，没有 Result 字段的
//       消息被当成成功空结果，握手被假成功；
//   (b) id 不撞车时被静默丢弃，真实服务器就此挂起等待客户端应答。
// JSON-RPC/LSP 要求客户端应答服务器请求；最小诚实应答是同 id 的 MethodNotFound。
//
// os.Pipe 在 Windows 上不支持读 deadline，因此 conn→子进程方向的帧由一个
// drain goroutine 解析后经 channel 交付，超时语义由 select 承担。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

func lspFrame(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

// readFrameBlocking 解析一帧；流断开时返回错误（drain goroutine 随之退出）。
func readFrameBlocking(r *bufio.Reader) (*jsonrpc.Message, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	var clen int
	if _, err := fmt.Sscanf(strings.TrimSpace(line), "Content-Length: %d", &clen); err != nil {
		return nil, fmt.Errorf("bad header %q: %w", line, err)
	}
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	body := make([]byte, clen)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var msg jsonrpc.Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// pipeConn builds a conn wired to in-process pipes: the test writes child
// output to stdoutW and receives what the conn writes back on the frames
// channel (a drain goroutine owns the stdin side; closed by cleanup).
func pipeConn(t *testing.T) (*Conn, *os.File, <-chan *jsonrpc.Message) {
	t.Helper()
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), RequestTimeout: 2 * time.Second})
	stdoutR, stdoutW, err := os.Pipe() // 子进程 stdout → conn readLoop
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stdinR, stdinW, err := os.Pipe() // conn 写入 → drain goroutine
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	conn.Attach(nil, stdinW, stdoutR)
	frames := make(chan *jsonrpc.Message, 8)
	go func() {
		defer close(frames)
		r := bufio.NewReader(stdinR)
		for {
			msg, err := readFrameBlocking(r)
			if err != nil {
				return
			}
			frames <- msg
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = stdoutW.Close()
		_ = stdinR.Close()
	})
	return conn, stdoutW, frames
}

func TestReadLoopAnswersChildRequestInsteadOfConsumingPending(t *testing.T) {
	conn, stdoutW, frames := pipeConn(t)
	_ = conn
	// 我们的 initialize 在途：id=1（Attach 之后注册，Attach 会换掉 pending 表）。
	ch := conn.RegisterPending(1)

	// 子进程→客户端请求，数字 id=1 与我们 pending 的 id=1 撞车。
	childReq := `{"jsonrpc":"2.0","id":1,"method":"workspace/configuration","params":{"items":[]}}`
	if _, err := io.WriteString(stdoutW, lspFrame(childReq)); err != nil {
		t.Fatalf("write child request: %v", err)
	}

	// 断言 1：pending 表不得被子进程请求消耗（修复前它被当成我们的响应投递）。
	select {
	case m := <-ch:
		t.Fatalf("child's server→client request (id=1) was consumed as the response to our pending id=1: method=%q error=%+v",
			m.Method, m.Error)
	default:
	}

	// 断言 2：子进程必须收到同 id 的 MethodNotFound（不被挂起）。
	var resp *jsonrpc.Message
	select {
	case resp = <-frames:
	case <-time.After(3 * time.Second):
		t.Fatal("no answer written to the child within 3s (the request was neither answered nor counted)")
	}
	if resp.ID == nil || resp.ID.IsStr || resp.ID.Num != 1 {
		t.Fatalf("first frame written to the child carries id=%+v, want numeric 1", resp.ID)
	}
	if resp.Error == nil || resp.Error.Code != jsonrpc.MethodNotFound {
		t.Fatalf("child request answered with error=%+v, want MethodNotFound(-32601)", resp.Error)
	}

	// 断言 3：真实响应仍能送达 pending。
	ours := `{"jsonrpc":"2.0","id":1,"result":{"capabilities":{}}}`
	if _, err := io.WriteString(stdoutW, lspFrame(ours)); err != nil {
		t.Fatalf("write real response: %v", err)
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			t.Fatalf("real response carried an error: %+v", m.Error)
		}
		if len(m.Result) == 0 {
			t.Fatalf("real response lost its result: %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("real response for pending id=1 never arrived")
	}
}

func TestReadLoopAnswersChildRequestWithUnknownID(t *testing.T) {
	conn, stdoutW, frames := pipeConn(t)
	_ = conn

	// id=500 与任何在途请求都不撞车：仍必须应答，不得静默丢弃。
	childReq := `{"jsonrpc":"2.0","id":500,"method":"client/registerCapability","params":{"registrations":[]}}`
	if _, err := io.WriteString(stdoutW, lspFrame(childReq)); err != nil {
		t.Fatalf("write child request: %v", err)
	}
	var resp *jsonrpc.Message
	select {
	case resp = <-frames:
	case <-time.After(3 * time.Second):
		t.Fatal("child's client-bound request (id=500) was silently dropped, never answered")
	}
	if resp.ID == nil || resp.ID.IsStr || resp.ID.Num != 500 {
		t.Fatalf("answer carries id=%+v, want numeric 500", resp.ID)
	}
	if resp.Error == nil || resp.Error.Code != jsonrpc.MethodNotFound {
		t.Fatalf("child request answered with error=%+v, want MethodNotFound(-32601)", resp.Error)
	}
}

func TestReadLoopStillDropsChildNotificationsSilently(t *testing.T) {
	conn, stdoutW, frames := pipeConn(t)
	_ = conn

	// 无 id 的通知按现状丢弃：不得产生任何回包。
	notif := `{"jsonrpc":"2.0","method":"$/unknownProgress","params":{}}`
	if _, err := io.WriteString(stdoutW, lspFrame(notif)); err != nil {
		t.Fatalf("write child notification: %v", err)
	}
	select {
	case msg := <-frames:
		t.Fatalf("a child notification (no id) was answered: %+v", msg)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestReadLoopReplyNeverStallsStdoutDrain 钉住 round20 第 2 圈修复：stdin 被
// 子进程灌满不排空时，MethodNotFound 回写绝不能把 readLoop 拖离 stdout——
// 修复前回写走 writeFrameBounded（全额 RequestTimeout 的阻塞写+超时 retire），
// readLoop 停读 stdout 长达 RequestTimeout，随后从读循环内部 retire 强撕管道
// （实测 256KB stdout 写在 ~RequestTimeout-50ms 后以 broken pipe 失败）。
// 修复后回写是旁路尽力而为：stdout 持续被排空（256KB 帧照常消化），管道
// 不被回写路径撕毁，epoch 不被 retire。
func TestReadLoopReplyNeverStallsStdoutDrain(t *testing.T) {
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), RequestTimeout: 300 * time.Millisecond})
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

	// 灌满 stdin：后台写 8MB，缓冲填满后永久阻塞（子进程停止排空 stdin）。
	stuckDone := make(chan error, 1)
	go func() {
		_, err := stdinW.Write(make([]byte, 8<<20))
		stuckDone <- err
	}()
	time.Sleep(100 * time.Millisecond) // 让管道缓冲填满、写入阻塞
	select {
	case err := <-stuckDone:
		t.Fatalf("setup: 8MB stdin write completed unexpectedly (%v) — pipe buffer larger than assumed, test shape broken", err)
	default:
	}

	// 子进程 client-bound 请求 → 触发 MethodNotFound 回写路径。
	childReq := `{"jsonrpc":"2.0","id":1,"method":"workspace/configuration","params":{"items":[]}}`
	if _, err := io.WriteString(stdoutW, lspFrame(childReq)); err != nil {
		t.Fatalf("write child request: %v", err)
	}

	// 回写卡死期间 readLoop 必须继续排空 stdout：256KB 的合法通知帧必须能
	// 完整写入（readLoop 消费 body 后写才算完成）。
	big := `{"jsonrpc":"2.0","method":"$/big","params":{"pad":"` + strings.Repeat("x", 256<<10) + `"}}`
	started := time.Now()
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(stdoutW, lspFrame(big))
		writeDone <- err
	}()
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("256KB stdout write failed after %v: %v (the reply path tore the pipes / stopped the drain)", time.Since(started), err)
		}
		t.Logf("256KB stdout frame consumed in %v while the reply write is stuck", time.Since(started))
	case <-time.After(3 * time.Second):
		t.Fatalf("256KB stdout write did not complete within 3s: readLoop stalled on the MethodNotFound reply (RequestTimeout-bounded write runs on the read loop)")
	}

	// 回写路径不得 retire：epoch 不变、stdin 不被它关闭。
	if got := conn.readerEpoch.Load(); got != 1 {
		t.Fatalf("readerEpoch = %d, want 1: a failing courtesy reply must not retire the child from inside the read loop", got)
	}
}
