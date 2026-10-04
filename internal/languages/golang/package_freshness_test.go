package golang

import (
	"context"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"golang.org/x/tools/go/packages"
)

func TestPackageCacheRevalidatesSameSizeSameModTimeDependencyEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()

	depContent := "package dep\n\ntype Selected struct { Old int }\n"
	depURI := writeGoFile(t, b, filepath.Join("dep", "dep.go"), depContent)
	mainContent := []byte("package main\n\nimport \"testmod/dep\"\n\nvar selected dep.Selected\n")
	mainURI := writeGoFile(t, b, "main.go", string(mainContent))
	captured := snapshot.New("go-package-cache-test", 1, map[string]snapshot.DocumentSnapshot{
		mainURI: {URI: mainURI, LanguageID: "go", Content: mainContent},
	})

	first, err := b.loadPackage(context.Background(), mainURI, mainContent, 1)
	if err != nil {
		t.Fatalf("initial package load: %v", err)
	}
	if got := selectedFieldName(t, first, "testmod/dep"); got != "Old" {
		t.Fatalf("initial dependency field = %q, want Old", got)
	}
	fingerprintBefore, err := b.SemanticInputFingerprint(context.Background(), captured, mainURI)
	if err != nil {
		t.Fatalf("SemanticInputFingerprint before dependency edit: %v", err)
	}
	if fingerprintBefore == "" {
		t.Fatal("complete initial package graph did not produce a semantic input fingerprint")
	}
	beforeMisses := b.cacheMisses.Load()
	depPath := uriToPath(depURI)
	beforeInfo, err := os.Stat(depPath)
	if err != nil {
		t.Fatalf("stat dependency before edit: %v", err)
	}
	updatedContent := strings.Replace(depContent, "Old", "New", 1)
	if len(updatedContent) != len(depContent) {
		t.Fatalf("test edit changed file size: %d -> %d", len(depContent), len(updatedContent))
	}
	if err := os.WriteFile(depPath, []byte(updatedContent), 0o644); err != nil {
		t.Fatalf("write dependency edit: %v", err)
	}
	if err := os.Chtimes(depPath, beforeInfo.ModTime(), beforeInfo.ModTime()); err != nil {
		t.Fatalf("restore dependency modification time: %v", err)
	}
	afterInfo, err := os.Stat(depPath)
	if err != nil {
		t.Fatalf("stat dependency after edit: %v", err)
	}
	if beforeInfo.Size() != afterInfo.Size() || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatalf("dependency metadata changed: before size/time %d/%s, after %d/%s", beforeInfo.Size(), beforeInfo.ModTime(), afterInfo.Size(), afterInfo.ModTime())
	}
	fingerprintAfter, err := b.SemanticInputFingerprint(context.Background(), captured, mainURI)
	if err != nil {
		t.Fatalf("SemanticInputFingerprint after dependency edit: %v", err)
	}
	if fingerprintAfter == "" || fingerprintAfter == fingerprintBefore {
		t.Fatalf("dependency edit did not change semantic input fingerprint: before %q, after %q", fingerprintBefore, fingerprintAfter)
	}

	second, err := b.loadPackage(context.Background(), mainURI, mainContent, 1)
	if err != nil {
		t.Fatalf("package load after closed dependency edit: %v", err)
	}
	if second == first {
		t.Fatal("same snapshot and requested file reused the old package after a dependency content change")
	}
	if got := selectedFieldName(t, second, "testmod/dep"); got != "New" {
		t.Fatalf("reloaded dependency field = %q, want New", got)
	}
	if b.cacheMisses.Load() <= beforeMisses {
		t.Fatal("dependency content change did not cause a package cache miss")
	}
}

func selectedFieldName(t *testing.T, pkg *packages.Package, importPath string) string {
	t.Helper()
	dependency := pkg.Imports[importPath]
	if dependency == nil {
		t.Fatalf("package graph does not contain dependency %q", importPath)
	}
	if dependency.Types == nil {
		t.Fatal("dependency package has no type facts")
	}
	selected := dependency.Types.Scope().Lookup("Selected")
	if selected == nil {
		t.Fatal("dependency package does not define Selected")
	}
	structure, ok := selected.Type().Underlying().(*types.Struct)
	if !ok || structure.NumFields() != 1 {
		t.Fatalf("Selected underlying type = %T, want a one-field struct", selected.Type().Underlying())
	}
	return structure.Field(0).Name()
}

func TestWorkspaceSnapshotOverlaysGoPackageDependencies(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()

	targetContent := []byte("package main\nvar _ = fromSnapshot\n")
	dependencyOnDisk := "package main\nvar fromDisk = 1\n"
	dependencyOverlay := []byte("package main\nvar fromSnapshot = 1\n")
	targetURI := writeGoFile(t, b, "main.go", string(targetContent))
	dependencyURI := writeGoFile(t, b, "dependency.go", dependencyOnDisk)
	requestLine := "var _ = fromSnapshot"
	column := uint32(strings.Index(requestLine, "fromSnapshot"))
	captured := snapshot.New("go-package-cache-test", 1, map[string]snapshot.DocumentSnapshot{
		targetURI:     {URI: targetURI, LanguageID: "go", Content: targetContent},
		dependencyURI: {URI: dependencyURI, LanguageID: "go", Content: dependencyOverlay},
	})
	workspace := languages.WorkspaceSnapshot{
		Revision: 1,
		Documents: []languages.WorkspaceDocument{
			{URI: targetURI, LanguageID: "go", Content: targetContent},
			{URI: dependencyURI, LanguageID: "go", Content: dependencyOverlay},
		},
	}
	leaseCtx, finish, err := b.BeginWorkspaceSnapshot(context.Background(), workspace)
	if err != nil {
		t.Fatalf("BeginWorkspaceSnapshot: %v", err)
	}
	result, requestErr := b.Definition(leaseCtx, languages.DefinitionRequest{
		URI: targetURI, Content: targetContent, SnapshotRev: 1,
		Line: 1, Column: column,
	})
	finishErr := finish()
	if requestErr != nil {
		t.Fatalf("Definition: %v", requestErr)
	}
	if finishErr != nil {
		t.Fatalf("finish workspace snapshot: %v", finishErr)
	}
	if len(result.Value) != 1 || result.Value[0].URI != dependencyURI {
		t.Fatalf("definition did not use the dirty dependency overlay: status %v locations %+v", result.Status, result.Value)
	}

	fingerprint, err := b.SemanticInputFingerprint(context.Background(), captured, targetURI)
	if err != nil {
		t.Fatalf("SemanticInputFingerprint: %v", err)
	}
	if fingerprint == "" {
		t.Fatal("complete Go package graph did not produce a semantic input fingerprint")
	}
	if result.Status != identity.ResultPartial {
		t.Fatalf("definition status = %v, want partial compiler-backed package result", result.Status)
	}
}

func TestSemanticInputFingerprintWithoutLoadedPackageIsEmpty(t *testing.T) {
	b := newTestBackend(t)
	defer b.Close()
	content := []byte("package main\nvar target int\n")
	uri := writeGoFile(t, b, "main.go", string(content))
	captured := snapshot.New("go-package-cache-test", 1, map[string]snapshot.DocumentSnapshot{
		uri: {URI: uri, LanguageID: "go", Content: content},
	})
	fingerprint, err := b.SemanticInputFingerprint(context.Background(), captured, uri)
	if err != nil {
		t.Fatalf("SemanticInputFingerprint without cache: %v", err)
	}
	if fingerprint != "" {
		t.Fatalf("fingerprint before loading a package = %q, want empty (not memoizable)", fingerprint)
	}
}
