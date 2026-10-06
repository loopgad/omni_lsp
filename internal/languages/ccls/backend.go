// Invariants:
//  1. Rename/edit operations fail closed without a compile database (X3).
//  2. Ambiguous header contexts surface advisory diagnostics rather than guessing,
//     on the paths that carry diagnostics at all -- hover today. Definition and
//     References report macro suspects only; Rename fails closed on a missing
//     compile database and the two symbol queries return bare slices.
//  3. Macro-suspect identifiers are reported via macroSuspectDiag on the hover,
//     definition and references success paths.
//
// Package ccls bridges C/C++ semantics to a nested clangd language server.
//
// The core normalizes clangd output into canonical semantic types per goal.md
// §G1: a nested LSP server MAY be used internally, but its output MUST be
// normalized before publication.
package ccls

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

type Backend struct {
	conn    *nested.Conn
	workDir string

	// Compile database presence (§X3), resolved once.
	dbOnce       sync.Once
	hasCompileDb bool
}

// BuildContextID returns the digest-backed identity of this backend's build
// context (§E0/E7). Derived once by the shared nested bridge from
// `clangd --version`; on probe failure a stable "unavailable" ID is returned
// so evidence never fabricates a real context. Satisfies the server's
// buildContextProvider interface.
func (b *Backend) BuildContextID() identity.BuildContextID {
	return b.conn.BuildContextID()
}

func (b *Backend) SupervisorEpoch() uint64 {
	if b.conn == nil {
		return 0
	}
	return b.conn.SupervisorEpoch()
}

func (b *Backend) currentBackendEpoch() identity.BackendEpoch {
	return identity.BackendEpoch(b.SupervisorEpoch())
}

// parseClangdVersion extracts the semantic version token from
// `clangd --version` output ("clangd version 18.1.3\n...").
func parseClangdVersion(output string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	fields := strings.Fields(output)
	for i, f := range fields {
		if f == "version" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("clangd --version: unparseable output")
}

func probeClangdVersion() (string, error) {
	out, err := exec.Command("clangd", "--version").Output()
	return string(out), err
}

func New(workDir string) (*Backend, error) {
	if _, err := exec.LookPath("clangd"); err != nil {
		return nil, fmt.Errorf("clangd not found: %w", err)
	}
	b := &Backend{workDir: workDir}
	b.conn = nested.New(nested.Config{
		Name:         "clangd",
		Lang:         "cpp",
		WorkDir:      workDir,
		Start:        spawnClangd,
		VersionProbe: probeClangdVersion,
		ParseVersion: parseClangdVersion,
	})
	if err := b.conn.StartSupervised(); err != nil {
		return nil, err
	}
	return b, nil
}

// spawnClangd is the production process factory (G6: argument array, never
// a shell string). Process plumbing lives in the shared nested bridge.
func spawnClangd(c *nested.Conn) error {
	workDir := c.WorkDir()
	cmd := exec.Command("clangd", clangdArgs(workDir)...)
	cmd.Dir = workDir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start clangd: %w", err)
	}

	c.Attach(cmd, stdin, stdout)
	// Handshake must succeed before the supervisor marks this worker Ready;
	// a half-initialized clangd would otherwise surface as a 30s timeout on
	// the first real request instead of an immediate restart (fail fast).
	if err := c.Initialize(); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("clangd initialize: %w", err)
	}
	c.MarkReady()
	return nil
}

func (b *Backend) LanguageID() string       { return "cpp" }
func (b *Backend) FileExtensions() []string { return []string{".c", ".cpp", ".cc", ".h", ".hpp"} }

func (b *Backend) Completion(ctx context.Context, req languages.CompletionRequest) ([]languages.CompletionItem, error) {
	result, err := b.CompletionList(ctx, req)
	return result.Items, err
}

func (b *Backend) CompletionList(ctx context.Context, req languages.CompletionRequest) (languages.CompletionList, error) {
	result, err := b.conn.SendRequestAtRevision(ctx, "cpp", req.URI, req.Content, req.SnapshotRev, "textDocument/completion", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	}, req.ParentRequestID)
	if err != nil {
		return languages.CompletionList{}, err
	}
	return lspwire.DecodeCompletionList(result)
}

func (b *Backend) Hover(ctx context.Context, req languages.HoverRequest) (envelope identity.SemanticResult[*languages.HoverResult], retErr error) {
	epoch := b.currentBackendEpoch()
	defer func() { envelope = languages.WithBackendEpoch(envelope, epoch) }()
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, "cpp", req.URI, req.Content, req.SnapshotRev, "textDocument/hover", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		return unknownHover(req, "clangd request failed: "+err.Error()), err
	}
	if result == nil {
		return identity.SemanticResult[*languages.HoverResult]{
			Status:   identity.ResultExact,
			Value:    nil,
			Evidence: evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "no-hover"),
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
		Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd-resolved"),
		InternalDiagnostics: append(headerAmbiguityDiag(req.URI), macroSuspectDiag(req.Content, req.Line, req.Column)...),
		Completeness:        identity.Complete,
	}, nil
}

func unknownHover(req languages.HoverRequest, detail string) identity.SemanticResult[*languages.HoverResult] {
	return identity.SemanticResult[*languages.HoverResult]{
		Status:              identity.ResultUnknown,
		Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, detail),
		Completeness:        identity.CompletenessUnknown,
		InternalDiagnostics: []string{detail},
	}
}

// evidenceForCcls builds §B4 evidence attributing resolution to clangd.
// headerAmbiguityDiag implements the §X3 header-context policy: for header
// files clangd resolves symbols against whichever TU it picked (often the
// first includer) — that choice is not visible to us, so we surface the
// ambiguity instead of hiding it. Macro-heavy regions would need token-level
// mapping data from clangd to do better (upgrade path: AST dump bridge).
func headerAmbiguityDiag(uri string) []string {
	base := strings.ToLower(uri)
	if strings.HasSuffix(base, ".h") || strings.HasSuffix(base, ".hpp") ||
		strings.HasSuffix(base, ".hh") || strings.HasSuffix(base, ".hxx") {
		return []string{"header context ambiguous: symbol resolution depends on an unseen translation unit"}
	}
	return nil
}

// macroSuspectDiag implements the §H2.4 v1 heuristic: a query landing on an
// ALL_CAPS_SNAKE identifier is likely in macro territory (macro name or a
// macro-generated site). Read operations stay available; the uncertainty is
// surfaced instead of hidden. Rename needs no extra gate — the compile-db
// check already fail-closes it.
// ponytail: byte-column approximation of the UTF-16 column; ALL_CAPS idents
// are ASCII so misalignment only shifts the guess on exotic lines — worst
// case a missed/extra advisory, never a wrong result. Upgrade path: token-
// level mapping from clangd AST data.
func macroSuspectDiag(content []byte, line, col uint32) []string {
	if isMacroName(identAt(content, line, col)) {
		return []string{"possible-macro-expansion-site"}
	}
	return nil
}

func identAt(content []byte, line, col uint32) string {
	lines := strings.Split(string(content), "\n")
	if int(line) >= len(lines) {
		return ""
	}
	l := lines[line]
	i := int(col)
	if i >= len(l) {
		i = len(l) - 1
	}
	if i < 0 || !isWordByte(l[i]) {
		return ""
	}
	start, end := i, i
	for start > 0 && isWordByte(l[start-1]) {
		start--
	}
	for end < len(l)-1 && isWordByte(l[end+1]) {
		end++
	}
	return l[start : end+1]
}

func isWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isMacroName(s string) bool {
	if len(s) < 3 {
		return false
	}
	hasUpper := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '_', c >= '0' && c <= '9':
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		default:
			return false
		}
	}
	return hasUpper
}

func evidenceForCcls(rev uint64, bc identity.BuildContextID, content []byte, detail string) []identity.Evidence {
	sum := sha256.Sum256(content)
	return []identity.Evidence{{
		Kind:         identity.EvidenceCompiler,
		Assurance:    identity.AssuranceCompilerResolved,
		Snapshot:     identity.SnapshotID{Revision: identity.SnapshotRevision(rev)},
		BuildContext: bc,
		Backend:      identity.BackendID{Language: "cpp", Name: "clangd"},
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

// toReferenceLocations removes only exact duplicate references while
// preserving upstream order. Definition and other location projections keep
// using toLocations so their cardinality remains an upstream fact.
func toReferenceLocations(raw lspLocationList) []languages.Location {
	locs := make([]languages.Location, 0, len(raw))
	type referenceKey struct {
		URI   string
		Range languages.Range
	}
	seen := make(map[referenceKey]struct{}, len(raw))
	for _, l := range raw {
		location := languages.Location{
			URI: l.URI,
			Range: languages.Range{
				StartLine: l.Range.Start.Line, StartCharacter: l.Range.Start.Character,
				EndLine: l.Range.End.Line, EndCharacter: l.Range.End.Character,
			},
		}
		canonicalURI := location.URI
		if parsed, err := workspaceuri.Parse(location.URI); err == nil {
			canonicalURI = parsed.Canonical()
		}
		key := referenceKey{URI: canonicalURI, Range: location.Range}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		locs = append(locs, location)
	}
	return locs
}

func (b *Backend) Definition(ctx context.Context, req languages.DefinitionRequest) (envelope identity.SemanticResult[[]languages.Location], retErr error) {
	epoch := b.currentBackendEpoch()
	defer func() { envelope = languages.WithBackendEpoch(envelope, epoch) }()
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, "cpp", req.URI, req.Content, req.SnapshotRev, "textDocument/definition", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		return unknownLocsCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd request failed: "+err.Error()), err
	}
	if result == nil {
		return identity.SemanticResult[[]languages.Location]{
			Status:   identity.ResultExact,
			Value:    nil,
			Evidence: evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "no-definition"),
		}, nil
	}
	var raw lspLocationList
	if err := json.Unmarshal(result, &raw); err != nil {
		return unknownLocsCcls(req.SnapshotRev, req.BuildContext, req.Content, "definition decode failed"), err
	}
	return identity.SemanticResult[[]languages.Location]{
		Status:              identity.ResultExact,
		Value:               toLocations(raw),
		Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd-resolved"),
		InternalDiagnostics: append(headerAmbiguityDiag(req.URI), macroSuspectDiag(req.Content, req.Line, req.Column)...),
		Completeness:        identity.Complete,
	}, nil
}

func unknownLocsCcls(rev uint64, bc identity.BuildContextID, content []byte, detail string) identity.SemanticResult[[]languages.Location] {
	return identity.SemanticResult[[]languages.Location]{
		Status:              identity.ResultUnknown,
		Evidence:            evidenceForCcls(rev, bc, content, detail),
		Completeness:        identity.CompletenessUnknown,
		InternalDiagnostics: []string{detail},
	}
}

func (b *Backend) References(ctx context.Context, req languages.ReferencesRequest) (envelope identity.SemanticResult[[]languages.Location], retErr error) {
	epoch := b.currentBackendEpoch()
	defer func() { envelope = languages.WithBackendEpoch(envelope, epoch) }()
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, "cpp", req.URI, req.Content, req.SnapshotRev, "textDocument/references", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
		"context":      map[string]bool{"includeDeclaration": req.IncludeDecl},
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		return unknownLocsCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd request failed: "+err.Error()), err
	}
	if result == nil {
		return identity.SemanticResult[[]languages.Location]{
			Status:   identity.ResultExact,
			Value:    nil,
			Evidence: evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "no-references"),
		}, nil
	}
	var raw lspLocationList
	if err := json.Unmarshal(result, &raw); err != nil {
		return unknownLocsCcls(req.SnapshotRev, req.BuildContext, req.Content, "references decode failed"), err
	}
	// clangd enumerates across its loaded index; the bridge inherits that
	// proof but cannot independently verify scope (§G1 normalization).
	return identity.SemanticResult[[]languages.Location]{
		Status:              identity.ResultPartial,
		Value:               toReferenceLocations(raw),
		Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd-index-scope"),
		InternalDiagnostics: append(headerAmbiguityDiag(req.URI), macroSuspectDiag(req.Content, req.Line, req.Column)...),
		Completeness:        identity.IncompleteKnownSubset,
	}, nil
}

func (b *Backend) DocumentSymbols(ctx context.Context, req languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	result, err := b.conn.SendRequestAtRevision(ctx, "cpp", req.URI, req.Content, req.SnapshotRev, "textDocument/documentSymbol", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
	}, req.ParentRequestID)
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
		if doc.LanguageID == "c" || doc.LanguageID == "cpp" {
			// All clangd semantic and diagnostic operations address documents as
			// "cpp". Use that same ID for snapshot didOpen so the scoped lease
			// validates those later requests for both C and C++ files.
			documents = append(documents, nested.SnapshotDocument{URI: doc.URI, LangID: "cpp", Content: doc.Content})
		}
	}
	return b.conn.BeginWorkspaceSnapshot(ctx, snapshot.Revision, documents)
}

func (b *Backend) WorkspaceSnapshotGeneration() uint64 {
	return b.conn.WorkspaceSnapshotGeneration()
}

// DiagnosticsWithEncoding keeps the revision-aware document state in sync
// while C++ diagnostics remain explicitly deferred; clangd push diagnostics
// are not captured by this backend.
func (b *Backend) DiagnosticsWithEncoding(ctx context.Context, uri string, content []byte, snapshotRev uint64, encoding int) ([]languages.Diagnostic, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if _, err := b.conn.SyncDocumentAtRevisionContext(ctx, "cpp", uri, content, snapshotRev); err != nil {
		return nil, err
	}
	return nil, nil
}

// SemanticTokens reports no tokens. clangd pushes them through
// textDocument/publishSemanticTokens, which this bridge does not consume yet, so
// the answer is an explicit empty list rather than a guess. The other nested
// bridges are in the same position and say so at the same spot.
func (b *Backend) SemanticTokens(ctx context.Context, uri string, content []byte) ([]languages.SemanticToken, error) {
	return nil, nil // push-based semantic tokens not yet consumed; explicit empty (Q3)
}

func (b *Backend) Rename(ctx context.Context, req languages.RenameRequest) (envelope identity.SemanticResult[languages.ValidatedEdit], retErr error) {
	epoch := b.currentBackendEpoch()
	defer func() { envelope = languages.WithBackendEpoch(envelope, epoch) }()
	// X3: without a compile database clangd works in single-file mode —
	// it cannot see all translation units, so project-wide rename completeness
	// is unprovable. Fail closed with an actionable diagnostic.
	if !b.compileDbPresent() {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status: identity.ResultUnavailable,
			Evidence: evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content,
				"no-compile-commands"),
			InternalDiagnostics: []string{
				"no compile_commands.json at the workspace root or under build/ — project-wide rename cannot be proven complete; " +
					"generate one (cmake -DCMAKE_EXPORT_COMPILE_COMMANDS=ON) and retry",
			},
		}, nil
	}
	kind, refusal, err := b.classifyRenameTarget(ctx, req)
	if refusal != "" {
		message := "rename refused: C/C++ target classification is unavailable (SEM-SAFE-001)"
		if refusal == "rename-target-outside-document" {
			message = "rename refused: C/C++ target is outside the current document; collision analysis is not proven (SEM-SAFE-001)"
		} else if refusal == "rename-target-ambiguous" {
			message = "rename refused: C/C++ target is ambiguous; collision analysis is not proven (SEM-SAFE-001)"
		}
		return unavailableCclsRename(req, refusal, message), nil
	}
	if err != nil {
		if ierrors.IsKind(err, ierrors.ErrContentModified) {
			return unavailableCclsRename(req, "rename-target-revision-changed", "rename refused: C/C++ target classification changed with the document revision (SEM-SAFE-001)"), err
		}
		return unavailableCclsRename(req, "rename-target-classification-failed", "rename refused: C/C++ target classification is unavailable (SEM-SAFE-001)"), err
	}
	if cclsRenameNeedsCollisionProof(kind) {
		return unavailableCclsRename(req, "rename-function-collision-unproven", "rename refused: C/C++ function/method collision analysis is not proven (SEM-SAFE-001)"), nil
	}
	result, requestEpoch, err := b.conn.SendRequestAtRevisionWithEpoch(ctx, "cpp", req.URI, req.Content, req.SnapshotRev, "textDocument/rename", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
		"newName":      req.NewName,
	})
	epoch = identity.BackendEpoch(requestEpoch)
	if err != nil {
		result := identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd request failed"),
			InternalDiagnostics: []string{err.Error()},
		}
		return result, err
	}
	if len(strings.TrimSpace(string(result))) == 0 || strings.TrimSpace(string(result)) == "null" {
		// clangd itself refuses renames it cannot prove; inherit the refusal.
		return unavailableCclsRename(req, "clangd-refused", "upstream language service refused rename"), nil
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
			Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "rename decode failed"),
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
	// clangd's edit is accepted only after the target classification gate above;
	// function-like targets are refused because clangd does not prove overload-
	// set collision safety for this bridge.
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status: identity.ResultExact,
		Value: languages.ValidatedEdit{
			Edits:    edits,
			Complete: true,
		},
		Evidence:     evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd-attested-complete"),
		Completeness: identity.Complete,
	}, nil
}

const cclsFunctionLikeSymbolOperator languages.SymbolKind = 25 // LSP SymbolKind::Operator.

// classifyRenameTarget uses the same revision-bound child connection as the
// eventual edit request. Definition resolution must identify one in-document
// declaration, and document symbols must classify its selection range; without
// both facts the bridge cannot safely apply the non-function rename policy.
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
	if !validCclsRange(target.Range) {
		return 0, "rename-target-unclassified", nil
	}
	symbols, err := b.DocumentSymbols(ctx, languages.DocumentSymbolRequest{
		URI: req.URI, Content: req.Content, SnapshotRev: req.SnapshotRev,
		Encoding: req.Encoding, EncodingSet: req.EncodingSet,
	})
	if err != nil {
		return 0, "", err
	}
	kind, ok := classifyCclsSymbolAtDefinition(symbols, target.Range)
	if !ok {
		return 0, "rename-target-unclassified", nil
	}
	return kind, "", nil
}

func unavailableCclsRename(req languages.RenameRequest, detailCode, message string) identity.SemanticResult[languages.ValidatedEdit] {
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status:              identity.ResultUnavailable,
		Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, detailCode),
		InternalDiagnostics: []string{message},
		Completeness:        identity.CompletenessUnknown,
	}
}

func cclsRenameNeedsCollisionProof(kind languages.SymbolKind) bool {
	return kind == languages.SymbolFunction || kind == languages.SymbolMethod ||
		kind == languages.SymbolConstructor || kind == cclsFunctionLikeSymbolOperator
}

func classifyCclsSymbolAtDefinition(symbols []languages.DocumentSymbol, target languages.Range) (languages.SymbolKind, bool) {
	var candidates []languages.DocumentSymbol
	var visit func([]languages.DocumentSymbol)
	visit = func(items []languages.DocumentSymbol) {
		for _, item := range items {
			selection := languages.Range{
				StartLine: item.SelectionLine, StartCharacter: item.SelectionCharacter,
				EndLine: item.SelectionEndLine, EndCharacter: item.SelectionEndCharacter,
			}
			if item.SelectionRangeSet && validCclsRange(selection) && cclsRangeContains(selection, target) {
				candidates = append(candidates, item)
			}
			visit(item.Children)
		}
	}
	visit(symbols)
	if len(candidates) == 0 {
		return 0, false
	}
	// Prefer a unique narrowest selection range so a containing namespace or
	// type cannot mask the declaration at the target position.
	narrowest := make([]languages.DocumentSymbol, 0, len(candidates))
	for i, candidate := range candidates {
		candidateRange := cclsSymbolSelection(candidate)
		isNarrowest := true
		for j, other := range candidates {
			if i == j {
				continue
			}
			otherRange := cclsSymbolSelection(other)
			if cclsRangeStrictlyContains(candidateRange, otherRange) {
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
	if kind < languages.SymbolFile || kind > 26 { // LSP SymbolKind currently spans 1 through 26.
		return 0, false
	}
	for _, candidate := range narrowest[1:] {
		if candidate.Kind != kind {
			return 0, false
		}
	}
	return kind, true
}

func cclsSymbolSelection(symbol languages.DocumentSymbol) languages.Range {
	return languages.Range{
		StartLine: symbol.SelectionLine, StartCharacter: symbol.SelectionCharacter,
		EndLine: symbol.SelectionEndLine, EndCharacter: symbol.SelectionEndCharacter,
	}
}

func validCclsRange(r languages.Range) bool {
	return cclsPositionLess(r.StartLine, r.StartCharacter, r.EndLine, r.EndCharacter)
}

func cclsRangeContains(outer, inner languages.Range) bool {
	return !cclsPositionLess(inner.StartLine, inner.StartCharacter, outer.StartLine, outer.StartCharacter) &&
		!cclsPositionLess(outer.EndLine, outer.EndCharacter, inner.EndLine, inner.EndCharacter)
}

func cclsRangeStrictlyContains(outer, inner languages.Range) bool {
	return cclsRangeContains(outer, inner) &&
		(outer.StartLine != inner.StartLine || outer.StartCharacter != inner.StartCharacter ||
			outer.EndLine != inner.EndLine || outer.EndCharacter != inner.EndCharacter)
}

func cclsPositionLess(lineA, charA, lineB, charB uint32) bool {
	return lineA < lineB || (lineA == lineB && charA < charB)
}

func (b *Backend) didOpen(uri string, content []byte, revision ...uint64) error {
	var snapshotRevision uint64
	if len(revision) != 0 {
		snapshotRevision = revision[0]
	}
	_, err := b.conn.SyncDocumentAtRevision("cpp", uri, content, snapshotRevision)
	return err
}

// DidCloseDocument is an optional lifecycle hook used by the runtime server
// when an editor closes a document. It does not change the frozen Backend
// method set.
func (b *Backend) DidCloseDocument(uri string, snapshotRevision uint64) error {
	return b.conn.CloseDocument(uri, snapshotRevision)
}

// resolveCompileCommandsDir selects the directory clangd should use for this
// workspace (§X3). Prefer the root database used by the acceptance corpus and
// editor setup, while retaining compatibility with CMake's build/ layout.
func resolveCompileCommandsDir(workDir string) (string, bool) {
	if strings.TrimSpace(workDir) == "" {
		return "", false
	}
	for _, dir := range []string{workDir, filepath.Join(workDir, "build")} {
		info, err := os.Stat(filepath.Join(dir, "compile_commands.json"))
		if err == nil && !info.IsDir() {
			return dir, true
		}
	}
	return "", false
}

func clangdArgs(workDir string) []string {
	args := []string{"--log=error", "--pch-storage=memory"}
	if dir, ok := resolveCompileCommandsDir(workDir); ok {
		args = append(args, "--compile-commands-dir="+dir)
	}
	return args
}

// CommandArgs returns the production clangd arguments for a workspace. The
// acceptance oracle uses the same settings so its direct upstream comparison
// measures the same clangd configuration as the ccls backend.
func CommandArgs(workDir string) []string { return clangdArgs(workDir) }

// compileDbPresent reports whether the selected compile database exists for
// this workspace (§X3). Checked once per process and cached: the file appears
// at configure time, not mid-session. It shares path precedence with startup.
func (b *Backend) compileDbPresent() bool {
	b.dbOnce.Do(func() {
		_, b.hasCompileDb = resolveCompileCommandsDir(b.workDir)
	})
	return b.hasCompileDb
}

func (b *Backend) Close() error {
	return b.conn.Close()
}
