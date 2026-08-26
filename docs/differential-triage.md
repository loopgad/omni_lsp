# Differential Mismatch Triage（§S6）

## 六步流程（goal.md §S6，行 4142-4156）

```
detect mismatch
 -> record exact environment
 -> minimize fixture
 -> classify:
      OmniLSP bug
      upstream bug
      supported semantic difference
      build-context mismatch
 -> add regression
 -> fix/waive via explicit issue/ADR
```

## 四分类定义

- **omnilsp_bug**：OmniLSP 自身实现错误，必须修代码或登记回归测试。
- **upstream_bug**：gopls / pyright / tsserver / rust-analyzer 等上游工具的缺陷，向上游报告。
- **supported_semantic_difference**：有意为之且已文档化的行为差异，属受支持语义。
- **build_context_mismatch**：Go 版本、编译 flags、环境变量等构建上下文不同导致的差异。

## 工件 schema 示例（`test/corpus/triage.go`，`omnilsp.triage.v1`）

```json
{
  "schema": "omnilsp.triage.v1",
  "backend": "go",
  "fixture_ref": "test/corpus/testdata/minimal/definition.go",
  "symptom": "definition target differs",
  "expected": "decl.go:10",
  "actual": "no result",
  "env": {
    "os": "windows", "arch": "amd64",
    "go_version": "go1.26.1", "module_version": "(devel)",
    "timestamp_utc": "2026-08-24T00:00:00Z"
  },
  "classification": "omnilsp_bug",
  "regression_test": "TestXxx",
  "waived_by_adr": ""
}
```

## 红线

**No unexplained golden update to hide regressions.**
任何 mismatch 必须有 `regression_test` 或 `waived_by_adr` 至少一项；
`Validate()` 对两者皆空的报告直接报错。

## 与 test/golden 及 corpus 差分测试的关系

corpus 差分测试将 OmniLSP 响应与上游后端逐字段比对；不一致即 detect 出一个
mismatch。golden 文件只在 triage 完成后更新——分类为 omnilsp_bug 的先加回归，
semantic/build-context 的挂 ADR 引用后再刷新 golden。工件落盘于
`triage-<backend>-<时间戳>.json`，供 nightly 汇总未豁免差异计数。
