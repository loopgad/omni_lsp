//go:build darwin || linux

package persistent

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
