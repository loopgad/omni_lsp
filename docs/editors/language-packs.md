# Language Packs (X4)

omnilsp 通过共享 nested-LSP 桥接入外部语言服务器。启动时用 `exec.LookPath`
发现服务二进制；缺失时返回 `ErrToolchainMissing`，该语言包被跳过注册
（doctor 对应探针报 SKIP，不影响其他语言）。

## rustanalyzer（Rust）

- 前置：安装 [rust-analyzer](https://rust-analyzer.github.io/)（PATH 可见）与 `rustc`（构建上下文身份来源）。
- 发现：`rust-analyzer` 在 PATH 即启用，语言 `rust`，扩展名 `.rs`。
- rename 门：工作区需存在 `Cargo.toml`，否则 rename fail-closed（Unavailable）。

## clangd（C/C++）

- 前置：安装 [clangd](https://clangd.llvm.org/)（PATH 可见）；版本矩阵见
  docs/language-packs.md（Min 14 / 推荐 17 / 最大实测 19）。
- 发现：`clangd` 在 PATH 即启用，语言 `cpp` 与 `c`，扩展名
  `.c/.cpp/.cc/.h/.hpp`；多 `.h` 归属歧义在解析落点就地报告（§X3）。
- rename 门：工作区需存在 `compile_commands.json`，否则项目级 rename
  fail-closed（§X3）。

## pyright（Python）

- 前置：`npm install -g pyright` 提供 `pyright-langserver`；另需 `python` 或 `python3`（版本探测）。
- 发现：`pyright-langserver --stdio`，语言 `python`，扩展名 `.py`。
- rename 门：工作区需存在 `pyproject.toml`。

## typescript（TS/JS）

- 前置：`npm install -g typescript typescript-language-server`；`node` 必需，`tsc` 用于版本探测。
- 发现：`typescript-language-server --stdio`，语言 `typescript`，扩展名 `.ts/.tsx/.js/.jsx`。
- rename 门：工作区需存在 `tsconfig.json`。

环境自检：`omnilsp doctor` 按类报告探针——语言包工具链 `go toolchain` /
`clangd` / `rust toolchain` / `python` / `node/tsc`（PASS 输出版本首行；工具链
缺失报 SKIP 或 WARN），以及环境项 `version` / `os/arch` / `config` /
`workspace` / `compile db` / `cache dir` / `port` / `disk space`。
