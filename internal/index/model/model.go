// Package model defines the format-neutral facts and extraction contract for
// persistent semantic indexes. It deliberately has no dependency on a storage
// format or language backend.
//
// Invariants: semantic identities come from an extractor, positions are
// canonical UTF-16, and unknown completeness is never promoted to complete.
package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
)

const SchemaVersion uint32 = 1

var (
	ErrMissingView       = errors.New("semantic index: missing immutable workspace view")
	ErrSnapshotMismatch  = errors.New("semantic index: extraction report belongs to another snapshot")
	ErrExtractionFailed  = errors.New("semantic index: extraction reported errors")
	ErrInvalidCoverage   = errors.New("semantic index: invalid coverage declaration")
	ErrDuplicateCoverage = errors.New("semantic index: duplicate coverage declaration")
	ErrMissingCoverage   = errors.New("semantic index: report omitted a required scope/fact coverage entry")
	ErrInvalidScope      = errors.New("semantic index: invalid or duplicate extraction scope")
	ErrInvalidProvenance = errors.New("semantic index: invalid extraction provenance")
)

type FactKind string

const (
	FactSymbol         FactKind = "symbol"
	FactDeclaration    FactKind = "declaration"
	FactDefinition     FactKind = "definition"
	FactReference      FactKind = "reference"
	FactImplementation FactKind = "implementation"
	FactTypeRelation   FactKind = "type_relation"
	FactCall           FactKind = "call"
	FactImport         FactKind = "import"
	FactInclude        FactKind = "include"
	FactModule         FactKind = "module"
	FactGenerated      FactKind = "generated_source"
)

var RequiredFactKinds = [...]FactKind{
	FactSymbol, FactDeclaration, FactDefinition, FactReference,
	FactImplementation, FactTypeRelation, FactCall, FactImport,
	FactInclude, FactModule, FactGenerated,
}

type Completeness string

const (
	Complete              Completeness = "complete"
	IncompleteKnownSubset Completeness = "incomplete_known_subset"
	Unknown               Completeness = "unknown"
	Unavailable           Completeness = "unavailable"
)

type EdgeKind string

const (
	EdgeImplementation EdgeKind = "implementation"
	EdgeTypeRelation   EdgeKind = "type_relation"
	EdgeCall           EdgeKind = "call"
	EdgeImport         EdgeKind = "import"
	EdgeInclude        EdgeKind = "include"
	EdgeModule         EdgeKind = "module"
	EdgeGenerated      EdgeKind = "generated_source"
)

type Scope struct {
	ID           string
	Language     string
	RootURI      string
	BuildContext identity.BuildContextID
	Build        BuildInputs
}

// BuildInputs records the inputs that affect semantic interpretation. BuildContext
// remains the canonical digest used by the existing runtime and storage layers.
// These fields make that digest auditable and give extractors explicit inputs.
type BuildInputs struct {
	Environment     map[string]string
	Arguments       []string
	PackagePatterns []string
	IncludePaths    []string
	Defines         map[string]string
	Features        []string
	Options         map[string]string
	Tests           bool
}

type Identity struct {
	Workspace   identity.WorkspaceID
	DiskDigest  identity.ContentHash
	Repository  string
	Revision    string
	SnapshotRev uint64 // process-local build guard; never a persistent cache key
}

type File struct {
	URI        string
	LanguageID string
	Size       int64
	SHA256     identity.ContentHash
	Generated  bool
	SourceURI  string
	SourceMap  []SourceMapSpan
}

// SourceMapSpan maps a generated-source range to its original logical source.
// Ranges use the canonical UTF-16 position encoding below.
type SourceMapSpan struct {
	Generated Position
	SourceURI string
	Source    Position
}

// WorkspaceView is immutable for its lifetime. Read returns bytes from the
// same captured view reported by Walk, never from the mutable source tree.
type WorkspaceView interface {
	Identity() Identity
	Walk(ctx context.Context, rootURI string, visit func(File) error) error
	Read(ctx context.Context, uri string) (io.ReadCloser, error)
}

// WorkspaceMaterializer is optional. External extractors should require it and
// run only against the captured snapshot, never the mutable user workspace.
type WorkspaceMaterializer interface {
	Materialize(ctx context.Context, rootURI, destination string) (MaterializedView, error)
}

// MaterializedView maps an immutable logical workspace subtree into an isolated
// local directory. PathForURI must reject URIs outside RootURI and return paths
// that stay inside RootPath.
type MaterializedView interface {
	RootURI() string
	RootPath() string
	PathForURI(uri string) (string, error)
	Close() error
}

type Position struct {
	// All positions are zero-based UTF-16 code units, independent of the
	// extractor's native encoding. Exporters must convert before writing.
	StartLine uint32
	StartChar uint32
	EndLine   uint32
	EndChar   uint32
}

type Symbol struct {
	ID        identity.SymbolID
	ScopeID   string
	Name      string
	Kind      string
	Signature string
}

type Occurrence struct {
	SymbolID     identity.SymbolID
	ScopeID      string
	URI          string
	Range        Position
	Role         string
	SourceHash   identity.ContentHash
	BuildContext identity.BuildContextID
}

type Edge struct {
	From         identity.SymbolID
	To           identity.SymbolID
	ScopeID      string
	Kind         EdgeKind
	SourceURI    string
	Range        Position
	SourceHash   identity.ContentHash
	BuildContext identity.BuildContextID
}

type Coverage struct {
	ScopeID string
	Fact    FactKind
	State   Completeness
	Reason  string
}

type Provenance struct {
	SchemaVersion uint32
	Identity      Identity
	Scope         Scope
	Extractor     string
	ExtractorVer  string
	Backend       identity.BackendID
	BackendEpoch  identity.BackendEpoch
	Toolchain     string
	Tools         []ToolIdentity
}

// ToolIdentity pins an external extractor/compiler executable used for this
// scope. SHA256 is the executable content hash, not a version label.
type ToolIdentity struct {
	Name    string
	Path    string
	Version string
	SHA256  string
}

type Request struct {
	// WorkspaceRootURI is the captured workspace boundary. Scope.RootURI remains
	// the project boundary; dependencies may live in sibling projects, but must
	// never be read outside this explicitly supplied immutable workspace.
	WorkspaceRootURI string
	View             WorkspaceView
	Scopes           []Scope
	Provenance       map[string]Provenance // keyed by Scope.ID
}

// ComputeBuildContextID derives the persistent semantic context key from the
// complete scoped inputs and pinned extractor/toolchain identities. The
// process-local snapshot revision is deliberately excluded.
func ComputeBuildContextID(scope Scope, extractor, extractorVersion, toolchain string, tools []ToolIdentity) identity.BuildContextID {
	canonicalTools := append([]ToolIdentity(nil), tools...)
	sort.Slice(canonicalTools, func(i, j int) bool {
		if canonicalTools[i].Name != canonicalTools[j].Name {
			return canonicalTools[i].Name < canonicalTools[j].Name
		}
		if canonicalTools[i].Path != canonicalTools[j].Path {
			return canonicalTools[i].Path < canonicalTools[j].Path
		}
		if canonicalTools[i].Version != canonicalTools[j].Version {
			return canonicalTools[i].Version < canonicalTools[j].Version
		}
		return canonicalTools[i].SHA256 < canonicalTools[j].SHA256
	})
	canonical := struct {
		ScopeID      string
		Language     string
		RootURI      string
		Build        BuildInputs
		Extractor    string
		ExtractorVer string
		Toolchain    string
		Tools        []ToolIdentity
	}{scope.ID, scope.Language, scope.RootURI, scope.Build, extractor, extractorVersion, toolchain, canonicalTools}
	data, _ := json.Marshal(canonical) // all fields above have deterministic encodings
	sum := sha256.Sum256(data)
	return identity.BuildContextID(scope.Language + ":sha256:" + hex.EncodeToString(sum[:16]))
}

type Report struct {
	Identity  Identity
	Coverage  []Coverage
	UsedTools map[string][]ToolIdentity
	Errors    []string
}

// Sink accepts bounded batches. Implementations must validate and persist each
// batch incrementally rather than retaining the whole workspace in memory.
type Sink interface {
	WriteSymbols(context.Context, []Symbol) error
	WriteOccurrences(context.Context, []Occurrence) error
	WriteEdges(context.Context, []Edge) error
}

func ValidateReport(req Request, report Report) error {
	if req.View == nil {
		return ErrMissingView
	}
	if report.Identity != req.View.Identity() {
		return ErrSnapshotMismatch
	}
	if len(report.Errors) != 0 {
		return errors.Join(ErrExtractionFailed, errors.New(report.Errors[0]))
	}
	seen := make(map[string]Completeness, len(req.Scopes)*len(RequiredFactKinds))
	scopes := make(map[string]Scope, len(req.Scopes))
	for _, scope := range req.Scopes {
		if scope.ID == "" || scope.Language == "" || scope.RootURI == "" || scope.BuildContext == "" {
			return ErrInvalidScope
		}
		if _, exists := scopes[scope.ID]; exists {
			return ErrInvalidScope
		}
		scopes[scope.ID] = scope
		p, ok := req.Provenance[scope.ID]
		if !ok || p.SchemaVersion != SchemaVersion || p.Identity != report.Identity || !reflect.DeepEqual(p.Scope, scope) ||
			p.Extractor == "" || p.ExtractorVer == "" || p.Toolchain == "" ||
			p.Backend.Language != scope.Language || p.Backend.Name == "" ||
			scope.BuildContext != ComputeBuildContextID(scope, p.Extractor, p.ExtractorVer, p.Toolchain, p.Tools) {
			return ErrInvalidProvenance
		}
		for _, tool := range p.Tools {
			if tool.Name == "" || !absoluteToolPath(tool.Path) || tool.Version == "" || !validSHA256(tool.SHA256) {
				return ErrInvalidProvenance
			}
		}
		used := report.UsedTools[scope.ID]
		if !ScopeHasNoAttestedFacts(scope.ID, report.Coverage) && !HasPinnedToolUsage(p.Tools, used) {
			return ErrInvalidProvenance
		}
		for _, actual := range used {
			if actual.Name == "" || !absoluteToolPath(actual.Path) || actual.Version == "" || !validSHA256(actual.SHA256) {
				return ErrInvalidProvenance
			}
		}
	}
	for scopeID := range report.UsedTools {
		if _, ok := scopes[scopeID]; !ok {
			return ErrInvalidProvenance
		}
	}
	for _, coverage := range report.Coverage {
		if coverage.State != Complete && coverage.State != IncompleteKnownSubset &&
			coverage.State != Unknown && coverage.State != Unavailable {
			return ErrInvalidCoverage
		}
		if _, ok := scopes[coverage.ScopeID]; !ok || !validFactKind(coverage.Fact) ||
			coverage.Reason == "" && coverage.State != Complete {
			return ErrInvalidCoverage
		}
		key := coverage.ScopeID + "\x00" + string(coverage.Fact)
		if _, exists := seen[key]; exists {
			return ErrDuplicateCoverage
		}
		seen[key] = coverage.State
	}
	for _, scope := range req.Scopes {
		for _, fact := range RequiredFactKinds {
			if _, ok := seen[scope.ID+"\x00"+string(fact)]; !ok {
				return ErrMissingCoverage
			}
		}
	}
	return nil
}

// ScopeHasNoAttestedFacts reports whether every required fact kind for scopeID
// is explicitly unknown or unavailable. Such scopes may have no tool usage
// when the extractor declines them before launching pinned tools.
func ScopeHasNoAttestedFacts(scopeID string, coverage []Coverage) bool {
	states := make(map[FactKind]Coverage, len(RequiredFactKinds))
	for _, item := range coverage {
		if item.ScopeID == scopeID {
			states[item.Fact] = item
		}
	}
	for _, fact := range RequiredFactKinds {
		item, ok := states[fact]
		if !ok || (item.State != Unavailable && item.State != Unknown) || item.Reason == "" {
			return false
		}
	}
	return true
}

// HasPinnedToolUsage reports whether every pinned tool has a matching record
// in used. Extra used tools are allowed because a provider may invoke helpers
// that do not affect the pinned build context.
func HasPinnedToolUsage(pinned, used []ToolIdentity) bool {
	for _, expected := range pinned {
		found := false
		for _, actual := range used {
			if actual == expected {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func validSHA256(value string) bool { return sha256Pattern.MatchString(value) }

func absoluteToolPath(value string) bool {
	if filepath.IsAbs(value) {
		return true
	}
	if len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && (value[2] == '/' || value[2] == '\\') {
		return true
	}
	return strings.HasPrefix(value, `\\`)
}

func validFactKind(kind FactKind) bool {
	for _, required := range RequiredFactKinds {
		if kind == required {
			return true
		}
	}
	return false
}
