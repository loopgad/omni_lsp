package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkflowsPinGoModToolchain pins the setup-go toolchain contract across
// .github/workflows: every actions/setup-go step must select its Go toolchain
// from go.mod (go-version-file: go.mod) and nothing else. nightly.yml had
// drifted to hardcoded go-version: "1.26" in three of its four jobs while
// go.mod moved on (go 1.26.1), so every go.mod bump would leave the nightly
// soak/verify/release signals running on a stale toolchain while only
// vulncheck tracked the lock — the same one-locked-Go rule ci.yml already
// follows (audit 2026-10-29, 验收面). Text-level scanning is deliberate: the
// workflows are repo-controlled literals and the module carries no YAML
// dependency (§U5).
func TestWorkflowsPinGoModToolchain(t *testing.T) {
	root := moduleRoot()
	if root == "" {
		t.Fatal("cannot locate module root")
	}
	entries, err := os.ReadDir(filepath.Join(root, ".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || (!strings.HasSuffix(e.Name(), ".yml") && !strings.HasSuffix(e.Name(), ".yaml")) {
			continue
		}
		checked++
		path := filepath.Join(root, ".github", "workflows", e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		steps, pinned := 0, 0
		for _, line := range strings.Split(string(data), "\n") {
			trim := strings.TrimSpace(line)
			if trim == "" || strings.HasPrefix(trim, "#") {
				continue
			}
			if strings.Contains(trim, "actions/setup-go@") {
				steps++
			}
			switch {
			case strings.HasPrefix(trim, "go-version:"):
				t.Errorf("%s: hardcoded %q — setup-go must pin go-version-file: go.mod so the job toolchain tracks go.mod", e.Name(), trim)
			case strings.HasPrefix(trim, "go-version-file:"):
				pinned++
				if trim != "go-version-file: go.mod" {
					t.Errorf("%s: %q — go-version-file must be exactly go.mod", e.Name(), trim)
				}
			}
		}
		if steps > 0 && pinned != steps {
			t.Errorf("%s: %d actions/setup-go steps but only %d pinned via go-version-file: go.mod — an unpinned step resolves a drifting toolchain", e.Name(), steps, pinned)
		}
	}
	if checked < 2 {
		t.Fatalf("suspiciously few workflow files checked (%d); walker broken?", checked)
	}
}
