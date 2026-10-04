// Package clang exports bounded semantic facts from C and C++ translation
// units using a separately executed helper linked to the caller-pinned
// libclang installation. The helper only sees a materialized WorkspaceView.
//
// Invariants: compiler contexts are explicit, paths are snapshot-bound, and
// unsupported or unmapped facts remain incomplete instead of being guessed.
package clang

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

const (
	ExtractorName        = "llvm-libclang"
	ExtractorVersion     = "omnilsp-clang-export-v3"
	helperVersion        = "omnilsp-clang-helper/3"
	batchSize            = 128
	maxEventLine         = 1 << 20
	maxStderrBytes       = 8 << 10
	maxContextIssues     = 128
	maxIssueBytes        = 512
	maxPositionCache     = 32 << 20
	maxCoverageReasons   = 24
	maxHelperTraceBytes  = 4 << 20
	maxHelperHeaders     = 32768
	maxHelperHeaderBytes = 512 << 20
)

//go:embed helper.cpp.txt
var helperSource string

// ExtractorVersionPinned binds the native helper source into the persistent
// extractor identity so a generated helper cannot be mistaken for another
// implementation built by the same compiler.
func ExtractorVersionPinned() string {
	sum := sha256.Sum256([]byte(helperSource))
	return ExtractorVersion + "+" + helperVersion + "+sha256:" + hex.EncodeToString(sum[:])
}

var compilerEnvironmentNames = []string{
	"CPATH", "C_INCLUDE_PATH", "CPLUS_INCLUDE_PATH", "OBJC_INCLUDE_PATH",
	"INCLUDE", "CL", "CCC_OVERRIDE_OPTIONS", "CLANG_CONFIG_FILE",
	"CLANG_CONFIG_FILE_USER_DIR", "CLANG_RESOURCE_DIR", "SDKROOT",
	"DEVELOPER_DIR", "MACOSX_DEPLOYMENT_TARGET", "IPHONEOS_DEPLOYMENT_TARGET",
	"TVOS_DEPLOYMENT_TARGET", "WATCHOS_DEPLOYMENT_TARGET", "XROS_DEPLOYMENT_TARGET",
	"VCToolsInstallDir", "VCINSTALLDIR", "WindowsSdkDir", "WindowsSDKVersion",
	"UniversalCRTSdkDir", "UCRTVersion", "PATH", "LD_LIBRARY_PATH",
	"DYLD_LIBRARY_PATH",
}

var compilerDriverConfigNames = []string{
	"CL", "CCC_OVERRIDE_OPTIONS", "CLANG_CONFIG_FILE", "CLANG_CONFIG_FILE_USER_DIR",
}

func captureCompilerEnvironment() map[string]string {
	values := make(map[string]string)
	for _, name := range compilerEnvironmentNames {
		if filepath.Separator == '\\' && strings.EqualFold(name, "PATH") {
			name = "Path"
		}
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	return values
}

func compilerEnvironment(base []string, values map[string]string, libraryDir string) []string {
	filtered := make([]string, 0, len(base)+len(values))
	for _, item := range base {
		name, _, ok := strings.Cut(item, "=")
		if !ok || !isCompilerEnvironmentName(name) {
			filtered = append(filtered, item)
		}
	}
	effective := make(map[string]string, len(values))
	for name, value := range values {
		if !isCompilerDriverConfigName(name) {
			effective[name] = value
		}
	}
	filtered = withEnvironment(filtered, effective)
	if libraryDir != "" {
		filtered = withSearchPath(filtered, libraryDir)
	}
	return filtered
}

func isCompilerDriverConfigName(name string) bool {
	for _, candidate := range compilerDriverConfigNames {
		if name == candidate || filepath.Separator == '\\' && strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}

func isCompilerEnvironmentName(name string) bool {
	for _, candidate := range compilerEnvironmentNames {
		if name == candidate || filepath.Separator == '\\' && strings.EqualFold(name, candidate) {
			return true
		}
	}
	return false
}

func extractorVersionPinnedForToolchain(ctx context.Context, compilerPath, libclangPath string, environment map[string]string) (string, error) {
	version, _, err := extractorVersionAndHeaderManifest(ctx, compilerPath, libclangPath, environment)
	return version, err
}

func extractorVersionAndHeaderManifest(ctx context.Context, compilerPath, libclangPath string, environment map[string]string) (string, []helperHeaderIdentity, error) {
	manifest, digest, err := helperDependencyManifest(ctx, compilerPath, libclangPath, environment)
	if err != nil {
		return "", nil, err
	}
	return ExtractorVersionPinned() + "+headers-sha256:" + digest, manifest, nil
}

func helperDependencyDigest(ctx context.Context, compilerPath, libclangPath string, environment map[string]string) (string, error) {
	_, digest, err := helperDependencyManifest(ctx, compilerPath, libclangPath, environment)
	return digest, err
}

func helperDependencyManifest(ctx context.Context, compilerPath, libclangPath string, environment map[string]string) ([]helperHeaderIdentity, string, error) {
	libRoot := filepath.Dir(filepath.Dir(libclangPath))
	includeDir := filepath.Join(libRoot, "include")
	if _, err := os.Stat(filepath.Join(includeDir, "clang-c", "Index.h")); err != nil {
		return nil, "", fmt.Errorf("clang-c/Index.h not found beside pinned libclang: %w", err)
	}
	dir, err := os.MkdirTemp("", "omnilsp-clang-header-scan-")
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(dir)
	source := filepath.Join(dir, "helper.cpp")
	if err := os.WriteFile(source, []byte(helperSource), 0o600); err != nil {
		return nil, "", err
	}
	commandCtx, cancel := context.WithTimeout(ctx, 30_000_000_000)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, compilerPath, "--no-default-config", "-x", "c++", "-std=c++17", "-I", includeDir, "-H", "-fsyntax-only", source)
	cmd.Dir = dir
	cmd.Env = compilerEnvironment(os.Environ(), environment, filepath.Dir(libclangPath))
	cmd.Stdout = io.Discard
	var stderr boundedBuffer
	stderr.limit = maxHelperTraceBytes
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if commandCtx.Err() != nil {
			return nil, "", commandCtx.Err()
		}
		return nil, "", fmt.Errorf("scan pinned clang helper headers: %w (%s)", err, stderr.String())
	}
	trace := stderr.String()
	if len(trace) >= maxHelperTraceBytes {
		return nil, "", errors.New("clang helper header dependency trace exceeded its bounded limit")
	}
	paths, err := parseHelperHeaderTrace(trace, dir)
	if err != nil {
		return nil, "", err
	}
	if strings.EqualFold(filepath.Ext(libclangPath), ".dll") {
		libRoot := filepath.Dir(filepath.Dir(libclangPath))
		paths = append(paths, filepath.Join(libRoot, "lib", "libclang.lib"))
	}
	if len(paths) == 0 {
		return nil, "", errors.New("clang compiler returned no helper header dependencies")
	}
	if len(paths) > maxHelperHeaders {
		return nil, "", errors.New("clang helper header count exceeded its bounded limit")
	}
	manifest, digest, err := helperDependencyContentManifest(ctx, paths)
	if err != nil {
		return nil, "", err
	}
	return manifest, digest, nil
}

func parseHelperHeaderTrace(trace, directory string) ([]string, error) {
	seen := make(map[string]string)
	for _, line := range strings.Split(trace, "\n") {
		depth := 0
		for depth < len(line) && line[depth] == '.' {
			depth++
		}
		if depth == 0 || depth >= len(line) || line[depth] != ' ' {
			continue
		}
		path := strings.Trim(strings.TrimSpace(line[depth+1:]), `"`)
		if path == "" {
			return nil, errors.New("clang helper header trace contains an empty path")
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(directory, path)
		}
		path, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("resolve clang helper header path %q: %w", path, err)
		}
		key := canonicalPath(path)
		seen[key] = path
		if len(seen) > maxHelperHeaders {
			return nil, errors.New("clang helper header count exceeded its bounded limit")
		}
	}
	paths := make([]string, 0, len(seen))
	for _, path := range seen {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool { return canonicalPath(paths[i]) < canonicalPath(paths[j]) })
	return paths, nil
}

var versionPattern = regexp.MustCompile(`(?i)clang version\s+([0-9]+\.[0-9]+(?:\.[0-9]+)?)`)

// Extract streams facts from every requested scope. Tool paths and hashes are
// taken only from that scope's provenance; this package never searches PATH
// for a compiler or chooses an installed LLVM version.
func Extract(ctx context.Context, request model.Request, sink model.Sink) (report model.Report, retErr error) {
	report = model.Report{UsedTools: make(map[string][]model.ToolIdentity)}
	defer func() { fillMissingCoverage(&report, request.Scopes) }()
	if request.View == nil {
		return report, model.ErrMissingView
	}
	report.Identity = request.View.Identity()
	if sink == nil {
		return report, errors.New("clang semantic index: missing sink")
	}
	if len(request.Scopes) == 0 {
		return report, model.ErrInvalidScope
	}
	seenScopes := make(map[string]struct{}, len(request.Scopes))
	for _, scope := range request.Scopes {
		if scope.ID == "" || scope.Language != "cpp" || scope.RootURI == "" || scope.BuildContext == "" {
			return report, model.ErrInvalidScope
		}
		if _, exists := seenScopes[scope.ID]; exists {
			return report, model.ErrInvalidScope
		}
		seenScopes[scope.ID] = struct{}{}
	}

	artifacts := make(map[string]*helperArtifact)
	verifiedCompiler := make(map[string]bool)
	verifiedLibclang := make(map[string]bool)
	verifiedHelper := make(map[string]bool)
	defer func() {
		for _, artifact := range artifacts {
			_ = os.RemoveAll(artifact.dir)
		}
	}()
	type capturedRoot struct {
		destination string
		view        model.MaterializedView
	}
	materializedByRoot := make(map[string]capturedRoot)
	defer func() {
		for _, snapshot := range materializedByRoot {
			if err := snapshot.view.Close(); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("close materialized clang snapshot: %w", err))
			}
			_ = os.RemoveAll(snapshot.destination)
		}
	}()
	filesByRoot := make(map[string][]model.File)

	for _, scope := range request.Scopes {
		coverage := newCoverage(scope.ID)
		if err := ctx.Err(); err != nil {
			return report, err
		}
		provenance, ok := request.Provenance[scope.ID]
		if !ok {
			coverage.unavailableAll("scope provenance is missing")
			report.Coverage = append(report.Coverage, coverage.results()...)
			return report, fmt.Errorf("clang semantic index: %w: missing provenance for %q", model.ErrInvalidProvenance, scope.ID)
		}
		if provenance.SchemaVersion != model.SchemaVersion || provenance.Identity != report.Identity || !reflect.DeepEqual(provenance.Scope, scope) ||
			provenance.Extractor != ExtractorName || provenance.ExtractorVer == "" || provenance.Toolchain == "" ||
			provenance.Backend.Language != scope.Language || provenance.Backend.Name == "" ||
			scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
			coverage.unavailableAll("scope provenance does not match the captured build context")
			report.Coverage = append(report.Coverage, coverage.results()...)
			return report, model.ErrInvalidProvenance
		}
		if reason := scope.Build.Options["omnilsp.clang.unavailable"]; reason != "" {
			coverage.unavailableAll(reason)
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		if reason := scope.Build.Options["omnilsp.clang.unknown"]; reason != "" {
			coverage.unknownAll(reason)
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		files, walked := filesByRoot[scope.RootURI]
		if !walked {
			var err error
			files, err = walkFiles(ctx, request.View, scope.RootURI)
			if err != nil {
				coverage.unavailableAll("immutable workspace walk failed: " + err.Error())
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
			filesByRoot[scope.RootURI] = files
		}
		if !hasCompileDatabase(files) {
			coverage.unavailableAll("compile_commands.json is absent from the captured workspace view")
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		materializer, ok := request.View.(model.WorkspaceMaterializer)
		if !ok {
			coverage.unavailableAll("workspace view cannot materialize an isolated snapshot for libclang")
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		snapshot, hasSnapshot := materializedByRoot[scope.RootURI]
		if !hasSnapshot {
			destination, err := os.MkdirTemp("", "omnilsp-clang-view-")
			if err != nil {
				coverage.unavailableAll("could not create isolated snapshot directory: " + err.Error())
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
			materialized, err := materializer.Materialize(ctx, scope.RootURI, destination)
			if err != nil {
				_ = os.RemoveAll(destination)
				coverage.unavailableAll("could not materialize immutable workspace snapshot: " + err.Error())
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
			rootURI, rootErr := workspaceuri.Parse(scope.RootURI)
			rootPath, pathErr := rootURI.Path()
			if materialized.RootURI() != scope.RootURI || rootErr != nil || pathErr != nil || pathWithin(rootPath, destination) || strings.TrimSpace(materialized.RootPath()) == "" {
				_ = materialized.Close()
				_ = os.RemoveAll(destination)
				coverage.unavailableAll("workspace materializer returned an invalid borrowed snapshot or the destination is inside the source workspace")
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
			snapshot = capturedRoot{destination: destination, view: materialized}
			materializedByRoot[scope.RootURI] = snapshot
		}

		workspace, err := newWorkspace(scope, snapshot.view, files)
		if err != nil {
			coverage.unavailableAll("workspace path mapping failed: " + err.Error())
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		commands, dbFound, dbErr := readCompilationDatabase(ctx, request.View, scope, files, workspace)
		if dbErr != nil {
			coverage.unavailableAll("compile_commands.json could not be read from the captured workspace view: " + dbErr.Error())
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		if !dbFound {
			coverage.unavailableAll("compile_commands.json is absent from the captured workspace view")
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		if len(commands) == 0 {
			coverage.unavailableAll("compile_commands.json contains no usable C/C++ translation units for this scope")
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		toolchain, err := pinnedToolchain(provenance.Tools)
		if err != nil {
			coverage.unavailableAll(err.Error())
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		extractorVersion, headerManifest, err := extractorVersionAndHeaderManifest(ctx, toolchain.compiler.Path, toolchain.libclang.Path, scope.Build.Environment)
		if err != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			coverage.unavailableAll("clang helper header environment could not be verified: " + err.Error())
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		if provenance.ExtractorVer != extractorVersion {
			coverage.unavailableAll("clang helper header or compiler environment changed after build context capture")
			report.Coverage = append(report.Coverage, coverage.results()...)
			continue
		}
		if rawManifest := scope.Build.Options[headerManifestOption]; rawManifest != "" {
			manifest, _, decodeErr := decodeHelperHeaderManifest(rawManifest)
			actualManifest, encodeErr := encodeHelperHeaderManifest(headerManifest)
			if decodeErr != nil || encodeErr != nil || !reflect.DeepEqual(manifest, headerManifest) || rawManifest != actualManifest {
				coverage.unavailableAll("clang helper header manifest changed after build context capture")
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
		}
		compilerKey := canonicalPath(toolchain.compiler.Path) + "\x00" + toolchain.compiler.SHA256
		if !verifiedCompiler[compilerKey] {
			if err := verifyTool(ctx, toolchain.compiler); err != nil {
				coverage.unavailableAll(err.Error())
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
			verifiedCompiler[compilerKey] = true
		}
		libclangKey := canonicalPath(toolchain.libclang.Path) + "\x00" + toolchain.libclang.SHA256
		if !verifiedLibclang[libclangKey] {
			if err := verifyHash(toolchain.libclang); err != nil {
				coverage.unavailableAll(err.Error())
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
			verifiedLibclang[libclangKey] = true
		}
		report.UsedTools[scope.ID] = appendTool(report.UsedTools[scope.ID], toolchain.compiler)
		report.UsedTools[scope.ID] = appendTool(report.UsedTools[scope.ID], toolchain.libclang)
		for _, command := range commands {
			if command.problem == "" && !samePath(command.compilerPath, toolchain.compiler.Path) {
				command.problem = "compile command compiler does not match the pinned compiler identity"
			}
		}
		environmentData, _ := json.Marshal(scope.Build.Environment)
		environmentHash := sha256.Sum256(environmentData)
		key := canonicalPath(toolchain.compiler.Path) + "\x00" + toolchain.compiler.SHA256 + "\x00" + canonicalPath(toolchain.libclang.Path) + "\x00" + toolchain.libclang.SHA256 + "\x00" + hex.EncodeToString(environmentHash[:])
		artifact := artifacts[key]
		if artifact == nil {
			artifact, err = buildHelper(ctx, toolchain, scope.Build.Environment)
			if err != nil {
				coverage.unavailableAll(err.Error())
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
			builtWithVersion, versionErr := extractorVersionPinnedForToolchain(ctx, toolchain.compiler.Path, toolchain.libclang.Path, scope.Build.Environment)
			if versionErr != nil || builtWithVersion != provenance.ExtractorVer {
				_ = os.RemoveAll(artifact.dir)
				reason := "clang helper header inputs changed while the helper was built"
				if versionErr != nil {
					reason += ": " + versionErr.Error()
				}
				coverage.unavailableAll(reason)
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
			report.UsedTools[scope.ID] = appendTool(report.UsedTools[scope.ID], artifact.identity)
			if err := verifyTool(ctx, toolchain.compiler); err != nil {
				_ = os.RemoveAll(artifact.dir)
				coverage.unavailableAll(err.Error())
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
			artifacts[key] = artifact
		} else {
			report.UsedTools[scope.ID] = appendTool(report.UsedTools[scope.ID], artifact.identity)
		}
		if !verifiedHelper[key] {
			if err := verifyHelperVersion(ctx, artifact, toolchain.libclang); err != nil {
				coverage.unavailableAll(err.Error())
				report.Coverage = append(report.Coverage, coverage.results()...)
				continue
			}
			verifiedHelper[key] = true
		}
		writer := newFactWriter(ctx, sink, scope)
		analysis := analyzeState{coverage: coverage, files: files, workspace: workspace, seenSource: make(map[string]bool), packagePatterns: scope.Build.PackagePatterns}
		for _, command := range commands {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			if command.problem != "" {
				analysis.addContextIssue(command.problem)
				continue
			}
			if err := runTranslationUnit(ctx, artifact, scope, command, workspace, &analysis, writer); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return report, err
				}
				analysis.addContextIssue(err.Error())
			} else {
				analysis.seenSource[command.rel] = true
			}
		}
		if analysis.processed == 0 {
			reason := strings.Join(uniqueStrings(analysis.issues), "; ")
			if reason == "" {
				reason = "no compile command could be executed in this scope"
			}
			coverage.unknownAll(reason)
		}
		if err := writer.flush(); err != nil {
			return report, err
		}
		analysis.finish()
		for _, generated := range files {
			if !generated.Generated {
				continue
			}
			generatedRel := workspace.uriRel[generated.URI]
			if !analysis.seenSource[generatedRel] {
				coverage.partial(model.FactGenerated, "generated source has no translation-unit context in this compile scope: "+generated.URI)
				continue
			}
			mappings := generated.SourceMap
			if len(mappings) == 0 {
				if generated.SourceURI == "" || !containsFileURI(files, generated.SourceURI) {
					coverage.partial(model.FactGenerated, "generated source mapping is absent from the captured workspace: "+generated.URI)
					continue
				}
				coverage.partial(model.FactGenerated, "generated source mapping has no range map: "+generated.URI)
				mappings = []model.SourceMapSpan{{SourceURI: generated.SourceURI}}
			}
			for _, mapping := range mappings {
				if mapping.SourceURI == "" || !containsFileURI(files, mapping.SourceURI) {
					coverage.partial(model.FactGenerated, "generated source range maps outside the captured workspace: "+generated.URI)
					continue
				}
				if err := writer.edge(model.Edge{
					From:         fileSymbolID(generated.URI),
					To:           fileSymbolID(mapping.SourceURI),
					ScopeID:      scope.ID,
					Kind:         model.EdgeGenerated,
					SourceURI:    generated.URI,
					Range:        mapping.Generated,
					SourceHash:   generated.SHA256,
					BuildContext: scope.BuildContext,
				}); err != nil {
					return report, err
				}
			}
		}
		if err := writer.flush(); err != nil {
			return report, err
		}
		report.Coverage = append(report.Coverage, coverage.results()...)
	}
	return report, nil
}

type pinned struct {
	compiler model.ToolIdentity
	libclang model.ToolIdentity
}

func pinnedToolchain(tools []model.ToolIdentity) (pinned, error) {
	var result pinned
	for _, tool := range tools {
		switch strings.ToLower(strings.TrimSpace(tool.Name)) {
		case "clang", "clang++", "clangxx", "llvm-clang++":
			if result.compiler.Path != "" {
				return pinned{}, errors.New("multiple clang++ tool identities were supplied")
			}
			result.compiler = tool
		case "libclang", "libclang.dll", "libclang.so", "libclang.dylib":
			if result.libclang.Path != "" {
				return pinned{}, errors.New("multiple libclang tool identities were supplied")
			}
			result.libclang = tool
		default:
			return pinned{}, fmt.Errorf("unsupported pinned clang tool identity %q", tool.Name)
		}
	}
	if result.compiler.Path == "" || result.libclang.Path == "" {
		return pinned{}, errors.New("provenance must pin one clang++ compiler and one libclang library by path, version, and SHA-256")
	}
	for _, tool := range []model.ToolIdentity{result.compiler, result.libclang} {
		if !filepath.IsAbs(tool.Path) || tool.Version == "" || len(tool.SHA256) != 64 {
			return pinned{}, fmt.Errorf("tool %q requires an absolute path, version, and SHA-256", tool.Name)
		}
		if _, err := hex.DecodeString(tool.SHA256); err != nil {
			return pinned{}, fmt.Errorf("tool %q has an invalid SHA-256", tool.Name)
		}
	}
	return result, nil
}

func verifyTool(ctx context.Context, tool model.ToolIdentity) error {
	if err := verifyHash(tool); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, tool.Path, "--version")
	var stdout, stderr boundedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("run pinned compiler --version: %w (%s)", err, stderr.String())
	}
	actual := parseClangVersion(stdout.String())
	if actual == "" || actual != tool.Version {
		return fmt.Errorf("pinned compiler version mismatch: expected %q, got %q", tool.Version, actual)
	}
	return nil
}

func verifyHash(tool model.ToolIdentity) error {
	file, err := os.Open(tool.Path)
	if err != nil {
		return fmt.Errorf("open pinned tool %q: %w", tool.Name, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("hash pinned tool %q: %w", tool.Name, err)
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, tool.SHA256) {
		return fmt.Errorf("pinned tool %q SHA-256 mismatch", tool.Name)
	}
	return nil
}

func parseClangVersion(text string) string {
	match := versionPattern.FindStringSubmatch(text)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

type helperArtifact struct {
	dir      string
	path     string
	identity model.ToolIdentity
	libDir   string
}

func buildHelper(ctx context.Context, tools pinned, environment map[string]string) (*helperArtifact, error) {
	libRoot := filepath.Dir(filepath.Dir(tools.libclang.Path))
	includeDir := filepath.Join(libRoot, "include")
	libDir := filepath.Join(libRoot, "lib")
	if _, err := os.Stat(filepath.Join(includeDir, "clang-c", "Index.h")); err != nil {
		return nil, fmt.Errorf("clang-c/Index.h not found beside pinned libclang: %w", err)
	}
	if _, err := os.Stat(filepath.Join(libDir, "libclang.lib")); err != nil && strings.EqualFold(filepath.Ext(tools.libclang.Path), ".dll") {
		return nil, fmt.Errorf("libclang.lib not found beside pinned libclang: %w", err)
	}
	dir, err := os.MkdirTemp("", "omnilsp-clang-helper-")
	if err != nil {
		return nil, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(dir)
		}
	}()
	source := filepath.Join(dir, "helper.cpp")
	if err := os.WriteFile(source, []byte(helperSource), 0o600); err != nil {
		return nil, err
	}
	exeName := "omnilsp-clang-helper"
	if strings.EqualFold(filepath.Ext(tools.compiler.Path), ".exe") || strings.EqualFold(filepath.Ext(tools.libclang.Path), ".dll") {
		exeName += ".exe"
	}
	exePath := filepath.Join(dir, exeName)
	args := []string{"--no-default-config", "-std=c++17", "-O1", "-I", includeDir, source}
	if strings.EqualFold(filepath.Ext(tools.libclang.Path), ".dll") {
		args = append(args, filepath.Join(libDir, "libclang.lib"))
	} else {
		args = append(args, "-L", libDir, "-lclang")
	}
	args = append(args, "-o", exePath)
	cmd := exec.CommandContext(ctx, tools.compiler.Path, args...)
	cmd.Dir = dir
	cmd.Env = compilerEnvironment(os.Environ(), environment, filepath.Dir(tools.libclang.Path))
	cmd.Stdout = io.Discard
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("compile libclang helper: %w (%s)", err, stderr.String())
	}
	digest, err := fileSHA256(exePath)
	if err != nil {
		return nil, err
	}
	artifact := &helperArtifact{
		dir:    dir,
		path:   exePath,
		libDir: filepath.Dir(tools.libclang.Path),
		identity: model.ToolIdentity{
			Name:    "omnilsp-clang-helper",
			Path:    exePath,
			Version: helperVersion,
			SHA256:  digest,
		},
	}
	cleanup = false
	return artifact, nil
}

func verifyHelperVersion(ctx context.Context, artifact *helperArtifact, libclang model.ToolIdentity) error {
	cmd := exec.CommandContext(ctx, artifact.path, "--version")
	cmd.Env = withSearchPath(os.Environ(), artifact.libDir)
	var stdout, stderr boundedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("run helper --version: %w (%s)", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != helperVersion {
		return errors.New("helper returned an unexpected version response")
	}
	actual := parseClangVersion(lines[1])
	if actual == "" || actual != libclang.Version {
		return fmt.Errorf("pinned libclang version mismatch: expected %q, got %q", libclang.Version, actual)
	}
	return nil
}

func withSearchPath(environment []string, directory string) []string {
	result := make([]string, 0, len(environment)+1)
	key := "PATH"
	if filepath.Separator == '\\' {
		key = "Path"
	}
	found := false
	for _, item := range environment {
		name, value, ok := strings.Cut(item, "=")
		if !ok || !strings.EqualFold(name, "PATH") {
			result = append(result, item)
			continue
		}
		result = append(result, name+"="+directory+string(os.PathListSeparator)+value)
		found = true
	}
	if !found {
		result = append(result, key+"="+directory)
	}
	return result
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func appendTool(tools []model.ToolIdentity, tool model.ToolIdentity) []model.ToolIdentity {
	for _, existing := range tools {
		if existing == tool {
			return tools
		}
	}
	return append(tools, tool)
}

type workspace struct {
	rootURI            string
	root               string
	files              []model.File
	byRel              map[string]model.File
	canonicalRel       map[string]string
	uriRel             map[string]string
	positionData       map[string][]byte
	positionBytes      int64
	positionIncomplete bool
	rootOS             string
}

func newWorkspace(scope model.Scope, materialized model.MaterializedView, files []model.File) (*workspace, error) {
	root, err := filepath.Abs(materialized.RootPath())
	if err != nil {
		return nil, err
	}
	rootOS, err := workspaceuri.Parse(scope.RootURI)
	if err != nil {
		return nil, err
	}
	rootPath, err := rootOS.Path()
	if err != nil {
		return nil, err
	}
	result := &workspace{
		rootURI:      scope.RootURI,
		root:         root,
		files:        files,
		byRel:        make(map[string]model.File),
		canonicalRel: make(map[string]string),
		uriRel:       make(map[string]string),
		positionData: make(map[string][]byte),
		rootOS:       filepath.Clean(rootPath),
	}
	for _, file := range files {
		path, err := materialized.PathForURI(file.URI)
		if err != nil {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil || !pathWithin(root, abs) {
			return nil, fmt.Errorf("materialized file %q is outside its root", file.URI)
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return nil, err
		}
		rel = normalizeRel(rel)
		result.byRel[rel] = file
		result.canonicalRel[canonicalRelKey(rel)] = rel
		result.uriRel[file.URI] = rel
	}
	return result, nil
}

func walkFiles(ctx context.Context, view model.WorkspaceView, rootURI string) ([]model.File, error) {
	files := make([]model.File, 0, 64)
	err := view.Walk(ctx, rootURI, func(file model.File) error {
		if strings.TrimSpace(file.URI) == "" {
			return errors.New("workspace view returned a file without a URI")
		}
		files = append(files, file)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].URI < files[j].URI })
	return files, nil
}

func (w *workspace) mapOriginalPath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(w.rootOS, path)
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil || !pathWithin(w.rootOS, abs) {
		return "", false
	}
	rel, err := filepath.Rel(w.rootOS, abs)
	if err != nil {
		return "", false
	}
	rel = normalizeRel(rel)
	if actual, ok := w.canonicalRel[canonicalRelKey(rel)]; ok {
		rel = actual
	} else if _, ok := w.byRel[rel]; !ok {
		return "", false
	}
	return filepath.Join(w.root, filepath.FromSlash(rel)), true
}

func (w *workspace) originalRel(abs string) (string, bool) {
	abs, err := filepath.Abs(abs)
	if err != nil || !pathWithin(w.rootOS, abs) {
		return "", false
	}
	rel, err := filepath.Rel(w.rootOS, abs)
	if err != nil {
		return "", false
	}
	rel = normalizeRel(rel)
	if actual, ok := w.canonicalRel[canonicalRelKey(rel)]; ok {
		return actual, true
	}
	_, ok := w.byRel[rel]
	return rel, ok
}

type compileCommand struct {
	directory    string
	file         string
	rel          string
	compilerPath string
	contextKey   string
	args         []string
	problem      string
}

type rawCompileCommand struct {
	Directory string   `json:"directory"`
	File      string   `json:"file"`
	Arguments []string `json:"arguments"`
	Command   string   `json:"command"`
}

func hasCompileDatabase(files []model.File) bool {
	for _, file := range files {
		if strings.EqualFold(filepath.Base(uriPath(file.URI)), "compile_commands.json") {
			return true
		}
	}
	return false
}

func containsFileURI(files []model.File, uri string) bool {
	for _, file := range files {
		if file.URI == uri {
			return true
		}
	}
	return false
}

func readCompilationDatabase(ctx context.Context, view model.WorkspaceView, scope model.Scope, files []model.File, workspace *workspace) ([]compileCommand, bool, error) {
	databases := make([]model.File, 0, 2)
	for _, file := range files {
		if strings.EqualFold(filepath.Base(uriPath(file.URI)), "compile_commands.json") {
			databases = append(databases, file)
		}
	}
	if len(databases) == 0 {
		return nil, false, nil
	}
	sort.Slice(databases, func(i, j int) bool {
		return pathDepth(uriPath(databases[i].URI)) < pathDepth(uriPath(databases[j].URI))
	})
	var commands []compileCommand
	for _, database := range databases {
		if database.Size < 0 || database.Size > maxCompileDBBytes {
			return nil, true, fmt.Errorf("compile_commands.json exceeds the %d-byte indexing limit", maxCompileDBBytes)
		}
		reader, err := view.Read(ctx, database.URI)
		if err != nil {
			return nil, true, err
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, 32<<20))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, true, readErr
		}
		if closeErr != nil {
			return nil, true, closeErr
		}
		var raw []rawCompileCommand
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, true, err
		}
		for _, item := range raw {
			command := compileCommand{directory: item.Directory}
			args := append([]string(nil), item.Arguments...)
			if len(args) == 0 && strings.TrimSpace(item.Command) != "" {
				args, err = splitCommand(item.Command)
				if err != nil {
					command.problem = "compile command could not be parsed: " + err.Error()
				}
			}
			if len(args) < 2 {
				if command.problem == "" {
					command.problem = "compile command has no compiler and source arguments"
				}
				if scope.Build.Options[contextOption] == "" {
					commands = append(commands, command)
				}
				continue
			}
			command.file = item.File
			command.args = args
			commands = append(commands, command)
		}
	}

	rootPath := workspace.rootOS
	for index := range commands {
		command := &commands[index]
		if command.problem != "" {
			continue
		}
		originalDir := command.directory
		if originalDir == "" {
			originalDir = rootPath
		} else if !filepath.IsAbs(originalDir) {
			originalDir = filepath.Join(rootPath, originalDir)
		}
		originalDir, err := filepath.Abs(filepath.Clean(originalDir))
		if err != nil || !pathWithin(rootPath, originalDir) {
			command.problem = "compile command working directory is outside the immutable workspace view"
			continue
		}
		if command.file == "" {
			command.problem = "compile command omitted its source file"
			continue
		}
		originalFile := command.file
		if !filepath.IsAbs(originalFile) {
			originalFile = filepath.Join(originalDir, originalFile)
		}
		originalFile, err = filepath.Abs(filepath.Clean(originalFile))
		if err != nil || !pathWithin(rootPath, originalFile) {
			command.problem = "translation unit is outside the immutable workspace view"
			continue
		}
		rel, ok := workspace.originalRel(originalFile)
		if !ok {
			command.problem = "translation unit is absent from the immutable workspace view"
			continue
		}
		command.file = filepath.Join(workspace.root, filepath.FromSlash(rel))
		command.rel = rel
		command.compilerPath, _ = explicitCommandPath(command.args[0], originalDir)
		if command.compilerPath == "" {
			command.problem = "compile command compiler token is not an explicit path; PATH lookup is not allowed"
			continue
		}
		contextArgs := normalizeBuildArgs(command.args, originalFile, originalDir)
		contextDir := normalizeRel(mustRel(rootPath, originalDir))
		if contextDir == "." {
			contextDir = ""
		}
		command.contextKey = compileContextDigest(command.compilerPath, contextDir, contextArgs)
		if expected := scope.Build.Options[contextOption]; expected != "" && command.contextKey != expected {
			continue
		}
		if len(scope.Build.PackagePatterns) > 0 && !matchesPackagePatterns(rel, scope.Build.PackagePatterns) {
			continue
		}
		if !scope.Build.Tests && looksLikeTestTranslationUnit(rel) {
			continue
		}
		command.directory, err = mapOriginalDir(workspace, originalDir)
		if err != nil {
			command.problem = err.Error()
			continue
		}
		command.args = normalizeCompileArgs(command.args, originalDir, originalFile, workspace)
		if hasPrecompiledInput(command.args) {
			command.problem = "precompiled header or module input is not available as captured source"
			continue
		}
		if scope.Build.Options[contextOption] == "" {
			command.args = appendBuildInputs(command.args, scope.Build, workspace)
		}
		command.args = append(command.args, "-working-directory="+command.directory)
	}
	if expected := scope.Build.Options[contextOption]; expected != "" {
		selected := commands[:0]
		for _, command := range commands {
			if command.contextKey == expected {
				selected = append(selected, command)
			}
		}
		commands = selected
	}
	return commands, true, nil
}

func mapOriginalDir(workspace *workspace, original string) (string, error) {
	if !pathWithin(workspace.rootOS, original) {
		return "", errors.New("compile command working directory is outside the immutable workspace view")
	}
	rel, err := filepath.Rel(workspace.rootOS, original)
	if err != nil {
		return "", err
	}
	return filepath.Join(workspace.root, rel), nil
}

func normalizeCompileArgs(args []string, originalDir, sourceFile string, workspace *workspace) []string {
	if len(args) > 0 {
		args = args[1:] // clang_parseTranslationUnit2 accepts arguments without argv[0].
	}
	result := make([]string, 0, len(args)+4)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" || arg == "/c" || arg == "-M" || arg == "-MM" || arg == "-MD" || arg == "-MMD" {
			continue
		}
		if arg == "-o" || arg == "-MF" || arg == "-MT" || arg == "-MQ" || arg == "/Fo" || arg == "/Fe" || arg == "-MJ" {
			i++
			continue
		}
		if isSourceArg(arg, sourceFile, originalDir) {
			continue
		}
		if strings.HasPrefix(arg, "-o") && len(arg) > 2 || strings.HasPrefix(strings.ToLower(arg), "/fo") && len(arg) > 3 {
			continue
		}
		if strings.HasPrefix(arg, "@") {
			if mapped, ok := workspace.mapOriginalPath(strings.TrimPrefix(arg, "@")); ok {
				result = append(result, "@"+mapped)
			} else {
				result = append(result, arg)
			}
			continue
		}
		if arg == "-I" || arg == "-isystem" || arg == "-iquote" || arg == "-idirafter" || arg == "-include" || arg == "-imacros" {
			result = append(result, arg)
			if i+1 < len(args) {
				i++
				result = append(result, rebasePathArg(args[i], originalDir, workspace))
			}
			continue
		}
		matched := false
		for _, prefix := range []string{"-I", "-isystem", "-iquote", "-idirafter", "-include=", "-imacros=", "-fmodule-map-file=", "-ivfsoverlay=", "-include-pch=", "--sysroot="} {
			if strings.HasPrefix(arg, prefix) && len(arg) > len(prefix) {
				result = append(result, prefix+rebasePathArg(arg[len(prefix):], originalDir, workspace))
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		result = append(result, arg)
	}
	return result
}

func rebasePathArg(path, originalDir string, workspace *workspace) string {
	path = strings.Trim(path, `"`)
	if filepath.IsAbs(path) {
		if mapped, ok := workspace.mapOriginalPath(path); ok {
			return mapped
		}
		if pathWithin(workspace.rootOS, path) {
			return filepath.Join(workspace.root, filepath.FromSlash(mustRel(workspace.rootOS, path)))
		}
		return path
	}
	resolved := filepath.Join(originalDir, path)
	if mapped, ok := workspace.mapOriginalPath(resolved); ok {
		return mapped
	}
	return path
}

func appendBuildInputs(args []string, build model.BuildInputs, workspace *workspace) []string {
	args = append(args, build.Arguments...)
	for _, include := range build.IncludePaths {
		args = append(args, "-I", rebasePathArg(include, workspace.rootOS, workspace))
	}
	defineKeys := make([]string, 0, len(build.Defines))
	for key := range build.Defines {
		defineKeys = append(defineKeys, key)
	}
	sort.Strings(defineKeys)
	for _, key := range defineKeys {
		value := build.Defines[key]
		if value == "" {
			args = append(args, "-D"+key)
		} else {
			args = append(args, "-D"+key+"="+value)
		}
	}
	for _, feature := range build.Features {
		if strings.HasPrefix(feature, "-") || strings.HasPrefix(feature, "/") {
			args = append(args, feature)
		} else if feature != "" {
			args = append(args, "-D"+feature)
		}
	}
	optionKeys := make([]string, 0, len(build.Options))
	for key := range build.Options {
		optionKeys = append(optionKeys, key)
	}
	sort.Strings(optionKeys)
	for _, key := range optionKeys {
		value := build.Options[key]
		if strings.HasPrefix(key, "-") {
			if value == "" {
				args = append(args, key)
			} else {
				args = append(args, key+"="+value)
			}
		}
	}
	return args
}

func splitCommand(command string) ([]string, error) {
	if filepath.Separator != '\\' {
		return splitPosixCommand(command)
	}
	var result []string
	for i := 0; i < len(command); {
		for i < len(command) && (command[i] == ' ' || command[i] == '\t' || command[i] == '\r' || command[i] == '\n') {
			i++
		}
		if i == len(command) {
			break
		}
		var token strings.Builder
		quoted := false
		for i < len(command) && (quoted || (command[i] != ' ' && command[i] != '\t' && command[i] != '\r' && command[i] != '\n')) {
			slashes := 0
			for i < len(command) && command[i] == '\\' {
				slashes++
				i++
			}
			if i < len(command) && command[i] == '"' {
				for n := 0; n < slashes/2; n++ {
					token.WriteByte('\\')
				}
				if slashes%2 == 1 {
					token.WriteByte('"')
					i++
				} else {
					quoted = !quoted
					i++
				}
				continue
			}
			for n := 0; n < slashes; n++ {
				token.WriteByte('\\')
			}
			if i < len(command) && (quoted || (command[i] != ' ' && command[i] != '\t' && command[i] != '\r' && command[i] != '\n')) {
				token.WriteByte(command[i])
				i++
			}
		}
		if quoted {
			return nil, errors.New("unterminated quoted string")
		}
		result = append(result, token.String())
	}
	return result, nil
}

func splitPosixCommand(command string) ([]string, error) {
	var result []string
	var token strings.Builder
	quoted := byte(0)
	escaped := false
	started := false
	for i := 0; i < len(command); i++ {
		c := command[i]
		if escaped {
			token.WriteByte(c)
			escaped = false
			started = true
			continue
		}
		if c == '\\' && quoted != '\'' {
			escaped = true
			started = true
			continue
		}
		if quoted != 0 {
			if c == quoted {
				quoted = 0
			} else {
				token.WriteByte(c)
			}
			started = true
			continue
		}
		if c == '\'' || c == '"' {
			quoted = c
			started = true
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			if started {
				result = append(result, token.String())
				token.Reset()
				started = false
			}
			continue
		}
		token.WriteByte(c)
		started = true
	}
	if escaped || quoted != 0 {
		return nil, errors.New("unterminated escape or quote")
	}
	if started {
		result = append(result, token.String())
	}
	return result, nil
}

func uriPath(uri string) string {
	parsed, err := workspaceuri.Parse(uri)
	if err != nil {
		return uri
	}
	return parsed.PathOr()
}

func pathDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}

func normalizeRel(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "./")
}

func canonicalPath(path string) string {
	clean := filepath.Clean(path)
	if filepath.Separator == '\\' {
		return strings.ToLower(clean)
	}
	return clean
}

func pathWithin(root, path string) bool {
	rootAbs, rootErr := filepath.Abs(root)
	pathAbs, pathErr := filepath.Abs(path)
	if rootErr != nil || pathErr != nil {
		return false
	}
	rootAbs = filepath.Clean(rootAbs)
	pathAbs = filepath.Clean(pathAbs)
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

func samePath(left, right string) bool { return canonicalPath(left) == canonicalPath(right) }

func mustRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.Clean(path)
	}
	return rel
}

type rawEvent struct {
	Type         string `json:"type"`
	USR          string `json:"usr"`
	Target       string `json:"target"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Signature    string `json:"signature"`
	Path         string `json:"path"`
	TargetPath   string `json:"target_path"`
	SpellingPath string `json:"spelling_path"`
	Role         string `json:"role"`
	EdgeKind     string `json:"edge_kind"`
	Message      string `json:"message"`
	Line         uint32 `json:"line"`
	Column       uint32 `json:"column"`
	EndLine      uint32 `json:"end_line"`
	EndColumn    uint32 `json:"end_column"`
	Severity     uint32 `json:"severity"`
}

type analyzeState struct {
	coverage        *coverageBuilder
	files           []model.File
	workspace       *workspace
	seenSource      map[string]bool
	packagePatterns []string
	issues          []string
	processed       int
}

func (a *analyzeState) addContextIssue(reason string) {
	if reason == "" {
		return
	}
	if len(reason) > maxIssueBytes {
		reason = reason[:maxIssueBytes-3] + "..."
	}
	if len(a.issues) < maxContextIssues {
		a.issues = append(a.issues, reason)
	} else if len(a.issues) == maxContextIssues {
		a.issues = append(a.issues, "additional semantic context issues omitted")
	}
}

func (a *analyzeState) addEvent(event rawEvent, scope model.Scope, writer *factWriter) error {
	if event.Type == "unresolved_module" || event.EdgeKind == "import" || event.EdgeKind == "module" {
		reason := event.Message
		if reason == "" {
			reason = "libclang reports a module import without an attested immutable source or PCM identity; no module edge was exported"
		}
		a.coverage.unknown(model.FactImport, reason)
		a.coverage.unknown(model.FactModule, reason)
		return nil
	}
	if event.Type == "incomplete" {
		reason := event.Message
		if reason == "" {
			reason = "libclang helper could not map a constructor reference to an exact source token"
		}
		a.coverage.partial(model.FactReference, reason)
		return nil
	}
	if event.Type == "incomplete_call" {
		reason := event.Message
		if reason == "" {
			reason = "libclang helper could not map a constructor call to an exact source token"
		}
		a.coverage.partial(model.FactReference, reason)
		a.coverage.partial(model.FactCall, reason)
		return nil
	}
	if event.Type == "incomplete_type_relation" {
		reason := event.Message
		if reason == "" {
			reason = "libclang helper could not resolve a type relation owner or target"
		}
		a.coverage.partial(model.FactTypeRelation, reason)
		return nil
	}
	if event.Type == "incomplete_implementation" {
		reason := event.Message
		if reason == "" {
			reason = "libclang helper could not resolve an implementation source or target"
		}
		a.coverage.partial(model.FactImplementation, reason)
		return nil
	}
	if event.Type == "truncated" {
		reason := event.Message
		if reason == "" {
			reason = "libclang helper reached a bounded fact limit"
		}
		a.coverage.partial(model.FactSymbol, reason)
		return nil
	}
	if event.Type == "fatal" {
		a.addContextIssue(event.Message)
		return nil
	}
	if event.Type == "diagnostic" {
		a.addContextIssue("compiler diagnostic: " + event.Message)
		return nil
	}
	if event.Type == "include" {
		sourceFile, ok := a.workspace.byRel[normalizeRel(event.Path)]
		if !ok {
			a.addContextIssue("include source could not be mapped to the immutable workspace")
			return nil
		}
		if event.TargetPath == "" {
			a.addContextIssue("included header is outside the captured workspace view")
			return nil
		}
		targetFile, ok := a.workspace.byRel[normalizeRel(event.TargetPath)]
		if !ok {
			a.addContextIssue("included header is absent from the captured workspace view")
			return nil
		}
		a.seenSource[normalizeRel(event.TargetPath)] = true
		return writer.edge(model.Edge{
			From:         fileSymbolID(sourceFile.URI),
			To:           fileSymbolID(targetFile.URI),
			ScopeID:      scope.ID,
			Kind:         model.EdgeInclude,
			SourceURI:    sourceFile.URI,
			Range:        convertPosition(a.workspace, sourceFile, event.Line, event.Column, event.EndLine, event.EndColumn),
			SourceHash:   sourceFile.SHA256,
			BuildContext: scope.BuildContext,
		})
	}
	path := normalizeRel(event.Path)
	file, hasFile := a.workspace.byRel[path]
	if event.Type == "external" {
		a.addContextIssue("a referenced declaration is outside the captured workspace view")
		return nil
	}
	if !hasFile {
		if event.Type == "edge" {
			// An external target may still be described by clang, but its source
			// and build context are not complete in this immutable snapshot.
			a.addContextIssue("semantic edge source or target is not mapped")
			return nil
		}
		return nil
	}
	if event.Type == "symbol" {
		return writer.symbol(model.Symbol{
			ID:        symbolID(event.USR, event.Name, event.Kind, event.Path, event.Line, event.Column),
			ScopeID:   scope.ID,
			Name:      event.Name,
			Kind:      event.Kind,
			Signature: event.Signature,
		})
	}
	if event.Type == "occurrence" {
		role := event.Role
		if role == "macro_expansion" {
			a.coverage.partial(model.FactReference, "macro expansion spelling and expansion locations are not both representable in the fact model")
			role = "reference"
		}
		if event.SpellingPath != "" && normalizeRel(event.SpellingPath) != path {
			a.coverage.partial(model.FactReference, "macro spelling and expansion locations map to different source files")
		}
		if role != "declaration" && role != "definition" && role != "reference" {
			return nil
		}
		if role == "reference" && event.Path == "" {
			return nil
		}
		position := convertPosition(a.workspace, file, event.Line, event.Column, event.EndLine, event.EndColumn)
		return writer.occurrence(model.Occurrence{
			SymbolID:     symbolID(event.USR, event.Name, event.Kind, event.Path, event.Line, event.Column),
			ScopeID:      scope.ID,
			URI:          file.URI,
			Range:        position,
			Role:         role,
			SourceHash:   file.SHA256,
			BuildContext: scope.BuildContext,
		})
	}
	if event.Type == "edge" {
		kind, _, ok := edgeKind(event.EdgeKind)
		if !ok {
			return nil
		}
		from := symbolID(event.USR, "", "", "", 0, 0)
		to := symbolID(event.Target, "", "", "", 0, 0)
		position := convertPosition(a.workspace, file, event.Line, event.Column, event.EndLine, event.EndColumn)
		if strings.HasPrefix(event.USR, "file:") {
			from = fileSymbolID(file.URI)
		}
		return writer.edge(model.Edge{
			From:         from,
			To:           to,
			ScopeID:      scope.ID,
			Kind:         kind,
			SourceURI:    file.URI,
			Range:        position,
			SourceHash:   file.SHA256,
			BuildContext: scope.BuildContext,
		})
	}
	return nil
}

func (a *analyzeState) finish() {
	if a.workspace.positionIncomplete {
		a.addContextIssue("UTF-16 position mapping exceeded the bounded immutable source cache")
	}
	for _, file := range a.files {
		rel := a.workspace.uriRel[file.URI]
		if len(a.packagePatterns) > 0 {
			if isCHeader(file.URI) || !matchesPackagePatterns(rel, a.packagePatterns) {
				continue
			}
		}
		if (!isCHeader(file.URI) && !isCSource(file.URI)) || a.seenSource[rel] {
			continue
		}
		if isCHeader(file.URI) {
			a.addContextIssue("header has no compile-command translation-unit context: " + file.URI)
		} else {
			a.addContextIssue("translation unit has no compile-command context: " + file.URI)
		}
	}
	if len(a.issues) > 0 {
		reason := strings.Join(uniqueStrings(a.issues), "; ")
		for _, fact := range []model.FactKind{
			model.FactSymbol, model.FactDeclaration, model.FactDefinition,
			model.FactReference, model.FactImplementation, model.FactTypeRelation,
			model.FactCall, model.FactImport, model.FactInclude,
		} {
			a.coverage.partial(fact, reason)
		}
	}
	a.coverage.unknown(model.FactImport, "libclang does not provide an attested immutable source or PCM identity for module imports")
	a.coverage.unknown(model.FactModule, "libclang C index exposes imported module metadata but no stable module declaration identity or complete module graph")
	for _, file := range a.files {
		if file.Generated && file.SourceURI == "" {
			a.coverage.partial(model.FactGenerated, "generated source has no immutable source mapping: "+file.URI)
		}
	}
}

func edgeKind(value string) (model.EdgeKind, model.FactKind, bool) {
	switch value {
	case "call":
		return model.EdgeCall, model.FactCall, true
	case "type_relation":
		return model.EdgeTypeRelation, model.FactTypeRelation, true
	case "implementation":
		return model.EdgeImplementation, model.FactImplementation, true
	case "import":
		return model.EdgeImport, model.FactImport, true
	case "module":
		return model.EdgeModule, model.FactModule, true
	default:
		return "", "", false
	}
}

func symbolID(usr, name, kind, path string, line, column uint32) identity.SymbolID {
	key := usr
	if key == "" {
		key = strings.Join([]string{kind, name, path, strconv.FormatUint(uint64(line), 10), strconv.FormatUint(uint64(column), 10)}, "\x00")
	}
	sum := sha256.Sum256([]byte(key))
	return identity.SymbolID("clang:" + hex.EncodeToString(sum[:]))
}

func fileSymbolID(uri string) identity.SymbolID {
	sum := sha256.Sum256([]byte("file:" + uri))
	return identity.SymbolID("clang-file:" + hex.EncodeToString(sum[:]))
}

func isCHeader(uri string) bool {
	switch strings.ToLower(filepath.Ext(uriPath(uri))) {
	case ".h", ".hh", ".hpp", ".hxx", ".inc":
		return true
	default:
		return false
	}
}

func convertPosition(workspace *workspace, file model.File, line, column, endLine, endColumn uint32) model.Position {
	if line == 0 {
		return model.Position{}
	}
	rel := workspace.uriRel[file.URI]
	data, loaded := workspace.positionData[rel]
	if !loaded {
		if file.Size < 0 || file.Size > maxPositionCache || workspace.positionBytes+file.Size > maxPositionCache {
			workspace.positionIncomplete = true
			return approximatePosition(line, column, endLine, endColumn)
		}
		var err error
		data, err = os.ReadFile(filepath.Join(workspace.root, filepath.FromSlash(rel)))
		if err != nil {
			workspace.positionIncomplete = true
			return approximatePosition(line, column, endLine, endColumn)
		}
		workspace.positionBytes += int64(len(data))
		workspace.positionData[rel] = data
	}
	if !utf8.Valid(data) {
		workspace.positionIncomplete = true
		return approximatePosition(line, column, endLine, endColumn)
	}
	if endLine == 0 {
		endLine = line
	}
	return model.Position{
		StartLine: line - 1,
		StartChar: sourcePosition(data, line, column),
		EndLine:   maxU32(line-1, endLine-1),
		EndChar:   sourcePosition(data, endLine, endColumn),
	}
}

func approximatePosition(line, column, endLine, endColumn uint32) model.Position {
	if line == 0 {
		return model.Position{}
	}
	if endLine == 0 {
		endLine = line
	}
	return model.Position{
		StartLine: line - 1,
		StartChar: maxU32(column, 1) - 1,
		EndLine:   maxU32(line-1, endLine-1),
		EndChar:   maxU32(endColumn, 1) - 1,
	}
}

func maxU32(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}

func sourcePosition(data []byte, line, column uint32) uint32 {
	if line == 0 || column == 0 {
		return 0
	}
	start := 0
	current := uint32(1)
	for i, b := range data {
		if current == line {
			start = i
			break
		}
		if b == '\n' {
			current++
			start = i + 1
		}
	}
	end := start + int(column-1)
	for end < len(data) && end > start && !utf8.RuneStart(data[end]) {
		end++
	}
	if end > len(data) {
		end = len(data)
	}
	units := uint32(0)
	for _, r := range string(data[start:end]) {
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
	}
	return units
}

type factWriter struct {
	ctx         context.Context
	sink        model.Sink
	scope       model.Scope
	symbols     []model.Symbol
	occurrences []model.Occurrence
	edges       []model.Edge
}

func newFactWriter(ctx context.Context, sink model.Sink, scope model.Scope) *factWriter {
	return &factWriter{ctx: ctx, sink: sink, scope: scope}
}

func (w *factWriter) symbol(value model.Symbol) error {
	w.symbols = append(w.symbols, value)
	if len(w.symbols) >= batchSize {
		return w.flushSymbols()
	}
	return nil
}

func (w *factWriter) occurrence(value model.Occurrence) error {
	w.occurrences = append(w.occurrences, value)
	if len(w.occurrences) >= batchSize {
		return w.flushOccurrences()
	}
	return nil
}

func (w *factWriter) edge(value model.Edge) error {
	w.edges = append(w.edges, value)
	if len(w.edges) >= batchSize {
		return w.flushEdges()
	}
	return nil
}

func (w *factWriter) flush() error {
	if err := w.flushSymbols(); err != nil {
		return err
	}
	if err := w.flushOccurrences(); err != nil {
		return err
	}
	return w.flushEdges()
}

func (w *factWriter) flushSymbols() error {
	if len(w.symbols) == 0 {
		return nil
	}
	if err := w.sink.WriteSymbols(w.ctx, w.symbols); err != nil {
		return err
	}
	w.symbols = nil
	return nil
}

func (w *factWriter) flushOccurrences() error {
	if len(w.occurrences) == 0 {
		return nil
	}
	if err := w.sink.WriteOccurrences(w.ctx, w.occurrences); err != nil {
		return err
	}
	w.occurrences = nil
	return nil
}

func (w *factWriter) flushEdges() error {
	if len(w.edges) == 0 {
		return nil
	}
	if err := w.sink.WriteEdges(w.ctx, w.edges); err != nil {
		return err
	}
	w.edges = nil
	return nil
}

func runTranslationUnit(ctx context.Context, artifact *helperArtifact, scope model.Scope, command compileCommand, workspace *workspace, analysis *analyzeState, writer *factWriter) error {
	args := []string{"--root", workspace.root, "--source", command.file, "--"}
	args = append(args, command.args...)
	args = append(args, "-fmodules-cache-path="+filepath.Join(artifact.dir, "module-cache"))
	cmd := exec.CommandContext(ctx, artifact.path, args...)
	cmd.Dir = artifact.dir
	cmd.Env = compilerEnvironment(os.Environ(), scope.Build.Environment, artifact.libDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr boundedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start libclang helper: %w", err)
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxEventLine)
	var streamErr error
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			streamErr = err
			break
		}
		var event rawEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			streamErr = fmt.Errorf("decode libclang helper event: %w", err)
			break
		}
		if err := analysis.addEvent(event, scope, writer); err != nil {
			streamErr = err
			break
		}
	}
	if streamErr == nil && scanner.Err() != nil {
		streamErr = scanner.Err()
	}
	if streamErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if streamErr != nil {
		return streamErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return fmt.Errorf("libclang helper failed: %w (%s)", waitErr, message)
		}
		return fmt.Errorf("libclang helper failed: %w", waitErr)
	}
	analysis.processed++
	return nil
}

type coverageBuilder struct {
	scope   string
	states  map[model.FactKind]model.Completeness
	reasons map[model.FactKind][]string
}

func newCoverage(scope string) *coverageBuilder {
	result := &coverageBuilder{scope: scope, states: make(map[model.FactKind]model.Completeness), reasons: make(map[model.FactKind][]string)}
	for _, fact := range model.RequiredFactKinds {
		result.states[fact] = model.Complete
	}
	return result
}

func (c *coverageBuilder) unavailableAll(reason string) {
	for _, fact := range model.RequiredFactKinds {
		c.set(fact, model.Unavailable, reason)
	}
}

func (c *coverageBuilder) unknownAll(reason string) {
	for _, fact := range model.RequiredFactKinds {
		c.set(fact, model.Unknown, reason)
	}
}

func (c *coverageBuilder) partial(fact model.FactKind, reason string) {
	c.set(fact, model.IncompleteKnownSubset, reason)
}

func (c *coverageBuilder) unknown(fact model.FactKind, reason string) {
	if c.states[fact] == model.Complete {
		c.set(fact, model.Unknown, reason)
	}
}

func (c *coverageBuilder) set(fact model.FactKind, state model.Completeness, reason string) {
	previous, ok := c.states[fact]
	if ok {
		if previous == model.Unavailable || previous == model.Unknown && state == model.IncompleteKnownSubset {
			return
		}
		if previous == model.IncompleteKnownSubset && state == model.Unknown {
			return
		}
	}
	c.states[fact] = state
	if reason != "" {
		reasons := c.reasons[fact]
		for _, existing := range reasons {
			if existing == reason {
				return
			}
		}
		if len(reasons) < maxCoverageReasons {
			c.reasons[fact] = append(reasons, reason)
		}
	}
}

func (c *coverageBuilder) results() []model.Coverage {
	result := make([]model.Coverage, 0, len(model.RequiredFactKinds))
	for _, fact := range model.RequiredFactKinds {
		state := c.states[fact]
		reason := ""
		if state != model.Complete {
			reason = strings.Join(uniqueStrings(c.reasons[fact]), "; ")
			if reason == "" {
				reason = "semantic coverage could not be established"
			}
		}
		result = append(result, model.Coverage{ScopeID: c.scope, Fact: fact, State: state, Reason: reason})
	}
	return result
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func withEnvironment(base []string, values map[string]string) []string {
	if len(values) == 0 {
		return base
	}
	result := make([]string, 0, len(base)+len(values))
	for _, item := range base {
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			result = append(result, item)
			continue
		}
		replaced := false
		for valueName := range values {
			if name == valueName || filepath.Separator == '\\' && strings.EqualFold(name, valueName) {
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, item)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func fillMissingCoverage(report *model.Report, scopes []model.Scope) {
	seen := make(map[string]struct{}, len(report.Coverage))
	for _, coverage := range report.Coverage {
		seen[coverage.ScopeID+"\x00"+string(coverage.Fact)] = struct{}{}
	}
	for _, scope := range scopes {
		for _, fact := range model.RequiredFactKinds {
			key := scope.ID + "\x00" + string(fact)
			if _, ok := seen[key]; ok {
				continue
			}
			report.Coverage = append(report.Coverage, model.Coverage{
				ScopeID: scope.ID,
				Fact:    fact,
				State:   model.Unknown,
				Reason:  "extractor stopped before this scope was processed",
			})
		}
	}
}

func hasPrecompiledInput(args []string) bool {
	for _, arg := range args {
		arg = strings.ToLower(arg)
		if arg == "-include-pch" || strings.HasPrefix(arg, "-include-pch=") ||
			strings.HasSuffix(arg, ".pch") || strings.HasSuffix(arg, ".gch") ||
			strings.HasSuffix(arg, ".pcm") || strings.Contains(arg, "-fmodule-file") ||
			strings.Contains(arg, "-fprebuilt-module-path") || strings.HasPrefix(arg, "@") ||
			arg == "-emit-pch" || arg == "-emit-module" || strings.HasPrefix(arg, "-fmodule-output") ||
			arg == "--config" || strings.HasPrefix(arg, "--config=") ||
			arg == "-load" || strings.HasPrefix(arg, "-fplugin=") {
			return true
		}
	}
	return false
}

func isCSource(uri string) bool {
	switch strings.ToLower(filepath.Ext(uriPath(uri))) {
	case ".c", ".i", ".cc", ".cpp", ".cxx", ".c++", ".m", ".mm":
		return true
	default:
		return false
	}
}

func matchesPackagePatterns(rel string, patterns []string) bool {
	path := filepath.ToSlash(rel)
	for _, pattern := range patterns {
		pattern = filepath.ToSlash(strings.TrimSpace(pattern))
		if pattern == "" || pattern == "." || pattern == "./..." || pattern == "..." {
			return true
		}
		if strings.HasSuffix(pattern, "/...") {
			prefix := strings.TrimSuffix(pattern, "/...")
			if path == prefix || strings.HasPrefix(path, prefix+"/") {
				return true
			}
			continue
		}
		if matched, _ := filepath.Match(pattern, path); matched {
			return true
		}
		if matched, _ := filepath.Match(pattern, filepath.Base(path)); matched {
			return true
		}
	}
	return false
}

func looksLikeTestTranslationUnit(rel string) bool {
	base := strings.ToLower(filepath.Base(rel))
	if strings.HasSuffix(base, "_test.cpp") || strings.HasSuffix(base, "_test.cc") || strings.HasSuffix(base, "_test.c") || strings.HasSuffix(base, "_tests.cpp") {
		return true
	}
	for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.EqualFold(segment, "test") || strings.EqualFold(segment, "tests") {
			return true
		}
	}
	return false
}

type boundedBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (b *boundedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.limit == 0 {
		b.limit = maxStderrBytes
	}
	if len(value) >= b.limit {
		b.data = append(b.data[:0], value[len(value)-b.limit:]...)
		return len(value), nil
	}
	if over := len(b.data) + len(value) - b.limit; over > 0 {
		copy(b.data, b.data[over:])
		b.data = b.data[:len(b.data)-over]
	}
	b.data = append(b.data, value...)
	return len(value), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(bytes.Clone(b.data))
}

var _ = utf8.RuneLen
