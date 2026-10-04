package golang

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
)

// BenchmarkGoReferences measures the real Go backend's warm reference walk.
func BenchmarkGoReferences(b *testing.B) {
	backend, req := referencesFixture(b, 1000)
	ctx := context.Background()
	res, err := backend.References(ctx, req)
	if err != nil || len(res.Value) < 1000 {
		b.Fatalf("warm references: %d locations, err=%v, status=%v", len(res.Value), err, res.Status)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := backend.References(ctx, req)
		if err != nil || len(res.Value) < 1000 {
			b.Fatalf("references: %d locations, err=%v", len(res.Value), err)
		}
	}
}

func TestReferencesPositionIndexReused(t *testing.T) {
	backend, req := referencesFixture(t, 1000)
	ctx := context.Background()
	var maxLine uint32
	check := func() {
		res, err := backend.References(ctx, req)
		if err != nil || len(res.Value) < 1000 {
			t.Fatalf("references: %d locations, err=%v", len(res.Value), err)
		}
		for _, loc := range res.Value {
			if loc.Range.StartLine > maxLine {
				maxLine = loc.Range.StartLine
			}
		}
	}
	check() // warm package cache before measuring allocations
	allocs := testing.AllocsPerRun(5, check)
	if maxLine != 1002 {
		t.Fatalf("last reference line = %d, want 1002", maxLine)
	}
	if allocs > 12000 {
		t.Fatalf("reference walk allocated %.0f times; position index may be rebuilt per result", allocs)
	}
}

func referencesFixture(tb testing.TB, count int) (*Backend, languages.ReferencesRequest) {
	tb.Helper()
	dir := tb.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module bench\n\ngo 1.26\n"), 0o644); err != nil {
		tb.Fatal(err)
	}
	var source strings.Builder
	source.WriteString("package bench\nvar target int\nfunc use() {\n")
	for i := 0; i < count; i++ {
		source.WriteString("_ = target\n")
	}
	source.WriteString("}\n")
	content := []byte(source.String())
	path := filepath.Join(dir, "bench.go")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		tb.Fatal(err)
	}
	backend := New(dir)
	tb.Cleanup(func() { _ = backend.Close() })
	req := languages.ReferencesRequest{URI: pathToUri(path), Content: content, Line: 1, Column: 5, SnapshotRev: 1, IncludeDecl: true}
	return backend, req
}
