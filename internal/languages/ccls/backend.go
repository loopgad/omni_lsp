// Invariants:
//  1. Rename/edit operations fail closed without a compile database (X3).
//  2. Ambiguous header contexts surface advisory diagnostics, never guesses.
//  3. Macro-suspect identifiers are reported on success paths via macroSuspectDiag.
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

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/nested"
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
	cmd := exec.Command("clangd",
		"--log=error",
		"--pch-storage=memory",
		"--compile-commands-dir="+filepath.Join(workDir, "build"),
	)
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
	b.didOpen(req.URI, req.Content)
	result, err := b.conn.SendRequest(ctx, "textDocument/completion", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	})
	if err != nil {
		return nil, err
	}
	var items []languages.CompletionItem
	var raw []struct {
		Label  string `json:"label"`
		Kind   int    `json:"kind"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(result, &raw); err == nil {
		for _, r := range raw {
			items = append(items, languages.CompletionItem{Label: r.Label, Kind: r.Kind, Detail: r.Detail})
		}
	}
	return items, nil
}

func (b *Backend) Hover(ctx context.Context, req languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.didOpen(req.URI, req.Content)
	result, err := b.conn.SendRequest(ctx, "textDocument/hover", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	})
	if err != nil {
		return unknownHover(req, "clangd request failed: "+err.Error()), nil
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
		return unknownHover(req, "hover decode failed"), nil
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

func (b *Backend) Definition(ctx context.Context, req languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.didOpen(req.URI, req.Content)
	result, err := b.conn.SendRequest(ctx, "textDocument/definition", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
	})
	if err != nil {
		return unknownLocsCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd request failed: "+err.Error()), nil
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
		return unknownLocsCcls(req.SnapshotRev, req.BuildContext, req.Content, "definition decode failed"), nil
	}
	return identity.SemanticResult[[]languages.Location]{
		Status:              identity.ResultExact,
		Value:               toLocations(raw),
		Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd-resolved"),
		InternalDiagnostics: macroSuspectDiag(req.Content, req.Line, req.Column),
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

func (b *Backend) References(ctx context.Context, req languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.didOpen(req.URI, req.Content)
	result, err := b.conn.SendRequest(ctx, "textDocument/references", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
		"context":      map[string]bool{"includeDeclaration": req.IncludeDecl},
	})
	if err != nil {
		return unknownLocsCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd request failed: "+err.Error()), nil
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
		return unknownLocsCcls(req.SnapshotRev, req.BuildContext, req.Content, "references decode failed"), nil
	}
	// clangd enumerates across its loaded index; the bridge inherits that
	// proof but cannot independently verify scope (§G1 normalization).
	return identity.SemanticResult[[]languages.Location]{
		Status:              identity.ResultPartial,
		Value:               toLocations(raw),
		Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd-index-scope"),
		InternalDiagnostics: macroSuspectDiag(req.Content, req.Line, req.Column),
		Completeness:        identity.IncompleteKnownSubset,
	}, nil
}

func (b *Backend) DocumentSymbols(ctx context.Context, req languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	b.didOpen(req.URI, req.Content)
	result, err := b.conn.SendRequest(ctx, "textDocument/documentSymbol", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
	})
	if err != nil || result == nil {
		return nil, err
	}
	var syms []languages.DocumentSymbol
	var raw []struct {
		Name  string `json:"name"`
		Kind  int    `json:"kind"`
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
	if err := json.Unmarshal(result, &raw); err == nil {
		for _, s := range raw {
			syms = append(syms, languages.DocumentSymbol{
				Name: s.Name, Kind: languages.SymbolKind(s.Kind),
				StartLine: s.Range.Start.Line, StartCharacter: s.Range.Start.Character,
				EndLine: s.Range.End.Line, EndCharacter: s.Range.End.Character,
			})
		}
	}
	return syms, nil
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
	b.didOpen(uri, content)
	return nil, nil // push diagnostics not yet consumed; explicit empty (Q3)
}

func (b *Backend) SemanticTokens(ctx context.Context, uri string, content []byte) ([]languages.SemanticToken, error) {
	return nil, nil
}

func (b *Backend) Rename(ctx context.Context, req languages.RenameRequest) (identity.SemanticResult[languages.ValidatedEdit], error) {
	// X3/R4.1: without a compile database clangd works in single-file mode —
	// it cannot see all translation units, so project-wide rename completeness
	// is unprovable. Fail closed with an actionable diagnostic.
	if !b.compileDbPresent() {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status: identity.ResultUnavailable,
			Evidence: evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content,
				"no-compile-commands"),
			InternalDiagnostics: []string{
				"no compile_commands.json under build/ — project-wide rename cannot be proven complete; " +
					"generate one (cmake -DCMAKE_EXPORT_COMPILE_COMMANDS=ON) and retry",
			},
		}, nil
	}
	b.didOpen(req.URI, req.Content)
	result, err := b.conn.SendRequest(ctx, "textDocument/rename", map[string]interface{}{
		"textDocument": map[string]string{"uri": req.URI},
		"position":     map[string]uint32{"line": req.Line, "character": req.Column},
		"newName":      req.NewName,
	})
	if err != nil {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd request failed"),
			InternalDiagnostics: []string{err.Error()},
		}, nil
	}
	if result == nil {
		// clangd itself refuses renames it cannot prove; inherit the refusal.
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "clangd-refused"),
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
			Evidence:            evidenceForCcls(req.SnapshotRev, req.BuildContext, req.Content, "rename decode failed"),
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
	// G9 Phase 1: clangd is the compiler-grade source of truth and refuses
	// unprovable renames itself; a non-null WorkspaceEdit carries that proof.
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

func (b *Backend) didOpen(uri string, content []byte) {
	b.conn.DidOpen("cpp", uri, content)
}

// compileDbPresent reports whether a compile database exists for this
// workspace (§X3). Checked once per process and cached: the file appears at
// configure time, not mid-session.
func (b *Backend) compileDbPresent() bool {
	b.dbOnce.Do(func() {
		_, err := os.Stat(filepath.Join(b.workDir, "build", "compile_commands.json"))
		b.hasCompileDb = err == nil
	})
	return b.hasCompileDb
}

func (b *Backend) Close() error {
	return b.conn.Close()
}
