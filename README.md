# OmniLSP - Universal Language Intelligence Platform

A production-grade, high-precision, multi-language Language Server and Language Intelligence Platform built in Go.

## Status

**Phase 1 - Foundation** (Current)

Core infrastructure is implemented and passing all tests:

- **JSON-RPC 2.0**: Request/response/notification handling, Content-Length framing
- **Transport**: stdio and TCP transports with proper shutdown semantics
- **Position Engine**: UTF-8/UTF-16/UTF-32 conversion with property-based tests
- **VFS**: Virtual file system with editor overlay precedence
- **Snapshot**: Immutable snapshot model with atomic publication
- **Scheduler**: Bounded priority queue with cancellation propagation
- **Error Taxonomy**: Structured errors with kind, operation, and context
- **LSP Types**: Core protocol types (Position, Range, Location, etc.)
- **LSP Server**: initialize, didOpen/Change/Save/Close, handler dispatch
- **CLI**:  and 

## Architecture



## Building



## Testing



## Usage



## Key Design Principles

1. **Unknown > Wrong**: Return explicit unknown rather than incorrect results
2. **Immutable Snapshots**: All requests bound to a single snapshot revision
3. **Compiler-grade Semantics**: Prefer compiler-native backends over heuristics
4. **Crash Isolation**: Backend failures do not crash the core
5. **Bounded Resources**: All queues, caches, and goroutines are bounded

## Next Steps (Phase 2)

- Go language backend (completion, hover, definition, references)
- C/C++ backend via clangd bridge
- Persistent index
- gRPC and HTTP gateways
- MCP server for AI agents

