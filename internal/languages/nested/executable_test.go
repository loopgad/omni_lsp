package nested

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestExecutableIdentityResolvesAndHashesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tool.exe")
	content := []byte("fixed executable bytes")
	if err := os.WriteFile(path, content, 0o700); err != nil {
		t.Fatal(err)
	}
	gotPath, gotHash, err := ExecutableIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(content)
	if gotPath != path || gotHash != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("identity = (%q,%q), want (%q,%q)", gotPath, gotHash, path, hex.EncodeToString(wantHash[:]))
	}
}

func TestExecutableIdentityRejectsDirectory(t *testing.T) {
	if _, _, err := ExecutableIdentity(t.TempDir()); err == nil {
		t.Fatal("directory was accepted as an executable")
	}
}
