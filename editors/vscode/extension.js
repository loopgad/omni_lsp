// OmniLSP VS Code extension: launches the omnilsp binary over stdio and
// bridges the omnilsp/explain evidence chain into a command.
const vscode = require("vscode");
const { LanguageClient } = require("vscode-languageclient/node");

let client;

async function activate(context) {
  const cfg = () => vscode.workspace.getConfiguration("omnilsp");
  const serverOpts = {
    command: cfg().get("path", "omnilsp"),
    // The client library adds --stdio for TransportKind.stdio; OmniLSP uses
    // --transport stdio and communicates over stdio when the transport is omitted.
    args: ["serve", "--transport", "stdio"],
    options: {
      // A workspace-relative server process is required for project markers,
      // trust and persistent indexing to resolve against the open folder.
      cwd: vscode.workspace.workspaceFolders?.[0]?.uri.fsPath,
    },
  };
  const clientOpts = {
    documentSelector: [
      { language: "go", scheme: "file" },
      { language: "c", scheme: "file" },
      { language: "cpp", scheme: "file" },
      { language: "rust", scheme: "file" },
      { language: "python", scheme: "file" },
      { language: "typescript", scheme: "file" },
      { language: "typescriptreact", scheme: "file" },
      { language: "javascript", scheme: "file" },
      { language: "javascriptreact", scheme: "file" },
    ],
    synchronize: { configurationSection: "omnilsp" },
  };
  client = new LanguageClient("omnilsp", "OmniLSP", serverOpts, clientOpts);
  await client.start();

  const channel = vscode.window.createOutputChannel("OmniLSP Evidence");
  context.subscriptions.push(channel);

  context.subscriptions.push(
    vscode.commands.registerCommand("omnilsp.explain", async () => {
      if (!client) return undefined;
      const params = {}; // no URI filter: full recent evidence ring
      const res = await client.sendRequest("omnilsp/explain", params);
      const lines = (res?.evidence ?? []).map(
        (e) =>
          `${e.method} ${e.uri} — ${e.kind}/${e.assurance}` +
          ` snap=${e.snapshotRev} ctx=${e.buildContext} backend=${e.backend}` +
          (e.diagnostics?.length ? ` ⚠ ${e.diagnostics.join("; ")}` : "")
      );
      channel.clear();
      channel.appendLine(lines.length ? lines.join("\n") : "No recorded evidence yet.");
      channel.show();
      return res;
    })
  );
}

function deactivate() {
  return client?.stop();
}

module.exports = { activate, deactivate };
