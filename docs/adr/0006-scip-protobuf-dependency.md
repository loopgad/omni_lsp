# ADR-0006: 引入 SCIP 官方 Go 绑定作为互操作适配器依赖

## Status

Accepted

## Context

goal.md §L14 要求 Canonical IR 与 SCIP 之间提供纯导入导出适配器。SCIP 的
wire 格式是 protobuf（scip.proto），手写编解码等于把 proto 子集复制进仓库，
上游演进后静默漂移且无测试对齐。U5 依赖白名单
（internal/conformance/meta_test.go allowedDeps）因此需要扩充。

一个现实约束：SCIP 项目已从 github.com/sourcegraph/scip 迁移到
github.com/scip-code/scip，且自 v0.7.0 起把 Go 绑定拆为独立发布的子模块
`github.com/scip-code/scip/bindings/go/scip`。旧路径根模块（v0.6.1 及更早）
的 go.mod 平铺声明了整个 CLI 工具链（buf、docker、otel、sqlite 等），
实测 `go mod tidy` 会拖入约 90 个间接依赖；bindings 子模块则只带
protobuf 与少量辅助依赖。

## Decision（U5 八项论证）

1. **Why needed**: SCIP 互操作是 T5 DoD 的硬性要求（§L14）；没有官方
   protobuf 绑定就无法产出/消费可被 Sourcegraph 等标准工具接受的索引。
2. **License**: MIT（scip-code/scip 仓库 LICENSE），与本项目兼容，无 copyleft 传染。
3. **Maintenance health**: 由 Sourcegraph 发起、已移交 scip-code 组织持续维护；
   本项目固定 bindings 子模块 v0.9.0（go 1.25），升级路径即 bump 版本。
4. **Security posture**: 唯一实质运行时传递依赖 google.golang.org/protobuf 是
   Google 官方 protobuf-go 库，安全维护等级高；适配器不引入任何网络/执行面。
5. **Platform impact**: 纯 Go 实现，无 protoc/代码生成步骤进入本项目构建流程。
6. **cgo impact**: 无 cgo。
7. **Core contamination risk**: 零——SCIP 类型只出现在 internal/index/interop
   包内，核心 index/query 包仅见 Canonical IR（§L14：SCIP 不是内存模型），
   包边界由 internal/conformance 的机械检查保证。
8. **Replacement cost**: 手写最小 .pb 编码可行（本项目所需字段子集约百余行），
   但 proto 枚举/rule 演进后会静默漂移且无法与上游 conformance 测试对齐，
   长期成本高于维护一个 MIT 只读依赖，不采纳。

### 白名单新增条目论证（逐条，与 go.mod 实际出现一致）

- `github.com/scip-code/scip/bindings/go/scip`: SCIP protobuf 绑定本体，
  T5 DoD / §L14 所需；选 bindings 子模块而非旧路径根模块，避免拖入其
  CLI 工具链约 90 个间接依赖（上述 Context）。
- `google.golang.org/protobuf`: scip 绑定生成代码的运行时依赖，
  Google 官方库（上述第 4 条）。
- `github.com/sourcegraph/beaut`: scip 绑定的传递依赖，仅服务其测试/
  格式化辅助路径；tidy 按"被导入包的测试依赖"规则收录，无独立风险面。

注：任务预期的 `google.golang.org/genproto` 未出现在实际 go.mod 中，
故不入白名单。

## Consequences

- go.mod 直接依赖由 1 变 2；TestU5 白名单同步放行上述三条。
- 未来升级只需跟踪 bindings 子模块版本；若再迁回根模块需重新评估依赖面。
- LSIF 路径（§L15）保持零第三方依赖（encoding/json 手写 JSONL）。
