package golang

import (
	"context"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
)

// TestK_StatusCacheCounters verifies CacheStats exposes the same counters
// the package cache updates (§K0: hits must be visible end to end).
func TestK_StatusCacheCounters(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	uri := writeGoFile(t, b, "main.go", "package main\n\nfunc main() {}\n")
	src := []byte("package main\n\nfunc main() {}\n")

	hits0, misses0 := b.CacheStats()

	req := languages.DefinitionRequest{URI: uri, Content: src, Line: 2, Column: 6}
	if _, err := b.Definition(context.Background(), req); err != nil {
		t.Fatalf("first definition: %v", err)
	}
	if _, err := b.Definition(context.Background(), req); err != nil {
		t.Fatalf("second definition: %v", err)
	}

	hits, misses := b.CacheStats()
	if misses <= misses0 {
		t.Errorf("misses = %d, want > %d after cold query", misses, misses0)
	}
	if hits <= hits0 {
		t.Errorf("hits = %d, want > %d after identical repeat query", hits, hits0)
	}
}
