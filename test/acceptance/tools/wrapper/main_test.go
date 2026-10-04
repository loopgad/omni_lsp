package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockedNodePathUsesConfiguredExecutableAndHash(t *testing.T) {
	nodePath := filepath.Join(t.TempDir(), "node.exe")
	contents := []byte("pinned node fixture")
	if err := os.WriteFile(nodePath, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	t.Setenv("OMNILSP_ACCEPTANCE_LOCKED_TOOLS", "1")
	t.Setenv("OMNILSP_ACCEPTANCE_NODE", nodePath)
	t.Setenv("OMNILSP_ACCEPTANCE_NODE_SHA256", hex.EncodeToString(digest[:]))

	got, err := lockedNodePath()
	if err != nil {
		t.Fatalf("locked Node path rejected: %v", err)
	}
	if got != nodePath {
		t.Fatalf("locked Node path = %q, want %q", got, nodePath)
	}

	t.Setenv("OMNILSP_ACCEPTANCE_NODE_SHA256", strings.Repeat("0", sha256.Size*2))
	if _, err := lockedNodePath(); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("wrong Node SHA-256 was accepted: %v", err)
	}
}

func TestLockedNodePathDoesNotFallBackToPATHInAcceptance(t *testing.T) {
	t.Setenv("OMNILSP_ACCEPTANCE_LOCKED_TOOLS", "1")
	t.Setenv("OMNILSP_ACCEPTANCE_NODE", "")
	t.Setenv("OMNILSP_ACCEPTANCE_NODE_SHA256", "")
	if _, err := lockedNodePath(); err == nil {
		t.Fatal("locked acceptance unexpectedly fell back to PATH for Node")
	}
}
