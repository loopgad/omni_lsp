package pyright

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
	semanticPyrightInternalEnv = "OMNILSP_SEMANTIC_PYRIGHT_INTERNAL_PATH"
	semanticPyrightVendorEnv   = "OMNILSP_SEMANTIC_PYRIGHT_VENDOR_PATH"
	semanticPyrightNodeEnv     = "OMNILSP_SEMANTIC_NODE_PATH"
	semanticPyrightPythonEnv   = "OMNILSP_SEMANTIC_PYTHON_PATH"
)

// NewRuntimeSemanticIndexProvider binds the live Python backend to the exact
// audited Pyright analyzer bundle. The two bundle paths are explicit because
// a PATH shim does not identify the modules that Node will actually load.
func NewRuntimeSemanticIndexProvider(ctx context.Context, backend *Backend) (*SemanticIndexProvider, error) {
	if backend == nil {
		return nil, errors.New("pyright semantic index: nil live backend")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	internalPath, err := requiredSemanticPath(semanticPyrightInternalEnv)
	if err != nil {
		return nil, err
	}
	vendorPath, err := requiredSemanticPath(semanticPyrightVendorEnv)
	if err != nil {
		return nil, err
	}
	nodePath, err := runtimeSemanticPath(semanticPyrightNodeEnv, "node")
	if err != nil {
		return nil, err
	}
	pythonPath, err := runtimeSemanticPath(semanticPyrightPythonEnv, "python")
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	paths := []struct{ name, version, path string }{
		{semanticIndexRuntimeName, semanticIndexRuntimeVersion, nodePath},
		{semanticIndexCompilerName, semanticIndexCompilerVersion, internalPath},
		{semanticIndexVendorName, semanticIndexCompilerVersion, vendorPath},
		{semanticIndexExporterName, semanticIndexExtractorVersion, self},
		{"python", "", pythonPath},
	}
	tools := make([]model.ToolIdentity, 0, len(paths))
	for _, entry := range paths {
		path, hash, identityErr := nested.ExecutableIdentity(entry.path)
		if identityErr != nil {
			return nil, fmt.Errorf("identify %s: %w", entry.name, identityErr)
		}
		tools = append(tools, model.ToolIdentity{Name: entry.name, Path: path, Version: entry.version, SHA256: hash})
	}
	if !strings.EqualFold(tools[1].SHA256, pyrightInternalSHA256) || !strings.EqualFold(tools[2].SHA256, pyrightVendorSHA256) {
		return nil, errors.New("Pyright analyzer bundles do not match the audited 1.1.414 hashes")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	nodeVersion, err := exec.CommandContext(probeCtx, tools[0].Path, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("probe pinned Node runtime: %w", err)
	}
	if got := strings.TrimPrefix(strings.TrimSpace(string(nodeVersion)), "v"); got != semanticIndexRuntimeVersion {
		return nil, fmt.Errorf("pinned Node runtime is %q; require %s", got, semanticIndexRuntimeVersion)
	}
	pythonVersion, err := exec.CommandContext(probeCtx, tools[4].Path, "--version").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("probe Python interpreter: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(pythonVersion)))
	if len(fields) != 2 || fields[0] != "Python" {
		return nil, fmt.Errorf("unexpected Python version %q", strings.TrimSpace(string(pythonVersion)))
	}
	tools[4].Version = fields[1]
	provider := NewPinnedSemanticIndexProvider(tools)
	provider.config.Backend = identity.BackendID{Language: langID, Name: serverName}
	provider.config.BackendEpoch = backend.currentBackendEpoch()
	return provider, nil
}

func requiredSemanticPath(env string) (string, error) {
	path := strings.TrimSpace(os.Getenv(env))
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must name an absolute pinned tool path", env)
	}
	return path, nil
}

func runtimeSemanticPath(env, command string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		if !filepath.IsAbs(value) {
			return "", fmt.Errorf("%s must be absolute", env)
		}
		return value, nil
	}
	return exec.LookPath(command)
}
