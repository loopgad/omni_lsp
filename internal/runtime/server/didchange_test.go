package server

// Regression tests for goal.md §C5/§D6 (PROT-SYNC-001): incremental didChange
// edits are state transitions and MUST be applied losslessly; invalid ranges
// and regressive versions must reject without corrupting VFS state.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/protocol/lsp"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

func didChangeMsg(t *testing.T, uri string, version int64, changes []lsp.TextDocumentContentChangeEvent) *jsonrpc.Message {
	t.Helper()
	params := DidChangeTextDocumentParams{
		TextDocument:   lsp.VersionedTextDocumentIdentifier{URI: uri, Version: version},
		ContentChanges: changes,
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return jsonrpc.NewNotification("textDocument/didChange", raw)
}

func openDoc(s *Server, uri string, version int64, content string) {
	s.vfs.Open(uri, "go", version, []byte(content), vfs.SourceEditor)
	s.publishSnapshot()
}

// TestPROT_SYNC_001_SequentialRangeEditsLossless verifies two range edits in
// one notification are both applied (the old last-change-wins path dropped
// all but the final edit).
func TestPROT_SYNC_001_SequentialRangeEditsLossless(t *testing.T) {
	s := New(DefaultConfig())
	uri := "file:///x.go"
	openDoc(s, uri, 1, "abcdef\n")

	line := uint32(0)
	msg := didChangeMsg(t, uri, 2, []lsp.TextDocumentContentChangeEvent{
		{Range: &lsp.Range{
			Start: lsp.Position{Line: line, Character: 0},
			End:   lsp.Position{Line: line, Character: 1},
		}, Text: "X"}, // abcdef -> Xbcdef
		{Range: &lsp.Range{
			Start: lsp.Position{Line: line, Character: 3},
			End:   lsp.Position{Line: line, Character: 4},
		}, Text: "Z"}, // Xbcdef -> XbcZef
	})
	s.dispatcher.Dispatch(context.Background(), msg)

	got := string(s.vfs.Content(uri))
	if got != "XbcZef\n" {
		t.Errorf("content = %q, want %q (both edits must apply)", got, "XbcZef\n")
	}
}

// TestDidChangeMultiByteBoundaries verifies UTF-16 columns against CJK and
// emoji content (INV-POS-001 at the sync layer).
func TestDidChangeMultiByteBoundaries(t *testing.T) {
	s := New(DefaultConfig())
	uri := "file:///cjk.go"
	// "中文" is 3 bytes per rune = 1 UTF-16 unit each.
	openDoc(s, uri, 1, "中文\n")

	// Replace 中 (UTF-16 col 0..1) with A.
	msg := didChangeMsg(t, uri, 2, []lsp.TextDocumentContentChangeEvent{
		{Range: &lsp.Range{
			Start: lsp.Position{Line: 0, Character: 0},
			End:   lsp.Position{Line: 0, Character: 1},
		}, Text: "A"},
	})
	if resp := s.dispatcher.Dispatch(context.Background(), msg); resp != nil && resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if got := string(s.vfs.Content(uri)); got != "A文\n" {
		t.Errorf("CJK edit: got %q, want %q", got, "A文\n")
	}

	// Emoji is a surrogate pair (2 UTF-16 units): a column strictly inside
	// the pair must be rejected (INV-POS-002).
	uri2 := "file:///emoji.go"
	openDoc(s, uri2, 1, "a😀b\n")
	msg = didChangeMsg(t, uri2, 2, []lsp.TextDocumentContentChangeEvent{
		{Range: &lsp.Range{
			Start: lsp.Position{Line: 0, Character: 2}, // inside surrogate pair
			End:   lsp.Position{Line: 0, Character: 3},
		}, Text: "!"},
	})
	s.dispatcher.Dispatch(context.Background(), msg)
	if got := string(s.vfs.Content(uri2)); got != "a😀b\n" {
		t.Errorf("surrogate-interior edit must be rejected, got %q", got)
	}

	// Delete the whole emoji (cols 1..3) is valid.
	msg = didChangeMsg(t, uri2, 3, []lsp.TextDocumentContentChangeEvent{
		{Range: &lsp.Range{
			Start: lsp.Position{Line: 0, Character: 1},
			End:   lsp.Position{Line: 0, Character: 3},
		}, Text: "!"},
	})
	s.dispatcher.Dispatch(context.Background(), msg)
	if got := string(s.vfs.Content(uri2)); got != "a!b\n" {
		t.Errorf("emoji edit: got %q, want %q", got, "a!b\n")
	}
}

// TestDidChangeCRLF verifies edits around CRLF terminators.
func TestDidChangeCRLF(t *testing.T) {
	s := New(DefaultConfig())
	uri := "file:///crlf.go"
	openDoc(s, uri, 1, "one\r\ntwo\r\n")

	// Insert at end of line 0 (col 3 = before \r\n).
	msg := didChangeMsg(t, uri, 2, []lsp.TextDocumentContentChangeEvent{
		{Range: &lsp.Range{
			Start: lsp.Position{Line: 0, Character: 3},
			End:   lsp.Position{Line: 0, Character: 3},
		}, Text: "!"},
	})
	s.dispatcher.Dispatch(context.Background(), msg)
	if got := string(s.vfs.Content(uri)); got != "one!\r\ntwo\r\n" {
		t.Errorf("CRLF edit: got %q, want %q", got, "one!\r\ntwo\r\n")
	}
}

// TestDidChangeInvalidRangeRejects verifies D6: out-of-range positions reject
// the notification and leave prior content intact (never clamp).
func TestDidChangeInvalidRangeRejects(t *testing.T) {
	s := New(DefaultConfig())
	uri := "file:///x.go"

	cases := []struct {
		name    string
		content string
		rng     lsp.Range
	}{
		{"line out of range", "short\n", lsp.Range{
			Start: lsp.Position{Line: 5, Character: 0},
			End:   lsp.Position{Line: 5, Character: 1}}},
		{"column past line width", "short\n", lsp.Range{
			Start: lsp.Position{Line: 0, Character: 100},
			End:   lsp.Position{Line: 0, Character: 101}}},
		{"start after end", "short\n", lsp.Range{
			Start: lsp.Position{Line: 0, Character: 4},
			End:   lsp.Position{Line: 0, Character: 2}}},
		{"column past multibyte line width", "中\n", lsp.Range{
			Start: lsp.Position{Line: 0, Character: 1},
			End:   lsp.Position{Line: 0, Character: 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s.vfs.Open(uri, "go", 9, []byte(tc.content), vfs.SourceEditor)
			before := string(s.vfs.Content(uri))
			rejectsBefore := s.syncRejects.Load()
			msg := didChangeMsg(t, uri, 10, []lsp.TextDocumentContentChangeEvent{{Range: &tc.rng, Text: "junk"}})
			s.dispatcher.Dispatch(context.Background(), msg)
			if after := string(s.vfs.Content(uri)); after != before {
				t.Errorf("VFS mutated by rejected change: %q -> %q", before, after)
			}
			if delta := s.syncRejects.Load() - rejectsBefore; delta != 1 {
				t.Errorf("syncRejects delta = %d, want 1 (rejection must be observable)", delta)
			}
		})
	}
}

// TestDidChangeVersionRegressionRejected verifies D6: non-increasing versions
// are rejected explicitly instead of silently patching.
func TestDidChangeVersionRegressionRejected(t *testing.T) {
	s := New(DefaultConfig())
	uri := "file:///x.go"
	openDoc(s, uri, 5, "v5\n")

	for _, v := range []int64{5, 4, 1} {
		msg := didChangeMsg(t, uri, v, []lsp.TextDocumentContentChangeEvent{{Text: "stale\n"}})
		s.dispatcher.Dispatch(context.Background(), msg)
		if got := string(s.vfs.Content(uri)); got != "v5\n" {
			t.Errorf("version %d applied over v5: content = %q", v, got)
		}
	}
	if s.syncRejects.Load() != 3 {
		t.Errorf("syncRejects = %d, want 3", s.syncRejects.Load())
	}
}

// TestDidChangeFullReplacementStillWorks keeps the legacy full-sync path green.
func TestDidChangeFullReplacementStillWorks(t *testing.T) {
	s := New(DefaultConfig())
	uri := "file:///x.go"
	openDoc(s, uri, 1, "old\n")

	params := `{"textDocument":{"uri":"` + uri + `","version":2},"contentChanges":[{"text":"brand new\n"}]}`
	msg := jsonrpc.NewNotification("textDocument/didChange", json.RawMessage(params))
	s.dispatcher.Dispatch(context.Background(), msg)
	if got := string(s.vfs.Content(uri)); got != "brand new\n" {
		t.Errorf("full replacement: got %q", got)
	}
	f := s.vfs.Get(uri)
	if f == nil || f.Version != 2 || !f.Dirty {
		t.Errorf("file state wrong after replacement: %+v", f)
	}
}

// TestDidChangeFirstChangeCreatesDocument mirrors fuzz seed behavior: a
// didChange for an unopened document creates it (insertion into empty content).
func TestDidChangeFirstChangeCreatesDocument(t *testing.T) {
	s := New(DefaultConfig())
	uri := "file:///new.go"
	msg := didChangeMsg(t, uri, 1, []lsp.TextDocumentContentChangeEvent{
		{Range: &lsp.Range{
			Start: lsp.Position{Line: 0, Character: 0},
			End:   lsp.Position{Line: 0, Character: 0},
		}, Text: "hello"},
	})
	s.dispatcher.Dispatch(context.Background(), msg)
	if got := string(s.vfs.Content(uri)); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}
