package golang

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
)

func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	dir := t.TempDir()
	goMod := `module testmod
go 1.26`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	return New(dir)
}

func writeGoFile(t *testing.T, b *Backend, name, content string) string {
	t.Helper()
	fpath := filepath.Join(b.workDir, name)
	if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fpath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return pathToUri(fpath)
}

func TestBackendLocked(t *testing.T) {
	b := New(t.TempDir())
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLanguageID(t *testing.T) {
	b := New(t.TempDir())
	if b.LanguageID() != "go" {
		t.Errorf("LanguageID() = %q, want go", b.LanguageID())
	}
}

func TestFileExtensions(t *testing.T) {
	b := New(t.TempDir())
	exts := b.FileExtensions()
	if len(exts) != 1 || exts[0] != ".go" {
		t.Errorf("FileExtensions() = %v, want [.go]", exts)
	}
}

func TestDocumentSymbols_Parseable(t *testing.T) {
	b := New(t.TempDir())
	src := `package main
func foo(x int) {}
type Bar struct{ X int }`
	syms, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
		URI:     "file:///tmp/test.go",
		Content: []byte(src),
	})
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(syms) == 0 {
		t.Fatal("expected at least one symbol")
	}
	found := false
	for _, s := range syms {
		if s.Name == "foo" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected foo symbol, got: %v", syms)
	}
}

func TestURIPathConversion(t *testing.T) {
	uri := "file:///hello/world.go"
	got := uriToPath(uri)
	if got == "" {
		t.Error("uriToPath should not return empty")
	}
	back := pathToUri(got)
	if back == "" {
		t.Error("pathToUri should not return empty")
	}
}

func TestH_CompletionNoDeadlock(t *testing.T) {
	b := newTestBackend(t)
	done := make(chan error, 1)
	go func() {
		// 不存在的子目录让 packages.Load 快速失败，走语法回退路径；
		// 无论返回结果还是错误，都必须在锁未被重入时及时返回。
		_, err := b.Completion(context.Background(), languages.CompletionRequest{
			URI:     pathToUri(filepath.Join(b.workDir, "missing", "x.go")),
			Content: []byte("package p\n"),
		})
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Completion 挂起：b.mu 在持有状态下被 locked() 重入死锁")
	}
}

func TestConcurrentAccess(t *testing.T) {
	b := New(t.TempDir())
	done := make(chan struct{}, 5)
	for i := 0; i < 5; i++ {
		go func() {
			defer func() { recover() }()
			_, _ = b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
				URI:     "file:///tmp/x.go",
				Content: []byte("package main\n"),
			})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 5; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("goroutine leak")
		}
	}
}
