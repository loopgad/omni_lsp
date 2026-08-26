package vfs

import (
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
