# Neovim + OmniLSP

OmniLSP speaks standard LSP over stdio, so Neovim (≥0.8) needs no plugin —
only `vim.lsp.start` (built-in) or a one-line nvim-lspconfig custom entry.

## Built-in client (no plugins)

```lua
-- ~/.config/nvim/lua/omnilsp.lua
local function start_omnilsp()
  vim.lsp.start({
    name = "omnilsp",
    cmd = { "omnilsp", "serve" },
    root_dir = vim.fs.root(0, { "go.mod", ".git" }),
    filetypes = { "go", "c", "cpp" },
  })
end

vim.api.nvim_create_autocmd("FileType", {
  pattern = { "go", "c", "cpp" },
  callback = start_omnilsp,
})
```

Require the file from `init.lua`:

```lua
require("omnilsp")
```

## nvim-lspconfig style

```lua
local lspconfig = require("lspconfig")
local configs = require("lspconfig.configs")

configs.omnilsp = {
  default_config = {
    cmd = { "omnilsp", "serve" },
    filetypes = { "go", "c", "cpp" },
    root_dir = lspconfig.util.root_pattern("go.mod", ".git"),
    settings = {},
  },
}
lspconfig.omnilsp.setup({})
```

## Verifying

1. `omnilsp doctor` — every probe PASS/WARN, no FAIL.
2. Open a Go file; run `:LspInfo` — the `omnilsp` client should be attached.
3. Hover a function: results carry evidence; ask the server for
   `omnilsp/explain` via `vim.lsp.buf.execute_command` to audit them.
