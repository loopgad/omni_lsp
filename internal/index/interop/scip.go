package interop

// SCIP wire 约定常量，供本包各 SCIP/LSIF 适配器共享：omnilsp toolInfo 名与
// omnilsp.repository=/omnilsp.commit= 键值扩展（有损映射见 LossyNotes）。
// Canonical Index ↔ SCIP protobuf 的直转适配器（ExportSCIP/ImportSCIP）已按
// 死码清理移除；生产路径走 ImportSCIPToModel（scip_model.go）与
// ExportSCIPSemanticScope（scip_semantic_export.go）。
const (
	scipToolName            = "omnilsp"
	commitArgPrefix         = "omnilsp.commit="
	scipRepositoryArgPrefix = "omnilsp.repository="
)
