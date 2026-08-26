package plugin

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiscover_ScansAndToleratesBroken pins Discover's contract (§O3):
// only directories with a plugin.json are considered; a broken manifest is
// reported per-entry via VerifyErr without aborting the scan; results are
// sorted by dir so discovery order is deterministic.
func TestDiscover_ScansAndToleratesBroken(t *testing.T) {
	root := t.TempDir()

	good := newPluginDir(t) // valid plugin in its own temp dir — move under root
	if err := os.Rename(good, filepath.Join(root, "good")); err != nil {
		t.Skipf("rename across volumes: %v", err)
	}

	// A directory with no manifest: silently skipped.
	if err := os.MkdirAll(filepath.Join(root, "nomanifest"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory with a corrupt manifest: surfaced, not fatal.
	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "plugin.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A plain file at top level: ignored.
	if err := os.WriteFile(filepath.Join(root, "loose.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := NewManager().Discover(root)
	if len(got) != 2 {
		t.Fatalf("discover = %d entries, want 2", len(got))
	}
	// Sorted by dir: "broken" < "good".
	if got[0].Dir != broken {
		t.Errorf("first entry should be the broken one, got %s", got[0].Dir)
	}
	if got[0].VerifyErr == nil {
		t.Error("broken manifest must carry VerifyErr")
	}
	if got[1].VerifyErr != nil || got[1].Manifest.ID != "demo.plugin" {
		t.Errorf("good entry wrong: verifyErr=%v id=%q", got[1].VerifyErr, got[1].Manifest.ID)
	}

	// Missing root: nil, no panic.
	if r := NewManager().Discover(filepath.Join(root, "does-not-exist")); r != nil {
		t.Errorf("missing root should return nil, got %d", len(r))
	}
}
