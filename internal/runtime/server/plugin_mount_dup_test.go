package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/plugin"
)

// 复证圈 2 审计发现 1（server 半边）的守卫：mountPlugins 对重复 manifest.ID
// 的发现结果必须可见拒绝——同 ID 的两个插件目录不得互相顶替（Install 侧的
// ErrDuplicatePluginID 由 plugin 包守卫钉住；本测试钉装配侧的可见拒绝）。
func TestPluginsMount_DuplicateManifestIDAcceptedOnlyOnce(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")
	t.Setenv("OMNILSP_TRUST", "trusted")

	root := t.TempDir()
	_, manifest := writePluginDir(t, root, "dup", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)
	// 第二个目录、同一 manifest.ID：手工落一份同 ID 的 plugin.json（entrypoint
	// 相对各自目录，二进制各自复制）。
	aliasDir := filepath.Join(root, "dup-alias")
	if err := os.MkdirAll(aliasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(filepath.Join(root, "dup", filepath.Base(os.Args[0])), filepath.Join(aliasDir, filepath.Base(os.Args[0]))); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(aliasDir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()

	// 白盒：重复 ID 只允许一条进入待 Launch 队列（旧实现两条都 Install
	// 成功——同 ID 互相顶替，先 Launch 的进程失去登记句柄）。
	s.pluginMu.Lock()
	pending := len(s.pluginPending)
	s.pluginMu.Unlock()
	if pending != 1 {
		t.Fatalf("重复 manifest.ID 的装配结果 pending=%d, want 1（同 ID 两目录互相顶替）", pending)
	}
}

// 复证圈 2 审计发现 6 的守卫：装配在 TCP accept 关键路径上同步执行且每个
// 会话重复——Discover+全量 sha256 信任链必须有界，超时落入 pluginDiscoverErr
// 可观测路径（与目录级失败同一约定），而不是让 accept 与首字节延迟无界。
// 与 c7a1ef6 给 toolchain pinning 加界的同一审计逻辑。
func TestPluginsMount_DiscoveryIsBoundedByMountBudget(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")
	t.Setenv("OMNILSP_TRUST", "trusted")

	root := t.TempDir()
	writePluginDir(t, root, "bounded", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	old := pluginMountBudget
	pluginMountBudget = -time.Second // 负预算：deadline 已然过期，ctx.Err 确定性返回
	t.Cleanup(func() { pluginMountBudget = old })

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()

	s.pluginMu.Lock()
	derr := s.pluginDiscoverErr
	s.pluginMu.Unlock()
	if derr == "" {
		t.Fatal("装配预算耗尽必须落入 pluginDiscoverErr 可观测路径（旧实现无界且无出口）")
	}
	if proc := mountedProc(t, s, "bounded"); proc != nil {
		t.Fatal("装配超时后不得有插件进程")
	}
}
