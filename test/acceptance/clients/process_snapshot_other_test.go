//go:build clients && !windows

package clients

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func configureHiddenClientProcess(cmd *exec.Cmd) {}

func newEditorProcessSnapshotSource() (editorProcessSnapshotSource, error) {
	return nil, fmt.Errorf("native PPID process-tree evidence requires Windows Toolhelp snapshots")
}

func TestEditorProcessTrackerFailsClosedWithoutWindowsPPIDs(t *testing.T) {
	if _, err := startEditorProcessTracker(os.Getpid()); err == nil || !strings.Contains(err.Error(), "requires Windows Toolhelp snapshots") {
		t.Fatalf("process tracker error = %v; want fail-closed native PPID requirement", err)
	}
}
