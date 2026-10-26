//go:build soak && (!windows || !amd64)

package soak

import "fmt"

// processTreeMember/processTreeSample shapes live in the untagged
// stdio_gates.go; only the refusing sampler stays behind the soak tag.

type processTreeBudget struct{}

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
