# OmniLSP VS Code Extension

Minimal language client for the OmniLSP platform.

## Install (development)

```bash
cd editors/vscode
npm install
npx vsce package   # produces omnilsp-0.2.0.vsix
code --install-extension omnilsp-0.2.0.vsix
```

The extension expects the `omnilsp` binary on PATH (or set `omnilsp.path`).
Build it from the repository root: `go build -o omnilsp.exe ./cmd/omnilsp`.

## Features

- Full LSP surface served by omnilsp: hover, definition, references,
  completion, rename (fail-closed), document symbols.
- **OmniLSP: Explain Last Result** — dumps the §I25 evidence ring
  (kind/assurance/snapshot/build-context/backend per recent query) into an
  output channel, so every answer is auditable.
