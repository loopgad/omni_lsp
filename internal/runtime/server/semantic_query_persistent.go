package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/ccls"
	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/languages/pyright"
	"github.com/omnilsp/omni/internal/languages/rustanalyzer"
	"github.com/omnilsp/omni/internal/languages/typescript"
	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

type persistentSemanticQuery uint8

const (
	persistentDefinition persistentSemanticQuery = iota + 1
	persistentReferences
)

type persistedOccurrenceKey struct {
	scopeID string
	symbol  identity.SymbolID
}

type semanticOverlayDocument struct {
	URI        string               `json:"uri"`
	LanguageID string               `json:"language_id"`
	Version    int64                `json:"version"`
	Content    identity.ContentHash `json:"content_hash"`
}

// semanticOverlayIdentity binds a query to the exact captured editor overlay.
// It is deliberately request-local: the committed generation remains a disk
// generation, and any overlay whose bytes differ from disk falls back live.
type semanticOverlayIdentity struct {
	Revision         uint64
	SnapshotInstance uint64
	VFSRevision      uint64
	Documents        []semanticOverlayDocument
	Digest           identity.ContentHash
}

func captureSemanticOverlayIdentity(s *Server, ctx context.Context, revision uint64) (semanticOverlayIdentity, bool) {
	var zero semanticOverlayIdentity
	if s == nil || s.vfs == nil || s.currentRevision() != revision {
		return zero, false
	}
	captured := snapshotFromCtx(ctx)
	current := (*snapshot.Snapshot)(nil)
	if s.snapMgr != nil {
		current = s.snapMgr.Current()
	}
	if captured == nil {
		captured = current
	}
	if captured != nil && (current == nil || captured.InstanceID() != current.InstanceID() ||
		captured.ID().Revision != revision || current.ID().Revision != revision) {
		return zero, false
	}
	vfsRevision := s.vfs.Revision()
	openURIs := s.vfs.OpenFiles()
	var snapshotURIs []string
	if captured != nil {
		snapshotURIs = captured.Documents()
	}
	if len(openURIs) != len(snapshotURIs) {
		return zero, false
	}
	for i := range openURIs {
		if openURIs[i] != snapshotURIs[i] {
			return zero, false
		}
	}

	out := semanticOverlayIdentity{Revision: revision, VFSRevision: vfsRevision}
	if captured != nil {
		out.SnapshotInstance = captured.InstanceID()
	}
	out.Documents = make([]semanticOverlayDocument, 0, len(openURIs))
	for _, documentURI := range openURIs {
		if captured == nil {
			return zero, false
		}
		doc := captured.Document(documentURI)
		state := s.vfs.Get(documentURI)
		if doc == nil || state == nil || doc.URI != documentURI || state.URI != documentURI ||
			doc.LanguageID != state.LanguageID || doc.Version != state.Version || !bytes.Equal(doc.Content, state.Content) {
			return zero, false
		}
		hash := sha256.Sum256(doc.Content)
		out.Documents = append(out.Documents, semanticOverlayDocument{
			URI: canonicalDocumentURI(documentURI), LanguageID: doc.LanguageID, Version: doc.Version,
			Content: identity.ContentHash("sha256:" + hex.EncodeToString(hash[:])),
		})
	}
	if s.vfs.Revision() != vfsRevision || s.currentRevision() != revision {
		return zero, false
	}
	if s.snapMgr != nil {
		latest := s.snapMgr.Current()
		if captured != nil && (latest == nil || latest.InstanceID() != captured.InstanceID()) {
			return zero, false
		}
	}
	payload, err := json.Marshal(struct {
		Revision         uint64
		SnapshotInstance uint64
		VFSRevision      uint64
		Documents        []semanticOverlayDocument
	}{out.Revision, out.SnapshotInstance, out.VFSRevision, out.Documents})
	if err != nil {
		return zero, false
	}
	digest := sha256.Sum256(payload)
	out.Digest = identity.ContentHash("sha256:" + hex.EncodeToString(digest[:]))
	return out, true
}

func semanticOverlayMatchesDiskView(overlay semanticOverlayIdentity, view *diskSemanticView) bool {
	if view == nil {
		return false
	}
	for _, document := range overlay.Documents {
		file, ok := view.byURI[document.URI]
		if !ok || file.LanguageID != document.LanguageID || file.SHA256 != document.Content {
			return false
		}
	}
	return true
}

func semanticOverlayStillCurrent(s *Server, ctx context.Context, revision uint64, expected semanticOverlayIdentity) bool {
	current, ok := captureSemanticOverlayIdentity(s, ctx, revision)
	return ok && reflect.DeepEqual(current, expected)
}

// persistentSemanticLocations uses a committed semantic generation only when
// its complete identity can be reproduced from the current disk view. Planning
// confirms inputs only; ExportIndex is deliberately never called here.
func (s *Server) persistentSemanticLocations(
	ctx context.Context,
	uri string,
	line, column uint32,
	encoding int,
	revision uint64,
	query persistentSemanticQuery,
	includeDecl bool,
) (identity.SemanticResult[[]languages.Location], bool) {
	uri = canonicalDocumentURI(uri)
	var zero identity.SemanticResult[[]languages.Location]
	idx, _, _ := s.indexState()
	if idx == nil || idx.store == nil || s.currentRevision() != revision {
		return zero, false
	}
	overlay, ok := captureSemanticOverlayIdentity(s, ctx, revision)
	if !ok {
		return zero, false
	}
	if encoding < int(position.UTF8) || encoding > int(position.UTF32) {
		return zero, false
	}
	if query != persistentDefinition && query != persistentReferences {
		return zero, false
	}

	lease, err := idx.store.OpenSnapshotLease(ctx)
	if err != nil {
		return zero, false
	}
	defer lease.Close()
	view := lease.Snapshot()
	if view.ID == 0 {
		return zero, false
	}
	reader, err := semantic.OpenReader(ctx, view)
	if err != nil {
		return zero, false
	}
	defer reader.Close()
	metadata := reader.Metadata()
	if metadata.Identity.Workspace != idx.workspaceID || metadata.Identity.DiskDigest == "" ||
		metadata.Identity.SnapshotRev != 0 || metadata.DiskDigest != metadata.Identity.DiskDigest {
		return zero, false
	}
	for _, scope := range metadata.Scopes {
		if !hasCompleteCoverage(metadata.Coverage, scope.ID, model.FactSymbol) ||
			!hasCompleteCoverage(metadata.Coverage, scope.ID, model.FactDefinition) {
			return zero, false
		}
		if query == persistentReferences && !hasCompleteCoverage(metadata.Coverage, scope.ID, model.FactReference) {
			return zero, false
		}
		if query == persistentReferences && includeDecl && !hasCompleteCoverage(metadata.Coverage, scope.ID, model.FactDeclaration) {
			return zero, false
		}
	}

	diskView, err := captureSemanticView(ctx, idx.root, idx.workspaceID, revision, idx.dir)
	if err != nil {
		return zero, false
	}
	defer diskView.Close()
	if diskView.Identity().Workspace != metadata.Identity.Workspace || diskView.Identity().DiskDigest != metadata.DiskDigest {
		return zero, false
	}
	if !semanticOverlayMatchesDiskView(overlay, diskView) {
		return zero, false
	}
	if !semanticPlanningStillMatches(ctx, s.semanticIndexBindings(), diskView, metadata) {
		return zero, false
	}

	queryFile, ok := diskView.byURI[uri]
	if !ok {
		return zero, false
	}
	queryContent, ok := readSemanticViewFile(ctx, diskView, queryFile)
	if !ok {
		return zero, false
	}
	queryOffset, err := position.OffsetOfLineCharEncoding(queryContent, line, column, position.Encoding(encoding))
	if err != nil {
		return zero, false
	}

	scopes := make(map[string]model.Scope, len(metadata.Scopes))
	provenance := make(map[string]model.Provenance, len(metadata.Provenance))
	for _, scope := range metadata.Scopes {
		scopes[scope.ID] = scope
		provenance[scope.ID] = metadata.Provenance[scope.ID]
	}

	var target persistedOccurrenceKey
	var targetFound bool
	for {
		batch, readErr := reader.Next(ctx)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return zero, false
		}
		if batch.Kind != semantic.BatchOccurrences {
			continue
		}
		for _, occurrence := range batch.Occurrences {
			if occurrence.URI != uri {
				continue
			}
			if string(occurrence.SourceHash) != string(queryFile.SHA256) {
				return zero, false
			}
			start, end, valid := persistedOccurrenceOffsets(queryContent, occurrence.Range)
			if !valid {
				return zero, false
			}
			if queryOffset < start || queryOffset >= end {
				continue
			}
			if !knownOccurrenceRole(occurrence.Role) {
				return zero, false
			}
			candidate := persistedOccurrenceKey{scopeID: occurrence.ScopeID, symbol: occurrence.SymbolID}
			if targetFound && target != candidate {
				return zero, false
			}
			target, targetFound = candidate, true
		}
	}
	if !targetFound {
		return zero, false
	}
	if _, ok := scopes[target.scopeID]; !ok {
		return zero, false
	}
	if err := reader.Rewind(); err != nil {
		return zero, false
	}

	var symbolCount int
	var symbolScope string
	var resultOccurrences []model.Occurrence
	for {
		batch, readErr := reader.Next(ctx)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return zero, false
		}
		switch batch.Kind {
		case semantic.BatchSymbols:
			for _, symbol := range batch.Symbols {
				if symbol.ID != target.symbol {
					continue
				}
				symbolCount++
				symbolScope = symbol.ScopeID
			}
		case semantic.BatchOccurrences:
			for _, occurrence := range batch.Occurrences {
				if occurrence.SymbolID != target.symbol {
					continue
				}
				if !knownOccurrenceRole(occurrence.Role) {
					return zero, false
				}
				file, exists := diskView.byURI[occurrence.URI]
				if !exists || string(file.SHA256) != string(occurrence.SourceHash) {
					return zero, false
				}
				if !persistentOccurrenceIncluded(query, includeDecl, occurrence.Role) {
					continue
				}
				resultOccurrences = append(resultOccurrences, occurrence)
			}
		}
	}
	if symbolCount != 1 || symbolScope == "" {
		return zero, false
	}
	sort.Slice(resultOccurrences, func(i, j int) bool {
		left, right := resultOccurrences[i], resultOccurrences[j]
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
		return left.Range.EndChar < right.Range.EndChar
	})
	var locations []languages.Location
	seenLocations := make(map[languages.Location]struct{})
	var currentURI string
	var currentContent []byte
	for _, occurrence := range resultOccurrences {
		if occurrence.URI != currentURI {
			file := diskView.byURI[occurrence.URI]
			if occurrence.URI == uri {
				currentContent = queryContent
			} else {
				var readable bool
				currentContent, readable = readSemanticViewFile(ctx, diskView, file)
				if !readable {
					return zero, false
				}
			}
			currentURI = occurrence.URI
		}
		if _, _, valid := persistedOccurrenceOffsets(currentContent, occurrence.Range); !valid {
			return zero, false
		}
		start, ok := persistedPositionInEncoding(currentContent, occurrence.Range.StartLine, occurrence.Range.StartChar, position.Encoding(encoding))
		if !ok {
			return zero, false
		}
		end, ok := persistedPositionInEncoding(currentContent, occurrence.Range.EndLine, occurrence.Range.EndChar, position.Encoding(encoding))
		if !ok {
			return zero, false
		}
		location := languages.Location{
			URI: occurrence.URI,
			Range: languages.Range{
				StartLine: start.Line, StartCharacter: start.Col,
				EndLine: end.Line, EndCharacter: end.Col,
			},
		}
		if _, exists := seenLocations[location]; exists {
			continue
		}
		seenLocations[location] = struct{}{}
		locations = append(locations, location)
	}
	sort.Slice(locations, func(i, j int) bool { return semanticLocationLess(locations[i], locations[j]) })
	if err := ctx.Err(); err != nil || !semanticOverlayStillCurrent(s, ctx, revision, overlay) {
		return zero, false
	}
	currentDigest, err := semanticDiskDigest(ctx, idx.root, idx.dir)
	if err != nil || currentDigest != metadata.DiskDigest {
		return zero, false
	}
	currentIndex, _, _ := s.indexState()
	if currentIndex != idx || !semanticGenerationToolsStillMatch(ctx, metadata) ||
		!semanticOverlayStillCurrent(s, ctx, revision, overlay) {
		return zero, false
	}

	proof := provenance[target.scopeID]
	result := identity.SemanticResult[[]languages.Location]{
		Status:       identity.ResultExact,
		Value:        locations,
		Completeness: identity.Complete,
		Evidence: []identity.Evidence{{
			Kind: identity.EvidenceIndex, Assurance: identity.AssuranceIndexedExact,
			Snapshot:     identity.SnapshotID{Workspace: metadata.Identity.Workspace, Revision: identity.SnapshotRevision(revision)},
			BuildContext: proof.Scope.BuildContext, Backend: proof.Backend, BackendEpoch: proof.BackendEpoch,
			IndexGen: identity.IndexGeneration(view.ID), SourceHash: queryFile.SHA256,
			DetailCode: persistentSemanticDetail(query), Timestamp: time.Now().UTC(),
		}},
	}
	return result, true
}

func semanticPlanningStillMatches(ctx context.Context, bindings []namedSemanticIndexBinding, view *diskSemanticView, metadata semantic.Metadata) bool {
	if view == nil {
		return false
	}
	var requests []model.Request
	if len(bindings) == 0 {
		// Read-only recovery uses current source/config discovery and verifies
		// every pinned tool file. It never registers or starts an extractor.
		groups := make(map[string][]model.Provenance)
		for _, scope := range metadata.Scopes {
			provenance, ok := metadata.Provenance[scope.ID]
			if !ok {
				return false
			}
			family := scope.Language
			if family == "javascript" {
				family = "typescript"
			}
			if family == "c" {
				family = "cpp"
			}
			if family != "typescript" && family != "python" && family != "go" && family != "rust" && family != "cpp" {
				return false
			}
			groups[family] = append(groups[family], provenance)
		}
		for _, family := range []string{"typescript", "python", "go", "rust", "cpp"} {
			attestations := groups[family]
			if len(attestations) == 0 {
				continue
			}
			var request model.Request
			var err error
			switch family {
			case "typescript":
				request, err = typescript.RebuildVerifiedPlannerRequest(ctx, view, view.rootURI, attestations)
			case "python":
				request, err = pyright.RebuildVerifiedPlannerRequest(ctx, view, view.rootURI, attestations)
			case "go":
				request, err = golang.RebuildVerifiedPlannerRequest(ctx, view, view.rootURI, attestations)
			case "rust":
				request, err = rustanalyzer.RebuildVerifiedPlannerRequest(ctx, view, view.rootURI, attestations)
			case "cpp":
				request, err = ccls.RebuildVerifiedPlannerRequest(ctx, view, view.rootURI, attestations)
			}
			if err != nil {
				return false
			}
			requests = append(requests, request)
		}
	} else {
		for _, binding := range bindings {
			request, err := binding.binding.planner.BuildIndexRequest(ctx, view, view.rootURI)
			if err != nil {
				// One language whose planner cannot restate its plan this round
				// (a Go workspace without go.mod, for example) contributes no
				// request. The scope-count and provenance equality checks below
				// still reject the generation unless every saved scope is
				// reproduced by the remaining requests.
				continue
			}
			requests = append(requests, request)
		}
	}
	if len(requests) == 0 {
		return false
	}
	currentScopes := make(map[string]model.Scope)
	currentProvenance := make(map[string]model.Provenance)
	for _, request := range requests {
		if request.View == nil || request.View.Identity() != view.Identity() ||
			len(request.Scopes) == 0 || len(request.Provenance) != len(request.Scopes) {
			return false
		}
		for _, scope := range request.Scopes {
			if _, duplicate := currentScopes[scope.ID]; duplicate {
				return false
			}
			provenance, ok := request.Provenance[scope.ID]
			if !ok || scope.ID == "" || scope.BuildContext == "" || !semanticScopeWithinWorkspace(view.rootPath, scope.RootURI) ||
				!reflect.DeepEqual(provenance.Scope, scope) || provenance.Identity.Workspace != view.Identity().Workspace ||
				provenance.Identity.DiskDigest != view.Identity().DiskDigest || provenance.Identity.Repository != view.Identity().Repository ||
				provenance.Identity.Revision != view.Identity().Revision || provenance.SchemaVersion != model.SchemaVersion ||
				provenance.Extractor == "" || provenance.ExtractorVer == "" || provenance.Toolchain == "" ||
				provenance.Backend.Name == "" || provenance.Backend.Language != scope.Language ||
				scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
				return false
			}
			currentScopes[scope.ID] = scope
			currentProvenance[scope.ID] = provenance
		}
	}
	if len(currentScopes) != len(metadata.Scopes) || len(currentProvenance) != len(metadata.Provenance) {
		return false
	}
	for _, savedScope := range metadata.Scopes {
		currentScope, ok := currentScopes[savedScope.ID]
		if !ok || !reflect.DeepEqual(savedScope, currentScope) {
			return false
		}
		saved, ok := metadata.Provenance[savedScope.ID]
		if !ok {
			return false
		}
		current := currentProvenance[savedScope.ID]
		if saved.SchemaVersion != current.SchemaVersion || saved.Identity.Workspace != current.Identity.Workspace ||
			saved.Identity.DiskDigest != current.Identity.DiskDigest || saved.Identity.Repository != current.Identity.Repository ||
			saved.Identity.Revision != current.Identity.Revision || !reflect.DeepEqual(saved.Scope, current.Scope) ||
			saved.Extractor != current.Extractor || saved.ExtractorVer != current.ExtractorVer ||
			saved.Backend != current.Backend || saved.BackendEpoch != current.BackendEpoch || saved.Toolchain != current.Toolchain ||
			!sameToolIdentityTuples(saved.Tools, current.Tools) {
			return false
		}
	}
	return true
}

func semanticScopeWithinWorkspace(workspaceRoot, scopeRoot string) bool {
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return false
	}
	scope, err := uri.Parse(scopeRoot)
	if err != nil || !scope.IsFile() {
		return false
	}
	scopePath, err := scope.Path()
	if err != nil {
		return false
	}
	scopePath, err = filepath.Abs(scopePath)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, scopePath)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func sameToolIdentityTuples(left, right []model.ToolIdentity) bool {
	canonical := func(tools []model.ToolIdentity) []model.ToolIdentity {
		out := append([]model.ToolIdentity(nil), tools...)
		sort.Slice(out, func(i, j int) bool {
			if out[i].Name != out[j].Name {
				return out[i].Name < out[j].Name
			}
			if out[i].Path != out[j].Path {
				return out[i].Path < out[j].Path
			}
			if out[i].Version != out[j].Version {
				return out[i].Version < out[j].Version
			}
			return out[i].SHA256 < out[j].SHA256
		})
		return out
	}
	return reflect.DeepEqual(canonical(left), canonical(right))
}

func semanticGenerationToolsStillMatch(ctx context.Context, metadata semantic.Metadata) bool {
	hashes := make(map[string][]string)
	for scopeID, provenance := range metadata.Provenance {
		used := metadata.UsedTools[scopeID]
		if !sameToolIdentityTuples(provenance.Tools, used) {
			return false
		}
		for _, tool := range append(append([]model.ToolIdentity(nil), provenance.Tools...), used...) {
			if tool.Path == "" || tool.SHA256 == "" {
				return false
			}
			hashes[tool.Path] = append(hashes[tool.Path], tool.SHA256)
		}
	}
	for path, expected := range hashes {
		if !toolPathStillMatches(ctx, path, expected) {
			return false
		}
	}
	return true
}

func readSemanticViewFile(ctx context.Context, view *diskSemanticView, file model.File) ([]byte, bool) {
	reader, err := view.Read(ctx, file.URI)
	if err != nil {
		return nil, false
	}
	data, readErr := io.ReadAll(contextReader{ctx: ctx, reader: reader})
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || ctx.Err() != nil {
		return nil, false
	}
	sum := sha256.Sum256(data)
	actual := "sha256:" + hex.EncodeToString(sum[:])
	if actual != string(file.SHA256) {
		return nil, false
	}
	return data, true
}

func persistedOccurrenceOffsets(content []byte, r model.Position) (uint32, uint32, bool) {
	start, err := position.OffsetOfLineChar(content, r.StartLine, r.StartChar)
	if err != nil {
		return 0, 0, false
	}
	end, err := position.OffsetOfLineChar(content, r.EndLine, r.EndChar)
	if err != nil || end <= start {
		return 0, 0, false
	}
	return start, end, true
}

func persistedPositionInEncoding(content []byte, line, character uint32, encoding position.Encoding) (position.Pos, bool) {
	offset, err := position.OffsetOfLineChar(content, line, character)
	if err != nil {
		return position.Pos{}, false
	}
	converted, err := position.NewIndex(content, encoding).OffsetToPosition(content, offset)
	if err != nil {
		return position.Pos{}, false
	}
	return converted, true
}

func knownOccurrenceRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "definition", "declaration", "reference":
		return true
	default:
		return false
	}
}

func persistentOccurrenceIncluded(query persistentSemanticQuery, includeDecl bool, role string) bool {
	role = strings.ToLower(strings.TrimSpace(role))
	switch query {
	case persistentDefinition:
		return role == "definition"
	case persistentReferences:
		return role == "reference" || includeDecl && (role == "definition" || role == "declaration")
	default:
		return false
	}
}

func semanticLocationLess(left, right languages.Location) bool {
	if left.URI != right.URI {
		return left.URI < right.URI
	}
	if left.Range.StartLine != right.Range.StartLine {
		return left.Range.StartLine < right.Range.StartLine
	}
	if left.Range.StartCharacter != right.Range.StartCharacter {
		return left.Range.StartCharacter < right.Range.StartCharacter
	}
	if left.Range.EndLine != right.Range.EndLine {
		return left.Range.EndLine < right.Range.EndLine
	}
	return left.Range.EndCharacter < right.Range.EndCharacter
}

func persistentSemanticDetail(query persistentSemanticQuery) string {
	if query == persistentReferences {
		return "persistent-references"
	}
	return "persistent-definition"
}
