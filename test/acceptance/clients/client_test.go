//go:build clients

package clients

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/test/acceptance/report"
)

const (
	clientGate             = "OMNILSP_RUN_CLIENT_ACCEPTANCE"
	clientOnlyEnv          = "OMNILSP_CLIENT_ONLY"
	clientTimeout          = 10 * time.Minute
	clientStartupTimeout   = 90 * time.Second
	clientToolProbeTimeout = 30 * time.Second
	maxClientOutputBytes   = 64 << 10
)

type toolLock struct {
	ResolvedBinaries map[string]resolvedBinary `json:"resolvedBinaries"`
	Observed         struct {
		Go            string `json:"go"`
		Gopls         string `json:"gopls"`
		VSCode        string `json:"vscode"`
		Clangd        string `json:"clangd"`
		Clang         string `json:"clang"`
		ClangPlusPlus string `json:"clang++"`
		RustAnalyzer  string `json:"rustAnalyzer"`
		Rustc         string `json:"rustc"`
		Cargo         string `json:"cargo"`
		Python        string `json:"python"`
		Node          string `json:"node"`
		NPM           string `json:"npm"`
	} `json:"observed"`
	AcceptanceTools struct {
		Pyright struct {
			Version string `json:"version"`
		} `json:"pyright"`
		TypeScriptLanguageServer struct {
			Version string `json:"version"`
		} `json:"typescriptLanguageServer"`
		TypeScript struct {
			Version string `json:"version"`
		} `json:"typescript"`
		VSCodeLanguageClient struct {
			Version string `json:"version"`
		} `json:"vscodeLanguageClient"`
		Neovim struct {
			Version string `json:"version"`
		} `json:"neovim"`
		Emacs struct {
			Version string `json:"version"`
		} `json:"emacs"`
	} `json:"acceptanceTools"`
}

type resolvedBinary struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type editorCase struct {
	Name            string   `json:"name"`
	File            string   `json:"file"`
	LanguageID      string   `json:"languageId"`
	Family          string   `json:"family"`
	Symbol          string   `json:"symbol"`
	BackendLanguage string   `json:"backendLanguage"`
	RequiredTools   []string `json:"requiredTools"`
}

type editorCaseManifestEntry struct {
	Name       string
	LanguageID string
	Family     string
}

type clientMatrixClient struct {
	ID          string
	DisplayName string
	Native      bool
}

type clientMatrixLanguage struct {
	ID        string
	CaseNames []string
}

var clientMatrixClients = []clientMatrixClient{
	{ID: "vscode", DisplayName: "VS Code", Native: true},
	{ID: "neovim", DisplayName: "Neovim", Native: true},
	{ID: "emacs-eglot", DisplayName: "Emacs/Eglot", Native: true},
	{ID: "helix", DisplayName: "Helix"},
	{ID: "zed", DisplayName: "Zed"},
	{ID: "sublime-lsp", DisplayName: "Sublime LSP"},
}

var clientMatrixLanguages = []clientMatrixLanguage{
	{ID: "go", CaseNames: []string{"go"}},
	{ID: "c", CaseNames: []string{"c"}},
	{ID: "cpp", CaseNames: []string{"cpp"}},
	{ID: "rust", CaseNames: []string{"rust"}},
	{ID: "python", CaseNames: []string{"python"}},
	{ID: "typescript", CaseNames: []string{"typescript", "typescriptreact"}},
	{ID: "javascript", CaseNames: []string{"javascript", "javascriptreact"}},
}

var expectedEditorCaseManifest = []editorCaseManifestEntry{
	{Name: "go", LanguageID: "go", Family: "go"},
	{Name: "c", LanguageID: "c", Family: "cpp"},
	{Name: "cpp", LanguageID: "cpp", Family: "cpp"},
	{Name: "rust", LanguageID: "rust", Family: "rust"},
	{Name: "python", LanguageID: "python", Family: "python"},
	{Name: "typescript", LanguageID: "typescript", Family: "typescript"},
	{Name: "typescriptreact", LanguageID: "typescriptreact", Family: "typescript"},
	{Name: "javascript", LanguageID: "javascript", Family: "typescript"},
	{Name: "javascriptreact", LanguageID: "javascriptreact", Family: "typescript"},
}

type editorResult struct {
	RunID                string             `json:"run_id,omitempty"`
	CandidateSHA256      string             `json:"candidate_sha256,omitempty"`
	Client               string             `json:"client"`
	Status               string             `json:"status"`
	Stage                string             `json:"stage,omitempty"`
	CleanExit            bool               `json:"cleanExit"`
	ClientTestsCompleted bool               `json:"clientTestsCompleted"`
	ClientExitCode       *int               `json:"clientExitCode,omitempty"`
	ServerExitConfirmed  bool               `json:"serverExitConfirmed"`
	ServerExitEvidence   string             `json:"serverExitEvidence,omitempty"`
	Error                string             `json:"error,omitempty"`
	Cases                []editorCaseResult `json:"cases"`
}

type editorCaseResult struct {
	Name       string         `json:"name"`
	LanguageID string         `json:"languageId"`
	Family     string         `json:"family"`
	Status     string         `json:"status"`
	Stage      string         `json:"stage,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Error      string         `json:"error,omitempty"`
	Diagnostic map[string]any `json:"diagnostic,omitempty"`
	Observed   map[string]any `json:"observed,omitempty"`
}

func TestRealClientMatrix(t *testing.T) {
	runClientMatrix(t, false)
}

// TestFormalEmacsClientMatrix exercises Emacs through the same frozen-candidate,
// corpus, tool-lock, and report pipeline as the public client matrix.
func TestFormalEmacsClientMatrix(t *testing.T) {
	runClientMatrix(t, true)
}

func runClientMatrix(t *testing.T, formalEmacs bool) {
	started := time.Now().UTC()
	runID := strings.TrimSpace(os.Getenv("OMNILSP_RUN_ID"))
	if runID == "" {
		runID = fmt.Sprintf("clients-%s-%d", started.Format("20060102T150405Z"), os.Getpid())
	}
	r := report.New(runID)
	r.Environment["goos"] = runtime.GOOS
	r.Environment["goarch"] = runtime.GOARCH
	r.Environment["started_at"] = started.Format(time.RFC3339Nano)
	clientOnly, selectionErr := parseClientOnly(os.Getenv(clientOnlyEnv))
	if selectionErr != nil {
		block(&r, "client-selection", selectionErr.Error())
		t.Fatalf("invalid %s: %v", clientOnlyEnv, selectionErr)
	}
	var formalSelectionErr error
	if formalEmacs {
		if clientOnly != "" && clientOnly != "emacs-eglot" {
			formalSelectionErr = fmt.Errorf("%s=%q conflicts with the formal Emacs-only runner", clientOnlyEnv, clientOnly)
		}
		clientOnly = "emacs-eglot"
	}
	if clientOnly == "" {
		r.Environment["client_selection"] = "all"
	} else {
		r.Environment["client_selection"] = clientOnly
	}
	caseOnly := strings.TrimSpace(os.Getenv("OMNILSP_CLIENT_CASE_ONLY"))
	if formalEmacs && caseOnly != "" {
		formalSelectionErr = fmt.Errorf("OMNILSP_CLIENT_CASE_ONLY=%q is not allowed by the full formal Emacs run", caseOnly)
	}
	if caseOnly == "" {
		r.Environment["client_case_selection"] = "all"
	} else {
		r.Environment["client_case_selection"] = caseOnly
	}
	r.Corpus.Name = "seven-language-editor-fixtures-v1"
	root := repoRoot(t)
	reportPath := os.Getenv("OMNILSP_ACCEPTANCE_REPORT")
	if reportPath == "" {
		reportPath = filepath.Join(root, "test", "acceptance", "evidence", runID, "clients.json")
	}

	completed := false
	defer func() {
		r.Finalize(time.Now())
		if err := report.Write(reportPath, r); err != nil {
			t.Errorf("write client acceptance evidence %q: %v", reportPath, err)
		}
		if !completed {
			t.Logf("client acceptance evidence: %s (decision: %s)", reportPath, r.Decision)
		}
	}()
	if formalSelectionErr != nil {
		block(&r, "client-selection", formalSelectionErr.Error())
		t.Fatal(formalSelectionErr)
	}

	if os.Getenv(clientGate) != "1" {
		addCheck(&r, "runner/gate", report.NotVerified, "set OMNILSP_RUN_CLIENT_ACCEPTANCE=1 to run real editor clients")
		r.Skips = append(r.Skips, report.Skip{ID: "runner/gate", Reason: "the explicit real-client gate was not enabled"})
		t.Fatalf("real-client acceptance is not verified; set %s=1", clientGate)
	}
	validateMatrix := validateClientMatrixContract
	if formalEmacs {
		validateMatrix = validateFormalEmacsMatrixContract
	}

	lock, err := readToolLock(root)
	if err != nil {
		addCheck(&r, "tool-lock", report.Failed, err.Error())
		r.Errors = append(r.Errors, err.Error())
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		block(&r, "host", fmt.Sprintf("tool lock is pinned for windows/amd64; got %s/%s", runtime.GOOS, runtime.GOARCH))
		t.Fatalf("real-client matrix is blocked on this host: %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	toolChecks := verifyTools(root, lock)
	toolStatus := make(map[string]report.Status, len(toolChecks))
	for _, check := range toolChecks {
		r.Checks = append(r.Checks, check)
		toolStatus[strings.TrimPrefix(check.ID, "tool/")] = check.Status
		if check.Status == report.NotVerified {
			r.Skips = append(r.Skips, report.Skip{ID: check.ID, Reason: check.Summary})
		}
	}

	serverBin, err := buildServer(root, t.TempDir())
	if err != nil {
		block(&r, "candidate/binary", err.Error())
		t.Fatalf("frozen candidate is not verified: %v", err)
	}
	serverHash, err := fileSHA256(serverBin)
	if err != nil {
		addCheck(&r, "candidate/hash", report.Failed, err.Error())
		r.Errors = append(r.Errors, err.Error())
		t.Fatalf("hash server: %v", err)
	}
	r.Candidate.Binary = serverBin
	r.Candidate.SHA256 = serverHash
	if revision, err := gitRevision(root); err == nil {
		r.Candidate.Revision = revision
	}

	workspace := t.TempDir()
	cases, corpusHash, err := writeWorkspace(workspace, serverBin)
	if err != nil {
		addCheck(&r, "fixtures/write", report.Failed, err.Error())
		r.Errors = append(r.Errors, err.Error())
		t.Fatalf("write editor fixtures: %v", err)
	}
	if err := validateEditorCaseManifest(cases); err != nil {
		addCheck(&r, "fixtures/manifest", report.Failed, err.Error())
		r.Errors = append(r.Errors, err.Error())
		t.Fatal(err)
	}
	addCheck(&r, "fixtures/manifest", report.Passed, "all nine advertised editor cases match their fixed name/language/family manifest")
	r.Corpus.SHA256 = corpusHash
	clientEnv := makeClientEnv(root, workspace, serverBin, toolStatus, lock)
	semanticBinding := verifySemanticRuntimeBinding(lock, clientEnv)
	r.Checks = append(r.Checks, semanticBinding)
	resolvedExecution, _ := json.Marshal(map[string]any{
		"node_path":                      lock.ResolvedBinaries["node"].Path,
		"typescript_path":                lock.ResolvedBinaries["typescript"].Path,
		"semantic_node_path":             clientEnv["OMNILSP_SEMANTIC_NODE_PATH"],
		"semantic_typescript_path":       clientEnv["OMNILSP_SEMANTIC_TYPESCRIPT_PATH"],
		"semantic_pyright_internal_path": clientEnv["OMNILSP_SEMANTIC_PYRIGHT_INTERNAL_PATH"],
		"semantic_pyright_vendor_path":   clientEnv["OMNILSP_SEMANTIC_PYRIGHT_VENDOR_PATH"],
		"semantic_python_path":           clientEnv["OMNILSP_SEMANTIC_PYTHON_PATH"],
		"acceptance_node_path":           clientEnv["OMNILSP_ACCEPTANCE_NODE"],
		"acceptance_node_sha256":         clientEnv["OMNILSP_ACCEPTANCE_NODE_SHA256"],
	})
	r.Environment["resolved_execution"] = string(resolvedExecution)
	if semanticBinding.Status != report.Passed {
		reason := semanticBinding.Summary
		for _, clientSpec := range clientMatrixClients {
			appendClientChecks(&r, clientSpec.ID, cases, nil, report.NotVerified, reason)
		}
		r.Skips = append(r.Skips, report.Skip{ID: semanticBinding.ID, Reason: reason})
		if err := validateMatrix(r.Checks); err != nil {
			addCheck(&r, "client/matrix-contract", report.Failed, err.Error())
			r.Errors = append(r.Errors, err.Error())
			t.Fatal(err)
		}
		t.Fatalf("semantic runtime path binding is %s: %s", semanticBinding.Status, reason)
	}

	for _, clientSpec := range clientMatrixClients {
		client := clientSpec.ID
		if clientOnly != "" && client != clientOnly {
			reason := fmt.Sprintf("not executed because %s=%s", clientOnlyEnv, clientOnly)
			appendClientChecks(&r, client, cases, nil, report.NotVerified, reason)
			r.Skips = append(r.Skips, report.Skip{ID: "client/" + client, Reason: reason})
			continue
		}
		if !clientSpec.Native && !(formalEmacs && client == "emacs-eglot") {
			reason := fmt.Sprintf("%s client driver is not implemented by this acceptance runner", clientSpec.DisplayName)
			appendClientChecks(&r, client, cases, nil, report.NotVerified, reason)
			r.Skips = append(r.Skips, report.Skip{ID: "client/" + client, Reason: reason})
			continue
		}
		clientTool := clientToolName(client)
		if clientTool == "" {
			detail := fmt.Sprintf("no native runner is registered for %s", client)
			appendClientChecks(&r, client, cases, nil, report.Failed, detail)
			r.Errors = append(r.Errors, detail)
			continue
		}
		clientReady := toolStatus[clientTool] == report.Passed
		if client == "vscode" {
			clientReady = clientReady && toolStatus["vscode-languageclient"] == report.Passed
		}
		if !clientReady {
			reason := client + " client or project-local client library is missing or not at its locked version"
			appendClientChecks(&r, client, cases, nil, report.NotVerified, reason)
			r.Skips = append(r.Skips, report.Skip{ID: "client/" + client, Reason: reason})
			continue
		}

		resultPath := filepath.Join(filepath.Dir(reportPath), "client-"+client+"-result.json")
		if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
			detail := fmt.Sprintf("create %s client evidence directory: %v", client, err)
			appendClientChecks(&r, client, cases, nil, report.Failed, detail)
			r.Errors = append(r.Errors, detail)
			continue
		}
		if err := os.Remove(resultPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			detail := fmt.Sprintf("remove stale %s client result: %v", client, err)
			appendClientChecks(&r, client, cases, nil, report.Failed, detail)
			r.Errors = append(r.Errors, detail)
			continue
		}
		var runErr error
		switch client {
		case "vscode":
			runErr = runVSCode(root, workspace, resultPath, clientEnv, lock)
		case "neovim":
			runErr = runNeovim(root, workspace, resultPath, clientEnv, lock)
		case "emacs-eglot":
			runErr = runEmacsEglot(root, workspace, resultPath, clientEnv, lock)
		default:
			runErr = fmt.Errorf("no native runner is registered for %s", client)
		}
		results, resultErr := readEditorResult(resultPath)
		if runErr != nil {
			detail := recordNativeProcessFailure(&r, client, runErr.Error())
			if resultErr == nil && results.Client == client {
				appendClientHostFailure(&r, client, cases, results.Cases, detail)
			} else {
				appendClientChecks(&r, client, cases, nil, report.Failed, detail)
				r.Errors = append(r.Errors, detail)
			}
			continue
		}
		if resultErr != nil {
			detail := resultErr.Error()
			appendClientChecks(&r, client, cases, nil, report.Failed, detail)
			r.Errors = append(r.Errors, detail)
			continue
		}
		if results.Client != client || !results.CleanExit || results.ClientExitCode == nil || *results.ClientExitCode != 0 || !results.ServerExitConfirmed {
			detail := results.Error
			if detail == "" {
				detail = fmt.Sprintf("%s did not prove a clean client/LSP exit (status=%q exitCode=%v serverExit=%t evidence=%q)", client, results.Status, results.ClientExitCode, results.ServerExitConfirmed, results.ServerExitEvidence)
			}
			appendClientChecks(&r, client, cases, results.Cases, report.Failed, detail)
			r.Errors = append(r.Errors, detail)
			continue
		}
		appendClientChecks(&r, client, cases, results.Cases, "", "")
	}
	if err := validateMatrix(r.Checks); err != nil {
		addCheck(&r, "client/matrix-contract", report.Failed, err.Error())
		r.Errors = append(r.Errors, err.Error())
		t.Fatal(err)
	}

	r.Finalize(time.Now())
	if formalEmacs {
		if err := requireFormalEmacsOutcome(r); err != nil {
			t.Fatalf("%v; see %s", err, reportPath)
		}
		completed = true
		t.Logf("formal Emacs/Eglot acceptance evidence: %s", reportPath)
		return
	}
	if r.Decision != report.Passed {
		t.Fatalf("real-client acceptance decision is %s; see %s", r.Decision, reportPath)
	}
	completed = true
	t.Logf("real-client acceptance evidence: %s", reportPath)
}

func verifyTools(root string, lock toolLock) []report.Check {
	var checks []report.Check
	checkVersion := func(id, lockName string, args []string, want string) string {
		path, err := verifyLockedBinary(lock, lockName)
		if err != nil {
			checks = append(checks, report.Check{ID: "tool/" + id, Status: report.NotVerified, Summary: err.Error()})
			return ""
		}
		got, err := runLockedToolVersionProbe(lock, lockName, args...)
		if err != nil {
			checks = append(checks, report.Check{ID: "tool/" + id, Status: report.NotVerified, Summary: fmt.Sprintf("%s probe failed: %v", lockName, err)})
			return path
		}
		matches := exactToolVersion(id, got, want)
		status := report.Passed
		if !matches {
			status = report.NotVerified
		}
		checks = append(checks, report.Check{
			ID: "tool/" + id, Status: status,
			Summary:   fmt.Sprintf("%s at locked path %s reports %s; lock requires %s", lockName, path, got, want),
			Observed:  map[string]any{"path": path, "version": got},
			Threshold: map[string]any{"version": want},
		})
		return path
	}

	checkVersion("go", "go", []string{"version"}, lock.Observed.Go)
	checkVersion("gopls", "gopls", []string{"version"}, lock.Observed.Gopls)
	checkVersion("node", "node", []string{"--version"}, lock.Observed.Node)
	checkVersion("emacs", "emacs", []string{"--version"}, lock.AcceptanceTools.Emacs.Version)
	codePath, codeErr := verifyLockedBinary(lock, "vscode")
	if codeErr != nil {
		checks = append(checks, report.Check{ID: "tool/vscode", Status: report.NotVerified, Summary: codeErr.Error()})
	} else {
		got, err := readVSCodePackageVersion(codePath)
		status := report.Passed
		if err != nil || !exactToolVersion("vscode", got, lock.Observed.VSCode) {
			status = report.NotVerified
		}
		summary := fmt.Sprintf("VS Code version is %s; lock requires %s", got, lock.Observed.VSCode)
		if err != nil {
			summary = fmt.Sprintf("VS Code installation version could not be read: %v", err)
		}
		checks = append(checks, report.Check{ID: "tool/vscode", Status: status,
			Summary:  summary,
			Observed: map[string]any{"path": codePath, "version": got}, Threshold: map[string]any{"version": lock.Observed.VSCode}})
	}
	checkVersion("clangd", "clangd", []string{"--version"}, lock.Observed.Clangd)
	checkVersion("clang", "clang", []string{"--version"}, lock.Observed.Clang)
	checkVersion("clang++", "clang++", []string{"--version"}, lock.Observed.ClangPlusPlus)
	checkVersion("rust-analyzer", "rust-analyzer", []string{"--version"}, lock.Observed.RustAnalyzer)
	checkVersion("rustc", "rustc", []string{"--version"}, lock.Observed.Rustc)
	checkVersion("cargo", "cargo", []string{"--version"}, lock.Observed.Cargo)
	checkVersion("python", "python", []string{"--version"}, lock.Observed.Python)
	checkVersion("npm", "npm", []string{"--version"}, lock.Observed.NPM)
	checkLocalPackage := func(id, packagePath, expected string) {
		data, err := os.ReadFile(packagePath)
		if err != nil {
			checks = append(checks, report.Check{ID: "tool/" + id, Status: report.NotVerified, Summary: "pinned local package is missing: " + packagePath})
			return
		}
		var p struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(data, &p); err != nil || p.Version != expected {
			checks = append(checks, report.Check{ID: "tool/" + id, Status: report.NotVerified,
				Summary: fmt.Sprintf("local package version is %q; lock requires %q", p.Version, expected)})
			return
		}
		checks = append(checks, report.Check{ID: "tool/" + id, Status: report.Passed,
			Summary:  fmt.Sprintf("local package %s is pinned at %s", id, expected),
			Observed: map[string]any{"package": packagePath, "version": p.Version}, Threshold: map[string]any{"version": expected}})
	}
	toolsRoot := filepath.Join(root, "test", "acceptance", "tools")
	checkLocalPackage("pyright", filepath.Join(toolsRoot, "node_modules", "pyright", "package.json"), lock.AcceptanceTools.Pyright.Version)
	checkLocalPackage("typescript-language-server-package", filepath.Join(toolsRoot, "node_modules", "typescript-language-server", "package.json"), lock.AcceptanceTools.TypeScriptLanguageServer.Version)
	checkLocalPackage("typescript", filepath.Join(toolsRoot, "node_modules", "typescript", "package.json"), lock.AcceptanceTools.TypeScript.Version)
	checkLocalPackage("vscode-languageclient", filepath.Join(root, "editors", "vscode", "node_modules", "vscode-languageclient", "package.json"), lock.AcceptanceTools.VSCodeLanguageClient.Version)
	for _, name := range []string{"pyright-langserver", "typescript-language-server", "tsc"} {
		sourcePath, sourceErr := verifyLockedBinary(lock, nameForResolvedTool(name))
		path := filepath.Join(toolsRoot, "bin", executableName(name))
		if sourceErr != nil {
			checks = append(checks, report.Check{ID: "tool/" + name, Status: report.NotVerified, Summary: sourceErr.Error()})
		} else if info, err := os.Stat(path); err != nil || info.IsDir() {
			checks = append(checks, report.Check{ID: "tool/" + name, Status: report.NotVerified, Summary: "project-local locked wrapper is missing: " + path})
		} else {
			checks = append(checks, report.Check{ID: "tool/" + name, Status: report.Passed, Summary: "project-local wrapper launches the SHA-256 locked Node entrypoint", Observed: map[string]any{"path": path, "sourcePath": sourcePath, "nodePath": lock.ResolvedBinaries["node"].Path}})
		}
	}

	nvimPath, nvimErr := verifyLockedBinary(lock, "neovim")
	configuredNvim := strings.TrimSpace(os.Getenv("OMNILSP_NVIM_BIN"))
	if nvimErr != nil {
		checks = append(checks, report.Check{ID: "tool/neovim", Status: report.NotVerified, Summary: nvimErr.Error()})
	} else if configuredNvim != "" && !sameExecutablePath(configuredNvim, nvimPath) {
		checks = append(checks, report.Check{ID: "tool/neovim", Status: report.Failed, Summary: fmt.Sprintf("OMNILSP_NVIM_BIN %q does not match locked Neovim path %q", configuredNvim, nvimPath)})
	} else {
		got, err := runToolVersionProbe(nvimPath, "--version")
		got = firstLine(got)
		status := report.Passed
		if err != nil || !exactToolVersion("neovim", got, lock.AcceptanceTools.Neovim.Version) {
			status = report.NotVerified
		}
		summary := fmt.Sprintf("Neovim version is %s; lock requires v%s", got, lock.AcceptanceTools.Neovim.Version)
		if err != nil {
			summary = fmt.Sprintf("Neovim version probe failed: %v", err)
		}
		checks = append(checks, report.Check{ID: "tool/neovim", Status: status,
			Summary:  summary,
			Observed: map[string]any{"path": nvimPath, "version": got}, Threshold: map[string]any{"version": "v" + lock.AcceptanceTools.Neovim.Version}})
	}
	return checks
}

func verifySemanticRuntimeBinding(lock toolLock, env map[string]string) report.Check {
	observed := make(map[string]any)
	for _, binding := range semanticRuntimeBindings() {
		path, err := verifyLockedBinary(lock, binding.tool)
		if err != nil {
			return report.Check{ID: "tool/semantic-runtime-binding", Status: report.NotVerified, Summary: err.Error()}
		}
		configured := strings.TrimSpace(env[binding.env])
		if configured == "" {
			return report.Check{ID: "tool/semantic-runtime-binding", Status: report.NotVerified, Summary: binding.env + " is missing from the client environment"}
		}
		if !sameExecutablePath(configured, path) {
			return report.Check{ID: "tool/semantic-runtime-binding", Status: report.Failed, Summary: fmt.Sprintf("%s %q does not match locked %s path %q", binding.env, configured, binding.tool, path)}
		}
		observed[binding.tool] = path
	}
	return report.Check{ID: "tool/semantic-runtime-binding", Status: report.Passed,
		Summary: "semantic runtimes and analyzer bundles match locked paths and SHA-256 identities", Observed: observed}
}

func semanticRuntimeBindings() []struct{ tool, env string } {
	return []struct{ tool, env string }{
		{"node", "OMNILSP_SEMANTIC_NODE_PATH"}, {"typescript", "OMNILSP_SEMANTIC_TYPESCRIPT_PATH"},
		{"python", "OMNILSP_SEMANTIC_PYTHON_PATH"}, {"pyright-internal", "OMNILSP_SEMANTIC_PYRIGHT_INTERNAL_PATH"},
		{"pyright-vendor", "OMNILSP_SEMANTIC_PYRIGHT_VENDOR_PATH"},
	}
}

func readToolLock(root string) (toolLock, error) {
	var lock toolLock
	path := filepath.Join(root, "test", "acceptance", "tools", "tools.lock.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return lock, fmt.Errorf("read pinned tool lock: %w", err)
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return lock, fmt.Errorf("parse pinned tool lock: %w", err)
	}
	if lock.Observed.Go == "" || lock.Observed.VSCode == "" || lock.AcceptanceTools.Neovim.Version == "" || lock.AcceptanceTools.Emacs.Version == "" || lock.AcceptanceTools.VSCodeLanguageClient.Version == "" {
		return lock, errors.New("pinned tool lock is missing required versions")
	}
	for _, name := range requiredResolvedBinaryNames() {
		entry, ok := lock.ResolvedBinaries[name]
		if !ok || strings.TrimSpace(entry.Path) == "" || strings.TrimSpace(entry.SHA256) == "" {
			return lock, fmt.Errorf("pinned tool lock is missing resolvedBinaries.%s path/hash", name)
		}
	}
	return lock, nil
}

func requiredResolvedBinaryNames() []string {
	return []string{
		"go", "gopls", "vscode", "clang", "clang++", "clangd",
		"rust-analyzer", "rustc", "cargo", "rust-analyzer-proc-macro-srv",
		"python", "node", "npm", "neovim", "emacs",
		"pyright-langserver", "pyright-internal", "pyright-vendor", "typescript-language-server", "typescript",
	}
}

func nameForResolvedTool(name string) string {
	if name == "tsc" {
		return "typescript"
	}
	return name
}

func verifyLockedBinary(lock toolLock, name string) (string, error) {
	entry, ok := lock.ResolvedBinaries[name]
	if !ok {
		return "", fmt.Errorf("resolvedBinaries.%s is missing", name)
	}
	if !filepath.IsAbs(entry.Path) {
		return "", fmt.Errorf("resolvedBinaries.%s path is not absolute: %q", name, entry.Path)
	}
	if !isSHA256(strings.TrimSpace(entry.SHA256)) {
		return "", fmt.Errorf("resolvedBinaries.%s has an invalid SHA-256 pin", name)
	}
	path, err := filepath.Abs(filepath.Clean(entry.Path))
	if err != nil {
		return "", fmt.Errorf("resolve resolvedBinaries.%s path: %w", name, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("resolvedBinaries.%s is missing at %q", name, path)
	}
	actual, err := fileSHA256(path)
	if err != nil {
		return "", fmt.Errorf("hash resolvedBinaries.%s at %q: %w", name, path, err)
	}
	if !strings.EqualFold(actual, strings.TrimSpace(entry.SHA256)) {
		return "", fmt.Errorf("resolvedBinaries.%s at %q has SHA-256 %s; lock requires %s", name, path, actual, strings.ToLower(strings.TrimSpace(entry.SHA256)))
	}
	return path, nil
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func runLockedToolVersionProbe(lock toolLock, name string, args ...string) (string, error) {
	path, err := verifyLockedBinary(lock, name)
	if err != nil {
		return "", err
	}
	if ext := strings.ToLower(filepath.Ext(path)); ext != ".js" && ext != ".mjs" {
		return runToolVersionProbe(path, args...)
	}
	nodePath, err := verifyLockedBinary(lock, "node")
	if err != nil {
		return "", fmt.Errorf("run locked %s through Node: %w", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), clientToolProbeTimeout)
	defer cancel()
	commandArgs := append([]string{path}, args...)
	cmd := exec.CommandContext(ctx, nodePath, commandArgs...)
	output, runErr := runClientCommand(ctx, cmd)
	if ctx.Err() != nil {
		return strings.TrimSpace(string(output)), fmt.Errorf("version probe exceeded %s: %w", clientToolProbeTimeout, ctx.Err())
	}
	if runErr != nil {
		return strings.TrimSpace(string(output)), runErr
	}
	return strings.TrimSpace(string(output)), nil
}

func sameExecutablePath(left, right string) bool {
	leftAbs, leftErr := filepath.Abs(filepath.Clean(left))
	rightAbs, rightErr := filepath.Abs(filepath.Clean(right))
	return leftErr == nil && rightErr == nil && strings.EqualFold(leftAbs, rightAbs)
}

func exactToolVersion(id, output, expected string) bool {
	if id == "go" {
		version, ok := goVersionFromOutput(output)
		return ok && version == expected
	}

	line := strings.TrimSpace(firstLine(strings.TrimSpace(output)))
	fields := strings.Fields(line)
	switch id {
	case "node", "vscode", "npm":
		return line == expected
	case "gopls":
		return strings.Contains(line, expected)
	case "clangd":
		return len(fields) >= 3 && fields[0] == "clangd" && fields[1] == "version" && fields[2] == expected
	case "clang", "clang++":
		return len(fields) >= 3 && fields[0] == "clang" && fields[1] == "version" && fields[2] == expected
	case "rust-analyzer":
		return len(fields) >= 2 && fields[0] == "rust-analyzer" && strings.Join(fields[1:], " ") == expected
	case "rustc":
		return len(fields) >= 2 && fields[0] == "rustc" && strings.Join(fields[1:], " ") == expected
	case "cargo":
		return len(fields) >= 2 && fields[0] == "cargo" && strings.Join(fields[1:], " ") == expected
	case "python":
		return len(fields) == 2 && fields[0] == "Python" && fields[1] == expected
	case "neovim":
		return len(fields) == 2 && fields[0] == "NVIM" && strings.TrimPrefix(fields[1], "v") == expected
	case "emacs":
		return len(fields) == 3 && fields[0] == "GNU" && fields[1] == "Emacs" && fields[2] == expected
	default:
		return false
	}
}

func goVersionFromOutput(output string) (string, bool) {
	var version string
	var versionLine string
	found := false
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "go" || fields[1] != "version" {
			continue
		}
		if len(fields) != 4 || !isGoReleaseVersion(fields[2]) || !isGoPlatform(fields[3]) {
			return "", false
		}
		if found && strings.Join(fields, " ") != versionLine {
			return "", false
		}
		version = fields[2]
		versionLine = strings.Join(fields, " ")
		found = true
	}
	return version, found
}

func isGoReleaseVersion(version string) bool {
	if !strings.HasPrefix(version, "go") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(version, "go"), ".")
	if len(parts) < 2 || len(parts) > 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, char := range part {
			if char < '0' || char > '9' {
				return false
			}
		}
	}
	return true
}

func isGoPlatform(platform string) bool {
	if strings.Count(platform, "/") != 1 {
		return false
	}
	parts := strings.SplitN(platform, "/", 2)
	return parts[0] != "" && parts[1] != ""
}

func TestExactToolVersionRejectsSuffixAndSubstringMatches(t *testing.T) {
	tests := []struct {
		id, output, expected string
		want                 bool
	}{
		{"go", "go version go1.26.1 windows/amd64", "go1.26.1", true},
		{"go", "error acquiring upload token: creating token file: open telemetry/local/upload.token: Access is denied.\r\ngo version go1.26.1 windows/amd64", "go1.26.1", true},
		{"go", "go version go1.26.10 windows/amd64", "go1.26.1", false},
		{"go", "go version go1.26.2 windows/amd64", "go1.26.1", false},
		{"go", "go version go1.26.1 windows/amd64\ngo version go1.26.2 windows/amd64", "go1.26.1", false},
		{"go", "go version go1.26.1 windows/amd64\ngo version go1.26.1 linux/amd64", "go1.26.1", false},
		{"go", "go version go1.26.1 windows/amd64\ngo version go1.26.1 windows/amd64", "go1.26.1", true},
		{"go", "go version malformed windows/amd64\ngo version go1.26.1 windows/amd64", "go1.26.1", false},
		{"node", "v24.14.0", "v24.14.0", true},
		{"node", "v24.14.0-extra", "v24.14.0", false},
		{"python", "Python 3.13.13", "3.13.13", true},
		{"python", "Python 3.13.130", "3.13.13", false},
		{"clangd", "clangd version 22.1.5", "22.1.5", true},
		{"clangd", "clangd version 22.1.50", "22.1.5", false},
		{"rust-analyzer", "rust-analyzer 1.97.0 (2d8144b7 2026-07-07)", "1.97.0 (2d8144b7 2026-07-07)", true},
		{"rust-analyzer", "rust-analyzer 1.97.0-extra (2d8144b7 2026-07-07)", "1.97.0 (2d8144b7 2026-07-07)", false},
		{"neovim", "NVIM v0.12.5", "0.12.5", true},
		{"neovim", "NVIM v0.12.50", "0.12.5", false},
		{"emacs", "GNU Emacs 30.2\nCopyright (C) Free Software Foundation", "30.2", true},
		{"emacs", "GNU Emacs 30.20", "30.2", false},
		{"emacs", "GNU Emacs 30.2-modified", "30.2", false},
	}
	for _, test := range tests {
		t.Run(test.id+"/"+test.output, func(t *testing.T) {
			if got := exactToolVersion(test.id, test.output, test.expected); got != test.want {
				t.Fatalf("exactToolVersion(%q, %q, %q) = %t, want %t", test.id, test.output, test.expected, got, test.want)
			}
		})
	}
}

func TestFileURIUsesCanonicalWindowsDrivePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows drive URIs are platform-specific")
	}
	path := filepath.Join(t.TempDir(), "editor workspace")
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	want := (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(abs)}).String()
	got := fileURI(path)
	if got != want {
		t.Fatalf("fileURI(%q) = %q, want %q", path, got, want)
	}
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("fileURI(%q) returned an invalid URI: %v", path, err)
	}
	if parsed.Host != "" {
		t.Fatalf("fileURI(%q) set host %q; Windows drive paths must be URI paths", path, parsed.Host)
	}
}

func TestReadClientOutputCapsDiagnostics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-output.log")
	payload := bytes.Repeat([]byte("x"), maxClientOutputBytes+1024)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readClientOutput(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxClientOutputBytes {
		t.Fatalf("bounded client output length = %d, want %d", len(got), maxClientOutputBytes)
	}
	if !bytes.HasSuffix(got, []byte("[client output truncated]\n")) {
		t.Fatalf("bounded client output does not include truncation marker: %q", got[len(got)-32:])
	}
}

func TestNormalizeVSCodeExecutableResolvesShim(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(binDir, "code.cmd")
	executable := filepath.Join(root, "Code.exe")
	for _, path := range []string{shim, executable} {
		if err := os.WriteFile(path, []byte("placeholder"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := normalizeVSCodeExecutable(shim)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(executable)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("normalizeVSCodeExecutable(%q) = %q, want %q", shim, got, want)
	}
	missingRoot := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(missingRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeVSCodeExecutable(filepath.Join(missingRoot, "missing.cmd")); err == nil {
		t.Fatal("normalizeVSCodeExecutable accepted a missing VS Code shim")
	}
}

func TestReadVSCodePackageVersionSupportsDirectAndVersionedLayouts(t *testing.T) {
	writePackage := func(t *testing.T, path, version string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"version":%q}`, version)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("direct resources directory", func(t *testing.T) {
		root := t.TempDir()
		writePackage(t, filepath.Join(root, "resources", "app", "package.json"), "1.139.0")
		got, err := readVSCodePackageVersion(filepath.Join(root, "Code.exe"))
		if err != nil || got != "1.139.0" {
			t.Fatalf("readVSCodePackageVersion() = %q, %v; want 1.139.0", got, err)
		}
	})

	t.Run("versioned application directory", func(t *testing.T) {
		root := t.TempDir()
		writePackage(t, filepath.Join(root, "commit", "resources", "app", "package.json"), "1.139.0")
		got, err := readVSCodePackageVersion(filepath.Join(root, "Code.exe"))
		if err != nil || got != "1.139.0" {
			t.Fatalf("readVSCodePackageVersion() = %q, %v; want 1.139.0", got, err)
		}
	})

	t.Run("ambiguous application directories fail closed", func(t *testing.T) {
		root := t.TempDir()
		writePackage(t, filepath.Join(root, "commit-a", "resources", "app", "package.json"), "1.139.0")
		writePackage(t, filepath.Join(root, "commit-b", "resources", "app", "package.json"), "1.140.0")
		if got, err := readVSCodePackageVersion(filepath.Join(root, "Code.exe")); err == nil {
			t.Fatalf("readVSCodePackageVersion() = %q, nil; wanted ambiguous-install error", got)
		}
	})
}

func TestNeovimShutdownRequiresProcessTreeToExit(t *testing.T) {
	acknowledged := editorResult{
		ServerExitConfirmed: true,
		ServerExitEvidence:  "Neovim client was removed after client:stop(false)",
	}
	t.Run("LSP acknowledgement alone is insufficient", func(t *testing.T) {
		for _, process := range []string{"omnilsp.exe:123", "gopls.exe:123"} {
			t.Run(process, func(t *testing.T) {
				confirmed, evidence := confirmNeovimShutdown(acknowledged, nil, fmt.Errorf("managed process image=node.exe pid=40 ppid=30 remained: %s", process))
				if confirmed {
					t.Fatal("Neovim shutdown passed while a managed descendant remained")
				}
				if !strings.Contains(evidence, process) {
					t.Fatalf("shutdown evidence omitted remaining process: %q", evidence)
				}
			})
		}
	})
	t.Run("missing LSP acknowledgement remains a failure", func(t *testing.T) {
		result := acknowledged
		result.ServerExitConfirmed = false
		confirmed, _ := confirmNeovimShutdown(result, nil, nil)
		if confirmed {
			t.Fatal("process cleanup passed without Neovim LSP stop acknowledgement")
		}
	})
	t.Run("both client acknowledgement and process cleanup pass", func(t *testing.T) {
		confirmed, evidence := confirmNeovimShutdown(acknowledged, nil, nil)
		if !confirmed {
			t.Fatalf("complete shutdown evidence rejected: %s", evidence)
		}
		if !strings.Contains(evidence, "every observed descendant exited") {
			t.Fatalf("shutdown evidence omitted descendant tracking: %q", evidence)
		}
	})
}

type fakeEditorProcessSnapshotSource struct {
	processes      map[int]editorProcessRecord
	created        map[int]uint64
	creationErrors map[int]error
	err            error
}

func (f *fakeEditorProcessSnapshotSource) snapshot() (map[int]editorProcessRecord, error) {
	if f.err != nil {
		return nil, f.err
	}
	copy := make(map[int]editorProcessRecord, len(f.processes))
	for pid, process := range f.processes {
		copy[pid] = process
	}
	return copy, nil
}

func (f *fakeEditorProcessSnapshotSource) creationTime(pid int) (uint64, error) {
	if err, ok := f.creationErrors[pid]; ok {
		return 0, err
	}
	created, ok := f.created[pid]
	if !ok {
		return 0, fmt.Errorf("no creation time for PID %d", pid)
	}
	return created, nil
}

func TestEditorProcessTrackerIgnoresUnrelatedNodeAndReportsManagedLeak(t *testing.T) {
	root := editorProcessIdentity{PID: 10, ParentPID: 1, Image: "Code.exe", CreationTime: 100}
	source := &fakeEditorProcessSnapshotSource{
		processes: map[int]editorProcessRecord{
			10: {PID: 10, ParentPID: 1, Image: "Code.exe"},
			20: {PID: 20, ParentPID: 10, Image: "omnilsp.exe"},
			30: {PID: 30, ParentPID: 20, Image: "acceptance-tool-wrapper.exe"},
			40: {PID: 40, ParentPID: 30, Image: "node.exe"},
			50: {PID: 50, ParentPID: 1, Image: "node.exe"}, // unrelated process
		},
		created: map[int]uint64{10: 100, 20: 110, 30: 120, 40: 130, 50: 125},
	}
	tracker := &editorProcessTracker{
		source: source,
		root:   root,
		known:  map[int]editorProcessIdentity{root.PID: root},
	}
	if err := tracker.scanOnce(); err != nil {
		t.Fatalf("track initial descendant tree: %v", err)
	}
	for _, pid := range []int{10, 20, 30, 40} {
		if _, ok := tracker.known[pid]; !ok {
			t.Errorf("managed PID %d was not tracked: %+v", pid, tracker.known)
		}
	}
	if _, ok := tracker.known[50]; ok {
		t.Fatal("unrelated node.exe was attributed to the client process tree")
	}

	// The editor, candidate, and wrapper have exited; the Node descendant is
	// still visible with its original PPID and must keep the shutdown gate closed.
	source.processes = map[int]editorProcessRecord{
		40: {PID: 40, ParentPID: 30, Image: "node.exe"},
		50: {PID: 50, ParentPID: 1, Image: "node.exe"},
	}
	if err := tracker.scanOnce(); err != nil {
		t.Fatalf("scan orphaned descendants: %v", err)
	}
	if len(tracker.live) != 1 || tracker.live[0].PID != 40 {
		t.Fatalf("live managed descendants = %+v, want only node PID 40", tracker.live)
	}
	detail := tracker.live[0].String()
	for _, want := range []string{"image=node.exe", "pid=40", "ppid=30"} {
		if !strings.Contains(detail, want) {
			t.Errorf("process detail %q omitted %q", detail, want)
		}
	}
}

func TestEditorProcessTrackerRejectsReusedRootPID(t *testing.T) {
	root := editorProcessIdentity{PID: 10, ParentPID: 1, Image: "Code.exe", CreationTime: 100}
	source := &fakeEditorProcessSnapshotSource{
		processes: map[int]editorProcessRecord{
			10: {PID: 10, ParentPID: 1, Image: "Code.exe"},
			20: {PID: 20, ParentPID: 10, Image: "unrelated.exe"},
		},
		created: map[int]uint64{10: 200, 20: 210}, // PID 10 now names a different root process.
	}
	tracker := &editorProcessTracker{
		source: source,
		root:   root,
		known:  map[int]editorProcessIdentity{root.PID: root},
	}
	if err := tracker.scanOnce(); err != nil {
		t.Fatalf("scan after root PID reuse: %v", err)
	}
	if _, ok := tracker.known[20]; ok {
		t.Fatal("process below a reused root PID was attributed to the original client")
	}
}

func TestEditorProcessTrackerIgnoresReusedPIDBeforeCreationTimeQuery(t *testing.T) {
	identity := editorProcessIdentity{PID: 21472, ParentPID: 37008, Image: "rustup.exe", CreationTime: 100}
	source := &fakeEditorProcessSnapshotSource{
		processes: map[int]editorProcessRecord{
			identity.PID: {PID: identity.PID, ParentPID: 1520, Image: "svchost.exe"},
		},
		creationErrors: map[int]error{identity.PID: errors.New("OpenProcess: access is denied")},
	}
	tracker := &editorProcessTracker{
		source: source,
		root:   identity,
		known:  map[int]editorProcessIdentity{identity.PID: identity},
	}

	if err := tracker.scanOnce(); err != nil {
		t.Fatalf("scan after PID reuse: %v", err)
	}
	if len(tracker.live) != 0 {
		t.Fatalf("live managed processes after PID reuse = %+v, want none", tracker.live)
	}
}

func TestEditorProcessTrackerIgnoresReusedParentBeforeCreationTimeQuery(t *testing.T) {
	root := editorProcessIdentity{PID: 10, ParentPID: 1, Image: "nvim.exe", CreationTime: 100}
	parent := editorProcessIdentity{PID: 20, ParentPID: root.PID, Image: "rustup.exe", CreationTime: 110}
	source := &fakeEditorProcessSnapshotSource{
		processes: map[int]editorProcessRecord{
			root.PID:   {PID: root.PID, ParentPID: root.ParentPID, Image: root.Image},
			parent.PID: {PID: parent.PID, ParentPID: 1520, Image: "svchost.exe"},
			30:         {PID: 30, ParentPID: parent.PID, Image: "nvim.exe"},
		},
		created:        map[int]uint64{root.PID: root.CreationTime},
		creationErrors: map[int]error{parent.PID: errors.New("OpenProcess: access is denied")},
	}
	tracker := &editorProcessTracker{
		source: source,
		root:   root,
		known:  map[int]editorProcessIdentity{root.PID: root, parent.PID: parent},
	}

	if err := tracker.scanOnce(); err != nil {
		t.Fatalf("scan after managed parent PID reuse: %v", err)
	}
	if len(tracker.live) != 1 || tracker.live[0].PID != root.PID {
		t.Fatalf("live managed processes after parent PID reuse = %+v, want only root %+v", tracker.live, root)
	}
	if _, ok := tracker.known[30]; ok {
		t.Fatal("descendant of a reused parent PID was attributed to the managed process tree")
	}
}

func TestEditorProcessTrackerFailsClosedWhenCreationTimeIsDeniedForMatchingIdentity(t *testing.T) {
	root := editorProcessIdentity{PID: 10, ParentPID: 1, Image: "nvim.exe", CreationTime: 100}
	source := &fakeEditorProcessSnapshotSource{
		processes: map[int]editorProcessRecord{
			root.PID: {PID: root.PID, ParentPID: root.ParentPID, Image: root.Image},
		},
		creationErrors: map[int]error{root.PID: errors.New("OpenProcess: access is denied")},
	}
	tracker := &editorProcessTracker{
		source: source,
		root:   root,
		known:  map[int]editorProcessIdentity{root.PID: root},
	}

	err := tracker.scanOnce()
	if err == nil || !strings.Contains(err.Error(), "access is denied") {
		t.Fatalf("scan with matching identity and denied creation-time query = %v, want fail-closed access-denied error", err)
	}
}

func TestValidateEditorCaseManifestRequiresAllAdvertisedPairs(t *testing.T) {
	cases := make([]editorCase, 0, len(expectedEditorCaseManifest))
	for _, expected := range expectedEditorCaseManifest {
		cases = append(cases, editorCase{Name: expected.Name, LanguageID: expected.LanguageID, Family: expected.Family})
	}
	if err := validateEditorCaseManifest(cases); err != nil {
		t.Fatalf("valid fixed manifest rejected: %v", err)
	}

	for _, missingName := range []string{"javascript", "javascriptreact"} {
		t.Run("missing "+missingName, func(t *testing.T) {
			missing := make([]editorCase, 0, len(cases)-1)
			for _, testCase := range cases {
				if testCase.Name != missingName {
					missing = append(missing, testCase)
				}
			}
			if err := validateEditorCaseManifest(missing); err == nil {
				t.Fatalf("manifest without %s passed", missingName)
			}
		})
	}
	t.Run("wrong JavaScript language ID", func(t *testing.T) {
		wrongLanguage := append([]editorCase(nil), cases...)
		wrongLanguage[7].LanguageID = "typescript"
		if err := validateEditorCaseManifest(wrongLanguage); err == nil {
			t.Fatal("javascript case with TypeScript language ID passed")
		}
	})
	t.Run("unexpected case replacing JavaScript JSX", func(t *testing.T) {
		unexpected := append([]editorCase(nil), cases...)
		unexpected[len(unexpected)-1] = editorCase{Name: "javascriptmodule", LanguageID: "javascript", Family: "typescript"}
		if err := validateEditorCaseManifest(unexpected); err == nil {
			t.Fatal("unexpected case replacing javascriptreact passed")
		}
	})
}

func TestParseClientOnly(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
		bad   bool
	}{
		{input: "", want: ""},
		{input: " all ", bad: true},
		{input: " VSCode ", want: "vscode"},
		{input: "VS Code", want: "vscode"},
		{input: "neovim", want: "neovim"},
		{input: "Emacs/Eglot", want: "emacs-eglot"},
		{input: "helix", want: "helix"},
		{input: "Zed", want: "zed"},
		{input: "Sublime LSP", want: "sublime-lsp"},
	} {
		got, err := parseClientOnly(tc.input)
		if (err != nil) != tc.bad {
			t.Fatalf("parseClientOnly(%q) error = %v; bad=%t", tc.input, err, tc.bad)
		}
		if got != tc.want {
			t.Fatalf("parseClientOnly(%q) = %q; want %q", tc.input, got, tc.want)
		}
	}
}

func TestClientToolNameUsesExplicitNativeMapping(t *testing.T) {
	for _, tc := range []struct{ client, want string }{
		{"vscode", "vscode"},
		{"neovim", "neovim"},
		{"emacs-eglot", "emacs"},
		{"helix", ""},
	} {
		if got := clientToolName(tc.client); got != tc.want {
			t.Errorf("clientToolName(%q) = %q; want %q", tc.client, got, tc.want)
		}
	}
}

func TestClientMatrixContractRequiresEveryCell(t *testing.T) {
	ids := clientMatrixCheckIDs()
	if len(ids) != 42 {
		t.Fatalf("client matrix has %d cells, want 42", len(ids))
	}
	checks := make([]report.Check, 0, len(ids))
	for _, id := range ids {
		checks = append(checks, report.Check{ID: id, Status: report.NotVerified})
	}
	if err := validateClientMatrixContract(checks); err != nil {
		t.Fatalf("complete not_verified matrix rejected: %v", err)
	}
	manifestCases := make([]editorCase, 0, len(expectedEditorCaseManifest))
	for _, expected := range expectedEditorCaseManifest {
		manifestCases = append(manifestCases, editorCase{Name: expected.Name, LanguageID: expected.LanguageID, Family: expected.Family})
	}
	assembled := report.New("client-matrix-assembled")
	for _, client := range clientMatrixClients {
		appendClientChecks(&assembled, client.ID, manifestCases, nil, report.NotVerified, "synthetic missing driver")
	}
	if len(assembled.Checks) != len(ids) {
		t.Fatalf("assembled client report has %d checks, want %d unique matrix cells", len(assembled.Checks), len(ids))
	}
	if err := validateClientMatrixContract(assembled.Checks); err != nil {
		t.Fatalf("assembled client matrix rejected: %v", err)
	}
	for _, client := range clientMatrixClients {
		if client.Native {
			continue
		}
		id := "client/" + client.ID + "/go"
		passed := append([]report.Check(nil), checks...)
		for index := range passed {
			if passed[index].ID == id {
				passed[index].Status = report.Passed
				break
			}
		}
		if err := validateClientMatrixContract(passed); err == nil {
			t.Fatalf("unsupported client cell %s was allowed to pass", id)
		}
	}

	for _, missingID := range ids {
		t.Run("missing "+missingID, func(t *testing.T) {
			missing := make([]report.Check, 0, len(checks)-1)
			for _, check := range checks {
				if check.ID != missingID {
					missing = append(missing, check)
				}
			}
			if err := validateClientMatrixContract(missing); err == nil {
				t.Fatalf("matrix without %s passed", missingID)
			}
		})
	}
}

func TestFormalEmacsContractAllowsOnlyEmacsNativeEvidence(t *testing.T) {
	checks := make([]report.Check, 0, len(clientMatrixCheckIDs()))
	for _, id := range clientMatrixCheckIDs() {
		status := report.NotVerified
		if strings.HasPrefix(id, "client/emacs-eglot/") {
			status = report.Passed
		}
		checks = append(checks, report.Check{ID: id, Status: status})
	}
	if err := validateFormalEmacsMatrixContract(checks); err != nil {
		t.Fatalf("formal Emacs matrix rejected: %v", err)
	}
	if err := validateClientMatrixContract(checks); err != nil {
		t.Fatalf("public matrix contract rejected reviewed Emacs native evidence: %v", err)
	}
	if err := requireClientMatrixPassed(checks, "emacs-eglot"); err != nil {
		t.Fatalf("all Emacs matrix cells should be required and passed: %v", err)
	}

	for index := range checks {
		if checks[index].ID == "client/helix/go" {
			checks[index].Status = report.Passed
			break
		}
	}
	if err := validateFormalEmacsMatrixContract(checks); err == nil {
		t.Fatal("formal Emacs contract allowed unrelated Helix evidence before promotion")
	}
	for index := range checks {
		if checks[index].ID == "client/emacs-eglot/go" {
			checks[index].Status = report.NotVerified
			break
		}
	}
	if err := requireClientMatrixPassed(checks, "emacs-eglot"); err == nil {
		t.Fatal("formal Emacs gate allowed missing/not_verified Emacs evidence")
	}
}

func TestResolvedBinaryBindingRequiresLockedPathAndSHA256(t *testing.T) {
	dir := t.TempDir()
	lockedPath := filepath.Join(dir, "locked.exe")
	otherPath := filepath.Join(dir, "same-content.exe")
	if err := os.WriteFile(lockedPath, []byte("locked binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherPath, []byte("locked binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := fileSHA256(lockedPath)
	if err != nil {
		t.Fatal(err)
	}
	lock := toolLock{ResolvedBinaries: map[string]resolvedBinary{
		"fixture": {Path: lockedPath, SHA256: hash},
	}}
	if got, err := verifyLockedBinary(lock, "fixture"); err != nil || !sameExecutablePath(got, lockedPath) {
		t.Fatalf("locked binary rejected: path=%q err=%v", got, err)
	}
	if _, err := verifyLockedBinary(toolLock{ResolvedBinaries: map[string]resolvedBinary{
		"fixture": {Path: lockedPath, SHA256: strings.Repeat("0", sha256.Size*2)},
	}}, "fixture"); err == nil {
		t.Fatal("wrong SHA-256 was accepted")
	}
	if _, err := verifyLockedBinary(toolLock{ResolvedBinaries: map[string]resolvedBinary{}}, "fixture"); err == nil {
		t.Fatal("missing resolved binary was accepted")
	}
	if _, err := verifyLockedBinary(toolLock{ResolvedBinaries: map[string]resolvedBinary{
		"fixture": {Path: otherPath, SHA256: hash},
	}}, "fixture"); err != nil {
		t.Fatalf("same-content alternate path should verify its own explicit pin: %v", err)
	}
}

func TestSemanticRuntimeBindingRequiresLockedPaths(t *testing.T) {
	dir := t.TempDir()
	nodePath := filepath.Join(dir, "node.exe")
	typescriptPath := filepath.Join(dir, "typescript.js")
	otherPath := filepath.Join(dir, "other.exe")
	for path, contents := range map[string][]byte{
		nodePath:       []byte("locked node"),
		typescriptPath: []byte("locked TypeScript"),
		otherPath:      []byte("same path is not the lock"),
	} {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	nodeHash, err := fileSHA256(nodePath)
	if err != nil {
		t.Fatal(err)
	}
	typescriptHash, err := fileSHA256(typescriptPath)
	if err != nil {
		t.Fatal(err)
	}
	lock := toolLock{ResolvedBinaries: map[string]resolvedBinary{
		"node":       {Path: nodePath, SHA256: nodeHash},
		"typescript": {Path: typescriptPath, SHA256: typescriptHash},
	}}
	env := map[string]string{
		"OMNILSP_SEMANTIC_NODE_PATH":       nodePath,
		"OMNILSP_SEMANTIC_TYPESCRIPT_PATH": typescriptPath,
	}
	for _, binding := range semanticRuntimeBindings()[2:] {
		path := filepath.Join(dir, binding.tool+".bin")
		if err := os.WriteFile(path, []byte("locked "+binding.tool), 0o600); err != nil {
			t.Fatal(err)
		}
		hash, err := fileSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		lock.ResolvedBinaries[binding.tool] = resolvedBinary{Path: path, SHA256: hash}
		env[binding.env] = path
	}
	if check := verifySemanticRuntimeBinding(lock, env); check.Status != report.Passed {
		t.Fatalf("exact semantic runtime paths rejected: %+v", check)
	}
	for _, binding := range semanticRuntimeBindings() {
		original := env[binding.env]
		env[binding.env] = otherPath
		if check := verifySemanticRuntimeBinding(lock, env); check.Status != report.Failed {
			t.Fatalf("%s path mismatch was not rejected: %+v", binding.tool, check)
		}
		env[binding.env] = original
	}
	if check := verifySemanticRuntimeBinding(lock, map[string]string{}); check.Status != report.NotVerified {
		t.Fatalf("missing semantic runtime paths were not kept not_verified: %+v", check)
	}
}

func TestAppendFamilyChecksRecordsSortedSubcaseNames(t *testing.T) {
	cases := []editorCase{
		{Name: "typescriptreact", LanguageID: "typescriptreact", Family: "typescript"},
		{Name: "javascriptreact", LanguageID: "javascriptreact", Family: "typescript"},
		{Name: "javascript", LanguageID: "javascript", Family: "typescript"},
		{Name: "typescript", LanguageID: "typescript", Family: "typescript"},
	}
	r := report.New("client-case-manifest")
	appendFamilyChecks(&r, "vscode", cases, nil, report.NotVerified, "test fallback")
	for _, check := range r.Checks {
		if check.ID != "client/vscode/typescript" {
			continue
		}
		names, ok := check.Observed["subcase_names"].([]string)
		if !ok {
			t.Fatalf("subcase_names has type %T, want []string", check.Observed["subcase_names"])
		}
		if strings.Join(names, ",") != "javascript,javascriptreact,typescript,typescriptreact" {
			t.Fatalf("subcase_names = %v, want sorted JavaScript/TypeScript cases", names)
		}
		return
	}
	t.Fatal("client/vscode/typescript report check was not emitted")
}

func TestAppendFamilyChecksFailureSummaryIncludesClientRunFailure(t *testing.T) {
	cases := []editorCase{{Name: "go", LanguageID: "go", Family: "go"}}
	r := report.New("client-run-failure")
	appendFamilyChecks(&r, "vscode", cases, nil, report.Failed, "VS Code extension host timed out")
	if len(r.Checks) != 1 {
		t.Fatalf("got %d checks, want one", len(r.Checks))
	}
	check := r.Checks[0]
	if check.Status != report.Failed {
		t.Fatalf("status = %s, want failed", check.Status)
	}
	if check.Summary != "go: VS Code extension host timed out" {
		t.Fatalf("summary = %q, want the failed client-run reason", check.Summary)
	}
}

func TestAppendVSCodeHostFailurePreservesPartialResults(t *testing.T) {
	cases := []editorCase{
		{Name: "go", LanguageID: "go", Family: "go"},
		{Name: "c", LanguageID: "c", Family: "cpp"},
		{Name: "cpp", LanguageID: "cpp", Family: "cpp"},
		{Name: "rust", LanguageID: "rust", Family: "rust"},
		{Name: "python", LanguageID: "python", Family: "python"},
		{Name: "typescript", LanguageID: "typescript", Family: "typescript"},
		{Name: "javascript", LanguageID: "javascript", Family: "typescript"},
	}
	rows := []editorCaseResult{
		{Name: "go", Status: string(report.Passed)},
		{Name: "c", Status: string(report.Passed)},
		{Name: "cpp", Status: string(report.Failed), Reason: "fixture assertion failed"},
		{Name: "rust", Status: string(report.Running), Stage: "diagnostics"},
		{Name: "typescript", Status: string(report.Passed)},
	}
	detail := "VS Code extension host terminated with exit status 1"
	r := report.New("vscode-partial-results")

	appendVSCodeHostFailure(&r, cases, rows, detail)

	wantStatuses := map[string]report.Status{
		"client/vscode/go":         report.Passed,
		"client/vscode/cpp":        report.Failed,
		"client/vscode/rust":       report.NotVerified,
		"client/vscode/python":     report.NotVerified,
		"client/vscode/typescript": report.NotVerified,
		"client/vscode/host":       report.Failed,
	}
	for id, want := range wantStatuses {
		found := false
		for _, check := range r.Checks {
			if check.ID != id {
				continue
			}
			found = true
			if check.Status != want {
				t.Errorf("%s status = %s, want %s", id, check.Status, want)
			}
			if id == "client/vscode/host" && check.Summary != detail {
				t.Errorf("host summary = %q, want %q", check.Summary, detail)
			}
			if id == "client/vscode/cpp" {
				subcases, ok := check.Observed["subcases"].([]map[string]any)
				if !ok || len(subcases) != 2 || subcases[0]["status"] != string(report.Passed) || subcases[1]["status"] != string(report.Failed) {
					t.Errorf("C/C++ case statuses = %#v, want c=passed and cpp=failed", check.Observed["subcases"])
				}
			}
			break
		}
		if !found {
			t.Errorf("missing check %q", id)
		}
	}
	if len(r.Errors) != 2 || !strings.HasPrefix(r.Errors[0], "client/vscode/cpp:") || r.Errors[1] != detail {
		t.Fatalf("report errors = %v, want the C++ case failure followed by the host failure", r.Errors)
	}
}

func TestAppendEmacsProcessFailurePreservesCaseResultsAndFailsFormalGate(t *testing.T) {
	cases := make([]editorCase, 0, len(expectedEditorCaseManifest))
	rows := make([]editorCaseResult, 0, len(expectedEditorCaseManifest))
	for _, expected := range expectedEditorCaseManifest {
		cases = append(cases, editorCase{Name: expected.Name, LanguageID: expected.LanguageID, Family: expected.Family})
		status := string(report.Passed)
		row := editorCaseResult{Name: expected.Name, LanguageID: expected.LanguageID, Family: expected.Family, Status: status}
		if expected.Name == "c" || expected.Name == "cpp" {
			row.Observed = map[string]any{
				"diagnostic_scope":    "clean_source_no_false_positive",
				"deferred_capability": "DEF-CCLSDIAG",
				"published_count":     0,
			}
		}
		if expected.Name == "rust" {
			row.Status = string(report.Failed)
			row.Error = "Flymake did not show unresolved-name diagnostic for rust"
		}
		rows = append(rows, row)
	}

	t.Run("preserves completed cells", func(t *testing.T) {
		r := report.New("emacs-process-failure")
		appendClientHostFailure(&r, "emacs-eglot", cases, rows, "emacs-eglot process returned exit code 1")
		statuses := make(map[string]report.Status)
		for _, check := range r.Checks {
			statuses[check.ID] = check.Status
		}
		if statuses["client/emacs-eglot/go"] != report.Passed || statuses["client/emacs-eglot/rust"] != report.Failed || statuses["client/emacs-eglot/host"] != report.Failed {
			t.Fatalf("Emacs report statuses lost native outcomes: %#v", statuses)
		}
	})

	t.Run("rejects nonzero editor exit even if every case passed", func(t *testing.T) {
		passedRows := make([]editorCaseResult, 0, len(cases))
		for _, testCase := range cases {
			row := editorCaseResult{Name: testCase.Name, LanguageID: testCase.LanguageID, Family: testCase.Family, Status: string(report.Passed)}
			if testCase.Name == "c" || testCase.Name == "cpp" {
				row.Observed = map[string]any{
					"diagnostic_scope":    "clean_source_no_false_positive",
					"deferred_capability": "DEF-CCLSDIAG",
					"published_count":     0,
				}
			}
			passedRows = append(passedRows, row)
		}
		r := report.New("emacs-nonzero-exit")
		appendClientHostFailure(&r, "emacs-eglot", cases, passedRows, "emacs-eglot process returned exit code 1")
		r.Finalize(time.Now())
		if err := requireClientMatrixPassed(r.Checks, "emacs-eglot"); err != nil {
			t.Fatalf("synthetic Emacs case results should all pass: %v", err)
		}
		if err := requireFormalEmacsOutcome(r); err == nil {
			t.Fatal("formal Emacs gate accepted a nonzero native process exit")
		}
	})
}

func TestNativeProcessFailureEvidenceIsStoredOnceAndKeepsGateClosed(t *testing.T) {
	r := report.New("emacs-native-batch")
	detail := "emacs-eglot process returned exit code 1; client output: " + strings.Repeat("native stderr evidence\n", 8)
	reason := recordNativeProcessFailure(&r, "emacs-eglot", detail)
	cases := make([]editorCase, 0, len(expectedEditorCaseManifest))
	for _, expected := range expectedEditorCaseManifest {
		cases = append(cases, editorCase{Name: expected.Name, LanguageID: expected.LanguageID, Family: expected.Family})
	}
	appendClientChecks(&r, "emacs-eglot", cases, nil, report.Failed, reason)
	addCheck(&r, "client/emacs-eglot/host", report.Failed, reason)
	r.Errors = append(r.Errors, reason)
	r.Finalize(time.Now())

	if got := r.Environment["nativeProcessError.emacs-eglot"]; got != detail {
		t.Fatalf("process evidence = %q, want original bounded detail", got)
	}
	for _, check := range r.Checks {
		if strings.Contains(check.Summary, detail) {
			t.Fatalf("check %s duplicated detailed process output in its summary", check.ID)
		}
		if !strings.Contains(check.Summary, r.RunID) {
			t.Fatalf("check %s summary does not reference batch %s: %q", check.ID, r.RunID, check.Summary)
		}
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	encodedDetail, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	if count := strings.Count(string(encoded), string(encodedDetail)); count != 1 {
		t.Fatalf("detailed process output occurs %d times in report, want exactly once", count)
	}
	if err := requireFormalEmacsOutcome(r); err == nil {
		t.Fatal("formal Emacs gate accepted a native process exit failure")
	}

	tooLong := strings.Repeat("x", maxClientOutputBytes+1)
	bounded := report.New("bounded-native-batch")
	_ = recordNativeProcessFailure(&bounded, "emacs-eglot", tooLong)
	if got := len(bounded.Environment["nativeProcessError.emacs-eglot"]); got != maxClientOutputBytes {
		t.Fatalf("bounded process evidence is %d bytes, want at most %d", got, maxClientOutputBytes)
	}
}

func TestAppendCppFamilyRecordsDeferredCleanDiagnostics(t *testing.T) {
	cases := []editorCase{
		{Name: "cpp", LanguageID: "cpp", Family: "cpp"},
		{Name: "c", LanguageID: "c", Family: "cpp"},
	}
	diagnostic := map[string]any{
		"diagnostic_scope":    "clean_source_no_false_positive",
		"deferred_capability": "DEF-CCLSDIAG",
		"published_count":     0,
	}
	rows := []editorCaseResult{
		{Name: "c", LanguageID: "c", Family: "cpp", Status: string(report.Passed), Observed: diagnostic},
		{Name: "cpp", LanguageID: "cpp", Family: "cpp", Status: string(report.Passed), Observed: diagnostic},
	}
	r := report.New("cpp-diagnostic-scope")
	appendFamilyChecks(&r, "vscode", cases, rows, "", "")
	for _, check := range r.Checks {
		if check.ID != "client/vscode/cpp" {
			continue
		}
		if check.Status != report.Passed {
			t.Fatalf("C/C++ family status = %s, want passed: %s", check.Status, check.Summary)
		}
		if check.Observed["diagnostic_scope"] != "clean_source_no_false_positive" || check.Observed["deferred_capability"] != "DEF-CCLSDIAG" {
			t.Fatalf("C/C++ diagnostic scope was not preserved in aggregate evidence: %#v", check.Observed)
		}
		return
	}
	t.Fatal("client/vscode/cpp report check was not emitted")
}

func TestAppendCppFamilyRejectsMissingCleanDiagnosticEvidence(t *testing.T) {
	cases := []editorCase{
		{Name: "cpp", LanguageID: "cpp", Family: "cpp"},
		{Name: "c", LanguageID: "c", Family: "cpp"},
	}
	rows := []editorCaseResult{
		{Name: "c", LanguageID: "c", Family: "cpp", Status: string(report.Passed)},
		{Name: "cpp", LanguageID: "cpp", Family: "cpp", Status: string(report.Passed)},
	}
	r := report.New("cpp-missing-diagnostic-scope")
	appendFamilyChecks(&r, "emacs-eglot", cases, rows, "", "")
	for _, check := range r.Checks {
		if check.ID == "client/emacs-eglot/cpp" {
			if check.Status != report.Failed || !strings.Contains(check.Summary, "missing its clean-source/deferred-capability scope") {
				t.Fatalf("C/C++ family without evidence = %s %q, want explicit failure", check.Status, check.Summary)
			}
			return
		}
	}
	t.Fatal("client/emacs-eglot/cpp report check was not emitted")
}

func buildServer(root, outDir string) (string, error) {
	if fromEnv := os.Getenv("OMNILSP_BIN"); fromEnv != "" {
		if info, err := os.Stat(fromEnv); err == nil && !info.IsDir() {
			return filepath.Abs(fromEnv)
		}
		return "", fmt.Errorf("OMNILSP_BIN is not a file: %s", fromEnv)
	}
	return "", errors.New("OMNILSP_BIN must name the frozen candidate binary; client acceptance does not rebuild it")
}

func makeClientEnv(root, workspace, serverBin string, toolStatus map[string]report.Status, lock toolLock) map[string]string {
	toolsRoot := filepath.Join(root, "test", "acceptance", "tools")
	pathParts := []string{filepath.Join(toolsRoot, "bin")}
	for _, name := range requiredResolvedBinaryNames() {
		if entry, ok := lock.ResolvedBinaries[name]; ok && filepath.IsAbs(entry.Path) {
			directory := filepath.Dir(filepath.Clean(entry.Path))
			if directory != "" && !slicesContainsFold(pathParts, directory) {
				pathParts = append(pathParts, directory)
			}
		}
	}
	if current := os.Getenv("PATH"); current != "" {
		pathParts = append(pathParts, current)
	}
	available := make(map[string]bool, len(toolStatus))
	for name, status := range toolStatus {
		available[name] = status == report.Passed
	}
	availableData, _ := json.Marshal(available)
	return map[string]string{
		"OMNILSP_BIN":                            serverBin,
		"OMNILSP_TRUST":                          "trusted",
		"OMNILSP_MAX_CONCURRENT":                 "8",
		"OMNILSP_MAX_QUEUE":                      "64",
		"GOMAXPROCS":                             "8",
		"PATH":                                   strings.Join(pathParts, string(os.PathListSeparator)),
		"OMNILSP_ACCEPTANCE_NODE":                lock.ResolvedBinaries["node"].Path,
		"OMNILSP_ACCEPTANCE_NODE_SHA256":         lock.ResolvedBinaries["node"].SHA256,
		"OMNILSP_ACCEPTANCE_LOCKED_TOOLS":        "1",
		"OMNILSP_SEMANTIC_NODE_PATH":             lock.ResolvedBinaries["node"].Path,
		"OMNILSP_SEMANTIC_TYPESCRIPT_PATH":       lock.ResolvedBinaries["typescript"].Path,
		"OMNILSP_SEMANTIC_PYTHON_PATH":           lock.ResolvedBinaries["python"].Path,
		"OMNILSP_SEMANTIC_PYRIGHT_INTERNAL_PATH": lock.ResolvedBinaries["pyright-internal"].Path,
		"OMNILSP_SEMANTIC_PYRIGHT_VENDOR_PATH":   lock.ResolvedBinaries["pyright-vendor"].Path,
		"OMNILSP_INDEX_DIR":                      filepath.Join(workspace, ".omnilsp-index"),
		"OMNILSP_CLIENT_CASES":                   filepath.Join(workspace, ".omnilsp-client-cases.json"),
		"OMNILSP_CLIENT_CASE_ONLY":               strings.TrimSpace(os.Getenv("OMNILSP_CLIENT_CASE_ONLY")),
		"OMNILSP_CLIENT_TOOL_STATUS":             string(availableData),
	}
}

func slicesContainsFold(values []string, candidate string) bool {
	for _, value := range values {
		if strings.EqualFold(filepath.Clean(value), filepath.Clean(candidate)) {
			return true
		}
	}
	return false
}

func runVSCode(root, workspace, resultPath string, env map[string]string, lock toolLock) error {
	code, err := verifyLockedBinary(lock, "vscode")
	if err != nil {
		return err
	}
	if configured := strings.TrimSpace(os.Getenv("OMNILSP_VSCODE_BIN")); configured != "" && !sameExecutablePath(configured, code) {
		return fmt.Errorf("OMNILSP_VSCODE_BIN %q does not match locked VS Code path %q", configured, code)
	}
	userData, err := os.MkdirTemp("", "omnilsp-vscode-user-")
	if err != nil {
		return err
	}
	preserveUserData := strings.TrimSpace(os.Getenv("OMNILSP_VSCODE_TRACE")) != ""
	defer func() {
		if !preserveUserData {
			_ = os.RemoveAll(userData)
		}
	}()
	extensionsDir := filepath.Join(userData, "extensions")
	if err := os.MkdirAll(extensionsDir, 0o755); err != nil {
		return err
	}
	settingsDir := filepath.Join(userData, "User")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		return err
	}
	settings := map[string]any{
		"security.workspace.trust.enabled": false,
		"extensions.autoUpdate":            false,
		"extensions.autoCheckUpdates":      false,
	}
	if trace := strings.TrimSpace(os.Getenv("OMNILSP_VSCODE_TRACE")); trace != "" {
		settings["omnilsp.trace.server"] = trace
	}
	settingsData, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode VS Code test settings: %w", err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), settingsData, 0o600); err != nil {
		return err
	}
	args := []string{
		"--user-data-dir=" + userData,
		"--extensions-dir=" + extensionsDir,
		"--no-sandbox",
		"--disable-gpu-sandbox",
		"--disable-gpu",
		"--disable-updates",
		"--no-cached-data",
		"--disable-workspace-trust",
		"--disable-extension",
		"vscode.typescript-language-features",
		"--verbose",
		"--skip-welcome",
		"--skip-release-notes",
		"--new-window",
		"--extensionDevelopmentPath=" + filepath.Join(root, "editors", "vscode"),
		"--extensionTestsPath=" + filepath.Join(root, "test", "acceptance", "clients", "vscode", "test-entry.js"),
		"--folder-uri=" + fileURI(workspace),
	}
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, code, args...)
	cmd.Dir = workspace
	clientEnv := make(map[string]string, len(env)+1)
	for key, value := range env {
		clientEnv[key] = value
	}
	clientEnv["OMNILSP_CLIENT_RESULT"] = resultPath
	cmd.Env = mergeEnv(os.Environ(), clientEnv)
	type commandOutcome struct {
		output []byte
		err    error
	}
	commandDone := make(chan commandOutcome, 1)
	var processTree *editorProcessTracker
	go func() {
		output, runErr := runClientCommandTracked(ctx, cmd, func(pid int) error {
			var trackErr error
			processTree, trackErr = startEditorProcessTracker(pid)
			return trackErr
		})
		commandDone <- commandOutcome{output: output, err: runErr}
	}()
	startupTimer := time.NewTimer(clientStartupTimeout)
	defer startupTimer.Stop()
	progressTicker := time.NewTicker(250 * time.Millisecond)
	defer progressTicker.Stop()
	started := false
	startupTimedOut := false
	var outcome commandOutcome
waitForClient:
	for {
		select {
		case outcome = <-commandDone:
			break waitForClient
		case <-progressTicker.C:
			if !started {
				if _, statErr := os.Stat(resultPath); statErr == nil {
					started = true
					if !startupTimer.Stop() {
						select {
						case <-startupTimer.C:
						default:
						}
					}
				}
			}
		case <-startupTimer.C:
			if _, statErr := os.Stat(resultPath); statErr == nil {
				started = true
				continue
			}
			startupTimedOut = true
			cancel()
			outcome = <-commandDone
			break waitForClient
		}
	}
	output, runErr := outcome.output, outcome.err
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	serverExitErr := waitForEditorProcessTree(processTree)
	serverExitConfirmed := serverExitErr == nil
	serverExitEvidence := "the VS Code root process and every observed descendant exited"
	if serverExitErr != nil {
		serverExitEvidence = serverExitErr.Error()
	}
	recordErr := recordEditorExitOutcome(resultPath, exitCode, serverExitConfirmed, serverExitEvidence)
	if runErr != nil {
		if ctx.Err() != nil {
			detail := fmt.Sprintf("VS Code extension host exceeded %s: %v", clientTimeout, ctx.Err())
			if startupTimedOut {
				detail = fmt.Sprintf("VS Code acceptance runner produced no initial result heartbeat within %s", clientStartupTimeout)
			}
			if progress := readClientProgress(resultPath); progress != "" {
				detail += "; last client progress: " + progress
			}
			if serverExitErr != nil {
				detail += "; process cleanup: " + serverExitErr.Error()
			}
			if recordErr != nil {
				detail += "; record exit evidence: " + recordErr.Error()
			}
			if text := strings.TrimSpace(string(output)); text != "" {
				detail += "; client output: " + text
			}
			preserveUserData = true
			detail += "; VS Code profile retained at " + userData
			return errors.New(detail)
		}
		preserveUserData = true
		return fmt.Errorf("VS Code extension host: %w: %s", runErr, strings.TrimSpace(string(output)))
	}
	if recordErr != nil {
		preserveUserData = true
		return fmt.Errorf("record VS Code exit evidence: %w", recordErr)
	}
	if exitCode != 0 {
		preserveUserData = true
		return fmt.Errorf("VS Code process returned exit code %d", exitCode)
	}
	if serverExitErr != nil {
		preserveUserData = true
		return fmt.Errorf("VS Code process-tree cleanup: %w", serverExitErr)
	}
	return nil
}

func readClientProgress(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var progress struct {
		Stage string `json:"stage"`
		Cases []struct {
			Name  string `json:"name"`
			Stage string `json:"stage"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &progress); err != nil {
		return ""
	}
	if len(progress.Cases) > 0 {
		last := progress.Cases[len(progress.Cases)-1]
		if last.Stage != "" {
			return last.Name + ": " + last.Stage
		}
	}
	return progress.Stage
}

func runNeovim(root, workspace, resultPath string, env map[string]string, lock toolLock) error {
	nvim, err := verifyLockedBinary(lock, "neovim")
	if err != nil {
		return err
	}
	if configured := strings.TrimSpace(os.Getenv("OMNILSP_NVIM_BIN")); configured != "" && !sameExecutablePath(configured, nvim) {
		return fmt.Errorf("OMNILSP_NVIM_BIN %q does not match locked Neovim path %q", configured, nvim)
	}
	args := []string{
		"--headless", "--noplugin",
		"-u", filepath.Join(root, "test", "acceptance", "clients", "neovim", "init.lua"),
		"-l", filepath.Join(root, "test", "acceptance", "clients", "neovim", "run.lua"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, nvim, args...)
	cmd.Dir = workspace
	clientEnv := make(map[string]string, len(env)+1)
	for key, value := range env {
		clientEnv[key] = value
	}
	clientEnv["OMNILSP_CLIENT_RESULT"] = resultPath
	clientEnv["NVIM_LOG_FILE"] = filepath.Join(workspace, "nvim.log")
	// Keep Neovim's stdpath("log")/lsp.log inside this disposable fixture
	// workspace, and keep its low-level log there too. The host profile may be
	// unavailable to a sandboxed test run.
	clientEnv["XDG_STATE_HOME"] = filepath.Join(workspace, ".nvim-state")
	cmd.Env = mergeEnv(os.Environ(), clientEnv)
	var processTree *editorProcessTracker
	output, runErr := runClientCommandTracked(ctx, cmd, func(pid int) error {
		var trackErr error
		processTree, trackErr = startEditorProcessTracker(pid)
		return trackErr
	})
	processExitErr := waitForEditorProcessTree(processTree)
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	priorResult, resultErr := readEditorResult(resultPath)
	serverExitConfirmed, serverExitEvidence := confirmNeovimShutdown(priorResult, resultErr, processExitErr)
	recordErr := recordEditorExitOutcome(resultPath, exitCode, serverExitConfirmed, serverExitEvidence)
	if runErr != nil {
		if ctx.Err() != nil {
			detail := fmt.Sprintf("Neovim headless client exceeded %s: %v", clientTimeout, ctx.Err())
			if processExitErr != nil {
				detail += "; process cleanup: " + processExitErr.Error()
			}
			if recordErr != nil {
				detail += "; record exit evidence: " + recordErr.Error()
			}
			if text := strings.TrimSpace(string(output)); text != "" {
				detail += "; client output: " + text
			}
			return errors.New(detail)
		}
		return fmt.Errorf("Neovim headless client: %w: %s", runErr, strings.TrimSpace(string(output)))
	}
	if recordErr != nil {
		return fmt.Errorf("record Neovim exit evidence: %w", recordErr)
	}
	if exitCode != 0 {
		return fmt.Errorf("Neovim process returned exit code %d", exitCode)
	}
	if !serverExitConfirmed {
		return fmt.Errorf("Neovim did not prove graceful OmniLSP client and process-tree shutdown: %s", serverExitEvidence)
	}
	return nil
}

// runClientCommand deliberately redirects output to a regular file instead of
// runToolVersionProbe bounds editor/tool startup checks and uses the same
// file-backed output capture as the full client command.
func runToolVersionProbe(path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), clientToolProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	output, err := runClientCommand(ctx, cmd)
	if ctx.Err() != nil {
		return strings.TrimSpace(string(output)), fmt.Errorf("version probe exceeded %s: %w", clientToolProbeTimeout, ctx.Err())
	}
	if err != nil {
		return strings.TrimSpace(string(output)), err
	}
	return strings.TrimSpace(string(output)), nil
}

// runClientCommand avoids Cmd.CombinedOutput. On Windows, VS Code and language
// servers can spawn descendants that inherit stdout/stderr. A pipe can remain
// open after the launcher exits and block forever; a file does not keep the
// parent blocked. Process snapshots still decide whether shutdown was clean.
func runClientCommand(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	return runClientCommandTracked(ctx, cmd, nil)
}

// runClientCommandTracked reports the root PID as soon as the executable is
// started, before editor descendants can outlive it. The process tracker uses
// that PID and native PPID snapshots to attribute only this client's tree.
func runClientCommandTracked(ctx context.Context, cmd *exec.Cmd, onStarted func(pid int) error) ([]byte, error) {
	configureHiddenClientProcess(cmd)
	outputFile, err := os.CreateTemp("", "omnilsp-client-output-*.log")
	if err != nil {
		return nil, fmt.Errorf("create client output log: %w", err)
	}
	outputPath := outputFile.Name()
	cmd.Stdout = outputFile
	cmd.Stderr = outputFile

	runErr := cmd.Start()
	if runErr == nil && onStarted != nil {
		if startErr := onStarted(cmd.Process.Pid); startErr != nil {
			_ = cmd.Process.Kill()
			waitErr := cmd.Wait()
			runErr = fmt.Errorf("start managed client process tracking: %w", startErr)
			if waitErr != nil {
				runErr = fmt.Errorf("%w; wait after tracking failure: %v", runErr, waitErr)
			}
		}
	}
	if runErr == nil {
		runErr = cmd.Wait()
	}
	closeErr := outputFile.Close()
	output, readErr := readClientOutput(outputPath)
	// A descendant may still hold the inherited log handle. Removing the log
	// can consequently fail on Windows; diagnostics remain bounded and the
	// best-effort cleanup is retried by the OS when the handle is released.
	_ = os.Remove(outputPath)
	if closeErr != nil && runErr == nil {
		runErr = fmt.Errorf("close client output log: %w", closeErr)
	}
	if readErr != nil {
		if runErr == nil {
			runErr = fmt.Errorf("read client output log: %w", readErr)
		} else {
			runErr = fmt.Errorf("%w; read client output log: %v", runErr, readErr)
		}
	}
	return output, runErr
}

func readClientOutput(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxClientOutputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) <= maxClientOutputBytes {
		return data, nil
	}
	const suffix = "\n[client output truncated]\n"
	keep := maxClientOutputBytes - len(suffix)
	return append(append([]byte(nil), data[:keep]...), suffix...), nil
}

func confirmNeovimShutdown(result editorResult, resultErr, processExitErr error) (bool, string) {
	if resultErr != nil {
		return false, fmt.Sprintf("read Neovim shutdown evidence: %v", resultErr)
	}
	evidence := result.ServerExitEvidence
	if evidence == "" {
		evidence = "Neovim language client stop acknowledgement was not recorded"
	}
	if !result.ServerExitConfirmed {
		return false, evidence
	}
	if processExitErr != nil {
		return false, fmt.Sprintf("%s; Neovim process-tree cleanup failed: %v", evidence, processExitErr)
	}
	return true, evidence + "; the Neovim root process and every observed descendant exited"
}

const editorProcessPollPeriod = 100 * time.Millisecond

type editorProcessRecord struct {
	PID       int
	ParentPID int
	Image     string
}

type editorProcessIdentity struct {
	PID          int
	ParentPID    int
	Image        string
	CreationTime uint64
}

func (p editorProcessIdentity) String() string {
	return fmt.Sprintf("image=%s pid=%d ppid=%d creation_filetime=%d", p.Image, p.PID, p.ParentPID, p.CreationTime)
}

type editorProcessSnapshotSource interface {
	snapshot() (map[int]editorProcessRecord, error)
	creationTime(pid int) (uint64, error)
}

type editorProcessTracker struct {
	source editorProcessSnapshotSource
	root   editorProcessIdentity

	scanMu sync.Mutex
	mu     sync.Mutex
	known  map[int]editorProcessIdentity
	live   []editorProcessIdentity
	err    error

	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func startEditorProcessTracker(rootPID int) (*editorProcessTracker, error) {
	source, err := newEditorProcessSnapshotSource()
	if err != nil {
		return nil, fmt.Errorf("create native process snapshot source: %w", err)
	}
	snapshot, err := source.snapshot()
	if err != nil {
		return nil, fmt.Errorf("enumerate process PPIDs before tracking root PID %d: %w", rootPID, err)
	}
	rootRecord, ok := snapshot[rootPID]
	if !ok {
		return nil, fmt.Errorf("started editor root PID %d is absent from the native PPID snapshot", rootPID)
	}
	rootCreated, err := source.creationTime(rootPID)
	if err != nil {
		return nil, fmt.Errorf("identify editor root image=%s pid=%d ppid=%d: %w", rootRecord.Image, rootPID, rootRecord.ParentPID, err)
	}
	tracker := &editorProcessTracker{
		source: source,
		root: editorProcessIdentity{
			PID: rootPID, ParentPID: rootRecord.ParentPID, Image: rootRecord.Image, CreationTime: rootCreated,
		},
		known: make(map[int]editorProcessIdentity),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	tracker.known[rootPID] = tracker.root
	if err := tracker.scanOnce(); err != nil {
		return nil, err
	}
	go tracker.monitor()
	return tracker, nil
}

func (t *editorProcessTracker) monitor() {
	ticker := time.NewTicker(editorProcessPollPeriod)
	defer ticker.Stop()
	defer close(t.done)
	for {
		select {
		case <-t.stop:
			return
		case <-ticker.C:
			if err := t.scanOnce(); err != nil {
				return
			}
		}
	}
}

func (t *editorProcessTracker) scanOnce() error {
	t.scanMu.Lock()
	defer t.scanMu.Unlock()

	snapshot, err := t.source.snapshot()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.err != nil {
		return t.err
	}
	if err != nil {
		t.err = fmt.Errorf("native process/PPID enumeration failed for root %s: %w", t.root, err)
		return t.err
	}
	if err := t.observeDescendantsLocked(snapshot); err != nil {
		t.err = err
		return err
	}
	live, err := t.liveProcessesLocked(snapshot)
	if err != nil {
		t.err = err
		return err
	}
	t.live = live
	return nil
}

func (t *editorProcessTracker) observeDescendantsLocked(snapshot map[int]editorProcessRecord) error {
	for changed := true; changed; {
		changed = false
		for pid, record := range snapshot {
			parent, managed := t.known[record.ParentPID]
			if !managed || pid == record.ParentPID {
				continue
			}
			// Admit a process only while its recorded parent is present with the
			// same creation identity. This avoids attributing an unrelated process
			// whose stale/reused PPID happens to match an earlier client PID.
			parentRecord, parentPresent := snapshot[parent.PID]
			if !parentPresent {
				continue
			}
			if !strings.EqualFold(parentRecord.Image, parent.Image) || parentRecord.ParentPID != parent.ParentPID {
				continue
			}
			parentCreated, present, err := processCreationTimeInSnapshot(t.source, snapshot, parent.PID)
			if err != nil {
				return fmt.Errorf("identify managed parent image=%s pid=%d ppid=%d: %w", parent.Image, parent.PID, parent.ParentPID, err)
			}
			if !present || parentCreated != parent.CreationTime || parentRecord.PID != parent.PID {
				continue
			}
			created, present, err := processCreationTimeInSnapshot(t.source, snapshot, pid)
			if err != nil {
				return fmt.Errorf("identify possible descendant image=%s pid=%d ppid=%d: %w", record.Image, pid, record.ParentPID, err)
			}
			if !present || created < parent.CreationTime {
				continue
			}
			previous, alreadyKnown := t.known[pid]
			if alreadyKnown && previous.CreationTime == created {
				continue
			}
			t.known[pid] = editorProcessIdentity{PID: pid, ParentPID: record.ParentPID, Image: record.Image, CreationTime: created}
			changed = true
		}
	}
	return nil
}

func (t *editorProcessTracker) liveProcessesLocked(snapshot map[int]editorProcessRecord) ([]editorProcessIdentity, error) {
	var live []editorProcessIdentity
	for pid, identity := range t.known {
		record, present := snapshot[pid]
		if !present {
			continue
		}
		if !strings.EqualFold(record.Image, identity.Image) || record.ParentPID != identity.ParentPID {
			continue
		}
		created, present, err := processCreationTimeInSnapshot(t.source, snapshot, pid)
		if err != nil {
			return nil, fmt.Errorf("verify managed process image=%s pid=%d ppid=%d: %w", identity.Image, identity.PID, identity.ParentPID, err)
		}
		if !present || created != identity.CreationTime {
			continue
		}
		live = append(live, identity)
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].PID != live[j].PID {
			return live[i].PID < live[j].PID
		}
		return strings.ToLower(live[i].Image) < strings.ToLower(live[j].Image)
	})
	return live, nil
}

// processCreationTimeInSnapshot tolerates a process that exits between the
// Toolhelp snapshot and OpenProcess, but fails closed if PPID-visible identity
// data remains inaccessible.
func processCreationTimeInSnapshot(source editorProcessSnapshotSource, snapshot map[int]editorProcessRecord, pid int) (uint64, bool, error) {
	record, present := snapshot[pid]
	if !present {
		return 0, false, nil
	}
	created, err := source.creationTime(pid)
	if err == nil {
		return created, true, nil
	}
	current, snapshotErr := source.snapshot()
	if snapshotErr != nil {
		return 0, false, fmt.Errorf("recheck PID %d after creation-time query failure: %v; PPID enumeration: %w", pid, err, snapshotErr)
	}
	currentRecord, stillPresent := current[pid]
	if !stillPresent {
		return 0, false, nil
	}
	if currentRecord.ParentPID != record.ParentPID || !strings.EqualFold(currentRecord.Image, record.Image) {
		return 0, false, nil
	}
	created, err = source.creationTime(pid)
	if err != nil {
		return 0, false, fmt.Errorf("creation time remains unavailable for image=%s pid=%d ppid=%d: %w", currentRecord.Image, pid, currentRecord.ParentPID, err)
	}
	return created, true, nil
}

func (t *editorProcessTracker) waitForExit(timeout time.Duration) error {
	defer t.stopMonitor()
	deadline := time.Now().Add(timeout)
	for {
		if err := t.scanOnce(); err != nil {
			return err
		}
		t.mu.Lock()
		live := append([]editorProcessIdentity(nil), t.live...)
		trackerErr := t.err
		t.mu.Unlock()
		if trackerErr != nil {
			return trackerErr
		}
		if len(live) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			parts := make([]string, len(live))
			for i, process := range live {
				parts[i] = process.String()
			}
			return fmt.Errorf("managed editor process descendants remained after shutdown (root %s): %s", t.root, strings.Join(parts, ", "))
		}
		time.Sleep(editorProcessPollPeriod)
	}
}

func (t *editorProcessTracker) stopMonitor() {
	t.stopOnce.Do(func() { close(t.stop) })
	<-t.done
}

func waitForEditorProcessTree(tracker *editorProcessTracker) error {
	if tracker == nil {
		return errors.New("managed editor process tree was not established; process ownership is unverified")
	}
	return tracker.waitForExit(10 * time.Second)
}

func recordEditorExitOutcome(path string, exitCode int, serverExitConfirmed bool, serverExitEvidence string) error {
	result, err := readEditorResult(path)
	if err != nil {
		return err
	}
	result.ClientExitCode = &exitCode
	result.ServerExitConfirmed = serverExitConfirmed
	result.ServerExitEvidence = serverExitEvidence
	result.CleanExit = result.ClientTestsCompleted && exitCode == 0 && serverExitConfirmed
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func readEditorResult(path string) (editorResult, error) {
	var result editorResult
	data, err := os.ReadFile(path)
	if err != nil {
		return result, fmt.Errorf("editor result file: %w", err)
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, fmt.Errorf("decode editor result: %w", err)
	}
	if result.Client == "" || result.Status == "" {
		return result, errors.New("editor result is missing client or status")
	}
	return result, nil
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve client acceptance source path")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func addCheck(r *report.Report, id string, status report.Status, summary string) {
	r.Checks = append(r.Checks, report.Check{ID: id, Status: status, Summary: summary})
}

func appendVSCodeHostFailure(r *report.Report, cases []editorCase, rows []editorCaseResult, detail string) {
	appendClientHostFailure(r, "vscode", cases, rows, detail)
}

func appendClientHostFailure(r *report.Report, client string, cases []editorCase, rows []editorCaseResult, detail string) {
	byName := make(map[string]editorCaseResult, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	host := client + " process"
	if client == "vscode" {
		host = "VS Code host"
	}
	completeRows := make([]editorCaseResult, 0, len(cases))
	for _, testCase := range cases {
		row, ok := byName[testCase.Name]
		if !ok {
			row = editorCaseResult{
				Name:       testCase.Name,
				LanguageID: testCase.LanguageID,
				Family:     testCase.Family,
				Status:     string(report.NotVerified),
				Reason:     "not executed before the " + host + " exited",
			}
		} else if row.Status == string(report.Running) {
			row.Status = string(report.NotVerified)
			if row.Error == "" && row.Reason == "" {
				row.Reason = "case was still running when the " + host + " exited"
			}
		}
		completeRows = append(completeRows, row)
	}
	appendClientChecks(r, client, cases, completeRows, "", "")
	addCheck(r, "client/"+client+"/host", report.Failed, detail)
	r.Errors = append(r.Errors, detail)
}

func recordNativeProcessFailure(r *report.Report, client, detail string) string {
	const keyPrefix = "nativeProcessError."
	const truncated = "\n[native process error truncated]\n"

	if r.Environment == nil {
		r.Environment = make(map[string]string)
	}
	if len(detail) > maxClientOutputBytes {
		keep := maxClientOutputBytes - len(truncated)
		detail = detail[:keep] + truncated
	}
	key := keyPrefix + client
	r.Environment[key] = detail
	return fmt.Sprintf("native %s process failed (batch %s); details in environment[%q]", client, r.RunID, key)
}

func appendClientChecks(r *report.Report, client string, cases []editorCase, rows []editorCaseResult, fallback report.Status, fallbackReason string) {
	appendFamilyChecks(r, client, cases, rows, fallback, fallbackReason)
	appendClientMatrixChecks(r, client, cases, rows, fallback, fallbackReason)
}

func appendClientMatrixChecks(r *report.Report, client string, cases []editorCase, rows []editorCaseResult, fallback report.Status, fallbackReason string) {
	existing := make(map[string]struct{}, len(r.Checks))
	for _, check := range r.Checks {
		existing[check.ID] = struct{}{}
	}
	byName := make(map[string]editorCaseResult, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	caseByName := make(map[string]editorCase, len(cases))
	for _, testCase := range cases {
		caseByName[testCase.Name] = testCase
	}

	for _, language := range clientMatrixLanguages {
		status := report.Passed
		summaries := make([]map[string]any, 0, len(language.CaseNames))
		var problems []string
		for _, caseName := range language.CaseNames {
			testCase, manifestOK := caseByName[caseName]
			entry := map[string]any{"name": caseName, "languageId": caseName}
			if manifestOK {
				entry["languageId"] = testCase.LanguageID
			}
			if fallback != "" {
				entry["status"] = string(fallback)
				entry["reason"] = fallbackReason
				problems = append(problems, caseName+": "+fallbackReason)
				if fallback == report.Failed {
					status = report.Failed
				} else if fallback == report.NotVerified && status == report.Passed {
					status = report.NotVerified
				}
				summaries = append(summaries, entry)
				continue
			}
			if !manifestOK {
				entry["status"] = string(report.Failed)
				entry["reason"] = "client matrix case is missing from the fixed editor manifest"
				status = report.Failed
				problems = append(problems, caseName+": missing editor case")
				summaries = append(summaries, entry)
				continue
			}
			row, ok := byName[caseName]
			if !ok {
				entry["status"] = string(report.Failed)
				entry["reason"] = "client omitted this language subcase"
				status = report.Failed
				problems = append(problems, caseName+": missing result row")
				summaries = append(summaries, entry)
				continue
			}
			entry["status"] = row.Status
			if row.Diagnostic != nil {
				entry["diagnostic"] = row.Diagnostic
			}
			if row.Observed != nil {
				entry["observed"] = row.Observed
			}
			reason := row.Error
			if reason == "" {
				reason = row.Reason
			}
			if reason != "" {
				entry["reason"] = reason
			}
			switch row.Status {
			case string(report.Passed):
			case string(report.NotVerified):
				if status == report.Passed {
					status = report.NotVerified
				}
				problems = append(problems, caseName+": "+reason)
			case string(report.Failed):
				status = report.Failed
				problems = append(problems, caseName+": "+reason)
			default:
				status = report.Failed
				problems = append(problems, caseName+": invalid status "+row.Status)
			}
			summaries = append(summaries, entry)
		}
		if len(summaries) == 0 {
			continue
		}
		// The legacy family summaries already occupy the overlapping Go, C++,
		// Rust, Python, and TypeScript IDs. They are also valid matrix cells;
		// keep one report row per fixed ID while adding the split C and
		// JavaScript cells below.
		id := "client/" + client + "/" + language.ID
		if _, ok := existing[id]; ok {
			continue
		}
		summary := "all language subcases passed"
		if len(problems) > 0 {
			summary = strings.Join(problems, "; ")
		}
		r.Checks = append(r.Checks, report.Check{
			ID:      id,
			Status:  status,
			Summary: summary,
			Observed: map[string]any{
				"subcase_names": append([]string(nil), language.CaseNames...),
				"subcases":      summaries,
			},
		})
	}
}

func appendFamilyChecks(r *report.Report, client string, cases []editorCase, rows []editorCaseResult, fallback report.Status, fallbackReason string) {
	byName := make(map[string]editorCaseResult, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	families := []string{"go", "cpp", "rust", "python", "typescript"}
	for _, family := range families {
		status := report.Passed
		summaries := make([]map[string]any, 0)
		subcaseNames := make([]string, 0)
		var problems []string
		for _, testCase := range cases {
			if testCase.Family != family {
				continue
			}
			subcaseNames = append(subcaseNames, testCase.Name)
			row, ok := byName[testCase.Name]
			entry := map[string]any{"name": testCase.Name, "languageId": testCase.LanguageID}
			if fallback != "" {
				entry["status"] = string(fallback)
				entry["reason"] = fallbackReason
				problems = append(problems, testCase.Name+": "+fallbackReason)
				if fallback == report.Failed {
					status = report.Failed
				} else if fallback == report.NotVerified && status == report.Passed {
					status = report.NotVerified
				}
				summaries = append(summaries, entry)
				continue
			}
			if !ok {
				entry["status"] = string(report.Failed)
				entry["reason"] = "client omitted this language subcase"
				status = report.Failed
				problems = append(problems, testCase.Name+": missing result row")
				summaries = append(summaries, entry)
				continue
			}
			entry["status"] = row.Status
			if row.Diagnostic != nil {
				entry["diagnostic"] = row.Diagnostic
			}
			if row.Observed != nil {
				entry["observed"] = row.Observed
			}
			reason := row.Error
			if reason == "" {
				reason = row.Reason
			}
			if reason != "" {
				entry["reason"] = reason
			}
			switch row.Status {
			case string(report.Passed):
			case string(report.NotVerified):
				if status == report.Passed {
					status = report.NotVerified
				}
				problems = append(problems, testCase.Name+": "+reason)
			case string(report.Failed):
				status = report.Failed
				problems = append(problems, testCase.Name+": "+reason)
			default:
				status = report.Failed
				problems = append(problems, testCase.Name+": invalid status "+row.Status)
			}
			summaries = append(summaries, entry)
		}
		if len(summaries) == 0 {
			continue
		}
		sort.Strings(subcaseNames)
		cppDiagnosticsVerified := family == "cpp" && fallback == ""
		cppDiagnosticCaseCount := 0
		if cppDiagnosticsVerified {
			for _, testCase := range cases {
				if testCase.Family != family {
					continue
				}
				cppDiagnosticCaseCount++
				row, ok := byName[testCase.Name]
				if !ok || !hasCleanCPPDiagnosticEvidence(row) {
					cppDiagnosticsVerified = false
					break
				}
			}
			cppDiagnosticsVerified = cppDiagnosticsVerified && cppDiagnosticCaseCount > 0
		}
		summary := "all language/filetype subcases passed"
		if len(problems) > 0 {
			summary = strings.Join(problems, "; ")
		}
		id := "client/" + client + "/" + family
		observed := map[string]any{"subcase_names": subcaseNames, "subcases": summaries}
		if family == "cpp" && fallback == "" && !cppDiagnosticsVerified && status == report.Passed {
			status = report.Failed
			summary = "C/C++ diagnostic evidence is missing its clean-source/deferred-capability scope"
			problems = append(problems, summary)
		}
		if cppDiagnosticsVerified {
			observed["diagnostic_scope"] = "clean_source_no_false_positive"
			observed["deferred_capability"] = "DEF-CCLSDIAG"
		}
		r.Checks = append(r.Checks, report.Check{ID: id, Status: status, Summary: summary, Observed: observed})
		if status == report.Failed {
			r.Errors = append(r.Errors, id+": "+summary)
		} else if status == report.NotVerified {
			r.Skips = append(r.Skips, report.Skip{ID: id, Reason: summary})
		}
	}
}

func hasCleanCPPDiagnosticEvidence(row editorCaseResult) bool {
	if row.Status != string(report.Passed) {
		return false
	}
	for _, evidence := range []map[string]any{row.Diagnostic, row.Observed} {
		if evidence["diagnostic_scope"] == "clean_source_no_false_positive" &&
			evidence["deferred_capability"] == "DEF-CCLSDIAG" &&
			fmt.Sprint(evidence["published_count"]) == "0" {
			return true
		}
	}
	return false
}

func parseClientOnly(value string) (string, error) {
	client := strings.ToLower(strings.TrimSpace(value))
	client = strings.ReplaceAll(client, "_", "-")
	switch client {
	case "":
		return "", nil
	case "vscode", "vs code", "vs-code", "visual-studio-code":
		return "vscode", nil
	case "neovim", "nvim":
		return "neovim", nil
	case "emacs", "eglot", "emacs/eglot", "emacs / eglot", "emacs-eglot":
		return "emacs-eglot", nil
	case "helix":
		return "helix", nil
	case "zed":
		return "zed", nil
	case "sublime", "sublime-lsp", "sublime lsp":
		return "sublime-lsp", nil
	default:
		return "", fmt.Errorf("unknown client %q; choose vscode, neovim, emacs-eglot, helix, zed, or sublime-lsp", value)
	}
}

func clientToolName(client string) string {
	switch client {
	case "vscode":
		return "vscode"
	case "neovim":
		return "neovim"
	case "emacs-eglot":
		return "emacs"
	default:
		return ""
	}
}

func clientMatrixCheckIDs() []string {
	ids := make([]string, 0, len(clientMatrixClients)*len(clientMatrixLanguages))
	for _, client := range clientMatrixClients {
		for _, language := range clientMatrixLanguages {
			ids = append(ids, "client/"+client.ID+"/"+language.ID)
		}
	}
	return ids
}

func validateClientMatrixContract(checks []report.Check) error {
	return validateClientMatrixContractExcept(checks, "")
}

func validateFormalEmacsMatrixContract(checks []report.Check) error {
	return validateClientMatrixContractExcept(checks, "emacs-eglot")
}

func validateClientMatrixContractExcept(checks []report.Check, nativeException string) error {
	actual := make(map[string]report.Check, len(checks))
	for _, check := range checks {
		if _, exists := actual[check.ID]; exists {
			return fmt.Errorf("client matrix repeats check %q", check.ID)
		}
		actual[check.ID] = check
	}
	for _, id := range clientMatrixCheckIDs() {
		check, exists := actual[id]
		if !exists {
			return fmt.Errorf("client matrix is missing required cell %q", id)
		}
		switch check.Status {
		case report.Passed, report.Failed, report.NotVerified:
		default:
			return fmt.Errorf("client matrix cell %q has invalid status %q", id, check.Status)
		}
	}
	for _, client := range clientMatrixClients {
		if client.Native || client.ID == nativeException {
			continue
		}
		for _, language := range clientMatrixLanguages {
			id := "client/" + client.ID + "/" + language.ID
			if actual[id].Status != report.NotVerified {
				return fmt.Errorf("client matrix cell %q must remain not_verified until its native driver exists", id)
			}
		}
	}
	return nil
}

func requireClientMatrixPassed(checks []report.Check, client string) error {
	actual := make(map[string]report.Check, len(checks))
	for _, check := range checks {
		actual[check.ID] = check
	}
	for _, language := range clientMatrixLanguages {
		id := "client/" + client + "/" + language.ID
		check, exists := actual[id]
		if !exists {
			return fmt.Errorf("formal %s matrix is missing %s", client, id)
		}
		if check.Status != report.Passed {
			return fmt.Errorf("formal %s matrix check %s is %s: %s", client, id, check.Status, check.Summary)
		}
	}
	return nil
}

func requireFormalEmacsOutcome(r report.Report) error {
	if err := requireClientMatrixPassed(r.Checks, "emacs-eglot"); err != nil {
		return err
	}
	if r.Decision == report.Failed || r.Decision == report.Running {
		return fmt.Errorf("formal Emacs acceptance decision is %s", r.Decision)
	}
	return nil
}

func validateEditorCaseManifest(cases []editorCase) error {
	if len(cases) != len(expectedEditorCaseManifest) {
		return fmt.Errorf("editor case manifest has %d entries, want exactly %d", len(cases), len(expectedEditorCaseManifest))
	}
	got := make(map[string]editorCase, len(cases))
	for _, testCase := range cases {
		if _, exists := got[testCase.Name]; exists {
			return fmt.Errorf("editor case manifest repeats name %q", testCase.Name)
		}
		got[testCase.Name] = testCase
	}
	for _, expected := range expectedEditorCaseManifest {
		actual, exists := got[expected.Name]
		if !exists {
			return fmt.Errorf("editor case manifest is missing %q (languageId %q)", expected.Name, expected.LanguageID)
		}
		if actual.LanguageID != expected.LanguageID || actual.Family != expected.Family {
			return fmt.Errorf("editor case %q is languageId=%q family=%q, want languageId=%q family=%q", expected.Name, actual.LanguageID, actual.Family, expected.LanguageID, expected.Family)
		}
	}
	return nil
}

func block(r *report.Report, id, reason string) {
	addCheck(r, id, report.NotVerified, reason)
	r.Skips = append(r.Skips, report.Skip{ID: id, Reason: reason})
}

func firstLine(value string) string {
	if i := strings.IndexAny(value, "\r\n"); i >= 0 {
		return value[:i]
	}
	return value
}

func isWithin(root, candidate string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return false
	}
	return true
}

func executableName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func mergeEnv(base []string, overrides map[string]string) []string {
	values := make(map[string]string, len(base)+len(overrides))
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(key)] = key + "=" + value
		}
	}
	for key, value := range overrides {
		values[strings.ToUpper(key)] = key + "=" + value
	}
	result := make([]string, 0, len(values))
	for _, entry := range values {
		result = append(result, entry)
	}
	return result
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func gitRevision(root string) (string, error) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(out)), err
}

func findVSCode() (string, error) {
	if configured := os.Getenv("OMNILSP_VSCODE_BIN"); configured != "" {
		return normalizeVSCodeExecutable(configured)
	}
	path, err := exec.LookPath("code")
	if err != nil {
		return "", fmt.Errorf("VS Code CLI is missing: %w", err)
	}
	return normalizeVSCodeExecutable(path)
}

func readVSCodePackageVersion(executable string) (string, error) {
	installRoot := filepath.Dir(executable)
	candidates := []string{filepath.Join(installRoot, "resources", "app", "package.json")}
	entries, err := os.ReadDir(installRoot)
	if err != nil {
		return "", fmt.Errorf("read VS Code install directory: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidates = append(candidates, filepath.Join(installRoot, entry.Name(), "resources", "app", "package.json"))
	}
	existing := make([]string, 0, 1)
	for _, candidate := range candidates {
		info, statErr := os.Stat(candidate)
		if statErr == nil && !info.IsDir() {
			existing = append(existing, candidate)
		}
	}
	if len(existing) != 1 {
		return "", fmt.Errorf("expected one VS Code resources/app/package.json under %q; found %d", installRoot, len(existing))
	}
	data, err := os.ReadFile(existing[0])
	if err != nil {
		return "", fmt.Errorf("read VS Code package metadata: %w", err)
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", fmt.Errorf("decode VS Code package metadata: %w", err)
	}
	if strings.TrimSpace(metadata.Version) == "" {
		return "", errors.New("VS Code package metadata has no version")
	}
	return strings.TrimSpace(metadata.Version), nil
}

func normalizeVSCodeExecutable(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(filepath.Ext(abs), ".cmd") || strings.EqualFold(filepath.Ext(abs), ".bat") {
		candidate := filepath.Join(filepath.Dir(filepath.Dir(abs)), "Code.exe")
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate, nil
		}
		return "", fmt.Errorf("VS Code shim %q has no neighboring Code.exe", abs)
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("VS Code executable is unavailable: %s", abs)
	}
	return abs, nil
}

func fileURI(path string) string {
	abs, _ := filepath.Abs(path)
	uriPath := filepath.ToSlash(abs)
	if volume := filepath.VolumeName(abs); len(volume) == 2 && volume[1] == ':' {
		uriPath = "/" + uriPath
	}
	return (&url.URL{Scheme: "file", Path: uriPath}).String()
}

func writeWorkspace(root, serverBin string) ([]editorCase, string, error) {
	files := map[string]string{
		"go-case/go.mod":                "module acceptance/go\n\ngo 1.26\n",
		"go-case/fixture.go":            "package acceptance\n\nfunc SoakTarget(value int) int { return value + 1 }\nfunc UseTarget() int { return SoakTarget(1) }\nfunc privateTarget(value int) int { return value }\nfunc usePrivateTarget() int { return privateTarget(1) }\n",
		"c-case/fixture.c":              "int SoakTarget(int value) { return value + 1; }\nint UseTarget(void) { return SoakTarget(1); }\nstatic int privateTarget(int value) { return value; }\nint usePrivateTarget(void) { return privateTarget(1); }\n",
		"cpp-case/fixture.cpp":          "int SoakTarget(int value) { return value + 1; }\nint UseTarget() { return SoakTarget(1); }\nstatic int privateTarget(int value) { return value; }\nint usePrivateTarget() { return privateTarget(1); }\n",
		"Cargo.toml":                    "[workspace]\nmembers = [\"rust-case\"]\nresolver = \"2\"\n",
		"rust-case/Cargo.toml":          "[package]\nname = \"acceptance_rust\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"rust-case/src/lib.rs":          "pub fn SoakTarget(value: i32) -> i32 { value + 1 }\npub fn use_target() -> i32 { SoakTarget(1) }\nfn privateTarget(value: i32) -> i32 { value }\nfn use_private_target() -> i32 { privateTarget(1) }\n",
		"pyproject.toml":                "[project]\nname = \"omnilsp-acceptance\"\nversion = \"0.1.0\"\n",
		"pyrightconfig.json":            "{\"include\":[\"python-case\"],\"pythonVersion\":\"3.11\"}\n",
		"python-case/fixture.py":        "def SoakTarget(value: int) -> int:\n    return value + 1\n\ndef use_target() -> int:\n    return SoakTarget(1)\n\ndef privateTarget(value: int) -> int:\n    return value\n\ndef use_private_target() -> int:\n    return privateTarget(1)\n",
		"tsconfig.json":                 "{\"compilerOptions\":{\"target\":\"ES2020\",\"module\":\"commonjs\",\"strict\":true,\"jsx\":\"preserve\"},\"include\":[\"typescript-case/**/*.ts\",\"typescript-case/**/*.tsx\"]}\n",
		"typescript-case/jsx.d.ts":      "declare namespace JSX { interface IntrinsicElements { span: any; } }\n",
		"typescript-case/fixture.ts":    "export function SoakTarget(value: number): number { return value + 1; }\nexport function useTarget(): number { return SoakTarget(1); }\n",
		"typescript-case/component.tsx": "export function SoakTarget(value: number) { return <span>{value}</span>; }\nexport function useComponent() { usePrivateTarget(); return SoakTarget(1); }\nfunction privateTarget(value: number) { return value; }\nfunction usePrivateTarget() { return privateTarget(1); }\n",
		"javascript-case/jsconfig.json": "{\"compilerOptions\":{\"target\":\"ES2020\",\"module\":\"commonjs\",\"strict\":false,\"checkJs\":true,\"jsx\":\"preserve\"},\"include\":[\"**/*.js\",\"**/*.jsx\",\"**/*.d.ts\"]}\n",
		"javascript-case/jsx.d.ts":      "declare namespace JSX { interface IntrinsicElements { span: any; } }\n",
		"javascript-case/fixture.js":    "export function SoakTarget() { return 2; }\nexport function useTarget() { usePrivateTarget(); return SoakTarget(); }\nfunction privateTarget() { return 1; }\nfunction usePrivateTarget() { return privateTarget(); }\n",
		"javascript-case/component.jsx": "export function SoakTarget() { return <span>client</span>; }\nexport function useComponent() { usePrivateTarget(); return SoakTarget(); }\nfunction privateTarget() { return 1; }\nfunction usePrivateTarget() { return privateTarget(); }\n",
	}
	cases := []editorCase{
		{Name: "go", File: "go-case/fixture.go", LanguageID: "go", Family: "go", Symbol: "SoakTarget", BackendLanguage: "go", RequiredTools: []string{"go"}},
		{Name: "c", File: "c-case/fixture.c", LanguageID: "c", Family: "cpp", Symbol: "SoakTarget", BackendLanguage: "cpp", RequiredTools: []string{"clangd"}},
		{Name: "cpp", File: "cpp-case/fixture.cpp", LanguageID: "cpp", Family: "cpp", Symbol: "SoakTarget", BackendLanguage: "cpp", RequiredTools: []string{"clangd"}},
		{Name: "rust", File: "rust-case/src/lib.rs", LanguageID: "rust", Family: "rust", Symbol: "SoakTarget", BackendLanguage: "rust", RequiredTools: []string{"rust-analyzer"}},
		{Name: "python", File: "python-case/fixture.py", LanguageID: "python", Family: "python", Symbol: "SoakTarget", BackendLanguage: "python", RequiredTools: []string{"python", "node", "pyright", "pyright-langserver"}},
		{Name: "typescript", File: "typescript-case/fixture.ts", LanguageID: "typescript", Family: "typescript", Symbol: "SoakTarget", BackendLanguage: "typescript", RequiredTools: []string{"node", "typescript-language-server", "typescript", "tsc"}},
		{Name: "typescriptreact", File: "typescript-case/component.tsx", LanguageID: "typescriptreact", Family: "typescript", Symbol: "SoakTarget", BackendLanguage: "typescript", RequiredTools: []string{"node", "typescript-language-server", "typescript", "tsc"}},
		{Name: "javascript", File: "javascript-case/fixture.js", LanguageID: "javascript", Family: "typescript", Symbol: "SoakTarget", BackendLanguage: "typescript", RequiredTools: []string{"node", "typescript-language-server", "typescript", "tsc"}},
		{Name: "javascriptreact", File: "javascript-case/component.jsx", LanguageID: "javascriptreact", Family: "typescript", Symbol: "SoakTarget", BackendLanguage: "typescript", RequiredTools: []string{"node", "typescript-language-server", "typescript", "tsc"}},
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return nil, "", err
		}
	}
	compileDB := []map[string]any{}
	for _, source := range []struct{ rel, compiler string }{{"c-case/fixture.c", "clang"}, {"cpp-case/fixture.cpp", "clang++"}} {
		file := filepath.Join(root, filepath.FromSlash(source.rel))
		compileDB = append(compileDB, map[string]any{"directory": root, "file": file, "arguments": []string{source.compiler, "-c", file}})
	}
	encodedDB, _ := json.Marshal(compileDB)
	if err := os.WriteFile(filepath.Join(root, "compile_commands.json"), append(encodedDB, '\n'), 0o644); err != nil {
		return nil, "", err
	}
	settings, _ := json.Marshal(map[string]any{"omnilsp.path": serverBin, "omnilsp.trace.server": "off"})
	settingsDir := filepath.Join(root, ".vscode")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), append(settings, '\n'), 0o644); err != nil {
		return nil, "", err
	}
	caseData, err := json.Marshal(cases)
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(filepath.Join(root, ".omnilsp-client-cases.json"), caseData, 0o644); err != nil {
		return nil, "", err
	}
	var corpus strings.Builder
	for _, name := range sortedKeys(files) {
		corpus.WriteString(name)
		corpus.WriteByte(0)
		corpus.WriteString(files[name])
		corpus.WriteByte(0)
	}
	corpus.Write(encodedDB)
	hash := sha256.Sum256([]byte(corpus.String()))
	return cases, hex.EncodeToString(hash[:]), nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}
