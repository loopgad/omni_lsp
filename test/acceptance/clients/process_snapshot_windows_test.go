//go:build clients && windows

package clients

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"
)

func configureHiddenClientProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}

const (
	th32csSnapProcess              = 0x00000002
	processQueryLimitedInformation = 0x00001000
	errorNoMoreFiles               = syscall.Errno(18)
)

type windowsEditorProcessSnapshotSource struct{}

type editorProcessEntry32W struct {
	Size              uint32
	Usage             uint32
	ProcessID         uint32
	DefaultHeapID     uintptr
	ModuleID          uint32
	Threads           uint32
	ParentProcessID   uint32
	PriorityClassBase int32
	Flags             uint32
	ExeFile           [260]uint16
}

type editorFileTime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

var (
	editorKernel32               = syscall.NewLazyDLL("kernel32.dll")
	createEditorToolhelpSnapshot = editorKernel32.NewProc("CreateToolhelp32Snapshot")
	editorProcess32FirstW        = editorKernel32.NewProc("Process32FirstW")
	editorProcess32NextW         = editorKernel32.NewProc("Process32NextW")
	editorOpenProcess            = editorKernel32.NewProc("OpenProcess")
	editorGetProcessTimes        = editorKernel32.NewProc("GetProcessTimes")
)

func newEditorProcessSnapshotSource() (editorProcessSnapshotSource, error) {
	for name, proc := range map[string]*syscall.LazyProc{
		"CreateToolhelp32Snapshot": createEditorToolhelpSnapshot,
		"Process32FirstW":          editorProcess32FirstW,
		"Process32NextW":           editorProcess32NextW,
		"OpenProcess":              editorOpenProcess,
		"GetProcessTimes":          editorGetProcessTimes,
	} {
		if err := proc.Find(); err != nil {
			return nil, fmt.Errorf("resolve native %s: %w", name, err)
		}
	}
	return windowsEditorProcessSnapshotSource{}, nil
}

func (windowsEditorProcessSnapshotSource) snapshot() (map[int]editorProcessRecord, error) {
	handle, _, callErr := createEditorToolhelpSnapshot.Call(th32csSnapProcess, 0)
	if handle == 0 || handle == uintptr(syscall.InvalidHandle) {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS): %w", callErr)
	}
	defer syscall.CloseHandle(syscall.Handle(handle))

	entry := editorProcessEntry32W{Size: uint32(unsafe.Sizeof(editorProcessEntry32W{}))}
	ret, _, callErr := editorProcess32FirstW.Call(handle, uintptr(unsafe.Pointer(&entry)))
	if ret == 0 {
		return nil, fmt.Errorf("Process32FirstW: %w", callErr)
	}
	snapshot := make(map[int]editorProcessRecord)
	for {
		pid := int(entry.ProcessID)
		image := syscall.UTF16ToString(entry.ExeFile[:])
		if pid > 0 && image != "" {
			snapshot[pid] = editorProcessRecord{
				PID: pid, ParentPID: int(entry.ParentProcessID), Image: image,
			}
		}
		entry.Size = uint32(unsafe.Sizeof(editorProcessEntry32W{}))
		ret, _, callErr = editorProcess32NextW.Call(handle, uintptr(unsafe.Pointer(&entry)))
		if ret != 0 {
			continue
		}
		if errno, ok := callErr.(syscall.Errno); ok && errno == errorNoMoreFiles {
			return snapshot, nil
		}
		return nil, fmt.Errorf("Process32NextW: %w", callErr)
	}
}

func (windowsEditorProcessSnapshotSource) creationTime(pid int) (uint64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid process ID %d", pid)
	}
	handle, _, callErr := editorOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if handle == 0 {
		return 0, fmt.Errorf("OpenProcess(PID %d): %w", pid, callErr)
	}
	defer syscall.CloseHandle(syscall.Handle(handle))

	var created, exited, kernel, user editorFileTime
	ret, _, callErr := editorGetProcessTimes.Call(
		handle,
		uintptr(unsafe.Pointer(&created)),
		uintptr(unsafe.Pointer(&exited)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ret == 0 {
		return 0, fmt.Errorf("GetProcessTimes(PID %d): %w", pid, callErr)
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
}
