package ccls

// round23 C: probeClangdVersion was a bare exec.Command on the request path
// behind buildCtxOnce (nested Conn.BuildContextID) — a hung clangd binary
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

// TestMain lets the same test binary impersonate a hung clangd: staged on
// PATH as clangd.exe it sleeps 20s before exiting, so a probe without a
// deadline burns the full hang while CommandContext kills it at the
// shortened deadline. The fake tool must be an executable copy of the test
// binary, not a cmd shim: cmd.exe wraps the shim, its sleeper grandchild
// inherits the probe's pipe write handles, and Output() then blocks on the
// orphan long after cmd.exe is killed.
func TestMain(m *testing.M) {
	if os.Getenv("OMNILSP_FAKE_CLANGD_HANG") != "" {
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestVersionProbeCannotHangRequestPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("fake hung `clangd` is exercised via a copied Windows executable on PATH")
	}
	fakeDir := t.TempDir()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	data, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeDir, "clangd.exe"), data, 0o755); err != nil {
		t.Fatalf("stage fake clangd.exe: %v", err)
	}
	t.Setenv("OMNILSP_FAKE_CLANGD_HANG", "1")
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	orig := versionProbeTimeout
	versionProbeTimeout = 200 * time.Millisecond
	defer func() { versionProbeTimeout = orig }()

	conn := nested.New(nested.Config{
		Name:         "clangd",
		Lang:         "cpp",
		WorkDir:      t.TempDir(),
		VersionProbe: probeClangdVersion,
		ParseVersion: parseClangdVersion,
	})
	started := time.Now()
	id := conn.BuildContextID()
	elapsed := time.Since(started)
	if elapsed > 2*time.Second {
		t.Fatalf("BuildContextID returned after %v: the clangd version probe has no deadline and hung the request path on buildCtxOnce", elapsed)
	}
	if id != identity.BuildContextID("cpp:sha256:unavailable") {
		t.Fatalf("BuildContextID = %q after a timed-out probe, want the unavailable fallback", id)
	}
}
