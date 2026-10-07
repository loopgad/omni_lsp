# Helix client profile (§S11)

Helix merges `.helix/languages.toml` entries with its built-in language
definitions. Keep Helix's language name separate from the LSP `language-id`:
the built-in TSX and JSX definitions use `tsx` and `jsx`, while their LSP IDs
are `typescriptreact` and `javascriptreact`. The pinned Helix 25.07.1 source
has these mappings in its [built-in language profile](https://github.com/helix-editor/helix/blob/25.07.1/languages.toml).

The acceptance driver writes this profile into the disposable fixture
workspace. It binds the candidate executable directly, selects only OmniLSP,
and sets the server trust environment explicitly:

```toml
[language-server.omnilsp]
command = "C:/absolute/path/to/omnilsp.exe"
args = ["serve"]
environment = { "OMNILSP_TRUST" = "trusted" }

[[language]]
name = "go"
language-id = "go"
language-servers = ["omnilsp"]

[[language]]
name = "c"
language-id = "c"
language-servers = ["omnilsp"]

[[language]]
name = "cpp"
language-id = "cpp"
language-servers = ["omnilsp"]

[[language]]
name = "rust"
language-id = "rust"
language-servers = ["omnilsp"]

[[language]]
name = "python"
language-id = "python"
language-servers = ["omnilsp"]

[[language]]
name = "typescript"
language-id = "typescript"
language-servers = ["omnilsp"]

[[language]]
name = "tsx"
language-id = "typescriptreact"
language-servers = ["omnilsp"]

[[language]]
name = "javascript"
language-id = "javascript"
language-servers = ["omnilsp"]

[[language]]
name = "jsx"
language-id = "javascriptreact"
language-servers = ["omnilsp"]
```

Run the profile preflight with the locked Python runtime after the fixture
workspace and frozen candidate have been created:

```powershell
$env:OMNILSP_TRUST = 'trusted'
python test/acceptance/clients/helix/driver.py `
  --workspace <fixture-workspace> `
  --omnilsp-bin <frozen-candidate> `
  --result <evidence-directory>/client-helix-result.json
```

The preflight parses the generated TOML and checks all nine language IDs,
server selection, candidate path, and trust environment. It always reports
the native smoke as `not_verified`: a Helix TUI driver must trigger editor
actions and capture the rendered hover, definition, completion, diagnostics,
references, and refused-rename results. An LSP trace by itself is insufficient.
The default Helix bindings include `Space-k` for hover, `gd` for definition,
`gr` for references, `Ctrl-x` in insert mode for completion, `Space-d` for
diagnostics, and `Space-r` for rename, as listed in the [Helix keymap](https://docs.helix-editor.com/keymap.html).
The client negotiates UTF-16 position encoding during initialize (C4). A
refused collision rename must be visible in Helix and leave both the buffer
and the fixture file unchanged.

**Current result (Windows/amd64, 2026-10-01, historical):** the locked Helix
`25.07.1` binary and runtime were present under `test/acceptance/tools/bin`
at the time of the check; they have since been cleaned up (verified absent on
2026-10-07 — only the `helix-smoke` helper module remains there), while the
acceptance tool lock still records the `25.07.1` identity and SHA-256. Per
the missing→`not_verified` policy in `docs/acceptance.md` ("Fixed host
tools"), a native re-run requires reinstalling Helix and passing the recorded
SHA-256 check. Profile validation passed at the time. A real Helix TTY
session opened the Go fixture with the
diagnostic candidate (`SHA256 7966854AEFB6218CC350D2AF21B3E0FF377357FAE365F1E9650F0238D3E40F8B`):
`Space-k` rendered the `SoakTarget` signature, `gd` moved to its definition at
line 3, and `:q` returned exit code 0. This is manual evidence of Helix
consuming Go hover and definition results; it does not verify the other
features, language rows, or a frozen acceptance build. The native acceptance
driver still fails closed and reports all nine rows `not_verified` until it
can automate and capture the complete required result set. A separate ConPTY
control probe did not capture child terminal output and was discarded; it is
not evidence for or against Helix behavior.

**Pinned runtime:** Helix `25.07.1` for x86_64 Windows is available from the
[official release](https://github.com/helix-editor/helix/releases/tag/25.07.1)
and is recorded in the acceptance tool lock. It can be used for passing native
evidence once the TUI driver consumes and captures its results.
