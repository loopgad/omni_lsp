//go:build darwin || linux

package persistent

import (
	"errors"
	"os"
	"syscall"
)

func tryWriterLock(f *os.File) (func() error, error) {
	return tryGenerationLock(f, true)
}

func tryGenerationLock(f *os.File, exclusive bool) (func() error, error) {
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errWriterLockBusy
		}
		return nil, err
	}
	return func() error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
