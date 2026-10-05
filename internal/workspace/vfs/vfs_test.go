package vfs

import (
	"errors"
	"slices"
	"testing"
)

func TestOpenAndClose(t *testing.T) {
	v := New()
	uri := "file:///test.go"

	v.Open(uri, "go", 1, []byte("package main"), SourceEditor)

	f := v.Get(uri)
	if f == nil {
		t.Fatal("expected file to be open")
	}
	if f.URI != uri {
		t.Errorf("expected URI %s, got %s", uri, f.URI)
	}
	if f.LanguageID != "go" {
		t.Errorf("expected language go, got %s", f.LanguageID)
	}
	if !f.Dirty {
		t.Error("editor file should be dirty")
	}

	v.Close(uri)
	f = v.Get(uri)
	if f != nil {
		t.Error("file should be closed")
	}
}

func TestUpdate(t *testing.T) {
	v := New()
	uri := "file:///test.go"

	v.Open(uri, "go", 1, []byte("package main"), SourceEditor)
	v.Update(uri, 2, []byte("package main\nimport \"fmt\""))

	f := v.Get(uri)
	if f.Version != 2 {
		t.Errorf("expected version 2, got %d", f.Version)
	}
	if string(f.Content) != "package main\nimport \"fmt\"" {
		t.Errorf("unexpected content: %s", string(f.Content))
	}
}

func TestSave(t *testing.T) {
	v := New()
	uri := "file:///test.go"

	v.Open(uri, "go", 1, []byte("package main"), SourceEditor)
	if !v.Get(uri).Dirty {
		t.Error("should be dirty before save")
	}

	v.Save(uri)
	f := v.Get(uri)
	if f.Dirty {
		t.Error("should not be dirty after save")
	}
	if f.Source != SourceDisk {
		t.Errorf("expected source disk, got %d", f.Source)
	}
}

func TestContent(t *testing.T) {
	v := New()
	uri := "file:///test.go"
	content := []byte("package main")

	v.Open(uri, "go", 1, content, SourceDisk)

	got := v.Content(uri)
	if string(got) != string(content) {
		t.Errorf("expected %s, got %s", string(content), string(got))
	}
}

func TestOpenFiles(t *testing.T) {
	v := New()

	v.Open("file:///a.go", "go", 1, []byte("a"), SourceEditor)
	v.Open("file:///b.go", "go", 2, []byte("b"), SourceEditor)

	files := v.OpenFiles()
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
}

// TestOpenFilesAreSorted locks the ordering contract shared with
// snapshot.Snapshot.Documents(): the two lists are compared positionally to
// decide whether a captured snapshot still describes the live open set, so
// both sides must be ordered by the same key. Map iteration order is randomized
// per range — repeat so one lucky pass cannot hide a regression.
func TestOpenFilesAreSorted(t *testing.T) {
	v := New()
	want := []string{"file:///a.go", "file:///b.go", "file:///c.go", "file:///d.go", "file:///e.go"}
	for i, uri := range want {
		v.Open(uri, "go", int64(i+1), []byte("package main"), SourceEditor)
	}

	for i := range 32 {
		if got := v.OpenFiles(); !slices.Equal(got, want) {
			t.Fatalf("pass %d: OpenFiles() = %v, want sorted %v", i, got, want)
		}
	}
}

func TestRevision(t *testing.T) {
	v := New()

	if v.Revision() != 0 {
		t.Error("initial revision should be 0")
	}

	v.Open("file:///a.go", "go", 1, []byte("a"), SourceEditor)
	if v.Revision() != 1 {
		t.Errorf("expected revision 1, got %d", v.Revision())
	}

	v.Update("file:///a.go", 2, []byte("b"))
	if v.Revision() != 2 {
		t.Errorf("expected revision 2, got %d", v.Revision())
	}
}

func TestDiskOverlay(t *testing.T) {
	v := New()
	uri := "file:///test.go"

	v.Open(uri, "go", 1, []byte("disk content"), SourceDisk)
	v.Open(uri, "go", 2, []byte("editor content"), SourceEditor)

	f := v.Get(uri)
	if string(f.Content) != "editor content" {
		t.Errorf("editor should overlay disk, got %s", string(f.Content))
	}
}

func TestCanonicalIdentityAndTryOpen(t *testing.T) {
	v := New()
	displayURI := "file:///C:/test.go"
	aliasURI := "file:///c:/test.go"

	if err := v.TryOpen(displayURI, "go", 1, []byte("one"), SourceEditor); err != nil {
		t.Fatal(err)
	}
	if err := v.TryOpen(displayURI, "go", 1, []byte("one"), SourceEditor); err != nil {
		t.Fatalf("identical open should be idempotent: %v", err)
	}
	if got := v.Revision(); got != 1 {
		t.Fatalf("idempotent open changed revision: %d", got)
	}
	if err := v.TryOpen(displayURI, "go", 2, []byte("two"), SourceEditor); !errors.Is(err, ErrDuplicateOpen) {
		t.Fatalf("conflicting duplicate open error = %v", err)
	}
	if err := v.TryOpen(aliasURI, "go", 1, []byte("one"), SourceEditor); err != nil {
		t.Fatalf("identical canonical alias should be idempotent: %v", err)
	}
	if err := v.TryOpen(aliasURI, "go", 1, []byte("different"), SourceEditor); !errors.Is(err, ErrDuplicateOpen) {
		t.Fatalf("conflicting canonical URI alias error = %v", err)
	}
	if got := v.Get(aliasURI); got == nil || got.URI != displayURI || string(got.Content) != "one" {
		t.Fatalf("alias read did not preserve original document: %+v", got)
	}
	if got := v.OpenFiles(); len(got) != 1 || got[0] != displayURI {
		t.Fatalf("OpenFiles lost display spelling: %v", got)
	}

	v.Update(aliasURI, 2, []byte("updated"))
	if got := v.Content(displayURI); string(got) != "updated" {
		t.Fatalf("canonical update content = %q", got)
	}
	v.Save(aliasURI)
	if got := v.Get(displayURI); got == nil || got.Dirty {
		t.Fatalf("canonical save did not update state: %+v", got)
	}
	v.Close(aliasURI)
	if got := v.Get(displayURI); got != nil {
		t.Fatalf("canonical close left document open: %+v", got)
	}
}

func TestOpenRetainsLegacyUpdateBehavior(t *testing.T) {
	v := New()
	uri := "file:///test.go"
	v.Open(uri, "go", 1, []byte("one"), SourceEditor)
	v.Open(uri, "go", 2, []byte("two"), SourceEditor)
	if got := v.Get(uri); got == nil || got.Version != 2 || string(got.Content) != "two" {
		t.Fatalf("legacy Open did not replace state: %+v", got)
	}
}

func TestNonExistentFile(t *testing.T) {
	v := New()

	f := v.Get("file:///nonexistent.go")
	if f != nil {
		t.Error("non-existent file should return nil")
	}

	c := v.Content("file:///nonexistent.go")
	if c != nil {
		t.Error("non-existent file content should return nil")
	}
}

// TestAdvanceRevisionInvalidatesWithoutTouchingDocuments pins the external
// change contract. Unlike Open/Update/Save/Close, AdvanceRevision must move the
// revision counter and nothing else: the server keys its semantic memo tables
// and its overlay identity checks on vfs.Revision(), so bumping it is what
// retires answers computed from the previous disk state. Content and versions
// must survive byte-for-byte, because an external disk write must never
// overwrite an editor-authoritative buffer (D1 editor overlay precedence).
func TestAdvanceRevisionInvalidatesWithoutTouchingDocuments(t *testing.T) {
	v := New()
	const uri = "file:///main.go"
	v.Open(uri, "go", 7, []byte("package main"), SourceEditor)
	before := v.Revision()
	beforeFile := v.Get(uri)
	if beforeFile == nil {
		t.Fatal("fixture document missing")
	}

	got := v.AdvanceRevision()

	if got != before+1 {
		t.Fatalf("AdvanceRevision() = %d, want %d (strict +1)", got, before+1)
	}
	if v.Revision() != got {
		t.Fatalf("Revision() = %d, want %d", v.Revision(), got)
	}
	if got == before {
		t.Fatal("revision-keyed cache entries would survive; stale memo reused")
	}

	after := v.Get(uri)
	if after == nil {
		t.Fatal("AdvanceRevision dropped the document")
	}
	if after.Version != beforeFile.Version {
		t.Errorf("version changed: %d -> %d", beforeFile.Version, after.Version)
	}
	if string(after.Content) != string(beforeFile.Content) {
		t.Errorf("content changed: %q -> %q", beforeFile.Content, after.Content)
	}
	if after.Source != SourceEditor || !after.Dirty {
		t.Errorf("editor authority lost on a pure revision bump: source=%v dirty=%v",
			after.Source, after.Dirty)
	}
	if files := v.OpenFiles(); len(files) != 1 || files[0] != uri {
		t.Errorf("OpenFiles() = %v, want just %q", files, uri)
	}
}
