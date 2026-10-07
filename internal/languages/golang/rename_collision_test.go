package golang

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

// TestRenameCollisionDetection pins the §I11 collision half: renaming onto an
// identifier already bound in an enclosing scope must fail closed with a
// decisive report, while a free name proceeds.
func TestRenameCollisionDetection(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	uri := workspaceuri.FromPath(filepath.Join(dir, "main.go")).String()

	src := "package main\n\nfunc main() {\n\ttotal := 1\n\t_ = total\n}\n"
	if werr := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module w\n\ngo 1.21\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	if werr := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); werr != nil {
		t.Fatal(werr)
	}
	line := uint32(3) // zero-based: `total := 1`
	col := uint32(strings.Index("\ttotal := 1", "total"))

	t.Run("collision refused", func(t *testing.T) {
		res, err := b.Rename(context.Background(), languages.RenameRequest{
			URI: uri, Content: []byte(src),
			SnapshotRev: 1, Line: line, Column: col,
			NewName: "main", // already bound at package scope
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != identity.ResultUnavailable || len(res.Value.Edits) != 0 {
			t.Fatalf("expected refusal, got status=%v value=%+v diags=%v", res.Status, res.Value, res.InternalDiagnostics)
		}
		found := false
		for _, d := range res.InternalDiagnostics {
			if strings.Contains(d, `"main" already names a function`) {
				found = true
			}
		}
		if !found {
			t.Errorf("collision report missing: %v", res.InternalDiagnostics)
		}
	})

	t.Run("free name allowed", func(t *testing.T) {
		res, err := b.Rename(context.Background(), languages.RenameRequest{
			URI: uri, Content: []byte(src),
			SnapshotRev: 1, Line: line, Column: col,
			NewName: "grandTotal",
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status == identity.ResultUnavailable {
			t.Fatalf("free name refused: %v", res.InternalDiagnostics)
		}
		if len(res.Value.Edits) < 2 {
			t.Fatalf("expected edits for decl+use, got %+v", res.Value)
		}
	})
}
