//go:build soak && (!windows || !amd64)

package soak

import "fmt"

type processTreeBudget struct{}

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

func newProcessTreeBudget(_ int, _ uint64) (*processTreeBudget, error) {
	return nil, fmt.Errorf("Windows amd64 Job Object accounting is required for the process-tree resource gate")
}

func (*processTreeBudget) sample(_, _ int) (processTreeSample, error) {
	return processTreeSample{}, fmt.Errorf("Windows amd64 Job Object accounting is unavailable")
}

func (*processTreeBudget) verifyMember(_ int) error {
	return fmt.Errorf("Windows amd64 Job Object membership is unavailable")
}

func terminateJobPID(pid uint32) error {
	return fmt.Errorf("controlled backend process termination is unavailable on this host (PID %d)", pid)
}

func (*processTreeBudget) Close() {}
