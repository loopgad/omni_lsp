package golang

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
)

const semanticIndexBatchSize = 256
const pinnedGoPackagesVersion = "v0.49.0"
const goEnvironmentSourceOption = "go.context.environmentSourceSHA256"

// BuildIndexRequest discovers Go modules/workspaces from the immutable view,
// captures the effective Go build environment, and pins the exact go command
// used by the exporter. Runtime callers can pass its result directly to
// ExportIndex without reconstructing build inputs themselves.
func (b *Backend) BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (request model.Request, retErr error) {
	if view == nil {
		return model.Request{}, model.ErrMissingView
	}
	if rootURI == "" {
		return model.Request{}, model.ErrInvalidScope
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return model.Request{}, err
	}
	materializer, ok := view.(model.WorkspaceMaterializer)
	if !ok {
		return model.Request{}, errors.New("Go semantic index: immutable workspace view cannot be materialized")
	}
	tool, version, target, err := currentGoTool(ctx)
	if err != nil {
		return model.Request{}, err
	}
	root, err := os.MkdirTemp("", "omnilsp-go-index-request-*")
	if err != nil {
		return model.Request{}, err
	}
	materialized, err := materializer.Materialize(ctx, rootURI, root)
	if err != nil {
		_ = os.RemoveAll(root)
		return model.Request{}, fmt.Errorf("materialize immutable workspace for Go scope discovery: %w", err)
	}
	if materialized == nil {
		_ = os.RemoveAll(root)
		return model.Request{}, errors.New("Go semantic index: materializer returned no workspace")
	}
	closed := false
	cleanup := func() error {
		if closed {
			return nil
		}
		closed = true
		return errors.Join(materialized.Close(), os.RemoveAll(root))
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()
	if err := validateMaterializedRoot(materialized, rootURI); err != nil {
		return model.Request{}, fmt.Errorf("invalid Go discovery materialization: %w", err)
	}
	base := model.Scope{ID: "go:discovery", Language: "go", RootURI: rootURI, BuildContext: "go:discovery"}
	files, err := captureAndVerifyFiles(ctx, view, base, materialized)
	if err != nil {
		return model.Request{}, fmt.Errorf("verify immutable Go project files: %w", err)
	}
	goEnv, err := queryGoEnvironment(ctx, tool.path, materialized.RootPath(), files)
	if err != nil {
		return model.Request{}, err
	}
	buildEnv, err := semanticGoEnvironment(goEnv, materialized.RootPath(), files)
	if err != nil {
		return model.Request{}, err
	}
	// PATH selects auxiliary compiler tools (notably CC for cgo), so capture it
	// in the hashed scope and pass the same value to every Go command.
	if pathValue := os.Getenv("PATH"); pathValue != "" {
		buildEnv["PATH"] = pathValue
	} else {
		buildEnv["PATH"] = filepath.Dir(tool.path)
	}
	for _, key := range []string{"SYSTEMROOT", "WINDIR"} {
		if value := os.Getenv(key); value != "" {
			buildEnv[key] = value
		}
	}
	environmentSource, err := goEnvironmentSourceFingerprint(ctx)
	if err != nil {
		// Index extraction can still proceed from the verified go env result,
		// but a future pure read-back must not memoize without this proof.
		environmentSource = ""
	}
	packageScopes, err := discoverGoPackageScopes(rootURI, materialized.RootPath(), files, buildEnv)
	if err != nil {
		return model.Request{}, err
	}
	if len(packageScopes) == 0 {
		return model.Request{}, errors.New("Go semantic index: no go.mod or go.work project was found in the immutable scope")
	}
	toolIdentity := tool.identity
	toolchain := version + "/" + strings.ReplaceAll(target, "/", "-")
	extractorVersion, err := goPackagesVersion()
	if err != nil {
		return model.Request{}, err
	}
	request = model.Request{
		WorkspaceRootURI: rootURI,
		View:             view,
		Scopes:           make([]model.Scope, 0, len(packageScopes)),
		Provenance:       make(map[string]model.Provenance, len(packageScopes)),
	}
	for _, discovered := range packageScopes {
		scope := discovered
		scopeRoot, err := materialized.PathForURI(scope.RootURI)
		if err != nil {
			return model.Request{}, fmt.Errorf("map Go scope root %q into immutable view: %w", scope.RootURI, err)
		}
		scope.Build.Environment = cloneStringMap(buildEnv)
		scope.Build.PackagePatterns = append([]string(nil), discovered.Build.PackagePatterns...)
		scope.Build.Tests = true
		scope.Build.Options = workspaceBuildOptions(files, materialized.RootPath(), scopeRoot)
		scope.Build.Options[goEnvironmentSourceOption] = environmentSource
		mode, err := resolveGoModuleMode(scope.Build, files, scopeRoot)
		if err != nil {
			return model.Request{}, err
		}
		scope.Build.Options["go.mod"] = mode
		scope.BuildContext = model.ComputeBuildContextID(scope, "go/packages", extractorVersion, toolchain, []model.ToolIdentity{toolIdentity})
		provenance := model.Provenance{
			SchemaVersion: model.SchemaVersion,
			Identity:      view.Identity(),
			Scope:         scope,
			Extractor:     "go/packages",
			ExtractorVer:  extractorVersion,
			Backend:       identity.BackendID{Language: "go", Name: "golang-native"},
			Toolchain:     toolchain,
			Tools:         []model.ToolIdentity{toolIdentity},
		}
		request.Scopes = append(request.Scopes, scope)
		request.Provenance[scope.ID] = provenance
	}
	return request, nil
}

// RebuildVerifiedPlannerRequest reconstructs the Go planner request from a
// previously verified generation. It reads only the immutable workspace view,
// current process environment sources, and pinned Go executable bytes. It does
// not materialize the workspace, run the Go command, or load packages.
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
	tools, ok := pinnedGoTools(first.Tools)
	if !ok || first.SchemaVersion != model.SchemaVersion || first.Extractor != "go/packages" ||
		first.ExtractorVer == "" || first.Toolchain == "" || first.Backend != (identity.BackendID{Language: "go", Name: "golang-native"}) {
		return model.Request{}, model.ErrInvalidProvenance
	}
	extractorVersion, err := goPackagesVersion()
	if err != nil || first.ExtractorVer != extractorVersion {
		return model.Request{}, model.ErrInvalidProvenance
	}
	tool := tools[0]
	if toolchainMatchesTool(first.Toolchain, tool.Version) == false {
		return model.Request{}, model.ErrInvalidProvenance
	}
	if err := verifyPinnedGoToolFiles(ctx, tools); err != nil {
		return model.Request{}, err
	}
	environmentSource, err := goEnvironmentSourceFingerprint(ctx)
	if err != nil {
		return model.Request{}, fmt.Errorf("fingerprint current Go build environment sources: %w", err)
	}
	if environmentSource == "" {
		return model.Request{}, model.ErrInvalidProvenance
	}

	expected := make(map[string]model.Provenance, len(attestations))
	for _, provenance := range attestations {
		scope := provenance.Scope
		if provenance.SchemaVersion != model.SchemaVersion ||
			!sameStableGoWorkspaceIdentity(provenance.Identity, currentIdentity) ||
			provenance.Extractor != first.Extractor || provenance.ExtractorVer != first.ExtractorVer || provenance.Toolchain != first.Toolchain ||
			provenance.Backend != first.Backend || provenance.BackendEpoch != first.BackendEpoch || provenance.BackendEpoch != 0 ||
			!sameGoToolSet(tools, provenance.Tools) ||
			scope.ID == "" || scope.Language != "go" || scope.RootURI == "" || !goScopeWithinRoot(rootURI, scope.RootURI) ||
			scope.BuildContext == "" || scope.Build.Options[goEnvironmentSourceOption] != environmentSource ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		if _, duplicate := expected[scope.ID]; duplicate {
			return model.Request{}, model.ErrInvalidProvenance
		}
		expected[scope.ID] = provenance
	}

	files, workspaceRoot, err := captureGoWorkspaceFiles(ctx, view, rootURI)
	if err != nil {
		return model.Request{}, fmt.Errorf("read immutable Go project inputs: %w", err)
	}
	buildEnvironment := cloneStringMap(first.Scope.Build.Environment)
	if err := verifyCurrentGoEnvironment(buildEnvironment); err != nil {
		return model.Request{}, model.ErrInvalidProvenance
	}
	packageScopes, err := discoverGoPackageScopes(rootURI, workspaceRoot, files, buildEnvironment)
	if err != nil {
		return model.Request{}, err
	}
	if len(packageScopes) != len(expected) {
		return model.Request{}, model.ErrInvalidProvenance
	}

	request := model.Request{
		WorkspaceRootURI: rootURI,
		View:             view,
		Scopes:           make([]model.Scope, 0, len(packageScopes)),
		Provenance:       make(map[string]model.Provenance, len(packageScopes)),
	}
	for _, discovered := range packageScopes {
		prior, ok := expected[discovered.ID]
		if !ok {
			return model.Request{}, model.ErrInvalidProvenance
		}
		scopeRoot := uriToPath(discovered.RootURI)
		scope := discovered
		scope.Build.Environment = cloneStringMap(buildEnvironment)
		scope.Build.PackagePatterns = append([]string(nil), discovered.Build.PackagePatterns...)
		scope.Build.Tests = true
		scope.Build.Options = workspaceBuildOptions(files, workspaceRoot, scopeRoot)
		scope.Build.Options[goEnvironmentSourceOption] = environmentSource
		mode, err := resolveGoModuleMode(scope.Build, files, scopeRoot)
		if err != nil {
			return model.Request{}, err
		}
		scope.Build.Options["go.mod"] = mode
		scope.BuildContext = model.ComputeBuildContextID(scope, first.Extractor, first.ExtractorVer, first.Toolchain, tools)
		if !sameGoScope(scope, prior.Scope) {
			return model.Request{}, model.ErrInvalidProvenance
		}
		prior.Identity = currentIdentity
		prior.Tools = append([]model.ToolIdentity(nil), tools...)
		prior.Scope = scope
		request.Scopes = append(request.Scopes, scope)
		request.Provenance[scope.ID] = prior
	}
	return request, nil
}

func sameStableGoWorkspaceIdentity(a, b model.Identity) bool {
	return a.Workspace != "" && a.Workspace == b.Workspace && a.DiskDigest != "" && a.DiskDigest == b.DiskDigest &&
		a.Repository == b.Repository && a.Revision == b.Revision
}

func sameGoScope(left, right model.Scope) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func goScopeWithinRoot(rootURI, candidateURI string) bool {
	root, rootErr := localGoURIPath(rootURI)
	candidate, candidateErr := localGoURIPath(candidateURI)
	return rootErr == nil && candidateErr == nil && pathWithinRoot(candidate, root)
}

func localGoURIPath(value string) (string, error) {
	parsed, err := uri.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.Scheme() != "file" {
		return "", fmt.Errorf("Go workspace URI %q is not a file URI", value)
	}
	path, err := parsed.Path()
	if err != nil || !filepath.IsAbs(path) {
		return "", fmt.Errorf("Go workspace URI %q has no local absolute path", value)
	}
	return filepath.Clean(path), nil
}

func pinnedGoTools(tools []model.ToolIdentity) ([]model.ToolIdentity, bool) {
	if len(tools) != 1 {
		return nil, false
	}
	tool := tools[0]
	if tool.Name != "go" || tool.Version == "" || goVersionToken(tool.Version) != tool.Version ||
		!filepath.IsAbs(tool.Path) || len(tool.SHA256) != sha256.Size*2 {
		return nil, false
	}
	if _, err := hex.DecodeString(tool.SHA256); err != nil {
		return nil, false
	}
	return append([]model.ToolIdentity(nil), tools...), true
}

func toolchainMatchesTool(toolchain, version string) bool {
	return strings.HasPrefix(toolchain, version+"/") && len(strings.TrimPrefix(toolchain, version+"/")) > 0
}

func sameGoToolSet(expected, actual []model.ToolIdentity) bool {
	if len(expected) != 1 || len(actual) != 1 {
		return false
	}
	left, right := expected[0], actual[0]
	return left == right
}

func verifyPinnedGoToolFiles(ctx context.Context, tools []model.ToolIdentity) error {
	for _, tool := range tools {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := os.Open(tool.Path)
		if err != nil {
			return fmt.Errorf("open pinned Go executable: %w", err)
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, &contextReader{ctx: ctx, r: file})
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("hash pinned Go executable: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close pinned Go executable: %w", closeErr)
		}
		if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), tool.SHA256) {
			return errors.New("pinned Go executable SHA-256 mismatch")
		}
	}
	return nil
}

func verifyCurrentGoEnvironment(attested map[string]string) error {
	if len(attested) == 0 {
		return model.ErrInvalidProvenance
	}
	for _, key := range []string{
		"GOOS", "GOARCH", "GOFLAGS", "GO111MODULE", "GOEXPERIMENT", "CGO_ENABLED",
		"CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS", "CC", "CXX",
		"GOAMD64", "GOARM", "GOARM64", "GO386", "GOMIPS", "GOMIPS64", "GOWASM",
		"GOFIPS140", "GOPROXY", "GOSUMDB", "GONOSUMDB", "GONOPROXY", "GOPRIVATE",
		"GOVCS", "GOTOOLCHAIN", "GOPATH", "GOMODCACHE", "GOROOT", "PATH", "SYSTEMROOT", "WINDIR",
	} {
		current, present := os.LookupEnv(key)
		if present {
			if key == "GOTOOLCHAIN" {
				current = "local"
			}
			if key == "PATH" && current == "" {
				continue
			}
			if attested[key] != current {
				return model.ErrInvalidProvenance
			}
		}
	}
	return nil
}

func goEnvironmentSourceFingerprint(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	environment := os.Environ()
	sort.Strings(environment)
	goEnv := os.Getenv("GOENV")
	var configPath string
	if goEnv != "" && goEnv != "off" {
		configPath = goEnv
	} else if goEnv == "" {
		configDir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		configPath = filepath.Join(configDir, "go", "env")
	}
	configDigest := "off"
	if configPath != "" {
		if !filepath.IsAbs(configPath) {
			// The original discovery command ran from a disposable materialized
			// workspace. A relative GOENV path cannot be re-resolved after that
			// directory is removed, so it cannot support persistent read-back.
			return "", nil
		}
		absolute, err := filepath.Abs(configPath)
		if err != nil {
			return "", err
		}
		configPath = filepath.Clean(absolute)
		file, err := os.Open(configPath)
		if errors.Is(err, os.ErrNotExist) {
			configDigest = "absent"
		} else if err != nil {
			return "", err
		} else {
			hash := sha256.New()
			_, copyErr := io.Copy(hash, &contextReader{ctx: ctx, r: file})
			closeErr := file.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
			configDigest = hex.EncodeToString(hash.Sum(nil))
		}
	}
	canonical, err := json.Marshal(struct {
		Environment []string
		GoEnvPath   string
		GoEnvSHA256 string
	}{environment, configPath, configDigest})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(canonical)
	return hex.EncodeToString(hash[:]), nil
}

func currentGoTool(ctx context.Context) (verifiedGoTool, string, string, error) {
	path, err := exec.LookPath("go")
	if err != nil {
		return verifiedGoTool{}, "", "", fmt.Errorf("locate Go executable: %w", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return verifiedGoTool{}, "", "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return verifiedGoTool{}, "", "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return verifiedGoTool{}, "", "", copyErr
	}
	if closeErr != nil {
		return verifiedGoTool{}, "", "", closeErr
	}
	output, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return verifiedGoTool{}, "", "", fmt.Errorf("query Go executable version: %w", err)
	}
	versionLine := strings.TrimSpace(string(output))
	version := goVersionToken(versionLine)
	if version == "" {
		return verifiedGoTool{}, "", "", fmt.Errorf("unrecognized Go version output %q", versionLine)
	}
	fields := strings.Fields(versionLine)
	if len(fields) < 4 || fields[0] != "go" || fields[1] != "version" {
		return verifiedGoTool{}, "", "", fmt.Errorf("unrecognized Go toolchain target in %q", versionLine)
	}
	tool := model.ToolIdentity{
		Name: "go", Path: filepath.Clean(path), Version: version,
		SHA256: hex.EncodeToString(hash.Sum(nil)),
	}
	return verifiedGoTool{identity: tool, path: filepath.Clean(path)}, version, fields[3], nil
}

func goPackagesVersion() (string, error) {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "golang.org/x/tools" && dep.Version != "" {
				return dep.Version, nil
			}
		}
	}
	// Test binaries and some trimpath release builds omit dependency versions
	// from the runtime build info. This fallback is pinned to go.mod and guarded
	// by TestGoPackagesExtractorPinMatchesModuleManifest.
	return pinnedGoPackagesVersion, nil
}

func queryGoEnvironment(ctx context.Context, goPath, root string, files *fileIndex) (map[string]string, error) {
	command := exec.CommandContext(ctx, goPath, "env", "-json")
	command.Dir = root
	command.Env = os.Environ()
	rootWork := filepath.Join(root, "go.work")
	if _, ok := files.byPath[pathKey(rootWork)]; ok {
		configured := strings.TrimSpace(os.Getenv("GOWORK"))
		if configured != "" && configured != "auto" && configured != "off" {
			configuredPath, err := filepath.Abs(configured)
			if err != nil || !sameExecutablePath(configuredPath, rootWork) {
				return nil, errors.New("Go semantic index: configured GOWORK does not match the immutable workspace go.work")
			}
		}
		if configured == "off" {
			command.Env = replaceEnvironment(command.Env, "GOWORK", "off")
		} else {
			command.Env = replaceEnvironment(command.Env, "GOWORK", rootWork)
		}
	} else {
		if configured := os.Getenv("GOWORK"); configured != "" && configured != "off" {
			return nil, errors.New("Go semantic index: configured GOWORK is not present at the immutable workspace root")
		}
		command.Env = replaceEnvironment(command.Env, "GOWORK", "off")
	}
	command.Env = replaceEnvironment(command.Env, "GOTOOLCHAIN", "local")
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("read effective Go build environment: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(output, &values); err != nil {
		return nil, fmt.Errorf("decode effective Go build environment: %w", err)
	}
	return values, nil
}

func replaceEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if strings.HasPrefix(strings.ToUpper(entry), strings.ToUpper(prefix)) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, prefix+value)
}

func semanticGoEnvironment(goEnv map[string]string, root string, files *fileIndex) (map[string]string, error) {
	values := make(map[string]string)
	for _, key := range []string{
		"GOOS", "GOARCH", "GOFLAGS", "GO111MODULE", "GOEXPERIMENT", "CGO_ENABLED",
		"CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS", "CC", "CXX",
		"GOAMD64", "GOARM", "GOARM64", "GO386", "GOMIPS", "GOMIPS64", "GOWASM",
		"GOFIPS140", "GOPROXY", "GOSUMDB", "GONOSUMDB", "GONOPROXY", "GOPRIVATE",
		"GOVCS", "GOTOOLCHAIN", "GOPATH", "GOMODCACHE", "GOROOT",
	} {
		if value, ok := goEnv[key]; ok {
			values[key] = value
		}
	}
	if values["GOOS"] == "" || values["GOARCH"] == "" {
		return nil, errors.New("Go semantic index: go env did not resolve GOOS and GOARCH")
	}
	if err := validateGoFlags(values["GOFLAGS"]); err != nil {
		return nil, err
	}
	if _, hasWork := files.byPath[pathKey(filepath.Join(root, "go.work"))]; hasWork && os.Getenv("GOWORK") != "off" {
		values["GOWORK"] = "auto"
	} else {
		values["GOWORK"] = "off"
	}
	values["GOENV"] = "off"
	values["GOTOOLCHAIN"] = "local"
	return values, nil
}

func validateGoFlags(flags string) error {
	mode, err := goModModeFromFlags(strings.Fields(flags))
	if err != nil {
		return err
	}
	if mode == "mod" {
		return errors.New("Go semantic index: GOFLAGS requests mod=mod, which may modify immutable go.mod or go.sum files")
	}
	for _, flag := range strings.Fields(flags) {
		trimmed := strings.TrimLeft(flag, "-")
		name, _, _ := strings.Cut(trimmed, "=")
		if goFlagUsesExternalInput(name) {
			return fmt.Errorf("Go semantic index: GOFLAGS contains unsupported external-path or execution option %q", flag)
		}
	}
	return nil
}

func goFlagUsesExternalInput(name string) bool {
	switch name {
	case "toolexec", "overlay", "modfile", "exec", "pkgdir", "C", "workfile":
		return true
	default:
		return false
	}
}

func discoverGoPackageScopes(rootURI, root string, files *fileIndex, environment map[string]string) ([]model.Scope, error) {
	root = filepath.Clean(root)
	if _, ok := files.byPath[pathKey(filepath.Join(root, "go.work"))]; ok && environment["GOWORK"] != "off" {
		path := filepath.Join(root, "go.work")
		entry, ok := files.byPath[pathKey(path)]
		if !ok || entry.contents == nil {
			return nil, errors.New("immutable Go workspace is missing go.work bytes")
		}
		workFile, err := modfile.ParseWork(path, entry.contents, nil)
		if err != nil {
			return nil, fmt.Errorf("parse immutable go.work: %w", err)
		}
		var patterns []string
		for _, use := range workFile.Use {
			moduleRoot := filepath.Clean(filepath.Join(root, filepath.FromSlash(use.Path)))
			rel, err := filepath.Rel(root, moduleRoot)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				return nil, fmt.Errorf("go.work use path %q escapes the materialized workspace", use.Path)
			}
			if _, ok := files.byPath[pathKey(filepath.Join(moduleRoot, "go.mod"))]; !ok {
				return nil, fmt.Errorf("go.work use path %q has no go.mod in the immutable view", use.Path)
			}
			patterns = append(patterns, packagePattern(rel))
		}
		if len(patterns) == 0 {
			return nil, errors.New("immutable go.work has no use modules")
		}
		sort.Strings(patterns)
		return []model.Scope{{ID: goScopeID(rootURI, patterns), Language: "go", RootURI: rootURI,
			Build: model.BuildInputs{PackagePatterns: patterns}}}, nil
	}
	if _, ok := files.byPath[pathKey(filepath.Join(root, "go.mod"))]; ok {
		patterns := []string{"./..."}
		return []model.Scope{{ID: goScopeID(rootURI, patterns), Language: "go", RootURI: rootURI,
			Build: model.BuildInputs{PackagePatterns: patterns}}}, nil
	}
	var moduleDirs []string
	for _, file := range files.byURI {
		if filepath.Base(file.path) != "go.mod" {
			continue
		}
		dir := filepath.Dir(file.path)
		if dir == root {
			continue
		}
		moduleDirs = append(moduleDirs, dir)
	}
	sort.Strings(moduleDirs)
	unique := moduleDirs[:0]
	for _, dir := range moduleDirs {
		if len(unique) == 0 || pathKey(unique[len(unique)-1]) != pathKey(dir) {
			unique = append(unique, dir)
		}
	}
	var scopes []model.Scope
	for _, dir := range unique {
		uri := pathToUri(dir)
		patterns := []string{"./..."}
		scopes = append(scopes, model.Scope{ID: goScopeID(uri, patterns), Language: "go", RootURI: uri,
			Build: model.BuildInputs{PackagePatterns: patterns}})
	}
	return scopes, nil
}

func packagePattern(relativeModuleRoot string) string {
	if relativeModuleRoot == "." {
		return "./..."
	}
	return "./" + filepath.ToSlash(relativeModuleRoot) + "/..."
}

func goScopeID(rootURI string, patterns []string) string {
	canonical := rootURI + "\x00" + strings.Join(patterns, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return "go:scope:" + hex.EncodeToString(sum[:12])
}

func workspaceBuildOptions(files *fileIndex, materializedRoot, scopeRoot string) map[string]string {
	root := filepath.Clean(scopeRoot)
	if root == "." || root == "" {
		root = filepath.Clean(materializedRoot)
	}
	var rows []string
	for _, file := range files.byURI {
		base := filepath.Base(file.path)
		if base != "go.mod" && base != "go.sum" && base != "go.work" && base != "go.work.sum" &&
			!(base == "modules.txt" && filepath.Base(filepath.Dir(file.path)) == "vendor") {
			continue
		}
		rel, err := filepath.Rel(root, file.path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		rows = append(rows, filepath.ToSlash(rel)+"="+string(file.hash))
	}
	sort.Strings(rows)
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return map[string]string{"go.context.moduleInputsSHA256": hex.EncodeToString(sum[:])}
}

func cloneStringMap(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

// ExportIndex extracts compiler-resolved facts from the exact immutable view
// named by request. It only runs go/packages against a materialized copy of
// that view and verifies the pinned Go executable before loading any package.
func (b *Backend) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if request.View == nil {
		return model.Report{}, model.ErrMissingView
	}
	if sink == nil {
		return model.Report{}, errors.New("semantic index: missing sink")
	}
	report := model.Report{
		Identity:  request.View.Identity(),
		Coverage:  make([]model.Coverage, 0, len(request.Scopes)*len(model.RequiredFactKinds)),
		UsedTools: make(map[string][]model.ToolIdentity, len(request.Scopes)),
	}
	seenScopes := make(map[string]struct{}, len(request.Scopes))
	for _, scope := range request.Scopes {
		if scope.ID == "" || scope.RootURI == "" || scope.BuildContext == "" {
			return report, model.ErrInvalidScope
		}
		if _, ok := seenScopes[scope.ID]; ok {
			return report, model.ErrInvalidScope
		}
		seenScopes[scope.ID] = struct{}{}
		for _, fact := range model.RequiredFactKinds {
			report.Coverage = append(report.Coverage, model.Coverage{
				ScopeID: scope.ID,
				Fact:    fact,
				State:   model.Unavailable,
				Reason:  "scope has not been processed",
			})
		}
	}

	for _, scope := range request.Scopes {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		provenance, ok := request.Provenance[scope.ID]
		if !ok || provenance.SchemaVersion != model.SchemaVersion || provenance.Identity != report.Identity ||
			!sameScope(provenance.Scope, scope) || provenance.Backend.Language != scope.Language || provenance.Backend.Name == "" ||
			provenance.Extractor == "" || provenance.ExtractorVer == "" || provenance.Toolchain == "" ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			return report, model.ErrInvalidProvenance
		}
		if scope.Language != "go" {
			setScopeCoverage(&report, scope.ID, model.Unavailable, "Go exporter cannot interpret a non-Go scope")
			continue
		}

		materializer, ok := request.View.(model.WorkspaceMaterializer)
		if !ok {
			setScopeCoverage(&report, scope.ID, model.Unavailable, "immutable workspace view does not provide a materializer for compiler loading")
			continue
		}
		goTool, err := pinnedGoTool(ctx, provenance.Tools, provenance.Toolchain)
		if err != nil {
			setScopeCoverage(&report, scope.ID, model.Unavailable, err.Error())
			continue
		}
		report.UsedTools[scope.ID] = []model.ToolIdentity{goTool.identity}
		root, err := os.MkdirTemp("", "omnilsp-go-index-*")
		if err != nil {
			setScopeCoverage(&report, scope.ID, model.Unavailable, "create isolated compiler workspace: "+err.Error())
			continue
		}
		view, err := materializer.Materialize(ctx, scope.RootURI, root)
		if err != nil {
			_ = os.RemoveAll(root)
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			setScopeCoverage(&report, scope.ID, model.Unavailable, "materialize immutable workspace view: "+err.Error())
			continue
		}
		if view == nil {
			_ = os.RemoveAll(root)
			setScopeCoverage(&report, scope.ID, model.Unavailable, "materializer returned no immutable workspace")
			continue
		}
		closed := false
		cleanup := func() error {
			if closed {
				return nil
			}
			closed = true
			return errors.Join(view.Close(), os.RemoveAll(root))
		}
		if err := validateMaterializedRoot(view, scope.RootURI); err != nil {
			cleanupErr := cleanup()
			setScopeCoverage(&report, scope.ID, model.Unavailable, "materializer returned an invalid isolated root: "+err.Error())
			if cleanupErr != nil {
				return report, cleanupErr
			}
			continue
		}
		files, err := captureAndVerifyFiles(ctx, request.View, scope, view)
		if err != nil {
			cleanupErr := cleanup()
			if ctx.Err() != nil {
				return report, errors.Join(ctx.Err(), cleanupErr)
			}
			setScopeCoverage(&report, scope.ID, model.Unavailable, "materialized workspace differs from the immutable view: "+err.Error())
			if cleanupErr != nil {
				return report, cleanupErr
			}
			continue
		}

		batch := newSemanticIndexBatch(ctx, sink, scope)
		cacheRoot, cacheErr := os.MkdirTemp("", "omnilsp-go-build-cache-*")
		if cacheErr != nil {
			cleanupErr := cleanup()
			setScopeCoverage(&report, scope.ID, model.Unavailable, "create external Go build cache: "+cacheErr.Error())
			if cleanupErr != nil {
				return report, cleanupErr
			}
			continue
		}
		result, loadErr := loadGoWorkspace(ctx, view, scope, provenance, goTool, files, cacheRoot)
		cacheCleanupErr := os.RemoveAll(cacheRoot)
		if loadErr != nil && ctx.Err() != nil {
			return report, errors.Join(ctx.Err(), cleanup(), cacheCleanupErr)
		}
		if result != nil {
			result.batch = batch
			result.extract()
		}
		if batch.err == nil {
			batch.err = batch.flush()
		}
		cleanupErr := errors.Join(cleanup(), cacheCleanupErr)
		if batch.err != nil {
			return report, errors.Join(batch.err, cleanupErr)
		}
		if cleanupErr != nil {
			return report, cleanupErr
		}
		if result == nil {
			reason := "compiler returned no package data"
			if loadErr != nil {
				reason += ": " + loadErr.Error()
			}
			setScopeCoverage(&report, scope.ID, model.Unavailable, reason)
			continue
		}
		for fact, coverage := range result.coverage() {
			setOneCoverage(&report, scope.ID, fact, coverage.state, coverage.reason)
		}
	}
	return report, nil
}

func sameScope(left, right model.Scope) bool {
	// Scope contains maps and slices, so a JSON round-trip representation is
	// preferable to shallow equality while keeping this package independent of
	// the model's validator internals.
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func setScopeCoverage(report *model.Report, scopeID string, state model.Completeness, reason string) {
	for i := range report.Coverage {
		if report.Coverage[i].ScopeID == scopeID {
			report.Coverage[i].State = state
			report.Coverage[i].Reason = reason
		}
	}
}

func setOneCoverage(report *model.Report, scopeID string, fact model.FactKind, state model.Completeness, reason string) {
	for i := range report.Coverage {
		if report.Coverage[i].ScopeID == scopeID && report.Coverage[i].Fact == fact {
			report.Coverage[i].State = state
			report.Coverage[i].Reason = reason
			return
		}
	}
}

type verifiedGoTool struct {
	identity model.ToolIdentity
	path     string
}

func pinnedGoTool(ctx context.Context, tools []model.ToolIdentity, toolchain string) (verifiedGoTool, error) {
	var pinned *model.ToolIdentity
	for i := range tools {
		if tools[i].Name != "go" {
			return verifiedGoTool{}, fmt.Errorf("Go exporter cannot verify pinned tool %q", tools[i].Name)
		}
		if pinned != nil {
			return verifiedGoTool{}, errors.New("provenance pins more than one Go executable")
		}
		copy := tools[i]
		pinned = &copy
	}
	if pinned == nil {
		return verifiedGoTool{}, errors.New("provenance does not pin the Go executable used for type checking")
	}
	path, err := filepath.Abs(pinned.Path)
	if err != nil {
		return verifiedGoTool{}, fmt.Errorf("resolve pinned Go executable: %w", err)
	}
	path = filepath.Clean(path)
	resolved, err := exec.LookPath("go")
	if err != nil {
		return verifiedGoTool{}, fmt.Errorf("Go executable is unavailable: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return verifiedGoTool{}, fmt.Errorf("resolve active Go executable: %w", err)
	}
	if !sameExecutablePath(path, resolved) {
		return verifiedGoTool{}, fmt.Errorf("active Go executable %q does not match the pinned path %q", resolved, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return verifiedGoTool{}, fmt.Errorf("open pinned Go executable: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return verifiedGoTool{}, fmt.Errorf("hash pinned Go executable: %w", copyErr)
	}
	if closeErr != nil {
		return verifiedGoTool{}, fmt.Errorf("close pinned Go executable: %w", closeErr)
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actualHash, pinned.SHA256) {
		return verifiedGoTool{}, fmt.Errorf("pinned Go executable SHA-256 mismatch: got %s", actualHash)
	}
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return verifiedGoTool{}, fmt.Errorf("query pinned Go version: %w", err)
	}
	versionOutput := strings.TrimSpace(string(out))
	if !strings.Contains(versionOutput, pinned.Version) || !strings.Contains(versionOutput, "go version") {
		return verifiedGoTool{}, fmt.Errorf("pinned Go version mismatch: got %q, expected %q", versionOutput, pinned.Version)
	}
	if !strings.Contains(strings.ReplaceAll(versionOutput, " ", ""), strings.ReplaceAll(toolchain, " ", "")) {
		// Toolchain strings may be compact (go1.x/os-arch) or the complete `go
		// version` output. The executable identity is authoritative, but a
		// contradictory request is not safe to type-check.
		versionToken := goVersionToken(versionOutput)
		if versionToken == "" || !strings.Contains(toolchain, versionToken) {
			return verifiedGoTool{}, fmt.Errorf("Go executable %q conflicts with requested toolchain %q", versionOutput, toolchain)
		}
	}
	return verifiedGoTool{identity: *pinned, path: path}, nil
}

func goVersionToken(output string) string {
	for _, field := range strings.Fields(output) {
		if strings.HasPrefix(field, "go1.") || strings.HasPrefix(field, "go2.") {
			return field
		}
	}
	return ""
}

func sameExecutablePath(left, right string) bool {
	left, _ = filepath.Abs(left)
	right, _ = filepath.Abs(right)
	left, right = filepath.Clean(left), filepath.Clean(right)
	if os.PathSeparator == '\\' {
		return strings.EqualFold(left, right)
	}
	return left == right
}

type indexedFile struct {
	file     model.File
	path     string
	hash     identity.ContentHash
	contents []byte
}

type fileIndex struct {
	byURI  map[string]indexedFile
	byPath map[string]indexedFile
}

func captureAndVerifyFiles(ctx context.Context, source model.WorkspaceView, scope model.Scope, materialized model.MaterializedView) (*fileIndex, error) {
	if materialized == nil || materialized.RootURI() != scope.RootURI || materialized.RootPath() == "" {
		return nil, errors.New("materializer returned an invalid root mapping")
	}
	root, err := filepath.Abs(materialized.RootPath())
	if err != nil {
		return nil, err
	}
	root = filepath.Clean(root)
	index := &fileIndex{byURI: make(map[string]indexedFile), byPath: make(map[string]indexedFile)}
	walkErr := source.Walk(ctx, scope.RootURI, func(file model.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		path, err := materialized.PathForURI(file.URI)
		if err != nil {
			return fmt.Errorf("map snapshot URI %q: %w", file.URI, err)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("resolve materialized path for %q: %w", file.URI, err)
		}
		path = filepath.Clean(path)
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return fmt.Errorf("materialized path for %q escapes the immutable root", file.URI)
		}
		if _, exists := index.byURI[file.URI]; exists {
			return fmt.Errorf("immutable workspace lists duplicate URI %q", file.URI)
		}
		if _, exists := index.byPath[pathKey(path)]; exists {
			return fmt.Errorf("immutable workspace maps multiple URIs to %q", path)
		}
		hash, size, err := hashViewAndMaterialized(ctx, source, file, path)
		if err != nil {
			return fmt.Errorf("verify %q: %w", file.URI, err)
		}
		if file.Size >= 0 && file.Size != size {
			return fmt.Errorf("snapshot size is %d, materialized size is %d", file.Size, size)
		}
		if expected := normalizeContentHash(file.SHA256); expected != "" && !strings.EqualFold(expected, hash) {
			return errors.New("snapshot content hash does not match its immutable bytes")
		}
		entry := indexedFile{file: file, path: path, hash: identity.ContentHash("sha256:" + hash)}
		if isGoBuildInputFile(path) {
			contents, readErr := os.ReadFile(path)
			if readErr != nil {
				return fmt.Errorf("read Go build input %q: %w", file.URI, readErr)
			}
			entry.contents = contents
		}
		index.byURI[file.URI] = entry
		index.byPath[pathKey(path)] = entry
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return index, nil
}

func captureGoWorkspaceFiles(ctx context.Context, source model.WorkspaceView, rootURI string) (*fileIndex, string, error) {
	if source == nil || rootURI == "" {
		return nil, "", model.ErrInvalidScope
	}
	rootPath, err := localGoURIPath(rootURI)
	if err != nil {
		return nil, "", err
	}
	root, err := filepath.Abs(rootPath)
	if err != nil || !filepath.IsAbs(root) {
		return nil, "", fmt.Errorf("Go workspace root %q is not a local absolute path", rootURI)
	}
	root = filepath.Clean(root)
	index := &fileIndex{byURI: make(map[string]indexedFile), byPath: make(map[string]indexedFile)}
	err = source.Walk(ctx, rootURI, func(file model.File) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		filePath, err := localGoURIPath(file.URI)
		if err != nil {
			return err
		}
		path, err := filepath.Abs(filePath)
		if err != nil || !filepath.IsAbs(path) {
			return fmt.Errorf("Go workspace URI %q is not a local absolute path", file.URI)
		}
		path = filepath.Clean(path)
		if !pathWithinRoot(path, root) {
			return fmt.Errorf("Go workspace URI %q escapes the immutable root", file.URI)
		}
		if _, exists := index.byURI[file.URI]; exists {
			return fmt.Errorf("immutable workspace lists duplicate URI %q", file.URI)
		}
		key := pathKey(path)
		if _, exists := index.byPath[key]; exists {
			return fmt.Errorf("immutable workspace maps multiple URIs to %q", path)
		}
		reader, err := source.Read(ctx, file.URI)
		if err != nil {
			return fmt.Errorf("read immutable workspace file %q: %w", file.URI, err)
		}
		hash := sha256.New()
		var contents []byte
		var size int64
		if isGoBuildInputFile(path) {
			contents, err = io.ReadAll(&contextReader{ctx: ctx, r: reader})
			if err == nil {
				_, _ = hash.Write(contents)
				size = int64(len(contents))
			}
		} else {
			size, err = io.Copy(hash, &contextReader{ctx: ctx, r: reader})
		}
		closeErr := reader.Close()
		if err != nil {
			return fmt.Errorf("read immutable workspace file %q: %w", file.URI, err)
		}
		if closeErr != nil {
			return fmt.Errorf("close immutable workspace file %q: %w", file.URI, closeErr)
		}
		if file.Size >= 0 && file.Size != size {
			return fmt.Errorf("immutable workspace file %q size does not match its metadata", file.URI)
		}
		actualHash := hex.EncodeToString(hash.Sum(nil))
		if expected := normalizeContentHash(file.SHA256); expected != "" && !strings.EqualFold(expected, actualHash) {
			return fmt.Errorf("immutable workspace file %q content hash does not match its metadata", file.URI)
		}
		entry := indexedFile{file: file, path: path, hash: identity.ContentHash("sha256:" + actualHash), contents: contents}
		index.byURI[file.URI] = entry
		index.byPath[key] = entry
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return index, root, nil
}

func isGoBuildInputFile(path string) bool {
	base := filepath.Base(path)
	return base == "go.mod" || base == "go.sum" || base == "go.work" || base == "go.work.sum" ||
		(base == "modules.txt" && filepath.Base(filepath.Dir(path)) == "vendor")
}

func normalizeContentHash(value identity.ContentHash) string {
	s := strings.TrimPrefix(strings.TrimSpace(string(value)), "sha256:")
	if len(s) != sha256.Size*2 {
		return ""
	}
	if _, err := hex.DecodeString(s); err != nil {
		return ""
	}
	return strings.ToLower(s)
}

func hashViewAndMaterialized(ctx context.Context, view model.WorkspaceView, file model.File, path string) (string, int64, error) {
	reader, err := view.Read(ctx, file.URI)
	if err != nil {
		return "", 0, err
	}
	viewHash := sha256.New()
	viewSize, copyErr := io.Copy(viewHash, &contextReader{ctx: ctx, r: reader})
	closeErr := reader.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	materializedFile, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	materializedHash := sha256.New()
	materializedSize, copyErr := io.Copy(materializedHash, materializedFile)
	closeErr = materializedFile.Close()
	if copyErr != nil {
		return "", 0, copyErr
	}
	if closeErr != nil {
		return "", 0, closeErr
	}
	if viewSize != materializedSize || !bytes.Equal(viewHash.Sum(nil), materializedHash.Sum(nil)) {
		return "", 0, errors.New("materialized bytes differ from WorkspaceView.Read")
	}
	return hex.EncodeToString(viewHash.Sum(nil)), viewSize, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func pathKey(path string) string {
	path = filepath.Clean(path)
	if os.PathSeparator == '\\' {
		return strings.ToLower(path)
	}
	return path
}

type indexBatch struct {
	ctx    context.Context
	sink   model.Sink
	scope  model.Scope
	symbol []model.Symbol
	occ    []model.Occurrence
	edge   []model.Edge
	err    error
}

func newSemanticIndexBatch(ctx context.Context, sink model.Sink, scope model.Scope) *indexBatch {
	return &indexBatch{
		ctx: ctx, sink: sink, scope: scope,
		symbol: make([]model.Symbol, 0, semanticIndexBatchSize),
		occ:    make([]model.Occurrence, 0, semanticIndexBatchSize),
		edge:   make([]model.Edge, 0, semanticIndexBatchSize),
	}
}

func (b *indexBatch) addSymbol(symbol model.Symbol) {
	if b.err != nil {
		return
	}
	b.symbol = append(b.symbol, symbol)
	if len(b.symbol) == semanticIndexBatchSize {
		b.err = b.flushSymbols()
	}
}

func (b *indexBatch) addOccurrence(occ model.Occurrence) {
	if b.err != nil {
		return
	}
	b.occ = append(b.occ, occ)
	if len(b.occ) == semanticIndexBatchSize {
		b.err = b.flushOccurrences()
	}
}

func (b *indexBatch) addEdge(edge model.Edge) {
	if b.err != nil {
		return
	}
	b.edge = append(b.edge, edge)
	if len(b.edge) == semanticIndexBatchSize {
		b.err = b.flushEdges()
	}
}

func (b *indexBatch) flush() error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if err := b.flushSymbols(); err != nil {
		return err
	}
	if err := b.flushOccurrences(); err != nil {
		return err
	}
	return b.flushEdges()
}

func (b *indexBatch) flushSymbols() error {
	if len(b.symbol) == 0 {
		return nil
	}
	if err := b.ctx.Err(); err != nil {
		return err
	}
	batch := b.symbol
	b.symbol = make([]model.Symbol, 0, semanticIndexBatchSize)
	return b.sink.WriteSymbols(b.ctx, batch)
}

func (b *indexBatch) flushOccurrences() error {
	if len(b.occ) == 0 {
		return nil
	}
	if err := b.ctx.Err(); err != nil {
		return err
	}
	batch := b.occ
	b.occ = make([]model.Occurrence, 0, semanticIndexBatchSize)
	return b.sink.WriteOccurrences(b.ctx, batch)
}

func (b *indexBatch) flushEdges() error {
	if len(b.edge) == 0 {
		return nil
	}
	if err := b.ctx.Err(); err != nil {
		return err
	}
	batch := b.edge
	b.edge = make([]model.Edge, 0, semanticIndexBatchSize)
	return b.sink.WriteEdges(b.ctx, batch)
}

type loadResult struct {
	ctx         context.Context
	scope       model.Scope
	provenance  model.Provenance
	tool        verifiedGoTool
	files       *fileIndex
	root        string
	env         []string
	packages    []*packages.Package
	packageErr  error
	batch       *indexBatch
	seenSymbols map[identity.SymbolID]struct{}
	seenOcc     map[string]struct{}
	seenEdges   map[string]struct{}
	partial     map[model.FactKind]string
	generated   []model.File
}

func loadGoWorkspace(ctx context.Context, view model.MaterializedView, scope model.Scope, provenance model.Provenance, tool verifiedGoTool, files *fileIndex, cacheRoot string) (*loadResult, error) {
	root := filepath.Clean(view.RootPath())
	if root == "" {
		return nil, errors.New("materialized root path is empty")
	}
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	rootPath = filepath.Clean(rootPath)
	patterns := append([]string(nil), scope.Build.PackagePatterns...)
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	buildInputs := scope.Build
	if buildInputs.Options == nil {
		buildInputs.Options = make(map[string]string)
	} else {
		buildInputs.Options = cloneStringMap(buildInputs.Options)
	}
	mode, err := resolveGoModuleMode(buildInputs, files, rootPath)
	if err != nil {
		return nil, err
	}
	buildInputs.Options["go.mod"] = mode
	flags, err := goBuildFlags(buildInputs)
	if err != nil {
		return nil, err
	}
	env, err := goPackageEnvironment(scope.Build.Environment, rootPath, tool.path, files, cacheRoot)
	if err != nil {
		return nil, err
	}
	for i, entry := range env {
		if strings.HasPrefix(entry, "GOFLAGS=") {
			env[i] = "GOFLAGS=" + strings.TrimSpace(strings.TrimPrefix(entry, "GOFLAGS=")) + " -mod=" + mode
			break
		}
	}
	config := &packages.Config{
		Context: ctx,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedModule |
			packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedTypesSizes,
		Dir:        rootPath,
		Env:        env,
		BuildFlags: flags,
		Tests:      scope.Build.Tests,
		Fset:       token.NewFileSet(),
	}
	pkgs, loadErr := packages.Load(config, patterns...)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(pkgs) == 0 {
		if loadErr == nil {
			loadErr = errors.New("go/packages returned no packages")
		}
		return nil, loadErr
	}
	result := &loadResult{
		ctx: ctx, scope: scope, provenance: provenance, tool: tool, files: files,
		root: rootPath, env: append([]string(nil), env...),
		packages: pkgs, packageErr: loadErr,
		seenSymbols: make(map[identity.SymbolID]struct{}),
		seenOcc:     make(map[string]struct{}), seenEdges: make(map[string]struct{}),
		partial: make(map[model.FactKind]string),
	}
	for _, entry := range files.byURI {
		if entry.file.Generated {
			result.generated = append(result.generated, entry.file)
		}
	}
	sort.Slice(result.generated, func(i, j int) bool { return result.generated[i].URI < result.generated[j].URI })
	return result, loadErr
}

func goBuildFlags(inputs model.BuildInputs) ([]string, error) {
	if len(inputs.Defines) != 0 {
		return nil, errors.New("Go exporter does not interpret generic build defines; use Go build tags")
	}
	if len(inputs.IncludePaths) != 0 {
		return nil, errors.New("Go exporter cannot apply generic include paths; configure cgo environment flags explicitly")
	}
	flags, argumentTags, err := supportedGoBuildArguments(inputs.Arguments)
	if err != nil {
		return nil, err
	}
	tags := append(argumentTags, inputs.Features...)
	if optionTags := inputs.Options["go.tags"]; optionTags != "" {
		tags = append(tags, strings.Split(optionTags, ",")...)
	}
	for _, feature := range tags {
		feature = strings.TrimSpace(feature)
		if feature == "" || strings.ContainsAny(feature, " \t\r\n,") {
			return nil, fmt.Errorf("invalid Go build tag %q", feature)
		}
	}
	if len(tags) > 0 {
		flags = append(flags, "-tags="+strings.Join(tags, ","))
	}
	if mode := inputs.Options["go.mod"]; mode != "" {
		if mode != "readonly" && mode != "vendor" {
			return nil, fmt.Errorf("unsupported Go module mode %q", mode)
		}
		flags = append(flags, "-mod="+mode)
	} else {
		return nil, errors.New("Go module mode must be explicit for immutable source snapshots")
	}
	for key := range inputs.Options {
		if key != "go.tags" && key != "go.mod" && !strings.HasPrefix(key, "go.context.") {
			return nil, fmt.Errorf("unsupported Go build option %q", key)
		}
	}
	return flags, nil
}

// supportedGoBuildArguments only accepts build inputs that are represented by
// this provider's immutable scope. Path-bearing options and unknown arguments
// could make packages.Load read or execute inputs that module extraction and
// provenance do not capture.
func supportedGoBuildArguments(arguments []string) ([]string, []string, error) {
	var flags []string
	var tags []string
	for i := 0; i < len(arguments); i++ {
		argument := arguments[i]
		if !strings.HasPrefix(argument, "-") {
			return nil, nil, fmt.Errorf("Go semantic index: unsupported positional build argument %q", argument)
		}
		trimmed := strings.TrimLeft(argument, "-")
		name, value, hasValue := strings.Cut(trimmed, "=")
		if goFlagUsesExternalInput(name) {
			return nil, nil, fmt.Errorf("Go semantic index: unsupported external-path or execution build argument %q", argument)
		}
		switch name {
		case "tags", "mod":
			if !hasValue {
				if i+1 >= len(arguments) {
					return nil, nil, fmt.Errorf("Go semantic index: build argument %q has no value", argument)
				}
				i++
				value = arguments[i]
			}
			if name == "mod" {
				if value != "readonly" && value != "vendor" {
					return nil, nil, fmt.Errorf("Go semantic index: unsupported Go module mode %q", value)
				}
				continue // resolveGoModuleMode validates and applies this via Options.
			}
			tags = append(tags, strings.Split(value, ",")...)
		case "race", "msan", "asan":
			if hasValue {
				return nil, nil, fmt.Errorf("Go semantic index: build argument %q does not take a value", argument)
			}
			flags = append(flags, "-"+name)
		default:
			return nil, nil, fmt.Errorf("Go semantic index: unsupported Go build argument %q", argument)
		}
	}
	return flags, tags, nil
}

func resolveGoModuleMode(inputs model.BuildInputs, files *fileIndex, root string) (string, error) {
	mode := inputs.Options["go.mod"]
	for _, candidates := range [][]string{inputs.Arguments, strings.Fields(inputs.Environment["GOFLAGS"])} {
		argumentMode, err := goModModeFromFlags(candidates)
		if err != nil {
			return "", err
		}
		if argumentMode == "" {
			continue
		}
		if mode != "" && mode != argumentMode {
			return "", errors.New("conflicting Go module modes in environment, arguments, and options")
		}
		mode = argumentMode
	}
	if mode == "" {
		if _, vendored := files.byPath[pathKey(filepath.Join(root, "vendor", "modules.txt"))]; vendored {
			mode = "vendor"
		} else {
			mode = "readonly"
		}
	}
	if mode == "mod" {
		return "", errors.New("Go module mode mod may modify immutable go.mod or go.sum files")
	}
	if mode != "readonly" && mode != "vendor" {
		return "", fmt.Errorf("unsupported Go module mode %q", mode)
	}
	return mode, nil
}

func goModModeFromFlags(flags []string) (string, error) {
	mode := ""
	for i := 0; i < len(flags); i++ {
		flag := strings.TrimLeft(flags[i], "-")
		name, value, hasValue := strings.Cut(flag, "=")
		if name != "mod" {
			continue
		}
		if !hasValue {
			if i+1 >= len(flags) {
				return "", errors.New("Go -mod flag has no value")
			}
			i++
			value = flags[i]
		}
		value = strings.TrimSpace(value)
		if value != "readonly" && value != "vendor" && value != "mod" {
			return "", fmt.Errorf("unsupported Go module mode %q", value)
		}
		if mode != "" && mode != value {
			return "", errors.New("conflicting -mod flags")
		}
		mode = value
	}
	return mode, nil
}

func goPackageEnvironment(input map[string]string, root, goPath string, files *fileIndex, cacheRoot string) ([]string, error) {
	goos, hasGOOS := input["GOOS"]
	goarch, hasGOARCH := input["GOARCH"]
	if !hasGOOS || !hasGOARCH || strings.TrimSpace(goos) == "" || strings.TrimSpace(goarch) == "" {
		return nil, errors.New("Go build context must explicitly provide GOOS and GOARCH")
	}
	cgoEnabled, hasCGOEnabled := input["CGO_ENABLED"]
	if !hasCGOEnabled || (cgoEnabled != "0" && cgoEnabled != "1") {
		return nil, errors.New("Go build context must explicitly provide CGO_ENABLED as 0 or 1")
	}
	values := make(map[string]string)
	for _, key := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "HOME", "USERPROFILE", "GOROOT", "GOPATH", "GOMODCACHE", "GOCACHE"} {
		if value := os.Getenv(key); value != "" {
			values[key] = value
		}
	}
	for key, value := range input {
		if strings.ContainsAny(key, "=\x00") || strings.ContainsRune(value, '\x00') {
			return nil, fmt.Errorf("invalid environment entry %q", key)
		}
		values[key] = value
	}
	values["GOOS"] = goos
	values["GOARCH"] = goarch
	if cacheRoot == "" || pathWithinRoot(cacheRoot, root) {
		return nil, errors.New("Go compiler cache must be outside the immutable workspace")
	}
	cacheRoot, cacheErr := filepath.Abs(cacheRoot)
	if cacheErr != nil {
		return nil, fmt.Errorf("resolve external Go cache path: %w", cacheErr)
	}
	values["GOCACHE"] = filepath.Join(cacheRoot, "gocache")
	if moduleCache := values["GOMODCACHE"]; moduleCache != "" && pathWithinRoot(moduleCache, root) {
		values["GOMODCACHE"] = filepath.Join(cacheRoot, "modcache")
	}
	values["GOENV"] = "off"
	values["GOTOOLCHAIN"] = "local"
	if _, ok := input["GOPROXY"]; !ok {
		values["GOPROXY"] = "off"
	}
	if _, ok := input["GOFLAGS"]; !ok {
		values["GOFLAGS"] = ""
	}
	workFile := filepath.Join(root, "go.work")
	if entry, exists := files.byPath[pathKey(workFile)]; exists {
		if entry.file.URI == "" {
			return nil, errors.New("materialized go.work has no immutable URI")
		}
		if inputWork := input["GOWORK"]; inputWork == "off" {
			values["GOWORK"] = "off"
		} else {
			values["GOWORK"] = workFile
		}
	} else if inputWork := input["GOWORK"]; inputWork == "" || inputWork == "auto" || inputWork == "off" {
		values["GOWORK"] = "off"
	} else {
		return nil, errors.New("requested GOWORK is outside the immutable materialized root")
	}
	if goPath != "" {
		pathValue := values["PATH"]
		goDir := filepath.Dir(goPath)
		if pathValue == "" {
			values["PATH"] = goDir
		} else if !pathListContains(pathValue, goDir) {
			values["PATH"] = goDir + string(os.PathListSeparator) + pathValue
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}

func pathWithinRoot(path, root string) bool {
	pathAbs, pathErr := filepath.Abs(path)
	rootAbs, rootErr := filepath.Abs(root)
	if pathErr != nil || rootErr != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(pathAbs))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func pathListContains(pathList, candidate string) bool {
	for _, element := range filepath.SplitList(pathList) {
		if sameExecutablePath(element, candidate) {
			return true
		}
	}
	return false
}

func validateMaterializedRoot(view model.MaterializedView, expectedURI string) error {
	if view == nil || view.RootURI() != expectedURI || view.RootPath() == "" {
		return errors.New("root URI/path does not match the requested subtree")
	}
	root, err := filepath.Abs(view.RootPath())
	if err != nil {
		return err
	}
	info, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("root path is not a directory")
	}
	return nil
}

type completeness struct {
	state  model.Completeness
	reason string
}

func (r *loadResult) coverage() map[model.FactKind]completeness {
	out := make(map[model.FactKind]completeness, len(model.RequiredFactKinds))
	for _, fact := range model.RequiredFactKinds {
		if fact == model.FactInclude {
			out[fact] = completeness{model.Unavailable, "Go has no language-level include relation; cgo preamble includes are external C inputs"}
			continue
		}
		if fact == model.FactGenerated {
			if reason := r.partial[fact]; reason != "" {
				out[fact] = completeness{model.IncompleteKnownSubset, reason}
			} else {
				out[fact] = completeness{model.Complete, ""}
			}
			continue
		}
		if reason := r.partial[fact]; reason != "" {
			out[fact] = completeness{model.IncompleteKnownSubset, reason}
		} else {
			out[fact] = completeness{model.Complete, ""}
		}
	}
	return out
}

func (r *loadResult) extract() {
	if r.batch == nil {
		return
	}
	if r.packageErr != nil {
		for _, fact := range []model.FactKind{
			model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference,
			model.FactImplementation, model.FactTypeRelation, model.FactCall, model.FactImport,
			model.FactModule, model.FactGenerated,
		} {
			r.partial[fact] = "go/packages reported incomplete loading: " + r.packageErr.Error()
		}
	}
	rootPackages := make([]*packages.Package, 0, len(r.packages))
	seenPackage := make(map[string]struct{}, len(r.packages))
	for _, pkg := range r.packages {
		if pkg == nil {
			continue
		}
		if _, exists := seenPackage[pkg.ID]; exists {
			continue
		}
		seenPackage[pkg.ID] = struct{}{}
		rootPackages = append(rootPackages, pkg)
		r.markUnpinnedCgoInputs(pkg)
		if len(pkg.Errors) != 0 {
			reason := "type checking reported package errors: " + pkg.Errors[0].Msg
			for _, fact := range []model.FactKind{
				model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference,
				model.FactImplementation, model.FactTypeRelation, model.FactCall, model.FactImport,
				model.FactModule,
			} {
				if r.partial[fact] == "" {
					r.partial[fact] = reason
				}
			}
		}
		if pkg.Types == nil || pkg.TypesInfo == nil || len(pkg.Syntax) == 0 {
			r.partial[model.FactSymbol] = "one or more requested packages did not produce type-checked syntax"
			r.partial[model.FactDeclaration] = r.partial[model.FactSymbol]
			r.partial[model.FactDefinition] = r.partial[model.FactSymbol]
			r.partial[model.FactReference] = r.partial[model.FactSymbol]
			r.partial[model.FactImplementation] = r.partial[model.FactSymbol]
			r.partial[model.FactTypeRelation] = r.partial[model.FactSymbol]
			r.partial[model.FactCall] = r.partial[model.FactSymbol]
			r.partial[model.FactImport] = r.partial[model.FactSymbol]
			continue
		}
		r.exportPackageFacts(pkg)
	}
	sort.Slice(rootPackages, func(i, j int) bool {
		if rootPackages[i].PkgPath != rootPackages[j].PkgPath {
			return rootPackages[i].PkgPath < rootPackages[j].PkgPath
		}
		return rootPackages[i].ID < rootPackages[j].ID
	})
	r.exportTypeRelationships(rootPackages)
	r.exportImplementations(rootPackages)
	r.exportImports(rootPackages)
	r.exportModules(rootPackages)
	r.exportGeneratedSources()
}

func (r *loadResult) markUnpinnedCgoInputs(pkg *packages.Package) {
	if r.scope.Build.Environment["CGO_ENABLED"] != "1" || pkg == nil {
		return
	}
	for _, file := range pkg.Syntax {
		if file == nil {
			continue
		}
		for _, spec := range file.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil || path != "C" {
				continue
			}
			reason := "cgo compiler and external C headers are not pinned to immutable build inputs"
			for _, fact := range []model.FactKind{
				model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference,
				model.FactImplementation, model.FactTypeRelation, model.FactCall, model.FactImport,
				model.FactGenerated,
			} {
				if r.partial[fact] == "" {
					r.partial[fact] = reason
				}
			}
			return
		}
	}
}

func (r *loadResult) exportPackageFacts(pkg *packages.Package) {
	packageID := identity.SymbolID("go:package:" + pkg.PkgPath)
	r.addSymbol(model.Symbol{ID: packageID, ScopeID: r.scope.ID, Name: pkg.Name, Kind: "package", Signature: pkg.PkgPath})
	for _, file := range pkg.Syntax {
		path := pkg.Fset.Position(file.Pos()).Filename
		entry, ok := r.files.byPath[pathKey(path)]
		if !ok {
			reason := "type-checked syntax contains a generated or external file outside the immutable view"
			r.partial[model.FactSymbol] = reason
			for _, fact := range []model.FactKind{model.FactDeclaration, model.FactDefinition, model.FactReference, model.FactCall, model.FactImport, model.FactGenerated} {
				r.partial[fact] = "type-checked syntax contains a file outside the immutable view"
			}
			continue
		}
		if filepath.Ext(entry.path) != ".go" {
			continue
		}
		source, sourceErr := os.ReadFile(entry.path)
		if sourceErr != nil {
			r.partial[model.FactReference] = "cannot read compiler-checked source from immutable materialization: " + sourceErr.Error()
			continue
		}
		if pkg.TypesInfo == nil {
			continue
		}
		info := pkg.TypesInfo
		identObjects := make(map[*ast.Ident]types.Object, len(info.Defs)+len(info.Uses))
		for ident, obj := range info.Defs {
			if obj != nil {
				identObjects[ident] = obj
			}
		}
		for ident, obj := range info.Uses {
			if obj != nil {
				identObjects[ident] = obj
			}
		}
		for selector, selection := range info.Selections {
			if selection != nil && selector.Sel != nil {
				identObjects[selector.Sel] = selection.Obj()
			}
		}
		idents := make([]*ast.Ident, 0, len(identObjects))
		for ident := range identObjects {
			idents = append(idents, ident)
		}
		sort.Slice(idents, func(i, j int) bool {
			if idents[i].Pos() != idents[j].Pos() {
				return idents[i].Pos() < idents[j].Pos()
			}
			return idents[i].End() < idents[j].End()
		})
		for _, ident := range idents {
			if !sameExecutablePath(pkg.Fset.PositionFor(ident.Pos(), false).Filename, entry.path) {
				continue
			}
			obj := identObjects[ident]
			if err := r.ctx.Err(); err != nil {
				r.partial[model.FactReference] = "extraction canceled while traversing typed identifiers"
				return
			}
			id := r.symbolID(obj, pkg.Fset, pkg.PkgPath)
			r.addSymbol(symbolForObject(id, r.scope.ID, obj))
			rng, ok := sourceRange(pkg.Fset, file, source, ident.Pos(), ident.End())
			if !ok {
				r.partial[model.FactReference] = "typed identifier has no valid source range"
				continue
			}
			role := "reference"
			if info.Defs[ident] != nil {
				role = "definition"
			}
			r.addOccurrence(model.Occurrence{
				SymbolID: id, ScopeID: r.scope.ID, URI: entry.file.URI,
				Range: rng, Role: role, SourceHash: entry.hash, BuildContext: r.scope.BuildContext,
			})
		}
		instanceIdents := make([]*ast.Ident, 0, len(info.Instances))
		for ident := range info.Instances {
			instanceIdents = append(instanceIdents, ident)
		}
		sort.Slice(instanceIdents, func(i, j int) bool { return instanceIdents[i].Pos() < instanceIdents[j].Pos() })
		for _, ident := range instanceIdents {
			if !sameExecutablePath(pkg.Fset.PositionFor(ident.Pos(), false).Filename, entry.path) {
				continue
			}
			instance := info.Instances[ident]
			if instance.TypeArgs == nil || instance.TypeArgs.Len() == 0 {
				continue
			}
			obj := info.Uses[ident]
			if obj == nil {
				obj = info.Defs[ident]
			}
			if obj == nil {
				continue
			}
			from := r.symbolID(obj, pkg.Fset, pkg.PkgPath)
			rng, ok := sourceRange(pkg.Fset, file, source, ident.Pos(), ident.End())
			if !ok {
				continue
			}
			for i := 0; i < instance.TypeArgs.Len(); i++ {
				for _, target := range r.typeSymbolIDs(instance.TypeArgs.At(i)) {
					r.addEdge(model.Edge{
						From: from, To: target, ScopeID: r.scope.ID, Kind: model.EdgeTypeRelation,
						SourceURI: entry.file.URI, Range: rng, SourceHash: entry.hash, BuildContext: r.scope.BuildContext,
					})
				}
			}
		}
		r.exportCalls(pkg, file, entry, source)
	}
}

func (r *loadResult) exportCalls(pkg *packages.Package, file *ast.File, entry indexedFile, source []byte) {
	if pkg.TypesInfo == nil {
		return
	}
	visitor := callIndexVisitor{result: r, pkg: pkg, file: file, entry: entry, source: source, owner: identity.SymbolID("go:package:" + pkg.PkgPath)}
	ast.Walk(visitor, file)
}

type callIndexVisitor struct {
	result *loadResult
	pkg    *packages.Package
	file   *ast.File
	entry  indexedFile
	source []byte
	owner  identity.SymbolID
}

func (v callIndexVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil {
		return nil
	}
	switch n := node.(type) {
	case *ast.FuncDecl:
		if n.Name != nil {
			if obj := v.pkg.TypesInfo.Defs[n.Name]; obj != nil {
				v.owner = v.result.symbolID(obj, v.pkg.Fset, v.pkg.PkgPath)
			}
		}
	case *ast.FuncLit:
		// go/types deliberately gives function literals no object identity. Do
		// not synthesize one from a token range; calls in a closure are retained
		// under their nearest named/function owner and reported as a subset.
		v.result.partial[model.FactCall] = "anonymous function values have no package-stable compiler object identity"
	case *ast.CallExpr:
		v.recordCall(n)
	}
	return v
}

func (v callIndexVisitor) recordCall(call *ast.CallExpr) {
	info := v.pkg.TypesInfo
	if typ, ok := info.Types[call.Fun]; ok && typ.IsType() {
		return // a conversion is a type relation, not a call edge
	}
	var obj types.Object
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		obj = info.Uses[fun]
	case *ast.SelectorExpr:
		if selection := info.Selections[fun]; selection != nil {
			obj = selection.Obj()
			if typeHasDynamicMethodSet(selection.Recv()) {
				v.result.partial[model.FactCall] = "one or more method calls dispatch through an interface or type parameter"
			}
		} else {
			obj = info.Uses[fun.Sel]
		}
	}
	if obj == nil {
		v.result.partial[model.FactCall] = "one or more call targets are not statically resolved by go/types"
		return
	}
	if _, ok := obj.(*types.Func); !ok {
		if _, builtin := obj.(*types.Builtin); !builtin {
			v.result.partial[model.FactCall] = "one or more calls invoke a function-valued object with a dynamic target"
			return
		}
	}
	rng, ok := sourceRange(v.pkg.Fset, v.file, v.source, call.Pos(), call.End())
	if !ok {
		v.result.partial[model.FactCall] = "resolved call has no valid source range"
		return
	}
	to := v.result.symbolID(obj, v.pkg.Fset, v.pkg.PkgPath)
	v.result.addSymbol(symbolForObject(to, v.result.scope.ID, obj))
	v.result.addEdge(model.Edge{
		From: v.owner, To: to, ScopeID: v.result.scope.ID, Kind: model.EdgeCall,
		SourceURI: v.entry.file.URI, Range: rng, SourceHash: v.entry.hash,
		BuildContext: v.result.scope.BuildContext,
	})
}

func typeHasDynamicMethodSet(typ types.Type) bool {
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = pointer.Elem()
	}
	if parameter, ok := typ.(*types.TypeParam); ok {
		typ = parameter.Constraint()
	}
	_, isInterface := interfaceType(typ)
	return isInterface
}

func (r *loadResult) exportTypeRelationships(pkgs []*packages.Package) {
	for _, pkg := range pkgs {
		if pkg == nil || pkg.Types == nil || pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			path := pkg.Fset.Position(file.Pos()).Filename
			entry, ok := r.files.byPath[pathKey(path)]
			if !ok {
				continue
			}
			source, sourceErr := os.ReadFile(entry.path)
			if sourceErr != nil {
				r.partial[model.FactTypeRelation] = "cannot read compiler-checked source from immutable materialization: " + sourceErr.Error()
				continue
			}
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						typeSpec, ok := spec.(*ast.TypeSpec)
						if !ok || typeSpec.Name == nil {
							continue
						}
						obj := pkg.TypesInfo.Defs[typeSpec.Name]
						if obj == nil {
							continue
						}
						from := r.symbolID(obj, pkg.Fset, pkg.PkgPath)
						for _, target := range r.typeSymbolIDs(obj.Type()) {
							if target == from {
								continue
							}
							rng, rangeOK := sourceRange(pkg.Fset, file, source, typeSpec.Type.Pos(), typeSpec.Type.End())
							if !rangeOK {
								r.partial[model.FactTypeRelation] = "type relation has no valid source range"
								continue
							}
							r.addEdge(model.Edge{From: from, To: target, ScopeID: r.scope.ID, Kind: model.EdgeTypeRelation,
								SourceURI: entry.file.URI, Range: rng, SourceHash: entry.hash, BuildContext: r.scope.BuildContext})
						}
					}
				case *ast.FuncDecl:
					if d.Name == nil {
						continue
					}
					obj := pkg.TypesInfo.Defs[d.Name]
					if obj == nil {
						continue
					}
					from := r.symbolID(obj, pkg.Fset, pkg.PkgPath)
					for _, target := range r.typeSymbolIDs(obj.Type()) {
						if target == from {
							continue
						}
						rng, rangeOK := sourceRange(pkg.Fset, file, source, d.Type.Pos(), d.Type.End())
						if !rangeOK {
							r.partial[model.FactTypeRelation] = "signature type relation has no valid source range"
							continue
						}
						r.addEdge(model.Edge{From: from, To: target, ScopeID: r.scope.ID, Kind: model.EdgeTypeRelation,
							SourceURI: entry.file.URI, Range: rng, SourceHash: entry.hash, BuildContext: r.scope.BuildContext})
					}
				}
			}
		}
	}
}

type indexedGoType struct {
	obj    *types.TypeName
	typ    types.Type
	pkg    *packages.Package
	file   *ast.File
	ident  *ast.Ident
	entry  indexedFile
	source []byte
}

func (r *loadResult) exportImplementations(pkgs []*packages.Package) {
	var concrete []indexedGoType
	var interfaces []indexedGoType
	seenTypes := make(map[string]struct{})
	seenInterfaces := make(map[string]struct{})
	inScopePackages := make(map[string]struct{}, len(pkgs))
	for _, pkg := range pkgs {
		if pkg != nil && pkg.PkgPath != "" {
			inScopePackages[pkg.PkgPath] = struct{}{}
		}
	}
	for _, pkg := range pkgs {
		if pkg == nil || pkg.Types == nil || pkg.TypesInfo == nil {
			continue
		}
		for _, name := range pkg.Types.Scope().Names() {
			obj, ok := pkg.Types.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			if named, ok := types.Unalias(obj.Type()).(*types.Named); ok && named.TypeParams() != nil && named.TypeParams().Len() > 0 {
				r.partial[model.FactImplementation] = "generic named declarations without a finite set of observed instantiations prevent exhaustive implementation coverage"
			}
			var declaredFile *ast.File
			var declaredIdent *ast.Ident
			for _, file := range pkg.Syntax {
				ast.Inspect(file, func(node ast.Node) bool {
					id, ok := node.(*ast.Ident)
					if ok && pkg.TypesInfo.Defs[id] == obj {
						declaredFile = file
						declaredIdent = id
						return false
					}
					return declaredIdent == nil
				})
				if declaredIdent != nil {
					break
				}
			}
			if declaredFile == nil || declaredIdent == nil {
				continue
			}
			path := pkg.Fset.Position(declaredIdent.Pos()).Filename
			entry, ok := r.files.byPath[pathKey(path)]
			if !ok {
				continue
			}
			source, sourceErr := os.ReadFile(entry.path)
			if sourceErr != nil {
				r.partial[model.FactImplementation] = "cannot read type declaration from immutable materialization: " + sourceErr.Error()
				continue
			}
			typ := types.Unalias(obj.Type())
			id := r.symbolID(obj, pkg.Fset, pkg.PkgPath)
			item := indexedGoType{obj: obj, typ: typ, pkg: pkg, file: declaredFile, ident: declaredIdent, entry: entry, source: source}
			if _, isInterface := interfaceType(typ); isInterface {
				key := string(id) + "\x00" + types.TypeString(typ, packageQualifier)
				if _, exists := seenInterfaces[key]; exists {
					continue
				}
				seenInterfaces[key] = struct{}{}
				interfaces = append(interfaces, item)
			} else {
				key := string(id) + "\x00" + types.TypeString(typ, packageQualifier)
				if _, exists := seenTypes[key]; exists {
					continue
				}
				seenTypes[key] = struct{}{}
				concrete = append(concrete, item)
			}
		}
		instanceIdents := make([]*ast.Ident, 0, len(pkg.TypesInfo.Instances))
		for ident := range pkg.TypesInfo.Instances {
			instanceIdents = append(instanceIdents, ident)
		}
		sort.Slice(instanceIdents, func(i, j int) bool { return instanceIdents[i].Pos() < instanceIdents[j].Pos() })
		for _, ident := range instanceIdents {
			instance := pkg.TypesInfo.Instances[ident]
			obj, ok := pkg.TypesInfo.Uses[ident].(*types.TypeName)
			if !ok || obj.Pkg() == nil || instance.TypeArgs == nil || instance.TypeArgs.Len() == 0 {
				continue
			}
			if _, inScope := inScopePackages[obj.Pkg().Path()]; !inScope {
				continue
			}
			typ := types.Unalias(instance.Type)
			if _, ok := typ.(*types.Named); !ok {
				continue
			}
			id := r.symbolID(obj, pkg.Fset, obj.Pkg().Path())
			key := string(id) + "\x00" + types.TypeString(typ, packageQualifier)
			var target *[]indexedGoType
			if _, isInterface := interfaceType(typ); isInterface {
				target = &interfaces
				if _, exists := seenInterfaces[key]; exists {
					continue
				}
				seenInterfaces[key] = struct{}{}
			} else {
				target = &concrete
				if _, exists := seenTypes[key]; exists {
					continue
				}
				seenTypes[key] = struct{}{}
			}
			path := pkg.Fset.Position(ident.Pos()).Filename
			entry, ok := r.files.byPath[pathKey(path)]
			if !ok {
				continue
			}
			var instanceFile *ast.File
			for _, file := range pkg.Syntax {
				if file.Pos() <= ident.Pos() && ident.Pos() <= file.End() {
					instanceFile = file
					break
				}
			}
			if instanceFile == nil {
				continue
			}
			source, err := os.ReadFile(entry.path)
			if err != nil {
				r.partial[model.FactImplementation] = "cannot read generic instantiation from immutable materialization: " + err.Error()
				continue
			}
			*target = append(*target, indexedGoType{obj: obj, typ: typ, pkg: pkg, file: instanceFile, ident: ident, entry: entry, source: source})
		}
	}
	// Include interfaces from each direct imported package so local concrete
	// types can be matched against dependency contracts.
	seenPackages := make(map[string]struct{})
	var addImported func(*types.Package)
	addImported = func(pkg *types.Package) {
		if pkg == nil {
			return
		}
		if _, seen := seenPackages[pkg.Path()]; seen {
			return
		}
		seenPackages[pkg.Path()] = struct{}{}
		imports := append([]*types.Package(nil), pkg.Imports()...)
		sort.Slice(imports, func(i, j int) bool { return imports[i].Path() < imports[j].Path() })
		for _, imported := range imports {
			addImported(imported)
		}
		for _, name := range pkg.Scope().Names() {
			obj, ok := pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			if _, ok := interfaceType(types.Unalias(obj.Type())); !ok {
				continue
			}
			id := objectSymbolID(obj, nil, "", pkg.Path())
			key := string(id) + "\x00" + types.TypeString(types.Unalias(obj.Type()), packageQualifier)
			if _, exists := seenInterfaces[key]; exists {
				continue
			}
			seenInterfaces[key] = struct{}{}
			interfaces = append(interfaces, indexedGoType{obj: obj, typ: types.Unalias(obj.Type())})
		}
	}
	for _, pkg := range pkgs {
		if pkg != nil && pkg.Types != nil {
			imports := append([]*types.Package(nil), pkg.Types.Imports()...)
			sort.Slice(imports, func(i, j int) bool { return imports[i].Path() < imports[j].Path() })
			for _, imported := range imports {
				addImported(imported)
			}
		}
	}
	for _, implementer := range concrete {
		for _, iface := range interfaces {
			if iface.typ == nil {
				continue
			}
			it, ok := interfaceType(iface.typ)
			if !ok {
				continue
			}
			it.Complete()
			if types.Implements(implementer.typ, it) {
				r.emitImplementation(implementer, iface)
			}
			if _, isPointer := implementer.typ.(*types.Pointer); !isPointer && types.Implements(types.NewPointer(implementer.typ), it) {
				r.emitImplementation(implementer, iface)
			}
		}
	}
}

func interfaceType(typ types.Type) (*types.Interface, bool) {
	typ = types.Unalias(typ)
	if named, ok := typ.(*types.Named); ok {
		typ = named.Underlying()
	}
	iface, ok := typ.(*types.Interface)
	return iface, ok
}

func (r *loadResult) emitImplementation(implementer, iface indexedGoType) {
	from := r.symbolID(implementer.obj, nil, implementer.obj.Pkg().Path())
	toPkg := ""
	if iface.obj.Pkg() != nil {
		toPkg = iface.obj.Pkg().Path()
	}
	to := r.symbolID(iface.obj, nil, toPkg)
	rng := model.Position{}
	if implementer.pkg != nil && implementer.file != nil {
		if source, ok := sourceRange(implementer.pkg.Fset, implementer.file, implementer.source, implementer.ident.Pos(), implementer.ident.End()); ok {
			rng = source
		}
	}
	r.addSymbol(symbolForObject(from, r.scope.ID, implementer.obj))
	r.addSymbol(symbolForObject(to, r.scope.ID, iface.obj))
	r.addEdge(model.Edge{From: from, To: to, ScopeID: r.scope.ID, Kind: model.EdgeImplementation,
		SourceURI: implementer.entry.file.URI, Range: rng, SourceHash: implementer.entry.hash,
		BuildContext: r.scope.BuildContext})
}

func (r *loadResult) exportImports(pkgs []*packages.Package) {
	for _, pkg := range pkgs {
		if pkg == nil || pkg.Types == nil || pkg.TypesInfo == nil {
			continue
		}
		packageID := identity.SymbolID("go:package:" + pkg.PkgPath)
		for _, file := range pkg.Syntax {
			path := pkg.Fset.Position(file.Pos()).Filename
			entry, ok := r.files.byPath[pathKey(path)]
			if !ok {
				continue
			}
			source, sourceErr := os.ReadFile(entry.path)
			if sourceErr != nil {
				r.partial[model.FactImport] = "cannot read compiler-checked source from immutable materialization: " + sourceErr.Error()
				continue
			}
			for _, spec := range file.Imports {
				literal, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					r.partial[model.FactImport] = "malformed import path in compiler-checked source"
					continue
				}
				imported := pkg.Imports[literal]
				if imported == nil || imported.Types == nil {
					r.partial[model.FactImport] = "go/packages did not resolve an import target"
					continue
				}
				to := identity.SymbolID("go:package:" + imported.PkgPath)
				r.addSymbol(model.Symbol{ID: to, ScopeID: r.scope.ID, Name: imported.Name, Kind: "package", Signature: imported.PkgPath})
				rng, ok := sourceRange(pkg.Fset, file, source, spec.Pos(), spec.End())
				if !ok {
					r.partial[model.FactImport] = "resolved import has no valid source range"
					continue
				}
				r.addEdge(model.Edge{From: packageID, To: to, ScopeID: r.scope.ID, Kind: model.EdgeImport,
					SourceURI: entry.file.URI, Range: rng, SourceHash: entry.hash, BuildContext: r.scope.BuildContext})
			}
		}
	}
}

type moduleInfo struct {
	Path    string
	Version string
	Main    bool
	Dir     string
	GoMod   string
	Replace *moduleInfo
	Error   *struct{ Err string }
}

func commandFailure(err error, stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err.Error()
	}
	return err.Error() + ": " + stderr
}

func (r *loadResult) exportModules(pkgs []*packages.Package) {
	root := filepath.Clean(r.root)
	r.exportPackageModuleEdges(pkgs)
	moduleRoot := root
	for _, pkg := range pkgs {
		if pkg != nil && pkg.Module != nil && pkg.Module.Main && pkg.Module.Dir != "" {
			moduleRoot = pkg.Module.Dir
			break
		}
	}
	graphSource := r.moduleSourcePath(filepath.Join(root, "go.work"), filepath.Join(moduleRoot, "go.mod"), filepath.Join(root, "go.mod"))
	cmd := exec.CommandContext(r.ctx, r.tool.path, "list", "-m", "-json", "all")
	cmd.Dir = moduleRoot
	cmd.Env = append([]string(nil), r.env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		r.partial[model.FactModule] = "go list could not resolve the module graph: " + commandFailure(err, stderr.String())
		r.exportKnownModuleRequirements()
		return
	}
	decoder := json.NewDecoder(&stdout)
	modules := make(map[string]moduleInfo)
	for {
		var mod moduleInfo
		if err := decoder.Decode(&mod); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			r.partial[model.FactModule] = "go list returned malformed module graph data"
			r.exportKnownModuleRequirements()
			return
		}
		if mod.Path == "" || mod.Error != nil {
			r.partial[model.FactModule] = "go list returned an unresolved module"
			continue
		}
		modules[mod.Path+"@"+mod.Version] = mod
		id := moduleSymbolID(mod.Path, mod.Version, mod.Main)
		name := mod.Path
		if mod.Version != "" {
			name += "@" + mod.Version
		}
		r.addSymbol(model.Symbol{ID: id, ScopeID: r.scope.ID, Name: name, Kind: "module", Signature: moduleSignature(mod)})
		if mod.Replace != nil {
			replaceID := moduleSymbolID(mod.Replace.Path, mod.Replace.Version, mod.Replace.Main)
			r.addSymbol(model.Symbol{ID: replaceID, ScopeID: r.scope.ID, Name: mod.Replace.Path, Kind: "module", Signature: moduleSignature(*mod.Replace)})
			r.addModuleEdge(id, replaceID, mod.GoMod, graphSource)
		}
	}
	graphCmd := exec.CommandContext(r.ctx, r.tool.path, "mod", "graph")
	graphCmd.Dir = moduleRoot
	graphCmd.Env = append([]string(nil), r.env...)
	stdout.Reset()
	stderr.Reset()
	graphCmd.Stdout, graphCmd.Stderr = &stdout, &stderr
	err = graphCmd.Run()
	if err != nil {
		r.partial[model.FactModule] = "go mod graph could not enumerate module requirements: " + commandFailure(err, stderr.String())
		r.exportKnownModuleRequirements()
		return
	}
	scanner := bufio.NewScanner(&stdout)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			r.partial[model.FactModule] = "go mod graph returned an invalid edge"
			continue
		}
		from := moduleIDFromGraph(fields[0], modules)
		to := moduleIDFromGraph(fields[1], modules)
		r.addSymbol(model.Symbol{ID: from, ScopeID: r.scope.ID, Name: string(from), Kind: "module"})
		r.addSymbol(model.Symbol{ID: to, ScopeID: r.scope.ID, Name: string(to), Kind: "module"})
		r.addModuleEdge(from, to, graphSource, "")
	}
	if err := scanner.Err(); err != nil {
		r.partial[model.FactModule] = "read module graph: " + err.Error()
	}
	r.exportKnownModuleRequirements()
}

func (r *loadResult) exportPackageModuleEdges(pkgs []*packages.Package) {
	fallback := r.moduleSourcePath(filepath.Join(r.root, "go.work"), filepath.Join(r.root, "go.mod"))
	for _, pkg := range pkgs {
		if pkg == nil || pkg.Module == nil {
			continue
		}
		pkgID := identity.SymbolID("go:package:" + pkg.PkgPath)
		modID := moduleSymbolID(pkg.Module.Path, pkg.Module.Version, pkg.Module.Main)
		r.addSymbol(model.Symbol{ID: modID, ScopeID: r.scope.ID, Name: pkg.Module.Path, Kind: "module", Signature: pkg.Module.GoVersion})
		r.addModuleEdge(pkgID, modID, pkg.Module.GoMod, fallback)
	}
}

func (r *loadResult) moduleSourcePath(candidates ...string) string {
	if r.files == nil {
		return ""
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if entry, ok := r.files.byPath[pathKey(candidate)]; ok {
			return entry.path
		}
	}
	return ""
}

func (r *loadResult) addModuleEdge(from, to identity.SymbolID, sourcePath, fallbackPath string) {
	if r.files == nil {
		r.partial[model.FactModule] = "module relationship has no immutable source manifest"
		return
	}
	source, ok := r.files.byPath[pathKey(sourcePath)]
	if !ok && fallbackPath != "" {
		source, ok = r.files.byPath[pathKey(fallbackPath)]
	}
	if !ok {
		r.partial[model.FactModule] = "module relationship has no immutable source manifest"
		return
	}
	r.addEdge(model.Edge{
		From: from, To: to, ScopeID: r.scope.ID, Kind: model.EdgeModule,
		SourceURI: source.file.URI, SourceHash: source.hash, BuildContext: r.scope.BuildContext,
	})
}

type goModuleManifest struct {
	root string
	file string
	mod  *modfile.File
}

// exportKnownModuleRequirements emits the exact direct require and replace
// facts available in immutable go.mod/go.work inputs if Go cannot resolve the
// complete graph. Coverage remains incomplete because direct manifests do not
// prove the selected transitive MVS graph.
func (r *loadResult) exportKnownModuleRequirements() {
	manifests, err := r.knownModuleManifests()
	if err != nil && r.partial[model.FactModule] == "" {
		r.partial[model.FactModule] = err.Error()
	}
	byRoot := make(map[string]goModuleManifest, len(manifests))
	for _, manifest := range manifests {
		if manifest.mod.Module == nil || manifest.mod.Module.Mod.Path == "" {
			continue
		}
		modulePath := manifest.mod.Module.Mod.Path
		byRoot[pathKey(manifest.root)] = manifest
		id := moduleSymbolID(modulePath, "", true)
		r.addSymbol(model.Symbol{ID: id, ScopeID: r.scope.ID, Name: modulePath, Kind: "module", Signature: "main; gomod=" + filepath.Base(manifest.file)})
	}
	for _, manifest := range manifests {
		if manifest.mod.Module == nil || manifest.mod.Module.Mod.Path == "" {
			continue
		}
		fromID := moduleSymbolID(manifest.mod.Module.Mod.Path, "", true)
		for _, requirement := range manifest.mod.Require {
			toPath, version := requirement.Mod.Path, requirement.Mod.Version
			toID := moduleSymbolID(toPath, version, false)
			signature := "require"
			if requirement.Indirect {
				signature += "; indirect"
			}
			r.addSymbol(model.Symbol{ID: toID, ScopeID: r.scope.ID, Name: toPath + "@" + version, Kind: "module", Signature: signature})
			r.addModuleEdge(fromID, toID, manifest.file, "")
		}
		for _, replacement := range manifest.mod.Replace {
			oldPath, oldVersion := replacement.Old.Path, replacement.Old.Version
			from := moduleSymbolID(oldPath, oldVersion, oldVersion == "")
			newPath, newVersion := replacement.New.Path, replacement.New.Version
			to := moduleSymbolID(newPath, newVersion, newVersion == "")
			if newVersion == "" && !filepath.IsAbs(newPath) {
				replacementRoot := filepath.Clean(filepath.Join(manifest.root, filepath.FromSlash(newPath)))
				if local, ok := byRoot[pathKey(replacementRoot)]; ok && local.mod.Module != nil {
					newPath = local.mod.Module.Mod.Path
					to = moduleSymbolID(newPath, "", true)
				} else {
					rel, relErr := filepath.Rel(r.root, replacementRoot)
					if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
						r.partial[model.FactModule] = "go.mod contains a local replacement outside the immutable workspace"
						continue
					}
					newPath = "local:" + filepath.ToSlash(rel)
					to = moduleSymbolID(newPath, "", true)
				}
			}
			r.addSymbol(model.Symbol{ID: from, ScopeID: r.scope.ID, Name: oldPath, Kind: "module"})
			r.addSymbol(model.Symbol{ID: to, ScopeID: r.scope.ID, Name: newPath, Kind: "module", Signature: "replacement"})
			r.addModuleEdge(from, to, manifest.file, "")
		}
	}
}

func (r *loadResult) knownModuleManifests() ([]goModuleManifest, error) {
	var roots []string
	workPath := filepath.Join(r.root, "go.work")
	if entry, ok := r.files.byPath[pathKey(workPath)]; ok {
		data, err := os.ReadFile(entry.path)
		if err != nil {
			return nil, fmt.Errorf("read immutable go.work module inputs: %w", err)
		}
		workFile, err := modfile.ParseWork(entry.path, data, nil)
		if err != nil {
			return nil, fmt.Errorf("parse immutable go.work module inputs: %w", err)
		}
		for _, use := range workFile.Use {
			root := filepath.Clean(filepath.Join(r.root, filepath.FromSlash(use.Path)))
			rel, relErr := filepath.Rel(r.root, root)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				return nil, fmt.Errorf("go.work module path %q escapes the immutable workspace", use.Path)
			}
			roots = append(roots, root)
		}
	} else if _, ok := r.files.byPath[pathKey(filepath.Join(r.root, "go.mod"))]; ok {
		roots = append(roots, r.root)
	}
	sort.Strings(roots)
	manifests := make([]goModuleManifest, 0, len(roots))
	for _, root := range roots {
		manifestPath := filepath.Join(root, "go.mod")
		entry, ok := r.files.byPath[pathKey(manifestPath)]
		if !ok {
			return manifests, fmt.Errorf("workspace module %q has no immutable go.mod", root)
		}
		data, err := os.ReadFile(entry.path)
		if err != nil {
			return manifests, fmt.Errorf("read immutable go.mod: %w", err)
		}
		parsed, err := modfile.Parse(entry.path, data, nil)
		if err != nil {
			return manifests, fmt.Errorf("parse immutable go.mod %q: %w", entry.path, err)
		}
		manifests = append(manifests, goModuleManifest{root: root, file: entry.path, mod: parsed})
	}
	return manifests, nil
}

func moduleSignature(mod moduleInfo) string {
	parts := []string{}
	if mod.Main {
		parts = append(parts, "main")
	}
	if mod.GoMod != "" {
		parts = append(parts, "gomod="+filepath.Base(mod.GoMod))
	}
	return strings.Join(parts, ";")
}

func moduleSymbolID(path, version string, main bool) identity.SymbolID {
	if main || version == "" {
		return identity.SymbolID("go:module:" + path + "@workspace")
	}
	return identity.SymbolID("go:module:" + path + "@" + version)
}

func moduleIDFromGraph(value string, modules map[string]moduleInfo) identity.SymbolID {
	path, version, hasVersion := strings.Cut(value, "@")
	if !hasVersion || version == "" {
		return moduleSymbolID(path, "", true)
	}
	if mod, ok := modules[value]; ok {
		return moduleSymbolID(path, version, mod.Main)
	}
	return moduleSymbolID(path, version, false)
}

func (r *loadResult) exportGeneratedSources() {
	for _, file := range r.generated {
		if file.URI == "" || file.SourceURI == "" && len(file.SourceMap) == 0 {
			r.partial[model.FactGenerated] = "generated file lacks a source URI or source map"
			continue
		}
		generatedID := fileSymbolID(file.URI)
		r.addSymbol(model.Symbol{ID: generatedID, ScopeID: r.scope.ID, Name: filepath.Base(uriToPath(file.URI)), Kind: "generated_file"})
		if len(file.SourceMap) > 0 {
			generatedSource, sourceErr := os.ReadFile(r.files.byURI[file.URI].path)
			if sourceErr != nil {
				r.partial[model.FactGenerated] = "cannot read generated source from immutable materialization: " + sourceErr.Error()
				continue
			}
			for _, span := range file.SourceMap {
				source, exists := r.files.byURI[span.SourceURI]
				if span.SourceURI == "" || !exists {
					r.partial[model.FactGenerated] = "generated source map refers to a source URI outside the immutable view"
					continue
				}
				sourceBytes, err := os.ReadFile(source.path)
				if err != nil || !validSourceMapRange(generatedSource, span.Generated) || !validSourceMapRange(sourceBytes, span.Source) {
					r.partial[model.FactGenerated] = "generated source map contains an invalid or unreadable source range"
					continue
				}
				sourceID := fileSymbolID(span.SourceURI)
				r.addSymbol(model.Symbol{ID: sourceID, ScopeID: r.scope.ID, Name: filepath.Base(uriToPath(span.SourceURI)), Kind: "source_file"})
				r.addEdge(model.Edge{From: generatedID, To: sourceID, ScopeID: r.scope.ID, Kind: model.EdgeGenerated,
					SourceURI: file.URI, Range: span.Generated, SourceHash: fileHash(r.files, file.URI), BuildContext: r.scope.BuildContext})
			}
			continue
		}
		sourceID := fileSymbolID(file.SourceURI)
		r.addSymbol(model.Symbol{ID: sourceID, ScopeID: r.scope.ID, Name: filepath.Base(uriToPath(file.SourceURI)), Kind: "source_file"})
		r.addEdge(model.Edge{From: generatedID, To: sourceID, ScopeID: r.scope.ID, Kind: model.EdgeGenerated,
			SourceURI: file.URI, SourceHash: fileHash(r.files, file.URI), BuildContext: r.scope.BuildContext})
	}
}

func validSourceMapRange(source []byte, value model.Position) bool {
	index := position.NewIndex(source, position.UTF16)
	start, err := index.PositionToOffset(source, value.StartLine, value.StartChar, position.UTF16)
	if err != nil {
		return false
	}
	end, err := index.PositionToOffset(source, value.EndLine, value.EndChar, position.UTF16)
	return err == nil && end >= start
}

func fileHash(files *fileIndex, uri string) identity.ContentHash {
	if files == nil {
		return ""
	}
	if file, ok := files.byURI[uri]; ok {
		return file.hash
	}
	return ""
}

func fileSymbolID(uri string) identity.SymbolID {
	sum := sha256.Sum256([]byte(uri))
	return identity.SymbolID("go:file:sha256:" + hex.EncodeToString(sum[:]))
}

func (r *loadResult) symbolID(obj types.Object, fset *token.FileSet, packagePath string) identity.SymbolID {
	if obj == nil {
		return ""
	}
	if pkg := obj.Pkg(); pkg == nil || obj.Parent() == pkg.Scope() {
		return objectSymbolID(obj, nil, "", packagePath)
	}
	if fn, ok := obj.(*types.Func); ok {
		if signature, ok := fn.Type().(*types.Signature); ok && signature.Recv() != nil {
			return objectSymbolID(obj, nil, "", packagePath)
		}
	}
	if fset != nil && obj.Pos().IsValid() {
		position := fset.PositionFor(obj.Pos(), false)
		if entry, ok := r.files.byPath[pathKey(position.Filename)]; ok {
			kind := fmt.Sprintf("%T", obj)
			return identity.SymbolID("go:object:" + packagePath + ":local:" + entry.file.URI + "@" + strconv.Itoa(position.Offset) + ":" + kind + ":" + obj.Name())
		}
	}
	return objectSymbolID(obj, fset, "", packagePath)
}

func (r *loadResult) emitSymbol(symbol model.Symbol) {
	if symbol.ID == "" {
		return
	}
	symbol.ID = r.scopedSymbolID(symbol.ID)
	if _, exists := r.seenSymbols[symbol.ID]; exists {
		return
	}
	r.seenSymbols[symbol.ID] = struct{}{}
	r.batch.addSymbol(symbol)
}

func (r *loadResult) addSymbol(symbol model.Symbol) { r.emitSymbol(symbol) }

func (r *loadResult) addOccurrence(occ model.Occurrence) {
	occ.SymbolID = r.scopedSymbolID(occ.SymbolID)
	key := string(occ.SymbolID) + "\x00" + occ.URI + "\x00" + strconv.FormatUint(uint64(occ.Range.StartLine), 10) + ":" + strconv.FormatUint(uint64(occ.Range.StartChar), 10) + ":" + occ.Role
	if _, exists := r.seenOcc[key]; exists {
		return
	}
	r.seenOcc[key] = struct{}{}
	r.batch.addOccurrence(occ)
}

func (r *loadResult) addEdge(edge model.Edge) {
	edge.From = r.scopedSymbolID(edge.From)
	edge.To = r.scopedSymbolID(edge.To)
	key := string(edge.From) + "\x00" + string(edge.To) + "\x00" + string(edge.Kind) + "\x00" + edge.SourceURI + "\x00" + strconv.FormatUint(uint64(edge.Range.StartLine), 10) + ":" + strconv.FormatUint(uint64(edge.Range.StartChar), 10)
	if _, exists := r.seenEdges[key]; exists {
		return
	}
	r.seenEdges[key] = struct{}{}
	r.batch.addEdge(edge)
}

func (r *loadResult) scopedSymbolID(id identity.SymbolID) identity.SymbolID {
	if id == "" {
		return ""
	}
	return identity.SymbolID("go:scope:" + string(r.scope.BuildContext) + ":" + string(id))
}

func objectSymbolID(obj types.Object, fset *token.FileSet, sourcePath, packagePath string) identity.SymbolID {
	if obj == nil {
		return ""
	}
	if builtin, ok := obj.(*types.Builtin); ok {
		return identity.SymbolID("go:builtin:" + builtin.Name())
	}
	pkgPath := packagePath
	if obj.Pkg() != nil {
		pkgPath = obj.Pkg().Path()
	}
	if pkgPath == "" {
		return identity.SymbolID("go:predeclared:" + obj.Name())
	}
	if fn, ok := obj.(*types.Func); ok {
		if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
			return identity.SymbolID("go:object:" + pkgPath + ":method:" + receiverIdentity(sig.Recv().Type()) + "." + fn.Name())
		}
	}
	if pkg := obj.Pkg(); pkg != nil && obj.Parent() == pkg.Scope() {
		switch typed := obj.(type) {
		case *types.Func:
			receiver := ""
			if sig, ok := typed.Type().(*types.Signature); ok && sig.Recv() != nil {
				receiver = receiverIdentity(sig.Recv().Type()) + "."
			}
			return identity.SymbolID("go:object:" + pkgPath + ":func:" + receiver + typed.Name())
		case *types.TypeName:
			return identity.SymbolID("go:object:" + pkgPath + ":type:" + typed.Name())
		case *types.Const:
			return identity.SymbolID("go:object:" + pkgPath + ":const:" + typed.Name())
		case *types.Var:
			return identity.SymbolID("go:object:" + pkgPath + ":var:" + typed.Name())
		}
	}
	pos := obj.Pos()
	if pos.IsValid() && fset != nil {
		position := fset.PositionFor(pos, false)
		file := position.Filename
		if sourcePath != "" && sameExecutablePath(file, sourcePath) {
			file = sourcePath
		}
		file = filepath.Clean(file)
		if file != "" && position.Offset >= 0 {
			return identity.SymbolID("go:object:" + pkgPath + ":local:" + filepath.ToSlash(file) + "@" + strconv.Itoa(position.Offset))
		}
	}
	// A compiler object without a package scope or source position is unusual
	// (predeclared objects are handled above). Its Go type object string is an
	// exact semantic descriptor and avoids guessing from a token range.
	return identity.SymbolID("go:object:" + pkgPath + ":semantic:" + types.TypeString(obj.Type(), packageQualifier) + ":" + obj.Name())
}

func receiverIdentity(typ types.Type) string {
	if ptr, ok := typ.(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	if named, ok := types.Unalias(typ).(*types.Named); ok && named.Obj().Pkg() != nil {
		return named.Obj().Pkg().Path() + "." + named.Obj().Name()
	}
	return types.TypeString(typ, packageQualifier)
}

func packageQualifier(pkg *types.Package) string {
	if pkg == nil {
		return ""
	}
	return pkg.Path()
}

func symbolForObject(id identity.SymbolID, scopeID string, obj types.Object) model.Symbol {
	kind := "object"
	switch obj.(type) {
	case *types.Func:
		kind = "function"
	case *types.TypeName:
		kind = "type"
	case *types.Var:
		kind = "variable"
	case *types.Const:
		kind = "constant"
	case *types.PkgName:
		kind = "package_name"
	case *types.Builtin:
		kind = "builtin"
	case *types.Label:
		kind = "label"
	}
	signature := ""
	if obj.Type() != nil {
		signature = types.TypeString(obj.Type(), packageQualifier)
	}
	if constant, ok := obj.(*types.Const); ok {
		signature += " = " + constant.Val().ExactString()
	}
	return model.Symbol{ID: id, ScopeID: scopeID, Name: obj.Name(), Kind: kind, Signature: signature}
}

func typeSymbolObjects(typ types.Type) []types.Object {
	seen := make(map[identity.SymbolID]struct{})
	var result []types.Object
	var visit func(types.Type)
	visit = func(t types.Type) {
		if t == nil {
			return
		}
		switch v := t.(type) {
		case *types.Alias:
			if v.Obj() != nil {
				id := objectSymbolID(v.Obj(), nil, "", "")
				if _, exists := seen[id]; exists {
					return
				}
				seen[id] = struct{}{}
				result = append(result, v.Obj())
			}
			visit(types.Unalias(v))
		case *types.Named:
			if v.Obj() != nil {
				id := objectSymbolID(v.Obj(), nil, "", "")
				if _, exists := seen[id]; exists {
					return
				}
				seen[id] = struct{}{}
				result = append(result, v.Obj())
			}
			if v.TypeArgs() != nil {
				for i := 0; i < v.TypeArgs().Len(); i++ {
					visit(v.TypeArgs().At(i))
				}
			}
			visit(v.Underlying())
		case *types.Pointer:
			visit(v.Elem())
		case *types.Array:
			visit(v.Elem())
		case *types.Slice:
			visit(v.Elem())
		case *types.Map:
			visit(v.Key())
			visit(v.Elem())
		case *types.Chan:
			visit(v.Elem())
		case *types.Signature:
			if v.Recv() != nil {
				visit(v.Recv().Type())
			}
			if v.TypeParams() != nil {
				for i := 0; i < v.TypeParams().Len(); i++ {
					visit(v.TypeParams().At(i).Constraint())
				}
			}
			visitTuple := func(tuple *types.Tuple) {
				if tuple != nil {
					for i := 0; i < tuple.Len(); i++ {
						visit(tuple.At(i).Type())
					}
				}
			}
			visitTuple(v.Params())
			visitTuple(v.Results())
		case *types.Struct:
			for i := 0; i < v.NumFields(); i++ {
				visit(v.Field(i).Type())
			}
		case *types.Interface:
			v.Complete()
			for i := 0; i < v.NumMethods(); i++ {
				visit(v.Method(i).Type())
			}
			for i := 0; i < v.NumEmbeddeds(); i++ {
				visit(v.EmbeddedType(i))
			}
		case *types.TypeParam:
			visit(v.Constraint())
		case *types.Union:
			for i := 0; i < v.Len(); i++ {
				visit(v.Term(i).Type())
			}
		case *types.Basic:
			if obj := types.Universe.Lookup(v.Name()); obj != nil {
				id := objectSymbolID(obj, nil, "", "")
				if _, exists := seen[id]; !exists {
					seen[id] = struct{}{}
					result = append(result, obj)
				}
			}
		}
	}
	visit(typ)
	return result
}

func (r *loadResult) typeSymbolIDs(typ types.Type) []identity.SymbolID {
	objects := typeSymbolObjects(typ)
	result := make([]identity.SymbolID, 0, len(objects))
	for _, obj := range objects {
		packagePath := ""
		if obj.Pkg() != nil {
			packagePath = obj.Pkg().Path()
		}
		id := r.symbolID(obj, nil, packagePath)
		r.addSymbol(symbolForObject(id, r.scope.ID, obj))
		result = append(result, id)
	}
	return result
}

func typeSymbolIDs(typ types.Type) []identity.SymbolID {
	objects := typeSymbolObjects(typ)
	result := make([]identity.SymbolID, 0, len(objects))
	for _, obj := range objects {
		packagePath := ""
		if obj.Pkg() != nil {
			packagePath = obj.Pkg().Path()
		}
		result = append(result, objectSymbolID(obj, nil, "", packagePath))
	}
	return result
}

func sourceRange(fset *token.FileSet, file *ast.File, source []byte, start, end token.Pos) (model.Position, bool) {
	if fset == nil || file == nil || !start.IsValid() || !end.IsValid() || end < start {
		return model.Position{}, false
	}
	tf := fset.File(file.Pos())
	if tf == nil {
		return model.Position{}, false
	}
	startOffset := tf.Offset(start)
	endOffset := tf.Offset(end)
	if startOffset < 0 || endOffset < startOffset || endOffset > tf.Size() {
		return model.Position{}, false
	}
	if endOffset > len(source) {
		return model.Position{}, false
	}
	index := position.NewIndex(source, position.UTF16)
	startPos, err := index.OffsetToPosition(source, uint32(startOffset))
	if err != nil {
		return model.Position{}, false
	}
	endPos, err := index.OffsetToPosition(source, uint32(endOffset))
	if err != nil {
		return model.Position{}, false
	}
	return model.Position{StartLine: startPos.Line, StartChar: startPos.Col, EndLine: endPos.Line, EndChar: endPos.Col}, true
}
