package interop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

const (
	maxSCIPRelativePathBytes = 1 << 20
	maxSCIPStreamBytes       = 64 << 20
	scipLossyEdgeArgPrefix   = "omnilsp.lossy-edge."
)

// SemanticSCIPExportLoss records a semantic edge family omitted from an
// explicitly lossy SCIP export. Counts are stored in the SCIP ToolInfo as
// well, so the artifact carries its own loss declaration.
type SemanticSCIPExportLoss struct {
	Kind  model.EdgeKind `json:"kind"`
	Count uint64         `json:"count"`
}

// ExportSCIPFromSemantic exports one verified, persistent semantic generation
// as a SCIP protobuf stream. The reader is rewound and consumed to EOF.
func ExportSCIPFromSemantic(ctx context.Context, reader *semantic.Reader) ([]byte, error) {
	if ctx == nil || reader == nil {
		return nil, fmt.Errorf("%w: nil context or semantic reader", errInvalidSCIP)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metadata := reader.Metadata()
	if len(metadata.Scopes) != 1 {
		return nil, fmt.Errorf("%w: SCIP export requires exactly one semantic scope", errInvalidSCIP)
	}
	return ExportSCIPSemanticScope(ctx, reader, metadata.Scopes[0].ID)
}

// ExportSCIPSemanticScope exports one scope from a verified multi-scope
// generation. Other scopes are ignored; records in the selected scope retain
// the same validation and size limits as ExportSCIPFromSemantic.
func ExportSCIPSemanticScope(ctx context.Context, reader *semantic.Reader, scopeID string) ([]byte, error) {
	return exportSCIPSemanticScope(ctx, reader, scopeID, nil)
}

// ExportSCIPSemanticScopeWithLosses exports representable SCIP facts and
// deliberately omits only edge kinds SCIP cannot encode. The omitted counts
// are returned to the caller and embedded in ToolInfo.Arguments. Callers must
// opt into this API explicitly; ExportSCIPSemanticScope remains strict.
func ExportSCIPSemanticScopeWithLosses(ctx context.Context, reader *semantic.Reader, scopeID string) ([]byte, []SemanticSCIPExportLoss, error) {
	losses := make([]SemanticSCIPExportLoss, 0)
	data, err := exportSCIPSemanticScope(ctx, reader, scopeID, &losses)
	if err != nil {
		return nil, nil, err
	}
	return data, losses, nil
}

func exportSCIPSemanticScope(ctx context.Context, reader *semantic.Reader, scopeID string, losses *[]SemanticSCIPExportLoss) ([]byte, error) {
	if ctx == nil || reader == nil || scopeID == "" {
		return nil, fmt.Errorf("%w: nil context, semantic reader, or scope ID", errInvalidSCIP)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metadata := reader.Metadata()
	var scope model.Scope
	foundScope := false
	for _, candidate := range metadata.Scopes {
		if candidate.ID == scopeID {
			scope = candidate
			foundScope = true
			break
		}
	}
	if !foundScope {
		return nil, fmt.Errorf("%w: semantic scope %q is not present in the generation", errInvalidSCIP, scopeID)
	}
	if scope.ID == "" || scope.Language == "" || scope.RootURI == "" {
		return nil, fmt.Errorf("%w: invalid semantic scope", errInvalidSCIP)
	}
	if err := reader.Rewind(); err != nil {
		return nil, fmt.Errorf("%w: rewind semantic reader: %v", errInvalidSCIP, err)
	}

	index := &scip.Index{Metadata: &scip.Metadata{
		ProjectRoot:          scope.RootURI,
		TextDocumentEncoding: scip.TextEncoding_UTF16,
		ToolInfo:             &scip.ToolInfo{Name: scipToolName},
	}}
	if provenance, ok := metadata.Provenance[scope.ID]; ok {
		index.Metadata.ToolInfo.Version = provenance.ExtractorVer
	}
	if metadata.Identity.Repository != "" {
		index.Metadata.ToolInfo.Arguments = append(index.Metadata.ToolInfo.Arguments, scipRepositoryArgPrefix+string(metadata.Identity.Repository))
	}
	if metadata.Identity.Revision != "" {
		index.Metadata.ToolInfo.Arguments = append(index.Metadata.ToolInfo.Arguments, commitArgPrefix+metadata.Identity.Revision)
	}
	estimatedBytes := proto.Size(index.Metadata) + 32
	reserveBytes := func(size int) error {
		var err error
		estimatedBytes, err = reserveSCIPBytes(estimatedBytes, size)
		if err != nil {
			return err
		}
		return nil
	}

	symbols := make(map[identity.SymbolID]model.Symbol)
	definitionURI := make(map[identity.SymbolID]string)
	documents := make(map[string]*scip.Document)
	documentSymbols := make(map[string]map[identity.SymbolID]*scip.SymbolInformation)
	lossCounts := make(map[model.EdgeKind]uint64)
	ensureDocument := func(uri string) (*scip.Document, string, error) {
		relative, err := modelURIToSCIPPath(scope.RootURI, uri)
		if err != nil {
			return nil, "", err
		}
		doc := documents[relative]
		if doc == nil {
			if err := reserveBytes(len(relative) + len(scope.Language) + 64); err != nil {
				return nil, "", err
			}
			doc = &scip.Document{RelativePath: relative, Language: scope.Language}
			documents[relative] = doc
			documentSymbols[relative] = make(map[identity.SymbolID]*scip.SymbolInformation)
		}
		return doc, relative, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: read semantic generation: %v", errInvalidSCIP, err)
		}
		switch batch.Kind {
		case semantic.BatchSymbols:
			for _, symbol := range batch.Symbols {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if symbol.ScopeID != scope.ID {
					continue
				}
				if symbol.ID == "" {
					return nil, fmt.Errorf("%w: symbol is outside the exported scope or has no identity", errInvalidSCIP)
				}
				if previous, exists := symbols[symbol.ID]; exists && previous != symbol {
					return nil, fmt.Errorf("%w: conflicting semantic symbol records for %q", errInvalidSCIP, symbol.ID)
				}
				if _, exists := symbols[symbol.ID]; !exists {
					if err := reserveBytes(proto.Size(scipSymbolInformation(symbol, scope.Language)) + 8); err != nil {
						return nil, err
					}
				}
				symbols[symbol.ID] = symbol
			}
		case semantic.BatchOccurrences:
			for _, occurrence := range batch.Occurrences {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if occurrence.ScopeID != scope.ID {
					continue
				}
				doc, relative, err := ensureDocument(occurrence.URI)
				if err != nil {
					return nil, err
				}
				role := int32(0)
				switch occurrence.Role {
				case "reference":
				case "definition":
					role = int32(scip.SymbolRole_Definition)
					if previous, exists := definitionURI[occurrence.SymbolID]; !exists || relative < previous {
						definitionURI[occurrence.SymbolID] = relative
					}
				default:
					return nil, fmt.Errorf("%w: unsupported semantic occurrence role %q", errInvalidSCIP, occurrence.Role)
				}
				position, err := scipPosition(occurrence.Range)
				if err != nil {
					return nil, err
				}
				wireOccurrence := &scip.Occurrence{
					Symbol:      string(occurrence.SymbolID),
					SymbolRoles: role,
					Range:       position,
				}
				if err := reserveBytes(proto.Size(wireOccurrence) + 8); err != nil {
					return nil, err
				}
				doc.Occurrences = append(doc.Occurrences, wireOccurrence)
			}
		case semantic.BatchEdges:
			for _, edge := range batch.Edges {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if edge.ScopeID != scope.ID {
					continue
				}
				kind := model.EdgeKind("")
				switch edge.Kind {
				case model.EdgeImplementation, model.EdgeTypeRelation:
					kind = edge.Kind
				default:
					if losses == nil || !scipLossyEdgeKind(edge.Kind) {
						return nil, fmt.Errorf("%w: SCIP cannot represent semantic edge kind %q", errInvalidSCIP, edge.Kind)
					}
					if _, err := modelURIToSCIPPath(scope.RootURI, edge.SourceURI); err != nil {
						return nil, err
					}
					lossCounts[edge.Kind]++
					continue
				}
				_, relative, err := ensureDocument(edge.SourceURI)
				if err != nil {
					return nil, err
				}
				info := documentSymbols[relative][edge.From]
				if info == nil {
					info = &scip.SymbolInformation{Symbol: string(edge.From)}
					documentSymbols[relative][edge.From] = info
				}
				relationship := &scip.Relationship{Symbol: string(edge.To)}
				if kind == model.EdgeImplementation {
					relationship.IsImplementation = true
				} else {
					relationship.IsTypeDefinition = true
				}
				if err := reserveBytes(proto.Size(relationship) + 8); err != nil {
					return nil, err
				}
				info.Relationships = append(info.Relationships, relationship)
			}
		default:
			return nil, fmt.Errorf("%w: unsupported semantic batch kind %q", errInvalidSCIP, batch.Kind)
		}
	}

	for id, relative := range definitionURI {
		if _, ok := symbols[id]; !ok {
			return nil, fmt.Errorf("%w: definition references missing semantic symbol %q", errInvalidSCIP, id)
		}
		if documents[relative] == nil {
			return nil, fmt.Errorf("%w: definition has no SCIP document", errInvalidSCIP)
		}
	}

	symbolIDs := make([]string, 0, len(symbols))
	for id := range symbols {
		symbolIDs = append(symbolIDs, string(id))
	}
	sort.Strings(symbolIDs)
	for _, rawID := range symbolIDs {
		id := identity.SymbolID(rawID)
		info := scipSymbolInformation(symbols[id], scope.Language)
		if relative, hasDefinition := definitionURI[id]; hasDefinition {
			if existing := documentSymbols[relative][id]; existing != nil {
				mergeSCIPSymbolInformation(existing, info)
			} else {
				documentSymbols[relative][id] = info
			}
		} else {
			index.ExternalSymbols = append(index.ExternalSymbols, info)
		}
	}

	for relative, infos := range documentSymbols {
		for id, info := range infos {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			symbol, exists := symbols[id]
			if !exists {
				return nil, fmt.Errorf("%w: edge source symbol %q is missing", errInvalidSCIP, id)
			}
			for _, relationship := range info.Relationships {
				if _, exists := symbols[identity.SymbolID(relationship.GetSymbol())]; !exists {
					return nil, fmt.Errorf("%w: edge target symbol %q is missing", errInvalidSCIP, relationship.GetSymbol())
				}
			}
			primaryRelative, hasDefinition := definitionURI[id]
			if !hasDefinition || relative != primaryRelative {
				if err := reserveBytes(proto.Size(scipSymbolInformation(symbol, scope.Language)) + 8); err != nil {
					return nil, err
				}
			}
			mergeSCIPSymbolInformation(info, scipSymbolInformation(symbol, scope.Language))
		}
	}

	paths := make([]string, 0, len(documents))
	for relative, doc := range documents {
		for _, info := range documentSymbols[relative] {
			doc.Symbols = append(doc.Symbols, info)
		}
		sort.Slice(doc.Symbols, func(i, j int) bool { return doc.Symbols[i].GetSymbol() < doc.Symbols[j].GetSymbol() })
		sort.Slice(doc.Occurrences, func(i, j int) bool {
			a, b := doc.Occurrences[i], doc.Occurrences[j]
			if a.GetRange()[0] != b.GetRange()[0] {
				return a.GetRange()[0] < b.GetRange()[0]
			}
			if a.GetRange()[1] != b.GetRange()[1] {
				return a.GetRange()[1] < b.GetRange()[1]
			}
			return a.GetSymbol() < b.GetSymbol()
		})
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	for _, relative := range paths {
		index.Documents = append(index.Documents, documents[relative])
	}

	if losses != nil {
		kinds := make([]string, 0, len(lossCounts))
		for kind := range lossCounts {
			kinds = append(kinds, string(kind))
		}
		sort.Strings(kinds)
		for _, rawKind := range kinds {
			kind := model.EdgeKind(rawKind)
			count := lossCounts[kind]
			argument := fmt.Sprintf("%s%s=%d", scipLossyEdgeArgPrefix, kind, count)
			if err := reserveBytes(proto.Size(&scip.ToolInfo{Arguments: []string{argument}})); err != nil {
				return nil, err
			}
			index.Metadata.ToolInfo.Arguments = append(index.Metadata.ToolInfo.Arguments, argument)
			*losses = append(*losses, SemanticSCIPExportLoss{Kind: kind, Count: count})
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size := proto.Size(index); size > maxSCIPStreamBytes {
		return nil, fmt.Errorf("%w: SCIP export is %d bytes, above the %d MiB encoded-size budget", errInvalidSCIP, size, maxSCIPStreamBytes>>20)
	}
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("%w: encode SCIP: %v", errInvalidSCIP, err)
	}
	return data, nil
}

func scipLossyEdgeKind(kind model.EdgeKind) bool {
	switch kind {
	case model.EdgeCall, model.EdgeImport, model.EdgeInclude, model.EdgeModule, model.EdgeGenerated:
		return true
	default:
		return false
	}
}

func mergeSCIPSymbolInformation(dst, src *scip.SymbolInformation) {
	dst.DisplayName = src.GetDisplayName()
	dst.Kind = src.GetKind()
	dst.SignatureDocumentation = src.GetSignatureDocumentation()
}

func reserveSCIPBytes(current, size int) (int, error) {
	if current < 0 || size < 0 || current > maxSCIPStreamBytes || size > maxSCIPStreamBytes-current {
		return current, fmt.Errorf("%w: SCIP export exceeds the %d MiB encoded-size budget", errInvalidSCIP, maxSCIPStreamBytes>>20)
	}
	return current + size, nil
}

func modelURIToSCIPPath(rootURI, sourceURI string) (string, error) {
	root, err := url.Parse(rootURI)
	if err != nil || root.Scheme != "file" || root.Opaque != "" || root.RawQuery != "" || root.Fragment != "" || root.User != nil || root.RawPath != "" {
		return "", fmt.Errorf("%w: invalid file workspace URI %q", errInvalidSCIP, rootURI)
	}
	source, err := url.Parse(sourceURI)
	if err != nil || source.Scheme != "file" || source.Opaque != "" || source.RawQuery != "" || source.Fragment != "" || source.User != nil ||
		!strings.EqualFold(root.Host, source.Host) || source.RawPath != "" {
		return "", fmt.Errorf("%w: invalid source file URI %q", errInvalidSCIP, sourceURI)
	}
	rootPath := path.Clean(root.Path)
	sourcePath := path.Clean(source.Path)
	if rootPath != root.Path || sourcePath != source.Path || rootPath == "." || sourcePath == "." || strings.ContainsAny(sourcePath, "\\\x00") {
		return "", fmt.Errorf("%w: non-canonical source path %q", errInvalidSCIP, sourceURI)
	}
	if rootPath != "/" && !strings.HasSuffix(rootPath, "/") {
		rootPath += "/"
	}
	if !strings.HasPrefix(sourcePath, rootPath) {
		return "", fmt.Errorf("%w: source URI escapes the semantic scope: %q", errInvalidSCIP, sourceURI)
	}
	relative := strings.TrimPrefix(sourcePath, rootPath)
	if relative == "" || path.IsAbs(relative) || path.Clean(relative) != relative || relative == "." ||
		strings.HasPrefix(relative, "../") || strings.Contains(relative, ":") || len(relative) > maxSCIPRelativePathBytes {
		return "", fmt.Errorf("%w: unsafe or oversized source path %q", errInvalidSCIP, sourceURI)
	}
	return relative, nil
}

func scipPosition(position model.Position) ([]int32, error) {
	if position.StartLine > uint32(^uint32(0)>>1) || position.StartChar > uint32(^uint32(0)>>1) ||
		position.EndLine > uint32(^uint32(0)>>1) || position.EndChar > uint32(^uint32(0)>>1) ||
		position.EndLine < position.StartLine || (position.EndLine == position.StartLine && position.EndChar < position.StartChar) {
		return nil, fmt.Errorf("%w: semantic position cannot be represented by SCIP", errInvalidSCIP)
	}
	return []int32{int32(position.StartLine), int32(position.StartChar), int32(position.EndLine), int32(position.EndChar)}, nil
}

func scipSymbolInformation(symbol model.Symbol, language string) *scip.SymbolInformation {
	info := &scip.SymbolInformation{
		Symbol:      string(symbol.ID),
		DisplayName: symbol.Name,
		Kind:        scipKindFromModel(symbol.Kind),
	}
	if symbol.Signature != "" {
		info.SignatureDocumentation = &scip.Signature{Language: language, Text: symbol.Signature}
	}
	return info
}

func scipKindFromModel(kind string) scip.SymbolInformation_Kind {
	switch strings.ToLower(kind) {
	case "interface":
		return scip.SymbolInformation_Interface
	case "class", "struct":
		return scip.SymbolInformation_Class
	case "function":
		return scip.SymbolInformation_Function
	case "method":
		return scip.SymbolInformation_Method
	case "variable", "field":
		return scip.SymbolInformation_Variable
	case "constant":
		return scip.SymbolInformation_Constant
	default:
		return scip.SymbolInformation_UnspecifiedKind
	}
}
