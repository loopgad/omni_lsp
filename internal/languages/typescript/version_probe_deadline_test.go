package typescript

// round23 C: probeTscVersion was a bare exec.Command on the request path
// behind buildCtxOnce (nested Conn.BuildContextID) — a hung tsc binary
// parked the first build-context-dependent request forever and queued every
// later one on the Once. The fix gives the probe a CommandContext deadline
// that falls back to "<lang>:sha256:unavailable"; this pin fails on elapsed
// time when the deadline is removed (red by mutation, same experiment as the
// golang build_context_probe_test.go pin).

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
		t.Skip("fake hung `tsc` is exercised via a copied Windows executable on PATH")
	}
	// The hang hook lives in the package TestMain (stderr_drain_order_test.go):
	// the test binary impersonates a hung tsc when the env var is set. The
	// fake tool must be an executable copy of the test binary, not a cmd
	// shim: cmd.exe wraps the shim, its sleeper grandchild inherits the
	// probe's pipe write handles, and Output() then blocks on the orphan
	// long after cmd.exe is killed.
	fakeDir := t.TempDir()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	data, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeDir, "tsc.exe"), data, 0o755); err != nil {
		t.Fatalf("stage fake tsc.exe: %v", err)
	}
	t.Setenv("OMNILSP_FAKE_TSC_HANG", "1")
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	orig := versionProbeTimeout
	versionProbeTimeout = 200 * time.Millisecond
	defer func() { versionProbeTimeout = orig }()

	conn := nested.New(nested.Config{
		Name:         serverName,
		Lang:         langID,
		WorkDir:      t.TempDir(),
		VersionProbe: probeTscVersion,
		ParseVersion: parseTscVersion,
	})
	started := time.Now()
	id := conn.BuildContextID()
	elapsed := time.Since(started)
	if elapsed > 2*time.Second {
		t.Fatalf("BuildContextID returned after %v: the tsc version probe has no deadline and hung the request path on buildCtxOnce", elapsed)
	}
	if id != identity.BuildContextID("typescript:sha256:unavailable") {
		t.Fatalf("BuildContextID = %q after a timed-out probe, want the unavailable fallback", id)
	}
}
