package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/omnilsp/omni/internal/languages/lspwire"
	"github.com/omnilsp/omni/internal/protocol/lsp"
)

func TestCompletionProjectionPreservesChildEditSemantics(t *testing.T) {
	const uri = "file:///w/main.cpp"
	childResponse := []byte(`{"isIncomplete":true,"items":[
		{"label":"range-edit","kind":3,"detail":"function","documentation":{"kind":"markdown","value":"**target**"},"insertText":"target($0)","sortText":"01","filterText":"tar","textEdit":{"range":{"start":{"line":1,"character":2},"end":{"line":1,"character":6}},"newText":"target"},"additionalTextEdits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"#include <target>\n"}],"insertTextFormat":2},
		{"label":"insert-replace","kind":3,"detail":"overload","documentation":"plain docs","insertText":"target","sortText":"02","filterText":"target","textEdit":{"insert":{"start":{"line":2,"character":3},"end":{"line":2,"character":5}},"replace":{"start":{"line":2,"character":3},"end":{"line":2,"character":9}},"newText":"target"},"insertTextFormat":1}
	]}`)
	decoded, err := lspwire.DecodeCompletionList(childResponse)
	if err != nil {
		t.Fatalf("decode child completion response: %v", err)
	}
	if !decoded.IsIncomplete || len(decoded.Items) != 2 {
		t.Fatalf("decoded child completion list = %+v", decoded)
	}

	s := New(DefaultConfig())
	s.RegisterBackend("cpp", &completionListBackend{
		mockBackend: mockBackend{langID: "cpp", exts: []string{".cpp"}},
		result:      decoded,
	})
	s.vfs.Open(uri, "cpp", 1, []byte("int target;\n"), 0)
	response := dispatchCompletion(t, s, uri)
	var got struct {
		IsIncomplete bool `json:"isIncomplete"`
		Items        []struct {
			Label               string          `json:"label"`
			Kind                int             `json:"kind"`
			Detail              string          `json:"detail"`
			Documentation       string          `json:"documentation"`
			InsertText          string          `json:"insertText"`
			SortText            string          `json:"sortText"`
			FilterText          string          `json:"filterText"`
			TextEdit            json.RawMessage `json:"textEdit"`
			AdditionalTextEdits []lsp.TextEdit  `json:"additionalTextEdits"`
			InsertTextFormat    int             `json:"insertTextFormat"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response, &got); err != nil {
		t.Fatalf("decode completion response: %v", err)
	}
	if !got.IsIncomplete || len(got.Items) != 2 || got.Items[0].Label != "range-edit" || got.Items[1].Label != "insert-replace" {
		t.Fatalf("handler changed list state or candidate order: %+v", got)
	}
	if got.Items[0].Kind != 3 || got.Items[0].Detail != "function" || got.Items[0].Documentation != "**target**" ||
		got.Items[0].InsertText != "target($0)" || got.Items[0].SortText != "01" || got.Items[0].FilterText != "tar" ||
		got.Items[1].Kind != 3 || got.Items[1].Detail != "overload" || got.Items[1].Documentation != "plain docs" ||
		got.Items[1].InsertText != "target" || got.Items[1].SortText != "02" || got.Items[1].FilterText != "target" {
		t.Fatalf("handler changed supported completion item metadata: %+v", got.Items)
	}

	var rangeEdit lsp.TextEdit
	if err := json.Unmarshal(got.Items[0].TextEdit, &rangeEdit); err != nil {
		t.Fatalf("decode projected TextEdit: %v", err)
	}
	wantRangeEdit := lsp.TextEdit{
		Range:   lsp.Range{Start: lsp.Position{Line: 1, Character: 2}, End: lsp.Position{Line: 1, Character: 6}},
		NewText: "target",
	}
	if !reflect.DeepEqual(rangeEdit, wantRangeEdit) || got.Items[0].InsertTextFormat != 2 {
		t.Fatalf("projected TextEdit/format = %+v/%d; want %+v/2", rangeEdit, got.Items[0].InsertTextFormat, wantRangeEdit)
	}
	wantAdditional := []lsp.TextEdit{{
		Range:   lsp.Range{Start: lsp.Position{Line: 0, Character: 0}, End: lsp.Position{Line: 0, Character: 0}},
		NewText: "#include <target>\n",
	}}
	if !reflect.DeepEqual(got.Items[0].AdditionalTextEdits, wantAdditional) {
		t.Fatalf("projected additionalTextEdits = %+v; want %+v", got.Items[0].AdditionalTextEdits, wantAdditional)
	}
	var insertReplace lsp.InsertReplaceEdit
	if err := json.Unmarshal(got.Items[1].TextEdit, &insertReplace); err != nil {
		t.Fatalf("decode projected InsertReplaceEdit: %v", err)
	}
	wantInsertReplace := lsp.InsertReplaceEdit{
		Insert:  lsp.Range{Start: lsp.Position{Line: 2, Character: 3}, End: lsp.Position{Line: 2, Character: 5}},
		Replace: lsp.Range{Start: lsp.Position{Line: 2, Character: 3}, End: lsp.Position{Line: 2, Character: 9}},
		NewText: "target",
	}
	if !reflect.DeepEqual(insertReplace, wantInsertReplace) || got.Items[1].InsertTextFormat != 1 {
		t.Fatalf("projected InsertReplaceEdit/format = %+v/%d; want %+v/1", insertReplace, got.Items[1].InsertTextFormat, wantInsertReplace)
	}
}
