# 长程审计 Spec：测试盲区、契约缺口与分数口径

本文件是一次高强度核查的收敛产物。目标不是复述发现，而是让后续每一轮
都知道**当前站在哪里、哪些已经修好、剩下什么按什么顺序做、哪些必须先有人
拍板**。

三条纪律贯穿全文：

1. **PASS 不等于锁住了东西。** 每个新增测试都做过变异：把被测的那一行改坏，
   确认测试变红。没做这一步的测试只能算「跑过了」。
2. **静态推断不算结论。** 下面标 `[推断]` 的条目来自只读代码的子代理审计，
   尚未经实测证伪；标 `[已复核]` 的我亲自跑过命令或读过源码确认。
3. **分数必须能自己解释。** 97.7% 不是目标，「为什么是 97.7%」才是。

---

## §1 现状基线

`bash scripts/test.sh`（等价 `make test`）是唯一的完整门禁：gofmt → build →
vet → `go test -count=1 -timeout 90m ./...` → `verify --min 90`。后三步里
verify 跑的是 CI 出货的同一二进制，所以本地分数与 CI 同源。

| 域 | 分数 | 说明 |
|---|---|---|
| **core** | **97.7%** | = `baseline.json` 的 `97.70959595959597`，逐位吻合 |
| post-x8 | 0.0% | |
| post-x9 | 6.9% | DEF-V4FREEZE 记账收口后自 3.4% 升至 6.9%（第四轮）；A1c 的诚实代价见 §2.1 |
| x5 | 85.7% | INDEX 83.3% / INTEROP 50.0% |
| x7 | 100.0% | |
| x8 | 66.7% | CLIENTS 50% / DIST 50% |
| x9 | 75.0% | RELEASE 75% |

registry 124 条（2026-10-07 实测 `grep -c "Status: Status" internal/conformance/registry.go`）。`gofmt -l internal cmd test` 只剩 3 个
`test/acceptance/evidence/**` fixture —— 那是捕获的编辑器证据，不是我们的源码。

### 分数口径的一个已知弱点 `[已复核]`

`TestConformanceNoRegression` 用 `conformance.FastReport`，而 fast 模式**只做
AST 符号存在性扫描**（`probe.go:19-66`），从不执行任何测试。

> 结论：`baseline.json` 的 97.71 可以在「所有被引用的测试全红」的情况下保持
> 不变。它防的是「有人删了探针指向的测试」，不是「探针指向的测试是否通过」。

算分本身是自洽的：按 `report.go:182-195` 的加权式
`0.24*Y0+0.18*Y3+0.18*Y1+0.13*Y2+0.09*Y4+0.09*Y5+0.09*PERF` 代入 7 个有权类目，
精确等于 `97.70959595959597`；Y1=10.5/11、Y2=8.5/9、PERF=5.5/6 的分母也与
registry 的 AUTO/PARTIAL/GATE 分布一致。

---

## §2 已完成（每条都有变异证据）

### 2.1 A1c：把 PERF-3 拆成它声称的两件事 `991be43`

PERF-3 的 Text 声称「qualified known-symbol positive results」+「six-bucket
zero-error classification」，两件事**都**由 `TestS21_ZeroErrorClassification`
验证，而那个测试需要 `OMNILSP_BIN` 指向的冻结候选 ⇒ 结果取决于机器上恰好有
没有这个产物。它唯一的另一个探针组只验证「没候选时闸门 fail-closed」，
**两件都不验证**。

拆成：

- `PERF-3`：Text 只写闸门，探针指向 in-package 的
  `TestPERF3_S21GateFailsClosedWithoutCandidate`。
- `DEF-S21CORPUS`（post-x9 / DEFERRED / Probe=nil）：承担「冻结候选语料分类通过」。

`engine_test.go` 的断言同步改为要求 PERF-3 恰好 1 个探针组且为该 in-package 组，
并断言 `DEF-S21CORPUS` 必须是 probe-less 的 post-x9 DEFERRED —— 防止有人日后
把机器相关的语料跑悄悄挂回计分项。

代价是诚实的：post-x9 由 3.6% 降到 3.4%，core 不变。

### 2.2 修好 `scripts/test.sh` 一个 Windows 上必然失败的检查 `f66c1aa`

gofmt 步骤用 `grep -v "evidence/"` 过滤 acceptance fixture，但 Windows 上
`gofmt -l` 打印的是**反斜杠**路径 ⇒ 干净树上 `FAILED: gofmt`。改成
`grep -v evidence`。同时加了 `Makefile`（同流程的 make 版）。

### 2.3 锁住可选能力接口的方法集 `5a3443f`

ADR-0009 冻结了核心 `Backend` 的 12 个方法，但**10 个可选能力接口的方法集
没有任何约束**。而它们是靠运行时 type assertion 发现的 —— 抽掉一个方法，
全仓零编译错误，`backend.(languages.DeclarationProvider)` 静默变 false，特性
消失，没有任何测试失败。

`backend_conformance_test.go`（`package languages_test`，反射 `(*T)(nil)` 取
指针类型、不实例化，因此不需要 toolchain/工作区/进程）要求每个可选接口至少
被一个已出货后端满足。**故意不抄方法名** —— 那会变成会腐化的第二真相源，
而且会在接口正常新增方法时误报。

同时修了 `backend_shape_test.go` 里指向**不存在的** `features_gate_test.go`
的注释。

> 敏感性：改 `DeclarationProvider.Declaration` 的方法名 ⇒ 🔴 精确报出被孤立的
> 接口与方法名。

### 2.4 修一条永不触发的超线性断言 `3ca1f51`

fixture 每级增长 4×，所以线性 = 4×、O(n^1.5) = 8×、**O(n²) = 16×**。原阈值
20× 落在 n^2.16–n^2.32 之间 —— **不对应任何自然复杂度，且高于 16×，所以真正
的二次 blowup 都不会触发**。改成 8×。

噪声余量不能靠抬高阈值来买：墙钟平均双向散乱，一个高到能吞掉噪声的界也
吞掉回归。本机实测三个尺寸是 `734µs / 1.01ms / 3.16ms`，比值 1.37× 与 3.14×，
本就亚线性。

### 2.5 修一个客户端永久挂死的协议违反 `a710069`

`Message.Method` 是 `string`，解码后无法区分「method 字段缺失」（真响应）与
「method 存在但为空」（畸形请求 `{"id":1,"method":""}`）。原来的
`IsResponse() = ID != nil && Method == ""` 把两者混为一谈，而
`server.go:355` 的 `if msg.IsResponse() { continue }` 在调度**之前**丢弃所有
响应 ⇒ **畸形请求被静默丢掉，客户端永远等不到回复**。

现在解码时记录字段是否存在（`methodPresent`，经 type-alias + key 探测），
三个谓词改按**存在性**判定。`Dispatch` 一行未改：`{"id":1,"method":""}` 现在
是 request ⇒ `handlers[""]` 不存在 ⇒ 自动得到 `MethodNotFound`。

> 走过的弯路：第一版改窄 `IsResponse` 为「还需 result 或 error」，立刻破坏
> `TestMessageKindDetection` —— `NewResponse(id, nil)` 是合法响应，且
> `result: null` 在 wire 上合法。**现有测试锁的是合理契约，是我的条件太激进。**
>
> 双向敏感性：`methodPresent` 恒 true ⇒ 🔴 三个 response 用例红；恒 false ⇒ 🔴
> 畸形请求与空 method dispatch 用例红（即原缺陷精确复现）。

### 2.6 让安全测试真的断言安全 `268105b`

五个测试 dispatch 恶意输入后写 `_ = resp`。它们实际验证的只有「不 panic」——
而这个测试框架本来就免费给（panic 必然让 run 失败）。注释却声称验证了具体
契约。**安全测试零断言比没有测试更危险，因为它提供虚假信心。**

新增两个 helper：`wantRejected`（必须是 typed error 响应，而非 result 或
nil —— handler 静默成功才是真正的失败模式）与 `stillAlive`（每次恶意输入后
dispatcher 仍在应答，这正是 INV-ARCH-001 的契约）。

顺带发现「超大 payload」测试从未验证自己造出了超大 payload；malformed JSON
的六个输入里有四个根本没到 handler；shell 测试完全没检查，改成读回
`srv.vfs.OpenFiles()`（**不是** `URIs()`，VFS 无此方法）比对严格前缀。

### 2.7 其他事实修正 `4cc8839`

- 删掉 `probe.go` 的 `probeRequiredEnv`：A1c 之后唯一探测 `test/corpus` 的是
  PERF-4，而它的三个 metamorphic 测试不读 S21 闸门 ⇒ 注入的是一个没有探测
  测试会看的环境变量。需求本就应按**测试**而非**按包**表达。
- `shape_test.go` 注释的「9 个无权类目」实为 8。

---

## §3 P0：真实缺陷（未修，按此顺序做）

### 3.1 JSON-RPC 批量请求会杀掉整个连接 `[推断，需实测确认影响面]`

`codec.go:64-65` 的 `ReadMessage` 直接 `json.Unmarshal(body, &msg)`（struct
目标），JSON 数组体必然失败并返回 `CodecError{ParseError}`。而
`transport/stdio.go:86-93` 的 `readLoop` 把**任何** `ReadMessage` 错误当致命 ⇒
`errCh` ⇒ `server.Run`（`server.go:348`）`errors.Join` ⇒ **一个合法的批量帧
断开整条连接**。

同时是语义错位：JSON-RPC §5.1 要求「无法解析的单条只回该条 ParseError，
**不中断**流」，这里是 per-message 可恢复退化成了 per-stream 致命。

全仓 0 处测试构造数组体；`ParseError` 在 `internal/transport/` 下 0 命中。
对应 `goal.md:4251` S10。

**第一步不是写代码**，是先决定语义：拒绝并保持连接，还是实现批量。两者都
可接受，但必须有一个测试钉住选定的那一个。

### 3.2 `test/soak/` 在默认 `go test ./...` 下不参与编译 `[已复核，已降级]`

**已收口（2026-10-07 实测）**：剩下的文档半边已落地——`Makefile:12-13` 与
`scripts/test.sh:12-13` 的入口注释均已写明 test/soak 在 `soak` build tag
之后、按自己的节奏由 nightly workflow 跑 `go test -tags soak`，「默认不跑、
nightly 跑」在入口处可见。下段保留为发现时的记录。

全部 10 个文件都有 `//go:build soak` ⇒ `go vet ./test/soak/` 报
`build constraints exclude all Go files`。`resource_trend_test.go` 里的资源
泄漏闸门在本地与 PR CI 下**零执行**。

**这不是缺陷，是设计** —— 已复核：`.github/workflows/nightly.yml:29` 用
`go test -tags soak -timeout 70m -count=1 ./test/soak/` 显式跑了它，
`scripts/acceptance.ps1` 也在多处引用该目录的测试文件。长跑闸门不进默认矩阵
是对的：单次默认运行要几分钟到几十分钟，而它本该按 nightly 节奏产出证据。

剩下的真实问题只有**文档层面**：`scripts/test.sh` 与 `Makefile` 都没有提
`soak`，读它们的人不会知道存在这条 nightly 路径。最小的修法是在两个入口的
注释里加一行指向 `nightly.yml:29`，让「默认不跑、nightly 跑」这件事在入口处
可见，而不是要读到 CI 配置才知道。

若要让资源趋势断言在 PR 阶段也有信号，方向是把**不需要长跑也能判定**的那部分
（`resource_trend_test.go` 的纯阈值断言）移出 build tag。这是新增覆盖而非修
缺陷，优先级低于上面那行注释。

### 3.3 C/C++ 的 semanticTokens 是假声明 `[推断]`

**半边收口（2026-10-07 实测）**：桩注释半边已修——`ccls.SemanticTokens`
（`backend.go:604-606`）返回空结果时已带注释说明为何是桩（push-based 语义
token 尚未消费，显式空列表而非猜测）。**能力声明与空结果语义不一致的半边
仍在**：`handlers.go:295` 仍无条件声明 `semanticTokensProvider` ⇒ C/C++
客户端拿到 `{"data":[]}`，按 LSP 语义等于「该文档无语义 token」，编辑器会
**关掉自己的语法高亮**。

同一 server 内 `handleSignatureHelp`（`features_handlers.go:40`）与
`handleFormatting`（`:102`）走 `errNotSupported` 干净拒绝 ⇒ **两种不一致的
「不支持」投影**。

同型待查：`handlers.go:771-810` 的 `handleDeclaration` 对无
`DeclarationProvider` 的后端返回什么（`DeclarationProvider` 只有 golang 真实现）。

### 3.4 `position.PositionToOffset` 的 `enc` 形参从未被读取 `[推断，但可自查]`

`position.go:200` 起，函数体只用 `idx.offsetCol`，传 `UTF8` 与传 `UTF32`
行为完全相同。而 `golang/semantic_index.go:2679/2683` 传 `position.UTF16`，
索引却可能以别的编码构建 ⇒ **静默错配**。

同文件 `position.go:122-124` 的 `UTF16ColumnAt` 名不副实 —— 它直接转发
`ColumnAt`，返回的是「索引构建编码」的列而非 UTF-16；唯一调用点
`golang/backend.go:1307`。

**收口状态（2026-10-07 实测，两半分开记）**：enc 校验半边**已完成**——
`position.go:201-206` 的 `PositionToOffset` 现在真正校验，`enc` 与索引构建
编码不一致时返回带两种编码名的错误，静默错配已堵死；`UTF16ColumnAt`
命名/转发半边**仍待办**（`position.go:122-124` 行为同上）。

### 3.5 数字型 `workDoneToken` 被静默丢弃 `[推断]`

**已收口（2026-10-07 实测）**：`progress.go:27-40` 的 `extractWorkDoneToken`
改为返回 raw JSON，数字 token 原样回传（注释明确记录「Decoding to string
first would drop a numeric token」）；`progress_test.go:64-73` 补齐 string 与
integer 两形态用例（integer `4242` → `"token":4242`）。下段保留为发现时的
记录。

`progress.go:31-33` 只做 `if s, ok := ....(string)`，而 LSP 3.17 的
`ProgressToken = integer | string`。客户端传 `42` 时服务端**完全不发**
`$/progress`，且无测试覆盖。

---

## §4 数据自欺：registry 的 Text 与探针实际验证的东西不符

这一类最危险 —— 分数好看，声明与事实相反。**每条改 registry 都必须
`OMNISP_UPDATE_CONFORMANCE=1` 重生成 `docs/conformance.md` + `baseline.json`，
且分数会变**，所以 §7 里有几条需要先拍板。

| 条目 | Text 声称 | 探针实际验证 | 证据 |
|---|---|---|---|
| **X5-4** | 「Dependency graph with selective invalidation + fingerprints + generator edges」，记 **AUTO=1.0** | 只有 `internal/index/graph` 的**自测**；而 ADR-0008 D5 明写「graph 选择性失效经评估同样缓期」，且该包**零生产 import**（全仓唯一命中是 registry 里的 Probe 字符串字面量） | `[推断]` 建议用 `go list -deps ./... \| grep internal/index/graph` 复核 |
| **X5-1** | 含 "corruption quarantine" | 三个探针无一断言 quarantine：一个走 Abort 路径、一个只断言返回 error、一个是压实。真正断言 `Quarantined()` 的 `TestL7`/`TestL9` 挂在别的条目下 | `[推断]` |
| **Y1-7** | 「Disk budget refusal **preserves last good generation**」 | `TestBudget_RejectsWhenFull` 从**空 store** 起，根本没有 last good generation 可保留；另一个探针是 Abort 路径，与 disk budget 无关 | `[推断]` |
| **Y1-6** | 「Index corruption isolated」（IDX-TXN-004/L8） | `TestL7` 的回退半边是 `t.Logf`（非断言），`TestL6_TXN003` 只比错误 nil 布尔与 quarantine 长度单调性 ⇒ 合起来都没验证「回退到最新存活历史」 | `[推断]` |
| **X5-2** | 「SCIP/LSIF conversions preserve provenance and report conversion losses」 | 探针缺两个**孤儿测试**：`TestL11_IdentityPreserved`（唯一做双向 repo/commit identity 往返）、`TestL14_LossyNotesDocumented`（唯一做 §L14 有损登记） | `[推断]` |
| **X5-7** | 「Typed semantic generations persist and serve fresh-only ... queries」 | persistent 探针组是纯租约/取消语义，两测试只用 `publishOne(t, s, "string")`，**不触碰任何 semantic payload** | `[推断]` |

### 4.1 两个 ADR 的机制描述与代码不符

- **ADR-0006** 称「SCIP 类型只出现在 `internal/index/interop` 包内，且包边界
  由 internal/conformance 的机械检查保证」。两个命题都不成立：
  `internal/languages/rustanalyzer/semantic_index.go:28` 是生产代码且直接
  import scip 绑定；而**不存在任何这样的机械检查**。同样的假命题也写在
  `internal/index/interop/scip.go:11`。
- **ADR-0002** 称「CI 跑 `go list -deps` 并对除 `internal/protocol/jsonrpc` 外的
  禁用匹配失败」。实际 `ci.yml:30` 只覆盖
  `./internal/workspace/... ./internal/languages/... ./internal/runtime/scheduler
  ./internal/runtime/supervisor`，**不含 `internal/index/`**；仓内唯一自动检查
  `TestARCH002_NoProtocolImportsInCore` 是源码字符串匹配，且**禁用整个
  `/internal/protocol/` 前缀**（与 ADR 的「jsonrpc 豁免」语义相反）。

### 4.2 Y5-2「No protocol imports in Semantic Core」的覆盖面被低估 `[推断]`

`meta_test.go:95-100` 的 `coreDirs` 只有 4 个目录（`internal/semantic`、
`internal/identity`、`internal/runtime/scheduler`、`internal/workspace`），
**不含 `internal/index/`** —— 而 `internal/index/{model,semantic,persistent,graph}`
才是 Semantic Core 的主体。

### 4.3 ADR-0009 已在本轮补齐一半 `[已复核]`

原先声称「12 方法集已冻结」，实际 `backend_shape_test.go` 只反射接口自身，
不对任何具体后端断言。补完后：核心 12 方法 + 10 个可选接口方法集都有约束。
（5 个后端的**编译期**符合性本来就由 `server.go:259 RegisterBackend` 与
`cmd/omnilsp/main.go:302-304` 的 `wrap(...)` 实例化保证，不需要额外断言。）

---

## §5 测试盲区清单（按投入产出排序）

### 5.1 恒真测试与空转测试

| 位置 | 问题 |
|---|---|
| `jsonrpc/fuzz_test.go:74-113` | `FuzzPosition` 整个函数空转：11 个**常量**字符串 unmarshal 后 `_ = raw`，从不碰 `Codec`/`Message`，注释却宣称「验证解码不 panic」 |
| `jsonrpc/s10_conformance_test.go:81-95` | `TestStructuredErrorResponse` 注册 handler 却从不 Dispatch，断言只覆盖构造函数 |
| `jsonrpc/s10_conformance_test.go:39-49` | `TestMalformedJSONDoesNotPanic` 的 `"not json"` 从不被解析（直接路由到未知方法 ⇒ `MethodNotFound`），测的是「未知方法」 |
| `position/fuzz_test.go:62` | 往返断言被 `_ = back` 丢弃。代码注释已给出理由（模糊位置往返不精确，INV-POS-001 允许），所以不是缺陷而是**断言偏弱**；同文件 `FuzzMultiByteBoundary:156` 真断言了 `back != off`，两者强度不一致 |
| `position/fuzz_test.go:112-117` | 空 if 体，`err` 从不检查。`OffsetToPosition` 的错误路径因此无人验证 |
| `position/position_test.go:68-76` | `TestCJKCharacters` 对 3 字节 CJK 逐字节 offset 调 `OffsetToPosition`，返回值零断言 |
| `golang/uri_fuzz_test.go:25-31` | `FuzzURI` 零断言 ⇒ `pathToUri`（`backend.go:1462`）唯一调用点是空转 fuzz，覆盖率应 0% |
| `golang/features.go:90-92` | `if endCol == 0 { endCol = 0 }` 字面空操作；且 `:85-93` 的 endCol/endLine 计算在 `features_test.go:41-62` 完全未断言 |
| `server/progress_test.go:26-29` | `orig := s.send; if orig == nil { t.Fatal }` —— Go 的 method value 是闭包，**永远非 nil**；同行 `notifications`/`orig` 声明后从未使用 |
| `server/lifecycle_test.go:385-388` | 只有 `Logf` 没有 `Errorf`，文案「rejection is silent」自相矛盾 |

### 5.2 恒假守卫与不可达分支（代码本身的问题，测试再多也覆盖不到）

> 复核记录：以下各条已逐条 grep/读码验证。其中 `auth.go:48`（window 只有定义、无赋值无读取）与 `meta_test.go:85-92`（`isLeafHelper` 两个分支都 `return false`，且 `:74` 真实调用它）确认成立。

| 位置 | 问题 |
|---|---|
| `server/engine_wiring.go:239-241` | `rangesOverlap` 的 URI 不等分支**恒假**：`ValidateEditSet` 以 `canonicalDocumentURI` 分桶，只在同桶内成对调用。`engine_wiring_test.go:641` 的「cross-file overlap impossible」子测试因两文件分属不同桶**根本没调用到** |
| `server/handlers.go:1798-1800` | `invalidateExternalSources` 的 `if count == 0` **不可达**（唯一调用点已在 `:48` 提前返回） |
| `server/diagnostics.go:368-370` | 与 `:362-364` 逐字重复的守卫，单线程语义下恒不成立 ⇒ 错误文案不可达。**需 `-race -count=100` 复核** |
| `server/index.go:378` | `firstInventoryChange` 兜底 return **不可达**（records 已排序 + json.Marshal ⇒ 摘要相同 ⟺ 记录全等 ⟺ 循环必先 return） |
| `httpserver/auth.go:86` | `lim != nil` **恒真**（唯一调用点第三实参恒为 `newPerSourceLimiter(...)`） |
| `httpserver/auth.go:48` | `perSourceLimiter.window int64` 是**完全死字段**（窗口硬编码 `now()/60`） |
| `server/handlers.go:1872-1873` | `completenessName` 的 `default` 臂**不可达**（枚举恰 3 值且 3 个 case 全列出） |
| `index/semantic/semantic.go:716` | `size < 0` 恒假（全部来自 `len()`） |
| `index/semantic/semantic.go:769-771` | `total > MaxSegmentBytes` 不可达（前置条件已逐次封顶） |
| `index/persistent/recovery.go:192-196` | `ParseUint` 失败 continue **不可达**（`validSegmentID` 已保证可解析） |
| `conformance/meta_test.go:85-92` | `isLeafHelper` 恒返回 false 的死函数 |

### 5.3 死代码与「声明了却无人消费」的契约

| 位置 | 问题 |
|---|---|
| `index/persistent/store.go:289,338` | `FileStore.Quarantine` / `QuarantineGeneration` 全仓零调用点零测试（TXN-004 声明在 `IndexStore` 接口却从未接线，实际走 `quarantineSegsLocked`） |
| `index/persistent/store.go:85-91,96-98` | `IndexStore` 与 `LeasedIndexStore` 接口**全仓无任何生产代码当类型使用或做类型断言**（全部直接用 `*FileStore`） |
| `index/graph/**` | **整包零生产 import**；`Propagate`/`RegisterGenerated`/`InvalidateGenerator` 的调用点全在 `graph_test.go` 内 |
| `index/interop` Canonical IR | `ExportSCIP`/`ImportSCIP`/`FromOccurrences` 及整套类型只被 `interop_test.go` 使用，生产走 `scip_model.go` 另一套 |
| `jsonrpc/codec.go:14` | `HeaderContentType` 常量**全仓零引用** |
| `server/source_changes.go:164-176` | `externalSourceSyncError` **只有测试调用点** |
| `golang/features.go:31,76,104` | SignatureHelp/Formatting/InlayHint **只有 golang 实现**，无测试记录这是有意的 |
| `server/handlers.go:1504-1507` | `CacheStats()` 聚合分支：全仓无任何测试 backend 实现该接口 |
| `server/features_handlers.go:139-192` | **`handleInlayHints` 成功路径整体零覆盖** —— 全仓无测试 backend 实现 `InlayHintProvider` |
| `golang/backend.go:1593-1595` | `Declaration`（`DeclarationProvider` 唯一真实现）零测试 |
| `golang/package_freshness.go:151` | `WorkspaceSnapshotGeneration(){return 0}` 是常量桩但**零测试钉住「永远 0」**；server 侧 snapshot-isolation 依赖 generation 单调性 |

### 5.4 perf / soak / corpus 的假信号

- `perf_test.go:429-431` 的 `d > 2*time.Second` 是任意绝对墙钟阈值（flaky 源）。
  它测的是「取消是否被及时响应」而非复杂度，所以不打算跟 §2.4 一起改。
- `perf_test.go:369-371` 的 `TestS17_MetadataPresent` 断言的是 `buildCorpus()`
  自己硬编码生成的 198 行 ⇒ **自证式断言**；且对 `fs=unavailable`（`:299-306`
  带注释承认未实现）这类占位符不设约束 ⇒ §S17「记录确切环境」被降级为
  「打印占位符」。
- `perf/e2e_lsp_test.go:797-800` 的 `TestS18S19_RealProcessPerformanceAcceptance`
  被 `OMNILSP_ACCEPTANCE=1` 静默 skip，**无 `=required` 硬失败模式** —— 与
  `test/soak/stdio_soak_test.go:155` 的 `OMNILSP_SOAK_GATE=required` 和
  `test/corpus/s21_report_test.go:27` 的 `s21GateRequired()` 模式不一致。
- `test/corpus/s21_stdio_test.go:45-54` 的 `OMNILSP_CANDIDATE_SHA256` 是
  **可选**的（`if claimed != ""`）⇒ 未设时「frozen candidate」前提不被验证。
- `test/corpus/classify_test.go` 裸跑（无 `OMNILSP_S21_REPORT`）时所有语言全
  skip ⇒ `buckets.Total()==0` ⇒ **测试通过、零 signal**。

### 5.5 并发 / 事务测试的假信号（`internal/index/persistent`）

- `:234-245` 的取消竞态测试注释花 6 行论证 rollback 不留痕迹，实际**只查
  `staging-*` glob 与 `manifest.tmp`，不查已 promote 到 store root 的孤儿
  segment**（对照非并发版 `:130` 有 `os.Stat`）。
- `:221-233` 取消分支不断言 `view.ID` 保持旧代，且 `view.Segments[0]` 无长度
  保护 —— `ErrNoGeneration` 时会 **panic 而不是 fail**（flaky 假信号）。
- `:604-612` 的「幂等恢复」只查单调不查相等 ⇒ **两次 `OpenSnapshot` 都失败的
  完全损坏状态也能通过**。
- `:696-734` 正确断言段文件被物理删除，但**没断言 `history.json` 内容** ⇒
  `pruneHistoryGeneration` 若出错测试仍绿。
- `reindex_lease_test.go:10-48` 注释声称防护「different server processes」，
  测试却是**同进程内**两次 `AcquireReindexLease`。
- `:113`+`:116-118` 依赖 `cancelAt: 6` 这个对 `ctx.Err()` **调用次数**的硬编码
  （当前 6 处），任意新增/重排一处都会让断言在错误时间点取样而不报错。
- **TXN-004「整代丢弃 + 回退到最新存活历史」专项**：整代丢弃本身只被
  `TestCompactRetiresOnlyTheCompactedFallback` 间接覆盖（唯一真正断言回退内容
  正确处，但它**未被任何 registry 条目引用**）；**没有任何测试构造「多段
  generation 中一段坏」**，而 `quarantineSegsLocked` 的多段交集分支从未走过；
  `historyK=3` 的多级回退顺序无测试。

### 5.6 顺序不确定性（同 §2.1 那类问题的残留）

`index/interop/lsif.go:187`+`:221-223`+`:239`/`:247` 对 `vertices` 做 map 迭代
产出 `idx.Documents`，顺序不确定，而测试全用集合比较 ⇒ 非确定性从不被暴露。

---

## §6 长程路线

每一阶段都要求「改完 → 变异验证 → 全量绿 → 提交」才算完成。

### 阶段 A：不需要任何人拍板（可立即开始）

1. §5.1 的恒真测试：逐个补断言或改注释（11 处）。
2. §5.3 的死代码：删 `HeaderContentType`、`perSourceLimiter.window`、
   `isLeafHelper`；给 `golang.WorkspaceSnapshotGeneration` 加一条钉住 `0` 的
   测试；给 `handleInlayHints` 成功路径加 mock。
3. §5.2 的不可达分支：逐个确认后删掉（`invalidateExternalSources` 的
   `count == 0`、`completenessName` 的 default、`isLeafHelper`），或改写成
   真正可达的形式。
4. §3.4 的 `enc` 形参：**enc 校验半边已完成**（`position.go:201-206`）；
   `UTF16ColumnAt` 命名/转发半边（`position.go:122-124`）仍待办。
5. §3.5 的数字型 `workDoneToken`：**已完成**（`progress.go:27-40` raw JSON
   透传数字形态 + `progress_test.go:64-73` 用例）。
6. §5.5 的并发测试假信号：补 store root 孤儿 segment 检查、给
   `view.Segments[0]` 加长度保护、把幂等恢复改成断言相等而非单调。

### 阶段 B：需要先测量的前置工作

1. §3.1 的批量请求：先加一个「数组体进 `ReadMessage`」的测试把当前行为钉住
   （必然失败或返回 ParseError），再决定语义。
2. §5.2 中标「需 `-race` 复核」的三条：跑 `-race -count=100 -cover` 拿数据，
   再决定删还是留。
3. §4 中所有标 `[推断]` 的：先用 grep/`go list -deps`/临时变异逐条证伪。
   **不要在未证伪前改 registry** —— 那会动分数。

### 阶段 C：需要拍板（见 §7）

registry 的 Text↔探针对齐、X5-4 的降级、`soak` 的默认门禁、
`TestARCH002` 的范围、fast 模式是否该执行探针。

### 阶段 D：结构性（不急但别忘）

- `internal/index/graph` 要么接线要么降级 —— 一个零生产 import 的包不该在
  AUTO 满分项上。
- 两个 ADR 的机制描述需要与代码对齐（ADR-0006 的 scip 边界、ADR-0002 的
  CI 检查）。
- fast 报告的语义：如果它只做符号存在性，就该在字段名或文档里说清楚它
  不是「测试通过率」。

---

## §7 需要拍板的开放项

按影响从大到小。每条都给出建议，但都需要确认，因为它们会改分数口径或改
门禁范围。

1. **X5-4 是否从 AUTO 降级？** 它声称选择性失效 + 生成器边，探针只有自测，
   而 ADR-0008 说缓期、包零生产接线。降级会拉低 x5 分数，但让分数与事实
   一致。**建议降级，并同时决定 `internal/index/graph` 的去留。**
2. **fast 模式是否应执行探针？** 当前 97.71 分可以在所有被引用测试全红时保持
   不变。这是「分数能不能自己解释」的核心问题。**建议至少让
   `TestConformanceNoRegression` 能有一种模式真正跑探针**，哪怕慢。
3. **`test/soak/` 的默认门禁？** 加进 `scripts/test.sh` 与 CI 默认矩阵，还是
   把不需要长跑也能给信号的测试移出 build tag。**建议后者**：资源趋势断言需要
   时间，但「form-only」的那些不需要。
4. **`TestARCH002` 的 `coreDirs` 是否该含 `internal/index`？** 需要查
   `goal.md` §U1/INV-ARCH-002 对 "Semantic Core" 的定义，判断当前是有意分层
   还是遗漏。
5. **JSON-RPC 批量的语义**：拒绝并保持连接，还是实现批量。
6. **`probeRequiredEnv` 已删，但 `perf_test.go:429-431` 的 2s 墙钟阈值要不要
   改？** 它测的是「取消是否被及时响应」，2s 是任意值。

---

## §8 工作约束

### 环境

- **Windows + Git Bash。** `os.Rename` 不能覆盖；`exec.Cmd.Wait` 不能并发；
  `TestWatchedCreateRenameDelete...` 偶发文件锁 flake，**非回归**。
- **`gofmt -l` 在 Windows 上输出反斜杠路径** ⇒ 任何 `grep -v "evidence/"`
  之类的过滤都会失效（本会话已因此抓到一个真实 bug）。
- **本会话的沙箱把 `BUILTIN\Authenticated Users:(M)` 这条 ACE 从它介入过的
  文件上剥掉了** ⇒ 工作区大量文件 `Access denied`（WinError 5 = ACL 拒绝，
  **不是**文件锁 —— 锁会是 error 32 "being used by another process"）。
  修法：`powershell -NoProfile -Command "& icacls <path> /grant '*S-1-5-11:(M)'"`
  （Git Bash 会吞 `/grant` 的斜杠参数，必须从 PowerShell 传）。
  仍有 9 个文件连 ACL 都改不了（`internal/languages/nested/` 下的
  `diagnostics.go`、`document_request_test.go`、`executable.go`、
  `executable_test.go`、`request_timing.go`、`source_changes.go`、
  `source_changes_test.go`、`workspace_snapshot.go`、`workspace_snapshot_test.go`）
  —— 绕过办法是**改可写的上游文件做等价变异验证**（本会话已用此法成功三次）。

### 纪律

- `go test` 一律加 `-count=1`；慢测试要显式 `-timeout`
  （`TestFullReport_RunsAutoProbes` 需 ~20min，默认 600s 会砍断）。
- git 身份：`-c user.name="omnilsp-dev" -c user.email="dev@omnilsp.local"`。
- **任何改 registry 的 Text 或 Probe 都必须**
  `OMNISP_UPDATE_CONFORMANCE=1` 重生成 `docs/conformance.md` + `baseline.json`。
  `TestGenerateDocs` 在 **`test/conformance` 包**（`conformance_test.go:59`），
  **不在** `internal/conformance`。
- **任何在 `handlers.go`/`index.go`/`types.go` 新增顶层声明都要重生成 manifest。**
- **不要滥用脚本。** 用脚本前先确认它不会写出工作区之外的东西，产生的临时
  文件要在同一轮清掉。
- **shell heredoc 不可用于含 CJK + 转义引号 + `\x00` 的 Go 源码** ——
  会写入真 NUL 字节（git 显示 `Bin … 0 insertions`）。用 `edit`/`write` 工具。
  `git commit -F -` 传纯文本 commit message 用 heredoc 是安全的。
- **变异必须按精确行号**，且**保留 `if`、只把条件改成 `if false {`** ——
  整行替换会让块体悬空导致 build failed，而 build failed **不算敏感性证据**。
- **不要用 grep 过滤 `go test` 的输出来判定红绿** —— 踩过两次，4 个变异全无
  输出。必须看完整输出（`-v` + `tail`）。
- **写断言前先确认语义。** 凭直觉写断言会让多个子测试假红（offset 是
  **边界**不是字节，`content[5]=='\n'` 在「内容末尾」语义下是正确的）。
- **梯子第 2 级要先查**：「这个约束是不是已经在别处存在了」。本会话两次靠它
  避免重复劳动（5 个后端的编译期符合性、`wrap` 的调用点）。
- **只读代码的子代理结论一律 `[推断]`。** 本会话三路审计累计约 15 处需要
  修正（不存在的文件、不存在的包、不存在的符号、错误前提）。