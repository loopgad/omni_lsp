package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// TestN14_HostileEntrypointRejected —— 路径穿越、绝对路径、缺失文件三类敌意样本全部拒绝。
func TestN14_HostileEntrypointRejected(t *testing.T) {
	for _, ep := range []string{"../evil.exe", "/abs/path", "ghost.exe"} {
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
// 真实子进程握手需要编译产物，此处跳过（骨架由单元门覆盖）。
func TestExecutor_LaunchHandshake(t *testing.T) {
	ctx := context.Background()
	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapNetworkAccess}

	t.Run("能力门_未授权不出进程", func(t *testing.T) {
		p, err := Launch(ctx, m, DefaultGrants(), nil)
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
		p, err := Launch(ctx, bad, NewGrant(CapNetworkAccess), nil)
		if err == nil || errors.Is(err, ErrCapabilityDenied) {
			t.Fatalf("完整性门应先行拒绝: %v", err)
		}
		if p != nil {
			t.Fatal("被拒后不应返回进程")
		}
	})

	t.Skip("executor e2e needs compiled entrypoint; skeleton covered by unit gates")
}
