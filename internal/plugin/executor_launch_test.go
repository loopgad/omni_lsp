package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLaunch_RejectsBeforeSpawn pins §O2: both pre-spawn gates fire without
// ever creating a child process — manifest integrity first, then the
// capability gate (default deny).
func TestLaunch_RejectsBeforeSpawn(t *testing.T) {
	// Gate 1: manifest integrity. The dir argument is unused here — Validate
	// refuses before the launch-time trust recheck ever reads it.
	bad := newValidManifest([]byte("#!/bin/demo\n"))
	bad.ID = ""
	if _, err := Launch(context.Background(), t.TempDir(), bad, NewGrant(AllCapabilities()...), nil, trustAllow); err == nil {
		t.Fatal("empty id must be rejected by Validate before spawn")
	}

	// Gate 2: capability denied — manifest asks for two, grant allows one.
	// Still pre-spawn, and ahead of the trust recheck (fixed gate order).
	m := newValidManifest([]byte("#!/bin/demo\n"))
	m.Entrypoint = "whatever-the-gate-rejects-first"
	m.Capabilities = []Capability{CapNetworkAccess, CapFilesystemWrite}
	_, err := Launch(context.Background(), t.TempDir(), m, NewGrant(CapNetworkAccess), nil, trustAllow)
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("err = %v, want ErrCapabilityDenied", err)
	}
}

// TestLaunch_ExecutesChecksummedEntrypoint 钉住 N14/§O3 的「校验体=执行体」：
// Launch 执行的必须是插件目录内通过 checksum 的那个文件——裸文件名绝不经
// PATH 解析（同名影子二进制不得顶替目录内已校验入口），且启动前须对目录内
// 文件重算 sha256 复核（Verify 之后被调包的二进制不得起飞）。
func TestLaunch_ExecutesChecksummedEntrypoint(t *testing.T) {
	ctx := context.Background()

	// 真插件目录：entrypoint 是测试二进制自身的副本（TestMain helper 模式，
	// 能说完整插件协议），checksum 按副本实算。
	dir := t.TempDir()
	ep := filepath.Base(os.Args[0])
	realBytes, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatalf("读取测试二进制: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ep), realBytes, 0o755); err != nil {
		t.Fatalf("写入目录内 entrypoint: %v", err)
	}
	m := newValidManifest(realBytes)
	m.Entrypoint = ep
	m.Capabilities = []Capability{CapIndexQuery}

	t.Run("PATH 影子不遮蔽目录内已校验入口", func(t *testing.T) {
		// 同名垃圾文件置于 PATH 最前：裸名经 PATH 解析时它必被先找到——
		// 若 Launch 以裸名 exec，起飞的就是这个影子而不是目录内入口。
		shadowDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(shadowDir, ep), []byte("definitely not an executable\n"), 0o755); err != nil {
			t.Fatalf("写入 PATH 影子: %v", err)
		}
		t.Setenv("PATH", shadowDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("OMNISP_PLUGIN_HELPER", "1")
		t.Setenv("GO_PLUGIN_MODE", "roundtrip")

		p, err := Launch(ctx, dir, m, NewGrant(CapIndexQuery), nil, trustAllow)
		if err != nil {
			t.Fatalf("Launch 应执行目录内已校验入口而非 PATH 影子: %v", err)
		}
		t.Cleanup(func() { _ = p.cmd.Process.Kill() })
		res, err := p.Call("index/query", map[string]any{"uri": "file:///shadow.go"})
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if !strings.Contains(string(res), "file:///shadow.go") {
			t.Fatalf("roundtrip 结果不符: %s", res)
		}
	})

	t.Run("Verify 之后换包必须被启动前复核拒绝", func(t *testing.T) {
		tampered := t.TempDir()
		if err := os.WriteFile(filepath.Join(tampered, ep), realBytes, 0o755); err != nil {
			t.Fatalf("写入 entrypoint: %v", err)
		}
		if err := os.WriteFile(filepath.Join(tampered, ep), []byte("tampered payload — not the verified binary\n"), 0o755); err != nil {
			t.Fatalf("调包 entrypoint: %v", err)
		}
		_, err := Launch(ctx, tampered, m, NewGrant(CapIndexQuery), nil, trustAllow)
		if !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("err = %v, want ErrChecksumMismatch（启动前 sha256 复核）", err)
		}
	})
}

// TestLaunch_TrustAssertionIsRequiredGate pins the audit batch 2 finding:
// the workspace trust assertion is a MANDATORY pre-spawn gate of Launch —
// no callback (nil) is a refusal, not an allow, and a refusing callback
// aborts before any child process exists. This replaces the old optional
// Manager.TrustGate (nil meant allow) whose only production assignment read
// the policy during New, before any trust policy could exist.
func TestLaunch_TrustAssertionIsRequiredGate(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "shutdown-exit")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	g := NewGrant(CapDiagnosticsEmit)

	// nil 信任断言 = 拒绝：必经前置不允许「无策略即放行」。
	if _, err := launchTestBinary(t, m, g, nil, nil); err == nil {
		t.Fatal("nil trust 回调必须被拒（必经前置，nil 即拒）")
	}

	// 断言拒绝 → 原样上抛，不产生子进程。
	denied := errors.New("workspace untrusted: plugin.load refused")
	if _, err := launchTestBinary(t, m, g, nil, func() error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("trust 拒绝必须原样上抛，got %v", err)
	}

	// 断言放行 → 正常启动并可用。
	p, err := launchTestBinary(t, m, g, nil, func() error { return nil })
	if err != nil {
		t.Fatalf("trust 放行后 Launch: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	if _, err := p.Call("probe/alive", nil); err != nil {
		t.Fatalf("放行后的插件应可用: %v", err)
	}
}

// TestLaunchChecksumBounded（复证圈 7 sweep 发现 4 的守卫）：Launch 时刻的
// entrypoint sha256 复核腿必须受 launchChecksumBudget 约束——唯一生产调用
// 点（server launchReadyPlugins）传 context.Background()，旧实现该腿无
// deadline 不可取消，entrypoint 被持续追加时 EOF-唯一退出循环永不结束，
// initialize 永久挂起。守卫以 1ns 预算注入：预算必须立即到期并以含预算语
// 义的错误可观测（「未完成」），Launch 不得成功。
func TestLaunchChecksumBounded(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	old := launchChecksumBudget
	launchChecksumBudget = 1 // 1ns：预算立即到期
	t.Cleanup(func() { launchChecksumBudget = old })

	m := newValidManifest([]byte("x"))
	p, err := launchTestBinary(t, m, NewGrant(CapDiagnosticsEmit), nil, trustAllow)
	if err == nil {
		_ = p.cmd.Process.Kill()
		t.Fatal("checksum 复核腿不受 launchChecksumBudget 约束：1ns 预算下 Launch 仍成功（该腿无界）")
	}
	if !strings.Contains(err.Error(), "未完成") {
		t.Fatalf("超时错误必须携带预算语义（含「未完成」）: %v", err)
	}
}
