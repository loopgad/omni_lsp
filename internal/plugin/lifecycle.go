package plugin

import (
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
type Manager struct {
	mu        sync.Mutex
	states    map[string]State
	manifests map[string]Manifest
	grants    map[string]Grant
	crashes   map[string]int
	// TrustGate, when non-nil, is consulted before Enable (§N1/N3: plugin.load
	// is refused in Untrusted workspaces). Nil means allow.
	TrustGate func(id string) error
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
// 单目录失败不中断整体，只记录进对应 Discovery.VerifyErr。
func (m *Manager) Discover(root string) []Discovery {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Discovery
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, statErr := os.Stat(filepath.Join(dir, "plugin.json")); statErr != nil {
			continue
		}
		d := Discovery{Dir: dir}
		d.Manifest, d.VerifyErr = Verify(dir)
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

// Install 将一个通过信任链的发现结果注册为 Verified；
// VerifyErr 非 nil 时拒绝注册（不可信元数据不入表）。
func (m *Manager) Install(d Discovery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.VerifyErr != nil {
		return fmt.Errorf("plugin: 未通过校验，拒绝安装: %w", d.VerifyErr)
	}
	m.manifests[d.Manifest.ID] = d.Manifest
	m.states[d.Manifest.ID] = StateVerified
	return nil
}

// Enable 仅允许 Verified → Enabled，并按 manifest.capabilities 建立授权表。
// 隔离态或连续崩溃达阈值时返回 ErrQuarantined。成功启用会清零连续崩溃计数。
func (m *Manager) Enable(id string) (Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.states[id] == StateQuarantined || m.crashes[id] >= MaxConsecutivePluginCrashes {
		return Grant{}, ErrQuarantined
	}
	if m.states[id] != StateVerified {
		return Grant{}, fmt.Errorf("plugin: %s 处于 %v，仅 verified 可启用", id, m.states[id])
	}
	if m.TrustGate != nil {
		if err := m.TrustGate(id); err != nil {
			return Grant{}, fmt.Errorf("plugin: %s 被工作区信任策略拒绝: %w", id, err)
		}
	}
	caps := m.manifests[id].Capabilities
	g := Grant{caps: make(map[Capability]bool, len(caps))}
	for _, c := range caps {
		g.caps[c] = true // Enable 即按声明授予（仍受 Launch 前能力门约束）
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

// RecordCrash 累计连续崩溃；达 MaxConsecutivePluginCrashes 即转入 Quarantined，
// 之后 Enable 一律返回 ErrQuarantined（§O4 崩溃循环遏制）。
func (m *Manager) RecordCrash(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.crashes[id]++
	if m.crashes[id] >= MaxConsecutivePluginCrashes {
		m.states[id] = StateQuarantined
	}
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
