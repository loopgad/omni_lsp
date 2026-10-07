# ADR-0008: 最后一公里协议面交付与缓期治理

Date: 2026-08-26
Status: Accepted
日期：2026-08-26
状态：已采纳
关联：ADR-0007（信任门与协议指纹）、goal.md §C11/§C4/§I19/§D14-15/§R7/§O5/§W8

## 背景

核心正确性机器（快照、调度、索引事务、信任门、查询 memo 引擎）在多轮深化后
已达 verify --full core 100%。全局缺口审计显示剩余差距集中在两类：

1. **"最后一公里"协议面**——诊断管道、位置编码协商、codeAction、文件监听
   等直接决定编辑器体验的能力此前未交付；
2. **无声差距**——Java/Tier A 语言、多 BuildContext、沙箱等规范承诺既未实现
   也无书面缓期标注，违反 §V9/V10 的诚实原则。

## 决策

### D1: 诊断采用 C11 完整形态（push + pull + 六元组缓存键）

推送路径在 didOpen/didChange 后按 URI 去抖（150ms）异步执行；拉取路径
`textDocument/diagnostic` 与推送共享同一有界缓存（FIFO 512 条）。缓存键包含
§C11 强制清单全部字段：snapshot rev、内容哈希、BuildContextID、后端 epoch、
诊断配置哈希。resultId 按语义字段（source|code|message|range）派生而非纯行号
（§I18），行漂移不闪断。

### D2: 位置编码协商贯穿到 backend 请求

initialize 读取 `general.positionEncodings`，取客户端首个受支持项（utf-8/
utf-16/utf-32 全部原生支持）；结果回填能力声明，并以 int 注入 languages
请求结构——激活此前的 Encoding 死参数。静默客户端保持 UTF-16 基线。

### D3: codeAction 三分类安全模型

非变更型（解释/导航）自由提供；变更型编辑仅携带 proposal，应用时仍走 S3 门；
可执行命令 v1 一律不暴露——杜绝"无害标题背后藏任意执行"（§I19 MUST）。

### D4: 文件监听为零依赖轮询

依赖白名单排除 fsnotify。轮询扫描器以 (mtime,size,content) 指纹做差分，深度
上限 64，首次扫描建立基线不报事件。`workspace/didChangeWatchedFiles` 及 D15 三通知
作为客户端驱动通道并行接受。

### D5: 缓期必须显式化

12 个新 DEFERRED 注册表行覆盖 Java/Tier A/H8/多 context/hierarchies/sandbox/
trace 导出/插件挂载/remote index/自更新/notebook 同步/Compact 调度，每行携带
Reason 与升级里程碑。graph 选择性失效（M1-M3）经评估同样缓期：当前全局快照
rev 模型下任何 didChange 已使全部 memo 条目过期，图接线的增量收益为零——待
per-file revision 模型（X10 规模化）落地才有意义。装饰性接线是伪完成（V9）。

### D6: 可复现构建进入 gate

build-multiplatform.sh 升级 `-buildvcs=true` 并增加双构建字节比对验证；
SBOM 路径规范化修复绝对 OUTDIR 双重拼接缺陷。

## 后果

- 编辑器基础体验闭环：诊断、编码正确性、codeAction、外部文件感知。
- 规范与实现之间不再存在"无声差距"：每个未实现条款都有带 Reason 的注册表行。
- X10 规模化（per-file rev、索引服务、插件 API 面）获得明确的接线锚点。
