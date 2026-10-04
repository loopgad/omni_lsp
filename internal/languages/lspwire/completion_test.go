package lspwire

import (
	"reflect"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
)

func TestDecodeCompletionListPreservesListAndItemMetadata(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want languages.CompletionList
	}{
		{
			name: "array",
			raw:  `[{"label":"print","kind":3,"detail":"function","documentation":"print docs","insertText":"print($0)","sortText":"01","filterText":"pri"}]`,
			want: languages.CompletionList{Items: []languages.CompletionItem{{
				Label: "print", Kind: 3, Detail: "function", Documentation: "print docs",
				InsertText: "print($0)", SortText: "01", FilterText: "pri",
			}}},
		},
		{
			name: "incomplete markup list",
			raw:  `{"isIncomplete":true,"items":[{"label":"Path","kind":7,"documentation":{"kind":"markdown","value":"**Path**"},"insertText":"Path","sortText":"02","filterText":"Pat"}]}`,
			want: languages.CompletionList{IsIncomplete: true, Items: []languages.CompletionItem{{
				Label: "Path", Kind: 7, Documentation: "**Path**", InsertText: "Path", SortText: "02", FilterText: "Pat",
			}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DecodeCompletionList([]byte(test.raw))
			if err != nil {
				t.Fatalf("DecodeCompletionList: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("DecodeCompletionList = %+v; want %+v", got, test.want)
			}
		})
	}
}

func TestDecodeCompletionListRejectsMalformedEnvelope(t *testing.T) {
	for _, raw := range []string{`{}`, `{"items":null}`, `{"items":"not-an-array"}`} {
		if _, err := DecodeCompletionList([]byte(raw)); err == nil {
			t.Errorf("DecodeCompletionList(%s) accepted a malformed CompletionList", raw)
		}
	}
}

func TestDecodeCompletionListPreservesCompletionEdits(t *testing.T) {
	raw := `{"isIncomplete":true,"items":[
		{"label":"range-edit","textEdit":{"range":{"start":{"line":1,"character":2},"end":{"line":1,"character":6}},"newText":"target"},"additionalTextEdits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"#include <target>\n"}],"insertTextFormat":2},
		{"label":"insert-replace","textEdit":{"insert":{"start":{"line":2,"character":3},"end":{"line":2,"character":5}},"replace":{"start":{"line":2,"character":3},"end":{"line":2,"character":9}},"newText":"target"},"insertTextFormat":1}
	]}`
	got, err := DecodeCompletionList([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeCompletionList: %v", err)
	}
	want := languages.CompletionList{
		IsIncomplete: true,
		Items: []languages.CompletionItem{
			{
				Label: "range-edit",
				TextEdit: &languages.CompletionTextEdit{
					Range:   &languages.Range{StartLine: 1, StartCharacter: 2, EndLine: 1, EndCharacter: 6},
					NewText: "target",
				},
				AdditionalTextEdits: []languages.TextEdit{{
					StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 0, NewText: "#include <target>\n",
				}},
				InsertTextFormat: 2,
			},
			{
				Label: "insert-replace",
				TextEdit: &languages.CompletionTextEdit{
					InsertReplace: &languages.CompletionInsertReplaceEdit{
						Insert:  languages.Range{StartLine: 2, StartCharacter: 3, EndLine: 2, EndCharacter: 5},
						Replace: languages.Range{StartLine: 2, StartCharacter: 3, EndLine: 2, EndCharacter: 9},
					},
					NewText: "target",
				},
				InsertTextFormat: 1,
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DecodeCompletionList = %+v; want %+v", got, want)
	}
}

func TestDecodeCompletionListRejectsMalformedCompletionEdits(t *testing.T) {
	for _, raw := range []string{
		`{"items":[{"label":"bad","textEdit":{"newText":"x"}}]}`,
		`{"items":[{"label":"bad","textEdit":{"insert":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"newText":"x"}}]}`,
		`{"items":[{"label":"bad","textEdit":{"range":{"start":{"line":1,"character":2},"end":{"line":1}},"newText":"x"}}]}`,
		`{"items":[{"label":"bad","additionalTextEdits":[{"range":{},"newText":"x"}]}]}`,
		`{"items":[{"label":"bad","insertTextFormat":"snippet"}]}`,
	} {
		if _, err := DecodeCompletionList([]byte(raw)); err == nil {
			t.Errorf("DecodeCompletionList(%s) accepted malformed completion edit data", raw)
		}
	}
}
