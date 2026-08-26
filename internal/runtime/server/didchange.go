package server

// Document synchronization edit application per goal.md §C5/§D6.
//
// Invariants:
//  1. PROT-SYNC-001: every accepted content change is applied sequentially —
//     no change is ever dropped as "old".
//  2. D6: an out-of-range position rejects the whole notification; the VFS
//     keeps its previous content. Never clamp into "something that looks valid".
//  3. LSP character offsets are UTF-16 code units (C4 default encoding).

import (
	"fmt"

	"github.com/omnilsp/omni/internal/protocol/lsp"
	"github.com/omnilsp/omni/internal/workspace/position"
)

// applyContentChanges applies changes in protocol order to content and
// returns the new text. A nil Range means full-document replacement.
func applyContentChanges(content []byte, changes []lsp.TextDocumentContentChangeEvent) ([]byte, error) {
	for i, ch := range changes {
		var next []byte
		var err error
		if ch.Range == nil {
			next = []byte(ch.Text)
		} else {
			next, err = applyRangeEdit(content, *ch.Range, ch.Text)
			if err != nil {
				return nil, fmt.Errorf("change %d: %w", i, err)
			}
		}
		content = next
	}
	return content, nil
}

// applyRangeEdit splices newText into content at the given LSP range.
func applyRangeEdit(content []byte, r lsp.Range, newText string) ([]byte, error) {
	idx := position.NewIndex(content, position.UTF16)
	start, err := idx.OffsetOfLineChar(content, r.Start.Line, r.Start.Character)
	if err != nil {
		return nil, fmt.Errorf("range start: %w", err)
	}
	end, err := idx.OffsetOfLineChar(content, r.End.Line, r.End.Character)
	if err != nil {
		return nil, fmt.Errorf("range end: %w", err)
	}
	if start > end {
		return nil, fmt.Errorf("range start %d:%d after end %d:%d",
			r.Start.Line, r.Start.Character, r.End.Line, r.End.Character)
	}
	out := make([]byte, 0, len(content)-int(end-start)+len(newText))
	out = append(out, content[:start]...)
	out = append(out, newText...)
	out = append(out, content[end:]...)
	return out, nil
}
