package golang

// round23 C: the fingerprint hit path pays one SHA-256 per closure input per
// request (measured: ~160ms for a fmt-importing main with a 65-package
// closure — the whole hit-path cost). packageDigestCache memoizes per-file
// digests keyed by (path, size, mtime), scoped to toolchain-managed roots
// (GOROOT, GOMODCACHE) so the pinned workspace invariant —
// TestPackageCacheRevalidatesSameSizeSameModTimeDependencyEdit — keeps
// catching size+mtime-preserving workspace edits at full strength. These
// pins hold the scoping, the directory-listing stat path, and the disclosed
// residual trade in place.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDigestCacheReuseScopedToToolchainRoots(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "proj")
	toolchain := filepath.Join(t.TempDir(), "toolchain")
	c := newPackageDigestCache()
	if c.reusable(filepath.Join(workspace, "dep.go")) {
		t.Fatal("digest reuse enabled before any toolchain roots were set; workspace files would lose the same-size-same-mtime revalidation pin")
	}
	c.setToolchainRoots(toolchain, "")
	if !c.reusable(filepath.Join(toolchain, "src", "fmt", "print.go")) {
		t.Fatal("digest reuse disabled for a file inside the configured toolchain root")
	}
	if c.reusable(filepath.Join(workspace, "main.go")) {
		t.Fatal("digest reuse enabled for a workspace file outside the toolchain roots")
	}
	// Prefix safety: c:\go must not match c:\go-evil\....
	evil := toolchain + "-evil"
	if c.reusable(filepath.Join(evil, "x.go")) {
		t.Fatalf("root %q matched sibling path %q — prefix check misses the separator", toolchain, evil)
	}
}

// Documents the disclosed residual inside toolchain roots: a same-size,
// same-mtime rewrite there reuses the old digest by design. Workspace files
// never reach this path (scope pin above + the freshness invariant test).
func TestDigestCacheReusesDigestUnderIdenticalStatInsideRoots(t *testing.T) {
	root := t.TempDir()
	c := newPackageDigestCache()
	c.setToolchainRoots(root, "")
	file := filepath.Join(root, "print.go")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	c.remember(file, before, "sha256:first")
	if got, ok := c.reuse(file); !ok || got != "sha256:first" {
		t.Fatalf("first reuse = (%q, %v), want the stored digest", got, ok)
	}
	if err := os.WriteFile(file, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != before.Size() || !info.ModTime().Equal(before.ModTime()) {
		t.Fatalf("fixture drifted: stat signature changed")
	}
	if got, ok := c.reuse(file); !ok || got != "sha256:first" {
		t.Fatalf("same-stat rewrite returned (%q, %v); within toolchain roots the memo serves the stale digest by design (disclosed residual)", got, ok)
	}
}

// A file rewritten between the pre-read stat and the post-read stat has
// content matching neither; remember must refuse to pin either stat.
func TestDigestCacheRememberRejectsFileChangedDuringRead(t *testing.T) {
	root := t.TempDir()
	c := newPackageDigestCache()
	c.setToolchainRoots(root, "")
	file := filepath.Join(root, "print.go")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the mid-read rewrite: bigger file after the read than before.
	if err := os.WriteFile(file, []byte("one-two-three"), 0o644); err != nil {
		t.Fatal(err)
	}
	c.remember(file, before, "sha256:mixed")
	if _, ok := c.reuse(file); ok {
		t.Fatal("digest pinned although the file changed between the pre- and post-read stats")
	}
}

// The directory listing (one ReadDir per package directory) must produce the
// same digest-cache key a direct os.Stat would, so reuseStat via listings and
// reuse via stat agree.
func TestDigestCacheListingKeyMatchesDirectStat(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "print.go")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	listings := readDirectoryListings(t.Context(), map[string]string{"k": dir})
	if _, ok := listings.digestKey(filepath.Join(dir, "print.go")); !ok {
		t.Fatal("listing has no stat for a regular file it listed")
	}
	if _, ok := listings.digestKey(filepath.Join(dir, "absent.go")); ok {
		t.Fatal("listing produced a stat key for a file it does not contain")
	}
	listing := listings.listingFor(dir)
	if listing == nil {
		t.Fatal("listingFor returned nil for the listed directory")
	}
	if _, ok := listing.types["print.go"]; !ok {
		t.Fatal("listing.types lost the regular entry hashPackageDirectory hashes")
	}
}
