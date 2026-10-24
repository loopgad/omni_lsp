package interop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/semantic"
)

var errInvalidLSIF = errors.New("interop: invalid LSIF semantic index")

// SemanticLSIFExportLoss 与 SCIP 有损导出共用同一形状：被省略的边家族与数量。
type SemanticLSIFExportLoss = SemanticSCIPExportLoss

// lsifExportOcc 是 emitLSIF 的单条出现点输入。
type lsifExportOcc struct {
	symbol  string
	r       Range
	defined bool
}

// lsifExportDoc 是 emitLSIF 的单文档输入；URI 为绝对 file URI。
type lsifExportDoc struct {
	uri      string
	language string
	occs     []lsifExportOcc
}

// lsifExportInput 汇集一次 LSIF 导出的全部输入。emitLSIF 内部完成全部
// 排序与编号，保证同一输入逐位确定：
//   - 文档按 URI 升序，出现点按 (起行, 起列, 止行, 止列, 符号, 角色) 升序；
//   - metaData 携带 projectRoot/commitId/toolInfo（omnilsp.repository= 与
//     omnilsp.lossy-edge.* 计数），provenance 由此保留（§L11/L14）。
type lsifExportInput struct {
	projectRoot string
	repository  string
	revision    string
	toolVersion string
	lossArgs    []string
	docs        []lsifExportDoc
}

// emitLSIF 产出合法的 LSIF 0.5.x JSONL 字节流，形状与 Canonical ExportLSIF
// 一致（metaData/document/range/resultSet/referenceResult 顶点与
// contains/next/textDocument/references/item 边），另加 metaData.toolInfo。
func emitLSIF(in lsifExportInput) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	nextID := 1
	emit := func(l lsifLine) error {
		return enc.Encode(l)
	}

	meta := lsifVertex{
		ID: nextID, Label: "metaData",
		Version:          lsifVersion,
		PositionEncoding: "utf-16",
		ProjectRoot:      in.projectRoot,
		CommitID:         in.revision,
		ToolInfo:         &lsifToolInfo{Name: scipToolName, Version: in.toolVersion},
	}
	args := make([]string, 0, 1+len(in.lossArgs))
	if in.repository != "" {
		args = append(args, scipRepositoryArgPrefix+in.repository)
	}
	args = append(args, in.lossArgs...)
	if len(args) > 0 {
		meta.ToolInfo.Args = args
	}
	nextID++
	if err := emit(meta); err != nil {
		return nil, fmt.Errorf("lsif 导出 metaData: %w", err)
	}

	docs := append([]lsifExportDoc(nil), in.docs...)
	sort.Slice(docs, func(i, j int) bool { return docs[i].uri < docs[j].uri })
	for _, doc := range docs {
		docID := nextID
		nextID++
		if err := emit(lsifVertex{
			ID: docID, Label: "document",
			URI:        doc.uri,
			LanguageID: doc.language,
		}); err != nil {
			return nil, fmt.Errorf("lsif 导出 document %s: %w", doc.uri, err)
		}

		occs := append([]lsifExportOcc(nil), doc.occs...)
		sort.Slice(occs, func(i, j int) bool {
			a, b := occs[i], occs[j]
			if a.r.StartLine != b.r.StartLine {
				return a.r.StartLine < b.r.StartLine
			}
			if a.r.StartChar != b.r.StartChar {
				return a.r.StartChar < b.r.StartChar
			}
			if a.r.EndLine != b.r.EndLine {
				return a.r.EndLine < b.r.EndLine
			}
			if a.r.EndChar != b.r.EndChar {
				return a.r.EndChar < b.r.EndChar
			}
			if a.symbol != b.symbol {
				return a.symbol < b.symbol
			}
			return !a.defined && b.defined
		})

		type symOcc struct {
			rangeIDs []int
			defined  []bool
		}
		bySym := map[string]*symOcc{}
		var order []string
		allRangeIDs := make([]int, 0, len(occs))
		for _, occ := range occs {
			rid := nextID
			nextID++
			if err := emit(lsifVertex{
				ID: rid, Label: "range",
				Start: &lsifPosition{Line: occ.r.StartLine, Character: occ.r.StartChar},
				End:   &lsifPosition{Line: occ.r.EndLine, Character: occ.r.EndChar},
			}); err != nil {
				return nil, fmt.Errorf("lsif 导出 range: %w", err)
			}
			s, ok := bySym[occ.symbol]
			if !ok {
				s = &symOcc{}
				bySym[occ.symbol] = s
				order = append(order, occ.symbol)
			}
			s.rangeIDs = append(s.rangeIDs, rid)
			s.defined = append(s.defined, occ.defined)
			allRangeIDs = append(allRangeIDs, rid)
		}
		if len(allRangeIDs) > 0 {
			if err := emit(lsifEdge{ID: nextID, Label: "contains", OutV: docID, InVs: allRangeIDs}); err != nil {
				return nil, err
			}
			nextID++
		}
		for _, name := range order {
			s := bySym[name]
			rsID := nextID
			nextID++
			refID := nextID
			nextID++
			tdrID := nextID
			nextID++
			if err := emit(lsifVertex{ID: rsID, Label: "resultSet", Symbol: name}); err != nil {
				return nil, err
			}
			if err := emit(lsifVertex{ID: refID, Label: "referenceResult"}); err != nil {
				return nil, err
			}
			if err := emit(lsifEdge{ID: tdrID, Label: "textDocument/references", OutV: rsID, InV: refID}); err != nil {
				return nil, err
			}
			for i, rid := range s.rangeIDs {
				if err := emit(lsifEdge{ID: nextID, Label: "next", OutV: rid, InV: rsID}); err != nil {
					return nil, err
				}
				nextID++
				p := "referenceResults"
				if s.defined[i] {
					p = "definitions"
				}
				if err := emit(lsifEdge{ID: nextID, Label: "item", OutV: refID, InV: rid, Property: p}); err != nil {
					return nil, err
				}
				nextID++
			}
		}
	}
	return buf.Bytes(), nil
}

// ExportLSIFFromSemantic exports one verified, persistent semantic generation
// as LSIF JSON Lines. The reader is rewound and consumed to EOF. The export
// carries provenance (scope root, repository, revision, extractor version) in
// the metaData vertex and refuses semantic edge kinds the omnilsp LSIF subset
// cannot represent.
func ExportLSIFFromSemantic(ctx context.Context, reader *semantic.Reader) ([]byte, error) {
	if ctx == nil || reader == nil {
		return nil, fmt.Errorf("%w: nil context or semantic reader", errInvalidLSIF)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metadata := reader.Metadata()
	if len(metadata.Scopes) != 1 {
		return nil, fmt.Errorf("%w: LSIF export requires exactly one semantic scope", errInvalidLSIF)
	}
	return ExportLSIFSemanticScope(ctx, reader, metadata.Scopes[0].ID)
}

// ExportLSIFFromSemanticWithLosses exports representable LSIF facts and
// deliberately omits every semantic edge kind (the omnilsp LSIF subset encodes
// none). Omitted counts are returned to the caller and embedded in
// metaData.toolInfo.args. Callers must opt into this API explicitly.
func ExportLSIFFromSemanticWithLosses(ctx context.Context, reader *semantic.Reader) ([]byte, []SemanticLSIFExportLoss, error) {
	if ctx == nil || reader == nil {
		return nil, nil, fmt.Errorf("%w: nil context or semantic reader", errInvalidLSIF)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	metadata := reader.Metadata()
	if len(metadata.Scopes) != 1 {
		return nil, nil, fmt.Errorf("%w: LSIF export requires exactly one semantic scope", errInvalidLSIF)
	}
	losses := make([]SemanticLSIFExportLoss, 0)
	data, err := exportLSIFSemanticScope(ctx, reader, metadata.Scopes[0].ID, &losses)
	if err != nil {
		return nil, nil, err
	}
	return data, losses, nil
}

// ExportLSIFSemanticScope exports one scope from a verified multi-scope
// generation. Other scopes are ignored; records in the selected scope retain
// the same validation as ExportLSIFFromSemantic.
func ExportLSIFSemanticScope(ctx context.Context, reader *semantic.Reader, scopeID string) ([]byte, error) {
	return exportLSIFSemanticScope(ctx, reader, scopeID, nil)
}

// ExportLSIFSemanticScopeWithLosses 与严格版相同，但省略全部语义边并在
// metaData.toolInfo.args 记录各类数量。
func ExportLSIFSemanticScopeWithLosses(ctx context.Context, reader *semantic.Reader, scopeID string) ([]byte, []SemanticLSIFExportLoss, error) {
	losses := make([]SemanticLSIFExportLoss, 0)
	data, err := exportLSIFSemanticScope(ctx, reader, scopeID, &losses)
	if err != nil {
		return nil, nil, err
	}
	return data, losses, nil
}

func exportLSIFSemanticScope(ctx context.Context, reader *semantic.Reader, scopeID string, losses *[]SemanticLSIFExportLoss) ([]byte, error) {
	if ctx == nil || reader == nil || scopeID == "" {
		return nil, fmt.Errorf("%w: nil context, semantic reader, or scope ID", errInvalidLSIF)
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
		return nil, fmt.Errorf("%w: semantic scope %q is not present in the generation", errInvalidLSIF, scopeID)
	}
	if scope.ID == "" || scope.Language == "" || scope.RootURI == "" {
		return nil, fmt.Errorf("%w: invalid semantic scope", errInvalidLSIF)
	}
	if err := reader.Rewind(); err != nil {
		return nil, fmt.Errorf("%w: rewind semantic reader: %v", errInvalidLSIF, err)
	}

	toolVersion := ""
	if provenance, ok := metadata.Provenance[scope.ID]; ok {
		toolVersion = provenance.ExtractorVer
	}
	docs := make(map[string]*lsifExportDoc)
	ensureDocument := func(uri string) (*lsifExportDoc, error) {
		// 校验并规范化 URI：出现点 URI 必须落在 scope root 内且为合法 file URI。
		relative, err := modelURIToSCIPPath(scope.RootURI, uri)
		if err != nil {
			return nil, err
		}
		canonical, err := scipDocumentURI(scope.RootURI, relative)
		if err != nil {
			return nil, err
		}
		doc := docs[canonical]
		if doc == nil {
			doc = &lsifExportDoc{uri: canonical, language: scope.Language}
			docs[canonical] = doc
		}
		return doc, nil
	}
	lossCounts := make(map[model.EdgeKind]uint64)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: read semantic generation: %v", errInvalidLSIF, err)
		}
		switch batch.Kind {
		case semantic.BatchSymbols:
			// LSIF 子集只携带符号 ID 字符串（经出现点体现），符号记录本身
			// （显示名/kind/signature）无编码位，见 LossyNotes。
		case semantic.BatchOccurrences:
			for _, occurrence := range batch.Occurrences {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if occurrence.ScopeID != scope.ID {
					continue
				}
				doc, err := ensureDocument(occurrence.URI)
				if err != nil {
					return nil, err
				}
				defined := false
				switch occurrence.Role {
				case "reference":
				case "definition":
					defined = true
				default:
					return nil, fmt.Errorf("%w: unsupported semantic occurrence role %q", errInvalidLSIF, occurrence.Role)
				}
				position, err := semanticPosition(occurrence.Range)
				if err != nil {
					return nil, err
				}
				doc.occs = append(doc.occs, lsifExportOcc{
					symbol:  string(occurrence.SymbolID),
					r:       position,
					defined: defined,
				})
			}
		case semantic.BatchEdges:
			for _, edge := range batch.Edges {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if edge.ScopeID != scope.ID {
					continue
				}
				// omnilsp 的 LSIF 子集不编码任何模型边家族：严格导出直接拒绝，
				// 显式有损导出省略并计数（对齐 SCIP 的严格/有损双轨）。
				if losses == nil {
					return nil, fmt.Errorf("%w: LSIF cannot represent semantic edge kind %q", errInvalidLSIF, edge.Kind)
				}
				if _, err := modelURIToSCIPPath(scope.RootURI, edge.SourceURI); err != nil {
					return nil, err
				}
				lossCounts[edge.Kind]++
			}
		default:
			return nil, fmt.Errorf("%w: unsupported semantic batch kind %q", errInvalidLSIF, batch.Kind)
		}
	}

	input := lsifExportInput{
		projectRoot: scope.RootURI,
		repository:  string(metadata.Identity.Repository),
		revision:    metadata.Identity.Revision,
		toolVersion: toolVersion,
	}
	kinds := make([]string, 0, len(lossCounts))
	for kind := range lossCounts {
		kinds = append(kinds, string(kind))
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		count := lossCounts[model.EdgeKind(kind)]
		input.lossArgs = append(input.lossArgs, fmt.Sprintf("%s%s=%d", scipLossyEdgeArgPrefix, kind, count))
		if losses != nil {
			*losses = append(*losses, SemanticLSIFExportLoss{Kind: model.EdgeKind(kind), Count: count})
		}
	}
	uris := make([]string, 0, len(docs))
	for uri := range docs {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	for _, uri := range uris {
		input.docs = append(input.docs, *docs[uri])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return emitLSIF(input)
}

// semanticPosition 把模型 UTF-16 位置转成 LSIF Range；模型位置即 UTF-16 码元，
// 无需换算，仅校验 int32 可表示与区间有序。
func semanticPosition(position model.Position) (Range, error) {
	if position.StartLine > uint32(^uint32(0)>>1) || position.StartChar > uint32(^uint32(0)>>1) ||
		position.EndLine > uint32(^uint32(0)>>1) || position.EndChar > uint32(^uint32(0)>>1) ||
		position.EndLine < position.StartLine || (position.EndLine == position.StartLine && position.EndChar < position.StartChar) {
		return Range{}, fmt.Errorf("%w: semantic position cannot be represented by LSIF", errInvalidLSIF)
	}
	return Range{
		StartLine: int32(position.StartLine),
		StartChar: int32(position.StartChar),
		EndLine:   int32(position.EndLine),
		EndChar:   int32(position.EndChar),
	}, nil
}
