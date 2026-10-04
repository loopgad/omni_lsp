//go:build !windows

package fileio

import "os"

// OpenReadShared opens a file for reading. Non-Windows platforms already
// permit concurrent rename and delete of open files.
func OpenReadShared(path string) (*os.File, error) { return os.Open(path) }
