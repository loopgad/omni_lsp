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
	if _, err := Launch(context.Background(), t.TempDir(), bad, NewGrant(AllCapabilities()...), nil); err == nil {
		t.Fatal("empty id must be rejected by Validate before spawn")
	}

	// Gate 2: capability denied — manifest asks for two, grant allows one.
	// Still pre-spawn, and ahead of the trust recheck (fixed gate order).
	m := newValidManifest([]byte("#!/bin/demo\n"))
	m.Entrypoint = "whatever-the-gate-rejects-first"
	m.Capabilities = []Capability{CapNetworkAccess, CapFilesystemWrite}
	_, err := Launch(context.Background(), t.TempDir(), m, NewGrant(CapNetworkAccess), nil)
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

		p, err := Launch(ctx, dir, m, NewGrant(CapIndexQuery), nil)
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
		_, err := Launch(ctx, tampered, m, NewGrant(CapIndexQuery), nil)
		if !errors.Is(err, ErrChecksumMismatch) {
			t.Fatalf("err = %v, want ErrChecksumMismatch（启动前 sha256 复核）", err)
		}
	})
}
