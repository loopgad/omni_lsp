# Version Surfaces (§R4)

Every externally visible version surface, its pin, and its compatibility
promise. This page is the machine-auditable carrier for registry check Y5-6.

| Surface | Version | Pin mechanism | Compatibility promise (§R5) |
|---|---|---|---|
| LSP wire baseline | 3.17 | `internal/protocol/lsp/types.go:LSPBaseline` + `protocol.manifest` fingerprint (`gen-protocol -check`) | additive-only; drift fails CI |
| MCP protocol revision | 2026-07-28 | `internal/protocol/mcp/server.go:ProtocolRevision` | pinned; new revision is a breaking release |
| Replay log format | v2 | ADR-0004 + `internal/replay/session.go:FormatVersion` | readers accept v1 and v2; a v1 recording loads but cannot carry response-identity evidence (replay returns `ErrSemanticResponseBindingUnverified`) |
| Plugin API | omnilsp.plugin.v1 | `internal/plugin/manifest.go:SupportedAPIVersion` (§O5) | host negotiates across listed versions during transitions |
| HTTP read API | omnilsp.read.v1 | `internal/transport/httpserver/server.go:ReadAPIVersion` + route table | additive routes only; removals need a deprecation cycle (§R8) |
| Go module | github.com/omnilsp/omni | `go.mod` + tags | semver from v1.0.0; core interface freeze per ADR-0009 |

Every row's Pin mechanism names a `path:Ident` Go constant so the table can be
checked mechanically against the source (see
`internal/conformance/versions_test.go`). Adding a version surface means adding
a row with a pinned constant in the same change.
