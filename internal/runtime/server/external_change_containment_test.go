package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

// TestExternalChangesConfinedToWorkspaceRoot executes the §N7 containment
// policy on the external-change funnel (audit circle 2, finding 3): before
// the funnel queues anything for the child backends, every client-supplied
// file URI must resolve inside the session's workspace root. Pre-fix the
// repo's own ResolveInWorkspace had zero production callers and URIs from
// outside the root (and cross-host UNC authorities) were forwarded to the
// child processes verbatim.
func TestExternalChangesConfinedToWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	ctx := context.Background()
	s := New(DefaultConfig())
	defer s.diag.Close()
	backend := &sourceChangeBackend{mockBackend: &mockBackend{langID: "go", exts: []string{".go"}}, epoch: 1}
	s.RegisterBackend("go", backend)
	rootURI := workspaceuri.FromPath(root).String()
	if _, err := s.CallMethod(ctx, "initialize", InitializeParams{RootURI: rootURI}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CallMethod(ctx, "initialized", nil); err != nil {
		t.Fatal(err)
	}

	inRoot := filepath.Join(root, "dependency.go")
	if err := os.WriteFile(inRoot, []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "elsewhere.go"), []byte("package p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	change := func(uri string) []languages.SourceChange {
		return []languages.SourceChange{{URI: uri, Kind: languages.SourceChangeChanged}}
	}

	// An in-root change is delivered to the backend synchronizer.
	if err := s.applyExternalSourceChanges(ctx, change(workspaceuri.FromPath(inRoot).String())); err != nil {
		t.Fatalf("in-root external change was refused: %v", err)
	}
	if err := s.flushExternalSourceChanges(ctx, backend); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(backend.changes) == 0 {
		t.Fatal("in-root change never reached the backend")
	}

	refused := func(name, uri string) {
		t.Helper()
		if err := s.applyExternalSourceChanges(ctx, change(uri)); err == nil {
			t.Fatalf("%s was admitted past the workspace boundary: %s", name, uri)
		}
	}
	refused("out-of-root path", workspaceuri.FromPath(filepath.Join(outside, "elsewhere.go")).String())
	refused("cross-host UNC authority", "file://evil-host/share/x.go")
	refused("parent traversal", workspaceuri.FromPath(filepath.Join(root, "..", "sibling.go")).String())

	// A mixed batch is refused whole — no partial admission.
	mixed := append(change(workspaceuri.FromPath(inRoot).String()),
		languages.SourceChange{URI: workspaceuri.FromPath(filepath.Join(outside, "elsewhere.go")).String(), Kind: languages.SourceChangeChanged})
	if err := s.applyExternalSourceChanges(ctx, mixed); err == nil {
		t.Fatal("mixed in-root/out-of-root batch was partially admitted")
	}
}
