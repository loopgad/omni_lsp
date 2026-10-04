package clang

import (
	"context"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/index/model"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

// RebuildVerifiedPlannerRequest re-derives C/C++ compile contexts from the
// captured compile database and verifies the pinned tool and helper-header
// bytes. It never executes clang or the helper.
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
	if first.Backend.Language != "cpp" || first.Backend.Name == "" ||
		first.SchemaVersion != model.SchemaVersion || first.Extractor != ExtractorName || first.Toolchain == "" {
		return model.Request{}, model.ErrInvalidProvenance
	}
	backend, epoch := first.Backend, first.BackendEpoch
	currentEnvironment := captureCompilerEnvironment()
	expected := make(map[string]model.Provenance, len(attestations))
	identities := make(map[string]toolPair, len(attestations))
	verifiedTools := make(map[model.ToolIdentity]struct{}, len(attestations)*2)

	for _, provenance := range attestations {
		if err := ctx.Err(); err != nil {
			return model.Request{}, err
		}
		scope := provenance.Scope
		if provenance.SchemaVersion != model.SchemaVersion ||
			!sameStableWorkspaceIdentity(provenance.Identity, currentIdentity) ||
			provenance.Extractor != first.Extractor || provenance.Backend != backend || provenance.BackendEpoch != epoch ||
			provenance.Toolchain == "" || scope.ID == "" || scope.Language != "cpp" || scope.RootURI != rootURI ||
			scope.BuildContext == "" || !reflect.DeepEqual(scope.Build.Environment, currentEnvironment) ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		if len(scope.Build.Options) != 2 {
			return model.Request{}, fmt.Errorf("%w: clang build context has no pinned helper-header manifest", model.ErrInvalidProvenance)
		}
		contextKey := scope.Build.Options[contextOption]
		if !validHexDigest(contextKey) || scope.ID != "clang-"+contextKey[:16] {
			return model.Request{}, model.ErrInvalidProvenance
		}
		rawManifest := scope.Build.Options[headerManifestOption]
		manifest, headerDigest, err := decodeHelperHeaderManifest(rawManifest)
		if err != nil || provenance.ExtractorVer != ExtractorVersionPinned()+"+headers-sha256:"+headerDigest {
			return model.Request{}, fmt.Errorf("%w: invalid or mismatched clang helper-header manifest", model.ErrInvalidProvenance)
		}
		if _, duplicate := expected[contextKey]; duplicate {
			return model.Request{}, model.ErrInvalidProvenance
		}
		if err := verifyHelperHeaderManifest(ctx, manifest); err != nil {
			return model.Request{}, fmt.Errorf("%w: %v", model.ErrInvalidProvenance, err)
		}
		pinnedTools, err := pinnedToolchain(provenance.Tools)
		if err != nil || provenance.Toolchain != "clang/"+pinnedTools.compiler.Version || pinnedTools.libclang.Version != pinnedTools.compiler.Version ||
			pinnedTools.compiler.Name != compilerToolName(pinnedTools.compiler.Path) || !validCompilerVersion(pinnedTools.compiler.Version) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		if !samePath(pinnedTools.libclang.Path, libclangPathForCompiler(pinnedTools.compiler.Path)) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		pair := toolPair{compiler: pinnedTools.compiler, libclang: pinnedTools.libclang}
		compilerKey := filepath.Clean(pinnedTools.compiler.Path)
		if prior, ok := identities[compilerKey]; ok && prior != pair {
			return model.Request{}, model.ErrInvalidProvenance
		}
		identities[compilerKey] = pair
		for _, tool := range provenance.Tools {
			if _, ok := verifiedTools[tool]; ok {
				continue
			}
			if err := verifyHashContext(ctx, tool); err != nil {
				return model.Request{}, fmt.Errorf("%w: %v", model.ErrInvalidProvenance, err)
			}
			verifiedTools[tool] = struct{}{}
		}
		expected[contextKey] = provenance
	}

	files, err := walkFiles(ctx, view, rootURI)
	if err != nil {
		return model.Request{}, err
	}
	entries, found, err := readRawCommands(ctx, view, files)
	if err != nil {
		return model.Request{}, err
	}
	if !found {
		return model.Request{}, model.ErrInvalidProvenance
	}
	root, err := workspaceuri.Parse(rootURI)
	if err != nil {
		return model.Request{}, err
	}
	rootPath, err := root.Path()
	if err != nil {
		return model.Request{}, err
	}
	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return model.Request{}, err
	}
	knownFiles := make(map[string]string, len(files))
	for _, file := range files {
		parsed, parseErr := workspaceuri.Parse(file.URI)
		if parseErr != nil {
			continue
		}
		path, pathErr := parsed.Path()
		if pathErr != nil {
			continue
		}
		rel, relErr := filepath.Rel(rootPath, path)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			knownFiles[canonicalRelKey(rel)] = normalizeRel(rel)
		}
	}

	contexts := make(map[string]*compileContext)
	var discoveryProblems []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return model.Request{}, err
		}
		command, sourceRel, problem := canonicalCommandWithPins(ctx, entry, rootPath, knownFiles, identities, false)
		if problem != "" {
			appendDiscoveryProblem(&discoveryProblems, problem)
			continue
		}
		key := compileContextDigest(command.compiler.Path, command.dir, command.args)
		group := contexts[key]
		if group == nil {
			if len(contexts) >= maxCompileContexts {
				appendDiscoveryProblem(&discoveryProblems, fmt.Sprintf("compile context limit %d exceeded", maxCompileContexts))
				continue
			}
			group = &compileContext{
				key: key, compiler: command.compiler, libclang: command.libclang,
				args: command.args, dir: command.dir, files: make(map[string]struct{}),
			}
			contexts[key] = group
		}
		group.files[sourceRel] = struct{}{}
		group.tests = group.tests || looksLikeTestTranslationUnit(sourceRel)
		if command.problem != "" {
			group.problem = command.problem
			group.unavailable = command.unavailable
		}
	}
	if len(discoveryProblems) != 0 || len(contexts) != len(expected) {
		return model.Request{}, fmt.Errorf("%w: captured compile database no longer reproduces every pinned C/C++ context", model.ErrInvalidProvenance)
	}
	ordered := make([]*compileContext, 0, len(contexts))
	for key, group := range contexts {
		provenance, ok := expected[key]
		if !ok || group.problem != "" {
			return model.Request{}, model.ErrInvalidProvenance
		}
		group.extractorVersion = provenance.ExtractorVer
		group.headerManifest = provenance.Scope.Build.Options[headerManifestOption]
		ordered = append(ordered, group)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].key < ordered[j].key })

	request, err := requestForContexts(ctx, view, rootURI, backend, epoch, ordered)
	if err != nil {
		return model.Request{}, err
	}
	if len(request.Provenance) != len(expected) {
		return model.Request{}, model.ErrInvalidProvenance
	}
	for contextKey, prior := range expected {
		current, ok := request.Provenance["clang-"+contextKey[:16]]
		if !ok || !reflect.DeepEqual(current.Scope, prior.Scope) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		prior.Identity = currentIdentity
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

func validHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func validCompilerVersion(version string) bool {
	return versionPattern.MatchString("clang version " + version)
}

func libclangPathForCompiler(compilerPath string) string {
	name := "libclang.so"
	if strings.EqualFold(filepath.Ext(compilerPath), ".exe") {
		name = "libclang.dll"
	}
	return filepath.Join(filepath.Dir(compilerPath), name)
}

func verifyHashContext(ctx context.Context, tool model.ToolIdentity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	actual, _, err := hashRegularFile(ctx, tool.Path, int64(^uint64(0)>>1))
	if err != nil {
		return fmt.Errorf("hash pinned tool %q: %w", tool.Name, err)
	}
	if actual != strings.ToLower(tool.SHA256) {
		return fmt.Errorf("pinned tool %q SHA-256 mismatch", tool.Name)
	}
	return nil
}
