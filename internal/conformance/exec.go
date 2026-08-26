package conformance

import (
	"fmt"
	"os/exec"
)

// execCommand runs a gate command from the module root, returning the output
// tail on failure so a red gate is diagnosable from the report alone.
func execCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = moduleRoot()
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return fmt.Errorf("%v: %s", err, tail)
	}
	return nil
}
