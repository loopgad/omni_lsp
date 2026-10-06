// Invariants:
//  1. Rename fails closed without tsconfig.json/jsconfig.json.
//  2. All wire traffic flows through nested.Conn single-reader supervision.
//  3. Errors carry typed identity per internal/errors conventions.
//
// Package typescript bridges TS/JS semantics to a nested
// typescript-language-server through the shared stdio bridge (goal.md §G1/X4).
//
// Same normalization contract as ccls: raw LSP output never escapes without
// a canonical envelope and §B4 evidence attributing resolution upstream.
package typescript

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/lspwire"
	"github.com/omnilsp/omni/internal/languages/nested"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

// ErrToolchainMissing reports that typescript-language-server itself is
// absent so main can skip registering this language pack instead of failing
// startup.
var ErrToolchainMissing = errors.New("typescript-language-server not found")

const (
	serverName = "tsserver"
	langID     = "typescript"
)

type Backend struct {
	conn    *nested.Conn
	workDir string
	cfgFile string // tsconfig.json presence gates project-wide rename (§I10)

	gateOnce   sync.Once
	hasCfgFile bool
}

// New wires the production typescript bridge. A missing binary is a
// sentinel (errors.Is-able), not a fatal misconfiguration.
func New(workDir string) (*Backend, error) {
	if _, err := exec.LookPath("typescript-language-server"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrToolchainMissing, err)
	}
	b := &Backend{workDir: workDir, cfgFile: "tsconfig.json"}
	b.conn = nested.New(nested.Config{
		Name:                        serverName,
		Lang:                        langID,
		WorkDir:                     workDir,
		Start:                       spawnServer,
		VersionProbe:                probeTscVersion,
		ParseVersion:                parseTscVersion,
		CaptureDiagnostics:          true,
		AllowUnversionedDiagnostics: true,
	})
	if err := b.conn.StartSupervised(); err != nil {
		return nil, err
	}
	return b, nil
}

// spawnServer is the production process factory (§G6: argument array).
func spawnServer(c *nested.Conn) error {
	cmd := exec.Command("typescript-language-server", "--stdio")
	cmd.Dir = c.WorkDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", serverName, err)
	}
	c.Attach(cmd, stdin, stdout)
	if err := c.Initialize(); err != nil {
		return fmt.Errorf("handshake %s: %w", serverName, err)
	}
	c.MarkReady()
	return nil
}

func probeTscVersion() (string, error) {
	out, err := exec.Command("tsc", "--version").CombinedOutput()
	return string(out), err
}

// parseTscVersion extracts the version token from "Version 5.3.3" (some tsc
// builds print "Version:" with a colon).
func parseTscVersion(output string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	for i, f := range fields {
		if strings.TrimSuffix(f, ":") == "Version" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", errors.New("tsc --version: unparseable output")
}

func (b *Backend) LanguageID() string { return langID }

// BackendStatusMessage projects the supervisor lifecycle state (§Q2).
func (b *Backend) BackendStatusMessage() string { return b.conn.SupervisorMessage() }
func (b *Backend) FileExtensions() []string     { return []string{".ts", ".tsx", ".js", ".jsx"} }

func documentLanguageID(documentURI string) string {
	document, err := uri.Parse(documentURI)
	if err != nil {
		return langID
	}
	path, err := document.Path()
	if err != nil {
		return langID
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".js":
		return "javascript"
	case ".jsx":
		return "javascriptreact"
	case ".tsx":
		return "typescriptreact"
	default:
		return langID
	}
}

func (b *Backend) BuildContextID() identity.BuildContextID { return b.conn.BuildContextID() }
func (b *Backend) SupervisorEpoch() uint64                 { return b.conn.SupervisorEpoch() }

func (b *Backend) currentBackendEpoch() identity.BackendEpoch {
	if b.conn == nil {
		return 0
	}
	return identity.BackendEpoch(b.SupervisorEpoch())
}

func (b *Backend) Completion(ctx context.Context, req languages.CompletionRequest) ([]languages.CompletionItem, error) {
	result, err := b.CompletionList(ctx, req)
	return result.Items, err
}

func (b *Backend) CompletionList(ctx context.Context, req languages.CompletionRequest) (languages.CompletionList, error) {
	result, err := b.conn.SendRequestAtRevision(ctx, documentLanguageID(req.URI), req.URI, req.Content, req.SnapshotRev, "textDocument/completion", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	})
	if err != nil {
		return languages.CompletionList{}, err
	}
	return lspwire.DecodeCompletionList(result)
}

func (b *Backend) Hover(ctx context.Context, req languages.HoverRequest) (envelope identity.SemanticResult[*languages.HoverResult], retErr error) {
	epoch := b.currentBackendEpoch()
	defer func() { envelope = languages.WithBackendEpoch(envelope, epoch) }()
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, documentLanguageID(req.URI), req.URI, req.Content, req.SnapshotRev, "textDocument/hover", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		return unknownHover(req, serverName+" request failed: "+err.Error()), err
	}
	if result == nil {
		return identity.SemanticResult[*languages.HoverResult]{
			Status:   identity.ResultExact,
			Value:    nil,
			Evidence: evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, "no-hover"),
		}, nil
	}
	var hover struct {
		Contents struct {
			Value string `json:"value"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(result, &hover); err != nil {
		return unknownHover(req, "hover decode failed"), err
	}
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value: &languages.HoverResult{
			Contents: hover.Contents.Value,
			Evidence: languages.EvidenceL3,
		},
		Evidence:     evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, serverName+"-resolved"),
		Completeness: identity.Complete,
	}, nil
}

func unknownHover(req languages.HoverRequest, detail string) identity.SemanticResult[*languages.HoverResult] {
	return identity.SemanticResult[*languages.HoverResult]{
		Status:              identity.ResultUnknown,
		Evidence:            evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, detail),
		Completeness:        identity.CompletenessUnknown,
		InternalDiagnostics: []string{detail},
	}
}

func (b *Backend) Definition(ctx context.Context, req languages.DefinitionRequest) (envelope identity.SemanticResult[[]languages.Location], retErr error) {
	epoch := b.currentBackendEpoch()
	defer func() { envelope = languages.WithBackendEpoch(envelope, epoch) }()
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, documentLanguageID(req.URI), req.URI, req.Content, req.SnapshotRev, "textDocument/definition", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content, serverName+" request failed: "+err.Error()), err
	}
	if result == nil {
		return identity.SemanticResult[[]languages.Location]{
			Status:   identity.ResultExact,
			Value:    nil,
			Evidence: evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, "no-definition"),
		}, nil
	}
	var raw lspLocationList
	if err := json.Unmarshal(result, &raw); err != nil {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content, "definition decode failed"), err
	}
	return identity.SemanticResult[[]languages.Location]{
		Status:       identity.ResultExact,
		Value:        toLocations(raw),
		Evidence:     evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, serverName+"-resolved"),
		Completeness: identity.Complete,
	}, nil
}

func unknownLocs(rev uint64, bc identity.BuildContextID, content []byte, detail string) identity.SemanticResult[[]languages.Location] {
	return identity.SemanticResult[[]languages.Location]{
		Status:              identity.ResultUnknown,
		Evidence:            evidenceForTs(rev, bc, content, detail),
		Completeness:        identity.CompletenessUnknown,
		InternalDiagnostics: []string{detail},
	}
}

func (b *Backend) References(ctx context.Context, req languages.ReferencesRequest) (envelope identity.SemanticResult[[]languages.Location], retErr error) {
	epoch := b.currentBackendEpoch()
	defer func() { envelope = languages.WithBackendEpoch(envelope, epoch) }()
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, documentLanguageID(req.URI), req.URI, req.Content, req.SnapshotRev, "textDocument/references", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
		"context":      map[string]bool{"includeDeclaration": req.IncludeDecl},
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content, serverName+" request failed: "+err.Error()), err
	}
	if result == nil {
		return identity.SemanticResult[[]languages.Location]{
			Status:   identity.ResultExact,
			Value:    nil,
			Evidence: evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, "no-references"),
		}, nil
	}
	var raw lspLocationList
	if err := json.Unmarshal(result, &raw); err != nil {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content, "references decode failed"), err
	}
	// The server enumerates over its loaded project; the bridge inherits
	// that proof but cannot independently verify scope (§G1 normalization).
	return identity.SemanticResult[[]languages.Location]{
		Status:       identity.ResultPartial,
		Value:        toLocations(raw),
		Evidence:     evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, serverName+"-index-scope"),
		Completeness: identity.IncompleteKnownSubset,
	}, nil
}

func (b *Backend) DocumentSymbols(ctx context.Context, req languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	result, err := b.conn.SendRequestAtRevision(ctx, documentLanguageID(req.URI), req.URI, req.Content, req.SnapshotRev, "textDocument/documentSymbol", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
	})
	if err != nil || result == nil {
		return nil, err
	}
	return decodeDocumentSymbols(result)
}

type documentSymbolPosition struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

type documentSymbolRange struct {
	Start documentSymbolPosition `json:"start"`
	End   documentSymbolPosition `json:"end"`
}

type documentSymbolWire struct {
	Name           string               `json:"name"`
	Detail         string               `json:"detail"`
	Kind           int                  `json:"kind"`
	Range          documentSymbolRange  `json:"range"`
	SelectionRange *documentSymbolRange `json:"selectionRange"`
	Children       []documentSymbolWire `json:"children"`
}

type symbolInformationWire struct {
	Name     string `json:"name"`
	Kind     int    `json:"kind"`
	Location struct {
		Range documentSymbolRange `json:"range"`
	} `json:"location"`
}

// decodeDocumentSymbols accepts both LSP document symbol response forms.
// Nested DocumentSymbol entries retain their selection range and children;
// flat SymbolInformation entries can only prove the location range, so that
// range is used for both the symbol span and its selection position.
func decodeDocumentSymbols(result []byte) ([]languages.DocumentSymbol, error) {
	if len(result) == 0 {
		return nil, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(result, &entries); err != nil {
		return nil, fmt.Errorf("document symbol decode: %w", err)
	}
	if len(entries) == 0 {
		return nil, nil
	}
	syms := make([]languages.DocumentSymbol, 0, len(entries))
	for i, entry := range entries {
		var shape struct {
			Location json.RawMessage `json:"location"`
		}
		if err := json.Unmarshal(entry, &shape); err != nil {
			return nil, fmt.Errorf("document symbol %d decode: %w", i, err)
		}
		if len(shape.Location) != 0 && string(shape.Location) != "null" {
			var info symbolInformationWire
			if err := json.Unmarshal(entry, &info); err != nil {
				return nil, fmt.Errorf("symbol information %d decode: %w", i, err)
			}
			syms = append(syms, projectSymbolInformation(info))
			continue
		}
		var symbol documentSymbolWire
		if err := json.Unmarshal(entry, &symbol); err != nil {
			return nil, fmt.Errorf("document symbol %d decode: %w", i, err)
		}
		syms = append(syms, projectDocumentSymbol(symbol))
	}
	return syms, nil
}

func projectDocumentSymbol(raw documentSymbolWire) languages.DocumentSymbol {
	selection := raw.Range
	if raw.SelectionRange != nil {
		selection = *raw.SelectionRange
	}
	children := make([]languages.DocumentSymbol, 0, len(raw.Children))
	for _, child := range raw.Children {
		children = append(children, projectDocumentSymbol(child))
	}
	return languages.DocumentSymbol{
		Name: raw.Name, Detail: raw.Detail, Kind: languages.SymbolKind(raw.Kind),
		StartLine: raw.Range.Start.Line, StartCharacter: raw.Range.Start.Character,
		EndLine: raw.Range.End.Line, EndCharacter: raw.Range.End.Character,
		SelectionLine: selection.Start.Line, SelectionCharacter: selection.Start.Character,
		SelectionEndLine: selection.End.Line, SelectionEndCharacter: selection.End.Character,
		SelectionRangeSet: true,
		Children:          children,
	}
}

func projectSymbolInformation(raw symbolInformationWire) languages.DocumentSymbol {
	rangeValue := raw.Location.Range
	return languages.DocumentSymbol{
		Name: raw.Name, Kind: languages.SymbolKind(raw.Kind),
		StartLine: rangeValue.Start.Line, StartCharacter: rangeValue.Start.Character,
		EndLine: rangeValue.End.Line, EndCharacter: rangeValue.End.Character,
		SelectionLine: rangeValue.Start.Line, SelectionCharacter: rangeValue.Start.Character,
		SelectionEndLine: rangeValue.End.Line, SelectionEndCharacter: rangeValue.End.Character,
		SelectionRangeSet: true,
	}
}

func (b *Backend) WorkspaceSymbols(ctx context.Context, req languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	result, err := b.conn.SendRequest(ctx, "workspace/symbol", map[string]interface{}{"query": req.Query})
	if err != nil || result == nil {
		return nil, err
	}
	var syms []languages.WorkspaceSymbol
	var raw []struct {
		Name     string `json:"name"`
		Kind     int    `json:"kind"`
		Location struct {
			URI   string `json:"uri"`
			Range struct {
				Start struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"start"`
			} `json:"range"`
		} `json:"location"`
	}
	if err := json.Unmarshal(result, &raw); err == nil {
		for _, s := range raw {
			syms = append(syms, languages.WorkspaceSymbol{
				Name: s.Name, Kind: languages.SymbolKind(s.Kind),
				URI:       s.Location.URI,
				StartLine: s.Location.Range.Start.Line, StartCol: s.Location.Range.Start.Character,
			})
		}
	}
	return syms, nil
}

func (b *Backend) Diagnostics(ctx context.Context, uri string, content []byte) ([]languages.Diagnostic, error) {
	return b.DiagnosticsWithEncoding(ctx, uri, content, 0, 1)
}

func (b *Backend) BeginWorkspaceSnapshot(ctx context.Context, snapshot languages.WorkspaceSnapshot) (context.Context, func() error, error) {
	documents := make([]nested.SnapshotDocument, 0, len(snapshot.Documents))
	for _, doc := range snapshot.Documents {
		switch doc.LanguageID {
		case "typescript", "javascript", "typescriptreact", "javascriptreact":
			// Preserve the actual TS/JS family language ID so tsserver selects
			// the matching configured project for each open document.
			documents = append(documents, nested.SnapshotDocument{URI: doc.URI, LangID: documentLanguageID(doc.URI), Content: doc.Content})
		}
	}
	return b.conn.BeginWorkspaceSnapshot(ctx, snapshot.Revision, documents)
}

func (b *Backend) WorkspaceSnapshotGeneration() uint64 {
	return b.conn.WorkspaceSnapshotGeneration()
}

// SetDiagnosticsUpdateHandler forwards current child diagnostic changes to
// the parent diagnostic cache coordinator.
func (b *Backend) SetDiagnosticsUpdateHandler(handler func(uri string)) {
	b.conn.SetDiagnosticsUpdateHandler(handler)
}

func (b *Backend) DiagnosticsWithEncoding(ctx context.Context, uri string, content []byte, snapshotRev uint64, encoding int) ([]languages.Diagnostic, error) {
	token, err := b.conn.RefreshDiagnosticsAtRevision(ctx, documentLanguageID(uri), uri, content, snapshotRev)
	if err != nil {
		return nil, err
	}
	upstream, err := b.conn.WaitForDiagnostics(ctx, token)
	if err != nil {
		return nil, err
	}
	converted, err := nested.ConvertDiagnosticPositions(content, upstream, encoding)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", serverName, err)
	}
	result := make([]languages.Diagnostic, 0, len(converted))
	for _, diagnostic := range converted {
		result = append(result, languages.Diagnostic{
			StartLine: diagnostic.StartLine, StartChar: diagnostic.StartChar,
			EndLine: diagnostic.EndLine, EndChar: diagnostic.EndChar,
			Severity: diagnostic.Severity, Code: diagnostic.Code, Source: diagnostic.Source, Message: diagnostic.Message,
		})
	}
	return result, nil
}

func (b *Backend) SemanticTokens(ctx context.Context, uri string, content []byte) ([]languages.SemanticToken, error) {
	return nil, nil // push-based semantic tokens not yet consumed; explicit empty (Q3)
}

func (b *Backend) Rename(ctx context.Context, req languages.RenameRequest) (envelope identity.SemanticResult[languages.ValidatedEdit], retErr error) {
	epoch := b.currentBackendEpoch()
	defer func() { envelope = languages.WithBackendEpoch(envelope, epoch) }()
	// Fail-closed gate (§I10): without a tsconfig the server works in
	// inferred-project mode with no project-wide file set, so rename
	// completeness is unprovable.
	if !b.cfgPresent() {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status: identity.ResultUnavailable,
			Evidence: evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content,
				"no-"+b.cfgFile),
			InternalDiagnostics: []string{
				"no " + b.cfgFile + " — project-wide rename cannot be proven complete",
			},
		}, nil
	}
	kind, refusal, err := b.classifyRenameTarget(ctx, req)
	if refusal != "" {
		message := "rename refused: TypeScript/JavaScript target classification is unavailable (SEM-SAFE-001)"
		if refusal == "rename-target-outside-document" {
			message = "rename refused: TypeScript/JavaScript target is outside the current document; collision analysis is not proven (SEM-SAFE-001)"
		}
		return unavailableTSRename(req, refusal, message), nil
	}
	if err != nil {
		if ierrors.IsKind(err, ierrors.ErrContentModified) {
			return unavailableTSRename(req, "rename-target-revision-changed", "rename refused: TypeScript/JavaScript target classification changed with the document revision (SEM-SAFE-001)"), err
		}
		return unavailableTSRename(req, "rename-target-classification-failed", "rename refused: TypeScript/JavaScript target classification is unavailable (SEM-SAFE-001)"), err
	}
	if tsRenameNeedsCollisionProof(kind) {
		return unavailableTSRename(req, "rename-function-collision-unproven", "rename refused: TypeScript/JavaScript function/method collision analysis is not proven (SEM-SAFE-001)"), nil
	}
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, documentLanguageID(req.URI), req.URI, req.Content, req.SnapshotRev, "textDocument/rename", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
		"newName":      req.NewName,
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		result := identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, serverName+" request failed"),
			InternalDiagnostics: []string{err.Error()},
		}
		return result, err
	}
	if len(bytes.TrimSpace(result)) == 0 || bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		// The server refuses renames it cannot prove; inherit the refusal.
		return unavailableTSRename(req, serverName+"-refused", "upstream language service refused rename"), nil
	}
	var edits []languages.TextEdit
	var workspaceEdit struct {
		Changes map[string][]struct {
			Range struct {
				Start struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"start"`
				End struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"end"`
			} `json:"range"`
			NewText string `json:"newText"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(result, &workspaceEdit); err != nil {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, "rename decode failed"),
			InternalDiagnostics: []string{err.Error()},
		}, err
	}
	for uri, changes := range workspaceEdit.Changes {
		for _, c := range changes {
			edits = append(edits, languages.TextEdit{
				URI:       uri,
				StartLine: c.Range.Start.Line, StartChar: c.Range.Start.Character,
				EndLine: c.Range.End.Line, EndChar: c.Range.End.Character,
				NewText: c.NewText,
			})
		}
	}
	if len(edits) == 0 {
		return unavailableTSRename(req, serverName+"-refused", "upstream language service refused rename"), nil
	}
	// G9 Phase 1: a non-null WorkspaceEdit carries the server's own proof.
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status: identity.ResultExact,
		Value: languages.ValidatedEdit{
			Edits:    edits,
			Complete: true,
		},
		Evidence:     evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, serverName+"-attested-complete"),
		Completeness: identity.Complete,
	}, nil
}

const tsFunctionLikeSymbolOperator languages.SymbolKind = 25 // LSP SymbolKind::Operator.

// classifyRenameTarget asks the same revision-bound language service to
// resolve the target and classify its declaration. Rename cannot proceed
// without one unambiguous declaration in the current document.
func (b *Backend) classifyRenameTarget(ctx context.Context, req languages.RenameRequest) (languages.SymbolKind, string, error) {
	definition, err := b.Definition(ctx, languages.DefinitionRequest{
		URI: req.URI, Content: req.Content, SnapshotRev: req.SnapshotRev,
		BuildContext: req.BuildContext, Line: req.Line, Column: req.Column,
		Encoding: req.Encoding, EncodingSet: req.EncodingSet,
	})
	if err != nil {
		return 0, "", err
	}
	if definition.Status != identity.ResultExact || definition.Completeness != identity.Complete || len(definition.Value) != 1 {
		return 0, "rename-target-unclassified", nil
	}
	target := definition.Value[0]
	if target.URI != req.URI {
		return 0, "rename-target-outside-document", nil
	}
	if !validTSRange(target.Range) {
		return 0, "rename-target-unclassified", nil
	}
	symbols, err := b.DocumentSymbols(ctx, languages.DocumentSymbolRequest{
		URI: req.URI, Content: req.Content, SnapshotRev: req.SnapshotRev,
		Encoding: req.Encoding, EncodingSet: req.EncodingSet,
	})
	if err != nil {
		return 0, "", err
	}
	kind, ok := classifyTSSymbolAtDefinition(symbols, target.Range)
	if !ok {
		return 0, "rename-target-unclassified", nil
	}
	return kind, "", nil
}

func unavailableTSRename(req languages.RenameRequest, detailCode, message string) identity.SemanticResult[languages.ValidatedEdit] {
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status:              identity.ResultUnavailable,
		Evidence:            evidenceForTs(req.SnapshotRev, req.BuildContext, req.Content, detailCode),
		InternalDiagnostics: []string{message},
		Completeness:        identity.CompletenessUnknown,
	}
}

func tsRenameNeedsCollisionProof(kind languages.SymbolKind) bool {
	return kind == languages.SymbolFunction || kind == languages.SymbolMethod ||
		kind == languages.SymbolConstructor || kind == tsFunctionLikeSymbolOperator
}

func classifyTSSymbolAtDefinition(symbols []languages.DocumentSymbol, target languages.Range) (languages.SymbolKind, bool) {
	var candidates []languages.DocumentSymbol
	var visit func([]languages.DocumentSymbol)
	visit = func(items []languages.DocumentSymbol) {
		for _, item := range items {
			selection := tsSymbolSelection(item)
			if item.SelectionRangeSet && validTSRange(selection) && tsRangeContains(selection, target) {
				candidates = append(candidates, item)
			}
			visit(item.Children)
		}
	}
	visit(symbols)
	if len(candidates) == 0 {
		return 0, false
	}
	var narrowest []languages.DocumentSymbol
	for i, candidate := range candidates {
		candidateRange := tsSymbolSelection(candidate)
		isNarrowest := true
		for j, other := range candidates {
			if i != j && tsRangeStrictlyContains(candidateRange, tsSymbolSelection(other)) {
				isNarrowest = false
				break
			}
		}
		if isNarrowest {
			narrowest = append(narrowest, candidate)
		}
	}
	if len(narrowest) == 0 {
		return 0, false
	}
	kind := narrowest[0].Kind
	if kind < languages.SymbolFile || kind > 26 {
		return 0, false
	}
	for _, candidate := range narrowest[1:] {
		if candidate.Kind != kind {
			return 0, false
		}
	}
	return kind, true
}

func tsSymbolSelection(symbol languages.DocumentSymbol) languages.Range {
	return languages.Range{
		StartLine: symbol.SelectionLine, StartCharacter: symbol.SelectionCharacter,
		EndLine: symbol.SelectionEndLine, EndCharacter: symbol.SelectionEndCharacter,
	}
}

func validTSRange(r languages.Range) bool {
	return tsPositionLess(r.StartLine, r.StartCharacter, r.EndLine, r.EndCharacter)
}

func tsRangeContains(outer, inner languages.Range) bool {
	return !tsPositionLess(inner.StartLine, inner.StartCharacter, outer.StartLine, outer.StartCharacter) &&
		!tsPositionLess(outer.EndLine, outer.EndCharacter, inner.EndLine, inner.EndCharacter)
}

func tsRangeStrictlyContains(outer, inner languages.Range) bool {
	return tsRangeContains(outer, inner) &&
		(outer.StartLine != inner.StartLine || outer.StartCharacter != inner.StartCharacter ||
			outer.EndLine != inner.EndLine || outer.EndCharacter != inner.EndCharacter)
}

func tsPositionLess(lineA, charA, lineB, charB uint32) bool {
	return lineA < lineB || (lineA == lineB && charA < charB)
}

func (b *Backend) didOpen(uri string, content []byte, revision ...uint64) error {
	var snapshotRevision uint64
	if len(revision) != 0 {
		snapshotRevision = revision[0]
	}
	_, err := b.conn.SyncDocumentAtRevision(documentLanguageID(uri), uri, content, snapshotRevision)
	return err
}

// DidCloseDocument is an optional lifecycle hook used by the runtime server
// when an editor closes a document. It does not change the frozen Backend
// method set.
func (b *Backend) DidCloseDocument(uri string, snapshotRevision uint64) error {
	return b.conn.CloseDocument(uri, snapshotRevision)
}

// cfgPresent reports whether the workspace tsconfig exists (§I10), cached:
// it appears at scaffold time, not mid-session.
func (b *Backend) cfgPresent() bool {
	b.gateOnce.Do(func() {
		_, err := os.Stat(filepath.Join(b.workDir, b.cfgFile))
		b.hasCfgFile = err == nil
	})
	return b.hasCfgFile
}

// evidenceForTs builds §B4 evidence attributing resolution to tsserver.
func evidenceForTs(rev uint64, bc identity.BuildContextID, content []byte, detail string) []identity.Evidence {
	sum := sha256.Sum256(content)
	return []identity.Evidence{{
		Kind:         identity.EvidenceCompiler,
		Assurance:    identity.AssuranceCompilerResolved,
		Snapshot:     identity.SnapshotID{Revision: identity.SnapshotRevision(rev)},
		BuildContext: bc,
		Backend:      identity.BackendID{Language: langID, Name: serverName},
		SourceHash:   identity.ContentHash(hex.EncodeToString(sum[:8])),
		DetailCode:   detail,
	}}
}

type lspLocationList []struct {
	URI   string `json:"uri"`
	Range struct {
		Start struct {
			Line      uint32 `json:"line"`
			Character uint32 `json:"character"`
		} `json:"start"`
		End struct {
			Line      uint32 `json:"line"`
			Character uint32 `json:"character"`
		} `json:"end"`
	} `json:"range"`
}

func toLocations(raw lspLocationList) []languages.Location {
	var locs []languages.Location
	for _, l := range raw {
		locs = append(locs, languages.Location{
			URI: l.URI,
			Range: languages.Range{
				StartLine: l.Range.Start.Line, StartCharacter: l.Range.Start.Character,
				EndLine: l.Range.End.Line, EndCharacter: l.Range.End.Character,
			},
		})
	}
	return locs
}

func (b *Backend) Close() error {
	return b.conn.Close()
}
