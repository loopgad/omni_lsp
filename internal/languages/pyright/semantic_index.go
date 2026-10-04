package pyright

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
)

const (
	semanticIndexCompilerName     = "pyright"
	semanticIndexCompilerVersion  = "1.1.414"
	semanticIndexVendorName       = "pyright-vendor"
	semanticIndexRuntimeName      = "node"
	semanticIndexRuntimeVersion   = "24.14.0"
	semanticIndexExporterName     = "omnilsp-pyright-exporter"
	semanticIndexBatchLimit       = 256
	semanticIndexExtractor        = "omnilsp-pyright-analyzer"
	semanticIndexExtractorVersion = "3"
	semanticIndexToolchain        = "pyright/1.1.414+node/24.14.0"
)

// PyrightSemanticBatch is one bounded batch of facts emitted from Pyright's
// analyzed program. Symbol IDs are stable declaration anchors derived from the
// compiler's resolved declaration, never from source spelling alone.
type PyrightSemanticBatch struct {
	Symbols     []model.Symbol
	Occurrences []model.Occurrence
	Edges       []model.Edge
}

// PyrightSemanticRequest gives the runner only pinned tools and an immutable,
// read-only materialized snapshot. Exporters must use Pyright's analyzed
// semantic model; source-text guessing and document-symbol approximations are
// not acceptable. Temporary files and outputs must be written outside RootPath.
type PyrightSemanticRequest struct {
	Scope         model.Scope
	ProjectScopes []model.Scope
	ProjectFiles  map[string][]model.File
	Files         []model.File
	Materialized  model.MaterializedView
	Tools         []model.ToolIdentity
}

type PyrightSemanticResult struct {
	Coverage []model.Coverage
}

// ScopeBuilder discovers each Python project and its deterministic analysis
// inputs from the immutable view. It must not inspect the mutable filesystem.
type ScopeBuilder func(ctx context.Context, view model.WorkspaceView, rootURI string) ([]model.Scope, error)

// SemanticIndexConfig carries project discovery and exact tool pins.
type SemanticIndexConfig struct {
	Runner           PyrightSemanticRunner
	BuildScopes      ScopeBuilder
	Tools            []model.ToolIdentity
	Extractor        string
	ExtractorVersion string
	Toolchain        string
	Backend          identity.BackendID
	BackendEpoch     identity.BackendEpoch
}

// PyrightSemanticRunner attests to the pinned tools it will actually use,
// streams bounded compiler-derived facts, and declares per-fact coverage.
// Unresolved facts must be omitted and their coverage kept incomplete or
// unknown.
type PyrightSemanticRunner interface {
	VerifyTools(ctx context.Context, request PyrightSemanticRequest) ([]model.ToolIdentity, error)
	// Export emits coordinates as zero-based UTF-16 code-unit positions.
	Export(ctx context.Context, request PyrightSemanticRequest, emit func(PyrightSemanticBatch) error) (PyrightSemanticResult, error)
}

// SemanticIndexProvider exports Python facts through Pyright's semantic model.
type SemanticIndexProvider struct {
	config SemanticIndexConfig
}

var _ languages.SemanticIndexProvider = (*SemanticIndexProvider)(nil)

var _ languages.SemanticIndexRequestBuilder = (*SemanticIndexProvider)(nil)

func NewSemanticIndexProvider(config SemanticIndexConfig) *SemanticIndexProvider {
	config.Tools = append([]model.ToolIdentity(nil), config.Tools...)
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

// NewPinnedSemanticIndexProvider wires the production analyzer to the tool
// identities supplied by the application. The Python interpreter and Node
// executable are selected only from that pinned set.
func NewPinnedSemanticIndexProvider(tools []model.ToolIdentity) *SemanticIndexProvider {
	nodePath, pythonPath := "", ""
	for _, tool := range tools {
		switch tool.Name {
		case semanticIndexRuntimeName:
			nodePath = tool.Path
		case "python":
			pythonPath = tool.Path
		}
	}
	return NewSemanticIndexProvider(SemanticIndexConfig{
		Runner: NewDirectPyrightRunner(nodePath),
		BuildScopes: func(ctx context.Context, view model.WorkspaceView, rootURI string) ([]model.Scope, error) {
			return DiscoverPythonScopes(ctx, view, rootURI, pythonPath)
		},
		Tools: tools,
	})
}

// BuildIndexRequest discovers and pins one scope for every Python project
// under rootURI. Interpreter, effective config digest, import roots, and the
// stub path (including an explicit no-stub sentinel) must be represented in
// each returned scope's BuildInputs; config paths alone are not sufficient.
func (p *SemanticIndexProvider) BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	if view == nil {
		return model.Request{}, model.ErrMissingView
	}
	if p == nil || p.config.BuildScopes == nil {
		return model.Request{}, errors.New("pyright semantic index: no project scope builder is configured")
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
	request := model.Request{WorkspaceRootURI: rootURI, View: view, Scopes: make([]model.Scope, 0, len(scopes)), Provenance: make(map[string]model.Provenance, len(scopes))}
	seen := make(map[string]struct{}, len(scopes))
	for _, discoveredScope := range scopes {
		if err := ctx.Err(); err != nil {
			return model.Request{}, err
		}
		scope := cloneScope(discoveredScope)
		if scope.ID == "" || scope.Language != langID || scope.RootURI == "" || !uriWithinRoot(rootURI, scope.RootURI) {
			return model.Request{}, model.ErrInvalidScope
		}
		if _, ok := seen[scope.ID]; ok {
			return model.Request{}, model.ErrInvalidScope
		}
		seen[scope.ID] = struct{}{}
		backend := p.config.Backend
		if backend.Language == "" {
			backend.Language = scope.Language
		}
		if backend.Name == "" {
			backend.Name = "pyright-semantic-index"
		}
		if backend.Language != scope.Language {
			return model.Request{}, model.ErrInvalidProvenance
		}
		provenance := model.Provenance{
			SchemaVersion: model.SchemaVersion, Identity: view.Identity(), Scope: cloneScope(scope),
			Extractor: p.config.Extractor, ExtractorVer: p.config.ExtractorVersion,
			Backend: backend, BackendEpoch: p.config.BackendEpoch, Toolchain: p.config.Toolchain,
			Tools: append([]model.ToolIdentity(nil), tools...),
		}
		scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
		provenance.Scope = cloneScope(scope)
		request.Scopes = append(request.Scopes, scope)
		request.Provenance[scope.ID] = provenance
	}
	return request, nil
}

// RebuildVerifiedPlannerRequest reconstructs the current Python planner
// request from a previously verified generation. It only reads the immutable
// view and pinned tool files; it never creates or invokes a runner. Python
// project/config scopes are rediscovered so changed closure inputs invalidate
// the saved attestation instead of being copied into the new request.
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
	if first.Backend.Language != langID || first.Backend.Name == "" {
		return model.Request{}, model.ErrInvalidProvenance
	}
	var pythonPath string
	pythonTools := 0
	for _, tool := range tools {
		if tool.Name == "python" {
			pythonTools++
			pythonPath = tool.Path
		}
	}
	if pythonTools > 1 {
		return model.Request{}, model.ErrInvalidProvenance
	}
	expected := make(map[string]model.Provenance, len(attestations))
	for _, provenance := range attestations {
		scope := provenance.Scope
		if provenance.SchemaVersion != model.SchemaVersion ||
			!sameStableWorkspaceIdentity(provenance.Identity, currentIdentity) ||
			provenance.Extractor != first.Extractor || provenance.ExtractorVer != first.ExtractorVer || provenance.Toolchain != first.Toolchain ||
			!sameToolSet(tools, provenance.Tools) || !validToolList(provenance.Tools) ||
			scope.ID == "" || scope.Language != langID || scope.RootURI == "" || !uriWithinRoot(rootURI, scope.RootURI) ||
			provenance.Backend != first.Backend || provenance.BackendEpoch != first.BackendEpoch ||
			provenance.Scope.BuildContext == "" || scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		if _, duplicate := expected[scope.ID]; duplicate {
			return model.Request{}, model.ErrInvalidProvenance
		}
		expected[scope.ID] = provenance
	}
	if err := verifyPinnedToolFiles(ctx, tools); err != nil {
		return model.Request{}, err
	}

	currentScopes, err := DiscoverPythonScopes(ctx, view, rootURI, pythonPath)
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
		Backend: first.Backend, BackendEpoch: first.BackendEpoch,
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
		return model.Report{}, errors.New("pyright semantic index: nil sink")
	}
	report = model.Report{Identity: request.View.Identity(), UsedTools: make(map[string][]model.ToolIdentity)}
	seenScopes := make(map[string]struct{}, len(request.Scopes))
	filesByScope := make(map[string][]model.File, len(request.Scopes))
	for _, scope := range request.Scopes {
		files, err := walkScopeFiles(ctx, request.View, scope.RootURI)
		if err != nil {
			return report, err
		}
		filesByScope[scope.ID] = excludeNestedPythonProjects(files, scope, request.Scopes)
	}

	for _, scope := range request.Scopes {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		if scope.ID == "" || scope.Language != langID || scope.RootURI == "" || scope.BuildContext == "" {
			return report, model.ErrInvalidScope
		}
		if _, ok := seenScopes[scope.ID]; ok {
			return report, model.ErrInvalidScope
		}
		seenScopes[scope.ID] = struct{}{}
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
			appendUnavailable(&report, scope.ID, "pinned Pyright 1.1.414 and the embedded Pyright semantic exporter are required")
			continue
		}
		if reason := pythonBuildOption(scope.Build.Options, "pyrightconfigunsupported"); reason != "" {
			appendUnknown(&report, scope.ID, reason)
			continue
		}
		if !deterministicPythonBuild(scope) {
			appendUnknown(&report, scope.ID, "Python interpreter and project configuration are not deterministically recorded in BuildInputs")
			continue
		}
		if p == nil || p.config.Runner == nil {
			appendUnavailable(&report, scope.ID, "no Pyright semantic runner is configured")
			continue
		}
		materializer, ok := request.View.(model.WorkspaceMaterializer)
		if !ok {
			appendUnavailable(&report, scope.ID, "immutable workspace view cannot be materialized for an external semantic tool")
			continue
		}
		files := filesByScope[scope.ID]
		materialized, cleanup, err := materialize(ctx, materializer, scope.RootURI, files, "omnilsp-pyright-index-")
		if err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			appendUnavailable(&report, scope.ID, "immutable snapshot materialization failed: "+err.Error())
			continue
		}
		runRequest := PyrightSemanticRequest{
			Scope: cloneScope(scope), ProjectScopes: cloneScopes(request.Scopes),
			ProjectFiles: cloneProjectFiles(filesByScope), Files: append([]model.File(nil), files...),
			Materialized: materialized, Tools: tools,
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
			appendUnavailable(&report, scope.ID, "pinned Pyright semantic tool verification failed: "+verifyErr.Error())
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
		emit := func(batch PyrightSemanticBatch) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return writeBatch(ctx, sink, scope, fileByURI, batch)
		}
		result, exportErr := p.config.Runner.Export(ctx, runRequest, emit)
		cleanupErr := cleanup()
		if ctx.Err() != nil {
			return report, errors.Join(ctx.Err(), exportErr, cleanupErr)
		}
		if exportErr != nil || cleanupErr != nil {
			joined := errors.Join(exportErr, cleanupErr)
			appendUnavailable(&report, scope.ID, "Pyright semantic export failed: "+joined.Error())
			report.Errors = append(report.Errors, joined.Error())
			return report, joined
		}
		if err := appendCoverage(&report, scope.ID, result.Coverage, files); err != nil {
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

func cloneProjectFiles(files map[string][]model.File) map[string][]model.File {
	cloned := make(map[string][]model.File, len(files))
	for scopeID, scopedFiles := range files {
		cloned[scopeID] = append([]model.File(nil), scopedFiles...)
	}
	return cloned
}

func deterministicPythonBuild(scope model.Scope) bool {
	interpreter := false
	for key, value := range scope.Build.Environment {
		switch strings.ToLower(strings.ReplaceAll(key, "_", "")) {
		case "python", "pythoninterpreter", "pythonexecutable", "virtualenv":
			interpreter = interpreter || strings.TrimSpace(value) != ""
		}
	}
	project, configDigest, stubPath := false, false, false
	for key, value := range scope.Build.Options {
		normalized := strings.ToLower(strings.ReplaceAll(key, "_", ""))
		if (normalized == "pyrightconfig" || normalized == "projectconfig") && strings.TrimSpace(value) != "" {
			project = true
		}
		if (normalized == "pyrightconfigdigest" || normalized == "projectconfigdigest") && validConfigDigest(value) {
			configDigest = true
		}
		if normalized == "stubpath" && strings.TrimSpace(value) != "" {
			stubPath = true
		}
	}
	return interpreter && project && configDigest && stubPath && len(scope.Build.IncludePaths) > 0
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

func pinnedTools(tools []model.ToolIdentity) ([]model.ToolIdentity, bool) {
	var hasCompiler, hasExporter bool
	for _, tool := range tools {
		if tool.Name == semanticIndexCompilerName && tool.Version == semanticIndexCompilerVersion {
			hasCompiler = true
		}
		if tool.Name == semanticIndexExporterName {
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

func excludeNestedPythonProjects(files []model.File, scope model.Scope, scopes []model.Scope) []model.File {
	nestedRoots := make([]string, 0, len(scopes))
	for _, candidate := range scopes {
		if candidate.ID != scope.ID && candidate.RootURI != scope.RootURI && uriWithinRoot(scope.RootURI, candidate.RootURI) {
			nestedRoots = append(nestedRoots, candidate.RootURI)
		}
	}
	if len(nestedRoots) == 0 {
		return files
	}
	filtered := make([]model.File, 0, len(files))
	for _, file := range files {
		nested := false
		for _, root := range nestedRoots {
			if uriWithinRoot(root, file.URI) {
				nested = true
				break
			}
		}
		if !nested {
			filtered = append(filtered, file)
		}
	}
	return filtered
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

func writeBatch(ctx context.Context, sink model.Sink, scope model.Scope, files map[string]model.File, batch PyrightSemanticBatch) error {
	if len(batch.Symbols) > semanticIndexBatchLimit || len(batch.Occurrences) > semanticIndexBatchLimit || len(batch.Edges) > semanticIndexBatchLimit {
		return fmt.Errorf("Pyright semantic runner exceeded bounded batch size %d", semanticIndexBatchLimit)
	}
	for _, symbol := range batch.Symbols {
		if symbol.ID == "" || symbol.Name == "" || (symbol.ScopeID != "" && symbol.ScopeID != scope.ID) {
			return errors.New("Pyright semantic runner emitted an invalid symbol")
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
				return errors.New("Pyright semantic runner emitted an invalid occurrence")
			}
			mappedURI, mappedRange := remapGeneratedRange(files, occurrence.URI, occurrence.Range)
			file, ok := files[mappedURI]
			if !ok {
				return fmt.Errorf("Pyright semantic occurrence points outside the immutable scope: %q", mappedURI)
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
				return errors.New("Pyright semantic runner emitted an invalid edge")
			}
			mappedURI, mappedRange := remapGeneratedRange(files, edge.SourceURI, edge.Range)
			file, ok := files[mappedURI]
			if !ok {
				return fmt.Errorf("Pyright semantic edge points outside the immutable scope: %q", mappedURI)
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
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scopeID, Fact: fact, State: model.Unknown, Reason: reason})
	}
}

func appendCoverage(report *model.Report, scopeID string, raw []model.Coverage, files []model.File) error {
	coverageByFact := make(map[model.FactKind]model.Coverage, len(raw))
	for _, item := range raw {
		if item.ScopeID != "" && item.ScopeID != scopeID {
			return fmt.Errorf("Pyright semantic runner returned coverage for unexpected scope %q", item.ScopeID)
		}
		if item.ScopeID == "" {
			item.ScopeID = scopeID
		}
		if _, exists := coverageByFact[item.Fact]; exists {
			return fmt.Errorf("Pyright semantic runner returned duplicate coverage for %q", item.Fact)
		}
		if !validCoverageState(item.State) {
			return fmt.Errorf("Pyright semantic runner returned invalid coverage state %q", item.State)
		}
		if !requiredFact(item.Fact) {
			return fmt.Errorf("Pyright semantic runner returned unsupported fact %q", item.Fact)
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
			item = model.Coverage{ScopeID: scopeID, Fact: fact, State: model.Unknown, Reason: "Pyright semantic runner did not declare coverage for this fact"}
		}
		item.ScopeID = scopeID
		if fact == model.FactInclude {
			item.State, item.Reason = model.Unavailable, "preprocessor include relationships do not apply to Python"
		}
		if item.State == model.Complete {
			if fact == model.FactGenerated && hasGenerated {
				item.State = model.IncompleteKnownSubset
				if hasUnmappedGenerated {
					item.Reason = "one or more generated Python files lack usable range-level source-map data"
				} else {
					item.Reason = "range-level source-map data has no exhaustive-coverage attestation"
				}
			}
		}
		if item.State != model.Complete && item.Reason == "" {
			item.Reason = "Pyright semantic runner reported incomplete coverage"
		}
		report.Coverage = append(report.Coverage, item)
	}
	return nil
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
