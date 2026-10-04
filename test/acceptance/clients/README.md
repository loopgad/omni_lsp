# Real editor acceptance

The client gate drives nine filetype cases through real VS Code, Neovim and Emacs/Eglot
clients: Go, C, C++, Rust, Python, TypeScript, TSX, JavaScript, and JSX. The
structured report retains ten VS Code/Neovim family summaries and requires
six clients by seven languages in its 42-cell matrix. Helix, Zed and Sublime
remain unverified until their complete native evidence passes. Each
runnable subcase opens and edits a document, proves the server snapshot
advanced, checks hover, definition, completion, and references, and rejects a
collision rename only when it receives the expected JSON-RPC `RequestFailed`
response and leaves source text unchanged.

For Go, Rust, Python, and TypeScript/JavaScript, the harness also injects an
intentionally unresolved fixture symbol and validates the client-visible
diagnostic. C/C++ unresolved-symbol diagnostics are not validated because
clangd `publishDiagnostics` forwarding is deferred (`DEF-CCLSDIAG`). C/C++
cases instead verify a clean-source diagnostics notification with zero
entries, and report that deferred scope explicitly. Every client case then
closes cleanly.

Tool availability is evaluated independently. Missing pinned tools produce
`not_verified` rows for only the affected family/client, and do not stop other
available combinations from running. A `not_verified` row still makes the
gate exit nonzero; skips are not passes. The test uses the frozen binary from
`OMNILSP_BIN` and the shared run ID from `OMNILSP_RUN_ID`.

Run the pinned tool setup first from the repository root:

```powershell
./scripts/install-acceptance-tools.ps1
npm ci --prefix editors/vscode
```

Then run the gate:

```powershell
$env:OMNILSP_RUN_CLIENT_ACCEPTANCE = '1'
$env:OMNILSP_RUN_ID = '<runId>'
$env:OMNILSP_BIN = 'test/acceptance/evidence/<runId>/omnilsp.exe'
$env:OMNILSP_ACCEPTANCE_REPORT = 'test/acceptance/evidence/<runId>/clients.json'
# Optional override must resolve to the locked executable under test/acceptance/tools.
$env:OMNILSP_NVIM_BIN = 'test/acceptance/tools/bin/nvim-0.12.5/nvim-win64/bin/nvim.exe'
go test -tags clients -run '^TestRealClientMatrix$' -count=1 -timeout 30m ./test/acceptance/clients/
```

For diagnosis, `OMNILSP_CLIENT_ONLY` may be set to `vscode` or `neovim`, and
`OMNILSP_CLIENT_CASE_ONLY` may select one case (`go`, `c`, `cpp`, `rust`,
`python`, `typescript`, `typescriptreact`, `javascript`, or `javascriptreact`).
The report marks every omitted client/case as `not_verified`, so filtered runs
cannot pass the full client gate.

The npm servers, wrappers, and Neovim executable stay under
`test/acceptance/tools`; the VS Code extension uses its project-local
`vscode-languageclient` dependency. The harness never installs editor tools
globally.
