//go:build windows

package plugin

import (
	"fmt"
	"os"
	"syscall"
)

// openEntryFile 以拒绝写入/删除的共享模式打开 entrypoint（审计批 2 发现 3）：
// 复核句柄自持到 exec.Start 返回，句柄存续期间任何新的写打开或删除/换名
// （调包所需的全部步骤）都因共享冲突失败——被 sha256 复核过的字节与随后
// CreateProcess 执行的字节钉在同一文件上，哈希完成到 Start 之间的 TOCTOU
// 窗口对普通文件在本平台闭合。读共享保留（FILE_SHARE_READ）：CreateProcess
// 自身的 GENERIC_READ + FILE_SHARE_READ|FILE_SHARE_DELETE 打开与之兼容，
// 不得也不会被挡住（executor_verify_windows_test.go 端到端钉住）。
//
// reparse point 门（复证圈 5 sweep 发现 7）：entrypoint 本身是 NTFS
// symlink/junction 时，旧实现跟随链接打开最终目标——共享冲突锁的是目标文
// 件的句柄，对 symlink 记录本身的删除/替换不触碰该句柄，随后 cmd.Start 按
// 路径重新解析 symlink 到新目标，被哈希的字节 ≠ 被执行的字节，「闭合」承
// 诺对该形态失真。修复：以 FILE_FLAG_OPEN_REPARSE_POINT 打开（命中
// symlink/junction 时打开 reparse point 记录本身而非跟随），再按句柄校验
// FILE_ATTRIBUTE_REPARSE_POINT，命中即 fail-closed 拒绝——symlink/junction
// 不是合法执行入口；普通文件打开语义不变（该标志只影响 reparse 遍历）。
//
// 已持写句柄的先行写入者不在本原语的射程内（打开即共享冲突只能挡后来的）；
// 那属于插件目录写权限本身的威胁模型，不是复核窗口。
const (
	fileFlagOpenReparsePoint  = 0x00200000 // FILE_FLAG_OPEN_REPARSE_POINT
	fileAttributeReparsePoint = 0x00000400 // FILE_ATTRIBUTE_REPARSE_POINT
)

func openEntryFile(path string) (*os.File, error) {
	p16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p16, syscall.GENERIC_READ, syscall.FILE_SHARE_READ,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL|fileFlagOpenReparsePoint, 0)
	if err != nil {
		return nil, err
	}
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		syscall.CloseHandle(h)
		return nil, err
	}
	if info.FileAttributes&fileAttributeReparsePoint != 0 {
		syscall.CloseHandle(h)
		return nil, fmt.Errorf("plugin: entrypoint 是 reparse point（symlink/junction），拒绝作为执行入口复核")
	}
	return os.NewFile(uintptr(h), path), nil
}
