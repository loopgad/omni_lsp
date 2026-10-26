//go:build soak && !windows

package soak

import "fmt"

// The suspendResumeGuard interface itself lives in the untagged
// continuity_monitor.go; only the refusing non-Windows constructor stays
// behind the soak tag.
func newSuspendResumeGuard() (suspendResumeGuard, error) {
	return nil, fmt.Errorf("Windows suspend/resume notifications are required for soak continuity")
}
