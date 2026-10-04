//go:build windows

package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
	processTerminate                  = 0x0001
	processSetQuota                   = 0x0100
)

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	createJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	setInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	assignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
)

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IOInfo                jobIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type killOnCloseJob struct {
	handle syscall.Handle
}

func newKillOnCloseJob() (processJob, error) {
	handle, _, callErr := createJobObjectW.Call(0, 0)
	if handle == 0 {
		return nil, winCallError("CreateJobObjectW", callErr)
	}
	job := &killOnCloseJob{handle: syscall.Handle(handle)}
	info := jobExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	result, _, callErr := setInformationJobObject.Call(
		handle,
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	runtime.KeepAlive(&info)
	if result == 0 {
		_ = job.Close()
		return nil, winCallError("SetInformationJobObject", callErr)
	}
	return job, nil
}

func (j *killOnCloseJob) Assign(process *os.Process) error {
	handle, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(process.Pid))
	if err != nil {
		return fmt.Errorf("open Node process: %w", err)
	}
	defer syscall.CloseHandle(handle)
	result, _, callErr := assignProcessToJobObject.Call(uintptr(j.handle), uintptr(handle))
	if result == 0 {
		return winCallError("AssignProcessToJobObject", callErr)
	}
	return nil
}

func (j *killOnCloseJob) Close() error {
	if j.handle == 0 {
		return nil
	}
	handle := j.handle
	j.handle = 0
	if err := syscall.CloseHandle(handle); err != nil {
		return fmt.Errorf("CloseHandle job: %w", err)
	}
	return nil
}

func winCallError(operation string, callErr error) error {
	if callErr == nil || callErr == syscall.Errno(0) {
		callErr = syscall.EINVAL
	}
	return fmt.Errorf("%s: %w", operation, callErr)
}
