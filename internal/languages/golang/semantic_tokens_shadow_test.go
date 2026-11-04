package golang

// round23 C: SemanticTokensWithEncoding emitted a same-span shadow token on
// every declaration name. The FuncDecl/TypeSpec branches sent
// TokFunction/TokType on the name and then let ast.Inspect descend, so the
// name ident hit the Ident branch and sent a second TokVariable at the
// identical position — LSP requires ordered, non-overlapping token streams,
// and later-writer-wins clients rendered function/type names in variable
// color. The fix gives the declaration branches sole ownership of the name
// span; this pin fails on any same-span duplicate.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
)

type semanticTokenSpan struct {
	line, col, length, tt uint32
}

func TestSemanticTokensNoShadowOnDeclarationNames(t *testing.T) {
	src := []byte("package main\n\ntype T int\n\nfunc foo() {}\n")
	b := New(t.TempDir())
	tokens, err := b.SemanticTokensWithEncoding(context.Background(), pathToUri(filepath.Join(b.workDir, "main.go")), src, 1)
	if err != nil {
		t.Fatalf("SemanticTokensWithEncoding: %v", err)
	}

	decodeDeltas := func(tokens []languages.SemanticToken) []semanticTokenSpan {
		var spans []semanticTokenSpan
		var line, col uint32
		for _, tok := range tokens {
			if tok.DeltaLine == 0 {
				col += tok.DeltaStart
			} else {
				line += tok.DeltaLine
				col = tok.DeltaStart
			}
			spans = append(spans, semanticTokenSpan{line, col, tok.Length, tok.TokenType})
		}
		return spans
	}
	spans := decodeDeltas(tokens)

	// No two tokens may share a position+extent: a same-span duplicate is
	// exactly the shadow the fix removes.
	seen := make(map[semanticTokenSpan]int, len(spans))
	for _, s := range spans {
		pos := semanticTokenSpan{s.line, s.col, s.length, 0}
		if prev := seen[pos]; prev != 0 {
			t.Fatalf("overlapping same-span tokens at line=%d col=%d len=%d: tokenTypes %d and %d", s.line, s.col, s.length, prev, s.tt)
		}
		seen[pos] = int(s.tt)
	}

	// T in `type T int` must be one TokType, not a TokType+TokVariable pair.
	assertSingleToken(t, spans, 2, 5, 1, languages.TokType, "type name T")
	// foo in `func foo() {}` must be one TokFunction, not a
	// TokFunction+TokVariable pair.
	assertSingleToken(t, spans, 4, 5, 3, languages.TokFunction, "func name foo")
}

func assertSingleToken(t *testing.T, spans []semanticTokenSpan, line, col, length, wantTT uint32, what string) {
	t.Helper()
	hits := 0
	for _, s := range spans {
		if s.line == line && s.col == col && s.length == length {
			hits++
			if s.tt != wantTT {
				t.Fatalf("%s at line=%d col=%d len=%d: tokenType=%d, want %d", what, line, col, length, s.tt, wantTT)
			}
		}
	}
	if hits != 1 {
		t.Fatalf("%s at line=%d col=%d len=%d: %d tokens, want exactly 1", what, line, col, length, hits)
	}
}
