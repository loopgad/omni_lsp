package interop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// LSIF 0.5.x JSON Lines 解析与共享 wire 结构（goal.md §L15：兼容性支持，
// 不驱动内部架构）。导出侧走 lsif_semantic.go（emitLSIF），导入模型侧走
// lsif_model.go（ImportLSIFToModel）；早期的 Canonical 直转适配器
// ExportLSIF/ImportLSIF 已按死码清理移除。
//
// ponytail: resultSet 上的 "symbol"、metaData 上的 "projectRoot"/"commitId"
// 是自定义扩展字段，用于在最小标签集合内完成无损往返；标准读取方忽略未知字段。
// 需要严格标准形状时改走 moniker 顶点链。

// lsifVersion 是 metaData 顶点声明的 LSIF 版本。
const lsifVersion = "0.5.0"

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

	// metaData 顶点的 positionEncoding 声明（LSIF 0.5 必填；utf-8/utf-32 为
	// 合法值）：流内所有 range 列值的解释口径。缺失或未知值由导入入口
	// fail-closed 拒绝，不再默认按 UTF-16 解读。
	PositionEncoding string `json:"positionEncoding"`
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
