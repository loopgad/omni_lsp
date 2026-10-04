package golang

import (
	"fmt"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// BenchmarkFindIdentAtLargeFile measures the position lookup shared by hover
// and definition. The target is at the end so a full AST walk is exposed.
func BenchmarkFindIdentAtLargeFile(b *testing.B) {
	const count = 5000
	var src strings.Builder
	src.WriteString("package bench\n")
	for i := 0; i < count; i++ {
		fmt.Fprintf(&src, "func symbol%d() {}\n", i)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bench.go", src.String(), 0)
	if err != nil {
		b.Fatal(err)
	}
	target := f.Decls[len(f.Decls)-1].Pos() + token.Pos(len("func "))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if ident := findIdentAt(f, fset, target); ident == nil || ident.Name != "symbol4999" {
			b.Fatalf("last declaration not found: %v", ident)
		}
	}
}
