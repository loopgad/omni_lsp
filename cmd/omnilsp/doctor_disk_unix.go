//go:build unix

package main

// Unix half of the doctor disk-space probe (goal.md §P8): statfs(2) via the
// syscall package. Pure stdlib — go.mod deliberately has no golang.org/x/sys,
// and cgo is not wanted for a doctor probe.

import (
	"fmt"
	"syscall"
)

// diskSpace returns the number of bytes free to unprivileged users (Bavail —
// the btrfs/ext4 reserved blocks are excluded on purpose; that is the figure
// that actually caps index growth) and the total capacity of the volume
// holding path.
func diskSpace(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	// Field widths differ across the unix ports (Bavail is int64 on darwin,
	// uint64 on linux), so go through uint64 explicitly on both operands.
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil
}
