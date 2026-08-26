// Package identity provides canonical identity types for the OmniLSP platform.
//
// Invariants:
//  1. All IDs are stable, non-memory-address-based identifiers.
//  2. IDs are immutable after creation.
//  3. Process memory addresses MUST NEVER be used as persistent identity.
//
// Corresponds to goal.md §B0 (Identity primitives).
package identity

import (
	"fmt"
	"time"
)

// WorkspaceID uniquely identifies a workspace.
type WorkspaceID string

// SessionID uniquely identifies an LSP client session.
type SessionID string

// SnapshotRevision is a monotonically increasing revision counter.
type SnapshotRevision uint64

// DocumentVersion is the editor-reported version for a single document.
type DocumentVersion int64

// BackendEpoch increments each time a backend is successfully restarted.
// Used to prevent publishing stale results from a previous backend generation.
//
// INV-BACKEND-001: A result produced by epoch N MUST NOT be published as if it came from epoch N+1.
type BackendEpoch uint64

// IndexGeneration increments when the index schema or content is rebuilt.
type IndexGeneration uint64

// ContentHash is a cryptographic digest of document or file content.
type ContentHash string

// SymbolID is a stable, language-aware semantic identity for a symbol.
// Must NOT be a memory address.
type SymbolID string

// QueryKey is a canonical deterministic key for memoized query results.
type QueryKey string

// TraceID uniquely identifies a single request trace across all components.
type TraceID string

// RequestID uniquely identifies a single protocol request.
type RequestID string

// BackendID identifies a specific backend instance.
type BackendID struct {
	Language string
	Name     string
}

// BuildContextID is a digest-backed identifier for a build configuration.
type BuildContextID string

// SnapshotID uniquely identifies a workspace snapshot.
type SnapshotID struct {
	Workspace WorkspaceID
	Revision  SnapshotRevision
}

func (id SnapshotID) String() string {
	return fmt.Sprintf("snap{%s:%d}", id.Workspace, id.Revision)
}

// VFSRevision is the monotonically increasing revision counter for the VFS.
type VFSRevision uint64

// BuildContextSetID identifies a set of build contexts active for a workspace.
type BuildContextSetID string

// WorkspaceModelRevision tracks changes to the workspace model (folders, etc.).
type WorkspaceModelRevision uint64

// EvidenceKind categorizes the source of semantic evidence.
type EvidenceKind uint8

const (
	EvidenceNone     EvidenceKind = iota
	EvidenceLexical               // Text/regex — basic structural info only
	EvidenceSyntax                // AST-derived, no type information
	EvidenceIndex                 // Cross-file index lookup
	EvidenceSemantic              // Type-checked semantic resolution
	EvidenceCompiler              // Compiler-native definitive answer
)

func (k EvidenceKind) String() string {
	switch k {
	case EvidenceLexical:
		return "lexical"
	case EvidenceSyntax:
		return "syntax"
	case EvidenceIndex:
		return "index"
	case EvidenceSemantic:
		return "semantic"
	case EvidenceCompiler:
		return "compiler"
	default:
		return "unknown"
	}
}

// Assurance represents the strength of semantic evidence.
type Assurance uint8

const (
	AssuranceLexical Assurance = iota
	AssuranceSyntax
	AssuranceIndexedExact
	AssuranceCompilerResolved
)

// ResultStatus indicates the completeness of a semantic result.
type ResultStatus uint8

const (
	ResultExact       ResultStatus = iota // Complete and verified
	ResultPartial                         // Known subset, incompleteness acknowledged
	ResultUnknown                         // Cannot determine with available evidence
	ResultUnavailable                     // Backend or data source unavailable
)

func (s ResultStatus) String() string {
	switch s {
	case ResultExact:
		return "exact"
	case ResultPartial:
		return "partial"
	case ResultUnknown:
		return "unknown"
	case ResultUnavailable:
		return "unavailable"
	default:
		return "invalid"
	}
}

// Completeness describes how complete a result is.
type Completeness uint8

const (
	Complete              Completeness = iota // Fully complete
	IncompleteKnownSubset                     // Known to be a subset
	CompletenessUnknown                       // Cannot determine completeness
)

// Evidence records the provenance of a semantic result.
//
// Corresponds to goal.md §B4 (Evidence record).
type Evidence struct {
	Kind         EvidenceKind
	Assurance    Assurance
	Snapshot     SnapshotID
	BuildContext BuildContextID
	Backend      BackendID
	BackendEpoch BackendEpoch
	IndexGen     IndexGeneration
	SourceHash   ContentHash
	DetailCode   string
	Timestamp    time.Time
}

// SemanticResult is the canonical envelope for all semantic query outputs.
//
// Corresponds to goal.md §B5 (Canonical semantic result envelope).
type SemanticResult[T any] struct {
	Status       ResultStatus
	Value        T
	Evidence     []Evidence
	Completeness Completeness
	// InternalDiagnostics are not projected to the client; used for explain API.
	InternalDiagnostics []string
}

// NewExactResult creates a result marked as exact with the given evidence.
func NewExactResult[T any](val T, ev []Evidence) SemanticResult[T] {
	return SemanticResult[T]{
		Status:       ResultExact,
		Value:        val,
		Evidence:     ev,
		Completeness: Complete,
	}
}

// NewPartialResult creates a result marked as a known subset.
func NewPartialResult[T any](val T, ev []Evidence) SemanticResult[T] {
	return SemanticResult[T]{
		Status:       ResultPartial,
		Value:        val,
		Evidence:     ev,
		Completeness: IncompleteKnownSubset,
	}
}

// NewUnknownResult returns an explicitly unknown result (never a wrong guess).
func NewUnknownResult[T any](ev []Evidence) SemanticResult[T] {
	var zero T
	return SemanticResult[T]{
		Status:       ResultUnknown,
		Value:        zero,
		Evidence:     ev,
		Completeness: CompletenessUnknown,
	}
}

// NewUnavailableResult indicates the backend could not produce a result.
func NewUnavailableResult[T any](ev []Evidence) SemanticResult[T] {
	var zero T
	return SemanticResult[T]{
		Status:       ResultUnavailable,
		Value:        zero,
		Evidence:     ev,
		Completeness: CompletenessUnknown,
	}
}

// IsStale checks whether this result is a stale unpublished result.
// A ResultUnavailable with no evidence indicates the backend never produced anything
// (stale/never-resolved). A ResultUnavailable WITH evidence is a valid explicit unknown.
// ResultStale is intentionally absent from publishable results per §B5.
func (r SemanticResult[T]) IsStale() bool {
	return r.Status == ResultUnavailable && len(r.Evidence) == 0
}

// SafetyClass enumerates the §B7 semantic feature safety tiers. Higher values
// mutate more; S3+ requires completeness proof, S4 additionally requires
// §S4-grade trust gates at execution time.
type SafetyClass int

const (
	// SafetyReadOnly never mutates workspace state (hover, definition).
	SafetyReadOnly SafetyClass = iota
	// SafetyLocal derives purely from the open buffer (folding, syntax tokens).
	SafetyLocal
	// SafetyIndex consults cross-file index data (references, workspace symbol).
	SafetyIndex
	// SafetyMutating produces edits (rename, formatting) — SEM-SAFE-001 applies.
	SafetyMutating
	// SafetyExecuting runs commands or processes — §N trust gates apply.
	SafetyExecuting
)

func (s SafetyClass) String() string {
	switch s {
	case SafetyReadOnly:
		return "S0"
	case SafetyLocal:
		return "S1"
	case SafetyIndex:
		return "S2"
	case SafetyMutating:
		return "S3"
	case SafetyExecuting:
		return "S4"
	default:
		return "S?"
	}
}
