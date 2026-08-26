# Version Surfaces (§R4)

Every externally visible version surface, its pin, and its compatibility
promise. This page is the machine-auditable carrier for registry check Y5-6.

| Surface | Version | Pin mechanism | Compatibility promise (§R5) |
|---|---|---|---|
| LSP wire baseline | 3.17 | `internal/protocol/lsp/types.go` + `protocol.manifest` fingerprint (`gen-protocol -check`) | additive-only; drift fails CI |
| MCP protocol revision | 2026-07-28 | `mcpserver.ProtocolRevision` const | pinned; new revision is a breaking release |
| Replay log format | v1 | ADR-0004 + `replay` package | readers accept all recorded v1 files forever |
| Plugin API | omnilsp.plugin.v1 | `SupportedAPIVersions` range (§O5) | host negotiates across listed versions during transitions |
| HTTP read API | omnilsp.read.v1 | route table in `transport/httpserver` | additive routes only; removals need a deprecation cycle (§R8) |
| Go module | github.com/omnilsp/omni | go.mod + tags | semver from v1.0.0; core interface freeze per ADR-0009 |
