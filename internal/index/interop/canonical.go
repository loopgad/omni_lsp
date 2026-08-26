// Package interop 实现 Canonical IR 与 SCIP/LSIF 交换格式之间的纯导入导出
// 适配器（goal.md §L11/L14/L15）。
//
// Invariant:
//   - 本包是唯一允许依赖 SCIP 绑定库（github.com/scip-code/scip/bindings/go/scip）
//     的位置；核心 index/query 包对两种交换格式零感知（§L14：SCIP 不是内存模型）。
//   - Canonical IR 独立于任何 wire 格式；LSIF 仅作为兼容性导入导出存在，
//     不据此设计内部架构（§L15）。
//   - 从交换格式导入的产物必须携带 Provenance 与 FreshnessLimit（§L14）。
//   - 静态索引的 repo identity + commit identity 在双向转换中保留（§L11）。
//   - 有损映射统一记录于 LossyNotes()，禁止散落在各处注释里。
package interop

// Kind 是 Canonical 符号种类枚举，取 SCIP 与常见语言语义的交集。
type Kind int32

const (
	KindUnspecified Kind = iota
	KindInterface
	KindClass
	KindFunction
	KindMethod
	KindVariable
	KindConstant
)

// Role 区分定义与引用出现点。
type Role int8

const (
	RoleReference Role = iota
	RoleDefinition
)

// Range 是半开区间 [Start, End)，0 起始行列，与 LSP/SCIP/LSIF 对齐。
type Range struct {
	StartLine int32
	StartChar int32
	EndLine   int32
	EndChar   int32
}

// Document 是单个源文件的规范表示及其出现点列表。
type Document struct {
	Path        string
	LanguageID  string
	Occurrences []Occurrence
}

// Symbol 是规范符号记录。Name 是完整符号标识符字符串；
// 具体格式（如 SCIP scheme）不属于 Canonical IR 的语义。
type Symbol struct {
	Name      string
	Kind      Kind
	Signature string
}

// Occurrence 是文档内一次符号出现（定义或引用）。
type Occurrence struct {
	DocPath    string
	SymbolName string
	Range      Range
	Role       Role
}

// Metadata 携带 §L11 身份信息与 §L14 provenance/freshness。
// Provenance/FreshnessLimit 只存在于内存结构，不进任何 wire 格式。
type Metadata struct {
	RepoIdentity   string
	CommitID       string
	ToolVersion    string
	Provenance     string // 导入来源："scip"/"lsif"；空表示本进程内构造
	FreshnessLimit string // 静态数据新鲜度限制说明；导入时必填非空
}

// Index 是最小 Canonical IR。
type Index struct {
	Documents []Document
	Symbols   []Symbol
	Metadata  Metadata
}

// RawOccurrence 是 FromOccurrences 的轻量输入行。
type RawOccurrence struct {
	Name                                   string
	StartLine, StartChar, EndLine, EndChar int32
	Definition                             bool
}

// FromOccurrences 由单文档的原始出现点轻构造一个 Canonical Index：
// 文档唯一，符号按首现顺序去重，kind/signature 未标注为 Unspecified/空。
func FromOccurrences(docPath string, occs []RawOccurrence) Index {
	idx := Index{Documents: []Document{{Path: docPath}}}
	seen := map[string]bool{}
	for _, o := range occs {
		role := RoleReference
		if o.Definition {
			role = RoleDefinition
		}
		idx.Documents[0].Occurrences = append(idx.Documents[0].Occurrences, Occurrence{
			DocPath:    docPath,
			SymbolName: o.Name,
			Range: Range{
				StartLine: o.StartLine,
				StartChar: o.StartChar,
				EndLine:   o.EndLine,
				EndChar:   o.EndChar,
			},
			Role: role,
		})
		if !seen[o.Name] {
			seen[o.Name] = true
			idx.Symbols = append(idx.Symbols, Symbol{Name: o.Name})
		}
	}
	return idx
}
