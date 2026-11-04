package pyright

// round23 C: probePythonVersion was a bare exec.Command pair on the request
// path behind buildCtxOnce (nested Conn.BuildContextID) — a hung interpreter
// binary parked the first build-context-dependent request forever and queued
// every later one on the Once. The fix gives both probes a CommandContext
// deadline that falls back to "<lang>:sha256:unavailable"; this pin fails on
// elapsed time when the deadline is removed (red by mutation, same
// experiment as the golang build_context_probe_test.go pin).

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages/nested"
)

func TestVersionProbeCannotHangRequestPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("fake hung interpreters are exercised via copied Windows executables on PATH")
	}
	// The hang hook lives in the package TestMain (stderr_drain_order_test.go):
	// the test binary impersonates a hung interpreter when the env var is set.
	// Both interpreter names get a copy: python would be killed at the
	// deadline, and without its own copy python3 could resolve to a real
	// interpreter on PATH and make the fallback assertion machine-dependent.
	// The fakes must be executable copies of the test binary, not cmd shims:
	// cmd.exe wraps the shim, its sleeper grandchild inherits the probe's
	// pipe write handles, and Output() then blocks on the orphan long after
	// cmd.exe is killed.
	fakeDir := t.TempDir()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	data, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	for _, name := range []string{"python.exe", "python3.exe"} {
		if err := os.WriteFile(filepath.Join(fakeDir, name), data, 0o755); err != nil {
			t.Fatalf("stage fake %s: %v", name, err)
		}
	}
	t.Setenv("OMNILSP_FAKE_PYRIGHT_HANG", "1")
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	orig := versionProbeTimeout
	versionProbeTimeout = 200 * time.Millisecond
	defer func() { versionProbeTimeout = orig }()

	conn := nested.New(nested.Config{
		Name:         serverName,
		Lang:         langID,
		WorkDir:      t.TempDir(),
		VersionProbe: probePythonVersion,
		ParseVersion: parsePythonVersion,
	})
	started := time.Now()
	id := conn.BuildContextID()
	elapsed := time.Since(started)
	if elapsed > 2*time.Second {
		t.Fatalf("BuildContextID returned after %v: the python version probes have no deadline and hung the request path on buildCtxOnce", elapsed)
	}
	if id != identity.BuildContextID("python:sha256:unavailable") {
		t.Fatalf("BuildContextID = %q after a timed-out probe, want the unavailable fallback", id)
	}
}
