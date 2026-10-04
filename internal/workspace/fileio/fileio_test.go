package fileio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileShared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.go")
	want := []byte("package source\n")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFileShared(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("ReadFileShared() = %q, want %q", got, want)
	}
}
