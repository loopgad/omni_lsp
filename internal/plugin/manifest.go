// Concurrency model: Manager serializes lifecycle transitions with mu;
// capability Grant tables are immutable after Enable.
//
// Invariants:
//  1. Default deny: a capability not explicitly granted is refused (O2).
//  2. Unverified metadata is untrusted: checksum/API-version gates run before
//     any entrypoint executes (N14).
//  3. Crash loops quarantine: MaxConsecutivePluginCrashes failures disable
//     re-enable until operator action (O4).
//
// Package plugin 实现进程外插件框架（goal.md §O0-O5、§N14，Tier 1）。
//
// 职责：manifest 校验、信任链（checksum/路径安全）、能力授予表（默认 deny）、
// 生命周期状态机（含崩溃循环隔离）、进程外执行骨架。
//
// 拥有的可变状态：Manager 内的 states/manifests/crashes/grants 四张表；
// Process 内的单飞互斥锁与 OnExit 回调。
//
// 并发模型：Manager 所有公开方法经 mu 串行化；Process.Call 单飞锁串行化
// 读写对；OnExit 由唯一的 Wait goroutine 在退出时调用一次。
//
// 不变量：
//   - 未通过 Verify 的元数据不可信（N14），不进入 Manager 注册表；
//   - 能力默认 deny，任何未声明即拒绝；
//   - 连续崩溃达 MaxConsecutivePluginCrashes 必进入 Quarantined 且不可再启用；
//   - 能力门在子进程启动之前生效。
//
// 允许的依赖：仅标准库；禁止 import internal/protocol/*、internal/languages/*、
// internal/runtime/server（依赖方向由 §U1/§O5 约束）。
//
// 失败行为：所有校验失败返回哨兵错误（errors.Is 可判别），绝不 panic；
// Discover 对单个目录的失败仅记录到 Discovery.VerifyErr，不影响其余目录。
//
// 主要测试：plugin_test.go 中 TestO3/TestN14/TestO2/TestO4/TestExecutor 系列
// （go test -race ./internal/plugin/）。
package plugin

import (
	"fmt"
	"regexp"
)

// SupportedAPIVersion 是宿主当前首选的插件 API 版本（§O5）。
const SupportedAPIVersion = "omnilsp.plugin.v1"

// SupportedAPIVersions 是宿主愿意协商的版本区间（§O5 range negotiation）：
// 插件声明其中任意一个即可加载；宿主按自身能力以列表内版本响应。
// 未来引入 v2 时，宿主在过渡期同时列出 v1/v2，弃用期满后移除。
var SupportedAPIVersions = []string{"omnilsp.plugin.v1"}

// APIVersionSupported 报告 manifest 声明的 apiVersion 是否落在支持区间内。
func APIVersionSupported(v string) bool {
	for _, ok := range SupportedAPIVersions {
		if v == ok {
			return true
		}
	}
	return false
}

// Capability 是九项能力之一（§O2），以字符串标识便于 manifest 直接映射。
type Capability string

// 九能力白名单。
const (
	CapDocumentRead    Capability = "document.read"
	CapWorkspaceRead   Capability = "workspace.read"
	CapIndexQuery      Capability = "index.query"
	CapDiagnosticsEmit Capability = "diagnostics.emit"
	CapCodeActionEmit  Capability = "code_action.emit"
	CapNetworkAccess   Capability = "network.access"
	CapProcessExecute  Capability = "process.execute"
	CapFilesystemWrite Capability = "filesystem.write"
	CapIndexWrite      Capability = "index.write"
)

// AllCapabilities 返回九能力白名单（稳定顺序）。
func AllCapabilities() []Capability {
	return []Capability{
		CapDocumentRead, CapWorkspaceRead, CapIndexQuery,
		CapDiagnosticsEmit, CapCodeActionEmit, CapNetworkAccess,
		CapProcessExecute, CapFilesystemWrite, CapIndexWrite,
	}
}

// ErrUnknownCapability 表示 manifest 声明了白名单之外的能力。
var ErrUnknownCapability = fmt.Errorf("plugin: 未知能力")

var checksumRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Manifest 对应 §O3 的七必填字段；Signature 为可选信任字段，
// 缺省或 "unsigned" 时 Validate 记录 Warning 而不报错（本地开发档）。
type Manifest struct {
	ID           string       `json:"id"`
	Version      string       `json:"version"`
	APIVersion   string       `json:"apiVersion"`
	Entrypoint   string       `json:"entrypoint"`
	Languages    []string     `json:"languages"`
	Capabilities []Capability `json:"capabilities"`
	Checksum     string       `json:"checksum"`
	Signature    string       `json:"signature,omitempty"`

	// Warning 由 Validate 填写，承载非致命告警（如 unsigned），不参与序列化。
	Warning string `json:"-"`
}

// Validate 按 §O3/§N14 校验清单：必填字段非空、API 版本精确匹配、
// 能力全部在白名单内、checksum 格式合法。校验失败返回可 errors.Is 判别的错误。
func (m *Manifest) Validate() error {
	for _, f := range []struct {
		name string
		val  string
	}{
		{"id", m.ID},
		{"version", m.Version},
		{"apiVersion", m.APIVersion},
		{"entrypoint", m.Entrypoint},
	} {
		if f.val == "" {
			return fmt.Errorf("plugin: manifest.%s 为空", f.name)
		}
	}
	if len(m.Languages) == 0 {
		return fmt.Errorf("plugin: manifest.languages 为空")
	}
	if len(m.Capabilities) == 0 {
		return fmt.Errorf("plugin: manifest.capabilities 为空")
	}
	if !APIVersionSupported(m.APIVersion) {
		return fmt.Errorf("plugin: apiVersion %q 与宿主支持集合 %v 不兼容", m.APIVersion, SupportedAPIVersions)
	}
	white := map[Capability]bool{}
	for _, c := range AllCapabilities() {
		white[c] = true
	}
	for _, c := range m.Capabilities {
		if !white[c] {
			return fmt.Errorf("%w: %q", ErrUnknownCapability, c)
		}
	}
	if !checksumRe.MatchString(m.Checksum) {
		return fmt.Errorf("plugin: checksum 格式非法（期望 sha256:<64位十六进制>）")
	}
	if m.Signature == "" || m.Signature == "unsigned" {
		m.Warning = "signature 未签名（本地开发档）；发布前必须签名"
	}
	return nil
}
