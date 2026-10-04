//go:build soak && windows && amd64

package soak

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectBasicAccounting          = 1
	jobObjectBasicProcessIDList       = 3
	jobLimitJobMemory                 = 0x00000200
	jobLimitKillOnClose               = 0x00002000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
	processQueryInformation           = 0x0400
	processVMRead                     = 0x0010
	errorMoreData                     = 234
	errorNoMoreFiles                  = 18
	th32csSnapProcess                 = 0x00000002
)

var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	psapi                      = syscall.NewLazyDLL("psapi.dll")
	createJobObjectW           = kernel32.NewProc("CreateJobObjectW")
	setInformationJobObject    = kernel32.NewProc("SetInformationJobObject")
	queryInformationJobObject  = kernel32.NewProc("QueryInformationJobObject")
	assignProcessToJobObject   = kernel32.NewProc("AssignProcessToJobObject")
	isProcessInJob             = kernel32.NewProc("IsProcessInJob")
	openProcess                = kernel32.NewProc("OpenProcess")
	closeHandle                = kernel32.NewProc("CloseHandle")
	queryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	terminateProcessProc       = kernel32.NewProc("TerminateProcess")
	getProcessHandleCount      = kernel32.NewProc("GetProcessHandleCount")
	getProcessMemoryInfo       = psapi.NewProc("GetProcessMemoryInfo")
	createToolhelp32Snapshot   = kernel32.NewProc("CreateToolhelp32Snapshot")
	process32FirstW            = kernel32.NewProc("Process32FirstW")
	process32NextW             = kernel32.NewProc("Process32NextW")
)

type jobBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	_                       uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	_                       uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobExtendedLimitInformation struct {
	BasicLimitInformation jobBasicLimitInformation
	IOInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type jobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

type processMemoryCountersEx struct {
	CB                         uint32
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

type processTreeBudget struct {
	handle syscall.Handle
	limit  uint64
}

type processTreeMember struct {
	PID          uint32 `json:"pid"`
	ParentPID    uint32 `json:"parent_pid"`
	Role         string `json:"role"`
	ImagePath    string `json:"image_path"`
	PrivateBytes uint64 `json:"private_bytes"`
	Handles      uint32 `json:"handles"`
}

type processTreeSample struct {
	PrivateBytes       uint64              `json:"private_bytes"`
	PeakJobCommitBytes uint64              `json:"peak_job_commit_bytes"`
	JobMemoryLimit     uint64              `json:"job_memory_limit_bytes"`
	ActiveProcesses    uint32              `json:"active_processes"`
	HandleCounts       map[string]uint64   `json:"handles_by_role"`
	Members            []processTreeMember `json:"members"`
}

type processEntry32W struct {
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

func TestProcessParentPIDsIncludesRunner(t *testing.T) {
	parents, err := processParentPIDs()
	if err != nil {
		t.Fatalf("processParentPIDs() error = %v", err)
	}
	pid := uint32(os.Getpid())
	parentPID, ok := parents[pid]
	if !ok || parentPID == 0 {
		t.Fatalf("process parent snapshot has runner PID %d with parent %d, present=%t", pid, parentPID, ok)
	}
}

func newProcessTreeBudget(pid int, limit uint64) (*processTreeBudget, error) {
	h, _, callErr := createJobObjectW.Call(0, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateJobObjectW: %w", callErr)
	}
	job := &processTreeBudget{handle: syscall.Handle(h), limit: limit}
	info := jobExtendedLimitInformation{}
	info.BasicLimitInformation.LimitFlags = jobLimitJobMemory | jobLimitKillOnClose
	info.JobMemoryLimit = uintptr(limit)
	ret, _, callErr := setInformationJobObject.Call(
		uintptr(job.handle), jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info),
	)
	if ret == 0 {
		job.Close()
		return nil, fmt.Errorf("SetInformationJobObject (8 GiB job-memory limit): %w", callErr)
	}
	if err := job.assign(pid); err != nil {
		job.Close()
		return nil, fmt.Errorf("assign test runner PID %d to Job Object: %w", pid, err)
	}
	var verified jobExtendedLimitInformation
	if err := job.query(jobObjectExtendedLimitInformation, unsafe.Pointer(&verified), uint32(unsafe.Sizeof(verified))); err != nil {
		// The runner is already a member. Closing this job would terminate it.
		return nil, fmt.Errorf("verify Job Object memory limit: %w", err)
	}
	if verified.BasicLimitInformation.LimitFlags&(jobLimitJobMemory|jobLimitKillOnClose) != jobLimitJobMemory|jobLimitKillOnClose || uint64(verified.JobMemoryLimit) != limit {
		return nil, fmt.Errorf("Job Object limit readback mismatch: flags=%#x limit=%d want=%d", verified.BasicLimitInformation.LimitFlags, verified.JobMemoryLimit, limit)
	}
	if err := job.verifyMember(pid); err != nil {
		return nil, fmt.Errorf("verify test runner Job Object membership: %w", err)
	}
	return job, nil
}

func (j *processTreeBudget) assign(pid int) error {
	proc, err := openProcessHandle(uint32(pid), processSetQuota|processTerminate|processQueryInformation)
	if err != nil {
		return err
	}
	defer closeHandle.Call(uintptr(proc))
	ret, _, callErr := assignProcessToJobObject.Call(uintptr(j.handle), uintptr(proc))
	if ret == 0 {
		return callErr
	}
	return nil
}

func (j *processTreeBudget) verifyMember(pid int) error {
	proc, err := openProcessHandle(uint32(pid), processQueryInformation)
	if err != nil {
		return err
	}
	defer closeHandle.Call(uintptr(proc))
	var inJob int32
	ret, _, callErr := isProcessInJob.Call(uintptr(proc), uintptr(j.handle), uintptr(unsafe.Pointer(&inJob)))
	if ret == 0 {
		return callErr
	}
	if inJob == 0 {
		return fmt.Errorf("PID %d is not in the measured Job Object", pid)
	}
	return nil
}

func (j *processTreeBudget) sample(runnerPID, candidatePID int) (processTreeSample, error) {
	var lastErr error
	// Restart injection intentionally changes the Job PID list. Retry longer
	// than a normal sample so a fast supervised backend replacement is not
	// misclassified as an accounting failure.
	for attempt := 0; attempt < 40; attempt++ {
		ids, err := j.processIDs()
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var accounting jobBasicAccountingInformation
		if err := j.query(jobObjectBasicAccounting, unsafe.Pointer(&accounting), uint32(unsafe.Sizeof(accounting))); err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if accounting.ActiveProcesses != uint32(len(ids)) {
			lastErr = fmt.Errorf("Job Object PID list has %d entries but accounting reports %d active processes", len(ids), accounting.ActiveProcesses)
			time.Sleep(20 * time.Millisecond)
			continue
		}
		parentPIDs, parentErr := processParentPIDs()
		if parentErr != nil {
			lastErr = parentErr
			time.Sleep(20 * time.Millisecond)
			continue
		}
		var limits jobExtendedLimitInformation
		if err := j.query(jobObjectExtendedLimitInformation, unsafe.Pointer(&limits), uint32(unsafe.Sizeof(limits))); err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		sample := processTreeSample{
			PeakJobCommitBytes: uint64(limits.PeakJobMemoryUsed),
			JobMemoryLimit:     uint64(limits.JobMemoryLimit),
			ActiveProcesses:    accounting.ActiveProcesses,
			HandleCounts:       map[string]uint64{"runner": 0, "candidate": 0, "backend": 0},
			Members:            make([]processTreeMember, 0, len(ids)),
		}
		stable := true
		for _, pid := range ids {
			member, sampleErr := sampleProcess(pid, runnerPID, candidatePID, parentPIDs)
			if sampleErr != nil {
				lastErr = sampleErr
				stable = false
				break
			}
			sample.PrivateBytes += member.PrivateBytes
			sample.HandleCounts[member.Role] += uint64(member.Handles)
			sample.Members = append(sample.Members, member)
		}
		if stable {
			latestIDs, listErr := j.processIDs()
			var latestAccounting jobBasicAccountingInformation
			accountErr := j.query(jobObjectBasicAccounting, unsafe.Pointer(&latestAccounting), uint32(unsafe.Sizeof(latestAccounting)))
			if listErr == nil && accountErr == nil && latestAccounting.ActiveProcesses == uint32(len(latestIDs)) && samePIDs(ids, latestIDs) {
				if sample.JobMemoryLimit != j.limit {
					return processTreeSample{}, fmt.Errorf("Job Object memory limit changed: got %d want %d", sample.JobMemoryLimit, j.limit)
				}
				if len(sample.Members) != int(sample.ActiveProcesses) {
					return processTreeSample{}, fmt.Errorf("sampled %d Job Object members but accounting reports %d active processes", len(sample.Members), sample.ActiveProcesses)
				}
				return sample, nil
			}
			lastErr = fmt.Errorf("Job Object membership changed during process sampling")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return processTreeSample{}, fmt.Errorf("Job Object process tree did not stabilize after retries: %w", lastErr)
}

func samePIDs(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[uint32]int, len(a))
	for _, pid := range a {
		seen[pid]++
	}
	for _, pid := range b {
		seen[pid]--
		if seen[pid] < 0 {
			return false
		}
	}
	return true
}

func (j *processTreeBudget) processIDs() ([]uint32, error) {
	capacity := 64
	for attempt := 0; attempt < 8; attempt++ {
		buf := make([]byte, 8+capacity*int(unsafe.Sizeof(uintptr(0))))
		var returned uint32
		ret, _, callErr := queryInformationJobObject.Call(
			uintptr(j.handle), jobObjectBasicProcessIDList,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&returned)),
		)
		if ret == 0 {
			if errno, ok := callErr.(syscall.Errno); ok && errno == errorMoreData {
				capacity *= 2
				continue
			}
			return nil, fmt.Errorf("QueryInformationJobObject(process IDs): %w", callErr)
		}
		assigned := *(*uint32)(unsafe.Pointer(&buf[0]))
		count := *(*uint32)(unsafe.Pointer(&buf[4]))
		if int(count) > capacity || count != assigned {
			capacity = max(capacity*2, int(assigned)+16)
			continue
		}
		ids := make([]uint32, count)
		for i := range ids {
			value := *(*uintptr)(unsafe.Pointer(&buf[8+i*int(unsafe.Sizeof(uintptr(0)))]))
			ids[i] = uint32(value)
		}
		return ids, nil
	}
	return nil, fmt.Errorf("Job Object process list did not stabilize after retries")
}

func (j *processTreeBudget) query(class uintptr, output unsafe.Pointer, size uint32) error {
	var returned uint32
	ret, _, callErr := queryInformationJobObject.Call(uintptr(j.handle), class, uintptr(output), uintptr(size), uintptr(unsafe.Pointer(&returned)))
	if ret == 0 {
		return callErr
	}
	return nil
}

func (j *processTreeBudget) Close() {
	if j == nil || j.handle == 0 {
		return
	}
	// A successfully assigned job has KILL_ON_JOB_CLOSE set. The soak keeps
	// its handle open until the Go test process exits so abrupt runner exit
	// closes the handle and terminates candidate/backend descendants.
	closeHandle.Call(uintptr(j.handle))
	j.handle = 0
}

func openProcessHandle(pid uint32, access uintptr) (syscall.Handle, error) {
	h, _, callErr := openProcess.Call(access, 0, uintptr(pid))
	if h == 0 {
		return 0, callErr
	}
	return syscall.Handle(h), nil
}

func sampleProcess(pid uint32, runnerPID, candidatePID int, parentPIDs map[uint32]uint32) (processTreeMember, error) {
	role := "backend"
	if int(pid) == runnerPID {
		role = "runner"
	} else if int(pid) == candidatePID {
		role = "candidate"
	}
	proc, err := openProcessHandle(pid, processQueryInformation|processVMRead)
	if err != nil {
		return processTreeMember{}, fmt.Errorf("open Job Object PID %d (%s): %w", pid, role, err)
	}
	defer closeHandle.Call(uintptr(proc))
	imagePath, err := processImagePath(proc)
	if err != nil {
		return processTreeMember{}, fmt.Errorf("QueryFullProcessImageNameW PID %d: %w", pid, err)
	}
	counters := processMemoryCountersEx{CB: uint32(unsafe.Sizeof(processMemoryCountersEx{}))}
	ret, _, callErr := getProcessMemoryInfo.Call(uintptr(proc), uintptr(unsafe.Pointer(&counters)), uintptr(counters.CB))
	if ret == 0 {
		return processTreeMember{}, fmt.Errorf("GetProcessMemoryInfo PID %d: %w", pid, callErr)
	}
	var handles uint32
	ret, _, callErr = getProcessHandleCount.Call(uintptr(proc), uintptr(unsafe.Pointer(&handles)))
	if ret == 0 {
		return processTreeMember{}, fmt.Errorf("GetProcessHandleCount PID %d: %w", pid, callErr)
	}
	parentPID, found := parentPIDs[pid]
	if !found {
		return processTreeMember{}, fmt.Errorf("PID %d is absent from the process-parent snapshot", pid)
	}
	return processTreeMember{PID: pid, ParentPID: parentPID, Role: role, ImagePath: imagePath, PrivateBytes: uint64(counters.PrivateUsage), Handles: handles}, nil
}

func processParentPIDs() (map[uint32]uint32, error) {
	handle, _, callErr := createToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if handle == uintptr(syscall.InvalidHandle) || handle == 0 {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS): %w", callErr)
	}
	defer syscall.CloseHandle(syscall.Handle(handle))

	entry := processEntry32W{Size: uint32(unsafe.Sizeof(processEntry32W{}))}
	ret, _, callErr := process32FirstW.Call(handle, uintptr(unsafe.Pointer(&entry)))
	if ret == 0 {
		return nil, fmt.Errorf("Process32FirstW: %w", callErr)
	}
	parents := make(map[uint32]uint32)
	for {
		parents[entry.ProcessID] = entry.ParentProcessID
		entry.Size = uint32(unsafe.Sizeof(processEntry32W{}))
		ret, _, callErr = process32NextW.Call(handle, uintptr(unsafe.Pointer(&entry)))
		if ret != 0 {
			continue
		}
		if errno, ok := callErr.(syscall.Errno); ok && errno == errorNoMoreFiles {
			return parents, nil
		}
		return nil, fmt.Errorf("Process32NextW: %w", callErr)
	}
}

func processImagePath(proc syscall.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	ret, _, callErr := queryFullProcessImageNameW.Call(uintptr(proc), 0, uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)))
	if ret == 0 {
		return "", callErr
	}
	return syscall.UTF16ToString(buffer[:size]), nil
}

func terminateJobPID(pid uint32) error {
	proc, err := openProcessHandle(pid, processTerminate|processQueryInformation)
	if err != nil {
		return fmt.Errorf("open PID %d for controlled backend restart: %w", pid, err)
	}
	defer closeHandle.Call(uintptr(proc))
	ret, _, callErr := terminateProcessProc.Call(uintptr(proc), 0xE7)
	if ret == 0 {
		return fmt.Errorf("TerminateProcess PID %d: %w", pid, callErr)
	}
	return nil
}
