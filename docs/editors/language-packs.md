# Language Packs (X4)

omnilsp 通过共享 nested-LSP 桥接入外部语言服务器。启动时用 `exec.LookPath`
发现服务二进制；缺失时返回 `ErrToolchainMissing`，该语言包被跳过注册
（doctor 对应探针报 SKIP，不影响其他语言）。

## rustanalyzer（Rust）

- 前置：安装 [rust-analyzer](https://rust-analyzer.github.io/)（PATH 可见）与 `rustc`（构建上下文身份来源）。
- 发现：`rust-analyzer` 在 PATH 即启用，语言 `rust`，扩展名 `.rs`。
- rename 门：工作区需存在 `Cargo.toml`，否则 rename fail-closed（Unavailable）。

## pyright（Python）

- 前置：`npm install -g pyright` 提供 `pyright-langserver`；另需 `python` 或 `python3`（版本探测）。
- 发现：`pyright-langserver --stdio`，语言 `python`，扩展名 `.py`。
- rename 门：工作区需存在 `pyproject.toml`。

## typescript（TS/JS）

- 前置：`npm install -g typescript typescript-language-server`；`node` 必需，`tsc` 用于版本探测。
- 发现：`typescript-language-server --stdio`，语言 `typescript`，扩展名 `.ts/.tsx/.js/.jsx`。
- rename 门：工作区需存在 `tsconfig.json`。

环境自检：`omnilsp doctor` 会报告 `rust toolchain` / `python` / `node/tsc` 三个探针
（PASS 输出版本首行；工具链缺失报 SKIP）。
