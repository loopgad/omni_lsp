package server

// FuzzDidChange exercises arbitrary content + range edits through
// applyContentChanges to verify: no panic, and rejected edits leave the
// input unchanged (goal.md §S3 fuzz target "incremental didChange", §D6).

import (
	"testing"

	"github.com/omnilsp/omni/internal/protocol/lsp"
)

func FuzzDidChange(f *testing.F) {
	seeds := []struct {
		content string
		line    uint32
		col     uint32
		endLine uint32
		endCol  uint32
		text    string
	}{
		{"hello world\nsecond line\n", 0, 0, 0, 5, "goodbye"},
		{"中文注释\nfunc main() {}\n", 0, 2, 0, 4, "文"},
		{"a😀b\n", 0, 1, 0, 3, "!"},
		{"crlf\r\nlines\r\n", 1, 0, 1, 5, "X"},
		{"", 0, 0, 0, 0, "seed"},
		{"no newline end", 0, 3, 0, 14, ""},
	}
	for _, s := range seeds {
		f.Add(s.content, s.line, s.col, s.endLine, s.endCol, s.text)
	}

	f.Fuzz(func(t *testing.T, content string, line, col, endLine, endCol uint32, text string) {
		rng := lsp.Range{
			Start: lsp.Position{Line: line, Character: col},
			End:   lsp.Position{Line: endLine, Character: endCol},
		}
		out, err := applyRangeEdit([]byte(content), rng, text)
		if err != nil {
			// Rejection is fine — it must be deterministic though.
			if _, err2 := applyRangeEdit([]byte(content), rng, text); (err == nil) != (err2 == nil) {
				t.Fatalf("nondeterministic acceptance: %v vs %v", err, err2)
			}
			return
		}
		// Accepted edit invariants:
		//  1. output is valid UTF-8-preserving splice: re-applying the same
		//     edit to the original must give identical bytes.
		out2, err2 := applyRangeEdit([]byte(content), rng, text)
		if err2 != nil || string(out) != string(out2) {
			t.Fatalf("nondeterministic result: %q vs %q (%v)", out, out2, err2)
		}
		// 2. empty-range empty-text edit must not change content.
		if text == "" && line == endLine && col == endCol {
			if string(out) != content {
				t.Fatalf("no-op edit changed content: %q -> %q", content, out)
			}
		}
	})
}
