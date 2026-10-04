# Sublime Text client profile

Sublime Text needs the community-maintained LSP package for LSP support. Use
the LSP package's server configuration (for example, in
`Packages/User/LanguageServers.sublime-settings`):

```json
{
  "omnilsp": {
    "enabled": true,
    "command": ["omnilsp", "serve"],
    "selector": "source.go | source.c | source.c++ | source.rust | source.python | source.ts | source.tsx | source.js | source.jsx",
    "schemes": ["file"]
  }
}
```

Install the package into Sublime's isolated portable profile from the pinned
official [Sublime LSP release](https://github.com/sublimelsp/LSP/releases).
The profile must use the pinned package directly, not a floating Package
Control update.

**Acceptance rule:** use Sublime's LSP client and editor API to open and edit
the fixture. A test plugin can issue Sublime's `lsp_hover`,
`lsp_symbol_definition`, `lsp_symbol_references`, and
`lsp_symbol_rename` [commands](https://lsp.sublimetext.io/keyboard_shortcuts/),
then observe actual results through the LSP package's documented
[`LspPlugin` response/notification hooks](https://lsp.sublimetext.io/migrating_to_lsp_plugin/)
and Sublime's view/panel state. Completion must be invoked through the editor's
autocomplete UI. The LSP package response hook only reports successful
responses; the typed rename refusal is consumed through the locked package's
native `Session` error callback after the rename command is invoked. Record both
directions of the actual server stdio stream and require clean client/server
exit. A trace request by itself is insufficient; the unsafe rename must be
refused and leave the buffer and file unchanged.

**Current result:** the opt-in acceptance driver is implemented in
`test/acceptance/clients/sublime/driver.py`. It invokes Sublime LSP commands,
checks results through the package response/notification hooks, and waits for
the server session exit callback. Sublime Text Build `4215` and the LSP
`4070-2.13.0` package archive are staged under
`test/acceptance/tools/bin`, and Build 4215 starts and reports its version.
The portable profile does not yet contain the LSP package or its dependencies,
so the driver has not run against the native client and all rows remain
`not_verified`.

The pinned package archive's `dependencies.json` requires these Sublime
Package Control libraries for builds `>=4096`: `bracex`, `mdpopups`, `orjson`,
`typing_extensions`, and `wcmatch`. Lock their exact package artifacts and
hashes before installing them in the isolated portable profile; the Python
module names alone are not a dependency lock.

**Host check (Windows/amd64, 2026-10-01):** the official [download
page](https://www.sublimetext.com/download) provides stable Build `4215` and a
Windows x64 portable option. The upstream package archive is
`4070-2.13.0` from the [official LSP release
page](https://github.com/sublimelsp/LSP/releases). Both archives have been
downloaded and the editor extracted locally; their versions are recorded in
`test/acceptance/tools/bin`, while the central acceptance lock and the five
package dependency artifacts still need to be completed before a native run.
