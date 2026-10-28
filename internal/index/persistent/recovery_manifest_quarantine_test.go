package persistent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Guards the audit finding "a corrupt manifest.json is silently deleted and
// the delete failure is ignored": recover() used to `_ = os.Remove` an
// undecodable manifest, so the publish pointer vanished without any forensic
// residue while corrupt segments get quarantineDir evidence (§L8: decode /
// integrity failure → quarantine for diagnostics). The corrupt manifest must
// be moved into quarantine/ under a manifest-corrupt-* name, and a blocked
// move must fail the recovery loudly instead of silently continuing without
// evidence.

func findQuarantinedManifest(t *testing.T, s *FileStore) []byte {
	t.Helper()
	entries, err := os.ReadDir(s.quarantineDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, "manifest-corrupt-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.quarantineDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	t.Fatalf("no manifest-corrupt-*.json evidence in %s", s.quarantineDir())
	return nil
}

func TestRecoverQuarantinesCorruptManifest(t *testing.T) {
	for name, corrupt := range map[string][]byte{
		"undecodable": []byte("{not json"),
		// Parseable JSON that fails Generation validation.
		"invalid-generation": []byte(`{"generation": 0, "segments": []}`),
	} {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			publishOne(t, s, "survivor")

			// Corrupt the publish pointer behind the store's back.
			if err := os.WriteFile(s.manifestPath(), corrupt, 0o644); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(s.manifestPath())
			if err != nil {
				t.Fatal(err)
			}

			// recover must quarantine the corrupt manifest, not silently
			// remove it, and must not fail the open (history fallback).
			if err := s.recover(); err != nil {
				t.Fatalf("recover with corrupt manifest: %v", err)
			}
			if _, err := os.Stat(s.manifestPath()); !os.IsNotExist(err) {
				t.Fatalf("corrupt manifest still at root (stat err = %v)", err)
			}
			if got := findQuarantinedManifest(t, s); string(got) != string(raw) {
				t.Fatalf("quarantined manifest bytes differ from the corrupt file: got %q want %q", got, raw)
			}
		})
	}
}

// TestRecoverFailsClosedWhenManifestCannotBeQuarantined: if the corrupt
// manifest can be neither preserved nor moved (here: the quarantine directory
// path is occupied by a regular file), recovery must surface the failure
// instead of deleting the only diagnostic and carrying on.
func TestRecoverFailsClosedWhenManifestCannotBeQuarantined(t *testing.T) {
	s := newStore(t)
	publishOne(t, s, "survivor")

	if err := os.Remove(s.quarantineDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.quarantineDir(), []byte("occupied"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.manifestPath(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.recover(); err == nil {
		t.Fatal("recover must fail closed when the corrupt manifest cannot be moved to quarantine")
	}
}

// TestFreshStoreRecoversAfterManifestQuarantine: the quarantine move must
// leave the store openable — the next NewFileStore falls back through history
// instead of tripping over the pointer again.
func TestFreshStoreRecoversAfterManifestQuarantine(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	publishOne(t, s, "older-generation")
	publishOne(t, s, "newest-generation")

	if err := os.WriteFile(s.manifestPath(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := NewFileStore(dir, Config{})
	if err != nil {
		t.Fatalf("reopen after manifest quarantine: %v", err)
	}
	view, err := again.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot after manifest quarantine: %v", err)
	}
	if view.ID != 1 {
		t.Fatalf("fell back to generation %d, want the newest history survivor 1", view.ID)
	}
}
