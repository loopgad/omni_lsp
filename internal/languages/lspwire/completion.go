// Package lspwire decodes language-server response envelopes into OmniLSP's
// internal language result types.
//
// Invariants:
//   - Malformed or unsupported wire shapes return an error rather than a
//     partially decoded result.
//   - Optional presentation fields are preserved without assigning semantic
//     meaning to backend-specific content.
package lspwire

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/omnilsp/omni/internal/languages"
)

type completionItemWire struct {
	Label               string          `json:"label"`
	Kind                int             `json:"kind"`
	Detail              string          `json:"detail"`
	Documentation       json.RawMessage `json:"documentation"`
	InsertText          string          `json:"insertText"`
	SortText            string          `json:"sortText"`
	FilterText          string          `json:"filterText"`
	TextEdit            json.RawMessage `json:"textEdit"`
	AdditionalTextEdits json.RawMessage `json:"additionalTextEdits"`
	InsertTextFormat    json.RawMessage `json:"insertTextFormat"`
}

// DecodeCompletionList accepts the array and CompletionList response forms.
// It preserves the incompleteness bit and fields that affect how clients
// display, filter, order, or insert the returned candidates.
func DecodeCompletionList(raw []byte) (languages.CompletionList, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return languages.CompletionList{}, nil
	}
	var itemsRaw json.RawMessage
	var incomplete bool
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		itemsRaw = raw
	} else {
		var envelope struct {
			IsIncomplete bool            `json:"isIncomplete"`
			Items        json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return languages.CompletionList{}, fmt.Errorf("completion decode: %w", err)
		}
		if len(envelope.Items) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Items), []byte("null")) {
			return languages.CompletionList{}, fmt.Errorf("completion decode: CompletionList has no items array")
		}
		itemsRaw, incomplete = envelope.Items, envelope.IsIncomplete
	}
	var wireItems []completionItemWire
	if err := json.Unmarshal(itemsRaw, &wireItems); err != nil {
		return languages.CompletionList{}, fmt.Errorf("completion decode items: %w", err)
	}
	items, err := projectCompletionItems(wireItems)
	if err != nil {
		return languages.CompletionList{}, err
	}
	return languages.CompletionList{Items: items, IsIncomplete: incomplete}, nil
}

func projectCompletionItems(raw []completionItemWire) ([]languages.CompletionItem, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	items := make([]languages.CompletionItem, 0, len(raw))
	for i, item := range raw {
		textEdit, err := decodeCompletionTextEdit(item.TextEdit)
		if err != nil {
			return nil, fmt.Errorf("completion decode item %d textEdit: %w", i, err)
		}
		additionalEdits, err := decodeAdditionalTextEdits(item.AdditionalTextEdits)
		if err != nil {
			return nil, fmt.Errorf("completion decode item %d additionalTextEdits: %w", i, err)
		}
		insertTextFormat, err := decodeInsertTextFormat(item.InsertTextFormat)
		if err != nil {
			return nil, fmt.Errorf("completion decode item %d insertTextFormat: %w", i, err)
		}
		items = append(items, languages.CompletionItem{
			Label: item.Label, Kind: item.Kind, Detail: item.Detail,
			Documentation: completionDocumentation(item.Documentation),
			InsertText:    item.InsertText, SortText: item.SortText, FilterText: item.FilterText,
			TextEdit: textEdit, AdditionalTextEdits: additionalEdits, InsertTextFormat: insertTextFormat,
		})
	}
	return items, nil
}

type completionPositionWire struct {
	Line      *uint32 `json:"line"`
	Character *uint32 `json:"character"`
}

type completionRangeWire struct {
	Start *completionPositionWire `json:"start"`
	End   *completionPositionWire `json:"end"`
}

func decodeCompletionTextEdit(raw json.RawMessage) (*languages.CompletionTextEdit, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		if err == nil {
			err = fmt.Errorf("must be an object")
		}
		return nil, err
	}
	newTextRaw, hasNewText := fields["newText"]
	var newText *string
	if !hasNewText {
		return nil, fmt.Errorf("missing newText")
	}
	if err := json.Unmarshal(newTextRaw, &newText); err != nil {
		return nil, fmt.Errorf("invalid newText: %w", err)
	}
	if newText == nil {
		return nil, fmt.Errorf("newText must be a string")
	}

	rangeRaw, hasRange := fields["range"]
	insertRaw, hasInsert := fields["insert"]
	replaceRaw, hasReplace := fields["replace"]
	if hasRange && (hasInsert || hasReplace) {
		return nil, fmt.Errorf("cannot combine range and insert/replace")
	}
	if hasRange {
		editRange, err := decodeCompletionRange(rangeRaw)
		if err != nil {
			return nil, fmt.Errorf("invalid range: %w", err)
		}
		return &languages.CompletionTextEdit{Range: &editRange, NewText: *newText}, nil
	}
	if !hasInsert || !hasReplace {
		return nil, fmt.Errorf("must contain range or both insert and replace")
	}
	insertRange, err := decodeCompletionRange(insertRaw)
	if err != nil {
		return nil, fmt.Errorf("invalid insert range: %w", err)
	}
	replaceRange, err := decodeCompletionRange(replaceRaw)
	if err != nil {
		return nil, fmt.Errorf("invalid replace range: %w", err)
	}
	return &languages.CompletionTextEdit{
		InsertReplace: &languages.CompletionInsertReplaceEdit{Insert: insertRange, Replace: replaceRange},
		NewText:       *newText,
	}, nil
}

func decodeCompletionRange(raw json.RawMessage) (languages.Range, error) {
	var wire completionRangeWire
	if err := json.Unmarshal(raw, &wire); err != nil {
		return languages.Range{}, err
	}
	if wire.Start == nil || wire.End == nil || wire.Start.Line == nil || wire.Start.Character == nil || wire.End.Line == nil || wire.End.Character == nil {
		return languages.Range{}, fmt.Errorf("start/end line and character are required")
	}
	editRange := languages.Range{
		StartLine: *wire.Start.Line, StartCharacter: *wire.Start.Character,
		EndLine: *wire.End.Line, EndCharacter: *wire.End.Character,
	}
	if editRange.EndLine < editRange.StartLine ||
		(editRange.EndLine == editRange.StartLine && editRange.EndCharacter < editRange.StartCharacter) {
		return languages.Range{}, fmt.Errorf("end precedes start")
	}
	return editRange, nil
}

func decodeAdditionalTextEdits(raw json.RawMessage) ([]languages.TextEdit, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("must be an array")
	}
	var wireEdits []json.RawMessage
	if err := json.Unmarshal(raw, &wireEdits); err != nil {
		return nil, err
	}
	edits := make([]languages.TextEdit, 0, len(wireEdits))
	for i, rawEdit := range wireEdits {
		edit, err := decodeCompletionTextEdit(rawEdit)
		if err != nil {
			return nil, fmt.Errorf("edit %d: %w", i, err)
		}
		if edit.Range == nil || edit.InsertReplace != nil {
			return nil, fmt.Errorf("edit %d must use TextEdit range form", i)
		}
		edits = append(edits, languages.TextEdit{
			StartLine: edit.Range.StartLine, StartChar: edit.Range.StartCharacter,
			EndLine: edit.Range.EndLine, EndChar: edit.Range.EndCharacter, NewText: edit.NewText,
		})
	}
	return edits, nil
}

func decodeInsertTextFormat(raw json.RawMessage) (int, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	var format *int
	if err := json.Unmarshal(raw, &format); err != nil {
		return 0, err
	}
	if format == nil || (*format != 1 && *format != 2) {
		return 0, fmt.Errorf("must be 1 (PlainText) or 2 (Snippet)")
	}
	return *format, nil
}

func completionDocumentation(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var markup struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &markup); err == nil {
		return markup.Value
	}
	return ""
}
