package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

// Bounded read-only budgets for the dirty overlay pre-screen, mirroring the
// request budget constants of semantic_overlay_facts.go. The screen walks at
// most overlayDirtyScreenMaxDocuments open documents overall and
// overlayDirtyScreenMaxPerLanguage documents of any one language, comparing at
// most overlayDirtyScreenMaxBytes of disk content. Anything beyond those
// budgets stays unscreened: the request falls back to the live backend instead
// of degrading the pre-screen into an unbounded scan.
const (
	overlayDirtyScreenMaxDocuments   = 2048
	overlayDirtyScreenMaxPerLanguage = 512
	overlayDirtyScreenMaxBytes       = int64(32 << 20)
)

// goSnapshotSemanticLocations answers a definition or references request from
// a complete, type-checked export of the exact dirty snapshot for the query
// file's own language. The language is taken from the open query document's
// LanguageID, the matching semantic index binding is selected by it, and any
// failure returns used=false so the caller can preserve the live-backend
// fallback.
func (s *Server) goSnapshotSemanticLocations(
	ctx context.Context,
	queryURI string,
	line, character uint32,
	encoding int,
	revision uint64,
	query persistentSemanticQuery,
	includeDeclaration bool,
) (identity.SemanticResult[[]languages.Location], bool) {
	var zero identity.SemanticResult[[]languages.Location]
	if s == nil || revision == 0 {
		return zero, false
	}
	var current bool
	ctx, current = goOverlayContextAtRevision(s, ctx, revision)
	if !current || ctx.Err() != nil || s.currentRevision() != revision ||
		encoding < int(position.UTF8) || encoding > int(position.UTF32) ||
		query != persistentDefinition && query != persistentReferences {
		return zero, false
	}

	idx, _, _ := s.indexState()
	if idx == nil {
		return zero, false
	}
	parsed, err := uri.Parse(queryURI)
	if err != nil || !parsed.IsFile() {
		return zero, false
	}
	queryURI = parsed.Canonical()
	captured := snapshotFromCtx(ctx)
	if captured == nil {
		return zero, false
	}
	queryDocument := captured.Document(queryURI)
	if queryDocument == nil || queryDocument.URI != queryURI || queryDocument.LanguageID == "" {
		return zero, false
	}
	language := queryDocument.LanguageID
	binding, foundBinding := s.semanticIndexBindingForLanguage(language)
	if !foundBinding || s.overlayFacts == nil {
		return zero, false
	}
	requestOverlay, dirtyLanguages := s.snapshotOverlayMightBeDirty(ctx, idx, queryURI, revision)
	if _, dirty := dirtyLanguages[language]; !dirty {
		return zero, false
	}

	base, err := captureSemanticView(ctx, idx.root, idx.workspaceID, revision, idx.dir)
	if err != nil {
		return zero, false
	}
	defer base.Close()
	view, err := captureGoSnapshotSemanticViewForIdentityExcluding(s, ctx, base, requestOverlay, idx.dir)
	if err != nil {
		return zero, false
	}
	defer view.Close()
	if !view.HasLanguageChanges(language) || !view.StillCurrent(s, ctx) {
		return zero, false
	}
	facts, err := s.overlayFacts.GetOrExport(ctx, s, view, binding.planner, binding.provider, language)
	if err != nil || ctx.Err() != nil || !view.StillCurrent(s, ctx) {
		return zero, false
	}
	locations, queryFile, scope, ok, err := facts.locations(ctx, view, queryURI, line, character, position.Encoding(encoding), query, includeDeclaration)
	if err != nil || !ok {
		return zero, false
	}
	provenance, ok := facts.provenance[scope.ID]
	if !ok || provenance.Backend.Name == "" || provenance.Backend.Language != facts.language || scope.BuildContext == "" {
		return zero, false
	}
	detail := language + "-snapshot-overlay-definition"
	if query == persistentReferences {
		detail = language + "-snapshot-overlay-references"
	}
	if ctx.Err() != nil || !view.StillCurrent(s, ctx) || !view.DiskStillCurrent(ctx) {
		return zero, false
	}
	return identity.SemanticResult[[]languages.Location]{
		Status:       identity.ResultExact,
		Value:        locations,
		Completeness: identity.Complete,
		Evidence: []identity.Evidence{{
			Kind: identity.EvidenceCompiler, Assurance: identity.AssuranceCompilerResolved,
			Snapshot:     identity.SnapshotID{Workspace: facts.identity.Workspace, Revision: identity.SnapshotRevision(revision)},
			BuildContext: scope.BuildContext, Backend: provenance.Backend, BackendEpoch: provenance.BackendEpoch,
			SourceHash: queryFile.SHA256, DetailCode: detail, Timestamp: time.Now().UTC(),
		}},
	}, true
}

// goSnapshotSemanticWorkspaceSymbols returns complete Go symbols from the
// exact dirty editor snapshot. used is true for every established dirty Go
// overlay, including an explicit unknown result when the export cannot prove
// a complete answer; callers must not substitute disk-only Go symbols then.
func (s *Server) goSnapshotSemanticWorkspaceSymbols(
	ctx context.Context,
	query string,
	revision uint64,
	encoding int,
) (identity.SemanticResult[[]languages.WorkspaceSymbol], bool) {
	return s.snapshotSemanticWorkspaceSymbols(ctx, query, revision, encoding, "go")
}

// snapshotSemanticWorkspaceSymbols returns complete workspace symbols of one
// language from the exact dirty editor snapshot. The language selects the
// semantic index binding; used is true for every established dirty overlay of
// that language, including an explicit unknown result when the export cannot
// prove a complete answer; callers must not substitute disk-only symbols then.
func (s *Server) snapshotSemanticWorkspaceSymbols(
	ctx context.Context,
	query string,
	revision uint64,
	encoding int,
	language string,
) (identity.SemanticResult[[]languages.WorkspaceSymbol], bool) {
	var zero identity.SemanticResult[[]languages.WorkspaceSymbol]
	if s == nil || revision == 0 || language == "" || encoding < int(position.UTF8) || encoding > int(position.UTF32) {
		return zero, false
	}
	var current bool
	ctx, current = goOverlayContextAtRevision(s, ctx, revision)
	if !current || ctx.Err() != nil || s.currentRevision() != revision {
		return zero, false
	}
	idx, _, _ := s.indexState()
	if idx == nil {
		return zero, false
	}
	requestOverlay, dirtyLanguages := s.snapshotOverlayMightBeDirty(ctx, idx, "", revision)
	if _, dirty := dirtyLanguages[language]; !dirty {
		return zero, false
	}
	unknown := func(reason string) (identity.SemanticResult[[]languages.WorkspaceSymbol], bool) {
		if ctx.Err() != nil {
			return zero, false
		}
		result := identity.NewUnknownResult[[]languages.WorkspaceSymbol](nil)
		if reason != "" {
			result.InternalDiagnostics = []string{reason}
		}
		return result, true
	}
	binding, foundBinding := s.semanticIndexBindingForLanguage(language)
	if !foundBinding || s.overlayFacts == nil {
		return unknown(fmt.Sprintf("dirty %s snapshot has no complete semantic exporter", language))
	}
	base, err := captureSemanticView(ctx, idx.root, idx.workspaceID, revision, idx.dir)
	if err != nil {
		return unknown(err.Error())
	}
	defer base.Close()
	view, err := captureGoSnapshotSemanticViewForIdentityExcluding(s, ctx, base, requestOverlay, idx.dir)
	if err != nil {
		return unknown(err.Error())
	}
	defer view.Close()
	if !view.HasLanguageChanges(language) || !view.StillCurrent(s, ctx) {
		return unknown(fmt.Sprintf("dirty %s snapshot changed during capture", language))
	}
	facts, err := s.overlayFacts.GetOrExport(ctx, s, view, binding.planner, binding.provider, language)
	if err != nil || ctx.Err() != nil || !view.StillCurrent(s, ctx) {
		if err == nil {
			err = ctx.Err()
		}
		return unknown(errorString(err, language))
	}

	candidates := facts.WorkspaceSymbols(query, maxPersistentWorkspaceSymbols)
	symbols := make([]languages.WorkspaceSymbol, 0, len(candidates))
	contentByURI := make(map[string][]byte)
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return unknown(err.Error())
		}
		occurrence := candidate.Definition
		file, ok := view.file(occurrence.URI)
		if !ok || file.LanguageID != facts.language || file.SHA256 == "" || file.SHA256 != occurrence.SourceHash ||
			candidate.Symbol.ScopeID != occurrence.ScopeID || candidate.Symbol.ID != occurrence.SymbolID {
			return unknown(fmt.Sprintf("workspace symbol definition does not match its captured %s source", language))
		}
		content, cached := contentByURI[occurrence.URI]
		if !cached {
			content, err = readGoSnapshotViewFile(ctx, view, file)
			if err != nil {
				return unknown(err.Error())
			}
			contentByURI[occurrence.URI] = content
		}
		startOffset, endOffset, valid := persistedOccurrenceOffsets(content, occurrence.Range)
		if !valid || string(content[startOffset:endOffset]) != candidate.Symbol.Name {
			return unknown(fmt.Sprintf("workspace symbol range does not identify its captured %s name", language))
		}
		start, valid := persistedPositionInEncoding(content, occurrence.Range.StartLine, occurrence.Range.StartChar, position.Encoding(encoding))
		if !valid {
			return unknown("workspace symbol position cannot be represented in the negotiated encoding")
		}
		symbols = append(symbols, languages.WorkspaceSymbol{
			Name: candidate.Symbol.Name, Kind: semanticLanguageSymbolKind(candidate.Symbol.Kind), URI: occurrence.URI,
			StartLine: start.Line, StartCol: start.Col,
		})
	}
	evidence := make([]identity.Evidence, 0, len(facts.scopes))
	for _, scope := range facts.scopes {
		provenance, ok := facts.provenance[scope.ID]
		if !ok || provenance.Backend.Name == "" || provenance.Backend.Language != facts.language || scope.BuildContext == "" {
			return unknown(fmt.Sprintf("%s workspace symbol provenance is incomplete", language))
		}
		evidence = append(evidence, identity.Evidence{
			Kind: identity.EvidenceCompiler, Assurance: identity.AssuranceCompilerResolved,
			Snapshot:     identity.SnapshotID{Workspace: facts.identity.Workspace, Revision: identity.SnapshotRevision(revision)},
			BuildContext: scope.BuildContext, Backend: provenance.Backend, BackendEpoch: provenance.BackendEpoch,
			SourceHash: view.Identity().DiskDigest, DetailCode: language + "-snapshot-overlay-workspace-symbols", Timestamp: time.Now().UTC(),
		})
	}
	if ctx.Err() != nil || !view.StillCurrent(s, ctx) || !view.DiskStillCurrent(ctx) {
		return unknown(fmt.Sprintf("workspace changed while verifying dirty %s symbols", language))
	}
	return identity.NewExactResult(symbols, evidence), true
}

func errorString(err error, language string) string {
	if err == nil {
		return fmt.Sprintf("dirty %s semantic overlay could not be verified", language)
	}
	return err.Error()
}

func goOverlayContextAtRevision(s *Server, ctx context.Context, revision uint64) (context.Context, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil || s.snapMgr == nil || ctx.Err() != nil {
		return ctx, false
	}
	captured := snapshotFromCtx(ctx)
	if captured == nil {
		captured = s.snapMgr.Current()
		if captured != nil {
			ctx = withSnapshot(ctx, captured)
		}
	}
	current := s.snapMgr.Current()
	return ctx, captured != nil && current != nil && captured.InstanceID() == current.InstanceID() &&
		captured.ID().Revision == revision && current.ID().Revision == revision && s.currentRevision() == revision
}

// semanticIndexBindingForLanguage returns the registered semantic index
// binding for one language, if that language has a complete provider/planner
// capability.
func (s *Server) semanticIndexBindingForLanguage(language string) (semanticIndexBinding, bool) {
	if language == "" {
		return semanticIndexBinding{}, false
	}
	for _, candidate := range s.semanticIndexBindings() {
		if candidate.language == language && candidate.binding.provider != nil && candidate.binding.planner != nil {
			return candidate.binding, true
		}
	}
	return semanticIndexBinding{}, false
}

// snapshotOverlayMightBeDirty cheaply screens normal requests before the
// immutable workspace capture. It only reads a bounded set of open documents —
// at most overlayDirtyScreenMaxDocuments overall and
// overlayDirtyScreenMaxPerLanguage per language, comparing at most
// overlayDirtyScreenMaxBytes of disk content — and returns the languages with
// at least one open document differing from the corresponding disk file.
// Documents that are excluded, over budget, or whose declared language cannot
// be confirmed from the file extension are not attributed to any language, so
// those languages stay out of the result and the caller falls back to the live
// backend instead of scanning without bound. Structural anomalies (documents
// outside the workspace, unreadable targets, missing snapshots) abort the
// whole screen exactly as before.
func (s *Server) snapshotOverlayMightBeDirty(
	ctx context.Context,
	idx *indexService,
	queryURI string,
	revision uint64,
) (semanticOverlayIdentity, map[string]struct{}) {
	if s == nil || idx == nil || ctx == nil || ctx.Err() != nil || revision == 0 {
		return semanticOverlayIdentity{}, nil
	}
	if s.snapMgr == nil {
		return semanticOverlayIdentity{}, nil
	}
	captured := snapshotFromCtx(ctx)
	current := s.snapMgr.Current()
	if captured == nil || current == nil || captured.InstanceID() != current.InstanceID() ||
		captured.ID().Revision != revision || s.currentRevision() != revision {
		return semanticOverlayIdentity{}, nil
	}
	if queryURI != "" {
		queryDocument := captured.Document(queryURI)
		if queryDocument == nil || queryDocument.URI != queryURI {
			return semanticOverlayIdentity{}, nil
		}
	}
	documents := captured.Documents()
	// Snapshot.Documents ranges a map, so its order changes every run. The
	// per-language budget below screens only the first N documents, which
	// would make an otherwise identical query answer from the overlay on one
	// run and fall back to the live backend on the next. Sort so the screened
	// subset is a function of the document set alone.
	sort.Strings(documents)
	if len(documents) > overlayDirtyScreenMaxDocuments {
		return semanticOverlayIdentity{}, nil
	}
	root, err := filepath.Abs(idx.root)
	if err != nil {
		return semanticOverlayIdentity{}, nil
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return semanticOverlayIdentity{}, nil
	}
	dirtyLanguages := make(map[string]struct{})
	screenedPerLanguage := make(map[string]int)
	var screenedBytes int64
	for _, documentURI := range documents {
		if err := ctx.Err(); err != nil {
			return semanticOverlayIdentity{}, nil
		}
		open := captured.Document(documentURI)
		if open == nil || open.URI != documentURI {
			return semanticOverlayIdentity{}, nil
		}
		parsed, err := uri.Parse(open.URI)
		if err != nil || !parsed.IsFile() {
			return semanticOverlayIdentity{}, nil
		}
		path, err := parsed.Path()
		if err != nil {
			return semanticOverlayIdentity{}, nil
		}
		path, err = filepath.Abs(path)
		if err != nil || ensureContained(root, path) != nil {
			return semanticOverlayIdentity{}, nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return semanticOverlayIdentity{}, nil
		}
		if semanticOverlayPathExcluded(root, filepath.ToSlash(rel)) {
			continue
		}
		// Only documents whose declared language matches the workspace file
		// extension can be attributed to a language cheaply. Others (plain
		// text, unknown extensions, editors overriding the extension) are
		// skipped without aborting the screen for every other language.
		if inferred := semanticLanguageID(path); inferred == "" || inferred != open.LanguageID {
			continue
		}
		if screenedPerLanguage[open.LanguageID] >= overlayDirtyScreenMaxPerLanguage {
			continue
		}
		screenedPerLanguage[open.LanguageID]++
		parentReal, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			if os.IsNotExist(err) {
				dirtyLanguages[open.LanguageID] = struct{}{}
				continue
			}
			return semanticOverlayIdentity{}, nil
		}
		if ensureContained(rootReal, parentReal) != nil {
			return semanticOverlayIdentity{}, nil
		}
		target := filepath.Join(parentReal, filepath.Base(path))
		info, err := os.Lstat(target)
		if err != nil {
			if os.IsNotExist(err) {
				dirtyLanguages[open.LanguageID] = struct{}{}
				continue
			}
			return semanticOverlayIdentity{}, nil
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return semanticOverlayIdentity{}, nil
		}
		if info.Size() != int64(len(open.Content)) {
			dirtyLanguages[open.LanguageID] = struct{}{}
			continue
		}
		if info.Size() > goSemanticOverlayMaxOpenBytes {
			return semanticOverlayIdentity{}, nil
		}
		if screenedBytes+info.Size() > overlayDirtyScreenMaxBytes {
			return semanticOverlayIdentity{}, nil
		}
		diskContent, err := os.ReadFile(target)
		if err != nil {
			return semanticOverlayIdentity{}, nil
		}
		screenedBytes += info.Size()
		if !bytes.Equal(diskContent, open.Content) {
			dirtyLanguages[open.LanguageID] = struct{}{}
		}
	}
	if len(dirtyLanguages) == 0 {
		return semanticOverlayIdentity{}, nil
	}
	requestOverlay, ok := captureDirtyOverlayIdentity(s, ctx, revision)
	if !ok {
		return semanticOverlayIdentity{}, nil
	}
	return requestOverlay, dirtyLanguages
}

func captureDirtyOverlayIdentity(s *Server, ctx context.Context, revision uint64) (semanticOverlayIdentity, bool) {
	if ctx == nil || ctx.Err() != nil {
		return semanticOverlayIdentity{}, false
	}
	return captureSemanticOverlayIdentity(s, ctx, revision)
}

func (f *goSnapshotSemanticFacts) locations(
	ctx context.Context,
	view *goSnapshotSemanticView,
	queryURI string,
	line, character uint32,
	encoding position.Encoding,
	query persistentSemanticQuery,
	includeDeclaration bool,
) ([]languages.Location, model.File, model.Scope, bool, error) {
	var noFile model.File
	var noScope model.Scope
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, noFile, noScope, false, err
	}
	if f == nil || view == nil || f.identity != view.Identity() ||
		f.snapshotInstance == 0 || f.snapshotInstance != view.SnapshotInstanceID() ||
		f.revision == 0 || f.revision != view.identity.SnapshotRev {
		return nil, noFile, noScope, false, errGoSemanticOverlayStale
	}
	if encoding < position.UTF8 || encoding > position.UTF32 ||
		query != persistentDefinition && query != persistentReferences {
		return nil, noFile, noScope, false, fmt.Errorf("unsupported %s snapshot semantic query", f.language)
	}
	file, ok := view.file(queryURI)
	if !ok || file.LanguageID != f.language {
		return nil, noFile, noScope, false, nil
	}
	content, err := readGoSnapshotViewFile(ctx, view, file)
	if err != nil {
		return nil, noFile, noScope, false, err
	}
	queryOffset, err := position.OffsetOfLineCharEncoding(content, line, character, encoding)
	if err != nil {
		return nil, noFile, noScope, false, nil
	}
	canonical := position.NewIndex(content, position.UTF16)
	queryPosition, err := canonical.OffsetToPosition(content, queryOffset)
	if err != nil {
		return nil, noFile, noScope, false, nil
	}
	target, found := f.symbolKeyAt(queryURI, queryPosition.Line, queryPosition.Col)
	if !found {
		return nil, noFile, noScope, false, nil
	}
	var scope model.Scope
	for _, candidate := range f.scopes {
		if candidate.ID == target.scopeID {
			scope = candidate
			break
		}
	}
	if scope.ID == "" || !factsScopeComplete(f.coverage, scope.ID) {
		return nil, noFile, noScope, false, errGoSemanticOverlayIncomplete
	}

	selected := make([]model.Occurrence, 0, len(f.bySymbol[target]))
	for _, index := range f.bySymbol[target] {
		if err := ctx.Err(); err != nil {
			return nil, noFile, noScope, false, err
		}
		occurrence := f.occurrences[index]
		include := false
		if query == persistentDefinition {
			include = occurrence.Role == "definition"
		} else {
			include = occurrence.Role == "reference" || includeDeclaration &&
				(occurrence.Role == "definition" || occurrence.Role == "declaration")
		}
		if include {
			selected = append(selected, occurrence)
		}
	}
	if query == persistentDefinition && len(selected) == 0 {
		// An imported symbol may have no definition in this workspace. Let the
		// live Go backend resolve its GOROOT/module source location.
		return nil, noFile, noScope, false, nil
	}
	sort.Slice(selected, func(i, j int) bool { return goOverlayOccurrenceLess(selected[i], selected[j]) })

	locations := make([]languages.Location, 0, len(selected))
	seen := make(map[languages.Location]struct{}, len(selected))
	var sourceURI string
	var sourceContent []byte
	var encoded *position.Index
	for _, occurrence := range selected {
		if err := ctx.Err(); err != nil {
			return nil, noFile, noScope, false, err
		}
		if occurrence.ScopeID != target.scopeID || occurrence.BuildContext != scope.BuildContext {
			return nil, noFile, noScope, false, errGoSemanticOverlayIncomplete
		}
		if occurrence.URI != sourceURI {
			sourceFile, exists := view.file(occurrence.URI)
			if !exists || sourceFile.LanguageID != f.language || sourceFile.SHA256 != occurrence.SourceHash {
				return nil, noFile, noScope, false, errGoSemanticOverlayStale
			}
			sourceContent, err = readGoSnapshotViewFile(ctx, view, sourceFile)
			if err != nil {
				return nil, noFile, noScope, false, err
			}
			sourceURI = occurrence.URI
			encoded = position.NewIndex(sourceContent, encoding)
		}
		startOffset, endOffset, valid := persistedOccurrenceOffsets(sourceContent, occurrence.Range)
		if !valid {
			return nil, noFile, noScope, false, errGoSemanticOverlayIncomplete
		}
		start, err := encoded.OffsetToPosition(sourceContent, startOffset)
		if err != nil {
			return nil, noFile, noScope, false, errGoSemanticOverlayIncomplete
		}
		end, err := encoded.OffsetToPosition(sourceContent, endOffset)
		if err != nil {
			return nil, noFile, noScope, false, errGoSemanticOverlayIncomplete
		}
		location := languages.Location{
			URI: occurrence.URI,
			Range: languages.Range{
				StartLine: start.Line, StartCharacter: start.Col,
				EndLine: end.Line, EndCharacter: end.Col,
			},
		}
		if _, exists := seen[location]; exists {
			continue
		}
		seen[location] = struct{}{}
		locations = append(locations, location)
	}
	sort.Slice(locations, func(i, j int) bool { return semanticLocationLess(locations[i], locations[j]) })
	if err := ctx.Err(); err != nil {
		return nil, noFile, noScope, false, err
	}
	return locations, file, scope, true, nil
}

func factsScopeComplete(coverage []model.Coverage, scopeID string) bool {
	for _, fact := range [...]model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference} {
		complete := false
		for _, item := range coverage {
			if item.ScopeID == scopeID && item.Fact == fact {
				complete = item.State == model.Complete
				break
			}
		}
		if !complete {
			return false
		}
	}
	return true
}

func goOverlayOccurrenceLess(left, right model.Occurrence) bool {
	if left.URI != right.URI {
		return left.URI < right.URI
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}
	if left.Range.StartChar != right.Range.StartChar {
		return left.Range.StartChar < right.Range.StartChar
	}
	if left.Range.EndLine != right.Range.EndLine {
		return left.Range.EndLine < right.Range.EndLine
	}
	if left.Range.EndChar != right.Range.EndChar {
		return left.Range.EndChar < right.Range.EndChar
	}
	if left.Role != right.Role {
		return left.Role < right.Role
	}
	return left.ScopeID < right.ScopeID
}
