// Invariants:
//  1. Rename fails closed without a Cargo.toml in the workspace root. The
//     check is a single stat on that one directory, not a walk up the tree,
//     so a crate in a subdirectory with its manifest above the workspace root
//     is refused.
//  2. All wire traffic flows through nested.Conn single-reader supervision.
//  3. Errors carry typed identity per internal/errors conventions.
//
// Package rustanalyzer bridges Rust semantics to a nested rust-analyzer
// language server through the shared stdio bridge (goal.md §G1/X4).
//
// Same normalization contract as ccls: raw LSP output never escapes without
// a canonical envelope and §B4 evidence attributing resolution upstream.
package rustanalyzer

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
	"sync/atomic"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/lspwire"
	"github.com/omnilsp/omni/internal/languages/nested"
)

// ErrToolchainMissing reports that rust-analyzer itself is absent so main
// can skip registering this language pack instead of failing startup.
var ErrToolchainMissing = errors.New("rust-analyzer not found")

const (
	serverName = "rust-analyzer"
	langID     = "rust"
)

type Backend struct {
	conn    *nested.Conn
	workDir string
	cfgFile string // Cargo.toml presence gates project-wide rename (§I10)

	gateOnce           sync.Once
	hasCfgFile         bool
	semanticReadyEpoch atomic.Uint64
}

// New wires the production rust-analyzer bridge. A missing binary is a
// sentinel (errors.Is-able), not a fatal misconfiguration.
func New(workDir string) (*Backend, error) {
	if _, err := exec.LookPath(serverName); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrToolchainMissing, err)
	}
	b := &Backend{workDir: workDir, cfgFile: "Cargo.toml"}
	b.conn = nested.New(nested.Config{
		Name:               serverName,
		Lang:               langID,
		WorkDir:            workDir,
		Start:              spawnServer,
		VersionProbe:       probeRustcVersion,
		ParseVersion:       parseRustcVersion,
		CaptureDiagnostics: true,
	})
	if err := b.conn.StartSupervised(); err != nil {
		return nil, err
	}
	return b, nil
}

// spawnServer is the production process factory (§G6: argument array).
func spawnServer(c *nested.Conn) error {
	cmd := exec.Command(serverName)
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

func probeRustcVersion() (string, error) {
	out, err := exec.Command("rustc", "--version").Output()
	return string(out), err
}

// parseRustcVersion extracts the version token from
// "rustc 1.75.0 (82e1608df 2023-12-21)".
func parseRustcVersion(output string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	for i, f := range fields {
		if f == "rustc" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", errors.New("rustc --version: unparseable output")
}

func (b *Backend) LanguageID() string { return langID }

// BackendStatusMessage projects the supervisor lifecycle state (§Q2).
func (b *Backend) BackendStatusMessage() string { return b.conn.SupervisorMessage() }
func (b *Backend) FileExtensions() []string     { return []string{".rs"} }

func (b *Backend) BuildContextID() identity.BuildContextID { return b.conn.BuildContextID() }
func (b *Backend) SupervisorEpoch() uint64                 { return b.conn.SupervisorEpoch() }

// SemanticReadiness reports whether this rust-analyzer supervisor epoch has
// returned a positive semantic result. The nested connection's Ready state
// still means only that the LSP initialize handshake completed.
func (b *Backend) SemanticReadiness() (identity.BackendEpoch, bool) {
	for {
		epoch := b.currentBackendEpoch()
		if epoch == 0 {
			return 0, false
		}
		observed := b.semanticReadyEpoch.Load()
		if observed != 0 && observed != uint64(epoch) {
			b.semanticReadyEpoch.CompareAndSwap(observed, 0)
			continue
		}
		if epoch != b.currentBackendEpoch() {
			continue
		}
		return epoch, observed == uint64(epoch)
	}
}

func (b *Backend) markSemanticReady(epoch identity.BackendEpoch) {
	if epoch == 0 || epoch != b.currentBackendEpoch() {
		return
	}
	value := uint64(epoch)
	b.semanticReadyEpoch.Store(value)
	if epoch != b.currentBackendEpoch() {
		b.semanticReadyEpoch.CompareAndSwap(value, 0)
	}
}

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
	result, err := b.conn.SendRequestAtRevision(ctx, langID, req.URI, req.Content, req.SnapshotRev, "textDocument/completion", map[string]interface{}{
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
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, langID, req.URI, req.Content, req.SnapshotRev, "textDocument/hover", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		return unknownHover(req, serverName+" request failed: "+err.Error()), err
	}
	if result == nil || bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		// rust-analyzer can return null while it is still loading a workspace.
		// We cannot distinguish that transient state from a stable negative
		// hover, so keep the result unknown and let the query engine retry it.
		return unknownHover(req, serverName+" returned null hover; readiness or a negative result is not proven"), nil
	}
	var hover struct {
		Contents struct {
			Value string `json:"value"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(result, &hover); err != nil {
		return unknownHover(req, "hover decode failed"), err
	}
	if strings.TrimSpace(hover.Contents.Value) == "" {
		return identity.SemanticResult[*languages.HoverResult]{
			Status:   identity.ResultExact,
			Value:    nil,
			Evidence: evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content, "no-hover"),
		}, nil
	}
	b.markSemanticReady(epoch)
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value: &languages.HoverResult{
			Contents: hover.Contents.Value,
			Evidence: languages.EvidenceL3,
		},
		Evidence:     evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content, serverName+"-resolved"),
		Completeness: identity.Complete,
	}, nil
}

func unknownHover(req languages.HoverRequest, detail string) identity.SemanticResult[*languages.HoverResult] {
	return identity.SemanticResult[*languages.HoverResult]{
		Status:              identity.ResultUnknown,
		Evidence:            evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content, detail),
		Completeness:        identity.CompletenessUnknown,
		InternalDiagnostics: []string{detail},
	}
}

func (b *Backend) Definition(ctx context.Context, req languages.DefinitionRequest) (envelope identity.SemanticResult[[]languages.Location], retErr error) {
	epoch := b.currentBackendEpoch()
	defer func() { envelope = languages.WithBackendEpoch(envelope, epoch) }()
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, langID, req.URI, req.Content, req.SnapshotRev, "textDocument/definition", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content, serverName+" request failed: "+err.Error()), err
	}
	if emptyLocationsResult(result) {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content,
			serverName+" returned no definition locations; readiness or a negative result is not proven"), nil
	}
	var raw lspLocationList
	if err := json.Unmarshal(result, &raw); err != nil {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content, "definition decode failed"), err
	}
	locations := toLocations(raw)
	if len(locations) == 0 {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content,
			serverName+" returned no definition locations; readiness or a negative result is not proven"), nil
	}
	b.markSemanticReady(epoch)
	return identity.SemanticResult[[]languages.Location]{
		Status:       identity.ResultExact,
		Value:        locations,
		Evidence:     evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content, serverName+"-resolved"),
		Completeness: identity.Complete,
	}, nil
}

func unknownLocs(rev uint64, bc identity.BuildContextID, content []byte, detail string) identity.SemanticResult[[]languages.Location] {
	return identity.SemanticResult[[]languages.Location]{
		Status:              identity.ResultUnknown,
		Evidence:            evidenceForRA(rev, bc, content, detail),
		Completeness:        identity.CompletenessUnknown,
		InternalDiagnostics: []string{detail},
	}
}

func emptyLocationsResult(result []byte) bool {
	trimmed := bytes.TrimSpace(result)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func (b *Backend) References(ctx context.Context, req languages.ReferencesRequest) (envelope identity.SemanticResult[[]languages.Location], retErr error) {
	epoch := b.currentBackendEpoch()
	defer func() { envelope = languages.WithBackendEpoch(envelope, epoch) }()
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, langID, req.URI, req.Content, req.SnapshotRev, "textDocument/references", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
		"context":      map[string]bool{"includeDeclaration": req.IncludeDecl},
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content, serverName+" request failed: "+err.Error()), err
	}
	if emptyLocationsResult(result) {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content,
			serverName+" returned no reference locations; readiness or a negative result is not proven"), nil
	}
	var raw lspLocationList
	if err := json.Unmarshal(result, &raw); err != nil {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content, "references decode failed"), err
	}
	locations := toLocations(raw)
	if len(locations) == 0 {
		return unknownLocs(req.SnapshotRev, req.BuildContext, req.Content,
			serverName+" returned no reference locations; readiness or a negative result is not proven"), nil
	}
	b.markSemanticReady(epoch)
	// The server enumerates over its loaded index; the bridge inherits that
	// proof but cannot independently verify scope (§G1 normalization).
	return identity.SemanticResult[[]languages.Location]{
		Status:       identity.ResultPartial,
		Value:        locations,
		Evidence:     evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content, serverName+"-index-scope"),
		Completeness: identity.IncompleteKnownSubset,
	}, nil
}

func (b *Backend) DocumentSymbols(ctx context.Context, req languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	result, err := b.conn.SendRequestAtRevision(ctx, langID, req.URI, req.Content, req.SnapshotRev, "textDocument/documentSymbol", map[string]interface{}{
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
		if doc.LanguageID == "rust" {
			documents = append(documents, nested.SnapshotDocument{URI: doc.URI, LangID: doc.LanguageID, Content: doc.Content})
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
	token, err := b.conn.RefreshDiagnosticsAtRevision(ctx, langID, uri, content, snapshotRev)
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
	// Fail-closed gate (§I10): without a workspace-level build manifest
	// rust-analyzer indexes an incomplete crate graph, so project-wide rename
	// completeness is unprovable.
	if !b.cfgPresent() {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status: identity.ResultUnavailable,
			Evidence: evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content,
				"no-"+b.cfgFile),
			InternalDiagnostics: []string{
				"no " + b.cfgFile + " — project-wide rename cannot be proven complete",
			},
		}, nil
	}
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, langID, req.URI, req.Content, req.SnapshotRev, "textDocument/rename", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
		"newName":      req.NewName,
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		result := identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content, serverName+" request failed"),
			InternalDiagnostics: []string{err.Error()},
		}
		if ierrors.IsKind(err, ierrors.ErrContentModified) {
			return result, err
		}
		return result, nil
	}
	if result == nil || bytes.Equal(bytes.TrimSpace(result), []byte("null")) {
		// rust-analyzer refuses renames it cannot prove; inherit the refusal.
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content, serverName+"-refused"),
			InternalDiagnostics: []string{"upstream language service refused rename"},
		}, nil
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
			Evidence:            evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content, "rename decode failed"),
			InternalDiagnostics: []string{err.Error()},
		}, nil
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
		// An empty WorkspaceEdit is not proof that the requested rename was
		// safely applied. rust-analyzer uses it for rejected/conflicting edits.
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content, serverName+"-refused"),
			InternalDiagnostics: []string{"upstream language service refused rename"},
		}, nil
	}
	b.markSemanticReady(epoch)
	// G9 Phase 1: a non-null WorkspaceEdit carries the server's own proof.
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status: identity.ResultExact,
		Value: languages.ValidatedEdit{
			Edits:    edits,
			Complete: true,
		},
		Evidence:     evidenceForRA(req.SnapshotRev, req.BuildContext, req.Content, serverName+"-attested-complete"),
		Completeness: identity.Complete,
	}, nil
}

func (b *Backend) didOpen(uri string, content []byte, revision ...uint64) error {
	var snapshotRevision uint64
	if len(revision) != 0 {
		snapshotRevision = revision[0]
	}
	_, err := b.conn.SyncDocumentAtRevision(langID, uri, content, snapshotRevision)
	return err
}

// DidCloseDocument is an optional lifecycle hook used by the runtime server
// when an editor closes a document. It does not change the frozen Backend
// method set.
func (b *Backend) DidCloseDocument(uri string, snapshotRevision uint64) error {
	return b.conn.CloseDocument(uri, snapshotRevision)
}

// DidSaveDocument forwards editor saves to rust-analyzer after synchronizing
// the saved source revision. It does not change the frozen Backend interface.
func (b *Backend) DidSaveDocument(uri string, content []byte, snapshotRevision uint64) error {
	return b.conn.DidSaveDocumentAtRevision(langID, uri, content, snapshotRevision)
}

// cfgPresent reports whether the workspace build manifest exists (§I10),
// cached: the manifest appears at scaffold time, not mid-session.
func (b *Backend) cfgPresent() bool {
	b.gateOnce.Do(func() {
		_, err := os.Stat(filepath.Join(b.workDir, b.cfgFile))
		b.hasCfgFile = err == nil
	})
	return b.hasCfgFile
}

// evidenceForRA builds §B4 evidence attributing resolution to rust-analyzer.
func evidenceForRA(rev uint64, bc identity.BuildContextID, content []byte, detail string) []identity.Evidence {
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
