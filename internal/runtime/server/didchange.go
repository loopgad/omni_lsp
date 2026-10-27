package server

// Document synchronization edit application per goal.md §C5/§D6.
//
// Invariants:
//  1. PROT-SYNC-001: every accepted content change is applied sequentially —
//     no change is ever dropped as "old".
//  2. D6: an out-of-range position rejects the whole notification; the VFS
//     keeps its previous content. Never clamp into "something that looks valid".
//  3. LSP character offsets use the encoding negotiated for the session
//     (C4); UTF-16 is only the fallback for clients that offered none.
//     This path used to hardcode UTF-16 regardless, which mis-spliced any
//     line containing non-ASCII once a client negotiated utf-8 or utf-32.

import (
	"fmt"

	"github.com/omnilsp/omni/internal/protocol/lsp"
	"github.com/omnilsp/omni/internal/workspace/position"
)

// applyContentChanges applies changes in protocol order to content and
// returns the new text. A nil Range means full-document replacement. enc is
// the negotiated position encoding; range characters are counted in it.
func applyContentChanges(content []byte, changes []lsp.TextDocumentContentChangeEvent, enc position.Encoding) ([]byte, error) {
	for i, ch := range changes {
		var next []byte
		var err error
		if ch.Range == nil {
			next = []byte(ch.Text)
		} else {
			next, err = applyRangeEdit(content, *ch.Range, ch.Text, enc)
			if err != nil {
				return nil, fmt.Errorf("change %d: %w", i, err)
			}
		}
		content = next
	}
	return content, nil
}

// applyRangeEdit splices newText into content at the given LSP range. The
// range characters are counted in enc, so the conversion must resolve columns
// in the same encoding the client negotiated -- any other encoding turns a
// character's column into a different byte offset on any line with non-ASCII.
// The endpoints resolve inline (line scan + O(line length) column walk) instead
// of building a per-call position.Index: ranged didChange is the §S18 hot path
// and NewIndex would rescan the whole document and allocate a 4-bytes-per-byte
// column map on every edit. Offsets and errors are identical to the Index path
// (pinned by TestOffsetOfLineCharInlineMatchesIndexPath).
func applyRangeEdit(content []byte, r lsp.Range, newText string, enc position.Encoding) ([]byte, error) {
	start, err := position.OffsetOfLineCharInline(content, r.Start.Line, r.Start.Character, enc)
	if err != nil {
		return nil, fmt.Errorf("range start: %w", err)
	}
	end, err := position.OffsetOfLineCharInline(content, r.End.Line, r.End.Character, enc)
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
