package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/index/interop"
	"github.com/omnilsp/omni/internal/runtime/server"
	"github.com/omnilsp/omni/internal/trust"
)

const maxSemanticSCIPFileBytes = 64 << 20

func cmdIndex(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: omnilsp index <import|export> --format scip --scope SCOPE_ID [flags]")
	}
	operation := args[0]
	if operation != "import" && operation != "export" {
		return fmt.Errorf("index: unknown operation %q (want import or export)", operation)
	}
	flags := flag.NewFlagSet("index "+operation, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "path to omnilsp.json")
	workspace := flags.String("workspace", "", "workspace root")
	format := flags.String("format", "scip", "index format (scip)")
	scopeID := flags.String("scope", "", "exact semantic scope ID from indexStats.semanticCoverage")
	language := flags.String("language", "", "semantic provider language, required for import")
	input := flags.String("input", "", "SCIP input file, required for import")
	output := flags.String("output", "", "SCIP output file, required for export; existing files are never overwritten")
	allowLossy := flags.Bool("allow-lossy", false, "export only: explicitly omit unsupported SCIP relation kinds and record counted losses in the artifact")
	if err := flags.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("index: unexpected arguments: %v", flags.Args())
	}
	if operation != "export" && *allowLossy {
		return errors.New("index: --allow-lossy is only valid for export")
	}
	if *format != "scip" {
		return fmt.Errorf("index: unsupported format %q (only scip is available)", *format)
	}
	if *scopeID == "" {
		return errors.New("index: --scope is required")
	}
	if operation == "import" {
		if *language == "" || *input == "" || *output != "" {
			return errors.New("index import requires --language, --input, and no --output")
		}
	} else if *output == "" || *input != "" || *language != "" {
		return errors.New("index export requires --output and does not accept --input or --language")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if *workspace != "" {
		cfg.WorkspaceDir = *workspace
	}
	if cfg.WorkspaceDir == "" {
		cfg.WorkspaceDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve workspace: %w", err)
		}
	}
	workspacePath, err := filepath.Abs(cfg.WorkspaceDir)
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	cfg.WorkspaceDir = workspacePath
	if err := cfg.Validate(); err != nil {
		return err
	}
	var artifactPath string
	if operation == "import" {
		artifactPath, err = validateArtifactPathOutsideWorkspace(workspacePath, *input)
	} else {
		artifactPath, err = validateArtifactPathOutsideWorkspace(workspacePath, *output)
	}
	if err != nil {
		return err
	}
	policy, err := trust.PolicyForBackend(workspacePath, "semantic-index")
	if err != nil {
		return err
	}
	for _, capability := range []trust.Capability{
		trust.CapProcessExecute,
		trust.CapCompilerExecute,
		trust.CapBuildScriptExecute,
		trust.CapProcMacroExecute,
	} {
		if err := policy.Gate(capability); err != nil {
			return fmt.Errorf("index %s blocked by workspace trust: %w", operation, err)
		}
	}

	srvCfg := server.DefaultConfig()
	srvCfg.Scheduler.MaxConcurrent = cfg.MaxConcurrentRequests
	srvCfg.Scheduler.MaxQueueSize = cfg.MaxQueueSize
	srvCfg.IndexDir = cfg.IndexDir
	srvCfg.IndexDiskBudgetBytes = cfg.IndexDiskBudgetBytes
	srv := server.New(srvCfg)
	registerBackends(srv, cfg)
	srv.InitializeWorkspace(workspacePath)

	ctx := context.Background()
	if operation == "import" {
		data, err := readBoundedSCIP(artifactPath)
		if err != nil {
			return err
		}
		result, err := srv.ImportSemanticSCIP(ctx, *language, *scopeID, data)
		if err != nil {
			return err
		}
		fmt.Printf("SCIP imported: generation=%d scope=%s coverage=%d (SCIP facts are not declared complete)\n", result.Generation, result.ScopeID, len(result.Coverage))
		return nil
	}

	var data []byte
	var losses []interop.SemanticSCIPExportLoss
	if *allowLossy {
		data, losses, err = srv.ExportSemanticSCIPWithLosses(ctx, *scopeID)
	} else {
		data, err = srv.ExportSemanticSCIP(ctx, *scopeID)
	}
	if err != nil {
		return err
	}
	if err := writeNewFile(artifactPath, data); err != nil {
		return fmt.Errorf("write SCIP export: %w", err)
	}
	if *allowLossy {
		return json.NewEncoder(os.Stdout).Encode(struct {
			Operation string                           `json:"operation"`
			Scope     string                           `json:"scope"`
			Bytes     int                              `json:"bytes"`
			Losses    []interop.SemanticSCIPExportLoss `json:"losses"`
		}{Operation: "export", Scope: *scopeID, Bytes: len(data), Losses: losses})
	}
	fmt.Printf("SCIP exported: scope=%s bytes=%d\n", *scopeID, len(data))
	return nil
}

func validateArtifactPathOutsideWorkspace(workspace, path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve SCIP artifact path: %w", err)
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace for SCIP artifact: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace for SCIP artifact: %w", err)
	}
	target := absolute
	if _, statErr := os.Lstat(absolute); statErr == nil {
		target, err = filepath.EvalSymlinks(absolute)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("inspect SCIP artifact path: %w", statErr)
	} else {
		parent, resolveErr := filepath.EvalSymlinks(filepath.Dir(absolute))
		if resolveErr != nil {
			return "", fmt.Errorf("resolve SCIP artifact directory: %w", resolveErr)
		}
		target = filepath.Join(parent, filepath.Base(absolute))
	}
	if err != nil {
		return "", fmt.Errorf("resolve SCIP artifact path: %w", err)
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return "", fmt.Errorf("compare SCIP artifact with workspace: %w", err)
	}
	if relative == "." || (!filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return "", errors.New("SCIP input and output files must be outside the workspace to preserve its source identity")
	}
	return absolute, nil
}

func readBoundedSCIP(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open SCIP input: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat SCIP input: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxSemanticSCIPFileBytes {
		return nil, fmt.Errorf("SCIP input must be a regular file between 1 and %d bytes", maxSemanticSCIPFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSemanticSCIPFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read SCIP input: %w", err)
	}
	if len(data) == 0 || len(data) > maxSemanticSCIPFileBytes {
		return nil, fmt.Errorf("SCIP input exceeds the %d byte limit", maxSemanticSCIPFileBytes)
	}
	return data, nil
}

func writeNewFile(path string, data []byte) error {
	if len(data) == 0 || len(data) > maxSemanticSCIPFileBytes {
		return fmt.Errorf("SCIP output must be between 1 and %d bytes", maxSemanticSCIPFileBytes)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(absolute), ".omnilsp-scip-export-*")
	if err != nil {
		return fmt.Errorf("stage SCIP export beside destination: %w", err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	n, err := file.Write(data)
	if err != nil {
		_ = file.Close()
		return err
	}
	if n != len(data) {
		_ = file.Close()
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Link(temporary, absolute); err != nil {
		return fmt.Errorf("publish %s without overwrite: %w", absolute, err)
	}
	return nil
}
