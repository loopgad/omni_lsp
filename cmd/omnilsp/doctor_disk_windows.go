//go:build windows

package main

// Windows half of the doctor disk-space probe (goal.md §P8): GetDiskFreeSpaceExW
// out of kernel32 via the lazy loader. Pure stdlib — go.mod deliberately has no
// golang.org/x/sys, and cgo is not wanted for a doctor probe.

import (
	"fmt"
	"syscall"
	"unsafe"
)

// diskSpace returns the number of bytes free and the total capacity of the
// volume holding path. Only the caller's own free quota (and the volume total)
// are reported; the "available to unprivileged users" figure is discarded.
func diskSpace(path string) (free, total uint64, err error) {
	getDiskFreeSpaceExW := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	// A LazyProc panics inside Call if the DLL or entry point cannot be
	// loaded; Find() turns that panic into an ordinary error so the doctor
	// probe can degrade to SKIP instead of crashing.
	if ferr := getDiskFreeSpaceExW.Find(); ferr != nil {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceExW unavailable: %w", ferr)
	}
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeBytes, totalBytes, availBytes uint64
	r1, _, callErr := getDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeBytes)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&availBytes)),
	)
	if r1 == 0 {
		return 0, 0, fmt.Errorf("GetDiskFreeSpaceExW %s: %v", path, callErr)
	}
	return freeBytes, totalBytes, nil
}
