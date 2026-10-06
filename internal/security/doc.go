// Package security documents the dependency-direction policy for OmniLSP.
//
// INV-ARCH-002: Semantic Core MUST NOT import LSP-, MCP-, HTTP-, WebSocket-,
// editor-, or CLI-specific packages (goal.md §U1). Concretely, these packages
// are forbidden as transitive dependencies of internal/workspace/*,
// internal/languages/*, and semantic runtime components:
//
//	internal/protocol/lsp     (LSP wire types)
//	internal/protocol/mcp     (future)
//	internal/protocol/dap     (future)
//	internal/transport        (transports)
//	internal/runtime/server   (protocol adapter boundary)
//
// Single sanctioned exception: internal/protocol/jsonrpc — a generic JSON-RPC
// 2.0 framing/decoding mechanism with no LSP semantic types. Leaf backend
// adapters (e.g., ccls) may reuse it for nested-LSP framing.
//
// Enforcement: Go cannot express negative import constraints at compile time.
// CI runs `go list -deps` over core packages and fails on any forbidden match:
//
//	go list -deps ./internal/workspace/... ./internal/languages/... \
//	  ./internal/runtime/scheduler ./internal/runtime/supervisor \
//	  | grep -E 'internal/(protocol/(lsp|mcp|dap)|transport|runtime/server)' \
//	  && exit 1 || exit 0
//
// That check covers workspace, languages, scheduler and supervisor.
// TestARCH002_NoProtocolImportsInCore in internal/conformance covers a
// different set -- semantic, identity, scheduler and workspace -- from the
// parsed import list rather than the dependency graph. The two are
// complementary: neither package set is covered by the other alone, so both
// must agree with the forbidden set listed above, which is also why the test's
// list names the forbidden packages individually instead of banning
// internal/protocol wholesale and accidentally rejecting the sanctioned jsonrpc
// framing package.
//
// This file intentionally contains no imports — a previous version imported
// core packages here and claimed that proved the invariant, which it did not.
package security
