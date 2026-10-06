package ccls

// X3 policy tests: compile-database gating for rename (fail-closed) and
// header-context ambiguity surfacing. No real clangd required — the gates
// run before any RPC is issued.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

// bareTestBackend builds a Backend with no process attached: the fail-closed gates
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

func TestX3_CompileCommandsDirectorySelection(t *testing.T) {
	tests := []struct {
		name       string
		rootDB     bool
		buildDB    bool
		wantDir    string
		wantExists bool
	}{
		{name: "workspace root", rootDB: true, wantDir: "root", wantExists: true},
		{name: "build fallback", buildDB: true, wantDir: "build", wantExists: true},
		{name: "missing", wantExists: false},
		{name: "root takes precedence", rootDB: true, buildDB: true, wantDir: "root", wantExists: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			writeDB := func(dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "compile_commands.json"), []byte("[]"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.rootDB {
				writeDB(workDir)
			}
			if tt.buildDB {
				writeDB(filepath.Join(workDir, "build"))
			}

			wantDir := ""
			switch tt.wantDir {
			case "root":
				wantDir = workDir
			case "build":
				wantDir = filepath.Join(workDir, "build")
			}
			gotDir, gotExists := resolveCompileCommandsDir(workDir)
			if gotDir != wantDir || gotExists != tt.wantExists {
				t.Fatalf("resolveCompileCommandsDir() = (%q, %t), want (%q, %t)", gotDir, gotExists, wantDir, tt.wantExists)
			}

			backend := &Backend{workDir: workDir}
			if got := backend.compileDbPresent(); got != tt.wantExists {
				t.Errorf("compileDbPresent() = %t, want %t", got, tt.wantExists)
			}

			gotCompileDirArgs := []string{}
			for _, arg := range clangdArgs(workDir) {
				if strings.HasPrefix(arg, "--compile-commands-dir=") {
					gotCompileDirArgs = append(gotCompileDirArgs, arg)
				}
			}
			wantCompileDirArgs := []string{}
			if wantDir != "" {
				wantCompileDirArgs = append(wantCompileDirArgs, "--compile-commands-dir="+wantDir)
			}
			if len(gotCompileDirArgs) != len(wantCompileDirArgs) ||
				(len(wantCompileDirArgs) == 1 && gotCompileDirArgs[0] != wantCompileDirArgs[0]) {
				t.Errorf("clangd compile database args = %v, want %v", gotCompileDirArgs, wantCompileDirArgs)
			}
		})
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

func TestReferencesDeduplicateCanonicalLocations(t *testing.T) {
	var raw lspLocationList
	if err := json.Unmarshal([]byte(`[
		{"uri":"file:///w/a.cpp","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}},
		{"uri":"file:///w/a.cpp","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}},
		{"uri":"file:///w/a.cpp","range":{"start":{"line":1,"character":3},"end":{"line":1,"character":5}}},
		{"uri":"file:///w/b.cpp","range":{"start":{"line":1,"character":2},"end":{"line":1,"character":5}}},
		{"uri":"file:///C:/w/drive.cpp","range":{"start":{"line":0,"character":4},"end":{"line":0,"character":9}}},
		{"uri":"file:///c:/w/drive.cpp","range":{"start":{"line":0,"character":4},"end":{"line":0,"character":9}}}
	]`), &raw); err != nil {
		t.Fatal(err)
	}

	all := toLocations(raw)
	if len(all) != 6 {
		t.Fatalf("toLocations collapsed upstream duplicates: got %d, want 6", len(all))
	}
	deduped := toReferenceLocations(raw)
	if len(deduped) != 4 {
		t.Fatalf("toReferenceLocations() = %d, want 4", len(deduped))
	}
	for i := range deduped {
		for j := i + 1; j < len(deduped); j++ {
			if deduped[i] == deduped[j] {
				t.Fatalf("duplicate canonical reference locations remain: %+v", deduped)
			}
		}
	}
	if deduped[0].URI != "file:///w/a.cpp" || deduped[1].Range.StartCharacter != 3 || deduped[2].URI != "file:///w/b.cpp" || deduped[3].URI != "file:///C:/w/drive.cpp" {
		t.Fatalf("deduped order/data = %+v", deduped)
	}
}
