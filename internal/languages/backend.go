// Package languages defines the canonical Backend interface for OmniLSP.
//
// Responsibility:
//
//	Abstract interface that all language backends (Go, C/C++, etc.) must implement.
//	The semantic core depends only on this interface, not on concrete implementations.
//
// Owned mutable state:
//
//	None — this is an interface package.
//
// Concurrency model:
//
//	Backend methods receive context.Context for cancellation; backends are responsible
//	for their own thread-safety.
//
// Invariants:
//  1. U1: Semantic core packages MUST NOT import concrete backend packages.
//  2. Every method returns SemanticResult[T] with explicit Status and Evidence.
//  3. BackendID identifies the implementation; LanguageID identifies the language.
package languages

import (
	"context"

	"github.com/omnilsp/omni/internal/identity"
)

// EvidenceLevel represents the confidence level of a semantic result.
type EvidenceLevel int

const (
	EvidenceL0 EvidenceLevel = iota // Text/lexical - basic
	EvidenceL1                      // Syntax AST - structural navigation
	EvidenceL2                      // Semantic Index - cross-file candidates
	EvidenceL3                      // Compiler Native - definitive semantic
)

// CompletionRequest contains parameters for a completion query.
type CompletionRequest struct {
	URI      string
	Content  []byte
	Line     uint32
	Column   uint32
	Encoding int // 0=UTF8, 1=UTF16, 2=UTF32
}

// CompletionItem represents a single completion candidate.
type CompletionItem struct {
	Label         string
	Kind          int
	Detail        string
	Documentation string
	InsertText    string
	SortText      string
	FilterText    string
	Evidence      EvidenceLevel
}

// HoverRequest contains parameters for a hover query.
type HoverRequest struct {
	URI          string
	Content      []byte
	SnapshotRev  uint64
	BuildContext identity.BuildContextID
	Line         uint32
	Column       uint32
	Encoding     int
}

// HoverResult contains hover information.
type HoverResult struct {
	Contents string
	Range    *Range
	Evidence EvidenceLevel
}

// DefinitionRequest contains parameters for a definition query.
type DefinitionRequest struct {
	URI          string
	Content      []byte
	SnapshotRev  uint64
	BuildContext identity.BuildContextID
	Line         uint32
	Column       uint32
	Encoding     int
}

// Location represents a location in a file.
type Location struct {
	URI   string
	Range Range
}

// Range represents a range in a document.
type Range struct {
	StartLine      uint32
	StartCharacter uint32
	EndLine        uint32
	EndCharacter   uint32
}

// ReferencesRequest contains parameters for a references query.
type ReferencesRequest struct {
	URI          string
	Content      []byte
	SnapshotRev  uint64
	BuildContext identity.BuildContextID
	Line         uint32
	Column       uint32
	Encoding     int
	IncludeDecl  bool
}

// DocumentSymbolRequest contains parameters for document symbols.
type DocumentSymbolRequest struct {
	URI      string
	Content  []byte
	Encoding int
}

// SymbolKind mirrors LSP SymbolKind.
type SymbolKind int

const (
	SymbolFile        SymbolKind = 1
	SymbolModule      SymbolKind = 2
	SymbolNamespace   SymbolKind = 3
	SymbolPackage     SymbolKind = 4
	SymbolClass       SymbolKind = 5
	SymbolMethod      SymbolKind = 6
	SymbolProperty    SymbolKind = 7
	SymbolField       SymbolKind = 8
	SymbolConstructor SymbolKind = 9
	SymbolEnum        SymbolKind = 10
	SymbolInterface   SymbolKind = 11
	SymbolFunction    SymbolKind = 12
	SymbolVariable    SymbolKind = 13
	SymbolConstant    SymbolKind = 14
	SymbolStruct      SymbolKind = 23
)

// CompletionItemKind mirrors LSP CompletionItemKind.
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

// DocumentSymbol represents a symbol inside a document.
type DocumentSymbol struct {
	Name               string
	Detail             string
	Kind               SymbolKind
	StartLine          uint32
	StartCharacter     uint32
	EndLine            uint32
	EndCharacter       uint32
	SelectionLine      uint32
	SelectionCharacter uint32
	Children           []DocumentSymbol
}

// Diagnostic represents a compiler diagnostic.
type Diagnostic struct {
	StartLine uint32
	StartChar uint32
	EndLine   uint32
	EndChar   uint32
	Severity  int
	Code      string
	Source    string
	Message   string
}

// SemanticToken represents a semantic token for syntax highlighting.
type SemanticToken struct {
	DeltaLine  uint32
	DeltaStart uint32
	Length     uint32
	TokenType  uint32
	TokenMods  uint32
}

// WorkspaceSymbolRequest contains parameters for workspace symbol search.
type WorkspaceSymbolRequest struct {
	Query string
	Limit int
}

// WorkspaceSymbol represents a workspace-level symbol.
type WorkspaceSymbol struct {
	Name      string
	Kind      SymbolKind
	URI       string
	StartLine uint32
	StartCol  uint32
}

// RenameRequest contains parameters for rename.
type RenameRequest struct {
	URI          string
	Content      []byte
	SnapshotRev  uint64
	BuildContext identity.BuildContextID
	Line         uint32
	Column       uint32
	Encoding     int
	NewName      string
}

// ValidatedEdit is the S3-safe outcome of a mutating operation (§B7/§G).
type ValidatedEdit struct {
	Edits []TextEdit
	// Complete records whether the backend proved the reference set covers
	// every applicable destination for this operation (§B6). Only a Complete
	// proof may be projected to clients as a WorkspaceEdit.
	Complete bool
}

// TextEdit represents a text edit.
type TextEdit struct {
	URI       string
	StartLine uint32
	StartChar uint32
	EndLine   uint32
	EndChar   uint32
	NewText   string
}

// Backend is the interface that language backends must implement.
type Backend interface {
	// LanguageID returns the language identifier.
	LanguageID() string

	// FileExtensions returns the file extensions this backend handles.
	FileExtensions() []string

	// Completion returns completion candidates at the given position.
	Completion(ctx context.Context, req CompletionRequest) ([]CompletionItem, error)

	// Hover returns hover information at the given position.
	// Envelope per §B5: operational failure stays in the Go error; semantic
	// status/evidence live in the envelope (§Q3 separates the two).
	Hover(ctx context.Context, req HoverRequest) (identity.SemanticResult[*HoverResult], error)

	// Definition returns the definition location(s) at the given position.
	Definition(ctx context.Context, req DefinitionRequest) (identity.SemanticResult[[]Location], error)

	// References returns all references to the symbol at the given position.
	References(ctx context.Context, req ReferencesRequest) (identity.SemanticResult[[]Location], error)

	// DocumentSymbols returns all symbols in the given document.
	DocumentSymbols(ctx context.Context, req DocumentSymbolRequest) ([]DocumentSymbol, error)

	// WorkspaceSymbols searches for symbols across the workspace.
	WorkspaceSymbols(ctx context.Context, req WorkspaceSymbolRequest) ([]WorkspaceSymbol, error)

	// Diagnostics returns diagnostics for the given document.
	Diagnostics(ctx context.Context, uri string, content []byte) ([]Diagnostic, error)

	// SemanticTokens returns semantic tokens for syntax highlighting.
	SemanticTokens(ctx context.Context, uri string, content []byte) ([]SemanticToken, error)

	// Rename is an S3 operation (§B7/SEM-SAFE-001): the envelope's
	// Completeness must be Complete for the result to be publishable; backends
	// that cannot prove completeness MUST return Unavailable/Incomplete.
	Rename(ctx context.Context, req RenameRequest) (identity.SemanticResult[ValidatedEdit], error)

	// Close releases any resources held by the backend.
	Close() error
}

// --- Optional capability interfaces (§I16/§I20/§I22) -------------------------
//
// Not every backend can honor every feature. Servers type-assert these
// interfaces and answer with a clean "not supported" error instead of
// forcing every bridge to grow stub methods.

// SignatureHelpRequest positions a signature-help query (§I16).
type SignatureHelpRequest struct {
	URI          string
	Content      []byte
	SnapshotRev  uint64
	BuildContext identity.BuildContextID
	Line         uint32
	Column       uint32
}

// SignatureInformation is one callable's rendered signature.
type SignatureInformation struct {
	Label           string   // e.g. "fmt.Fprintf(w io.Writer, format string, a ...any)"
	Parameters      []string // parameter labels, positional
	ActiveParameter int      // index highlighted at the cursor
}

// SignatureHelpResult carries the active call's candidates (best first).
type SignatureHelpResult struct {
	Signatures      []SignatureInformation
	ActiveSignature int
	ActiveParameter int
}

// SignatureHelper is an optional backend capability (§I16).
type SignatureHelper interface {
	SignatureHelp(ctx context.Context, req SignatureHelpRequest) (identity.SemanticResult[*SignatureHelpResult], error)
}

// FormattingRequest asks for a whole-document format pass (§I20).
type FormattingRequest struct {
	URI          string
	Content      []byte
	TabSize      int
	InsertSpaces bool
}

// Formatter is an optional backend capability (§I20).
type Formatter interface {
	Formatting(ctx context.Context, req FormattingRequest) ([]TextEdit, error)
}

// InlayHintRequest positions an inlay-hint query (§I22).
type InlayHintRequest struct {
	URI          string
	Content      []byte
	SnapshotRev  uint64
	BuildContext identity.BuildContextID
	StartLine    uint32
	EndLine      uint32
}

// InlayHint is one inline annotation.
type InlayHint struct {
	Line   uint32
	Column uint32
	Label  string
	Kind   string // "parameter", "type", …
}

// InlayHintProvider is an optional backend capability (§I22).
type InlayHintProvider interface {
	InlayHints(ctx context.Context, req InlayHintRequest) ([]InlayHint, error)
}

// StatusReporter is an optional backend capability (§Q2): surfaces the
// backend's human-facing lifecycle state so the server can project recovery
// guidance into failed-request errors instead of bare process messages.
type StatusReporter interface {
	BackendStatusMessage() string
}

// IncompleteCompletionProvider is an optional backend capability (§I9): the
// bridge declares its completion lists as heuristic subsets, so clients keep
// re-querying while typing instead of treating one response as canonical.
type IncompleteCompletionProvider interface {
	CompletionIsIncomplete() bool
}
