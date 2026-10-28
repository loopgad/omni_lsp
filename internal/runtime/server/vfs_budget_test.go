package server

import (
	"context"
	"testing"

	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// TestDidChangeImplicitOpenRespectsDocumentBudget pins the server-side wiring
// of the open-document budget (audit circle 2, finding 2): a didChange for a
// never-opened document would silently create VFS state and republish the
// snapshot — at the budget's edge that implicit open is refused fail-closed,
// while changes to already-open documents stay uncapped (the budget bounds
// distinct documents, not edits).
func TestDidChangeImplicitOpenRespectsDocumentBudget(t *testing.T) {
	ctx := context.Background()
	s := New(DefaultConfig())
	defer s.diag.Close()
	if _, err := s.CallMethod(ctx, "initialize", InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CallMethod(ctx, "initialized", nil); err != nil {
		t.Fatal(err)
	}
	s.vfs.SetMaxOpenDocuments(1)
	if err := s.vfs.TryOpen("file:///w/a.go", "go", 1, []byte("package a\n"), vfs.SourceEditor); err != nil {
		t.Fatalf("setup open: %v", err)
	}

	change := func(uri string, version int64) error {
		_, err := s.CallMethod(ctx, "textDocument/didChange", map[string]any{
			"textDocument": map[string]any{"uri": uri, "version": version},
			"contentChanges": []map[string]any{
				{"text": "package changed\n"},
			},
		})
		return err
	}

	// The open document stays editable: the budget bounds documents, not edits.
	if err := change("file:///w/a.go", 2); err != nil {
		t.Fatalf("didChange on the open document = %v, want admitted", err)
	}
	if got := s.vfs.Content("file:///w/a.go"); string(got) != "package changed\n" {
		t.Fatalf("open document content = %q, want the applied change", got)
	}

	// An implicit open through didChange is refused at the budget's edge.
	if err := change("file:///w/b.go", 1); err == nil {
		t.Fatal("didChange implicitly opening a new document at the budget edge was admitted")
	}
	if s.vfs.Get("file:///w/b.go") != nil {
		t.Fatal("rejected implicit open left VFS state behind")
	}
}
