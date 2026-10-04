//go:build clients

package clients

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNativeSublimeGoDriver(t *testing.T) {
	if os.Getenv("OMNILSP_RUN_SUBLIME_NATIVE_GO") != "1" {
		t.Skip("set OMNILSP_RUN_SUBLIME_NATIVE_GO=1 to run the isolated native Sublime Go case")
	}
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		t.Skipf("the Sublime tool lock is pinned for windows/amd64; got %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	root := repoRoot(t)
	lock, err := readToolLock(root)
	if err != nil {
		t.Fatal(err)
	}
	serverBin, err := buildServer(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	serverHash, err := fileSHA256(serverBin)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("isolated native Sublime diagnostic candidate: %s (SHA-256 %s)", serverBin, serverHash)

	workspace := t.TempDir()
	if _, _, err := writeWorkspace(workspace, serverBin); err != nil {
		t.Fatal(err)
	}
	toolStatus, err := json.Marshal(map[string]bool{"go": true})
	if err != nil {
		t.Fatal(err)
	}
	env := makeClientEnv(root, workspace, serverBin, nil, lock)
	env["OMNILSP_CLIENT_CASE_ONLY"] = "go"
	env["OMNILSP_CLIENT_TOOL_STATUS"] = string(toolStatus)
	runID := strings.TrimSpace(os.Getenv("OMNILSP_RUN_ID"))
	if runID == "" {
		runID = fmt.Sprintf("sublime-go-%s-%d", time.Now().UTC().Format("20060102T150405Z"), os.Getpid())
	}
	for _, char := range runID {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_') {
			t.Fatalf("OMNILSP_RUN_ID %q contains a character unsafe for the Sublime evidence path", runID)
		}
	}
	evidenceDir := filepath.Join(root, "test", "acceptance", "evidence", runID)
	if acceptanceReport := strings.TrimSpace(os.Getenv("OMNILSP_ACCEPTANCE_REPORT")); acceptanceReport != "" {
		evidenceDir = filepath.Dir(acceptanceReport)
	}
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		t.Fatalf("create Sublime Go evidence directory: %v", err)
	}
	resultPath := filepath.Join(evidenceDir, fmt.Sprintf("sublime-go-%s-%s.json", runID, serverHash))
	if _, err := os.Stat(resultPath); err == nil {
		t.Fatalf("Sublime Go evidence already exists at %q; choose a new OMNILSP_RUN_ID", resultPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("inspect Sublime Go evidence path %q: %v", resultPath, err)
	}
	env["OMNILSP_SUBLIME_GO_RUN_ID"] = runID
	env["OMNILSP_SUBLIME_GO_CANDIDATE_SHA256"] = serverHash
	if err := runSublimeLSP(root, workspace, resultPath, env, lock); err != nil {
		result, resultErr := readEditorResult(resultPath)
		t.Fatalf("native Sublime Go driver failed: %v (result=%#v, result read error=%v; evidence=%s)", err, result, resultErr, resultPath)
	}
	result, err := readEditorResult(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var evidenceIdentity struct {
		RunID           string `json:"run_id"`
		CandidateSHA256 string `json:"candidate_sha256"`
	}
	evidenceBytes, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read persisted Sublime Go evidence %q: %v", resultPath, err)
	}
	if err := json.Unmarshal(evidenceBytes, &evidenceIdentity); err != nil {
		t.Fatalf("decode persisted Sublime Go evidence identity %q: %v", resultPath, err)
	}
	var goCase *editorCaseResult
	if len(result.Cases) != len(expectedEditorCaseManifest) {
		t.Fatalf("case-only Sublime run returned %d rows; want all %d manifest rows", len(result.Cases), len(expectedEditorCaseManifest))
	}
	goCaseCount := 0
	for index := range result.Cases {
		caseResult := &result.Cases[index]
		if caseResult.Name == "go" {
			goCaseCount++
			goCase = &result.Cases[index]
			continue
		}
		if caseResult.Status != "not_verified" || !strings.Contains(caseResult.Reason, "OMNILSP_CLIENT_CASE_ONLY=go") {
			t.Errorf("unexecuted Sublime case %q must remain not_verified with the case-only reason, got %#v", caseResult.Name, *caseResult)
		}
	}
	if result.Client != "sublime-lsp" || result.Status != "not_verified" || !result.ClientTestsCompleted || !result.CleanExit || !result.ServerExitConfirmed {
		t.Fatalf("native Sublime shutdown or client result is incomplete: result=%#v; run identity=%#v want run %q candidate %s",
			result, evidenceIdentity, runID, serverHash)
	}
	if goCaseCount != 1 || goCase == nil || goCase.Status != "passed" {
		t.Fatalf("native Sublime Go case did not pass: case=%#v; run identity=%#v want run %q candidate %s",
			goCase, evidenceIdentity, runID, serverHash)
	}
	if evidenceIdentity.RunID != runID || !strings.EqualFold(evidenceIdentity.CandidateSHA256, serverHash) {
		t.Fatalf("persisted Sublime Go evidence identity mismatch at %q: got %#v, want run %q candidate %s (native case=%#v)",
			resultPath, evidenceIdentity, runID, serverHash, goCase)
	}
	for _, field := range []string{"hover", "completion", "definition", "references", "rename_refusal", "diagnostic_scope"} {
		if _, ok := goCase.Observed[field]; !ok {
			t.Errorf("native Sublime Go result is missing %q evidence: %#v", field, goCase.Observed)
		}
	}
	completion, ok := goCase.Observed["completion"].(map[string]any)
	if !ok || completion["clientConsumptionApi"] != "sublime.CompletionList.set_completions" ||
		completion["completionListTargetBound"] != true || completion["completionPopupVisible"] != true {
		t.Fatalf("native Sublime Go completion did not reach the bound API and visible popup: %#v", goCase.Observed["completion"])
	}
	candidate, _ := completion["candidate"].(string)
	responseItem, _ := completion["responseItem"].(map[string]any)
	responseLabel, _ := responseItem["label"].(string)
	deliveredItem, _ := completion["deliveredItem"].(map[string]any)
	deliveredTrigger, _ := deliveredItem["trigger"].(string)
	annotation, _ := deliveredItem["annotation"].(string)
	if candidate == "" || responseLabel != candidate ||
		strings.TrimSuffix(deliveredTrigger, "\t"+annotation) != candidate {
		t.Fatalf("native Sublime Go completion consumer evidence lost the response candidate: %#v", completion)
	}
	hoverConsumer, ok := goCase.Observed["hoverConsumer"].(map[string]any)
	hoverContent, _ := hoverConsumer["renderedContent"].(string)
	if !ok || hoverConsumer["api"] != "LSP.plugin.hover.show_lsp_popup" ||
		hoverConsumer["popupVisible"] != true || hoverConsumer["contentMatchesCandidate"] != true ||
		!strings.Contains(hoverContent, "SoakTarget") {
		t.Fatalf("native Sublime Go hover did not reach its matching visible popup consumer: %#v", goCase.Observed)
	}
	if definition, ok := goCase.Observed["definition"].(map[string]any); !ok ||
		definition["clientConsumerApi"] != "LSP.plugin.locationpicker.open_location_async via Window.active_view" {
		t.Fatalf("native Sublime Go definition did not reach its navigation consumer: %#v", goCase.Observed["definition"])
	}
	if references, ok := goCase.Observed["references"].(map[string]any); !ok ||
		references["clientConsumerApi"] != "LSP references output.references panel" || references["renderedFixtureLines"] != float64(2) {
		t.Fatalf("native Sublime Go references did not reach the output panel consumer: %#v", goCase.Observed["references"])
	}
	if sync, ok := goCase.Observed["buffer_edit_sync"].(map[string]any); !ok || sync["notification"] != "textDocument/didChange" {
		t.Fatalf("native Sublime Go buffer edit lacks didChange sync evidence: %#v", goCase.Observed["buffer_edit_sync"])
	}
}

func runEmacsEglot(root, workspace, resultPath string, env map[string]string, lock toolLock) error {
	emacs, err := verifyLockedBinary(lock, "emacs")
	if err != nil {
		return err
	}
	if configured := strings.TrimSpace(os.Getenv("OMNILSP_EMACS_BIN")); configured != "" && !sameExecutablePath(configured, emacs) {
		return fmt.Errorf("OMNILSP_EMACS_BIN %q does not match locked Emacs path %q", configured, emacs)
	}
	profile, err := os.MkdirTemp("", "omnilsp-emacs-profile-")
	if err != nil {
		return fmt.Errorf("create isolated Emacs profile: %w", err)
	}
	if strings.TrimSpace(os.Getenv("OMNILSP_EMACS_TRACE")) == "" {
		defer os.RemoveAll(profile)
	}
	clientEnv := copyClientEnv(env)
	clientEnv["HOME"] = profile
	clientEnv["APPDATA"] = filepath.Join(profile, "AppData", "Roaming")
	clientEnv["LOCALAPPDATA"] = filepath.Join(profile, "AppData", "Local")
	clientEnv["OMNILSP_CLIENT_RESULT"] = resultPath
	args := []string{
		"--batch", "-Q",
		"--load", filepath.Join(root, "test", "acceptance", "clients", "emacs", "runner.el"),
	}
	return runNativeEditorProcess("emacs-eglot", emacs, args, workspace, resultPath, clientEnv)
}

func runSublimeLSP(root, workspace, resultPath string, env map[string]string, lock toolLock) (finalErr error) {
	sublime, err := verifyLockedBinary(lock, "sublime-text")
	if err != nil {
		return err
	}
	if configured := strings.TrimSpace(os.Getenv("OMNILSP_SUBLIME_BIN")); configured != "" && !sameExecutablePath(configured, sublime) {
		return fmt.Errorf("OMNILSP_SUBLIME_BIN %q does not match locked Sublime Text path %q", configured, sublime)
	}
	lspPackage, err := verifyLockedBinary(lock, "sublime-lsp-package")
	if err != nil {
		return err
	}
	if configured := strings.TrimSpace(os.Getenv("OMNILSP_SUBLIME_LSP_PACKAGE")); configured != "" && !sameExecutablePath(configured, lspPackage) {
		return fmt.Errorf("OMNILSP_SUBLIME_LSP_PACKAGE %q does not match locked package path %q", configured, lspPackage)
	}
	if err := ensureNoSublimeTextProcess(); err != nil {
		return err
	}
	wheels, err := sublimeLSPRuntimeWheels(lock)
	if err != nil {
		return err
	}

	dataDir, err := sublimePortableDataDir(root, sublime)
	if err != nil {
		return err
	}
	launchAttempted := false
	defer func() {
		if strings.TrimSpace(os.Getenv("OMNILSP_SUBLIME_TRACE")) != "" {
			return
		}
		if err := ensureNoSublimeTextProcess(); err != nil {
			finalErr = errors.Join(finalErr, fmt.Errorf("preserved the isolated portable Sublime Data directory: %w", err))
			return
		}
		if launchAttempted && finalErr != nil {
			var processErr *nativeEditorProcessError
			if !errors.As(finalErr, &processErr) || !processErr.processTreeExited {
				finalErr = errors.Join(finalErr, errors.New("preserved the isolated portable Sublime Data directory because process-tree exit was not verified"))
				return
			}
		}
		if cleanupErr := cleanSublimePortableData(dataDir); cleanupErr != nil {
			finalErr = errors.Join(finalErr, cleanupErr)
		}
	}()
	if err := installSublimeAcceptanceProfile(root, dataDir, lspPackage, env["OMNILSP_BIN"],
		env["OMNILSP_SUBLIME_GO_RUN_ID"], env["OMNILSP_SUBLIME_GO_CANDIDATE_SHA256"], wheels); err != nil {
		return err
	}
	if err := ensureNoSublimeTextProcess(); err != nil {
		return err
	}
	clientEnv := copyClientEnv(env)
	clientEnv["OMNILSP_CLIENT_RESULT"] = resultPath
	clientEnv["OMNILSP_SUBLIME_DATA"] = dataDir
	args := []string{"--new-window", workspace}
	launchAttempted = true
	return runNativeEditorProcess("sublime-lsp", sublime, args, workspace, resultPath, clientEnv)
}

func ensureNoSublimeTextProcess() error {
	command := "$ErrorActionPreference = 'Stop'; $found = Get-Process | Where-Object { $_.ProcessName -ieq 'sublime_text' }; if ($found) { $found | ForEach-Object { $_.Id }; exit 17 }"
	output, err := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 17 {
			return fmt.Errorf("an existing Sublime Text process is running (PID output %q)", strings.TrimSpace(string(output)))
		}
		return fmt.Errorf("verify no Sublime Text process is running: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) != "" {
		return fmt.Errorf("Sublime Text process check returned unexpected output: %q", strings.TrimSpace(string(output)))
	}
	return nil
}

func sublimePortableDataDir(root, sublimeBinary string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository root for portable Sublime profile: %w", err)
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", fmt.Errorf("resolve repository root for portable Sublime profile: %w", err)
	}
	sublimeReal, err := filepath.EvalSymlinks(sublimeBinary)
	if err != nil {
		return "", fmt.Errorf("resolve locked Sublime executable for portable profile: %w", err)
	}
	dataDir := filepath.Join(filepath.Dir(sublimeReal), "Data")
	dataInfo, err := os.Lstat(dataDir)
	if err != nil {
		return "", fmt.Errorf("inspect portable Sublime Data directory: %w", err)
	}
	if !dataInfo.IsDir() || dataInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("portable Sublime Data path is not a real directory: %q", dataDir)
	}
	dataReal, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve portable Sublime Data directory: %w", err)
	}
	if !strings.EqualFold(filepath.Clean(dataReal), filepath.Clean(dataDir)) {
		return "", fmt.Errorf("portable Sublime Data directory resolves outside its adjacent path: %q", dataDir)
	}
	relative, err := filepath.Rel(rootReal, dataReal)
	if err != nil || relative == "." || !filepath.IsLocal(relative) {
		return "", fmt.Errorf("portable Sublime Data directory is outside the repository: %q", dataReal)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return "", fmt.Errorf("inspect portable Sublime Data baseline: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != "KEEPME" {
		return "", fmt.Errorf("portable Sublime Data baseline must contain only KEEPME; found %v", sublimeEntryNames(entries))
	}
	sentinel := filepath.Join(dataDir, "KEEPME")
	sentinelInfo, err := os.Lstat(sentinel)
	if err != nil {
		return "", fmt.Errorf("inspect portable Sublime Data sentinel: %w", err)
	}
	if !sentinelInfo.Mode().IsRegular() || sentinelInfo.Size() != 0 {
		return "", fmt.Errorf("portable Sublime Data sentinel must be a zero-byte regular file: %q", sentinel)
	}
	return dataDir, nil
}

func cleanSublimePortableData(dataDir string) error {
	dataInfo, err := os.Lstat(dataDir)
	if err != nil {
		return fmt.Errorf("inspect portable Sublime Data directory before cleanup: %w", err)
	}
	if !dataInfo.IsDir() || dataInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to clean a replaced portable Sublime Data path: %q", dataDir)
	}
	dataReal, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return fmt.Errorf("resolve portable Sublime Data directory before cleanup: %w", err)
	}
	if !strings.EqualFold(filepath.Clean(dataReal), filepath.Clean(dataDir)) {
		return fmt.Errorf("refusing to clean redirected portable Sublime Data path: %q", dataDir)
	}
	sentinel := filepath.Join(dataDir, "KEEPME")
	sentinelInfo, err := os.Lstat(sentinel)
	if err != nil {
		return fmt.Errorf("preserve portable Sublime Data sentinel: %w", err)
	}
	if !sentinelInfo.Mode().IsRegular() || sentinelInfo.Size() != 0 {
		return fmt.Errorf("preserve portable Sublime Data sentinel with unexpected type or contents: %q", sentinel)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("inspect portable Sublime Data after run: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == "KEEPME" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dataDir, entry.Name())); err != nil {
			return fmt.Errorf("remove test-created portable Sublime Data entry %q: %w", entry.Name(), err)
		}
	}
	entries, err = os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("verify portable Sublime Data cleanup: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != "KEEPME" {
		return fmt.Errorf("portable Sublime Data cleanup did not restore KEEPME-only baseline; found %v", sublimeEntryNames(entries))
	}
	return nil
}

func sublimeEntryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestSublimePortableDataBaselineAndCleanup(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "tools", "sublime")
	dataDir := filepath.Join(appDir, "Data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(appDir, "sublime_text.exe")
	if err := os.WriteFile(binary, []byte("test executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dataDir, "KEEPME")
	if err := os.WriteFile(sentinel, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := sublimePortableDataDir(root, binary)
	if err != nil {
		t.Fatal(err)
	}
	if got != dataDir {
		t.Fatalf("portable Data path = %q, want %q", got, dataDir)
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "Packages", "omnilsp_acceptance"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "Packages", "omnilsp_acceptance", "driver.py"), []byte("test profile"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanSublimePortableData(dataDir); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "KEEPME" {
		t.Fatalf("portable Data cleanup entries = %v, want [KEEPME]", sublimeEntryNames(entries))
	}
	if _, err := os.Lstat(sentinel); err != nil {
		t.Fatalf("KEEPME sentinel was not preserved: %v", err)
	}
}

func TestSublimePortableDataRejectsUnexpectedBaseline(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "tools", "sublime")
	dataDir := filepath.Join(appDir, "Data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(appDir, "sublime_text.exe")
	if err := os.WriteFile(binary, []byte("test executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "KEEPME"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unexpected := filepath.Join(dataDir, "user-settings.json")
	if err := os.WriteFile(unexpected, []byte("preserve me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := sublimePortableDataDir(root, binary); err == nil {
		t.Fatal("portable Data validation accepted a nonempty baseline")
	}
	if _, err := os.Stat(unexpected); err != nil {
		t.Fatalf("unexpected baseline content was changed: %v", err)
	}
}

type sublimeLSPTestArchiveEntry struct {
	name string
	data []byte
	mode os.FileMode
}

func writeSublimeLSPTestArchive(t *testing.T, entries []sublimeLSPTestArchiveEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sublime-lsp.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
			if strings.HasSuffix(entry.name, "/") {
				mode = os.ModeDir | 0o755
			}
		}
		header.SetMode(mode)
		output, err := archive.CreateHeader(header)
		if err != nil {
			archive.Close()
			file.Close()
			t.Fatal(err)
		}
		if _, err := output.Write(entry.data); err != nil {
			archive.Close()
			file.Close()
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractSublimeLSPPackageStripsPinnedArchiveRoot(t *testing.T) {
	archivePath := writeSublimeLSPTestArchive(t, []sublimeLSPTestArchiveEntry{
		{name: sublimeLSPArchivePrefix, mode: os.ModeDir | 0o755},
		{name: sublimeLSPArchivePrefix + "plugin/", mode: os.ModeDir | 0o755},
		{name: sublimeLSPArchivePrefix + "plugin/__init__.py", data: []byte("class LspPlugin: pass\n")},
		{name: sublimeLSPArchivePrefix + "protocol/__init__.py", data: []byte("\n")},
	})
	original, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	packagesDir := filepath.Join(t.TempDir(), "Packages")
	if err := os.MkdirAll(packagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(packagesDir, "LSP")
	if err := extractSublimeLSPPackage(archivePath, destination); err != nil {
		t.Fatal(err)
	}
	plugin, err := os.ReadFile(filepath.Join(destination, "plugin", "__init__.py"))
	if err != nil || string(plugin) != "class LspPlugin: pass\n" {
		t.Fatalf("extracted plugin init = %q, read error = %v", plugin, err)
	}
	if _, err := os.Lstat(filepath.Join(destination, "LSP-4070-2.13.0")); !os.IsNotExist(err) {
		t.Fatalf("versioned archive prefix was not stripped: %v", err)
	}
	got, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("extracting the pinned Sublime LSP archive modified the original archive")
	}
}

func TestPinnedSublimeLSPArchiveExtractsPackageRoot(t *testing.T) {
	root := repoRoot(t)
	lock, err := readToolLock(root)
	if err != nil {
		t.Fatal(err)
	}
	lockedArchive, ok := lock.ResolvedBinaries["sublime-lsp-package"]
	if !ok {
		t.Fatal("pinned tool lock is missing resolvedBinaries.sublime-lsp-package")
	}
	if _, err := os.Stat(lockedArchive.Path); os.IsNotExist(err) {
		t.Skip("pinned Sublime LSP archive is not installed on this host")
	} else if err != nil {
		t.Fatal(err)
	}
	archivePath, err := verifyLockedBinary(lock, "sublime-lsp-package")
	if err != nil {
		t.Fatal(err)
	}
	packagesDir := filepath.Join(t.TempDir(), "Packages")
	if err := os.MkdirAll(packagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(packagesDir, "LSP")
	if err := extractSublimeLSPPackage(archivePath, destination); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"plugin/__init__.py", "protocol/__init__.py"} {
		info, err := os.Stat(filepath.Join(destination, filepath.FromSlash(path)))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("extracted package root is missing %s: info=%v error=%v", path, info, err)
		}
	}
}

func TestSublimeAcceptanceSyntaxAssetsUseBasicVersion2Rules(t *testing.T) {
	root := repoRoot(t)
	syntaxes := map[string]string{
		"OmniLSP-Go.sublime-syntax":         "source.go",
		"OmniLSP-C.sublime-syntax":          "source.c",
		"OmniLSP-Cpp.sublime-syntax":        "source.c++",
		"OmniLSP-Rust.sublime-syntax":       "source.rust",
		"OmniLSP-Python.sublime-syntax":     "source.python",
		"OmniLSP-TypeScript.sublime-syntax": "source.ts",
		"OmniLSP-TSX.sublime-syntax":        "source.tsx",
		"OmniLSP-JavaScript.sublime-syntax": "source.js",
		"OmniLSP-JSX.sublime-syntax":        "source.jsx",
	}
	for name, scope := range syntaxes {
		path := filepath.Join(root, "test", "acceptance", "clients", "sublime", "syntaxes", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read Sublime syntax %s: %v", name, err)
		}
		source := string(data)
		if !strings.Contains(source, "%YAML 1.2\n---\n") ||
			!strings.Contains(source, "scope: "+scope+"\nversion: 2\n") ||
			!strings.Contains(source, "- match: '.+'\n      scope: "+scope+"\n") ||
			strings.Contains(source, "(?s)") {
			t.Errorf("Sublime syntax %s must use a version-2 header and a plain, non-empty catch-all rule for %s", name, scope)
		}
	}
}

func TestSublimeDriverUsesPinnedClientConfigsAPI(t *testing.T) {
	root, python := lockedSublimeDriverPython(t)
	lock, err := readToolLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := lock.ResolvedBinaries["sublime-lsp-package"]; !ok {
		t.Fatal("pinned tool lock is missing resolvedBinaries.sublime-lsp-package")
	}
	archivePath, err := verifyLockedBinary(lock, "sublime-lsp-package")
	if err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(root, "test", "acceptance", "clients", "sublime", "driver.py")
	const probe = `
import ast, sys, zipfile
archive_path, driver_path, prefix = sys.argv[1:4]
with zipfile.ZipFile(archive_path) as archive:
    source = archive.read(prefix + "plugin/core/settings.py").decode("utf-8")
upstream = ast.parse(source)
client_configs = next((node for node in upstream.body
                       if isinstance(node, ast.ClassDef) and node.name == "ClientConfigs"), None)
assert client_configs is not None, "pinned LSP archive has no ClientConfigs class"
assert not any(isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name == "get_config"
               for node in client_configs.body), "ClientConfigs unexpectedly gained get_config"
assert any(isinstance(node, ast.Attribute) and isinstance(node.value, ast.Name)
           and node.value.id == "self" and node.attr == "all"
           for node in ast.walk(client_configs)), "pinned ClientConfigs no longer exposes self.all"
assert any(isinstance(node, ast.Assign) and any(isinstance(target, ast.Name)
               and target.id == "client_configs" for target in node.targets)
               and isinstance(node.value, ast.Call) and isinstance(node.value.func, ast.Name)
               and node.value.func.id == "ClientConfigs" for node in upstream.body), \
       "pinned LSP archive does not instantiate client_configs from ClientConfigs"

with open(driver_path, encoding="utf-8") as stream:
    driver = ast.parse(stream.read())
def has_all_get(function):
    return any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
               and node.func.attr == "get" and isinstance(node.func.value, ast.Attribute)
               and node.func.value.attr == "all" and isinstance(node.func.value.value, ast.Name)
               and node.func.value.value.id == "client_configs" for node in ast.walk(function))
for function_name in ("_profile_registration_state", "_session_startup_state"):
    function = next((node for node in ast.walk(driver)
                     if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
                     and node.name == function_name), None)
    assert function is not None, "driver is missing " + function_name
    assert has_all_get(function), function_name + " does not use pinned client_configs.all mapping"
`
	command := exec.Command(python, "-I", "-B", "-c", probe, archivePath, driverPath, sublimeLSPArchivePrefix)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pinned Sublime LSP ClientConfigs API contract failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

func TestSublimeDriverUsesPinnedSyntaxLoadingAPI(t *testing.T) {
	root, python := lockedSublimeDriverPython(t)
	lock, err := readToolLock(root)
	if err != nil {
		t.Fatal(err)
	}
	lockedSublime, ok := lock.ResolvedBinaries["sublime-text"]
	if !ok {
		t.Fatal("pinned tool lock is missing resolvedBinaries.sublime-text")
	}
	if _, err := os.Stat(lockedSublime.Path); os.IsNotExist(err) {
		t.Skip("pinned Sublime Text installation is not present on this host")
	} else if err != nil {
		t.Fatal(err)
	}
	sublime, err := verifyLockedBinary(lock, "sublime-text")
	if err != nil {
		t.Fatal(err)
	}
	sublimeAPI := filepath.Join(filepath.Dir(sublime), "Lib", "python314", "sublime.py")
	if _, err := os.Stat(sublimeAPI); os.IsNotExist(err) {
		t.Skip("pinned Sublime Text Python API stub is not present on this host")
	} else if err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(root, "test", "acceptance", "clients", "sublime", "driver.py")
	const probe = `
import ast, sys
api_path, driver_path = sys.argv[1:3]
with open(api_path, encoding="utf-8") as stream:
    api = ast.parse(stream.read())
def function(nodes, name):
    return next((node for node in nodes if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
                 and node.name == name), None)
syntax_from_path = function(api.body, "syntax_from_path")
assert syntax_from_path is not None, "pinned Sublime API has no syntax_from_path"
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
           and isinstance(node.func.value, ast.Name) and node.func.value.id == "sublime_api"
           and node.func.attr == "get_syntax" for node in ast.walk(syntax_from_path)), \
       "pinned syntax_from_path does not query Sublime's syntax registry"
set_timeout = function(api.body, "set_timeout")
assert set_timeout is not None and "main thread" in (ast.get_docstring(set_timeout) or ""), \
       "pinned set_timeout is not documented to dispatch on Sublime's main thread"
view = next(node for node in api.body if isinstance(node, ast.ClassDef) and node.name == "View")
assign = function(view.body, "assign_syntax")
assert assign is not None
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
           and node.func.id == "isinstance" and len(node.args) == 2
           and isinstance(node.args[1], ast.Name) and node.args[1].id == "Syntax"
           for node in ast.walk(assign)), "pinned assign_syntax does not accept a Syntax object"
syntax_class = next(node for node in api.body if isinstance(node, ast.ClassDef) and node.name == "Syntax")
slots = next((node.value for node in syntax_class.body if isinstance(node, ast.Assign)
              and any(isinstance(target, ast.Name) and target.id == "__slots__" for target in node.targets)), None)
assert isinstance(slots, (ast.List, ast.Tuple))
assert {item.value for item in slots.elts if isinstance(item, ast.Constant)} >= {"path", "scope"}
window = next(node for node in api.body if isinstance(node, ast.ClassDef) and node.name == "Window")
assert function(window.body, "find_output_panel") is not None
assert function(window.body, "run_command") is not None

with open(driver_path, encoding="utf-8") as stream:
    driver = ast.parse(stream.read())
def method(name):
    return next(node for node in ast.walk(driver) if isinstance(node, ast.FunctionDef) and node.name == name)
def queues_on_main_thread(function):
    return any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
               and node.func.attr == "set_timeout" and isinstance(node.func.value, ast.Name)
               and node.func.value.id == "sublime" for node in ast.walk(function))
wait_for_view = method("_wait_for_view_load")
wait_for_syntax = method("_wait_for_case_syntax")
assert queues_on_main_thread(wait_for_view), "driver must queue syntax assignment through the main-thread API"
assert queues_on_main_thread(wait_for_syntax), "driver must poll syntax acknowledgement through the main-thread API"
assign_case_syntax = method("_assign_case_syntax")
assign_calls = sorted((node.lineno, node.func.attr) for node in ast.walk(assign_case_syntax)
                       if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute))
positions = {name: min(line for line, attr in assign_calls if attr == name)
             for name in ("syntax_from_path", "assign_syntax")}
assert positions["syntax_from_path"] < positions["assign_syntax"], \
       "driver must resolve the loaded syntax object before assigning it"
wait_calls = sorted((node.lineno, node.func.attr) for node in ast.walk(wait_for_syntax)
                    if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute))
assert min(line for line, attr in wait_calls if attr == "syntax") < \
       min(line for line, attr in wait_calls if attr == "_continue_loaded_case"), \
       "driver must observe the active syntax before continuing the case"
continue_case = method("_continue_loaded_case")
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
           and node.func.attr == "run_command" and isinstance(node.func.value, ast.Name)
           and node.func.value.id == "view" for node in ast.walk(continue_case)), \
       "driver must invoke LSP applicability only after syntax acknowledgement"
capture_diagnostics = method("_capture_syntax_diagnostics")
diagnostic_calls = {node.func.attr for node in ast.walk(capture_diagnostics)
                    if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)}
assert {"find_output_panel", "run_command", "substr"} <= diagnostic_calls, \
       "driver must capture bounded diagnostics from Sublime's actual console panel API"
prepare = method("_prepare_loaded_case")
prepare_calls = sorted((node.lineno, node.func.attr) for node in ast.walk(prepare)
                       if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute))
assert min(line for line, attr in prepare_calls if attr == "_assign_case_syntax") < \
       min(line for line, attr in prepare_calls if attr == "_wait_for_case_syntax"), \
       "driver must assign syntax before entering its acknowledgement poll"
`
	command := exec.Command(python, "-I", "-B", "-c", probe, sublimeAPI, driverPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pinned Sublime syntax API contract failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

func TestSublimeDriverWaitsForPinnedCompletionListConsumer(t *testing.T) {
	root, python := lockedSublimeDriverPython(t)
	lock, err := readToolLock(root)
	if err != nil {
		t.Fatal(err)
	}
	lockedSublime, ok := lock.ResolvedBinaries["sublime-text"]
	if !ok {
		t.Fatal("pinned tool lock is missing resolvedBinaries.sublime-text")
	}
	if _, err := os.Stat(lockedSublime.Path); os.IsNotExist(err) {
		t.Skip("pinned Sublime Text installation is not present on this host")
	} else if err != nil {
		t.Fatal(err)
	}
	sublime, err := verifyLockedBinary(lock, "sublime-text")
	if err != nil {
		t.Fatal(err)
	}
	sublimeAPI := filepath.Join(filepath.Dir(sublime), "Lib", "python314", "sublime.py")
	if _, err := os.Stat(sublimeAPI); os.IsNotExist(err) {
		t.Skip("pinned Sublime Text CompletionList API stub is not present on this host")
	} else if err != nil {
		t.Fatal(err)
	}
	if _, ok := lock.ResolvedBinaries["sublime-lsp-package"]; !ok {
		t.Fatal("pinned tool lock is missing resolvedBinaries.sublime-lsp-package")
	}
	lspArchivePath, err := verifyLockedBinary(lock, "sublime-lsp-package")
	if err != nil {
		t.Fatal(err)
	}
	mdpopupsWheelPath, err := verifyLockedBinary(lock, "sublime-mdpopups")
	if err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(root, "test", "acceptance", "clients", "sublime", "driver.py")
	const probe = `
import ast, importlib.util, json, sys, time, types, zipfile
api_path, driver_path, archive_path, prefix, mdpopups_path = sys.argv[1:6]
with open(api_path, encoding="utf-8") as stream:
    api = ast.parse(stream.read())
completion_list = next(node for node in api.body
                       if isinstance(node, ast.ClassDef) and node.name == "CompletionList")
set_completions = next(node for node in completion_list.body
                       if isinstance(node, ast.FunctionDef) and node.name == "set_completions")
assert any(isinstance(node, ast.Assign) and any(isinstance(target, ast.Attribute)
               and target.attr == "completions" and isinstance(target.value, ast.Name)
               and target.value.id == "self" for target in node.targets)
               for node in ast.walk(set_completions)), "pinned CompletionList must store the delivered results"
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
           and node.func.attr == "completions_ready" and isinstance(node.func.value, ast.Name)
           and node.func.value.id == "target" for node in ast.walk(set_completions)), \
       "pinned CompletionList must hand results to Sublime's native completion consumer"

with zipfile.ZipFile(archive_path) as archive:
    completion_source = archive.read(prefix + "plugin/completion.py").decode("utf-8")
    documents_source = archive.read(prefix + "plugin/documents.py").decode("utf-8")
    goto_source = archive.read(prefix + "plugin/goto.py").decode("utf-8")
    references_source = archive.read(prefix + "plugin/references.py").decode("utf-8")
    hover_source = archive.read(prefix + "plugin/hover.py").decode("utf-8")
    views_source = archive.read(prefix + "plugin/core/views.py").decode("utf-8")
assert 'response_items = sorted(response_items, key=lambda item: item.get("sortText") or item["label"])' in completion_source, \
       "driver's response index must match the pinned client's item ordering"
assert 'lsp_select_completion {{"index":{index},"session_name":"{session_name}"}}' in completion_source, \
       "pinned client no longer binds delivered items to their LSP response index and session"
documents = ast.parse(documents_source)
listener = next(node for node in documents.body if isinstance(node, ast.ClassDef) and node.name == "DocumentSyncListener")
query = next(node for node in listener.body if isinstance(node, ast.FunctionDef) and node.name == "on_query_completions")
query_async = next(node for node in listener.body if isinstance(node, ast.FunctionDef) and node.name == "_on_query_completions_async")
resolved = next(node for node in listener.body if isinstance(node, ast.FunctionDef) and node.name == "_on_query_completions_resolved_async")
assert any(isinstance(base, ast.Attribute) and base.attr == "ViewEventListener"
           for base in listener.bases), "pinned completion listener must be attached to a concrete Sublime view"
assert any(isinstance(node, ast.Attribute) and node.attr == "view" and isinstance(node.value, ast.Name)
           and node.value.id == "self" for node in ast.walk(query_async)), "pinned completion query task must use its source view"
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
           and node.func.attr == "set_completions" and isinstance(node.func.value, ast.Name)
           and node.func.value.id == "clist" for node in ast.walk(resolved)), \
       "pinned completion resolver must hand the response to this view's CompletionList"
assert "ST_VERSION >= 4184" in documents_source, "pinned completion handoff version contract changed"
goto = ast.parse(goto_source)
goto_base = next(node for node in goto.body if isinstance(node, ast.ClassDef) and node.name == "LspGotoCommand")
goto_response = next(node for node in goto_base.body if isinstance(node, ast.FunctionDef) and node.name == "_handle_response_async")
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == "open_location_async"
           for node in ast.walk(goto_response)), "pinned definition client must navigate through open_location_async"
references = ast.parse(references_source)
references_panel = next(node for node in ast.walk(references)
                        if isinstance(node, ast.FunctionDef) and node.name == "_show_references_in_output_panel")
assert any(isinstance(node, ast.Constant) and node.value == "output.references" for node in ast.walk(references_panel))
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
           and node.func.attr == "append" for node in ast.walk(references_panel)), \
       "pinned references consumer must append rendered results to its output panel"
hover = ast.parse(hover_source)
hover_command = next(node for node in hover.body if isinstance(node, ast.ClassDef) and node.name == "LspHoverCommand")
show_hover = next(node for node in hover_command.body if isinstance(node, ast.FunctionDef) and node.name == "_show_hover")
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Name) and node.func.id == "show_lsp_popup"
           for node in ast.walk(show_hover)), "pinned hover client must display the rendered popup"
views = ast.parse(views_source)
show_popup = next(node for node in ast.walk(views)
                  if isinstance(node, ast.FunctionDef) and node.name == "show_lsp_popup")
assert any(isinstance(node, ast.Attribute) and node.attr == "show_popup" for node in ast.walk(show_popup)), \
       "pinned LSP popup helper must reach Sublime View.show_popup"
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
           and node.func.attr == "show_popup" and isinstance(node.func.value, ast.Name)
           and node.func.value.id == "mdpopups" for node in ast.walk(show_popup)), \
       "pinned LSP popup renderer must pass content to mdpopups"
with zipfile.ZipFile(mdpopups_path) as archive:
    mdpopups_source = archive.read("mdpopups/__init__.py").decode("utf-8")
mdpopups = ast.parse(mdpopups_source)
mdpopups_show_popup = next(node for node in mdpopups.body
                           if isinstance(node, ast.FunctionDef) and node.name == "show_popup")
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
           and node.func.attr == "show_popup" and isinstance(node.func.value, ast.Name)
           and node.func.value.id == "view" for node in ast.walk(mdpopups_show_popup)), \
       "pinned mdpopups renderer must call the native view popup API"

with open(driver_path, encoding="utf-8") as stream:
    driver = ast.parse(stream.read())
def method(name):
    return next(node for node in ast.walk(driver)
                if isinstance(node, ast.FunctionDef) and node.name == name)
install = method("_install_completion_consumption_hook")
tracker = next(node for node in ast.walk(install)
               if isinstance(node, ast.FunctionDef) and node.name == "tracked_set_completions")
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
           and node.func.id == "original" for node in ast.walk(tracker)), \
       "completion instrumentation must call the pinned API implementation"
assert any(isinstance(node, ast.Assign) and any(isinstance(target, ast.Attribute)
               and target.attr == "set_completions" for target in node.targets)
               for node in ast.walk(install)), "driver does not install the CompletionList consumer hook"
wait = method("_wait_completion_consumed")
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
           and node.func.attr == "set_timeout" for node in ast.walk(wait)), \
       "driver must allow the asynchronous client handoff after the response hook"
assert any(isinstance(node, ast.Constant) and node.value == "sublime.CompletionList.set_completions"
           for node in ast.walk(wait)), "driver must identify the actual Sublime consumer API"
assert method("_completion_consumption_matches") is not None
identity_reader = method("_native_case_evidence_identity")
assert any(isinstance(node, ast.Constant) and node.value == "acceptance_identity.json"
           for node in ast.walk(identity_reader)), "driver must read the profile-bound identity bootstrap"
hover_hook = method("_install_hover_popup_consumption_hook")
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
           and node.func.attr == "import_module" and any(isinstance(arg, ast.Constant)
           and arg.value == "LSP.plugin.hover" for arg in node.args) for node in ast.walk(hover_hook)), \
       "hover consumer instrumentation must hook the pinned LSP hover module"
tracked_hover = next(node for node in ast.walk(hover_hook)
                     if isinstance(node, ast.FunctionDef) and node.name == "tracked_show_lsp_popup")
tracked_calls = [(node.lineno, node.func.id if isinstance(node.func, ast.Name) else "")
                 for node in ast.walk(tracked_hover) if isinstance(node, ast.Call)]
assert any(name == "original" for _, name in tracked_calls), "hover instrumentation must call the original renderer"
assert any(isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
           and node.func.attr == "is_popup_visible" for node in ast.walk(tracked_hover)), \
       "hover consumer instrumentation must check native popup visibility after rendering"
assert any(isinstance(node, ast.Assign) and any(isinstance(target, ast.Attribute)
           and target.attr == "show_lsp_popup" for target in node.targets) for node in ast.walk(hover_hook)), \
       "hover consumer instrumentation must wrap the LSP module's actual popup renderer"
assert method("_wait_hover_consumed") is not None
assert method("_wait_definition_consumed") is not None
assert method("_wait_references_consumed") is not None
assert method("_wait_document_change_sent") is not None

class CompletionItem:
    def __init__(self, trigger, index=1, session_name="omnilsp_acceptance", annotation="function"):
        self.trigger, self.annotation = trigger, annotation
        self.completion = "lsp_select_completion " + json.dumps({"index": index, "session_name": session_name})
        self.completion_format, self.kind, self.details, self.flags = "text", "function", "fixture detail", 0
class View:
    def __init__(self, view_id): self._id, self._auto_visible = view_id, True
    def id(self): return self._id
    def is_auto_complete_visible(self): return self._auto_visible
class Target:
    def __init__(self): self.received = None
    def completions_ready(self, completions, flags): self.received = (completions, flags)
class CompletionList:
    def __init__(self, target=None): self.target, self.completions, self.flags = target, None, None
    def set_completions(self, completions, flags=0):
        assert self.completions is None
        self.completions, self.flags = completions, flags
        target = self.target
        if target is not None: target.completions_ready(completions, flags)
class DocumentSyncListener:
    def __init__(self, view): self.view = view
    def on_query_completions(self, prefix="", locations=None): return CompletionList(Target())
    def _on_query_completions_resolved_async(self, clist, completions, flags=0):
        clist.set_completions(completions, flags)

timeouts = []
sublime = types.ModuleType("sublime")
sublime.CompletionList = CompletionList
sublime.View = View
sublime.set_timeout = lambda callback, delay: timeouts.append((callback, delay))
sys.modules["sublime"] = sublime
sys.modules["sublime_plugin"] = types.ModuleType("sublime_plugin")
LSP = types.ModuleType("LSP"); LSP.__path__ = []
plugin = types.ModuleType("LSP.plugin"); plugin.__path__ = []
class LspPlugin:
    def __init__(self, weaksession): pass
    @classmethod
    def register(cls): pass
    @classmethod
    def unregister(cls): pass
plugin.LspPlugin = LspPlugin
protocol = types.ModuleType("LSP.plugin.core.protocol"); protocol.Request = object()
core = types.ModuleType("LSP.plugin.core"); core.__path__ = []; core.protocol = protocol
documents = types.ModuleType("LSP.plugin.documents"); documents.DocumentSyncListener = DocumentSyncListener
plugin.documents = documents
plugin.core = core; LSP.plugin = plugin
sys.modules.update({"LSP": LSP, "LSP.plugin": plugin, "LSP.plugin.core": core,
                    "LSP.plugin.core.protocol": protocol, "LSP.plugin.documents": documents})
spec = importlib.util.spec_from_file_location("omnilsp_completion_probe", driver_path)
module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
module._install_completion_consumption_hook()
module._active_completion_capture = {
    "candidate": "wantedSymbol", "sessionName": "omnilsp_acceptance", "viewId": 9,
    "caseEpoch": 3, "captureToken": "3:1"
}
CompletionList(Target()).set_completions([CompletionItem("otherSymbol")], 1)
CompletionList().set_completions([CompletionItem("wantedSymbol")], 1)
CompletionList(Target()).set_completions([CompletionItem("wantedSymbolExtra")], 1)
assert module._completion_consumptions == [], "nonmatching, near-prefix, or unbound lists must not count as consumed"
assert module._normalize_completion_trigger("wantedSymbol\tfunction", "function") == "wantedSymbol"
assert module._normalize_completion_trigger("wantedSymbol\tother", "function") is None
assert not module._completion_consumption_matches({
    "targetBound": True, "candidate": "wantedSymbol", "normalizedTrigger": "wantedSymbolExtra",
    "selection": {"index": 1, "session_name": "omnilsp_acceptance"},
    "item": {"trigger": "wantedSymbolExtra", "annotation": "function"},
    "viewId": 9, "caseEpoch": 3, "captureToken": "3:1",
}, "wantedSymbol", 1, "omnilsp_acceptance", 9, 3, "3:1"), "near-prefix delivered items must not pass exact matching"
assert not module._completion_consumption_matches({
    "targetBound": True, "candidate": "wantedSymbol", "normalizedTrigger": "wantedSymbol",
    "selection": {"index": 1, "session_name": "omnilsp_acceptance"},
    "item": {"trigger": "wantedSymbol", "annotation": "function"},
    "viewId": 8, "caseEpoch": 2, "captureToken": "2:1",
}, "wantedSymbol", 1, "omnilsp_acceptance", 9, 3, "3:1"), "another view or an earlier case must not satisfy this response"

prefix_run = module._AcceptanceRun(); prefix_run.symbol = "wantedSymbol"
try:
    prefix_run._on_completion({"result": {"items": [{"label": "wantedSymbolExtra"}]}})
    raise AssertionError("a near-prefix response label must not satisfy completion")
except RuntimeError as exc:
    assert "exactly one item labeled" in str(exc)

run = module._AcceptanceRun(); run.case = {"name": "go"}; run.symbol = "wantedSymbol"
run.session_name = "omnilsp_acceptance"
run.view = View(9); run.case_epoch = 3; run.completion_capture_token = "3:1"
run.positions = [1, 2]; run.observed = {}; run.completion_consumption_mark = 0
run._set_cursor = lambda point: setattr(run, "cursor", point)
commands = []; run._command_response = lambda *args, **kwargs: commands.append((args, kwargs))
run._on_completion({"result": {"items": [
    {"label": "wantedSymbol", "sortText": "b", "kind": 3, "insertText": "wantedSymbol()"},
    {"label": "wantedSymbolExtra", "sortText": "a"},
]}})
assert run.completion_response_index == 1 and run.completion_response_item["label"] == "wantedSymbol"
assert len(timeouts) == 1 and timeouts[0][1] == 50 and not commands, \
       "the response hook must wait for the independent client consumer callback: " + repr((timeouts, module._completion_consumptions, commands))
timeouts.clear()
wrong_view = DocumentSyncListener(View(8))
wrong_view_list = wrong_view.on_query_completions()
wrong_view._on_query_completions_resolved_async(wrong_view_list, [CompletionItem("wantedSymbol", index=1)], 7)
assert len(module._completion_consumptions) == 0, "a different native view must not be captured"
listener = DocumentSyncListener(run.view)
wrong_index_list = listener.on_query_completions()
listener._on_query_completions_resolved_async(wrong_index_list, [CompletionItem("wantedSymbol", index=0)], 7)
assert len(module._completion_consumptions) == 1
run._wait_completion_consumed()
assert len(timeouts) == 1 and not commands, "a different completion index must not satisfy this response"
timeouts.pop(0)
wrong_session_list = listener.on_query_completions()
listener._on_query_completions_resolved_async(wrong_session_list, [
    CompletionItem("wantedSymbol", index=1, session_name="other-session")], 7)
assert len(module._completion_consumptions) == 1, "a different LSP session must not be captured"
delivered = [CompletionItem("wantedSymbol\tfunction", index=1)]
completion_list = listener.on_query_completions()
target = completion_list.target
listener._on_query_completions_resolved_async(completion_list, delivered, 7)
assert target.received == (delivered, 7), "instrumentation must preserve the real consumer callback"
assert len(module._completion_consumptions) == 2
assert module._completion_consumptions[-1]["targetBound"] is True
assert module._completion_consumptions[-1]["item"]["trigger"] == "wantedSymbol\tfunction"
assert module._completion_consumptions[-1]["selection"] == {"index": 1, "session_name": "omnilsp_acceptance"}
assert module._completion_consumptions[-1]["viewId"] == 9
assert module._completion_consumptions[-1]["caseEpoch"] == 3
run._wait_completion_consumed()
assert run.observed["completion"]["clientConsumptionApi"] == "sublime.CompletionList.set_completions"
assert run.observed["completion"]["completionListTargetBound"] is True
assert run.observed["completion"]["deliveredItem"]["trigger"] == "wantedSymbol\tfunction"
assert run.observed["completion"]["responseIndex"] == 1
assert run.observed["completion"]["sessionName"] == "omnilsp_acceptance"
assert run.observed["completion"]["selection"] == {"index": 1, "session_name": "omnilsp_acceptance"}
assert run.observed["completion"]["completionPopupVisible"] is True
assert len(commands) == 1 and commands[0][0][0:2] == ("lsp_symbol_definition", "textDocument/definition")

failed = module._AcceptanceRun(); failed.case = {"name": "go"}; failed.symbol = "wantedSymbol"
failed.completion_consumption_mark = len(module._completion_consumptions)
failed.completion_response_item = {"label": "wantedSymbol"}; failed.observed = {}
failed.completion_consumption_deadline = time.monotonic() - 1
errors = []; failed._case_failed = lambda error: errors.append(str(error))
failed._wait_completion_consumed()
assert errors and "same-view" in errors[0]
`
	command := exec.Command(python, "-I", "-B", "-c", probe, sublimeAPI, driverPath, lspArchivePath, sublimeLSPArchivePrefix, mdpopupsWheelPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pinned Sublime completion consumer contract failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

func TestSublimeDriverRequiresNativeClientConsumers(t *testing.T) {
	root, python := lockedSublimeDriverPython(t)
	lock, err := readToolLock(root)
	if err != nil {
		t.Fatal(err)
	}
	sublime, err := verifyLockedBinary(lock, "sublime-text")
	if err != nil {
		t.Fatal(err)
	}
	sublimeAPI := filepath.Join(filepath.Dir(sublime), "Lib", "python314", "sublime.py")
	if _, err := os.Stat(sublimeAPI); os.IsNotExist(err) {
		t.Skip("pinned Sublime Text Python API stub is not present on this host")
	} else if err != nil {
		t.Fatal(err)
	}
	driverPath := filepath.Join(root, "test", "acceptance", "clients", "sublime", "driver.py")
	const probe = `
import ast, importlib.util, json, os, sys, tempfile, time, types
api_path, driver_path = sys.argv[1:3]
with open(api_path, encoding="utf-8") as stream:
    api = ast.parse(stream.read())
def method(nodes, name):
    return next((node for node in nodes if isinstance(node, ast.FunctionDef) and node.name == name), None)
view_api = next(node for node in api.body if isinstance(node, ast.ClassDef) and node.name == "View")
window_api = next(node for node in api.body if isinstance(node, ast.ClassDef) and node.name == "Window")
assert all(method(view_api.body, name) for name in ("show_popup", "is_popup_visible", "rowcol", "sel", "id", "file_name"))
assert all(method(window_api.body, name) for name in ("active_view", "active_panel", "find_output_panel"))

with open(driver_path, encoding="utf-8") as stream:
    driver_ast = ast.parse(stream.read())
def driver_method(name):
    return next(node for node in ast.walk(driver_ast)
                if isinstance(node, ast.FunctionDef) and node.name == name)
for source, waiter, forbidden in (("_on_hover", "_wait_hover_consumed", "_sync_edit"),
                                  ("_on_definition", "_wait_definition_consumed", "_command_response"),
                                  ("_on_references", "_wait_references_consumed", "_request_rename")):
    node = driver_method(source)
    called = {item.func.attr for item in ast.walk(node)
              if isinstance(item, ast.Call) and isinstance(item.func, ast.Attribute)}
    assert waiter in called and forbidden not in called, source + " may not advance on a raw response"

class Region:
    def __init__(self, a, b=None): self.a, self.b = a, a if b is None else b
class View:
    def __init__(self, view_id, path, text=""):
        self._id, self._path, self._text = view_id, path, text
        self.visible, self.selection, self._window, self.no_popup = False, [Region(0)], None, False
    def id(self): return self._id
    def is_valid(self): return True
    def file_name(self): return self._path
    def window(self): return self._window
    def show_popup(self, content, *args, **kwargs): self.visible, self.popup_content = not self.no_popup, content
    def is_popup_visible(self): return self.visible
    def hide_popup(self): self.visible = False
    def is_auto_complete_visible(self): return True
    def sel(self): return self.selection
    def rowcol(self, point): return self.rowcol_by_point.get(point, (0, point))
    def substr(self, region): return self._text[region.a:region.b]
    def size(self): return len(self._text)
    def line(self, point):
        start = self._text.rfind("\n", 0, point) + 1
        end = self._text.find("\n", point)
        return Region(start, len(self._text) if end < 0 else end)
class Panel:
    def __init__(self, content): self.content = content
    def is_valid(self): return True
    def size(self): return len(self.content)
    def substr(self, region): return self.content[region.a:region.b]
class Window:
    def __init__(self, view): self.view, self.panel, self.panel_name = view, None, None
    def active_view(self): return self.view
    def active_panel(self): return self.panel_name
    def find_output_panel(self, name): return self.panel if name == "references" else None

timeouts = []
class SublimeView(View): pass
sublime = types.ModuleType("sublime")
sublime.View = SublimeView
sublime.Region = Region
sublime.set_timeout = lambda callback, delay=0: timeouts.append((callback, delay))
sys.modules["sublime"] = sublime
sys.modules["sublime_plugin"] = types.ModuleType("sublime_plugin")
LSP = types.ModuleType("LSP"); LSP.__path__ = []
plugin = types.ModuleType("LSP.plugin"); plugin.__path__ = []
class LspPlugin:
    def __init__(self, weaksession): pass
    @classmethod
    def register(cls): pass
    @classmethod
    def unregister(cls): pass
plugin.LspPlugin = LspPlugin
protocol = types.ModuleType("LSP.plugin.core.protocol"); protocol.Request = object()
core = types.ModuleType("LSP.plugin.core"); core.__path__ = []; core.protocol = protocol
hover_module = types.ModuleType("LSP.plugin.hover")
def show_lsp_popup(view, content, *args, **kwargs): return view.show_popup(content)
hover_module.show_lsp_popup = show_lsp_popup
plugin.hover = hover_module
plugin.core = core; LSP.plugin = plugin
sys.modules.update({"LSP": LSP, "LSP.plugin": plugin, "LSP.plugin.core": core,
                    "LSP.plugin.core.protocol": protocol, "LSP.plugin.hover": hover_module})
spec = importlib.util.spec_from_file_location("omnilsp_native_consumer_probe", driver_path)
module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
os.environ.pop("OMNILSP_SUBLIME_GO_RUN_ID", None)
os.environ.pop("OMNILSP_SUBLIME_GO_CANDIDATE_SHA256", None)
with tempfile.TemporaryDirectory() as identity_dir:
    module.__file__ = os.path.join(identity_dir, "omnilsp_acceptance.py")
    with open(os.path.join(identity_dir, "acceptance_identity.json"), "w", encoding="utf-8") as stream:
        json.dump({"run_id": "sublime-go-probe", "candidate_sha256": "a" * 64}, stream)
    assert module._native_case_evidence_identity() == {
        "run_id": "sublime-go-probe", "candidate_sha256": "a" * 64
    }, "driver must include identity written in the isolated profile"
module.__file__ = driver_path

view_path = "D:/fixture.go"
view = SublimeView(41, view_path)
window = Window(view); view._window = window
hover = module._AcceptanceRun(); hover.case = {"languageId": "go"}; hover.symbol = "SoakTarget"
hover.view, hover.case_epoch, hover.hover_popup_mark = view, 4, 0
hover.hover_capture_token, hover.hover_consumer_deadline = "4:1", time.monotonic() + 5
hover.rust_hover_attempts, hover.rust_hover_deadline = 0, 0
hover.observed = {}; edits = []; hover._sync_edit = lambda: edits.append("sync")
module._install_hover_popup_consumption_hook()
hover._on_hover({"result": {"contents": "SoakTarget documentation"}})
assert not edits and timeouts and not module._hover_popup_events, "a raw hover response alone must not advance"
module._active_hover_capture = {"viewId": 41, "caseEpoch": 4, "captureToken": "4:1", "candidate": "SoakTarget"}
wrong_view = SublimeView(42, view_path); Window(wrong_view)
hover_module.show_lsp_popup(wrong_view, "<body>SoakTarget documentation</body>")
assert module._hover_popup_events[-1]["viewId"] == 42
assert not module._hover_popup_consumption_matches(
    module._hover_popup_events[-1], 41, 4, "4:1", "SoakTarget"
), "a matching popup from another native view must not pass"
hover_module.show_lsp_popup(view, "<body>SoakTargetExtra documentation</body>")
assert not module._hover_popup_consumption_matches(
    module._hover_popup_events[-1], 41, 4, "4:1", "SoakTarget"
), "a near-prefix popup candidate must not pass"
invisible_view = SublimeView(43, view_path); invisible_view.no_popup = True
hover_module.show_lsp_popup(invisible_view, "<body>SoakTarget documentation</body>")
assert not module._hover_popup_consumption_matches(
    module._hover_popup_events[-1], 41, 4, "4:1", "SoakTarget"
), "a popup request without visible native UI must not pass"
hover_module.show_lsp_popup(view, "<body>SoakTarget documentation</body>")
assert view.is_popup_visible() and module._hover_popup_events[-1]["contentMatchesCandidate"]
hover._wait_hover_consumed()
assert edits == ["sync"] and hover.observed["hoverConsumer"]["popupVisible"] is True
assert not module._hover_popup_consumption_matches({}, 41, 4, "4:1", "SoakTarget")

source = "func SoakTarget() {}\nfunc Use() { SoakTarget() }\n"
decl_pos = source.index("SoakTarget")
use_pos = source.index("SoakTarget", decl_pos + 1)
nav_view = SublimeView(51, view_path, source)
nav_view.rowcol_by_point = {decl_pos + 2: (0, 7), use_pos: (1, use_pos - source.index("\n") - 1)}
nav_view.selection = [Region(use_pos)]
nav_window = Window(nav_view); nav_view._window = nav_window
uri = "file:///D:/fixture.go"
definition = module._AcceptanceRun(); definition.case = {"languageId": "go"}; definition.symbol = "SoakTarget"
definition.view, definition.positions, definition.observed, definition.uri = nav_view, [decl_pos, use_pos], {}, uri
definition._set_cursor = lambda point: None
definition_calls = []; definition._command_response = lambda *args, **kwargs: definition_calls.append(args)
definition._on_definition({"result": {"uri": uri, "range": {
    "start": {"line": 0, "character": 5}, "end": {"line": 0, "character": 15}
}}})
assert not definition_calls and timeouts, "a raw definition response alone must not advance"
nav_view.selection = [Region(decl_pos + 2)]
definition._wait_definition_consumed()
assert definition_calls and definition.observed["definition"]["landing"]["line"] == 0
assert not module._definition_landing_matches(nav_window, definition.definition_expected, 999)

references = module._AcceptanceRun(); references.case = {"languageId": "go"}; references.symbol = "SoakTarget"
references.view, references.positions, references.uri = nav_view, [decl_pos, use_pos], uri
references.observed = {}; references._set_cursor = lambda point: None
rename_calls = []; references._request_rename = lambda: rename_calls.append("rename")
def location(line, column):
    return {"uri": uri, "range": {"start": {"line": line, "character": column},
                                   "end": {"line": line, "character": column + 10}}}
references._on_references({"result": [location(0, 5), location(1, 18)]})
assert not rename_calls and timeouts, "a raw references response alone must not advance"
assert module._references_panel_consumption(nav_window, "SoakTarget", references.reference_panel_lines) is None
nav_window.panel = Panel("2 references for 'SoakTarget'\n\n 1:6 " + source.splitlines()[0]
                         + "\n 2:19 " + source.splitlines()[1])
nav_window.panel_name = "output.references"
references._wait_references_consumed()
assert rename_calls == ["rename"]
assert references.observed["references"]["renderedFixtureLines"] == 2
assert module._references_panel_consumption(nav_window, "SoakTarget", references.reference_panel_lines)

assert module._did_change_matches({"method": "textDocument/didChange", "uri": uri, "version": 2,
    "contentChanges": [{"text": "\n// sublime-client-sync-probe\n"}]}, uri, 1, "// sublime-client-sync-probe")
assert not module._did_change_matches({"method": "textDocument/didChange", "uri": uri, "version": 1,
    "contentChanges": [{"text": "// sublime-client-sync-probe"}]}, uri, 1, "// sublime-client-sync-probe")
assert not module._did_change_matches({"method": "textDocument/didChange", "uri": uri, "version": 2,
    "contentChanges": [{"text": "// sublime-client-sync-probe-extra"}]}, uri, 1, "// sublime-client-sync-probe")
`
	command := exec.Command(python, "-I", "-B", "-c", probe, sublimeAPI, driverPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Sublime native consumer contract failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

func lockedSublimeDriverPython(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		t.Skipf("the locked Python interpreter is pinned for windows/amd64; got %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	root := repoRoot(t)
	lock, err := readToolLock(root)
	if err != nil {
		t.Fatal(err)
	}
	pythonLock, ok := lock.ResolvedBinaries["python"]
	if !ok {
		t.Fatal("pinned tool lock is missing resolvedBinaries.python")
	}
	if _, err := os.Stat(pythonLock.Path); os.IsNotExist(err) {
		t.Skip("pinned Python interpreter is not installed on this host")
	} else if err != nil {
		t.Fatal(err)
	}
	python, err := verifyLockedBinary(lock, "python")
	if err != nil {
		t.Fatal(err)
	}
	return root, python
}

func TestSublimeDriverWritesBoundedFailureWhenLSPImportFails(t *testing.T) {
	root, python := lockedSublimeDriverPython(t)
	driverPath := filepath.Join(root, "test", "acceptance", "clients", "sublime", "driver.py")
	resultPath := filepath.Join(t.TempDir(), "sublime-startup-result.json")
	const probe = `
import importlib.util, json, sys, types
driver_path, result_path = sys.argv[1:3]
timeouts = []
sublime = types.ModuleType("sublime")
sublime.set_timeout = lambda callback, delay: timeouts.append(delay)
sublime.run_command = lambda name: None
sys.modules["sublime"] = sublime
sys.modules["sublime_plugin"] = types.ModuleType("sublime_plugin")
class FailLSPImport:
    def find_spec(self, fullname, path=None, target=None):
        if fullname == "LSP" or fullname.startswith("LSP."):
            raise ImportError("synthetic import failure " + ("x" * 8192))
sys.meta_path.insert(0, FailLSPImport())
spec = importlib.util.spec_from_file_location("omnilsp_acceptance", driver_path)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module.plugin_loaded()
with open(result_path, encoding="utf-8") as source:
    result = json.load(source)
assert result["client"] == "sublime-lsp"
assert result["status"] == "failed"
assert result["clientTestsCompleted"] is False
assert result["serverExitConfirmed"] is False
assert "synthetic import failure" in result["error"]
assert len(result["error"]) <= len("Sublime LSP import failed before acceptance plugin initialization: ") + 4096
assert timeouts == [250]
`
	command := exec.Command(python, "-I", "-B", "-c", probe, driverPath, resultPath)
	command.Env = mergeEnv(os.Environ(), map[string]string{"OMNILSP_CLIENT_RESULT": resultPath})
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Sublime driver import failure probe failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

func TestSublimeDriverWaitsForLoadedViewAndDoesNotFakeNoSessionShutdown(t *testing.T) {
	root, python := lockedSublimeDriverPython(t)
	driverPath := filepath.Join(root, "test", "acceptance", "clients", "sublime", "driver.py")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\nfunc wantedSymbol() {}\nfunc main() { wantedSymbol() }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(t.TempDir(), "sublime-driver-result.json")
	const probe = `
import importlib.util, json, sys, time, types
driver_path, workspace, result_path = sys.argv[1:4]
timeouts = []
class Syntax:
    def __init__(self, path, scope): self.path, self.scope = path, scope
go_syntax_path = "Packages/omnilsp_acceptance/OmniLSP-Go.sublime-syntax"
syntax_registry = {go_syntax_path: Syntax(go_syntax_path, "source.go")}
class Region:
    def __init__(self, a, b=None): self.a, self.b = a, a if b is None else b
class Settings:
    def __init__(self, view): self.view = view
    def get(self, key):
        if key == "syntax": return self.view.syntax_resource
        if key == "lsp_active": return self.view.lsp_active
        return None
class Selection:
    def clear(self): pass
    def add(self, region): pass
class Console:
    def __init__(self, text): self.text = text
    def size(self): return len(self.text)
    def substr(self, region): return self.text[region.a:region.b]
class View:
    def __init__(self, text):
        self.text, self.loading, self.reads, self.syntax_resource = text, True, 0, None
        self.syntax_object, self.pending_syntax, self.events = None, None, []
        self.lsp_active, self.owner, self.path = False, None, ""
        self.selection = Selection()
    def is_valid(self): return True
    def is_loading(self): return self.loading
    def size(self): return len(self.text)
    def substr(self, region):
        assert not self.loading, "buffer text was read before Sublime finished loading it"
        self.reads += 1
        return self.text[region.a:region.b]
    def assign_syntax(self, syntax):
        assert not self.loading, "syntax assigned before Sublime finished loading the buffer"
        assert isinstance(syntax, Syntax), "driver must assign Sublime's loaded Syntax object"
        self.pending_syntax = syntax
        self.events.append("assign_syntax")
    def apply_pending_syntax(self):
        self.syntax_object = self.pending_syntax
        self.syntax_resource = self.pending_syntax.path
    def settings(self): return Settings(self)
    def sel(self): return self.selection
    def window(self): return self.owner
    def file_name(self): return self.path
    def syntax(self): return self.syntax_object
    def run_command(self, name, args):
        self.last_command = (name, args)
        self.events.append("run_command")
    def set_scratch(self, value): self.scratch = value
class Window:
    def __init__(self, views): self.views, self.console = views, None
    def open_file(self, path):
        view = self.views.pop(0); view.owner, view.path = self, path; return view
    def focus_view(self, view): pass
    def find_output_panel(self, name):
        assert name == "console"
        return self.console
    def run_command(self, name, args=None): self.last_command = (name, args)
class LspPlugin:
    def __init__(self, weaksession): self.weaksession = weaksession
    @classmethod
    def register(cls): pass
    @classmethod
    def unregister(cls): pass
sublime = types.ModuleType("sublime")
sublime.Region = Region
sublime.score_selector = lambda scope, selector: 1 if scope == "source.go" and "source.go" in selector else 0
sublime.syntax_from_path = lambda path: syntax_registry.get(path)
sublime.set_timeout = lambda callback, delay: timeouts.append((callback, delay))
sublime.run_command = lambda name: None
sys.modules["sublime"] = sublime
sys.modules["sublime_plugin"] = types.ModuleType("sublime_plugin")
LSP = types.ModuleType("LSP"); LSP.__path__ = []
plugin = types.ModuleType("LSP.plugin"); plugin.__path__ = []; plugin.LspPlugin = LspPlugin
core = types.ModuleType("LSP.plugin.core"); core.__path__ = []
protocol = types.ModuleType("LSP.plugin.core.protocol"); protocol.Request = object()
registered_plugins = {}
client_config = types.SimpleNamespace(enabled=True, selector="source.go", schemes=["file"], command=["omnilsp-dev.exe", "serve"])
class ClientConfigs:
    def __init__(self): self.all = {"omnilsp_acceptance": client_config}
api = types.ModuleType("LSP.plugin.api"); api.get_plugin = lambda name: registered_plugins.get(name)
settings_module = types.ModuleType("LSP.plugin.core.settings"); settings_module.client_configs = ClientConfigs()
class WindowManager:
    def listener_for_view(self, view): return True
registry = types.ModuleType("LSP.plugin.core.registry")
registry.windows = types.SimpleNamespace(lookup=lambda window: WindowManager())
LSP.plugin = plugin; plugin.core = core; core.protocol = protocol
sys.modules.update({"LSP": LSP, "LSP.plugin": plugin, "LSP.plugin.core": core,
                    "LSP.plugin.core.protocol": protocol, "LSP.plugin.api": api,
                    "LSP.plugin.core.settings": settings_module, "LSP.plugin.core.registry": registry})
good = View("package main\nfunc wantedSymbol() {}\nfunc main() { wantedSymbol() }\n")
bad = View("package main\nfunc main() {}\n")
window = Window([good, bad])
sublime.active_window = lambda: window
spec = importlib.util.spec_from_file_location("omnilsp_acceptance", driver_path)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module._registration_requested = True
unregistered = module._profile_registration_state()
assert unregistered["registrationName"] == "omnilsp_acceptance"
assert unregistered["settingsResource"] == "Packages/omnilsp_acceptance/omnilsp_acceptance.sublime-settings"
assert unregistered["registered"] is False and unregistered["configPresent"] is True
registered_plugins["omnilsp_acceptance"] = module.OmniLspAcceptance
registered = module._profile_registration_state()
assert registered["registered"] is True and registered["enabled"] is True
assert registered["selector"] == "source.go" and registered["commandConfigured"] is True
profile_run = module._AcceptanceRun(); advanced = []
profile_run._next_case = lambda: advanced.append(True)
profile_run._wait_for_profile_registration(time.monotonic() + 1)
assert advanced == [True]
client_config.enabled = False
disabled_run = module._AcceptanceRun(); registration_failures = []
disabled_run._fatal = lambda error: registration_failures.append(str(error))
disabled_run._wait_for_profile_registration(time.monotonic() + 1)
assert registration_failures and "registered but disabled" in registration_failures[0]
client_config.enabled = True
registered_plugins.clear()
missing_run = module._AcceptanceRun(); registration_failures = []
missing_run._fatal = lambda error: registration_failures.append(str(error))
missing_run._wait_for_profile_registration(time.monotonic() - 1)
assert registration_failures and "was not registered within 5 seconds" in registration_failures[0]
case = {"name": "go", "languageId": "go", "family": "go", "file": "main.go", "symbol": "wantedSymbol"}
run = module._AcceptanceRun(); run.workspace = workspace; run.case = case
run._open_case()
assert good.reads == 0 and good.syntax_resource is None
callback, delay = timeouts.pop(0)
assert delay == 50
good.loading = False
callback()
assert good.reads == 0 and good.syntax_resource is None and not good.events
prepare, delay = timeouts.pop(0)
assert delay == 0, "syntax setup must be queued on Sublime's main thread"
prepare()
assert good.reads == 0 and good.events == ["assign_syntax"]
assert run.observed["syntaxRegistration"]["assigned"] is False
poll_syntax, delay = timeouts.pop(0)
assert delay == 50 and not hasattr(good, "last_command"), "LSP recheck must wait for syntax acknowledgement"
good.apply_pending_syntax()
poll_syntax()
assert good.reads == 1 and good.syntax_resource == "Packages/omnilsp_acceptance/OmniLSP-Go.sublime-syntax"
assert good.syntax_object is syntax_registry[go_syntax_path]
assert good.events == ["assign_syntax", "run_command"], "LSP applicability must be rechecked only after syntax validation"
assert run.observed["syntaxRegistration"]["assigned"] is True
assert run.observed["activation_recheck"]["afterSyntaxApplied"] is True
assert len(run.positions) == 2 and timeouts[-1][1] == 100
assert good.last_command == ("lsp_check_applicable", {"session_name": "omnilsp_acceptance"})
timeouts.clear()
session = types.SimpleNamespace(window=window, state=types.SimpleNamespace(name="READY"),
                                config=types.SimpleNamespace(name="omnilsp_acceptance"))
plugin_instance = module.OmniLspAcceptance(lambda: session)
plugin_instance.on_initialized_async()
good.lsp_active = True
module._client_notifications.append({"method": "textDocument/didOpen", "uri": run.uri, "languageId": "go"})
run._wait_for_session_ready(good, time.monotonic() + 1)
assert run.observed["session_startup"]["ready"] is True
assert timeouts[-1][1] == 0
timeouts.clear()
bad_run = module._AcceptanceRun(); bad_run.workspace = workspace; bad_run.case = case
failures = []; bad_run._case_failed = lambda error: failures.append(str(error))
bad_run._open_case()
assert bad.reads == 0
callback, delay = timeouts.pop(0); assert delay == 50
bad.loading = False
callback()
prepare, delay = timeouts.pop(0); assert delay == 0
prepare()
poll_syntax, delay = timeouts.pop(0); assert delay == 50
assert bad.events == ["assign_syntax"]
bad.apply_pending_syntax()
poll_syntax()
assert failures == ["fixture does not contain symbol declaration and use"] and not timeouts
syntax_registry.clear()
missing_syntax_view = View("package main\nfunc wantedSymbol() {}\nfunc main() { wantedSymbol() }\n")
missing_syntax_run = module._AcceptanceRun(); missing_syntax_run.case = case
try:
    missing_syntax_run._assign_case_syntax(missing_syntax_view)
    raise AssertionError("driver accepted an unregistered generated syntax")
except RuntimeError as exc:
    assert "did not load the acceptance syntax resource" in str(exc)
assert missing_syntax_run.observed["syntaxRegistration"]["loaded"] is False
assert missing_syntax_view.events == [], "LSP recheck/assignment must not occur when syntax is unregistered"
syntax_registry[go_syntax_path] = Syntax(go_syntax_path, "text.plain")
wrong_scope_view = View("package main\nfunc wantedSymbol() {}\nfunc main() { wantedSymbol() }\n")
wrong_scope_run = module._AcceptanceRun(); wrong_scope_run.case = case
try:
    wrong_scope_run._assign_case_syntax(wrong_scope_view)
    raise AssertionError("driver accepted an unexpected base scope")
except RuntimeError as exc:
    assert "loaded an unexpected acceptance syntax" in str(exc)
assert wrong_scope_view.events == [], "wrong-scope syntax must be rejected before assignment"
syntax_registry[go_syntax_path] = Syntax(go_syntax_path, "source.go")
timeout_view = View("package main\nfunc wantedSymbol() {}\nfunc main() { wantedSymbol() }\n")
timeout_view.loading = False
timeout_view.owner = window
window.console = Console(("unrelated\n" * 1000) +
                         "error parsing lexer: Packages/omnilsp_acceptance/OmniLSP-Go.sublime-syntax: invalid rule\n")
timeout_run = module._AcceptanceRun(); timeout_run.case = case; timeout_run.view = timeout_view
timeout_run.observed = {}; timeout_errors = []
timeout_run._case_failed = lambda error: timeout_errors.append(str(error))
timeout_run._assign_case_syntax(timeout_view)
timeout_run._wait_for_case_syntax(timeout_view, time.monotonic() - 1)
assert timeout_errors and "did not apply the acceptance syntax within 5 seconds" in timeout_errors[0]
assert timeout_view.events == ["assign_syntax"] and timeout_run.observed["syntaxRegistration"]["assigned"] is False
syntax_console = timeout_run.observed["syntaxRegistration"]["syntaxDiagnostics"]
assert syntax_console["source"] == "Sublime console" and syntax_console["available"] is True
assert "error parsing lexer" in syntax_console["relevantOutput"]
assert len(syntax_console["relevantOutput"]) <= 4096
window.console = None
command_run = module._AcceptanceRun(); command_run.case = case; command_run.case_path = workspace + "/main.go"
command_run.view = good; command_run.observed = {}; received = []
module._client_requests.clear(); module._responses.clear()
command_run._command_response("lsp_hover", "textDocument/hover", {}, received.append, timeout=1)
assert good.last_command == ("lsp_hover", {})
wait_for_response, delay = timeouts.pop(0); assert delay == 100
module._client_requests.append({"method": "textDocument/hover", "view_path": command_run.case_path})
module._responses.append({"method": "textDocument/hover", "result": {"contents": "wantedSymbol"}})
wait_for_response()
assert received and command_run.observed["lsp_commands"][-1] == {
    "command": "lsp_hover", "method": "textDocument/hover", "commandInvocationAttempted": True,
    "commandInvocationReturned": True,
    "requestPreSendHookObserved": True, "successfulResponseHookObserved": True}
timeouts.clear()
dispatch_failure = module._AcceptanceRun(); dispatch_failure.case = case
dispatch_failure.observed = {}; errors = []
dispatch_failure._case_failed = lambda error: errors.append(str(error))
module._client_requests.clear(); module._responses.clear()
operation = {"command": "lsp_hover", "method": "textDocument/hover", "commandInvocationAttempted": True,
            "commandInvocationReturned": False,
            "requestPreSendHookObserved": False, "successfulResponseHookObserved": False}
dispatch_failure._wait_response("textDocument/hover", 0, 0, operation, lambda response: None,
                                time.monotonic() + 1, time.monotonic() - 1)
assert errors and "did not observe that request" in errors[0]
module._session_plugins.clear()
module._client_notifications.clear()
failed_run = module._AcceptanceRun(); failed_run.rows.append({"status": "failed"})
failed_run._finish()
with open(result_path, encoding="utf-8") as source: result = json.load(source)
assert result["status"] == "failed" and result["clientTestsCompleted"] is False
assert result["serverExitConfirmed"] is False and result["cleanExit"] is False
assert "no Sublime LSP session was started" in result["serverExitEvidence"]
assert "clientExitCode" not in result and timeouts[-1][1] == 250
timeouts.clear()
healthy_run = module._AcceptanceRun(); healthy_run.finalizing = True; module._driver = healthy_run
module.OmniLspAcceptance(None).on_session_end_async(None, None)
assert healthy_run.result["serverExitConfirmed"] is True
assert "without an exception" in healthy_run.result["serverExitEvidence"]
assert len(timeouts) == 1 and timeouts[0][1] == 250
`
	command := exec.Command(python, "-I", "-B", "-c", probe, driverPath, workspace, resultPath)
	command.Env = mergeEnv(os.Environ(), map[string]string{"OMNILSP_CLIENT_RESULT": resultPath})
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Sublime driver lifecycle probe failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

func TestExtractSublimeLSPPackageRejectsUnsafeArchive(t *testing.T) {
	tests := []struct {
		name    string
		entries []sublimeLSPTestArchiveEntry
	}{
		{
			name: "mixed root",
			entries: []sublimeLSPTestArchiveEntry{
				{name: sublimeLSPArchivePrefix + "plugin/__init__.py"},
				{name: "other-root/plugin.py"},
			},
		},
		{
			name: "traversal",
			entries: []sublimeLSPTestArchiveEntry{
				{name: sublimeLSPArchivePrefix + "plugin/__init__.py"},
				{name: sublimeLSPArchivePrefix + "../outside.py"},
			},
		},
		{
			name: "symbolic link",
			entries: []sublimeLSPTestArchiveEntry{
				{name: sublimeLSPArchivePrefix + "plugin/__init__.py"},
				{name: sublimeLSPArchivePrefix + "linked.py", data: []byte("outside.py"), mode: os.ModeSymlink | 0o777},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertSublimeLSPArchiveRejected(t, test.entries)
		})
	}
}

func TestExtractSublimeLSPPackageEnforcesArchiveBudgets(t *testing.T) {
	t.Run("entry count", func(t *testing.T) {
		entries := make([]sublimeLSPTestArchiveEntry, 0, sublimeLSPMaxArchiveEntries+1)
		for index := 0; index < sublimeLSPMaxArchiveEntries+1; index++ {
			entries = append(entries, sublimeLSPTestArchiveEntry{
				name: fmt.Sprintf("%sfile-%03d.py", sublimeLSPArchivePrefix, index),
			})
		}
		assertSublimeLSPArchiveRejected(t, entries)
	})
	t.Run("uncompressed byte count", func(t *testing.T) {
		fileData := bytes.Repeat([]byte("x"), sublimeLSPMaxArchiveFileSize)
		assertSublimeLSPArchiveRejected(t, []sublimeLSPTestArchiveEntry{
			{name: sublimeLSPArchivePrefix + "plugin/__init__.py"},
			{name: sublimeLSPArchivePrefix + "first.bin", data: fileData},
			{name: sublimeLSPArchivePrefix + "second.bin", data: fileData},
			{name: sublimeLSPArchivePrefix + "third.bin", data: []byte("x")},
		})
	})
}

func assertSublimeLSPArchiveRejected(t *testing.T, entries []sublimeLSPTestArchiveEntry) {
	t.Helper()
	archivePath := writeSublimeLSPTestArchive(t, entries)
	packagesDir := filepath.Join(t.TempDir(), "Packages")
	if err := os.MkdirAll(packagesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(packagesDir, "LSP")
	if err := extractSublimeLSPPackage(archivePath, destination); err == nil {
		t.Fatal("unsafe Sublime LSP package archive was accepted")
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("rejected archive created or modified its destination: %v", err)
	}
}

func sublimeLSPRuntimeWheels(lock toolLock) ([]string, error) {
	dependencies := []struct {
		lockName string
		filename string
	}{
		{lockName: "sublime-bracex", filename: "sublime-bracex-3.0.1-py3-none-any.whl"},
		{lockName: "sublime-wcmatch", filename: "sublime-wcmatch-11.0.1-py3-none-any.whl"},
		{lockName: "sublime-typing-extensions", filename: "sublime-typing_extensions-4.16.0-py3-none-any.whl"},
		{lockName: "sublime-orjson", filename: "sublime-orjson-3.12.0-cp314-cp314-win_amd64.whl"},
		{lockName: "sublime-mdpopups", filename: "sublime-mdpopups-5.1.2-py3-none-any.whl"},
	}
	wheels := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		wheel, err := verifyLockedBinary(lock, dependency.lockName)
		if err != nil {
			return nil, err
		}
		if filepath.Base(wheel) != dependency.filename {
			return nil, fmt.Errorf("resolvedBinaries.%s path must end in %q, got %q", dependency.lockName, dependency.filename, filepath.Base(wheel))
		}
		wheels = append(wheels, wheel)
	}
	return wheels, nil
}

const (
	sublimeLSPArchivePrefix      = "LSP-4070-2.13.0/"
	sublimeLSPMaxArchiveEntries  = 512
	sublimeLSPMaxArchiveFileSize = 4 << 20
	sublimeLSPMaxArchiveSize     = 8 << 20
)

type sublimeLSPArchiveEntry struct {
	file *zip.File
	path string
	dir  bool
}

func extractSublimeLSPPackage(archivePath, destination string) error {
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	if len(archive.File) == 0 {
		return errors.New("Sublime LSP package archive is empty")
	}
	if len(archive.File) > sublimeLSPMaxArchiveEntries {
		return fmt.Errorf("Sublime LSP package archive has %d entries; limit is %d", len(archive.File), sublimeLSPMaxArchiveEntries)
	}

	entries := make([]sublimeLSPArchiveEntry, 0, len(archive.File))
	seen := make(map[string]bool, len(archive.File))
	var totalSize uint64
	hasPluginInit := false
	for _, file := range archive.File {
		name := file.Name
		if !strings.HasPrefix(name, sublimeLSPArchivePrefix) {
			return fmt.Errorf("Sublime LSP package archive has a mixed or unexpected root: %q", name)
		}
		mode := file.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("Sublime LSP package archive contains unsupported symbolic link %q", name)
		}
		isDir := mode.IsDir()
		if strings.HasSuffix(name, "/") && !isDir {
			return fmt.Errorf("Sublime LSP package archive has a non-directory path with a trailing slash: %q", name)
		}
		relative := strings.TrimPrefix(name, sublimeLSPArchivePrefix)
		if relative == "" {
			if !isDir || file.UncompressedSize64 != 0 {
				return fmt.Errorf("Sublime LSP package archive root entry is not an empty directory: %q", name)
			}
			continue
		}
		if strings.HasSuffix(relative, "/") {
			relative = strings.TrimSuffix(relative, "/")
		}
		if relative == "" || strings.ContainsAny(relative, `\\:`) || !fs.ValidPath(relative) || !filepath.IsLocal(filepath.FromSlash(relative)) {
			return fmt.Errorf("Sublime LSP package archive contains unsafe path %q", name)
		}
		if !isDir && !mode.IsRegular() {
			return fmt.Errorf("Sublime LSP package archive contains unsupported file type %q", name)
		}
		if _, exists := seen[relative]; exists {
			return fmt.Errorf("Sublime LSP package archive repeats path %q", relative)
		}
		seen[relative] = isDir
		if isDir {
			if file.UncompressedSize64 != 0 {
				return fmt.Errorf("Sublime LSP package archive directory contains data: %q", name)
			}
		} else {
			if file.UncompressedSize64 > sublimeLSPMaxArchiveFileSize {
				return fmt.Errorf("Sublime LSP package archive file %q exceeds the %d-byte limit", name, sublimeLSPMaxArchiveFileSize)
			}
			if totalSize > sublimeLSPMaxArchiveSize-file.UncompressedSize64 {
				return fmt.Errorf("Sublime LSP package archive exceeds the %d-byte uncompressed limit", sublimeLSPMaxArchiveSize)
			}
			totalSize += file.UncompressedSize64
			if relative == "plugin/__init__.py" {
				hasPluginInit = true
			}
		}
		entries = append(entries, sublimeLSPArchiveEntry{file: file, path: relative, dir: isDir})
	}
	if !hasPluginInit {
		return errors.New("Sublime LSP package archive is missing plugin/__init__.py")
	}
	for _, entry := range entries {
		for parent := filepath.ToSlash(filepath.Dir(entry.path)); parent != "."; parent = filepath.ToSlash(filepath.Dir(parent)) {
			if isDir, exists := seen[parent]; exists && !isDir {
				return fmt.Errorf("Sublime LSP package archive file %q is a parent of another entry", parent)
			}
		}
	}

	parentDir := filepath.Dir(destination)
	parentInfo, err := os.Lstat(parentDir)
	if err != nil {
		return fmt.Errorf("inspect Sublime LSP package destination parent: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Sublime LSP package destination parent is not a real directory: %q", parentDir)
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("Sublime LSP package destination already exists: %q", destination)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect Sublime LSP package destination: %w", err)
	}
	stagingDir, err := os.MkdirTemp(parentDir, ".sublime-lsp-package-")
	if err != nil {
		return fmt.Errorf("create temporary Sublime LSP package directory: %w", err)
	}
	defer os.RemoveAll(stagingDir)
	stagingRoot, err := filepath.Abs(stagingDir)
	if err != nil {
		return fmt.Errorf("resolve temporary Sublime LSP package directory: %w", err)
	}
	for _, entry := range entries {
		target := filepath.Join(stagingRoot, filepath.FromSlash(entry.path))
		relative, err := filepath.Rel(stagingRoot, target)
		if err != nil || !filepath.IsLocal(relative) {
			return fmt.Errorf("Sublime LSP package path %q escapes the package directory", entry.path)
		}
		if entry.dir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create Sublime LSP package directory %q: %w", entry.path, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create parent for Sublime LSP package file %q: %w", entry.path, err)
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("create Sublime LSP package file %q: %w", entry.path, err)
		}
		input, err := entry.file.Open()
		if err != nil {
			output.Close()
			return fmt.Errorf("open Sublime LSP package archive entry %q: %w", entry.path, err)
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, int64(entry.file.UncompressedSize64)+1))
		inputErr := input.Close()
		outputErr := output.Close()
		if copyErr != nil {
			return fmt.Errorf("extract Sublime LSP package file %q: %w", entry.path, copyErr)
		}
		if inputErr != nil {
			return fmt.Errorf("close Sublime LSP package archive entry %q: %w", entry.path, inputErr)
		}
		if outputErr != nil {
			return fmt.Errorf("close Sublime LSP package file %q: %w", entry.path, outputErr)
		}
		if uint64(written) != entry.file.UncompressedSize64 {
			return fmt.Errorf("Sublime LSP package archive entry %q size mismatch: wrote %d of %d bytes", entry.path, written, entry.file.UncompressedSize64)
		}
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("Sublime LSP package destination appeared during extraction: %q", destination)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("recheck Sublime LSP package destination: %w", err)
	}
	if err := os.Rename(stagingDir, destination); err != nil {
		return fmt.Errorf("install extracted Sublime LSP package: %w", err)
	}
	return nil
}

func copyClientEnv(env map[string]string) map[string]string {
	clientEnv := make(map[string]string, len(env)+5)
	for key, value := range env {
		clientEnv[key] = value
	}
	return clientEnv
}

func installSublimeAcceptanceProfile(root, dataDir, lspPackage, serverBin, runID, candidateSHA256 string, libraryWheels []string) error {
	if strings.TrimSpace(serverBin) == "" {
		return errors.New("OMNILSP_BIN is required for Sublime LSP")
	}
	for _, directory := range []string{
		filepath.Join(dataDir, "Packages", "omnilsp_acceptance"),
		filepath.Join(dataDir, "Lib", "python314"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("create isolated Sublime profile directory %q: %w", directory, err)
		}
	}
	packageDestination := filepath.Join(dataDir, "Packages", "LSP")
	if err := extractSublimeLSPPackage(lspPackage, packageDestination); err != nil {
		return fmt.Errorf("install locked Sublime LSP package: %w", err)
	}
	pluginSource := filepath.Join(root, "test", "acceptance", "clients", "sublime", "driver.py")
	pluginDestination := filepath.Join(dataDir, "Packages", "omnilsp_acceptance", "omnilsp_acceptance.py")
	if err := copyFile(pluginSource, pluginDestination); err != nil {
		return fmt.Errorf("install Sublime acceptance plugin: %w", err)
	}
	if err := writeSublimeAcceptanceIdentity(
		filepath.Join(dataDir, "Packages", "omnilsp_acceptance", "acceptance_identity.json"),
		runID, candidateSHA256,
	); err != nil {
		return fmt.Errorf("write Sublime acceptance identity bootstrap: %w", err)
	}
	if err := copyFile(
		filepath.Join(root, "test", "acceptance", "clients", "sublime", ".python-version"),
		filepath.Join(dataDir, "Packages", "omnilsp_acceptance", ".python-version"),
	); err != nil {
		return fmt.Errorf("select Sublime acceptance plugin Python runtime: %w", err)
	}
	for _, wheel := range libraryWheels {
		if err := extractSublimePythonWheel(wheel, filepath.Join(dataDir, "Lib", "python314")); err != nil {
			return fmt.Errorf("install Sublime LSP runtime wheel %q: %w", filepath.Base(wheel), err)
		}
	}
	for _, syntax := range []string{"OmniLSP-Go.sublime-syntax", "OmniLSP-C.sublime-syntax", "OmniLSP-Cpp.sublime-syntax", "OmniLSP-Rust.sublime-syntax", "OmniLSP-Python.sublime-syntax", "OmniLSP-TypeScript.sublime-syntax", "OmniLSP-TSX.sublime-syntax", "OmniLSP-JavaScript.sublime-syntax", "OmniLSP-JSX.sublime-syntax"} {
		if err := copyFile(
			filepath.Join(root, "test", "acceptance", "clients", "sublime", "syntaxes", syntax),
			filepath.Join(dataDir, "Packages", "omnilsp_acceptance", syntax),
		); err != nil {
			return fmt.Errorf("install Sublime syntax %s: %w", syntax, err)
		}
	}
	settings := map[string]any{
		"enabled": true,
		"command": []string{serverBin, "serve"},
		"selector": strings.Join([]string{
			"source.go", "source.c", "source.c++", "source.rust", "source.python",
			"source.ts", "source.tsx", "source.js", "source.jsx",
		}, " | "),
		"schemes": []string{"file"},
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Sublime LSP settings: %w", err)
	}
	settingsPath := filepath.Join(dataDir, "Packages", "omnilsp_acceptance", "omnilsp_acceptance.sublime-settings")
	if err := os.WriteFile(settingsPath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write isolated Sublime LSP settings: %w", err)
	}
	return nil
}

func writeSublimeAcceptanceIdentity(path, runID, candidateSHA256 string) error {
	if runID == "" && candidateSHA256 == "" {
		return nil
	}
	if len(runID) == 0 || len(runID) > 128 {
		return fmt.Errorf("Sublime acceptance run ID must contain 1 to 128 safe characters")
	}
	for _, char := range runID {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_') {
			return fmt.Errorf("Sublime acceptance run ID contains unsafe character %q", char)
		}
	}
	if !isSHA256(candidateSHA256) {
		return fmt.Errorf("Sublime acceptance candidate identity must be a SHA-256 digest")
	}
	identity := struct {
		RunID           string `json:"run_id"`
		CandidateSHA256 string `json:"candidate_sha256"`
	}{RunID: runID, CandidateSHA256: strings.ToLower(candidateSHA256)}
	data, err := json.Marshal(identity)
	if err != nil {
		return fmt.Errorf("encode Sublime acceptance identity: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write Sublime acceptance identity %q: %w", path, err)
	}
	return nil
}

func TestSublimeAcceptanceIdentityBootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acceptance_identity.json")
	if err := writeSublimeAcceptanceIdentity(path, "sublime-go-test-123", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		RunID           string `json:"run_id"`
		CandidateSHA256 string `json:"candidate_sha256"`
	}
	if err := json.Unmarshal(data, &identity); err != nil {
		t.Fatal(err)
	}
	if identity.RunID != "sublime-go-test-123" || identity.CandidateSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("identity sidecar = %#v", identity)
	}
	if err := writeSublimeAcceptanceIdentity(path, "unsafe/run", strings.Repeat("a", 64)); err == nil {
		t.Fatal("unsafe run ID was accepted")
	}
	if err := writeSublimeAcceptanceIdentity(path, "sublime-go-test-123", "not-a-sha256"); err == nil {
		t.Fatal("invalid candidate SHA-256 was accepted")
	}
}

func extractSublimePythonWheel(wheelPath, destination string) error {
	if filepath.Ext(wheelPath) != ".whl" {
		return fmt.Errorf("expected a Python wheel, got %q", filepath.Base(wheelPath))
	}
	archive, err := zip.OpenReader(wheelPath)
	if err != nil {
		return err
	}
	defer archive.Close()
	if len(archive.File) == 0 {
		return errors.New("wheel archive is empty")
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	for _, entry := range archive.File {
		name := entry.Name
		if entry.FileInfo().IsDir() {
			name = strings.TrimSuffix(name, "/")
		}
		if name == "" || name == "." || strings.Contains(name, "\\") || strings.Contains(name, ":") || !fs.ValidPath(name) || !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("wheel contains unsafe path %q", entry.Name)
		}
		if entry.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("wheel contains unsupported symbolic link %q", entry.Name)
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		relative, err := filepath.Rel(root, target)
		if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("wheel path %q escapes the isolated Python library directory", entry.Name)
		}
		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		input, err := entry.Open()
		if err != nil {
			output.Close()
			return err
		}
		_, copyErr := io.Copy(output, input)
		inputCloseErr := input.Close()
		outputCloseErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		if inputCloseErr != nil {
			return inputCloseErr
		}
		if outputCloseErr != nil {
			return outputCloseErr
		}
	}
	return nil
}

func copyFile(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		return err
	}
	return nil
}

// A failed operation remains failed even when its managed process tree exits.
// Cleanup may use that independent exit fact without mistaking it for success.
type nativeEditorProcessError struct {
	err               error
	processTreeExited bool
}

func (e *nativeEditorProcessError) Error() string { return e.err.Error() }
func (e *nativeEditorProcessError) Unwrap() error { return e.err }

func runNativeEditorProcess(client, binary string, args []string, workspace, resultPath string, env map[string]string) (finalErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), clientTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = workspace
	cmd.Env = mergeEnv(os.Environ(), env)
	var processTree *editorProcessTracker
	output, runErr := runClientCommandTracked(ctx, cmd, func(pid int) error {
		var trackErr error
		processTree, trackErr = startEditorProcessTracker(pid)
		return trackErr
	})
	processExitErr := waitForEditorProcessTree(processTree)
	defer func() {
		if finalErr != nil {
			finalErr = &nativeEditorProcessError{err: finalErr, processTreeExited: processTree != nil && processExitErr == nil}
		}
	}()
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	priorResult, resultErr := readEditorResult(resultPath)
	serverExitConfirmed := resultErr == nil && priorResult.Client == client && priorResult.ServerExitConfirmed && processExitErr == nil
	serverExitEvidence := "client confirmed graceful LSP shutdown; managed editor process tree exited"
	if resultErr != nil {
		serverExitEvidence = "client shutdown result unavailable: " + resultErr.Error()
	} else if priorResult.Client != client {
		serverExitEvidence = fmt.Sprintf("client result identity mismatch: got %q, want %q", priorResult.Client, client)
	} else if !priorResult.ServerExitConfirmed {
		serverExitEvidence = "client did not confirm graceful LSP shutdown"
	} else if processExitErr != nil {
		serverExitEvidence = "managed editor process tree did not exit: " + processExitErr.Error()
	}
	recordErr := recordEditorExitOutcome(resultPath, exitCode, serverExitConfirmed, serverExitEvidence)
	if runErr != nil {
		if ctx.Err() != nil {
			detail := fmt.Sprintf("%s exceeded %s: %v", client, clientTimeout, ctx.Err())
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
		return fmt.Errorf("%s: %w: %s", client, runErr, strings.TrimSpace(string(output)))
	}
	if recordErr != nil {
		return fmt.Errorf("record %s exit evidence: %w", client, recordErr)
	}
	if exitCode != 0 {
		return fmt.Errorf("%s process returned exit code %d", client, exitCode)
	}
	if !serverExitConfirmed {
		return fmt.Errorf("%s did not prove graceful LSP and process-tree shutdown: %s", client, serverExitEvidence)
	}
	return nil
}
