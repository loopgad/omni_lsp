//go:build !windows

package plugin

import "os"

// openEntryFile 打开 entrypoint 供 sha256 复核（审计批 2 发现 3）。
//
// Unix 没有强制共享锁：句柄存续不阻止其他进程写入，也不阻止 rename-over
// 换入新文件——「复核体与执行体钉在同一份字节」在本平台只能收窄、无法
// 闭合（按句柄 exec 需要 /proc 与 fd 继承的平台特定路径，对 low 级发现
// 不成比例）。残余窗口在此显式记录：哈希完成到 exec.Start 重开路径之间，
// 对插件目录有写权限的本地进程仍可换入未校验二进制；窗口可重复
// （mountPlugins 每会话重跑 Discover→Install→Launch），但可与 Windows 侧
// 的真闭合（verify_entry_windows.go）在同一调用形状下共存。持句柄仍有
// 一点收益：被复核的 inode 在窗口内保持存活，为将来按句柄 exec 留路。
func openEntryFile(path string) (*os.File, error) {
	return os.Open(path)
}
