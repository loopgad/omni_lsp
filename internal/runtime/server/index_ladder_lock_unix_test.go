//go:build !windows

package server

import (
	"fmt"
	"os"
	"syscall"
)

// holdExclusiveFileLock 以独占 flock 占住 path（与
// internal/index/persistent 的 file_lock_unix.go 同一锁定原语），返回释放
// 函数。守卫测试用它模拟「另一进程持有 writer.lock」的竞争态。
func holdExclusiveFileLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
