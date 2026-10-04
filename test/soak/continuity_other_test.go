//go:build soak && !windows

package soak

import "fmt"

type suspendResumeGuard interface {
	Check() error
	Close() error
}

func newSuspendResumeGuard() (suspendResumeGuard, error) {
	return nil, fmt.Errorf("Windows suspend/resume notifications are required for soak continuity")
}
