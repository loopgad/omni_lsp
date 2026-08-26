package interop

// LossyNotes 返回已知的有损映射清单（goal.md §L14：有损映射 MUST 文档化）。
// 每条以「有损」开头，说明损失内容与原因；新增有损映射必须在此登记。
func LossyNotes() []string {
	return []string{
		"有损：SCIP Symbol 是完整字符串标识符（含 scheme/package 语义），Canonical Symbol.Name 仅保留字符串本身，scheme/version 结构不可恢复。",
		"有损：Symbol kind 枚举取 Canonical 与交换格式的交集，交集之外的 kind 双向映射为 Unspecified。",
		"有损：LSIF 不携带 kind/signature 概念，导入后 Symbol.Kind 恒为 Unspecified、Signature 为空。",
		"有损：provenance/freshness 不进任何 wire 格式，仅存在于 Canonical IR 内存结构中，导出后丢失、导入时重新标注为当前适配器来源。",
		"有损：LSIF 标准 schema 无 repo identity/commit 字段，通过 metaData 自定义扩展字段 projectRoot/commitId 传递，非 omnilsp 读取方会忽略它们。",
		"有损：SCIP 无 commit 字段，CommitID 通过 ToolInfo.Arguments 的 omnilsp.commit= 键值扩展传递，非 omnilsp 读取方会忽略它。",
		"有损：LSIF resultSet 上的 symbol 字段为自定义扩展（替代 moniker 顶点链），仅 omnilsp 读取方可恢复符号名。",
	}
}
