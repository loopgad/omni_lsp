# Zed client profile (§S11)

Zed's built-in language names for the TypeScript family are `TypeScript`,
`TSX`, `JavaScript`, and `JSX`. Configure each fixture name explicitly. Keep
the registered Zed adapter name (`gopls`, `clangd`, `rust-analyzer`,
`basedpyright`, or `vtsls`) in `language_servers`, then point that adapter's
binary at OmniLSP:

```json
{
  "languages": {
    "Go": { "language_servers": ["gopls"] },
    "C": { "language_servers": ["clangd"] },
    "C++": { "language_servers": ["clangd"] },
    "Rust": { "language_servers": ["rust-analyzer"] },
    "Python": { "language_servers": ["basedpyright"] },
    "TypeScript": { "language_servers": ["vtsls"] },
    "TSX": { "language_servers": ["vtsls"] },
    "JavaScript": { "language_servers": ["vtsls"] },
    "JSX": { "language_servers": ["vtsls"] }
  },
  "lsp": {
    "gopls": {
      "binary": {
        "path": "C:/absolute/path/to/omnilsp.exe",
        "arguments": ["serve"],
        "env": { "OMNILSP_TRUST": "trusted" }
      }
    },
    "clangd": {
      "binary": {
        "path": "C:/absolute/path/to/omnilsp.exe",
        "arguments": ["serve"],
        "env": { "OMNILSP_TRUST": "trusted" }
      }
    },
    "rust-analyzer": {
      "binary": {
        "path": "C:/absolute/path/to/omnilsp.exe",
        "arguments": ["serve"],
        "env": { "OMNILSP_TRUST": "trusted" }
      }
    },
    "basedpyright": {
      "binary": {
        "path": "C:/absolute/path/to/omnilsp.exe",
        "arguments": ["serve"],
        "env": { "OMNILSP_TRUST": "trusted" }
      }
    },
    "vtsls": {
      "binary": {
        "path": "C:/absolute/path/to/omnilsp.exe",
        "arguments": ["serve"],
        "env": { "OMNILSP_TRUST": "trusted" }
      }
    }
  }
}
```

Zed starts newly opened worktrees in Restricted Mode. In that mode it does
not apply `.zed/settings.json` or start project-configured language servers.
The smoke runner must use a disposable `--user-data-dir` and write this user
setting there before opening the fixture:

```json
{
  "session": { "trust_all_worktrees": true }
}
```

This trusts only worktrees opened under the disposable test profile. The
preflight also writes `OMNILSP_TRUST=trusted` into each adapter's binary
environment so OmniLSP receives its own explicit trust signal. Zed documents
the isolated worktree trust behavior in [Worktree Trust](https://zed.dev/docs/worktree-trust)
and the language names/adapters in its [TypeScript language guide](https://zed.dev/docs/languages/typescript).
OmniLSP runs over the adapter's stdio transport (C13). The profile selects one
adapter per language and replaces that adapter's binary with the frozen
candidate, so a bundled language server cannot compete for the document.

Run the profile preflight after the fixture workspace and frozen candidate
have been created:

```powershell
python test/acceptance/clients/zed/driver.py `
  --workspace <fixture-workspace> `
  --user-data-dir <disposable-zed-user-data> `
  --omnilsp-bin <frozen-candidate> `
  --zed-bin <pinned-stable-zed> `
  --result <evidence-directory>/client-zed-result.json
```

The preflight validates all nine Zed language entries, adapter executable
paths, arguments, server trust environment, and isolated worktree trust. That
checks settings only; it does not establish the `languageId` Zed sends in
`textDocument/didOpen`. In the upstream `vtsls` adapter source reviewed here,
`TypeScript`, `JavaScript`, and `TSX` are mapped, while `JSX` has no mapping
([source, `main`, lines 1601–1613](https://github.com/zed-industries/zed/blob/main/crates/languages/src/vtsls.rs#L1601-L1613)).
Keep JSX `not_verified` until the pinned client’s `didOpen` confirms
`javascriptreact` or a supported adapter mapping is added. All native smoke
rows remain `not_verified`: acceptance still requires editor actions,
client-visible hover, definition, completion, diagnostics, references,
refused rename, unchanged source, and clean shutdown. A server trace alone
cannot pass this check.

**Current result (historical, 2026-10-07):** this host previously had Zed
Preview `1.19.0` (`f69e805e36a2ffcd63fd39285e07738f452a1687`) at
`D:\Programs\Zed Preview\bin\Zed.exe`; that local artifact has since been
cleaned up (verified absent on 2026-10-07) and it was not the proposed stable
pin. Zed's installer identity is not yet recorded in the acceptance tool lock,
so per the missing→`not_verified` policy in `docs/acceptance.md` ("Fixed host
tools") the rows stay `not_verified`: a native re-run requires reinstalling
the pinned client, recording its identity and SHA-256 in the tool lock, and
passing the identity check. No native UI action/result automation has been
verified. The helper keeps all nine cases `not_verified`. Its
`settings_contract` result is separate from the unverified language ID
mapping and native result consumption.

A collision rename must be visibly refused and leave both the editor buffer
and fixture file unchanged; the app and OmniLSP process tree must also exit
cleanly before any language row can pass.

**Pinned runtime:** use stable Zed `1.21.0` from its [official release page](https://zed.dev/releases/stable/1.21.0).
The Windows x86_64 installer identity and checksum still need to be recorded
in the acceptance tool lock before a passing run.
