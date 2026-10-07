# OmniLSP — Universal Language Intelligence Platform

A production-grade, multi-language Language Server and Language Intelligence
Platform built in Go. One binary serves Go, C/C++, Rust, Python, and
TypeScript/JavaScript over stdio or TCP, speaking LSP 3.17, with an MCP
surface and a read-only HTTP API alongside the editor protocol.

## Status

Release candidate `v0.2.0`. The per-item conformance ledger — what is proven
by an executable probe, what is partial, and what is explicitly deferred with
a reason — is generated into [docs/conformance.md](docs/conformance.md). The
repeatable release-candidate gate runbook is
[docs/acceptance.md](docs/acceptance.md). Passing individual unit or
conformance tests does not imply a language/client combination passed; check
both documents before claiming RC readiness.

Implemented and verified by registry probes:

- **Language packs**: a native in-process Go backend (`go/packages` type
  checking), plus nested-LSP bridges to clangd (C/C++), rust-analyzer (Rust),
  pyright (Python), and typescript-language-server (TS/JS) — matrix and
  toolchain floors in [docs/language-packs.md](docs/language-packs.md)
- **Protocol**: JSON-RPC 2.0 over stdio/TCP, UTF-8/16/32 position encoding
  negotiation, LSP 3.17 baseline, MCP server, HTTP read API with auth gate
  and rate limiting
- **Correctness core**: immutable snapshots (one per request), bounded
  scheduler with end-to-end cancellation, build-context identity,
  evidence-tagged results that never fabricate semantics, fail-closed
  rename/edit gates
- **Persistent intelligence**: transactional semantic index serving
  fresh-only workspace symbols/definitions/references, scoped SCIP
  import/export, JSONL replay format v2
- **Operations**: `doctor` environment probes, `verify` conformance
  scorecard, out-of-process plugins behind trust gates

Remaining work — the gRPC read-API projection, the 42-cell real-client
matrix, Java and Tier A languages, LSIF command integration, and more —
carries explicit registry rows with reasons in
[docs/conformance.md](docs/conformance.md). Nothing is silently missing.

## Architecture

A single binary layered around a snapshot core:

- `internal/runtime/server` — JSON-RPC dispatch, scheduler admission,
  per-request snapshot capture, evidence-carrying result envelopes
- `internal/languages/*` — one backend per language behind a frozen
  12-method interface (ADR-0009); Go is native in-process (ADR-0001), the
  others bridge nested language servers and normalize their output
- `internal/semantic` + `internal/index` — incremental query engine
  (memoization, singleflight, freshness gates) over a transactional
  persistent index
- `internal/transport` — stdio, TCP, and the HTTP read API
- `internal/trust`, `internal/security`, `internal/plugin` — workspace trust
  gates, path containment, secret redaction, plugin lifecycle

Architecture decisions are recorded as ADRs in [docs/adr/](docs/adr/).

## Building

Requires the Go toolchain pinned in `go.mod`; runtime dependencies beyond the
standard library are limited to the whitelisted `golang.org/x/tools` and the
SCIP bindings.

```sh
go build ./...    # compile everything
make fast         # gofmt + vet + build + verify scorecard
```

Windows has no `make` by default; use `mingw32-make` with the same targets
(see the Makefile header).

## Testing

```sh
make test                          # fmt + vet + build + full suite + verify
make race                          # the mode CI runs: go test -race -count=1 ./...
make short                         # skip packages that shell out to language toolchains
go run ./cmd/omnilsp verify --min 90   # conformance scorecard against the 90 floor
```

`docs/conformance.md` is generated from `internal/conformance/registry.go`;
regenerate with `OMNISP_UPDATE_CONFORMANCE=1 go test ./test/conformance -run
TestGenerateDocs`. Committed drift fails that test.

## Usage

```sh
omnilsp serve      # LSP over stdio (default); --transport tcp --addr ... for TCP
omnilsp doctor     # environment probes (PASS/WARN/FAIL/SKIP per toolchain)
omnilsp verify     # conformance scorecard (text or --json)
omnilsp index      # import/export a verified persistent semantic scope (SCIP)
omnilsp replay     # replay a recorded JSONL session
omnilsp repro      # build a reproducible bug-report bundle (.zip)
omnilsp plugins    # list/validate out-of-process plugins
omnilsp version
```

Editor setup lives in [docs/editors/](docs/editors/) (Neovim guide, client
profiles); the VS Code extension is under `editors/vscode/`.

## Key Design Principles

1. **Unknown > Wrong**: Return explicit unknown rather than incorrect results
2. **Immutable Snapshots**: All requests bound to a single snapshot revision
3. **Compiler-grade Semantics**: Prefer compiler-native backends over heuristics
4. **Crash Isolation**: Backend failures do not crash the core
5. **Bounded Resources**: All queues, caches, and goroutines are bounded
