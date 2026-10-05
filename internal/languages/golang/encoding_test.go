package golang

import (
	"context"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

// TestC4_RenamePropagatesEncodingSetToReferences pins that the rename
// request's negotiated encoding reaches the internal References hop.
//
// §C4 requires one position-encoding decision per request. Rename walks to
// References internally; when that hop dropped EncodingSet it fell back to
// the UTF-16 default while the caller had agreed UTF-8, so the returned
// edits were in the wrong unit on any line preceded by non-ASCII text.
//
// Line 2 below is 19 ASCII bytes followed by three CJK chars, which puts the
// use site at byte column 32 (UTF-8) but code-unit column 26 (UTF-16).
func TestC4_RenamePropagatesEncodingSetToReferences(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	const src = "package main\n\nfunc main() { _ = \"日本語\" + 変数 }\n\nvar 変数 = 1\n"
	uri := writeGoFile(t, b, "main.go", src)

	res, err := b.Rename(context.Background(), languages.RenameRequest{
		URI: uri, Content: []byte(src), SnapshotRev: 1,
		Line: 4, Column: 4, Encoding: 0, EncodingSet: true,
		NewName: "renamed",
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if res.Status != identity.ResultExact || !res.Value.Complete {
		t.Fatalf("status/complete = %v/%t, diagnostics %v", res.Status, res.Value.Complete, res.InternalDiagnostics)
	}
	var use *languages.TextEdit
	for i := range res.Value.Edits {
		if res.Value.Edits[i].StartLine == 2 {
			use = &res.Value.Edits[i]
		}
	}
	if use == nil {
		t.Fatalf("no edit on the use line 2: %+v", res.Value.Edits)
	}
	if use.StartChar != 32 || use.EndChar != 38 {
		t.Errorf("use-site edit columns = %d..%d, want 32..38 (UTF-8 byte units); "+
			"26..28 means the internal References hop fell back to UTF-16", use.StartChar, use.EndChar)
	}
}

// TestSemanticTokensMeasureLengthInTheNegotiatedEncoding pins that a token's
// length shares the basis of the column that positions it.
//
// §C10 binds a token to document identity, version, legend and backend
// generation, but says nothing about the unit of the delta stream — the client
// decodes it with whatever it negotiated in general.positionEncodings. So a
// column taken in UTF-8 byte units next to a length taken in UTF-16 code units
// places every token at the wrong extent, and the delta encoding silently
// desynchronises from the first non-ASCII character on.
//
// The astral emoji is what makes the three bases distinguishable: it is 4 UTF-8
// bytes, 2 UTF-16 code units and 1 code point. The CJK identifier ahead of it
// keeps the column observable too — byte 13 in UTF-8, code unit 9 elsewhere.
func TestSemanticTokensMeasureLengthInTheNegotiatedEncoding(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	const src = "package main\n\nvar 日本 = \"\U0001F389\"\n"
	uri := writeGoFile(t, b, "main.go", src)

	for _, tc := range []struct {
		name       string
		encoding   int
		wantColumn uint32
		wantLength uint32
	}{
		{"utf-8", 0, 13, 6},
		{"utf-16", 1, 9, 4},
		{"utf-32", 2, 9, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokens, err := b.SemanticTokensWithEncoding(context.Background(), uri, []byte(src), tc.encoding)
			if err != nil {
				t.Fatalf("SemanticTokensWithEncoding(%d): %v", tc.encoding, err)
			}
			// The wire form is relative, so fold the deltas back to an absolute
			// column before comparing: a test that asserts deltas would pin the
			// order of unrelated AST nodes instead of the unit.
			var line, col uint32
			var lit *languages.SemanticToken
			for i := range tokens {
				if i == 0 {
					line, col = tokens[i].DeltaLine, tokens[i].DeltaStart
				} else if tokens[i].DeltaLine == 0 {
					col += tokens[i].DeltaStart
				} else {
					line += tokens[i].DeltaLine
					col = tokens[i].DeltaStart
				}
				if tokens[i].TokenType == languages.TokString {
					lit = &tokens[i]
				}
			}
			if lit == nil {
				t.Fatalf("no string token in %+v", tokens)
			}
			_ = line
			if col != tc.wantColumn {
				t.Errorf("string token column = %d, want %d (%s units)", col, tc.wantColumn, tc.name)
			}
			if lit.Length != tc.wantLength {
				t.Errorf("string token length = %d, want %d; a different value means the "+
					"length was measured in another basis than the column", lit.Length, tc.wantLength)
			}
		})
	}
}
