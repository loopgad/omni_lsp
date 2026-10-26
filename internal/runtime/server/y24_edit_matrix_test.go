package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// TestY24_WorkspaceEditCapabilityMatrix locks the §C9 negotiation: clients
// that declare workspace.workspaceEdit.documentChanges receive version-aware
// TextDocumentEdit entries; legacy clients keep the plain changes form.
func TestY24_WorkspaceEditCapabilityMatrix(t *testing.T) {
	const uri = "file:///w/main.go"
	newServer := func(caps json.RawMessage) *Server {
		s := New(DefaultConfig())
		s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"},
			renResult: languages.ValidatedEdit{Complete: true,
				Edits: []languages.TextEdit{{URI: uri, StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 7, NewText: "renamed"}}}})
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
		if caps == nil {
			caps = json.RawMessage(`{}`)
		}
		initParams := fmt.Sprintf(`{"rootUri":"file:///w","capabilities":%s}`, caps)
		msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 0}, "initialize", json.RawMessage(initParams))
		if resp := s.dispatcher.Dispatch(context.Background(), msg); resp != nil && resp.Error != nil {
			t.Fatalf("initialize failed: %+v", resp.Error)
		}
		return s
	}
	rename := func(s *Server) map[string]json.RawMessage {
		params := fmt.Sprintf(`{"textDocument":{"uri":%q},"position":{"line":0,"character":0},"newName":"zz"}`, uri)
		msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/rename", json.RawMessage(params))
		resp := s.dispatcher.Dispatch(context.Background(), msg)
		if resp == nil || resp.Error != nil {
			t.Fatalf("rename failed: %+v", resp)
		}
		var out struct {
			Result json.RawMessage `json:"result"`
		}
		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal response: %v", err)
		}
		json.Unmarshal(raw, &out)
		var edit map[string]json.RawMessage
		if err := json.Unmarshal(out.Result, &edit); err != nil {
			t.Fatalf("workspaceedit shape: %v (%s)", err, out.Result)
		}
		return edit
	}

	t.Run("negotiating client gets versioned documentChanges", func(t *testing.T) {
		s := newServer(json.RawMessage(`{"workspace":{"workspaceEdit":{"documentChanges":true}}}`))
		edit := rename(s)
		dc, ok := edit["documentChanges"]
		if !ok {
			t.Fatalf("want documentChanges form, got %v", edit)
		}
		if _, hasLegacy := edit["changes"]; hasLegacy {
			t.Error("mutually exclusive forms both present")
		}
		var docs []struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
			} `json:"textDocument"`
			Edits []json.RawMessage `json:"edits"`
		}
		if err := json.Unmarshal(dc, &docs); err != nil {
			t.Fatalf("documentChanges decode: %v", err)
		}
		if len(docs) != 1 || docs[0].TextDocument.URI != uri {
			t.Fatalf("docs = %+v", docs)
		}
		// §C9/LSP 3.17: the version must be the document's own version (the
		// doc was opened at version 1), not a workspace revision or a
		// placeholder — a conforming client discards a TextDocumentEdit whose
		// version does not match the document it tracks.
		if docs[0].TextDocument.Version != 1 {
			t.Errorf("version must be the document's own version (1), got %d", docs[0].TextDocument.Version)
		}
	})

	t.Run("documentChanges carries each document's own version", func(t *testing.T) {
		mainURI := "file:///w/main.go"
		otherURI := "file:///w/other.go"
		s := New(DefaultConfig())
		s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"},
			renResult: languages.ValidatedEdit{Complete: true,
				Edits: []languages.TextEdit{
					{URI: mainURI, StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 7, NewText: "renamed"},
					{URI: otherURI, StartLine: 1, StartChar: 0, EndLine: 1, EndChar: 5, NewText: "renamed"},
				}}})
		s.vfs.Open(mainURI, "go", 7, []byte("package main\n"), vfs.SourceEditor)
		s.vfs.Open(otherURI, "go", 41, []byte("package other\n"), vfs.SourceEditor)
		s.publishSnapshot()
		if cur := s.snapMgr.Current(); cur == nil || cur.ID().Revision == 7 || cur.ID().Revision == 41 {
			t.Fatalf("fixture precondition: workspace revision must differ from both document versions, got %+v", cur)
		}
		initParams := `{"rootUri":"file:///w","capabilities":{"workspace":{"workspaceEdit":{"documentChanges":true}}}}`
		initMsg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 0}, "initialize", json.RawMessage(initParams))
		if resp := s.dispatcher.Dispatch(context.Background(), initMsg); resp != nil && resp.Error != nil {
			t.Fatalf("initialize failed: %+v", resp.Error)
		}
		edit := rename(s)
		dc, ok := edit["documentChanges"]
		if !ok {
			t.Fatalf("want documentChanges form, got %v", edit)
		}
		var docs []struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
			} `json:"textDocument"`
			Edits []json.RawMessage `json:"edits"`
		}
		if err := json.Unmarshal(dc, &docs); err != nil {
			t.Fatalf("documentChanges decode: %v", err)
		}
		if len(docs) != 2 {
			t.Fatalf("docs = %+v", docs)
		}
		want := map[string]int{mainURI: 7, otherURI: 41}
		for _, doc := range docs {
			if doc.TextDocument.Version != want[doc.TextDocument.URI] {
				t.Errorf("%s: version must be that document's own version (%d), got %d",
					doc.TextDocument.URI, want[doc.TextDocument.URI], doc.TextDocument.Version)
			}
		}
	})

	t.Run("legacy client keeps changes form", func(t *testing.T) {
		for name, caps := range map[string]json.RawMessage{
			"no capabilities":     nil,
			"empty workspaceEdit": json.RawMessage(`{"workspace":{"workspaceEdit":{}}}`),
		} {
			s := newServer(caps)
			edit := rename(s)
			if _, ok := edit["changes"]; !ok {
				t.Errorf("%s: want legacy changes form, got %v", name, edit)
			}
			if strings.Contains(string(mustJSON(t, edit)), "documentChanges") {
				t.Errorf("%s: documentChanges leaked to legacy client", name)
			}
		}
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
