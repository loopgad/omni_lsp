package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 进程生命周期三态（LSP shutdown/exit 语义的宿主半边，goal.md §C2）：
//
//		lcActive --Shutdown()--> lcShutdown --Exit()--> lcExited
//
//	  - lcShutdown：shutdown 已应答、exit 未发生——插件仍须响应请求（§C2
//	    「shutdown 停止接受新的语义工作，同时允许有界清理」），Call 照常放行；
//	  - lcActive 态直接 Exit 属于不合规退出（LSP：exit 之前必须 shutdown），
//	    宿主拒绝发送并返回 ErrNonCompliantExit；
//	  - lcExited 之后一切 Call 被拒。
type procLifecycle int

const (
	lcActive procLifecycle = iota
	lcShutdown
	lcExited
)

// ErrNonCompliantExit 表示未先 shutdown 就请求 exit——宿主拒绝发送 plugin/exit
// （LSP 语义：不合规退出，违反生命周期次序）。
var ErrNonCompliantExit = errors.New("plugin: 不合规退出：exit 之前必须先 shutdown")

// ErrAlreadyShutdown 表示 shutdown 已完成，重复 shutdown 被拒。
var ErrAlreadyShutdown = errors.New("plugin: 已 shutdown")

// ErrExited 表示插件已 exit，进程生命周期终态之后的请求一律拒绝。
var ErrExited = errors.New("plugin: 进程已 exit")

// callTimeout 是单次 Call 读取响应行的上限。
const callTimeout = 5 * time.Second

// Process 是一个进程外插件子进程的句柄（协议完整版）。
//
// 并发模型：Call 可任意并发——每个请求分配自增 id，readLoop 按响应 id 路由到
// pending 表中对应的等待者（多路复用）；所有对 stdin 的写入（Call 请求与
// Notify 通知）由 writeMu 串行化，帧与帧之间不会交错。插件→宿主的帧若带
// method 而无 id（插件侧通知），当前由 readLoop 丢弃——宿主侧订阅 API 待
// 有真实消费者再开放。OnExit 在 Launch 参数中传入（spawn 前接线），由唯一
// 的 Wait goroutine 在进程退出时调用恰好一次——事后赋值与 Wait goroutine
// 的读取构成数据竞争。
type Process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	grant  Grant

	writeMu    sync.Mutex // 串行化对 stdin 的写入
	mu         sync.Mutex // 保护 nextID、pending 响应表与 lifecycle 状态机
	nextID     int
	pending    map[int]chan response
	lifecycle  procLifecycle // shutdown/exit 三态（mu 保护）
	onExitOnce sync.Once
	// caps 是 manifest.capabilities 的快照（Launch 后不可变）：Call 级能力门
	// 的「声明」半边——Launch 已保证声明 ⊆ 授予，故 Call 只需查声明表。
	caps map[Capability]bool
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
// 协议完整版已落地：id 关联多路复用（Call 任意并发，按响应 id 配对）、
// Content-Length 头分帧（附加头容忍、帧边界精确消费、单帧大小上限）、
// 宿主→插件通知广播（Notify）、优雅停机（Shutdown/Exit 三态状态机，
// LSP shutdown/exit 语义）、以及按方法→能力映射下沉到 Call 级别的能力门
// （methodCapability + manifest 声明快照，见 Call）。
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
		caps:    make(map[Capability]bool, len(m.Capabilities)),
		OnExit:  onExit,
	}
	for _, c := range m.Capabilities {
		if c != "" { // 防御：空能力不进声明表（Validate 已拒，双保险）
			p.caps[c] = true
		}
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

// maxPluginFrameSize 是单帧负载上限：敌意/损坏的 Content-Length 声明不允许
// 让宿主按声明值无界分配内存（N14 防御纵深，覆盖 manifest 信任链之外的运行期）。
const maxPluginFrameSize = 4 << 20 // 4 MiB

func readFrame(r *bufio.Reader) ([]byte, error) {
	size := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			if size >= 0 {
				break // 头块结束，接下来读取精确长度的负载
			}
			continue // 帧间杂散空行（骨架路径兼容）
		}
		if size >= 0 {
			return nil, fmt.Errorf("plugin: 帧头块中出现意外行 %q", trimmed)
		}
		name, value, isHeader := strings.Cut(trimmed, ":")
		// 头名只可能由字母/连字符组成；JSON 负载行的首个冒号前必然带
		// 引号或花括号，据此区分「头块」与骨架的行分隔负载。
		if !isHeader || !isHeaderName(name) {
			return []byte(trimmed), nil
		}
		if strings.EqualFold(strings.TrimSpace(name), "content-length") {
			var n int
			if _, err := fmt.Sscanf(strings.TrimSpace(value), "%d", &n); err != nil {
				return nil, fmt.Errorf("plugin: malformed Content-Length header: %w", err)
			}
			if n < 0 {
				return nil, fmt.Errorf("plugin: invalid Content-Length %d", n)
			}
			if n > maxPluginFrameSize {
				return nil, fmt.Errorf("plugin: Content-Length %d 超出单帧上限 %d", n, maxPluginFrameSize)
			}
			size = n
		}
		// 其余头（如 Content-Type）按规范忽略。
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// isHeaderName 报告 s 是否形如 HTTP 头名（仅 ASCII 字母与连字符）。
func isHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// rpcError 是 JSON-RPC 响应中的 error 对象。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// readLoop 是唯一的响应读取者：按 Content-Length 帧边界逐帧读取，把响应按
// id 路由进 pending 表（多路复用的宿主半边）。致命错误（EOF/管道损坏）才会
// 中止循环并唤醒全部等待者；单帧负载损坏只丢弃该帧——多路复用通道上其余
// 并发请求必须照常获得响应，不能被一帧垃圾连坐。
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
			// 单帧负载损坏：丢弃该帧，连接与其余并发请求不受牵连。
			continue
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

// methodCapability 是插件协议「方法 → 所需能力」映射（§O2 能力门下沉）。
// 映射内的方法，Call 时要求 manifest 已声明对应能力（授予半边由 Launch 的
// 前置门保证：声明 ⊆ grant）；映射外的方法（plugin/hello、plugin/shutdown、
// 审计等协议管线）不设能力门——默认 deny 约束能力本身，不约束无能力语义的
// 协议方法。宿主侧固定映射：方法名由协议定义，不由 manifest 自由扩展。
var methodCapability = map[string]Capability{
	"index/query":      CapIndexQuery,
	"index/write":      CapIndexWrite,
	"diagnostics/emit": CapDiagnosticsEmit,
	"code_action/emit": CapCodeActionEmit,
}

// Call 发送一次 JSON-RPC 请求并按 id 等待对应响应，可任意并发（多路复用），
// 超时 callTimeout。
//
// 两道 Call 级检查：
//  1. 生命周期：lcExited 之后一律拒绝（ErrExited）；lcShutdown 照常放行——
//     LSP 语义 shutdown 之后插件仍须响应，直到 exit；
//  2. 能力门下沉：映射内方法要求 manifest 已声明所需能力（§O2），
//     未声明返回 ErrCapabilityDenied。
func (p *Process) Call(method string, params any) (json.RawMessage, error) {
	p.mu.Lock()
	if p.lifecycle == lcExited {
		p.mu.Unlock()
		return nil, fmt.Errorf("%w: Call 被拒绝", ErrExited)
	}
	if need, mapped := methodCapability[method]; mapped {
		if !p.caps[need] {
			p.mu.Unlock()
			return nil, fmt.Errorf("%w: 方法 %q 需要能力 %q，manifest 未声明", ErrCapabilityDenied, method, need)
		}
	}
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

// Shutdown 执行优雅停机的第一步：发送 plugin/shutdown 请求并等待插件应答
// （应答即停机握手确认）。按 LSP shutdown 语义（goal.md §C2）：shutdown 之后
// 插件仍须响应请求，直到 exit——故 lcShutdown 态下 Call 照常放行。
//
// 状态迁移：lcActive → lcShutdown 先行占位（阻止并发 Exit 抢跑），握手失败
// （写失败/超时）回滚到 lcActive——不合规退出拦截不得因失败的 shutdown 被绕过。
// 重复 shutdown 返回 ErrAlreadyShutdown；已 exit 后返回 ErrExited。
func (p *Process) Shutdown() error {
	p.mu.Lock()
	cur := p.lifecycle
	if cur == lcExited {
		p.mu.Unlock()
		return fmt.Errorf("%w: shutdown 被拒绝", ErrExited)
	}
	if cur == lcShutdown {
		p.mu.Unlock()
		return ErrAlreadyShutdown
	}
	p.lifecycle = lcShutdown
	p.mu.Unlock()

	if _, err := p.Call("plugin/shutdown", nil); err != nil {
		p.mu.Lock()
		p.lifecycle = lcActive
		p.mu.Unlock()
		return fmt.Errorf("plugin: shutdown 握手: %w", err)
	}
	return nil
}

// Exit 是优雅停机的终点：发送 plugin/exit 通知，插件收到后自行退出
// （干净退出经 OnExit 以 nil 上报）。按 LSP 语义 exit 之前必须先 shutdown——
// lcActive 态直接 Exit 属于不合规退出，宿主拒绝迁移状态、不向管道写任何
// 字节，返回 ErrNonCompliantExit。lcExited 态重复 Exit 返回 ErrExited。
func (p *Process) Exit() error {
	p.mu.Lock()
	cur := p.lifecycle
	if cur == lcActive {
		p.mu.Unlock()
		return fmt.Errorf("%w（宿主拒绝发送 plugin/exit）", ErrNonCompliantExit)
	}
	if cur == lcExited {
		p.mu.Unlock()
		return fmt.Errorf("%w: 重复 exit", ErrExited)
	}
	p.lifecycle = lcExited
	p.mu.Unlock()

	if err := p.Notify("plugin/exit", nil); err != nil {
		return fmt.Errorf("plugin: exit 通知: %w", err)
	}
	return nil
}

// Notify 是宿主→插件的广播通道：发送一条无 id 的 JSON-RPC 通知帧
// （method + params），不占用请求 id、不进入 pending 表、不等待回应。
// 与 Call 共享 writeMu，帧之间不会交错；写入失败（进程已死、管道关闭）
// 原样返回，由调用方决定是否接入 RecordCrash（§O4）。
func (p *Process) Notify(method string, params any) error {
	if method == "" {
		return fmt.Errorf("plugin: 通知 method 为空")
	}
	notif, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("plugin: 序列化通知: %w", err)
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if err := writeFrame(p.stdin, notif); err != nil {
		return fmt.Errorf("plugin: 写通知: %w", err)
	}
	return nil
}
