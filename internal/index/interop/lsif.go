package interop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// LSIF 0.5.x JSON Lines 导出/导入（goal.md §L15：兼容性支持，不驱动内部架构）。
//
// ponytail: resultSet 上的 "symbol"、metaData 上的 "projectRoot"/"commitId"
// 是自定义扩展字段，用于在最小标签集合内完成无损往返；标准读取方忽略未知字段。
// 需要严格标准形状时改走 moniker 顶点链。

const (
	lsifVersion       = "0.5.0"
	lsifProvenance    = "lsif"
	lsifFreshnessNote = "静态索引快照：数据可能已过时，不反映工作区当前状态（LSIF 导入）"
)

// lsifToolInfo 覆盖 LSIF metaData 顶点的标准 toolInfo 字段。
// args 沿用 omnilsp 键值扩展约定（omnilsp.repository= 等），与 SCIP 对齐。
type lsifToolInfo struct {
	Name    string   `json:"name,omitempty"`
	Version string   `json:"version,omitempty"`
	Args    []string `json:"args,omitempty"`
}

type lsifPosition struct {
	Line      int32 `json:"line"`
	Character int32 `json:"character"`
}

// lsifVertex 覆盖 metaData/document/range/resultSet/referenceResult 五种顶点。
type lsifVertex struct {
	ID    int           `json:"id"`
	Label string        `json:"label"`
	Start *lsifPosition `json:"start,omitempty"`
	End   *lsifPosition `json:"end,omitempty"`

	Version          string        `json:"version,omitempty"`
	PositionEncoding string        `json:"positionEncoding,omitempty"`
	ProjectRoot      string        `json:"projectRoot,omitempty"`
	CommitID         string        `json:"commitId,omitempty"` // 扩展字段，见文件头注释
	ToolInfo         *lsifToolInfo `json:"toolInfo,omitempty"`
	URI              string        `json:"uri,omitempty"`
	LanguageID       string        `json:"languageId,omitempty"`
	Symbol           string        `json:"symbol,omitempty"` // resultSet 扩展字段
}

// lsifEdge 覆盖 contains/next/textDocument/references/item 五种边。
type lsifEdge struct {
	ID       int    `json:"id"`
	Label    string `json:"label"`
	OutV     int    `json:"outV"`
	InV      int    `json:"inV,omitempty"`
	InVs     []int  `json:"inVs,omitempty"`
	Property string `json:"property,omitempty"`
}

type lsifLine interface{ lineID() int }

func (v lsifVertex) lineID() int { return v.ID }
func (e lsifEdge) lineID() int   { return e.ID }

// ExportLSIF 产出合法的 LSIF 0.5.x JSON Lines 字节流。
// 结构正确性优先于完备性：仅使用 metaData/document/range/resultSet/
// referenceResult 顶点与 contains/next/textDocument/references/item 边。
func ExportLSIF(idx Index) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	nextID := 1
	emit := func(l lsifLine) error {
		if err := enc.Encode(l); err != nil {
			return err
		}
		return nil
	}

	meta := lsifVertex{
		ID: nextID, Label: "metaData",
		Version:          lsifVersion,
		PositionEncoding: "utf-16",
		ProjectRoot:      idx.Metadata.RepoIdentity,
		CommitID:         idx.Metadata.CommitID,
	}
	nextID++
	if err := emit(meta); err != nil {
		return nil, fmt.Errorf("lsif 导出 metaData: %w", err)
	}

	for _, doc := range idx.Documents {
		docID := nextID
		nextID++
		if err := emit(lsifVertex{
			ID: docID, Label: "document",
			URI:        "file:///" + strings.TrimPrefix(doc.Path, "/"),
			LanguageID: doc.LanguageID,
		}); err != nil {
			return nil, fmt.Errorf("lsif 导出 document %s: %w", doc.Path, err)
		}

		type symOcc struct {
			rangeIDs []int
			defined  []bool
		}
		bySym := map[string]*symOcc{}
		var order []string
		allRangeIDs := make([]int, 0, len(doc.Occurrences))
		for _, occ := range doc.Occurrences {
			rid := nextID
			nextID++
			sl, sc := occ.Range.StartLine, occ.Range.StartChar
			el, ec := occ.Range.EndLine, occ.Range.EndChar
			if err := emit(lsifVertex{
				ID: rid, Label: "range",
				Start: &lsifPosition{Line: sl, Character: sc},
				End:   &lsifPosition{Line: el, Character: ec},
			}); err != nil {
				return nil, err
			}
			s, ok := bySym[occ.SymbolName]
			if !ok {
				s = &symOcc{}
				bySym[occ.SymbolName] = s
				order = append(order, occ.SymbolName)
			}
			s.rangeIDs = append(s.rangeIDs, rid)
			s.defined = append(s.defined, occ.Role == RoleDefinition)
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

// lsifNodeRaw 是导入侧的宽容联合结构：顶点与边共用。
type lsifNodeRaw struct {
	ID     int           `json:"id"`
	Label  string        `json:"label"`
	OutV   int           `json:"outV"`
	InV    int           `json:"inV"`
	InVs   []int         `json:"inVs"`
	Prop   string        `json:"property"`
	Start  *lsifPosition `json:"start"`
	End    *lsifPosition `json:"end"`
	URI    string        `json:"uri"`
	Lang   string        `json:"languageId"`
	Symbol string        `json:"symbol"`
	Root   string        `json:"projectRoot"`
	Commit string        `json:"commitId"`
	Tool   *lsifToolInfo `json:"toolInfo"`
}

// lsifItemEdge 是一条 item 边的精简记录（range→role 归属）。
type lsifItemEdge struct {
	outV, inV int
	prop      string
}

// lsifParsed 是 parseLSIFStream 的产物：顶点与三类边索引。
type lsifParsed struct {
	vertices      map[int]lsifNodeRaw
	items         []lsifItemEdge
	nextRangeToRS map[int]int // range id -> resultSet id
	docRangeIDs   map[int][]int
}

// parseLSIFStream 解析 LSIF JSONL 为宽容结构。空行跳过；解析错误带行号。
func parseLSIFStream(data []byte) (lsifParsed, error) {
	parsed := lsifParsed{
		vertices:      map[int]lsifNodeRaw{},
		nextRangeToRS: map[int]int{},
		docRangeIDs:   map[int][]int{},
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var n lsifNodeRaw
		if err := json.Unmarshal(line, &n); err != nil {
			return lsifParsed{}, fmt.Errorf("lsif 第 %d 行解析失败: %w", lineNo, err)
		}
		switch n.Label {
		case "contains":
			parsed.docRangeIDs[n.OutV] = append(parsed.docRangeIDs[n.OutV], n.InVs...)
		case "next":
			parsed.nextRangeToRS[n.OutV] = n.InV
		case "item":
			parsed.items = append(parsed.items, lsifItemEdge{outV: n.OutV, inV: n.InV, prop: n.Prop})
		default:
			parsed.vertices[n.ID] = n
		}
	}
	if err := sc.Err(); err != nil {
		return lsifParsed{}, err
	}
	return parsed, nil
}

// lsifVertexIDsAscending 返回顶点 id 升序切片，保证消费顺序可复现。
func (p lsifParsed) lsifVertexIDsAscending() []int {
	ids := make([]int, 0, len(p.vertices))
	for id := range p.vertices {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// lsifRangeRoles 依 item 边推导 range 的定义/引用角色；无 item 边的 range 记为引用。
func (p lsifParsed) lsifRangeRoles() map[int]Role {
	rangeRole := map[int]Role{}
	for _, it := range p.items {
		if it.prop == "definitions" {
			rangeRole[it.inV] = RoleDefinition
		} else {
			rangeRole[it.inV] = RoleReference
		}
	}
	return rangeRole
}

// lsifMetaData 返回 id 最小的 metaData 顶点（无则零值+false）。
func (p lsifParsed) lsifMetaData() (lsifNodeRaw, bool) {
	for _, id := range p.lsifVertexIDsAscending() {
		if v := p.vertices[id]; v.Label == "metaData" {
			return v, true
		}
	}
	return lsifNodeRaw{}, false
}

// ImportLSIF 解析 LSIF JSONL 并映射为 Canonical IR。
// 导入产物标记 Provenance="lsif" 与新鲜度限制（§L14）；
// LSIF 不携带 kind/signature，导入后恒为 Unspecified/空（见 LossyNotes）。
// 仓库身份优先取 metaData.toolInfo.args 的 omnilsp.repository= 扩展，
// 否则回落到 metaData.projectRoot；commit 取 commitId 扩展字段，
// 缺失时回落 omnilsp.commit= 扩展参数（与 SCIP 导入形态对齐）。
func ImportLSIF(data []byte) (Index, error) {
	parsed, err := parseLSIFStream(data)
	if err != nil {
		return Index{}, err
	}

	idx := Index{Metadata: Metadata{
		Provenance:     lsifProvenance,
		FreshnessLimit: lsifFreshnessNote,
	}}
	if meta, ok := parsed.lsifMetaData(); ok {
		idx.Metadata.RepoIdentity = meta.Root
		idx.Metadata.CommitID = meta.Commit
		if meta.Tool != nil {
			idx.Metadata.ToolVersion = meta.Tool.Version
			for _, a := range meta.Tool.Args {
				if v, ok := strings.CutPrefix(a, scipRepositoryArgPrefix); ok {
					idx.Metadata.RepoIdentity = v
				}
				if v, ok := strings.CutPrefix(a, commitArgPrefix); ok && idx.Metadata.CommitID == "" {
					idx.Metadata.CommitID = v
				}
			}
		}
	}
	ids := parsed.lsifVertexIDsAscending()
	rangeRole := parsed.lsifRangeRoles()

	seenSyms := map[string]bool{}
	for _, id := range ids {
		v := parsed.vertices[id]
		if v.Label != "document" {
			continue
		}
		path := strings.TrimPrefix(v.URI, "file:///")
		d := Document{Path: path, LanguageID: v.Lang}
		for _, rid := range parsed.docRangeIDs[v.ID] {
			rv := parsed.vertices[rid]
			if rv.Label != "range" || rv.Start == nil || rv.End == nil {
				continue
			}
			name := ""
			if rsID, ok := parsed.nextRangeToRS[rid]; ok {
				name = parsed.vertices[rsID].Symbol
			}
			d.Occurrences = append(d.Occurrences, Occurrence{
				DocPath:    path,
				SymbolName: name,
				Range: Range{
					StartLine: rv.Start.Line,
					StartChar: rv.Start.Character,
					EndLine:   rv.End.Line,
					EndChar:   rv.End.Character,
				},
				Role: rangeRole[rid],
			})
			if name != "" && !seenSyms[name] {
				seenSyms[name] = true
				idx.Symbols = append(idx.Symbols, Symbol{Name: name})
			}
		}
		idx.Documents = append(idx.Documents, d)
	}
	return idx, nil
}
