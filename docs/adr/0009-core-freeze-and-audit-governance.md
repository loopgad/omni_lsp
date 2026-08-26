# ADR-0009: Core 接口冻结纪律与二轮审计治理

日期：2026-08-26
状态：Accepted
关联：ADR-0007、ADR-0008、goal.md §V4/§U10/Appendix D/§N8/§P2

## 背景

第二轮 goal.md 深审发现三类问题：
1. **安全洞**：TCP 主传输未过 §N8 loopback 校验（`--addr 0.0.0.0:9301` 可裸奔上线）；
2. **冻结纪律缺位**：languages.Backend 核心接口外已累积 5 个可选能力接口，
   无 ADR 记录演进规则、无 shape-guard 测试；
3. **审计可信度**：Y5-6 探针错位（用 replay 往返测试冒充"版本政策已文档化"）、
   Y2-2 文案宣称的覆盖大于实际；supervisor 迁移不可观测违反 Appendix D MUST。

## 决策

### D1: TCP 传输与 HTTP/debug 同受 §N8 闸门
listen 前强制 ValidateAddr，非 loopback 一律 fail-closed（exit 1）。TCP 无 auth
是既定 v1 边界，因此绑定面必须收窄到本机。

### D2: Core 接口冻结 = 固定方法集 + 独立可选接口
- languages.Backend 的 12 方法集由 `TestV4_CoreInterfaceShapeFreeze` 反射锁定；
  增删改任一方法必须先有 ADR + §U10 规范修订。
- 新能力一律走独立可选接口（SignatureHelper/Formatter/InlayHintProvider/
  StatusReporter/IncompleteCompletionProvider），同一测试守护其存在性。
- 能力协商模式（server 类型断言 + MethodNotFound 干净拒绝）是唯一扩展路径。

### D3: 审计探针必须语义对齐
注册表探针与条款文本的映射接受人工复核：Y5-6 改为 protocol.manifest 指纹检查
（版本面的机器可验证载体）+ docs/versions.md；Y2-2 文案改为实测范围（方法/
通知级容忍 + 字段级容忍测试），3.18 特性门控显式缓期（DEF-A7GATE 行）。

### D4: Supervisor 迁移可观测
notify() 在 Ready→非Ready 迁移时输出 stderr 结构化行（Appendix D MUST 的
always-on 观察者），结构化消费者经 RegisterCallbacks 挂载。

### D5: 骨架目录即意图
删除 6 个空壳目录（backend/api、index/dynamic、semantic/{api,evidence,symbol}、
workspace/document）——空目录误导布局审计（§U0）；目录在内容落地时创建。

## 后果
- 远程攻击面收窄至显式配置错误（会被闸门拒绝）。
- 接口演进有了机器可执行的绊线；可选接口成为唯一合法扩展点。
- 注册表探针恢复"探针即证据"的可信度。
- P2 指标全集、Appendix E 请求状态机观测、INV 具名补齐、A6/A8/G9 文档页
  作为 DEFERRED 行显式登记（见 registry.go post-x9 域）。
