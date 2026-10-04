// Package fileio provides filesystem reads that allow concurrent editor
// atomic replacement on platforms with share-mode file handles.
//
// Invariants: reads never modify the source, ReadFileShared closes its handle
// on every read outcome, and read or close failures remain errors. Sharing
// permits replacement; callers must validate content identity when they need
// a consistent workspace view.
package fileio

import (
	"io"
	"os"
)

// ReadFileShared opens path for reading with replacement-friendly sharing,
// reads its contents, and closes the handle.
func ReadFileShared(path string) ([]byte, error) {
	file, err := OpenReadShared(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, &os.PathError{Op: "read", Path: path, Err: readErr}
	}
	if closeErr != nil {
		return nil, &os.PathError{Op: "close", Path: path, Err: closeErr}
	}
	return data, nil
}
