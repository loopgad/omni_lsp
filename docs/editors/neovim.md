# Neovim + OmniLSP

OmniLSP speaks standard LSP over stdio, so Neovim (≥0.8) needs no plugin —
only `vim.lsp.start` (built-in) or a one-line nvim-lspconfig custom entry.

## Built-in client (no plugins)

```lua
-- ~/.config/nvim/lua/omnilsp.lua
local function start_omnilsp()
  vim.lsp.start({
    name = "omnilsp",
    cmd = { vim.env.OMNILSP_BIN or "omnilsp", "serve" },
    root_dir = vim.fs.root(0, {
      "go.mod", "compile_commands.json", "Cargo.toml", "pyproject.toml",
      "pyrightconfig.json", "tsconfig.json", "jsconfig.json", "package.json", ".git",
    }) or vim.fn.getcwd(),
  })
end

vim.api.nvim_create_autocmd("FileType", {
  pattern = {
    "go", "c", "cpp", "rust", "python", "typescript", "typescriptreact",
    "javascript", "javascriptreact",
  },
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
    cmd = { vim.env.OMNILSP_BIN or "omnilsp", "serve" },
    filetypes = {
      "go", "c", "cpp", "rust", "python", "typescript", "typescriptreact",
      "javascript", "javascriptreact",
    },
    root_dir = lspconfig.util.root_pattern(
      "go.mod", "compile_commands.json", "Cargo.toml", "pyproject.toml",
      "pyrightconfig.json", "tsconfig.json", "jsconfig.json", "package.json", ".git"
    ),
    settings = {},
  },
}
lspconfig.omnilsp.setup({})
```

## Verifying

1. Use the project-local client acceptance setup in `test/acceptance/clients/README.md`; it pins Neovim and the Python/TypeScript language servers and fails with `not_verified` when any prerequisite is missing.
2. Open a Go, C/C++, Rust, Python, TypeScript, or JavaScript file; run `:LspInfo` and confirm `omnilsp` is attached.
3. Hover a known symbol. To inspect the evidence ring from Lua, send the server's custom request directly:

   ```lua
   local client = vim.lsp.get_clients({ bufnr = 0, name = "omnilsp" })[1]
   client:request("omnilsp/explain", {}, function(err, result)
     assert(not err, vim.inspect(err))
     print(vim.inspect(result.evidence))
   end, 0)
   ```

   The real-client acceptance run checks the server's evidence response for each language, so a missing server or missing hover is a failure rather than a skipped pass.

`go`, `c`, `cpp`, `rust`, `python`, `typescript`, `typescriptreact`,
`javascript`, and `javascriptreact` are the filetypes covered by the built-in
client example. Install a language grammar/filetype provider separately when
Neovim does not recognize one of those filetypes.
