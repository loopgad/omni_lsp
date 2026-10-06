package server

import (
	"encoding/json"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/protocol/lsp"
	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// TestC4_DidChangeUsesNegotiatedEncoding covers the one path that used to
// hardcode UTF-16 regardless of what the client negotiated.
//
// The fixture is "é = 1" on one line: the leading é is one character but two
// bytes in UTF-8 and one UTF-16 code unit. Replacing the trailing "1"
// therefore means a different character column per encoding -- 3 under UTF-8
// and UTF-16 (é, space, =), but 4 under UTF-32 -- so a path that reads the
// range with the wrong encoding splices at the wrong byte and leaves the "1"
// behind. That is silent: the out-of-range guard in didchange.go does not fire,
// because the smaller column is still a legal position under the wrong
// encoding.
func TestC4_DidChangeUsesNegotiatedEncoding(t *testing.T) {
	const src = "é = 1\n"
	// Column of the final "1" in each encoding's own units. The é is two
	// bytes in UTF-8 but one UTF-16 code unit and one UTF-32 code unit, so the
	// "1" sits at a different column per encoding -- reading the range under
	// the wrong one lands inside the "=" or past the line.
	col := map[position.Encoding]uint32{
		position.UTF8:  5,
		position.UTF16: 4,
		position.UTF32: 4,
	}
	for _, enc := range []position.Encoding{position.UTF8, position.UTF16, position.UTF32} {
		t.Run(enc.String(), func(t *testing.T) {
			c := col[enc]
			got, err := applyContentChanges([]byte(src), []lsp.TextDocumentContentChangeEvent{{
				Range: &lsp.Range{
					Start: lsp.Position{Line: 0, Character: c},
					End:   lsp.Position{Line: 0, Character: c + 1},
				},
				Text: "2",
			}}, enc)
			if err != nil {
				t.Fatalf("applyContentChanges(%s): %v", enc, err)
			}
			if want := "é = 2\n"; string(got) != want {
				t.Errorf("content = %q, want %q (range read in the wrong encoding)", got, want)
			}
		})
	}
}

// TestC4_NegotiatedEncodingReachesDidChange is the end-to-end half: a client
// that negotiates utf-8 must have its didChange ranges interpreted as UTF-8
// characters. applyContentChanges already threads the encoding; this checks the
// handler supplies the negotiated value rather than a default.
func TestC4_NegotiatedEncodingReachesDidChange(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
	s.vfs.Open(uri, "go", 1, []byte("é = 1\n"), vfs.SourceEditor)

	// general is a sibling of capabilities in InitializeParams, not a member of
	// it -- nesting it inside capabilities decodes fine and negotiates nothing.
	initParams := `{"processId":null,"rootUri":null,"capabilities":{},"general":{"positionEncodings":["utf-8","utf-16"]}}`
	if resp := s.Dispatcher().Dispatch(t.Context(),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", json.RawMessage(initParams))); resp == nil || resp.Error != nil {
		t.Fatalf("initialize = %+v, want success", resp)
	}
	if got := s.negotiatedEncodingInt(); got != 0 {
		t.Fatalf("negotiated encoding = %d, want 0 (utf-8)", got)
	}

	// Replace the "1" at UTF-8 column 5.
	change := `{"textDocument":{"uri":"` + uri + `","version":2},"contentChanges":[{"range":{"start":{"line":0,"character":5},"end":{"line":0,"character":6}},"text":"2"}]}`
	if resp := s.Dispatcher().Dispatch(t.Context(),
		jsonrpc.NewNotification("textDocument/didChange", json.RawMessage(change))); resp != nil && resp.Error != nil {
		t.Fatalf("didChange = %+v, want success", resp)
	}

	if got := string(s.vfs.Content(uri)); got != "é = 2\n" {
		t.Errorf("document = %q, want %q (the handler did not pass the negotiated utf-8 encoding)", got, "é = 2\n")
	}
}
