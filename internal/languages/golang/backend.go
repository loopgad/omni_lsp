// Package golang provides an in-process Go language backend using go/packages
// with a go/parser fallback for environments where the full toolchain is unavailable.
//
// Responsibility:
//
// Implements languages.Backend for Go — hover, definition, completion, references,
//
//	document/workspace symbols, diagnostics, semantic tokens, and rename.
//
// Owned mutable state:
//
//	pkgCache and its FileSet generation, protected by gate. Published package
//
// ASTs are immutable; reference enumeration captures one generation and walks
// it after releasing gate so interactive requests can proceed concurrently.
//
// Concurrency model:
//
//	A cancellable gate serializes cache/package loading and FileSet rotation.
//	Read-only traversal uses a captured package/FileSet generation outside the
//	gate; go/packages is still called sequentially per request.
//
// Invariants:
//  1. A3: Never fabricate semantic results — use go/parser fallback with EvidenceSyntax.
//  2. D2: URI/path conversion is bidirectional for file:// URIs.
//  3. B5: All results carry evidence with SourceHash and BackendEpoch.
package golang

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/buildctx"
	"github.com/omnilsp/omni/internal/workspace/fileio"
	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"golang.org/x/tools/go/packages"
)

type Backend struct {
	gate       chan struct{} // cancellable serialization for fset and pkgCache access
	workDir    string
	pkgCache   map[string]packageCacheEntry
	pkgOrder   []string // FIFO eviction order for pkgCache
	fset       *token.FileSet
	fsetBase   int // token.FileSet position at the beginning of this cache generation
	goFlags    string
	goWorkPath string

	// Build context identity (§E0): derived lazily once from `go env`.
	buildCtxOnce sync.Once
	buildCtxID   identity.BuildContextID

	// Cache observability (§K0): hits must be visible.
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
}

func New(workDir string) *Backend {
	b := &Backend{
		gate:     make(chan struct{}, 1),
		workDir:  workDir,
		pkgCache: make(map[string]packageCacheEntry),
		fset:     token.NewFileSet(),
	}
	b.fsetBase = b.fset.Base()
	b.gate <- struct{}{}
	return b
}

func (b *Backend) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-b.gate:
	}
	if err := ctx.Err(); err != nil {
		b.unlock()
		return err
	}
	return nil
}

func (b *Backend) lockUncancellable() { <-b.gate }
func (b *Backend) unlock()            { b.gate <- struct{}{} }

// resetPackageState drops every cached AST before replacing its FileSet.
// FileSet has no file-removal API, so keeping it while evicting package keys
// would retain every document revision indefinitely.
func (b *Backend) resetPackageState() {
	b.pkgCache = make(map[string]packageCacheEntry)
	b.pkgOrder = nil
	b.fset = token.NewFileSet()
	b.fsetBase = b.fset.Base()
}

const maxPackageFsetBytes = 64 << 20

type referenceWalkCanceled struct{ err error }

// rotateFileSetIfNeeded bounds retained token position tables across document
// revisions. One package load may exceed the budget; the next cold load drops
// that generation before adding more files.
func (b *Backend) rotateFileSetIfNeeded() {
	if b.fset != nil && b.fset.Base()-b.fsetBase >= maxPackageFsetBytes {
		b.resetPackageState()
	}
}

// out-of-process worker. This in-process implementation uses a cancellable
// gate for mutable package/cache state; token.FileSet methods synchronize
// their own reads and writes.

// BuildContextID returns the digest-backed identity of this backend's build
// context (§E0/§E7). Derived once from `go env`; on probe failure a stable
// "unavailable" ID is returned so evidence never fabricates a real context.
func (b *Backend) BuildContextID() identity.BuildContextID {
	b.buildCtxOnce.Do(b.deriveBuildContext)
	return b.buildCtxID
}

func (b *Backend) deriveBuildContext() {
	b.buildCtxID = "go:sha256:unavailable"
	command := exec.Command("go", "env", "GOVERSION", "GOOS", "GOARCH", "GOFLAGS", "GOWORK")
	command.Dir = b.workDir
	out, err := command.Output()
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
		b.goFlags = v
		env["GOFLAGS"] = v // §E7: vendor mode rides GOFLAGS
	}
	if v := get(4); v != "" {
		b.goWorkPath = v
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

func (b *Backend) loadPackage(ctx context.Context, uri string, content []byte, snapshotRev uint64) (*packages.Package, error) {
	return b.loadPackageWithOverlays(ctx, uri, content, snapshotRev, packageOverlaysFromContext(ctx))
}

func (b *Backend) loadPackageWithOverlays(ctx context.Context, uri string, content []byte, snapshotRev uint64, overlays packageOverlays) (*packages.Package, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	filePath := uriToPath(uri)
	if filePath == "" {
		return nil, fmt.Errorf("invalid Go file URI %q", uri)
	}
	filePath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, err
	}
	filePath = filepath.Clean(filePath)
	if content != nil {
		overlays, err = overlays.withFile(filePath, content)
		if err != nil {
			return nil, err
		}
	}
	buildContext := b.BuildContextID()
	key := packageCacheKey(filePath, snapshotRev, content, overlays, buildContext)
	if entry, ok := b.pkgCache[key]; ok && entry.pkg != nil {
		fingerprint, complete, checkErr := packageInputFingerprint(ctx, entry.pkg, filePath, overlays, buildContext, b.goWorkPath, b.goFlags)
		if checkErr == nil && complete && fingerprint == entry.inputFingerprint {
			b.cacheHits.Add(1)
			return entry.pkg, nil
		}
		delete(b.pkgCache, key)
		b.removePackageCacheOrderKey(key)
	}
	b.cacheMisses.Add(1)
	b.rotateFileSetIfNeeded()
	if b.pkgCache == nil {
		b.pkgCache = make(map[string]packageCacheEntry)
	}

	dir := filepath.Dir(filePath)
	cfg := &packages.Config{
		Context: ctx,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedEmbedFiles | packages.NeedModule |
			packages.NeedImports | packages.NeedDeps | packages.NeedExportFile |
			packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo |
			packages.NeedTypesSizes,
		Dir:       dir,
		Tests:     false,
		Fset:      b.fset,
		ParseFile: parsePackageSource,
	}
	cfg.Overlay = overlays.goPackagesOverlay()
	pkgs, err := packages.Load(cfg, "file="+filePath)
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return nil, canceled
		}
		return nil, fmt.Errorf("packages.Load: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages found for %s", filePath)
	}
	fingerprint, complete, fingerprintErr := packageInputFingerprint(ctx, pkgs[0], filePath, overlays, buildContext, b.goWorkPath, b.goFlags)
	if fingerprintErr == nil && complete {
		b.cachePackage(key, pkgs[0], fingerprint)
	}
	return pkgs[0], nil
}

// cachePackage inserts one entry and evicts FIFO past pkgCacheLimit.
// §K0 requires bounded caches; the eviction lives here so the bound is
// reachable without driving 65 real packages.Load calls.
func (b *Backend) cachePackage(key string, pkg *packages.Package, fingerprint identity.ContentHash) {
	if b.pkgCache == nil {
		b.pkgCache = make(map[string]packageCacheEntry)
	}
	b.pkgCache[key] = packageCacheEntry{pkg: pkg, inputFingerprint: fingerprint}
	b.pkgOrder = append(b.pkgOrder, key)
	if len(b.pkgOrder) > pkgCacheLimit {
		old := b.pkgOrder[0]
		b.pkgOrder = b.pkgOrder[1:]
		delete(b.pkgCache, old)
	}
}

func (b *Backend) removePackageCacheOrderKey(key string) {
	kept := b.pkgOrder[:0]
	for _, existing := range b.pkgOrder {
		if existing != key {
			kept = append(kept, existing)
		}
	}
	b.pkgOrder = kept
}

// lspPosToTokenPos converts an LSP negotiated line/character position into a
// token.Pos within tf, whose content must be src (INV-POS-001).
// Invalid positions return a typed error, never panic (INV-POS-002).
func lspPosToTokenPos(src []byte, tf *token.File, line, character uint32, encoding int, encodingSet bool) (token.Pos, error) {
	off, err := position.OffsetOfLineCharEncoding(src, line, character, requestEncoding(encoding, encodingSet))
	if err != nil {
		return token.NoPos, errors.New(errors.ErrInvalidPosition, "position", err.Error())
	}
	return tf.Pos(int(off)), nil
}

func requestEncoding(value int, set bool) position.Encoding {
	if !set {
		return position.UTF16
	}
	switch value {
	case 0:
		return position.UTF8
	case 2:
		return position.UTF32
	default:
		return position.UTF16
	}
}

// posToLineCol converts a token.Pos into an LSP UTF-16 line/character pair.
// src must be the exact content the fset offsets refer to.
func posToLineCol(src []byte, fset *token.FileSet, pos token.Pos, encoding int, encodingSet bool) (uint32, uint32) {
	return posToLineColIndexed(src, position.NewIndex(src, requestEncoding(encoding, encodingSet)), fset, pos)
}

func posToLineColIndexed(src []byte, idx *position.Index, fset *token.FileSet, pos token.Pos) (uint32, uint32) {
	p := fset.Position(pos)
	if p.Offset < 0 || p.Offset > len(src) {
		// Offset out of range should not happen for positions from this fset;
		// fall back to the line only rather than fabricate a column.
		return uint32(maxInt(p.Line-1, 0)), 0
	}
	posn, err := idx.OffsetToPosition(src, uint32(p.Offset))
	if err != nil {
		return uint32(maxInt(p.Line-1, 0)), 0
	}
	return posn.Line, posn.Col
}

func maxInt(value, min int) int {
	if value < min {
		return min
	}
	return value
}

func sourceForFile(requestContent []byte, requestPath, targetPath string, overlays packageOverlays) []byte {
	if filepath.Clean(requestPath) == filepath.Clean(targetPath) {
		return requestContent
	}
	if file, ok := overlays.byPath[pathKey(targetPath)]; ok {
		return file.content
	}
	content, err := fileio.ReadFileShared(targetPath)
	if err != nil {
		return nil
	}
	return content
}

// parsePackageSource matches go/packages' default parser options while using
// the shared-read helper if the loader requests a file without source bytes.
func parsePackageSource(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
	if src == nil {
		var err error
		src, err = fileio.ReadFileShared(filename)
		if err != nil {
			return nil, err
		}
	}
	return parser.ParseFile(fset, filename, src, parser.AllErrors|parser.ParseComments)
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

func packageConfidence(pkg *packages.Package) (identity.ResultStatus, languages.EvidenceLevel, identity.Completeness, string) {
	if len(pkg.Errors) > 0 {
		return identity.ResultPartial, languages.EvidenceL2, identity.IncompleteKnownSubset, "types-resolved-with-package-errors"
	}
	return identity.ResultExact, languages.EvidenceL3, identity.Complete, "types-resolved"
}

func findIdentAt(f *ast.File, fset *token.FileSet, pos token.Pos) *ast.Ident {
	var result *ast.Ident
	ast.Inspect(f, func(n ast.Node) bool {
		if n == nil || result != nil {
			return result == nil
		}
		if pos < n.Pos() || pos > n.End() {
			return false
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
	if err := b.lock(ctx); err != nil {
		return nil, err
	}
	defer b.unlock()
	return b.completionImpl(ctx, req)
}

func (b *Backend) completionImpl(ctx context.Context, req languages.CompletionRequest) ([]languages.CompletionItem, error) {
	pkg, err := b.loadPackage(ctx, req.URI, req.Content, req.SnapshotRev)
	if err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return nil, canceled
		}
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
	pos, err := lspPosToTokenPos(req.Content, tf, req.Line, req.Column, req.Encoding, req.EncodingSet)
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
	if err := b.lock(ctx); err != nil {
		return identity.SemanticResult[*languages.HoverResult]{}, err
	}
	defer b.unlock()
	unknown := func(detail string) identity.SemanticResult[*languages.HoverResult] {
		return identity.SemanticResult[*languages.HoverResult]{
			Status:              identity.ResultUnknown,
			Evidence:            evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, languages.EvidenceL1, detail),
			Completeness:        identity.CompletenessUnknown,
			InternalDiagnostics: []string{detail},
		}
	}
	pkg, err := b.loadPackage(ctx, req.URI, req.Content, req.SnapshotRev)
	if err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return identity.SemanticResult[*languages.HoverResult]{}, canceled
		}
		return unknown("package load failed: " + err.Error()), nil
	}
	filePath := uriToPath(req.URI)
	tf, f := b.findFileInPkg(pkg, filePath)
	if tf == nil || f == nil {
		return unknown("file not in compiled package"), nil
	}
	pos, perr := lspPosToTokenPos(req.Content, tf, req.Line, req.Column, req.Encoding, req.EncodingSet)
	if perr != nil {
		return unknown(perr.Error()), nil
	}
	ident := findIdentAt(f, b.fset, pos)
	if ident == nil {
		// Exact negative: the query resolved; there is genuinely no symbol here.
		// ponytail: Definition/References report the same fact as Unknown, and
		// the two are deliberately NOT unified. Unknown is never memoized
		// (§J4), so making hover Unknown would re-run loadPackage on every
		// pointer move over whitespace; making Definition Exact would defeat
		// SEM-SAFE-001, whose rename path (below) refuses to publish unless
		// References came back Unknown. Both directions regress; keep the split
		// and record it rather than trade one invariant for another.
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
	status, level, completeness, detail := packageConfidence(pkg)
	sl, sc := posToLineCol(req.Content, b.fset, ident.Pos(), req.Encoding, req.EncodingSet)
	el, ec := posToLineCol(req.Content, b.fset, ident.End(), req.Encoding, req.EncodingSet)
	var buf strings.Builder
	buf.WriteString("```go\n")
	switch o := obj.(type) {
	case *types.Func:
		sig := o.Type().(*types.Signature)
		buf.WriteString(formatGoFunctionHoverSignature(o.Name(), sig.String()) + "\n")
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
		Status: status,
		Value: &languages.HoverResult{
			Contents: buf.String(),
			Range: &languages.Range{
				StartLine: sl, StartCharacter: sc,
				EndLine: el, EndCharacter: ec,
			},
			Evidence: level,
		},
		Evidence:     evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, level, detail),
		Completeness: completeness,
	}, nil
}

func formatGoFunctionHoverSignature(name, signature string) string {
	return "func " + name + strings.TrimPrefix(signature, "func")
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
	if err := b.lock(ctx); err != nil {
		return identity.SemanticResult[[]languages.Location]{}, err
	}
	defer b.unlock()
	pkg, err := b.loadPackage(ctx, req.URI, req.Content, req.SnapshotRev)
	if err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return identity.SemanticResult[[]languages.Location]{}, canceled
		}
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "package load failed: "+err.Error()), nil
	}
	filePath := uriToPath(req.URI)
	tf, f := b.findFileInPkg(pkg, filePath)
	if tf == nil || f == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "file not in compiled package"), nil
	}
	tpos, perr := lspPosToTokenPos(req.Content, tf, req.Line, req.Column, req.Encoding, req.EncodingSet)
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
	_, level, _, detail := packageConfidence(pkg)
	status := identity.ResultPartial
	completeness := identity.IncompleteKnownSubset
	if len(pkg.Errors) > 0 {
		completeness = identity.CompletenessUnknown
	}
	defSource := sourceForFile(req.Content, filePath, df.Name(), packageOverlaysFromContext(ctx))
	if defSource == nil {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, "definition source unavailable"), nil
	}
	sl, sc := posToLineCol(defSource, b.fset, defPos, req.Encoding, req.EncodingSet)
	el, ec := posToLineCol(defSource, b.fset, obj.Pos()+token.Pos(len(obj.Name())), req.Encoding, req.EncodingSet)
	return identity.SemanticResult[[]languages.Location]{
		Status: status,
		Value: []languages.Location{{
			URI: pathToUri(df.Name()),
			Range: languages.Range{
				StartLine: sl, StartCharacter: sc,
				EndLine: el, EndCharacter: ec,
			},
		}},
		Evidence:     evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, level, detail),
		Completeness: completeness,
	}, nil
}

func (b *Backend) References(ctx context.Context, req languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	pkg, fset, obj, detail, err := b.resolveReferenceTarget(ctx, req)
	if err != nil {
		return identity.SemanticResult[[]languages.Location]{}, err
	}
	if detail != "" {
		return unknownLocs(b, req.SnapshotRev, req.BuildContext, req.Content, detail), nil
	}
	info := pkg.TypesInfo
	filePath := uriToPath(req.URI)
	// A reference set may contain thousands of locations in the same file.
	// Build its position index and read external source files once per query.
	type indexedSource struct {
		content []byte
		index   *position.Index
		uri     string
	}
	sources := make(map[string]indexedSource)
	location := func(path string, start, end token.Pos) (languages.Location, bool) {
		entry, ok := sources[path]
		if !ok {
			content := sourceForFile(req.Content, filePath, path, packageOverlaysFromContext(ctx))
			if content == nil {
				return languages.Location{}, false
			}
			entry = indexedSource{
				content: content,
				index:   position.NewIndex(content, requestEncoding(req.Encoding, req.EncodingSet)),
				uri:     pathToUri(path),
			}
			sources[path] = entry
		}
		sl, sc := posToLineColIndexed(entry.content, entry.index, fset, start)
		el, ec := posToLineColIndexed(entry.content, entry.index, fset, end)
		return languages.Location{
			URI: entry.uri,
			Range: languages.Range{
				StartLine: sl, StartCharacter: sc,
				EndLine: el, EndCharacter: ec,
			},
		}, true
	}
	var refs []languages.Location
	seenLocations := make(map[languages.Location]struct{})
	appendReference := func(loc languages.Location) {
		if _, exists := seenLocations[loc]; exists {
			return
		}
		seenLocations[loc] = struct{}{}
		refs = append(refs, loc)
	}
	if req.IncludeDecl {
		defPos := obj.Pos()
		if defPos != token.NoPos {
			df := fset.File(defPos)
			if df != nil {
				if loc, ok := location(df.Name(), defPos, obj.Pos()+token.Pos(len(obj.Name()))); ok {
					appendReference(loc)
				}
			}
		}
	}
	var walkErr error
	visited := 0
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if canceled, ok := recovered.(referenceWalkCanceled); ok {
					walkErr = canceled.err
					return
				}
				panic(recovered)
			}
		}()
		for _, syntax := range pkg.Syntax {
			ast.Inspect(syntax, func(n ast.Node) bool {
				if n == nil {
					return false
				}
				visited++
				if visited&0xff == 0 {
					if err := ctx.Err(); err != nil {
						panic(referenceWalkCanceled{err: err})
					}
				}
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if info.ObjectOf(id) == obj {
					idf := fset.File(id.Pos())
					if idf != nil {
						if loc, ok := location(idf.Name(), id.Pos(), id.End()); ok {
							appendReference(loc)
						}
					}
				}
				return true
			})
		}
	}()
	if walkErr != nil {
		return identity.SemanticResult[[]languages.Location]{}, walkErr
	}
	if err := ctx.Err(); err != nil {
		return identity.SemanticResult[[]languages.Location]{}, err
	}
	// §B6: references MAY be a conservative known subset — this enumeration
	// covers the loaded package graph, not necessarily every importer.
	// Exportedness rides along so S3 callers can apply Go scope rules:
	// references to an unexported identifier provably live inside this
	// package, which makes the subset complete FOR THAT SYMBOL CLASS.
	status, level, completeness, detail := packageConfidence(pkg)
	return identity.SemanticResult[[]languages.Location]{
		Status:   status,
		Value:    refs,
		Evidence: evidenceFor(b, req.SnapshotRev, req.BuildContext, req.Content, level, detail+"-package-scope"),
		InternalDiagnostics: []string{
			fmt.Sprintf("symbol=%s exported=%t", obj.Name(), ast.IsExported(obj.Name())),
		},
		Completeness: completeness,
	}, nil
}

// resolveReferenceTarget serializes package/cache access only while resolving
// the target identifier. The returned package and FileSet generation are
// immutable for readers; References can then walk the AST without holding the
// backend gate needed by completion and hover.
func (b *Backend) resolveReferenceTarget(ctx context.Context, req languages.ReferencesRequest) (*packages.Package, *token.FileSet, types.Object, string, error) {
	if err := b.lock(ctx); err != nil {
		return nil, nil, nil, "", err
	}
	defer b.unlock()
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, "", err
	}
	pkg, err := b.loadPackage(ctx, req.URI, req.Content, req.SnapshotRev)
	if err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return nil, nil, nil, "", canceled
		}
		return nil, nil, nil, "package load failed: " + err.Error(), nil
	}
	filePath := uriToPath(req.URI)
	tf, f := b.findFileInPkg(pkg, filePath)
	if tf == nil || f == nil {
		return nil, nil, nil, "file not in compiled package", nil
	}
	tpos, err := lspPosToTokenPos(req.Content, tf, req.Line, req.Column, req.Encoding, req.EncodingSet)
	if err != nil {
		return nil, nil, nil, err.Error(), nil
	}
	ident := findIdentAt(f, b.fset, tpos)
	if ident == nil {
		return nil, nil, nil, "no identifier at position", nil
	}
	if pkg.TypesInfo == nil {
		return nil, nil, nil, "type information unavailable", nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, "", err
	}
	obj := pkg.TypesInfo.ObjectOf(ident)
	if obj == nil {
		return nil, nil, nil, "symbol unresolved", nil
	}
	return pkg, b.fset, obj, "", nil
}

func (b *Backend) DocumentSymbols(ctx context.Context, req languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	if err := b.lock(ctx); err != nil {
		return nil, err
	}
	defer b.unlock()
	filePath := uriToPath(req.URI)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, req.Content, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	// UTF-16 column lookup over the parsed content (INV-POS-001).
	idx := position.NewIndex(req.Content, requestEncoding(req.Encoding, req.EncodingSet))
	lc := func(pos token.Pos) (line, col uint32) {
		p := fset.Position(pos)
		return uint32(p.Line - 1), idx.ColumnAt(uint32(p.Offset))
	}
	sym := func(name, detail string, kind languages.SymbolKind, pos, end, sel, selEnd token.Pos) languages.DocumentSymbol {
		sl, sc := lc(pos)
		el, ec := lc(end)
		selL, selC := lc(sel)
		selEndL, selEndC := lc(selEnd)
		return languages.DocumentSymbol{
			Name: name, Detail: detail, Kind: kind,
			StartLine: sl, StartCharacter: sc,
			EndLine: el, EndCharacter: ec,
			SelectionLine: selL, SelectionCharacter: selC,
			SelectionEndLine: selEndL, SelectionEndCharacter: selEndC, SelectionRangeSet: true,
		}
	}
	var symbols []languages.DocumentSymbol
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			s := sym(d.Name.Name, "", languages.SymbolFunction, d.Pos(), d.End(), d.Name.Pos(), d.Name.End())
			if d.Recv != nil && len(d.Recv.List) > 0 {
				s.Kind = languages.SymbolMethod
				s.Detail = fmt.Sprintf("(%s).%s", exprToString(d.Recv.List[0].Type), d.Name.Name)
			} else {
				s.Detail = d.Name.Name
			}
			if d.Type.Params != nil {
				for _, field := range d.Type.Params.List {
					for _, name := range field.Names {
						c := sym(name.Name, exprToString(field.Type), languages.SymbolVariable, name.Pos(), name.End(), name.Pos(), name.End())
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
					s := sym(sp.Name.Name, fmt.Sprintf("type %s", sp.Name.Name), kind, d.Pos(), d.End(), sp.Name.Pos(), sp.Name.End())
					if st, ok := sp.Type.(*ast.StructType); ok && st.Fields != nil {
						for _, field := range st.Fields.List {
							for _, name := range field.Names {
								s.Children = append(s.Children, sym(name.Name, exprToString(field.Type), languages.SymbolField, name.Pos(), name.End(), name.Pos(), name.End()))
							}
						}
					} else if it, ok := sp.Type.(*ast.InterfaceType); ok && it.Methods != nil {
						for _, method := range it.Methods.List {
							for _, name := range method.Names {
								s.Children = append(s.Children, sym(name.Name, exprToString(method.Type), languages.SymbolMethod, name.Pos(), name.End(), name.Pos(), name.End()))
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
						symbols = append(symbols, sym(name.Name, exprToString(sp.Type), kind, name.Pos(), name.End(), name.Pos(), name.End()))
					}
				}
			}
		}
	}
	return symbols, nil
}

func (b *Backend) WorkspaceSymbols(ctx context.Context, req languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	if err := b.lock(ctx); err != nil {
		return nil, err
	}
	defer b.unlock()
	b.rotateFileSetIfNeeded()
	cfg := &packages.Config{
		Context:   ctx,
		Mode:      packages.NeedName | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedFiles,
		Dir:       b.workDir,
		Fset:      b.fset,
		Overlay:   packageOverlaysFromContext(ctx).goPackagesOverlay(),
		ParseFile: parsePackageSource,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(req.Query)
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	var results []languages.WorkspaceSymbol
	// Lazy per-file position indexes (INV-POS-001); files are small.
	fileIdx := make(map[string]*position.Index)
	encoding := requestEncoding(req.Encoding, req.EncodingSet)
	for _, pkg := range pkgs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if pkg.Types == nil {
			continue
		}
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
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
				col = idx.ColumnAt(uint32(p.Offset))
			} else if data, rerr := fileio.ReadFileShared(p.Filename); rerr == nil {
				ix := position.NewIndex(data, encoding)
				fileIdx[p.Filename] = ix
				col = ix.ColumnAt(uint32(p.Offset))
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
	return b.DiagnosticsWithEncoding(ctx, uri, content, 0, 1)
}

func (b *Backend) DiagnosticsWithEncoding(ctx context.Context, uri string, content []byte, snapshotRev uint64, encoding int) ([]languages.Diagnostic, error) {
	if err := b.lock(ctx); err != nil {
		return nil, err
	}
	defer b.unlock()
	pkg, err := b.loadPackage(ctx, uri, content, snapshotRev)
	if err != nil {
		return nil, err
	}
	idx := position.NewIndex(content, requestEncoding(encoding, true))
	var diags []languages.Diagnostic
	parserErrorBeforeEOF := false
	for _, e := range pkg.Errors {
		filePath, line, column, ok := parseCompilerPosition(e.Pos)
		if ok && e.Kind == packages.ParseError && filepath.Clean(filePath) == filepath.Clean(uriToPath(uri)) &&
			!compilerPositionAtEOF(content, line, column) {
			parserErrorBeforeEOF = true
			break
		}
	}
	for _, e := range pkg.Errors {
		filePath, line, column, ok := parseCompilerPosition(e.Pos)
		if !ok || filepath.Clean(filePath) != filepath.Clean(uriToPath(uri)) {
			continue
		}
		// Once the parser has reported an earlier syntax error, its EOF recovery
		// messages are cascades from the broken construct rather than independent
		// actionable diagnostics. Keep the primary error and avoid reporting the
		// same malformed construct as several new errors at end of file.
		if e.Kind == packages.ParseError && parserErrorBeforeEOF && compilerPositionAtEOF(content, line, column) {
			continue
		}
		sl, sc, ok := diagnosticPosition(content, idx, line, column)
		if !ok {
			continue
		}
		el, ec := sl, sc
		if e.Kind != packages.ParseError {
			ec += diagnosticWidth(content, line, column, requestEncoding(encoding, true))
		}
		diags = append(diags, languages.Diagnostic{
			StartLine: sl, StartChar: sc,
			EndLine: el, EndChar: ec,
			Severity: 1, Source: "omnilsp-go", Message: e.Msg,
		})
	}
	return diags, nil
}

func diagnosticWidth(content []byte, line, column int, encoding position.Encoding) uint32 {
	if line < 1 || column < 1 {
		return 1
	}
	lineStart := 0
	for current := 1; current < line; current++ {
		next := bytes.IndexByte(content[lineStart:], '\n')
		if next < 0 {
			return 1
		}
		lineStart += next + 1
	}
	offset := lineStart + column - 1
	if offset < 0 || offset >= len(content) {
		return 1
	}
	r, size := utf8.DecodeRune(content[offset:])
	if size <= 0 {
		return 1
	}
	width := encodedRuneWidth(r, size, encoding)
	if !goIdentifierStart(r) {
		return width
	}

	end := offset + size
	for end < len(content) {
		r, size = utf8.DecodeRune(content[end:])
		if size <= 0 || !goIdentifierContinue(r) {
			break
		}
		width += encodedRuneWidth(r, size, encoding)
		end += size
	}
	if width == 0 {
		return 1
	}
	return width
}

func encodedRuneWidth(r rune, size int, encoding position.Encoding) uint32 {
	switch encoding {
	case position.UTF8:
		return uint32(size)
	case position.UTF16:
		if r > 0xFFFF {
			return 2
		}
		return 1
	case position.UTF32:
		return 1
	default:
		return uint32(size)
	}
}

func goIdentifierStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func goIdentifierContinue(r rune) bool {
	return goIdentifierStart(r) || unicode.IsDigit(r)
}

func parseCompilerPosition(raw string) (string, int, int, bool) {
	last := strings.LastIndexByte(raw, ':')
	if last <= 0 {
		return "", 0, 0, false
	}
	column, err := strconv.Atoi(raw[last+1:])
	if err != nil {
		return "", 0, 0, false
	}
	previous := strings.LastIndexByte(raw[:last], ':')
	if previous <= 0 {
		return "", 0, 0, false
	}
	line, err := strconv.Atoi(raw[previous+1 : last])
	if err != nil || line < 1 || column < 1 {
		return "", 0, 0, false
	}
	return raw[:previous], line, column, true
}

func compilerPositionAtEOF(content []byte, line, column int) bool {
	if line < 1 || column < 1 {
		return false
	}
	lineStart := 0
	for current := 1; current < line; current++ {
		next := bytes.IndexByte(content[lineStart:], '\n')
		if next < 0 {
			return false
		}
		lineStart += next + 1
	}
	return lineStart+column-1 == len(content)
}

func diagnosticPosition(content []byte, idx *position.Index, line, column int) (uint32, uint32, bool) {
	if line < 1 || column < 1 {
		return 0, 0, false
	}
	lineStart := 0
	for current := 1; current < line; current++ {
		next := bytes.IndexByte(content[lineStart:], '\n')
		if next < 0 {
			return 0, 0, false
		}
		lineStart += next + 1
	}
	offset := lineStart + column - 1
	if offset < lineStart || offset > len(content) {
		return 0, 0, false
	}
	pos, err := idx.OffsetToPosition(content, uint32(offset))
	if err != nil {
		return 0, 0, false
	}
	return pos.Line, pos.Col, true
}

func (b *Backend) SemanticTokens(ctx context.Context, uri string, content []byte) ([]languages.SemanticToken, error) {
	return b.SemanticTokensWithEncoding(ctx, uri, content, 1)
}

func (b *Backend) SemanticTokensWithEncoding(ctx context.Context, uri string, content []byte, encoding int) ([]languages.SemanticToken, error) {
	if err := b.lock(ctx); err != nil {
		return nil, err
	}
	defer b.unlock()
	filePath := uriToPath(uri)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filePath, content, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	var tokens []languages.SemanticToken
	var lastLine, lastCol uint32
	// UTF-16 columns/lengths over the parsed content (INV-POS-001).
	idx := position.NewIndex(content, requestEncoding(encoding, true))
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
	if err := ctx.Err(); err != nil {
		return identity.SemanticResult[languages.ValidatedEdit]{}, err
	}
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
		EncodingSet:  req.EncodingSet,
		IncludeDecl:  true,
	})
	if err != nil {
		if canceled := ctx.Err(); canceled != nil {
			return identity.SemanticResult[languages.ValidatedEdit]{}, canceled
		}
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
	collisions, err := b.detectRenameCollision(ctx, req)
	if err != nil {
		return identity.SemanticResult[languages.ValidatedEdit]{}, err
	}
	if len(collisions) > 0 {
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
	b.lockUncancellable()
	defer b.unlock()
	b.resetPackageState()
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
func (b *Backend) detectRenameCollision(ctx context.Context, req languages.RenameRequest) ([]string, error) {
	if err := b.lock(ctx); err != nil {
		return nil, err
	}
	defer b.unlock()
	pkg, err := b.loadPackage(ctx, req.URI, req.Content, req.SnapshotRev)
	if err != nil || pkg == nil || pkg.TypesInfo == nil {
		return nil, err
	}
	tf, f := b.findFileInPkg(pkg, uriToPath(req.URI))
	if tf == nil || f == nil {
		return nil, nil
	}
	pos, perr := lspPosToTokenPos(req.Content, tf, req.Line, req.Column, req.Encoding, req.EncodingSet)
	if perr != nil {
		return nil, nil
	}
	ident := findIdentAt(f, b.fset, pos)
	if ident == nil {
		return nil, nil
	}
	obj := pkg.TypesInfo.ObjectOf(ident)
	if obj == nil || obj.Parent() == nil {
		return nil, nil
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
	return conflicts, nil
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

// Declaration implements languages.DeclarationProvider (§I14/T2). Under
// go/types a declaration site and a definition site coincide — obj.Pos() is
// where the name is introduced — so this is Definition's semantics under the
// declaration method name, served through the optional interface so backends
// with a real distinction can override it.
func (b *Backend) Declaration(ctx context.Context, req languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	return b.Definition(ctx, req)
}
