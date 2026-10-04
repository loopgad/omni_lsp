//go:build windows

package perf

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

const (
	toolhelpProcessSnapshot = 0x00000002
	processQueryInformation = 0x00000400
	processVMRead           = 0x00000010
)

var (
	kernel32Perf               = syscall.NewLazyDLL("kernel32.dll")
	createToolhelpSnapshotPerf = kernel32Perf.NewProc("CreateToolhelp32Snapshot")
	process32FirstWPerf        = kernel32Perf.NewProc("Process32FirstW")
	process32NextWPerf         = kernel32Perf.NewProc("Process32NextW")
	openProcessPerf            = kernel32Perf.NewProc("OpenProcess")
	getProcessHandleCountPerf  = kernel32Perf.NewProc("GetProcessHandleCount")
	closeHandlePerf            = kernel32Perf.NewProc("CloseHandle")
	psapiPerf                  = syscall.NewLazyDLL("psapi.dll")
	getProcessMemoryInfoPerf   = psapiPerf.NewProc("GetProcessMemoryInfo")
)

type processEntry32Perf struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriorityBase    int32
	Flags           uint32
	ExecutableName  [260]uint16
}

type processMemoryCountersExPerf struct {
	Size                       uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

func e2eWindowsProcessTreeResources(rootPID int, stage string) e2eResourceSnapshot {
	snapshot := e2eResourceSnapshot{Stage: stage, SampledAt: time.Now().UTC(), RootPID: rootPID, Status: "not_verified"}
	parents, threadCounts, err := windowsProcessParents()
	if err != nil {
		snapshot.Error = "Toolhelp process snapshot failed: " + err.Error()
		return snapshot
	}
	root := uint32(rootPID)
	processes := map[uint32]bool{root: true}
	for changed := true; changed; {
		changed = false
		for pid, parent := range parents {
			if processes[parent] && !processes[pid] {
				processes[pid] = true
				changed = true
			}
		}
	}
	snapshot.ProcessCount = len(processes)
	measured := 0
	rootMeasured := false
	for pid := range processes {
		handle, _, callErr := openProcessPerf.Call(processQueryInformation|processVMRead, 0, uintptr(pid))
		if handle == 0 {
			continue
		}
		memory := processMemoryCountersExPerf{Size: uint32(unsafe.Sizeof(processMemoryCountersExPerf{}))}
		ok, _, memoryErr := getProcessMemoryInfoPerf.Call(handle, uintptr(unsafe.Pointer(&memory)), uintptr(memory.Size))
		var handleCount uint32
		handlesOK, _, handlesErr := getProcessHandleCountPerf.Call(handle, uintptr(unsafe.Pointer(&handleCount)))
		_, _, _ = closeHandlePerf.Call(handle)
		if ok == 0 || handlesOK == 0 {
			_ = callErr
			_ = memoryErr
			_ = handlesErr
			continue
		}
		measured++
		rootMeasured = rootMeasured || pid == root
		snapshot.WorkingSetBytes += uint64(memory.WorkingSetSize)
		snapshot.PrivateBytes += uint64(memory.PrivateUsage)
		snapshot.HandleCount += uint64(handleCount)
		snapshot.ThreadCount += uint64(threadCounts[pid])
	}
	if !rootMeasured {
		snapshot.Error = "Toolhelp found candidate PID but Windows denied or lost its process metrics"
		return snapshot
	}
	if measured != len(processes) {
		snapshot.Error = fmt.Sprintf("Toolhelp enumerated %d candidate/descendant processes but only %d had readable memory and handle metrics", len(processes), measured)
		return snapshot
	}
	if snapshot.PrivateBytes > e2eS19MaxPrivateMemoryBytes {
		snapshot.Status = "failed"
		snapshot.Error = fmt.Sprintf("process-tree private memory %d bytes exceeded %d byte acceptance limit", snapshot.PrivateBytes, e2eS19MaxPrivateMemoryBytes)
		return snapshot
	}
	snapshot.Status = "observed"
	return snapshot
}

func windowsProcessParents() (map[uint32]uint32, map[uint32]uint32, error) {
	handle, _, callErr := createToolhelpSnapshotPerf.Call(toolhelpProcessSnapshot, 0)
	if handle == ^uintptr(0) {
		return nil, nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", callErr)
	}
	defer closeHandlePerf.Call(handle)
	entry := processEntry32Perf{Size: uint32(unsafe.Sizeof(processEntry32Perf{}))}
	ok, _, firstErr := process32FirstWPerf.Call(handle, uintptr(unsafe.Pointer(&entry)))
	if ok == 0 {
		return nil, nil, fmt.Errorf("Process32FirstW: %w", firstErr)
	}
	parents := make(map[uint32]uint32)
	threads := make(map[uint32]uint32)
	for {
		parents[entry.ProcessID] = entry.ParentProcessID
		threads[entry.ProcessID] = entry.Threads
		entry.Size = uint32(unsafe.Sizeof(processEntry32Perf{}))
		ok, _, _ = process32NextWPerf.Call(handle, uintptr(unsafe.Pointer(&entry)))
		if ok == 0 {
			break
		}
	}
	return parents, threads, nil
}
