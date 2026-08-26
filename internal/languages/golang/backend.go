// Package golang provides an in-process Go language backend using go/packages
// with a go/parser fallback for environments where the full toolchain is unavailable.
//
// Responsibility:
//
//	Implements languages.Backend for Go — hover, definition, completion, references,
//	document/workspace symbols, diagnostics, semantic tokens, and rename.
//
// Owned mutable state:
//
//	pkgCache (map of package URI to *packages.Package), protected by mu.
//
// Concurrency model:
//
//	sync.Mutex for pkgCache; go/packages API is called sequentially per request.
//
// Invariants:
//  1. A3: Never fabricate semantic results — use go/parser fallback with EvidenceSyntax.
//  2. D2: URI/path conversion is bidirectional for file:// URIs.
//  3. B5: All results carry evidence with SourceHash and BackendEpoch.
package golang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/buildctx"
	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"golang.org/x/tools/go/packages"
)

type Backend struct {
	mu       sync.Mutex // serializes all access: fset and pkgCache are not concurrency-safe
	workDir  string
	pkgCache map[string]*packages.Package
	pkgOrder []string // FIFO eviction order for pkgCache
	fset     *token.FileSet

	// Build context identity (§E0): derived lazily once from `go env`.
	buildCtxOnce sync.Once
	buildCtxID   identity.BuildContextID

	// Cache observability (§K0): hits must be visible.
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
}

func New(workDir string) *Backend {
	return &Backend{
		workDir:  workDir,
		pkgCache: make(map[string]*packages.Package),
		fset:     token.NewFileSet(),
	}
}

// out-of-process worker. This in-process implementation is a fallback that
// serializes all access via a single mutex to ensure token.FileSet safety.

// BuildContextID returns the digest-backed identity of this backend's build
// context (§E0/§E7). Derived once from `go env`; on probe failure a stable
// "unavailable" ID is returned so evidence never fabricates a real context.
func (b *Backend) BuildContextID() identity.BuildContextID {
	b.buildCtxOnce.Do(b.deriveBuildContext)
	return b.buildCtxID
}

func (b *Backend) deriveBuildContext() {
	b.buildCtxID = "go:sha256:unavailable"
	out, err := exec.Command("go", "env", "GOVERSION", "GOOS", "GOARCH", "GOFLAGS", "GOWORK").Output()
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	get := func(i int) string {
		if i < len(lines) {
			return strings.TrimSpace(lines[i])
		}
		return ""
	}
	env := map[string]string{}
	if v := get(3); v != "" {
		env["GOFLAGS"] = v // §E7: vendor mode rides GOFLAGS
	}
	if v := get(4); v != "" {
		env["GOWORK"] = v
	}
	ctx, derr := buildctx.DeriveGo(b.workDir, get(0), get(1)+"/"+get(2), env, nil)
	if derr != nil {
		return
	}
	b.buildCtxID = ctx.ID()
}

func (b *Backend) LanguageID() string       { return "go" }
func (b *Backend) FileExtensions() []string { return []string{".go"} }

// CacheStats reports §K0 cache observability counters (hits, misses).
// Satisfies the server's optional cacheStatsProvider interface.
func (b *Backend) CacheStats() (int64, int64) {
	return b.cacheHits.Load(), b.cacheMisses.Load()
}

// pkgCacheLimit bounds the per-path package cache (§K0: caches MUST be
// bounded). Oldest entries are evicted FIFO via the order slice.
const pkgCacheLimit = 64

func (b *Backend) loadPackage(ctx context.Context, uri string, content []byte) (*packages.Package, error) {
	filePath := uriToPath(uri)

	// §K1: cache key includes every semantic input — file identity plus exact
	// content digest. Same hash ⇒ same fset offsets ⇒ safe reuse.
	sum := sha256.Sum256(content)
	key := filePath + "|" + hex.EncodeToString(sum[:8])
	if pkg, ok := b.pkgCache[key]; ok && pkg != nil {
		b.cacheHits.Add(1)
		return pkg, nil
	}
	b.cacheMisses.Add(1)

	dir := filepath.Dir(filePath)
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedExportFile |
			packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo |
			packages.NeedTypesSizes,
		Dir:   dir,
		Tests: false,
		Fset:  b.fset,
	}
	if content != nil && len(content) > 0 {
		cfg.Overlay = map[string][]byte{filePath: content}
	}
	pkgs, err := packages.Load(cfg, "file="+filePath)
	if err != nil {
		return nil, fmt.Errorf("packages.Load: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages found for %s", filePath)
	}
	b.pkgCache[key] = pkgs[0]
	b.pkgOrder = append(b.pkgOrder, key)
	if len(b.pkgOrder) > pkgCacheLimit { // FIFO eviction
		old := b.pkgOrder[0]
		b.pkgOrder = b.pkgOrder[1:]
		delete(b.pkgCache, old)
	}
	return pkgs[0], nil
}

// lspPosToTokenPos converts an LSP UTF-16 line/character position into a
// token.Pos within tf, whose content must be src (INV-POS-001).
// Invalid positions return a typed error, never panic (INV-POS-002).
func lspPosToTokenPos(src []byte, tf *token.File, line, character uint32) (token.Pos, error) {
	off, err := position.OffsetOfLineChar(src, line, character)
	if err != nil {
		return token.NoPos, errors.New(errors.ErrInvalidPosition, "position", err.Error())
	}
	return tf.Pos(int(off)), nil
}

// posToLineCol converts a token.Pos into an LSP UTF-16 line/character pair.
// src must be the exact content the fset offsets refer to.
func posToLineCol(src []byte, fset *token.FileSet, pos token.Pos) (uint32, uint32) {
	p := fset.Position(pos)
	line, col, err := position.LineCharAt(src, uint32(p.Offset))
	if err != nil {
		// Offset out of range should not happen for positions from this fset;
		// fall back to the line only rather than fabricate a column.
		return line, 0
	}
	return line, col
}

// evidenceFor builds a §B4 evidence record bound to the request's snapshot,
// build context, and content identity. lvl selects kind/assurance mapping.
func evidenceFor(b *Backend, rev uint64, bc identity.BuildContextID, content []byte, lvl languages.EvidenceLevel, detail string) []identity.Evidence {
	var kind identity.EvidenceKind
	var assurance identity.Assurance
	switch lvl {
	case languages.EvidenceL3:
		kind, assurance = identity.EvidenceCompiler, identity.AssuranceCompilerResolved
	case languages.EvidenceL2:
		kind, assurance = identity.EvidenceIndex, identity.AssuranceIndexedExact
	default:
		kind, assurance = identity.EvidenceSyntax, identity.AssuranceSyntax
	}
	if bc == "" {
		// E7: the caller (server) injects the context; a direct caller that
		// did not may still never fabricate one — fall back to the backend's
		// own derived identity so evidence is always grounded.
		bc = b.BuildContextID()
	}
	sum := sha256.Sum256(content)
	return []identity.Evidence{{
		Kind:         kind,
		Assurance:    assurance,
		Snapshot:     identity.SnapshotID{Revision: identity.SnapshotRevision(rev)},
		BuildContext: bc,
		Backend:      identity.BackendID{Language: "go", Name: "golang-native"},
		SourceHash:   identity.ContentHash(hex.EncodeToString(sum[:8])),
		DetailCode:   detail,
	}}
}

func findIdentAt(f *ast.File, fset *token.FileSet, pos token.Pos) *ast.Ident {
	var result *ast.Ident
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil || result != nil {
			return result == nil
		}
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if ident.Pos() <= pos && pos <= ident.End() {
			result = ident
			return false
		}
		return true
	})
	return result
}

func (b *Backend) findFileInPkg(pkg *packages.Package, filePath string) (*token.File, *ast.File) {
	for i, sf := range pkg.CompiledGoFiles {
		if sf == filePath && i < len(pkg.Syntax) {
			f := pkg.Syntax[i]
			tf := b.fset.File(f.Pos())
			if tf != nil {
				return tf, f
			}
		}
	}
	return nil, nil
}
func (b *Backend) Completion(ctx context.Context, req languages.CompletionRequest) ([]languages.CompletionItem, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.completionImpl(ctx, req)
}

func (b *Backend) completionImpl(ctx context.Context, req languages.CompletionRequest) ([]languages.CompletionItem, error) {
	pkg, err := b.loadPackage(ctx, req.URI, req.Content)
	if err != nil {
		return b.completionSyntax(req)
	}
	return b.completionWithTypes(ctx, pkg, req)
}

func (b *Backend) completionSyntax(req languages.CompletionRequest) ([]languages.CompletionItem, error) {
	filePath := uriToPath(req.URI)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, req.Content, parser.ParseComments)
	if err != nil {
		return nil, nil
	}
	seen := make(map[string]bool)
	var items []languages.CompletionItem
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if !seen[id.Name] {
				seen[id.Name] = true
				items = append(items, languages.CompletionItem{
					Label: id.Name, Kind: int(languages.CompletionVariable),
					Evidence: languages.EvidenceL1,
				})
			}
		}
		return true
	})
	return items, nil
}

func (b *Backend) completionWithTypes(ctx context.Context, pkg *packages.Package, req languages.CompletionRequest) ([]languages.CompletionItem, error) {
	filePath := uriToPath(req.URI)
	info := pkg.TypesInfo
	if info == nil {
		return b.completionSyntax(req)
	}
	tf, f := b.findFileInPkg(pkg, filePath)
	if tf == nil || f == nil {
		return b.completionSyntax(req)
	}
	pos, err := lspPosToTokenPos(req.Content, tf, req.Line, req.Column)
	if err != nil {
		// Latency-sensitive path (§I13): invalid position falls back to
		// syntax-level candidates rather than fabricating a typed scope.
		return b.completionSyntax(req)
	}
	var scope *types.Scope
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			return true
		}
		if n.Pos() <= pos && pos <= n.End() {
			if block, ok := n.(*ast.BlockStmt); ok {
				scope = info.Scopes[block]
			}
		}
		return true
	})
	if scope == nil {
		scope = pkg.Types.Scope()
	}
	seen := make(map[string]bool)
	var items []languages.CompletionItem
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if !seen[name] {
			seen[name] = true
			items = append(items, languages.CompletionItem{
				Label: name, Kind: objKindToCompletionKind(obj),
				Detail: obj.Type().String(), Evidence: languages.EvidenceL2,
			})
		}
	}
	for _, imp := range pkg.Imports {
		for _, name := range imp.Types.Scope().Names() {
			if !seen[name] && ast.IsExported(name) {
				seen[name] = true
				obj := imp.Types.Scope().Lookup(name)
				items = append(items, languages.CompletionItem{
					Label: name, Kind: objKindToCompletionKind(obj),
					Detail:        obj.Type().String(),
					Documentation: fmt.Sprintf("from package %s", imp.Name),
					Evidence:      languages.EvidenceL2,
				})
			}
		}
	}
	return items, nil
}
func (b *Backend) Hover(ctx context.Context, req languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	unknown := func(detail string) identity.SemanticResult[*languages.HoverResult] {
		return identity.SemanticResult[*languages.HoverResult]{
			Status:              identity.ResultUnknown,
			Evidence:            evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, languages.EvidenceL1, detail),
			Completeness:        identity.CompletenessUnknown,
			InternalDiagnostics: []string{detail},
		}
	}
	pkg, err := b.loadPackage(ctx, req.URI, req.Content)
	if err != nil {
		return unknown("package load failed: " + err.Error()), nil
	}
	filePath := uriToPath(req.URI)
	tf, f := b.findFileInPkg(pkg, filePath)
	if tf == nil || f == nil {
		return unknown("file not in compiled package"), nil
	}
	pos, perr := lspPosToTokenPos(req.Content, tf, req.Line, req.Column)
	if perr != nil {
		return unknown(perr.Error()), nil
	}
	ident := findIdentAt(f, b.fset, pos)
	if ident == nil {
		// Exact negative: the query resolved; there is genuinely no symbol here.
		return identity.SemanticResult[*languages.HoverResult]{
			Status:   identity.ResultExact,
			Value:    nil,
			Evidence: evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, languages.EvidenceL1, "no-symbol-at-position"),
		}, nil
	}
	info := pkg.TypesInfo
	if info == nil {
		return identity.SemanticResult[*languages.HoverResult]{
			Status: identity.ResultExact,
			Value: &languages.HoverResult{
				Contents: "```go\n" + ident.Name + "\n```",
				Evidence: languages.EvidenceL1,
			},
			Evidence:     evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, languages.EvidenceL1, "syntax-only"),
			Completeness: identity.IncompleteKnownSubset,
		}, nil
	}
	obj := info.ObjectOf(ident)
	if obj == nil {
		return identity.SemanticResult[*languages.HoverResult]{
			Status:   identity.ResultExact,
			Value:    nil,
			Evidence: evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, languages.EvidenceL1, "no-symbol-at-position"),
		}, nil
	}
	sl, sc := posToLineCol(req.Content, b.fset, ident.Pos())
	el, ec := posToLineCol(req.Content, b.fset, ident.End())
	var buf strings.Builder
	buf.WriteString("```go\n")
	switch o := obj.(type) {
	case *types.Func:
		sig := o.Type().(*types.Signature)
		buf.WriteString("func " + o.Name() + sig.String() + "\n")
	case *types.Var:
		buf.WriteString("var " + o.Name() + " " + o.Type().String() + "\n")
	case *types.Const:
		buf.WriteString("const " + o.Name() + " " + o.Type().String() + " = " + o.Val().String() + "\n")
	case *types.TypeName:
		buf.WriteString("type " + o.Name() + " " + o.Type().Underlying().String() + "\n")
	case *types.PkgName:
		buf.WriteString("package " + o.Imported().Path() + "\n")
	default:
		buf.WriteString(o.Name() + " " + o.Type().String() + "\n")
	}
	buf.WriteString("```")
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value: &languages.HoverResult{
			Contents: buf.String(),
			Range: &languages.Range{
				StartLine: sl, StartCharacter: sc,
				EndLine: el, EndCharacter: ec,
			},
			Evidence: languages.EvidenceL3,
		},
		Evidence:     evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, languages.EvidenceL3, "types-resolved"),
		Completeness: identity.Complete,
	}, nil
}

// unknownLocs builds an Unknown envelope for definition/references queries.
func unknownLocs(b *Backend, rev uint64, bc identity.BuildContextID, content []byte, detail string) identity.SemanticResult[[]languages.Location] {
	return identity.SemanticResult[[]languages.Location]{
		Status:              identity.ResultUnknown,
		Evidence:            evidenceFor(b, rev, bc, content, languages.EvidenceL1, detail),
		Completeness:        identity.CompletenessUnknown,
		InternalDiagnostics: []string{detail},
	}
}

func (b *Backend) Definition(ctx context.Context, req languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pkg, err := b.loadPackage(ctx, req.URI, req.Content)
	if err != nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "package load failed: "+err.Error()), nil
	}
	filePath := uriToPath(req.URI)
	tf, f := b.findFileInPkg(pkg, filePath)
	if tf == nil || f == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "file not in compiled package"), nil
	}
	tpos, perr := lspPosToTokenPos(req.Content, tf, req.Line, req.Column)
	if perr != nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, perr.Error()), nil
	}
	ident := findIdentAt(f, b.fset, tpos)
	if ident == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "no identifier at position"), nil
	}
	info := pkg.TypesInfo
	if info == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "type information unavailable"), nil
	}
	obj := info.ObjectOf(ident)
	if obj == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "symbol unresolved"), nil
	}
	defPos := obj.Pos()
	if defPos == token.NoPos {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "symbol has no source position"), nil
	}
	df := b.fset.File(defPos)
	if df == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "definition outside loaded file set"), nil
	}
	sl, sc := posToLineCol(req.Content, b.fset, defPos)
	el, ec := posToLineCol(req.Content, b.fset, obj.Pos()+token.Pos(len(obj.Name())))
	return identity.SemanticResult[[]languages.Location]{
		Status: identity.ResultExact,
		Value: []languages.Location{{
			URI: pathToUri(df.Name()),
			Range: languages.Range{
				StartLine: sl, StartCharacter: sc,
				EndLine: el, EndCharacter: ec,
			},
		}},
		Evidence:     evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, languages.EvidenceL3, "types-resolved"),
		Completeness: identity.Complete,
	}, nil
}

func (b *Backend) References(ctx context.Context, req languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pkg, err := b.loadPackage(ctx, req.URI, req.Content)
	if err != nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "package load failed: "+err.Error()), nil
	}
	filePath := uriToPath(req.URI)
	tf, f := b.findFileInPkg(pkg, filePath)
	if tf == nil || f == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "file not in compiled package"), nil
	}
	tpos, perr := lspPosToTokenPos(req.Content, tf, req.Line, req.Column)
	if perr != nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, perr.Error()), nil
	}
	ident := findIdentAt(f, b.fset, tpos)
	if ident == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "no identifier at position"), nil
	}
	info := pkg.TypesInfo
	if info == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "type information unavailable"), nil
	}
	obj := info.ObjectOf(ident)
	if obj == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "symbol unresolved"), nil
	}
	var refs []languages.Location
	if req.IncludeDecl {
		defPos := obj.Pos()
		if defPos != token.NoPos {
			df := b.fset.File(defPos)
			if df != nil {
				sl, sc := posToLineCol(req.Content, b.fset, defPos)
				el, ec := posToLineCol(req.Content, b.fset, obj.Pos()+token.Pos(len(obj.Name())))
				refs = append(refs, languages.Location{
					URI: pathToUri(df.Name()),
					Range: languages.Range{
						StartLine: sl, StartCharacter: sc,
						EndLine: el, EndCharacter: ec,
					},
				})
			}
		}
	}
	for _, syntax := range pkg.Syntax {
		ast.Inspect(syntax, func(n ast.Node) bool {
			if n == nil {
				return false
			}
			id, ok := n.(*ast.Ident)
			if !ok {
				return true
			}
			if info.ObjectOf(id) == obj {
				idf := b.fset.File(id.Pos())
				if idf != nil {
					sl, sc := posToLineCol(req.Content, b.fset, id.Pos())
					el, ec := posToLineCol(req.Content, b.fset, id.End())
					refs = append(refs, languages.Location{
						URI: pathToUri(idf.Name()),
						Range: languages.Range{
							StartLine: sl, StartCharacter: sc,
							EndLine: el, EndCharacter: ec,
						},
					})
				}
			}
			return true
		})
	}
	// §B6: references MAY be a conservative known subset — this enumeration
	// covers the loaded package graph, not necessarily every importer.
	// Exportedness rides along so S3 callers can apply Go scope rules:
	// references to an unexported identifier provably live inside this
	// package, which makes the subset complete FOR THAT SYMBOL CLASS.
	return identity.SemanticResult[[]languages.Location]{
		Status:   identity.ResultPartial,
		Value:    refs,
		Evidence: evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, languages.EvidenceL3, "types-resolved-package-scope"),
		InternalDiagnostics: []string{
			fmt.Sprintf("symbol=%s exported=%t", obj.Name(), ast.IsExported(obj.Name())),
		},
		Completeness: func() identity.Completeness {
			if len(pkg.Errors) == 0 {
				return identity.IncompleteKnownSubset
			}
			return identity.CompletenessUnknown
		}(),
	}, nil
}

func (b *Backend) DocumentSymbols(ctx context.Context, req languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	filePath := uriToPath(req.URI)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, req.Content, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	// UTF-16 column lookup over the parsed content (INV-POS-001).
	idx := position.NewIndex(req.Content, position.UTF16)
	lc := func(pos token.Pos) (line, col uint32) {
		p := fset.Position(pos)
		return uint32(p.Line - 1), idx.UTF16ColumnAt(uint32(p.Offset))
	}
	sym := func(name, detail string, kind languages.SymbolKind, pos, end, sel token.Pos) languages.DocumentSymbol {
		sl, sc := lc(pos)
		el, ec := lc(end)
		selL, selC := lc(sel)
		return languages.DocumentSymbol{
			Name: name, Detail: detail, Kind: kind,
			StartLine: sl, StartCharacter: sc,
			EndLine: el, EndCharacter: ec,
			SelectionLine: selL, SelectionCharacter: selC,
		}
	}
	var symbols []languages.DocumentSymbol
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			s := sym(d.Name.Name, "", languages.SymbolFunction, d.Pos(), d.End(), d.Name.Pos())
			if d.Recv != nil && len(d.Recv.List) > 0 {
				s.Kind = languages.SymbolMethod
				s.Detail = fmt.Sprintf("(%s).%s", exprToString(d.Recv.List[0].Type), d.Name.Name)
			} else {
				s.Detail = d.Name.Name
			}
			if d.Type.Params != nil {
				for _, field := range d.Type.Params.List {
					for _, name := range field.Names {
						c := sym(name.Name, exprToString(field.Type), languages.SymbolVariable, name.Pos(), name.End(), name.Pos())
						s.Children = append(s.Children, c)
					}
				}
			}
			symbols = append(symbols, s)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch sp := spec.(type) {
				case *ast.TypeSpec:
					var kind languages.SymbolKind
					switch sp.Type.(type) {
					case *ast.StructType:
						kind = languages.SymbolStruct
					case *ast.InterfaceType:
						kind = languages.SymbolInterface
					default:
						kind = languages.SymbolInterface
					}
					s := sym(sp.Name.Name, fmt.Sprintf("type %s", sp.Name.Name), kind, d.Pos(), d.End(), sp.Name.Pos())
					if st, ok := sp.Type.(*ast.StructType); ok && st.Fields != nil {
						for _, field := range st.Fields.List {
							for _, name := range field.Names {
								s.Children = append(s.Children, sym(name.Name, exprToString(field.Type), languages.SymbolField, name.Pos(), name.End(), name.Pos()))
							}
						}
					} else if it, ok := sp.Type.(*ast.InterfaceType); ok && it.Methods != nil {
						for _, method := range it.Methods.List {
							for _, name := range method.Names {
								s.Children = append(s.Children, sym(name.Name, exprToString(method.Type), languages.SymbolMethod, name.Pos(), name.End(), name.Pos()))
							}
						}
					}
					symbols = append(symbols, s)
				case *ast.ValueSpec:
					var kind languages.SymbolKind
					if d.Tok == token.CONST {
						kind = languages.SymbolConstant
					} else {
						kind = languages.SymbolVariable
					}
					for _, name := range sp.Names {
						symbols = append(symbols, sym(name.Name, exprToString(sp.Type), kind, name.Pos(), name.End(), name.Pos()))
					}
				}
			}
		}
	}
	return symbols, nil
}

func (b *Backend) WorkspaceSymbols(ctx context.Context, req languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedFiles,
		Dir:  b.workDir,
		Fset: b.fset,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(req.Query)
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	var results []languages.WorkspaceSymbol
	// Lazy per-file UTF-16 column indexes (INV-POS-001); files are small.
	fileIdx := make(map[string]*position.Index)
	for _, pkg := range pkgs {
		if pkg.Types == nil {
			continue
		}
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			if len(results) >= limit {
				return results, nil
			}
			if !strings.Contains(strings.ToLower(name), query) {
				continue
			}
			obj := scope.Lookup(name)
			pos := obj.Pos()
			if pos == token.NoPos {
				continue
			}
			p := b.fset.Position(pos)
			col := uint32(p.Column - 1) // fallback: byte column
			if idx, ok := fileIdx[p.Filename]; ok {
				col = idx.UTF16ColumnAt(uint32(p.Offset))
			} else if data, rerr := os.ReadFile(p.Filename); rerr == nil {
				ix := position.NewIndex(data, position.UTF16)
				fileIdx[p.Filename] = ix
				col = ix.UTF16ColumnAt(uint32(p.Offset))
			}
			results = append(results, languages.WorkspaceSymbol{
				Name:      name,
				Kind:      objKindToSymbolKind(obj),
				URI:       pathToUri(p.Filename),
				StartLine: uint32(p.Line - 1),
				StartCol:  col,
			})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results, nil
}

func (b *Backend) Diagnostics(ctx context.Context, uri string, content []byte) ([]languages.Diagnostic, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	pkg, err := b.loadPackage(ctx, uri, content)
	if err != nil {
		return nil, err
	}
	var diags []languages.Diagnostic
	for _, e := range pkg.Errors {
		p := e.Pos
		var sl, sc, el, ec uint32
		if p != "" {
			parts := strings.SplitN(p, ":", 3)
			if len(parts) >= 2 {
				var line int
				fmt.Sscanf(parts[1], "%d", &line)
				sl = uint32(line - 1)
				if len(parts) >= 3 {
					var col int
					fmt.Sscanf(parts[2], "%d", &col)
					sc = uint32(col - 1)
				}
			}
		}
		el = sl
		if sc > 0 {
			ec = sc + 1
		} else {
			ec = sc
		}
		diags = append(diags, languages.Diagnostic{
			StartLine: sl, StartChar: sc,
			EndLine: el, EndChar: ec,
			Severity: 1, Source: "omnilsp-go", Message: e.Msg,
		})
	}
	return diags, nil
}

func (b *Backend) SemanticTokens(ctx context.Context, uri string, content []byte) ([]languages.SemanticToken, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	filePath := uriToPath(uri)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, content, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var tokens []languages.SemanticToken
	var lastLine, lastCol uint32
	// UTF-16 columns/lengths over the parsed content (INV-POS-001).
	idx := position.NewIndex(content, position.UTF16)
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		var (
			p   token.Pos
			end token.Pos
			tt  uint32
		)
		switch v := n.(type) {
		case *ast.Ident:
			p = v.Pos()
			end = v.End()
			tt = languages.TokVariable
		case *ast.FuncDecl:
			if v.Name != nil {
				p = v.Name.Pos()
				end = v.Name.End()
				tt = languages.TokFunction
			}
		case *ast.TypeSpec:
			if v.Name != nil {
				p = v.Name.Pos()
				end = v.Name.End()
				tt = languages.TokType
			}
		case *ast.BasicLit:
			p = v.Pos()
			end = v.End()
			switch v.Kind {
			case token.STRING:
				tt = languages.TokString
			case token.INT, token.FLOAT, token.IMAG:
				tt = languages.TokNumber
			default:
				return true
			}
		case *ast.Comment:
			p = v.Pos()
			end = v.End()
			tt = languages.TokComment
		default:
			return true
		}
		if p == token.NoPos {
			return true
		}
		sp := fset.Position(p)
		ep := fset.Position(end)
		line := uint32(sp.Line - 1)
		col := idx.UTF16ColumnAt(uint32(sp.Offset))
		length := uint32(position.UTF16Len(content[sp.Offset:ep.Offset]))
		if length == 0 {
			return true
		}
		deltaLine := line - lastLine
		var deltaCol uint32
		if deltaLine == 0 {
			deltaCol = col - lastCol
		} else {
			deltaCol = col
		}
		lastLine = line
		lastCol = col
		tokens = append(tokens, languages.SemanticToken{
			DeltaLine: deltaLine, DeltaStart: deltaCol,
			Length: length, TokenType: tt,
		})
		return true
	})
	return tokens, nil
}

func (b *Backend) Rename(ctx context.Context, req languages.RenameRequest) (identity.SemanticResult[languages.ValidatedEdit], error) {
	// SEM-SAFE-001: rename is S3. It publishes only when the reference set is
	// provably complete; otherwise it fails closed with an explicit reason.
	ev := evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, languages.EvidenceL3, "rename")
	refResult, err := b.References(ctx, languages.ReferencesRequest{
		URI: req.URI, Content: req.Content,
		SnapshotRev:  req.SnapshotRev,
		BuildContext: req.BuildContext,
		Line:         req.Line,
		Column:       req.Column,
		Encoding:     req.Encoding,
		IncludeDecl:  true,
	})
	if err != nil {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            ev,
			InternalDiagnostics: []string{"reference enumeration failed: " + err.Error()},
		}, nil
	}
	if refResult.Status == identity.ResultUnknown {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            ev,
			InternalDiagnostics: append([]string{"cannot prove completeness"}, refResult.InternalDiagnostics...),
		}, nil
	}
	if refResult.Completeness != identity.IncompleteKnownSubset && refResult.Completeness != identity.Complete {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            ev,
			InternalDiagnostics: []string{"reference completeness unknown"},
		}, nil
	}
	// Go scope rule (§I11 collision/completeness analysis): references to an
	// unexported identifier cannot escape the declaring package, so a clean
	// package-scope enumeration is provably complete. Exported identifiers
	// may be referenced by unseen importers — refuse until module-wide
	// loading lands (G9 Phase 2).
	exported := false
	for _, d := range refResult.InternalDiagnostics {
		if strings.Contains(d, " exported=true") {
			exported = true
		}
	}
	if exported {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status:   identity.ResultUnavailable,
			Evidence: ev,
			InternalDiagnostics: []string{
				"rename refused: symbol is exported; importer packages are not loaded, " +
					"so reference completeness cannot be proven (SEM-SAFE-001)",
			},
		}, nil
	}
	var edits []languages.TextEdit
	for _, ref := range refResult.Value {
		edits = append(edits, languages.TextEdit{
			URI:       ref.URI,
			StartLine: ref.Range.StartLine,
			StartChar: ref.Range.StartCharacter,
			EndLine:   ref.Range.EndLine,
			EndChar:   ref.Range.EndCharacter,
			NewText:   req.NewName,
		})
	}
	// §I11 collision analysis: NewName already bound in an enclosing scope
	// would shadow or clash — fail closed with the decisive report.
	if collisions := b.detectRenameCollision(ctx, req); len(collisions) > 0 {
		return identity.SemanticResult[languages.ValidatedEdit]{
			Status:              identity.ResultUnavailable,
			Evidence:            ev,
			InternalDiagnostics: append([]string{"rename refused"}, collisions...),
		}, nil
	}
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status: identity.ResultExact,
		Value: languages.ValidatedEdit{
			Edits:    edits,
			Complete: true,
		},
		Evidence:     ev,
		Completeness: identity.Complete,
	}, nil
}

// CompletionIsIncomplete declares the go bridge's completion lists heuristic
// subsets (§I9): ranking is best-effort, so clients must re-query while typing.
func (b *Backend) CompletionIsIncomplete() bool { return true }

func (b *Backend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pkgCache = nil
	return nil
}

// uriToPath converts a document URI to an OS path via the canonical URI
// engine (goal.md §D2). Unparseable input is returned unchanged so the
// downstream failure names the offending value instead of a guess.
func uriToPath(s string) string {
	u, err := uri.Parse(s)
	if err != nil {
		return s
	}
	if p, perr := u.Path(); perr == nil {
		return p
	}
	return strings.TrimPrefix(u.Canonical(), "file://")
}

// pathToUri converts an OS path into a canonical file URI (§D2).
func pathToUri(path string) string {
	return uri.FromPath(path).Canonical()
}

func exprToString(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + exprToString(v.X)
	case *ast.SelectorExpr:
		return exprToString(v.X) + "." + v.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprToString(v.Elt)
	case *ast.FuncType:
		return "func(...)"
	case *ast.InterfaceType:
		return "interface{...}"
	case *ast.MapType:
		return fmt.Sprintf("map[%s]%s", exprToString(v.Key), exprToString(v.Value))
	case *ast.ChanType:
		return "chan " + exprToString(v.Value)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

func objKindToCompletionKind(obj types.Object) int {
	switch obj.(type) {
	case *types.Func:
		return int(languages.CompletionFunction)
	case *types.Var:
		return int(languages.CompletionVariable)
	case *types.Const:
		return int(languages.CompletionConstant)
	case *types.TypeName:
		return int(languages.CompletionClass)
	case *types.PkgName:
		return int(languages.CompletionModule)
	default:
		return int(languages.CompletionVariable)
	}
}

func objKindToSymbolKind(obj types.Object) languages.SymbolKind {
	switch obj.(type) {
	case *types.Func:
		return languages.SymbolFunction
	case *types.Var:
		return languages.SymbolVariable
	case *types.Const:
		return languages.SymbolConstant
	case *types.TypeName:
		return languages.SymbolInterface
	case *types.PkgName:
		return languages.SymbolPackage
	default:
		return languages.SymbolVariable
	}
}

// detectRenameCollision implements the §I11 collision half of rename
// validation: with the declaring package's type info, walk the renamed
// object's scope chain; if NewName is already bound to a different object in
// an enclosing (or file/package) scope, the rename would shadow or clash.
// Returns human-readable collision reasons; nil means no type info to check
// (callers already gate on provable completeness before consulting this).
func (b *Backend) detectRenameCollision(ctx context.Context, req languages.RenameRequest) []string {
	pkg, err := b.loadPackage(ctx, req.URI, req.Content)
	if err != nil || pkg == nil || pkg.TypesInfo == nil {
		return nil
	}
	tf, f := b.findFileInPkg(pkg, uriToPath(req.URI))
	if tf == nil || f == nil {
		return nil
	}
	pos, perr := lspPosToTokenPos(req.Content, tf, req.Line, req.Column)
	if perr != nil {
		return nil
	}
	ident := findIdentAt(f, b.fset, pos)
	if ident == nil {
		return nil
	}
	obj := pkg.TypesInfo.ObjectOf(ident)
	if obj == nil || obj.Parent() == nil {
		return nil
	}
	var conflicts []string
	for scope := obj.Parent(); scope != nil; scope = scope.Parent() {
		if existing := scope.Lookup(req.NewName); existing != nil && existing != obj {
			conflicts = append(conflicts, fmt.Sprintf(
				"collision: %q already names a %s (declared at %s); renaming would shadow or clash",
				req.NewName, objKindName(existing), b.fset.Position(existing.Pos())))
			break // innermost binding wins; one decisive report suffices
		}
	}
	return conflicts
}

// objKindName maps a types.Object to a readable kind label for collision
// messages.
func objKindName(o types.Object) string {
	switch o.(type) {
	case *types.Var:
		return "variable"
	case *types.Func:
		return "function"
	case *types.TypeName:
		return "type"
	case *types.Const:
		return "constant"
	case *types.PkgName:
		return "import"
	default:
		return "symbol"
	}
}
