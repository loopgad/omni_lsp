//go:build windows

package server

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// holdExclusiveFileLock 以独占字节区间锁占住 path（与
// internal/index/persistent 的 file_lock_windows.go 同一锁定原语），返回
// 释放函数。守卫测试用它模拟「另一进程持有 writer.lock」的竞争态。
func holdExclusiveFileLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	const failImmediately = 0x1
	const exclusiveLock = 0x2
	overlapped := new(syscall.Overlapped)
	result, _, callErr := proc.Call(f.Fd(), failImmediately|exclusiveLock, 0, 1, 0, uintptr(unsafe.Pointer(overlapped)))
	runtime.KeepAlive(overlapped)
	if result == 0 {
		_ = f.Close()
		return nil, fmt.Errorf("LockFileEx: %v", callErr)
	}
	return func() {
		unlock := syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
		_, _, _ = unlock.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(overlapped)))
		runtime.KeepAlive(overlapped)
		_ = f.Close()
	}, nil
}
