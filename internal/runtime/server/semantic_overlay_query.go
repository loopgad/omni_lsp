package server

import (
	"bytes"
	"context"
	"errors"
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

// goSnapshotSemanticLocations answers a definition or references request from
// a complete, type-checked export of the exact dirty Go snapshot. Any failure
// returns used=false so the caller can preserve the live-backend fallback.
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
	requestOverlay, dirty := s.goSnapshotOverlayMightBeDirty(ctx, idx, queryURI, revision)
	if !dirty {
		return zero, false
	}
	var binding semanticIndexBinding
	foundBinding := false
	for _, candidate := range s.semanticIndexBindings() {
		if candidate.language == "go" {
			binding, foundBinding = candidate.binding, true
			break
		}
	}
	if !foundBinding || binding.provider == nil || binding.planner == nil || s.overlayFacts == nil {
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
	if !view.HasGoChanges() || !view.changedLanguagesAreGo() || !view.StillCurrent(s, ctx) {
		return zero, false
	}
	facts, err := s.overlayFacts.GetOrExport(ctx, s, view, binding.planner, binding.provider)
	if err != nil || ctx.Err() != nil || !view.StillCurrent(s, ctx) {
		return zero, false
	}
	locations, queryFile, scope, ok, err := facts.locations(ctx, view, queryURI, line, character, position.Encoding(encoding), query, includeDeclaration)
	if err != nil || !ok {
		return zero, false
	}
	provenance, ok := facts.provenance[scope.ID]
	if !ok || provenance.Backend.Name == "" || provenance.Backend.Language != "go" || scope.BuildContext == "" {
		return zero, false
	}
	detail := "go-snapshot-overlay-definition"
	if query == persistentReferences {
		detail = "go-snapshot-overlay-references"
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
	var zero identity.SemanticResult[[]languages.WorkspaceSymbol]
	if s == nil || revision == 0 || encoding < int(position.UTF8) || encoding > int(position.UTF32) {
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
	requestOverlay, dirty := s.goSnapshotOverlayMightBeDirty(ctx, idx, "", revision)
	if !dirty {
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
	var binding semanticIndexBinding
	foundBinding := false
	for _, candidate := range s.semanticIndexBindings() {
		if candidate.language == "go" {
			binding, foundBinding = candidate.binding, true
			break
		}
	}
	if !foundBinding || binding.provider == nil || binding.planner == nil || s.overlayFacts == nil {
		return unknown("dirty Go snapshot has no complete semantic exporter")
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
	if !view.HasGoChanges() || !view.changedLanguagesAreGo() || !view.StillCurrent(s, ctx) {
		return unknown("dirty Go snapshot changed during capture")
	}
	facts, err := s.overlayFacts.GetOrExport(ctx, s, view, binding.planner, binding.provider)
	if err != nil || ctx.Err() != nil || !view.StillCurrent(s, ctx) {
		if err == nil {
			err = ctx.Err()
		}
		return unknown(errorString(err))
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
		if !ok || file.LanguageID != "go" || file.SHA256 == "" || file.SHA256 != occurrence.SourceHash ||
			candidate.Symbol.ScopeID != occurrence.ScopeID || candidate.Symbol.ID != occurrence.SymbolID {
			return unknown("workspace symbol definition does not match its captured Go source")
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
			return unknown("workspace symbol range does not identify its captured Go name")
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
		if !ok || provenance.Backend.Name == "" || provenance.Backend.Language != "go" || scope.BuildContext == "" {
			return unknown("Go workspace symbol provenance is incomplete")
		}
		evidence = append(evidence, identity.Evidence{
			Kind: identity.EvidenceCompiler, Assurance: identity.AssuranceCompilerResolved,
			Snapshot:     identity.SnapshotID{Workspace: facts.identity.Workspace, Revision: identity.SnapshotRevision(revision)},
			BuildContext: scope.BuildContext, Backend: provenance.Backend, BackendEpoch: provenance.BackendEpoch,
			SourceHash: view.Identity().DiskDigest, DetailCode: "go-snapshot-overlay-workspace-symbols", Timestamp: time.Now().UTC(),
		})
	}
	if ctx.Err() != nil || !view.StillCurrent(s, ctx) || !view.DiskStillCurrent(ctx) {
		return unknown("workspace changed while verifying dirty Go symbols")
	}
	return identity.NewExactResult(symbols, evidence), true
}

func errorString(err error) string {
	if err == nil {
		return "dirty Go semantic overlay could not be verified"
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

// goSnapshotOverlayMightBeDirty cheaply screens normal requests before the
// immutable workspace capture. It only reads bounded open Go documents and
// returns true when at least one differs from the corresponding disk file.
func (s *Server) goSnapshotOverlayMightBeDirty(
	ctx context.Context,
	idx *indexService,
	queryURI string,
	revision uint64,
) (semanticOverlayIdentity, bool) {
	var zero semanticOverlayIdentity
	if s == nil || idx == nil || ctx == nil || ctx.Err() != nil || revision == 0 {
		return zero, false
	}
	if s.snapMgr == nil {
		return zero, false
	}
	captured := snapshotFromCtx(ctx)
	current := s.snapMgr.Current()
	if captured == nil || current == nil || captured.InstanceID() != current.InstanceID() ||
		captured.ID().Revision != revision || s.currentRevision() != revision {
		return zero, false
	}
	if queryURI != "" {
		queryDocument := captured.Document(queryURI)
		if queryDocument == nil || queryDocument.URI != queryURI || queryDocument.LanguageID != "go" {
			return zero, false
		}
	}
	hasOpenGoDocument := false
	for _, documentURI := range captured.Documents() {
		document := captured.Document(documentURI)
		if document != nil && document.LanguageID == "go" {
			hasOpenGoDocument = true
			break
		}
	}
	if !hasOpenGoDocument {
		return zero, false
	}
	root, err := filepath.Abs(idx.root)
	if err != nil {
		return zero, false
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return zero, false
	}
	for _, documentURI := range captured.Documents() {
		if err := ctx.Err(); err != nil {
			return zero, false
		}
		open := captured.Document(documentURI)
		if open == nil {
			return zero, false
		}
		if open.LanguageID != "go" {
			continue
		}
		if open.URI != documentURI {
			return zero, false
		}
		parsed, err := uri.Parse(open.URI)
		if err != nil || !parsed.IsFile() {
			return zero, false
		}
		path, err := parsed.Path()
		if err != nil {
			return zero, false
		}
		path, err = filepath.Abs(path)
		if err != nil || ensureContained(root, path) != nil {
			return zero, false
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return zero, false
		}
		if semanticOverlayPathExcluded(root, filepath.ToSlash(rel)) {
			continue
		}
		if semanticLanguageID(path) != open.LanguageID {
			return zero, false
		}
		parentReal, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			if os.IsNotExist(err) {
				return captureDirtyGoOverlayIdentity(s, ctx, revision)
			}
			return zero, false
		}
		if ensureContained(rootReal, parentReal) != nil {
			return zero, false
		}
		target := filepath.Join(parentReal, filepath.Base(path))
		info, err := os.Lstat(target)
		if err != nil {
			if os.IsNotExist(err) {
				return captureDirtyGoOverlayIdentity(s, ctx, revision)
			}
			return zero, false
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return zero, false
		}
		if info.Size() != int64(len(open.Content)) {
			return captureDirtyGoOverlayIdentity(s, ctx, revision)
		}
		if info.Size() > goSemanticOverlayMaxOpenBytes {
			return zero, false
		}
		diskContent, err := os.ReadFile(target)
		if err != nil {
			return zero, false
		}
		if !bytes.Equal(diskContent, open.Content) {
			return captureDirtyGoOverlayIdentity(s, ctx, revision)
		}
	}
	return zero, false
}

func captureDirtyGoOverlayIdentity(s *Server, ctx context.Context, revision uint64) (semanticOverlayIdentity, bool) {
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
		return nil, noFile, noScope, false, errors.New("unsupported Go snapshot semantic query")
	}
	file, ok := view.file(queryURI)
	if !ok || file.LanguageID != "go" {
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
			if !exists || sourceFile.LanguageID != "go" || sourceFile.SHA256 != occurrence.SourceHash {
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
