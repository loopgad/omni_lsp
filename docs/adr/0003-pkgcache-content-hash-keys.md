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

> **Amendment (cache key format, added later).** The decision above stands —
> path identity plus a content digest still partition the key, and cached
> packages are still position-safe by construction. The formula itself moved
> and strengthened with the freshness-probe work (cc43348): the key is now
> built by the shared `packageCacheKey`
> (`internal/languages/golang/package_freshness.go`) as
> `pathKey(filePath)|snapshotRev|hex(sha256(content))|buildContext`. Three
> deltas from the original formula:
>
> - **The digest is the full SHA-256.** The 8-byte truncation is gone: on a
>   key hit the cached package is served directly, and the post-hit
>   fingerprint check re-derives from the cached package itself, so a
>   truncated-digest collision would serve a package built from different
>   source and nothing downstream would catch it. At full digest width that
>   residual is cryptographically negligible.
> - **The snapshot revision is in the key.** The effective compile input of a
>   request is the whole snapshot overlay set — other open buffers ride
>   `cfg.Overlay` via `packages.Load` — and those vary independently of the
>   target file's own bytes. The revision partitions cache entries per
>   snapshot, so a package loaded under one snapshot is never reused to
>   answer another.
> - **The build-context ID is in the key.** The original text below
>   deliberately left build-context identity to the instance-scoping
>   invariant ("one backend instance lives within one build context"). That
>   invariant still holds (`buildCtxOnce`), but the key is now
>   self-contained per §K1's letter — every semantic input is carried
>   explicitly instead of relying on the scoping assumption — and freshness
>   evidence refuses to certify entries whose context ID is
>   `go:sha256:unavailable`, so the ID was already load-bearing on the
>   evidence side.
>
> The §K0 boundedness story is unchanged: the same 64-entry FIFO and
> hit/miss counters apply to the widened key.

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

*(Amended — see the decision amendment above. The original trade-off
argument below applied to the 8-byte truncated digest and is obsolete since
the key moved to the full SHA-256.)*

With the truncation gone, the collision argument no longer bounds anything:
at full SHA-256 width a content-digest collision is negligible for any
practical horizon, so the key's correctness rests on the digest rather than
on an eviction-time probability budget. The live trade-offs are churn and
width: every keystroke still changes the hash (by design, see Consequences),
and the wider key — now also carrying the snapshot revision and the
build-context ID — costs more memory per entry against the same 64-entry
FIFO. The original rule that truncated hashes must never be reused as global
identifiers is superseded: the full digest may be treated as a content
address, though the key remains internal to the backend, not a wire format.

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
  map; they are cache keys, not content addresses. *(Amendment 2026-10-29:
  the digest is no longer truncated — see Known Trade-off; the full-digest
  key remains internal to the backend.)*
