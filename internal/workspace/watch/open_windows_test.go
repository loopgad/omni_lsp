//go:build windows

package watch

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScannerReadHandleDoesNotBlockRenameAndDelete(t *testing.T) {
	for _, name := range []string{"ordinary", "long-path"} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			if name == "long-path" {
				directory = filepath.Join(directory, strings.Repeat("a", 100), strings.Repeat("b", 100))
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(directory, "source.go")
			const content = "package source\n"
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := openReadShared(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			renamed := filepath.Join(filepath.Dir(path), "renamed.go")
			if err := os.Rename(path, renamed); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(renamed); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(file)
			if err != nil || string(got) != content {
				t.Fatalf("captured read=%q err=%v", got, err)
			}
		})
	}
}
