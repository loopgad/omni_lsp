package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newValidManifest 返回一个可通过 Validate 的完整清单，checksum 对应 content 的 sha256。
func newValidManifest(content []byte) Manifest {
	sum := sha256.Sum256(content)
	return Manifest{
		ID:           "demo.plugin",
		Version:      "1.0.0",
		APIVersion:   SupportedAPIVersion,
		Entrypoint:   "demo.bin",
		Languages:    []string{"go"},
		Capabilities: []Capability{CapDiagnosticsEmit},
		Checksum:     "sha256:" + hex.EncodeToString(sum[:]),
	}
}

// newPluginDir 在临时目录写入合法插件（plugin.json + demo.bin），
// muts 用于在写入前篡改字段（构造敌意样本）。
func newPluginDir(t *testing.T, muts ...func(*Manifest)) string {
	t.Helper()
	dir := t.TempDir()
	content := []byte("#!/bin/demo\n")
	m := newValidManifest(content)
	for _, f := range muts {
		f(&m)
	}
	if err := os.WriteFile(filepath.Join(dir, "demo.bin"), content, 0o755); err != nil {
		t.Fatalf("写入 entrypoint: %v", err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化 manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), b, 0o644); err != nil {
		t.Fatalf("写入 plugin.json: %v", err)
	}
	return dir
}

// TestO3_ManifestValidation —— 七个必填字段缺一各拒一次；未知能力拒；
// apiVersion 不匹配拒；checksum 格式错拒；unsigned 允许但记录 warning。
func TestO3_ManifestValidation(t *testing.T) {
	base := newValidManifest([]byte("x"))
	cases := []struct {
		name string
		mut  func(*Manifest)
		want error // 非 nil 表示必须返回该哨兵错误；占位 errAny 表示任意错误
	}{
		{"缺id", func(m *Manifest) { m.ID = "" }, nil},
		{"缺version", func(m *Manifest) { m.Version = "" }, nil},
		{"缺apiVersion", func(m *Manifest) { m.APIVersion = "" }, nil},
		{"缺entrypoint", func(m *Manifest) { m.Entrypoint = "" }, nil},
		{"缺languages", func(m *Manifest) { m.Languages = nil }, nil},
		{"缺capabilities", func(m *Manifest) { m.Capabilities = nil }, nil},
		{"缺checksum", func(m *Manifest) { m.Checksum = "" }, nil},
		{"未知能力", func(m *Manifest) { m.Capabilities = []Capability{"teleport"} }, ErrUnknownCapability},
		{"apiVersion不匹配", func(m *Manifest) { m.APIVersion = "omnilsp.plugin.v2" }, nil},
		{"checksum格式错", func(m *Manifest) { m.Checksum = "sha256:xyz" }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			tc.mut(&m)
			err := m.Validate()
			if err == nil {
				t.Fatalf("期望被拒绝，实际通过")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("期望错误 %v，实际 %v", tc.want, err)
			}
		})
	}

	t.Run("unsigned记录warning", func(t *testing.T) {
		m := base
		m.Signature = "unsigned"
		if err := m.Validate(); err != nil {
			t.Fatalf("unsigned 不应报错: %v", err)
		}
		if m.Warning == "" {
			t.Fatal("unsigned 应记录 warning")
		}
	})
}

// TestLoad_TrailingBytesAfterManifestRejected（复证圈 1 sweep 发现 3）：
// DisallowUnknownFields 的「未知字段一律拒绝」只作用于第一个 JSON 文档——
// 旧实现 Decode 成功后不检查解码器是否还有余量，plugin.json 首个值之后的
// 尾随字节（第二个文档、垃圾字节）被静默吞掉，清单文件的完整字节不在校验
// 面内。修复后：尾随非空白内容按帧结构损坏拒绝；纯空白尾随仍合法（JSON
// 流分帧允许空白填充，dec.More() 对空白/EOF 返回 false）。
func TestLoad_TrailingBytesAfterManifestRejected(t *testing.T) {
	// 合法清单 + 尾随第二个文档：旧实现 err=nil 且尾随文档被吞。
	hostile := newPluginDir(t)
	manifest := filepath.Join(hostile, "plugin.json")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("读取 plugin.json: %v", err)
	}
	if err := os.WriteFile(manifest, append(raw, []byte("\n{\"id\":\"evil\"}")...), 0o644); err != nil {
		t.Fatalf("追加尾随文档: %v", err)
	}
	if _, err := Load(hostile); err == nil {
		t.Fatal("首个 JSON 值之后的尾随文档必须被拒绝（旧实现静默吞掉）")
	}

	// 尾随垃圾字节同样拒绝（同一对账点）。
	junk := newPluginDir(t)
	manifest = filepath.Join(junk, "plugin.json")
	raw, err = os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("读取 plugin.json: %v", err)
	}
	if err := os.WriteFile(manifest, append(raw, []byte("\x00trailing-junk")...), 0o644); err != nil {
		t.Fatalf("追加尾随垃圾: %v", err)
	}
	if _, err := Load(junk); err == nil {
		t.Fatal("首个 JSON 值之后的垃圾字节必须被拒绝")
	}

	// 纯空白尾随仍合法：JSON 流分帧允许空白填充，不得误伤。
	padded := newPluginDir(t)
	manifest = filepath.Join(padded, "plugin.json")
	raw, err = os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("读取 plugin.json: %v", err)
	}
	if err := os.WriteFile(manifest, append(raw, []byte("\n\n  \n")...), 0o644); err != nil {
		t.Fatalf("追加空白: %v", err)
	}
	m, err := Load(padded)
	if err != nil {
		t.Fatalf("纯空白尾随不得被拒绝: %v", err)
	}
	if m.ID != "demo.plugin" {
		t.Fatalf("ID = %q, want demo.plugin（空白尾随下清单字段不受影响）", m.ID)
	}
}

// TestN14_ChecksumMismatchRejected —— entrypoint 实际内容与声明 sha256 不符必须拒绝；
// 内容一致时校验链放行。
func TestN14_ChecksumMismatchRejected(t *testing.T) {
	dir := newPluginDir(t, func(m *Manifest) {
		m.Checksum = "sha256:" + hex.EncodeToString(make([]byte, sha256.Size)) // 全零哈希，必然不匹配
	})
	if _, err := Verify(dir); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("期望 ErrChecksumMismatch，实际 %v", err)
	}

	okDir := newPluginDir(t)
	m, err := Verify(okDir)
	if err != nil {
		t.Fatalf("合法插件应通过校验链: %v", err)
	}
	if m.ID != "demo.plugin" {
		t.Fatalf("ID = %q", m.ID)
	}
}

// TestN14_HostileEntrypointRejected —— 路径穿越、绝对路径、裸点、缺失文件
// 四类敌意样本全部拒绝（复证圈 3 sweep 发现 1 补 "."：Verify 级的端到端
// 拒绝此前只靠 openEntryFile 打不开目录偶然闭合，形态门现显式拒绝）。
func TestN14_HostileEntrypointRejected(t *testing.T) {
	for _, ep := range []string{"../evil.exe", "/abs/path", ".", "ghost.exe"} {
		t.Run(ep, func(t *testing.T) {
			dir := newPluginDir(t, func(m *Manifest) { m.Entrypoint = ep })
			if _, err := Verify(dir); err == nil {
				t.Fatal("敌意 entrypoint 必须被拒绝")
			}
		})
	}
}

// TestO2_DefaultDeny —— 默认全拒绝；显式授权后放行且未授权项仍拒绝。
func TestO2_DefaultDeny(t *testing.T) {
	g := DefaultGrants()
	for _, c := range AllCapabilities() {
		if err := g.Check(c); !errors.Is(err, ErrCapabilityDenied) {
			t.Fatalf("默认授权下 %q 应拒绝，实际 %v", c, err)
		}
	}

	g2 := NewGrant(CapIndexQuery, CapDiagnosticsEmit)
	if err := g2.Check(CapIndexQuery); err != nil {
		t.Fatalf("显式授权后 index.query 应放行: %v", err)
	}
	if err := g2.Check(CapDiagnosticsEmit); err != nil {
		t.Fatalf("显式授权后 diagnostics.emit 应放行: %v", err)
	}
	if err := g2.Check(CapNetworkAccess); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("未授权的 network.access 应拒绝，实际 %v", err)
	}
}

// TestO4_QuarantineAfterCrashLoop —— 连续崩溃达到阈值后进入隔离态，
// 隔离态下 Enable 返回 ErrQuarantined；未达阈值可正常启用。
func TestO4_QuarantineAfterCrashLoop(t *testing.T) {
	id := "demo.plugin"

	// 边界：4 次崩溃（< 5）仍可启用。
	mgr := NewManager()
	if err := mgr.Install(Discovery{Dir: "d", Manifest: newValidManifest([]byte("x"))}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	for i := 0; i < MaxConsecutivePluginCrashes-1; i++ {
		mgr.RecordCrash(id)
	}
	if _, err := mgr.Enable(id); err != nil {
		t.Fatalf("未达崩溃阈值应可启用: %v", err)
	}
	if err := mgr.Disable(id); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	// 达到阈值：第 5 次崩溃后进入 Quarantined，Enable 被拒。
	for i := 0; i < MaxConsecutivePluginCrashes; i++ {
		mgr.RecordCrash(id)
	}
	st, err := mgr.State(id)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st != StateQuarantined {
		t.Fatalf("期望 Quarantined，实际 %v", st)
	}
	if _, err := mgr.Enable(id); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("期望 ErrQuarantined，实际 %v", err)
	}
}

// TestExecutor_LaunchHandshake —— 能力门在进程启动前生效：声明的能力未被授予时
// 返回 ErrCapabilityDenied 且不产生子进程；manifest 完整性同样前置校验。
// e2e 以测试二进制自身作为插件入口（TestMain helper 模式，零额外编译产物）
// 实跑完整环路：前置门 → spawn → 握手 → 请求-响应 → 宿主→插件通知广播。
func TestExecutor_LaunchHandshake(t *testing.T) {
	ctx := context.Background()
	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapNetworkAccess}

	t.Run("能力门_未授权不出进程", func(t *testing.T) {
		p, err := Launch(ctx, t.TempDir(), m, DefaultGrants(), nil, trustAllow)
		if !errors.Is(err, ErrCapabilityDenied) {
			t.Fatalf("期望 ErrCapabilityDenied，实际 %v", err)
		}
		if p != nil {
			t.Fatal("被拒后不应返回进程")
		}
	})

	t.Run("完整性门_api版本不匹配不出进程", func(t *testing.T) {
		bad := m
		bad.APIVersion = "omnilsp.plugin.v2"
		p, err := Launch(ctx, t.TempDir(), bad, NewGrant(CapNetworkAccess), nil, trustAllow)
		if err == nil || errors.Is(err, ErrCapabilityDenied) {
			t.Fatalf("完整性门应先行拒绝: %v", err)
		}
		if p != nil {
			t.Fatal("被拒后不应返回进程")
		}
	})

	t.Run("e2e_真实子进程_握手_调用_广播", func(t *testing.T) {
		t.Setenv("OMNISP_PLUGIN_HELPER", "1")
		t.Setenv("GO_PLUGIN_MODE", "collect-notify")

		em := newValidManifest([]byte("x"))
		em.Capabilities = []Capability{CapDiagnosticsEmit}
		p := spawnHelper(t, "collect-notify", em, NewGrant(em.Capabilities...))

		// 请求-响应：握手后插件应答，params 原样往返。
		res, err := p.Call("diagnostics/emit", map[string]any{"uri": "file:///e2e.go", "line": 9})
		if err != nil {
			t.Fatalf("e2e Call: %v", err)
		}
		var pong struct {
			Pong struct {
				URI  string `json:"uri"`
				Line int    `json:"line"`
			} `json:"pong"`
		}
		if err := json.Unmarshal(res, &pong); err != nil {
			t.Fatalf("e2e result unparsable: %v (%s)", err, res)
		}
		if pong.Pong.URI != "file:///e2e.go" || pong.Pong.Line != 9 {
			t.Fatalf("e2e params mangled: %+v", pong.Pong)
		}

		// 广播：宿主→插件通知被插件逐帧接收，audit 回传核对。
		if err := p.Notify("plugin/documentDidOpen", map[string]any{"uri": "file:///e2e.go"}); err != nil {
			t.Fatalf("e2e Notify: %v", err)
		}
		audit, err := p.Call("broadcast/audit", nil)
		if err != nil {
			t.Fatalf("e2e audit: %v", err)
		}
		var got struct {
			Notifications []struct {
				Method string `json:"method"`
				Params struct {
					URI string `json:"uri"`
				} `json:"params"`
			} `json:"notifications"`
		}
		if err := json.Unmarshal(audit, &got); err != nil {
			t.Fatalf("e2e audit unparsable: %v (%s)", err, audit)
		}
		if len(got.Notifications) != 1 ||
			got.Notifications[0].Method != "plugin/documentDidOpen" ||
			got.Notifications[0].Params.URI != "file:///e2e.go" {
			t.Fatalf("e2e broadcast mismatch: %s", audit)
		}
	})
}

// TestO2_GrantPolicyGatesHighPrivilege（审计批·注册变更越权覆盖面）：
// EnableWithPolicy 是 Enable 的授予策略输入——声明能力先过策略白名单再入
// Grant，授权表不再由 manifest 声明逐字构造。生产装配点默认策略
// （DefaultGrantPolicy）把三项高权能力（process.execute/filesystem.write/
// network.access）降级为需显式 opt-in：「进入 PluginsDir 且过 checksum」
// 不再等价于自授全部声明能力。
func TestO2_GrantPolicyGatesHighPrivilege(t *testing.T) {
	id := "demo.plugin"
	newMgr := func(t *testing.T) *Manager {
		t.Helper()
		m := newValidManifest([]byte("x"))
		m.Capabilities = []Capability{CapDiagnosticsEmit, CapProcessExecute, CapFilesystemWrite, CapNetworkAccess}
		mgr := NewManager()
		if err := mgr.Install(Discovery{Dir: "d", Manifest: m}); err != nil {
			t.Fatalf("Install: %v", err)
		}
		return mgr
	}

	// 高权集合恰为审计点名的三项。
	for _, c := range AllCapabilities() {
		want := c == CapProcessExecute || c == CapFilesystemWrite || c == CapNetworkAccess
		if got := IsHighPrivilegeCapability(c); got != want {
			t.Errorf("IsHighPrivilegeCapability(%s) = %v, want %v", c, got, want)
		}
	}

	// 默认策略（无 opt-in）：高权能力全部剔除，低权照常授予。
	g, err := newMgr(t).EnableWithPolicy(id, DefaultGrantPolicy(nil))
	if err != nil {
		t.Fatalf("EnableWithPolicy: %v", err)
	}
	for _, c := range []Capability{CapProcessExecute, CapFilesystemWrite, CapNetworkAccess} {
		if err := g.Check(c); !errors.Is(err, ErrCapabilityDenied) {
			t.Errorf("无 opt-in 时高权 %q 应拒绝（默认第二道门），实际 %v", c, err)
		}
	}
	if err := g.Check(CapDiagnosticsEmit); err != nil {
		t.Errorf("低权 diagnostics.emit 应照常授予: %v", err)
	}

	// 显式 opt-in：仅列名者回到授予表，未列名的高权仍剔除。
	g2, err := newMgr(t).EnableWithPolicy(id, DefaultGrantPolicy(map[Capability]bool{CapProcessExecute: true}))
	if err != nil {
		t.Fatalf("EnableWithPolicy(opt-in): %v", err)
	}
	if err := g2.Check(CapProcessExecute); err != nil {
		t.Errorf("opt-in 后 process.execute 应授予: %v", err)
	}
	if err := g2.Check(CapFilesystemWrite); !errors.Is(err, ErrCapabilityDenied) {
		t.Errorf("未列入 opt-in 的 filesystem.write 应拒绝，实际 %v", err)
	}

	// ApplyGrantPolicy 与授予同源过滤声明：生产装配点用它产出「有效声明」
	// 传给 Launch，使 Launch 前置门（声明 ⊆ 授予）按构造成立。
	eff := ApplyGrantPolicy([]Capability{CapDiagnosticsEmit, CapProcessExecute, CapFilesystemWrite, CapNetworkAccess},
		DefaultGrantPolicy(map[Capability]bool{CapNetworkAccess: true}))
	if len(eff) != 2 || eff[0] != CapDiagnosticsEmit || eff[1] != CapNetworkAccess {
		t.Errorf("ApplyGrantPolicy 有效声明 = %v, want [diagnostics.emit network.access]", eff)
	}

	// nil 策略 = Enable 旧语义（直接嵌入 Manager 的宿主按声明授予；
	// 生产装配点必须走 DefaultGrantPolicy）。
	g3, err := newMgr(t).EnableWithPolicy(id, nil)
	if err != nil {
		t.Fatalf("EnableWithPolicy(nil): %v", err)
	}
	for _, c := range []Capability{CapDiagnosticsEmit, CapProcessExecute, CapFilesystemWrite, CapNetworkAccess} {
		if err := g3.Check(c); err != nil {
			t.Errorf("nil 策略下 %q 应按声明授予: %v", c, err)
		}
	}
}

// TestSafeEntrypoint_BareDotRejected（复证圈 3 sweep 发现 1）：裸点
// entrypoint="." 通过旧四项形态检查（Base(".")==". "、无 ".."、非绝对、
// 无冒号），形态门与下游拒绝之间的耦合是隐式的——Verify/Launch 只因
// openEntryFile 打不开目录而偶然 fail-closed（Windows Access denied /
// Unix EISDIR）。修复后 safeEntrypoint 显式拒绝，形态门自持，不再依赖
// 下游文件语义。 "./" 形态今天已被 Base 不等检查拒绝，一并钉住。
func TestSafeEntrypoint_BareDotRejected(t *testing.T) {
	for _, ep := range []string{".", "./"} {
		if err := safeEntrypoint(ep); err == nil {
			t.Fatalf("safeEntrypoint(%q) 必须显式拒绝（旧实现放行，靠下游打开目录失败偶然闭合）", ep)
		}
	}
	// 合法裸文件名不回退。
	if err := safeEntrypoint("demo.bin"); err != nil {
		t.Fatalf("合法裸文件名不得被拒: %v", err)
	}
}

// TestLoad_OversizedManifestRejected（复证圈 5 sweep 发现 6）：plugin.json
// 是插件车道唯一读取前无大小上限的协议输入面（生产链 server.New 每会话装
// 配 → DiscoverContext → VerifyContext → Load），非敌意损坏（大二进制误拷
// 成 manifest）或本地低权写者可制造按文件大小的全量内存尖峰。运行期帧界
// 纪律（MaxPluginFrameSize 4MiB、帧头 8KiB、codec 64MiB 分配前拒绝）在
// manifest 车道补齐对应物：1 MiB 上限，超限按帧结构损坏类错误拒绝。
// 守卫用「合法 JSON 但超大」构造——parse 失败类拒绝无法区分大小门。
func TestLoad_OversizedManifestRejected(t *testing.T) {
	dir := t.TempDir()
	m := newValidManifest([]byte("x"))
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	giant := strings.Replace(string(raw),
		`"version":"1.0.0"`, `"version":"`+strings.Repeat("v", 2<<20)+`"`, 1)
	if len(giant) <= maxManifestBytes {
		t.Fatalf("守卫样本未超上限：%d 字节", len(giant))
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(giant), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Load(dir)
	if err == nil {
		t.Fatal("超限 plugin.json 必须被大小门拒绝（旧实现按文件大小全量读入）")
	}
	if !strings.Contains(err.Error(), "超出") {
		t.Fatalf("拒绝原因必须是大小门而非解析错误: %v", err)
	}
	// 常规 manifest 不受影响。
	okDir := newPluginDir(t)
	if _, err := Load(okDir); err != nil {
		t.Fatalf("常规 manifest 被误拒: %v", err)
	}
}

// TestReadManifestBounded_HardCap（复证圈 7 sweep 发现 3 的机制钉）：读面
// 本身必须硬上界——无论底层来源尺寸声明如何（Stat 之后被并发增长正是
// check-then-act 的缺口形态），readManifestBounded 读入超过 maxManifestBytes
// 即拒绝。Load 的句柄 Stat 门是第一条带，本函数的读后复核是第二条带；变
// 异敏感性：移除 len 复核此测试即红。
func TestReadManifestBounded_HardCap(t *testing.T) {
	over := strings.NewReader(strings.Repeat(" ", maxManifestBytes+1))
	if b, err := readManifestBounded(over); err == nil {
		t.Fatalf("读入 %d 字节未被拒绝（读面无界）", len(b))
	} else if !strings.Contains(err.Error(), "超出") {
		t.Fatalf("拒绝原因必须是大小门: %v", err)
	}
	// 恰在上限内的输入照常通过。
	exact := strings.NewReader(strings.Repeat(" ", maxManifestBytes))
	b, err := readManifestBounded(exact)
	if err != nil {
		t.Fatalf("恰在上限内的输入被误拒: %v", err)
	}
	if len(b) != maxManifestBytes {
		t.Fatalf("读入 %d 字节, want %d", len(b), maxManifestBytes)
	}
}
