//go:build unix

package watch

import (
	"os"
	"syscall"
)

// Unix file identity (bug round 20 A#1): the inode is the kernel file
// identity — rename always swaps in a different inode, and an inode is never
// reused while the old file still exists, so a replaced target provably
// differs from the baseline fingerprint. Read straight from the stat the
// walk already took; no extra syscall.
func statIdentity(_ string, info os.FileInfo) (fileID, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{valid: true, vol: uint64(st.Dev), index: st.Ino}, true
}
