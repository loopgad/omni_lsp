//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	createSuspended          = 0x00000004
	threadSuspendResume      = 0x0002
	waitFailed               = 0xffffffff
	threadSnapshotNoMoreData = 18
)

var (
	thread32First = syscall.NewLazyDLL("kernel32.dll").NewProc("Thread32First")
	thread32Next  = syscall.NewLazyDLL("kernel32.dll").NewProc("Thread32Next")
	openThread    = syscall.NewLazyDLL("kernel32.dll").NewProc("OpenThread")
	resumeThread  = syscall.NewLazyDLL("kernel32.dll").NewProc("ResumeThread")
)

type threadEntry32 struct {
	Size           uint32
	Usage          uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePriority   int32
	DeltaPriority  int32
	Flags          uint32
}

func runCommandWithJob(cmd *exec.Cmd) (err error) {
	job, err := newKillOnCloseJob()
	if err != nil {
		return fmt.Errorf("create Node process job: %w", err)
	}
	defer func() {
		if closeErr := job.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close Node process job: %w", closeErr))
		}
	}()

	procAttr := &syscall.SysProcAttr{CreationFlags: createSuspended}
	if cmd.SysProcAttr != nil {
		*procAttr = *cmd.SysProcAttr
		procAttr.CreationFlags |= createSuspended
	}
	cmd.SysProcAttr = procAttr
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := job.Assign(cmd.Process); err != nil {
		return errors.Join(fmt.Errorf("assign Node process to kill-on-close job: %w", err), stopSuspendedProcess(cmd))
	}
	if err := resumePrimaryThread(uint32(cmd.Process.Pid)); err != nil {
		return errors.Join(fmt.Errorf("resume Node process after job assignment: %w", err), stopSuspendedProcess(cmd))
	}
	return cmd.Wait()
}

func stopSuspendedProcess(cmd *exec.Cmd) error {
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("terminate suspended Node process: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return fmt.Errorf("wait for suspended Node process: %w", err)
		}
	}
	return nil
}

func resumePrimaryThread(pid uint32) error {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("snapshot process threads: %w", err)
	}
	defer syscall.CloseHandle(snapshot)

	entry := threadEntry32{Size: uint32(unsafe.Sizeof(threadEntry32{}))}
	result, _, callErr := thread32First.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
	runtime.KeepAlive(&entry)
	if result == 0 {
		return winCallError("Thread32First", callErr)
	}
	for {
		if entry.OwnerProcessID == pid {
			thread, _, openErr := openThread.Call(threadSuspendResume, 0, uintptr(entry.ThreadID))
			if thread == 0 {
				return winCallError("OpenThread", openErr)
			}
			defer syscall.CloseHandle(syscall.Handle(thread))
			previousCount, _, resumeErr := resumeThread.Call(thread)
			if previousCount == waitFailed {
				return winCallError("ResumeThread", resumeErr)
			}
			if previousCount == 0 {
				return errors.New("primary thread was not suspended")
			}
			return nil
		}
		entry.Size = uint32(unsafe.Sizeof(threadEntry32{}))
		result, _, callErr = thread32Next.Call(uintptr(snapshot), uintptr(unsafe.Pointer(&entry)))
		runtime.KeepAlive(&entry)
		if result == 0 {
			if callErr == syscall.Errno(threadSnapshotNoMoreData) {
				return fmt.Errorf("primary thread for process %d was not found", pid)
			}
			return winCallError("Thread32Next", callErr)
		}
	}
}
