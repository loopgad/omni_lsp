// Invariants:
//  1. Types mirror the LSP 3.17 baseline; unknown fields tolerated per A7.
//  2. types.go holds declarations only; manifest.go in this same package builds and
//     verifies the protocol fingerprint, so the package is not declaration-only.
package lsp

// LSPBaseline pins the wire protocol revision this package mirrors
// (§U4/§C17). docs/versions.md carries the same value and is checked against
// it by internal/conformance/versions_test.go — keep the two in step.
const LSPBaseline = "3.17"

// DocumentURI is a URI identifying a document.
type DocumentURI = string

// Position represents a position in a text document (0-based).
type Position struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

// Range represents a range in a text document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location represents a location inside a resource.
type Location struct {
	URI   DocumentURI `json:"uri"`
	Range Range       `json:"range"`
}

// TextEdit represents a textual edit.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// TextDocumentItem represents a text document.
type TextDocumentItem struct {
	URI        DocumentURI `json:"uri"`
	LanguageID string      `json:"languageId"`
	Version    int64       `json:"version"`
	Text       string      `json:"text"`
}

// TextDocumentIdentifier identifies a text document.
type TextDocumentIdentifier struct {
	URI DocumentURI `json:"uri"`
}

// VersionedTextDocumentIdentifier identifies a specific version of a text document.
type VersionedTextDocumentIdentifier struct {
	URI     DocumentURI `json:"uri"`
	Version int64       `json:"version"`
}

// TextDocumentContentChangeEvent describes a content change.
type TextDocumentContentChangeEvent struct {
	// If Range is nil, the full text is replaced.
	Range *Range `json:"range,omitempty"`
	Text  string `json:"text"`
}

// DiagnosticSeverity represents the severity of a diagnostic.
type DiagnosticSeverity int

const (
	DiagnosticError       DiagnosticSeverity = 1
	DiagnosticWarning     DiagnosticSeverity = 2
	DiagnosticInformation DiagnosticSeverity = 3
	DiagnosticHint        DiagnosticSeverity = 4
)

// Diagnostic represents a diagnostic (e.g., a compiler error).
type Diagnostic struct {
	Range    Range              `json:"range"`
	Severity DiagnosticSeverity `json:"severity,omitempty"`
	Code     string             `json:"code,omitempty"`
	Source   string             `json:"source,omitempty"`
	Message  string             `json:"message"`
}

// CompletionItemKind represents the kind of a completion item.
type CompletionItemKind int

const (
	CompletionText          CompletionItemKind = 1
	CompletionMethod        CompletionItemKind = 2
	CompletionFunction      CompletionItemKind = 3
	CompletionConstructor   CompletionItemKind = 4
	CompletionField         CompletionItemKind = 5
	CompletionVariable      CompletionItemKind = 6
	CompletionClass         CompletionItemKind = 7
	CompletionInterface     CompletionItemKind = 8
	CompletionModule        CompletionItemKind = 9
	CompletionProperty      CompletionItemKind = 10
	CompletionUnit          CompletionItemKind = 11
	CompletionValue         CompletionItemKind = 12
	CompletionEnum          CompletionItemKind = 13
	CompletionKeyword       CompletionItemKind = 14
	CompletionSnippet       CompletionItemKind = 15
	CompletionColor         CompletionItemKind = 16
	CompletionFile          CompletionItemKind = 17
	CompletionReference     CompletionItemKind = 18
	CompletionFolder        CompletionItemKind = 19
	CompletionEnumMember    CompletionItemKind = 20
	CompletionConstant      CompletionItemKind = 21
	CompletionStruct        CompletionItemKind = 22
	CompletionEvent         CompletionItemKind = 23
	CompletionOperator      CompletionItemKind = 24
	CompletionTypeParameter CompletionItemKind = 25
)

// InsertTextFormat indicates whether insertText is plain text or a snippet.
type InsertTextFormat int

const (
	InsertTextFormatPlainText InsertTextFormat = 1
	InsertTextFormatSnippet   InsertTextFormat = 2
)

// InsertReplaceEdit is the LSP completion-specific edit form.
type InsertReplaceEdit struct {
	Insert  Range  `json:"insert"`
	Replace Range  `json:"replace"`
	NewText string `json:"newText"`
}

// CompletionItem represents a completion item.
type CompletionItem struct {
	Label               string             `json:"label"`
	Kind                CompletionItemKind `json:"kind,omitempty"`
	Detail              string             `json:"detail,omitempty"`
	Documentation       string             `json:"documentation,omitempty"`
	InsertText          string             `json:"insertText,omitempty"`
	SortText            string             `json:"sortText,omitempty"`
	FilterText          string             `json:"filterText,omitempty"`
	TextEdit            any                `json:"textEdit,omitempty"`
	AdditionalTextEdits []TextEdit         `json:"additionalTextEdits,omitempty"`
	InsertTextFormat    InsertTextFormat   `json:"insertTextFormat,omitempty"`
}

// CompletionList represents a list of completion items.
type CompletionList struct {
	IsIncomplete bool             `json:"isIncomplete"`
	Items        []CompletionItem `json:"items"`
}

// Hover represents the result of a hover request.
type Hover struct {
	Contents MarkupContent `json:"contents"`
	Range    *Range        `json:"range,omitempty"`
}

// MarkupContent represents markup content.
type MarkupContent struct {
	Kind  string `json:"kind"` // "plaintext" or "markdown"
	Value string `json:"value"`
}

// SymbolKind represents the kind of a symbol.
type SymbolKind int

const (
	SymbolFile          SymbolKind = 1
	SymbolModule        SymbolKind = 2
	SymbolNamespace     SymbolKind = 3
	SymbolPackage       SymbolKind = 4
	SymbolClass         SymbolKind = 5
	SymbolMethod        SymbolKind = 6
	SymbolProperty      SymbolKind = 7
	SymbolField         SymbolKind = 8
	SymbolConstructor   SymbolKind = 9
	SymbolEnum          SymbolKind = 10
	SymbolInterface     SymbolKind = 11
	SymbolFunction      SymbolKind = 12
	SymbolVariable      SymbolKind = 13
	SymbolConstant      SymbolKind = 14
	SymbolString        SymbolKind = 15
	SymbolNumber        SymbolKind = 16
	SymbolBoolean       SymbolKind = 17
	SymbolArray         SymbolKind = 18
	SymbolObject        SymbolKind = 19
	SymbolKey           SymbolKind = 20
	SymbolNull          SymbolKind = 21
	SymbolEnumMember    SymbolKind = 22
	SymbolStruct        SymbolKind = 23
	SymbolEvent         SymbolKind = 24
	SymbolOperator      SymbolKind = 25
	SymbolTypeParameter SymbolKind = 26
)

// DocumentSymbol represents a symbol inside a document.
type DocumentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           SymbolKind       `json:"kind"`
	Range          Range            `json:"range"`
	SelectionRange Range            `json:"selectionRange"`
	Children       []DocumentSymbol `json:"children,omitempty"`
}

// WorkspaceSymbol represents a symbol in the workspace.
type WorkspaceSymbol struct {
	Name     string     `json:"name"`
	Kind     SymbolKind `json:"kind"`
	Location Location   `json:"location"`
}

// TextEditMap maps document URIs to edits.
type TextEditMap = map[DocumentURI][]TextEdit

// OptionalVersionedTextDocumentIdentifier identifies a document with the
// version the edit was computed against (LSP 3.17 documentChanges form).
type OptionalVersionedTextDocumentIdentifier struct {
	URI     DocumentURI `json:"uri"`
	Version int         `json:"version"`
}

// TextDocumentEdit is one document's edits in the version-aware
// documentChanges form (§C9).
type TextDocumentEdit struct {
	TextDocument OptionalVersionedTextDocumentIdentifier `json:"textDocument"`
	Edits        []TextEdit                              `json:"edits"`
}

// WorkspaceEdit represents changes to many resources. Servers emit exactly
// one of the two forms: legacy `changes`, or version-aware `documentChanges`
// when the client negotiated it (§C9 SHOULD).
type WorkspaceEdit struct {
	Changes         TextEditMap        `json:"changes,omitempty"`
	DocumentChanges []TextDocumentEdit `json:"documentChanges,omitempty"`
}
