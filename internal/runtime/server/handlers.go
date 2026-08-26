package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/protocol/lsp"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"github.com/omnilsp/omni/internal/workspace/vfs"
	"github.com/omnilsp/omni/internal/workspace/virtual"
)

// --- Handler types ----------------------------------------------------------

type InitializeParams struct {
	ProcessID        int               `json:"processId"`
	RootURI          string            `json:"rootUri"`
	Capabilities     json.RawMessage   `json:"capabilities"`
	WorkspaceFolders []WorkspaceFolder `json:"workspaceFolders"`
	General          struct {
		PositionEncodings []string `json:"positionEncodings"`
	} `json:"general"`
}

type WorkspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

type InitializeResult struct {
	Capabilities ServerCapabilities `json:"capabilities"`
}

type ServerCapabilities struct {
	PositionEncoding           string                   `json:"positionEncoding,omitempty"`
	TextDocumentSync           *TextDocumentSyncOptions `json:"textDocumentSync"`
	HoverProvider              bool                     `json:"hoverProvider"`
	CompletionProvider         *CompletionOptions       `json:"completionProvider"`
	DefinitionProvider         bool                     `json:"definitionProvider"`
	DeclarationProvider        bool                     `json:"declarationProvider,omitempty"`
	ReferencesProvider         bool                     `json:"referencesProvider"`
	DocumentSymbolProvider     bool                     `json:"documentSymbolProvider"`
	RenameProvider             bool                     `json:"renameProvider"`
	SemanticTokensProvider     *SemanticTokensOptions   `json:"semanticTokensProvider"`
	WorkspaceSymbolProvider    bool                     `json:"workspaceSymbolProvider"`
	SignatureHelpProvider      bool                     `json:"signatureHelpProvider,omitempty"`
	CodeActionProvider         bool                     `json:"codeActionProvider,omitempty"`
	DiagnosticProvider         bool                     `json:"diagnosticProvider,omitempty"`
	DocumentFormattingProvider bool                     `json:"documentFormattingProvider,omitempty"`
	InlayHintProvider          bool                     `json:"inlayHintProvider,omitempty"`
}

type TextDocumentSyncOptions struct {
	OpenClose bool         `json:"openClose"`
	Change    int          `json:"change"`
	Save      *SaveOptions `json:"save"`
}

type SaveOptions struct {
	IncludeText bool `json:"includeText"`
}

type CompletionOptions struct {
	TriggerCharacters []string `json:"triggerCharacters"`
}

type SemanticTokensOptions struct {
	Legend      SemanticTokensLegend `json:"legend"`
	Full        bool                 `json:"full"`
	Range       bool                 `json:"range,omitempty"`
	Incremental bool                 `json:"incremental,omitempty"`
}

type SemanticTokensLegend struct {
	TokenTypes     []string `json:"tokenTypes"`
	TokenModifiers []string `json:"tokenModifiers"`
}

type DidOpenTextDocumentParams struct {
	TextDocument lsp.TextDocumentItem `json:"textDocument"`
}

type DidChangeTextDocumentParams struct {
	TextDocument   lsp.VersionedTextDocumentIdentifier  `json:"textDocument"`
	ContentChanges []lsp.TextDocumentContentChangeEvent `json:"contentChanges"`
}

type DidSaveTextDocumentParams struct {
	TextDocument lsp.TextDocumentIdentifier `json:"textDocument"`
	Text         string                     `json:"text,omitempty"`
}

type DidCloseTextDocumentParams struct {
	TextDocument lsp.TextDocumentIdentifier `json:"textDocument"`
}

type HoverParams struct {
	TextDocument lsp.TextDocumentIdentifier `json:"textDocument"`
	Position     lsp.Position               `json:"position"`
}

type CompletionParams struct {
	TextDocument lsp.TextDocumentIdentifier `json:"textDocument"`
	Position     lsp.Position               `json:"position"`
}

type DefinitionParams struct {
	TextDocument lsp.TextDocumentIdentifier `json:"textDocument"`
	Position     lsp.Position               `json:"position"`
}

type ReferencesParams struct {
	TextDocument lsp.TextDocumentIdentifier `json:"textDocument"`
	Position     lsp.Position               `json:"position"`
	Context      struct {
		IncludeDeclaration bool `json:"includeDeclaration"`
	} `json:"context"`
}

type DocumentSymbolParams struct {
	TextDocument lsp.TextDocumentIdentifier `json:"textDocument"`
}

type RenameParams struct {
	TextDocument lsp.TextDocumentIdentifier `json:"textDocument"`
	Position     lsp.Position               `json:"position"`
	NewName      string                     `json:"newName"`
}

type SemanticTokensParams struct {
	TextDocument lsp.TextDocumentIdentifier `json:"textDocument"`
}

type WorkspaceSymbolParams struct {
	Query string `json:"query"`
}

// --- Core handler dispatch helpers ------------------------------------------

// resolveBackend looks up the language backend best suited for the given URI.
// Returns (backend, nil) on success or (nil, error) if no backend is registered.
func (s *Server) resolveBackend(uri string) (languages.Backend, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for langID, be := range s.languages {
		for _, ext := range be.FileExtensions() {
			if strings.HasSuffix(strings.ToLower(uri), ext) {
				return be, nil
			}
		}
		// Also match by file extension derived from URI path.
		basename := uri[strings.LastIndex(uri, "/")+1:]
		if strings.Contains(basename, ".") {
			ext := "." + strings.SplitN(basename, ".", 2)[1]
			if strings.EqualFold(ext, langID) || strings.HasSuffix(ext, "."+langID) {
				return be, nil
			}
		}
	}
	return nil, errors.New(errors.ErrBackendUnavailable, "resolve",
		fmt.Sprintf("no backend registered for URI %s", uri))
}

// buildContextProvider is optionally implemented by backends that derive a
// canonical build context (§E0). Backends without one report "unavailable".
type buildContextProvider interface {
	BuildContextID() identity.BuildContextID
}

func backendBuildContext(be languages.Backend) identity.BuildContextID {
	if p, ok := be.(buildContextProvider); ok {
		return p.BuildContextID()
	}
	return "unavailable"
}

// projectLocation converts a languages.Location to LSP protocol Locations.
func projectLocations(locs []languages.Location) []lsp.Location {
	out := make([]lsp.Location, len(locs))
	for i, l := range locs {
		out[i] = lsp.Location{
			URI: l.URI,
			Range: lsp.Range{
				Start: lsp.Position{Line: l.Range.StartLine, Character: l.Range.StartCharacter},
				End:   lsp.Position{Line: l.Range.EndLine, Character: l.Range.EndCharacter},
			},
		}
	}
	return out
}

// --- Lifecycle handlers -----------------------------------------------------

// handleInitialize transitions Uninitialized -> Initializing (C2).
// A second initialize is a protocol violation.
func (s *Server) handleInitialize(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	s.mu.Lock()
	if s.state != StateUninitialized {
		st := s.state
		s.mu.Unlock()
		return nil, &jsonrpc.ResponseError{
			Code:    jsonrpc.InvalidRequest,
			Message: fmt.Sprintf("initialize received in state %s", st),
		}
	}
	s.state = StateInitializing
	s.mu.Unlock()

	var params InitializeParams
	if msg.Params != nil {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			return nil, fmt.Errorf("invalid initialize params: %w", err)
		}
		if params.RootURI != "" {
			s.mu.Lock()
			s.workspaceID = identity.WorkspaceID(params.RootURI)
			s.mu.Unlock()
		}
		// §C9 negotiation: remember whether the client wants version-aware
		// documentChanges in future WorkspaceEdits.
		s.clientWantsDocumentChanges.Store(parseClientEditCapability(params.Capabilities))
		// §C4 position encoding negotiation: pick the client's first supported
		// encoding; canonical engine order is preference order. Default stays
		// UTF-16 (LSP baseline) when the client declares nothing.
		if enc := negotiatePositionEncoding(params.General.PositionEncodings); enc != "" {
			s.mu.Lock()
			s.positionEncoding = enc
			s.mu.Unlock()
		}
	}

	var negotiated string
	s.mu.RLock()
	negotiated = s.positionEncoding
	s.mu.RUnlock()
	result := InitializeResult{Capabilities: buildCapabilities(negotiated)}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal initialize result: %w", err)
	}
	return data, nil
}

// buildCapabilities assembles the advertised capability set (§C3). Every
// entry here must have a registered handler behind it and vice versa — a
// served-but-undeclared feature is unreachable, a declared-but-unserved one
// is a lie. TestI21_LegendMatchesEmittedTypes pins the tokens side.
func buildCapabilities(positionEncoding string) ServerCapabilities {
	return ServerCapabilities{
		PositionEncoding: positionEncoding,
		TextDocumentSync: &TextDocumentSyncOptions{
			OpenClose: true,
			Change:    2,
			Save:      &SaveOptions{IncludeText: true},
		},
		HoverProvider:          true,
		CompletionProvider:     &CompletionOptions{TriggerCharacters: []string{".", ":", ">", "\""}},
		DefinitionProvider:     true,
		DeclarationProvider:    true,
		ReferencesProvider:     true,
		DocumentSymbolProvider: true,
		RenameProvider:         true, // prepareRename served (I10); plain bool keeps the typed struct honest
		SemanticTokensProvider: &SemanticTokensOptions{
			Legend: SemanticTokensLegend{
				// §I21: legend indexes are the contract with backend emitters —
				// both sides read the same slice (languages.SemanticTokenTypes).
				TokenTypes:     languages.SemanticTokenTypes,
				TokenModifiers: []string{"declaration", "definition", "readonly"},
			},
			Full: true,
			// No delta handler is implemented; claiming incremental would
			// make clients request semanticTokens/delta and get
			// method-not-found (§C3 honesty).
		},
		WorkspaceSymbolProvider:    true,
		SignatureHelpProvider:      true,
		CodeActionProvider:         true,
		DiagnosticProvider:         true, // pull + push (C11)
		DocumentFormattingProvider: true,
		InlayHintProvider:          true,
	}
}

// handleInitialized transitions Initializing -> Running (C2).
func (s *Server) handleInitialized(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	s.mu.Lock()
	if s.state == StateInitializing {
		s.state = StateRunning
	}
	s.mu.Unlock()
	return nil, nil
}

// handleShutdown transitions to ShuttingDown; new semantic work is rejected
// afterwards (C2). Shutdown before initialize is a protocol violation.
func (s *Server) handleShutdown(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	s.mu.Lock()
	switch s.state {
	case StateRunning, StateInitializing:
		s.state = StateShuttingDown
		s.mu.Unlock()
		return json.RawMessage("null"), nil
	case StateShuttingDown:
		s.mu.Unlock()
		return json.RawMessage("null"), nil
	default:
		st := s.state
		s.mu.Unlock()
		return nil, &jsonrpc.ResponseError{
			Code:    jsonrpc.InvalidRequest,
			Message: fmt.Sprintf("shutdown received in state %s", st),
		}
	}
}

// handleExit terminates the connection per LSP semantics: any state -> Exited,
// transport closed so the Run loop returns (C2).
func (s *Server) handleExit(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	s.mu.Lock()
	s.state = StateExited
	t := s.transport
	s.mu.Unlock()
	if t != nil {
		_ = t.Close()
	}
	return nil, nil
}

// handleCancelRequest maps $/cancelRequest onto the scheduled request (C7).
// Unknown or already-finished IDs are ignored per LSP semantics.
func (s *Server) handleCancelRequest(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params struct {
		ID jsonrpc.RequestID `json:"id"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid $/cancelRequest params: %w", err)
	}
	s.cancelRequest(params.ID)
	return nil, nil
}

// --- Document synchronization handlers --------------------------------------

// handleDidOpen updates VFS and publishes a new Snapshot (F1, D9).
func (s *Server) handleDidOpen(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params DidOpenTextDocumentParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid didOpen params: %w", err)
	}
	s.vfs.Open(params.TextDocument.URI, params.TextDocument.LanguageID,
		params.TextDocument.Version, []byte(params.TextDocument.Text), vfs.SourceEditor)
	s.publishSnapshot()
	// §C11 push path: debounced diagnostics for the freshly opened document.
	s.diag.request(params.TextDocument.URI)
	return nil, nil
}

// handleDidChange applies content changes and publishes a new Snapshot (F1).
//
// PROT-SYNC-001: all content changes are applied sequentially as state
// transitions — none is dropped. Range edits are converted from UTF-16
// positions to byte offsets against the current intermediate text (D6).
// D6: an invalid range or a non-increasing version rejects the whole
// notification, leaving VFS content unchanged — never silent patching.
func (s *Server) handleDidChange(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params DidChangeTextDocumentParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid didChange params: %w", err)
	}
	if len(params.ContentChanges) == 0 {
		return nil, nil
	}
	uri := params.TextDocument.URI
	newVersion := params.TextDocument.Version

	if cur := s.vfs.Get(uri); cur != nil && newVersion <= cur.Version {
		s.syncRejects.Add(1)
		s.metrics.SyncRejects.Inc(1)
		return nil, errors.New(errors.ErrInvalidDocumentVersion, "didChange",
			fmt.Sprintf("version %d does not exceed current %d for %s", newVersion, cur.Version, uri)).
			WithWorkspace(string(s.workspaceID))
	}

	content := s.vfs.Content(uri)
	if content == nil {
		content = []byte{}
	}
	next, err := applyContentChanges(content, params.ContentChanges)
	if err != nil {
		s.syncRejects.Add(1)
		s.metrics.SyncRejects.Inc(1)
		return nil, errors.New(errors.ErrInvalidPosition, "didChange", err.Error()).
			WithWorkspace(string(s.workspaceID))
	}
	s.vfs.Update(uri, newVersion, next)
	s.publishSnapshot()

	// §C11 push path: debounced diagnostics after the edit lands.
	s.diag.request(uri)
	return nil, nil
}

func (s *Server) handleDidSave(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params DidSaveTextDocumentParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid didSave params: %w", err)
	}
	s.vfs.Save(params.TextDocument.URI)
	s.publishSnapshot()
	return nil, nil
}

func (s *Server) handleDidClose(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params DidCloseTextDocumentParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid didClose params: %w", err)
	}
	s.vfs.Close(params.TextDocument.URI)
	s.publishSnapshot()
	return nil, nil
}

// --- Semantic request handlers ----------------------------------------------

// handleHover dispatches to the appropriate language backend and projects
// the result to the LSP Hover response shape.
func (s *Server) handleHover(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params HoverParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid hover params: %w", err)
	}
	return s.dispatchSemanticRequest(ctx, msg, params.TextDocument.URI, params.Position.Line, params.Position.Character,
		func(be languages.Backend, src []byte, snapRev uint64, bc identity.BuildContextID) (json.RawMessage, error) {
			result, err := semanticViaEngine(s, ctx, "hover", params.TextDocument.URI, snapRev, bc,
				fmt.Sprintf("%d:%d", params.Position.Line, params.Position.Character),
				func() (identity.SemanticResult[*languages.HoverResult], error) {
					return be.Hover(ctx, languages.HoverRequest{
						URI:          params.TextDocument.URI,
						Content:      src,
						SnapshotRev:  snapRev,
						BuildContext: bc,
						Line:         params.Position.Line,
						Column:       params.Position.Character,
					})
				})
			if err != nil {
				return nil, err
			}
			s.recordEvidence("textDocument/hover", params.TextDocument.URI, result.Evidence, result.InternalDiagnostics)
			if result.Value == nil {
				// Exact negative or unknown: LSP projects both as null.
				return json.RawMessage("null"), nil
			}
			resp := struct {
				Contents lsp.MarkupContent `json:"contents"`
				Range    *lsp.Range        `json:"range,omitempty"`
			}{
				Contents: lsp.MarkupContent{Kind: "markdown", Value: result.Value.Contents},
			}
			if result.Value.Range != nil {
				resp.Range = &lsp.Range{
					Start: lsp.Position{Line: result.Value.Range.StartLine, Character: result.Value.Range.StartCharacter},
					End:   lsp.Position{Line: result.Value.Range.EndLine, Character: result.Value.Range.EndCharacter},
				}
			}
			return json.Marshal(resp)
		},
	)
}

// handleCompletion dispatches to the appropriate language backend.
func (s *Server) handleCompletion(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params CompletionParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid completion params: %w", err)
	}
	return s.dispatchSemanticRequest(ctx, msg, params.TextDocument.URI, params.Position.Line, params.Position.Character,
		func(be languages.Backend, src []byte, _ uint64, _ identity.BuildContextID) (json.RawMessage, error) {
			items, err := be.Completion(ctx, languages.CompletionRequest{
				URI: params.TextDocument.URI, Content: src,
				Line: params.Position.Line, Column: params.Position.Character,
			})
			if err != nil {
				return nil, err
			}
			lspItems := make([]lsp.CompletionItem, len(items))
			for i, it := range items {
				lspItems[i] = lsp.CompletionItem{
					Label:         it.Label,
					Kind:          lsp.CompletionItemKind(it.Kind),
					Detail:        it.Detail,
					Documentation: it.Documentation,
					InsertText:    it.InsertText,
					SortText:      it.SortText,
					FilterText:    it.FilterText,
				}
			}
			// §I9: honest incompleteness lets clients re-query as they type.
			incomplete := false
			if p, ok := be.(languages.IncompleteCompletionProvider); ok {
				incomplete = p.CompletionIsIncomplete()
			}
			return json.Marshal(lsp.CompletionList{IsIncomplete: incomplete, Items: lspItems})
		},
	)
}

// handleDefinition dispatches to the appropriate language backend.
func (s *Server) handleDefinition(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params DefinitionParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid definition params: %w", err)
	}
	return s.dispatchSemanticRequest(ctx, msg, params.TextDocument.URI, params.Position.Line, params.Position.Character,
		func(be languages.Backend, src []byte, snapRev uint64, bc identity.BuildContextID) (json.RawMessage, error) {
			result, err := semanticViaEngine(s, ctx, "definition", params.TextDocument.URI, snapRev, bc,
				fmt.Sprintf("%d:%d", params.Position.Line, params.Position.Character),
				func() (identity.SemanticResult[[]languages.Location], error) {
					return be.Definition(ctx, languages.DefinitionRequest{
						URI:          params.TextDocument.URI,
						Content:      src,
						SnapshotRev:  snapRev,
						BuildContext: bc,
						Line:         params.Position.Line,
						Column:       params.Position.Character,
					})
				})
			if err != nil {
				return nil, err
			}
			s.recordEvidence("textDocument/definition", params.TextDocument.URI, result.Evidence, result.InternalDiagnostics)
			if len(result.Value) == 0 {
				return json.RawMessage("null"), nil
			}
			return json.Marshal(projectLocations(result.Value))
		},
	)
}

// handleDeclaration serves textDocument/declaration (§I14/T2) via the
// optional DeclarationProvider capability. Backends without a declaration
// concept get a clean MethodNotFound-style refusal, not a silent alias.
func (s *Server) handleDeclaration(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params DefinitionParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid declaration params: %w", err)
	}
	return s.dispatchSemanticRequest(ctx, msg, params.TextDocument.URI, params.Position.Line, params.Position.Character,
		func(be languages.Backend, src []byte, snapRev uint64, bc identity.BuildContextID) (json.RawMessage, error) {
			dp, ok := be.(languages.DeclarationProvider)
			if !ok {
				return nil, &jsonrpc.ResponseError{Code: jsonrpc.MethodNotFound,
					Message: "declaration not supported by backend for " + params.TextDocument.URI}
			}
			result, err := semanticViaEngine(s, ctx, "declaration", params.TextDocument.URI, snapRev, bc,
				fmt.Sprintf("%d:%d", params.Position.Line, params.Position.Character),
				func() (identity.SemanticResult[[]languages.Location], error) {
					return dp.Declaration(ctx, languages.DefinitionRequest{
						URI:          params.TextDocument.URI,
						Content:      src,
						SnapshotRev:  snapRev,
						BuildContext: bc,
						Line:         params.Position.Line,
						Column:       params.Position.Character,
					})
				})
			if err != nil {
				return nil, err
			}
			s.recordEvidence("textDocument/declaration", params.TextDocument.URI, result.Evidence, result.InternalDiagnostics)
			if len(result.Value) == 0 {
				return json.RawMessage("null"), nil
			}
			return json.Marshal(projectLocations(result.Value))
		},
	)
}

// handleDocumentSymbol dispatches to the appropriate language backend.
func (s *Server) handleDocumentSymbol(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params DocumentSymbolParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid documentSymbol params: %w", err)
	}
	return s.dispatchSemanticRequest(ctx, msg, params.TextDocument.URI, 0, 0,
		func(be languages.Backend, src []byte, _ uint64, _ identity.BuildContextID) (json.RawMessage, error) {
			syms, err := be.DocumentSymbols(ctx, languages.DocumentSymbolRequest{
				URI: params.TextDocument.URI, Content: src,
			})
			if err != nil {
				return nil, err
			}
			lspSyms := make([]lsp.DocumentSymbol, len(syms))
			for i, s := range syms {
				lspSyms[i] = lsp.DocumentSymbol{
					Name:   s.Name,
					Detail: s.Detail,
					Kind:   lsp.SymbolKind(s.Kind),
					Range: lsp.Range{
						Start: lsp.Position{Line: s.StartLine, Character: s.StartCharacter},
						End:   lsp.Position{Line: s.EndLine, Character: s.EndCharacter},
					},
					SelectionRange: lsp.Range{
						Start: lsp.Position{Line: s.SelectionLine, Character: s.SelectionCharacter},
						End:   lsp.Position{Line: s.SelectionLine, Character: s.SelectionCharacter + uint32(len(s.Name))},
					},
					Children: projectChildren(s.Children),
				}
			}
			return json.Marshal(lspSyms)
		},
	)
}

func projectChildren(children []languages.DocumentSymbol) []lsp.DocumentSymbol {
	out := make([]lsp.DocumentSymbol, len(children))
	for i, c := range children {
		out[i] = lsp.DocumentSymbol{
			Name:   c.Name,
			Detail: c.Detail,
			Kind:   lsp.SymbolKind(c.Kind),
			Range: lsp.Range{
				Start: lsp.Position{Line: c.StartLine, Character: c.StartCharacter},
				End:   lsp.Position{Line: c.EndLine, Character: c.EndCharacter},
			},
			SelectionRange: lsp.Range{
				Start: lsp.Position{Line: c.SelectionLine, Character: c.SelectionCharacter},
				End:   lsp.Position{Line: c.SelectionLine, Character: c.SelectionCharacter + uint32(len(c.Name))},
			},
			Children: projectChildren(c.Children),
		}
	}
	return out
}

// handleReferences dispatches to the appropriate language backend.
func (s *Server) handleReferences(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params ReferencesParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid references params: %w", err)
	}
	// §C8: report progress when the client opted in with a workDoneToken.
	token := extractWorkDoneToken(msg.Params)
	s.progressBegin(token, "Finding references")
	defer s.progressEnd(token, "")
	return s.dispatchSemanticRequest(ctx, msg, params.TextDocument.URI, params.Position.Line, params.Position.Character,
		func(be languages.Backend, src []byte, snapRev uint64, bc identity.BuildContextID) (json.RawMessage, error) {
			result, err := semanticViaEngine(s, ctx, "references", params.TextDocument.URI, snapRev, bc,
				fmt.Sprintf("%d:%d:decl=%v", params.Position.Line, params.Position.Character, params.Context.IncludeDeclaration),
				func() (identity.SemanticResult[[]languages.Location], error) {
					return be.References(ctx, languages.ReferencesRequest{
						URI:          params.TextDocument.URI,
						Content:      src,
						SnapshotRev:  snapRev,
						BuildContext: bc,
						Line:         params.Position.Line,
						Column:       params.Position.Character,
						IncludeDecl:  params.Context.IncludeDeclaration,
						Encoding:     s.negotiatedEncodingInt(),
					})
				})
			if err != nil {
				return nil, err
			}
			s.recordEvidence("textDocument/references", params.TextDocument.URI, result.Evidence, result.InternalDiagnostics)
			if len(result.Value) == 0 {
				return json.RawMessage("[]"), nil
			}
			return json.Marshal(projectLocations(result.Value))
		},
	)
}

// handleRename dispatches to the appropriate language backend.
// SEM-SAFE-001: rename is S3 — an unproven result fails closed with a typed
// error carrying the reason; it is never projected as a WorkspaceEdit.
func (s *Server) handleRename(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params RenameParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid rename params: %w", err)
	}
	// §Y0/§D12 S3 gate: a rename must not cross an UnmappedGenerated region.
	// The concrete edit span is only known after backend analysis, so any
	// virtual document whose map cannot prove safety fails closed here
	// (SEM-SAFE-001). Plain host documents are untouched.
	if reg := s.VirtualRegistry(); reg.IsVirtual(params.TextDocument.URI) {
		if err := virtual.ValidateEditSpan(reg, params.TextDocument.URI, 0, ^uint32(0)); err != nil {
			return nil, &jsonrpc.ResponseError{
				Code:    jsonrpc.RequestFailed,
				Message: fmt.Sprintf("rename rejected on generated/virtual document: %v", err),
			}
		}
	}
	return s.dispatchSemanticRequest(ctx, msg, params.TextDocument.URI, params.Position.Line, params.Position.Character,
		func(be languages.Backend, src []byte, snapRev uint64, bc identity.BuildContextID) (json.RawMessage, error) {
			// D11/§B7: a mutating (S3) result must be fresh. If any edit
			// landed after this request captured its snapshot, refuse with
			// the LSP ContentModified code so clients retry transparently.
			if captured := snapshotFromCtx(ctx); captured != nil {
				if cur := s.snapMgr.Current(); cur != nil && cur.ID().Revision != captured.ID().Revision {
					return nil, &jsonrpc.ResponseError{
						Code: jsonrpc.ContentModified,
						Message: fmt.Sprintf("workspace changed since request (snapshot %d, current %d)",
							captured.ID().Revision, cur.ID().Revision),
					}
				}
			}
			result, err := be.Rename(ctx, languages.RenameRequest{
				URI:          params.TextDocument.URI,
				Content:      src,
				SnapshotRev:  snapRev,
				BuildContext: bc,
				Line:         params.Position.Line,
				Column:       params.Position.Character,
				NewName:      params.NewName,
				Encoding:     s.negotiatedEncodingInt(),
			})
			if err != nil {
				return nil, err
			}
			s.recordEvidence("textDocument/rename", params.TextDocument.URI, result.Evidence, result.InternalDiagnostics)
			if result.Status != identity.ResultExact || !result.Value.Complete {
				reason := "rename unavailable: completeness not proven"
				if len(result.InternalDiagnostics) > 0 {
					reason = strings.Join(result.InternalDiagnostics, "; ")
				}
				return nil, &jsonrpc.ResponseError{
					Code:    jsonrpc.RequestFailed,
					Message: reason,
				}
			}
			if len(result.Value.Edits) == 0 {
				return json.Marshal(struct{ Changes json.RawMessage }{json.RawMessage("null")})
			}
			// Group edits by URI into a TextEditMap.
			changeMap := make(map[string][]lsp.TextEdit)
			for _, e := range result.Value.Edits {
				changeMap[e.URI] = append(changeMap[e.URI], lsp.TextEdit{
					Range: lsp.Range{
						Start: lsp.Position{Line: e.StartLine, Character: e.StartChar},
						End:   lsp.Position{Line: e.EndLine, Character: e.EndChar},
					},
					NewText: e.NewText,
				})
			}
			// §C9 SHOULD: version-aware documentChanges when the client
			// negotiated it; legacy changes form otherwise.
			if s.wantsDocumentChanges() {
				version := 0
				if cur := s.snapMgr.Current(); cur != nil {
					version = int(cur.ID().Revision)
				}
				docs := make([]lsp.TextDocumentEdit, 0, len(changeMap))
				for uri, edits := range changeMap {
					docs = append(docs, lsp.TextDocumentEdit{
						TextDocument: lsp.OptionalVersionedTextDocumentIdentifier{
							URI:     lsp.DocumentURI(uri),
							Version: version,
						},
						Edits: edits,
					})
				}
				sort.Slice(docs, func(i, j int) bool { return docs[i].TextDocument.URI < docs[j].TextDocument.URI })
				return json.Marshal(lsp.WorkspaceEdit{DocumentChanges: docs})
			}
			return json.Marshal(lsp.WorkspaceEdit{Changes: changeMap})
		},
	)
}

// handleSemanticTokens dispatches to the appropriate language backend.
func (s *Server) handleSemanticTokens(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params SemanticTokensParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid semanticTokens params: %w", err)
	}
	return s.dispatchSemanticRequest(ctx, msg, params.TextDocument.URI, 0, 0,
		func(be languages.Backend, src []byte, _ uint64, _ identity.BuildContextID) (json.RawMessage, error) {
			tokens, err := be.SemanticTokens(ctx, params.TextDocument.URI, src)
			if err != nil {
				return nil, err
			}
			// §I21 wire format: five uint32 per token in a flat "data" array —
			// NOT an array of objects. Marshalling the struct slice directly
			// produces protocol-invalid responses.
			data := make([]uint32, 0, len(tokens)*5)
			for _, tk := range tokens {
				data = append(data, tk.DeltaLine, tk.DeltaStart, tk.Length, tk.TokenType, tk.TokenMods)
			}
			return json.Marshal(struct {
				Data []uint32 `json:"data"`
			}{Data: data})
		},
	)
}

// handleWorkspaceSymbol searches symbols across the workspace via the matching backend.
func (s *Server) handleWorkspaceSymbol(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params WorkspaceSymbolParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid workspaceSymbol params: %w", err)
	}
	// §C8: report progress when the client opted in with a workDoneToken.
	token := extractWorkDoneToken(msg.Params)
	s.progressBegin(token, "Searching workspace symbols")
	defer s.progressEnd(token, "")
	// F2 lock discipline: findWorkspaceBackend takes its own RLock — never
	// nest it under an outer read lock, or a pending writer deadlocks both
	// (found via TestS13_P0FloodDoesNotStarveP4 fault injection).
	be := s.findWorkspaceBackend()
	if be == nil {
		return json.RawMessage("null"), nil
	}
	syms, err := be.WorkspaceSymbols(ctx, languages.WorkspaceSymbolRequest{Query: params.Query, Limit: 100})
	if err != nil {
		return nil, err
	}
	lspSyms := make([]lsp.WorkspaceSymbol, len(syms))
	for i, s := range syms {
		lspSyms[i] = lsp.WorkspaceSymbol{
			Name: s.Name,
			Kind: lsp.SymbolKind(s.Kind),
			Location: lsp.Location{
				URI: s.URI,
				Range: lsp.Range{
					Start: lsp.Position{Line: s.StartLine, Character: s.StartCol},
					End:   lsp.Position{Line: s.StartLine, Character: s.StartCol + uint32(len(s.Name))},
				},
			},
		}
	}
	return json.Marshal(lspSyms)
}

// findWorkspaceBackend returns any registered backend suitable for workspace-wide symbol search.
func (s *Server) findWorkspaceBackend() languages.Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, be := range s.languages {
		return be // First registered backend handles workspace symbols.
	}
	return nil
}

// dispatchSemanticRequest is the unified dispatch path for all semantic
// request handlers. It:
//  1. Resolves the language backend for the URI.
//  2. Reads document content from the request's CAPTURED snapshot when
//     available (INV-SNAPSHOT-002: one request, one snapshot). Live VFS is a
//     fallback only for documents absent from the snapshot.
//  3. Fills §B5 identity inputs (snapshot revision, build context) into the
//     request via the project callback.
//  4. Wraps the result JSON in a proper jsonrpc.Message response.
func (s *Server) dispatchSemanticRequest(
	ctx context.Context,
	msg *jsonrpc.Message,
	uri string,
	line, column uint32,
	project func(be languages.Backend, src []byte, snapRev uint64, bc identity.BuildContextID) (json.RawMessage, error),
) (json.RawMessage, error) {
	be, err := s.resolveBackend(uri)
	if err != nil {
		s.metrics.BackendFails.Inc(1)
		return nil, err
	}
	// The captured snapshot is authoritative for this request.
	captured := snapshotFromCtx(ctx)
	snapRev := uint64(0)
	src := s.vfs.Content(uri)
	if captured != nil {
		snapRev = captured.ID().Revision
		if doc := captured.Document(uri); doc != nil && doc.Content != nil {
			src = doc.Content
		}
	} else if snap := s.snapMgr.Current(); snap != nil {
		snapRev = snap.ID().Revision
	}
	bc := backendBuildContext(be)
	if src == nil {
		// File never opened — nothing to analyze; let the backend decide.
		src = []byte{}
	}
	resultJSON, err := project(be, src, snapRev, bc)
	if err != nil {
		return nil, err
	}
	if resultJSON == nil {
		resultJSON = json.RawMessage("null")
	}
	return resultJSON, nil
}

// handleOmnilspStatus returns runtime status per goal.md §C12, including
// §K0 observability counters: scheduler totals (joined = requests served by
// an in-flight twin) and aggregate backend cache hit/miss.
func (s *Server) handleOmnilspStatus(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	stats := s.scheduler.Stats()
	var cacheHits, cacheMisses int64
	s.mu.RLock()
	for _, be := range s.languages {
		if p, ok := be.(interface{ CacheStats() (int64, int64) }); ok {
			h, m := p.CacheStats()
			cacheHits += h
			cacheMisses += m
		}
	}
	s.mu.RUnlock()
	return json.Marshal(struct {
		State              string
		QueueLen           int
		InFlight           int64
		TotalEnq           int64
		TotalExec          int64
		TotalRej           int64
		TotalJoined        int64
		SyncRejects        int64
		CacheHits          int64
		CacheMisses        int64
		MetricRequests     int64 `json:",omitempty"`
		MetricRejects      int64 `json:",omitempty"`
		MetricBackendFails int64
		SnapshotEpochs     int64
	}{
		State:              s.State().String(),
		QueueLen:           s.scheduler.QueueLen(),
		InFlight:           stats.InFlight,
		TotalEnq:           stats.TotalEnqueued,
		TotalExec:          stats.TotalExecuted,
		TotalRej:           stats.TotalRejected,
		TotalJoined:        stats.TotalJoined,
		SyncRejects:        s.syncRejects.Load(),
		CacheHits:          cacheHits,
		CacheMisses:        cacheMisses,
		MetricRequests:     s.metrics.RequestsTotal.Value(),
		MetricRejects:      s.metrics.RejectsTotal.Value(),
		MetricBackendFails: s.metrics.BackendFails.Value(),
		SnapshotEpochs:     s.metrics.SnapshotEpochs.Value(),
	})
}

// handleOmnilspExplain reports the engineering evidence behind recent
// semantic results per goal.md §I25/§C12 — what source of truth was used,
// which snapshot/build context, and why anything was rejected.
func (s *Server) handleOmnilspExplain(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	type explainReq struct {
		URI string `json:"uri"`
	}
	var req explainReq
	if msg.Params != nil {
		_ = json.Unmarshal(msg.Params, &req)
	}
	snap := s.snapMgr.Current()
	var rev uint64
	if snap != nil {
		rev = snap.ID().Revision
	}

	type evidenceJSON struct {
		Method       string   `json:"method"`
		URI          string   `json:"uri"`
		Kind         string   `json:"kind"`
		Assurance    string   `json:"assurance"`
		SnapshotRev  uint64   `json:"snapshotRev"`
		BuildContext string   `json:"buildContext"`
		Backend      string   `json:"backend"`
		SourceHash   string   `json:"sourceHash"`
		Detail       string   `json:"detail,omitempty"`
		Diagnostics  []string `json:"diagnostics,omitempty"`
		At           string   `json:"at"`
	}
	records := s.recentEvidence()
	out := make([]evidenceJSON, 0, len(records))
	for _, r := range records {
		if req.URI != "" && r.URI != req.URI {
			continue
		}
		for _, ev := range r.Ev {
			e := evidenceJSON{
				Method:       r.Method,
				URI:          r.URI,
				Kind:         ev.Kind.String(),
				Assurance:    assuranceString(ev.Assurance),
				SnapshotRev:  uint64(ev.Snapshot.Revision),
				BuildContext: string(ev.BuildContext),
				Backend:      ev.Backend.Language + "/" + ev.Backend.Name,
				SourceHash:   string(ev.SourceHash),
				Detail:       ev.DetailCode,
				Diagnostics:  r.Diag,
				At:           r.At.Format(time.RFC3339Nano),
			}
			out = append(out, e)
		}
	}
	return json.Marshal(struct {
		Snapshot    uint64         `json:"snapshot"`
		SyncRejects int64          `json:"syncRejects"`
		Evidence    []evidenceJSON `json:"evidence"`
	}{Snapshot: rev, SyncRejects: s.syncRejects.Load(), Evidence: out})
}

func assuranceString(a identity.Assurance) string {
	switch a {
	case identity.AssuranceLexical:
		return "lexical"
	case identity.AssuranceSyntax:
		return "syntax"
	case identity.AssuranceIndexedExact:
		return "indexed_exact"
	case identity.AssuranceCompilerResolved:
		return "compiler_resolved"
	default:
		return "unknown"
	}
}

// handleOmnilspBackendStatus returns backend health per goal.md §C12.
func (s *Server) handleOmnilspBackendStatus(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	type bsReq struct {
		Language string
	}
	var req bsReq
	if msg.Params != nil {
		json.Unmarshal(msg.Params, &req)
	}
	s.mu.RLock()
	be := s.languages[req.Language]
	s.mu.RUnlock()
	return json.Marshal(struct {
		Language string
		Found    bool
	}{Language: req.Language, Found: be != nil})
}

// publishSnapshot constructs an immutable snapshot from VFS and publishes atomically (D9).
func (s *Server) publishSnapshot() {
	rev := s.vfs.Revision()
	// §J7: a revision advance invalidates memo entries bound to older
	// revisions, so semantic reads recompute against the new snapshot.
	s.queries.InvalidateSnapshot(rev)
	files := s.vfs.OpenFiles()
	docs := make(map[string]snapshot.DocumentSnapshot, len(files))
	for _, uri := range files {
		f := s.vfs.Get(uri)
		if f != nil {
			docs[uri] = snapshot.DocumentSnapshot{
				URI: f.URI, LanguageID: f.LanguageID,
				Version: f.Version, Content: f.Content,
			}
		}
	}
	s.snapMgr.Publish(snapshot.New(string(s.workspaceID), rev, docs))
	s.metrics.SnapshotEpochs.Inc(1)
}

// negotiatePositionEncoding picks the first client-proposed encoding this
// engine implements (§C4). The engine natively supports all three; empty
// input means the client said nothing and the UTF-16 baseline stands.
// negotiatedEncodingInt maps the §C4 negotiation result onto the wire int
// every languages request struct carries (0=UTF8, 1=UTF16, 2=UTF32) — this
// activates what was a dead parameter before the negotiation existed.
func (s *Server) negotiatedEncodingInt() int {
	s.mu.RLock()
	enc := s.positionEncoding
	s.mu.RUnlock()
	switch enc {
	case "utf-8":
		return 0
	case "utf-32":
		return 2
	default:
		return 1
	}
}

func negotiatePositionEncoding(proposals []string) string {
	supported := map[string]bool{"utf-8": true, "utf-16": true, "utf-32": true}
	for _, p := range proposals {
		if supported[strings.ToLower(p)] {
			return strings.ToLower(p)
		}
	}
	return ""
}

// workspaceRoot resolves the on-disk root for the §D14 poller.
func (s *Server) workspaceRoot() string {
	s.mu.RLock()
	ws := string(s.workspaceID)
	s.mu.RUnlock()
	if ws == "" {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(ws, "file://"), "file:")
}

// handleDidChangeWatchedFiles records client-reported external changes (§D14).
// Open documents stay editor-authoritative; the notification's v1 value is
// observability: it proves the channel and counts churn per session.
func (s *Server) handleDidChangeWatchedFiles(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params struct {
		Changes []struct {
			URI  string `json:"uri"`
			Type int    `json:"type"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid didChangeWatchedFiles params: %w", err)
	}
	s.metrics.ExternalSyncs.Inc(1) // reuse the sync counter: external-change traffic
	return nil, nil
}

// handleDidCreateFiles / Rename / Delete (§D15): closed-document lifecycle
// notifications. v1 semantics: open documents are untouched (editor wins);
// the notifications are accepted, counted, and acknowledged.
func (s *Server) handleDidCreateFiles(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	s.metrics.ExternalSyncs.Inc(1)
	return nil, nil
}

func (s *Server) handleDidRenameFiles(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	s.metrics.ExternalSyncs.Inc(1)
	return nil, nil
}

func (s *Server) handleDidDeleteFiles(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	s.metrics.ExternalSyncs.Inc(1)
	return nil, nil
}
