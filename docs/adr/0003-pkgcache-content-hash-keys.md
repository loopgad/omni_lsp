# ADR 0003: Package Cache Keys Use File Path Plus Content Hash

- **Status:** Accepted
- **Date:** 2026-08-23
- **Deciders:** OmniLSP core maintainers
- **Relates to:** goal.md §K0 (cache layers), §K1 (cache key contract), §E0/§E7 (build context identity), §U9 (ADRs)

## Context

The native Go backend (`internal/languages/golang/backend.go`, see
ADR 0001) type-checks packages through `golang.org/x/tools/go/packages`.
Loading a package is expensive — `packages.Load` invokes the go tool,
parses, and type-checks — so results are cached in the backend's
`pkgCache`. goal.md §K1 requires that any semantic cache key include every
input that can affect semantics; a key that omits a semantic input produces
stale or wrong answers served with fresh-looking evidence, which violates
the grounding guarantees of §A3.

Inputs that affect Go semantics for an open file:

- **File content.** Any byte change can change types, and also changes
  token offsets — reusing a cached `token.FileSet` entry for different
  content would misalign every position.
- **File path / package identity.** The same bytes at a different path can
  belong to a different package, with different imports and scope.

§K0 additionally requires every cache layer to be bounded, versioned, and
observable: bounded (no unbounded growth across a long session), observable
(hit/miss visibility so cache health is diagnosable), invalidatable and
evictable.

## Decision

The golang backend's package cache key is:

```text
key = filePath + "|" + hex(sha256(content)[:8])
```

implemented in `Backend.loadPackage` (`internal/languages/golang/backend.go`).

Key completeness rationale per §K1:

- The SHA-256 content digest covers the source text: identical hash implies
  identical content implies identical fset offsets, making cached reuse of
  the `*packages.Package` position-safe.
- The file path covers package identity: the same buffer contents under a
  different path resolve to a different package and never collide on the
  key.
- Build-context identity (toolchain, GOOS/GOARCH, GOFLAGS/GOWORK per §E0/§E7)
  is deliberately not part of this key: it is derived once per backend
  instance (`buildCtxOnce`) and stamped into evidence via
  `BuildContextID`; a backend instance lives within one build context.
  A ponytail note records this scoping decision at the derivation site.

Boundedness and observability per §K0:

- `pkgCache` is capped at `pkgCacheLimit = 64` entries with FIFO eviction
  via the `pkgOrder` slice.
- `cacheHits` / `cacheMisses` atomic counters are exposed through
  `CacheStats()` and surfaced by the server's `omnilsp/status` handler as
  aggregate `CacheHits`/`CacheMisses` fields, satisfying K-layer
  observability for all backends implementing the optional interface.

### Known Trade-off

The digest is truncated to 8 bytes (64 bits). Collision probability is
~2^-64 under random hashing — negligible over an LSP session's cache
lifetime and eviction horizon. If future requirements demand more margin
(e.g., persistent cross-session caches where silent collision is
unrecoverable), widen the truncation; the key format is internal to the
backend and not a wire format.

## Consequences

### Positive

- Correct-by-construction invalidation: edit ⇒ new hash ⇒ guaranteed miss;
  no explicit invalidation protocol to get wrong.
- Position safety is structural — no separate fset-reuse bug class.
- Hit/miss counters make cache effectiveness visible in `omnilsp/status`.

### Negative / Risks

- Every keystroke changes the hash, so typing causes misses by design; the
  64-entry FIFO bounds memory but does not smooth churn. If profiling shows
  load cost dominating interactive latency, add an L1 syntax layer in front
  rather than weakening the key.
- Truncated hashes must never be reused as global identifiers outside this
  map; they are cache keys, not content addresses.
