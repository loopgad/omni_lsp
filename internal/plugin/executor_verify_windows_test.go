//go:build windows

package plugin

// 审计批 2 发现 3 守卫（Windows 半边）：Launch 的启动前 sha256 复核与
// exec.Start 之间存在残余 TOCTOU 窗口——对插件目录有写权限的本地进程可在
// 哈希完成到 Start 重开之间换入未校验二进制，且 mountPlugins 每会话重跑
// （Discover→Install→Launch），窗口可重复。修复形态：复核句柄自持到
// Start 返回，且以拒绝写/删的共享模式打开（FILE_SHARE_READ）——窗口内任何
// 写打开、删除或换名（调包的标准步骤）都因共享冲突失败，被复核的字节与
// CreateProcess 执行的字节由此钉在同一文件上。
//
// 红阶段：openEntryFile 未定义（编译红）；旧实现 os.Open 以全共享模式打开
// 且复核后即关句柄，写打开在句柄存续期间也照常成功。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenEntryFileDeniesTamperWhileHeld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "entry.exe")
	if err := os.WriteFile(path, []byte("verified bytes\n"), 0o755); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}

	f, err := openEntryFile(path)
	if err != nil {
		t.Fatalf("openEntryFile: %v", err)
	}

	// 调包第一步——写打开：句柄存续期间必须失败（共享冲突）。
	if wf, werr := os.OpenFile(path, os.O_WRONLY, 0); werr == nil {
		_ = wf.Close()
		t.Fatal("write-open succeeded while the verify handle is held: TOCTOU window open (audit batch 2, finding 3)")
	}

	// 调包第二步——换名腾位（rename-over 是最常见的换包实现）：
	// 句柄存续期间必须失败。
	if rerr := os.Rename(path, filepath.Join(dir, "moved.exe")); rerr == nil {
		t.Fatal("rename succeeded while the verify handle is held: the verified file can be swapped out")
	}

	// 句柄关闭后写打开恢复成功：窗口只在复核→Start 之间闭合，不是永久锁。
	if err := f.Close(); err != nil {
		t.Fatalf("close verify handle: %v", err)
	}
	wf, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("write-open after handle close: %v (window must close, not stay locked)", err)
	}
	_ = wf.Close()
}

// TestLaunchSucceedsWithDenyShareHandle 钉住共享模式与 CreateProcess 的
// 兼容性：拒绝写共享的复核句柄绝不能挡住 Start 的读打开（GENERIC_READ +
// FILE_SHARE_READ|FILE_SHARE_DELETE 与 FILE_SHARE_READ 兼容）。真子进程
// 端到端走一遍 Launch；若共享模式配置错误，Launch 在 Start 处失败。
func TestLaunchSucceedsWithDenyShareHandle(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "echo-hello")

	m := newValidManifest([]byte("ignored"))
	m.Capabilities = []Capability{CapIndexQuery}
	p, err := launchTestBinary(t, m, NewGrant(CapIndexQuery), nil, trustAllow)
	if err != nil {
		t.Fatalf("Launch with deny-share verify handle: %v (share modes must not block CreateProcess)", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
}

// TestVerify_RejectsSymlinkEntrypoint（复证圈 5 sweep 发现 7）：entrypoint
// 本身是 NTFS symlink 的形态下，旧实现跟随链接打开最终目标——共享冲突锁
// 的是目标文件的句柄，对 symlink 记录本身的删除/替换不触碰该句柄，随后
// cmd.Start 按路径重新解析 symlink 到新目标：被哈希的字节 ≠ 被执行的字节，
// 注释声称的「TOCTOU 窗口在本平台闭合」对该形态失真。修复后
// openEntryFile 以 FILE_FLAG_OPEN_REPARSE_POINT 打开并对 reparse point
// fail-closed 拒绝（symlink/junction 不是合法执行入口）。
// symlink 创建在 Windows 需要开发者模式/管理员特权——创建失败时如实跳过。
func TestVerify_RejectsSymlinkEntrypoint(t *testing.T) {
	dir := newPluginDir(t)
	entry := filepath.Join(dir, "demo.bin")
	raw, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "real.bin")
	if err := os.WriteFile(target, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, entry); err != nil {
		t.Skipf("symlink 创建失败（需开发者模式/管理员特权）: %v", err)
	}
	if _, err := Verify(dir); err == nil {
		t.Fatal("symlink entrypoint 必须被 reparse-point 门拒绝（旧实现跟随链接，哈希目标字节后放行）")
	}
}
