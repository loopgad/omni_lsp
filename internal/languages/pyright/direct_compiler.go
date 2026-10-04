package pyright

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
	"sort"
	"strings"
	"time"

	"github.com/omnilsp/omni/internal/index/model"
)

const (
	pyrightInternalSHA256 = "41fb260ad72f2a16c5657e1f2fe837104d7529255d0fcd955f48432b276e7cae"
	pyrightVendorSHA256   = "1c691bf9e3fa954dec751ace203beb5591b46a73e3848af5d1ee011d62540798"
	semanticProbeTimeout  = 5 * time.Second
)

//go:embed pyright_exporter.js
var pyrightExporterScript []byte

// DirectPyrightRunner runs the exact Pyright analyzer bundle through its
// analyzer program. The private API is deliberately tied to the pinned bundle
// hashes above; a new upstream version must be audited before it can export.
type DirectPyrightRunner struct {
	NodePath string
}

var _ PyrightSemanticRunner = (*DirectPyrightRunner)(nil)

func NewDirectPyrightRunner(nodePath string) *DirectPyrightRunner {
	return &DirectPyrightRunner{NodePath: strings.TrimSpace(nodePath)}
}

type directPyrightFile struct {
	URI          string `json:"uri"`
	Path         string `json:"path"`
	Language     string `json:"languageID,omitempty"`
	Generated    bool   `json:"generated,omitempty"`
	ScopeID      string `json:"scopeID"`
	BuildContext string `json:"buildContext"`
}

type directPyrightRequest struct {
	RootPath       string              `json:"rootPath"`
	ConfigPath     string              `json:"configPath"`
	ConfigDigest   string              `json:"configDigest"`
	ScopeID        string              `json:"scopeID"`
	BuildContext   string              `json:"buildContext"`
	CompilerPath   string              `json:"compilerPath"`
	VendorPath     string              `json:"vendorPath"`
	PythonPath     string              `json:"pythonPath"`
	PythonVersion  string              `json:"pythonVersion,omitempty"`
	PythonPlatform string              `json:"pythonPlatform,omitempty"`
	StubPath       string              `json:"stubPath,omitempty"`
	IncludePaths   []string            `json:"includePaths,omitempty"`
	Files          []directPyrightFile `json:"files"`
	ProjectFiles   []directPyrightFile `json:"projectFiles,omitempty"`
}

type directPyrightMessage struct {
	Type   string                `json:"Type"`
	Batch  PyrightSemanticBatch  `json:"Batch"`
	Result PyrightSemanticResult `json:"Result"`
	Error  string                `json:"Error"`
}

func (r *DirectPyrightRunner) VerifyTools(ctx context.Context, request PyrightSemanticRequest) ([]model.ToolIdentity, error) {
	if r == nil {
		return nil, errors.New("nil Pyright semantic runner")
	}
	tools, err := directPyrightTools(request.Tools)
	if err != nil {
		return nil, err
	}
	for _, tool := range []model.ToolIdentity{tools.node, tools.compiler, tools.vendor, tools.exporter, tools.python} {
		if err := verifyPyrightToolFile(ctx, tool); err != nil {
			return nil, err
		}
	}
	if !strings.EqualFold(tools.compiler.SHA256, pyrightInternalSHA256) || !strings.EqualFold(tools.vendor.SHA256, pyrightVendorSHA256) {
		return nil, errors.New("Pyright analyzer bundles do not match the audited 1.1.414 hashes")
	}
	if err := r.verifyNode(ctx, tools.node); err != nil {
		return nil, err
	}
	if err := verifyEmbeddedExporter(tools.exporter); err != nil {
		return nil, err
	}
	probe := fmt.Sprintf("const p=require(%q); if (p.version !== %q) process.exit(2); require(%q);", filepath.ToSlash(filepath.Join(filepath.Dir(tools.compiler.Path), "..", "package.json")), semanticIndexCompilerVersion, filepath.ToSlash(tools.compiler.Path))
	probeCtx, cancel := context.WithTimeout(ctx, semanticProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, tools.node.Path, "-e", probe)
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		if probeCtx.Err() != nil {
			return nil, probeCtx.Err()
		}
		return nil, fmt.Errorf("load pinned Pyright analyzer with Node: %w (%s)", runErr, strings.TrimSpace(string(output)))
	}
	return append([]model.ToolIdentity(nil), request.Tools...), nil
}

func (r *DirectPyrightRunner) Export(ctx context.Context, request PyrightSemanticRequest, emit func(PyrightSemanticBatch) error) (PyrightSemanticResult, error) {
	if r == nil {
		return PyrightSemanticResult{}, errors.New("nil Pyright semantic runner")
	}
	if emit == nil {
		return PyrightSemanticResult{}, errors.New("nil Pyright semantic batch sink")
	}
	if err := ctx.Err(); err != nil {
		return PyrightSemanticResult{}, err
	}
	tools, err := directPyrightTools(request.Tools)
	if err != nil {
		return PyrightSemanticResult{}, err
	}
	if err := verifyPyrightToolFile(ctx, tools.compiler); err != nil {
		return PyrightSemanticResult{}, err
	}
	if err := verifyPyrightToolFile(ctx, tools.vendor); err != nil {
		return PyrightSemanticResult{}, err
	}
	if err := verifyPyrightToolFile(ctx, tools.node); err != nil {
		return PyrightSemanticResult{}, err
	}
	if err := verifyPyrightToolFile(ctx, tools.exporter); err != nil {
		return PyrightSemanticResult{}, err
	}
	if err := verifyPyrightToolFile(ctx, tools.python); err != nil {
		return PyrightSemanticResult{}, err
	}
	if !strings.EqualFold(tools.compiler.SHA256, pyrightInternalSHA256) || !strings.EqualFold(tools.vendor.SHA256, pyrightVendorSHA256) {
		return PyrightSemanticResult{}, errors.New("Pyright analyzer bundles do not match the audited 1.1.414 hashes")
	}
	if err := r.verifyNode(ctx, tools.node); err != nil {
		return PyrightSemanticResult{}, err
	}
	if err := verifyEmbeddedExporter(tools.exporter); err != nil {
		return PyrightSemanticResult{}, err
	}
	payload, err := makeDirectPyrightRequest(request, tools)
	if err != nil {
		return PyrightSemanticResult{}, err
	}
	return r.runScript(ctx, tools.node.Path, payload, emit)
}

type directPyrightToolSet struct {
	node, compiler, vendor, exporter, python model.ToolIdentity
}

func directPyrightTools(tools []model.ToolIdentity) (directPyrightToolSet, error) {
	var out directPyrightToolSet
	seen := make(map[string]bool, 4)
	for _, tool := range tools {
		var target *model.ToolIdentity
		switch tool.Name {
		case semanticIndexRuntimeName:
			target = &out.node
		case semanticIndexCompilerName:
			target = &out.compiler
		case semanticIndexVendorName:
			target = &out.vendor
		case semanticIndexExporterName:
			target = &out.exporter
		case "python":
			target = &out.python
		default:
			continue
		}
		if seen[tool.Name] {
			return out, fmt.Errorf("multiple pinned Pyright tools named %q", tool.Name)
		}
		seen[tool.Name] = true
		*target = tool
	}
	for _, item := range []struct {
		name string
		tool model.ToolIdentity
	}{
		{semanticIndexRuntimeName, out.node},
		{semanticIndexCompilerName, out.compiler},
		{semanticIndexVendorName, out.vendor},
		{semanticIndexExporterName, out.exporter},
		{"python", out.python},
	} {
		if item.tool.Name == "" {
			return out, fmt.Errorf("pinned Pyright tool %q is missing", item.name)
		}
		if !filepath.IsAbs(item.tool.Path) || !fixedToolVersion(item.tool.Version) || item.tool.SHA256 == "" {
			return out, fmt.Errorf("pinned Pyright tool %q has invalid identity metadata", item.name)
		}
	}
	if out.compiler.Version != semanticIndexCompilerVersion || out.vendor.Version != semanticIndexCompilerVersion {
		return out, fmt.Errorf("pinned Pyright bundle version must be %s", semanticIndexCompilerVersion)
	}
	if out.node.Version != semanticIndexRuntimeVersion {
		return out, fmt.Errorf("pinned Node version %q does not match required %s", out.node.Version, semanticIndexRuntimeVersion)
	}
	if out.exporter.Version != semanticIndexExtractorVersion {
		return out, fmt.Errorf("pinned Pyright exporter version %q does not match required %s", out.exporter.Version, semanticIndexExtractorVersion)
	}
	if out.python.Version == "" {
		return out, errors.New("pinned Python interpreter identity is missing")
	}
	return out, nil
}

func (r *DirectPyrightRunner) verifyNode(ctx context.Context, node model.ToolIdentity) error {
	selected := filepath.Clean(node.Path)
	if r != nil && r.NodePath != "" {
		if !filepath.IsAbs(r.NodePath) {
			return fmt.Errorf("NodePath must be absolute: %q", r.NodePath)
		}
		if !samePyrightExecutable(r.NodePath, node.Path) {
			return fmt.Errorf("NodePath %q does not match pinned Node executable %q", r.NodePath, node.Path)
		}
		selected = filepath.Clean(r.NodePath)
	}
	output, err := exec.CommandContext(ctx, selected, "--version").CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("probe pinned Node executable: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	version := normalizePyrightRuntimeVersion(string(output))
	if version != semanticIndexRuntimeVersion || version != normalizePyrightRuntimeVersion(node.Version) {
		return fmt.Errorf("pinned Node executable reports %q; expected %s", version, semanticIndexRuntimeVersion)
	}
	return nil
}

func makeDirectPyrightRequest(request PyrightSemanticRequest, tools directPyrightToolSet) (directPyrightRequest, error) {
	if request.Materialized == nil || request.Materialized.RootPath() == "" {
		return directPyrightRequest{}, errors.New("missing materialized Pyright project")
	}
	rootPath, err := filepath.Abs(request.Materialized.RootPath())
	if err != nil {
		return directPyrightRequest{}, err
	}
	configName := pythonBuildOption(request.Scope.Build.Options, "pyrightconfig", "projectconfig")
	if configName == "" {
		return directPyrightRequest{}, errors.New("Pyright project config path is missing from BuildInputs")
	}
	configDigest := pythonBuildOption(request.Scope.Build.Options, "pyrightconfigdigest", "projectconfigdigest")
	if !validDigest(configDigest) {
		return directPyrightRequest{}, errors.New("Pyright project config digest is invalid")
	}
	configPath := ""
	if configName == "<default>" {
		if !strings.EqualFold(strings.TrimPrefix(configDigest, "sha256:"), strings.TrimPrefix(defaultPyrightConfigDigest(), "sha256:")) {
			return directPyrightRequest{}, errors.New("default Pyright config digest is invalid")
		}
	} else {
		if filepath.IsAbs(configName) {
			return directPyrightRequest{}, fmt.Errorf("Pyright config path must be relative to the immutable root: %q", configName)
		}
		configPath = filepath.Join(rootPath, filepath.FromSlash(configName))
		if !withinRoot(rootPath, configPath) {
			return directPyrightRequest{}, fmt.Errorf("Pyright config path escapes the immutable root: %q", configName)
		}
		if err := verifyPyrightConfigDigest(configPath, configDigest); err != nil {
			return directPyrightRequest{}, err
		}
	}
	pythonPath := pythonBuildEnvironment(request.Scope.Build.Environment, "python", "pythoninterpreter", "pythonexecutable", "virtualenv")
	if pythonPath == "" || !filepath.IsAbs(pythonPath) {
		return directPyrightRequest{}, errors.New("Pyright interpreter path must be absolute in BuildInputs")
	}
	if !samePyrightExecutable(pythonPath, tools.python.Path) {
		return directPyrightRequest{}, errors.New("Pyright interpreter path differs from the pinned Python tool")
	}
	projectScopes := make(map[string]model.Scope, len(request.ProjectScopes)+1)
	projectScopes[request.Scope.ID] = request.Scope
	for _, scope := range request.ProjectScopes {
		if scope.ID == "" || scope.BuildContext == "" {
			return directPyrightRequest{}, errors.New("Pyright project-scope identity is incomplete")
		}
		if prior, exists := projectScopes[scope.ID]; exists && prior.BuildContext != scope.BuildContext {
			return directPyrightRequest{}, fmt.Errorf("Pyright project scope %q has conflicting build contexts", scope.ID)
		}
		projectScopes[scope.ID] = scope
	}
	files := make([]directPyrightFile, 0, len(request.Files))
	for _, file := range request.Files {
		extension := strings.ToLower(filepath.Ext(file.URI))
		if extension != ".py" && extension != ".pyi" {
			continue
		}
		if file.URI == "" {
			return directPyrightRequest{}, errors.New("Pyright immutable file manifest contains an empty URI")
		}
		localPath, pathErr := request.Materialized.PathForURI(file.URI)
		if pathErr != nil {
			return directPyrightRequest{}, fmt.Errorf("map Pyright source %q: %w", file.URI, pathErr)
		}
		if !withinRoot(rootPath, localPath) {
			return directPyrightRequest{}, fmt.Errorf("Pyright source %q escapes the immutable root", file.URI)
		}
		files = append(files, directPyrightFile{
			URI: file.URI, Path: localPath, Language: file.LanguageID, Generated: file.Generated,
			ScopeID: request.Scope.ID, BuildContext: string(request.Scope.BuildContext),
		})
	}
	if len(files) == 0 {
		return directPyrightRequest{}, errors.New("Pyright immutable scope contains no Python source files")
	}
	projectFiles := make([]directPyrightFile, 0)
	projectFileScopes := make([]string, 0, len(request.ProjectFiles))
	for scopeID := range request.ProjectFiles {
		projectFileScopes = append(projectFileScopes, scopeID)
	}
	sort.Strings(projectFileScopes)
	for _, scopeID := range projectFileScopes {
		scopedFiles := request.ProjectFiles[scopeID]
		projectScope, ok := projectScopes[scopeID]
		if !ok {
			return directPyrightRequest{}, fmt.Errorf("Pyright project file manifest has no scope identity for %q", scopeID)
		}
		if scopeID == request.Scope.ID || !uriWithinRoot(request.Scope.RootURI, projectScope.RootURI) {
			continue
		}
		for _, file := range scopedFiles {
			extension := strings.ToLower(filepath.Ext(file.URI))
			if extension != ".py" && extension != ".pyi" || !uriWithinRoot(request.Scope.RootURI, file.URI) {
				continue
			}
			localPath, pathErr := request.Materialized.PathForURI(file.URI)
			if pathErr != nil {
				return directPyrightRequest{}, fmt.Errorf("map nested Pyright source %q: %w", file.URI, pathErr)
			}
			if !withinRoot(rootPath, localPath) {
				return directPyrightRequest{}, fmt.Errorf("nested Pyright source %q escapes the immutable root", file.URI)
			}
			projectFiles = append(projectFiles, directPyrightFile{
				URI: file.URI, Path: localPath, Language: file.LanguageID, Generated: file.Generated,
				ScopeID: projectScope.ID, BuildContext: string(projectScope.BuildContext),
			})
		}
	}
	for _, includePath := range request.Scope.Build.IncludePaths {
		if filepath.IsAbs(includePath) || !withinRoot(rootPath, filepath.Join(rootPath, filepath.FromSlash(includePath))) {
			return directPyrightRequest{}, fmt.Errorf("Pyright import path escapes the immutable root: %q", includePath)
		}
	}
	return directPyrightRequest{
		RootPath: rootPath, ConfigPath: configPath, ConfigDigest: configDigest,
		ScopeID: request.Scope.ID, BuildContext: string(request.Scope.BuildContext),
		CompilerPath: tools.compiler.Path, VendorPath: tools.vendor.Path, PythonPath: pythonPath,
		PythonVersion:  pythonBuildOption(request.Scope.Build.Options, "pythonversion"),
		PythonPlatform: pythonBuildOption(request.Scope.Build.Options, "pythonplatform"),
		StubPath:       pythonBuildOption(request.Scope.Build.Options, "stubpath"),
		IncludePaths:   append([]string(nil), request.Scope.Build.IncludePaths...), Files: files, ProjectFiles: projectFiles,
	}, nil
}

func defaultPyrightConfigDigest() string {
	digest := sha256.Sum256([]byte(defaultPyrightConfigSentinel))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (r *DirectPyrightRunner) runScript(ctx context.Context, nodePath string, payload directPyrightRequest, emit func(PyrightSemanticBatch) error) (PyrightSemanticResult, error) {
	script, err := os.CreateTemp("", "omnilsp-pyright-exporter-*.js")
	if err != nil {
		return PyrightSemanticResult{}, fmt.Errorf("create embedded Pyright exporter: %w", err)
	}
	scriptPath := script.Name()
	defer func() { _ = os.Remove(scriptPath) }()
	if _, err := script.Write(pyrightExporterScript); err != nil {
		_ = script.Close()
		return PyrightSemanticResult{}, fmt.Errorf("write embedded Pyright exporter: %w", err)
	}
	if err := script.Close(); err != nil {
		return PyrightSemanticResult{}, fmt.Errorf("close embedded Pyright exporter: %w", err)
	}
	input, err := json.Marshal(payload)
	if err != nil {
		return PyrightSemanticResult{}, fmt.Errorf("encode Pyright exporter request: %w", err)
	}
	cmd := exec.CommandContext(ctx, nodePath, scriptPath)
	cmd.Stdin = strings.NewReader(string(input))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return PyrightSemanticResult{}, fmt.Errorf("open Pyright exporter stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return PyrightSemanticResult{}, fmt.Errorf("open Pyright exporter stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return PyrightSemanticResult{}, fmt.Errorf("start Pyright exporter: %w", err)
	}
	stderrDone := make(chan []byte, 1)
	go func() {
		value, _ := io.ReadAll(stderr)
		stderrDone <- value
	}()
	decoder := json.NewDecoder(bufio.NewReader(stdout))
	var result PyrightSemanticResult
	gotResult := false
	for {
		var message directPyrightMessage
		decodeErr := decoder.Decode(&message)
		if errors.Is(decodeErr, io.EOF) {
			break
		}
		if decodeErr != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return PyrightSemanticResult{}, fmt.Errorf("decode Pyright exporter output: %w", decodeErr)
		}
		switch message.Type {
		case "batch":
			if err := emit(message.Batch); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return PyrightSemanticResult{}, err
			}
		case "result":
			result = message.Result
			gotResult = true
		case "error":
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return PyrightSemanticResult{}, errors.New(message.Error)
		default:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return PyrightSemanticResult{}, fmt.Errorf("Pyright exporter emitted unknown message %q", message.Type)
		}
	}
	waitErr := cmd.Wait()
	stderrOutput := <-stderrDone
	if err := ctx.Err(); err != nil {
		return PyrightSemanticResult{}, err
	}
	if waitErr != nil {
		if detail := strings.TrimSpace(string(stderrOutput)); detail != "" {
			return PyrightSemanticResult{}, fmt.Errorf("Pyright exporter failed: %w (%s)", waitErr, detail)
		}
		return PyrightSemanticResult{}, fmt.Errorf("Pyright exporter failed: %w", waitErr)
	}
	if !gotResult {
		return PyrightSemanticResult{}, errors.New("Pyright exporter ended without a coverage result")
	}
	return result, nil
}

func verifyEmbeddedExporter(exporter model.ToolIdentity) error {
	if exporter.Version != semanticIndexExtractorVersion {
		return fmt.Errorf("pinned exporter version %q does not match required %s", exporter.Version, semanticIndexExtractorVersion)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve embedded Pyright exporter binary: %w", err)
	}
	if !samePyrightExecutable(self, exporter.Path) {
		return errors.New("pinned Pyright exporter is not the running OmniLSP binary")
	}
	return nil
}

func verifyPyrightToolFile(ctx context.Context, tool model.ToolIdentity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(tool.Path)
	if err != nil {
		return fmt.Errorf("open pinned Pyright tool %s: %w", tool.Name, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		if err == nil {
			err = errors.New("tool path is a directory")
		}
		return fmt.Errorf("stat pinned Pyright tool %s: %w", tool.Name, err)
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
			return fmt.Errorf("read pinned Pyright tool %s: %w", tool.Name, readErr)
		}
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), tool.SHA256) {
		return fmt.Errorf("pinned Pyright tool %s SHA-256 mismatch", tool.Name)
	}
	return nil
}

func samePyrightExecutable(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func normalizePyrightRuntimeVersion(value string) string {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimPrefix(strings.TrimPrefix(fields[0], "v"), "V")
}

func verifyPyrightConfigDigest(configPath, expected string) error {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read immutable Pyright config: %w", err)
	}
	hash := sha256.Sum256(content)
	actual := hex.EncodeToString(hash[:])
	expected = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(expected)), "sha256:")
	if actual != expected {
		return errors.New("immutable Pyright config digest differs from BuildInputs")
	}
	return nil
}

func pythonBuildOption(options map[string]string, names ...string) string {
	for key, value := range options {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
		for _, name := range names {
			if normalized == name {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func pythonBuildEnvironment(environment map[string]string, names ...string) string {
	for key, value := range environment {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
		for _, name := range names {
			if normalized == name {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func validDigest(value string) bool {
	value = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "sha256:")
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
