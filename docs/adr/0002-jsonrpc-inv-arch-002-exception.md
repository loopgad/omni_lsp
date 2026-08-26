# ADR 0002: INV-ARCH-002 Exception — internal/protocol/jsonrpc

- **Status:** Accepted
- **Date:** 2026-08-23
- **Deciders:** OmniLSP core maintainers
- **Supersedes policy text in:** `internal/security/doc.go` (upgraded from documentation to formal decision record)
- **Relates to:** goal.md §U1 (INV-ARCH-002), §U9 (ADRs), §U10 (spec amendment process)

## Context

goal.md **INV-ARCH-002** mandates:

> Semantic Core MUST NOT import LSP-, MCP-, HTTP-, WebSocket-, editor-, or
> CLI-specific packages.

Concretely, the packages forbidden as transitive dependencies of
`internal/workspace/*`, `internal/languages/*`, and semantic runtime
components are:

```text
internal/protocol/lsp     (LSP wire types)
internal/protocol/mcp     (future)
internal/protocol/dap     (future)
internal/transport        (transports)
internal/runtime/server   (protocol adapter boundary)
```

The intent is architectural: the semantic core must remain a pure library
that can be embedded in an LSP server, an MCP server, a CLI, or tests,
without dragging protocol or transport dependencies into every consumer.
The full rationale is documented in `internal/security/doc.go`.

However, the codebase contains one deliberate exception:
`internal/protocol/jsonrpc` — the generic JSON-RPC 2.0 framing/decoding
mechanism. It is imported by semantic-side components (for example, the
ccls leaf backend adapter uses it for nested-LSP framing, and the replay
package records its messages). Without a formally sanctioned exception,
every such import is a standing violation of INV-ARCH-002, and reviewers
must re-litigate the question each time it appears.

## Decision

**INV-ARCH-002 has exactly one sanctioned exception:
`internal/protocol/jsonrpc`.**

This package implements only the generic JSON-RPC 2.0 mechanism: message
envelope parsing, codec, and dispatch plumbing. It carries no LSP, MCP, or
DAP domain semantics — no document, diagnostic, hover, completion, position,
or resource types of any kind.

The boundary is precise:

1. `internal/protocol/jsonrpc` MUST NOT define or import any domain types
   (`document`, `diagnostic`, `hover`, etc.). It knows about JSON-RPC
   requests, responses, notifications, and errors — nothing else.
2. Semantic core packages MAY import `internal/protocol/jsonrpc` for
   framing purposes only; they MUST NOT import any other protocol,
   transport, or adapter package listed above.
3. If a change ever requires jsonrpc to know domain types, or requires a
   second exception, that change violates this ADR's decision and MUST be
   recorded as a new ADR per goal.md §U10 before landing.

Enforcement remains as described in `internal/security/doc.go`: Go cannot
express negative import constraints at compile time, so CI runs
`go list -deps` over core packages and fails on any forbidden match other
than the sanctioned `internal/protocol/jsonrpc`.

This ADR upgrades the existing prose in `internal/security/doc.go` to a
formal architecture decision record under §U9, so that the exception is a
recorded, reviewable decision rather than tribal knowledge.

## Consequences

### Positive

- The single import path shared by core components and adapters is legal,
  explicit, and auditable instead of being a perpetual rule violation.
- Reviewers have a bright-line test: "does this patch make jsonrpc aware of
  domain types?" If yes, it needs a new ADR.
- The semantic core keeps its embeddability guarantee — it can sit behind
  LSP, MCP, DAP, or CLI frontends without protocol coupling.

### Negative / Risks

- The exception is a load-bearing crack in the invariant: if domain types
  ever leak into `jsonrpc`, they become transitively importable by the whole
  core, silently eroding the boundary. CI enforcement must therefore also
  watch jsonrpc's own dependency set, not just who imports it.
- New transports or protocols (MCP, DAP) may be tempted to reuse the
  exception rather than write their own framing; that temptation must be
  refused at review time — the exception covers JSON-RPC 2.0 framing only.
