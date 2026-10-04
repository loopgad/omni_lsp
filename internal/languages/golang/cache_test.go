package golang

import (
	"context"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
)

// TestK0_PackageCacheHits verifies the §K1 cache serves identical inputs from
// the package cache (S18: hot definition/hover must not re-run packages.Load).
func TestK0_PackageCacheHits(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	uri := writeGoFile(t, b, "main.go", "package main\n\nfunc main() {}\n")
	src := []byte("package main\n\nfunc main() {}\n")

	req := languages.DefinitionRequest{URI: uri, Content: src, Line: 2, Column: 6}
	if _, err := b.Definition(context.Background(), req); err != nil {
		t.Fatalf("first definition: %v", err)
	}
	misses := b.cacheMisses.Load()
	if misses == 0 {
		t.Fatal("expected at least one cache miss on first query")
	}
	if _, err := b.Definition(context.Background(), req); err != nil {
		t.Fatalf("second definition: %v", err)
	}
	if b.cacheHits.Load() == 0 {
		t.Error("identical repeat query must hit the package cache")
	}
	beforeRevisionChange := b.cacheMisses.Load()
	if _, err := b.Definition(context.Background(), languages.DefinitionRequest{
		URI: uri, Content: src, SnapshotRev: 2, Line: 2, Column: 6,
	}); err != nil {
		t.Fatalf("new snapshot definition: %v", err)
	}
	if b.cacheMisses.Load() <= beforeRevisionChange {
		t.Error("new snapshot revision must not reuse the old package cache entry")
	}

	// Different content ⇒ different key ⇒ miss (no stale reuse).
	if _, err := b.Definition(context.Background(), languages.DefinitionRequest{
		URI: uri, Content: []byte("package main\n\nfunc changed() {}\n"), Line: 2, Column: 6,
	}); err != nil {
		t.Fatalf("changed-content definition: %v", err)
	}
	if b.cacheMisses.Load() <= misses {
		t.Error("changed content must not be served from the old cache entry")
	}
}

func TestK0_FileSetAndPackageCacheRotateAcrossRevisions(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	uri := writeGoFile(t, b, "main.go", "package main\n\nvar target int\n")
	src := []byte("package main\n\nvar target int\n")
	if _, err := b.Definition(context.Background(), languages.DefinitionRequest{URI: uri, Content: src, SnapshotRev: 1, Line: 2, Column: 5}); err != nil {
		t.Fatalf("initial definition: %v", err)
	}
	oldFset := b.fset
	if len(b.pkgCache) == 0 {
		t.Fatal("expected the first revision to populate the package cache")
	}

	// Model a FileSet generation at its configured high-water mark. The next
	// document revision must drop old ASTs and start a fresh position table.
	b.fsetBase = b.fset.Base() - maxPackageFsetBytes
	if _, err := b.Definition(context.Background(), languages.DefinitionRequest{URI: uri, Content: src, SnapshotRev: 2, Line: 2, Column: 5}); err != nil {
		t.Fatalf("next-revision definition: %v", err)
	}
	if b.fset == oldFset {
		t.Fatal("FileSet was retained after its memory budget was reached")
	}
	if len(b.pkgCache) != 1 || len(b.pkgOrder) != 1 {
		t.Fatalf("package cache after rotation = %d entries, order %d; want only the new revision", len(b.pkgCache), len(b.pkgOrder))
	}
}
