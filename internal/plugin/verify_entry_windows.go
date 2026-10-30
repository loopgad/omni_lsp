//go:build windows

package plugin

import (
	"os"
	"syscall"
)

// openEntryFile 以拒绝写入/删除的共享模式打开 entrypoint（审计批 2 发现 3）：
// 复核句柄自持到 exec.Start 返回，句柄存续期间任何新的写打开或删除/换名
// （调包所需的全部步骤）都因共享冲突失败——被 sha256 复核过的字节与随后
// CreateProcess 执行的字节钉在同一文件上，哈希完成到 Start 之间的 TOCTOU
// 窗口在本平台闭合。读共享保留（FILE_SHARE_READ）：CreateProcess 自身的
// GENERIC_READ + FILE_SHARE_READ|FILE_SHARE_DELETE 打开与之兼容，不得也
// 不会被挡住（executor_verify_windows_test.go 端到端钉住）。
//
// 已持写句柄的先行写入者不在本原语的射程内（打开即共享冲突只能挡后来的）；
// 那属于插件目录写权限本身的威胁模型，不是复核窗口。
func openEntryFile(path string) (*os.File, error) {
	p16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p16, syscall.GENERIC_READ, syscall.FILE_SHARE_READ,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}
