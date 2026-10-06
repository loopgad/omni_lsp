package golang

import (
	"context"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

// TestFeatures_GolangBridgeSyntaxTier exercises the three optional
// capabilities (§I16/§I20/§I22) against real source text through the go
// bridge's syntax-tier implementations.
func TestFeatures_GolangBridgeSyntaxTier(t *testing.T) {
	b := &Backend{}

	src := "package main\n\nfunc demo(a int, b int) int {\n\treturn a + b\n}\n\nfunc main() {\n\tdemo(1, 2)\n}\n"

	t.Run("signature help finds innermost call", func(t *testing.T) {
		lines := strings.Split(src, "\n")
		col := uint32(strings.Index(lines[7], "demo(1, 2)") + len("demo(")) // cursor after '('
		res, err := b.SignatureHelp(context.Background(), languages.SignatureHelpRequest{
			URI: "file:///w/main.go", Content: []byte(src), Line: 7, Column: col,
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Value == nil || len(res.Value.Signatures) != 1 {
			t.Fatalf("expected one signature, got %+v", res)
		}
		sig := res.Value.Signatures[0]
		if !strings.Contains(sig.Label, "demo(") {
			t.Errorf("label %q missing callee", sig.Label)
		}
		if len(sig.Parameters) != 2 {
			t.Errorf("same-file params = %v, want 2 entries", sig.Parameters)
		}
	})

	t.Run("formatting rewrites and no-ops", func(t *testing.T) {
		messy := "package main\nfunc m(){x:=1;_=x}\n"
		edits, err := b.Formatting(context.Background(), languages.FormattingRequest{
			URI: "file:///w/m.go", Content: []byte(messy),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(edits) != 1 || edits[0].NewText == string(messy) {
			t.Fatalf("expected one canonicalizing edit, got %+v", edits)
		}
		// The edit replaces the whole document, so its end has to be the
		// position just past the last byte. A source ending in a newline has
		// an empty final line, which makes that position column 0 -- and the
		// arithmetic used to underflow uint32 there, handing the client
		// EndChar 4294967295. Nothing checked this before.
		if edits[0].EndLine != 2 || edits[0].EndChar != 0 {
			t.Errorf("edit end = %d:%d, want 2:0 for a document ending in a newline",
				edits[0].EndLine, edits[0].EndChar)
		}
		clean := edits[0].NewText
		again, err := b.Formatting(context.Background(), languages.FormattingRequest{
			URI: "file:///w/m.go", Content: []byte(clean),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != 0 {
			t.Errorf("canonical input reformatted again: %+v", again)
		}
	})

	// The whole-document end position across the shapes a source can end in.
	// Each needs gofmt to actually change something, so the messy form keeps a
	// trailing space that format.Source strips.
	t.Run("full-span end position", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			src      string
			wantLine uint32
			wantChar uint32
		}{
			{"ends with a newline", "package main\nfunc m(){x:=1;_=x} \n", 2, 0},
			{"no newline at all", "package main\nfunc m(){x:=1;_=x} ", 1, 19},
			{"trailing blank line", "package main\nfunc m(){x:=1;_=x}\n\n", 3, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				edits, err := b.Formatting(context.Background(), languages.FormattingRequest{
					URI: "file:///w/m.go", Content: []byte(tc.src),
				})
				if err != nil {
					t.Fatal(err)
				}
				if len(edits) != 1 {
					t.Skipf("gofmt left this input alone (%d edits), nothing to check", len(edits))
				}
				if edits[0].EndLine != tc.wantLine || edits[0].EndChar != tc.wantChar {
					t.Errorf("edit end = %d:%d, want %d:%d",
						edits[0].EndLine, edits[0].EndChar, tc.wantLine, tc.wantChar)
				}
			})
		}
	})

	t.Run("inlay hints annotate anonymous params", func(t *testing.T) {
		hints, err := b.InlayHints(context.Background(), languages.InlayHintRequest{
			URI:     "file:///w/h.go",
			Content: []byte("package h\n\nfunc f(int, string) {}\n"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(hints) != 2 {
			t.Fatalf("hints = %+v, want 2 anonymous-param hints", hints)
		}
		for i, want := range []string{"arg0:", "arg1:"} {
			if hints[i].Label != want || hints[i].Kind != "parameter" {
				t.Errorf("hint[%d] = %+v, want label %q parameter", i, hints[i], want)
			}
		}
		_ = identity.ResultPartial
	})
}
