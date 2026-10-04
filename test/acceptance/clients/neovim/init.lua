local markers = {
  "go.mod", "compile_commands.json", "Cargo.toml", "pyproject.toml",
  "pyrightconfig.json", "tsconfig.json", "jsconfig.json", "package.json", ".git",
}
local filetypes = {
  "go", "c", "cpp", "rust", "python", "typescript", "typescriptreact",
  "javascript", "javascriptreact",
}

_G.omnilsp_diagnostic_counts = {}
_G.omnilsp_diagnostics = {}
local previous_diagnostics = vim.lsp.handlers["textDocument/publishDiagnostics"]
vim.lsp.handlers["textDocument/publishDiagnostics"] = function(err, result, context, config)
  if result and result.uri then
    local counts = _G.omnilsp_diagnostic_counts
    counts[result.uri] = (counts[result.uri] or 0) + 1
    _G.omnilsp_diagnostics[result.uri] = result.diagnostics or {}
  end
  if previous_diagnostics then
    return previous_diagnostics(err, result, context, config)
  end
end

vim.api.nvim_create_autocmd("FileType", {
  pattern = filetypes,
  callback = function(event)
    local root = vim.fs.root(event.buf, markers) or vim.fn.getcwd()
    vim.lsp.start({
      name = "omnilsp",
      cmd = { vim.env.OMNILSP_BIN, "serve" },
      root_dir = root,
      workspace_folders = { { uri = vim.uri_from_fname(root), name = vim.fn.fnamemodify(root, ":t") } },
      settings = {},
    }, { bufnr = event.buf })
  end,
})
