package vfs

import (
	"errors"
	"fmt"
	"testing"
)

// Guards for audit circle 2 finding 2: a session could open unboundedly many
// documents, and every didOpen/didChange republishes the full snapshot
// (OpenFiles sort + docs map copy), so an open flood turned into O(N^2) CPU
// and unbounded memory. The client-facing editor surface is now capped
// fail-closed; server-internal sources (virtual/remote) stay exempt.

func TestVFSTryOpenEditorFloodHitsCap(t *testing.T) {
	v := New()
	for i := 0; i < 512; i++ {
		uri := fmt.Sprintf("file:///flood/%d.go", i)
		if err := v.TryOpen(uri, "go", int64(i+1), []byte("package p\n"), SourceEditor); err != nil {
			t.Fatalf("open %d rejected before the cap: %v", i, err)
		}
	}
	if got := v.OpenCount(); got != 512 {
		t.Fatalf("open count = %d, want 512", got)
	}
	err := v.TryOpen("file:///flood/512.go", "go", 513, []byte("package p\n"), SourceEditor)
	if !errors.Is(err, ErrTooManyOpenDocuments) {
		t.Fatalf("open 513 = %v, want ErrTooManyOpenDocuments (fail-closed cap)", err)
	}
	if got := v.OpenCount(); got != 512 {
		t.Fatalf("open count after the rejected open = %d, want 512 (state intact)", got)
	}

	// Freeing a slot allows the next open again.
	v.Close("file:///flood/0.go")
	if err := v.TryOpen("file:///flood/512.go", "go", 513, []byte("package p\n"), SourceEditor); err != nil {
		t.Fatalf("open after close = %v, want admitted", err)
	}
}

func TestVFSVirtualSourceExemptFromCap(t *testing.T) {
	v := New()
	for i := 0; i < 600; i++ {
		uri := fmt.Sprintf("file:///virtual/%d.go", i)
		if err := v.TryOpen(uri, "go", 1, []byte("package p\n"), SourceVirtual); err != nil {
			t.Fatalf("server-internal virtual open %d must not be capped: %v", i, err)
		}
	}
}

func TestVFSOpenCapIsConfigurable(t *testing.T) {
	v := New()
	v.SetMaxOpenDocuments(2)
	if err := v.TryOpen("file:///a.go", "go", 1, []byte("a"), SourceEditor); err != nil {
		t.Fatalf("open a: %v", err)
	}
	if err := v.TryOpen("file:///b.go", "go", 1, []byte("b"), SourceEditor); err != nil {
		t.Fatalf("open b: %v", err)
	}
	if err := v.TryOpen("file:///c.go", "go", 1, []byte("c"), SourceEditor); !errors.Is(err, ErrTooManyOpenDocuments) {
		t.Fatalf("open c = %v, want ErrTooManyOpenDocuments", err)
	}
}

func TestVFSReOpenExistingDocumentNeverTripsCap(t *testing.T) {
	v := New()
	v.SetMaxOpenDocuments(1)
	if err := v.TryOpen("file:///a.go", "go", 1, []byte("a"), SourceEditor); err != nil {
		t.Fatalf("open a: %v", err)
	}
	// Idempotent reopen of the same document is not a new open.
	if err := v.TryOpen("file:///a.go", "go", 1, []byte("a"), SourceEditor); err != nil {
		t.Fatalf("idempotent reopen = %v, want nil", err)
	}
	// A conflicting reopen still reports the duplicate-open error, not the cap.
	if err := v.TryOpen("file:///a.go", "go", 2, []byte("b"), SourceEditor); !errors.Is(err, ErrDuplicateOpen) {
		t.Fatalf("conflicting reopen = %v, want ErrDuplicateOpen", err)
	}
}
