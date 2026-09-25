//go:build windows

package persistent

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	lockFileExProc   = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")
	unlockFileExProc = syscall.NewLazyDLL("kernel32.dll").NewProc("UnlockFileEx")
)

func tryWriterLock(f *os.File) (func() error, error) {
	const failImmediately = 0x1
	const exclusiveLock = 0x2
	const lockViolation = syscall.Errno(33)
	overlapped := new(syscall.Overlapped)
	result, _, callErr := lockFileExProc.Call(f.Fd(), failImmediately|exclusiveLock, 0, 1, 0, uintptr(unsafe.Pointer(overlapped)))
	runtime.KeepAlive(overlapped)
	if result == 0 {
		if callErr == lockViolation {
			return nil, errWriterLockBusy
		}
		if callErr != syscall.Errno(0) {
			return nil, callErr
		}
		return nil, syscall.EINVAL
	}
	return func() error {
		result, _, callErr := unlockFileExProc.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(overlapped)))
		runtime.KeepAlive(overlapped)
		if result == 0 {
			return callErr
		}
		return nil
	}, nil
}
