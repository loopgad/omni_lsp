# OmniLSP — Industrial Multi-language Language Intelligence Platform

## Canonical Engineering Specification

> **Document role**: This file is the single normative engineering specification for OmniLSP.
>
> **Primary implementation language**: Go.
>
> **Primary objective**: maximize semantic correctness, consistency, recoverability, observability, and long-term maintainability before feature count or raw throughput.
>
> **Canonical rule**: if this specification conflicts with a README, code comment, old design note, issue description, generated document, or historical prompt, **this specification wins unless an accepted ADR explicitly amends it**.

---

# Part A — Specification Governance and Product Contract

## A0. Normative language

The keywords **MUST**, **MUST NOT**, **REQUIRED**, **SHALL**, **SHALL NOT**, **SHOULD**, **SHOULD NOT**, **RECOMMENDED**, **MAY**, and **OPTIONAL** are normative.

Interpretation:

| Keyword             | Meaning                  | Review treatment                      |
| ------------------- | ------------------------ | ------------------------------------- |
| MUST / MUST NOT     | hard invariant           | violation blocks merge/release        |
| SHOULD / SHOULD NOT | default engineering rule | deviation requires written rationale  |
| MAY / OPTIONAL      | permitted extension      | must not weaken MUST-level invariants |

Every implementation-facing requirement SHOULD have one or more of:

- a unit/property/integration test;
- a fuzz target;
- a differential test;
- a release gate;
- an observable metric;
- an ADR when the implementation choice is intentionally left open.

## A1. Product definition

OmniLSP is:

**Language Intelligence Runtime + Multi-language Semantic Gateway + Incremental Query/Index Engine + Evidence Validation Runtime**

It is not a collection of protocol proxies.

The canonical flow is:

```text
IDE / Editor / Agent / CLI / Remote Client
                  │
                  ▼
        Protocol Adapter Boundary
      ┌───────────┼────────────┐
      │           │            │
     LSP         MCP      gRPC / HTTP
      │           │            │
      └───────────┼────────────┘
                  ▼
          Canonical Request API
                  │
                  ▼
       Admission + Scheduler
                  │
                  ▼
        Immutable Snapshot
                  │
       ┌──────────┼───────────┐
       ▼          ▼           ▼
      VFS      Build Set    Query DB
       │          │           │
       └──────────┼───────────┘
                  ▼
           Semantic Router
       ┌──────────┼───────────┐
       ▼          ▼           ▼
  Compiler     Language     Syntax
  Native       Worker       Backend
  Backend      Adapter      Fallback
       │          │           │
       └──────────┼───────────┘
                  ▼
         Canonical Semantic IR
                  │
                  ▼
      Evidence + Freshness Validation
                  │
                  ▼
          Protocol Projection
                  │
                  ▼
               Result
```

**INV-ARCH-001**: Protocol adapters MUST NOT become the semantic source of truth.

**INV-ARCH-002**: Semantic Core MUST NOT import LSP-, MCP-, HTTP-, WebSocket-, editor-, or CLI-specific packages.

**INV-ARCH-003**: Every externally visible semantic result MUST be derived from one immutable Snapshot.

## A2. Hard priority order

When requirements conflict, engineering decisions MUST follow:

```text
Semantic Correctness
    >
Snapshot / Version Consistency
    >
Safety of Code Modification
    >
Fault Containment
    >
Determinism / Reproducibility
    >
Observability
    >
Responsiveness
    >
Throughput
    >
Feature Coverage
```

No optimization may invert this ordering without a reviewed ADR.

## A3. Zero-Wrong-Result policy

OmniLSP MUST prefer an explicit incomplete/unknown/unavailable result over a semantically fabricated result.

The system MUST NOT infer semantic identity solely from:

- equal identifier text;
- similar qualified names;
- file proximity;
- AST shape similarity;
- regex matches;
- embedding/vector similarity;
- an LLM guess;
- stale index entries;
- an inferred C/C++ header compile command when the ambiguity can change meaning;
- an unavailable macro/proc-macro expansion.

```text
Unknown
  -> explicit conservative result

Unknown
  -X-> heuristic presented as semantic truth
```

For high-risk operations, uncertainty is a hard stop.

## A4. Scope tiers

### A4.1 Core scope — REQUIRED

The stable product core is:

- JSON-RPC runtime;
- LSP adapter;
- VFS;
- URI engine;
- Position engine;
- immutable Snapshot engine;
- Build Context model;
- scheduler;
- cancellation;
- backpressure;
- canonical semantic API;
- language backend abstraction;
- backend supervisor;
- dynamic index;
- query engine;
- evidence validation;
- diagnostics;
- refactoring safety;
- telemetry;
- configuration;
- replay/repro tooling;
- security boundary.

### A4.2 Product adapters — REQUIRED eventually, not Foundation blockers

- CLI;
- gRPC API;
- HTTP API;
- WebSocket gateway;
- MCP adapter;
- SCIP import/export.

### A4.3 Adjacent features — OPTIONAL

- DAP integration;
- distributed indexing;
- remote semantic workers;
- multi-tenant hosted service;
- plugin marketplace;
- WASM plugin runtime;
- AI explanation generation.

**DAP is not part of Semantic Core.** It MAY reuse workspace/build/source-map infrastructure, but debugging capability MUST remain a separable adapter/subsystem.

## A5. Non-goals

OmniLSP MUST NOT become:

- a regex-based code intelligence engine;
- a thin reverse proxy for arbitrary language servers with no canonical semantic model;
- a monolithic process where one language backend crash terminates every client;
- a build system;
- a package manager;
- a source-control system;
- an AI agent that guesses compiler semantics;
- a universal formatter implementation;
- a replacement for each language compiler frontend.

## A6. Product operating modes

The same canonical core MUST support:

### Local daemon mode

```text
Editor -> stdio/pipe -> OmniLSP Core -> local language workers
```

Default security assumption: one local user, one OS account, no remote exposure.

### Remote single-tenant mode

```text
Client -> TLS -> Gateway -> OmniLSP Core -> worker pool/index service
```

Authentication REQUIRED on non-loopback interfaces.

### Remote multi-tenant mode

OPTIONAL. If enabled, tenant isolation becomes a hard invariant:

```text
Tenant A data/cache/index/trace
               !=
Tenant B data/cache/index/trace
```

Cross-tenant cache reuse is forbidden unless the object is explicitly public, content-addressed, and free of tenant-local metadata.

## A7. External protocol/version policy

Protocol implementations MUST pin a concrete schema/spec revision in source control. Builds MUST NOT fetch “latest protocol schema” during compilation.

### LSP

- LSP 3.17.x compatibility is the stable baseline.
- LSP 3.18.x features MAY be supported behind capability gates.
- Because 3.18 evolves, generated types MUST be tied to a pinned Meta Model revision/commit.
- Unknown future enum values and unknown object fields MUST NOT crash initialization.
- A new protocol revision MUST enter through compatibility tests before becoming default.

### MCP

- The MCP adapter SHOULD target the **2026-07-28** protocol revision or a later explicitly pinned revision.
- The adapter MUST follow that revision’s stateless request model rather than assuming the historical initialize/session lifecycle.
- MCP capabilities MUST be independently versioned from Semantic Core.

### DAP

- DAP support, if implemented, MUST use a pinned official JSON schema revision.
- DAP MUST remain outside the correctness path for LSP semantic results.

## A8. Go toolchain policy

The repository MUST declare a supported Go language/toolchain floor.

Initial policy:

```text
Build language floor: Go 1.26
CI:
  - latest supported Go 1.26.x security patch
  - previous supported Go line where feasible
```

The project MUST NOT depend on a specific patch release in API semantics. Security patch updates SHOULD be adopted rapidly.

## A9. Requirement traceability

Critical requirements use stable IDs:

```text
INV-*   architecture/state invariant
SEC-*   security invariant
PROT-*  protocol invariant
SEM-*   semantic invariant
IDX-*   index invariant
OPS-*   operations/observability invariant
TEST-*  verification invariant
REL-*   release gate
```

Tests SHOULD mention the requirement ID they protect.

Example:

```go
func TestINV_POS_001_UTF16RoundTrip(t *testing.T) { ... }
```

---

# Part B — Canonical Identity, Result, and Evidence Model

## B0. Identity primitives

All major state MUST have explicit identity. Process memory addresses MUST NEVER be persistent identity.

```go
type WorkspaceID string
type SessionID string
type SnapshotRevision uint64
type DocumentVersion int64
type BackendEpoch uint64
type IndexGeneration uint64

type SnapshotID struct {
    Workspace WorkspaceID
    Revision  SnapshotRevision
}

type BackendID struct {
    Language string
    Name     string
}

type BuildContextID string   // digest-backed
type ContentHash string      // cryptographic digest
type SymbolID string         // language-aware stable semantic identity
type QueryKey string         // canonical deterministic query key
type TraceID string
type RequestID string
```

IDs exposed in telemetry SHOULD be hashed/redacted when they may reveal paths, repositories, or user identities.

## B1. Snapshot identity

A Snapshot is a logical immutable view containing references to:

```go
type Snapshot struct {
    ID             SnapshotID
    VFSRoot        VFSRevision
    BuildSet       BuildContextSetID
    WorkspaceModel WorkspaceModelRevision
    CreatedAt      time.Time
}
```

`CreatedAt` is diagnostic metadata only and MUST NOT participate in semantic equality.

**INV-SNAPSHOT-001**: After publication, a Snapshot MUST be logically immutable.

**INV-SNAPSHOT-002**: A request MUST NOT read semantic state from two Snapshot revisions.

**INV-SNAPSHOT-003**: Publishing a newer Snapshot MUST NOT mutate objects reachable from older Snapshots unless those objects are themselves immutable/content-addressed.

## B2. BuildContext identity

Semantic interpretation is:

```text
Source
+ Toolchain Identity
+ Build Configuration
+ Dependency State
+ Environment Inputs
= Semantic Context
```

A BuildContextID MUST be derived from canonicalized semantic inputs, not wall-clock time.

Representative fields:

```go
type BuildContext struct {
    ID              BuildContextID
    Language        string
    Toolchain       ToolchainIdentity
    Target          string
    WorkingDir      CanonicalPath
    Args            []string
    Defines         map[string]string
    IncludePaths    []CanonicalPath
    Env             map[string]string // allowlisted semantic environment only
    DependencyLock  ContentHash
    ConfigFiles     []ConfigIdentity
    Provenance      BuildContextProvenance
}
```

Ordering MUST be canonical before hashing.

## B3. Evidence classes

Do not use an unexplained floating-point “confidence” score as semantic truth.

Use categorical assurance:

```go
type Assurance uint8

const (
    AssuranceLexical Assurance = iota
    AssuranceSyntax
    AssuranceIndexedExact
    AssuranceCompilerResolved
)
```

Optional dynamic-language classifications:

```text
Provable
Conservative
DynamicUnknown
```

A “likely” result MUST NOT satisfy an operation that requires `CompilerResolved`.

## B4. Evidence record

```go
type Evidence struct {
    Kind           EvidenceKind
    Assurance      Assurance
    Snapshot       SnapshotID
    BuildContext   BuildContextID
    Backend        BackendID
    BackendEpoch   BackendEpoch
    IndexGen       IndexGeneration
    SourceHash     ContentHash
    DetailCode     string
}
```

Evidence MUST be machine-checkable enough to answer:

- which Snapshot?
- which build context?
- which backend and backend epoch?
- which content hash?
- compiler-native, index-derived, syntax-derived, or fallback?
- was the index complete for this scope?
- was a source-map hop used?

## B5. Canonical semantic result envelope

Internal APIs SHOULD use an explicit result envelope:

```go
type ResultStatus uint8

const (
    ResultExact ResultStatus = iota
    ResultPartial
    ResultUnknown
    ResultUnavailable
)

type SemanticResult[T any] struct {
    Status       ResultStatus
    Value        T
    Evidence     []Evidence
    Completeness Completeness
    Diagnostics  []InternalDiagnostic
}
```

`ResultStale` is intentionally absent from publishable results.

A stale result MUST be rejected during validation and converted to `ContentModified`, cancellation, retry, or another explicit terminal outcome before protocol projection.

## B6. Completeness model

```go
type Completeness uint8

const (
    Complete Completeness = iota
    IncompleteKnownSubset
    CompletenessUnknown
)
```

Examples:

- `definition`: complete if semantic resolver proves all applicable destinations;
- `references`: may be a conservative known subset;
- `rename`: MUST require `Complete`;
- `workspace symbol`: MAY be incomplete during background indexing;
- `diagnostics`: may be partial while a compiler backend is unavailable, but must not masquerade as a complete compiler result.

## B7. Semantic feature safety classes

| Class                 | Examples                                 | Minimum publish rule                                       |
| --------------------- | ---------------------------------------- | ---------------------------------------------------------- |
| S0 structural/display | folding, selection, syntax tokens        | Syntax evidence acceptable                                 |
| S1 local semantic     | hover, signature, local definition       | fresh exact/index/compiler evidence                        |
| S2 cross-file query   | references, implementations, hierarchy   | exact identities; incompleteness tracked                   |
| S3 mutating           | rename, code-action edit, workspace edit | complete semantic proof + fresh snapshot + edit validation |
| S4 execution          | build/test/tool command                  | trust policy + explicit execution capability               |

**SEM-SAFE-001**: Any S3 operation MUST fail closed when required evidence is unavailable.

---

# Part C — Protocol Boundary

## C0. Canonical protocol rule

Protocol semantics and transport semantics are separate.

```text
LSP / MCP / HTTP / gRPC payload
              │
              ▼
      Protocol Projection
              │
              ▼
     Canonical Request/Result
              │
              ▼
        Semantic Core
```

No language backend may need to know whether the caller was VS Code, Neovim, MCP, CLI, or HTTP.

## C1. JSON-RPC runtime

The JSON-RPC layer MUST support:

- request;
- response;
- notification;
- server-initiated request;
- cancellation mapping;
- unique request IDs per connection;
- structured error responses;
- bounded frame/message size;
- malformed-message containment;
- unknown field tolerance where protocol permits it;
- deterministic method dispatch registry;
- instrumentation hooks.

It MUST NOT:

- panic on malformed JSON;
- allocate unbounded memory based on attacker-controlled length;
- recurse without depth limits on pathological payloads;
- leak a goroutine per abandoned request.

Default defensive limits SHOULD be configurable:

```text
max JSON-RPC message: 64 MiB
max nesting/decode depth: implementation-defined bounded value
max in-flight requests per connection: bounded
max queued bytes per connection: bounded
```

A client exceeding policy receives an explicit protocol/transport error or connection close according to transport semantics.

## C2. LSP lifecycle state machine

```text
New
 -> Initializing
 -> Running
 -> ShuttingDown
 -> Exited
```

Rules:

- only initialization-safe messages are accepted before `initialize` completes;
- `initialized` transitions capability-dependent runtime setup;
- `shutdown` stops accepting new semantic work while allowing bounded cleanup;
- `exit` terminates the connection/server according to LSP semantics;
- invalid lifecycle ordering MUST NOT corrupt workspace state.

## C3. LSP capability matrix

```go
type CapabilityMatrix struct {
    Client    ClientCapabilities
    Server    ServerCapabilities
    Effective EffectiveCapabilities
}
```

Every handler MUST depend on `Effective`, not client-name checks.

Forbidden:

```go
if clientName == "vscode" { ...semantic behavior... }
```

Allowed:

```go
if effective.WorkspaceEdit.DocumentChanges { ... }
```

Client-specific compatibility shims MAY exist only in `compat/*` and MUST preserve canonical semantics.

## C4. Position encoding negotiation

The Position Engine MUST support at least:

- UTF-16;
- UTF-8 when negotiated;
- UTF-32 when negotiated/required by supported clients.

If the client does not negotiate an alternative encoding, the LSP-compatible default MUST be applied.

The core MUST carry the negotiated encoding in the connection/session context, not as global mutable state.

## C5. Document synchronization

The server MUST correctly support full or incremental synchronization according to negotiated capability.

Critical correction:

> **Raw incremental `didChange` notifications are state transitions and MUST NOT be dropped merely because they are “old”.**

Allowed coalescing:

```text
didChange v100
didChange v101
didChange v102
       │
       ├─ all edits are sequentially applied to the VFS writer
       └─ semantic recomputation for v100/v101 may be cancelled/coalesced
```

Forbidden:

```text
drop v100/v101 incremental edits
apply only v102 delta
```

unless v102 contains a full-document replacement that is independently sufficient.

**PROT-SYNC-001**: VFS document state MUST be lossless with respect to accepted incremental changes.

Version gaps or invalid ranges MUST trigger resynchronization/error behavior, never silent patching.

## C6. LSP request lifecycle

```text
Decoded
 -> Admitted
 -> SnapshotCaptured
 -> Queued
 -> Running
 -> Validating
 -> Projecting
 -> Responded
```

Terminal alternatives:

```text
Cancelled
TimedOut
ContentModified
RejectedOverload
BackendUnavailable
ProtocolError
InternalError
```

A request MUST have exactly one terminal protocol outcome.

## C7. Cancellation mapping

Cancellation MUST flow:

```text
LSP request
 -> scheduler context
 -> query engine
 -> backend adapter
 -> index
 -> child process / RPC
```

If cancellation occurs:

- shared work needed by another waiter MUST NOT be accidentally destroyed;
- per-request projections MUST terminate;
- no orphan goroutine may remain;
- external process cancellation MUST be bounded;
- response lifecycle MUST still complete correctly.

## C8. Partial results and progress

Partial result streaming is allowed only if all chunks belong to the same Snapshot and Build Context set.

```text
Chunk 1 @ Snapshot 42
Chunk 2 @ Snapshot 42
Chunk 3 @ Snapshot 43   // FORBIDDEN
```

If a newer edit invalidates the query and correctness cannot be preserved, terminate rather than mixing revisions.

## C9. Workspace edits

Mutating operations SHOULD project edits using version-aware `documentChanges` / `TextDocumentEdit` when the client supports them.

Before emitting any WorkspaceEdit:

1. resolve semantic identities;
2. capture a single Snapshot;
3. compute edits;
4. validate all source hashes/versions;
5. reject overlap/conflict;
6. verify file-operation capability;
7. revalidate freshness;
8. emit versioned changes where possible.

If a client cannot support sufficient version preconditions for a high-risk cross-file edit, OmniLSP SHOULD refuse the operation instead of silently downgrading safety.

## C10. Semantic Tokens

Token result IDs MUST be bound to:

```text
document identity
+ document version/snapshot
+ legend identity
+ backend/tokenizer generation
```

Delta requests MUST be validated against the exact base result.

Unknown or evicted base result:

```text
semanticTokens/delta
 -> full recomputation / protocol-compatible fallback
```

not arbitrary delta application.

## C11. Pull diagnostics

Diagnostic result IDs MUST be stable only for an unchanged semantic state.

A diagnostic cache key MUST include at least:

- Snapshot;
- document content hash;
- BuildContextID;
- backend version/epoch;
- diagnostic configuration hash.

## C12. Custom LSP extension namespace

All private methods MUST use one namespace:

```text
omnilsp/*
```

Canonical methods:

```text
omnilsp/status
omnilsp/explain
omnilsp/indexStats
omnilsp/backendStatus
omnilsp/queryTrace
omnilsp/reindex
omnilsp/resultMeta
```

Do not use mixed historical names such as `polyLsp/*` or `omni/*`.

## C13. Transport layer

Required local transports:

- stdio;
- named pipe / Unix Domain Socket.

Optional:

- TCP;
- WebSocket;
- HTTP;
- gRPC.

Transport responsibilities are limited to:

```text
framing
connection lifecycle
authentication
encryption
rate limiting
peer metadata
```

Semantic behavior MUST NOT differ because a request arrived over HTTP instead of stdio.

## C14. MCP adapter

The MCP adapter is a projection of canonical code-intelligence operations.

Candidate tools/resources:

```text
find_definition
find_references
type_at
symbol_search
workspace_symbols
diagnostics
call_hierarchy
explain_symbol
index_status
```

Rules:

- MCP MUST NOT bypass Snapshot capture.
- MCP MUST NOT return stale index data as exact semantic truth.
- MCP tool descriptions MUST NOT claim stronger guarantees than the canonical result.
- MCP 2026-07-28 requests are treated as stateless at the protocol layer; any OmniLSP state handle must be explicit application data.
- Mutating tools SHOULD be disabled by default until consent/auth/edit-precondition semantics are fully implemented.

## C15. gRPC / HTTP API

Public APIs MUST expose a versioned canonical schema.

Example gRPC namespace:

```text
omnilsp.v1.CodeIntelligence
omnilsp.v1.Workspace
omnilsp.v1.Index
omnilsp.v1.Status
```

HTTP MAY mirror these resources, but HTTP JSON objects MUST not become a second semantic model.

## C16. DAP boundary

DAP is OPTIONAL and MUST be isolated:

```text
DAP Adapter
   -> Debug Runtime
   -> may consume SourceMap / BuildContext / Workspace facts
```

DAP MUST NOT introduce dependencies from Semantic Core to debugger-specific state.

## C17. Protocol code generation

Generated LSP/DAP/protobuf types:

- MUST come from pinned inputs;
- MUST be reproducible;
- MUST include generator version;
- MUST NOT be hand-edited;
- MUST be regenerated in CI to detect drift.

Compatibility patches belong in explicit adapters, not generated files.

---

# Part D — Workspace, VFS, URI, Text, Position, and Snapshot

## D0. Workspace state machine

```text
Absent
 -> Discovering
 -> Loading
 -> Ready
 -> Degraded
 -> Closing
 -> Closed
```

`Degraded` means the core is alive but one or more optional/semantic subsystems cannot provide full capability.

Examples:

- missing compiler;
- broken build context;
- backend crash loop;
- index rebuild;
- untrusted project blocks proc macros.

A degraded workspace MUST remain structurally usable where safe.

## D1. VFS precedence

Canonical precedence:

```text
Editor Overlay
    >
Generated / Virtual Document
    >
Trusted Materialized Workspace File
    >
Disk
    >
Remote Static Cache
```

Every `FileState` MUST state provenance.

```go
type FileState struct {
    URI          DocumentURI
    LanguageID   string
    Version      DocumentVersion
    Hash         ContentHash
    Content      ImmutableBytes
    Source       FileSource
    LineEnding   LineEndingMode
    TextEncoding TextEncoding
}
```

## D2. URI engine

URI and OS paths MUST be distinct types.

Forbidden:

```go
uri := "file://" + path
```

Required handling:

- file URI percent escaping;
- UTF-8 names;
- Windows drive letters;
- Windows UNC paths;
- case-sensitive vs case-insensitive filesystems;
- macOS normalization edge cases;
- symlinks;
- multi-root workspaces;
- virtual URI schemes;
- notebook cells;
- generated documents.

URI normalization MUST preserve a stable canonical identity without corrupting display spelling.

## D3. Path identity and symlinks

The system MUST distinguish:

```text
logical path identity
physical filesystem identity
display path
```

Symlink resolution policy MUST be explicit.

For security-sensitive containment checks:

```text
canonical physical target
  -> verify inside allowed root
```

A lexical prefix check is insufficient.

## D4. Text representation

Internal editor overlays SHOULD use UTF-8 bytes as the canonical text representation.

The system MUST NOT perform silent lossy conversion.

Disk files that cannot be represented under the active text encoding policy MUST produce an explicit unsupported/degraded state.

For languages with compiler-specific source encoding:

- raw bytes MAY be retained;
- compiler backend MAY use language/toolchain encoding rules;
- LSP-visible text MUST still have a deterministic Unicode mapping.

## D5. Document buffer

The DocumentBuffer abstraction MUST support:

- immutable snapshot views;
- efficient incremental edits;
- fast line lookup;
- byte offset lookup;
- content hashing;
- bounded memory;
- CRLF preservation;
- safe EOF positions.

The underlying data structure MAY be rope, piece table, gap-buffer derivative, or immutable chunk tree, but the public contract MUST not expose the representation.

## D6. Applying `didChange`

Algorithmic contract:

```text
current FileState(version = N)
    -> validate incoming version/ranges
    -> convert each LSP range using current intermediate text
    -> apply in protocol-defined order
    -> construct immutable FileState(version = N+1 or supplied version)
    -> publish through single writer
    -> create new Snapshot
```

Range conversion failures MUST reject the change safely.

Never clamp an out-of-range edit into “something that looks valid”.

## D7. Position Engine

The Position Engine owns conversion among:

```text
byte offset
UTF-8 code units
UTF-16 code units
UTF-32 code units
line/character
compiler offsets
AST offsets
virtual-document offsets
host-document offsets
```

It MUST handle:

- ASCII;
- CJK;
- emoji;
- surrogate pairs;
- combining marks;
- zero-width code points;
- CRLF;
- LF;
- empty lines;
- EOF;
- non-ASCII identifiers;
- mixed Unicode.

Important semantic rule:

> LSP `character` is a code-unit offset in the negotiated encoding, **not a grapheme-cluster index**.

Combining characters therefore MUST NOT be “visually normalized” during position conversion.

**INV-POS-001**: valid position round trips MUST preserve the exact logical boundary.

**INV-POS-002**: invalid positions MUST return a typed error, never panic.

## D8. Position Engine properties

Property tests MUST include:

```text
offset -> position -> offset == original
position(valid) -> offset -> position == canonical position
edit sequence preserves line index equivalence with full rebuild
UTF-8/16/32 conversions agree on scalar boundaries
CRLF line starts match full scanner
```

Fuzz targets MUST mutate both text and positions.

## D9. Snapshot publication

Use a single-writer publication model.

```text
Workspace Writer
  -> apply state mutation
  -> construct immutable nodes
  -> allocate Revision N+1
  -> atomically publish Snapshot pointer
```

Readers acquire a Snapshot pointer once.

No reader may “refresh” itself mid-query.

## D10. Snapshot retention

Old Snapshots MAY remain alive while referenced by in-flight requests.

Retention MUST be bounded by:

- active request references;
- cache policy;
- memory budget;
- optional replay/debug retention.

A Snapshot can be garbage-collected only when no required consumer references it.

## D11. Stale result prevention

Before protocol projection, validate:

```text
request Snapshot
+ relevant document version
+ BuildContextID
+ backend epoch
+ index generation/freshness
```

For a mutating result, validate again immediately before emission.

```text
Computed
 -> Freshness Validation
    -> fresh: publish
    -> changed: ContentModified / abort
```

## D12. Virtual documents and source maps

Embedded/generated language support MUST use an explicit source map:

```text
Host Document
 -> Region Extraction
 -> Virtual Document
 -> Language Backend
 -> Virtual Result
 -> Reverse Source Map
 -> Host Result
```

Source-map segments MUST specify mapping quality.

Suggested classification:

```text
Exact
ManyToOne
OneToMany
UnmappedGenerated
```

S3 operations MUST NOT cross an `UnmappedGenerated` region.

## D13. Notebook model

Notebook support MUST model:

```text
Notebook document
 -> ordered cells
 -> per-cell URI/identity
 -> cell language
 -> cell version
 -> optional execution metadata
```

Notebook semantic ordering is language/backend-specific and MUST not be guessed by generic VFS code.

## D14. File watcher model

Watch events are hints, not absolute truth.

The workspace layer MUST tolerate:

- duplicate events;
- missing events;
- rename represented as delete+create;
- event reordering;
- editor writes via atomic replace;
- network filesystem lag.

A bounded reconciliation scan MAY be triggered when watcher reliability is uncertain.

## D15. Workspace file operations

Create/rename/delete MUST be processed as state transitions through the single writer.

Case-only rename on case-insensitive filesystems MUST be handled explicitly.

A rename MUST update:

- VFS identity;
- source maps;
- build-context inputs where applicable;
- dependency graph;
- index invalidation;
- open-document mapping;
- client-visible URI projections.

---

# Part E — Build Context and Semantic Environment

## E0. Build Context is semantic input

A file path alone is not enough to identify program meaning.

```text
same source bytes
+ different build flags/features/target/env
= potentially different program
```

Therefore all semantic cache/query/index keys MUST include a BuildContextID or a proof that the query is build-context-independent.

## E1. Build Context provenance

Every context MUST record provenance:

```go
type BuildContextProvenance uint8

const (
    ProvenanceExplicitUserConfig BuildContextProvenance = iota
    ProvenanceCanonicalBuildDB
    ProvenanceBuildSystemMetadata
    ProvenanceBackendDerived
    ProvenanceHeuristic
)
```

High-risk operations MUST NOT depend solely on `ProvenanceHeuristic` if the heuristic can change symbol resolution.

## E2. Multiple contexts per file

A source file MAY belong to several valid build contexts.

Examples:

- C/C++ header included by multiple translation units;
- Go file under multiple build tags;
- Rust crate with several feature sets/targets;
- TypeScript project references;
- Python source under different interpreters;
- generated code consumed by multiple targets.

The core MUST support:

```text
Document
 -> Context A
 -> Context B
 -> Context C
```

It MUST NOT force global uniqueness when reality is ambiguous.

## E3. Active context selection

Context selection SHOULD follow:

```text
explicit client/user selection
    >
workspace configuration
    >
language-native project model
    >
backend selection
    >
heuristic
```

If two plausible contexts produce materially different S3 results:

```text
ambiguity
 -> abort mutating operation
```

## E4. Build Context canonicalization

Before hashing:

- normalize paths using the URI/path engine;
- preserve argument ordering where semantically meaningful;
- sort unordered sets/maps;
- strip non-semantic timestamps;
- include toolchain version/identity;
- include relevant lockfiles/config hashes;
- include semantic environment allowlist;
- include generated-input hashes when required.

Environment MUST be allowlisted. Do not hash all process environment variables blindly.

## E5. Toolchain identity

```go
type ToolchainIdentity struct {
    Kind       string
    Executable ContentHashOrTrustedPath
    Version    string
    Target     string
    Digest     ContentHash
}
```

The effective toolchain identity SHOULD be reproducible.

A changed compiler/toolchain MUST invalidate semantic caches whose meaning can change.

## E6. C/C++ Build Context

C/C++ semantic identity MUST account for at least:

```text
compile_commands.json entry
compiler family/version
language mode
language standard
target triple
sysroot
include paths
system include paths
defines/undefines
forced includes
module flags
PCH/module state where relevant
working directory
resource directory
```

`compile_commands.json` MUST be parsed as data.

OmniLSP MUST NOT pass its `command` string to a shell.

In untrusted workspaces:

- arbitrary compiler wrappers are not automatically trusted;
- `-fplugin`, dynamic compiler plugins, shell metacharacters, arbitrary `-Xclang -load`, and equivalent code-loading mechanisms MUST be blocked or trust-gated;
- a configured trusted clang/clangd executable SHOULD be used to interpret flags.

Header compile-command inference MUST be surfaced as inferred provenance.

If header semantics differ across translation units, a rename MUST not silently choose one.

## E7. Go Build Context

At minimum:

```text
module / go.work scope
GOOS
GOARCH
build tags
toolchain
module graph
workspace modules
GOWORK
GOMOD
vendor mode
relevant GOFLAGS
```

Different build scopes MUST remain isolated.

The backend MAY track several builds simultaneously.

## E8. Rust Build Context

At minimum:

```text
Cargo workspace
crate graph
features
default-features
cfg
target
toolchain
sysroot
build-script output
proc-macro availability
dependency lock state
```

Build scripts and proc macros can execute code and MUST participate in the trust model.

In untrusted mode:

```text
unsafe expansion unavailable
 -> explicit degraded semantic capability
```

not silent guessing.

## E9. Python semantic environment

At minimum:

```text
interpreter identity
virtual environment
Python version
platform
sys.path/search roots
pyproject/config
type-checking mode
stub paths
typeshed version
package roots
editable installs
```

Dynamic runtime behavior MUST not be presented as statically proven unless the backend can prove it.

## E10. TypeScript / JavaScript environment

At minimum:

```text
tsconfig/jsconfig
compilerOptions
module resolution mode
paths/baseUrl
project references
package.json
lockfile/dependency graph
TypeScript version
workspace TypeScript plugin policy
```

Workspace-provided TypeScript plugins execute code and MUST be trust-gated.

## E11. Java environment

At minimum:

```text
JDK identity
language level
module path
classpath
build-tool project model
annotation processing policy
generated sources
workspace modules
```

Annotation processors and build plugins MUST be trust-gated.

---

# Part F — Runtime Concurrency, Scheduling, Cancellation, and Resource Control

## F0. Ownership model

Every shared mutable object MUST belong to exactly one category:

```text
immutable
atomic
mutex-protected
actor/single-writer owned
```

There is no fifth category.

## F1. Single-writer workspace mutation

Workspace state transitions MUST be serialized through a writer/actor.

Examples:

- didOpen;
- didChange;
- didSave;
- didClose;
- file watcher reconciliation;
- workspace folder changes;
- build-context replacement;
- index generation publication.

The writer MUST avoid long compiler work while holding ownership. Heavy computation occurs outside, then publication returns through a validated commit step.

## F2. Locking policy

Packages using mutexes MUST document:

- protected fields;
- allowed lock acquisition order;
- whether callbacks may execute under lock;
- whether blocking I/O is forbidden under lock.

Core code SHOULD avoid nested locks. If unavoidable, a global lock-order invariant MUST be documented and tested where possible.

## F3. Request classes

Canonical scheduler classes:

| Class | Typical operations                                  | Intent                |
| ----- | --------------------------------------------------- | --------------------- |
| P0    | completion, hover, signature                        | immediate interaction |
| P1    | definition, declaration, implementation, local refs | navigation            |
| P2    | diagnostics, code action preparation                | correctness feedback  |
| P3    | semantic tokens, inlay hints, hierarchy             | rich editor data      |
| P4    | background index, workspace symbol refresh          | throughput            |
| P5    | compaction, maintenance, cleanup                    | opportunistic         |

Priority is not permission to starve lower classes forever.

## F4. Scheduler request record

```go
type ScheduledRequest struct {
    RequestID      RequestID
    Workspace      WorkspaceID
    Snapshot       SnapshotID
    BuildContext   BuildContextID
    Priority       Priority
    CostClass      CostClass
    Deadline       time.Time
    CoalesceKey    string
    ClientClass    string
    EnqueuedAt     time.Time
}
```

## F5. Admission control

Admission MUST happen before expensive work.

Checks MAY include:

- connection in-flight count;
- workspace in-flight count;
- queue byte budget;
- request cost class;
- memory pressure state;
- backend health;
- deadline feasibility;
- duplicate/coalescible work.

Rejected work MUST produce an explicit outcome.

## F6. Bounded queues

Every queue/channel/buffer MUST be bounded or indirectly bounded by a documented global budget.

Forbidden:

```go
go func() { work <- req }() // used to disguise an unbounded queue
```

Queue saturation MUST be observable.

## F7. Fairness and starvation

The scheduler SHOULD implement bounded fairness using one or more:

- weighted fair queuing;
- aging;
- per-workspace quotas;
- per-client quotas;
- reserved interactive capacity.

A permanent stream of completion requests MUST NOT prevent essential workspace maintenance forever.

## F8. Cost classes

Requests SHOULD be cost-classified:

```text
Tiny       local lookup/cache hit
Small      single-file parse/query
Medium     package/module semantic work
Large      workspace references/index query
VeryLarge  full indexing/rebuild
```

Large work MUST have stricter concurrency caps.

## F9. Deadlines

Protocol adapters MAY supply deadlines; otherwise server defaults apply.

Deadlines are a resource policy, not a semantic shortcut.

On timeout:

```text
stop work
 -> cleanup
 -> return typed timeout/unavailable result
```

Never return an unvalidated partial edit just to beat a deadline.

## F10. Cancellation

All potentially long functions MUST accept `context.Context` or an equivalent explicit cancellation token.

Cancellation checks SHOULD occur:

- before expensive parse/typecheck;
- between query graph stages;
- before/after backend RPC;
- during large index iteration;
- before result validation;
- before protocol projection.

## F11. Shared computation cancellation

Memoized work can have multiple waiters.

```text
Query Q
  ├─ Request A
  └─ Request B
```

Cancelling A MUST NOT cancel Q while B still needs it.

The query engine SHOULD use shared futures/singleflight with waiter-aware cancellation.

## F12. Coalescing

Safe to coalesce:

- background semantic recomputation for superseded snapshots;
- repeated identical workspace symbol refreshes;
- repeated diagnostics for the same newer document state;
- redundant index maintenance.

Unsafe to coalesce by dropping state transitions:

- incremental document edits not yet applied;
- create/delete/rename state mutations;
- lifecycle notifications whose ordering carries meaning.

## F13. Memory Budget Manager

Memory categories MUST be measured separately:

```text
document text
line/position indexes
syntax trees
semantic query cache
dynamic index
persistent-index read cache
snapshots
backend process RSS
RPC buffers
trace/replay buffers
goroutines/stacks
```

The manager SHOULD operate with thresholds:

```text
< 70% soft budget   normal
70-80%              evict cold caches
80-90%              pause/throttle background work
>= 90%              reject expensive work / restart optional workers / emergency eviction
```

Percentages are defaults, configurable.

## F14. Per-backend budgets

External workers MUST have:

- maximum RSS or monitored budget;
- CPU concurrency limit;
- request timeout;
- maximum restart frequency;
- maximum log volume;
- process/file-descriptor accounting where feasible.

Backend memory MUST count against workspace/global policy.

## F15. Goroutine policy

Every goroutine MUST have a clear owner and termination condition.

Forbidden:

- goroutine per file forever;
- goroutine per watcher event with no bound;
- background loops without context;
- abandoned RPC receive loops;
- retry loops without backoff.

## F16. Panic boundary

A single request MUST NOT terminate the core process.

Recover only at controlled boundaries:

```text
RPC dispatch
worker adapter boundary
plugin boundary
background job supervisor
```

On recover:

1. capture stack;
2. attach TraceID/RequestID;
3. classify internal error;
4. fail the affected request/job;
5. preserve process integrity when possible.

Tests SHOULD panic rather than hide defects.

## F17. Race policy

CI MUST run:

```bash
go test -race ./...
```

for the supported race-enabled platform matrix.

No core-package race waiver without an accepted ADR and issue.

---

# Part G — Backend Architecture and Supervision

## G0. Backend abstraction

The core talks only to canonical backend contracts.

```go
type LanguageBackend interface {
    Identity() BackendIdentity
    Capabilities(context.Context, BackendContext) (Capabilities, error)

    Parse(context.Context, ParseQuery) SemanticResult[SyntaxTree]

    Hover(context.Context, HoverQuery) SemanticResult[Hover]
    Complete(context.Context, CompletionQuery) SemanticResult[CompletionList]
    Signature(context.Context, SignatureQuery) SemanticResult[SignatureHelp]

    Definition(context.Context, SymbolQuery) SemanticResult[[]Location]
    Declaration(context.Context, SymbolQuery) SemanticResult[[]Location]
    TypeDefinition(context.Context, SymbolQuery) SemanticResult[[]Location]
    Implementation(context.Context, SymbolQuery) SemanticResult[[]Location]
    References(context.Context, ReferenceQuery) SemanticResult[[]Reference]

    Diagnostics(context.Context, DiagnosticQuery) SemanticResult[[]Diagnostic]
    Rename(context.Context, RenameQuery) SemanticResult[ValidatedEdit]

    Symbols(context.Context, SymbolQuery) SemanticResult[[]Symbol]
    SemanticTokens(context.Context, TokenQuery) SemanticResult[SemanticTokenSet]
    InlayHints(context.Context, InlayQuery) SemanticResult[[]InlayHint]
    CodeActions(context.Context, ActionQuery) SemanticResult[[]CodeAction]
}
```

An implementation MAY split this interface into capability-specific smaller interfaces to avoid a giant concrete interface.

## G1. Backend classes

### Compiler-native/in-process

Used only where maintenance cost is justified and compiler-quality APIs are available.

### Compiler/language-service worker

Preferred for complex languages.

```text
Core
 -> Canonical Worker Adapter
 -> gopls / clangd / rust-analyzer / tsserver / ...
```

A nested LSP server MAY be used internally, but the core MUST normalize its output into canonical semantic types/evidence.

### Syntax backend

Tree-sitter or equivalent.

Permitted capabilities:

- syntax tree/CST;
- folding;
- structural selection;
- basic document symbols;
- syntax tokens;
- embedded-language discovery.

It MUST NOT claim compiler-grade type/name/overload/trait/template resolution.

## G2. Backend lifecycle state machine

```text
Disabled
 -> Starting
 -> Ready
 -> Degraded
 -> Unhealthy
 -> Backoff
 -> Starting

Unhealthy
 -> Quarantined
```

Each successful start increments `BackendEpoch`.

**INV-BACKEND-001**: A result produced by epoch N MUST NOT be published as if it came from epoch N+1.

## G3. Health model

Health is multidimensional:

```go
type BackendHealth struct {
    ProcessAlive      bool
    RPCResponsive     bool
    WorkspaceLoaded   bool
    BuildContextValid bool
    IndexReady        bool
    LastSuccess       time.Time
    FailureClass      string
}
```

`ProcessAlive == true` does not imply semantic readiness.

## G4. Supervisor restart policy

Restart MUST be bounded.

Example:

```text
failure 1 -> 100 ms
failure 2 -> 500 ms
failure 3 -> 2 s
failure 4 -> 10 s
then bounded exponential backoff + jitter
```

A crash loop MUST transition to `Quarantined` after a configurable threshold.

User-visible status MUST explain the cause and recovery action.

## G5. Hung worker handling

If cancellation/deadline is ignored:

1. send backend-native cancellation;
2. wait bounded grace;
3. mark request failed;
4. if worker remains nonresponsive, terminate the worker process group;
5. increment BackendEpoch;
6. restart under supervisor policy.

No zombie compiler processes.

## G6. Worker process creation

Process spawning MUST use argument arrays, never concatenated shell commands.

Worker environment MUST be explicitly constructed.

Untrusted workspace values MUST NOT become executable paths without policy validation.

## G7. Backend RPC

Canonical out-of-process worker protocol SHOULD use versioned Protobuf + gRPC or another strongly typed framed protocol.

It MUST include:

```text
protocol version
backend identity
backend epoch
capability discovery
workspace attach/detach
snapshot/build identifiers
request
cancel
streaming result
health
structured error
```

Large content transfer SHOULD use hashes/shared immutable blobs when safe to avoid copying entire workspaces repeatedly.

## G8. Worker snapshot semantics

A worker query MUST identify the exact content/build state it is supposed to analyze.

Allowed strategies:

- worker mirrors VFS versions and acknowledges them;
- request carries immutable content/blob hashes;
- worker owns a snapshot handle previously synchronized.

The core MUST never assume “the worker probably has the latest file”.

## G9. Bootstrap language strategy

Correctness-first implementation order:

```text
Phase 1:
  reuse mature canonical language services
  normalize through Backend Adapter
  build differential corpus

Phase 2:
  optionally replace selected paths with native implementation
  only after parity gates
```

This prevents years of reimplementing mature compiler semantics before the product is usable.

---

# Part H — Language-Specific Backend Contracts

## H0. Tier S

Required high-depth languages:

```text
Go
C
C++
Rust
Python
TypeScript / JavaScript
```

Tier S means “deep semantics with explicit build context”, not merely syntax highlighting.

## H1. Go

### H1.1 Source of truth strategy

Initial production backend SHOULD use `gopls` as a compiler-grade semantic worker.

A future in-process/native Go backend MAY use:

```text
go/parser
go/ast
go/token
go/types
golang.org/x/tools/go/packages
go/analysis
SSA
```

but it MUST NOT become default until differential tests show acceptable parity against the pinned canonical backend for required features.

### H1.2 Go semantic keys

Cache/index keys MUST include:

```text
module/workspace identity
Go toolchain
GOOS
GOARCH
build tags
module graph
vendor/work mode
content hash
backend version
```

### H1.3 Go rename

Rename MUST account for:

- package scopes;
- method sets;
- embedded fields;
- promoted methods;
- type parameters;
- imports;
- generated code policy;
- build tags;
- references across active build scope.

No textual fallback.

## H2. C/C++

### H2.1 Source of truth

Production semantics SHOULD be obtained from clang/clangd.

Tree-sitter MUST NOT implement:

- overload resolution;
- templates;
- SFINAE;
- concepts;
- macro expansion;
- C++ name lookup;
- include semantics.

### H2.2 Compilation database

`compile_commands.json` is preferred.

When absent:

- structural features may continue;
- compiler backend MAY use a clearly marked fallback context;
- S3 features requiring exact context SHOULD be unavailable.

### H2.3 Header ambiguity

Headers often have no unique compile command.

The backend adapter MUST preserve:

```text
selected translation-unit context
provenance
ambiguity state
```

If a header symbol differs by macro/target/context, high-risk operations MUST abort or require explicit context selection.

### H2.4 Macro locations

Locations MUST distinguish:

```text
spelling location
expansion location
generated location
```

Navigation/edit projection MUST use the language/backend’s semantically appropriate location and retain evidence.

## H3. Rust

### H3.1 Source of truth

Use `rust-analyzer` or another compiler-grade Rust semantic engine.

### H3.2 Required model

Track:

- Cargo workspace;
- crate graph;
- features;
- cfg;
- target;
- toolchain/sysroot;
- macro expansion;
- proc macro availability;
- build-script-derived cfg/env.

### H3.3 Expansion mapping

```text
source token
 -> macro invocation
 -> expansion node
 -> generated semantic symbol
```

must retain reversible source mapping where possible.

A symbol that exists only in generated expansion cannot be blindly edited at a synthetic position.

## H4. Python

### H4.1 Source of truth

Use a mature static semantic engine such as Pyright for type-oriented semantics, combined with syntax infrastructure where necessary.

Capabilities MUST be feature-tested; do not assume every Python language-service feature exists because the type checker exists.

### H4.2 Dynamic uncertainty

Classify results:

```text
Provable
Conservative
DynamicUnknown
```

For example:

```python
x = importlib.import_module(name_from_network)
```

Static resolution cannot be represented as certain.

### H4.3 Python rename

Rename MAY be restricted when:

- dynamic attribute access dominates;
- `__getattr__`/metaprogramming invalidates proof;
- import graph is incomplete;
- interpreter/module search path is ambiguous.

Explicit refusal is valid.

## H5. TypeScript / JavaScript

### H5.1 Source of truth

Prefer the TypeScript compiler service / tsserver semantic model.

### H5.2 Project identity

A file can belong to:

- configured project;
- inferred project;
- referenced project;
- composite project graph.

The adapter MUST expose which project produced a result.

### H5.3 Plugins

Workspace TypeScript plugins are executable code.

Untrusted workspace:

```text
plugins disabled
 -> capability/status explains degradation
```

## H6. Java

Use a mature Java semantic engine such as JDT LS or equivalent.

Required semantic inputs:

- JDK;
- language level;
- module/class path;
- project model;
- generated sources;
- annotation processing policy.

## H7. Tier A

Candidate languages:

```text
Kotlin
C#
Lua
Bash
CMake
Proto
```

A language enters Tier A only with a written backend/evidence/build-context plan.

## H8. Data/config/markup languages

```text
JSON
YAML
TOML
XML
Dockerfile
Markdown
```

These MAY combine:

- parser/tree-sitter;
- schema validation;
- language-specific validator;
- embedded-language extraction.

A schema-aware validator MAY provide high-quality semantics without a compiler, but assurance class must describe the actual evidence.

---

# Part I — Canonical Symbol, Reference, Graph, and Semantic APIs

## I0. Symbol model

```go
type Symbol struct {
    ID           SymbolID
    Language     string
    Name         string
    Qualified    string
    Kind         SymbolKind

    Declaration  Location
    Definition   *Location
    Container    *SymbolID
    Signature    string

    BuildContext BuildContextID
    Origin       SymbolOrigin
    Flags        SymbolFlags
}
```

## I1. Stable SymbolID

A persistent symbol ID SHOULD derive from:

```text
language
+ package/module/crate identity
+ qualified semantic identity
+ language-specific disambiguator
+ optional signature identity
```

It MUST NOT derive from:

- memory pointer;
- goroutine-local counter;
- current line number alone;
- hash of display text alone.

Stability across arbitrary refactors is not guaranteed and MUST NOT be falsely promised.

## I2. Local/anonymous symbol IDs

Local variables, lambdas, anonymous types, and generated symbols may need source-anchored identity.

The language backend SHOULD define:

```text
containing stable symbol
+ lexical role
+ stable syntax anchor
+ semantic ordinal/disambiguator
```

with clear invalidation rules.

## I3. Reference model

```go
type Reference struct {
    Symbol       SymbolID
    Location     Location
    Kind         ReferenceKind
    BuildContext BuildContextID
    Origin       ReferenceOrigin
}
```

Kinds SHOULD include:

```text
Read
Write
Call
TypeUse
Import
Declaration
Definition
Implementation
Inheritance
AddressTake
MacroUse
```

Language backends MAY add internal kinds and project them to canonical kinds.

## I4. Code graph

Canonical graph nodes MAY represent:

- file;
- module/package/crate;
- symbol;
- type;
- build target;
- generated artifact.

Edges MAY include:

```text
declares
defines
references
calls
imports
includes
inherits
implements
uses_type
generates
expands_to
depends_on
```

Every edge MUST have provenance.

## I5. Cross-language graph

Cross-language edges MAY include:

```text
C ABI <-> Rust FFI
Go <-> cgo
Python <-> native extension
Proto -> generated Go/C++/Java/TS
JNI
generated OpenAPI clients
```

Only establish an edge from reliable evidence:

- explicit FFI declaration;
- generator manifest/source map;
- schema-generated identity;
- linker/build metadata;
- compiler/backend mapping.

Name similarity alone is forbidden.

## I6. Location model

```go
type Location struct {
    URI   DocumentURI
    Range ByteRangeOrCanonicalRange
}
```

Internally, locations SHOULD be represented using immutable content identity plus byte ranges, then projected through the Position Engine.

This prevents repeated ambiguous UTF conversion.

## I7. Precise navigation

Definition/declaration locations SHOULD target the defining identifier token, not an entire function/statement/file.

If the backend returns only a broad range, the adapter MAY refine the token only if refinement cannot change semantic identity.

## I8. Definition contract

A definition result is publishable when:

- symbol identity is resolved;
- destination is fresh for the request Snapshot/build context;
- source-map projection is exact enough;
- URI/position is valid.

Multiple semantically valid definitions MAY be returned where the language permits them.

Do not force a fake unique answer.

## I9. References contract

References MAY merge:

```text
open-file dynamic semantic state
+ fresh local index
+ persistent index
+ backend compiler query
+ remote static index
```

Deduplicate by semantic identity + normalized location + build context semantics.

A location from an older content hash MUST not override a fresh overlay.

## I10. Rename contract

Rename is S3.

Mandatory pipeline:

```text
Resolve target
 -> establish SymbolID
 -> establish complete semantic scope
 -> enumerate references
 -> shadowing/capture analysis
 -> collision analysis
 -> generated/source-map policy
 -> build versioned edits
 -> validate non-overlap
 -> revalidate Snapshot
 -> protocol capability validation
 -> return WorkspaceEdit
```

Any failure:

```text
abort rename
```

Never text-search fallback.

## I11. Rename conflict analysis

Must detect as applicable:

- same-scope collision;
- import alias collision;
- field/method collision;
- shadowing change;
- overload-set semantic change;
- generated-name collision;
- filesystem/module rename collision;
- case-insensitive path collision;
- language keyword invalidity.

## I12. Edit set validation

Within a document:

- ranges MUST be valid;
- edits MUST be non-overlapping unless the protocol/backend explicitly defines a safe merged form;
- edits SHOULD be normalized/deduplicated;
- application to the captured document MUST produce deterministic output.

A preflight test SHOULD apply the edit to an immutable copy before publication.

## I13. Completion contract

Completion stages MAY include:

```text
lexical candidates
scope candidates
type-valid members
imports
snippets
semantic ranking
```

Policy:

```text
candidate semantic validity > candidate count > ranking perfection
```

Ranking may be approximate.

Semantic invalidity SHOULD be minimized and MUST NOT be fabricated from stale build state.

## I14. Completion cancellation

Completion is latency-sensitive.

The backend MAY return an incomplete list, but the result must:

- be fresh for the captured Snapshot;
- contain semantically safe candidates under its evidence level;
- not mix candidates from incompatible build contexts without tagging/filtering.

## I15. Hover and type information

Hover SHOULD separate:

```text
signature/type
documentation
constant value
source/provenance
```

If the type is dynamic/unknown, say so instead of inventing a concrete type.

## I16. Signature help

Signature help MUST bind the active call to semantic candidates.

If overload resolution is incomplete, candidates MAY be returned conservatively, but the adapter must not claim a unique active signature without evidence.

## I17. Diagnostics

Canonical diagnostic:

```go
type Diagnostic struct {
    Range       Location
    Severity    Severity
    Code        string
    Source      string
    Message     string
    Related     []RelatedInformation
    QuickFixIDs []string

    Evidence    Evidence
}
```

Internally retain:

- backend;
- snapshot;
- build context;
- trace;
- symbol if known.

## I18. Diagnostic identity

A stable diagnostic ID SHOULD be derived from semantic fields rather than absolute line number alone.

This enables pull-diagnostic result reuse and reduces flicker.

## I19. Code actions

Code actions are split into:

```text
non-mutating explanation/navigation
mutating edit
executable command
```

Mutating edits follow S3 rules.

Executable commands follow S4 trust rules.

A code action MUST NOT hide arbitrary command execution behind a harmless title.

## I20. Formatting

Formatting MAY be delegated to language-native formatters.

Formatting output is still an edit and MUST satisfy:

- exact captured document version;
- valid ranges;
- deterministic application;
- trust policy if external formatter/config/plugin executes code.

## I21. Semantic tokens

Syntax tokens and semantic augmentation are separate.

```text
syntax backend unavailable? -> no syntax
semantic backend unavailable? -> syntax can remain
```

Token ranges MUST be validated by Position Engine.

## I22. Inlay hints / code lens

Hints and lenses may be dropped under overload because they are lower priority, but any published range MUST be fresh and valid.

Commands attached to lenses are S4 if they execute external effects.

## I23. Hierarchies

Call/type hierarchy edges MUST carry SymbolID/evidence.

For dynamic languages, missing edges are preferable to guessed edges.

## I24. Workspace symbol

Workspace symbol results MAY stream and be incomplete during index build.

Fresh open-file symbols MUST override stale persistent entries.

## I25. Explain API

`omnilsp/explain` returns engineering evidence, not a fabricated chain of thought.

Example:

```text
Request: definition
Snapshot: ws1@1842
BuildContext: cpp:sha256:...
Backend: clangd@epoch-7
Target SymbolID: ...
Resolution evidence: compiler-resolved
Index: local generation 91
Location mapping: exact
Freshness validation: PASS
```

It SHOULD answer:

- what source of truth was used?
- why this build context?
- which fallback, if any?
- why was a result rejected?
- what is incomplete?
- what can the user do to restore full semantics?

---

# Part J — Incremental Query Engine

## J0. Query contract

The canonical form is:

```text
Query(Key, Snapshot, BuildContext)
```

A query SHOULD be:

```text
deterministic for identical immutable inputs
memoizable
dependency-tracked
snapshot-bound
cancelable
observable
side-effect free from caller perspective
```

“Pure-ish” means a query MAY read immutable caches/indexes/backends, but MUST NOT mutate workspace truth directly.

## J1. QueryKey

A QueryKey MUST include every input that can alter meaning.

Conceptually:

```go
type QueryKey struct {
    Kind         QueryKind
    Workspace    WorkspaceID
    Snapshot     SnapshotID
    BuildContext BuildContextID
    Subject      CanonicalSubject
    OptionsHash  ContentHash
}
```

Do not put `time.Now()` or request IDs in semantic keys.

## J2. Query dependency recording

Example:

```text
TypeOf(expr)
  ├─ ResolveName(expr)
  │    ├─ PackageSymbols(pkg)
  │    └─ Imports(file)
  └─ InferCall(call)
       └─ SignatureOf(callee)
```

The engine records actual dependencies.

When a dependency changes:

```text
invalidate dependent nodes
```

Do not clear the entire workspace cache unless safe selective invalidation cannot be proven.

## J3. Dependency identity

Dependencies SHOULD be content/version identities, not mutable pointers.

Examples:

```text
FileContentHash
BuildContextID
ToolchainDigest
IndexGeneration
ConfigHash
DependencyGraphRevision
BackendEpoch
```

## J4. Query states

Internal query entry state MAY be:

```text
Absent
Computing
Ready
FailedStable
FailedTransient
Evicted
```

A transient backend timeout SHOULD NOT be cached indefinitely.

Stable negative knowledge MAY be cached if its invalidation dependencies are known.

## J5. Cycle handling

Recursive semantic graphs can contain legal cycles.

The query engine MUST distinguish:

- dependency graph cycle;
- implementation deadlock;
- language semantic cycle/error.

Cycle handling MUST be explicit per query family.

A goroutine waiting on itself through memoized futures is a bug and MUST be detected.

## J6. Singleflight/shared futures

Identical in-flight queries SHOULD coalesce.

```text
A asks Q
B asks Q
    -> one computation
    -> two waiters
```

Each waiter retains independent cancellation/deadline.

## J7. Publication rule

A computed query result is not automatically publishable.

```text
compute
 -> attach evidence
 -> validate freshness
 -> validate feature safety class
 -> return canonical result
```

The query cache MAY retain a result that later becomes non-publishable for another Snapshot, but it must never bypass validation.

## J8. Query observability

For sampled/diagnostic traces, record:

```text
query kind
cache state
dependency count
backend calls
duration
cancel status
result status
assurance
```

Do not emit full identifiers/source text into metrics labels.

---

# Part K — Cache Architecture

## K0. Cache layers

Canonical logical layers:

```text
L0 request-local memo
L1 syntax/line-map cache
L2 semantic query cache
L3 dynamic workspace index/cache
L4 persistent local cache/index
L5 remote/static index cache
```

Each layer MUST be:

- bounded;
- versioned;
- observable;
- invalidatable;
- evictable;
- safe under concurrent reads.

## K1. Cache key contract

A semantic cache key includes as applicable:

```text
query kind
content hash
snapshot-independent immutable dependencies
language
backend implementation version
backend semantic version/config
toolchain identity
BuildContextID
configuration hash
schema version
source-map identity
```

If a key omits an input that can change semantics, that is a correctness bug.

## K2. Eviction

Eviction MAY use:

- LRU;
- LFU;
- segmented LRU;
- size-aware policy;
- generational policy.

Correctness MUST NOT depend on cache presence.

Evicted state must be recomputable or explicitly unavailable.

## K3. Negative caching

Negative results MAY be cached only with known invalidation.

Examples:

Safe-ish:

```text
file hash H has no symbol X under BuildContext B
```

Unsafe:

```text
backend unavailable forever
```

Transient operational failures SHOULD use short bounded retry/backoff, not semantic negative caching.

## K4. Cache poisoning prevention

Data loaded from:

- persistent cache;
- remote index;
- old schema;
- old backend;
- other workspace/tenant;

MUST pass identity/version checks before becoming visible.

---

# Part L — Index Architecture

## L0. Index layers

```text
Open-file / Dynamic Semantic Index
              >
Fresh Workspace Persistent Index
              >
Verified Remote/Static Index
```

Freshness and semantic authority dominate physical storage priority.

## L1. Index contents

At minimum:

```text
Symbol
Declaration
Definition
Reference
Implementation
Type relation
Call edge
Import/include edge
Module/package edge
Generated/source-map edge
```

Each record MUST carry enough provenance to prove where it came from.

## L2. Index record identity

Representative metadata:

```go
type IndexRecordMeta struct {
    SchemaVersion   uint32
    Workspace       WorkspaceID
    Repository      RepositoryIdentity
    Revision        SourceRevisionIdentity
    Language        string
    Backend         BackendIdentity
    Toolchain       ToolchainIdentity
    BuildContext    BuildContextID
    SourceHash      ContentHash
    Generation      IndexGeneration
}
```

## L3. Dynamic index

The dynamic index reflects current open/edited state.

It SHOULD be cheap to replace by document/package granularity.

Dynamic entries MUST mask older persistent entries for the same semantic scope.

## L4. Persistent index

Persistent index writes MUST be transactional at publication level.

Required behavior:

```text
build new segment/generation
 -> validate
 -> fsync/commit as appropriate
 -> atomically publish manifest/generation pointer
 -> retire old generation later
```

A crash before publication MUST leave the previous good generation readable.

## L5. Storage engine boundary

The storage implementation is an adapter behind:

```go
type IndexStore interface {
    OpenSnapshot(ctx context.Context) (IndexSnapshot, error)
    BeginBuild(ctx context.Context, meta BuildMeta) (IndexBuilder, error)
    Publish(ctx context.Context, build IndexBuild) error
    Quarantine(ctx context.Context, generation IndexGeneration, reason error) error
    Compact(ctx context.Context, policy CompactPolicy) error
}
```

The spec does not require Semantic Core to know whether the implementation uses:

- immutable segment files;
- SQLite;
- Pebble/LSM;
- another verified store.

The selected v1 store MUST be justified by ADR + corruption/fault benchmarks.

## L6. Index transaction invariants

**IDX-TXN-001**: Readers observe either generation N or N+1, never a half-published mixture.

**IDX-TXN-002**: A partially written segment MUST not be discoverable from the committed manifest.

**IDX-TXN-003**: Publication MUST be idempotently recoverable after process crash.

**IDX-TXN-004**: Corrupt data MUST be quarantined, never trusted because “most fields decoded”.

## L7. Checksums/integrity

Persistent data MUST include:

- format magic;
- schema version;
- record/segment length bounds;
- checksums;
- generation metadata.

Decoders MUST treat file length and offsets as untrusted.

## L8. Corruption recovery

```text
decode/integrity failure
 -> mark generation corrupt
 -> stop serving it
 -> quarantine for diagnostics
 -> fall back to older verified generation if safe
 -> rebuild
```

Core process survival is REQUIRED.

## L9. Index schema migration

Schema change requires one of:

```text
forward-compatible reader
explicit migration
safe rebuild
```

Migration MUST NOT rewrite the only good copy in place without rollback capability.

## L10. Index freshness

A record is fresh only if its semantic identity inputs still match.

At minimum compare applicable:

```text
source hash
build context
toolchain
backend semantic version
workspace/repository revision
schema
generated-source identity
```

## L11. Repository revision identity

For VCS-backed static indexes:

```text
repo identity + commit/tree identity
```

SHOULD be retained.

Working-tree overlays are separate and MUST override repository-static data.

## L12. Remote index

Remote index results are untrusted until validated for:

- repository identity;
- revision/commit;
- schema;
- language;
- backend/indexer version;
- BuildContext compatibility;
- mount/path mapping;
- tenant/auth scope.

Remote results MUST not overwrite an editor overlay.

## L13. Remote index availability

Remote index failure MUST degrade gracefully.

```text
remote timeout
 -> local/dynamic sources continue
```

An outage must not kill local code navigation if local evidence exists.

## L14. SCIP

SCIP is an interoperability/import-export format, not the canonical in-memory model.

```text
Canonical IR
 <-> SCIP Adapter
```

Lossy mappings MUST be documented.

SCIP data imported from elsewhere MUST retain provenance and freshness limits.

## L15. LSIF

LSIF MAY be supported for compatibility.

It SHOULD NOT define new internal architecture when SCIP/canonical IR provides a better model.

## L16. Compaction

Compaction MUST:

- be background priority;
- be cancelable between safe checkpoints;
- not block P0/P1 work for long;
- publish atomically;
- preserve the prior readable generation until the new one is committed.

## L17. Index size limits

The system MUST enforce:

- max record sizes;
- max segment sizes or bounded streaming;
- max decoded collection counts;
- max path/string lengths where appropriate;
- disk budget.

Disk-full errors MUST preserve the last good index.

---

# Part M — Dependency Graph and Incremental Invalidation

## M0. Graph layers

Model:

```text
File
 -> Module/Package/Crate
 -> Build Target
 -> Workspace
 -> External Dependency
```

Edges include:

```text
imports
includes
uses
depends_on
generates
expands
implements
inherits
```

## M1. Change impact

A change MUST invalidate only proven dependents where possible.

Example:

```text
private function body changed
 -> local body queries
 -> callers only if exported semantic facts changed
```

The exact granularity is language-specific.

## M2. Public semantic fingerprint

Language backends SHOULD provide a semantic fingerprint for reusable invalidation.

Examples:

- exported declarations/types;
- public signatures;
- macro output;
- package metadata.

If the public fingerprint is unchanged, downstream invalidation MAY be avoided.

## M3. Conservative fallback

If the engine cannot prove selective invalidation safe:

```text
invalidate broader scope
```

Correctness beats cache retention.

## M4. Generated dependencies

Generated source relationships MUST be explicit:

```text
schema.proto
 -> generator version
 -> generated.pb.go
 -> Go package semantics
```

Changing the generator version can invalidate output even if source schema is unchanged.

---

# Part N — Security and Trust Boundary

## N0. Threat model

Workspace content is attacker-controlled input.

Threat sources include:

- malicious source files;
- malformed Unicode;
- gigantic files;
- crafted parser inputs;
- compile databases;
- build scripts;
- compiler plugins;
- Rust build scripts/proc macros;
- TypeScript plugins;
- Java annotation processors;
- formatter/plugin configurations;
- symlink escapes;
- hostile indexes;
- malicious plugins;
- compromised external backends;
- remote clients;
- oversized protocol messages;
- secret leakage through logs/repro bundles.

## N1. Workspace trust states

```text
Untrusted
Restricted
Trusted
```

Default for newly opened arbitrary repository SHOULD be `Untrusted` unless product UX/policy says otherwise.

Trust is a policy input, not a semantic guess.

## N2. Operation capability model

Dangerous actions require capabilities:

```text
workspace.read
workspace.write
process.execute
network.access
compiler.execute
build_script.execute
proc_macro.execute
plugin.load
formatter.execute
remote_index.read
telemetry.export
```

Capabilities MAY be granted by local policy, admin policy, or explicit user trust.

## N3. Untrusted mode

In Untrusted mode OmniLSP MUST NOT automatically:

- execute arbitrary workspace scripts;
- run shell commands from repository files;
- load compiler plugins from the workspace;
- load TypeScript plugins;
- run arbitrary formatters;
- download/execute binaries;
- send source code to remote services;
- install plugins;
- run code actions containing unapproved commands.

## N4. Compile database safety

`compile_commands.json` may contain shell-like command strings.

Policy:

```text
parse
 -> extract semantic flags
 -> invoke configured trusted compiler/backend directly
```

Do not execute the original command through `sh -c`, `cmd /C`, PowerShell, or equivalent.

## N5. Process execution

All process APIs MUST accept structured argv/environment.

```go
type ExecSpec struct {
    Executable TrustedExecutable
    Args       []string
    Env        map[string]string
    Dir        CanonicalPath
    Limits     ResourceLimits
    Network    NetworkPolicy
}
```

No generic “run arbitrary string command” primitive in Semantic Core.

## N6. Sandbox

External workers SHOULD support OS-level isolation where available:

- restricted working directory;
- environment allowlist;
- CPU limit;
- memory limit;
- process-count limit;
- file descriptor limit;
- network deny by default in untrusted mode;
- restricted writable paths;
- child-process containment.

The exact mechanism is platform-specific.

## N7. Symlink containment

Security checks MUST resolve effective filesystem targets.

Forbidden:

```text
workspaceRoot prefix check only
```

Required:

```text
canonical target
 -> containment policy
```

Race-sensitive operations SHOULD minimize TOCTOU exposure.

## N8. Remote transport security

Non-loopback remote endpoints MUST require encrypted transport.

Recommended:

```text
TLS 1.3 where platform permits
authentication
authorization/RBAC
rate limiting
request size limits
audit events for mutating/execution actions
```

Local loopback debug endpoints MAY use ephemeral bearer tokens.

## N9. MCP security

MCP adapter security MUST follow the pinned MCP revision’s authorization model where remote HTTP authorization is used.

Tool calls that mutate code or execute commands SHOULD require stronger scopes/consent than read-only code intelligence.

## N10. Multi-tenant security

If multi-tenancy is enabled:

- tenant ID is part of every persistent namespace;
- caches cannot cross tenants by accident;
- trace search is tenant-scoped;
- remote index auth is tenant-scoped;
- worker filesystem roots are isolated;
- credentials are not inherited across tenants.

## N11. Secret redaction

Logs/traces/crash reports MUST redact by default:

```text
API keys
authorization headers
cookies
tokens
private keys
environment secrets
credentials in URLs
full source contents
```

## N12. Source privacy

Telemetry MUST NOT export source text by default.

Allowed by default:

- aggregate timing;
- error codes;
- hashed backend/version identity;
- bounded structural metrics.

Source snippets require explicit opt-in and clear UX/policy.

## N13. Repro bundle privacy

`omnilsp repro` MUST support:

```text
metadata-only
redacted
full-source explicit opt-in
```

The default MUST be metadata/redacted, not full private source.

## N14. Plugin trust

Plugin metadata is untrusted until verified.

Plugin installation SHOULD validate:

- package signature/checksum;
- declared capabilities;
- publisher/source policy;
- API version;
- platform;
- dependency integrity.

## N15. Supply chain

Release pipeline SHOULD produce:

- SBOM;
- dependency license inventory;
- cryptographic release checksums;
- signed artifacts where supported;
- provenance/CI metadata.

Dependency updates SHOULD be vulnerability-scanned.

---

# Part O — Plugin Architecture

## O0. Plugin principle

Plugins MUST NOT receive pointers to mutable core state.

They interact only through stable capability APIs.

## O1. Plugin tiers

### Tier 1 — Out-of-process backend/analyzer

Preferred for high isolation.

### Tier 2 — WASM/WASI sandbox

OPTIONAL, for constrained analyzers where runtime maturity and performance are acceptable.

### Tier 3 — In-process Go extension

NOT a general public plugin mechanism. Reserved for built-in, reviewed modules compiled with the product.

Go `plugin` MUST NOT be the only extensibility strategy.

## O2. Plugin capabilities

Example manifest capabilities:

```text
document.read
workspace.read
index.query
diagnostics.emit
code_action.emit
network.access
process.execute
filesystem.write
```

Default is deny.

## O3. Plugin manifest

```yaml
id: vendor.plugin
version: 1.2.3
apiVersion: omnilsp.plugin.v1
entrypoint: ...
languages: [go]
capabilities:
  - document.read
checksum: ...
signature: ...
```

## O4. Plugin lifecycle

```text
Discovered
 -> Verified
 -> Installed
 -> Disabled/Enabled
 -> Running
 -> Failed
 -> Quarantined
 -> Removed
```

Crash loops MUST be contained.

## O5. Plugin API versioning

Core API and Plugin API version independently.

A plugin must declare compatible API ranges.

No direct import of unstable `internal/*` packages.

---

# Part P — Observability and Operations

## P0. Correlation IDs

Every request SHOULD carry/derive:

```text
TraceID
RequestID
WorkspaceID
SnapshotID
LanguageID
BackendID
BackendEpoch
BuildContextID
```

## P1. Trace structure

Example:

```text
protocol.request
  -> admission
  -> snapshot.capture
  -> scheduler.wait
  -> query
     -> cache
     -> backend
     -> index
  -> validation
  -> protocol.project
```

A TraceID SHOULD reconstruct the operational path without exposing private source by default.

## P2. Metrics

At minimum:

```text
request_count
request_latency
queue_latency
active_requests
rejected_requests
cancelled_requests
timeout_requests

backend_latency
backend_failures
backend_restarts
backend_quarantine

query_cache_hit
query_cache_miss
query_compute_latency

index_records
index_bytes
index_build_latency
index_corruption
index_generation

snapshot_live_count
snapshot_age
memory_bytes
goroutines
open_fds where available
```

## P3. Metric cardinality

Metrics MUST NOT use high-cardinality labels such as:

- URI;
- full workspace path;
- SymbolID;
- RequestID;
- TraceID;
- raw error text.

These belong in traces/logs, often hashed/redacted.

## P4. Logs

Structured logs SHOULD contain:

```text
timestamp
level
component
operation
error kind/code
trace ID
request ID
backend
snapshot revision
recoverability
```

Source text is excluded by default.

## P5. Sampling

Tracing MAY be sampled.

Errors, panics, backend crashes, and S3 failures SHOULD have elevated sampling.

Sampling MUST NOT alter behavior.

## P6. Debug server

Optional debug server:

```text
/health
/ready
/metrics
/debug/pprof/
/debug/state
/debug/requests
/debug/index
/debug/backends
```

Policy:

- default bind loopback only;
- remote exposure requires auth;
- endpoints MUST not expose raw source by default;
- expensive endpoints need rate/budget limits.

## P7. Health semantics

`/health`:

```text
core process alive and event loop responsive
```

`/ready`:

```text
core ready to serve configured minimum capability
```

Backend-specific degradation belongs in detailed status.

Do not mark the whole server unhealthy because one optional language worker is unavailable.

## P8. Doctor

`omnilsp doctor` SHOULD check:

- binary version;
- OS/arch;
- config parse;
- workspace trust;
- backend discovery;
- backend versions;
- Go/C++/Rust/Python/Node/JDK environments as applicable;
- compile database presence;
- index health;
- cache directory permissions;
- disk space;
- socket/port conflicts;
- telemetry/debug settings.

Doctor output MUST distinguish:

```text
PASS
WARN
FAIL
SKIP
```

## P9. Replay

Protocol replay MUST be able to reconstruct:

```text
initialize
workspace attach
open
change
request
cancel
save
close
shutdown
```

A replay record SHOULD include:

- protocol messages;
- timestamps or logical sequence;
- environment/toolchain digests;
- config hash;
- backend versions;
- snapshot transitions.

Deterministic replay SHOULD use logical ordering rather than relying on original wall-clock timing.

## P10. Reproduction bundle

A repro bundle SHOULD contain:

```text
manifest.json
session trace
config snapshot
backend inventory
index metadata
environment digest
optional redacted/full source fixture
```

No secrets.

---

# Part Q — Error Model

## Q0. Typed error taxonomy

```go
type ErrorKind uint16

const (
    ErrProtocol ErrorKind = iota
    ErrInvalidPosition
    ErrInvalidDocumentVersion
    ErrStaleSnapshot
    ErrContentModified
    ErrBackendUnavailable
    ErrBackendCrashed
    ErrBuildContext
    ErrParse
    ErrSemantic
    ErrIndex
    ErrIndexCorrupt
    ErrTimeout
    ErrCancelled
    ErrOverloaded
    ErrPermission
    ErrUntrustedOperation
    ErrUnsupported
    ErrInternal
)
```

## Q1. Error record

```go
type OmniError struct {
    Kind         ErrorKind
    Code         string
    Operation    string
    Workspace    WorkspaceID
    Snapshot     SnapshotID
    BuildContext BuildContextID
    Backend      BackendID
    TraceID      TraceID
    Recoverable  bool
    Cause        error
}
```

## Q2. Error projection

Each protocol adapter owns mapping from canonical error to protocol-specific errors.

Semantic Core MUST NOT return LSP numeric error codes directly.

## Q3. No generic swallowing

Forbidden:

```go
if err != nil {
    return nil // pretending "no results"
}
```

unless the canonical API explicitly defines that error as an empty successful semantic set.

Operational failure and semantic emptiness are distinct.

## Q4. No silent fallback

Fallback MUST be observable in `Evidence`.

Example:

```text
compiler backend unavailable
 -> syntax fallback
 -> result assurance = Syntax
 -> mutating feature disabled
```

Never:

```text
compiler backend unavailable
 -> regex result
 -> report as exact
```

---

# Part R — Configuration and Versioning

## R0. Configuration layers

Canonical precedence, lowest to highest:

```text
compiled defaults
 -> system/admin policy
 -> user/global
 -> workspace
 -> language
 -> backend
 -> session/CLI override
```

Security/admin policy MAY define non-overridable ceilings.

## R1. Configuration schema

Every setting MUST have:

```text
key
type
default
description
scope
reload class
validation
security sensitivity
deprecated state
replacement/migration
```

Configuration SHOULD be backed by a machine-readable schema.

Unknown configuration keys:

```text
warning
```

not silent ignore.

## R2. Reload classes

Each setting belongs to:

```text
Live
WorkspaceReload
BackendRestart
CoreRestart
ImmutableForSession
```

A hot reload MUST never mutate a Snapshot already captured by a request.

New settings become visible through a new configuration/workspace revision.

## R3. Configuration validation

Validation MUST catch:

- invalid types;
- invalid enum;
- impossible numeric ranges;
- conflicting options;
- unsafe remote bind without auth where prohibited;
- nonexistent explicitly required backend;
- invalid path/URI;
- unsupported protocol version.

## R4. Version surfaces

Version independently:

```text
Product Version
Canonical Core API
LSP Compatibility Profile
MCP Compatibility Profile
Backend Worker RPC
Plugin API
Index Schema
Configuration Schema
Public gRPC/HTTP API
Custom LSP Extension API
Replay Format
```

Do not couple all schema migrations to the product SemVer.

## R5. Compatibility policy

Public stable APIs SHOULD follow:

```text
same major: backward compatible by default
minor: additive feature
patch: bug/security fix
major: breaking change permitted with migration notes
```

Internal package APIs are not public compatibility commitments.

## R6. Backend version policy

A backend descriptor MUST include:

```text
backend adapter version
upstream engine name/version
protocol version
semantic capability fingerprint
```

A backend update that can alter semantic results SHOULD invalidate affected persistent semantic caches/indexes.

## R7. Feature flags

Feature flags MUST have:

- owner;
- default;
- purpose;
- expiration/removal plan;
- safety classification;
- observability.

Flags MUST NOT become permanent hidden configuration.

Critical correctness behavior MUST NOT silently depend on random experiment assignment.

## R8. Deprecation

Deprecations SHOULD have:

```text
introduced version
deprecated version
replacement
warning behavior
earliest removal version/date policy
migration tool if needed
```

---

# Part S — Testing and Verification

## S0. Test philosophy

Testing is part of semantic architecture.

A feature is incomplete until its correctness boundary is testable.

Required layers:

```text
unit
property
fuzz
golden
differential
integration
protocol conformance
client compatibility
fault injection
race
stress
soak
performance
security
replay
```

## S1. Unit tests

Unit tests focus on pure contracts:

- URI parsing/normalization;
- position conversion;
- text edits;
- line maps;
- cache keys;
- BuildContext canonicalization;
- SymbolID encoding;
- error projection;
- configuration precedence.

## S2. Property-based testing

Mandatory high-value properties:

### Position

```text
byte -> position -> byte roundtrip
valid UTF-16 boundary conversion
incremental line map == full rebuild
```

### Edits

```text
apply normalized edit set
 == reference implementation
```

### URI

```text
parse -> canonical -> parse
preserves identity
```

### Snapshot

```text
published snapshot never changes
reader sees one revision
```

### Index

```text
publish crash at any injected phase
 -> old or new valid generation
 -> never mixed generation
```

## S3. Fuzzing

Minimum Go fuzz targets:

```text
JSON-RPC framing
JSON decode
LSP decode
URI parsing
position conversion
incremental didChange
WorkspaceEdit validation
source-map projection
BuildContext parser/canonicalizer
index decoder
index manifest recovery
protocol code generation parser
custom extension decode
config decoder
replay decoder
```

Fuzz invariants:

```text
no panic
no deadlock
no unbounded allocation
no out-of-range memory access
no invalid published edit/location
```

## S4. Cancel-race fuzz/stress

Explicitly exercise:

```text
request starts
 -> backend call
 -> edit arrives
 -> cancel arrives
 -> backend responds
 -> index publishes
```

Permute ordering.

Expected result is a valid terminal state, never double response or stale publish.

## S5. Differential testing

For mature backends:

```text
OmniLSP canonical result
        vs
Pinned upstream canonical engine
```

Compare as applicable:

- definition;
- references;
- hover/type;
- completion candidate validity;
- diagnostics;
- rename;
- symbol identity;
- hierarchy.

Normalize only protocol-format differences, not semantic mismatches.

## S6. Differential mismatch workflow

```text
detect mismatch
 -> record exact environment
 -> minimize fixture
 -> classify:
      OmniLSP bug
      upstream bug
      supported semantic difference
      build-context mismatch
 -> add regression
 -> fix/waive via explicit issue/ADR
```

No unexplained “golden update” to hide regressions.

## S7. Golden repositories

Every Tier S language MUST include:

```text
tiny
medium
large
pathological
broken-code
real-world representative
multi-root
generated-code
Unicode
platform-conditional
```

Licensing of real-world fixtures MUST permit repository inclusion or reproducible fetching.

## S8. Broken-code corpus

IDE code is frequently invalid.

Test:

```text
unfinished identifiers
missing delimiters
half-written generic/template
broken import
partial expression
temporary type errors
unterminated string/comment
merge conflict markers
rapid undo/redo
```

Expected:

```text
responsive
no panic
safe partial structural capability
semantic uncertainty explicit
```

## S9. Edit-sequence tests

Required sequences include:

```text
initialize
open
change
change
completion
cancel
change
definition
save
rename
close
shutdown
```

Also:

```text
open A
open B
rename file A externally
watcher event loss
rescan
references
```

and:

```text
header with context X
header with context Y
rename
```

which MUST reject ambiguity if semantic scope differs.

## S10. Protocol conformance

Test:

- valid/invalid JSON-RPC IDs;
- notifications vs requests;
- initialize lifecycle;
- cancellation;
- progress;
- partial results;
- unknown enums;
- unknown fields;
- dynamic registration;
- position encoding;
- document sync modes;
- workspace folders;
- file operations;
- semantic token full/delta;
- pull diagnostics;
- notebook sync;
- server initiated requests.

## S11. Client compatibility matrix

At minimum:

```text
VS Code
Neovim
Emacs/Eglot
Helix
Zed
Sublime LSP
```

Each client profile records:

- version tested;
- position encoding;
- dynamic registration behavior;
- WorkspaceEdit capabilities;
- pull diagnostics;
- semantic tokens;
- notebook support if relevant;
- known compatibility shims.

Client-specific behavior MUST stay in compatibility layer.

## S12. Backend compatibility matrix

For each Tier S backend:

```text
minimum supported upstream version
recommended version
maximum tested version
protocol/CLI assumptions
known unsupported capabilities
security/trust notes
```

CI SHOULD test at least minimum + recommended/latest-supported versions.

## S13. Fault injection

Inject:

```text
backend SIGKILL
backend hang
backend malformed response
backend version mismatch
disk full
permission denied
index checksum failure
manifest torn write
cache directory deleted
workspace deleted
symlink target changed
network disconnect
remote index timeout
client disconnect
partial JSON-RPC frame
out-of-order watcher events
memory pressure
file descriptor exhaustion
```

Validate:

```text
core survives where specified
state remains consistent
request terminates exactly once
resources released
status explains degradation
recovery path works
```

## S14. Race detector

Release CI MUST include race testing for core concurrency-sensitive packages.

A race in:

```text
snapshot
vfs
scheduler
query
index
backend supervisor
protocol runtime
```

is a release blocker.

## S15. Stress testing

Stress workload:

- many open documents;
- rapid edits;
- concurrent completion/hover/definition;
- background indexing;
- backend restart;
- repeated cancellation;
- repeated workspace attach/detach.

Track:

```text
tail latency
memory growth
goroutine growth
FD growth
queue saturation
backend churn
error rate
```

## S16. Soak testing

Release Candidate SHOULD sustain at least 24 hours of representative mixed workload.

Stable release MUST have a defined soak duration; 24h is the initial minimum target.

Failure conditions:

- monotonic memory leak;
- monotonic goroutine leak;
- FD leak;
- deadlock/livelock;
- repeated backend crash loop;
- stale edit/location;
- corrupt index publication.

## S17. Performance benchmark methodology

Every benchmark report MUST state:

```text
hardware
OS/filesystem
product build/commit
backend versions
toolchains
repository/corpus revision
cold/warm state
cache state
worker count
memory limit
```

Without this metadata, latency numbers are not comparable.

## S18. Initial interactive SLO targets

These are initial engineering targets on a documented local SSD reference machine, warmed semantic state, representative Tier S corpus:

| Operation                      | P50 target | P95 target | P99 target |
| ------------------------------ | ---------: | ---------: | ---------: |
| hot hover                      |   <= 20 ms |   <= 75 ms |  <= 150 ms |
| hot definition                 |   <= 25 ms |  <= 100 ms |  <= 200 ms |
| completion first usable result |   <= 40 ms |  <= 120 ms |  <= 250 ms |
| syntax update after edit       |   <= 15 ms |   <= 50 ms |  <= 100 ms |

These targets MUST NOT justify incorrect semantics.

A backend that cannot meet them may ship with a documented performance exception if correctness/reliability gates pass and an optimization issue exists.

## S19. Large-query SLO

References/workspace symbols/indexing are throughput-oriented.

Requirements:

- cancelable;
- progress-observable;
- bounded memory;
- partial/streaming where protocol allows;
- no starvation of P0/P1.

Absolute latency depends on corpus size and MUST be reported as scaling curves.

## S20. Accuracy KPI

Track separately per feature/language/backend:

```text
Precision
Recall
False Positive Rate
False Negative Rate
Unknown/Refusal Rate
Stale Result Rejection Rate
Wrong-file Rate
Position Mapping Failure
Edit Validation Failure
```

Priority:

```text
False Positive for S3
    >
False Negative / refusal
```

## S21. Qualified corpus zero-error goals

Release goal for qualified corpus:

```text
Wrong Edit Rate            = 0
Stale Edit Applied         = 0
Wrong-file Location        = 0
Position Mapping Error     = 0
Protocol-invalid Response  = 0
Snapshot Mixing            = 0
```

“0” is a release-corpus requirement, not a mathematical claim about all future programs.

## S22. Metamorphic tests

Useful transformations:

- add irrelevant whitespace/comments;
- reorder independent declarations where language permits;
- change line endings;
- move file without semantic namespace change;
- add unrelated file;
- add unrelated build target.

Results whose semantics should be invariant MUST remain invariant modulo locations.

## S23. Security tests

Test:

- path traversal;
- symlink escape;
- malicious compile command;
- command injection strings;
- oversized JSON;
- decompression/expansion bombs if applicable;
- hostile plugin manifest;
- remote auth bypass;
- cross-tenant cache leakage;
- secret redaction.

---

# Part T — Release Gates and Definition of Done

## T0. P0 release blockers

Any of the following blocks Stable:

```text
stale edit applied
wrong-file edit
out-of-range edit published
snapshot mixing
silent semantic guess in S3
rename textual false-positive
protocol-invalid response
core deadlock
unbounded goroutine growth
backend crash kills core
index corruption kills core
cross-tenant data leak
remote unauthenticated code mutation
secret leakage in default telemetry
```

## T1. Foundation DoD

Foundation is complete only when:

```text
JSON-RPC runtime
LSP initialize/lifecycle
VFS
URI engine
Position engine
incremental document sync
Snapshot engine
Scheduler
Cancellation
Backpressure
Error taxonomy
Telemetry skeleton
Replay skeleton
Config schema
Race CI
Fuzz CI
```

are implemented and integration-tested.

No Tier S language feature is allowed to bypass these foundations.

## T2. Go Product DoD

Go is daily-usable only when:

```text
completion
hover
signature
definition/declaration
references
document/workspace symbols
diagnostics
semantic tokens
rename
code actions required by product scope
```

work through canonical APIs with evidence and snapshot validation.

## T3. C/C++ DoD

Required:

```text
clangd/clang-backed adapter
compile_commands support
context provenance
header ambiguity handling
definition
references
hover
diagnostics
completion
rename
macro location policy
```

No full semantic claim when compile context is unknown.

## T4. Core language pack DoD

Rust/Python/TS-JS become stable only after:

- backend supervisor integration;
- trust model integration;
- language build/environment model;
- differential corpus;
- broken-code corpus;
- rename safety rules;
- install/doctor support.

## T5. Persistent intelligence DoD

Required:

```text
persistent index
atomic generation publish
corruption detection
migration/rebuild
dependency-directed invalidation
SCIP adapter
replay/repro
disk budget
```

## T6. Universal interfaces DoD

MCP/gRPC/HTTP/WS are complete only when they:

- project canonical semantics;
- preserve auth/trust;
- preserve Snapshot/evidence rules;
- have versioned schemas;
- have compatibility tests;
- do not introduce semantic-core dependencies.

## T7. Hardening DoD

Stable candidate requires:

```text
race clean
fuzz corpus clean
golden clean
differential triaged
protocol conformance pass
client compatibility pass
fault injection pass
24h+ soak pass
memory leak pass
cancellation pass
Unicode/position pass
security suite pass
```

---

# Part U — Repository Architecture and Dependency Rules

## U0. Reference repository layout

```text
cmd/
  omnilsp/
  omni-index/
  omni-debug/

internal/
  canonical/
    request/
    result/
    evidence/
    identity/

  protocol/
    jsonrpc/
    lsp/
    mcp/
    dap/

  transport/
    stdio/
    pipe/
    tcp/
    websocket/
    http/

  runtime/
    server/
    lifecycle/
    admission/
    scheduler/
    resource/

  workspace/
    model/
    vfs/
    document/
    uri/
    position/
    snapshot/
    source_map/
    buildctx/
    watcher/

  semantic/
    api/
    router/
    symbol/
    reference/
    graph/
    diagnostics/
    refactor/
    completion/

  query/
    engine/
    dependency/
    memo/

  index/
    api/
    dynamic/
    persistent/
    remote/
    scip/
    lsif/

  backend/
    api/
    supervisor/
    process/
    rpc/
    bridge/

  languages/
    golang/
    cpp/
    rust/
    python/
    typescript/
    java/
    syntax/

  plugin/
    api/
    manifest/
    runtime/
    policy/

  config/
  security/
  telemetry/
  debug/
  doctor/
  replay/
  repro/
  compat/

api/
  proto/
  openapi/
  mcp/

editors/
  vscode/

test/
  corpus/
  golden/
  differential/
  integration/
  protocol/
  compat/
  fuzz/
  race/
  fault/
  stress/
  soak/
  security/
  replay/

tools/
  protocolgen/
  benchmark/
  indexinspect/
  release/
  corpus/

docs/
  architecture/
  adr/
  compatibility/
  language/
  operations/
  security/
```

## U1. Dependency direction

Canonical dependency direction:

```text
Protocol/CLI Adapter
      ↓
Canonical API
      ↓
Runtime / Workspace / Semantic
      ↓
Backend API / Query / Index
      ↓
Concrete language/storage/process adapters
```

Forbidden:

```text
semantic -> lsp
query -> vscode
workspace -> mcp
canonical -> concrete clangd process package
```

## U2. Package invariants

Every core package MUST have package documentation stating:

- responsibility;
- owned mutable state;
- concurrency model;
- invariants;
- allowed dependencies;
- failure behavior;
- primary tests.

## U3. Interface placement

Interfaces SHOULD be owned by the consumer when idiomatic and practical, except canonical public contracts intentionally shared across modules.

Avoid giant “god interfaces”.

## U4. Generated code

Generated code lives in clearly marked directories/files.

CI MUST verify:

```text
generator inputs pinned
run generator
git diff --exit-code
```

## U5. Dependency policy

Each third-party dependency must justify:

```text
why needed
license
maintenance health
security posture
platform impact
cgo/native dependency impact
core contamination risk
replacement cost
```

Compiler/tool integration belongs behind adapters.

## U6. cgo policy

cgo MAY be used in isolated adapters if it materially improves correctness/performance.

Semantic Core SHOULD remain buildable without pervasive cgo.

A cgo dependency must document:

- supported platforms;
- cross-compilation impact;
- crash boundary;
- license;
- allocator/thread interaction if relevant.

## U7. Code quality

Required:

```text
gofmt
go vet
static analysis
race tests
fuzz targets
lint policy with low false-positive noise
```

Warnings MUST NOT be globally disabled simply to make CI green.

## U8. No global mutable singleton

Global immutable registries MAY exist.

Global mutable workspace/runtime state is forbidden unless explicitly actor-owned and lifecycle-scoped.

## U9. ADRs

Architecture Decision Records are REQUIRED for decisions that materially affect:

- canonical interfaces;
- concurrency model;
- index store;
- protocol compatibility policy;
- plugin security;
- language backend source of truth;
- storage format;
- public API break;
- telemetry privacy.

ADR states:

```text
Proposed
Accepted
Superseded
Rejected
```

## U10. Spec amendment process

A change that violates a MUST requires:

1. issue/problem statement;
2. alternative analysis;
3. ADR;
4. spec update;
5. migration/test update;
6. compatibility impact;
7. release note if user-visible.

Code alone cannot silently redefine the spec.

---

# Part V — Agent / Multi-Worktree Execution Contract

## V0. Goal

This section is explicitly optimized for AI coding agents and multiple parallel worktrees.

The objective is not maximum generated code volume.

The objective is:

```text
small verified increments
+ stable interfaces
+ explicit invariants
+ reproducible tests
```

## V1. Mandatory task sequence

Before implementing a complex module:

```text
Inspect repository
 -> identify existing implementation
 -> read relevant Spec clauses
 -> research upstream primary source if needed
 -> define invariants
 -> define API boundary
 -> define tests/fuzz hooks
 -> implement smallest complete slice
 -> run focused tests
 -> run affected integration tests
 -> benchmark if hot path
 -> review for invariant violations
```

## V2. No blind rewrite

An Agent MUST NOT replace a working subsystem merely because a fresh implementation is easier to generate.

Before rewrite it MUST state:

- current behavior;
- measured defect;
- migration plan;
- compatibility risk;
- test coverage preserving behavior.

## V3. No compiler reinvention

For complex language semantics, the Agent MUST prefer mature compiler/language-service sources of truth unless an ADR authorizes native reimplementation.

Forbidden task framing:

```text
"Implement C++ overload resolution with Tree-sitter"
```

## V4. Core-interface freeze

Shared contracts:

```text
Snapshot
Canonical Result/Evidence
BuildContext
SymbolID
Backend API
Query API
Index API
Error model
```

require coordinated review.

Parallel Agents MUST NOT independently mutate these interfaces.

## V5. Workstream split

Recommended tracks:

```text
A  JSON-RPC + LSP + protocol codegen
B  VFS + URI + Position + Snapshot
C  Scheduler + cancellation + resource manager
D  Query + cache + dependency graph
E  Index + persistence + SCIP
F  Backend supervisor + worker RPC
G  Go backend
H  C/C++ backend
I  Rust/Python/TS backends
J  Security + plugin policy
K  Testing + fuzz + replay
L  Telemetry + debug + doctor
M  Distribution + editor integration
```

## V6. Task contract

Every nontrivial Agent task SHOULD start with:

```text
Spec clauses:
Inputs:
Outputs:
Invariants:
Out of scope:
Tests required:
Compatibility impact:
```

## V7. PR contract

Every PR SHOULD report:

```text
Spec clauses implemented
Behavior changed
Tests added
Fuzz target impact
Benchmark impact
Security impact
Migration impact
Known limitations
```

## V8. Bug policy

Every semantic/correctness bug:

```text
reproduce
 -> minimize
 -> identify violated invariant
 -> regression test
 -> fix root cause
 -> search sibling edge cases
 -> add fuzz seed if useful
```

A fix without regression coverage is incomplete unless impossible and documented.

## V9. No fake completion

Agents MUST NOT declare a module complete because:

```text
it compiles
```

Completion means its DoD and tests pass.

## V10. Temporary fallback policy

Temporary fallback must be:

- explicit;
- lower assurance;
- capability-gated;
- issue-tracked;
- unable to satisfy higher safety classes;
- observable.

Never leave a TODO fallback that silently returns fabricated exact data.

---

# Part W — CLI, Packaging, Distribution, and Operations

## W0. CLI contract

Canonical commands:

```bash
omnilsp serve
omnilsp doctor
omnilsp status
omnilsp languages
omnilsp index <workspace>
omnilsp query definition ...
omnilsp query references ...
omnilsp explain <file:line:column>
omnilsp replay <trace>
omnilsp repro ...
omnilsp plugins ...
omnilsp version
```

`install` MAY be an external installer command rather than a self-mutating binary.

## W1. CLI exit codes

Define stable categories:

```text
0 success
2 invalid CLI/config
3 workspace/build-context error
4 backend unavailable
5 semantic result unavailable/unknown where command requires exact result
6 permission/trust error
7 protocol/remote error
8 internal error
```

Exact values MAY be adjusted before first stable release, then become compatibility surface.

## W2. Machine output

CLI SHOULD support:

```text
--json
```

Machine output schema MUST be versioned.

Human logs MUST not corrupt stdout JSON; use stderr for diagnostics.

## W3. Supported platforms

Core target:

```text
Windows x86_64
Linux x86_64
Linux arm64
macOS arm64
macOS x86_64 while ecosystem/toolchains remain supported
```

Additional platforms MAY be community-supported.

Backend availability matrix can be narrower than core binary matrix, but must be explicit.

## W4. Packaging

Target distribution channels MAY include:

- GitHub releases;
- Homebrew;
- Scoop/WinGet/Chocolatey according to maintenance capacity;
- Linux packages;
- container image for remote mode.

Do not block core engineering on supporting every package manager.

## W5. Release artifact contents

Each release SHOULD include:

```text
binary
checksums
signature/provenance when available
SBOM
license notices
compatibility matrix
migration notes
known issues
backend requirements
```

## W6. Backend discovery

Search order SHOULD be deterministic:

```text
explicit configured path
 -> managed OmniLSP backend directory
 -> trusted PATH lookup
 -> language-specific discovery
```

Report which binary/version was selected.

Never silently switch to a different backend executable mid-session without creating a new backend epoch/context.

## W7. Updates

Auto-update is OPTIONAL.

If implemented:

- signed metadata/artifacts;
- rollback;
- no update during active critical edit transaction;
- explicit channel (stable/beta/nightly);
- no silent downgrade.

## W8. Reproducible builds

Release pipeline SHOULD strive for reproducibility.

At minimum record:

```text
Go version
module graph
build flags
commit
generated protocol inputs
embedded assets hashes
```

---

# Part X — Milestones

## X0. Milestone 0 — Spec and skeleton

Deliver:

- this canonical spec;
- ADR template;
- package skeleton;
- CI;
- requirement-to-test convention.

Exit: no duplicate architecture document is treated as authoritative.

## X1. Milestone 1 — Foundation

Deliver Part T1.

Exit criteria:

```text
open/edit/query lifecycle works on synthetic backend
UTF-16/UTF-8 tests pass
race clean
fuzz targets active
replay can reproduce session
```

## X2. Milestone 2 — Go daily-use product

Use gopls bridge first.

Exit:

- daily editor use;
- rename safety;
- differential corpus;
- doctor/install path;
- VS Code + Neovim compatibility.

## X3. Milestone 3 — C/C++

Use clangd bridge.

Exit:

- compile_commands;
- header-context ambiguity handled;
- macro mapping policy;
- large repo index tests.

## X4. Milestone 4 — Core language pack

Rust + Python + TS/JS.

Exit:

- each language has environment/build-context contract;
- supervisor isolation;
- language-specific correctness corpus;
- security gating for executable project features.

## X5. Milestone 5 — Persistent intelligence

Deliver:

- local persistent index;
- atomic recovery;
- SCIP;
- dependency-directed invalidation;
- index inspector;
- corruption testing.

## X6. Milestone 6 — Universal read APIs

Deliver:

- MCP 2026-07-28 adapter;
- gRPC v1;
- HTTP read API;
- auth/rate limiting;
- canonical result metadata.

Mutating remote APIs remain opt-in until S3 security gates pass.

## X7. Milestone 7 — Plugin platform

Deliver:

- out-of-process plugins;
- manifest;
- capability policy;
- quarantine;
- signing/checksum flow.

WASM optional.

## X8. Milestone 8 — Distribution

Deliver:

- supported-platform binaries;
- VS Code extension;
- generic LSP docs;
- package/install workflow;
- doctor;
- release metadata/SBOM.

## X9. Milestone 9 — Hardening

Deliver Part T7.

## X10. Milestone 10 — Advanced scale

OPTIONAL:

- remote index service;
- remote semantic workers;
- distributed indexing;
- multi-tenant hosted service;
- advanced cross-language graph;
- DAP integration.

These MUST NOT destabilize local core.

---

# Part Y — Canonical Acceptance Checklist

## Y0. Correctness

- [ ] Every request captures exactly one Snapshot.
- [ ] Incremental edits are never state-dropped.
- [ ] Position conversion is property-tested.
- [ ] Build context participates in semantic identity.
- [ ] Stale results cannot publish.
- [ ] Rename never falls back to textual replacement.
- [ ] Workspace edits are preflight validated.
- [ ] Ambiguous build contexts fail closed for S3.
- [ ] Generated/source-map gaps block unsafe edits.
- [ ] Backend restart invalidates old-epoch results.

## Y1. Stability

- [ ] Queues bounded.
- [ ] Caches bounded.
- [ ] Goroutines owned.
- [ ] Cancellation propagates.
- [ ] Backend crash isolated.
- [ ] Index corruption isolated.
- [ ] Disk full preserves last good state.
- [ ] Panic boundary tested.
- [ ] Race detector clean.
- [ ] Soak has no monotonic resource leak.

## Y2. Compatibility

- [ ] LSP 3.17 baseline passes.
- [ ] LSP 3.18 features pinned/gated.
- [ ] Unknown future fields/enums tolerated where allowed.
- [ ] Position encoding negotiation tested.
- [ ] WorkspaceEdit capability differences tested.
- [ ] Client compatibility matrix current.
- [ ] MCP revision pinned.
- [ ] Public API schemas versioned.

## Y3. Security

- [ ] New workspace defaults follow trust policy.
- [ ] No shell execution of compile database commands.
- [ ] Dangerous plugins/build scripts trust-gated.
- [ ] Remote non-loopback endpoint authenticated/encrypted.
- [ ] Secrets redacted.
- [ ] Telemetry source-free by default.
- [ ] Repro bundle privacy modes tested.
- [ ] Plugin capabilities least-privilege.
- [ ] Supply-chain artifacts generated.

## Y4. Observability

- [ ] Request -> Snapshot -> Query -> Backend -> Index traceable.
- [ ] Backend health visible.
- [ ] Queue saturation visible.
- [ ] Cache hit/miss visible.
- [ ] Metrics cardinality bounded.
- [ ] Explain API reports evidence/fallback.
- [ ] Doctor identifies environment faults.

## Y5. Maintainability

- [ ] Core packages document invariants.
- [ ] No protocol imports in Semantic Core.
- [ ] ADRs exist for major architecture choices.
- [ ] Generated files reproducible.
- [ ] Dependencies justified.
- [ ] Public API/version policy documented.
- [ ] Agent tasks reference Spec clauses.

---

# Appendix A — Core Invariant Register

| ID               | Invariant                                     | Failure severity |
| ---------------- | --------------------------------------------- | ---------------- |
| INV-ARCH-001     | protocol is not semantic truth                | P0/P1            |
| INV-ARCH-003     | one request, one Snapshot                     | P0               |
| INV-SNAPSHOT-001 | published Snapshot immutable                  | P0               |
| INV-SNAPSHOT-003 | old Snapshot not mutated by new publication   | P0               |
| PROT-SYNC-001    | accepted incremental edits applied losslessly | P0               |
| INV-POS-001      | valid position roundtrip exact                | P0               |
| SEM-SAFE-001     | S3 fails closed without proof                 | P0               |
| INV-BACKEND-001  | backend epoch prevents stale worker result    | P0               |
| IDX-TXN-001      | no mixed index generations                    | P0               |
| IDX-TXN-004      | corrupt index never trusted                   | P0               |
| SEC-EXEC-001     | no untrusted shell command execution          | P0               |
| OPS-PRIV-001     | no source export by default telemetry         | P0/P1            |

---

# Appendix B — Feature/Evidence Matrix

| Feature             |                   Syntax |                                           Fresh Index | Compiler/Language Service | Completeness requirement |
| ------------------- | -----------------------: | ----------------------------------------------------: | ------------------------: | ------------------------ |
| folding             |                      yes |                                                    no |                        no | local                    |
| selection range     |                      yes |                                                    no |                        no | local                    |
| document symbols    |                      yes |                                              optional |                  optional | may partial              |
| syntax tokens       |                      yes |                                                    no |                        no | local                    |
| semantic tokens     |                  partial |                                              optional |                 preferred | may partial              |
| hover               |                  limited |                                          yes if exact |                 preferred | target exact             |
| definition          | structural-only fallback |                                    yes if exact/fresh |                 preferred | returned targets exact   |
| references          |         no textual guess |                                                   yes |                 preferred | may known-subset         |
| implementation      |                       no |                                          yes if exact |                 preferred | may known-subset         |
| completion          |           lexical subset |                                         index assists |                 preferred | candidates safe          |
| diagnostics         |        syntax-only class |                                               limited |                 preferred | class explicit           |
| rename              |                       NO | insufficient alone unless backend proves completeness | REQUIRED equivalent proof | complete                 |
| workspace edit      |                       NO |                                                    NO |   semantic proof required | complete + fresh         |
| code-action command |                      n/a |                                                   n/a |                   backend | trust required           |

---

# Appendix C — Canonical Cache-Key Matrix

| Cache                   | Required semantic inputs                                               |
| ----------------------- | ---------------------------------------------------------------------- |
| line map                | content hash + line-ending/text encoding policy                        |
| syntax tree             | content hash + parser version + language dialect                       |
| Go package              | file hashes + Go toolchain + module graph + GOOS/GOARCH/tags           |
| C++ TU                  | source hash + compile command canonical hash + clang identity          |
| Rust crate              | source set + crate graph + features/cfg + toolchain + expansion inputs |
| Python analysis         | source + interpreter/version + search roots + config/stubs             |
| TS project              | source + tsconfig/compilerOptions + TS version + package graph         |
| diagnostic              | semantic query key + diagnostic config                                 |
| semantic tokens         | snapshot/document + legend + backend/tokenizer version                 |
| persistent symbol index | schema + backend + toolchain + build context + source hash             |

---

# Appendix D — Backend Supervisor Transition Rules

```text
Disabled
  | enable
  v
Starting
  | handshake+capability OK
  v
Ready
  | partial dependency/index failure
  v
Degraded
  | semantic RPC failures exceed threshold
  v
Unhealthy
  | restart allowed
  v
Backoff
  | timer
  +------> Starting

Unhealthy
  | crash-loop threshold
  v
Quarantined
  | manual/config/toolchain change
  +------> Starting
```

Any transition away from `Ready` MUST be observable.

---

# Appendix E — Request State Machine

```text
Decoded
  -> Admitted
  -> SnapshotCaptured
  -> Queued
  -> Running
  -> Validating
  -> Projecting
  -> Responded

From Queued/Running/Validating:
  -> Cancelled
  -> TimedOut
  -> ContentModified
  -> BackendUnavailable
  -> InternalError

From Decoded/Admitted:
  -> ProtocolError
  -> RejectedOverload
```

Exactly one terminal outcome.

---

# Appendix F — Workspace Mutation Commit Pattern

```text
1. Receive mutation.
2. Validate lifecycle/document identity.
3. Load current immutable state.
4. Apply mutation to new immutable objects.
5. Recompute cheap derived identities/hashes.
6. Increment workspace revision.
7. Atomically publish Snapshot N+1.
8. Schedule invalidation/background work.
9. Do not wait for expensive semantic work in writer.
```

---

# Appendix G — S3 Refactoring Proof Obligations

Before returning a mutating edit, prove:

```text
P1 target symbol resolved
P2 build context unambiguous or explicitly selected
P3 reference set complete for operation
P4 all locations map to editable source
P5 no stale document/index/backend epoch
P6 no lexical/semantic collisions after rename
P7 edit ranges valid and non-overlapping
P8 file operations permitted
P9 client can enforce required version semantics
P10 trust/security policy permits action
```

Failure of any Pi aborts the operation.

---

# Appendix H — Production Review Questions

Every architecture/code review SHOULD ask:

1. What is the source of truth?
2. Which Snapshot is this bound to?
3. Which BuildContext changes the answer?
4. Can stale data cross this boundary?
5. What happens if the backend crashes here?
6. What happens if cancellation races with completion?
7. Is this queue/cache/goroutine bounded?
8. Can a malicious workspace turn this into code execution?
9. Can a Unicode/CRLF/path edge case move the location?
10. Is fallback explicit in Evidence?
11. How is this fuzzed/differentially tested?
12. Can a TraceID explain the result?
13. What invalidates the cache/index record?
14. What happens after disk full or corrupt persistence?
15. Which release gate catches a regression?

---

# Appendix I — Final Engineering Law

```text
Source of Truth
      ↓
Canonicalized Build Context
      ↓
Immutable Snapshot
      ↓
Compiler-grade / Explicitly Classified Semantics
      ↓
Dependency-tracked Query
      ↓
Versioned Index
      ↓
Evidence + Freshness Validation
      ↓
Protocol Projection
      ↓
Result
```

Never invert this into:

```text
Protocol request
 -> heuristic shortcut
 -> plausible answer
```

The final product is acceptable only when a user, editor, agent, or remote service can reasonably answer:

```text
Can it install?
Can it start?
Can it understand the correct build?
Can it survive malformed input?
Can it survive backend failure?
Can it avoid stale edits?
Can it explain where a result came from?
Can it recover its index?
Can it be upgraded?
Can it be extended without modifying Semantic Core?
Can it be trusted with a real production repository?
```

All must be **YES** within the declared capability/trust profile.