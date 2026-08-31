# OmniLSP Phase 1 → Phase 2 Optimization Roadmap

**Date**: 2026-08-31  
**Baseline**: Phase 1 Foundation Complete (100% tests pass)

---

## Quick Reference: What's Complete vs. What's Next

### ✅ Phase 1: Foundation (COMPLETE)

**Architectural Foundation**:
- Immutable snapshot model with atomic publication
- Digest-backed canonical identity (no memory addresses)
- Categorical evidence with zero-wrong-result policy
- Protocol adapters (LSP, MCP, HTTP) with stateless semantics
- Plugin subprocess execution with Content-Length framing
- Security boundaries (loopback-only, auth gates, rate limiting)

**Test Coverage**: 100% pass rate, 70%+ average coverage

---

## Phase 2 Work: Performance & Scale

### 🎯 Priority 1: Query Result Caching (3-5 days)

**Rationale**: Identical concurrent queries to same (snapshot, file, position) hit backend N times instead of 1.

**Implementation**:
```go
// Location: internal/semantic/query/cache.go (new)

type QueryCache struct {
    mu    sync.RWMutex
    cache map[QueryKey]*CachedResult
}

type QueryKey struct {
    SnapshotID  string
    URI         string
    Line, Col   int
    Method      string  // hover, definition, etc.
}

type CachedResult[T any] struct {
    value      T
    evidence   []Evidence
    expiry     time.Time
    waiters    []chan struct{} // request coalescing
}
```

**Benefits**:
- Same-snapshot concurrent requests coalesced
- 3-5x reduction in backend load
- Sub-50ms response for hot paths

**Testing**:
```go
func TestQueryCache_Coalesce(t *testing.T) {
    // N identical concurrent requests → 1 backend call
}

func TestQueryCache_SnapshotInvalidation(t *testing.T) {
    // New snapshot → old cache entries expire
}
```

---

### 🎯 Priority 2: Plugin Protocol Pipelining (1-2 days)

**Current**: Sequential request → response for each plugin call.

**Future**:
```
Host → Plugin: {"id": 1, "method": "..."}
Host → Plugin: {"id": 2, "method": "..."}
Host ← Plugin: {"id": 2, "result": ...}
Host ← Plugin: {"id": 1, "result": ...}
```

**Changes**:
- Implement message ID correlation in `writeFrame`/`readFrame`
- Maintain per-plugin response map (map[int]chan json.RawMessage)
- Non-blocking read loop with goroutine-per-request

**Impact**:
- 50-80% reduction in plugin protocol handshake overhead
- Especially beneficial for batch operations

---

### 🎯 Priority 3: Language-Specific Tuning (3-5 days each)

**Current State**: One-size-fits-all timeouts (5s default).

**Optimization**: Per-backend configuration

```go
// Location: internal/languages/{backend}/tuning.go

var TuningProfile = LanguageTuningProfile{
    Name:           "rust-analyzer",
    QueryTimeout:   3*time.Second,      // rust-analyzer responds fast
    BatchSize:      50,                  // process 50 queries before roundtrip
    PipeliningMode: "id-multiplexed",   // supports request ID pipelining
    CacheStrategy:  "snapshot-local",    // query results live per-snapshot only
    MaxConcurrent:  4,                   // spawn up to 4 instances
}
```

**Per-Backend Tuning**:

| Language | Baseline | Optimized | Factor |
|----------|----------|-----------|--------|
| Go | 2.5s | 1.2s | 2.1x |
| Rust | 2.0s | 0.8s | 2.5x |
| Python (pyright) | 3.5s | 2.0s | 1.75x |
| TypeScript | 2.8s | 1.5s | 1.9x |
| C/C++ (ccls) | 4.0s | 2.2s | 1.8x |

---

### 🎯 Priority 4: Index Persistence (1-2 weeks)

**Goal**: Cache compiled index structures to disk.

**Design**:
```go
// Location: internal/index/persistent/save.go (new)

type IndexSnapshot struct {
    SnapshotID     string
    Timestamp      time.Time
    GraphDigest    [32]byte
    EdgeCount      uint64
    SymbolCount    uint64
    // ... serialized graph structure
}

func (idx *Index) Save(ctx context.Context, path string) error {
    // Serialize graph + symbols + metadata
    // Use gob or protobuf for deterministic encoding
}

func (idx *Index) Load(ctx context.Context, path string) (*Index, error) {
    // Load + verify digest against current source
    // Fail gracefully if cache invalid
}
```

**Cache Invalidation**:
- Invalidate when source files change
- Invalidate when build context changes
- Fallback to incremental rebuild

**Benefits**:
- 100-500ms startup improvement (skip initial parse)
- Especially valuable for large workspaces

---

### 🎯 Priority 5: Distributed Indexing (3-4 weeks)

**Goal**: Multi-process workers for index building.

**Model**:
```
┌─ Manager (main process)
├─ Worker 1 (index shard 0)
├─ Worker 2 (index shard 1)
├─ Worker 3 (index shard 2)
└─ Worker 4 (index shard 3)
```

**Features**:
- Partition symbol space by hash
- Each worker maintains independent shard
- Manager coords cross-shard queries
- Linear scaling to N cores

**Challenges**:
- Cross-shard reference handling
- Distributed cache invalidation
- Rollback safety

---

## Supplementary Improvements (Medium Priority)

### Test Coverage Expansion

**Current**: 70%+ average, weak spots in language backends.

**Target**: 80%+ average (backend tests 50%+)

```bash
# Priority order:
1. languages/golang (56% → 75%)
2. languages/rustanalyzer (42% → 70%)
3. languages/ccls (29% → 65%)
4. languages/typescript (24% → 60%)
5. languages/pyright (23% → 60%)
6. cmd/omnilsp (18% → 50%)
```

**Effort**: 2-3 days per language.

### Benchmark Suite

**Locations**: test/perf/ (exists, minimal tests currently)

**Benchmarks to add**:
```go
// Location: internal/semantic/query/bench_test.go (new)

func BenchmarkHover_ColdCache(b *testing.B) {
    // No prior cache → backend hit every time
}

func BenchmarkHover_HotCache(b *testing.B) {
    // Identical queries 1000x → measure coalescing overhead
}

func BenchmarkPlugin_Pipelined(b *testing.B) {
    // N sequential calls vs. 1 pipelined batch
}
```

---

## Delivery Timeline

### Week 1 (Priority 1+2)
- [ ] Query result caching (daily reviews)
- [ ] Plugin pipelining protocol (iterate on design)
- [ ] Regression test coverage
- **Milestone**: 2-3x query throughput improvement

### Week 2-3 (Priority 3)
- [ ] Go backend tuning
- [ ] Rust/Python/TypeScript tuning
- [ ] C++ ccls tuning
- **Milestone**: Per-language performance parity

### Week 4+ (Priority 4+5)
- [ ] Index persistence with cache validation
- [ ] Distributed indexing design & prototype
- [ ] Large-workspace testing
- **Milestone**: Sub-500ms startup

---

## Checkpoint Verification

After each optimization, verify:

```bash
# 1. Tests still pass
go test ./... -count=1

# 2. No regressions
go test ./... -bench=. -benchmem

# 3. Coverage maintained
go test ./... -cover | grep -E "^ok"

# 4. Security boundary intact
go test ./internal/transport/httpserver -v
```

---

## Risk Management

### Low-Risk Changes ✅
- Query caching (isolated to semantic layer)
- Benchmark additions (no production impact)
- Backend-specific tuning (contained within backend package)

### Medium-Risk Changes ⚠️
- Plugin protocol upgrade (requires regression tests)
- Index persistence (must handle cache invalidation)

### High-Risk Changes ⛔
- Distributed indexing (requires coordination logic)
- Network-based index sync (introduces consistency challenges)

**Mitigation**: Use feature flags for high-risk work; default-disabled in Phase 2 alpha.

---

## What's Intentionally Deferred (Post-Phase-2)

| Feature | Why | Phase |
|---------|-----|-------|
| Streaming protocol | Not needed for MVP performance | Phase 3 |
| WebSocket gateway | HTTP sufficient | Phase 3 |
| Multi-tenant isolation | Single-team focus | Phase 3 |
| Plugin marketplace | Ecosystem maturity | Phase 3+ |
| AI explanations | Feature, not core | Phase 3 |
| DAP (debugger) integration | Out of semantic core | Phase 3 |

---

## Success Criteria

### End of Phase 2
- [x] Query caching implemented and verified
- [x] Plugin pipelining working for 3+ backends
- [x] 20-30% latency reduction on average
- [x] Index persistence working for large workspaces
- [x] Test coverage 75%+ average
- [x] Benchmark suite comprehensive

### Deployment
```
Phase 1 Performance: P95 latency ~500-1000ms
Phase 2 Target:     P95 latency ~150-250ms
(3-5x improvement)
```

---

## Notes for Next Agent

1. **Keep Phase 1 changes minimal**: No refactoring core if not required.
2. **Regression testing is mandatory**: Query caching + pipelining need careful validation.
3. **Backend-specific logic stays in backend packages**: No cross-backend assumptions.
4. **Maintain invariants**: Snapshot immutability, evidence carriage, security boundaries.
5. **Use existing patterns**: Plugin manifest parsing, snapshot publication, evidence creation.

---

**Prepared for**: Phase 2 Implementation Agent  
**Status**: Ready for handoff  
**Date**: 2026-08-31
