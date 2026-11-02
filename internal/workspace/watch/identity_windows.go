//go:build windows

package watch

import (
	"os"
	"syscall"
	"unsafe"
)

// Windows file identity (bug round 20 A#1): NTFS timestamp coalescing lands
// writes milliseconds apart in the same timestamp tick, so an atomic replace
// (write sibling → rename over target) can leave the target's mtime exactly
// equal to the baseline's while size is unchanged too. The stat signature
// alone then misreports a replaced file as quiet. The discriminating signal
// is the kernel file identity: rename always swaps in a different file
// object, and a filesystem never hands out the index of a file that still
// exists, so the replacement's (volume serial, file index) provably differs
// from the baseline's at the moment of replacement.
//
// Stdlib only: golang.org/x/sys is not on the dependency whitelist, so the
// identity probe goes through CreateFile (query access, full sharing, never
// blocks on a regular file) and GetFileInformationByHandle.

var (
	modkernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procGetFileInformationByHandle = modkernel32.NewProc("GetFileInformationByHandle")
)

const (
	fileReadAttributes      = 0x0080 // query metadata without read access
	fileFlagBackupSemantics = 0x02000000
)

// byHandleFileInformation mirrors the kernel32 structure (52 bytes on
// both 386 and amd64; field order is fixed by the ABI).
type byHandleFileInformation struct {
	FileAttributes     uint32
	CreationTime       syscall.Filetime
	LastAccessTime     syscall.Filetime
	LastWriteTime      syscall.Filetime
	VolumeSerialNumber uint32
	FileSizeHigh       uint32
	FileSizeLow        uint32
	NumberOfLinks      uint32
	FileIndexHigh      uint32
	FileIndexLow       uint32
}

func statIdentity(path string, _ os.FileInfo) (fileID, bool) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fileID{}, false
	}
	handle, err := syscall.CreateFile(name, fileReadAttributes,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL|fileFlagBackupSemantics, 0)
	if err != nil {
		return fileID{}, false
	}
	defer syscall.CloseHandle(handle)
	var info byHandleFileInformation
	r1, _, _ := procGetFileInformationByHandle.Call(uintptr(handle), uintptr(unsafe.Pointer(&info)))
	if r1 == 0 {
		return fileID{}, false
	}
	return fileID{
		valid: true,
		vol:   uint64(info.VolumeSerialNumber),
		index: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, true
}
