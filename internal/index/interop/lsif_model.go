package interop

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
)

// ImportLSIFToModel converts an LSIF JSON Lines stream into the format-neutral
// semantic model, mirroring ImportSCIPToModel. It deliberately reports fact
// families the omnilsp LSIF subset cannot encode as incomplete/unknown rather
// than treating the format's omissions as negatives. Symbol records carry only
// the authoritative symbol string: LSIF has no display name, kind, or
// signature (declared in LossyNotes).
func ImportLSIFToModel(ctx context.Context, data []byte, req model.Request, sink model.Sink) (model.Report, error) {
	if sink == nil || req.View == nil || len(req.Scopes) != 1 {
		return model.Report{}, fmt.Errorf("%w: expected one scope, immutable view, and sink", errInvalidLSIF)
	}
	if len(data) == 0 {
		return model.Report{}, fmt.Errorf("%w: empty input", errInvalidLSIF)
	}
	parsed, err := parseLSIFStream(data)
	if err != nil {
		return model.Report{}, fmt.Errorf("%w: %v", errInvalidLSIF, err)
	}
	scope := req.Scopes[0]
	if err := validateLSIFSourceIdentity(parsed, req.View.Identity(), scope.RootURI); err != nil {
		return model.Report{}, err
	}
	// The declared positionEncoding governs how every range column is
	// interpreted (LSIF 0.5 requires the field; utf-8 and utf-32 are legal).
	// The old import ignored the declaration and read every column as
	// UTF-16, silently mis-decoding positions after any multi-byte character.
	positionEncoding, err := lsifStreamPositionEncoding(parsed)
	if err != nil {
		return model.Report{}, err
	}
	prov, ok := req.Provenance[scope.ID]
	if !ok || !reflect.DeepEqual(prov.Scope, scope) || prov.Identity != req.View.Identity() {
		return model.Report{}, fmt.Errorf("%w: request provenance mismatch", errInvalidLSIF)
	}
	if err := ctx.Err(); err != nil {
		return model.Report{}, err
	}
	files := make(map[string]model.File)
	if err := req.View.Walk(ctx, scope.RootURI, func(file model.File) error {
		if file.URI == "" || file.SHA256 == "" {
			return fmt.Errorf("%w: snapshot file missing URI or content hash", errInvalidLSIF)
		}
		if _, exists := files[file.URI]; exists {
			return fmt.Errorf("%w: duplicate snapshot URI %s", errInvalidLSIF, file.URI)
		}
		files[file.URI] = file
		return nil
	}); err != nil {
		return model.Report{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Report{}, err
	}

	type resolvedDoc struct {
		uri      string
		ranges   []model.Position
		symbols  []identity.SymbolID
		defRoles []string
	}
	var docs []resolvedDoc
	symbols := make(map[identity.SymbolID]model.Symbol)
	rangeRole := parsed.lsifRangeRoles()
	for _, id := range parsed.lsifVertexIDsAscending() {
		vertex := parsed.vertices[id]
		if vertex.Label != "document" {
			continue
		}
		if _, err := modelURIToSCIPPath(scope.RootURI, vertex.URI); err != nil {
			return model.Report{}, err
		}
		if !scipLanguageMatches(scope.Language, vertex.Lang) {
			return model.Report{}, fmt.Errorf("%w: LSIF document language %q does not match scope language %q", errInvalidLSIF, vertex.Lang, scope.Language)
		}
		doc := resolvedDoc{uri: vertex.URI}
		for _, rid := range parsed.docRangeIDs[id] {
			rv, ok := parsed.vertices[rid]
			if !ok || rv.Label != "range" || rv.Start == nil || rv.End == nil {
				return model.Report{}, fmt.Errorf("%w: document %q references missing or malformed range %d", errInvalidLSIF, vertex.URI, rid)
			}
			symbol := ""
			if rsID, ok := parsed.nextRangeToRS[rid]; ok {
				symbol = parsed.vertices[rsID].Symbol
			}
			if symbol == "" {
				return model.Report{}, fmt.Errorf("%w: occurrence without symbol (range %d)", errInvalidLSIF, rid)
			}
			role, ok := rangeRole[rid]
			if !ok {
				return model.Report{}, fmt.Errorf("%w: range %d has no item edge", errInvalidLSIF, rid)
			}
			if rv.Start.Line < 0 || rv.Start.Character < 0 || rv.End.Line < rv.Start.Line ||
				rv.End.Character < 0 || (rv.End.Line == rv.Start.Line && rv.End.Character < rv.Start.Character) {
				return model.Report{}, fmt.Errorf("%w: invalid occurrence range", errInvalidLSIF)
			}
			position := model.Position{
				StartLine: uint32(rv.Start.Line), StartChar: uint32(rv.Start.Character),
				EndLine: uint32(rv.End.Line), EndChar: uint32(rv.End.Character),
			}
			modelRole := "reference"
			if role == RoleDefinition {
				modelRole = "definition"
			}
			doc.ranges = append(doc.ranges, position)
			doc.symbols = append(doc.symbols, identity.SymbolID(symbol))
			doc.defRoles = append(doc.defRoles, modelRole)
			symbols[identity.SymbolID(symbol)] = model.Symbol{ID: identity.SymbolID(symbol), ScopeID: scope.ID}
		}
		docs = append(docs, doc)
	}

	report := model.Report{Identity: req.View.Identity(), UsedTools: map[string][]model.ToolIdentity{scope.ID: append([]model.ToolIdentity(nil), prov.Tools...)}}
	var emittedSymbols []model.Symbol
	symbolIDs := make([]string, 0, len(symbols))
	for id := range symbols {
		symbolIDs = append(symbolIDs, string(id))
	}
	sort.Strings(symbolIDs)
	for _, id := range symbolIDs {
		emittedSymbols = append(emittedSymbols, symbols[identity.SymbolID(id)])
		if len(emittedSymbols) == 2048 {
			if err := sink.WriteSymbols(ctx, emittedSymbols); err != nil {
				return model.Report{}, err
			}
			emittedSymbols = emittedSymbols[:0]
		}
	}
	if len(emittedSymbols) > 0 {
		if err := sink.WriteSymbols(ctx, emittedSymbols); err != nil {
			return model.Report{}, err
		}
	}

	for _, doc := range docs {
		if err := ctx.Err(); err != nil {
			return model.Report{}, err
		}
		file, exists := files[doc.uri]
		if !exists {
			return model.Report{}, fmt.Errorf("%w: LSIF document is outside the captured snapshot: %s", errInvalidLSIF, doc.uri)
		}
		if len(doc.ranges) > 0 {
			reader, readErr := req.View.Read(ctx, doc.uri)
			if readErr != nil {
				return model.Report{}, readErr
			}
			var rangeErr error
			switch positionEncoding {
			case "utf-8":
				doc.ranges, rangeErr = utf8RangesToUTF16(ctx, reader, doc.ranges)
			case "utf-32":
				doc.ranges, rangeErr = utf32RangesToUTF16(ctx, reader, doc.ranges)
			default: // "utf-16": columns are already UTF-16; validate bounds.
				rangeErr = validateUTF16Ranges(ctx, reader, doc.ranges)
			}
			closeErr := reader.Close()
			if rangeErr != nil {
				return model.Report{}, rangeErr
			}
			if closeErr != nil {
				return model.Report{}, closeErr
			}
		}
		batch := make([]model.Occurrence, 0, 2048)
		for i, position := range doc.ranges {
			batch = append(batch, model.Occurrence{
				SymbolID: doc.symbols[i], ScopeID: scope.ID, URI: doc.uri, Range: position,
				Role: doc.defRoles[i], SourceHash: file.SHA256, BuildContext: scope.BuildContext,
			})
			if len(batch) == cap(batch) {
				if err := sink.WriteOccurrences(ctx, batch); err != nil {
					return model.Report{}, err
				}
				batch = make([]model.Occurrence, 0, 2048)
			}
		}
		if len(batch) > 0 {
			if err := sink.WriteOccurrences(ctx, batch); err != nil {
				return model.Report{}, err
			}
		}
	}
	// omnilsp 的 LSIF 子集不编码任何模型关系边，这里没有可写的边家族；
	// 相关 coverage 在下方如实声明为 Unknown。

	for _, fact := range model.RequiredFactKinds {
		state, reason := model.Unknown, "LSIF does not encode this fact family with sufficient completeness evidence"
		switch fact {
		case model.FactSymbol, model.FactDefinition, model.FactReference:
			state, reason = model.IncompleteKnownSubset, "LSIF facts were imported, but format coverage does not prove completeness for the requested scope"
		case model.FactDeclaration:
			reason = "LSIF occurrence roles do not distinguish declaration completeness from definitions"
		}
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: state, Reason: reason})
	}
	if err := model.ValidateReport(req, report); err != nil {
		return model.Report{}, err
	}
	return report, nil
}

// lsifStreamPositionEncoding resolves the metaData positionEncoding
// declaration. LSIF 0.5 makes the field required and utf-8/utf-32 are legal
// values; a missing or unknown declaration is rejected instead of silently
// interpreted as UTF-16 — the same fail-closed policy the SCIP importer
// applies to TextDocumentEncoding.
func lsifStreamPositionEncoding(parsed lsifParsed) (string, error) {
	meta, ok := parsed.lsifMetaData()
	if !ok {
		return "", fmt.Errorf("%w: LSIF metaData vertex is required", errInvalidLSIF)
	}
	switch meta.PositionEncoding {
	case "utf-16", "utf-8", "utf-32":
		return meta.PositionEncoding, nil
	default:
		return "", fmt.Errorf("%w: unsupported LSIF positionEncoding %q", errInvalidLSIF, meta.PositionEncoding)
	}
}

// validateLSIFSourceIdentity mirrors validateSCIPSourceIdentity: the stream's
// metaData must bind to the captured workspace via projectRoot (scope root or
// repository identity) and may pin repository/revision through the
// omnilsp.repository=/omnilsp.commit= extensions and the commitId field.
func validateLSIFSourceIdentity(parsed lsifParsed, current model.Identity, rootURI string) error {
	meta, ok := parsed.lsifMetaData()
	if !ok {
		return fmt.Errorf("%w: LSIF metaData vertex is required", errInvalidLSIF)
	}
	projectRoot := meta.Root
	if projectRoot == "" {
		return fmt.Errorf("%w: LSIF projectRoot is required to bind documents to the captured workspace", errInvalidLSIF)
	}
	if projectRoot != rootURI && (current.Repository == "" || projectRoot != current.Repository) {
		return fmt.Errorf("%w: LSIF projectRoot does not match the captured repository", errInvalidLSIF)
	}
	var repository, revision string
	if meta.Tool != nil {
		for _, argument := range meta.Tool.Args {
			if value, ok := strings.CutPrefix(argument, scipRepositoryArgPrefix); ok {
				if repository != "" && repository != value {
					return fmt.Errorf("%w: conflicting LSIF repository identity arguments", errInvalidLSIF)
				}
				repository = value
			}
			if value, ok := strings.CutPrefix(argument, commitArgPrefix); ok {
				if revision != "" && revision != value {
					return fmt.Errorf("%w: conflicting LSIF revision arguments", errInvalidLSIF)
				}
				revision = value
			}
		}
	}
	if meta.Commit != "" {
		if revision != "" && revision != meta.Commit {
			return fmt.Errorf("%w: conflicting LSIF revision between commitId and toolInfo arguments", errInvalidLSIF)
		}
		revision = meta.Commit
	}
	if repository != "" && current.Repository != "" && repository != current.Repository {
		return fmt.Errorf("%w: LSIF repository identity does not match the captured snapshot", errInvalidLSIF)
	}
	if revision != "" && current.Revision != "" && revision != current.Revision {
		return fmt.Errorf("%w: LSIF revision does not match the captured snapshot", errInvalidLSIF)
	}
	return nil
}
