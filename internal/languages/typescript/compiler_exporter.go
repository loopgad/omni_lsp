package typescript

import (
	"bufio"
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
	"strings"

	"github.com/omnilsp/omni/internal/index/model"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

//go:embed compiler_exporter.js
var compilerExporterScript []byte

// DirectCompilerRunner drives the pinned TypeScript compiler through its
// public Program/TypeChecker API. It intentionally has no source parser of
// its own: all symbols, references, calls, and heritage edges come from the
// same compiler Program.
//
// NodePath is kept explicit so callers can pin the host runtime. An empty
// value, or the bare name "node", selects the absolute executable from the
// request's pinned ToolIdentity. An explicit absolute path must identify that
// same executable.
type DirectCompilerRunner struct {
	NodePath string
}

var _ SCIPRunner = (*DirectCompilerRunner)(nil)

func NewDirectCompilerRunner(nodePath string) *DirectCompilerRunner {
	return &DirectCompilerRunner{NodePath: strings.TrimSpace(nodePath)}
}

type directCompilerRequest struct {
	RootPath             string                `json:"rootPath"`
	ScopeRootPath        string                `json:"scopeRootPath"`
	ConfigPath           string                `json:"configPath"`
	ScopeID              string                `json:"scopeID"`
	BuildContext         string                `json:"buildContext"`
	CompilerPath         string                `json:"compilerPath"`
	ExpectedConfigDigest string                `json:"expectedConfigDigest,omitempty"`
	ExpectedOptions      string                `json:"expectedCompilerOptions,omitempty"`
	ExpectedReferences   string                `json:"expectedProjectReferences,omitempty"`
	ReferenceScopes      []directCompilerScope `json:"referenceScopes,omitempty"`
	Files                []directCompilerFile  `json:"files"`
}

type directCompilerScope struct {
	ScopeID              string               `json:"scopeID"`
	RootURI              string               `json:"rootURI"`
	RootPath             string               `json:"rootPath"`
	BuildContext         string               `json:"buildContext"`
	ConfigPath           string               `json:"configPath"`
	ExpectedConfigDigest string               `json:"expectedConfigDigest,omitempty"`
	ExpectedOptions      string               `json:"expectedCompilerOptions,omitempty"`
	ExpectedReferences   string               `json:"expectedProjectReferences,omitempty"`
	Files                []directCompilerFile `json:"files"`
}

type directCompilerFile struct {
	URI        string `json:"uri"`
	Path       string `json:"path"`
	Language   string `json:"languageID,omitempty"`
	SourceHash string `json:"sourceHash"`
	Generated  bool   `json:"generated,omitempty"`
}

type directCompilerMessage struct {
	Type   string     `json:"Type"`
	Batch  SCIPBatch  `json:"Batch"`
	Result SCIPResult `json:"Result"`
	Error  string     `json:"Error"`
}

// VerifyTools confirms that the pinned TypeScript module can actually be
// loaded by the selected Node executable and reports the same identities back
// to the provider. The provider performs the SHA-256 attestation before this
// method is called.
func (r *DirectCompilerRunner) VerifyTools(ctx context.Context, request SCIPRequest) ([]model.ToolIdentity, error) {
	if r == nil {
		return nil, errors.New("nil TypeScript compiler runner")
	}
	if request.Materialized == nil || request.Materialized.RootPath() == "" {
		return nil, errors.New("missing materialized TypeScript project")
	}
	_, nodePath, err := r.verifyNodeTool(ctx, request.Tools)
	if err != nil {
		return nil, err
	}
	compiler, err := directCompilerTool(request.Tools)
	if err != nil {
		return nil, err
	}
	if err := verifyDirectToolFile(ctx, compiler); err != nil {
		return nil, err
	}
	if err := verifyEmbeddedExporter(ctx, request.Tools); err != nil {
		return nil, err
	}
	modulePath, err := resolveCompilerModulePath(compiler.Path)
	if err != nil {
		return nil, err
	}
	probe := fmt.Sprintf("const ts=require(%q); if (ts.version !== %q) process.exit(2)", modulePath, semanticIndexCompilerVersion)
	cmd := exec.CommandContext(ctx, nodePath, "-e", probe)
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("load pinned TypeScript %s with Node: %w (%s)", semanticIndexCompilerVersion, runErr, strings.TrimSpace(string(output)))
	}
	return append([]model.ToolIdentity(nil), request.Tools...), nil
}

func (r *DirectCompilerRunner) ExportSCIP(ctx context.Context, request SCIPRequest, emit func(SCIPBatch) error) (SCIPResult, error) {
	if r == nil {
		return SCIPResult{}, errors.New("nil TypeScript compiler runner")
	}
	if emit == nil {
		return SCIPResult{}, errors.New("nil TypeScript batch sink")
	}
	if err := ctx.Err(); err != nil {
		return SCIPResult{}, err
	}
	if request.Materialized == nil || request.Materialized.RootPath() == "" {
		return SCIPResult{}, errors.New("missing materialized TypeScript project")
	}
	if strings.TrimSpace(string(request.Scope.BuildContext)) == "" {
		return SCIPResult{}, errors.New("missing TypeScript build context")
	}
	_, nodePath, err := r.verifyNodeTool(ctx, request.Tools)
	if err != nil {
		return SCIPResult{}, err
	}
	compiler, err := directCompilerTool(request.Tools)
	if err != nil {
		return SCIPResult{}, err
	}
	if err := verifyDirectToolFile(ctx, compiler); err != nil {
		return SCIPResult{}, err
	}
	if err := verifyEmbeddedExporter(ctx, request.Tools); err != nil {
		return SCIPResult{}, err
	}
	compilerModule, err := resolveCompilerModulePath(compiler.Path)
	if err != nil {
		return SCIPResult{}, err
	}
	if len(request.ReferenceScopes) != 0 {
		if request.WorkspaceRootURI == "" || request.Materialized.RootURI() != request.WorkspaceRootURI {
			return SCIPResult{}, errors.New("TypeScript project references require materialization at the explicit immutable workspace boundary")
		}
	}
	scopeInput, err := r.directScopeInput(ctx, request, request.Scope, request.Files)
	if err != nil {
		return SCIPResult{}, err
	}
	if len(scopeInput.Files) == 0 {
		return SCIPResult{}, errors.New("TypeScript scope manifest is empty")
	}
	referenceInputs := make([]directCompilerScope, 0, len(request.ReferenceScopes))
	for _, referenceScope := range request.ReferenceScopes {
		if err := ctx.Err(); err != nil {
			return SCIPResult{}, err
		}
		referenceFiles, ok := request.ReferenceFiles[referenceScope.ID]
		if !ok {
			return SCIPResult{}, fmt.Errorf("TypeScript project-reference file manifest is missing for scope %q", referenceScope.ID)
		}
		input, inputErr := r.directScopeInput(ctx, request, referenceScope, referenceFiles)
		if inputErr != nil {
			return SCIPResult{}, inputErr
		}
		referenceInputs = append(referenceInputs, input)
	}

	payload := directCompilerRequest{
		RootPath: request.Materialized.RootPath(), ScopeRootPath: scopeInput.RootPath, ConfigPath: scopeInput.ConfigPath,
		ScopeID: request.Scope.ID, BuildContext: string(request.Scope.BuildContext),
		CompilerPath: compilerModule, Files: scopeInput.Files, ReferenceScopes: referenceInputs,
		ExpectedConfigDigest: scopeInput.ExpectedConfigDigest, ExpectedOptions: scopeInput.ExpectedOptions,
		ExpectedReferences: scopeInput.ExpectedReferences,
	}
	return r.runScript(ctx, nodePath, payload, emit)
}

func (r *DirectCompilerRunner) directScopeInput(ctx context.Context, request SCIPRequest, scope model.Scope, sourceFiles []model.File) (directCompilerScope, error) {
	configURI, err := scopeConfigURI(scope)
	if err != nil {
		return directCompilerScope{}, fmt.Errorf("resolve TypeScript scope %q config: %w", scope.ID, err)
	}
	if !uriWithinRoot(request.Materialized.RootURI(), configURI) ||
		(request.WorkspaceRootURI != "" && !uriWithinRoot(request.WorkspaceRootURI, configURI)) {
		return directCompilerScope{}, fmt.Errorf("TypeScript config %q is outside the materialized immutable boundary", configURI)
	}
	configPath, err := request.Materialized.PathForURI(configURI)
	if err != nil {
		return directCompilerScope{}, fmt.Errorf("map TypeScript config %q: %w", configURI, err)
	}
	if !withinRoot(request.Materialized.RootPath(), configPath) {
		return directCompilerScope{}, fmt.Errorf("TypeScript config %q escapes materialized root", configURI)
	}
	configInfo, err := os.Stat(configPath)
	if err != nil {
		return directCompilerScope{}, fmt.Errorf("stat TypeScript config %q: %w", configURI, err)
	}
	if configInfo.IsDir() {
		return directCompilerScope{}, fmt.Errorf("TypeScript config %q is a directory", configURI)
	}
	rootPath, err := directMaterializedScopeRoot(scope.RootURI, request.Materialized)
	if err != nil {
		return directCompilerScope{}, err
	}
	files := make([]directCompilerFile, 0, len(sourceFiles))
	seen := make(map[string]struct{}, len(sourceFiles))
	for _, file := range sourceFiles {
		if err := ctx.Err(); err != nil {
			return directCompilerScope{}, err
		}
		if file.URI == "" || !uriWithinRoot(scope.RootURI, file.URI) {
			return directCompilerScope{}, fmt.Errorf("TypeScript scope %q manifest contains a source outside its root", scope.ID)
		}
		if _, exists := seen[file.URI]; exists {
			return directCompilerScope{}, fmt.Errorf("TypeScript scope %q manifest repeats URI %q", scope.ID, file.URI)
		}
		seen[file.URI] = struct{}{}
		localPath, mapErr := request.Materialized.PathForURI(file.URI)
		if mapErr != nil {
			return directCompilerScope{}, fmt.Errorf("map TypeScript source %q: %w", file.URI, mapErr)
		}
		if !withinRoot(request.Materialized.RootPath(), localPath) || !withinRoot(rootPath, localPath) {
			return directCompilerScope{}, fmt.Errorf("TypeScript source %q escapes its materialized project root", file.URI)
		}
		files = append(files, directCompilerFile{
			URI: file.URI, Path: localPath, Language: file.LanguageID,
			SourceHash: string(file.SHA256), Generated: file.Generated,
		})
	}
	return directCompilerScope{
		ScopeID: scope.ID, RootURI: scope.RootURI, RootPath: rootPath, BuildContext: string(scope.BuildContext), ConfigPath: configPath,
		ExpectedConfigDigest: directBuildOption(scope.Build.Options, "projectconfigdigest", "typescriptconfigdigest", "tsconfigdigest", "jsconfigdigest"),
		ExpectedOptions:      directBuildOption(scope.Build.Options, "compileroptions"),
		ExpectedReferences:   directBuildOption(scope.Build.Options, "projectreferences"),
		Files:                files,
	}, nil
}

func directMaterializedScopeRoot(scopeRootURI string, materialized model.MaterializedView) (string, error) {
	if !uriWithinRoot(materialized.RootURI(), scopeRootURI) {
		return "", fmt.Errorf("TypeScript project root %q is outside materialized root %q", scopeRootURI, materialized.RootURI())
	}
	materializedRoot, err := workspaceuri.Parse(materialized.RootURI())
	if err != nil {
		return "", err
	}
	materializedPath, err := materializedRoot.Path()
	if err != nil {
		return "", err
	}
	scopeRoot, err := workspaceuri.Parse(scopeRootURI)
	if err != nil {
		return "", err
	}
	scopePath, err := scopeRoot.Path()
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(materializedPath, scopePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("TypeScript project root %q escapes materialized root", scopeRootURI)
	}
	localPath := filepath.Join(materialized.RootPath(), relative)
	if !withinRoot(materialized.RootPath(), localPath) {
		return "", fmt.Errorf("TypeScript project root %q escapes materialized root", scopeRootURI)
	}
	return filepath.Abs(localPath)
}

func (r *DirectCompilerRunner) runScript(ctx context.Context, nodePath string, payload directCompilerRequest, emit func(SCIPBatch) error) (SCIPResult, error) {
	script, err := os.CreateTemp("", "omnilsp-typescript-exporter-*.js")
	if err != nil {
		return SCIPResult{}, fmt.Errorf("create TypeScript exporter script: %w", err)
	}
	scriptPath := script.Name()
	defer func() { _ = os.Remove(scriptPath) }()
	if _, err := script.Write(compilerExporterScript); err != nil {
		_ = script.Close()
		return SCIPResult{}, fmt.Errorf("write TypeScript exporter script: %w", err)
	}
	if err := script.Close(); err != nil {
		return SCIPResult{}, fmt.Errorf("close TypeScript exporter script: %w", err)
	}
	input, err := json.Marshal(payload)
	if err != nil {
		return SCIPResult{}, fmt.Errorf("encode TypeScript compiler request: %w", err)
	}

	// Cap each helper's V8 heap so two concurrent project exports leave room
	// for OmniLSP and operating-system overhead under the 8 GiB process-tree
	// gate. The runtime's process-tree monitor remains the hard total limit.
	cmd := exec.CommandContext(ctx, nodePath, "--max-old-space-size=3072", scriptPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return SCIPResult{}, fmt.Errorf("open TypeScript exporter stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return SCIPResult{}, fmt.Errorf("open TypeScript exporter stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return SCIPResult{}, fmt.Errorf("open TypeScript exporter stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return SCIPResult{}, fmt.Errorf("start Node TypeScript exporter: %w", err)
	}
	if _, err := stdin.Write(input); err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return SCIPResult{}, fmt.Errorf("write TypeScript compiler request: %w", err)
	}
	if err := stdin.Close(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return SCIPResult{}, fmt.Errorf("close TypeScript compiler request: %w", err)
	}
	stderrDone := make(chan []byte, 1)
	go func() {
		value, _ := io.ReadAll(stderr)
		stderrDone <- value
	}()

	decoder := json.NewDecoder(bufio.NewReader(stdout))
	var result SCIPResult
	gotResult := false
	for {
		var message directCompilerMessage
		decodeErr := decoder.Decode(&message)
		if errors.Is(decodeErr, io.EOF) {
			break
		}
		if decodeErr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return SCIPResult{}, fmt.Errorf("decode TypeScript exporter output: %w", decodeErr)
		}
		switch message.Type {
		case "batch":
			if err := emitDirectBatch(ctx, emit, message.Batch); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return SCIPResult{}, err
			}
		case "result":
			result = message.Result
			gotResult = true
		case "error":
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return SCIPResult{}, errors.New(message.Error)
		default:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return SCIPResult{}, fmt.Errorf("TypeScript exporter emitted unknown message %q", message.Type)
		}
	}
	waitErr := cmd.Wait()
	stderrOutput := <-stderrDone
	if err := ctx.Err(); err != nil {
		return SCIPResult{}, err
	}
	if waitErr != nil {
		if detail := strings.TrimSpace(string(stderrOutput)); detail != "" {
			return SCIPResult{}, fmt.Errorf("TypeScript exporter failed: %w (%s)", waitErr, detail)
		}
		return SCIPResult{}, fmt.Errorf("TypeScript exporter failed: %w", waitErr)
	}
	if !gotResult {
		return SCIPResult{}, errors.New("TypeScript exporter ended without a coverage result")
	}
	return result, nil
}

func emitDirectBatch(ctx context.Context, emit func(SCIPBatch) error, batch SCIPBatch) error {
	if len(batch.Symbols) == 0 && len(batch.Occurrences) == 0 && len(batch.Edges) == 0 {
		return nil
	}
	for start := 0; start < len(batch.Symbols); start += semanticIndexBatchLimit {
		end := start + semanticIndexBatchLimit
		if end > len(batch.Symbols) {
			end = len(batch.Symbols)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(SCIPBatch{Symbols: batch.Symbols[start:end]}); err != nil {
			return err
		}
	}
	for start := 0; start < len(batch.Occurrences); start += semanticIndexBatchLimit {
		end := start + semanticIndexBatchLimit
		if end > len(batch.Occurrences) {
			end = len(batch.Occurrences)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(SCIPBatch{Occurrences: batch.Occurrences[start:end]}); err != nil {
			return err
		}
	}
	for start := 0; start < len(batch.Edges); start += semanticIndexBatchLimit {
		end := start + semanticIndexBatchLimit
		if end > len(batch.Edges) {
			end = len(batch.Edges)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(SCIPBatch{Edges: batch.Edges[start:end]}); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func directCompilerTool(tools []model.ToolIdentity) (model.ToolIdentity, error) {
	var compiler model.ToolIdentity
	found := false
	for _, tool := range tools {
		if tool.Name != semanticIndexCompilerName {
			continue
		}
		if found {
			return model.ToolIdentity{}, fmt.Errorf("multiple pinned TypeScript %q tools", semanticIndexCompilerName)
		}
		compiler = tool
		found = true
	}
	if !found {
		return model.ToolIdentity{}, fmt.Errorf("pinned TypeScript %s tool is missing", semanticIndexCompilerVersion)
	}
	if compiler.Version != semanticIndexCompilerVersion {
		return model.ToolIdentity{}, fmt.Errorf("pinned TypeScript tool version %q does not match required %s", compiler.Version, semanticIndexCompilerVersion)
	}
	return compiler, nil
}

func directNodeTool(tools []model.ToolIdentity) (model.ToolIdentity, error) {
	var node model.ToolIdentity
	found := false
	for _, tool := range tools {
		if tool.Name != semanticIndexNodeName {
			continue
		}
		if found {
			return model.ToolIdentity{}, fmt.Errorf("multiple pinned Node %q tools", semanticIndexNodeName)
		}
		node = tool
		found = true
	}
	if !found {
		return model.ToolIdentity{}, fmt.Errorf("pinned Node executable %q is missing", semanticIndexNodeName)
	}
	if !fixedToolVersion(node.Version) {
		return model.ToolIdentity{}, fmt.Errorf("pinned Node version %q is not fixed", node.Version)
	}
	if !filepath.IsAbs(node.Path) {
		return model.ToolIdentity{}, fmt.Errorf("pinned Node executable path is not absolute: %q", node.Path)
	}
	return node, nil
}

func (r *DirectCompilerRunner) verifyNodeTool(ctx context.Context, tools []model.ToolIdentity) (model.ToolIdentity, string, error) {
	node, err := directNodeTool(tools)
	if err != nil {
		return model.ToolIdentity{}, "", err
	}
	if err := verifyDirectToolFile(ctx, node); err != nil {
		return model.ToolIdentity{}, "", err
	}
	selected := filepath.Clean(node.Path)
	requested := ""
	if r != nil {
		requested = strings.TrimSpace(r.NodePath)
	}
	if requested != "" && !strings.EqualFold(requested, "node") && !strings.EqualFold(requested, "node.exe") {
		if !filepath.IsAbs(requested) {
			return model.ToolIdentity{}, "", fmt.Errorf("NodePath must be the bare node name or an absolute pinned executable path: %q", requested)
		}
		if !sameExecutableFile(requested, node.Path) {
			return model.ToolIdentity{}, "", fmt.Errorf("NodePath %q does not match pinned Node executable %q", requested, node.Path)
		}
	}
	info, err := os.Stat(selected)
	if err != nil {
		return model.ToolIdentity{}, "", fmt.Errorf("stat pinned Node executable: %w", err)
	}
	if info.IsDir() {
		return model.ToolIdentity{}, "", fmt.Errorf("pinned Node executable is a directory: %q", selected)
	}
	output, err := exec.CommandContext(ctx, selected, "--version").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return model.ToolIdentity{}, "", ctx.Err()
		}
		return model.ToolIdentity{}, "", fmt.Errorf("probe pinned Node executable: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	actual := normalizeRuntimeVersion(string(output))
	expected := normalizeRuntimeVersion(node.Version)
	if actual == "" || actual != expected {
		return model.ToolIdentity{}, "", fmt.Errorf("pinned Node executable reports version %q, expected %q", actual, expected)
	}
	return node, selected, nil
}

func normalizeRuntimeVersion(value string) string {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(fields[0], "v"), "V")
}

func sameExecutableFile(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return os.SameFile(leftInfo, rightInfo)
}

func verifyDirectToolFile(ctx context.Context, tool model.ToolIdentity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(tool.Path) {
		return fmt.Errorf("pinned tool %s path is not absolute: %q", tool.Name, tool.Path)
	}
	if !fixedToolVersion(tool.Version) {
		return fmt.Errorf("pinned tool %s version %q is not fixed", tool.Name, tool.Version)
	}
	if len(tool.SHA256) != sha256.Size*2 {
		return fmt.Errorf("pinned tool %s has an invalid SHA-256", tool.Name)
	}
	if _, err := hex.DecodeString(tool.SHA256); err != nil {
		return fmt.Errorf("pinned tool %s has an invalid SHA-256: %w", tool.Name, err)
	}
	file, err := os.Open(tool.Path)
	if err != nil {
		return fmt.Errorf("open pinned tool %s: %w", tool.Name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat pinned tool %s: %w", tool.Name, err)
	}
	if info.IsDir() {
		return fmt.Errorf("pinned tool %s is a directory", tool.Name)
	}
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
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
			return fmt.Errorf("read pinned tool %s: %w", tool.Name, readErr)
		}
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), tool.SHA256) {
		return fmt.Errorf("pinned tool %s SHA-256 mismatch", tool.Name)
	}
	return nil
}

func verifyEmbeddedExporter(ctx context.Context, tools []model.ToolIdentity) error {
	var exporter *model.ToolIdentity
	for i := range tools {
		if tools[i].Name == semanticIndexDirectExporterName {
			if exporter != nil {
				return errors.New("multiple pinned direct TypeScript exporters")
			}
			exporter = &tools[i]
		}
	}
	if exporter == nil {
		return fmt.Errorf("pinned direct TypeScript exporter %q is missing", semanticIndexDirectExporterName)
	}
	if exporter.Version != semanticIndexExtractorVersion {
		return fmt.Errorf("pinned direct TypeScript exporter version %q does not match required %s", exporter.Version, semanticIndexExtractorVersion)
	}
	if err := verifyDirectToolFile(ctx, *exporter); err != nil {
		return err
	}
	// The adapter is embedded in the OmniLSP candidate. Pin the candidate
	// executable itself instead of materializing an independent script whose
	// path would change on every process restart and invalidate persistent data.
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve embedded TypeScript exporter executable: %w", err)
	}
	if !sameExecutableFile(self, exporter.Path) {
		return errors.New("pinned direct TypeScript exporter is not the running OmniLSP candidate")
	}
	return nil
}

func resolveCompilerModulePath(toolPath string) (string, error) {
	if !filepath.IsAbs(toolPath) {
		return "", fmt.Errorf("pinned TypeScript tool path is not absolute: %q", toolPath)
	}
	pathValue := filepath.Clean(toolPath)
	if info, err := os.Stat(pathValue); err != nil || info.IsDir() {
		if err == nil {
			err = errors.New("tool path is a directory")
		}
		return "", fmt.Errorf("pinned TypeScript tool path is unavailable: %w", err)
	}
	base := strings.ToLower(filepath.Base(pathValue))
	if base == "tsc.js" || base == "tsserver.js" || base == "_tsc.js" || base == "_tsserver.js" ||
		base == "tsc" || base == "tsc.cmd" || base == "tsc.ps1" || base == "tsserver" || base == "tsserver.cmd" || base == "tsserver.ps1" {
		directory := filepath.Dir(pathValue)
		candidates := []string{
			filepath.Join(directory, "typescript.js"),
			filepath.Join(directory, "..", "lib", "typescript.js"),
			filepath.Join(directory, "..", "typescript", "lib", "typescript.js"),
		}
		for _, candidate := range candidates {
			if _, err := os.Stat(candidate); err == nil {
				pathValue = candidate
				break
			}
		}
	}
	return filepath.Abs(pathValue)
}

func directConfigPath(scope model.Scope, rootPath string) (string, error) {
	value := directBuildOption(scope.Build.Options, "typescriptconfig", "tsconfig", "jsconfig", "projectconfig")
	if strings.TrimSpace(value) == "" {
		if scope.Language == "javascript" || scope.Language == "javascriptreact" {
			value = "jsconfig.json"
		} else {
			value = "tsconfig.json"
		}
	}
	value = strings.TrimSpace(value)
	if filepath.IsAbs(value) {
		return "", fmt.Errorf("TypeScript project config must be relative to materialized root: %q", value)
	}
	configPath := filepath.Join(rootPath, filepath.FromSlash(value))
	if !withinRoot(rootPath, configPath) {
		return "", fmt.Errorf("TypeScript project config escapes materialized root: %q", value)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		return "", fmt.Errorf("TypeScript project config %q: %w", value, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("TypeScript project config %q is a directory", value)
	}
	return filepath.Abs(configPath)
}

func directBuildOption(options map[string]string, names ...string) string {
	for key, value := range options {
		normalized := normalizeBuildOption(key)
		for _, name := range names {
			if normalized == name {
				return value
			}
		}
	}
	return ""
}
