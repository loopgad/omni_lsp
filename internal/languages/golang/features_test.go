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
