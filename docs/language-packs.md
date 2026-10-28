
## Backend version matrix (§S12)

Compatibility policy per goal.md §R6: minimum = oldest toolchain the bridge
exercises in CI; recommended = what the team dogfoods; max tested = newest
known-good at last verification round. Beyond "max tested" is unsupported
until re-verified.

| Backend | Bridge class | Min | Recommended | Max tested | Notes |
|---|---|---|---|---|---|
| Go | native in-process (golang.org/x/tools) | go1.26.1 | go1.26 | go1.27.1 | toolchain-missing fallback: completion degrades to the go/parser syntax tier (EvidenceL1), type-tier queries report Unknown (§A3) |
| C/C++ | clangd nested LSP | 14 | 17 | 22 | compile_commands.json required for project-wide rename (§X3) |
| Rust | rust-analyzer nested LSP | 2024-01 | latest stable | latest stable | Cargo.toml gates project rename |
| Python | pyright-langserver nested LSP | 1.1.330 | latest | latest | venv discovery via workdir |
| TypeScript/JS | typescript-language-server nested LSP | 4.0 | latest | latest | tsconfig.json presence gates project scope |

The 2026-10-26 tool re-lock (test/acceptance/tools/tools.lock.json) moved the
acceptance host to go1.27.1 and clangd 22.1.5; the "Max tested" entries above
track that lock. The strict S18 latency targets have not been re-measured under
go1.27.1 — the recorded strict-S18 evidence was collected on the go1.26.1
locked candidate — so the Go bump is lock-observed, not S18-re-verified.

Deferred languages carry explicit registry rows (E2/E3 multi-context, E11/H6
Java, H7 Tier A, H8 data languages) — see internal/conformance/registry.go
and ADR-0008.
