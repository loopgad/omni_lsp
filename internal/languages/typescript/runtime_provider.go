package typescript

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages/nested"
)

const (
	semanticIndexNodeVersion = "24.14.0"
	semanticNodePathEnv      = "OMNILSP_SEMANTIC_NODE_PATH"
	semanticTSPathEnv        = "OMNILSP_SEMANTIC_TYPESCRIPT_PATH"
	toolProbeTimeout         = 5 * time.Second
)

// NewPinnedSemanticIndexProvider wires the native TypeScript compiler
// exporter to explicit absolute tool paths when configured, otherwise to the
// resolved Node and TypeScript tools on PATH. Missing, changed, or unsupported
// identities disable semantic indexing without affecting the live LSP backend.
func NewPinnedSemanticIndexProvider(ctx context.Context, backend *Backend) (*SemanticIndexProvider, error) {
	if backend == nil {
		return nil, errors.New("typescript semantic index: nil live backend")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	nodeCommand, err := configuredToolPath(semanticNodePathEnv, "node")
	if err != nil {
		return nil, fmt.Errorf("resolve pinned Node runtime: %w", err)
	}
	nodePath, nodeHash, err := nested.ExecutableIdentity(nodeCommand)
	if err != nil {
		return nil, fmt.Errorf("identify Node runtime: %w", err)
	}
	nodeVersion, err := probeToolVersion(ctx, nodePath, "--version")
	if err != nil {
		return nil, fmt.Errorf("probe Node runtime: %w", err)
	}
	if nodeVersion != semanticIndexNodeVersion {
		return nil, fmt.Errorf("Node version %s is unsupported; require %s", nodeVersion, semanticIndexNodeVersion)
	}

	tscCommand, err := configuredToolPath(semanticTSPathEnv, "tsc")
	if err != nil {
		return nil, fmt.Errorf("resolve TypeScript compiler: %w", err)
	}
	compilerModule, err := resolveCompilerModulePath(tscCommand)
	if err != nil {
		return nil, fmt.Errorf("resolve TypeScript compiler module: %w", err)
	}
	compilerPath, compilerHash, err := nested.ExecutableIdentity(compilerModule)
	if err != nil {
		return nil, fmt.Errorf("identify TypeScript compiler module: %w", err)
	}
	compilerVersion, err := probeTypeScriptVersion(ctx, nodePath, compilerPath)
	if err != nil {
		return nil, fmt.Errorf("probe TypeScript compiler: %w", err)
	}
	if compilerVersion != semanticIndexCompilerVersion {
		return nil, fmt.Errorf("TypeScript compiler version %s is unsupported; require %s", compilerVersion, semanticIndexCompilerVersion)
	}

	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve OmniLSP executable for exporter provenance: %w", err)
	}
	selfPath, selfHash, err := nested.ExecutableIdentity(self)
	if err != nil {
		return nil, fmt.Errorf("identify OmniLSP exporter executable: %w", err)
	}
	tools := []model.ToolIdentity{
		{Name: semanticIndexNodeName, Path: nodePath, Version: nodeVersion, SHA256: nodeHash},
		{Name: semanticIndexCompilerName, Path: compilerPath, Version: compilerVersion, SHA256: compilerHash},
		{Name: semanticIndexDirectExporterName, Path: selfPath, Version: semanticIndexExtractorVersion, SHA256: selfHash},
	}
	return NewSemanticIndexProvider(SemanticIndexConfig{
		Runner: NewDirectCompilerRunner(nodePath), Tools: tools,
		Toolchain: "node/" + nodeVersion + "+typescript/" + compilerVersion,
		BackendState: func(language string) (identity.BackendID, identity.BackendEpoch) {
			return identity.BackendID{Language: language, Name: serverName}, backend.currentBackendEpoch()
		},
	}), nil
}

func configuredToolPath(envName, fallbackCommand string) (string, error) {
	if configured := strings.TrimSpace(os.Getenv(envName)); configured != "" {
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("%s must be an absolute path", envName)
		}
		return configured, nil
	}
	return exec.LookPath(fallbackCommand)
}

func probeToolVersion(parent context.Context, executable string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, toolProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, executable, args...).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%s %s: %w (%s)", executable, strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	version := normalizeRuntimeVersion(string(output))
	if version == "" {
		return "", errors.New("tool returned an empty version")
	}
	return version, nil
}

func probeTypeScriptVersion(parent context.Context, nodePath, compilerPath string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, toolProbeTimeout)
	defer cancel()
	probe := fmt.Sprintf("const ts=require(%q); process.stdout.write(ts.version)", compilerPath)
	output, err := exec.CommandContext(ctx, nodePath, "-e", probe).CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("load %s: %w (%s)", compilerPath, err, strings.TrimSpace(string(output)))
	}
	version := strings.TrimSpace(string(output))
	if version == "" {
		return "", errors.New("TypeScript compiler returned an empty version")
	}
	return version, nil
}
