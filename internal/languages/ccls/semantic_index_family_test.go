package ccls

// Family-key evidence for the explicit cpp/c semantic index registration
// (behavior-authorization round). cmd/omnilsp now registers the ccls exporter
// capability under both the canonical "cpp" key and the "c" alias key. The
// server resolves both keys to this capability, but a "c"-keyed dirty overlay
// export validates scope language against the requesting document language —
// so the safe fallback for .c/.h documents depends on this planner fact: the
// clang planner derives EVERY scope from the canonical backend identity
// (Language "cpp"), never from the per-document language. This test pins that
// fact on a real compile database containing a C translation unit.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestCclsBackendPlansCTranslationUnitsUnderCanonicalCppFamily drives
// BuildIndexRequest over a compile database with a .c TU and asserts the
// derived scope keeps the canonical cpp family language on the scope and its
// provenance, so alias-key binding resolution cannot fork a second semantic
// family for C sources.
func TestCclsBackendPlansCTranslationUnitsUnderCanonicalCppFamily(t *testing.T) {
	compiler := pinnedLLVM22ClangPlus(t)
	logicalRoot := t.TempDir()
	snapshotRoot := t.TempDir()
	files := map[string][]byte{
		"lib.h": []byte("int shared_value(void);\n"),
		"main.c": []byte(`#include "lib.h"
int shared_value(void) { return 42; }
int main(void) { return shared_value() != 42; }
`),
	}
	args := []string{compiler, "-std=c17", "-I", logicalRoot, "-c",
		filepath.Join(logicalRoot, "main.c"), "-o", filepath.Join(logicalRoot, "main.obj")}
	db, err := json.Marshal([]map[string]any{
		{"directory": logicalRoot, "file": filepath.Join(logicalRoot, "main.c"), "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	files["compile_commands.json"] = db
	view := newIndexCoverageView(t, logicalRoot, snapshotRoot, files)
	backend := newCclsProtocolBackendOnly(t)
	request, err := backend.BuildIndexRequest(context.Background(), view, view.rootURI)
	if err != nil {
		t.Fatalf("BuildIndexRequest over a C translation unit: %v", err)
	}
	if len(request.Scopes) != 1 {
		t.Fatalf("C translation unit derived %d scopes, want exactly one cpp-family scope", len(request.Scopes))
	}
	scope := request.Scopes[0]
	if scope.Language != "cpp" {
		t.Fatalf("scope language = %q, want the canonical cpp family key", scope.Language)
	}
	provenance, ok := request.Provenance[scope.ID]
	if !ok {
		t.Fatalf("scope %q has no provenance", scope.ID)
	}
	if provenance.Backend.Language != "cpp" || provenance.Backend.Name != "ccls" {
		t.Fatalf("scope %q binds backend %+v, want the canonical cpp/ccls identity", scope.ID, provenance.Backend)
	}
	patterns := strings.Join(scope.Build.PackagePatterns, " ")
	if !strings.Contains(patterns, "main.c") {
		t.Fatalf("cpp-family scope package patterns %q do not cover the C translation unit", patterns)
	}
}
