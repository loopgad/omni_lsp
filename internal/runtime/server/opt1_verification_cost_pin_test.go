package server

// Pins for round 23 OPT-1: the persistent/overlay verification path must keep
// proving the workspace did not change under a request, but through the
// (path,size,mtime) stat recheck and the tool-hash verdict cache instead of a
// second full read+hash pass and per-call tool re-hashing.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func opt1TempWorkspace(t *testing.T) (root string, files map[string]string) {
	t.Helper()
	root = t.TempDir()
	files = map[string]string{
		"main.go":  "package main\n",
		"lib.go":   "package main\n\nfunc helper() {}\n",
		"docs.txt": "notes\n",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, files
}

func opt1Sha256Hex(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// TestSemanticDiskUnchangedSincePinsCapturedTree pins the OPT-1 stat recheck:
// unchanged trees pass, and every structural or stat change — content edit,
// file added, file deleted, mtime touched, symlink injected — fails closed.
func TestSemanticDiskUnchangedSincePinsCapturedTree(t *testing.T) {
	ctx := context.Background()
	root, _ := opt1TempWorkspace(t)

	view, err := captureSemanticView(ctx, root, "", 0, "")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	defer func() { _ = view.Close() }()

	if !semanticDiskUnchangedSince(ctx, view) {
		t.Fatal("unchanged tree reported as changed — stat recheck must accept the capture's own tree")
	}

	// Content edit (size change).
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main // edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if semanticDiskUnchangedSince(ctx, view) {
		t.Fatal("edited file (size change) not detected by the stat recheck")
	}

	// File added.
	if err := os.WriteFile(filepath.Join(root, "extra.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if semanticDiskUnchangedSince(ctx, view) {
		t.Fatal("added file not detected by the stat recheck")
	}
	if err := os.Remove(filepath.Join(root, "extra.go")); err != nil {
		t.Fatal(err)
	}

	// File deleted.
	if err := os.Remove(filepath.Join(root, "docs.txt")); err != nil {
		t.Fatal(err)
	}
	if semanticDiskUnchangedSince(ctx, view) {
		t.Fatal("deleted file not detected by the stat recheck")
	}
	if err := os.WriteFile(filepath.Join(root, "docs.txt"), []byte("notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Same content, mtime touched: fail-closed (conservative direction).
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "lib.go"), future, future); err != nil {
		t.Fatal(err)
	}
	if semanticDiskUnchangedSince(ctx, view) {
		t.Fatal("mtime-only touch not detected by the stat recheck (fail-closed required)")
	}

	// Symlink injected: the walk rejects it, the recheck fails closed.
	if err := os.Symlink(filepath.Join(root, "main.go"), filepath.Join(root, "link.go")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if semanticDiskUnchangedSince(ctx, view) {
		t.Fatal("injected symlink not detected by the stat recheck (fail-closed required)")
	}
}

// TestToolPathStillMatchesCachesByStatAndExpected pins the OPT-1 tool verdict
// cache: a (path,size,mtime,expected) hit reuses the observed hash without
// re-reading the file; any size change re-hashes and refreshes the entry; a
// changed expected set re-hashes; a mismatch verdict stays false.
func TestToolPathStillMatchesCachesByStatAndExpected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	toolPath := filepath.Join(dir, "tool.bin")
	const v1 = "tool-v1-bytes"
	if err := os.WriteFile(toolPath, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	h1 := opt1Sha256Hex(v1)

	if !toolPathStillMatches(ctx, toolPath, []string{h1}) {
		t.Fatal("first verification of the pinned tool must match")
	}
	toolHashVerdicts.Lock()
	entry, cached := toolHashVerdicts.entries[toolPath]
	toolHashVerdicts.Unlock()
	if !cached || entry.observed != h1 {
		t.Fatalf("tool hash verdict not cached after first verification: %+v (cached=%v)", entry, cached)
	}

	// Second verification: cache hit — the entry must survive unchanged.
	if !toolPathStillMatches(ctx, toolPath, []string{h1}) {
		t.Fatal("cached re-verification must match")
	}
	toolHashVerdicts.Lock()
	entry2, _ := toolHashVerdicts.entries[toolPath]
	toolHashVerdicts.Unlock()
	if entry2 != entry {
		t.Fatal("cache hit must not refresh the entry")
	}

	// Same size, different content: the registered residual window — the
	// cached verdict may stand. Verify the cache records what it observed
	// rather than claiming anything stronger than the stat signature.
	toolHashVerdicts.Lock()
	before := toolHashVerdicts.entries[toolPath]
	toolHashVerdicts.Unlock()
	if before.observed != h1 || before.size != int64(len(v1)) {
		t.Fatalf("unexpected cached verdict: %+v", before)
	}

	// Size change: stat signature differs → full re-hash → mismatch against
	// the stale expected hash, and the entry carries the NEW observed hash.
	const v2 = "tool-v2-bytes-longer"
	if err := os.WriteFile(toolPath, []byte(v2), 0o600); err != nil {
		t.Fatal(err)
	}
	if toolPathStillMatches(ctx, toolPath, []string{h1}) {
		t.Fatal("changed tool (size change) must not verify against the old hash")
	}
	toolHashVerdicts.Lock()
	entry3, cached3 := toolHashVerdicts.entries[toolPath]
	toolHashVerdicts.Unlock()
	if !cached3 || entry3.observed != opt1Sha256Hex(v2) {
		t.Fatalf("entry not refreshed after size change: %+v (cached=%v)", entry3, cached3)
	}
	if !toolPathStillMatches(ctx, toolPath, []string{opt1Sha256Hex(v2)}) {
		t.Fatal("changed tool must verify against its new hash")
	}

	// Different expected set: fingerprint differs → full re-hash path; a bogus
	// expected hash must yield a mismatch verdict.
	if toolPathStillMatches(ctx, toolPath, []string{strings.Repeat("ab", 32)}) {
		t.Fatal("bogus expected hash must not verify")
	}
	toolHashVerdicts.Lock()
	entry4, cached4 := toolHashVerdicts.entries[toolPath]
	toolHashVerdicts.Unlock()
	if !cached4 || entry4.observed != opt1Sha256Hex(v2) {
		t.Fatalf("entry must keep the observed (not the expected) hash: %+v (cached=%v)", entry4, cached4)
	}

	// Empty expected set and empty path refuse (fail-closed contract).
	if toolPathStillMatches(ctx, toolPath, nil) || toolPathStillMatches(ctx, "", []string{h1}) {
		t.Fatal("empty path or expected set must refuse")
	}
}
