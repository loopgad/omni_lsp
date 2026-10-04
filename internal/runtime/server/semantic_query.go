package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/position"
)

type persistedSymbol struct {
	key        string
	name       string
	language   string
	where      languages.WorkspaceSymbol
	definition model.Occurrence
}

const maxPersistentWorkspaceSymbols = 100

// persistentWorkspaceSymbols only serves a complete, disk-current generation.
// Open documents can share that generation when their captured bytes match
// disk; unsaved changes continue to use the live backend.
func (s *Server) persistentWorkspaceSymbols(ctx context.Context, query string, revision uint64) ([]languages.WorkspaceSymbol, bool) {
	idx, _, _ := s.indexState()
	if idx == nil || idx.store == nil {
		return nil, false
	}
	if s.currentRevision() != revision {
		return nil, false
	}
	overlay, ok := captureSemanticOverlayIdentity(s, ctx, revision)
	if !ok {
		return nil, false
	}
	lease, err := idx.store.OpenSnapshotLease(ctx)
	if err != nil {
		return nil, false
	}
	defer lease.Close()
	reader, err := semantic.OpenReader(ctx, lease.Snapshot())
	if err != nil {
		return nil, false
	}
	defer reader.Close()
	metadata := reader.Metadata()
	if metadata.Identity.Workspace != idx.workspaceID || metadata.Identity.DiskDigest == "" ||
		metadata.Identity.SnapshotRev != 0 || metadata.DiskDigest != metadata.Identity.DiskDigest {
		return nil, false
	}
	for _, scope := range metadata.Scopes {
		if !hasCompleteCoverage(metadata.Coverage, scope.ID, model.FactSymbol) ||
			!hasCompleteCoverage(metadata.Coverage, scope.ID, model.FactDefinition) {
			return nil, false
		}
	}
	toolHashes := toolHashesByPath(metadata.Provenance)
	for path, hashes := range toolHashes {
		if !toolPathStillMatches(ctx, path, hashes) {
			return nil, false
		}
	}
	diskView, err := captureSemanticView(ctx, idx.root, idx.workspaceID, revision, idx.dir)
	if err != nil {
		return nil, false
	}
	defer diskView.Close()
	if diskView.Identity().Workspace != metadata.Identity.Workspace ||
		diskView.Identity().DiskDigest != metadata.DiskDigest ||
		!semanticOverlayMatchesDiskView(overlay, diskView) ||
		!semanticPlanningStillMatches(ctx, s.semanticIndexBindings(), diskView, metadata) {
		return nil, false
	}

	scopes := make(map[string]model.Scope, len(metadata.Scopes))
	for _, scope := range metadata.Scopes {
		scopes[scope.ID] = scope
	}
	needle := strings.ToLower(query)
	// Reader validates every segment before exposing it. Since it does not
	// promise a symbol/occurrence segment order, select the bounded result set
	// first and read definitions in a second pass over the same leased view.
	matches := make(map[string]persistedSymbol)
	for {
		batch, readErr := reader.Next(ctx)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, false
		}
		switch batch.Kind {
		case semantic.BatchSymbols:
			for _, symbol := range batch.Symbols {
				if symbol.Kind == "module_graph_node" || symbol.Kind == "synthetic_script_scope" {
					continue // graph ownership nodes are not source declarations
				}
				key := symbol.ScopeID + "\x00" + string(symbol.ID)
				if symbol.Name == "" || !strings.Contains(strings.ToLower(symbol.Name), needle) {
					continue
				}
				scope, ok := scopes[symbol.ScopeID]
				if !ok {
					return nil, false
				}
				candidate := persistedSymbol{
					key:      key,
					name:     symbol.Name,
					language: scope.Language,
					where: languages.WorkspaceSymbol{
						Name: symbol.Name, Kind: semanticLanguageSymbolKind(symbol.Kind),
					},
				}
				retainWorkspaceSymbolCandidate(matches, candidate)
			}
		}
	}
	if err := ctx.Err(); err != nil || !semanticOverlayStillCurrent(s, ctx, revision, overlay) {
		return nil, false
	}
	if len(matches) != 0 {
		if err := reader.Rewind(); err != nil {
			return nil, false
		}
		for {
			batch, readErr := reader.Next(ctx)
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				return nil, false
			}
			if batch.Kind != semantic.BatchOccurrences {
				continue
			}
			for _, occurrence := range batch.Occurrences {
				if occurrence.Role != "definition" && occurrence.Role != "declaration" {
					continue
				}
				key := occurrence.ScopeID + "\x00" + string(occurrence.SymbolID)
				candidate, ok := matches[key]
				if !ok {
					continue
				}
				if occurrence.Role == "declaration" && !hasCompleteCoverage(metadata.Coverage, occurrence.ScopeID, model.FactDeclaration) {
					continue
				}
				if candidate.definition.Role == "definition" && occurrence.Role == "declaration" {
					continue
				}
				if candidate.where.URI == "" || candidate.definition.Role == "declaration" && occurrence.Role == "definition" || occurrenceBeforeWorkspaceSymbol(occurrence, candidate.where) {
					setWorkspaceSymbolLocation(&candidate.where, occurrence)
					candidate.definition = occurrence
					matches[key] = candidate
				}
			}
		}
	}
	if err := ctx.Err(); err != nil || !semanticOverlayStillCurrent(s, ctx, revision, overlay) {
		return nil, false
	}
	out := make([]persistedSymbol, 0, len(matches))
	for _, candidate := range matches {
		if candidate.where.URI == "" {
			return nil, false
		}
		out = append(out, candidate)
	}
	// Validate bounded selected definitions against the captured source bytes,
	// and translate their UTF-16 positions to the negotiated client encoding.
	sort.Slice(out, func(i, j int) bool { return out[i].where.URI < out[j].where.URI })
	var currentURI string
	var currentContent []byte
	for i := range out {
		candidate := &out[i]
		file, exists := diskView.byURI[candidate.where.URI]
		if !exists || file.SHA256 != candidate.definition.SourceHash {
			return nil, false
		}
		if currentURI != file.URI {
			var readable bool
			currentContent, readable = readSemanticViewFile(ctx, diskView, file)
			if !readable {
				return nil, false
			}
			currentURI = file.URI
		}
		if _, _, valid := persistedOccurrenceOffsets(currentContent, candidate.definition.Range); !valid {
			return nil, false
		}
		start, valid := persistedPositionInEncoding(currentContent, candidate.definition.Range.StartLine, candidate.definition.Range.StartChar, position.Encoding(s.negotiatedEncodingInt()))
		if !valid {
			return nil, false
		}
		candidate.where.StartLine, candidate.where.StartCol = start.Line, start.Col
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := strings.ToLower(out[i].name), strings.ToLower(out[j].name)
		if left != right {
			return left < right
		}
		if out[i].language != out[j].language {
			return out[i].language < out[j].language
		}
		if out[i].where.URI != out[j].where.URI {
			return out[i].where.URI < out[j].where.URI
		}
		if out[i].where.StartLine != out[j].where.StartLine {
			return out[i].where.StartLine < out[j].where.StartLine
		}
		if out[i].where.StartCol != out[j].where.StartCol {
			return out[i].where.StartCol < out[j].where.StartCol
		}
		return out[i].key < out[j].key
	})
	if len(out) > maxPersistentWorkspaceSymbols {
		out = out[:maxPersistentWorkspaceSymbols]
	}
	digest, err := semanticDiskDigest(ctx, idx.root, idx.dir)
	if err != nil || digest != metadata.DiskDigest {
		return nil, false
	}
	currentIndex, _, _ := s.indexState()
	if currentIndex != idx || !semanticGenerationToolsStillMatch(ctx, metadata) ||
		!semanticOverlayStillCurrent(s, ctx, revision, overlay) {
		return nil, false
	}
	result := make([]languages.WorkspaceSymbol, len(out))
	for i := range out {
		result[i] = out[i].where
	}
	evidence := make([]identity.Evidence, 0, len(metadata.Scopes))
	for _, scope := range metadata.Scopes {
		proof := metadata.Provenance[scope.ID]
		evidence = append(evidence, identity.Evidence{
			Kind: identity.EvidenceIndex, Assurance: identity.AssuranceIndexedExact,
			Snapshot:     identity.SnapshotID{Workspace: metadata.Identity.Workspace, Revision: identity.SnapshotRevision(revision)},
			BuildContext: scope.BuildContext, Backend: proof.Backend, BackendEpoch: proof.BackendEpoch,
			IndexGen: identity.IndexGeneration(lease.Snapshot().ID), SourceHash: metadata.DiskDigest,
			DetailCode: "persistent.workspace_symbols", Timestamp: time.Now().UTC(),
		})
	}
	s.recordEvidence(ctx, "workspace/symbol", "", identity.ResultExact, identity.Complete, evidence, nil)
	return result, true
}

func hasCompleteCoverage(coverage []model.Coverage, scope string, fact model.FactKind) bool {
	for _, item := range coverage {
		if item.ScopeID == scope && item.Fact == fact {
			return item.State == model.Complete
		}
	}
	return false
}

func toolIdentityStillMatches(ctx context.Context, tool model.ToolIdentity) bool {
	return toolPathStillMatches(ctx, tool.Path, []string{tool.SHA256})
}

func toolHashesByPath(provenance map[string]model.Provenance) map[string][]string {
	result := make(map[string][]string)
	for _, item := range provenance {
		for _, tool := range item.Tools {
			hashes := result[tool.Path]
			duplicate := false
			for _, hash := range hashes {
				if strings.EqualFold(hash, tool.SHA256) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				result[tool.Path] = append(hashes, tool.SHA256)
			}
		}
	}
	return result
}

func toolPathStillMatches(ctx context.Context, path string, expectedHashes []string) bool {
	if err := ctx.Err(); err != nil || path == "" || len(expectedHashes) == 0 {
		return false
	}
	for _, expected := range expectedHashes {
		if expected == "" {
			return false
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, contextReader{ctx: ctx, reader: file})
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || ctx.Err() != nil {
		return false
	}
	actual := hex.EncodeToString(h.Sum(nil))
	for _, expected := range expectedHashes {
		if !strings.EqualFold(actual, expected) {
			return false
		}
	}
	return true
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func setWorkspaceSymbolLocation(where *languages.WorkspaceSymbol, occurrence model.Occurrence) {
	where.URI = occurrence.URI
	where.StartLine = occurrence.Range.StartLine
	where.StartCol = occurrence.Range.StartChar
}

func retainWorkspaceSymbolCandidate(selected map[string]persistedSymbol, candidate persistedSymbol) {
	if len(selected) < maxPersistentWorkspaceSymbols {
		selected[candidate.key] = candidate
		return
	}
	worstKey := ""
	var worst persistedSymbol
	for key, current := range selected {
		if worstKey == "" || persistedSymbolCandidateLess(worst, current) {
			worstKey = key
			worst = current
		}
	}
	if !persistedSymbolCandidateLess(candidate, worst) {
		return
	}
	delete(selected, worstKey)
	selected[candidate.key] = candidate
}

func persistedSymbolCandidateLess(left, right persistedSymbol) bool {
	// Definition locations are unavailable in the bounded selection pass, so
	// use a total symbol-only order. The symbol key makes case-insensitive name
	// ties deterministic without retaining extra candidates.
	leftName, rightName := strings.ToLower(left.name), strings.ToLower(right.name)
	if leftName != rightName {
		return leftName < rightName
	}
	if left.language != right.language {
		return left.language < right.language
	}
	if left.name != right.name {
		return left.name < right.name
	}
	return left.key < right.key
}

func occurrenceBeforeWorkspaceSymbol(left model.Occurrence, right languages.WorkspaceSymbol) bool {
	if left.URI != right.URI {
		return left.URI < right.URI
	}
	if left.Range.StartLine != right.StartLine {
		return left.Range.StartLine < right.StartLine
	}
	return left.Range.StartChar < right.StartCol
}

func semanticLanguageSymbolKind(kind string) languages.SymbolKind {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch {
	case strings.Contains(kind, "interface"):
		return languages.SymbolInterface
	case strings.Contains(kind, "class"):
		return languages.SymbolClass
	case strings.Contains(kind, "struct"):
		return languages.SymbolStruct
	case strings.Contains(kind, "enum"):
		return languages.SymbolEnum
	case strings.Contains(kind, "method"):
		return languages.SymbolMethod
	case strings.Contains(kind, "function"):
		return languages.SymbolFunction
	case strings.Contains(kind, "constructor"):
		return languages.SymbolConstructor
	case strings.Contains(kind, "constant"):
		return languages.SymbolConstant
	case strings.Contains(kind, "property"):
		return languages.SymbolProperty
	case strings.Contains(kind, "field"):
		return languages.SymbolField
	case strings.Contains(kind, "variable"):
		return languages.SymbolVariable
	case strings.Contains(kind, "package"):
		return languages.SymbolPackage
	case strings.Contains(kind, "module"):
		return languages.SymbolModule
	default:
		return languages.SymbolVariable
	}
}
