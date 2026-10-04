local result = {
  client = "neovim",
  status = "running",
  cleanExit = false,
  clientTestsCompleted = false,
  serverExitConfirmed = false,
  cases = {},
}

local function write_result()
  local target = vim.env.OMNILSP_CLIENT_RESULT
  if not target or target == "" then return end
  vim.fn.mkdir(vim.fn.fnamemodify(target, ":h"), "p")
  local file, err = io.open(target, "wb")
  if not file then error("open result file: " .. tostring(err)) end
  file:write(vim.json.encode(result), "\n")
  file:close()
end

local function response_result(response, label)
  if not response then error(label .. " returned no terminal response") end
  if response.err then error(label .. " failed: " .. vim.inspect(response.err)) end
  if response.result == nil or response.result == vim.NIL then
    error(label .. " returned null")
  end
  return response.result
end

local function pull_diagnostics(client, uri, buf)
  local response = response_result(client:request_sync("textDocument/diagnostic", {
    textDocument = { uri = uri },
  }, 30000, buf), "textDocument/diagnostic")
  if response.kind ~= "full" then
    error("textDocument/diagnostic returned " .. tostring(response.kind) .. " instead of a full report")
  end
  return response.items or {}
end

local function position_for(text, symbol, occurrence, character_offset)
  occurrence = occurrence or 1
  character_offset = character_offset or 0
  local start = 1
  local found
  for _ = 1, occurrence do
    found = string.find(text, symbol, start, true)
    if not found then error("fixture does not contain requested occurrence of " .. symbol) end
    start = found + #symbol
  end
  local before = string.sub(text, 1, found - 1)
  local _, line = string.gsub(before, "\n", "")
  local last_newline = string.match(before, ".*()\n")
  local column = found - (last_newline or 0) - 1 + character_offset
  return { line = line, character = column }
end

local function assert_evidence(response, uri, backend)
  if type(response.evidence) ~= "table" then error("explain returned no evidence array") end
  for _, entry in ipairs(response.evidence) do
    if entry.method == "textDocument/hover" and entry.uri == uri then
      if type(entry.backend) ~= "string" or not vim.startswith(entry.backend, backend .. "/") then
        error("unexpected hover evidence backend " .. vim.inspect(entry.backend))
      end
      return entry
    end
  end
  error("no hover evidence for " .. uri)
end

local function assert_method_evidence(client, uri, method, backend, buf)
  local response = response_result(client:request_sync("omnilsp/explain", { uri = uri }, 30000, buf), "omnilsp/explain")
  for _, entry in ipairs(response.evidence or {}) do
    if entry.method == method and entry.uri == uri then
      if type(entry.backend) ~= "string" or not vim.startswith(entry.backend, backend .. "/") then
        error(method .. " used unexpected backend " .. vim.inspect(entry.backend))
      end
      return response, entry
    end
  end
  error("no OmniLSP evidence for " .. method .. " on " .. uri)
end

local function missing_tools(case)
  local available = {}
  if vim.env.OMNILSP_CLIENT_TOOL_STATUS and vim.env.OMNILSP_CLIENT_TOOL_STATUS ~= "" then
    available = vim.json.decode(vim.env.OMNILSP_CLIENT_TOOL_STATUS)
  end
  local missing = {}
  for _, tool in ipairs(case.requiredTools or {}) do
    if available[tool] ~= true then table.insert(missing, tool) end
  end
  return missing
end

local function location_count(value)
  if type(value) ~= "table" then return 0 end
  return #value
end

local function hover_contains_symbol(value, symbol)
  return value ~= nil and string.find(vim.inspect(value), symbol, 1, true) ~= nil
end

local function wait_for_hover(client, uri, position, symbol, buf, timeout_ms)
  local deadline = vim.uv.now() + timeout_ms
  local last
  repeat
    local response = client:request_sync("textDocument/hover", {
      textDocument = { uri = uri }, position = position,
    }, 30000, buf)
    if response and response.err then error("textDocument/hover failed: " .. vim.inspect(response.err)) end
    if response and response.result ~= nil and response.result ~= vim.NIL then last = response.result end
    if last and hover_contains_symbol(last.contents, symbol) then return last end
    vim.wait(200, function() return false end, 10)
  until vim.uv.now() >= deadline
  return last
end

local function same_document_uri(left, right)
  if type(left) ~= "string" or type(right) ~= "string" then return false end
  local function canonical(uri)
    local path = vim.uri_to_fname(uri)
    path = string.gsub(path, "\\", "/")
    if vim.fn.has("win32") == 1 then path = string.lower(path) end
    return path
  end
  return canonical(left) == canonical(right)
end

local function location_at(location, expected_uri, position)
  if type(location) ~= "table" then return false end
  local uri = location.uri or location.targetUri
  local range = location.range or location.targetSelectionRange or location.targetRange
  local start = range and range.start or nil
  return same_document_uri(uri, expected_uri) and type(start) == "table" and
    tonumber(start.line) == tonumber(position.line) and tonumber(start.character) == tonumber(position.character)
end

local function rename_collision_name(case)
  if case.name == "go" or case.name == "c" or case.name == "cpp" then return "UseTarget" end
  if case.name == "rust" or case.name == "python" then return "use_target" end
  if case.name == "typescript" or case.name == "javascript" then return "useTarget" end
  if case.name == "typescriptreact" or case.name == "javascriptreact" then return "useComponent" end
  error("no known collision target for " .. tostring(case.name))
end

local function diagnostic_probe(case)
  local name = "omnilspMissingSymbol"
  if case.languageId == "go" then return "var _ = " .. name end
  if case.languageId == "c" or case.languageId == "cpp" then return "int clientDiagnosticProbe = " .. name .. ";" end
  if case.languageId == "rust" then return "fn client_diagnostic_probe() { let _ = " .. name .. "; }" end
  if case.languageId == "python" then return "client_diagnostic_probe = " .. name end
  if case.languageId == "typescript" or case.languageId == "typescriptreact" then
    return "export const clientDiagnosticProbe: number = " .. name .. ";"
  end
  if case.languageId == "javascript" or case.languageId == "javascriptreact" then
    return "export const clientDiagnosticProbe = " .. name .. ";"
  end
  error("no semantic diagnostics probe for " .. tostring(case.languageId))
end

local function expected_rename_refusal(case, code, message)
  if tonumber(code) ~= -32803 then return false end
  local normalized = string.lower(vim.trim(tostring(message or "")))
  if case.family == "go" then
    return string.find(normalized,
      "rename refused: symbol is exported; importer packages are not loaded", 1, true) == 1 and
      string.find(normalized, "sem-safe-001", 1, true) ~= nil
  end
  if case.family == "cpp" then
    return normalized == "rename refused: c/c++ function/method collision analysis is not proven (sem-safe-001)"
  end
  if case.family == "typescript" then
    return normalized == "rename refused: typescript/javascript function/method collision analysis is not proven (sem-safe-001)" or
      normalized == "rename refused: typescript/javascript target is outside the current document; collision analysis is not proven (sem-safe-001)"
  end
  if case.family == "python" then
    return normalized == "rename refused: upstream language service returned no edits; safety and completeness are not proven (sem-safe-001)"
  end
  return normalized == "upstream language service refused rename"
end

local function read_file_bytes(file)
  local stat, stat_err = vim.uv.fs_stat(file)
  if not stat then error("stat source file: " .. tostring(stat_err)) end
  local fd, open_err = vim.uv.fs_open(file, "r", 438)
  if not fd then error("open source file: " .. tostring(open_err)) end
  local data, read_err = vim.uv.fs_read(fd, stat.size, 0)
  local _, close_err = vim.uv.fs_close(fd)
  if not data then error("read source file: " .. tostring(read_err)) end
  if close_err then error("close source file: " .. tostring(close_err)) end
  return data
end

local function expected_semantic_diagnostic(case, diagnostic, position, probe)
  if type(diagnostic) ~= "table" or type(diagnostic.range) ~= "table" then return false end
  local start = diagnostic.range.start
  local finish = diagnostic.range["end"]
  if type(start) ~= "table" or type(finish) ~= "table" or tonumber(diagnostic.severity) ~= 1 then return false end
  if tonumber(start.line) ~= tonumber(position.line) or tonumber(start.character) ~= tonumber(position.character) or
    tonumber(finish.line) ~= tonumber(position.line) or tonumber(finish.character) ~= tonumber(position.character) + #probe then
    return false
  end
  local message = string.lower(tostring(diagnostic.message or ""))
  local code = diagnostic.code
  if type(code) == "table" then code = code.value end
	if case.family == "go" then
		return diagnostic.source == "omnilsp-go" and diagnostic.message == "undefined: " .. probe
	elseif case.family == "rust" then
		return diagnostic.source == "rustc" and tostring(code) == "E0425" and
			string.find(message, "cannot find value `" .. string.lower(probe) .. "` in this scope", 1, true) ~= nil
	elseif case.family == "python" then
		return diagnostic.source == "Pyright" and tostring(code) == "reportUndefinedVariable" and
			string.find(message, "\"" .. string.lower(probe) .. "\" is not defined", 1, true) ~= nil
	elseif case.family == "typescript" then
		return diagnostic.source == "typescript" and tonumber(code) == 2304 and
      string.find(message, "cannot find name '" .. string.lower(probe) .. "'", 1, true) ~= nil
  end
  return false
end

local function find_expected_diagnostic(case, uri, probe, position, diagnostics)
  for _, diagnostic in ipairs(diagnostics or _G.omnilsp_diagnostics[uri] or {}) do
    if expected_semantic_diagnostic(case, diagnostic, position, probe) then return diagnostic end
  end
  return nil
end

local function run_case(case)
  local row = { name = case.name, family = case.family, languageId = case.languageId, status = "running" }
  table.insert(result.cases, row)
  local missing = missing_tools(case)
  if #missing > 0 then
    row.status = "not_verified"
    row.reason = "locked prerequisites unavailable: " .. table.concat(missing, ", ")
    write_result()
    return
  end

  local buf
  local ok, err = xpcall(function()
    local file = vim.fn.fnamemodify(case.file, ":p")
    local uri = vim.uri_from_fname(file)
    local diagnosticCounts = _G.omnilsp_diagnostic_counts or {}
    local cleanDiagnosticsBeforeOpen = diagnosticCounts[uri] or 0
    buf = vim.fn.bufadd(file)
    vim.fn.bufload(buf)
    vim.api.nvim_set_current_buf(buf)
    vim.bo[buf].filetype = case.languageId
    local client
    local attached = vim.wait(20000, function()
      client = vim.lsp.get_clients({ bufnr = buf, name = "omnilsp" })[1]
      return client ~= nil and client.initialized == true
    end, 20)
    if not attached then error("omnilsp did not attach for filetype " .. case.languageId) end

    if case.family == "cpp" then
      if not vim.wait(15000, function()
        return (_G.omnilsp_diagnostic_counts[uri] or 0) > cleanDiagnosticsBeforeOpen
      end, 20) then
        error("no clean-source publishDiagnostics notification was observed for " .. case.languageId)
      end
      local cleanDiagnostics = _G.omnilsp_diagnostics[uri] or {}
      if #cleanDiagnostics ~= 0 then
        error("clean C/C++ source unexpectedly produced " .. #cleanDiagnostics .. " diagnostics")
      end
      row.diagnostic = {
        diagnostic_scope = "clean_source_no_false_positive",
        deferred_capability = "DEF-CCLSDIAG",
        published_count = 0,
      }
    end

    local text = table.concat(vim.api.nvim_buf_get_lines(buf, 0, -1, false), "\n")
    local definition_position = position_for(text, case.symbol, 1)
    local use_position = position_for(text, case.symbol, 2)
    local hover = wait_for_hover(client, uri, definition_position, case.symbol, buf, 45000)
    if not hover or not hover_contains_symbol(hover.contents, case.symbol) then
      local explain = response_result(client:request_sync("omnilsp/explain", { uri = uri }, 30000, buf), "omnilsp/explain after unexpected hover")
      row.observed = { hover = hover or vim.NIL, explain = explain }
      error("hover did not describe " .. case.symbol .. " before deadline; last response " .. vim.inspect(hover))
    end
    local beforeExplain = response_result(client:request_sync("omnilsp/explain", { uri = uri }, 30000, buf), "omnilsp/explain")
    local hoverEvidence = assert_evidence(beforeExplain, uri, case.backendLanguage)
    local beforeSnapshot = tonumber(beforeExplain.snapshot or hoverEvidence.snapshotRev or 0) or 0

    local statusBefore = response_result(client:request_sync("omnilsp/status", {}, 30000, buf), "omnilsp/status before edit")
    local beforeEpochs = tonumber(statusBefore.SnapshotEpochs or 0) or 0
    local comment = case.languageId == "python" and "# client-sync-probe" or "// client-sync-probe"
    vim.api.nvim_buf_set_lines(buf, -1, -1, false, { comment })
    vim.wait(100, function() return false end, 10)
    local status = response_result(client:request_sync("omnilsp/status", {}, 30000, buf), "omnilsp/status")
    local afterSnapshot = tonumber(status.SnapshotEpochs or 0) or 0
    if afterSnapshot <= beforeEpochs then
      error("server snapshot did not advance after Neovim buffer edit")
    end
    local changedText = table.concat(vim.api.nvim_buf_get_lines(buf, 0, -1, false), "\n")
    if not string.find(changedText, "client%-sync%-probe") then error("Neovim buffer edit was not retained") end

    hover = wait_for_hover(client, uri, definition_position, case.symbol, buf, 45000)
    if not hover or not hover_contains_symbol(hover.contents, case.symbol) then
      error("post-edit hover did not describe " .. case.symbol .. " before deadline; last response " .. vim.inspect(hover))
    end
    local syncedExplain = response_result(client:request_sync("omnilsp/explain", { uri = uri }, 30000, buf), "omnilsp/explain after edit")
    local syncedEvidence = assert_evidence(syncedExplain, uri, case.backendLanguage)
    if (tonumber(syncedExplain.snapshot or syncedEvidence.snapshotRev or 0) or 0) <= beforeSnapshot then
      error("explain snapshot did not advance after Neovim document synchronization")
    end
    local postEditDiagnostics = pull_diagnostics(client, uri, buf)
    row.observed = row.observed or {}
    row.observed.postEditDiagnosticCount = #postEditDiagnostics
    if #postEditDiagnostics ~= 0 then error("comment-only edit unexpectedly produced diagnostics: " .. vim.inspect(postEditDiagnostics)) end
    if case.family == "cpp" then
      -- Pull diagnostics above is the client-visible source of truth. Push
      -- notifications may be omitted when an unchanged set is unchanged.
    end

    local definitions = response_result(client:request_sync("textDocument/definition", {
      textDocument = { uri = uri }, position = use_position,
    }, 30000, buf), "textDocument/definition")
    local definitionFound = false
    for _, location in ipairs(definitions) do
      if location_at(location, uri, definition_position) then definitionFound = true; break end
    end
    if not definitionFound then
      row.observed = row.observed or {}
      row.observed.definition = definitions
      row.observed.expectedDefinition = { uri = uri, start = definition_position }
      error("definition did not resolve to the fixture declaration: got " .. vim.inspect(definitions) ..
        "; expected " .. vim.inspect(row.observed.expectedDefinition))
    end
    assert_method_evidence(client, uri, "textDocument/definition", case.backendLanguage, buf)

    local completion_position = position_for(text, case.symbol, 2, 5)
    local completions = response_result(client:request_sync("textDocument/completion", {
      textDocument = { uri = uri }, position = completion_position,
    }, 30000, buf), "textDocument/completion")
    local items = completions.items or completions
    local foundCompletion = false
    if type(items) == "table" then
      for _, item in ipairs(items) do
        if string.find(tostring(item.label or item.insertText or ""), case.symbol, 1, true) then
          foundCompletion = true
          break
        end
      end
    end
    if not foundCompletion then error("completion did not offer " .. case.symbol) end

    local references = response_result(client:request_sync("textDocument/references", {
      textDocument = { uri = uri }, position = use_position,
      context = { includeDeclaration = true }, workDoneToken = "neovim-client-soak",
    }, 30000, buf), "textDocument/references")
    local declarationFound, useFound = false, false
    for _, location in ipairs(references) do
      if location_at(location, uri, definition_position) then declarationFound = true end
      if location_at(location, uri, use_position) then useFound = true end
    end
    if location_count(references) < 2 or not declarationFound or not useFound then
      error("references did not contain both fixture declaration and use")
    end
    assert_method_evidence(client, uri, "textDocument/references", case.backendLanguage, buf)

    local collisionName = rename_collision_name(case)
    local textBeforeRename = table.concat(vim.api.nvim_buf_get_lines(buf, 0, -1, false), "\n")
    local diskSourceBeforeRename = read_file_bytes(file)
    local renameResponse = client:request_sync("textDocument/rename", {
      textDocument = { uri = uri }, position = definition_position, newName = collisionName,
    }, 30000, buf)
    if not renameResponse then error("unsafe rename returned no terminal response") end
    if not renameResponse.err then
      row.observed = row.observed or {}
      row.observed.unsafeRename = renameResponse.result
      error("unsafe rename did not return an error for colliding name " .. collisionName ..
        "; got " .. vim.inspect(renameResponse.result))
    end
    if not expected_rename_refusal(case, renameResponse.err.code, renameResponse.err.message) then
      error("unsafe rename returned unexpected error instead of the expected typed refusal: " .. vim.inspect(renameResponse.err))
    end
    local textAfterRename = table.concat(vim.api.nvim_buf_get_lines(buf, 0, -1, false), "\n")
    if textAfterRename ~= textBeforeRename then error("document source changed after unsafe rename refusal") end
    if read_file_bytes(file) ~= diskSourceBeforeRename then
      error("source file on disk changed after unsafe rename refusal")
    end
    assert_method_evidence(client, uri, "textDocument/rename", case.backendLanguage, buf)

    if case.family == "cpp" then
      local cleanDiagnostics = _G.omnilsp_diagnostics[uri] or {}
      if #cleanDiagnostics ~= 0 then
        error("C/C++ source acquired " .. #cleanDiagnostics .. " diagnostics during client interactions")
      end
      row.status = "passed"
      return
    end

    local probe = "omnilspMissingSymbol"
    vim.api.nvim_buf_set_lines(buf, -1, -1, false, { "", diagnostic_probe(case) })
    local diagnosticTimeout = 15000
    if case.languageId == "rust" then
      local saved, saveErr = pcall(vim.api.nvim_buf_call, buf, function() vim.cmd("write") end)
      if not saved then error("could not save Rust diagnostic fixture: " .. tostring(saveErr)) end
      diagnosticTimeout = 60000
    end
    local diagnosticText = table.concat(vim.api.nvim_buf_get_lines(buf, 0, -1, false), "\n")
    local diagnosticPosition = position_for(diagnosticText, probe, 1)
    local deadline = vim.uv.now() + diagnosticTimeout
    local diagnosticItems = {}
    local diagnostic
    repeat
      vim.wait(100, function() return false end, 10)
      diagnosticItems = pull_diagnostics(client, uri, buf)
      diagnostic = find_expected_diagnostic(case, uri, probe, diagnosticPosition, diagnosticItems)
    until diagnostic ~= nil or vim.uv.now() >= deadline
    if not diagnostic then
      row.observed = row.observed or {}
      row.observed.semanticDiagnostics = diagnosticItems
      error("textDocument/diagnostic did not return the expected semantic diagnostic for " .. case.languageId)
    end
    row.diagnostic = {
      diagnostic_scope = "semantic_unresolved_name",
      severity = "error",
      source = diagnostic.source,
      code = diagnostic.code,
      message = diagnostic.message,
      start = { line = diagnostic.range.start.line, character = diagnostic.range.start.character },
      ["end"] = { line = diagnostic.range["end"].line, character = diagnostic.range["end"].character },
    }
    row.status = "passed"
  end, debug.traceback)

  if buf and vim.api.nvim_buf_is_valid(buf) then
    pcall(vim.api.nvim_buf_delete, buf, { force = true })
  end
  if not ok then
    row.status = "failed"
    row.error = tostring(err)
  end
  write_result()
end

local ok, err = xpcall(function()
  if not vim.env.OMNILSP_CLIENT_CASES or vim.env.OMNILSP_CLIENT_CASES == "" then
    error("OMNILSP_CLIENT_CASES is required")
  end
  local content = table.concat(vim.fn.readfile(vim.env.OMNILSP_CLIENT_CASES), "\n")
  local cases = vim.json.decode(content)
  local caseOnly = vim.trim(vim.env.OMNILSP_CLIENT_CASE_ONLY or "")
  local knownCase = caseOnly == ""
  for _, case in ipairs(cases) do
    if case.name == caseOnly then knownCase = true end
  end
  if not knownCase then error("OMNILSP_CLIENT_CASE_ONLY does not match a manifest case: " .. caseOnly) end
  for _, case in ipairs(cases) do
    if caseOnly == "" or case.name == caseOnly then
      run_case(case)
    else
      table.insert(result.cases, {
        name = case.name, family = case.family, languageId = case.languageId,
        status = "not_verified", reason = "not executed because OMNILSP_CLIENT_CASE_ONLY=" .. caseOnly,
      })
      write_result()
    end
  end

  local clients = vim.lsp.get_clients({ name = "omnilsp" })
  for _, client in ipairs(clients) do client:stop(false) end
  local stopped = vim.wait(10000, function()
    for _, client in ipairs(clients) do
      if vim.lsp.get_client_by_id(client.id) ~= nil then return false end
    end
    return true
  end, 25)
  if not stopped then error("OmniLSP language clients did not complete graceful shutdown") end
  result.serverExitConfirmed = true
  result.serverExitEvidence = "Neovim LSP client was removed after client:stop(false); Go runner separately verifies candidate/backend process exit"

  local failed = 0
  local unverified = 0
  for _, row in ipairs(result.cases) do
    if row.status == "failed" then failed = failed + 1 end
    if row.status == "not_verified" then unverified = unverified + 1 end
  end
  result.status = failed > 0 and "failed" or unverified > 0 and "not_verified" or "passed"
  if failed > 0 then result.error = string.format("%d of %d language cases failed", failed, #cases) end
  result.clientTestsCompleted = failed == 0
  -- The Go runner adds cleanExit only after the Neovim process returns code 0.
  result.cleanExit = false
  write_result()
  if failed > 0 then error(result.error) end
end, debug.traceback)

if not ok then
  result.status = "failed"
  result.cleanExit = false
  result.error = tostring(err)
  write_result()
  vim.cmd("cquit 1")
else
  vim.cmd("qa!")
end
