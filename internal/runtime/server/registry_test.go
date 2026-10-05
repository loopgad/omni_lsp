package server

// §C17 method-registry fingerprint guard.
//
// TestAllHandlersRegistered used to be the only check on this surface, and its
// hardcoded list had already drifted (20 entries against 35 registrations).
// ADR-0009 D3 records exactly this failure shape: a probe that no longer
// describes what it guards. The manifest already carries the method names
// (protocol.manifest `method "…"` lines, generated from the Register call
// literals), so the guard compares against that instead of a fourth hand-kept
// list.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/lsp"
)

// TestRegisteredMethodsMatchManifest asserts the registered method set and the
// manifest's method fingerprint are the same set — no more, no fewer.
func TestRegisteredMethodsMatchManifest(t *testing.T) {
	want := manifestMethods(t)

	s := New(DefaultConfig()) // New calls registerHandlers
	got := s.Dispatcher().Methods()

	if len(got) == 0 {
		t.Fatal("dispatcher registered no methods; the guard would pass vacuously")
	}
	for _, m := range diffSets(got, want) {
		t.Errorf("method registry / manifest mismatch: %s\n"+
			"registered but unfingerprinted: %v\n"+
			"fingerprinted but unregistered: %v\n"+
			"run: go run scripts/gen-protocol.go", m, only(got, want), only(want, got))
	}
}

// diffSets returns one message describing the symmetric difference, or "" when
// the sets match.
func diffSets(got, want []string) []string {
	if slices.Equal(got, want) {
		return nil
	}
	return []string{"sets differ"}
}

func only(a, b []string) []string {
	var out []string
	for _, v := range a {
		if !slices.Contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}

// manifestMethods reads the `method "…"` fingerprint lines from
// protocol.manifest, unquoting each literal.
func manifestMethods(t *testing.T) []string {
	t.Helper()
	root, err := moduleRootForTest()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(lsp.ManifestRelPath)))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "method ") {
			continue
		}
		quoted := strings.TrimPrefix(line, "method ")
		name := strings.Trim(quoted, `"`)
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// moduleRootForTest walks up to the directory holding go.mod.
func moduleRootForTest() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
