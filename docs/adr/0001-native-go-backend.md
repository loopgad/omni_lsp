# ADR 0001: Native In-Process Go Backend Instead of a gopls Bridge

- **Status:** Accepted
- **Date:** 2026-08-23
- **Deciders:** OmniLSP core maintainers
- **Relates to:** goal.md §H1.1 (source of truth strategy), §H1.2 (Go semantic keys), §U9 (ADRs), §A3 (no fabricated results), §X2 (differential corpus exit criterion)

## Context

goal.md §H1.1 states that the initial production Go backend SHOULD use
`gopls` as a compiler-grade semantic worker, and that a future native
in-process backend MAY use `go/parser`, `go/types`, and
`golang.org/x/tools/go/packages` — but MUST NOT become the default until
differential tests show acceptable parity against the pinned canonical
backend for required features.

OmniLSP is a multi-language LSP platform that ships as a single binary.
The architecture must decide where Go language semantics come from: an
out-of-process `gopls` instance spoken to over nested LSP, or an in-process
implementation built directly on the standard-library toolchain packages.

The decision materially affects the language backend source of truth, so
§U9 requires an ADR.

## Decision

The Go backend (`internal/languages/golang/backend.go`) is implemented as a
**native in-process backend** using:

- `go/parser` / `go/ast` / `go/token` for syntax-level fallback evidence;
- `go/types` via `golang.org/x/tools/go/packages` for type-resolved semantics;
- `golang.org/x/tools/go/packages` (`packages.Load` with overlay support) as
  the driver, so unsaved buffer content is type-checked exactly as it stands
  in the editor.

It deliberately does **not** spawn or proxy a `gopls` process. The backend
implements the full `languages.Backend` surface — hover, definition,
completion, references, document/workspace symbols, diagnostics, semantic
tokens, rename — and stamps results with L3 ("types-resolved") evidence
whose provenance comes straight from the type checker, never from a nested
protocol hop.

Rationale:

1. **Single-binary distribution with zero external toolchain coupling.**
   No `gopls` binary needs to be located, versioned, downloaded, or
   supervised; users without a full Go installation still get syntax-level
   evidence via the `go/parser` fallback rather than a dead backend.
2. **Fully controllable evidence chain.** Every result's Evidence record
   (SourceHash, BackendEpoch, BuildContextID per §E0/§E7) is produced inside
   the same process that answers the request. With a gopls bridge, L3 claims
   would rest on trusting another server's responses over a nested-LSP hop,
   weakening the grounding guarantees of §A3.
3. **No nested-LSP protocol overhead.** Bridging would add a second JSON-RPC
   session, translation layers for URI schemes and positions, and lifecycle
   supervision complexity, all to reach the same underlying `go/types` data
   that `go/packages` already exposes in-process.

## Consequences

### Positive

- One binary, no sidecars; startup and shutdown are trivially deterministic.
- Evidence records are first-party and auditably grounded in the type checker.
- Cache keys can be controlled end-to-end per §K1 (see ADR 0003).
- The `go/parser` fallback degrades gracefully instead of failing hard when
  no toolchain is present.

### Negative / Risks

- We own feature parity with `gopls` ourselves. Advanced navigation
  features — call hierarchy, incoming/outgoing calls, type hierarchy,
  full-fidelity workspace-wide rename — may lag behind what `gopls`
  provides today.
- Rename completeness across module boundaries depends entirely on our
  `packages.Load` usage patterns.
- Toolchain drift (new Go releases) must be tracked by us, not inherited
  from upstream `gopls`.

### Parity Commitment (gate for this decision)

§H1.1 forbids a native backend from becoming default until differential
tests show acceptable parity. Our standing commitment:

- `TestCorpus_GoFilesProduceGroundedSemantics`
  (`internal/languages/golang/corpus_test.go`) runs every corpus file under
  `test/corpus/testdata/go` in its own isolated backend/workspace and fails
  unless hover is non-empty, definitions resolve, and evidence carries a
  digest-backed build context. This differential corpus is the regression
  gate for the native backend and MUST keep passing before any release.
- **Migration trigger:** if rename completeness or required advanced
  features (call hierarchy and similar) fall demonstrably short of what a
  compiler-grade worker provides, we will open a follow-up ADR evaluating a
  gopls bridge for those capabilities, per §H1.1's original guidance.
  Until that evaluation concludes, parity gaps are handled by narrowing the
  advertised capability set, not by fabricating results (§A3).
