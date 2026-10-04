package rustanalyzer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/interop"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/nested"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

const (
	rustAnalyzerToolName       = "rust-analyzer"
	rustSemanticHelperToolName = "rust-analyzer-semantic-helper"
	cargoToolName              = "cargo"
	rustcToolName              = "rustc"
	procMacroToolName          = "proc-macro-srv"
	rustPlannerInputsOption    = "cargo.plannerInputs.v1"
	rustPlannerInputsVersion   = 1
	rustSemanticModelVersion   = "3"
	maxRustPlannerInputsBytes  = 64 * 1024
	maxRustPlannerScopes       = 4096
	maxRustMetadataBytes       = 32 * 1024 * 1024
	maxRustRelationBytes       = 64 * 1024 * 1024
	maxRustRelations           = 250_000
	maxRustWitnessCandidates   = 262_144
	maxRustWitnessBytes        = 64 * 1024 * 1024
	maxRustSemanticFileBytes   = 64 * 1024 * 1024
	rustSemanticBatchSize      = 1024
	rustHelperOutputBytes      = 16 * 1024 * 1024
)

// scipInvocation is the complete, shell-free command contract. Keeping the
// runner behind this value makes tool execution deterministic in tests while
// production still runs the pinned executable directly.
type scipInvocation struct {
	Tool       model.ToolIdentity
	Args       []string
	Dir        string
	Env        []string
	OutputPath string
}

type scipRunner func(context.Context, scipInvocation) ([]byte, error)

type rustRelationSidecar struct {
	SchemaVersion     int                   `json:"schema_version"`
	SCIPSHA256        string                `json:"scip_sha256,omitempty"`
	SelectedCrateRoot string                `json:"selected_crate_root,omitempty"`
	SourceManifest    []rustWitnessSource   `json:"source_manifest,omitempty"`
	WitnessCounts     *rustWitnessCounts    `json:"witness_counts,omitempty"`
	Blockers          []string              `json:"blockers,omitempty"`
	Relations         []rustSidecarRelation `json:"relations"`
}

type rustWitnessSource struct {
	Document string `json:"document"`
	SHA256   string `json:"sha256"`
}

type rustWitnessCounts struct {
	SourceFiles               uint64 `json:"source_files"`
	UnmappedSourceFiles       uint64 `json:"unmapped_source_files"`
	SCIPDocuments             uint64 `json:"scip_documents"`
	SCIPSymbols               uint64 `json:"scip_symbols"`
	SCIPOccurrences           uint64 `json:"scip_occurrences"`
	CandidateTokens           uint64 `json:"candidate_tokens"`
	ClassifiedCandidateTokens uint64 `json:"classified_candidate_tokens"`
	UnknownCandidateTokens    uint64 `json:"unknown_candidate_tokens"`
	MacroSites                uint64 `json:"macro_sites"`
	ErrorDiagnostics          uint64 `json:"error_diagnostics"`
	ExternalTargets           uint64 `json:"external_targets"`
	DuplicateSymbols          uint64 `json:"duplicate_symbols"`
}

type rustSidecarRelation struct {
	Kind           string           `json:"kind"`
	SourceDocument string           `json:"source_document"`
	SourceSymbol   string           `json:"source_symbol"`
	TargetSymbol   string           `json:"target_symbol"`
	Range          rustSidecarRange `json:"range"`
}

type rustSidecarPosition struct {
	Line      *uint32 `json:"line"`
	Character *uint32 `json:"character"`
}

type rustSidecarRange struct {
	Start *rustSidecarPosition `json:"start"`
	End   *rustSidecarPosition `json:"end"`
}

type rustSCIPRange struct {
	startLine uint32
	startChar uint32
	endLine   uint32
	endChar   uint32
}

type rustPendingEdge struct {
	kind       model.EdgeKind
	from       identity.SymbolID
	to         identity.SymbolID
	sourceURI  string
	sourceHash identity.ContentHash
	rangeValue model.Position
}

type rustCompletenessValidation struct {
	Complete bool
	Reason   string
}

// RustIndexToolConfig contains explicit, trusted Cargo and rustc paths plus
// the requested Cargo build inputs. rust-analyzer is pinned from the exact
// executable attached to the live Backend. SemanticHelper is an optional,
// separately pinned same-source SCIP exporter; it never replaces the live
// protocol backend.
type RustIndexToolConfig struct {
	Tools          []model.ToolIdentity
	SemanticHelper model.ToolIdentity
	Build          model.BuildInputs
	Toolchain      string
}

type cargoMetadataRunner func(context.Context, scipInvocation) ([]byte, error)

type rustPlannerInputMarker struct {
	Version int               `json:"version"`
	Build   model.BuildInputs `json:"build"`
}

// SemanticIndexRequestBuilder discovers Cargo package/target scopes from an
// immutable materialization. Runtime wiring supplies this builder beside the
// live Backend, so request provenance pins that backend's exact RA executable.
type SemanticIndexRequestBuilder struct {
	backend      *Backend
	config       RustIndexToolConfig
	run          cargoMetadataRunner
	activeRAPath func() string
}

var _ languages.SemanticIndexRequestBuilder = (*SemanticIndexRequestBuilder)(nil)

func NewSemanticIndexRequestBuilder(backend *Backend, config RustIndexToolConfig) *SemanticIndexRequestBuilder {
	return &SemanticIndexRequestBuilder{
		backend: backend, config: cloneRustIndexToolConfig(config), run: runCargoMetadata,
		activeRAPath: func() string {
			if backend == nil || backend.conn == nil {
				return ""
			}
			return backend.conn.ExecutablePath()
		},
	}
}

func discoverRustCargoPackages(
	ctx context.Context,
	view model.WorkspaceView,
	rootURI string,
	build model.BuildInputs,
	tools map[string]model.ToolIdentity,
	run cargoMetadataRunner,
) (packages []cargoPackage, retErr error) {
	if view == nil {
		return nil, model.ErrMissingView
	}
	if run == nil {
		return nil, errors.New("rust semantic index: Cargo metadata runner is unavailable")
	}
	materializer, ok := view.(model.WorkspaceMaterializer)
	if !ok {
		return nil, errors.New("rust semantic index: immutable workspace view cannot be materialized")
	}
	tempRoot, err := os.MkdirTemp("", "omnilsp-rust-plan-")
	if err != nil {
		return nil, fmt.Errorf("create isolated Cargo metadata directory: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(tempRoot); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove isolated Cargo metadata directory: %w", cleanupErr))
		}
	}()
	materialized, err := materializer.Materialize(ctx, rootURI, filepath.Join(tempRoot, "workspace"))
	if err != nil {
		return nil, fmt.Errorf("materialize captured Cargo workspace: %w", err)
	}
	if materialized == nil || materialized.RootURI() != rootURI || materialized.RootPath() == "" {
		return nil, errors.New("rust semantic index: materializer returned an invalid workspace mapping")
	}
	defer func() {
		if closeErr := materialized.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close materialized Cargo workspace: %w", closeErr))
		}
	}()
	if err := verifyCapturedWorkspace(ctx, view, materialized, rootURI); err != nil {
		return nil, err
	}
	rootPath, err := filepath.Abs(materialized.RootPath())
	if err != nil {
		return nil, err
	}
	if err := verifyTemporaryIsolation(tempRoot, rootPath); err != nil {
		return nil, err
	}
	manifest := filepath.Join(rootPath, "Cargo.toml")
	if info, statErr := os.Stat(manifest); statErr != nil || !info.Mode().IsRegular() {
		if statErr != nil {
			return nil, fmt.Errorf("captured Rust workspace has no readable Cargo.toml: %w", statErr)
		}
		return nil, errors.New("captured Rust workspace Cargo.toml is not a regular file")
	}
	targetDir, err := os.MkdirTemp(tempRoot, "metadata-target-")
	if err != nil {
		return nil, fmt.Errorf("create isolated Cargo metadata target directory: %w", err)
	}
	baseScope := model.Scope{Language: langID, RootURI: rootURI, Build: cloneBuildInputs(build)}
	metadata, runErr := run(ctx, scipInvocation{
		Tool: tools[cargoToolName],
		Args: []string{"metadata", "--no-deps", "--locked", "--offline", "--format-version", "1", "--manifest-path", manifest},
		Dir:  rootPath, Env: rustCommandEnv(baseScope, tools, targetDir, tempRoot),
	})
	if err := verifyCapturedWorkspace(ctx, view, materialized, rootURI); err != nil {
		return nil, fmt.Errorf("Cargo metadata changed the captured workspace: %w", err)
	}
	if runErr != nil {
		return nil, fmt.Errorf("pinned Cargo metadata failed: %w", runErr)
	}
	if len(metadata) > maxRustMetadataBytes {
		return nil, fmt.Errorf("pinned Cargo metadata output exceeds %d bytes", maxRustMetadataBytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return decodeWorkspacePackages(metadata, rootPath, build.PackagePatterns)
}

func (b *SemanticIndexRequestBuilder) BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (request model.Request, retErr error) {
	if view == nil {
		return model.Request{}, model.ErrMissingView
	}
	if err := ctx.Err(); err != nil {
		return model.Request{}, err
	}
	capturedIdentity := view.Identity()
	if rootURI == "" || b == nil || b.backend == nil || b.activeRAPath == nil {
		return model.Request{}, errors.New("rust semantic index: request builder or workspace root is unavailable")
	}
	canonicalRoot, err := canonicalRustWorkspaceRoot(rootURI)
	if err != nil {
		return model.Request{}, fmt.Errorf("rust semantic index: invalid workspace root: %w", err)
	}
	rootURI = canonicalRoot
	plannerInputs, err := encodeRustPlannerInputs(b.config.Build)
	if err != nil {
		return model.Request{}, err
	}
	if b.run == nil {
		return model.Request{}, errors.New("rust semantic index: Cargo metadata runner is unavailable")
	}
	tools, err := b.verifiedConfiguredTools(ctx)
	if err != nil {
		return model.Request{}, err
	}
	packages, err := discoverRustCargoPackages(ctx, view, rootURI, b.config.Build, tools, b.run)
	if err != nil {
		return model.Request{}, err
	}
	if view.Identity() != capturedIdentity {
		return model.Request{}, model.ErrInvalidProvenance
	}
	identity := capturedIdentity
	request = model.Request{WorkspaceRootURI: rootURI, View: view, Provenance: make(map[string]model.Provenance)}
	for _, pkg := range packages {
		for _, target := range pkg.Targets {
			if target.isBuildScript() || (target.isTestTarget() && !b.config.Build.Tests) {
				continue
			}
			if len(request.Scopes) >= maxRustPlannerScopes {
				return model.Request{}, fmt.Errorf("Rust Cargo metadata produced more than %d planner scopes", maxRustPlannerScopes)
			}
			scope := makeRustTargetScope(rootURI, pkg, target, b.config.Build, plannerInputs)
			pinnedTools := toolsForBuildScope(scope, tools)
			extractorVersion := rustSemanticExtractorVersion(tools[rustAnalyzerToolName].Version)
			scope.BuildContext = model.ComputeBuildContextID(scope, "rust-analyzer-scip", extractorVersion, b.config.Toolchain, pinnedTools)
			provenance := model.Provenance{
				SchemaVersion: model.SchemaVersion, Identity: identity, Scope: scope,
				Extractor: "rust-analyzer-scip", ExtractorVer: extractorVersion,
				Backend: identityBackend(), Toolchain: b.config.Toolchain, Tools: pinnedTools,
			}
			request.Scopes = append(request.Scopes, scope)
			request.Provenance[scope.ID] = provenance
		}
	}
	if len(request.Scopes) == 0 {
		return model.Request{}, errors.New("pinned Cargo metadata contained no selected Rust crate targets")
	}
	sort.Slice(request.Scopes, func(i, j int) bool { return request.Scopes[i].ID < request.Scopes[j].ID })
	return request, nil
}

// RebuildVerifiedPlannerRequest repeats Cargo's cheap, locked scope discovery
// from the captured workspace and pinned tool files. It does not need a live
// rust-analyzer backend and never launches RA, the semantic helper, or SCIP.
// Cargo metadata is the only child process used to rediscover workspace
// members and auto-discovered targets.
func RebuildVerifiedPlannerRequest(ctx context.Context, view model.WorkspaceView, rootURI string, attestations []model.Provenance) (model.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if view == nil {
		return model.Request{}, model.ErrMissingView
	}
	currentIdentity := view.Identity()
	canonicalRoot, err := canonicalRustWorkspaceRoot(rootURI)
	if err != nil || rootURI == "" || currentIdentity.Workspace == "" || currentIdentity.DiskDigest == "" || len(attestations) == 0 || len(attestations) > maxRustPlannerScopes {
		return model.Request{}, model.ErrInvalidProvenance
	}
	if err := ctx.Err(); err != nil {
		return model.Request{}, err
	}
	first := attestations[0]
	tools := append([]model.ToolIdentity(nil), first.Tools...)
	toolMap, validTools := rustPlannerTools(tools)
	if !validTools || first.SchemaVersion != model.SchemaVersion || first.Extractor != "rust-analyzer-scip" ||
		first.ExtractorVer != rustSemanticExtractorVersion(toolMap[rustAnalyzerToolName].Version) || first.Toolchain == "" ||
		first.Backend != identityBackend() || first.BackendEpoch != 0 {
		return model.Request{}, model.ErrInvalidProvenance
	}
	if err := verifyRustPlannerTools(ctx, tools); err != nil {
		return model.Request{}, err
	}

	expected := make(map[string]model.Provenance, len(attestations))
	var plannerInputs model.BuildInputs
	var plannerMarker string
	for index, provenance := range attestations {
		scope := provenance.Scope
		if provenance.SchemaVersion != model.SchemaVersion || !sameRustStableWorkspaceIdentity(provenance.Identity, currentIdentity) ||
			provenance.Extractor != first.Extractor || provenance.ExtractorVer != first.ExtractorVer || provenance.Toolchain != first.Toolchain ||
			provenance.Backend != first.Backend || provenance.BackendEpoch != first.BackendEpoch ||
			!sameRustToolSet(tools, provenance.Tools) || scope.ID == "" || scope.Language != langID ||
			scope.RootURI != canonicalRoot || scope.BuildContext == "" ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		marker, ok := scope.Build.Options[rustPlannerInputsOption]
		if !ok {
			return model.Request{}, model.ErrInvalidProvenance
		}
		build, decodeErr := decodeRustPlannerInputs(marker)
		if decodeErr != nil {
			return model.Request{}, fmt.Errorf("rust semantic index: invalid saved planner inputs: %w", decodeErr)
		}
		if index == 0 {
			plannerInputs, plannerMarker = build, marker
		} else if marker != plannerMarker || !reflect.DeepEqual(build, plannerInputs) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		if _, duplicate := expected[scope.ID]; duplicate {
			return model.Request{}, model.ErrInvalidProvenance
		}
		expected[scope.ID] = provenance
	}

	packages, err := discoverRustCargoPackages(ctx, view, canonicalRoot, plannerInputs, toolMap, runCargoMetadata)
	if err != nil {
		return model.Request{}, err
	}
	if view.Identity() != currentIdentity {
		return model.Request{}, model.ErrInvalidProvenance
	}
	request := model.Request{WorkspaceRootURI: canonicalRoot, View: view, Provenance: make(map[string]model.Provenance, len(expected))}
	for _, pkg := range packages {
		for _, target := range pkg.Targets {
			if target.isBuildScript() || (target.isTestTarget() && !plannerInputs.Tests) {
				continue
			}
			if len(request.Scopes) >= maxRustPlannerScopes {
				return model.Request{}, model.ErrInvalidProvenance
			}
			scope := makeRustTargetScope(canonicalRoot, pkg, target, plannerInputs, plannerMarker)
			pinnedTools := toolsForBuildScope(scope, toolMap)
			scope.BuildContext = model.ComputeBuildContextID(scope, first.Extractor, first.ExtractorVer, first.Toolchain, pinnedTools)
			prior, ok := expected[scope.ID]
			if !ok {
				return model.Request{}, model.ErrInvalidProvenance
			}
			priorScope := cloneRustScope(prior.Scope)
			priorScope.BuildContext = ""
			currentScope := cloneRustScope(scope)
			currentScope.BuildContext = ""
			if !reflect.DeepEqual(currentScope, priorScope) || !sameRustToolSet(pinnedTools, prior.Tools) {
				return model.Request{}, model.ErrInvalidProvenance
			}
			provenance := model.Provenance{
				SchemaVersion: model.SchemaVersion, Identity: currentIdentity, Scope: scope,
				Extractor: first.Extractor, ExtractorVer: first.ExtractorVer,
				Backend: first.Backend, BackendEpoch: first.BackendEpoch,
				Toolchain: first.Toolchain, Tools: pinnedTools,
			}
			request.Scopes = append(request.Scopes, scope)
			request.Provenance[scope.ID] = provenance
		}
	}
	if len(request.Scopes) == 0 || len(request.Scopes) != len(expected) {
		return model.Request{}, model.ErrInvalidProvenance
	}
	sort.Slice(request.Scopes, func(i, j int) bool { return request.Scopes[i].ID < request.Scopes[j].ID })
	for scopeID, prior := range expected {
		current, ok := request.Provenance[scopeID]
		if !ok {
			return model.Request{}, model.ErrInvalidProvenance
		}
		prior.Identity = currentIdentity
		prior.Tools = current.Tools
		if !reflect.DeepEqual(current, prior) {
			return model.Request{}, model.ErrInvalidProvenance
		}
	}
	if err := verifyRustPlannerTools(ctx, tools); err != nil {
		return model.Request{}, err
	}
	if view.Identity() != currentIdentity {
		return model.Request{}, model.ErrInvalidProvenance
	}
	return request, nil
}

func rustPlannerTools(tools []model.ToolIdentity) (map[string]model.ToolIdentity, bool) {
	if len(tools) < 3 || len(tools) > 5 {
		return nil, false
	}
	allowed := map[string]bool{
		rustAnalyzerToolName: true, cargoToolName: true, rustcToolName: true,
		procMacroToolName: true, rustSemanticHelperToolName: true,
	}
	resolved := make(map[string]model.ToolIdentity, len(tools))
	for _, tool := range tools {
		if !allowed[tool.Name] || tool.Version == "" || len(tool.Version) > 4096 || !utf8.ValidString(tool.Version) || !filepath.IsAbs(tool.Path) || len(tool.SHA256) != sha256.Size*2 {
			return nil, false
		}
		if _, err := hex.DecodeString(tool.SHA256); err != nil {
			return nil, false
		}
		if _, duplicate := resolved[tool.Name]; duplicate {
			return nil, false
		}
		resolved[tool.Name] = tool
	}
	for _, name := range []string{rustAnalyzerToolName, cargoToolName, rustcToolName} {
		if _, ok := resolved[name]; !ok {
			return nil, false
		}
	}
	return resolved, true
}

func verifyRustPlannerTools(ctx context.Context, tools []model.ToolIdentity) error {
	for _, tool := range tools {
		if err := verifyToolIdentity(ctx, tool); err != nil {
			return fmt.Errorf("rust semantic index: saved %s identity is unavailable: %w", tool.Name, err)
		}
	}
	return nil
}

func sameRustStableWorkspaceIdentity(a, b model.Identity) bool {
	return a.Workspace != "" && a.Workspace == b.Workspace && a.DiskDigest != "" && a.DiskDigest == b.DiskDigest &&
		a.Repository == b.Repository && a.Revision == b.Revision
}

func sameRustToolSet(expected, actual []model.ToolIdentity) bool {
	left := append([]model.ToolIdentity(nil), expected...)
	right := append([]model.ToolIdentity(nil), actual...)
	less := func(items []model.ToolIdentity) func(int, int) bool {
		return func(i, j int) bool {
			if items[i].Name != items[j].Name {
				return items[i].Name < items[j].Name
			}
			return items[i].Path < items[j].Path
		}
	}
	sort.Slice(left, less(left))
	sort.Slice(right, less(right))
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func cloneRustScope(scope model.Scope) model.Scope {
	scope.Build = cloneBuildInputs(scope.Build)
	return scope
}

func (b *SemanticIndexRequestBuilder) verifiedConfiguredTools(ctx context.Context) (map[string]model.ToolIdentity, error) {
	if strings.TrimSpace(b.config.Toolchain) == "" {
		return nil, errors.New("rust semantic index: explicit Rust toolchain identity is required")
	}
	configured := make(map[string]model.ToolIdentity, len(b.config.Tools))
	for _, tool := range b.config.Tools {
		if _, exists := configured[tool.Name]; exists {
			return nil, fmt.Errorf("rust semantic index: duplicate configured tool %q", tool.Name)
		}
		if tool.Name != rustAnalyzerToolName && tool.Name != cargoToolName && tool.Name != rustcToolName && tool.Name != procMacroToolName {
			return nil, fmt.Errorf("rust semantic index: unsupported configured tool %q", tool.Name)
		}
		if err := verifyToolIdentity(ctx, tool); err != nil {
			return nil, fmt.Errorf("rust semantic index: configured %s identity is unavailable: %w", tool.Name, err)
		}
		configured[tool.Name] = tool
	}
	if helper := b.config.SemanticHelper; helper != (model.ToolIdentity{}) {
		if helper.Name != rustSemanticHelperToolName {
			return nil, fmt.Errorf("rust semantic index: semantic helper identity must be named %q", rustSemanticHelperToolName)
		}
		if _, exists := configured[helper.Name]; exists {
			return nil, fmt.Errorf("rust semantic index: duplicate configured tool %q", helper.Name)
		}
		if err := verifyToolIdentity(ctx, helper); err != nil {
			return nil, fmt.Errorf("rust semantic index: configured %s identity is unavailable: %w", helper.Name, err)
		}
		configured[helper.Name] = helper
	}
	for _, required := range []string{cargoToolName, rustcToolName} {
		if _, ok := configured[required]; !ok {
			return nil, fmt.Errorf("rust semantic index: explicit pinned %s path is required", required)
		}
	}
	if b.backend == nil || b.activeRAPath == nil {
		return nil, errors.New("rust semantic index: no live rust-analyzer backend is attached")
	}
	activePath, err := filepath.Abs(b.activeRAPath())
	if err != nil {
		return nil, fmt.Errorf("rust semantic index: cannot resolve live rust-analyzer executable path: %w", err)
	}
	_, activeHash, err := nested.ExecutableIdentity(activePath)
	if err != nil {
		return nil, fmt.Errorf("rust semantic index: cannot identify live rust-analyzer executable: %w", err)
	}
	if ra, ok := configured[rustAnalyzerToolName]; ok {
		raPath, identityErr := filepath.Abs(ra.Path)
		_, raHash, hashErr := nested.ExecutableIdentity(ra.Path)
		if identityErr != nil || hashErr != nil || filepath.Clean(raPath) != filepath.Clean(activePath) || !strings.EqualFold(raHash, activeHash) {
			return nil, errors.New("rust semantic index: pinned rust-analyzer does not match the live backend executable")
		}
		ra.Path, ra.SHA256 = activePath, activeHash
		configured[rustAnalyzerToolName] = ra
	} else {
		version, versionErr := probePinnedToolVersion(ctx, activePath)
		if versionErr != nil {
			return nil, fmt.Errorf("rust semantic index: cannot identify live rust-analyzer version: %w", versionErr)
		}
		configured[rustAnalyzerToolName] = model.ToolIdentity{
			Name: rustAnalyzerToolName, Path: activePath, Version: version, SHA256: activeHash,
		}
	}
	return configured, nil
}

func probePinnedToolVersion(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, path, "--version")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(output))
	if version == "" || len(version) > 4096 {
		return "", errors.New("executable returned an invalid version string")
	}
	return version, nil
}

type cargoMetadata struct {
	Packages []cargoPackage `json:"packages"`
	Members  []string       `json:"workspace_members"`
}

type cargoPackage struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Version      string        `json:"version"`
	ManifestPath string        `json:"manifest_path"`
	Targets      []cargoTarget `json:"targets"`
}

type cargoTarget struct {
	Name      string   `json:"name"`
	SrcPath   string   `json:"src_path"`
	Kind      []string `json:"kind"`
	CrateType []string `json:"crate_types"`
}

func decodeWorkspacePackages(data []byte, rootPath string, patterns []string) ([]cargoPackage, error) {
	var metadata cargoMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("decode pinned Cargo metadata: %w", err)
	}
	if len(metadata.Members) == 0 {
		return nil, errors.New("pinned Cargo metadata did not identify any workspace members")
	}
	memberSet := make(map[string]struct{}, len(metadata.Members))
	for _, id := range metadata.Members {
		memberSet[id] = struct{}{}
	}
	packages := make([]cargoPackage, 0, len(memberSet))
	matchedPatterns := make(map[string]bool, len(patterns))
	for _, pkg := range metadata.Packages {
		if _, member := memberSet[pkg.ID]; !member {
			continue
		}
		if pkg.Name == "" || pkg.Version == "" || pkg.ManifestPath == "" {
			return nil, errors.New("pinned Cargo metadata contains an incomplete workspace package")
		}
		manifest, err := filepath.Abs(pkg.ManifestPath)
		if err != nil || !pathWithin(rootPath, manifest) {
			return nil, fmt.Errorf("workspace member %q is outside the immutable materialized root", pkg.Name)
		}
		relativeManifest, err := filepath.Rel(rootPath, manifest)
		if err != nil {
			return nil, err
		}
		pkg.ManifestPath = filepath.ToSlash(relativeManifest)
		for targetIndex := range pkg.Targets {
			targetPath, pathErr := filepath.Abs(pkg.Targets[targetIndex].SrcPath)
			if pathErr != nil || !pathWithin(rootPath, targetPath) {
				return nil, fmt.Errorf("Rust target %q for package %q is outside the immutable materialized root", pkg.Targets[targetIndex].Name, pkg.Name)
			}
			relativeTarget, relErr := filepath.Rel(rootPath, targetPath)
			if relErr != nil {
				return nil, relErr
			}
			pkg.Targets[targetIndex].SrcPath = filepath.ToSlash(relativeTarget)
		}
		if len(patterns) != 0 {
			matched := false
			for _, pattern := range patterns {
				if cargoPackageMatches(pkg, pattern) {
					matched, matchedPatterns[pattern] = true, true
				}
			}
			if !matched {
				continue
			}
		}
		packages = append(packages, pkg)
	}
	for _, pattern := range patterns {
		if !matchedPatterns[pattern] {
			return nil, fmt.Errorf("Cargo package pattern %q does not identify a workspace member", pattern)
		}
	}
	if len(packages) == 0 {
		return nil, errors.New("pinned Cargo metadata contained no selected workspace packages")
	}
	sort.Slice(packages, func(i, j int) bool {
		if packages[i].Name != packages[j].Name {
			return packages[i].Name < packages[j].Name
		}
		return packages[i].Version < packages[j].Version
	})
	return packages, nil
}

func cargoPackageMatches(pkg cargoPackage, pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == pkg.Name {
		return true
	}
	return pattern == pkg.Name+"@"+pkg.Version
}

func encodeRustPlannerInputs(build model.BuildInputs) (string, error) {
	if err := validateRustPlannerBuildInputs(build); err != nil {
		return "", err
	}
	marker := rustPlannerInputMarker{Version: rustPlannerInputsVersion, Build: build}
	data, err := json.Marshal(marker)
	if err != nil {
		return "", fmt.Errorf("encode Rust planner inputs: %w", err)
	}
	if len(data) > maxRustPlannerInputsBytes {
		return "", fmt.Errorf("Rust planner inputs exceed %d bytes", maxRustPlannerInputsBytes)
	}
	return string(data), nil
}

func decodeRustPlannerInputs(encoded string) (model.BuildInputs, error) {
	if encoded == "" || len(encoded) > maxRustPlannerInputsBytes {
		return model.BuildInputs{}, errors.New("Rust planner input marker is missing or exceeds its size limit")
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var marker rustPlannerInputMarker
	if err := decoder.Decode(&marker); err != nil {
		return model.BuildInputs{}, fmt.Errorf("decode Rust planner input marker: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return model.BuildInputs{}, errors.New("Rust planner input marker contains trailing JSON")
		}
		return model.BuildInputs{}, fmt.Errorf("decode trailing Rust planner input marker data: %w", err)
	}
	if marker.Version != rustPlannerInputsVersion {
		return model.BuildInputs{}, fmt.Errorf("unsupported Rust planner input marker version %d", marker.Version)
	}
	if err := validateRustPlannerBuildInputs(marker.Build); err != nil {
		return model.BuildInputs{}, err
	}
	canonical, err := json.Marshal(marker)
	if err != nil || string(canonical) != encoded {
		return model.BuildInputs{}, errors.New("Rust planner input marker is not canonical JSON")
	}
	return marker.Build, nil
}

func validateRustPlannerBuildInputs(build model.BuildInputs) error {
	if _, reserved := build.Options[rustPlannerInputsOption]; reserved {
		return fmt.Errorf("Rust build option %q is reserved for planner provenance", rustPlannerInputsOption)
	}
	if len(build.Environment) > maxRustPlannerScopes || len(build.Arguments) > maxRustPlannerScopes ||
		len(build.PackagePatterns) > maxRustPlannerScopes || len(build.IncludePaths) > maxRustPlannerScopes ||
		len(build.Defines) > maxRustPlannerScopes || len(build.Features) > maxRustPlannerScopes || len(build.Options) > maxRustPlannerScopes {
		return fmt.Errorf("Rust planner inputs exceed the %d item resource limit", maxRustPlannerScopes)
	}
	validString := func(value string) bool { return utf8.ValidString(value) && len(value) <= maxRustPlannerInputsBytes }
	checkSlice := func(name string, values []string) error {
		for _, value := range values {
			if !validString(value) {
				return fmt.Errorf("Rust planner input %s contains an invalid or oversized string", name)
			}
		}
		return nil
	}
	for name, values := range map[string]map[string]string{
		"environment": build.Environment, "defines": build.Defines, "options": build.Options,
	} {
		for key, value := range values {
			if key == "" || !validString(key) || !validString(value) {
				return fmt.Errorf("Rust planner input %s contains an invalid or oversized key/value", name)
			}
		}
	}
	for name, values := range map[string][]string{
		"arguments": build.Arguments, "package patterns": build.PackagePatterns,
		"include paths": build.IncludePaths, "features": build.Features,
	} {
		if err := checkSlice(name, values); err != nil {
			return err
		}
	}
	return nil
}

func makeRustTargetScope(rootURI string, pkg cargoPackage, target cargoTarget, base model.BuildInputs, plannerInputs string) model.Scope {
	build := cloneBuildInputs(base)
	build.PackagePatterns = []string{pkg.Name + "@" + pkg.Version}
	build.Options[rustPlannerInputsOption] = plannerInputs
	build.Options["cargo.targetKind"] = strings.Join(target.Kind, ",")
	build.Options["cargo.targetName"] = target.Name
	build.Options["cargo.targetSrcPath"] = target.SrcPath
	build.Options["cargo.allTargets"] = "false"
	selector := cargoTargetSelector(target)
	if len(selector) != 0 {
		build.Arguments = append(build.Arguments, selector...)
	}
	material := strings.Join([]string{pkg.ManifestPath, pkg.Name, pkg.Version, strings.Join(target.Kind, ","), target.Name, target.SrcPath}, "\x00")
	sum := sha256.Sum256([]byte(filepath.ToSlash(material)))
	return model.Scope{
		ID:       "rust:" + pkg.Name + "@" + pkg.Version + ":" + hex.EncodeToString(sum[:8]),
		Language: langID, RootURI: rootURI, Build: build,
	}
}

func cargoTargetSelector(target cargoTarget) []string {
	if containsString(target.Kind, "custom-build") {
		return nil
	}
	switch {
	case containsString(target.Kind, "bin"):
		return []string{"--bin", target.Name}
	case containsString(target.Kind, "example"):
		return []string{"--example", target.Name}
	case containsString(target.Kind, "test"):
		return []string{"--test", target.Name}
	case containsString(target.Kind, "bench"):
		return []string{"--bench", target.Name}
	case containsString(target.Kind, "lib") || containsString(target.CrateType, "proc-macro"):
		return []string{"--lib"}
	default:
		return nil
	}
}

func (target cargoTarget) isBuildScript() bool { return containsString(target.Kind, "custom-build") }
func (target cargoTarget) isTestTarget() bool {
	return containsString(target.Kind, "test") || containsString(target.Kind, "bench")
}

func toolsForBuildScope(scope model.Scope, configured map[string]model.ToolIdentity) []model.ToolIdentity {
	procMacros, _, _ := semanticOptions(scope)
	names := []string{rustAnalyzerToolName, cargoToolName, rustcToolName}
	if procMacros {
		names = append(names, procMacroToolName)
	}
	if _, ok := configured[rustSemanticHelperToolName]; ok {
		names = append(names, rustSemanticHelperToolName)
	}
	tools := make([]model.ToolIdentity, 0, len(names))
	for _, name := range names {
		if tool, ok := configured[name]; ok {
			tools = append(tools, tool)
		}
	}
	return tools
}

func cloneRustIndexToolConfig(config RustIndexToolConfig) RustIndexToolConfig {
	config.Tools = append([]model.ToolIdentity(nil), config.Tools...)
	config.Build = cloneBuildInputs(config.Build)
	return config
}

func cloneBuildInputs(input model.BuildInputs) model.BuildInputs {
	return model.BuildInputs{
		Environment: cloneStringMap(input.Environment), Arguments: append([]string(nil), input.Arguments...),
		PackagePatterns: append([]string(nil), input.PackagePatterns...), IncludePaths: append([]string(nil), input.IncludePaths...),
		Defines: cloneStringMap(input.Defines), Features: append([]string(nil), input.Features...),
		Options: cloneStringMap(input.Options), Tests: input.Tests,
	}
}

func identityBackend() identity.BackendID {
	return identity.BackendID{Language: langID, Name: rustAnalyzerToolName}
}

func runCargoMetadata(ctx context.Context, invocation scipInvocation) ([]byte, error) {
	cmd := exec.CommandContext(ctx, invocation.Tool.Path, invocation.Args...)
	cmd.Dir = invocation.Dir
	cmd.Env = invocation.Env
	stdout := rustMetadataBuffer{limit: maxRustMetadataBytes}
	stderr := rustMetadataBuffer{limit: 256 * 1024}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		message := strings.TrimSpace(string(stderr.data))
		if message != "" {
			return nil, fmt.Errorf("%w: %s", err, message)
		}
		return nil, err
	}
	return append([]byte(nil), stdout.data...), nil
}

type rustMetadataBuffer struct {
	data  []byte
	limit int
}

func (b *rustMetadataBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - len(b.data)
	if len(data) > remaining {
		if remaining > 0 {
			b.data = append(b.data, data[:remaining]...)
		}
		return remaining, fmt.Errorf("Cargo metadata output exceeds %d bytes", b.limit)
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

// ExportIndex exports a Rust workspace snapshot through the configured,
// content-pinned rust-analyzer SCIP command. Each scope is materialized into a
// disposable directory and receives its own Cargo target directory, so the
// child process never observes mutable workspace files or writes build output
// into the user's checkout.
func (b *Backend) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	return exportRustIndex(ctx, request, sink, runRustAnalyzerSCIP)
}

func exportRustIndex(ctx context.Context, request model.Request, sink model.Sink, run scipRunner) (model.Report, error) {
	if request.View == nil {
		return model.Report{}, model.ErrMissingView
	}
	if sink == nil {
		return model.Report{}, errors.New("rust semantic index: missing fact sink")
	}
	if run == nil {
		return model.Report{}, errors.New("rust semantic index: missing SCIP runner")
	}

	identity := request.View.Identity()
	report := model.Report{Identity: identity, UsedTools: make(map[string][]model.ToolIdentity)}
	if err := validateRustRequest(request); err != nil {
		return report, err
	}
	materializer, canMaterialize := request.View.(model.WorkspaceMaterializer)
	for scopeIndex, scope := range request.Scopes {
		if err := ctx.Err(); err != nil {
			appendUnavailableScopes(&report, request.Scopes[scopeIndex:], "export cancelled before scope extraction: "+err.Error())
			return report, err
		}
		if scope.Language != langID {
			appendCoverage(&report, unavailableCoverage(scope.ID, "rust-analyzer exporter received a non-Rust scope")...)
			continue
		}
		provenance, ok := request.Provenance[scope.ID]
		if !ok || provenance.Scope.ID != scope.ID || provenance.Identity != identity {
			appendCoverage(&report, unavailableCoverage(scope.ID, "scope provenance is missing or belongs to another workspace snapshot")...)
			continue
		}
		if !canMaterialize {
			appendCoverage(&report, unavailableCoverage(scope.ID, "workspace view cannot materialize an immutable captured snapshot")...)
			continue
		}

		tools, reason := toolsForScope(ctx, provenance, scope, run)
		if reason != "" {
			appendCoverage(&report, unavailableCoverage(scope.ID, reason)...)
			if err := ctx.Err(); err != nil {
				appendUnavailableScopes(&report, request.Scopes[scopeIndex+1:], "export cancelled before scope extraction: "+err.Error())
				return report, err
			}
			continue
		}
		if _, _, configErr := semanticOptions(scope); configErr != nil {
			appendCoverage(&report, unavailableCoverage(scope.ID, configErr.Error())...)
			continue
		}

		scopeRequest := singleScopeRequest(request, scope, provenance)
		tempRoot, err := os.MkdirTemp("", "omnilsp-rust-index-")
		if err != nil {
			appendCoverage(&report, unavailableCoverage(scope.ID, "create isolated extraction directory: "+err.Error())...)
			continue
		}
		err = exportRustScope(ctx, scopeRequest, materializer, tempRoot, tools, run, sink, &report)
		removeErr := os.RemoveAll(tempRoot)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				appendUnavailableScopes(&report, request.Scopes[scopeIndex:], "export cancelled: "+err.Error())
				return report, err
			}
			appendCoverage(&report, unavailableCoverage(scope.ID, err.Error())...)
			continue
		}
		if removeErr != nil {
			// Facts already flowed to the sink from a successful immutable view;
			// cleanup failure affects neither their source identity nor coverage.
			// Surface the issue as a report error so the caller can clean up.
			report.Errors = append(report.Errors, "remove isolated Rust extraction directory: "+removeErr.Error())
		}
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := model.ValidateReport(request, report); err != nil {
		return report, err
	}
	return report, nil
}

func validateRustRequest(request model.Request) error {
	seen := make(map[string]struct{}, len(request.Scopes))
	for _, scope := range request.Scopes {
		if scope.ID == "" || scope.Language != langID || scope.RootURI == "" || scope.BuildContext == "" {
			return model.ErrInvalidScope
		}
		canonicalRoot, err := canonicalRustWorkspaceRoot(scope.RootURI)
		if err != nil || canonicalRoot != scope.RootURI {
			return model.ErrInvalidScope
		}
		if _, exists := seen[scope.ID]; exists {
			return model.ErrInvalidScope
		}
		seen[scope.ID] = struct{}{}
		provenance, ok := request.Provenance[scope.ID]
		if !ok || provenance.SchemaVersion != model.SchemaVersion || provenance.Identity != request.View.Identity() ||
			!reflect.DeepEqual(provenance.Scope, scope) || provenance.Extractor == "" || provenance.ExtractorVer == "" ||
			provenance.Toolchain == "" || provenance.Backend.Language != langID || provenance.Backend.Name != rustAnalyzerToolName ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return model.ErrInvalidProvenance
		}
		if provenance.Extractor == "rust-analyzer-scip" {
			for _, tool := range provenance.Tools {
				if tool.Name == rustAnalyzerToolName && provenance.ExtractorVer != rustSemanticExtractorVersion(tool.Version) {
					return model.ErrInvalidProvenance
				}
			}
		}
	}
	return nil
}

func rustSemanticExtractorVersion(rustAnalyzerVersion string) string {
	return rustAnalyzerVersion + ";omnilsp-semantic-model-v" + rustSemanticModelVersion
}

func canonicalRustWorkspaceRoot(rootURI string) (string, error) {
	parsedURL, err := url.Parse(rootURI)
	if err != nil || parsedURL.Scheme != "file" || parsedURL.Opaque != "" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
		return "", fmt.Errorf("expected a local file URI, got %q", rootURI)
	}
	parsed, err := workspaceuri.Parse(rootURI)
	if err != nil || !parsed.IsFile() {
		return "", fmt.Errorf("expected a local file URI, got %q", rootURI)
	}
	rootPath, err := parsed.Path()
	if err != nil {
		return "", err
	}
	rootPath = filepath.Clean(rootPath)
	if !filepath.IsAbs(rootPath) {
		return "", fmt.Errorf("workspace root path is not absolute: %q", rootPath)
	}
	return workspaceuri.FromPath(rootPath).Canonical(), nil
}

func appendUnavailableScopes(report *model.Report, scopes []model.Scope, reason string) {
	for _, scope := range scopes {
		appendCoverage(report, unavailableCoverage(scope.ID, reason)...)
	}
}

func singleScopeRequest(request model.Request, scope model.Scope, provenance model.Provenance) model.Request {
	return model.Request{
		View:       request.View,
		Scopes:     []model.Scope{scope},
		Provenance: map[string]model.Provenance{scope.ID: provenance},
	}
}

func exportRustScope(
	ctx context.Context,
	request model.Request,
	materializer model.WorkspaceMaterializer,
	tempRoot string,
	tools map[string]model.ToolIdentity,
	run scipRunner,
	sink model.Sink,
	combined *model.Report,
) (retErr error) {
	scope := request.Scopes[0]
	workspacePath := filepath.Join(tempRoot, "workspace")
	materialized, err := materializer.Materialize(ctx, scope.RootURI, workspacePath)
	if err != nil {
		return fmt.Errorf("materialize captured workspace: %w", err)
	}
	if materialized == nil || materialized.RootURI() != scope.RootURI || materialized.RootPath() == "" {
		return errors.New("materializer returned an invalid workspace root mapping")
	}
	defer func() {
		if closeErr := materialized.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close materialized Rust workspace: %w", closeErr))
		}
	}()
	rootPath, err := filepath.Abs(materialized.RootPath())
	if err != nil {
		return fmt.Errorf("resolve materialized workspace root: %w", err)
	}
	rootRealPath, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return fmt.Errorf("resolve materialized workspace root: %w", err)
	}
	if err := verifyTemporaryIsolation(tempRoot, rootRealPath); err != nil {
		return err
	}
	if err := verifyCapturedWorkspace(ctx, request.View, materialized, scope.RootURI); err != nil {
		return err
	}
	buildTargetPath, err := os.MkdirTemp(tempRoot, "target-")
	if err != nil {
		return fmt.Errorf("create isolated Cargo target directory: %w", err)
	}
	relativeTarget, err := filepath.Rel(rootPath, buildTargetPath)
	if err != nil || filepath.IsAbs(relativeTarget) {
		return errors.New("isolated Cargo target directory cannot be mapped from the materialized workspace")
	}
	config, err := rustAnalyzerConfig(scope, tools, filepath.ToSlash(relativeTarget))
	if err != nil {
		return err
	}
	configPath := filepath.Join(tempRoot, "rust-analyzer.json")
	configBytes, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("encode rust-analyzer Cargo configuration: %w", err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		return fmt.Errorf("write isolated rust-analyzer configuration: %w", err)
	}
	outputPath := filepath.Join(tempRoot, "index.scip")
	tool := tools[rustAnalyzerToolName]
	args := []string{"scip", rootPath, "--output", outputPath, "--config-path", configPath, "--num-threads", "1"}
	if helper, ok := tools[rustSemanticHelperToolName]; ok {
		tool = helper
		targetRoot := optionString(scope, "cargo.targetSrcPath")
		if targetRoot == "" {
			return errors.New("Rust completeness witness requires the Cargo-selected target source path")
		}
		if _, err := rustSCIPDocumentURI(scope.RootURI, targetRoot); err != nil {
			return fmt.Errorf("Rust completeness witness target source path is invalid: %w", err)
		}
		args = append(args, "--coverage-root", filepath.FromSlash(targetRoot))
	}
	env := rustCommandEnv(request.Scopes[0], tools, buildTargetPath, tempRoot)
	invocation := scipInvocation{
		Tool: tool,
		Args: args,
		Dir:  rootPath, Env: env, OutputPath: outputPath,
	}
	data, err := run(ctx, invocation)
	if err != nil {
		return fmt.Errorf("rust-analyzer SCIP export failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := verifyCapturedWorkspace(ctx, request.View, materialized, scope.RootURI); err != nil {
		return fmt.Errorf("materialized snapshot changed during SCIP export: %w", err)
	}
	rawSCIPSHA256 := sha256.Sum256(data)
	data, err = bindRustSCIPProjectRoot(data, rootPath, scope.RootURI, request.View.Identity().Repository)
	if err != nil {
		return fmt.Errorf("bind rust-analyzer SCIP project root to captured workspace: %w", err)
	}
	var relationEdges []rustPendingEdge
	var completeness rustCompletenessValidation
	if _, helperConfigured := tools[rustSemanticHelperToolName]; helperConfigured {
		files, fileErr := rustScopeFiles(ctx, request.View, scope.RootURI)
		if fileErr != nil {
			return fmt.Errorf("walk captured Rust sources for semantic relations: %w", fileErr)
		}
		sidecarData, sidecarErr := readRustRelationSidecar(outputPath + ".relations.json")
		if sidecarErr != nil {
			return fmt.Errorf("read patched Rust semantic helper sidecar: %w", sidecarErr)
		}
		relationEdges, err = decodeRustHelperRelations(ctx, data, sidecarData, scope, files, request.View)
		if err != nil {
			return fmt.Errorf("validate Rust semantic helper relations: %w", err)
		}
		completeness, err = validateRustCompletenessWitness(ctx, data, sidecarData, rawSCIPSHA256, scope, files, request.View)
		if err != nil {
			return fmt.Errorf("validate Rust completeness witness: %w", err)
		}
	}

	combined.UsedTools[scope.ID] = usedSemanticTools(scope, tools)
	imported, err := interop.ImportSCIPToModel(ctx, data, request, sink)
	if err != nil {
		return fmt.Errorf("import rust-analyzer SCIP facts: %w", err)
	}
	if len(relationEdges) > 0 {
		edges := make([]model.Edge, 0, len(relationEdges))
		for _, relation := range relationEdges {
			edges = append(edges, model.Edge{
				From: relation.from, To: relation.to, ScopeID: scope.ID, Kind: relation.kind,
				SourceURI: relation.sourceURI, Range: relation.rangeValue, SourceHash: relation.sourceHash,
				BuildContext: scope.BuildContext,
			})
			if len(edges) == cap(edges) {
				if err := sink.WriteEdges(ctx, edges); err != nil {
					return fmt.Errorf("write Rust semantic helper edges: %w", err)
				}
				edges = make([]model.Edge, 0, 2048)
			}
		}
		if len(edges) > 0 {
			if err := sink.WriteEdges(ctx, edges); err != nil {
				return fmt.Errorf("write Rust semantic helper edges: %w", err)
			}
		}
	}
	constrainRustSCIPRelationCoverage(&imported, scope.ID)
	applyRustCompletenessWitness(&imported, scope.ID, completeness)
	combined.Coverage = append(combined.Coverage, imported.Coverage...)
	combined.UsedTools[scope.ID] = append([]model.ToolIdentity(nil), imported.UsedTools[scope.ID]...)
	return nil
}

func rustScopeFiles(ctx context.Context, view model.WorkspaceView, rootURI string) (map[string]model.File, error) {
	files := make(map[string]model.File)
	err := view.Walk(ctx, rootURI, func(file model.File) error {
		if file.URI == "" || file.Size < 0 || !validRustContentHash(string(file.SHA256)) {
			return errors.New("captured Rust file is missing a valid URI, size, or content hash")
		}
		canonical, parseErr := workspaceuri.Parse(file.URI)
		if parseErr != nil || canonical.Canonical() != file.URI || !rustURIWithinRoot(rootURI, file.URI) {
			return fmt.Errorf("captured Rust file URI is not canonical or is outside its scope: %q", file.URI)
		}
		if _, exists := files[file.URI]; exists {
			return fmt.Errorf("captured Rust scope contains duplicate file URI %s", file.URI)
		}
		files[file.URI] = file
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func rustURIWithinRoot(rootURI, fileURI string) bool {
	root, rootErr := workspaceuri.Parse(rootURI)
	file, fileErr := workspaceuri.Parse(fileURI)
	if rootErr != nil || fileErr != nil || !root.IsFile() || !file.IsFile() {
		return false
	}
	rootPath := strings.TrimSuffix(root.Canonical(), "/")
	filePath := file.Canonical()
	if filePath == rootPath || strings.HasPrefix(filePath, rootPath+"/") {
		return true
	}
	if filepath.Separator == '\\' {
		return strings.EqualFold(filePath, rootPath) || strings.HasPrefix(strings.ToLower(filePath), strings.ToLower(rootPath+"/"))
	}
	return false
}

func validRustContentHash(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil
}

func readRustRelationSidecar(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxRustRelationBytes {
		return nil, fmt.Errorf("semantic relation sidecar is not a regular file within the %d-byte limit", maxRustRelationBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxRustRelationBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maxRustRelationBytes {
		return nil, fmt.Errorf("semantic relation sidecar exceeds the %d-byte limit", maxRustRelationBytes)
	}
	return data, nil
}

func decodeRustRelationSidecar(data []byte) (rustRelationSidecar, error) {
	if len(data) == 0 || len(data) > maxRustRelationBytes {
		return rustRelationSidecar{}, errors.New("semantic relation sidecar is empty or exceeds its byte limit")
	}
	if err := rejectDuplicateRustJSONKeys(data); err != nil {
		return rustRelationSidecar{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return rustRelationSidecar{}, errors.New("semantic relation sidecar must be a JSON object")
	}
	var sidecar rustRelationSidecar
	var schemaSeen, relationsSeen, scipHashSeen, crateRootSeen bool
	var sourceManifestSeen, witnessCountsSeen, blockersSeen bool
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return rustRelationSidecar{}, fmt.Errorf("decode semantic relation sidecar key: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return rustRelationSidecar{}, errors.New("semantic relation sidecar contains a non-string key")
		}
		switch key {
		case "schema_version":
			if schemaSeen {
				return rustRelationSidecar{}, errors.New("semantic relation sidecar repeats schema_version")
			}
			schemaSeen = true
			if err := decoder.Decode(&sidecar.SchemaVersion); err != nil {
				return rustRelationSidecar{}, fmt.Errorf("decode semantic relation schema: %w", err)
			}
		case "scip_sha256":
			if scipHashSeen {
				return rustRelationSidecar{}, errors.New("semantic relation sidecar repeats scip_sha256")
			}
			scipHashSeen = true
			if err := decoder.Decode(&sidecar.SCIPSHA256); err != nil {
				return rustRelationSidecar{}, fmt.Errorf("decode semantic relation SCIP digest: %w", err)
			}
		case "selected_crate_root":
			if crateRootSeen {
				return rustRelationSidecar{}, errors.New("semantic relation sidecar repeats selected_crate_root")
			}
			crateRootSeen = true
			if err := decoder.Decode(&sidecar.SelectedCrateRoot); err != nil {
				return rustRelationSidecar{}, fmt.Errorf("decode selected Rust crate root: %w", err)
			}
		case "source_manifest":
			if sourceManifestSeen {
				return rustRelationSidecar{}, errors.New("semantic relation sidecar repeats source_manifest")
			}
			sourceManifestSeen = true
			sidecar.SourceManifest = make([]rustWitnessSource, 0)
			array, err := decoder.Token()
			if err != nil || array != json.Delim('[') {
				return rustRelationSidecar{}, errors.New("semantic relation source_manifest must be an array")
			}
			for decoder.More() {
				if len(sidecar.SourceManifest) >= maxRustRelations {
					return rustRelationSidecar{}, errors.New("semantic relation sidecar exceeds its source-file limit")
				}
				var source rustWitnessSource
				if err := decoder.Decode(&source); err != nil {
					return rustRelationSidecar{}, fmt.Errorf("decode Rust witness source: %w", err)
				}
				if len(source.Document) > 4096 || len(source.SHA256) > 64 {
					return rustRelationSidecar{}, errors.New("Rust witness source exceeds a field-size limit")
				}
				sidecar.SourceManifest = append(sidecar.SourceManifest, source)
			}
			closeArray, err := decoder.Token()
			if err != nil || closeArray != json.Delim(']') {
				return rustRelationSidecar{}, errors.New("semantic relation source_manifest array is incomplete")
			}
		case "witness_counts":
			if witnessCountsSeen {
				return rustRelationSidecar{}, errors.New("semantic relation sidecar repeats witness_counts")
			}
			witnessCountsSeen = true
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				return rustRelationSidecar{}, fmt.Errorf("decode Rust witness counts: %w", err)
			}
			counts, err := decodeRustWitnessCounts(raw)
			if err != nil {
				return rustRelationSidecar{}, err
			}
			sidecar.WitnessCounts = counts
		case "blockers":
			if blockersSeen {
				return rustRelationSidecar{}, errors.New("semantic relation sidecar repeats blockers")
			}
			blockersSeen = true
			sidecar.Blockers = make([]string, 0)
			array, err := decoder.Token()
			if err != nil || array != json.Delim('[') {
				return rustRelationSidecar{}, errors.New("semantic relation blockers must be an array")
			}
			for decoder.More() {
				if len(sidecar.Blockers) >= 4096 {
					return rustRelationSidecar{}, errors.New("semantic relation sidecar exceeds its blocker-count limit")
				}
				var blocker string
				if err := decoder.Decode(&blocker); err != nil {
					return rustRelationSidecar{}, fmt.Errorf("decode Rust completeness blocker: %w", err)
				}
				if len(blocker) > 1024 {
					return rustRelationSidecar{}, errors.New("Rust completeness blocker exceeds its field-size limit")
				}
				sidecar.Blockers = append(sidecar.Blockers, blocker)
			}
			closeArray, err := decoder.Token()
			if err != nil || closeArray != json.Delim(']') {
				return rustRelationSidecar{}, errors.New("semantic relation blockers array is incomplete")
			}
		case "relations":
			if relationsSeen {
				return rustRelationSidecar{}, errors.New("semantic relation sidecar repeats relations")
			}
			relationsSeen = true
			sidecar.Relations = make([]rustSidecarRelation, 0)
			array, err := decoder.Token()
			if err != nil || array != json.Delim('[') {
				return rustRelationSidecar{}, errors.New("semantic relation sidecar relations must be an array")
			}
			for decoder.More() {
				if len(sidecar.Relations) >= maxRustRelations {
					return rustRelationSidecar{}, errors.New("semantic relation sidecar exceeds its relation-count limit")
				}
				var relation rustSidecarRelation
				if err := decoder.Decode(&relation); err != nil {
					return rustRelationSidecar{}, fmt.Errorf("decode semantic relation record: %w", err)
				}
				if len(relation.Kind) > 16 || len(relation.SourceDocument) > 4096 ||
					len(relation.SourceSymbol) > 4096 || len(relation.TargetSymbol) > 4096 {
					return rustRelationSidecar{}, errors.New("semantic relation record exceeds a field-size limit")
				}
				sidecar.Relations = append(sidecar.Relations, relation)
			}
			closeArray, err := decoder.Token()
			if err != nil || closeArray != json.Delim(']') {
				return rustRelationSidecar{}, errors.New("semantic relation sidecar relations array is incomplete")
			}
		default:
			return rustRelationSidecar{}, fmt.Errorf("semantic relation sidecar contains unknown field %q", key)
		}
	}
	closeObject, err := decoder.Token()
	if err != nil || closeObject != json.Delim('}') {
		return rustRelationSidecar{}, errors.New("semantic relation sidecar object is incomplete")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return rustRelationSidecar{}, errors.New("semantic relation sidecar has trailing JSON data")
		}
		return rustRelationSidecar{}, fmt.Errorf("decode semantic relation sidecar trailer: %w", err)
	}
	if !schemaSeen || !relationsSeen || sidecar.Relations == nil {
		return rustRelationSidecar{}, errors.New("semantic relation sidecar has an unsupported schema or relation count")
	}
	if sidecar.SchemaVersion == 1 && (scipHashSeen || crateRootSeen || sourceManifestSeen || witnessCountsSeen || blockersSeen) {
		return rustRelationSidecar{}, errors.New("legacy semantic relation sidecar contains completeness fields")
	}
	if sidecar.SchemaVersion == 2 && (!scipHashSeen || !crateRootSeen || !sourceManifestSeen ||
		!witnessCountsSeen || !blockersSeen || sidecar.SourceManifest == nil ||
		sidecar.WitnessCounts == nil || sidecar.Blockers == nil) {
		return rustRelationSidecar{}, errors.New("Rust completeness sidecar is missing required witness fields")
	}
	if sidecar.SchemaVersion != 1 && sidecar.SchemaVersion != 2 {
		return rustRelationSidecar{}, errors.New("semantic relation sidecar has an unsupported schema")
	}
	return sidecar, nil
}

func decodeRustWitnessCounts(data []byte) (*rustWitnessCounts, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errors.New("Rust witness counts must be an object")
	}
	want := []string{
		"source_files", "unmapped_source_files", "scip_documents", "scip_symbols", "scip_occurrences",
		"candidate_tokens", "classified_candidate_tokens", "unknown_candidate_tokens", "macro_sites",
		"error_diagnostics", "external_targets", "duplicate_symbols",
	}
	if len(fields) != len(want) {
		return nil, errors.New("Rust witness counts contain missing or extra fields")
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("Rust witness counts omit %q", key)
		}
	}
	var counts rustWitnessCounts
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&counts); err != nil {
		return nil, fmt.Errorf("decode Rust witness counts object: %w", err)
	}
	return &counts, nil
}

func rejectDuplicateRustJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var parseValue func() error
	parseValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("semantic relation JSON object contains a non-string key")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("semantic relation JSON object repeats key %q", key)
				}
				seen[key] = struct{}{}
				if err := parseValue(); err != nil {
					return err
				}
			}
			closeToken, err := decoder.Token()
			if err != nil || closeToken != json.Delim('}') {
				return errors.New("semantic relation JSON object is incomplete")
			}
		case '[':
			for decoder.More() {
				if err := parseValue(); err != nil {
					return err
				}
			}
			closeToken, err := decoder.Token()
			if err != nil || closeToken != json.Delim(']') {
				return errors.New("semantic relation JSON array is incomplete")
			}
		default:
			return errors.New("semantic relation JSON contains an unexpected delimiter")
		}
		return nil
	}
	if err := parseValue(); err != nil {
		return fmt.Errorf("validate semantic relation JSON keys: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("semantic relation JSON has trailing data")
		}
		return fmt.Errorf("validate semantic relation JSON trailer: %w", err)
	}
	return nil
}

func decodeRustHelperRelations(
	ctx context.Context,
	indexData, sidecarData []byte,
	scope model.Scope,
	files map[string]model.File,
	view model.WorkspaceView,
) ([]rustPendingEdge, error) {
	var index scip.Index
	if err := proto.Unmarshal(indexData, &index); err != nil {
		return nil, fmt.Errorf("decode SCIP for semantic relation validation: %w", err)
	}
	if index.GetMetadata() == nil || index.GetMetadata().GetTextDocumentEncoding() == scip.TextEncoding_UnspecifiedTextEncoding {
		return nil, errors.New("SCIP semantic relation source has no text encoding")
	}
	sidecar, err := decodeRustRelationSidecar(sidecarData)
	if err != nil {
		return nil, err
	}
	knownSymbols := make(map[string]struct{})
	for _, symbol := range index.ExternalSymbols {
		if symbol != nil && symbol.GetSymbol() != "" {
			knownSymbols[symbol.GetSymbol()] = struct{}{}
		}
	}
	documents := make(map[string]struct{}, len(index.Documents))
	for _, document := range index.Documents {
		if document == nil || document.GetRelativePath() == "" {
			return nil, errors.New("SCIP semantic relation source has a document without a relative path")
		}
		if _, err := rustSCIPDocumentURI(scope.RootURI, document.GetRelativePath()); err != nil {
			return nil, err
		}
		if _, exists := documents[document.GetRelativePath()]; exists {
			return nil, fmt.Errorf("SCIP semantic relation source contains duplicate document %q", document.GetRelativePath())
		}
		documents[document.GetRelativePath()] = struct{}{}
		for _, symbol := range document.Symbols {
			if symbol != nil && symbol.GetSymbol() != "" {
				knownSymbols[symbol.GetSymbol()] = struct{}{}
			}
		}
		for _, occurrence := range document.Occurrences {
			if occurrence != nil && occurrence.GetSymbol() != "" {
				knownSymbols[occurrence.GetSymbol()] = struct{}{}
			}
		}
	}

	if view == nil {
		return nil, model.ErrMissingView
	}
	textCache := rustCapturedTextCache{ctx: ctx, view: view, files: files, contents: make(map[string]*rustCapturedText)}
	relations := make([]rustPendingEdge, 0, len(sidecar.Relations))
	seen := make(map[string]struct{}, len(sidecar.Relations))
	for _, relation := range sidecar.Relations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		kind := model.EdgeKind(relation.Kind)
		if kind != model.EdgeCall && kind != model.EdgeModule {
			return nil, fmt.Errorf("semantic relation sidecar contains unsupported relation kind %q", relation.Kind)
		}
		if !validRustSCIPSymbol(relation.SourceSymbol) || !validRustSCIPSymbol(relation.TargetSymbol) {
			return nil, errors.New("semantic relation sidecar contains an invalid raw SCIP symbol id")
		}
		if _, ok := knownSymbols[relation.SourceSymbol]; !ok {
			return nil, fmt.Errorf("semantic relation source symbol is absent from its SCIP scope: %q", relation.SourceSymbol)
		}
		if _, ok := knownSymbols[relation.TargetSymbol]; !ok {
			return nil, fmt.Errorf("semantic relation target symbol is absent from its SCIP scope: %q", relation.TargetSymbol)
		}
		uri, pathErr := rustSCIPDocumentURI(scope.RootURI, relation.SourceDocument)
		if pathErr != nil {
			return nil, pathErr
		}
		if _, ok := documents[relation.SourceDocument]; !ok {
			return nil, fmt.Errorf("semantic relation source document is absent from SCIP: %q", relation.SourceDocument)
		}
		file, ok := files[uri]
		if !ok {
			return nil, fmt.Errorf("semantic relation source document is absent from the captured scope: %q", relation.SourceDocument)
		}
		rawRange, rangeErr := rustRangeCoordinates(relation.Range)
		if rangeErr != nil {
			return nil, rangeErr
		}
		sourceURI, sourceHash, sourceRange, sourceErr := resolveRustRelationSource(&textCache, scope.RootURI, file, rawRange)
		if sourceErr != nil {
			return nil, fmt.Errorf("validate semantic relation source %q: %w", relation.SourceDocument, sourceErr)
		}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%d:%d:%d:%d", kind, relation.SourceDocument, relation.SourceSymbol+"\x00"+relation.TargetSymbol,
			rawRange.startLine, rawRange.startChar, rawRange.endLine, rawRange.endChar)
		if _, duplicate := seen[key]; duplicate {
			return nil, errors.New("semantic relation sidecar contains a duplicate relation record")
		}
		seen[key] = struct{}{}
		relations = append(relations, rustPendingEdge{
			kind: kind, from: identity.SymbolID(relation.SourceSymbol), to: identity.SymbolID(relation.TargetSymbol),
			sourceURI: sourceURI, sourceHash: sourceHash, rangeValue: sourceRange,
		})
	}
	sort.Slice(relations, func(i, j int) bool {
		left, right := relations[i], relations[j]
		if left.sourceURI != right.sourceURI {
			return left.sourceURI < right.sourceURI
		}
		if left.rangeValue.StartLine != right.rangeValue.StartLine {
			return left.rangeValue.StartLine < right.rangeValue.StartLine
		}
		if left.rangeValue.StartChar != right.rangeValue.StartChar {
			return left.rangeValue.StartChar < right.rangeValue.StartChar
		}
		if left.kind != right.kind {
			return left.kind < right.kind
		}
		if left.from != right.from {
			return left.from < right.from
		}
		return left.to < right.to
	})
	return relations, nil
}

func validateRustCompletenessWitness(
	ctx context.Context,
	indexData, sidecarData []byte,
	rawSCIPSHA256 [sha256.Size]byte,
	scope model.Scope,
	files map[string]model.File,
	view model.WorkspaceView,
) (rustCompletenessValidation, error) {
	sidecar, err := decodeRustRelationSidecar(sidecarData)
	if err != nil {
		return rustCompletenessValidation{}, err
	}
	if sidecar.SchemaVersion != 2 {
		return rustCompletenessValidation{Reason: "legacy Rust sidecar has no completeness witness"}, nil
	}
	if !validRustSHA256Hex(sidecar.SCIPSHA256) || !strings.EqualFold(sidecar.SCIPSHA256, hex.EncodeToString(rawSCIPSHA256[:])) {
		return rustCompletenessValidation{}, errors.New("Rust completeness witness is not bound to the exact SCIP response bytes")
	}
	if view == nil {
		return rustCompletenessValidation{}, model.ErrMissingView
	}
	if len(indexData) == 0 || len(indexData) > rustHelperOutputBytes {
		return rustCompletenessValidation{}, errors.New("Rust completeness witness SCIP response is empty or exceeds its byte limit")
	}
	targetRoot := optionString(scope, "cargo.targetSrcPath")
	if targetRoot == "" || sidecar.SelectedCrateRoot != targetRoot {
		return rustCompletenessValidation{}, fmt.Errorf("Rust completeness witness selected crate root %q does not match Cargo target root %q", sidecar.SelectedCrateRoot, targetRoot)
	}
	if _, err := rustSCIPDocumentURI(scope.RootURI, targetRoot); err != nil {
		return rustCompletenessValidation{}, fmt.Errorf("Rust completeness witness has an unsafe selected crate root: %w", err)
	}
	counts := sidecar.WitnessCounts
	if counts == nil || len(sidecar.SourceManifest) == 0 || counts.SourceFiles > maxRustRelations ||
		counts.UnmappedSourceFiles > counts.SourceFiles {
		return rustCompletenessValidation{}, errors.New("Rust completeness witness has an invalid source manifest or count")
	}
	manifest := make(map[string]struct{}, len(sidecar.SourceManifest))
	previousPath := ""
	rootPresent := false
	manifestBytes := 0
	for i, source := range sidecar.SourceManifest {
		if source.Document == "" || !validRustSHA256Hex(source.SHA256) || (i > 0 && source.Document <= previousPath) {
			return rustCompletenessValidation{}, errors.New("Rust completeness witness source manifest is invalid or not strictly sorted")
		}
		previousPath = source.Document
		manifestBytes += len(source.Document) + 128
		if manifestBytes > maxRustWitnessBytes {
			return rustCompletenessValidation{}, errors.New("Rust completeness witness manifest exceeds its aggregate byte limit")
		}
		uri, pathErr := rustSCIPDocumentURI(scope.RootURI, source.Document)
		if pathErr != nil {
			return rustCompletenessValidation{}, pathErr
		}
		file, ok := files[uri]
		if !ok {
			return rustCompletenessValidation{}, fmt.Errorf("Rust completeness witness source %q is absent from the captured workspace", source.Document)
		}
		content, readErr := readRustCapturedFile(ctx, view, file)
		if readErr != nil {
			return rustCompletenessValidation{}, fmt.Errorf("read Rust witness source %q from the captured workspace: %w", source.Document, readErr)
		}
		digest := sha256.Sum256(content)
		if !strings.EqualFold(source.SHA256, hex.EncodeToString(digest[:])) {
			return rustCompletenessValidation{}, fmt.Errorf("Rust completeness witness source hash does not match captured bytes for %q", source.Document)
		}
		manifest[source.Document] = struct{}{}
		if source.Document == targetRoot {
			rootPresent = true
		}
	}
	if !rootPresent || counts.SourceFiles != uint64(len(sidecar.SourceManifest))+counts.UnmappedSourceFiles {
		return rustCompletenessValidation{}, errors.New("Rust completeness witness source manifest does not account for its selected crate files")
	}
	previousBlocker := ""
	for i, blocker := range sidecar.Blockers {
		if blocker == "" || (i > 0 && blocker <= previousBlocker) {
			return rustCompletenessValidation{}, errors.New("Rust completeness witness blockers are empty, duplicated, or not strictly sorted")
		}
		previousBlocker = blocker
	}
	if counts.SCIPDocuments > maxRustRelations || counts.SCIPSymbols > maxRustRelations ||
		counts.SCIPOccurrences > maxRustRelations || counts.CandidateTokens > maxRustWitnessCandidates ||
		counts.ClassifiedCandidateTokens > counts.CandidateTokens || counts.UnknownCandidateTokens > counts.CandidateTokens {
		return rustCompletenessValidation{Reason: "rust-analyzer witness exceeds the bounded source or SCIP classification budget"}, nil
	}

	var index scip.Index
	if err := proto.Unmarshal(indexData, &index); err != nil {
		return rustCompletenessValidation{}, fmt.Errorf("decode SCIP for Rust completeness witness: %w", err)
	}
	if uint64(len(index.Documents)) != counts.SCIPDocuments {
		return rustCompletenessValidation{}, errors.New("Rust completeness witness SCIP document count differs from the paired index")
	}
	symbolIDs := make(map[string]struct{})
	for _, symbol := range index.ExternalSymbols {
		if symbol == nil || symbol.GetSymbol() == "" {
			return rustCompletenessValidation{}, errors.New("Rust completeness witness SCIP has an empty external symbol id")
		}
		symbolIDs[symbol.GetSymbol()] = struct{}{}
	}
	var occurrenceCount uint64
	for _, document := range index.Documents {
		if document == nil || document.GetRelativePath() == "" {
			return rustCompletenessValidation{}, errors.New("Rust completeness witness SCIP has an empty document")
		}
		if _, ok := manifest[document.GetRelativePath()]; !ok {
			return rustCompletenessValidation{}, fmt.Errorf("Rust completeness witness SCIP document %q is absent from its source manifest", document.GetRelativePath())
		}
		for _, symbol := range document.Symbols {
			if symbol == nil || symbol.GetSymbol() == "" {
				return rustCompletenessValidation{}, errors.New("Rust completeness witness SCIP has an empty document symbol id")
			}
			symbolIDs[symbol.GetSymbol()] = struct{}{}
		}
		for _, occurrence := range document.Occurrences {
			if occurrence == nil || !validRustSCIPSymbol(occurrence.GetSymbol()) {
				return rustCompletenessValidation{}, errors.New("Rust completeness witness SCIP has an invalid occurrence symbol id")
			}
			if _, err := rustOccurrenceRange(occurrence); err != nil {
				return rustCompletenessValidation{}, fmt.Errorf("Rust completeness witness SCIP occurrence has an invalid range: %w", err)
			}
			symbolIDs[occurrence.GetSymbol()] = struct{}{}
			occurrenceCount++
		}
	}
	if uint64(len(symbolIDs)) != counts.SCIPSymbols || occurrenceCount != counts.SCIPOccurrences {
		return rustCompletenessValidation{}, errors.New("Rust completeness witness SCIP symbol/occurrence counts differ from the paired index")
	}

	if len(sidecar.Blockers) != 0 {
		return rustCompletenessValidation{Reason: "rust-analyzer reported completeness blockers: " + strings.Join(sidecar.Blockers, ", ")}, nil
	}
	if counts.UnmappedSourceFiles != 0 || counts.CandidateTokens == 0 ||
		counts.ClassifiedCandidateTokens != counts.CandidateTokens || counts.UnknownCandidateTokens != 0 ||
		counts.MacroSites != 0 || counts.ErrorDiagnostics != 0 || counts.ExternalTargets != 0 ||
		counts.DuplicateSymbols != 0 || counts.SCIPOccurrences == 0 || counts.SCIPSymbols == 0 {
		return rustCompletenessValidation{Reason: "rust-analyzer witness counts do not establish a closed, fully classified source scope"}, nil
	}
	return rustCompletenessValidation{Complete: true, Reason: "rust-analyzer same-response source and occurrence witness verified"}, nil
}

func validRustSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func applyRustCompletenessWitness(report *model.Report, scopeID string, witness rustCompletenessValidation) {
	if !witness.Complete {
		return
	}
	for i := range report.Coverage {
		item := &report.Coverage[i]
		if item.ScopeID != scopeID {
			continue
		}
		switch item.Fact {
		case model.FactSymbol, model.FactDefinition, model.FactReference:
			item.State = model.Complete
			item.Reason = witness.Reason
		}
	}
}

func rustSCIPDocumentURI(rootURI, relative string) (string, error) {
	if !utf8.ValidString(relative) || relative == "" || len(relative) > 4096 || strings.Contains(relative, "\\") || strings.Contains(relative, ":") ||
		path.IsAbs(relative) || path.Clean(relative) != relative || relative == "." || strings.HasPrefix(relative, "../") {
		return "", fmt.Errorf("unsafe SCIP semantic relation relative path %q", relative)
	}
	root, err := url.Parse(rootURI)
	if err != nil || root.Scheme != "file" || root.Opaque != "" || root.RawQuery != "" || root.Fragment != "" || root.User != nil || root.RawPath != "" {
		return "", fmt.Errorf("invalid Rust scope URI %q", rootURI)
	}
	root.Path = path.Join(root.Path, relative)
	root.RawPath = ""
	uri := root.String()
	if !rustURIWithinRoot(rootURI, uri) {
		return "", fmt.Errorf("SCIP semantic relation path escaped Rust scope: %q", relative)
	}
	return uri, nil
}

func validRustSCIPSymbol(symbol string) bool {
	return symbol != "" && len(symbol) <= 4096 && utf8.ValidString(symbol)
}

func rustRangeCoordinates(value rustSidecarRange) (rustSCIPRange, error) {
	if value.Start == nil || value.End == nil || value.Start.Line == nil || value.Start.Character == nil || value.End.Line == nil || value.End.Character == nil {
		return rustSCIPRange{}, errors.New("semantic relation has an incomplete source range")
	}
	rangeValue := rustSCIPRange{*value.Start.Line, *value.Start.Character, *value.End.Line, *value.End.Character}
	if rangeValue.endLine < rangeValue.startLine || (rangeValue.endLine == rangeValue.startLine && rangeValue.endChar < rangeValue.startChar) {
		return rustSCIPRange{}, errors.New("semantic relation has a reversed source range")
	}
	return rangeValue, nil
}

func rustOccurrenceRange(occurrence *scip.Occurrence) (rustSCIPRange, error) {
	if occurrence == nil {
		return rustSCIPRange{}, errors.New("SCIP import occurrence is nil")
	}
	raw := occurrence.GetRange()
	if single := occurrence.GetSingleLineRange(); single != nil {
		raw = []int32{single.GetLine(), single.GetStartCharacter(), single.GetEndCharacter()}
	} else if multi := occurrence.GetMultiLineRange(); multi != nil {
		raw = []int32{multi.GetStartLine(), multi.GetStartCharacter(), multi.GetEndLine(), multi.GetEndCharacter()}
	}
	var result rustSCIPRange
	switch len(raw) {
	case 3:
		if raw[0] < 0 || raw[1] < 0 || raw[2] < raw[1] {
			return rustSCIPRange{}, errors.New("SCIP import occurrence has an invalid range")
		}
		result = rustSCIPRange{uint32(raw[0]), uint32(raw[1]), uint32(raw[0]), uint32(raw[2])}
	case 4:
		if raw[0] < 0 || raw[1] < 0 || raw[2] < raw[0] || raw[3] < 0 || (raw[2] == raw[0] && raw[3] < raw[1]) {
			return rustSCIPRange{}, errors.New("SCIP import occurrence has an invalid range")
		}
		result = rustSCIPRange{uint32(raw[0]), uint32(raw[1]), uint32(raw[2]), uint32(raw[3])}
	default:
		return rustSCIPRange{}, errors.New("SCIP import occurrence has an unsupported range shape")
	}
	return result, nil
}

func rustRangeToUTF16(content []byte, value rustSCIPRange, encoding scip.TextEncoding) (model.Position, error) {
	if value.endLine < value.startLine || (value.endLine == value.startLine && value.endChar < value.startChar) || !utf8.Valid(content) {
		return model.Position{}, errors.New("semantic relation source range is invalid for captured UTF-8 source")
	}
	lines := bytes.Split(content, []byte{'\n'})
	startChar, err := rustColumnToUTF16(lines, value.startLine, value.startChar, encoding)
	if err != nil {
		return model.Position{}, err
	}
	endChar, err := rustColumnToUTF16(lines, value.endLine, value.endChar, encoding)
	if err != nil {
		return model.Position{}, err
	}
	if value.startLine == value.endLine && endChar < startChar {
		return model.Position{}, errors.New("semantic relation source range is reversed")
	}
	return model.Position{StartLine: value.startLine, StartChar: startChar, EndLine: value.endLine, EndChar: endChar}, nil
}

func rustColumnToUTF16(lines [][]byte, lineNo, column uint32, encoding scip.TextEncoding) (uint32, error) {
	if uint64(lineNo) >= uint64(len(lines)) {
		return 0, fmt.Errorf("semantic relation line %d is outside captured source", lineNo)
	}
	line := lines[lineNo]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	fromColumn, toUTF16 := uint32(0), uint32(0)
	for offset := 0; offset < len(line); {
		if column == fromColumn {
			return toUTF16, nil
		}
		r, size := utf8.DecodeRune(line[offset:])
		if r == utf8.RuneError && size == 1 {
			return 0, errors.New("semantic relation source line is not valid UTF-8")
		}
		switch encoding {
		case scip.TextEncoding_UTF8:
			fromColumn += uint32(size)
		case scip.TextEncoding_UTF16:
			if r > 0xFFFF {
				fromColumn += 2
			} else {
				fromColumn++
			}
		default:
			return 0, fmt.Errorf("unsupported SCIP source encoding %s", encoding)
		}
		if r > 0xFFFF {
			toUTF16 += 2
		} else {
			toUTF16++
		}
		offset += size
	}
	if column == fromColumn {
		return toUTF16, nil
	}
	return 0, fmt.Errorf("semantic relation column %d is outside a UTF-8/UTF-16 character boundary", column)
}

func readRustCapturedFile(ctx context.Context, view model.WorkspaceView, file model.File) ([]byte, error) {
	if file.Size < 0 || file.Size > maxRustSemanticFileBytes {
		return nil, fmt.Errorf("captured Rust semantic source %s exceeds the %d-byte validation limit", file.URI, maxRustSemanticFileBytes)
	}
	reader, err := view.Read(ctx, file.URI)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxRustSemanticFileBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) != file.Size || len(data) > maxRustSemanticFileBytes {
		return nil, fmt.Errorf("captured Rust semantic source size changed for %s", file.URI)
	}
	sum := sha256.Sum256(data)
	if !validRustContentHash(string(file.SHA256)) || !strings.EqualFold(hex.EncodeToString(sum[:]), normalizedSnapshotHash(string(file.SHA256))) {
		return nil, fmt.Errorf("captured Rust semantic source hash changed for %s", file.URI)
	}
	return data, nil
}

type rustCapturedText struct {
	file       model.File
	content    []byte
	lines      [][]byte
	lineUnits  []uint64
	lineStarts []uint64
}

type rustCapturedTextCache struct {
	ctx         context.Context
	view        model.WorkspaceView
	files       map[string]model.File
	contents    map[string]*rustCapturedText
	cachedBytes int64
}

func (cache *rustCapturedTextCache) get(uri string) (*rustCapturedText, error) {
	if cached := cache.contents[uri]; cached != nil {
		return cached, nil
	}
	file, ok := cache.files[uri]
	if !ok {
		return nil, fmt.Errorf("captured Rust source %s is absent from the immutable view", uri)
	}
	content, err := readRustCapturedFile(cache.ctx, cache.view, file)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(content) {
		return nil, fmt.Errorf("captured Rust source %s is not valid UTF-8", uri)
	}
	text := &rustCapturedText{file: file, content: content, lines: bytes.Split(content, []byte{'\n'})}
	text.lineUnits = make([]uint64, len(text.lines))
	text.lineStarts = make([]uint64, len(text.lines))
	for i, line := range text.lines {
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		for offset := 0; offset < len(line); {
			r, size := utf8.DecodeRune(line[offset:])
			if r == utf8.RuneError && size == 1 {
				return nil, fmt.Errorf("captured Rust source %s has invalid UTF-8", uri)
			}
			if r > 0xFFFF {
				text.lineUnits[i] += 2
			} else {
				text.lineUnits[i]++
			}
			offset += size
		}
		if i > 0 {
			text.lineStarts[i] = text.lineStarts[i-1] + text.lineUnits[i-1] + 1
		}
	}
	if cache.cachedBytes+int64(len(content)) <= maxRustRelationBytes {
		cache.contents[uri] = text
		cache.cachedBytes += int64(len(content))
	}
	return text, nil
}

func (text *rustCapturedText) positionOffset(line, character uint32) (uint64, error) {
	if uint64(line) >= uint64(len(text.lines)) {
		return 0, fmt.Errorf("source line %d is outside captured file", line)
	}
	if _, err := rustColumnToUTF16(text.lines, line, character, scip.TextEncoding_UTF16); err != nil {
		return 0, err
	}
	return text.lineStarts[line] + uint64(character), nil
}

func (text *rustCapturedText) positionAt(offset uint64) (model.Position, error) {
	for line, start := range text.lineStarts {
		if offset >= start && offset-start <= text.lineUnits[line] {
			character := uint32(offset - start)
			if _, err := rustColumnToUTF16(text.lines, uint32(line), character, scip.TextEncoding_UTF16); err != nil {
				return model.Position{}, err
			}
			return model.Position{StartLine: uint32(line), StartChar: character, EndLine: uint32(line), EndChar: character}, nil
		}
	}
	return model.Position{}, errors.New("mapped source offset is outside captured file")
}

func (text *rustCapturedText) validateRange(value rustSCIPRange) (model.Position, uint64, uint64, error) {
	start, err := text.positionOffset(value.startLine, value.startChar)
	if err != nil {
		return model.Position{}, 0, 0, err
	}
	end, err := text.positionOffset(value.endLine, value.endChar)
	if err != nil {
		return model.Position{}, 0, 0, err
	}
	if end < start {
		return model.Position{}, 0, 0, errors.New("source range is reversed")
	}
	return model.Position{StartLine: value.startLine, StartChar: value.startChar, EndLine: value.endLine, EndChar: value.endChar}, start, end, nil
}

func resolveRustRelationSource(
	cache *rustCapturedTextCache,
	rootURI string,
	file model.File,
	rangeValue rustSCIPRange,
) (string, identity.ContentHash, model.Position, error) {
	generated, err := cache.get(file.URI)
	if err != nil {
		return "", "", model.Position{}, err
	}
	rangePosition, rangeStart, rangeEnd, err := generated.validateRange(rangeValue)
	if err != nil {
		return "", "", model.Position{}, fmt.Errorf("invalid UTF-16 source range: %w", err)
	}
	if !file.Generated {
		return file.URI, file.SHA256, rangePosition, nil
	}
	if file.SourceURI == "" || len(file.SourceMap) == 0 || len(file.SourceMap) > maxRustRelations {
		return "", "", model.Position{}, errors.New("generated Rust source is missing a bounded source URI/map")
	}
	if !validRustRelationSourceURI(rootURI, file.SourceURI) {
		return "", "", model.Position{}, fmt.Errorf("generated Rust source has an unsafe source URI %q", file.SourceURI)
	}
	if _, err := cache.get(file.SourceURI); err != nil {
		return "", "", model.Position{}, fmt.Errorf("validate generated Rust source origin: %w", err)
	}
	var mappedURI string
	var mappedHash identity.ContentHash
	var mappedRange model.Position
	for _, span := range file.SourceMap {
		if span.SourceURI == "" || !validRustRelationSourceURI(rootURI, span.SourceURI) {
			return "", "", model.Position{}, fmt.Errorf("generated Rust source map has an unsafe source URI %q", span.SourceURI)
		}
		spanStart, err := generated.positionOffset(span.Generated.StartLine, span.Generated.StartChar)
		if err != nil {
			return "", "", model.Position{}, fmt.Errorf("invalid generated source-map start: %w", err)
		}
		spanEnd, err := generated.positionOffset(span.Generated.EndLine, span.Generated.EndChar)
		if err != nil || spanEnd < spanStart {
			return "", "", model.Position{}, errors.New("invalid generated source-map range")
		}
		if rangeStart < spanStart || rangeEnd > spanEnd {
			continue
		}
		source, err := cache.get(span.SourceURI)
		if err != nil {
			return "", "", model.Position{}, fmt.Errorf("validate generated source-map origin: %w", err)
		}
		sourceStart, err := source.positionOffset(span.Source.StartLine, span.Source.StartChar)
		if err != nil {
			return "", "", model.Position{}, fmt.Errorf("invalid source-map source start: %w", err)
		}
		sourceEnd, err := source.positionOffset(span.Source.EndLine, span.Source.EndChar)
		if err != nil || sourceEnd < sourceStart || spanEnd-spanStart != sourceEnd-sourceStart {
			return "", "", model.Position{}, errors.New("source-map span does not preserve UTF-16 offsets")
		}
		mappedStart := sourceStart + (rangeStart - spanStart)
		mappedEnd := sourceStart + (rangeEnd - spanStart)
		startPosition, err := source.positionAt(mappedStart)
		if err != nil {
			return "", "", model.Position{}, err
		}
		endPosition, err := source.positionAt(mappedEnd)
		if err != nil {
			return "", "", model.Position{}, err
		}
		candidate := model.Position{
			StartLine: startPosition.StartLine, StartChar: startPosition.StartChar,
			EndLine: endPosition.StartLine, EndChar: endPosition.StartChar,
		}
		if _, _, _, err := source.validateRange(rustSCIPRange{
			startLine: candidate.StartLine, startChar: candidate.StartChar,
			endLine: candidate.EndLine, endChar: candidate.EndChar,
		}); err != nil {
			return "", "", model.Position{}, fmt.Errorf("invalid mapped source range: %w", err)
		}
		if mappedURI != "" && (mappedURI != span.SourceURI || mappedRange != candidate) {
			return "", "", model.Position{}, errors.New("generated source range maps ambiguously")
		}
		mappedURI, mappedHash, mappedRange = span.SourceURI, source.file.SHA256, candidate
	}
	if mappedURI == "" {
		return "", "", model.Position{}, errors.New("generated relation range has no source-map entry")
	}
	return mappedURI, mappedHash, mappedRange, nil
}

func validRustRelationSourceURI(rootURI, sourceURI string) bool {
	parsed, err := workspaceuri.Parse(sourceURI)
	return err == nil && parsed.IsFile() && parsed.Canonical() == sourceURI && rustURIWithinRoot(rootURI, sourceURI)
}

// SCIP carries explicit Rust relationship records, but does not attest that
// every relation in the requested Cargo build context was exported. Keep
// those facts useful as a known subset without letting a lower-level adapter
// accidentally upgrade them to Complete.
func constrainRustSCIPRelationCoverage(report *model.Report, scopeID string) {
	for i := range report.Coverage {
		item := &report.Coverage[i]
		if item.ScopeID != scopeID || (item.Fact != model.FactImplementation && item.Fact != model.FactTypeRelation) {
			continue
		}
		if item.State == model.Complete || item.State == model.IncompleteKnownSubset {
			item.State = model.IncompleteKnownSubset
			item.Reason = "rust-analyzer SCIP relations were imported, but SCIP does not prove exhaustive relationship coverage for this Cargo build context"
		}
	}
}

func bindRustSCIPProjectRoot(data []byte, materializedRoot, capturedRootURI, repository string) ([]byte, error) {
	var index scip.Index
	if err := proto.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("decode SCIP index: %w", err)
	}
	if index.Metadata == nil || index.Metadata.GetProjectRoot() == "" {
		return nil, errors.New("SCIP metadata omitted project root")
	}
	projectRoot := index.Metadata.GetProjectRoot()
	if projectRoot != capturedRootURI && (repository == "" || projectRoot != repository) {
		projectPath, err := scipProjectRootPath(projectRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve SCIP project root %q: %w", projectRoot, err)
		}
		if !sameMaterializedDirectory(projectPath, materializedRoot) {
			return nil, fmt.Errorf("SCIP project root %q does not identify the verified materialized workspace", projectRoot)
		}
	}
	index.Metadata.ProjectRoot = capturedRootURI
	normalized, err := proto.Marshal(&index)
	if err != nil {
		return nil, fmt.Errorf("encode SCIP index with captured project root: %w", err)
	}
	return normalized, nil
}

func scipProjectRootPath(projectRoot string) (string, error) {
	if filepath.Separator == '\\' && strings.HasPrefix(strings.ToLower(projectRoot), "file://") {
		candidate := projectRoot[len("file://"):]
		if isWindowsDrivePath(candidate) {
			candidate = strings.ReplaceAll(candidate, "/", string(filepath.Separator))
			candidate = strings.ReplaceAll(candidate, "\\", string(filepath.Separator))
			return candidate, nil
		}
	}
	parsed, err := workspaceuri.Parse(projectRoot)
	if err != nil {
		return "", err
	}
	if !parsed.IsFile() {
		return "", errors.New("project root is not a file URI")
	}
	return parsed.Path()
}

func isWindowsDrivePath(path string) bool {
	return len(path) >= 3 && ((path[0] >= 'a' && path[0] <= 'z') || (path[0] >= 'A' && path[0] <= 'Z')) &&
		path[1] == ':' && (path[2] == '\\' || path[2] == '/')
}

func sameMaterializedDirectory(left, right string) bool {
	leftReal, err := realDirectory(left)
	if err != nil {
		return false
	}
	rightReal, err := realDirectory(right)
	if err != nil {
		return false
	}
	if filepath.Separator == '\\' {
		return strings.EqualFold(leftReal, rightReal)
	}
	return leftReal == rightReal
}

func realDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("project root path is not absolute")
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("project root is not a directory")
	}
	return filepath.Clean(real), nil
}

// verifyTemporaryIsolation ensures cleanup and build outputs cannot overlap
// the borrowed immutable source tree in either direction.
func verifyTemporaryIsolation(tempRoot, sourceRoot string) error {
	tempRealPath, err := filepath.Abs(tempRoot)
	if err != nil {
		return err
	}
	tempRealPath, err = filepath.EvalSymlinks(tempRealPath)
	if err != nil {
		return fmt.Errorf("resolve isolated build directory: %w", err)
	}
	sourceRealPath, err := filepath.Abs(sourceRoot)
	if err != nil {
		return err
	}
	sourceRealPath, err = filepath.EvalSymlinks(sourceRealPath)
	if err != nil {
		return fmt.Errorf("resolve immutable source root: %w", err)
	}
	if pathWithin(tempRealPath, sourceRealPath) || pathWithin(sourceRealPath, tempRealPath) {
		return errors.New("isolated build directory overlaps the borrowed immutable workspace")
	}
	return nil
}

func verifyCapturedWorkspace(ctx context.Context, view model.WorkspaceView, materialized model.MaterializedView, rootURI string) error {
	rootPath, err := filepath.Abs(materialized.RootPath())
	if err != nil {
		return err
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return fmt.Errorf("resolve isolated workspace root: %w", err)
	}
	expected := make(map[string]struct{})
	walkErr := view.Walk(ctx, rootURI, func(file model.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		capturedHash, capturedSize, err := hashViewFile(ctx, view, file.URI)
		if err != nil {
			return fmt.Errorf("read captured source %s: %w", file.URI, err)
		}
		if file.Size >= 0 && file.Size != capturedSize {
			return fmt.Errorf("captured source %s size mismatch: metadata=%d bytes=%d", file.URI, file.Size, capturedSize)
		}
		if expectedHash := normalizedSnapshotHash(string(file.SHA256)); expectedHash != "" && !strings.EqualFold(expectedHash, capturedHash) {
			return fmt.Errorf("captured source %s digest does not match view metadata", file.URI)
		}
		mapped, err := materialized.PathForURI(file.URI)
		if err != nil {
			return fmt.Errorf("map captured source %s into isolated workspace: %w", file.URI, err)
		}
		mapped, err = filepath.Abs(mapped)
		if err != nil {
			return err
		}
		mappedRealPath, err := filepath.EvalSymlinks(mapped)
		if err != nil {
			return fmt.Errorf("resolve isolated source %s: %w", file.URI, err)
		}
		if !pathWithin(rootPath, mappedRealPath) {
			return fmt.Errorf("materialized source %s escaped isolated workspace", file.URI)
		}
		materializedHash, materializedSize, err := hashFile(ctx, mappedRealPath)
		if err != nil {
			return fmt.Errorf("read isolated source %s: %w", file.URI, err)
		}
		if capturedSize != materializedSize || capturedHash != materializedHash {
			return fmt.Errorf("isolated source %s differs from immutable workspace view", file.URI)
		}
		expected[filepath.Clean(mappedRealPath)] = struct{}{}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	return filepath.WalkDir(rootPath, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("materialized workspace contains an untracked non-regular source entry: %s", path)
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		if _, ok := expected[filepath.Clean(canonical)]; !ok {
			return fmt.Errorf("materialized workspace contains a file absent from the immutable view: %s", path)
		}
		return nil
	})
}

func normalizedSnapshotHash(value string) string {
	if value == "" {
		return ""
	}
	text := strings.TrimSpace(value)
	text = strings.TrimPrefix(strings.ToLower(text), "sha256:")
	if len(text) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(text); err != nil {
		return ""
	}
	return text
}

func hashViewFile(ctx context.Context, view model.WorkspaceView, uri string) (string, int64, error) {
	reader, err := view.Read(ctx, uri)
	if err != nil {
		return "", 0, err
	}
	defer reader.Close()
	return hashReader(ctx, reader)
}

func hashFile(ctx context.Context, path string) (string, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	return hashReader(ctx, file)
}

func hashReader(ctx context.Context, source io.Reader) (string, int64, error) {
	hash := sha256.New()
	buffer := make([]byte, 64*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
			total += int64(n)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", total, readErr
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), total, nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

func toolsForScope(ctx context.Context, provenance model.Provenance, scope model.Scope, run scipRunner) (map[string]model.ToolIdentity, string) {
	configured := make(map[string]model.ToolIdentity, len(provenance.Tools))
	for _, tool := range provenance.Tools {
		if _, exists := configured[tool.Name]; exists {
			return nil, "duplicate pinned tool identity: " + tool.Name
		}
		configured[tool.Name] = tool
	}
	if !reflect.DeepEqual(provenance.Scope, scope) {
		return nil, "scope provenance does not match the requested Cargo build scope"
	}
	procMacros, _, configErr := semanticOptions(scope)
	if configErr != nil {
		return nil, configErr.Error()
	}
	required := []string{rustAnalyzerToolName, cargoToolName, rustcToolName}
	if _, ok := configured[rustSemanticHelperToolName]; ok {
		required = append(required, rustSemanticHelperToolName)
	}
	if procMacros {
		required = append(required, procMacroToolName)
	}
	resolved := make(map[string]model.ToolIdentity, len(required))
	for _, name := range required {
		tool, ok := configured[name]
		if !ok {
			return nil, "required pinned semantic tool is missing: " + name
		}
		if err := verifyToolIdentity(ctx, tool); err != nil {
			return nil, name + " tool identity is unavailable: " + err.Error()
		}
		if name != procMacroToolName {
			output, err := run(ctx, scipInvocation{Tool: tool, Args: []string{"--version"}})
			if err != nil {
				return nil, name + " version probe failed: " + err.Error()
			}
			if !versionMatches(string(output), tool.Version) {
				return nil, fmt.Sprintf("%s version mismatch: configured %q, executable reported %q", name, tool.Version, strings.TrimSpace(string(output)))
			}
		}
		resolved[name] = tool
	}
	for name := range configured {
		if _, ok := resolved[name]; !ok {
			return nil, "pinned tool is not used by this Rust scope: " + name
		}
	}
	return resolved, ""
}

func verifyToolIdentity(ctx context.Context, tool model.ToolIdentity) error {
	if tool.Name == "" || tool.Path == "" || tool.Version == "" || len(tool.SHA256) != sha256.Size*2 {
		return errors.New("incomplete pinned executable identity")
	}
	if _, err := hex.DecodeString(tool.SHA256); err != nil {
		return errors.New("pinned executable digest is not hexadecimal")
	}
	if !filepath.IsAbs(tool.Path) {
		return errors.New("pinned executable path is not absolute")
	}
	file, err := os.Open(tool.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash, _, err := hashReader(ctx, file)
	if err != nil {
		return err
	}
	if !strings.EqualFold(hash, tool.SHA256) {
		return errors.New("pinned executable digest mismatch")
	}
	return nil
}

func versionMatches(output, configured string) bool {
	output = strings.TrimSpace(output)
	configured = strings.TrimSpace(configured)
	if output == configured {
		return true
	}
	for _, field := range strings.Fields(output) {
		if field == configured {
			return true
		}
	}
	return false
}

func rustAnalyzerConfig(scope model.Scope, tools map[string]model.ToolIdentity, targetDir string) (map[string]any, error) {
	procMacros, buildScripts, err := semanticOptions(scope)
	if err != nil {
		return nil, err
	}
	args := []string{"--locked"}
	args = append(args, scope.Build.Arguments...)
	for _, packagePattern := range scope.Build.PackagePatterns {
		args = append(args, "--package", packagePattern)
	}
	cfgs := make([]string, 0, len(scope.Build.Defines))
	defineNames := make([]string, 0, len(scope.Build.Defines))
	for name := range scope.Build.Defines {
		defineNames = append(defineNames, name)
	}
	sort.Strings(defineNames)
	for _, name := range defineNames {
		value := scope.Build.Defines[name]
		if value == "" {
			cfgs = append(cfgs, name)
		} else {
			cfgs = append(cfgs, name+"="+value)
		}
	}
	features := append([]string(nil), scope.Build.Features...)
	var cargoFeatures any = features
	if len(features) == 1 && features[0] == "all" {
		cargoFeatures = "all"
	}
	cargo := map[string]any{
		"features":          cargoFeatures,
		"allTargets":        scope.Build.Tests,
		"targetDir":         targetDir,
		"extraArgs":         args,
		"extraEnv":          cloneStringMap(scope.Build.Environment),
		"cfgs":              cfgs,
		"buildScripts":      map[string]any{"enable": buildScripts},
		"noDefaultFeatures": optionBool(scope, "noDefaultFeatures", "cargo.noDefaultFeatures", false),
	}
	if target := optionString(scope, "target", "cargo.target"); target != "" {
		cargo["target"] = target
	}
	if value, ok := optionValue(scope, "cargo.allTargets"); ok {
		parsed, err := parseBool(value)
		if err != nil {
			return nil, fmt.Errorf("cargo.allTargets option: %w", err)
		}
		cargo["allTargets"] = parsed
	}
	procMacro := map[string]any{"enable": procMacros}
	if procMacros {
		procMacro["server"] = tools[procMacroToolName].Path
	}
	return map[string]any{
		"cargo":     cargo,
		"cfg":       map[string]any{"setTest": scope.Build.Tests},
		"procMacro": procMacro,
	}, nil
}

func semanticOptions(scope model.Scope) (procMacros, buildScripts bool, err error) {
	if len(scope.Build.IncludePaths) != 0 {
		return false, false, errors.New("Rust SCIP exporter cannot apply include-path build inputs")
	}
	if encoded, ok := scope.Build.Options[rustPlannerInputsOption]; ok {
		if _, err := decodeRustPlannerInputs(encoded); err != nil {
			return false, false, fmt.Errorf("invalid Rust planner input marker: %w", err)
		}
	}
	procMacros = optionBool(scope, "procMacros", "procMacro.enable", true)
	buildScripts = optionBool(scope, "buildScripts", "cargo.buildScripts.enable", true)
	if procMacros && !buildScripts {
		return false, false, errors.New("Rust proc-macro expansion requires build scripts, but this scope disables them")
	}
	known := map[string]bool{
		"target": true, "cargo.target": true,
		"noDefaultFeatures": true, "cargo.noDefaultFeatures": true,
		"procMacros": true, "procMacro.enable": true,
		"buildScripts": true, "cargo.buildScripts.enable": true,
		"cargo.allTargets": true,
		"cargo.targetKind": true, "cargo.targetName": true, "cargo.targetSrcPath": true,
		rustPlannerInputsOption: true,
	}
	for name, value := range scope.Build.Options {
		if !known[name] {
			return false, false, fmt.Errorf("unsupported Rust build input option %q", name)
		}
		if strings.Contains(name, "enable") || strings.Contains(name, "Features") || name == "procMacros" || name == "buildScripts" || name == "cargo.allTargets" {
			if _, err := parseBool(value); err != nil {
				return false, false, fmt.Errorf("Rust build input option %q: %w", name, err)
			}
		}
	}
	return procMacros, buildScripts, nil
}

func optionValue(scope model.Scope, keys ...string) (string, bool) {
	for _, key := range keys {
		if value, ok := scope.Build.Options[key]; ok {
			return value, true
		}
	}
	return "", false
}

func optionString(scope model.Scope, keys ...string) string {
	value, _ := optionValue(scope, keys...)
	return value
}

func optionBool(scope model.Scope, first, second string, fallback bool) bool {
	value, ok := optionValue(scope, first, second)
	if !ok {
		return fallback
	}
	parsed, _ := parseBool(value)
	return parsed
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, fmt.Errorf("expected boolean, got %q", value)
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return map[string]string{}
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func rustCommandEnv(scope model.Scope, tools map[string]model.ToolIdentity, targetDir, tempRoot string) []string {
	values := make(map[string]string)
	for _, key := range []string{"PATH", "SystemRoot", "SYSTEMROOT", "WINDIR", "HOME", "USERPROFILE", "PATHEXT"} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	for key, value := range scope.Build.Environment {
		values[key] = value
	}
	toolPaths := make([]string, 0, len(tools))
	for _, name := range []string{rustAnalyzerToolName, cargoToolName, rustcToolName, procMacroToolName} {
		if tool, ok := tools[name]; ok {
			toolPaths = append(toolPaths, filepath.Dir(tool.Path))
		}
	}
	pathParts := append(toolPaths, filepath.SplitList(values["PATH"])...)
	values["PATH"] = strings.Join(deduplicateStrings(pathParts), string(os.PathListSeparator))
	values["CARGO_TARGET_DIR"] = targetDir
	values["RUSTC"] = tools[rustcToolName].Path
	values["TMP"] = filepath.Join(tempRoot, "tmp")
	values["TEMP"] = filepath.Join(tempRoot, "tmp")
	values["TMPDIR"] = filepath.Join(tempRoot, "tmp")
	_ = os.MkdirAll(values["TMP"], 0o700)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func deduplicateStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		key := value
		if os.PathSeparator == '\\' {
			key = strings.ToLower(filepath.Clean(value))
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func usedSemanticTools(scope model.Scope, tools map[string]model.ToolIdentity) []model.ToolIdentity {
	procMacros, buildScripts, _ := semanticOptions(scope)
	_ = buildScripts // Cargo/rustc participate in loading build-script output.
	names := []string{rustAnalyzerToolName, cargoToolName, rustcToolName}
	if procMacros {
		names = append(names, procMacroToolName)
	}
	if _, ok := tools[rustSemanticHelperToolName]; ok {
		names = append(names, rustSemanticHelperToolName)
	}
	used := make([]model.ToolIdentity, 0, len(names))
	for _, name := range names {
		if tool, ok := tools[name]; ok {
			used = append(used, tool)
		}
	}
	return used
}

func runRustAnalyzerSCIP(ctx context.Context, invocation scipInvocation) ([]byte, error) {
	cmd := exec.CommandContext(ctx, invocation.Tool.Path, invocation.Args...)
	cmd.Dir = invocation.Dir
	cmd.Env = invocation.Env
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return nil, fmt.Errorf("%w: %s", err, message)
		}
		return nil, err
	}
	if len(invocation.Args) > 0 && invocation.Args[0] == "--version" {
		return output, nil
	}
	data, err := os.ReadFile(invocation.OutputPath)
	if err != nil {
		return nil, fmt.Errorf("read SCIP output: %w", err)
	}
	return data, nil
}

func unavailableCoverage(scopeID, reason string) []model.Coverage {
	result := make([]model.Coverage, 0, len(model.RequiredFactKinds))
	for _, fact := range model.RequiredFactKinds {
		result = append(result, model.Coverage{ScopeID: scopeID, Fact: fact, State: model.Unavailable, Reason: reason})
	}
	return result
}

func appendCoverage(report *model.Report, coverage ...model.Coverage) {
	report.Coverage = append(report.Coverage, coverage...)
}
