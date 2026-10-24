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
//
// 早期直转 Canonical IR（Index/Document/Symbol/Occurrence/Metadata 及其
// ExportSCIP/ImportSCIP、ExportLSIF/ImportLSIF 适配）已按死码清理移除；
// 生产导入/导出走 model 层（ImportSCIPToModel/ImportLSIFToModel 与
// ExportSCIPSemanticScope/ExportLSIFFromSemantic）。本文件仅保留两套适配器
// 共用的 Range/Role 基础类型。
package interop

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
