package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdIndexRejectsUnboundOrUnsupportedImports(t *testing.T) {
	if err := cmdIndex([]string{"import", "--format", "lsif", "--scope", "scope"}); err == nil || !strings.Contains(err.Error(), "unsupported format") {
		t.Fatalf("unsupported index format error = %v", err)
	}
	if err := cmdIndex([]string{"import", "--format", "scip", "--input", "input.scip", "--language", "go"}); err == nil || !strings.Contains(err.Error(), "--scope is required") {
		t.Fatalf("missing scope error = %v", err)
	}
	if err := cmdIndex([]string{"import", "--format", "scip", "--scope", "scope", "--input", "input.scip"}); err == nil || !strings.Contains(err.Error(), "requires --language") {
		t.Fatalf("missing language error = %v", err)
	}
}

func TestSCIPFileHelpersBoundReadsAndRefuseOverwrite(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.scip")
	if err := os.WriteFile(input, []byte("scip-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readBoundedSCIP(input)
	if err != nil || string(got) != "scip-bytes" {
		t.Fatalf("readBoundedSCIP = %q, %v", got, err)
	}
	empty := filepath.Join(dir, "empty.scip")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedSCIP(empty); err == nil {
		t.Fatal("empty SCIP input was accepted")
	}

	output := filepath.Join(dir, "output.scip")
	if err := writeNewFile(output, []byte("first")); err != nil {
		t.Fatalf("writeNewFile: %v", err)
	}
	if err := writeNewFile(output, []byte("second")); err == nil {
		t.Fatal("writeNewFile overwrote an existing file")
	}
	contents, err := os.ReadFile(output)
	if err != nil || string(contents) != "first" {
		t.Fatalf("existing export changed: %q, %v", contents, err)
	}
}

func TestValidateArtifactPathOutsideWorkspace(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "artifacts")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}

	for _, candidate := range []string{
		filepath.Join(workspace, "index.scip"),
		filepath.Join(workspace, "nested", "..", "index.scip"),
	} {
		if _, err := validateArtifactPathOutsideWorkspace(workspace, candidate); err == nil {
			t.Errorf("accepted workspace artifact path %q", candidate)
		}
	}

	candidate := filepath.Join(outside, "index.scip")
	got, err := validateArtifactPathOutsideWorkspace(workspace, candidate)
	if err != nil {
		t.Fatalf("rejected external artifact path: %v", err)
	}
	want, err := filepath.Abs(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved external artifact path = %q, want %q", got, want)
	}
}

func TestValidateArtifactPathOutsideWorkspaceResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	insideFile := filepath.Join(workspace, "index.scip")
	if err := os.WriteFile(insideFile, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(outside, "linked.scip")
	if err := os.Symlink(insideFile, link); err != nil {
		t.Skipf("file symlink creation is unavailable: %v", err)
	}
	if _, err := validateArtifactPathOutsideWorkspace(workspace, link); err == nil {
		t.Fatal("accepted symlink resolving inside workspace")
	}
}
