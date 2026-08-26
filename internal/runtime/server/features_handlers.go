package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// Optional-capability handlers (§I16/§I20/§I22): the backend is type-asserted
// against small optional interfaces; a bridge that cannot honor the feature
// answers with a clean not-supported error instead of stub methods on every
// implementation.

// errNotSupported builds the canonical refusal for an unimplemented optional
// capability.
func errNotSupported(method, backend string) *jsonrpc.ResponseError {
	return &jsonrpc.ResponseError{
		Code:    jsonrpc.MethodNotFound,
		Message: fmt.Sprintf("%s not supported by backend %q", method, backend),
	}
}

// handleSignatureHelp dispatches textDocument/signatureHelp (§I16).
func (s *Server) handleSignatureHelp(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params HoverParams // same position-only shape
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid signatureHelp params: %w", err)
	}
	be, err := s.resolveBackend(params.TextDocument.URI)
	if err != nil {
		s.metrics.BackendFails.Inc(1)
		return nil, err
	}
	helper, ok := be.(languages.SignatureHelper)
	if !ok {
		return nil, errNotSupported("signatureHelp", be.LanguageID())
	}
	src := s.vfs.Content(params.TextDocument.URI)
	if src == nil {
		src = []byte{}
	}
	result, err := helper.SignatureHelp(ctx, languages.SignatureHelpRequest{
		URI:          params.TextDocument.URI,
		Content:      src,
		SnapshotRev:  s.currentRevision(),
		BuildContext: backendBuildContext(be),
		Line:         params.Position.Line,
		Column:       params.Position.Character,
	})
	if err != nil {
		return nil, err
	}
	s.recordEvidence("textDocument/signatureHelp", params.TextDocument.URI, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
	if result.Value == nil {
		return json.RawMessage("null"), nil
	}
	type param struct {
		Label string `json:"label"`
	}
	type sig struct {
		Label           string  `json:"label"`
		Parameters      []param `json:"parameters,omitempty"`
		ActiveParameter int     `json:"activeParameter,omitempty"`
	}
	resp := struct {
		Signatures      []sig `json:"signatures"`
		ActiveSignature int   `json:"activeSignature"`
	}{ActiveSignature: result.Value.ActiveSignature}
	for _, si := range result.Value.Signatures {
		out := sig{Label: si.Label, ActiveParameter: si.ActiveParameter}
		for _, p := range si.Parameters {
			out.Parameters = append(out.Parameters, param{Label: p})
		}
		resp.Signatures = append(resp.Signatures, out)
	}
	return json.Marshal(resp)
}

// handleFormatting dispatches textDocument/formatting (§I20).
func (s *Server) handleFormatting(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params struct {
		TextDocument lspTextDocumentIdentifier `json:"textDocument"`
		Options      struct {
			TabSize      int  `json:"tabSize"`
			InsertSpaces bool `json:"insertSpaces"`
		} `json:"options"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid formatting params: %w", err)
	}
	be, err := s.resolveBackend(params.TextDocument.URI)
	if err != nil {
		s.metrics.BackendFails.Inc(1)
		return nil, err
	}
	formatter, ok := be.(languages.Formatter)
	if !ok {
		return nil, errNotSupported("formatting", be.LanguageID())
	}
	src := s.vfs.Content(params.TextDocument.URI)
	edits, err := formatter.Formatting(ctx, languages.FormattingRequest{
		URI:          params.TextDocument.URI,
		Content:      src,
		TabSize:      params.Options.TabSize,
		InsertSpaces: params.Options.InsertSpaces,
	})
	if err != nil {
		return nil, err
	}
	if len(edits) == 0 {
		return json.RawMessage("null"), nil // already canonical (LSP null = no-op)
	}
	type rng struct {
		Start lspPosition `json:"start"`
		End   lspPosition `json:"end"`
	}
	type editJSON struct {
		Range   rng    `json:"range"`
		NewText string `json:"newText"`
	}
	out := make([]editJSON, 0, len(edits))
	for _, e := range edits {
		out = append(out, editJSON{
			Range: rng{
				Start: lspPosition{Line: e.StartLine, Character: e.StartChar},
				End:   lspPosition{Line: e.EndLine, Character: e.EndChar},
			},
			NewText: e.NewText,
		})
	}
	return json.Marshal(out)
}

// handleInlayHints dispatches textDocument/inlayHints (§I22).
func (s *Server) handleInlayHints(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params struct {
		TextDocument lspTextDocumentIdentifier `json:"textDocument"`
		Range        struct {
			Start lspPosition `json:"start"`
			End   lspPosition `json:"end"`
		} `json:"range"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid inlayHint params: %w", err)
	}
	be, err := s.resolveBackend(params.TextDocument.URI)
	if err != nil {
		s.metrics.BackendFails.Inc(1)
		return nil, err
	}
	provider, ok := be.(languages.InlayHintProvider)
	if !ok {
		return nil, errNotSupported("inlayHint", be.LanguageID())
	}
	src := s.vfs.Content(params.TextDocument.URI)
	hints, err := provider.InlayHints(ctx, languages.InlayHintRequest{
		URI:          params.TextDocument.URI,
		Content:      src,
		SnapshotRev:  s.currentRevision(),
		BuildContext: backendBuildContext(be),
		StartLine:    params.Range.Start.Line,
		EndLine:      params.Range.End.Line,
	})
	if err != nil {
		return nil, err
	}
	if len(hints) == 0 {
		return json.RawMessage("[]"), nil
	}
	type pos struct {
		Line      uint32 `json:"line"`
		Character uint32 `json:"character"`
	}
	type hintJSON struct {
		Position pos    `json:"position"`
		Label    string `json:"label"`
		Kind     string `json:"kind,omitempty"`
	}
	out := make([]hintJSON, 0, len(hints))
	for _, h := range hints {
		out = append(out, hintJSON{
			Position: pos{Line: h.Line, Character: h.Column},
			Label:    h.Label,
			Kind:     h.Kind,
		})
	}
	return json.Marshal(out)
}

// currentRevision reports the latest snapshot revision (0 if none yet).
func (s *Server) currentRevision() uint64 {
	if cur := s.snapMgr.Current(); cur != nil {
		return cur.ID().Revision
	}
	return 0
}

// Small local aliases keep this file free of extra imports; shapes mirror
// protocol/lsp types used elsewhere in handlers.go.
type lspTextDocumentIdentifier struct {
	URI string `json:"uri"`
}

type lspPosition struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

var _ = identity.ResultExact // keep identity import for future evidence use
