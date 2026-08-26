# Helix Client Profile (§S11)

`languages.toml`:

```toml
[[language]]
name = "go"
language-servers = ["omnilsp"]

[language-server.omnilsp]
command = "omnilsp"
args = ["serve"]
```

Notes:

- Helix drives one server per language; omnilsp registers its language
  packs internally, so a single entry per primary language suffices.
- Position encoding: UTF-16 negotiated at initialize (C4).
- Rename refusals (fail-closed) surface as `ShowMessage` errors — expected
  behavior without compile context, not a client misconfiguration.
