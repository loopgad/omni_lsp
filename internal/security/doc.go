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
// This file intentionally contains no imports — a previous version imported
// core packages here and claimed that proved the invariant, which it did not.
package security
