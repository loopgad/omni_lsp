package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// MaxConsecutivePluginCrashes 是连续崩溃隔离阈值：达到即 Quarantined（§O4）。
const MaxConsecutivePluginCrashes = 5

// ErrQuarantined 表示插件已因崩溃循环被隔离，不再允许启用。
var ErrQuarantined = fmt.Errorf("plugin: 已隔离（崩溃循环）")

// ErrDuplicatePluginID 表示 manifest.ID 已在注册表——装配点对插件 ID 唯一
// （复证圈 2 审计发现 1，errors.Is 可判别）。
var ErrDuplicatePluginID = errors.New("plugin: 重复的 manifest.ID")

// State 枚举 §O4 全生命周期。
type State int

const (
	StateDiscovered State = iota
	StateVerified
	StateInstalled
	StateEnabled
	StateRunning
	StateFailed
	StateQuarantined
	StateRemoved
)

func (s State) String() string {
	switch s {
	case StateDiscovered:
		return "discovered"
	case StateVerified:
		return "verified"
	case StateInstalled:
		return "installed"
	case StateEnabled:
		return "enabled"
	case StateRunning:
		return "running"
	case StateFailed:
		return "failed"
	case StateQuarantined:
		return "quarantined"
	case StateRemoved:
		return "removed"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// Discovery 是一次磁盘发现的结果；VerifyErr 非 nil 表示该目录未通过信任链，
// 此时 Manifest 内容不可信，仅可用于诊断展示。
type Discovery struct {
	Dir       string
	Manifest  Manifest
	VerifyErr error
}

// Manager 维护插件生命周期状态机。
//
// 并发模型：全部公开方法经 mu 串行化；注册表以 manifest.ID 为键
// （仅 Verified 之后的元数据才入表，N14）。
//
// 工作区信任断言（§N1/N3 plugin.load）不属于 Manager：审计批 2 把它从
// Enable 上的可选回调（nil 即放行——生产赋值点只在装配期读过一次尚未建立
// 的策略）下沉为 Launch 的必经前置（executor.Launch 的 trust 参数，nil 即拒）。
// Enable 只做状态机与授权表。
type Manager struct {
	mu        sync.Mutex
	states    map[string]State
	manifests map[string]Manifest
	grants    map[string]Grant
	crashes   map[string]int
}

// NewManager 创建空管理器。
func NewManager() *Manager {
	return &Manager{
		states:    map[string]State{},
		manifests: map[string]Manifest{},
		grants:    map[string]Grant{},
		crashes:   map[string]int{},
	}
}

// Discover 扫描 root 的直接子目录，对每个含 plugin.json 的目录执行信任链。
// 单目录失败不中断整体，只记录进对应 Discovery.VerifyErr；root 本身不可读
// （不存在/无权限）作为目录级失败上抛——静默返回 nil 会把「不可用目录」伪装
// 成「无插件」（审计批 2：装配点对目录级失败没有任何可观测出口）。
func (m *Manager) Discover(root string) ([]Discovery, error) {
	return m.DiscoverContext(context.Background(), root)
}

// DiscoverContext 是带界变体（复证圈 2 审计发现 6）：逐目录信任链（含全文件
// sha256 复核）响应 ctx 取消——装配同步运行在 TCP accept 关键路径上且每个
// 会话重复，大插件目录的无界复核会让 accept 与首字节延迟无界（与 c7a1ef6
// 给 toolchain pinning 加界的同一审计逻辑）。ctx 耗尽作为目录级失败上抛，
// 由调用方落入 pluginDiscoverErr 可观测路径。
func (m *Manager) DiscoverContext(ctx context.Context, root string) ([]Discovery, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("plugin: 扫描插件根目录 %s: %w", root, err)
	}
	var out []Discovery
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, statErr := os.Stat(filepath.Join(dir, "plugin.json")); statErr != nil {
			continue
		}
		d := Discovery{Dir: dir}
		d.Manifest, d.VerifyErr = VerifyContext(ctx, dir)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// Install 将一个通过信任链的发现结果注册为 Verified；
// VerifyErr 非 nil 时拒绝注册（不可信元数据不入表）。
//
// manifest.ID 唯一性（复证圈 2 审计发现 1）：同 ID 的第二次 Install 拒绝并
// 返回 ErrDuplicatePluginID（errors.Is 可判别）——旧实现静默覆盖
// m.manifests[ID]，同 ID 的两个插件目录互相顶替，破坏有界拆除（宿主注册表
// 同键覆盖使先 Launch 的进程失去登记句柄）、崩溃隔离（RecordCrash 按 ID
// 连坐）与 emit 来源治理。
func (m *Manager) Install(d Discovery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.VerifyErr != nil {
		return fmt.Errorf("plugin: 未通过校验，拒绝安装: %w", d.VerifyErr)
	}
	if _, exists := m.manifests[d.Manifest.ID]; exists {
		return fmt.Errorf("plugin: %s 已注册（manifest.ID 必须唯一）: %w", d.Manifest.ID, ErrDuplicatePluginID)
	}
	m.manifests[d.Manifest.ID] = d.Manifest
	m.states[d.Manifest.ID] = StateVerified
	return nil
}

// GrantPolicy 是 EnableWithPolicy 的授予策略输入（审计批·注册变更越权覆盖
// 面）：对 manifest 声明的每一项能力做裁决，false 即该项不进入授予表——
// 授权表不再由 manifest 声明逐字构造。nil 等价旧语义（按声明授予），仅供
// 直接嵌入 Manager 的宿主使用；生产装配点必须经 DefaultGrantPolicy 建立
// 高权第二道门。
type GrantPolicy func(Capability) bool

// highPrivilegeCapabilities 是需第二道授予门的高权能力（审计批·注册变更越权
// 覆盖面）：这三项等价于在用户机器上执行任意进程/写文件系统/出网。旧的
// 「Enable 即按声明授予」使「进入 PluginsDir 且过 checksum」等同于自授它们
// ——必须由显式 opt-in（admin 策略/用户信任面，生产为 OMNILSP_PLUGIN_CAPS_ALLOW）
// 逐项授予，default deny 才对已声明项也有第二守门人。
var highPrivilegeCapabilities = []Capability{CapProcessExecute, CapFilesystemWrite, CapNetworkAccess}

// IsHighPrivilegeCapability 报告 c 是否属于高权集合。
func IsHighPrivilegeCapability(c Capability) bool {
	for _, hp := range highPrivilegeCapabilities {
		if c == hp {
			return true
		}
	}
	return false
}

// DefaultGrantPolicy 返回生产装配点的默认授予策略：低权能力按声明授予不变；
// 高权能力仅当出现在 allow（显式 opt-in 集合）时授予。allow 为 nil 或不含
// 该项即拒绝。
func DefaultGrantPolicy(allow map[Capability]bool) GrantPolicy {
	return func(c Capability) bool {
		if !IsHighPrivilegeCapability(c) {
			return true
		}
		return allow[c]
	}
}

// ApplyGrantPolicy 以与授予表同一策略过滤声明能力：生产装配点用它产出
// 「有效声明」传给 Launch，使 Launch 前置门（声明 ⊆ 授予）与授予表由同一
// 策略产出、按构造一致。policy 为 nil 时原样返回（旧语义）。
func ApplyGrantPolicy(caps []Capability, policy GrantPolicy) []Capability {
	if policy == nil {
		return caps
	}
	out := make([]Capability, 0, len(caps))
	for _, c := range caps {
		if policy(c) {
			out = append(out, c)
		}
	}
	return out
}

// Enable 允许 Verified/Failed → Enabled（Failed 是上次运行崩溃后的可重启态，
// 与 Disable 的 switch 保持同一覆盖面），并按 manifest.capabilities 建立授权表。
// 隔离态或连续崩溃达阈值时返回 ErrQuarantined。成功启用会清零连续崩溃计数。
//
// 无策略变体：按声明授予、无高权第二道门，仅供直接嵌入 Manager 的宿主；
// 生产装配点（runtime/server mountPlugins）必须走 EnableWithPolicy。
func (m *Manager) Enable(id string) (Grant, error) {
	return m.EnableWithPolicy(id, nil)
}

// EnableWithPolicy 在 Enable 的状态机约束之上叠加授予策略输入：声明能力逐项
// 过 policy，false 者不进入授予表（审计批·注册变更越权覆盖面）。policy 为
// nil 等价 Enable（按声明授予）。
func (m *Manager) EnableWithPolicy(id string, policy GrantPolicy) (Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states[id] == StateQuarantined || m.crashes[id] >= MaxConsecutivePluginCrashes {
		return Grant{}, ErrQuarantined
	}
	if m.states[id] != StateVerified && m.states[id] != StateFailed {
		return Grant{}, fmt.Errorf("plugin: %s 处于 %v，仅 verified/failed 可启用", id, m.states[id])
	}
	g := Grant{caps: make(map[Capability]bool)}
	for _, c := range ApplyGrantPolicy(m.manifests[id].Capabilities, policy) {
		g.caps[c] = true // 授予 = 声明 ∩ 策略白名单（仍受 Launch 前能力门约束）
	}
	m.grants[id] = g
	delete(m.crashes, id) // 全新启动，连续计数归零
	m.states[id] = StateEnabled
	return g, nil
}

// Disable 允许 Enabled/Running/Failed → Verified（回退到可再启用状态）。
func (m *Manager) Disable(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.states[id] {
	case StateEnabled, StateRunning, StateFailed:
		m.states[id] = StateVerified
		return nil
	default:
		return fmt.Errorf("plugin: %s 处于 %v，不可禁用", id, m.states[id])
	}
}

// MarkRunning 记录 Enabled → Running（Launch 成功后由宿主调用）。
func (m *Manager) MarkRunning(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states[id] != StateEnabled && m.states[id] != StateRunning {
		return fmt.Errorf("plugin: %s 处于 %v，不可标记为 running", id, m.states[id])
	}
	m.states[id] = StateRunning
	return nil
}

// RecordCrash 累计连续崩溃：未达阈值迁移 StateFailed（上次运行失败），
// 达 MaxConsecutivePluginCrashes 即转入 Quarantined，之后 Enable 一律返回
// ErrQuarantined（§O4 崩溃循环遏制）。未知 id 只计数，不经 RecordCrash 入表。
func (m *Manager) RecordCrash(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.crashes[id]++
	if _, known := m.states[id]; !known {
		return
	}
	if m.crashes[id] >= MaxConsecutivePluginCrashes {
		m.states[id] = StateQuarantined
		return
	}
	m.states[id] = StateFailed
}

// Remove 把插件迁入 StateRemoved 终态并清空授权与连续崩溃计数（§O4）。
// Running 中的插件宿主仍持有子进程，必须先 Disable；removed 后不可再启用；
// 重复 Remove 幂等；states 表以 Install 为唯一入表点，零值 StateDiscovered
// 即未知 id，报错而不落终态。
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.states[id] {
	case StateRemoved:
		return nil
	case StateRunning:
		return fmt.Errorf("plugin: %s 仍在运行，须先禁用再移除", id)
	case StateDiscovered:
		return fmt.Errorf("plugin: 未知插件 %q", id)
	}
	delete(m.grants, id)
	delete(m.crashes, id)
	m.states[id] = StateRemoved
	return nil
}

// State 返回插件当前生命周期状态；未知 id 返回错误。
func (m *Manager) State(id string) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.states[id]
	if !ok {
		return StateDiscovered, fmt.Errorf("plugin: 未知插件 %q", id)
	}
	return s, nil
}
