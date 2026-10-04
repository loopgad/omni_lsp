const fs = require("fs");
const path = require("path");
const vscode = require("vscode");

const result = {
  client: "vscode",
  status: "running",
  stage: "test runner loaded",
  cleanExit: false,
  clientTestsCompleted: false,
  serverExitConfirmed: false,
  cases: [],
};

function writeResult() {
  const target = process.env.OMNILSP_CLIENT_RESULT;
  if (!target) return;
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, `${JSON.stringify(result, null, 2)}\n`);
}

async function runStep(row, stage, action, timeoutMs = 60000) {
  row.stage = stage;
  result.stage = `${row.name}: ${stage}`;
  writeResult();
  let timer;
  try {
    return await Promise.race([
      Promise.resolve().then(action),
      new Promise((_, reject) => {
        timer = setTimeout(() => reject(new Error(`${stage} exceeded ${timeoutMs}ms`)), timeoutMs);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

function commandStep(row, stage, command, ...args) {
  return runStep(row, stage, () => vscode.commands.executeCommand(command, ...args));
}

function findPosition(text, symbol, occurrence = 1, characterOffset = 0) {
  let offset = -1;
  let searchFrom = 0;
  for (let index = 0; index < occurrence; index += 1) {
    const next = text.indexOf(symbol, searchFrom);
    if (next < 0) throw new Error(`fixture does not contain occurrence ${occurrence} of ${symbol}`);
    offset = next;
    searchFrom = next + symbol.length;
  }
  const before = text.slice(0, offset);
  const line = (before.match(/\n/g) || []).length;
  const lastNewline = before.lastIndexOf("\n");
  return new vscode.Position(line, offset - lastNewline - 1 + characterOffset);
}

function evidenceCursor(response, uri, method) {
  if (!response || !Array.isArray(response.evidence)) {
    throw new Error(`omnilsp/explain returned no evidence array for ${method}`);
  }
  return new Set(response.evidence
    .filter((item) => item.method === method && item.uri === uri)
    .map((item) => item.at)
    .filter((at) => typeof at === "string" && at.length > 0));
}

function assertMethodEvidence(response, uri, method, backendLanguage, previousEvidenceAt) {
  if (!response || !Array.isArray(response.evidence)) {
    throw new Error(`omnilsp/explain returned no evidence for ${method}`);
  }
  if (!(previousEvidenceAt instanceof Set)) {
    throw new Error(`missing prior evidence cursor for ${method}`);
  }
  const entry = response.evidence.find((item) =>
    item.method === method && item.uri === uri &&
    typeof item.at === "string" && item.at.length > 0 && !previousEvidenceAt.has(item.at)
  );
  if (!entry) throw new Error(`no fresh OmniLSP evidence recorded for ${method} on ${uri}`);
  if (typeof entry.backend !== "string" || !entry.backend.startsWith(`${backendLanguage}/`)) {
    throw new Error(`${method} evidence backend ${JSON.stringify(entry.backend)} does not start with ${backendLanguage}/`);
  }
  return entry;
}

function assertEvidence(response, uri, backendLanguage, previousEvidenceAt) {
  return assertMethodEvidence(response, uri, "textDocument/hover", backendLanguage, previousEvidenceAt);
}

async function waitUntil(predicate, timeoutMs, description) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  throw new Error(`timed out waiting for ${description}`);
}

function missingTools(testCase) {
  let status = {};
  if (process.env.OMNILSP_CLIENT_TOOL_STATUS) {
    status = JSON.parse(process.env.OMNILSP_CLIENT_TOOL_STATUS);
  }
  return (testCase.requiredTools || []).filter((tool) => status[tool] !== true);
}

function completionItems(value) {
  if (Array.isArray(value)) return value;
  if (value && Array.isArray(value.items)) return value.items;
  return [];
}

function hoverContainsSymbol(value, symbol) {
  if (!Array.isArray(value)) return false;
  return value.some((hover) => {
    const contents = Array.isArray(hover.contents) ? hover.contents : [hover.contents];
    const text = contents.map((item) => {
      if (typeof item === "string") return item;
      return item && typeof item.value === "string" ? item.value : "";
    }).join("\n");
    return text.includes(symbol);
  });
}

async function waitForHover(document, position, symbol, timeoutMs = 45000) {
  const deadline = Date.now() + timeoutMs;
  let last = [];
  while (Date.now() < deadline) {
    last = await vscode.commands.executeCommand(
      "vscode.executeHoverProvider", document.uri, position
    ) || [];
    if (hoverContainsSymbol(last, symbol)) return last;
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  return last;
}

function locationAt(location, expectedUri, expectedPosition) {
  if (!location) return false;
  const uri = location.uri || location.targetUri;
  const range = location.range || location.targetSelectionRange || location.targetRange;
  return Boolean(uri && range && range.start && uri.toString() === expectedUri.toString() &&
    range.start.line === expectedPosition.line && range.start.character === expectedPosition.character);
}

function hasWorkspaceEdits(edit) {
  if (!(edit instanceof vscode.WorkspaceEdit)) return false;
  return edit.entries().some(([, edits]) => edits.length > 0);
}

function renameCollisionName(testCase) {
  switch (testCase.name) {
    case "go":
    case "c":
    case "cpp": return "UseTarget";
    case "rust":
    case "python": return "use_target";
    case "typescript":
    case "javascript": return "useTarget";
    case "typescriptreact":
    case "javascriptreact": return "useComponent";
    default: throw new Error(`no known collision target for ${testCase.name}`);
  }
}

// VS Code's executeDocumentRenameProvider command converts provider rejections
// into a plain Error and drops ResponseError.code. This check validates the
// exact UI-boundary refusal; wire error codes are covered by protocol and Neovim checks.
function expectedRenameRefusal(testCase, message) {
  const normalized = String(message || "").trim().toLowerCase();
  if (testCase.family === "go") {
    return normalized === "rename refused: symbol is exported; importer packages are not loaded, so reference completeness cannot be proven (sem-safe-001)";
  }
  if (testCase.family === "cpp") {
    return normalized === "rename refused: c/c++ function/method collision analysis is not proven (sem-safe-001)" ||
      normalized === "rename refused: c/c++ target is outside the current document; collision analysis is not proven (sem-safe-001)";
  }
  if (testCase.family === "typescript") {
    return normalized === "rename refused: typescript/javascript function/method collision analysis is not proven (sem-safe-001)" ||
      normalized === "rename refused: typescript/javascript target is outside the current document; collision analysis is not proven (sem-safe-001)";
  }
  if (testCase.family === "python") {
    return normalized === "rename refused: upstream language service returned no edits; safety and completeness are not proven (sem-safe-001)";
  }
  return normalized === "upstream language service refused rename";
}

function diagnosticProbe(testCase) {
  const name = "omnilspMissingSymbol";
  switch (testCase.languageId) {
    case "go": return `var _ = ${name}`;
    case "c":
    case "cpp": return `int clientDiagnosticProbe = ${name};`;
    case "rust": return `fn client_diagnostic_probe() { let _ = ${name}; }`;
    case "python": return `client_diagnostic_probe = ${name}`;
    case "typescript":
    case "typescriptreact": return `export const clientDiagnosticProbe: number = ${name};`;
    case "javascript":
    case "javascriptreact": return `export const clientDiagnosticProbe = ${name};`;
    default: throw new Error(`no diagnostic probe for ${testCase.languageId}`);
  }
}

function findExpectedDiagnostic(testCase, diagnostics, document, probe) {
  const offset = document.getText().lastIndexOf(probe);
  if (offset < 0) throw new Error(`diagnostic probe did not contain ${probe}`);
  const expectedStart = document.positionAt(offset);
  const expectedEnd = document.positionAt(offset + probe.length);
  const normalizedProbe = probe.toLowerCase();
  const specs = {
    go: {
      source: "omnilsp-go",
      matches: (message) => message === `undefined: ${probe}`,
      code: null,
    },
    rust: {
      source: "rustc",
      matches: (message) => message.toLowerCase().includes("cannot find value `" + normalizedProbe + "` in this scope"),
      code: "E0425",
    },
    python: {
      source: "Pyright",
      // Pyright localizes its message (the runner host may be Chinese); exact
      // source, code, severity, and range below identify this unresolved name.
      matches: (message) => message.toLowerCase().includes(normalizedProbe),
      code: "reportUndefinedVariable",
    },
    typescript: {
      source: "typescript",
      matches: (message) => message.toLowerCase().includes("cannot find name") &&
        message.toLowerCase().includes(`'${normalizedProbe}'`),
      code: "2304",
    },
  };
  const spec = specs[testCase.family];
  if (!spec) throw new Error(`no diagnostic expectation for ${testCase.family}`);
  return diagnostics.find((item) => {
    const code = item.code && typeof item.code === "object" ? item.code.value : item.code;
    return item.severity === vscode.DiagnosticSeverity.Error &&
      item.source === spec.source &&
      (spec.code === null || String(code) === spec.code) &&
      typeof item.message === "string" && spec.matches(item.message) &&
      item.range.start.isEqual(expectedStart) && item.range.end.isEqual(expectedEnd);
  });
}

async function runCase(testCase, extension) {
  const row = {
    name: testCase.name,
    family: testCase.family,
    languageId: testCase.languageId,
    status: "running",
  };
  result.cases.push(row);
  result.stage = `${testCase.name}: case started`;
  writeResult();
  let listener;
  let document;
  try {
    const missing = missingTools(testCase);
    if (missing.length > 0) {
      row.status = "not_verified";
      row.reason = `locked prerequisites unavailable: ${missing.join(", ")}`;
      return;
    }

    const filePath = path.join(vscode.workspace.workspaceFolders[0].uri.fsPath, testCase.file);
    const uri = vscode.Uri.file(filePath);
    let diagnosticsNotifications = 0;
    listener = vscode.languages.onDidChangeDiagnostics((event) => {
      if (event.uris.some((changed) => changed.toString() === uri.toString())) {
        diagnosticsNotifications += 1;
      }
    });
    const cleanDiagnosticsNotificationsBeforeOpen = diagnosticsNotifications;
    document = await runStep(row, "open document", () => vscode.workspace.openTextDocument(uri));
    if (document.languageId !== testCase.languageId) {
      throw new Error(
        `VS Code inferred languageId ${document.languageId} for ${testCase.file}; expected ${testCase.languageId}`,
      );
    }
    await runStep(row, "show document", () => vscode.window.showTextDocument(document, { preview: false, preserveFocus: false }));

    if (!extension.isActive) {
      await runStep(row, "wait for extension activation", () =>
        waitUntil(() => extension.isActive, 15000, `${testCase.languageId} activation event`), 20000);
    }

    if (testCase.family === "cpp") {
      await runStep(row, "wait for clean-source diagnostics", () => waitUntil(
        () => diagnosticsNotifications > cleanDiagnosticsNotificationsBeforeOpen,
        15000,
        `clean-source publishDiagnostics for ${testCase.languageId}`
      ), 20000);
      const cleanDiagnostics = vscode.languages.getDiagnostics(document.uri);
      if (cleanDiagnostics.length !== 0) {
        throw new Error(`clean C/C++ source unexpectedly produced ${cleanDiagnostics.length} diagnostics`);
      }
      row.diagnostic = {
        diagnostic_scope: "clean_source_no_false_positive",
        deferred_capability: "DEF-CCLSDIAG",
        published_count: 0,
      };
    }

    const originalText = document.getText();
    const definitionPosition = findPosition(originalText, testCase.symbol, 1);
    const usePosition = findPosition(originalText, testCase.symbol, 2);
    const initialHoverCursor = evidenceCursor(
      await commandStep(row, "initial hover evidence baseline", "omnilsp.explain"),
      document.uri.toString(), "textDocument/hover"
    );
    const firstHover = await runStep(row, "initial hover", () =>
      waitForHover(document, definitionPosition, testCase.symbol));
    if (!hoverContainsSymbol(firstHover, testCase.symbol)) {
      const explain = await commandStep(row, "explain after unexpected hover", "omnilsp.explain");
      row.observed = { hover: firstHover, explain };
      throw new Error(`hover provider did not describe ${testCase.symbol} before deadline`);
    }
    const beforeExplain = await commandStep(row, "initial explain", "omnilsp.explain");
    const evidence = assertEvidence(beforeExplain, document.uri.toString(), testCase.backendLanguage, initialHoverCursor);
    const beforeSnapshot = Number(beforeExplain.snapshot || evidence.snapshotRev || 0);

    const previousVersion = document.version;
    const syncComment = testCase.languageId === "python" ? "\n# client-sync-probe\n" : "\n// client-sync-probe\n";
    const change = new vscode.WorkspaceEdit();
    change.insert(document.uri, document.positionAt(document.getText().length), syncComment);
    if (!(await runStep(row, "synchronization edit", () => vscode.workspace.applyEdit(change)))) {
      throw new Error("editor rejected the synchronization probe edit");
    }
    await runStep(row, "wait for synchronized document version", () =>
      waitUntil(() => document.version > previousVersion, 10000, "document version after edit"), 15000);
    if (!document.getText().includes("client-sync-probe")) {
      throw new Error("edited text was not retained in the open document");
    }

    const afterHoverCursor = evidenceCursor(
      await commandStep(row, "post-edit hover evidence baseline", "omnilsp.explain"),
      document.uri.toString(), "textDocument/hover"
    );
    const afterHover = await commandStep(row, "hover after edit", "vscode.executeHoverProvider", document.uri, definitionPosition);
    if (!hoverContainsSymbol(afterHover, testCase.symbol)) {
      throw new Error(`hover did not describe ${testCase.symbol} after document synchronization`);
    }
    const afterExplain = await commandStep(row, "explain after edit", "omnilsp.explain");
    const afterEvidence = assertEvidence(afterExplain, document.uri.toString(), testCase.backendLanguage, afterHoverCursor);
    const afterSnapshot = Number(afterExplain.snapshot || afterEvidence.snapshotRev || 0);
    if (afterSnapshot <= beforeSnapshot) {
      throw new Error(`server snapshot did not advance after editor change (${beforeSnapshot} -> ${afterSnapshot})`);
    }

    const definitionCursor = evidenceCursor(afterExplain, document.uri.toString(), "textDocument/definition");
    const definitions = await commandStep(row, "definition", "vscode.executeDefinitionProvider", document.uri, usePosition);
    if (!Array.isArray(definitions) || !definitions.some((location) => locationAt(location, document.uri, definitionPosition))) {
      throw new Error(`definition provider returned no location for ${testCase.symbol}`);
    }
    const definitionExplain = await commandStep(row, "definition explain", "omnilsp.explain");
    assertMethodEvidence(definitionExplain, document.uri.toString(), "textDocument/definition", testCase.backendLanguage, definitionCursor);

    const completionPosition = findPosition(originalText, testCase.symbol, 2, 5);
    const completions = completionItems(await commandStep(row, "completion", "vscode.executeCompletionItemProvider", document.uri, completionPosition));
    if (!completions.some((item) => String(item.label || item.insertText || "").includes(testCase.symbol))) {
      throw new Error(`completion did not offer ${testCase.symbol}`);
    }
    const referencesCursor = evidenceCursor(definitionExplain, document.uri.toString(), "textDocument/references");
    const references = await commandStep(row, "references", "vscode.executeReferenceProvider", document.uri, usePosition);
    if (!Array.isArray(references) || references.length < 2 ||
      !references.some((location) => locationAt(location, document.uri, definitionPosition)) ||
      !references.some((location) => locationAt(location, document.uri, usePosition))) {
      throw new Error(`references returned fewer than declaration and use for ${testCase.symbol}`);
    }
    const referencesExplain = await commandStep(row, "references explain", "omnilsp.explain");
    assertMethodEvidence(referencesExplain, document.uri.toString(), "textDocument/references", testCase.backendLanguage, referencesCursor);

    let renameRejected = false;
    const collisionName = renameCollisionName(testCase);
    const textBeforeRename = document.getText();
    const diskSourceBeforeRename = fs.readFileSync(filePath);
    const renameCursor = evidenceCursor(referencesExplain, document.uri.toString(), "textDocument/rename");
    try {
      const proposed = await commandStep(row, "unsafe rename", "vscode.executeDocumentRenameProvider", document.uri, definitionPosition, collisionName);
      if (proposed instanceof vscode.WorkspaceEdit && hasWorkspaceEdits(proposed)) {
        throw new Error(`unsafe rename returned edits for colliding name ${collisionName}`);
      }
      throw new Error(`unsafe rename did not return a RequestFailed refusal for ${collisionName}`);
    } catch (error) {
      if (String(error && error.message ? error.message : error).startsWith("unsafe rename ")) throw error;
      const message = String(error && error.message ? error.message : error);
      if (!expectedRenameRefusal(testCase, message)) {
        throw new Error(`unsafe rename refusal did not match the expected typed refusal (code=${String(error && error.code)}, message=${message})`);
      }
      renameRejected = true;
      row.observed = row.observed || {};
      row.observed.rename_refusal = {
        message,
        vscode_command_code: error && error.code !== undefined ? error.code : null,
      };
    }
    if (!renameRejected) throw new Error("unsafe rename was not refused");
    if (document.getText() !== textBeforeRename) {
      throw new Error("document source changed after the refused unsafe rename");
    }
    if (!fs.readFileSync(filePath).equals(diskSourceBeforeRename)) {
      throw new Error("source file on disk changed after the refused unsafe rename");
    }
    const renameExplain = await commandStep(row, "rename explain", "omnilsp.explain");
    assertMethodEvidence(renameExplain, document.uri.toString(), "textDocument/rename", testCase.backendLanguage, renameCursor);

    const diagnosticNotificationsBefore = diagnosticsNotifications;
    if (testCase.family === "cpp") {
      const currentDiagnostics = vscode.languages.getDiagnostics(document.uri);
      if (currentDiagnostics.length !== 0) {
        throw new Error(`C/C++ clean source acquired ${currentDiagnostics.length} diagnostics during client interactions`);
      }
      row.status = "passed";
      return;
    }

    const probe = "omnilspMissingSymbol";
    const diagnosticLine = diagnosticProbe(testCase);
    const diagnosticDocumentVersion = document.version;
    const diagnosticEdit = new vscode.WorkspaceEdit();
    diagnosticEdit.insert(document.uri, document.positionAt(document.getText().length), `\n${diagnosticLine}\n`);
    if (!(await runStep(row, "diagnostic probe edit", () => vscode.workspace.applyEdit(diagnosticEdit)))) {
      throw new Error("editor rejected the semantic diagnostics probe edit");
    }
    row.observed = row.observed || {};
    const diagnosticProbeEvidence = {
      uri: document.uri.toString(),
      document_version_before: diagnosticDocumentVersion,
      document_version_after_edit: document.version,
      dirty_after_edit: document.isDirty,
      save_requested: false,
    };
    row.observed.diagnostic_probe = diagnosticProbeEvidence;
    const diagnosticTimeout = testCase.languageId === "rust" ? 60000 : 20000;
    if (testCase.languageId === "rust") {
      diagnosticProbeEvidence.save_requested = true;
      const saved = await runStep(row, "save Rust diagnostic fixture", () => document.save(), diagnosticTimeout);
      diagnosticProbeEvidence.save_returned = saved;
      diagnosticProbeEvidence.document_version_after_save = document.version;
      diagnosticProbeEvidence.dirty_after_save = document.isDirty;
      if (!saved) throw new Error("editor could not save the Rust diagnostic fixture");
    }
    const diagnosticPredicate = () => {
      const current = vscode.languages.getDiagnostics(document.uri);
      if (diagnosticsNotifications <= diagnosticNotificationsBefore) return false;
      return findExpectedDiagnostic(testCase, current, document, probe) !== undefined;
    };
    await runStep(row, "wait for semantic diagnostics", () =>
      waitUntil(diagnosticPredicate, diagnosticTimeout, `expected semantic publishDiagnostics result for ${testCase.languageId}`), diagnosticTimeout + 5000);
    const publishedDiagnostics = vscode.languages.getDiagnostics(document.uri);
    const diagnostic = findExpectedDiagnostic(testCase, publishedDiagnostics, document, probe);
    if (!diagnostic) throw new Error(`publishDiagnostics did not report the expected ${testCase.family} unresolved-name error for ${probe}`);
    row.diagnostic = {
      diagnostic_scope: "semantic_unresolved_name",
      severity: "error",
      source: diagnostic.source,
      code: diagnostic.code,
      message: diagnostic.message,
      start: { line: diagnostic.range.start.line, character: diagnostic.range.start.character },
      end: { line: diagnostic.range.end.line, character: diagnostic.range.end.character },
    };

    row.status = "passed";
  } catch (error) {
    row.status = "failed";
    row.error = String(error && error.stack ? error.stack : error);
  } finally {
    if (listener) listener.dispose();
    if (document) {
      try {
        await runStep(row, "close active editor", () => vscode.commands.executeCommand("workbench.action.closeActiveEditor"), 15000);
      } catch (cleanupError) {
        row.cleanupError = String(cleanupError && cleanupError.message ? cleanupError.message : cleanupError);
        row.status = "failed";
        row.error = [row.error, row.cleanupError].filter(Boolean).join("; ");
      }
    }
    writeResult();
  }
}

async function run() {
  result.stage = "load acceptance cases";
  writeResult();
  const casesPath = process.env.OMNILSP_CLIENT_CASES;
  if (!casesPath) throw new Error("OMNILSP_CLIENT_CASES is required");
  const cases = JSON.parse(fs.readFileSync(casesPath, "utf8"));
  const caseOnly = String(process.env.OMNILSP_CLIENT_CASE_ONLY || "").trim();
  const selectedCases = new Set(caseOnly ? caseOnly.split(",").map((name) => name.trim()).filter(Boolean) : []);
  const unknownCases = [...selectedCases].filter((name) => !cases.some((testCase) => testCase.name === name));
  if (unknownCases.length > 0) {
    throw new Error(`OMNILSP_CLIENT_CASE_ONLY does not match manifest cases: ${unknownCases.join(", ")}`);
  }
  const manifest = JSON.parse(
    fs.readFileSync(path.resolve(__dirname, "../../../../editors/vscode/package.json"), "utf8")
  );
  const advertised = ["go", "c", "cpp", "rust", "python", "typescript", "typescriptreact", "javascript", "javascriptreact"];
  for (const languageId of advertised) {
    if (!manifest.activationEvents.includes(`onLanguage:${languageId}`)) {
      throw new Error(`extension is missing activation event for ${languageId}`);
    }
  }
  const extension = vscode.extensions.getExtension("omnilsp.omnilsp");
  if (!extension) throw new Error("development extension omnilsp.omnilsp was not found");
  for (const testCase of cases) {
    if (selectedCases.size === 0 || selectedCases.has(testCase.name)) {
      await runCase(testCase, extension);
    } else {
      result.cases.push({
        name: testCase.name,
        family: testCase.family,
        languageId: testCase.languageId,
        status: "not_verified",
        reason: `not executed because OMNILSP_CLIENT_CASE_ONLY=${caseOnly}`,
      });
      writeResult();
    }
    const last = result.cases[result.cases.length - 1];
    if (last && last.status === "failed" && /exceeded \d+ms/.test(last.error || "")) break;
  }
  const failed = result.cases.filter((row) => row.status === "failed");
  const unverified = result.cases.filter((row) => row.status === "not_verified");
  result.status = failed.length > 0 ? "failed" : unverified.length > 0 ? "not_verified" : "passed";
  if (failed.length > 0) result.error = `${failed.length} language cases failed`;
  result.clientTestsCompleted = failed.length === 0;
  await runStep({ name: "runner" }, "close all editors", () => vscode.commands.executeCommand("workbench.action.closeAllEditors"), 15000);
  // The Go runner records cleanExit only after the VS Code process and the
  // candidate/backend process images have actually exited.
  result.cleanExit = false;
  writeResult();
  if (failed.length > 0) throw new Error(result.error);
}

exports.run = async () => {
  try {
    await run();
  } catch (error) {
    result.status = "failed";
    result.cleanExit = false;
    result.error = String(error && error.stack ? error.stack : error);
    writeResult();
    throw error;
  }
};
