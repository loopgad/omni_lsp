//go:build !windows

package watch

import (
	"os"

	"github.com/omnilsp/omni/internal/workspace/fileio"
)

func openReadShared(path string) (*os.File, error) { return fileio.OpenReadShared(path) }
