package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

func TestDiskSemanticViewVerifiesCapturedContentAndMaterializesSubtree(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(filepath.Join(workspace, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".tmp-gocache"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := []byte("package sub\nvar Value = \"🙂\"\n")
	if err := os.WriteFile(filepath.Join(workspace, "sub", "source.go"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".git", "ignored.go"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".tmp-gocache", "build.cache"), []byte("volatile"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(workspace).Canonical()
	view, err := captureSemanticView(ctx, workspace, identity.WorkspaceID(rootURI), 9, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = view.Close() })
	if len(view.files) != 1 {
		t.Fatalf("captured files = %d, want only the workspace source", len(view.files))
	}
	diskDigest, err := semanticDiskDigest(ctx, workspace, "")
	if err != nil || diskDigest != view.Identity().DiskDigest {
		t.Fatalf("disk digest = %q, %v; captured identity = %q", diskDigest, err, view.Identity().DiskDigest)
	}
	read, err := view.Read(ctx, view.files[0].URI)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(read)
	_ = read.Close()
	if err != nil || string(got) != string(want) {
		t.Fatalf("captured read = %q, %v", got, err)
	}

	subURI := uri.FromPath(filepath.Join(workspace, "sub")).Canonical()
	materialized, err := view.Materialize(ctx, subURI, filepath.Join(base, "materialized"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := materialized.PathForURI(view.files[0].URI)
	if err != nil {
		t.Fatal(err)
	}
	materializedBytes, err := os.ReadFile(path)
	if err != nil || string(materializedBytes) != string(want) {
		t.Fatalf("materialized source = %q, %v", materializedBytes, err)
	}
	if writable, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0); err == nil {
		_ = writable.Close()
		t.Fatal("captured materialized source is writable")
	}
	if _, err := materialized.PathForURI(uri.FromPath(filepath.Join(workspace, "outside.go")).Canonical()); err == nil {
		t.Fatal("PathForURI accepted a file that was not in the captured subtree")
	}
	materializedRoot := materialized.RootPath()
	if err := materialized.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(materializedRoot); err != nil {
		t.Fatalf("closing a borrowed materialized view removed its captured snapshot: %v", err)
	}

	if err := os.WriteFile(filepath.Join(workspace, "sub", "source.go"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	changedDigest, err := semanticDiskDigest(ctx, workspace, "")
	if err != nil || changedDigest == view.Identity().DiskDigest {
		t.Fatalf("disk digest after mutation = %q, %v; want it to differ from captured digest", changedDigest, err)
	}
	read, err = view.Read(ctx, view.files[0].URI)
	if err != nil {
		t.Fatalf("Read failed after the source changed: %v", err)
	}
	got, err = io.ReadAll(read)
	_ = read.Close()
	if err != nil || string(got) != string(want) {
		t.Fatalf("Read after source mutation = %q, %v; want the captured bytes", got, err)
	}
	if _, err := view.Materialize(ctx, subURI, filepath.Join(base, "changed-materialized")); err != nil {
		t.Fatalf("Materialize failed after source mutation: %v", err)
	}
}

func TestDiskSemanticViewRejectsScopeOutsideWorkspace(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(workspace).Canonical()
	view, err := captureSemanticView(context.Background(), workspace, identity.WorkspaceID(rootURI), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = view.Close() })
	outside := uri.FromPath(filepath.Join(base, "outside")).Canonical()
	if err := view.Walk(context.Background(), outside, func(model.File) error { return nil }); err == nil {
		t.Fatal("Walk accepted a scope outside the captured workspace")
	}
	if _, err := view.Materialize(context.Background(), outside, filepath.Join(base, "snapshot")); err == nil {
		t.Fatal("Materialize accepted a scope outside the captured workspace")
	}
}
