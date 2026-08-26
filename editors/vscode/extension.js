// OmniLSP VS Code extension: launches the omnilsp binary over stdio and
// bridges the omnilsp/explain evidence chain into a command.
const { LanguageClient, TransportKind } = require("vscode-languageclient/node");

let client;

function activate(context) {
  const cfg = () => vscode.workspace.getConfiguration("omnilsp");
  const serverOpts = {
    command: cfg().get("path", "omnilsp"),
    args: ["serve"],
    transport: TransportKind.stdio,
  };
  const clientOpts = {
    documentSelector: [
      { language: "go", scheme: "file" },
      { language: "c", scheme: "file" },
      { language: "cpp", scheme: "file" },
    ],
    synchronize: { configurationSection: "omnilsp" },
  };
  client = new LanguageClient("omnilsp", "OmniLSP", serverOpts, clientOpts);
  client.start();

  context.subscriptions.push(
    vscode.commands.registerCommand("omnilsp.explain", async () => {
      if (!client) return;
      const params = {}; // no URI filter: full recent evidence ring
      const res = await client.sendRequest("workspace/executeCommand", {
        command: "omnilsp/explain",
        arguments: [params],
      });
      const lines = (res?.evidence ?? []).map(
        (e) =>
          `${e.method} ${e.uri} — ${e.kind}/${e.assurance}` +
          ` snap=${e.snapshotRev} ctx=${e.buildContext} backend=${e.backend}` +
          (e.diagnostics?.length ? ` ⚠ ${e.diagnostics.join("; ")}` : "")
      );
      const channel = vscode.window.createOutputChannel("OmniLSP Evidence");
      channel.clear();
      channel.appendLine(lines.length ? lines.join("\n") : "No recorded evidence yet.");
      channel.show();
    })
  );
}

function deactivate() {
  return client?.stop();
}

const vscode = require("vscode");
module.exports = { activate, deactivate };
