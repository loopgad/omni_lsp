package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// callTimeout 是单次 Call 读取响应行的上限。
const callTimeout = 5 * time.Second

// Process 是一个进程外插件子进程的句柄（Tier 1 执行骨架）。
//
// 并发模型：Call 内部单飞锁串行化「写请求 + 读一行响应」；OnExit 由唯一的
// Wait goroutine 在进程退出时调用恰好一次（须在 Launch 返回后立即接线）。
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	grant  Grant

	mu         sync.Mutex // 串行化 write+read 配对
	nextID     int
	onExitOnce sync.Once
	// OnExit 在进程退出后被调用一次，参数为 Wait 的返回值；
	// 宿主用它接入 Manager.RecordCrash 实现崩溃循环遏制。
	OnExit func(err error)
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
func Launch(ctx context.Context, m Manifest, grant Grant) (*Process, error) {
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
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReader(stdout),
		grant:  grant,
	}
	go func() {
		werr := cmd.Wait()
		p.onExitOnce.Do(func() {
			if p.OnExit != nil {
				p.OnExit(werr)
			}
		})
	}()

	hf := helloFrame{JSONRPC: "2.0", Method: "plugin/hello"}
	hf.Params.APIVersion = SupportedAPIVersion
	for _, c := range m.Capabilities {
		if grant.caps[c] {
			hf.Params.Grants = append(hf.Params.Grants, string(c))
		}
	}
	b, _ := json.Marshal(hf)
	if _, err := stdin.Write(append(b, '\n')); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("plugin: 握手写入失败: %w", err)
	}
	return p, nil
}

// rpcError 是 JSON-RPC 响应中的 error 对象。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Call 发送一次 JSON-RPC 请求并等待单行响应，超时 callTimeout。
// TODO(协议完整版)：见 Launch 尾注的升级路径。
func (p *Process) Call(method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.nextID++
	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      p.nextID,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("plugin: 序列化请求: %w", err)
	}
	if _, err := p.stdin.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("plugin: 写请求: %w", err)
	}

	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := p.stdout.ReadBytes('\n')
		ch <- result{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("plugin: 读响应: %w", r.err)
		}
		var frame struct {
			JSONRPC string          `json:"jsonrpc"`
			Result  json.RawMessage `json:"result"`
			Error   *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(r.line, &frame); err != nil {
			return nil, fmt.Errorf("plugin: 解析响应: %w", err)
		}
		if frame.Error != nil {
			return nil, fmt.Errorf("plugin: rpc 错误 %d: %s", frame.Error.Code, frame.Error.Message)
		}
		return frame.Result, nil
	case <-time.After(callTimeout):
		return nil, fmt.Errorf("plugin: 响应超时（>%v）", callTimeout)
	}
}
