package interop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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

	Version          string `json:"version,omitempty"`
	PositionEncoding string `json:"positionEncoding,omitempty"`
	ProjectRoot      string `json:"projectRoot,omitempty"`
	CommitID         string `json:"commitId,omitempty"` // 扩展字段，见文件头注释
	URI              string `json:"uri,omitempty"`
	LanguageID       string `json:"languageId,omitempty"`
	Symbol           string `json:"symbol,omitempty"` // resultSet 扩展字段
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
}

// ImportLSIF 解析 LSIF JSONL 并映射为 Canonical IR。
// 导入产物标记 Provenance="lsif" 与新鲜度限制（§L14）；
// LSIF 不携带 kind/signature，导入后恒为 Unspecified/空（见 LossyNotes）。
func ImportLSIF(data []byte) (Index, error) {
	vertices := map[int]lsifNodeRaw{}
	type itemEdge struct {
		outV, inV int
		prop      string
	}
	var items []itemEdge
	nextRangeToRS := map[int]int{} // range id -> resultSet id
	docRangeIDs := map[int][]int{} // document id -> range ids

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
			return Index{}, fmt.Errorf("lsif 第 %d 行解析失败: %w", lineNo, err)
		}
		switch n.Label {
		case "contains":
			docRangeIDs[n.OutV] = append(docRangeIDs[n.OutV], n.InVs...)
		case "next":
			nextRangeToRS[n.OutV] = n.InV
		case "item":
			items = append(items, itemEdge{outV: n.OutV, inV: n.InV, prop: n.Prop})
		default:
			vertices[n.ID] = n
		}
	}
	if err := sc.Err(); err != nil {
		return Index{}, err
	}

	rangeRole := map[int]Role{}
	for _, it := range items {
		if it.prop == "definitions" {
			rangeRole[it.inV] = RoleDefinition
		} else {
			rangeRole[it.inV] = RoleReference
		}
	}

	idx := Index{Metadata: Metadata{
		Provenance:     lsifProvenance,
		FreshnessLimit: lsifFreshnessNote,
	}}
	seenSyms := map[string]bool{}
	for _, v := range vertices {
		if v.Label != "metaData" {
			continue
		}
		idx.Metadata.RepoIdentity = v.Root
		idx.Metadata.CommitID = v.Commit
		break
	}
	for _, v := range vertices {
		if v.Label != "document" {
			continue
		}
		path := strings.TrimPrefix(v.URI, "file:///")
		d := Document{Path: path, LanguageID: v.Lang}
		for _, rid := range docRangeIDs[v.ID] {
			rv := vertices[rid]
			if rv.Label != "range" || rv.Start == nil || rv.End == nil {
				continue
			}
			name := ""
			if rsID, ok := nextRangeToRS[rid]; ok {
				name = vertices[rsID].Symbol
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
