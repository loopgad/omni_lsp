# Zed Client Profile (§S11)

`~/.config/zed/settings.json`:

```json
{
  "languages": {
    "Go": {
      "language_servers": ["!gopls", "omnilsp"]
    }
  },
  "language_servers": {
    "omnilsp": {
      "binary": {
        "path": "/usr/local/bin/omnilsp",
        "arguments": ["serve"]
      }
    }
  }
}
```

Notes:

- Zed requires an explicit binary path; `omnilsp serve` uses stdio (C13).
- The `!gopls` prefix disables the built-in server so omnilsp owns the
  language; remove it to compare servers side-by-side.
- Fail-closed rename refusals render as inline error decorations — see the
  conformance map Y0 row for why this is intended behavior.
