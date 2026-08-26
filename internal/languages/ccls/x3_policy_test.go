package ccls

// X3 policy tests: compile-database gating for rename (fail-closed) and
// header-context ambiguity surfacing. No real clangd required — the gates
// run before any RPC is issued.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

// bareTestBackend builds a Backend with no process attached: the R4 gates
// under test all sit before the first clangd round-trip.
func bareTestBackend(t *testing.T, withCompileDb bool) *Backend {
	t.Helper()
	dir := t.TempDir()
	if withCompileDb {
		buildDir := filepath.Join(dir, "build")
		if err := os.MkdirAll(buildDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(buildDir, "compile_commands.json"), []byte("[]"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Backend{workDir: dir}
}

func TestX3_RenameFailClosedWithoutCompileDb(t *testing.T) {
	b := bareTestBackend(t, false)

	res, err := b.Rename(t.Context(), languages.RenameRequest{
		URI: "file:///w/a.cpp", Content: []byte("int main(){}"), NewName: "x",
	})
	if err != nil {
		t.Fatalf("rename gate must be a semantic refusal, not an error: %v", err)
	}
	if res.Status != identity.ResultUnavailable {
		t.Errorf("status = %v, want unavailable", res.Status)
	}
	if len(res.InternalDiagnostics) == 0 || !strings.Contains(res.InternalDiagnostics[0], "compile_commands.json") {
		t.Errorf("diagnostics must name the missing compile database, got %v", res.InternalDiagnostics)
	}
	for _, ev := range res.Evidence {
		if ev.DetailCode != "no-compile-commands" {
			t.Errorf("evidence detail = %q, want no-compile-commands", ev.DetailCode)
		}
	}
}

func TestX3_CompileDbDetectedWhenPresent(t *testing.T) {
	// The positive gate is unit-level: presence flips the check; the full
	// rename path then proceeds to clangd (covered by integration, X9).
	b := bareTestBackend(t, true)
	if !b.compileDbPresent() {
		t.Fatal("compile database present but not detected")
	}
	bare := bareTestBackend(t, false)
	if bare.compileDbPresent() {
		t.Error("no database on disk but detection returned true")
	}
}

func TestX3_HeaderAmbiguitySurfaced(t *testing.T) {
	cases := map[string]bool{
		"file:///w/a.hpp": true,
		"file:///w/a.h":   true,
		"file:///w/a.HPP": true,
		"file:///w/a.cpp": false,
		"file:///w/a.cxx": false,
	}
	for uri, want := range cases {
		if got := len(headerAmbiguityDiag(uri)) > 0; got != want {
			t.Errorf("headerAmbiguityDiag(%s) = %v, want %v", uri, got, want)
		}
	}
}

func TestX3_MacroSuspectSurfaced(t *testing.T) {
	src := []byte("int main() {\n  MAX_BUFFER_SIZE;\n  int local = 1;\n  return 0;\n}\n")

	cases := []struct {
		line, col uint32
		want      bool
	}{
		{1, 2, true},   // MAX_BUFFER_SIZE
		{1, 16, true},  // inside the ident, tail
		{2, 6, false},  // local — lower case
		{3, 2, false},  // return keyword
		{0, 0, false},  // int
		{99, 0, false}, // out of range line
	}
	for _, c := range cases {
		got := len(macroSuspectDiag(src, c.line, c.col)) > 0
		if got != c.want {
			t.Errorf("macroSuspectDiag(line=%d col=%d) = %v, want %v (ident %q)",
				c.line, c.col, got, c.want, identAt(src, c.line, c.col))
		}
	}

	// The diagnostic rides on successful hovers without changing status.
	diag := macroSuspectDiag([]byte("#define FOO(x) x\nFOO(1)"), 1, 0)
	if len(diag) != 1 || diag[0] != "possible-macro-expansion-site" {
		t.Errorf("diag = %v, want [possible-macro-expansion-site]", diag)
	}
}
