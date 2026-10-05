package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// callTimeout 是单次 Call 读取响应行的上限。
const callTimeout = 5 * time.Second

// Process 是一个进程外插件子进程的句柄（Tier 1 执行骨架）。
//
// 并发模型：Call 内部单飞锁串行化「写请求 + 读一行响应」；OnExit 在 Launch
// 参数中传入（spawn 前接线），由唯一的 Wait goroutine 在进程退出时调用
// 恰好一次——事后赋值与 Wait goroutine 的读取构成数据竞争。
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	grant  Grant

	writeMu    sync.Mutex // 串行化对 stdin 的写入
	mu         sync.Mutex // 保护 nextID 与 pending 响应表
	nextID     int
	pending    map[int]chan response
	onExitOnce sync.Once
	// OnExit 在进程退出后被调用恰好一次，参数为 Wait 的返回值；
	// 宿主用它接入 Manager.RecordCrash 实现崩溃循环遏制。
	// 只能在 Launch 时设置（spawn 前接线，见 Launch 文档）。
	OnExit func(err error)
}

type response struct {
	result json.RawMessage
	err    error
}

// helloFrame 对应首次握手：宿主 → 插件的 plugin/hello 通知帧。
type helloFrame struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  struct {
		APIVersion string   `json:"apiVersion"`
		Grants     []string `json:"grants"`
	} `json:"params"`
}

// Launch 启动进程外插件并完成握手。前置门（顺序固定）：
//  1. manifest 完整性（Validate）；
//  2. 能力门——manifest 声明的每项能力必须在 grant 中放行，
//     任一被拒即返回 ErrCapabilityDenied，不产生子进程。
//
// TODO(协议完整版)：当前为行分隔 JSON-RPC 骨架。升级路径：id 关联多路复用、
// 并发请求、notification 广播、Content-Length 头分帧、优雅 shutdown（plugin/shutdown
// + plugin/exit）、以及按方法到能力的映射把能力门下沉到 Call 级别。
func Launch(ctx context.Context, m Manifest, grant Grant, onExit func(error)) (*Process, error) {
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("plugin: launch 前置完整性校验: %w", err)
	}
	for _, c := range m.Capabilities {
		if err := grant.Check(c); err != nil {
			return nil, err // 能力门在 spawn 之前生效（§O2）
		}
	}

	cmd := exec.CommandContext(ctx, m.Entrypoint)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: stdin 管道: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin: stdout 管道: %w", err)
	}
	cmd.Stderr = os.Stderr // ponytail: stderr 直通父进程；静默捕获留待完整协议
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin: 启动 %q: %w", m.Entrypoint, err)
	}

	p := &Process{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  bufio.NewReader(stdout),
		grant:   grant,
		pending: make(map[int]chan response),
		OnExit:  onExit,
	}
	hf := helloFrame{JSONRPC: "2.0", Method: "plugin/hello"}
	hf.Params.APIVersion = SupportedAPIVersion
	for _, c := range m.Capabilities {
		if grant.caps[c] {
			hf.Params.Grants = append(hf.Params.Grants, string(c))
		}
	}
	b, _ := json.Marshal(hf)
	if err := writeFrame(stdin, b); err != nil {
		_ = cmd.Process.Kill()
		// The Wait goroutine starts only after a successful handshake, so
		// nothing else reaps this child or closes the stdio pipes. Reap it here
		// or the plugin leaks as a zombie holding two pipe handles (§O4).
		_ = cmd.Wait()
		return nil, fmt.Errorf("plugin: 握手写入失败: %w", err)
	}
	// The reader and the Wait goroutine start only after the handshake lands.
	// Starting them earlier meant a handshake failure killed the process while
	// the Wait goroutine was already live, so OnExit fired once for a process
	// the caller never received — a launch failure counted as a plugin crash
	// against MaxConsecutivePluginCrashes (§O4).
	go p.readLoop()
	go func() {
		werr := cmd.Wait()
		p.onExitOnce.Do(func() {
			if p.OnExit != nil {
				p.OnExit(werr)
			}
		})
	}()
	return p, nil
}

func writeFrame(w io.Writer, payload []byte) error {
	_, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload))
	if err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return nil, nil
	}
	if !strings.HasPrefix(strings.ToLower(trimmed), "content-length:") {
		return []byte(strings.TrimRight(line, "\r\n")), nil
	}
	var size int
	if _, err := fmt.Sscanf(trimmed, "Content-Length: %d", &size); err != nil {
		return nil, fmt.Errorf("plugin: malformed Content-Length header: %w", err)
	}
	if size < 0 {
		return nil, fmt.Errorf("plugin: invalid Content-Length %d", size)
	}
	if _, err := r.Discard(2); err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// rpcError 是 JSON-RPC 响应中的 error 对象。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Call 发送一次 JSON-RPC 请求并等待单行响应，超时 callTimeout。
// TODO(协议完整版)：见 Launch 尾注的升级路径。
func (p *Process) readLoop() {
	for {
		line, err := readFrame(p.stdout)
		if err != nil {
			p.mu.Lock()
			p.notifyPendingLocked(fmt.Errorf("plugin: 读响应: %w", err))
			p.mu.Unlock()
			return
		}
		if len(line) == 0 {
			continue
		}

		var frame struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			p.mu.Lock()
			p.notifyPendingLocked(fmt.Errorf("plugin: 解析响应: %w", err))
			p.mu.Unlock()
			return
		}

		p.mu.Lock()
		ch, ok := p.pending[frame.ID]
		if ok {
			delete(p.pending, frame.ID)
		}
		p.mu.Unlock()
		if !ok {
			continue
		}
		if frame.Error != nil {
			ch <- response{err: fmt.Errorf("plugin: rpc 错误 %d: %s", frame.Error.Code, frame.Error.Message)}
			continue
		}
		ch <- response{result: frame.Result}
	}
}

func (p *Process) notifyPendingLocked(err error) {
	for id, ch := range p.pending {
		delete(p.pending, id)
		ch <- response{err: err}
	}
}

func (p *Process) Call(method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	p.nextID++
	id := p.nextID
	ch := make(chan response, 1)
	p.pending[id] = ch
	p.mu.Unlock()

	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("plugin: 序列化请求: %w", err)
	}

	p.writeMu.Lock()
	if err := writeFrame(p.stdin, req); err != nil {
		p.writeMu.Unlock()
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, fmt.Errorf("plugin: 写请求: %w", err)
	}
	p.writeMu.Unlock()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return r.result, nil
	case <-time.After(callTimeout):
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, fmt.Errorf("plugin: 响应超时（>%v）", callTimeout)
	}
}
