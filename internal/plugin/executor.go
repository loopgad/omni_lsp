package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omnilsp/omni/internal/runtime/budget"
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

// callTimeout 是单次 Call 读取响应行的上限。非常量——测试注入更短窗口
// （与 exitGracePeriod 同一约定；生产默认值不变）。
var callTimeout = 5 * time.Second

// writeStuckTimeout 是宿主→插件单帧写入的有界窗口（复证圈 1 审计发现 4）。
// 旧实现里 Call 的 callTimeout 只约束响应等待（select 在写完成之后才启动），
// Exit 的 grace timer 只在 Notify 写成功返回后才布防——子进程存活但停止读
// stdin 时 writeFrame 无界阻塞并持有 writeMu，挂死插件的 kill 界被整体绕过。
// 非常量——测试注入更短窗口。
var writeStuckTimeout = 2 * time.Second

// writeBounded 在有界时间内完成一次宿主→插件的 stdin 写帧：写移入独立
// goroutine（仍经 writeMu 串行化，帧间不交错），超时返回错误。
//
// 写阻塞超时后，该 goroutine 仍卡在 writeFrame 上直到进程死亡（kill 使管道
// 断裂、写以错误收场）才释放 writeMu——因此写超时必须由调用方的 kill 兜底
// 闭合（Exit 的通知写失败路径、closePlugins 对 Shutdown 失败的有界拆除），
// 单靠超时本身解除不了 writeMu。
func (p *Process) writeBounded(payload []byte) error {
	done := make(chan error, 1) // 容量 1：卡死的 goroutine 在进程死亡后完成发送，不泄漏
	go func() {
		p.writeMu.Lock()
		defer p.writeMu.Unlock()
		done <- writeFrame(p.stdin, payload)
	}()
	timer := time.NewTimer(writeStuckTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("plugin: stdin 写帧 %v 内未完成（插件存活但疑似停止读 stdin）", writeStuckTimeout)
	}
}

// pluginNotifyBuffer 是插件→宿主通知投递通道的容量（有界）。readLoop 以
// 非阻塞 select 投递：慢消费者不允许拖死唯一的响应读取循环——通道满即丢弃
// 该帧并计入 DroppedNotifies（丢弃可见，绝不静默，也绝不阻塞）。
const pluginNotifyBuffer = 64

// pluginNotifyEvent 是一条插件→宿主通知（无 id、带 method + params）。
type pluginNotifyEvent struct {
	method string
	params json.RawMessage
}

// Process 是一个进程外插件子进程的句柄（协议完整版）。
//
// 并发模型：Call 可任意并发——每个请求分配自增 id，readLoop 按响应 id 路由到
// pending 表中对应的等待者（多路复用）；所有对 stdin 的写入（Call 请求与
// Notify 通知）由 writeMu 串行化，帧与帧之间不会交错。插件→宿主的帧若带
// method 而无 id（插件侧通知），readLoop 解析后经有界通道投递给宿主订阅
// 回调（SetNotifySink）；无订阅者或通道满时按有界有损语义丢弃并计入
// DroppedNotifies——慢消费者拖不死 readLoop。OnExit 在 Launch 参数中传入
// （spawn 前接线），由唯一的 Wait goroutine 在进程退出时调用恰好一次——
// 事后赋值与 Wait goroutine 的读取构成数据竞争。
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
	// exited 在进程被收割（Wait 返回）时关闭，先于 OnExit 回调：Exit 的
	// 有界等待不依赖回调时序，kill 竞速不持任何锁。
	exited chan struct{}
	// caps 是 manifest.capabilities 的快照（Launch 后不可变）：Call 级能力门
	// 的「声明」半边——Launch 已保证声明 ⊆ 授予，故 Call 只需查声明表。
	caps map[Capability]bool
	// OnExit 在进程退出后被调用恰好一次，参数为 Wait 的返回值；
	// 宿主用它接入 Manager.RecordCrash 实现崩溃循环遏制。
	// 只能在 Launch 时设置（spawn 前接线，见 Launch 文档）。
	OnExit func(err error)

	// notifyCh 是插件→宿主通知的有界投递通道：readLoop 是唯一发送者
	// （退出时 close），pumpNotifies 是唯一消费者。满即丢弃并计数——
	// 慢消费者最多拖慢 pump 自己，永远拖不到 readLoop。
	notifyCh        chan pluginNotifyEvent
	droppedNotifies atomic.Uint64
	// notifyBudget 是通知缓冲区的 §F14 字节配额（第 18 轮开口 5，与
	// internal/languages 的后端缓冲接线同构：env 显式开关、默认 nil 关、
	// fail-closed 拒绝）。nil = 未配置 = 零行为变化（X10）。
	notifyBudget *budget.Manager
	// budgetDroppedNotifies 计数因超出配额被整帧拒绝的通知数（可见不静默；
	// 与通道饱和/无订阅者的 DroppedNotifies 分开记账，拒绝原因可辨）。
	budgetDroppedNotifies atomic.Uint64
	// sink 是宿主订阅回调（SetNotifySink 注册，sinkMu 保护：可在 Launch
	// 前后任意时刻并发注册/替换）。nil 表示无订阅者——通知照投递进通道、
	// 由 pump 计数丢弃，保持「丢弃可见不静默」的同一语义。
	sinkMu sync.Mutex
	sink   func(method string, params json.RawMessage)
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
//  2. 工作区信任断言——trust 是必经前置回调（§N1/N3 plugin.load）：nil 即拒，
//     非 nil 时在 spawn 前调用，返回错误即拒绝。审计批 2 把它从 Enable 上的
//     可选回调（nil 即放行）下沉至此——直调 Launch 绕过 Enable 也绕不开信任；
//  3. 能力门——manifest 声明的每项能力必须在 grant 中放行，
//     任一被拒即返回 ErrCapabilityDenied，不产生子进程；
//  4. 信任链执行时刻复核——entrypoint 必须是 dir 下的裸文件名，且启动前对
//     dir/<entrypoint> 重算 sha256 与 manifest.Checksum 比对（N14/§O3）。
//     复核句柄自持到 Start 返回（Windows 上为拒绝写/删的共享模式）：被
//     复核的字节与被执行的字节钉在同一份文件上，先前的 Verify 结果不顶替
//     本次复核（校验与 exec 之间二进制可能已被调包），裸文件名绝不经 PATH
//     解析——PATH 上的同名影子二进制不得顶替已通过 checksum 的那一个。
//     审计批 2 发现 3 更正：旧注释「钉在同一个文件上」按路径重开而言是
//     overstated（钉的是同一路径，不是同一份字节）；句柄贯穿修复后该句
//     在 Windows 上成立，Unix 上残余窗口在 openEntryFile 记录。
//
// 协议完整版已落地：id 关联多路复用（Call 任意并发，按响应 id 配对）、
// Content-Length 头分帧（附加头容忍、帧边界精确消费、单帧大小上限）、
// 宿主→插件通知广播（Notify）、优雅停机（Shutdown/Exit 三态状态机，
// LSP shutdown/exit 语义）、以及按方法→能力映射下沉到 Call 级别的能力门
// （methodCapability + manifest 声明快照，见 Call）。
//
// Launch 不注册订阅回调：Launch 返回与 SetNotifySink 之间的通知按有界有损
// 语义丢弃并计数（开口 4 的窗口在 Launch 自身签名上不可消除——Process 句柄
// 此刻才存在）。需要在注册窗口内零丢失的宿主走 LaunchWithSink。
func Launch(ctx context.Context, dir string, m Manifest, grant Grant, onExit func(error), trust func() error) (*Process, error) {
	return launch(ctx, dir, m, grant, onExit, trust, nil)
}

// LaunchWithSink 是 Launch 的 Launch 时刻订阅变体（第 18 轮治理开口 4）：
// sink 在宿主 readLoop/pump goroutine 启动之前注册——「Launch 返回 → 宿主
// SetNotifySink」之间的注册窗口从协议时序上整体移除，握手期/启动期到达的
// 插件通知不再有界有损。sink 仍可事后经 SetNotifySink 替换或置 nil 复位
// （无订阅者丢弃语义不变）。传 nil sink 等价 Launch。
func LaunchWithSink(ctx context.Context, dir string, m Manifest, grant Grant, onExit func(error), trust func() error, sink func(method string, params json.RawMessage)) (*Process, error) {
	return launch(ctx, dir, m, grant, onExit, trust, sink)
}

func launch(ctx context.Context, dir string, m Manifest, grant Grant, onExit func(error), trust func() error, sink func(method string, params json.RawMessage)) (*Process, error) {
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("plugin: launch 前置完整性校验: %w", err)
	}
	if trust == nil {
		return nil, fmt.Errorf("plugin: launch 缺少工作区信任断言（必经前置，nil 即拒）")
	}
	if err := trust(); err != nil {
		return nil, fmt.Errorf("plugin: launch 被工作区信任策略拒绝: %w", err)
	}
	for _, c := range m.Capabilities {
		if err := grant.Check(c); err != nil {
			return nil, err // 能力门在 spawn 之前生效（§O2）
		}
	}
	if dir == "" {
		return nil, fmt.Errorf("plugin: launch 缺少插件目录来源")
	}
	if err := safeEntrypoint(m.Entrypoint); err != nil {
		return nil, err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("plugin: 解析插件目录: %w", err)
	}
	exe := filepath.Join(absDir, m.Entrypoint)
	// 审计批 2 发现 3：复核与 exec 之间按路径重开是可复用的 TOCTOU 窗口
	// （mountPlugins 每会话重跑 Discover→Install→Launch）。改为打开-复核-
	// 执行同一句柄贯穿：Windows 上句柄以拒绝写/删共享模式打开并存续到
	// Start 返回，窗口内的写打开/删除/换名全部失败；Unix 无强制共享锁，
	// 残余窗口在 openEntryFile 处显式记录（收窄而非闭合）。注意 open 在
	// 哈希之前——被哈希的字节与被持有的句柄是同一对象。
	vf, err := openEntryFile(exe)
	if err != nil {
		return nil, fmt.Errorf("plugin: 打开 entrypoint: %w", err)
	}
	if err := verifyChecksumFromFile(ctx, vf, m.Checksum); err != nil {
		vf.Close()
		return nil, err
	}

	// 绝对路径启动：exec.Command 对含路径分隔符的名字不再走 LookPath，
	// PATH 解析（及其影子命中）从执行路径上整体移除。
	cmd := exec.CommandContext(ctx, exe)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		vf.Close()
		return nil, fmt.Errorf("plugin: stdin 管道: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		vf.Close()
		return nil, fmt.Errorf("plugin: stdout 管道: %w", err)
	}
	// 复证圈 5 sweep 发现 5：插件 stderr 不再直通宿主 stderr——子进程可向
	// 操作日志流写入任意字节（伪造 'omnilsp: …' 前缀行、凭据形态文本），
	// 宿主对其无前缀隔离、无有界性，日志可信边界在进程面上开放。改为经
	// pluginStderrLogger：每行带插件 id 前缀转发、行缓冲与转发总量有界
	// （原 ponytail 债「stderr 直通父进程；静默捕获留待完整协议」的折中
	// 交付——脱敏由宿主侧 stderr 统一脱敏口承接，plugin 包按其「仅标准库」
	// 依赖纪律不 import internal/security）。
	cmd.Stderr = newPluginStderrLogger(m.ID, os.Stderr)
	err = cmd.Start()
	vf.Close() // 复核→执行窗口在 Start 返回时闭合；此后句柄不再有用
	if err != nil {
		return nil, fmt.Errorf("plugin: 启动 %q: %w", m.Entrypoint, err)
	}

	p := &Process{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   bufio.NewReader(stdout),
		grant:    grant,
		pending:  make(map[int]chan response),
		caps:     make(map[Capability]bool, len(m.Capabilities)),
		OnExit:   onExit,
		notifyCh: make(chan pluginNotifyEvent, pluginNotifyBuffer),
		exited:   make(chan struct{}),
		// 第 18 轮开口 5：通知缓冲 §F14 配额（默认 nil 关，见 executor_budget.go）。
		notifyBudget: notifyBudgetFromEnv(),
	}
	// Launch 时刻订阅（开口 4）：sink 落在 readLoop/pump 启动之前——两个
	// goroutine 尚不存在，直接写字段不构成数据竞争；pump 随后经 sinkMu 读。
	if sink != nil {
		p.sink = sink
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
	go p.pumpNotifies()
	go func() {
		werr := cmd.Wait()
		close(p.exited) // 先于 OnExit：Exit 的有界等待不依赖回调时序
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

// MaxPluginFrameSize 是单帧负载上限：敌意/损坏的 Content-Length 声明不允许
// 让宿主按声明值无界分配内存（N14 防御纵深，覆盖 manifest 信任链之外的运行期）。
const MaxPluginFrameSize = 4 << 20 // 4 MiB

// maxHeaderLineBytes 是帧头块单行的硬上限：头行扫描逐行缓冲，敌意/损坏插件
// 的无换行长流不允许在宿主内无界累积内存（N14 运行期防御——4MiB 单帧上限只
// 约束 Content-Length 声明值，不约束头块扫描本身，见审计批 1 的 64MiB 堆增量
// 探针）。8 KiB 足以容纳任何规范头（Content-Type 等附加头）；骨架行分隔负载
// 一并限界——大负载必须走 Content-Length 分帧（上限 MaxPluginFrameSize）。
const maxHeaderLineBytes = 8 << 10 // 8 KiB

// readHeaderLine 以硬上限读一行（含行尾换行）：bufio.ReadSlice 在缓冲区填满
// 且无换行时返回 ErrBufferFull，逐段累积并在累计超限时拒绝——旧实现的
// ReadString('\n') 对无换行流无界累积。EOF/管道损坏与旧实现同样上抛，由
// readLoop 走致命错误路径唤醒全部等待者。
func readHeaderLine(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxHeaderLineBytes {
			return "", fmt.Errorf("plugin: 帧头行超出 %d 字节上限", maxHeaderLineBytes)
		}
		buf = append(buf, chunk...)
		switch err {
		case nil:
			return string(buf), nil
		case bufio.ErrBufferFull:
			continue
		default:
			return "", err
		}
	}
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	size := -1
	for {
		line, err := readHeaderLine(r)
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
		name, value, isHeader := strings.Cut(trimmed, ":")
		// 头名只可能由字母/连字符组成；JSON 负载行的首个冒号前必然带
		// 引号或花括号，据此区分「头块」与骨架的行分隔负载。
		if !isHeader || !isHeaderName(name) {
			if size >= 0 {
				return nil, fmt.Errorf("plugin: 帧头块中出现意外行 %q", trimmed)
			}
			return []byte(trimmed), nil
		}
		if strings.EqualFold(strings.TrimSpace(name), "content-length") {
			if size >= 0 {
				return nil, fmt.Errorf("plugin: 重复的 Content-Length 头")
			}
			// 严格解析（复证圈 2 审计发现 4）：旧实现 fmt.Sscanf("%d") 对
			// "12junk" 一类畸形值静默截断接受，与同函数其余 fail-fast 纪律
			// （重复 Content-Length、帧头块意外行均拒绝）不一致；strconv.ParseInt
			// 要求整串消耗，畸形值按帧结构损坏拒绝——与宿主侧 codec readHeader
			// 的 strconv 严格语义对齐。
			parsed, perr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if perr != nil {
				return nil, fmt.Errorf("plugin: malformed Content-Length header: %w", perr)
			}
			if parsed < 0 {
				return nil, fmt.Errorf("plugin: invalid Content-Length %d", parsed)
			}
			if parsed > MaxPluginFrameSize {
				return nil, fmt.Errorf("plugin: Content-Length %d 超出单帧上限 %d", parsed, MaxPluginFrameSize)
			}
			size = int(parsed)
			continue
		}
		// 其余头（如 Content-Type）按规范忽略——无论它出现在 Content-Length
		// 之前还是之后。旧实现把 Content-Length 之后的任何非空行判为 fatal，
		// 与自身的「附加头容忍」文档矛盾并击穿 LSP 常见的 Content-Length 在前
		// 头序（审计批 1）；放开仅限附加头，重复 Content-Length 与非头形态行
		// 仍按帧结构损坏拒绝。
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
// id 路由进 pending 表（多路复用的宿主半边）；无 id、带 method 的帧是
// 插件→宿主通知，解析后经有界通道投递宿主订阅回调（SetNotifySink）——
// 通道满或无订阅者即丢弃并计数，readLoop 永不因通知投递而阻塞。致命错误
// （EOF/管道损坏）才会中止循环并唤醒全部等待者；单帧负载损坏只丢弃该帧——
// 多路复用通道上其余并发请求必须照常获得响应，不能被一帧垃圾连坐。
func (p *Process) readLoop() {
	defer close(p.notifyCh) // 唯一发送者退出时关闭：pump 排空余量后结束
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
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(line, &frame); err != nil {
			// 单帧负载损坏：丢弃该帧，连接与其余并发请求不受牵连。
			continue
		}

		if frame.ID == nil {
			// 插件→宿主通知（无 id、带 method）：有界投递，满即丢弃计数。
			if frame.Method != "" {
				ev := pluginNotifyEvent{method: frame.Method, params: frame.Params}
				// §F14 通知缓冲配额（第 18 轮开口 5）：入队前先按驻留字节
				// Reserve——超配额整帧拒绝并计入 BudgetDroppedNotifies
				// （fail-closed 可见，不部分采纳，不牵连响应多路复用）。
				// Reserve 成功但入队失败（通道饱和）必须即时 Release，防止
				// 记账泄漏；入队成功方由 pumpNotifies 收货时 Release。
				if p.notifyBudget != nil {
					if err := p.notifyBudget.Reserve(notifyBudgetOwner, notifyEventBytes(ev)); err != nil {
						p.budgetDroppedNotifies.Add(1)
						continue
					}
				}
				select {
				case p.notifyCh <- ev:
				default:
					if p.notifyBudget != nil {
						p.notifyBudget.Release(notifyBudgetOwner, notifyEventBytes(ev))
					}
					p.droppedNotifies.Add(1)
				}
			}
			continue
		}

		p.mu.Lock()
		ch, ok := p.pending[*frame.ID]
		if ok {
			delete(p.pending, *frame.ID)
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

// SetNotifySink 注册插件→宿主通知的宿主半边（订阅回调）：readLoop 解析出
// 无 id、带 method 的通知帧后，经 pluginNotifyBuffer 容量的有界通道异步送达
// fn。并发安全——可在 Launch 前后任意时刻调用；注册窗口与通道饱和期到达的
// 通知按有界有损语义丢弃并计入 DroppedNotifies（丢弃可见，绝不静默）。
// 需要「Launch 返回前到达的通知零丢失」的宿主不走本方法，改用
// LaunchWithSink 在 readLoop/pump 启动前注册（第 18 轮开口 4）。
// 传 nil 恢复「无订阅者」默认态。
func (p *Process) SetNotifySink(fn func(method string, params json.RawMessage)) {
	p.sinkMu.Lock()
	p.sink = fn
	p.sinkMu.Unlock()
}

// DroppedNotifies 返回因无订阅者或通道饱和而被丢弃的插件通知数。
func (p *Process) DroppedNotifies() uint64 { return p.droppedNotifies.Load() }

// BudgetDroppedNotifies 返回因超出 §F14 通知缓冲配额而被整帧拒绝的通知数
// （第 18 轮开口 5）。未配置配额时恒为 0。
func (p *Process) BudgetDroppedNotifies() uint64 { return p.budgetDroppedNotifies.Load() }

// pumpNotifies 是通知投递的唯一消费者：从有界通道逐条取出，送达当前注册的
// 宿主回调。readLoop 退出（进程终止）时关闭通道，本循环排空余量后结束。
// 回调阻塞只拖慢 pump 自身——readLoop 的非阻塞投递永不等待这里。
func (p *Process) pumpNotifies() {
	for ev := range p.notifyCh {
		// §F14 记账闭环（第 18 轮开口 5）：入队时 Reserve 的驻留字节在收货
		// 时 Release——通道关闭后排空余量，每条入队通知恰好释放一次。
		if p.notifyBudget != nil {
			p.notifyBudget.Release(notifyBudgetOwner, notifyEventBytes(ev))
		}
		p.sinkMu.Lock()
		fn := p.sink
		p.sinkMu.Unlock()
		if fn == nil {
			p.droppedNotifies.Add(1)
			continue
		}
		fn(ev.method, ev.params)
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
//
// 登记纪律（审计批 3，§O2）：映射外放行是结构上的 fail-open（被
// TestCall_MethodCapabilityGate 的「映射外_无能力门」钉为协议管线的预期），
// 因此登记靠机制强制而非靠自觉——双向完备性守卫
// TestMethodCapabilityCompleteness 要求 AllCapabilities 的每一项能力要么是
// 本表某个条目的值（有方法触达面），要么显式列入 envOnlyCapabilities
// （环境型）。新增能力或方法触达面而漏登记，go test ./internal/plugin/
// 直接红灯，而不是零检查直发。
var methodCapability = map[string]Capability{
	"index/query":      CapIndexQuery,
	"index/write":      CapIndexWrite,
	"diagnostics/emit": CapDiagnosticsEmit,
	"code_action/emit": CapCodeActionEmit,
}

// envOnlyCapabilities 是「环境型」能力的显式花名册——methodCapability 的
// 完备性另一半（§O2「Default is deny」）：它们约束插件进程自身的运行行为
// （读文档、读工作区、联网、起进程、写文件系统），在插件协议 v1 中没有
// 宿主→插件的请求方法面，因此不经 Call 级门约束；其 deny 默认由授予半边
// 承担（Launch 前置门：声明 ⊆ grant）。向 AllCapabilities 新增能力时必须
// 二选一登记：methodCapability（有方法触达）或本表（环境型），两头都不沾
// 即由 TestMethodCapabilityCompleteness 在包级测试中拒绝。
var envOnlyCapabilities = []Capability{
	CapDocumentRead,
	CapWorkspaceRead,
	CapNetworkAccess,
	CapProcessExecute,
	CapFilesystemWrite,
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
		// 复证圈 1 审计发现 8：marshal 失败与写失败/超时两条错误路径对齐——
		// 旧实现此处直接 return 不删已注册的 pending 表项，每次失败永久占用
		// 一个自增 id 与一个 entry（直到 readLoop 终止的 notifyPendingLocked
		// 兜底才释放）。
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, fmt.Errorf("plugin: 序列化请求: %w", err)
	}

	// 有界写（复证圈 1 审计发现 4）：写阶段纳入超时覆盖，子进程停止读 stdin
	// 时 Call 以错误返回而非无界挂起。
	if err := p.writeBounded(req); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, fmt.Errorf("plugin: 写请求: %w", err)
	}

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
		// 条件迁移回滚（复证圈 1 审计发现 3）：仅当仍处于 lcShutdown 时回滚
		// lcActive。旧实现无条件写 lcActive——与并发 Exit 交错时把 Exit 已
		// 置位的 lcExited（可能已发出 plugin/exit 通知）覆写回 lcActive，状态
		// 机与已发线字节脱钩：后续 Exit 误报 ErrNonCompliantExit、lcExited 的
		// Call 拒绝门重开。终态 lcExited 一经设立不可回退。
		if p.lifecycle == lcShutdown {
			p.lifecycle = lcActive
		}
		p.mu.Unlock()
		return fmt.Errorf("plugin: shutdown 握手: %w", err)
	}
	return nil
}

// Kill 强制终止插件进程并等待收割（有界，复证圈 1 审计发现 7）：有界拆除的
// 兜底腿。Exit 的挂死处理（grace 超时 kill）覆盖 exit 腿；本方法供宿主拆除
// 路径（closePlugins 对 shutdown 握手失败的存活进程）显式终止——该路径下
// Shutdown 超时失败时旧实现只 continue 跳过 Exit（唯一带 kill 的腿），进程
// 与两根 stdio 管道泄漏为孤儿。进程死亡使 Wait goroutine 以非 nil 错误触发
// OnExit，§O4 崩溃记账照常闭合。已死进程上的 Kill 返回进程已结束类错误
// （无害，调用方记日志即可）。
func (p *Process) Kill() error {
	if err := p.cmd.Process.Kill(); err != nil {
		return err
	}
	select {
	case <-p.exited:
		return nil
	case <-time.After(exitGracePeriod):
		return fmt.Errorf("plugin: kill 后 %v 内进程未被收割", exitGracePeriod)
	}
}

// exitGracePeriod 是 Exit() 发出 plugin/exit 通知后等待插件自行退出的上限；
// 非常量——测试注入更短窗口（挂死插件的强制终止路径不能让守卫测试等 2s）。
var exitGracePeriod = 2 * time.Second

// Exit 是优雅停机的终点：发送 plugin/exit 通知，插件收到后自行退出
// （干净退出经 OnExit 以 nil 上报）。按 LSP 语义 exit 之前必须先 shutdown——
// lcActive 态直接 Exit 属于不合规退出，宿主拒绝迁移状态、不向管道写任何
// 字节，返回 ErrNonCompliantExit。lcExited 态重复 Exit 返回 ErrExited。
//
// 有界等待（审计批 2）：通知落地后至多等 exitGracePeriod——收到 exit 却挂死
// 的插件使 Wait goroutine 永不返回 → OnExit 永不触发 → RecordCrash 永不计入
// → Manager 永久停留 Running，进程与 stdio 管道泄漏。超时即强制终止（kill），
// Wait goroutine 随之以非 nil 错误触发 OnExit——§O4 崩溃记账对挂死类故障
// 照常闭合；Exit 本身返回说明性错误（调用方可观测），终态语义不变。
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
		// 复证圈 1 审计发现 4：写失败统一落入 kill 兜底——有界拆除不依赖写
		// 成功。EPIPE（进程已死）上 kill 是无害 no-op；写阻塞超时（插件停止
		// 读 stdin）上 kill 是唯一闭合路径：写超时后内部写 goroutine 仍持
		// writeMu 卡在 writeFrame，kill 使管道断裂才释放。
		_ = p.cmd.Process.Kill() // 竞速败给进程自行死亡时 Kill 报错，忽略
		return fmt.Errorf("plugin: exit 通知: %w", err)
	}
	timer := time.NewTimer(exitGracePeriod)
	defer timer.Stop()
	select {
	case <-p.exited:
		return nil
	case <-timer.C:
		_ = p.cmd.Process.Kill() // 竞速败给进程自行死亡时 Kill 报错，忽略
		return fmt.Errorf("plugin: exit 通知后 %v 内未退出，已强制终止", exitGracePeriod)
	}
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
	// 有界写（复证圈 1 审计发现 4）：旧实现无界写并持有 writeMu，Exit 的
	// grace timer 在 Notify 之后才布防——插件停止读 stdin 时 kill 界被整体
	// 绕过；写失败（含写超时）由调用方决定 kill 兜底。
	if err := p.writeBounded(notif); err != nil {
		return fmt.Errorf("plugin: 写通知: %w", err)
	}
	return nil
}

// pluginStderrBudgetBytes 是单插件进程 stderr 的转发字节预算：超预算后停
// 止转发并发一次抑制标记——插件不得用 stderr 洪泛宿主操作日志（有界性沿
// N14 防御纵深纪律）。
const pluginStderrBudgetBytes = 64 << 10

// pluginStderrMaxLineBytes 是转发行的缓冲上限：无换行流按该长度强制成行，
// 行缓冲不无界累积（与 maxHeaderLineBytes 同值不同责——那是帧头行扫描，
// 这是子进程日志行）。
const pluginStderrMaxLineBytes = 8 << 10

// pluginStderrLogger 把插件子进程的 stderr 以「omnilsp: plugin <id> stderr:
// 」前缀转发进宿主操作日志（复证圈 5 sweep 发现 5）：操作者可区分宿主行
// 与插件行，伪造宿主前缀的裸行不再以自身形态出现在日志流里。转发总量受
// pluginStderrBudgetBytes 约束——超预算发一次抑制标记（含丢弃字节数），
// 后续字节丢弃。单写者（子进程经 exec 拷贝 goroutine 串行到达），无需锁。
type pluginStderrLogger struct {
	id         string
	w          io.Writer
	buf        []byte
	forwarded  int
	suppressed bool
	dropped    int
}

func newPluginStderrLogger(id string, w io.Writer) *pluginStderrLogger {
	return &pluginStderrLogger{id: id, w: w}
}

// Write 消费整段输入并按行转发；返回值恒为 len(p)——转发预算只约束落到宿
// 主日志的量，不反压子进程（stderr 阻塞会拖死插件自身，不是宿主的故障面）。
func (l *pluginStderrLogger) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			l.buf = append(l.buf, p[:i]...)
			p = p[i+1:]
			l.flushLine()
			continue
		}
		l.buf = append(l.buf, p...)
		p = nil
		if len(l.buf) >= pluginStderrMaxLineBytes {
			l.flushLine() // 无换行长流强制成行，行缓冲有界
		}
	}
	return total, nil
}

func (l *pluginStderrLogger) flushLine() {
	line := strings.TrimRight(string(l.buf), "\r")
	l.buf = l.buf[:0]
	if line == "" {
		return
	}
	if l.suppressed {
		l.dropped += len(line)
		return
	}
	if l.forwarded+len(line) > pluginStderrBudgetBytes {
		l.suppressed = true
		fmt.Fprintf(l.w, "omnilsp: plugin %s stderr: 超过 %d 字节转发预算，后续 stderr 丢弃（当前丢弃 %d 字节）\n",
			l.id, pluginStderrBudgetBytes, l.dropped+len(line))
		return
	}
	l.forwarded += len(line)
	fmt.Fprintf(l.w, "omnilsp: plugin %s stderr: %s\n", l.id, line)
}
