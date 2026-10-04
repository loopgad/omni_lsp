//go:build clients && windows

package clients

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/replay"
)

const (
	helixNativeGoOptIn          = "OMNILSP_HELIX_NATIVE_GO"
	helixNativeEvidenceDir      = "OMNILSP_HELIX_NATIVE_EVIDENCE_DIR"
	helixNativeCandidatePath    = `D:\Destop\test\my_lsp\test\acceptance\tools\bin\omnilsp-dev.exe`
	helixNativeTraceLimit       = 16 * 1024 * 1024
	helixNativeSummaryLimit     = 256 * 1024
	helixNativeFailureLimit     = 8 * 1024
	helixNativeHoverPopupMarker = "func SoakTarget(value int) int"
)

// TestHelixNativeGoVertical proves one actual Helix UI path against the exact
// candidate selected by OMNILSP_BIN. It stays opt-in because it launches a
// native editor, OmniLSP, and gopls.
func TestHelixNativeGoVertical(t *testing.T) {
	if os.Getenv(helixNativeGoOptIn) != "1" {
		t.Skipf("set %s=1 to launch the bounded native Helix Go vertical", helixNativeGoOptIn)
	}
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		t.Skipf("the locked Helix runtime is windows/amd64; got %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	root := repoRoot(t)
	lockBytes, err := os.ReadFile(filepath.Join(root, "test", "acceptance", "tools", "tools.lock.json"))
	if err != nil {
		t.Fatalf("read acceptance tool lock: %v", err)
	}
	var lock toolLock
	if err := json.Unmarshal(lockBytes, &lock); err != nil {
		t.Fatalf("parse acceptance tool lock: %v", err)
	}
	helixPath, err := verifyLockedBinary(lock, "helix")
	if err != nil {
		t.Fatalf("verify locked Helix binary: %v", err)
	}
	goPath, err := verifyLockedBinary(lock, "go")
	if err != nil {
		t.Fatalf("verify locked Go toolchain: %v", err)
	}
	goplsPath, err := verifyLockedBinary(lock, "gopls")
	if err != nil {
		t.Fatalf("verify locked gopls binary: %v", err)
	}
	helixVersion, err := runToolVersionProbe(helixPath, "--version")
	if err != nil || !strings.Contains(helixVersion, "helix 25.07.1 (a05c151b)") {
		t.Fatalf("locked Helix version = %q, err=%v", helixVersion, err)
	}

	candidatePath := strings.TrimSpace(os.Getenv("OMNILSP_BIN"))
	if candidatePath == "" {
		t.Fatal("OMNILSP_BIN must name the explicitly scheduled diagnostic candidate")
	}
	candidatePath, err = filepath.Abs(candidatePath)
	if err != nil {
		t.Fatalf("resolve OMNILSP_BIN: %v", err)
	}
	if !sameExecutablePath(candidatePath, helixNativeCandidatePath) {
		t.Fatalf("OMNILSP_BIN must be the scheduled diagnostic candidate %q; got %q", helixNativeCandidatePath, candidatePath)
	}
	candidateHash, err := fileSHA256(candidatePath)
	if err != nil {
		t.Fatalf("hash scheduled candidate before launch: %v", err)
	}

	workspace := t.TempDir()
	mainPath := filepath.Join(workspace, "main.go")
	goMod := "module helixnative\n\ngo 1.26\n"
	mainSource := "package main\n\n" +
		"// SoakTarget is the documented symbol exposed in Helix hover.\n" +
		"func SoakTarget(value int) int { return value + 1 }\n\n" +
		"func UseTarget() int { return SoakTarget(1) }\n\n" +
		"func useTargetAgain() int { return SoakTarget(2) }\n\n" +
		"func completionProbe() int { return SoakT(3) }\n\n" +
		"var _ = omnilspMissingSymbol\n"
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte(goMod), 0o600); err != nil {
		t.Fatalf("write native Go module: %v", err)
	}
	if err := os.WriteFile(mainPath, []byte(mainSource), 0o600); err != nil {
		t.Fatalf("write native Go fixture: %v", err)
	}
	tracePath := filepath.Join(workspace, "evidence", "server-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(tracePath), 0o700); err != nil {
		t.Fatalf("create native evidence directory: %v", err)
	}
	profilePath := filepath.Join(workspace, ".helix", "languages.toml")
	profile := helixNativeGoProfile(candidatePath, workspace, tracePath)
	if err := os.MkdirAll(filepath.Dir(profilePath), 0o700); err != nil {
		t.Fatalf("create project-local Helix config: %v", err)
	}
	if err := os.WriteFile(profilePath, []byte(profile), 0o600); err != nil {
		t.Fatalf("write project-local Helix config: %v", err)
	}

	pathValue := strings.Join([]string{filepath.Dir(goplsPath), filepath.Dir(goPath), filepath.Join(filepath.Dir(helixPath), ".."), os.Getenv("PATH")}, string(os.PathListSeparator))
	env := []string{
		"HELIX_RUNTIME=" + filepath.Join(filepath.Dir(helixPath), "runtime"),
		"APPDATA=" + filepath.Join(workspace, ".appdata"),
		"LOCALAPPDATA=" + filepath.Join(workspace, ".localappdata"),
		"OMNILSP_TRUST=trusted",
		"OMNILSP_INDEX_DIR=" + filepath.Join(workspace, ".omnilsp-index"),
		"GOTOOLCHAIN=local",
		"GOWORK=off",
		"GOMAXPROCS=4",
		"PATH=" + pathValue,
	}
	terminalContext, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	logPath := filepath.Join(workspace, "helix.log")
	terminal, err := startHelixTerminal(terminalContext, helixPath, []string{"--log", logPath, mainPath + ":6:34"}, workspace, env, 120, 40)
	if err != nil {
		t.Fatalf("start locked Helix in ConPTY: %v", err)
	}
	defer func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if closeErr := terminal.Close(closeContext); closeErr != nil {
			t.Errorf("close Helix terminal: %v", closeErr)
		}
	}()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		frame := terminal.Snapshot()
		t.Logf("bounded Helix failure frame (at most %d bytes):\n%s", helixNativeFailureLimit, boundedHelixFailureText(frame.Text()))
		if evidenceDir := strings.TrimSpace(os.Getenv(helixNativeEvidenceDir)); evidenceDir != "" {
			evidenceTrace := filepath.Join(evidenceDir, "helix-go-server-session.jsonl")
			if size, copyErr := copyHelixNativeTrace(tracePath, evidenceTrace); copyErr != nil {
				t.Logf("Helix trace preservation failed: %v", copyErr)
			} else if size != 0 {
				t.Logf("full Helix trace retained at %s (%d bytes; bounded to %d bytes)", evidenceTrace, size, helixNativeTraceLimit)
			} else {
				t.Logf("Helix trace was empty at failure; source path was %s", tracePath)
			}
		}
		t.Logf("compact LSP trace summary: %s", boundedHelixFailureText(helixFailureTraceSummary(tracePath)))
	})

	healthCtx, healthCancel := context.WithTimeout(terminalContext, 15*time.Second)
	healthCmd := exec.CommandContext(healthCtx, helixPath, "--health", "go")
	healthCmd.Dir = workspace
	healthCmd.Env = mergeEnv(os.Environ(), terminalEnvMap(env))
	healthOutput, healthErr := runClientCommand(healthCtx, healthCmd)
	healthCancel()
	if healthErr != nil {
		t.Fatalf("locked Helix Go health check failed: %v: %s", healthErr, boundedHelixText(string(healthOutput)))
	}
	if !strings.Contains(string(healthOutput), "omnilsp") {
		t.Fatalf("Helix health output did not show the project-local OmniLSP profile: %s", boundedHelixText(string(healthOutput)))
	}

	initialFrame := awaitHelixFrame(t, terminal, terminalContext, "open Go buffer", func(frame terminalFrame) bool {
		return helixStatusHasCursorLine(frame, 6) && strings.Contains(frame.Text(), "func UseTarget() int { return SoakTarget(1) }")
	})
	goURI := (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(mainPath)}).String()
	awaitHelixSessionReady(t, tracePath, terminalContext, goURI)
	observed := map[string]any{
		"candidate_path":            candidatePath,
		"candidate_sha256":          candidateHash,
		"helix_path":                helixPath,
		"helix_version":             strings.TrimSpace(helixVersion),
		"fixture_path":              mainPath,
		"initial_document_location": helixStatusLocation(initialFrame),
		"server_ready_evidence":     "initialize response, initialized notification, and Go fixture didOpen observed in Helix trace",
	}

	initialHoverPopupCount := strings.Count(initialFrame.Text(), helixNativeHoverPopupMarker)
	if initialHoverPopupCount == 0 {
		t.Fatalf("initial buffer frame did not contain the Go symbol used for hover: %s", boundedHelixFailureText(initialFrame.Text()))
	}
	if err := sendHelixKeys(terminal, terminalContext, " "+"k"); err != nil {
		t.Fatalf("request Helix hover: %v", err)
	}
	hoverFrame := awaitHelixFrame(t, terminal, terminalContext, "client-visible hover documentation", func(frame terminalFrame) bool {
		return helixHoverPopupVisible(frame, initialFrame)
	})
	observed["hover"] = "visible documentation popup for SoakTarget"
	observed["hover_excerpt"] = boundedHelixText(hoverFrame.Text())
	if err := sendHelixKeys(terminal, terminalContext, "\x1b"); err != nil {
		t.Fatalf("close Helix hover popup: %v", err)
	}

	if err := sendHelixKeys(terminal, terminalContext, "gd"); err != nil {
		t.Fatalf("request Helix definition: %v", err)
	}
	definitionFrame := awaitHelixFrame(t, terminal, terminalContext, "navigate to SoakTarget definition", func(frame terminalFrame) bool {
		return helixStatusHasCursorLine(frame, 4)
	})
	observed["definition"] = map[string]any{"document_location": helixStatusLocation(definitionFrame)}

	if err := sendHelixKeys(terminal, terminalContext, "gr"); err != nil {
		t.Fatalf("request Helix references: %v", err)
	}
	referencesListFrame := awaitHelixFrame(t, terminal, terminalContext, "open Helix references picker", func(frame terminalFrame) bool {
		return helixReferencesListVisible(frame, "main.go")
	})
	observed["references_list"] = boundedHelixText(referencesListFrame.Text())
	if err := sendHelixPickerNext(terminal, terminalContext, 2); err != nil {
		t.Fatalf("navigate to the third reference with picker Ctrl-N: %v", err)
	}
	referencesFrame := awaitHelixFrame(t, terminal, terminalContext, "select the Go useTargetAgain reference", func(frame terminalFrame) bool {
		return helixReferenceSelectionVisible(frame, "main.go", 8)
	})
	observed["references_picker_selection"] = boundedHelixText(referencesFrame.Text())
	if err := sendHelixKeys(terminal, terminalContext, "\r"); err != nil {
		t.Fatalf("accept the selected Go reference: %v", err)
	}
	referenceTargetFrame := awaitHelixFrame(t, terminal, terminalContext, "navigate to the selected useTargetAgain reference", func(frame terminalFrame) bool {
		return helixStatusHasCursorLine(frame, 8)
	})
	observed["references"] = map[string]any{"selected_location": "main.go:8", "document_location": helixStatusLocation(referenceTargetFrame)}

	if err := sendHelixKeys(terminal, terminalContext, ":goto 10\r"); err != nil {
		t.Fatalf("navigate to the Go completion line: %v", err)
	}
	completionLineFrame := awaitHelixFrame(t, terminal, terminalContext, "navigate to the Go completion line", func(frame terminalFrame) bool {
		return helixStatusHasCursorLine(frame, 10) && strings.Contains(frame.Text(), "func completionProbe() int { return SoakT(3) }")
	})
	observed["completion_line_location"] = helixStatusLocation(completionLineFrame)
	if err := sendHelixKeys(terminal, terminalContext, "fSllllli"); err != nil {
		t.Fatalf("position at the Go completion prefix: %v", err)
	}
	if err := sendHelixKeys(terminal, terminalContext, "\x18"); err != nil {
		t.Fatalf("request Helix completion: %v", err)
	}
	completionFrame := awaitHelixFrame(t, terminal, terminalContext, "show SoakTarget completion item", func(frame terminalFrame) bool {
		return helixCompletionMenuItemVisible(frame, completionLineFrame, 10)
	})
	observed["completion_popup"] = boundedHelixText(completionFrame.Text())
	if err := sendHelixKeys(terminal, terminalContext, "\x0e"); err != nil {
		t.Fatalf("select the SoakTarget completion with Ctrl-N: %v", err)
	}
	expectedCompletionLine := "func completionProbe() int { return SoakTarget(3) }"
	selectedCompletionFrame := awaitHelixFrame(t, terminal, terminalContext, "select and preview the SoakTarget completion", func(frame terminalFrame) bool {
		return helixCompletionSelected(frame, completionLineFrame, 10, expectedCompletionLine)
	})
	observed["completion_selected_row"] = boundedHelixText(selectedCompletionFrame.Text())
	if err := sendHelixKeys(terminal, terminalContext, "\r"); err != nil {
		t.Fatalf("accept Helix completion item: %v", err)
	}
	completionAppliedFrame := awaitHelixFrame(t, terminal, terminalContext, "apply the selected completion in the editor buffer", func(frame terminalFrame) bool {
		return helixStatusHasCursorLine(frame, 10) && strings.Contains(frame.Text(), expectedCompletionLine)
	})
	observed["completion_applied_buffer"] = boundedHelixText(completionAppliedFrame.Text())
	if err := sendHelixKeys(terminal, terminalContext, "\x1b"); err != nil {
		t.Fatalf("return to normal mode after accepting completion: %v", err)
	}
	normalModeFrame := awaitHelixFrame(t, terminal, terminalContext, "return to Helix normal mode before saving", func(frame terminalFrame) bool {
		return helixStatusHasMode(frame, "NOR") && helixStatusHasCursorLine(frame, 10)
	})
	observed["completion_normal_mode_before_save"] = boundedHelixText(normalModeFrame.Text())
	if err := sendHelixKeys(terminal, terminalContext, ":write\r"); err != nil {
		t.Fatalf("write buffer after accepting completion: %v", err)
	}
	expectedCompletedSource := strings.Replace(mainSource, "func completionProbe() int { return SoakT(3) }", expectedCompletionLine, 1)
	completedSource, err := awaitHelixFileContents(terminalContext, mainPath, expectedCompletedSource)
	if err != nil {
		t.Fatalf("Helix did not write the accepted completion to disk: %v\nfinal screen:\n%s", err, boundedHelixFailureText(terminal.Snapshot().Text()))
	}
	observed["completion"] = "LSP result accepted and saved as SoakTarget(3)"
	observed["edit_synchronization"] = "saved completion edit verified on disk; didChange is checked in the OmniLSP trace"

	if err := sendHelixKeys(terminal, terminalContext, " d"); err != nil {
		t.Fatalf("open Helix diagnostics picker: %v", err)
	}
	diagnosticFrame := awaitHelixFrame(t, terminal, terminalContext, "client-visible Go diagnostic", func(frame terminalFrame) bool {
		return strings.Contains(frame.Text(), "undefined: omnilspMissingSymbol")
	})
	observed["diagnostic"] = boundedHelixText(diagnosticFrame.Text())
	if err := sendHelixKeys(terminal, terminalContext, "\x1b"); err != nil {
		t.Fatalf("close Helix diagnostics picker: %v", err)
	}

	if err := sendHelixKeys(terminal, terminalContext, ":goto 4\r"); err != nil {
		t.Fatalf("navigate to SoakTarget before opening rename: %v", err)
	}
	awaitHelixFrame(t, terminal, terminalContext, "navigate to SoakTarget for rename", func(frame terminalFrame) bool {
		return helixStatusHasCursorLine(frame, 4)
	})
	if err := sendHelixKeys(terminal, terminalContext, "fS r"); err != nil {
		t.Fatalf("open Helix rename prompt for SoakTarget: %v", err)
	}
	if err := sendHelixKeys(terminal, terminalContext, "\x01\x0bUseTarget\r"); err != nil {
		t.Fatalf("submit colliding Helix rename: %v", err)
	}
	renameFrame := awaitHelixFrame(t, terminal, terminalContext, "client-visible rename refusal", func(frame terminalFrame) bool {
		return strings.Contains(strings.ToLower(frame.Text()), "rename refused")
	})
	if !strings.Contains(strings.ToLower(renameFrame.Text()), "sem-safe-001") {
		t.Fatalf("Helix displayed a rename message without the SEM-SAFE-001 refusal: %s", boundedHelixFailureText(renameFrame.Text()))
	}
	afterRename, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("read Go fixture after rename refusal: %v", err)
	}
	if string(afterRename) != string(completedSource) {
		t.Fatalf("rename refusal changed source file on disk:\nbefore:\n%s\nafter:\n%s", boundedHelixText(string(completedSource)), boundedHelixText(string(afterRename)))
	}
	if !strings.Contains(renameFrame.Text(), "func SoakTarget") || !strings.Contains(renameFrame.Text(), "func UseTarget") {
		t.Fatalf("rename refusal did not preserve both symbols in the client buffer: %s", boundedHelixFailureText(renameFrame.Text()))
	}
	observed["rename_refusal"] = "Helix displayed typed rename refusal and the source remained unchanged"

	if err := sendHelixKeys(terminal, terminalContext, "\x1b:q\r"); err != nil {
		t.Fatalf("request normal Helix quit: %v", err)
	}
	finished, err := terminal.Wait(terminalContext)
	if err != nil {
		t.Fatalf("wait for normal Helix and server process-tree exit: %s", boundedHelixFailureText(fmt.Sprintf("%v\nfinal screen:\n%s", err, terminal.Snapshot().Text())))
	}
	if finished.ExitCode != 0 || !finished.ProcessTreeGone {
		t.Fatalf("Helix terminal exit = %#v, want exit 0 and an empty tracked process tree", finished)
	}
	observed["normal_exit"] = map[string]any{
		"exit_code":         finished.ExitCode,
		"process_tree_gone": finished.ProcessTreeGone,
		"identity":          finished.Identity.String(),
	}
	trace, methods, err := readHelixNativeTrace(tracePath)
	if err != nil {
		t.Fatalf("read bounded OmniLSP trace: %v", err)
	}
	if err := verifyHelixGoTrace(trace, methods, goURI); err != nil {
		t.Fatalf("verify recorded Helix LSP consumption/lifecycle: %v", err)
	}
	observed["trace_methods"] = methods
	observed["rename_response"] = map[string]any{"code": jsonrpc.RequestFailed, "message": "rename refused: symbol is exported; importer packages are not loaded (SEM-SAFE-001)"}
	observed["trace_entry_count"] = len(trace.Entries)
	observed["profile_path"] = profilePath
	observed["server_trace_path"] = tracePath

	currentHash, err := fileSHA256(candidatePath)
	if err != nil {
		t.Fatalf("hash scheduled candidate after normal exit: %v", err)
	}
	if !strings.EqualFold(candidateHash, currentHash) {
		t.Fatalf("candidate changed while Helix was running: before=%s after=%s", candidateHash, currentHash)
	}
	observed["candidate_unchanged_during_run"] = true
	if evidenceDir := strings.TrimSpace(os.Getenv(helixNativeEvidenceDir)); evidenceDir != "" {
		if err := writeHelixNativeEvidence(evidenceDir, observed, tracePath); err != nil {
			t.Fatalf("write persisted Helix Go evidence: %v", err)
		}
	}
	evidenceJSON, _ := json.Marshal(observed)
	t.Logf("HELIX_NATIVE_GO_EVIDENCE %s", boundedHelixText(string(evidenceJSON)))
	closeContext, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := terminal.Close(closeContext); err != nil {
		t.Fatalf("release normal Helix terminal session: %v", err)
	}
}

func helixNativeGoProfile(candidatePath, workspace, tracePath string) string {
	quote := func(value string) string {
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	return strings.Join([]string{
		"[language-server.omnilsp]",
		"command = " + quote(candidatePath),
		"args = [\"serve\", \"--workspace\", " + quote(workspace) + ", \"--record\", " + quote(tracePath) + "]",
		"environment = { \"OMNILSP_TRUST\" = \"trusted\" }",
		"",
		"[[language]]",
		"name = \"go\"",
		"language-id = \"go\"",
		"language-servers = [\"omnilsp\"]",
		"",
	}, "\n")
}

func terminalEnvMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, contents, ok := strings.Cut(value, "=")
		if ok {
			result[key] = contents
		}
	}
	return result
}

func sendHelixKeys(terminal *helixTerminal, parent context.Context, keys string) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	for len(keys) != 0 {
		escape := strings.IndexByte(keys, '\x1b')
		if escape < 0 {
			return terminal.Send(ctx, []byte(keys))
		}
		if escape != 0 {
			if err := terminal.Send(ctx, []byte(keys[:escape])); err != nil {
				return err
			}
		}
		if err := terminal.Send(ctx, []byte{0x1b}); err != nil {
			return err
		}
		keys = keys[escape+1:]
		// Crossterm treats ESC followed immediately by a character as an Alt
		// key. Let Helix consume standalone Escape before sending the next key.
		timer := time.NewTimer(80 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
	return nil
}

func sendHelixPickerNext(terminal *helixTerminal, parent context.Context, count int) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	for range count {
		if err := terminal.Send(ctx, []byte{0x0e}); err != nil {
			return err
		}
	}
	return nil
}

func awaitHelixFrame(t *testing.T, terminal *helixTerminal, parent context.Context, reason string, accept func(terminalFrame) bool) terminalFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	for {
		frame := terminal.Snapshot()
		if frame.Error != "" {
			t.Fatalf("Helix terminal parser failed while waiting to %s: %s", reason, boundedHelixFailureText(fmt.Sprintf("%s\n%s", frame.Error, frame.Text())))
		}
		if accept(frame) {
			return frame
		}
		if _, err := terminal.Read(ctx); err != nil {
			t.Fatalf("timed out waiting to %s: %s", reason, boundedHelixFailureText(fmt.Sprintf("%v\n%s", err, terminal.Snapshot().Text())))
		}
	}
}

func awaitHelixSessionReady(t *testing.T, path string, parent context.Context, expectedURI string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	for {
		ready, err := helixTraceHasReadyGoDocument(path, expectedURI)
		if err != nil {
			t.Fatalf("read OmniLSP trace while waiting for initialized Go didOpen: %v", err)
		}
		if ready {
			return
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("timed out waiting for initialize response, initialized, and Go didOpen %q", expectedURI)
		}
	}
}

func helixTraceHasReadyGoDocument(path, expectedURI string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, helixNativeTraceLimit+1))
	if err != nil {
		return false, err
	}
	if len(data) > helixNativeTraceLimit {
		return false, fmt.Errorf("session trace exceeds %d-byte limit", helixNativeTraceLimit)
	}
	var initializeID *jsonrpc.RequestID
	initializeCompleted := false
	initialized := false
	for _, line := range strings.Split(string(data), "\n") {
		var entry struct {
			Dir     string          `json:"dir"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		var message jsonrpc.Message
		if json.Unmarshal(entry.Payload, &message) != nil {
			continue
		}
		if entry.Dir == "in" && message.Method == "initialize" && message.ID != nil {
			initializeID = message.ID
			continue
		}
		if initializeID != nil && entry.Dir == "out" && message.ID != nil &&
			message.Method == "" && message.ID.Equals(*initializeID) {
			initializeCompleted = message.Error == nil
			continue
		}
		if initializeCompleted && entry.Dir == "in" && message.Method == "initialized" && message.ID == nil {
			initialized = true
			continue
		}
		if initialized && entry.Dir == "in" && message.Method == "textDocument/didOpen" && message.ID == nil {
			var params struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			if json.Unmarshal(message.Params, &params) == nil && params.TextDocument.URI == expectedURI {
				return true, nil
			}
		}
	}
	return false, nil
}

func cursorLine(frame terminalFrame) string {
	if frame.CursorRow < 0 || frame.CursorRow >= len(frame.Lines) {
		return ""
	}
	return frame.Lines[frame.CursorRow]
}

func helixHoverPopupVisible(frame, baseline terminalFrame) bool {
	if strings.Count(frame.Text(), helixNativeHoverPopupMarker) <= strings.Count(baseline.Text(), helixNativeHoverPopupMarker) {
		return false
	}
	baselineRows := make(map[int]struct{})
	for row, line := range baseline.Lines {
		if strings.Contains(line, helixNativeHoverPopupMarker) {
			baselineRows[row] = struct{}{}
		}
	}
	for row, line := range frame.Lines {
		if strings.Contains(line, helixNativeHoverPopupMarker) {
			if _, wasSourceRow := baselineRows[row]; !wasSourceRow {
				return true
			}
		}
	}
	return false
}

func helixCompletionMenuItemVisible(frame, baseline terminalFrame, line int) bool {
	if !helixStatusHasCursorLine(frame, line) {
		return false
	}
	baselineRows := make(map[string]int)
	for _, row := range baseline.Lines {
		if helixCompletionMenuItemRow(row) {
			baselineRows[row]++
		}
	}
	for _, row := range frame.Lines {
		if !helixCompletionMenuItemRow(row) {
			continue
		}
		if baselineRows[row] == 0 {
			return true
		}
		baselineRows[row]--
	}
	return false
}

func helixCompletionMenuItemRow(row string) bool {
	fields := strings.Fields(row)
	return len(fields) >= 2 && fields[0] == "SoakTarget" && fields[1] == "function"
}

func helixCompletionSelected(frame, baseline terminalFrame, line int, expectedBufferLine string) bool {
	if !helixCompletionMenuItemVisible(frame, baseline, line) {
		return false
	}
	for _, row := range frame.Lines {
		if strings.Contains(row, expectedBufferLine) {
			return true
		}
	}
	return false
}

func helixStatusHasMode(frame terminalFrame, expected string) bool {
	for _, line := range frame.Lines {
		for _, field := range strings.Fields(line) {
			if field == expected {
				return true
			}
		}
	}
	return false
}

func awaitHelixFileContents(parent context.Context, path, expected string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	for {
		contents, err := os.ReadFile(path)
		if err == nil && string(contents) == expected {
			return contents, nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return contents, fmt.Errorf("timed out waiting for exact file contents: %w", ctx.Err())
		}
	}
}

func TestHelixHoverPopupNeedsNewSignatureLine(t *testing.T) {
	baseline := terminalFrame{Lines: []string{"4  func SoakTarget(value int) int { return value + 1 }"}}
	if helixHoverPopupVisible(baseline, baseline) {
		t.Fatal("source frame alone was accepted as a hover popup")
	}
	sameRow := terminalFrame{Lines: []string{baseline.Lines[0] + " func SoakTarget(value int) int"}}
	if helixHoverPopupVisible(sameRow, baseline) {
		t.Fatal("additional text on the source row was accepted as a hover popup")
	}
	popup := terminalFrame{Lines: []string{baseline.Lines[0], "func SoakTarget(value int) int"}}
	if !helixHoverPopupVisible(popup, baseline) {
		t.Fatal("additional rendered function signature was not accepted as a hover popup")
	}
}

func TestHelixCompletionRequiresSelectedMenuItem(t *testing.T) {
	baseline := terminalFrame{Lines: []string{
		"3  // SoakTarget is a documented function",
		"4  func SoakTarget(value int) int { return value + 1 }",
		"10 func completionProbe() int { return SoakT(3) }",
		"NOR main.go 10:38",
	}}
	if helixCompletionMenuItemVisible(baseline, baseline, 10) ||
		helixCompletionSelected(baseline, baseline, 10, "func completionProbe() int { return SoakTarget(3) }") {
		t.Fatal("source text alone was accepted as an LSP completion menu or selected item")
	}

	menuWithoutSelection := terminalFrame{Lines: []string{
		"4  func SoakTarget(value int) int { return value + 1 }",
		"SoakTarget function",
		"10 func completionProbe() int { return SoakT(3) }",
		"NOR main.go 10:38",
	}}
	if !helixCompletionMenuItemVisible(menuWithoutSelection, baseline, 10) {
		t.Fatal("distinct LSP completion menu row was not recognized")
	}
	if helixCompletionSelected(menuWithoutSelection, baseline, 10, "func completionProbe() int { return SoakTarget(3) }") {
		t.Fatal("visible but unselected completion row was accepted")
	}

	previewWithoutMenu := terminalFrame{Lines: []string{
		"10 func completionProbe() int { return SoakTarget(3) }",
		"NOR main.go 10:43",
	}}
	if helixCompletionSelected(previewWithoutMenu, baseline, 10, "func completionProbe() int { return SoakTarget(3) }") {
		t.Fatal("completion preview text without a visible menu row was accepted")
	}

	selected := terminalFrame{Lines: []string{
		"4  func SoakTarget(value int) int { return value + 1 }",
		"SoakTarget function",
		"10 func completionProbe() int { return SoakTarget(3) }",
		"NOR main.go 10:43",
	}}
	if !helixCompletionSelected(selected, baseline, 10, "func completionProbe() int { return SoakTarget(3) }") {
		t.Fatal("selected completion preview with a distinct menu row was not recognized")
	}
}

func helixReferencesListVisible(frame terminalFrame, file string) bool {
	text := frame.Text()
	return strings.Contains(text, "3/3") &&
		strings.Contains(text, file+":4") &&
		strings.Contains(text, file+":6") &&
		strings.Contains(text, file+":8")
}

func helixReferenceSelectionVisible(frame terminalFrame, file string, line int) bool {
	selected := fmt.Sprintf("> %s:%d", file, line)
	if !helixReferencesListVisible(frame, file) {
		return false
	}
	for _, row := range frame.Lines {
		if strings.Contains(row, selected) {
			return true
		}
	}
	return false
}

func TestHelixReferenceSelectionRequiresPickerLocation(t *testing.T) {
	sourceOnly := terminalFrame{Lines: []string{
		"func useTargetAgain() int { return SoakTarget(2) }",
		"main.go:8",
	}}
	if helixReferenceSelectionVisible(sourceOnly, "main.go", 8) {
		t.Fatal("source text containing the target symbol and line was accepted as a selected picker row")
	}
	filteredOut := terminalFrame{Lines: []string{"jj0/3", "no matching entries"}}
	if helixReferencesListVisible(filteredOut, "main.go") || helixReferenceSelectionVisible(filteredOut, "main.go", 8) {
		t.Fatal("picker filter text with no results was accepted as a reference selection")
	}
	unselected := terminalFrame{Lines: []string{
		"3/3",
		"> main.go:4",
		"  main.go:6",
		"  main.go:8",
	}}
	if helixReferenceSelectionVisible(unselected, "main.go", 8) {
		t.Fatal("unselected references row was accepted as the picker selection")
	}
	selected := terminalFrame{Lines: []string{
		"3/3",
		"  main.go:4",
		"  main.go:6",
		"> main.go:8",
	}}
	if !helixReferenceSelectionVisible(selected, "main.go", 8) {
		t.Fatal("selected picker location was not accepted")
	}
}

func helixReferencesResponseMatches(raw json.RawMessage, expectedURI string, expectedLines []int) bool {
	var locations []struct {
		URI   string `json:"uri"`
		Range struct {
			Start struct {
				Line int `json:"line"`
			} `json:"start"`
		} `json:"range"`
	}
	if json.Unmarshal(raw, &locations) != nil || len(locations) != len(expectedLines) {
		return false
	}
	lines := make(map[int]struct{}, len(locations))
	for _, location := range locations {
		if location.URI != expectedURI {
			return false
		}
		lines[location.Range.Start.Line] = struct{}{}
	}
	for _, line := range expectedLines {
		if _, ok := lines[line]; !ok {
			return false
		}
	}
	return true
}

func TestHelixReferencesResponseMatchesPickerLocations(t *testing.T) {
	uri := "file:///workspace/main.go"
	valid := json.RawMessage(`[
		{"uri":"file:///workspace/main.go","range":{"start":{"line":3,"character":0},"end":{"line":3,"character":10}}},
		{"uri":"file:///workspace/main.go","range":{"start":{"line":5,"character":0},"end":{"line":5,"character":10}}},
		{"uri":"file:///workspace/main.go","range":{"start":{"line":7,"character":0},"end":{"line":7,"character":10}}}
	]`)
	if !helixReferencesResponseMatches(valid, uri, []int{3, 5, 7}) {
		t.Fatal("the expected references response did not match the visible picker locations")
	}
	wrongURI := json.RawMessage(strings.ReplaceAll(string(valid), "file:///workspace/main.go", "file:///other/main.go"))
	if helixReferencesResponseMatches(wrongURI, uri, []int{3, 5, 7}) {
		t.Fatal("references in a different document were accepted")
	}
	wrongLines := json.RawMessage(strings.Replace(string(valid), `"line":7`, `"line":9`, 1))
	if helixReferencesResponseMatches(wrongLines, uri, []int{3, 5, 7}) {
		t.Fatal("a response missing the selected picker line was accepted")
	}
}

// Helix's terminal emits its document cursor location in the status line. The
// terminal's final ANSI cursor position is separate and can point at a redraw
// location, so use the status location for UI-level cursor assertions.
func helixStatusHasCursorLine(frame terminalFrame, expected int) bool {
	location := helixStatusLocation(frame)
	row, _, ok := strings.Cut(location, ":")
	return ok && row == strconv.Itoa(expected)
}

func helixStatusLocation(frame terminalFrame) string {
	for _, line := range frame.Lines {
		for _, field := range strings.Fields(line) {
			row, column, ok := strings.Cut(field, ":")
			if !ok || column == "" {
				continue
			}
			if _, err := strconv.Atoi(row); err == nil {
				if _, err := strconv.Atoi(column); err == nil {
					return field
				}
			}
		}
	}
	return ""
}

func readHelixNativeTrace(path string) (*replay.Session, map[string]int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat session trace: %w", err)
	}
	if info.Size() > helixNativeTraceLimit {
		return nil, nil, fmt.Errorf("session trace is %d bytes, exceeding %d-byte bound", info.Size(), helixNativeTraceLimit)
	}
	session, err := replay.LoadSession(path)
	if err != nil {
		return nil, nil, err
	}
	methods := make(map[string]int)
	for _, entry := range session.Entries {
		var message jsonrpc.Message
		if err := json.Unmarshal(entry.Payload, &message); err != nil {
			return nil, nil, fmt.Errorf("decode trace entry %d: %w", entry.Seq, err)
		}
		if message.Method != "" {
			methods[entry.Dir+":"+message.Method]++
		}
	}
	return session, methods, nil
}

func verifyHelixGoTrace(session *replay.Session, methods map[string]int, goURI string) error {
	for _, method := range []string{
		"in:initialize",
		"in:textDocument/didOpen",
		"in:textDocument/hover",
		"in:textDocument/definition",
		"in:textDocument/references",
		"in:textDocument/completion",
		"in:textDocument/didChange",
		"in:textDocument/rename",
		"out:textDocument/publishDiagnostics",
		"in:shutdown",
		"in:exit",
	} {
		if methods[method] == 0 {
			return fmt.Errorf("recorded Helix session did not contain %s", method)
		}
	}
	if !traceRequestReturned(session, "textDocument/completion", func(message jsonrpc.Message) bool {
		return message.Error == nil && helixJSONContainsLabel(message.Result, "SoakTarget")
	}) {
		return errors.New("Helix completion request returned no SoakTarget completion item")
	}
	if !traceRequestReturned(session, "textDocument/rename", func(message jsonrpc.Message) bool {
		return message.Error != nil && message.Error.Code == jsonrpc.RequestFailed &&
			strings.Contains(message.Error.Message, "symbol is exported; importer packages are not loaded") &&
			strings.Contains(message.Error.Message, "SEM-SAFE-001")
	}) {
		return errors.New("Helix rename request did not receive the expected RequestFailed collision refusal")
	}
	if !traceRequestReturned(session, "textDocument/hover", func(message jsonrpc.Message) bool {
		return message.Error == nil && helixJSONContainsText(message.Result, helixNativeHoverPopupMarker)
	}) {
		return errors.New("Helix hover request did not return the signature rendered in the native popup")
	}
	if !traceRequestReturned(session, "textDocument/definition", func(message jsonrpc.Message) bool {
		return message.Error == nil && len(message.Result) != 0 && string(message.Result) != "null"
	}) {
		return errors.New("Helix definition request did not return a location for the visible navigation")
	}
	if !traceRequestReturned(session, "textDocument/references", func(message jsonrpc.Message) bool {
		return message.Error == nil && helixReferencesResponseMatches(message.Result, goURI, []int{3, 5, 7})
	}) {
		return errors.New("Helix references request did not return the three Go fixture locations shown in the native picker")
	}
	if !traceNotificationContains(session, "textDocument/didChange", "SoakTarget") {
		return errors.New("Helix didChange notification did not carry the accepted SoakTarget completion edit")
	}
	if !traceNotificationContains(session, "textDocument/publishDiagnostics", "omnilspMissingSymbol") {
		return errors.New("OmniLSP did not publish the expected unresolved Go diagnostic")
	}
	return nil
}

func traceRequestReturned(session *replay.Session, method string, accept func(jsonrpc.Message) bool) bool {
	requests := make([]jsonrpc.Message, 0, 1)
	for _, entry := range session.Entries {
		if entry.Dir != "in" {
			continue
		}
		var message jsonrpc.Message
		if json.Unmarshal(entry.Payload, &message) == nil && message.Method == method && message.ID != nil {
			requests = append(requests, message)
		}
	}
	for _, request := range requests {
		for _, entry := range session.Entries {
			if entry.Dir != "out" {
				continue
			}
			var response jsonrpc.Message
			if json.Unmarshal(entry.Payload, &response) == nil && response.ID != nil &&
				response.Method == "" && response.ID.Equals(*request.ID) && accept(response) {
				return true
			}
		}
	}
	return false
}

func traceNotificationContains(session *replay.Session, method, text string) bool {
	for _, entry := range session.Entries {
		if entry.Dir != "out" {
			continue
		}
		var message jsonrpc.Message
		if json.Unmarshal(entry.Payload, &message) == nil && message.Method == method && strings.Contains(string(message.Params), text) {
			return true
		}
	}
	return false
}

func helixJSONContainsLabel(raw json.RawMessage, label string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(item any) bool {
		switch typed := item.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "label" && child == label {
					return true
				}
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func helixJSONContainsText(raw json.RawMessage, text string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(item any) bool {
		switch typed := item.(type) {
		case string:
			return strings.Contains(typed, text)
		case map[string]any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

func copyHelixNativeTrace(source, destination string) (int64, error) {
	info, err := os.Stat(source)
	if err != nil {
		return 0, err
	}
	if info.Size() > helixNativeTraceLimit {
		return 0, fmt.Errorf("session trace is %d bytes, exceeding %d-byte evidence bound", info.Size(), helixNativeTraceLimit)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return 0, err
	}
	if len(data) > helixNativeTraceLimit {
		return 0, fmt.Errorf("session trace grew beyond %d-byte evidence bound while reading", helixNativeTraceLimit)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return 0, err
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func helixFailureTraceSummary(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "unavailable: " + err.Error()
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, helixNativeTraceLimit+1))
	if err != nil {
		return "unavailable: " + err.Error()
	}
	if len(data) > helixNativeTraceLimit {
		return fmt.Sprintf("unavailable: trace exceeds %d-byte limit", helixNativeTraceLimit)
	}
	type traceRequest struct {
		method string
		id     jsonrpc.RequestID
		result string
	}
	var requests []traceRequest
	counts := make(map[string]int)
	for _, line := range strings.Split(string(data), "\n") {
		var entry struct {
			Dir     string          `json:"dir"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		var message jsonrpc.Message
		if json.Unmarshal(entry.Payload, &message) != nil {
			continue
		}
		if message.Method != "" {
			counts[entry.Dir+":"+message.Method]++
		}
		if entry.Dir == "in" && message.ID != nil && helixNativeFeatureRequest(message.Method) {
			requests = append(requests, traceRequest{method: message.Method, id: *message.ID, result: "pending"})
			continue
		}
		if entry.Dir != "out" || message.Method != "" || message.ID == nil {
			continue
		}
		for index := range requests {
			if requests[index].id.Equals(*message.ID) {
				if message.Error != nil {
					requests[index].result = fmt.Sprintf("error:%d", message.Error.Code)
				} else {
					requests[index].result = "result"
				}
			}
		}
	}
	parts := make([]string, 0, len(requests)+len(counts))
	for _, request := range requests {
		id, _ := json.Marshal(request.id)
		parts = append(parts, fmt.Sprintf("%s id=%s %s", request.method, id, request.result))
	}
	for _, method := range []string{"initialize", "initialized", "textDocument/didOpen", "textDocument/didChange", "textDocument/publishDiagnostics", "shutdown", "exit"} {
		for _, direction := range []string{"in", "out"} {
			if count := counts[direction+":"+method]; count != 0 {
				parts = append(parts, fmt.Sprintf("%s:%s=%d", direction, method, count))
			}
		}
	}
	if len(parts) == 0 {
		return "no complete JSON-RPC entries available"
	}
	return strings.Join(parts, "; ")
}

func helixNativeFeatureRequest(method string) bool {
	switch method {
	case "textDocument/hover", "textDocument/definition", "textDocument/references", "textDocument/completion", "textDocument/rename":
		return true
	default:
		return false
	}
}

func boundedHelixFailureText(value string) string {
	if len(value) <= helixNativeFailureLimit {
		return value
	}
	return strings.ToValidUTF8(value[:helixNativeFailureLimit], "") + "\n[Helix failure excerpt truncated at 8 KiB]"
}

func boundedHelixText(value string) string {
	if len(value) <= helixNativeSummaryLimit {
		return value
	}
	return value[:helixNativeSummaryLimit] + "\n[Helix evidence truncated at 256 KiB]"
}

func writeHelixNativeEvidence(directory string, observed map[string]any, tracePath string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	traceInfo, err := os.Stat(tracePath)
	if err != nil {
		return err
	}
	if traceInfo.Size() > helixNativeTraceLimit {
		return fmt.Errorf("session trace is %d bytes, exceeding %d-byte evidence bound", traceInfo.Size(), helixNativeTraceLimit)
	}
	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(directory, "helix-go-server-session.jsonl"), traceBytes, 0o600); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(observed, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "helix-go-native-evidence.json"), append(encoded, '\n'), 0o600)
}
