package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestProtocolGenCheckProbe wraps the documented operator entry point
// `go run scripts/gen-protocol.go -check` (scripts/gen-protocol.go:7) as a
// probe so the protocol manifest cannot drift silently between the
// in-process lock (internal/conformance manifest test) and the CLI surface.
// A drift makes the generator exit 1, which fails this test — the evidence
// basis for the X9-2 probe wiring owned by the registry.
func TestProtocolGenCheckProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("probe shells out to the go toolchain; skipped in -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	cmd := exec.Command("go", "run", "scripts/gen-protocol.go", "-check")
	cmd.Dir = repoRootForProbe(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gen-protocol -check reported drift: %v\n%s", err, output)
	}
}

// repoRootForProbe walks up from the package directory to the module root so
// the probe works regardless of the go test working directory.
func repoRootForProbe(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
