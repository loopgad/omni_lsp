package typescript

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

const (
	semanticIndexCompilerName    = "typescript"
	semanticIndexCompilerVersion = "6.0.3"
	// semanticIndexNodeName identifies the pinned Node executable used to load
	// the compiler and run the direct adapter. Node's runtime behavior is part
	// of the semantic toolchain, so the direct runner requires this identity in
	// addition to the TypeScript module identity.
	semanticIndexNodeName     = "node"
	semanticIndexExporterName = "scip-typescript"
	// semanticIndexDirectExporterName identifies the in-process adapter that
	// drives TypeScript's public compiler API. TypeScript does not ship a SCIP
	// exporter; keeping this identity separate avoids claiming that the direct
	// adapter is scip-typescript.
	semanticIndexDirectExporterName   = "omnilsp-typescript-compiler-exporter"
	semanticIndexBatchLimit           = 256
	directJavaScriptCheckJSProof      = "typescript-direct-js-checkjs-static-v1"
	directTypeScriptStaticModuleProof = "typescript-direct-ts-static-modules-v1"
	semanticIndexExtractor            = "omnilsp-typescript-scip"
	semanticIndexExtractorVersion     = "8"
	semanticIndexToolchain            = "typescript/6.0.3"
)

// SCIPBatch is one bounded batch of compiler-derived facts. IDs are canonical
// compiler/SCIP semantic identities. The provider preserves them so references
// to declarations in another project remain linked; the runner must encode
// project/context identity in symbols when the compiler graph requires it.
type SCIPBatch struct {
	Symbols     []model.Symbol
	Occurrences []model.Occurrence
	Edges       []model.Edge
}

// SCIPRequest gives an injected TypeScript/SCIP adapter only the pinned tools
// and an immutable, read-only materialized snapshot. Implementations must use
// TypeScript's semantic model and a SCIP exporter; source-text guessing is not
// acceptable. Temporary files and outputs must be written outside RootPath.
type SCIPRequest struct {
	Scope            model.Scope
	ProjectScopes    []model.Scope
	ReferenceScopes  []model.Scope
	WorkspaceRootURI string
	ReferenceFiles   map[string][]model.File
	Materialized     model.MaterializedView
	// Files is the immutable scope manifest used to preserve canonical source
	// URIs and generated/source-map metadata after materialization. Runners must
	// never rediscover identity from the temporary filesystem path.
	Files []model.File
	Tools []model.ToolIdentity
}

type SCIPResult struct {
	Coverage []model.Coverage
}

// ScopeBuilder discovers TypeScript/JavaScript projects and their effective
// compiler inputs from the immutable view. It must not inspect the mutable
// filesystem.
type ScopeBuilder func(ctx context.Context, view model.WorkspaceView, rootURI string) ([]model.Scope, error)

// SemanticIndexConfig carries project discovery and exact compiler/exporter
// pins. The request builder derives each scope's context and provenance from
// these values.
type SemanticIndexConfig struct {
	Runner           SCIPRunner
	BuildScopes      ScopeBuilder
	Tools            []model.ToolIdentity
	Extractor        string
	ExtractorVersion string
	Toolchain        string
	Backend          identity.BackendID
	BackendEpoch     identity.BackendEpoch
	// BackendState supplies the live language/backend identity at request-build
	// time. This keeps an index build bound to the current supervised child
	// epoch after a restart instead of freezing the epoch at server startup.
	BackendState func(language string) (identity.BackendID, identity.BackendEpoch)
}

// SCIPRunner is injected by the host's pinned tool configuration. VerifyTools
// must attest to the executables that will actually be used. ExportSCIP must
// stream bounded compiler-derived batches and return the exact per-fact
// coverage it can support. It may emit cross-project references/edges only
// when their compiler semantic IDs resolve exactly; unresolved targets are
// omitted and the affected fact coverage is marked incomplete or unknown.
type SCIPRunner interface {
	VerifyTools(ctx context.Context, request SCIPRequest) ([]model.ToolIdentity, error)
	// ExportSCIP emits coordinates as zero-based UTF-16 code-unit positions,
	// matching model.Position, even when the exporter uses another coordinate
	// system.
	ExportSCIP(ctx context.Context, request SCIPRequest, emit func(SCIPBatch) error) (SCIPResult, error)
}

// SemanticIndexProvider exports TypeScript semantic facts through an injected
// TypeScript/SCIP runner. It never parses TypeScript source to invent semantic facts.
type SemanticIndexProvider struct {
	config SemanticIndexConfig
}

var _ languages.SemanticIndexProvider = (*SemanticIndexProvider)(nil)

var _ languages.SemanticIndexRequestBuilder = (*SemanticIndexProvider)(nil)

func NewSemanticIndexProvider(config SemanticIndexConfig) *SemanticIndexProvider {
	config.Tools = append([]model.ToolIdentity(nil), config.Tools...)
	if config.BuildScopes == nil {
		config.BuildScopes = DiscoverScopes
	}
	if config.Extractor == "" {
		config.Extractor = semanticIndexExtractor
	}
	if config.ExtractorVersion == "" {
		config.ExtractorVersion = semanticIndexExtractorVersion
	}
	if config.Toolchain == "" {
		config.Toolchain = semanticIndexToolchain
	}
	return &SemanticIndexProvider{config: config}
}

// BuildIndexRequest discovers and pins each TS/JS project under rootURI.
// Effective tsconfig/jsconfig digest, compiler options, project references,
// and explicit plugin policy must be represented in Scope.Build before it is
// returned. An empty plugin list is recorded as `plugins: []`.
func (p *SemanticIndexProvider) BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	if view == nil {
		return model.Request{}, model.ErrMissingView
	}
	if p == nil || p.config.BuildScopes == nil {
		return model.Request{}, errors.New("typescript semantic index: no project scope builder is configured")
	}
	if rootURI == "" {
		return model.Request{}, model.ErrInvalidScope
	}
	if err := ctx.Err(); err != nil {
		return model.Request{}, err
	}
	tools := append([]model.ToolIdentity(nil), p.config.Tools...)
	if !validToolMetadata(tools) {
		return model.Request{}, model.ErrInvalidProvenance
	}
	scopes, err := p.config.BuildScopes(ctx, view, rootURI)
	if err != nil {
		return model.Request{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Request{}, err
	}
	request := model.Request{
		WorkspaceRootURI: rootURI, View: view,
		Scopes: make([]model.Scope, 0, len(scopes)), Provenance: make(map[string]model.Provenance, len(scopes)),
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, discoveredScope := range scopes {
		if err := ctx.Err(); err != nil {
			return model.Request{}, err
		}
		scope := cloneScope(discoveredScope)
		if scope.ID == "" || !isTypeScriptLanguage(scope.Language) || scope.RootURI == "" || !uriWithinRoot(rootURI, scope.RootURI) {
			return model.Request{}, model.ErrInvalidScope
		}
		if _, ok := seen[scope.ID]; ok {
			return model.Request{}, model.ErrInvalidScope
		}
		seen[scope.ID] = struct{}{}
		backend := p.config.Backend
		backendEpoch := p.config.BackendEpoch
		if p.config.BackendState != nil {
			backend, backendEpoch = p.config.BackendState(scope.Language)
		}
		if backend.Language == "" {
			backend.Language = scope.Language
		}
		if backend.Name == "" {
			backend.Name = "typescript-semantic-index"
		}
		if backend.Language != scope.Language {
			return model.Request{}, model.ErrInvalidProvenance
		}
		provenance := model.Provenance{
			SchemaVersion: model.SchemaVersion, Identity: view.Identity(), Scope: cloneScope(scope),
			Extractor: p.config.Extractor, ExtractorVer: p.config.ExtractorVersion,
			Backend: backend, BackendEpoch: backendEpoch, Toolchain: p.config.Toolchain,
			Tools: append([]model.ToolIdentity(nil), tools...),
		}
		scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
		provenance.Scope = cloneScope(scope)
		request.Scopes = append(request.Scopes, scope)
		request.Provenance[scope.ID] = provenance
	}
	return request, nil
}

// RebuildVerifiedPlannerRequest reconstructs the current TypeScript/JavaScript
// planner request from a previously verified generation. It only reads the
// immutable view and pinned tool files; it never creates or invokes a runner.
// Scope discovery is repeated so config and project-reference closure changes
// cannot be hidden by reusing the generation's saved scopes.
func RebuildVerifiedPlannerRequest(ctx context.Context, view model.WorkspaceView, rootURI string, attestations []model.Provenance) (model.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if view == nil {
		return model.Request{}, model.ErrMissingView
	}
	currentIdentity := view.Identity()
	if rootURI == "" || currentIdentity.Workspace == "" || currentIdentity.DiskDigest == "" || len(attestations) == 0 {
		return model.Request{}, model.ErrInvalidProvenance
	}
	if err := ctx.Err(); err != nil {
		return model.Request{}, err
	}

	first := attestations[0]
	tools, configured := pinnedTools(first.Tools)
	if !configured || first.SchemaVersion != model.SchemaVersion || first.Extractor == "" || first.ExtractorVer == "" || first.Toolchain == "" {
		return model.Request{}, model.ErrInvalidProvenance
	}
	backends := make(map[string]struct {
		id    identity.BackendID
		epoch identity.BackendEpoch
	}, len(attestations))
	expected := make(map[string]model.Provenance, len(attestations))
	for _, provenance := range attestations {
		scope := provenance.Scope
		if provenance.SchemaVersion != model.SchemaVersion ||
			!sameStableWorkspaceIdentity(provenance.Identity, currentIdentity) ||
			provenance.Extractor != first.Extractor || provenance.ExtractorVer != first.ExtractorVer || provenance.Toolchain != first.Toolchain ||
			!sameToolSet(tools, provenance.Tools) || !validToolList(provenance.Tools) ||
			scope.ID == "" || !isTypeScriptLanguage(scope.Language) || scope.RootURI == "" || !uriWithinRoot(rootURI, scope.RootURI) ||
			provenance.Backend.Language != scope.Language || provenance.Backend.Name == "" ||
			provenance.Scope.BuildContext == "" || scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		if _, duplicate := expected[scope.ID]; duplicate {
			return model.Request{}, model.ErrInvalidProvenance
		}
		if prior, ok := backends[scope.Language]; ok && (prior.id != provenance.Backend || prior.epoch != provenance.BackendEpoch) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		backends[scope.Language] = struct {
			id    identity.BackendID
			epoch identity.BackendEpoch
		}{id: provenance.Backend, epoch: provenance.BackendEpoch}
		expected[scope.ID] = provenance
	}
	if err := verifyPinnedToolFiles(ctx, tools); err != nil {
		return model.Request{}, err
	}

	currentScopes, err := DiscoverScopes(ctx, view, rootURI)
	if err != nil {
		return model.Request{}, err
	}
	if len(currentScopes) != len(expected) {
		return model.Request{}, model.ErrInvalidProvenance
	}
	for _, currentScope := range currentScopes {
		prior, ok := expected[currentScope.ID]
		if !ok {
			return model.Request{}, model.ErrInvalidProvenance
		}
		attestedScope := cloneScope(prior.Scope)
		attestedScope.BuildContext = ""
		if !reflect.DeepEqual(currentScope, attestedScope) {
			return model.Request{}, model.ErrInvalidProvenance
		}
	}

	buildCurrentScopes := func(ctx context.Context, _ model.WorkspaceView, _ string) ([]model.Scope, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return cloneScopes(currentScopes), nil
	}
	planner := NewSemanticIndexProvider(SemanticIndexConfig{
		BuildScopes: buildCurrentScopes, Tools: tools,
		Extractor: first.Extractor, ExtractorVersion: first.ExtractorVer, Toolchain: first.Toolchain,
		BackendState: func(language string) (identity.BackendID, identity.BackendEpoch) {
			backend, ok := backends[language]
			if !ok {
				return identity.BackendID{}, 0
			}
			return backend.id, backend.epoch
		},
	})
	request, err := planner.BuildIndexRequest(ctx, view, rootURI)
	if err != nil {
		return model.Request{}, err
	}
	if len(request.Provenance) != len(expected) {
		return model.Request{}, model.ErrInvalidProvenance
	}
	for scopeID, prior := range expected {
		current, ok := request.Provenance[scopeID]
		if !ok {
			return model.Request{}, model.ErrInvalidProvenance
		}
		prior.Identity = currentIdentity
		// Tool order is not semantic identity; sameToolSet above validates the
		// exact attested tuple while allowing a stable set with another ordering.
		prior.Tools = current.Tools
		if !reflect.DeepEqual(current, prior) {
			return model.Request{}, model.ErrInvalidProvenance
		}
	}
	return request, nil
}

func sameStableWorkspaceIdentity(a, b model.Identity) bool {
	return a.Workspace != "" && a.Workspace == b.Workspace && a.DiskDigest != "" && a.DiskDigest == b.DiskDigest &&
		a.Repository == b.Repository && a.Revision == b.Revision
}

func uriWithinRoot(rootURI, candidateURI string) bool {
	root, err := url.Parse(rootURI)
	if err != nil {
		return false
	}
	candidate, err := url.Parse(candidateURI)
	if err != nil || root.Scheme != candidate.Scheme || root.Host != candidate.Host || root.RawQuery != "" || root.Fragment != "" || candidate.RawQuery != "" || candidate.Fragment != "" {
		return false
	}
	rootPath, candidatePath := path.Clean(root.Path), path.Clean(candidate.Path)
	if rootPath == candidatePath {
		return true
	}
	if rootPath == "/" {
		return strings.HasPrefix(candidatePath, "/")
	}
	return strings.HasPrefix(candidatePath, strings.TrimSuffix(rootPath, "/")+"/")
}

// scopeConfigURI derives a project config's captured URI from its declared
// project root and BuildInputs. The root is the config directory for scopes
// discovered from tsconfig/jsconfig files; keeping this mapping URI-based is
// important when the immutable view is materialized somewhere else.
func scopeConfigURI(scope model.Scope) (string, error) {
	value := strings.TrimSpace(directBuildOption(scope.Build.Options, "typescriptconfig", "tsconfig", "jsconfig", "projectconfig"))
	if value == "" {
		if scope.Language == "javascript" || scope.Language == "javascriptreact" {
			value = "jsconfig.json"
		} else {
			value = "tsconfig.json"
		}
	}
	if filepath.IsAbs(value) || filepath.VolumeName(value) != "" || strings.Contains(value, "://") {
		return "", fmt.Errorf("project config path is not relative to scope root: %q", value)
	}
	root, err := workspaceuri.Parse(scope.RootURI)
	if err != nil {
		return "", err
	}
	rootPath, err := root.Path()
	if err != nil {
		return "", err
	}
	configPath := filepath.Clean(filepath.Join(rootPath, filepath.FromSlash(value)))
	relative, err := filepath.Rel(rootPath, configPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("project config path escapes scope root: %q", value)
	}
	configURI := workspaceuri.FromPath(configPath).Canonical()
	if !uriWithinRoot(scope.RootURI, configURI) {
		return "", fmt.Errorf("project config URI escapes scope root: %q", value)
	}
	return configURI, nil
}

func projectReferencePaths(scope model.Scope) ([]string, error) {
	raw := directBuildOption(scope.Build.Options, "projectreferences")
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("project references are absent from BuildInputs")
	}
	var references []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &references); err != nil {
		return nil, fmt.Errorf("project references are invalid JSON: %w", err)
	}
	paths := make([]string, 0, len(references))
	for _, reference := range references {
		var value string
		if err := json.Unmarshal(reference["path"], &value); err != nil || strings.TrimSpace(value) == "" {
			return nil, errors.New("project reference has no valid path")
		}
		paths = append(paths, value)
	}
	return paths, nil
}

func scopeHasProjectReferences(scope model.Scope) bool {
	paths, err := projectReferencePaths(scope)
	return err != nil || len(paths) != 0
}

func resolveProjectReferenceConfigURI(scope model.Scope, reference, workspaceRootURI string) (string, error) {
	if strings.TrimSpace(reference) == "" || filepath.IsAbs(reference) || filepath.VolumeName(reference) != "" || strings.Contains(reference, "://") {
		return "", fmt.Errorf("project reference path is not relative: %q", reference)
	}
	configURI, err := scopeConfigURI(scope)
	if err != nil {
		return "", err
	}
	parsed, err := workspaceuri.Parse(configURI)
	if err != nil {
		return "", err
	}
	configPath, err := parsed.Path()
	if err != nil {
		return "", err
	}
	projectPath := filepath.Clean(filepath.Join(filepath.Dir(configPath), filepath.FromSlash(reference)))
	if filepath.Ext(projectPath) != ".json" {
		projectPath = filepath.Join(projectPath, "tsconfig.json")
	}
	resolvedURI := workspaceuri.FromPath(projectPath).Canonical()
	if !uriWithinRoot(workspaceRootURI, resolvedURI) {
		return "", fmt.Errorf("project reference %q resolves outside the immutable workspace boundary", reference)
	}
	return resolvedURI, nil
}

// referencedProjectScopeClosure resolves all direct and transitive project
// references to discovered scopes. A reference missing from the explicit
// scope set cannot be guessed from the host filesystem and therefore fails
// the closure proof.
func referencedProjectScopeClosure(scope model.Scope, scopes []model.Scope, workspaceRootURI string) ([]model.Scope, error) {
	paths, err := projectReferencePaths(scope)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, nil
	}
	if workspaceRootURI == "" {
		return nil, errors.New("explicit immutable workspace boundary is required")
	}
	if !uriWithinRoot(workspaceRootURI, scope.RootURI) {
		return nil, errors.New("project root lies outside the immutable workspace boundary")
	}
	byConfig := make(map[string]model.Scope, len(scopes))
	for _, candidate := range scopes {
		if !uriWithinRoot(workspaceRootURI, candidate.RootURI) {
			return nil, fmt.Errorf("project root %q lies outside the immutable workspace boundary", candidate.RootURI)
		}
		configURI, configErr := scopeConfigURI(candidate)
		if configErr != nil {
			return nil, fmt.Errorf("scope %q config: %w", candidate.ID, configErr)
		}
		if _, exists := byConfig[configURI]; exists {
			return nil, fmt.Errorf("multiple scopes describe project config %q", configURI)
		}
		byConfig[configURI] = candidate
	}
	currentConfig, err := scopeConfigURI(scope)
	if err != nil {
		return nil, err
	}
	if current, ok := byConfig[currentConfig]; !ok || current.ID != scope.ID || current.BuildContext != scope.BuildContext {
		return nil, errors.New("current project config is not represented exactly once in the captured scope set")
	}
	state := make(map[string]uint8, len(scopes))
	closure := make([]model.Scope, 0, len(scopes))
	closureIDs := make(map[string]struct{}, len(scopes))
	var visit func(model.Scope) error
	visit = func(current model.Scope) error {
		configURI, configErr := scopeConfigURI(current)
		if configErr != nil {
			return configErr
		}
		switch state[configURI] {
		case 1:
			return fmt.Errorf("project reference cycle reaches %q", configURI)
		case 2:
			return nil
		}
		state[configURI] = 1
		currentRefs, refsErr := projectReferencePaths(current)
		if refsErr != nil {
			return fmt.Errorf("scope %q: %w", current.ID, refsErr)
		}
		for _, reference := range currentRefs {
			targetURI, resolveErr := resolveProjectReferenceConfigURI(current, reference, workspaceRootURI)
			if resolveErr != nil {
				return resolveErr
			}
			target, found := byConfig[targetURI]
			if !found {
				return fmt.Errorf("referenced project config %q is not in the captured scope set", targetURI)
			}
			if err := visit(target); err != nil {
				return err
			}
			if target.ID != scope.ID {
				if _, exists := closureIDs[target.ID]; exists {
					continue
				}
				closureIDs[target.ID] = struct{}{}
				closure = append(closure, cloneScope(target))
			}
		}
		state[configURI] = 2
		return nil
	}
	if err := visit(scope); err != nil {
		return nil, err
	}
	return closure, nil
}

func mergeSemanticFiles(existing, incoming []model.File) ([]model.File, error) {
	byURI := make(map[string]model.File, len(existing)+len(incoming))
	for _, file := range append(append([]model.File(nil), existing...), incoming...) {
		if file.URI == "" {
			return nil, errors.New("project file manifest contains an empty URI")
		}
		if prior, ok := byURI[file.URI]; ok {
			if !reflect.DeepEqual(prior, file) {
				return nil, fmt.Errorf("captured file metadata differs for %q", file.URI)
			}
			continue
		}
		byURI[file.URI] = file
	}
	merged := make([]model.File, 0, len(byURI))
	for _, file := range byURI {
		merged = append(merged, file)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].URI < merged[j].URI })
	return merged, nil
}

func cloneSemanticFileMap(files map[string][]model.File) map[string][]model.File {
	if files == nil {
		return nil
	}
	cloned := make(map[string][]model.File, len(files))
	for scopeID, values := range files {
		clonedValues := append([]model.File(nil), values...)
		for i := range clonedValues {
			clonedValues[i].SourceMap = append([]model.SourceMapSpan(nil), values[i].SourceMap...)
		}
		cloned[scopeID] = clonedValues
	}
	return cloned
}

func cloneScope(scope model.Scope) model.Scope {
	scope.Build.Environment = cloneStringMap(scope.Build.Environment)
	scope.Build.Arguments = append([]string(nil), scope.Build.Arguments...)
	scope.Build.PackagePatterns = append([]string(nil), scope.Build.PackagePatterns...)
	scope.Build.IncludePaths = append([]string(nil), scope.Build.IncludePaths...)
	scope.Build.Defines = cloneStringMap(scope.Build.Defines)
	scope.Build.Features = append([]string(nil), scope.Build.Features...)
	scope.Build.Options = cloneStringMap(scope.Build.Options)
	return scope
}

func cloneScopes(scopes []model.Scope) []model.Scope {
	cloned := make([]model.Scope, len(scopes))
	for i, scope := range scopes {
		cloned[i] = cloneScope(scope)
	}
	return cloned
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func (p *SemanticIndexProvider) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (report model.Report, retErr error) {
	if request.View == nil {
		return model.Report{}, model.ErrMissingView
	}
	if sink == nil {
		return model.Report{}, errors.New("typescript semantic index: nil sink")
	}
	report = model.Report{Identity: request.View.Identity(), UsedTools: make(map[string][]model.ToolIdentity)}
	seenScopes := make(map[string]struct{}, len(request.Scopes))
	for _, scope := range request.Scopes {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if scope.ID == "" || !isTypeScriptLanguage(scope.Language) || scope.RootURI == "" || scope.BuildContext == "" {
			return report, model.ErrInvalidScope
		}
		if _, ok := seenScopes[scope.ID]; ok {
			return report, model.ErrInvalidScope
		}
		seenScopes[scope.ID] = struct{}{}
		if request.WorkspaceRootURI != "" && !uriWithinRoot(request.WorkspaceRootURI, scope.RootURI) {
			return report, model.ErrInvalidScope
		}
		provenance, ok := request.Provenance[scope.ID]
		if !ok || provenance.SchemaVersion != model.SchemaVersion || provenance.Identity != report.Identity ||
			!reflect.DeepEqual(provenance.Scope, scope) || provenance.Backend.Language != scope.Language || provenance.Backend.Name == "" ||
			provenance.Extractor == "" || provenance.ExtractorVer == "" || provenance.Toolchain == "" ||
			p == nil || !sameToolSet(p.config.Tools, provenance.Tools) ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return report, model.ErrInvalidProvenance
		}
	}

	for _, scope := range request.Scopes {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		provenance := request.Provenance[scope.ID]
		tools, configured := pinnedTools(provenance.Tools)
		if !configured {
			appendUnavailable(&report, scope.ID, "pinned Node, TypeScript compiler, and direct exporter identities are required")
			continue
		}
		if !deterministicTypeScriptBuild(scope) {
			appendUnavailable(&report, scope.ID, "project configuration and effective compiler options are not deterministically recorded in BuildInputs")
			continue
		}
		if hasUntrustedPlugins(scope) {
			appendUnavailable(&report, scope.ID, "workspace TypeScript plugins require an explicit trust record before the indexer can load them")
			continue
		}
		if p == nil || p.config.Runner == nil {
			appendUnavailable(&report, scope.ID, "no TypeScript/SCIP runner is configured")
			continue
		}
		materializer, ok := request.View.(model.WorkspaceMaterializer)
		if !ok {
			appendUnavailable(&report, scope.ID, "immutable workspace view cannot be materialized for an external semantic tool")
			continue
		}
		referenceScopes, referenceErr := referencedProjectScopeClosure(scope, request.Scopes, request.WorkspaceRootURI)
		if referenceErr != nil {
			if request.WorkspaceRootURI == "" && scopeHasProjectReferences(scope) {
				appendUnavailable(&report, scope.ID, "TypeScript project references require an explicit immutable WorkspaceRootURI")
			} else {
				appendUnknown(&report, scope.ID, "TypeScript project-reference closure is incomplete: "+referenceErr.Error())
			}
			continue
		}
		files, err := walkScopeFiles(ctx, request.View, scope.RootURI)
		if err != nil {
			return report, err
		}
		materializationRoot := scope.RootURI
		materializationFiles := files
		referenceFiles := make(map[string][]model.File, len(referenceScopes))
		needsWorkspaceMaterialization := len(referenceScopes) != 0 || typescriptConfigClosureNeedsWorkspaceMaterialization(scope)
		if needsWorkspaceMaterialization {
			if request.WorkspaceRootURI == "" {
				appendUnknown(&report, scope.ID, "TypeScript project config closure outside the project root requires an explicit immutable WorkspaceRootURI")
				continue
			}
			materializationRoot = request.WorkspaceRootURI
			materializationFiles, err = walkScopeFiles(ctx, request.View, materializationRoot)
			if err != nil {
				return report, err
			}
		}
		if len(referenceScopes) != 0 {
			for _, referenceScope := range referenceScopes {
				projectFiles, walkErr := walkScopeFiles(ctx, request.View, referenceScope.RootURI)
				if walkErr != nil {
					return report, walkErr
				}
				referenceFiles[referenceScope.ID] = append([]model.File(nil), projectFiles...)
				var merged []model.File
				merged, err = mergeSemanticFiles(materializationFiles, projectFiles)
				if err != nil {
					appendUnknown(&report, scope.ID, "TypeScript project-reference file closure conflicts: "+err.Error())
					break
				}
				materializationFiles = merged
			}
			if coverageRecorded(report.Coverage, scope.ID) {
				continue
			}
		}
		materialized, cleanup, err := materialize(ctx, materializer, materializationRoot, materializationFiles, "omnilsp-typescript-index-")
		if err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			appendUnavailable(&report, scope.ID, "immutable snapshot materialization failed: "+err.Error())
			continue
		}
		runRequest := SCIPRequest{
			Scope: cloneScope(scope), ProjectScopes: cloneScopes(request.Scopes), ReferenceScopes: cloneScopes(referenceScopes),
			WorkspaceRootURI: request.WorkspaceRootURI, Materialized: materialized,
			Files: append([]model.File(nil), files...), ReferenceFiles: cloneSemanticFileMap(referenceFiles), Tools: tools,
		}
		verifyErr := verifyPinnedToolFiles(ctx, tools)
		var actualTools []model.ToolIdentity
		if verifyErr == nil {
			actualTools, verifyErr = p.config.Runner.VerifyTools(ctx, runRequest)
		}
		if verifyErr == nil && !sameToolSet(tools, actualTools) {
			verifyErr = errors.New("runner tool identities differ from the pinned request")
		}
		if verifyErr != nil {
			cleanupErr := cleanup()
			if ctx.Err() != nil {
				return report, errors.Join(ctx.Err(), cleanupErr)
			}
			appendUnavailable(&report, scope.ID, "pinned TypeScript/SCIP tool verification failed: "+verifyErr.Error())
			if cleanupErr != nil {
				return report, errors.Join(verifyErr, cleanupErr)
			}
			continue
		}
		report.UsedTools[scope.ID] = append([]model.ToolIdentity(nil), actualTools...)
		fileByURI := make(map[string]model.File, len(files))
		for _, file := range files {
			fileByURI[file.URI] = file
		}
		emit := func(batch SCIPBatch) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return writeBatch(ctx, sink, scope, fileByURI, batch)
		}
		result, exportErr := p.config.Runner.ExportSCIP(ctx, runRequest, emit)
		cleanupErr := cleanup()
		if ctx.Err() != nil {
			return report, errors.Join(ctx.Err(), exportErr, cleanupErr)
		}
		if exportErr != nil || cleanupErr != nil {
			joined := errors.Join(exportErr, cleanupErr)
			appendUnavailable(&report, scope.ID, "TypeScript/SCIP export failed: "+joined.Error())
			report.Errors = append(report.Errors, joined.Error())
			return report, joined
		}
		_, directCompiler := p.config.Runner.(*DirectCompilerRunner)
		if err := appendCoverage(&report, scope.ID, result.Coverage, files, scope.Language, directCompiler); err != nil {
			report.Errors = append(report.Errors, err.Error())
			return report, err
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

func deterministicTypeScriptBuild(scope model.Scope) bool {
	project, configDigest, compilerOptions, projectReferences, plugins := false, false, false, false, false
	for key, value := range scope.Build.Options {
		normalized := normalizeBuildOption(key)
		if (normalized == "typescriptconfig" || normalized == "tsconfig" || normalized == "jsconfig" || normalized == "projectconfig") && strings.TrimSpace(value) != "" {
			project = true
		}
		if (normalized == "projectconfigdigest" || normalized == "typescriptconfigdigest" || normalized == "tsconfigdigest" || normalized == "jsconfigdigest") && validConfigDigest(value) {
			configDigest = true
		}
		if normalized == "compileroptions" && strings.TrimSpace(value) != "" {
			compilerOptions = true
		}
		if normalized == "projectreferences" && strings.TrimSpace(value) != "" {
			projectReferences = true
		}
		if normalized == "plugins" && strings.TrimSpace(value) != "" {
			plugins = true
		}
	}
	return project && configDigest && compilerOptions && projectReferences && plugins
}

// typescriptConfigClosureNeedsWorkspaceMaterialization reports whether a
// captured extends config lies outside the project root. The compiler adapter
// validates those files from the materialized snapshot, so a project-only
// materialization cannot safely validate a parent config.
func typescriptConfigClosureNeedsWorkspaceMaterialization(scope model.Scope) bool {
	var closure compilerOptionsClosure
	if err := json.Unmarshal([]byte(directBuildOption(scope.Build.Options, "compileroptions")), &closure); err != nil ||
		closure.Schema != "omnilsp-tsconfig-v1" || !closure.Complete {
		return false
	}
	root, err := workspaceuri.Parse(scope.RootURI)
	if err != nil {
		return false
	}
	rootPath, err := root.Path()
	if err != nil {
		return false
	}
	rootAbs, err := filepath.Abs(rootPath)
	if err != nil {
		return false
	}
	for _, config := range closure.Configs {
		if config.Path == "" || filepath.IsAbs(config.Path) || filepath.VolumeName(config.Path) != "" {
			continue
		}
		candidate := filepath.Join(rootPath, filepath.FromSlash(config.Path))
		candidateAbs, absErr := filepath.Abs(candidate)
		relative, relErr := filepath.Rel(rootAbs, candidateAbs)
		if absErr != nil || relErr != nil || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return true
		}
	}
	return false
}

func validConfigDigest(value string) bool {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(strings.ToLower(value), "sha256:")
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func normalizeBuildOption(value string) string {
	value = strings.ToLower(value)
	return strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(value)
}

func hasUntrustedPlugins(scope model.Scope) bool {
	for _, feature := range scope.Build.Features {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(feature)), "plugin:") {
			return true
		}
	}
	for key, value := range scope.Build.Options {
		if strings.Contains(strings.ToLower(key), "plugin") && hasConfiguredValue(value) {
			return true
		}
		normalized := normalizeBuildOption(key)
		if (normalized == "tsconfig" || normalized == "jsconfig" || normalized == "typescriptconfig" || normalized == "projectconfig") && containsConfiguredPlugin(value) {
			return true
		}
	}
	return false
}

func containsConfiguredPlugin(raw string) bool {
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(value any) bool {
		switch current := value.(type) {
		case map[string]any:
			for key, child := range current {
				if strings.EqualFold(key, "plugin") || strings.EqualFold(key, "plugins") {
					if configuredJSONValue(child) {
						return true
					}
					continue
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range current {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func configuredJSONValue(value any) bool {
	switch current := value.(type) {
	case nil:
		return false
	case bool:
		return current
	case string:
		return hasConfiguredValue(current)
	case []any:
		return len(current) > 0
	case map[string]any:
		return len(current) > 0
	default:
		return true
	}
}

func hasConfiguredValue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "[]", "{}", "null", "none", "false":
		return false
	default:
		return true
	}
}

func isTypeScriptLanguage(language string) bool {
	switch language {
	case "typescript", "typescriptreact", "javascript", "javascriptreact":
		return true
	default:
		return false
	}
}

func pinnedTools(tools []model.ToolIdentity) ([]model.ToolIdentity, bool) {
	var hasCompiler, hasExporter bool
	for _, tool := range tools {
		if tool.Name == semanticIndexCompilerName && tool.Version == semanticIndexCompilerVersion {
			hasCompiler = true
		}
		if tool.Name == semanticIndexExporterName || tool.Name == semanticIndexDirectExporterName {
			hasExporter = true
		}
	}
	return append([]model.ToolIdentity(nil), tools...), hasCompiler && hasExporter && validToolList(tools)
}

func validToolList(tools []model.ToolIdentity) bool {
	if len(tools) < 2 {
		return false
	}
	if !validToolMetadata(tools) {
		return false
	}
	for _, tool := range tools {
		if !fixedToolVersion(tool.Version) {
			return false
		}
	}
	return true
}

func validToolMetadata(tools []model.ToolIdentity) bool {
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if tool.Name == "" || tool.Version == "" || !filepath.IsAbs(tool.Path) || len(tool.SHA256) != 64 {
			return false
		}
		if _, err := hex.DecodeString(tool.SHA256); err != nil {
			return false
		}
		key := tool.Name + "\x00" + tool.Path
		if _, ok := seen[key]; ok {
			return false
		}
		seen[key] = struct{}{}
	}
	return true
}

func fixedToolVersion(version string) bool {
	version = strings.TrimSpace(version)
	lower := strings.ToLower(version)
	if version == "" || lower == "latest" || lower == "next" || lower == "stable" ||
		strings.ContainsAny(version, "*^~") || strings.Contains(version, "||") || strings.Contains(version, ",") ||
		strings.ContainsAny(version, "<>= \t\r\n") {
		return false
	}
	return true
}

func verifyPinnedToolFiles(ctx context.Context, tools []model.ToolIdentity) error {
	for _, tool := range tools {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := os.Open(tool.Path)
		if err != nil {
			return fmt.Errorf("open pinned tool %s: %w", tool.Name, err)
		}
		hash := sha256.New()
		buffer := make([]byte, 32*1024)
		for {
			if err := ctx.Err(); err != nil {
				file.Close()
				return err
			}
			n, readErr := file.Read(buffer)
			if n > 0 {
				_, _ = hash.Write(buffer[:n])
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				file.Close()
				return fmt.Errorf("read pinned tool %s: %w", tool.Name, readErr)
			}
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close pinned tool %s: %w", tool.Name, err)
		}
		if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), tool.SHA256) {
			return fmt.Errorf("pinned tool %s SHA-256 mismatch", tool.Name)
		}
	}
	return nil
}

func sameToolSet(expected, actual []model.ToolIdentity) bool {
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

func walkScopeFiles(ctx context.Context, view model.WorkspaceView, rootURI string) ([]model.File, error) {
	files := make([]model.File, 0, 32)
	seen := make(map[string]struct{})
	err := view.Walk(ctx, rootURI, func(file model.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.URI == "" || !uriWithinRoot(rootURI, file.URI) {
			return fmt.Errorf("workspace view returned a file outside semantic scope %q", rootURI)
		}
		if _, exists := seen[file.URI]; exists {
			return fmt.Errorf("workspace view returned duplicate URI %q", file.URI)
		}
		seen[file.URI] = struct{}{}
		files = append(files, file)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, ctx.Err()
}

func materialize(ctx context.Context, source model.WorkspaceMaterializer, rootURI string, files []model.File, prefix string) (model.MaterializedView, func() error, error) {
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		return nil, nil, fmt.Errorf("create temporary root: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	if err := ctx.Err(); err != nil {
		_ = cleanup()
		return nil, nil, err
	}
	view, err := source.Materialize(ctx, rootURI, dir)
	if view != nil {
		cleanup = func() error {
			return errors.Join(view.Close(), os.RemoveAll(dir))
		}
	}
	if err != nil {
		_ = cleanup()
		return nil, nil, fmt.Errorf("materialize captured view: %w", err)
	}
	if view == nil || view.RootURI() != rootURI || view.RootPath() == "" {
		_ = cleanup()
		return nil, nil, errors.New("materializer returned an invalid root")
	}
	rootPath, err := filepath.Abs(view.RootPath())
	if err != nil {
		_ = cleanup()
		return nil, nil, err
	}
	info, err := os.Stat(rootPath)
	if err != nil || !info.IsDir() {
		_ = cleanup()
		if err == nil {
			err = errors.New("materialized root is not a directory")
		}
		return nil, nil, err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			_ = cleanup()
			return nil, nil, err
		}
		mapped, err := view.PathForURI(file.URI)
		if err != nil || !withinRoot(rootPath, mapped) {
			_ = cleanup()
			if err == nil {
				err = errors.New("mapped file escapes materialized root")
			}
			return nil, nil, fmt.Errorf("materialized URI %q: %w", file.URI, err)
		}
	}
	return view, cleanup, nil
}

func withinRoot(root, path string) bool {
	if path == "" {
		return false
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return false
	}
	pathReal, err := filepath.EvalSymlinks(pathAbs)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootReal, pathReal)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func writeBatch(ctx context.Context, sink model.Sink, scope model.Scope, files map[string]model.File, batch SCIPBatch) error {
	if len(batch.Symbols) > semanticIndexBatchLimit || len(batch.Occurrences) > semanticIndexBatchLimit || len(batch.Edges) > semanticIndexBatchLimit {
		return fmt.Errorf("TypeScript/SCIP runner exceeded bounded batch size %d", semanticIndexBatchLimit)
	}
	for _, symbol := range batch.Symbols {
		if symbol.ID == "" || symbol.Name == "" || (symbol.ScopeID != "" && symbol.ScopeID != scope.ID) {
			return errors.New("TypeScript/SCIP runner emitted an invalid symbol")
		}
	}
	if len(batch.Symbols) != 0 {
		out := append([]model.Symbol(nil), batch.Symbols...)
		for i := range out {
			out[i].ScopeID = scope.ID
		}
		if err := sink.WriteSymbols(ctx, out); err != nil {
			return err
		}
	}
	if len(batch.Occurrences) != 0 {
		out := make([]model.Occurrence, 0, len(batch.Occurrences))
		for _, occurrence := range batch.Occurrences {
			if occurrence.SymbolID == "" || occurrence.URI == "" || occurrence.Role == "" ||
				(occurrence.ScopeID != "" && occurrence.ScopeID != scope.ID) || !validRange(occurrence.Range) {
				return errors.New("TypeScript/SCIP runner emitted an invalid occurrence")
			}
			mappedURI, mappedRange := remapGeneratedRange(files, occurrence.URI, occurrence.Range)
			file, ok := files[mappedURI]
			if !ok {
				return fmt.Errorf("TypeScript/SCIP occurrence points outside the immutable scope: %q", mappedURI)
			}
			out = append(out, model.Occurrence{
				SymbolID: occurrence.SymbolID, ScopeID: scope.ID,
				URI: mappedURI, Range: mappedRange, Role: occurrence.Role,
				SourceHash: file.SHA256, BuildContext: scope.BuildContext,
			})
		}
		if err := sink.WriteOccurrences(ctx, out); err != nil {
			return err
		}
	}
	if len(batch.Edges) != 0 {
		out := make([]model.Edge, 0, len(batch.Edges))
		for _, edge := range batch.Edges {
			if edge.From == "" || edge.To == "" || edge.SourceURI == "" || edge.Kind == "" ||
				(edge.ScopeID != "" && edge.ScopeID != scope.ID) || !validRange(edge.Range) {
				return errors.New("TypeScript/SCIP runner emitted an invalid edge")
			}
			mappedURI, mappedRange := remapGeneratedRange(files, edge.SourceURI, edge.Range)
			file, ok := files[mappedURI]
			if !ok {
				return fmt.Errorf("TypeScript/SCIP edge points outside the immutable scope: %q", mappedURI)
			}
			out = append(out, model.Edge{
				From: edge.From, To: edge.To,
				ScopeID: scope.ID, Kind: edge.Kind, SourceURI: mappedURI, Range: mappedRange,
				SourceHash: file.SHA256, BuildContext: scope.BuildContext,
			})
		}
		if err := sink.WriteEdges(ctx, out); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func validRange(r model.Position) bool {
	return r.StartLine < r.EndLine || (r.StartLine == r.EndLine && r.StartChar <= r.EndChar)
}

func remapGeneratedRange(files map[string]model.File, uri string, source model.Position) (string, model.Position) {
	file, ok := files[uri]
	if !ok || !file.Generated {
		return uri, source
	}
	for _, span := range file.SourceMap {
		if span.Generated == source && validRange(span.Source) {
			if _, ok := files[span.SourceURI]; ok {
				return span.SourceURI, span.Source
			}
		}
	}
	return uri, source
}

func appendUnavailable(report *model.Report, scopeID, reason string) {
	for _, fact := range model.RequiredFactKinds {
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scopeID, Fact: fact, State: model.Unavailable, Reason: reason})
	}
}

func appendUnknown(report *model.Report, scopeID, reason string) {
	for _, fact := range model.RequiredFactKinds {
		state := model.Unknown
		factReason := reason
		if fact == model.FactInclude {
			state = model.Unavailable
			factReason = "preprocessor include relationships do not apply to TypeScript"
		}
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scopeID, Fact: fact, State: state, Reason: factReason})
	}
}

func coverageRecorded(coverage []model.Coverage, scopeID string) bool {
	for _, item := range coverage {
		if item.ScopeID == scopeID {
			return true
		}
	}
	return false
}

func appendCoverage(report *model.Report, scopeID string, raw []model.Coverage, files []model.File, language string, directCompiler bool) error {
	coverageByFact := make(map[model.FactKind]model.Coverage, len(raw))
	for _, item := range raw {
		if item.ScopeID != "" && item.ScopeID != scopeID {
			return fmt.Errorf("TypeScript/SCIP runner returned coverage for unexpected scope %q", item.ScopeID)
		}
		if item.ScopeID == "" {
			item.ScopeID = scopeID
		}
		if _, exists := coverageByFact[item.Fact]; exists {
			return fmt.Errorf("TypeScript/SCIP runner returned duplicate coverage for %q", item.Fact)
		}
		if !validCoverageState(item.State) {
			return fmt.Errorf("TypeScript/SCIP runner returned invalid coverage state %q", item.State)
		}
		if !requiredFact(item.Fact) {
			return fmt.Errorf("TypeScript/SCIP runner returned unsupported fact %q", item.Fact)
		}
		coverageByFact[item.Fact] = item
	}
	hasGenerated, hasUnmappedGenerated := false, false
	for _, file := range files {
		if file.Generated {
			hasGenerated = true
			if !hasUsableSourceMap(file, files) {
				hasUnmappedGenerated = true
			}
		}
	}
	for _, fact := range model.RequiredFactKinds {
		item, ok := coverageByFact[fact]
		if !ok {
			item = model.Coverage{ScopeID: scopeID, Fact: fact, State: model.Unknown, Reason: "TypeScript/SCIP runner did not declare coverage for this fact"}
		}
		item.ScopeID = scopeID
		if fact == model.FactInclude {
			item.State, item.Reason = model.Unavailable, "preprocessor include relationships do not apply to TypeScript"
		}
		if item.State == model.Complete {
			if !directJavaScriptCheckJSProofApplies(item.Reason, fact, language, files, directCompiler) {
				if reason := typescriptSubsetReason(fact, language, files, directCompiler, item.Reason); reason != "" {
					item.State, item.Reason = model.IncompleteKnownSubset, reason
				}
			}
			if fact == model.FactGenerated && hasGenerated {
				item.State = model.IncompleteKnownSubset
				if hasUnmappedGenerated {
					item.Reason = "one or more generated TypeScript files lack usable range-level source-map data"
				} else {
					item.Reason = "range-level source-map data has no exhaustive-coverage attestation"
				}
			}
		}
		if item.State != model.Complete && item.Reason == "" {
			item.Reason = "TypeScript/SCIP runner reported incomplete coverage"
		}
		report.Coverage = append(report.Coverage, item)
	}
	return nil
}

func directJavaScriptCheckJSProofApplies(reason string, fact model.FactKind, language string, files []model.File, directCompiler bool) bool {
	// tsconfig projects can contain only checked JavaScript. Trust that source
	// profile from the direct compiler marker and file closure, not the config's
	// conventional language label.
	if !directCompiler || (language != "javascript" && language != "typescript") ||
		(fact != model.FactSymbol && fact != model.FactDeclaration && fact != model.FactDefinition && fact != model.FactReference) {
		return false
	}
	if !strings.HasPrefix(reason, directJavaScriptCheckJSProof+";") ||
		!strings.Contains(reason, ";typescript="+semanticIndexCompilerVersion+";") ||
		!strings.Contains(reason, ";allowJs=true;") || !strings.Contains(reason, ";checkJs=true;") ||
		!strings.Contains(reason, ";closedScopes=") || !strings.Contains(reason, ";sourceFiles=") {
		return false
	}
	hasJavaScriptSource := false
	for _, file := range files {
		uri := strings.ToLower(file.URI)
		languageID := strings.ToLower(file.LanguageID)
		switch path.Ext(uri) {
		case ".js", ".mjs", ".cjs":
			hasJavaScriptSource = true
		case ".jsx", ".tsx", ".ts", ".mts", ".cts":
			return false
		}
		if strings.Contains(languageID, "typescript") || strings.Contains(languageID, "react") {
			return false
		}
	}
	return hasJavaScriptSource
}

func validCoverageState(state model.Completeness) bool {
	return state == model.Complete || state == model.IncompleteKnownSubset || state == model.Unknown || state == model.Unavailable
}

func requiredFact(fact model.FactKind) bool {
	for _, required := range model.RequiredFactKinds {
		if fact == required {
			return true
		}
	}
	return false
}

func typescriptSubsetReason(fact model.FactKind, language string, files []model.File, directCompiler bool, reason string) string {
	jsSemantics := language == "javascript" || language == "javascriptreact"
	for _, file := range files {
		uri := strings.ToLower(file.URI)
		if strings.HasSuffix(uri, ".js") || strings.HasSuffix(uri, ".jsx") ||
			strings.HasSuffix(uri, ".mjs") || strings.HasSuffix(uri, ".cjs") ||
			strings.Contains(strings.ToLower(file.LanguageID), "javascript") {
			jsSemantics = true
		}
	}
	switch fact {
	case model.FactReference:
		if !directCompiler || jsSemantics {
			return "reflection, computed properties, metaprogramming, and unresolved JavaScript references prevent a complete static reference set"
		}
	case model.FactCall:
	case model.FactImport, model.FactModule:
		if !directTypeScriptStaticModuleProofApplies(reason, fact, language, files, directCompiler) {
			return "dynamic import(), require(), and runtime module loading cannot be enumerated from static imports alone"
		}
	case model.FactImplementation:
		if jsSemantics {
			return "JavaScript prototype mutation and runtime-generated classes can add implementation relations"
		}
	case model.FactTypeRelation:
		if jsSemantics {
			return "JavaScript prototype mutation and runtime-generated types can add type relations"
		}
	case model.FactSymbol, model.FactDeclaration, model.FactDefinition:
		if jsSemantics {
			return "JavaScript eval, proxies, and runtime property creation can introduce symbols outside the static compiler model"
		}
	default:
		return ""
	}
	if fact == model.FactCall {
		return "dynamic dispatch and reflective call construction can hide call targets"
	}
	return ""
}

func directTypeScriptStaticModuleProofApplies(reason string, fact model.FactKind, language string, files []model.File, directCompiler bool) bool {
	if !directCompiler || language != "typescript" ||
		(fact != model.FactImport && fact != model.FactModule) ||
		!strings.HasPrefix(reason, directTypeScriptStaticModuleProof+";") ||
		!strings.Contains(reason, ";typescript="+semanticIndexCompilerVersion+";") ||
		!strings.Contains(reason, ";closedScopes=1;") ||
		!strings.Contains(reason, ";projectReferences=0;") ||
		!strings.Contains(reason, ";sourceFiles=") {
		return false
	}
	for _, file := range files {
		if file.Generated {
			return false
		}
		extension := strings.ToLower(path.Ext(file.URI))
		languageID := strings.ToLower(file.LanguageID)
		if extension == ".js" || extension == ".jsx" || extension == ".mjs" || extension == ".cjs" || extension == ".tsx" ||
			strings.Contains(languageID, "javascript") || strings.Contains(languageID, "react") {
			return false
		}
	}
	return true
}

func hasUsableSourceMap(file model.File, files []model.File) bool {
	if !file.Generated || len(file.SourceMap) == 0 {
		return !file.Generated
	}
	fileURIs := make(map[string]struct{}, len(files))
	for _, candidate := range files {
		fileURIs[candidate.URI] = struct{}{}
	}
	for _, span := range file.SourceMap {
		if span.SourceURI == "" || !validRange(span.Generated) || !validRange(span.Source) {
			return false
		}
		if _, ok := fileURIs[span.SourceURI]; !ok {
			return false
		}
	}
	return true
}
