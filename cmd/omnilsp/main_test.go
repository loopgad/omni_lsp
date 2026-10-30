package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
)

// TestMain lets exit-code assertions re-run this test binary as a probe:
// main() (and fatal/fatalCode) call os.Exit directly, so the exit code can
// only be observed from a parent process. Each probe mode re-enters main()
// with a scripted os.Args and lets the process terminate with the very exit
// code the CLI contract (goal.md W1) assigns to that failure category.
func TestMain(m *testing.M) {
	// 第 18 轮治理开口 2 守卫的子进程半边：MCP 插件拆除测试以本测试二进制
	// 自身作为插件 entrypoint，子进程经环境变量 OMNILSP_CMD_PLUGIN_HELPER
	// 分流进最小插件协议循环（探针分流在其之后——父测试进程的 TestMain 先
	// 于测试内的 t.Setenv 执行，不会误入此分支）。
	if os.Getenv("OMNILSP_CMD_PLUGIN_HELPER") != "" {
		mcpPluginHelperMain()
		return
	}
	switch os.Getenv("OMNILSP_CLI_EXIT_PROBE") {
	case "bogus-command":
		os.Args = []string{"omnilsp", "bogus-command"}
		main() // goal.md W1: invalid CLI invocation → exit 2
	case "config-missing":
		// A --config path that cannot exist: serve must refuse to start
		// with defaults (exit 2), not silently boot the default config.
		os.Args = []string{"omnilsp", "serve", "--config",
			filepath.Join(os.TempDir(), fmt.Sprintf("omnilsp-absent-config-%d.json", os.Getpid()))}
		main()
	case "config-parse-invalid":
		// An existing but unparseable config file is invalid config (W1
		// category 2) exactly like a missing explicit config. It used to
		// fall through fatal() into the W1-undefined exit 1 — the audited
		// same-category inconsistency (missing file 2 vs parse failure 1).
		dir, mkErr := os.MkdirTemp("", "omnilsp-bad-config-")
		if mkErr != nil {
			fmt.Fprintf(os.Stderr, "probe setup: %v\n", mkErr)
			os.Exit(1)
		}
		path := filepath.Join(dir, "omnilsp.json")
		if wErr := os.WriteFile(path, []byte("{not json"), 0o644); wErr != nil {
			fmt.Fprintf(os.Stderr, "probe setup: %v\n", wErr)
			os.Exit(1)
		}
		os.Args = []string{"omnilsp", "serve", "--config", path}
		main()
	case "config-invalid-transport":
		// Pre-existing fatal() call site (invalid transport via env layer).
		// Semantics cut-over (audit batch 1): transport validation is the
		// config layer, so this now exits W1 category 2 like every other
		// invalid-config path — the former "keeps exit 1" pin froze the
		// very inconsistency the W1 mapping removes.
		os.Setenv("OMNILSP_TRANSPORT", "ws")
		os.Args = []string{"omnilsp", "serve"}
		main()
	case "index-trust-denied":
		// index's trust gate refuses the export before any server work.
		// The refusal wraps errors.ErrUntrustedOperation, so W1 category 6
		// (permission/trust error) is the only code automation may observe;
		// it used to collapse into the W1-undefined exit 1.
		ws, mkErr := os.MkdirTemp("", "omnilsp-trust-ws-")
		if mkErr != nil {
			fmt.Fprintf(os.Stderr, "probe setup: %v\n", mkErr)
			os.Exit(1)
		}
		out := filepath.Join(os.TempDir(), fmt.Sprintf("omnilsp-trust-export-%d.scip", os.Getpid()))
		os.Setenv("OMNILSP_TRUST", "untrusted")
		os.Args = []string{"omnilsp", "index", "export", "--format", "scip",
			"--scope", "probe", "--output", out, "--workspace", ws}
		main()
	case "record-unwritable":
		// --record under a directory that cannot exist: replay.NewRecorder's
		// os.Create fails with a plain filesystem error carrying no W1 kind.
		// This pins the documented residual of the W1 mapping: unclassifiable
		// failures keep exit 1 instead of guessing a category.
		os.Args = []string{"omnilsp", "serve", "--record",
			filepath.Join(os.TempDir(), fmt.Sprintf("omnilsp-no-such-dir-%d", os.Getpid()), "session.jsonl")}
		main()
	case "tcp-record":
		// Child server for TestServeTCPRecordCreatesSessionFile: the blocking
		// tcp accept loop and fatal() os.Exit calls cannot run in-process.
		os.Args = []string{"omnilsp", "serve", "--transport", "tcp",
			"--addr", os.Getenv("OMNILSP_TCP_PROBE_ADDR"),
			"--record", os.Getenv("OMNILSP_TCP_PROBE_RECORD")}
		main()
	}
	os.Exit(m.Run())
}

// captureCLIErrors swaps os.Stderr for a pipe so warn()/info() output can be
// asserted in-process. warn writes synchronously before emit returns, so a
// single ReadAll after closing the writer is race-free.
func captureCLIErrors(t *testing.T, emit func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}
	original := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = original }()
	emit()
	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("close stderr writer: %v", closeErr)
	}
	data, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatalf("read captured stderr: %v", readErr)
	}
	return string(data)
}

// TestWarnFormatsSingleVerb locks the single-verb call shape (the main.go
// registerBackends ccls warning) against the pre-rendered-string-as-format
// regression: the rendered line must contain the message exactly once.
func TestWarnFormatsSingleVerb(t *testing.T) {
	out := captureCLIErrors(t, func() {
		warn("ccls backend unavailable: %v", errors.New("clangd bridge missing"))
	})
	want := "omnilsp: WARN: ccls backend unavailable: clangd bridge missing\n"
	if out != want {
		t.Fatalf("warn single verb: got %q, want %q", out, want)
	}
}

// TestWarnFormatsMultipleVerbs locks the three-verb call shape from
// registerNested (main.go: langID, binary, error). The former bug fed the
// pre-rendered string back into Fprintf as the only argument, producing
// %!s(MISSING) noise for every verb beyond the first.
func TestWarnFormatsMultipleVerbs(t *testing.T) {
	out := captureCLIErrors(t, func() {
		warn("%s backend unavailable (%s not found): %v", "rust", "rust-analyzer", os.ErrNotExist)
	})
	want := "omnilsp: WARN: rust backend unavailable (rust-analyzer not found): file does not exist\n"
	if out != want {
		t.Fatalf("warn multiple verbs: got %q, want %q", out, want)
	}
}

// TestWarnRedactsCredentialValues keeps the §N11 guarantee that warn output
// passes the credential redactor after formatting.
func TestWarnRedactsCredentialValues(t *testing.T) {
	out := captureCLIErrors(t, func() {
		warn("connect failed: %s", "token=abc123")
	})
	want := "omnilsp: WARN: connect failed: token=[REDACTED]\n"
	if out != want {
		t.Fatalf("warn redaction: got %q, want %q", out, want)
	}
}

// TestInfoStaysUnprefixed guards the info() shape warn() was aligned with.
func TestInfoStaysUnprefixed(t *testing.T) {
	out := captureCLIErrors(t, func() {
		info("read-only http api on http://%s", "127.0.0.1:9100")
	})
	want := "omnilsp: read-only http api on http://127.0.0.1:9100\n"
	if out != want {
		t.Fatalf("info: got %q, want %q", out, want)
	}
}

type exitProbe struct {
	name       string
	mode       string
	wantCode   int
	wantStderr string
}

// TestW1ExitCodeMapping locks the error→W1 category table at the unit level
// (goal.md §W1): trust/permission → 6, build-context → 3, backend → 4,
// protocol → 7, internal → 8. Wrapped chains must classify transitively,
// and errors outside the internal/errors taxonomy keep the documented
// residual exit 1 rather than guessing a category.
func TestW1ExitCodeMapping(t *testing.T) {
	kindErr := func(kind ierrors.ErrorKind) error {
		return ierrors.New(kind, "op", "ctx")
	}
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"trust refusal maps to 6", kindErr(ierrors.ErrUntrustedOperation), 6},
		{"permission maps to 6", kindErr(ierrors.ErrPermission), 6},
		{"wrapped trust refusal classifies transitively", fmt.Errorf("index export blocked by workspace trust: %w",
			ierrors.New(ierrors.ErrUntrustedOperation, "trust.gate", "untrusted")), 6},
		{"build context maps to 3", kindErr(ierrors.ErrBuildContext), 3},
		{"backend unavailable maps to 4", kindErr(ierrors.ErrBackendUnavailable), 4},
		{"backend crashed maps to 4", kindErr(ierrors.ErrBackendCrashed), 4},
		{"protocol maps to 7", kindErr(ierrors.ErrProtocol), 7},
		{"internal maps to 8", kindErr(ierrors.ErrInternal), 8},
		{"plain error keeps residual 1", errors.New("boom"), 1},
		{"flag usage error keeps residual 1", errors.New("index: --scope is required"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := w1ExitCode(tc.err); got != tc.want {
				t.Fatalf("w1ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

// TestCLIExitCodes drives the re-executed test binary through the W1 exit
// categories the CLI actually produces: the invalid-CLI code (2) for unknown
// commands, missing configs, unparseable configs and invalid transport, the
// permission/trust code (6) for index's workspace-trust refusal, and the
// documented W1-undefined residual (1) for a failure that carries no taxonomy
// kind at all.
func TestCLIExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("exit probes re-exec the test binary; skipped in -short")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	for _, probe := range []exitProbe{
		{
			name: "unknown command exits 2", mode: "bogus-command", wantCode: 2,
			wantStderr: "unknown command: bogus-command",
		},
		{
			name: "explicit --config missing exits 2", mode: "config-missing", wantCode: 2,
			wantStderr: "config: file not found",
		},
		{
			name: "config parse failure exits 2", mode: "config-parse-invalid", wantCode: 2,
			wantStderr: "config: parse",
		},
		{
			name: "invalid transport via env exits 2 (config category)", mode: "config-invalid-transport", wantCode: 2,
			wantStderr: `config: unsupported transport "ws"`,
		},
		{
			name: "index trust refusal exits 6", mode: "index-trust-denied", wantCode: 6,
			wantStderr: "blocked by workspace trust",
		},
		{
			name: "unclassifiable record failure keeps exit 1", mode: "record-unwritable", wantCode: 1,
			wantStderr: "record:",
		},
	} {
		probe := probe
		t.Run(probe.name, func(t *testing.T) {
			cmd := exec.Command(exe)
			cmd.Env = append(os.Environ(), "OMNILSP_CLI_EXIT_PROBE="+probe.mode)
			output, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("run probe %q: %v\noutput:\n%s", probe.mode, err, output)
				}
				code = exitErr.ExitCode()
			}
			if code != probe.wantCode {
				t.Fatalf("exit code = %d, want %d\noutput:\n%s", code, probe.wantCode, output)
			}
			if !strings.Contains(string(output), probe.wantStderr) {
				t.Fatalf("output %q missing %q", output, probe.wantStderr)
			}
		})
	}
}

// TestServeMCPRecordWarnsInsteadOfSilentDrop: --record with --transport mcp
// cannot wrap the MCP projection (§P9 session recording wraps an LSP
// transport; the MCP projection serves os.Stdin/os.Stdout directly and offers
// none). Serve must say so on stderr — the audited failure mode was a silent
// drop where the .jsonl never appeared and nothing explained why.
func TestServeMCPRecordWarnsInsteadOfSilentDrop(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "session.jsonl")
	t.Setenv("OMNILSP_INDEX_DIR", filepath.Join(t.TempDir(), "index"))
	t.Setenv("OMNILSP_TRUST", "trusted")

	inRead, inWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdin pipe: %v", err)
	}
	_ = inWrite.Close() // immediate EOF: mcpserver.Serve returns, cmdServe exits
	previousStdin := os.Stdin
	os.Stdin = inRead
	defer func() { os.Stdin = previousStdin }()

	out := captureCLIErrors(t, func() {
		cmdServe([]string{"--transport", "mcp", "--record", recordPath})
	})

	if !strings.Contains(out, "WARN") || !strings.Contains(out, "--record") {
		t.Fatalf("mcp --record must warn on stderr, got %q", out)
	}
	if !strings.Contains(out, "mcp") {
		t.Fatalf("mcp --record warning must name the mcp transport, got %q", out)
	}
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("mcp --record must not create a recording file: %v", err)
	}
}

// TestServeTCPRecordCreatesSessionFile: --transport tcp --record must wrap
// the per-connection transport in a §P9 recorder — the audited failure mode
// was a .jsonl that never appeared because the record block sat behind the
// accept loop's return. Drives the re-executed test binary as the server
// child: the accept loop is infinite and fatal() exits the process, so the
// serving side cannot run in-process.
func TestServeTCPRecordCreatesSessionFile(t *testing.T) {
	if testing.Short() {
		t.Skip("tcp record probe re-execs the test binary; skipped in -short")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	recordPath := filepath.Join(t.TempDir(), "session.jsonl")
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		"OMNILSP_CLI_EXIT_PROBE=tcp-record",
		"OMNILSP_TCP_PROBE_ADDR=127.0.0.1:0",
		"OMNILSP_TCP_PROBE_RECORD="+recordPath,
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("probe stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tcp record probe: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// The child announces its ephemeral listen address before the accept loop.
	addrCh := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "omnilsp: listening on ") {
				select {
				case addrCh <- strings.TrimPrefix(line, "omnilsp: listening on "):
				default:
				}
			}
		}
	}()
	deadline := time.Now().Add(30 * time.Second)
	var addr string
	for addr == "" {
		select {
		case addr = <-addrCh:
		default:
			if time.Now().After(deadline) {
				t.Fatal("probe server never announced a listen address")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// The recorder opens on accept: the recording file must come into
	// existence as soon as a client connects (before any LSP handshake).
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial probe server: %v", err)
	}
	var body []byte
	for {
		b, rerr := os.ReadFile(recordPath)
		if rerr == nil {
			body = b
			break
		}
		if time.Now().After(deadline) {
			_ = conn.Close()
			t.Fatalf("recording never appeared at %s: tcp --record silently dropped the flag", recordPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Ending the session closes the recorder and flushes the meta header.
	_ = conn.Close()
	for len(body) == 0 {
		b, rerr := os.ReadFile(recordPath)
		if rerr == nil && len(b) > 0 {
			body = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recording at %s stayed empty after the session ended", recordPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(string(body), `"meta"`) {
		t.Fatalf("recording lacks the meta header line: %q", body)
	}
}
