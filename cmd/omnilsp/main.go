// Command omnilsp is the OmniLSP language-intelligence platform entry point.
//
// Subcommands:
//
//	serve    start the LSP server (stdio by default, tcp via --transport)
//	doctor   check the environment and report PASS/WARN/FAIL/SKIP per probe
//	replay   replay a recorded session against a fresh server instance
//	version  print version information
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof" // §P6: registers pprof handlers on DefaultServeMux
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/index/model"
	mcpserver "github.com/omnilsp/omni/internal/protocol/mcp"
	"github.com/omnilsp/omni/internal/security"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/languages"
	ccls "github.com/omnilsp/omni/internal/languages/ccls"
	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/languages/nested"
	pyright "github.com/omnilsp/omni/internal/languages/pyright"
	rustanalyzer "github.com/omnilsp/omni/internal/languages/rustanalyzer"
	typescript "github.com/omnilsp/omni/internal/languages/typescript"
	"github.com/omnilsp/omni/internal/replay"
	"github.com/omnilsp/omni/internal/runtime/server"
	"github.com/omnilsp/omni/internal/transport"
	httpserver "github.com/omnilsp/omni/internal/transport/httpserver"
)

const version = "0.2.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(0)
	}

	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "doctor":
		if err := cmdDoctor(os.Args[2:]); err != nil {
			os.Exit(w1ExitCode(err))
		}
	case "verify":
		if err := cmdVerify(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "verify: %s\n", security.RedactString(err.Error()))
			os.Exit(w1ExitCode(err))
		}
	case "replay":
		if err := cmdReplay(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "replay: %s\n", security.RedactString(err.Error()))
			os.Exit(w1ExitCode(err))
		}
	case "repro":
		if err := cmdRepro(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "repro: %s\n", security.RedactString(err.Error()))
			os.Exit(w1ExitCode(err))
		}
	case "plugins":
		if err := cmdPlugins(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "plugins: %s\n", security.RedactString(err.Error()))
			os.Exit(w1ExitCode(err))
		}
	case "index":
		if err := cmdIndex(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "index: %s\n", security.RedactString(err.Error()))
			os.Exit(w1ExitCode(err))
		}
	case "version", "--version", "-v":
		fmt.Println("omnilsp v" + version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(2) // goal.md W1: invalid CLI invocation exits 2
	}
}

func usage() {
	fmt.Println("omnilsp - OmniLSP Language Intelligence Platform")
	fmt.Println()
	fmt.Println("Usage: omnilsp <command> [flags]")
	fmt.Println()
	fmt.Println("Commands: serve | doctor | verify | replay | index | version")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  serve    Start the LSP server (--config, --transport stdio|tcp, --addr, --workspace, --plugins)")
	fmt.Println("  doctor   Probe the environment (PASS/WARN/FAIL/SKIP)")
	fmt.Println("  replay   Replay a recorded session file")
	fmt.Println("  index    Import or export a verified persistent semantic scope")
	fmt.Println("  repro    Build a reproducible bug-report bundle (.zip)")
	fmt.Println("  plugins  Manage out-of-process plugins (list/validate)")
	fmt.Println("  version  Show version information")
}

// startSessionRecorder opens the §P9 session recorder around inner and binds
// the semantic-response observer, so recording covers whichever transport the
// session runs on (§P9: "wraps whichever transport was selected"). Returns nil
// when recording is disabled (empty path). The caller owns closing the
// recorder when its session ends.
func startSessionRecorder(srv *server.Server, cfg config.Config, inner transport.Transport, recordPath string) *replay.RecorderTransport {
	if recordPath == "" {
		return nil
	}
	rec, err := replay.NewRecorder(inner, recordPath, sessionMeta(cfg, srv), srv.SnapshotRevision)
	if err != nil {
		fatal("record: %v", err)
	}
	srv.SetSemanticResponseObserver(indexedSemanticResponseBinder(srv, rec))
	info("recording session to %s", recordPath)
	return rec
}

// cmdServe wires configuration layers into a running server.
func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "path to omnilsp.json")
	transportName := fs.String("transport", "", "stdio, tcp, or mcp (overrides config)")
	addr := fs.String("addr", "", "listen address for tcp (e.g. 127.0.0.1:9301)")
	httpAddr := fs.String("http-addr", "", "also serve the read-only HTTP API on this loopback address (e.g. 127.0.0.1:9100)")
	debugAddr := fs.String("debug", "", "serve net/http/pprof on this loopback address (§P6, e.g. 127.0.0.1:6060)")
	workspace := fs.String("workspace", "", "workspace root (defaults to cwd)")
	recordPath := fs.String("record", "", "record the session to this .jsonl file (§P9; stdio and tcp transports; not supported by mcp)")
	// §O0-O5 插件车道：缺省空 = 禁用（零行为变化）。指向含插件子目录的根时，
	// serve 在 server.New 时装配 Discover → Enable → Launch（DEF-PLUGMOUNT）。
	pluginsDir := fs.String("plugins", "", "plugin root directory (mount verified plugins at startup; default: disabled)")
	_ = fs.Parse(args)

	// goal.md W1: an explicitly named config file that does not exist is an
	// invalid CLI invocation (exit 2), not a silent default start. Omitting
	// --config entirely keeps the documented "missing file → defaults win"
	// path inside config.Load.
	if *configPath != "" {
		if _, statErr := os.Stat(*configPath); os.IsNotExist(statErr) {
			fatalCode(2, "config: file not found: %s", *configPath)
		}
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		// Config layer, same W1 category as the missing-explicit-file exit
		// above: parse/read failures are invalid config, not internal faults.
		fatalCode(2, "config: %v", err)
	}
	// CLI layer wins over everything (spec: Default < Env < File < CLI).
	if *transportName != "" {
		cfg.Transport = *transportName
	}
	if *addr != "" {
		cfg.TCPAddr = *addr
	}
	if *workspace != "" {
		cfg.WorkspaceDir = *workspace
	}
	if cfg.WorkspaceDir == "" {
		wd, _ := os.Getwd()
		cfg.WorkspaceDir = wd
	}
	if err := cfg.Validate(); err != nil {
		// Validation failures are all "config: ..." rejections of the
		// layered config value — W1 category 2 (invalid CLI/config).
		fatalCode(2, "%v", err)
	}

	srv := server.New(serveConfig(cfg, *pluginsDir))

	registerBackends(srv, cfg)

	// §X6: optional read-only HTTP API alongside the main transport. Loopback
	// enforced by ValidateAddr (N8 v1); mutating endpoints do not exist.
	if *httpAddr != "" {
		if verr := httpserver.ValidateAddr(*httpAddr); verr != nil {
			fatalCode(2, "http-addr: %v", verr)
		}
		sharedCore.srv.Store(srv)
		opts := httpserver.Options{Addr: *httpAddr}
		// §X6/N8: configured read-API bearer tokens switch the HTTP surface
		// from the local-trust allow-all posture to bearer authentication.
		// An empty token set keeps the pre-existing baseline behavior.
		if tokens := cfg.ReadAPITokenSet(); len(tokens) > 0 {
			opts.Auth.Tokens = tokens
		}
		hs := httpserver.New(sharedCore, opts)
		ln, lerr := net.Listen("tcp", *httpAddr)
		if lerr != nil {
			fatal("http listen %s: %v", *httpAddr, lerr)
		}
		go func() {
			info("read-only http api on http://%s", ln.Addr())
			_ = http.Serve(ln, hs)
		}()
	}

	// §P6: optional debug/pprof endpoint. Loopback-only by ValidateAddr
	// convention; exposes runtime profiling, never workspace content.
	if *debugAddr != "" {
		if verr := httpserver.ValidateAddr(*debugAddr); verr != nil {
			fatalCode(2, "debug: %v", verr)
		}
		go func() {
			info("pprof debug server on http://%s/debug/pprof", *debugAddr)
			_ = http.ListenAndServe(*debugAddr, nil) // DefaultServeMux carries pprof via the import below
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var tr transport.Transport
	switch cfg.Transport {
	case "mcp":
		// §C14/X6: MCP tools projection over stdio, stateless per the
		// pinned 2026-07-28 revision.
		if *recordPath != "" {
			// §P9 recording wraps an LSP transport; the MCP projection reads
			// os.Stdin and writes os.Stdout directly and offers none to wrap.
			// Say so instead of silently dropping the flag (the recording file
			// would otherwise simply never appear).
			warn("--record is not supported with --transport mcp: §P9 session recording wraps an LSP transport and the MCP projection does not use one; no recording will be written")
		}
		srv.InitializeWorkspace(cfg.WorkspaceDir)
		info("serving MCP %s over stdio", mcpserver.ProtocolRevision)
		if err := serveMCP(ctx, srv, os.Stdin, os.Stdout); err != nil && ctx.Err() == nil {
			fatalErr(err, "mcp server error: %v", err)
		}
		return
	case "tcp":
		listenAddr := cfg.TCPAddr
		if listenAddr == "" {
			listenAddr = "127.0.0.1:9301"
		}
		// §N8/Y3-4: the TCP transport carries the full LSP surface with no
		// auth — it must never bind a non-loopback address. This is the same
		// fail-closed gate the HTTP and debug endpoints already pass.
		if verr := httpserver.ValidateAddr(listenAddr); verr != nil {
			fatalCode(2, "tcp-addr: %v", verr)
		}
		ln, lerr := net.Listen("tcp", listenAddr)
		if lerr != nil {
			fatal("tcp listen %s: %v", listenAddr, lerr)
		}
		fmt.Fprintf(os.Stderr, "omnilsp: listening on %s\n", ln.Addr())
		// Accept loop: a dropped client returns the listener to Ready instead
		// of killing the process — reconnect gets a fresh session server with
		// re-registered backends while the process, HTTP API and debug ports
		// stay warm. One session at a time (v1 semantics preserved); X10 may
		// later multiplex concurrent sessions over this loop.
		for session := 0; ; session++ {
			conn, aerr := ln.Accept()
			if aerr != nil {
				if errors.Is(aerr, net.ErrClosed) {
					return // listener shut down with the process
				}
				fmt.Fprintf(os.Stderr, "omnilsp: tcp accept: %v\n", aerr)
				continue
			}
			sessionSrv := server.New(serveConfig(cfg, *pluginsDir))
			registerBackends(sessionSrv, cfg)
			sharedCore.srv.Store(sessionSrv)
			runTCPSession(ctx, session, conn, sessionSrv, cfg, *recordPath)
			fmt.Fprintln(os.Stderr, "omnilsp: client disconnected; waiting for reconnect")
		}
	default:
		tr = transport.NewStdioTransportFromOS()
	}

	// §P9 session recording wraps whichever transport was selected. This path
	// serves the stdio transport; the tcp branch wraps its own per-session
	// transport above, and the mcp branch warns that recording is unsupported.
	if rec := startSessionRecorder(srv, cfg, tr, *recordPath); rec != nil {
		defer rec.Close()
		tr = rec
	}

	if err := srv.Run(ctx, tr); err != nil && ctx.Err() == nil {
		fatalErr(err, "server error: %v", err)
	}
}

// serveMCP 跑 MCP 工具投影并在会话结束后执行完整会话收尾。该分支不经过
// server.Run——Run 的 defer 链在此路径不会执行。第 18 轮治理开口 2 补了插件
// 腿（ClosePlugins）；审计批 2 发现 2 补齐其余腿：closeBackends（嵌套后端
// 唯一拆除点，含优雅 shutdown→exit 握手与 kill 兜底）、diag.Close、
// scheduler.Shutdown——四腿统一走 server.CloseSession（与 Run defer 链同序、
// 同幂等）。收尾必须显式在 fatalErr（os.Exit）之前完成——os.Exit 不跑 defer；
// 收尾错误并入返回值，与 Run 路径把 closeBackends 错误并入 runErr 的既有
// 语义一致。
func serveMCP(ctx context.Context, srv *server.Server, in io.Reader, out io.Writer) error {
	err := mcpserver.Serve(ctx, in, out, mcpCore{srv: srv})
	return errors.Join(err, srv.CloseSession())
}

// runTCPSession serves one accepted TCP connection to completion. Each TCP
// connection is one session (v1: one at a time); the recorder wraps the
// per-connection transport and the recording file is recreated per session,
// so it is closed here rather than with a defer — a defer inside the accept
// loop would only run at cmdServe exit and hold every recording open.
func runTCPSession(ctx context.Context, session int, conn net.Conn, srv *server.Server, cfg config.Config, recordPath string) {
	tr := transport.NewTCPTransport(conn)
	var sessionTr transport.Transport = tr
	var rec *replay.RecorderTransport
	if rec = startSessionRecorder(srv, cfg, tr, recordPath); rec != nil {
		sessionTr = rec
	}
	if rerr := srv.Run(ctx, sessionTr); rerr != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "omnilsp: session %d ended: %v\n", session, rerr)
	}
	if rec != nil {
		_ = rec.Close()
	}
	// Teardown must close the accepted connection no matter how the session
	// ended (audit finding 7): the exit handshake closes the transport from
	// the server side, but a clean EOF, a transport fault or a cancellation
	// used to leave the conn open and the transport's readLoop goroutine
	// blocked in ReadFrame — one fd and one goroutine leaked per reconnect.
	// Close is idempotent (closeOnce), and it unblocks readLoop, which then
	// exits through its own done channel.
	_ = tr.Close()
}

// registerBackends discovers available language toolchains and registers the
// corresponding backends. A missing toolchain is a warning, never fatal:
// partial capability beats no server.
func registerBackends(srv *server.Server, cfg config.Config) {
	enabled := func(lang string) bool {
		for _, b := range cfg.Backends {
			if b.LanguageID == lang && !b.Enabled {
				return false
			}
		}
		return true
	}

	workDir, _ := filepath.Abs(cfg.WorkspaceDir)
	if enabled("go") {
		gb := golang.New(workDir)
		srv.RegisterBackend(gb.LanguageID(), gb)
		// Explicit semantic index registration: the Go exporter can prove
		// complete dirty-snapshot answers, so dirty Go workspace/symbol
		// requests fail closed instead of serving stale disk symbols.
		srv.RegisterSemanticIndexProvider(gb.LanguageID(), gb, gb)
		info("go backend registered (workdir=%s)", workDir)
	}
	if enabled("cpp") || enabled("c") {
		cb, cerr := ccls.New(workDir)
		if cerr != nil {
			warn("ccls backend unavailable: %v", cerr)
		} else {
			srv.RegisterBackend(cb.LanguageID(), cb)
			srv.RegisterBackend("c", cb)
			// Explicit semantic index registration (behavior-authorized): the
			// dirty cpp workspace/symbol guard now trusts this capability, so
			// dirty cpp requests fail closed (or answer snapshot-exact when
			// the exporter proves completeness) instead of serving stale disk
			// symbols. Both family keys bind the same capability; the server
			// keeps enumerating it under the canonical cpp key.
			registerCclsSemanticIndex(srv, cb)
			info("ccls backend registered (clangd bridge)")
		}
	}
	registerNested(srv, enabled, workDir,
		nestedEntry{"rust", "rs", "rust-analyzer", wrap(rustanalyzer.New)},
		nestedEntry{"python", "py", "pyright-langserver", wrap(pyright.New)},
		nestedEntry{"typescript", "ts", "typescript-language-server", wrap(typescript.New)},
	)
}

func wrap[T languages.Backend](f func(string) (T, error)) func(string) (languages.Backend, error) {
	return func(dir string) (languages.Backend, error) { return f(dir) }
}

// registerCclsSemanticIndex explicitly registers the ccls exporter capability
// under both language keys the bridge serves: the canonical "cpp" backend
// identity and the "c" alias. The dirty workspace/symbol guard consults the
// canonical key; the alias keeps the capability resolvable for c-family
// document overlays. The registered key set is pinned by
// TestRegisterCclsSemanticIndexBindsCanonicalAndAliasKeys.
func registerCclsSemanticIndex(srv *server.Server, cb *ccls.Backend) {
	srv.RegisterSemanticIndexProvider(cb.LanguageID(), cb, cb)
	srv.RegisterSemanticIndexProvider("c", cb, cb)
}

// typescriptSemanticIndexLanguages lists every language key the pinned
// TypeScript exporter serves. The provider normalizes the whole TS/JS dialect
// family (typescript, javascript, typescriptreact, javascriptreact), so each
// dialect gets its own explicit binding key while the server keeps
// enumerating one capability under the canonical typescript key. Pinned by
// TestTypescriptSemanticIndexLanguagesCoverJSFamily.
var typescriptSemanticIndexLanguages = []string{"typescript", "javascript", "typescriptreact", "javascriptreact"}

// nestedEntry describes one optional nested-LSP language pack. A missing
// toolchain is a doctor-visible warning, never a fatal serve error (§D0:
// degraded workspace stays structurally usable).
type nestedEntry struct {
	langID string
	short  string
	binary string
	newFn  func(string) (languages.Backend, error)
}

func registerNested(srv *server.Server, enabled func(string) bool, workDir string, entries ...nestedEntry) {
	// Concurrent spawn+initialize: three toolchain bridges start in parallel
	// instead of serially, cutting time-to-ready by the slowest (not the sum).
	// Semantic-index pinning (rustup probing, tool identity hashing, TS/Py
	// provider construction) runs inside each spawn goroutine too, so the
	// pinning chain costs the slowest bridge rather than their sum on the TCP
	// accept loop; every subprocess on this path carries a deadline
	// (runProbe/toolchainPinTimeout), so a hung toolchain degrades through the
	// warn path instead of blocking accept indefinitely (audit batch 1).
	// Registration happens after all factories settle, keeping map writes on
	// one goroutine.
	type settled struct {
		e  nestedEntry
		be languages.Backend
		// semanticLangs/provider/planner carry the pinning product built during
		// spawn. provider nil means the language has no semantic provider or
		// pinning failed (already warned); registration still happens after
		// settle, in results order. semanticLangs lists every language key the
		// provider serves (the TypeScript exporter covers the whole JS dialect
		// family, so it registers four keys).
		semanticLangs []string
		provider      languages.SemanticIndexProvider
		planner       languages.SemanticIndexRequestBuilder
		providerNote  string
	}
	results := make(chan settled, len(entries))
	var wg sync.WaitGroup
	for _, e := range entries {
		if !enabled(e.langID) {
			continue
		}
		wg.Add(1)
		go func(e nestedEntry) {
			defer wg.Done()
			be, err := e.newFn(workDir)
			if err != nil {
				warn("%s backend unavailable (%s not found): %v", e.langID, e.binary, err)
				return
			}
			st := settled{e: e, be: be}
			switch backend := be.(type) {
			case *rustanalyzer.Backend:
				toolConfig, configErr := rustSemanticIndexToolConfig()
				if configErr != nil {
					warn("Rust semantic index unavailable: %v", configErr)
				} else {
					st.semanticLangs = []string{"rust"}
					st.provider = backend
					st.planner = rustanalyzer.NewSemanticIndexRequestBuilder(backend, toolConfig)
					st.providerNote = "Rust semantic index provider registered with pinned tool identities"
				}
			case *typescript.Backend:
				pinCtx, cancel := context.WithTimeout(context.Background(), toolchainPinTimeout)
				provider, providerErr := typescript.NewPinnedSemanticIndexProvider(pinCtx, backend)
				cancel()
				if providerErr != nil {
					warn("TypeScript/JavaScript semantic index unavailable: %v", providerErr)
				} else {
					// Explicit js-family registration: the pinned TypeScript
					// capability is bound under every dialect key it serves,
					// so dirty js/jsx documents resolve the same exporter as
					// typescript documents.
					st.semanticLangs = typescriptSemanticIndexLanguages
					st.provider = provider
					st.planner = provider
					st.providerNote = "TypeScript/JavaScript semantic index provider registered with pinned Node, compiler, and candidate identities"
				}
			case *pyright.Backend:
				pinCtx, cancel := context.WithTimeout(context.Background(), toolchainPinTimeout)
				provider, providerErr := pyright.NewRuntimeSemanticIndexProvider(pinCtx, backend)
				cancel()
				if providerErr != nil {
					warn("Python semantic index unavailable: %v", providerErr)
				} else {
					st.semanticLangs = []string{"python"}
					st.provider = provider
					st.planner = provider
					st.providerNote = "Python semantic index provider registered with pinned analyzer identities"
				}
			}
			results <- st
		}(e)
	}
	wg.Wait()
	close(results)
	for st := range results {
		srv.RegisterBackend(st.be.LanguageID(), st.be)
		if st.be.LanguageID() != st.e.short {
			srv.RegisterBackend(st.e.short, st.be) // short alias, e.g. "py" → python
		}
		if st.provider != nil {
			for _, lang := range st.semanticLangs {
				srv.RegisterSemanticIndexProvider(lang, st.provider, st.planner)
			}
			info("%s", st.providerNote)
		}
		info("%s backend registered (%s bridge)", st.e.langID, st.e.binary)
	}
}

const (
	// toolchainProbeTimeout bounds ONE toolchain subprocess probe; rustPinBudget
	// bounds the whole pinning chain (probes plus identity hashing). Both run
	// on the TCP accept-loop critical path (registerBackends per session), so
	// a hung rustup/rustc or a huge binary hash must degrade the semantic
	// index through the warn path instead of stalling accept indefinitely
	// (audit batch 1: the old exec calls had no context and no timeout).
	toolchainProbeTimeout = 30 * time.Second
	rustPinBudget         = 2 * time.Minute
	// toolchainPinTimeout bounds TS/Py pinned-provider construction the same
	// way (probe + identity hashing inside those constructors).
	toolchainPinTimeout = 2 * time.Minute
)

// runProbe executes name/args under a hard deadline: a hung toolchain is
// killed and surfaces as a context error (the same warn-and-degrade path as a
// missing toolchain), never as an unbounded wait.
func runProbe(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, name, args...).Output()
	if err != nil {
		if perr := probeCtx.Err(); perr != nil {
			return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), perr)
		}
		return out, err
	}
	return out, nil
}

func rustSemanticIndexToolConfig() (rustanalyzer.RustIndexToolConfig, error) {
	// One bounded context for the whole pinning chain; every subprocess probe
	// additionally carries its own deadline. A missing or hung toolchain is
	// the same warn-and-degrade path as any other backend gap — never a hang.
	ctx, cancel := context.WithTimeout(context.Background(), rustPinBudget)
	defer cancel()
	rustupPath, err := exec.LookPath("rustup")
	if err != nil {
		return rustanalyzer.RustIndexToolConfig{}, fmt.Errorf("rustup toolchain resolver not found: %w", err)
	}
	toolchainPaths := make(map[string]string, 3)
	for _, name := range []string{"cargo", "rustc", "rust-analyzer"} {
		output, runErr := runProbe(ctx, toolchainProbeTimeout, rustupPath, "which", name)
		if runErr != nil {
			return rustanalyzer.RustIndexToolConfig{}, fmt.Errorf("rustup which %s: %w", name, runErr)
		}
		toolchainPaths[name] = filepath.Clean(strings.TrimSpace(string(output)))
		if toolchainPaths[name] == "" || !filepath.IsAbs(toolchainPaths[name]) {
			return rustanalyzer.RustIndexToolConfig{}, fmt.Errorf("rustup returned an invalid %s path", name)
		}
	}

	tools := make([]model.ToolIdentity, 0, 3)
	versions := make(map[string]string, 2)
	for _, name := range []string{"cargo", "rustc", "rust-analyzer"} {
		path, digest, identityErr := nested.ExecutableIdentityContext(ctx, toolchainPaths[name])
		if identityErr != nil {
			return rustanalyzer.RustIndexToolConfig{}, fmt.Errorf("identify pinned %s: %w", name, identityErr)
		}
		output, versionErr := runProbe(ctx, toolchainProbeTimeout, path, "--version")
		if versionErr != nil || strings.TrimSpace(string(output)) == "" {
			return rustanalyzer.RustIndexToolConfig{}, fmt.Errorf("probe pinned %s version: %w", name, versionErr)
		}
		version := strings.TrimSpace(string(output))
		versions[name] = version
		if name != "rust-analyzer" {
			tools = append(tools, model.ToolIdentity{Name: name, Path: path, Version: version, SHA256: digest})
		}
	}
	sysrootOutput, err := runProbe(ctx, toolchainProbeTimeout, toolchainPaths["rustc"], "--print", "sysroot")
	if err != nil {
		return rustanalyzer.RustIndexToolConfig{}, fmt.Errorf("find pinned Rust sysroot: %w", err)
	}
	procMacroPath := filepath.Join(strings.TrimSpace(string(sysrootOutput)), "libexec", "rust-analyzer-proc-macro-srv.exe")
	procMacroPath, procMacroDigest, err := nested.ExecutableIdentityContext(ctx, procMacroPath)
	if err != nil {
		return rustanalyzer.RustIndexToolConfig{}, fmt.Errorf("identify pinned Rust proc-macro server: %w", err)
	}
	tools = append(tools, model.ToolIdentity{
		// rust-analyzer-proc-macro-srv uses a protocol on stdin and has no
		// standalone --version mode; its exact file hash and containing pinned
		// rustc toolchain version identify it without starting a server process.
		Name: "proc-macro-srv", Path: procMacroPath, Version: "bundled with " + versions["rustc"], SHA256: procMacroDigest,
	})
	rustcIdentity := sha256.Sum256([]byte(versions["rustc"] + "\x00" + versions["cargo"] + "\x00" + versions["rust-analyzer"]))
	helperPath := os.Getenv("OMNILSP_RUST_SCIP_HELPER_PATH")
	helperVersion := os.Getenv("OMNILSP_RUST_SCIP_HELPER_VERSION")
	helperSHA := os.Getenv("OMNILSP_RUST_SCIP_HELPER_SHA256")
	var helper model.ToolIdentity
	if helperPath != "" || helperVersion != "" || helperSHA != "" {
		if helperPath == "" || helperVersion == "" || helperSHA == "" {
			return rustanalyzer.RustIndexToolConfig{}, fmt.Errorf("Rust semantic helper requires path, version and SHA-256 together")
		}
		helper = model.ToolIdentity{Name: "rust-analyzer-semantic-helper", Path: helperPath, Version: helperVersion, SHA256: strings.ToLower(helperSHA)}
	}
	return rustanalyzer.RustIndexToolConfig{
		SemanticHelper: helper,
		Tools:          tools,
		Build:          model.BuildInputs{Options: map[string]string{"procMacros": "true", "buildScripts": "true"}},
		Toolchain:      "sha256:" + hex.EncodeToString(rustcIdentity[:]),
	}, nil
}

// w1ExitCode maps an error onto the goal.md §W1 CLI exit-code categories
// (goal.md §W1: 0 success, 2 invalid CLI/config, 3 workspace/build-context
// error, 4 backend unavailable, 5 semantic result unavailable, 6
// permission/trust error, 7 protocol/remote error, 8 internal error).
// Classification rides the internal/errors taxonomy, so wrapped refusals
// (e.g. index's trust gate wrapping ErrUntrustedOperation) classify
// transitively. Errors carrying no recognizable kind — flag/usage text,
// raw filesystem and net errors — keep exit 1, the W1-undefined residual:
// W1 predates most of the error taxonomy, category 5 has no CLI producer
// yet, and an unmapped failure stays on the historical default rather than
// guessing a category. Category 2 is assigned at the config/CLI call sites
// that know their layer statically (fatalCode), not inferred from the error.
func w1ExitCode(err error) int {
	switch {
	case ierrors.IsKind(err, ierrors.ErrUntrustedOperation), ierrors.IsKind(err, ierrors.ErrPermission):
		return 6 // W1: permission/trust error
	case ierrors.IsKind(err, ierrors.ErrBuildContext):
		return 3 // W1: workspace/build-context error
	case ierrors.IsKind(err, ierrors.ErrBackendUnavailable), ierrors.IsKind(err, ierrors.ErrBackendCrashed):
		return 4 // W1: backend unavailable
	case ierrors.IsKind(err, ierrors.ErrProtocol):
		return 7 // W1: protocol/remote error
	case ierrors.IsKind(err, ierrors.ErrInternal):
		return 8 // W1: internal error
	default:
		return 1
	}
}

func fatal(f string, args ...any) {
	// Unclassified fatal: the W1-undefined residual exit. Call sites holding
	// an error value use fatalErr so the §W1 category mapping applies; call
	// sites that know their category statically use fatalCode.
	exitFatal(1, f, args...)
}

// fatalErr renders a fatal diagnostic and exits with the goal.md §W1
// category of err (see w1ExitCode). Raw errors outside the taxonomy resolve
// to the residual exit 1.
func fatalErr(err error, f string, args ...any) {
	exitFatal(w1ExitCode(err), f, args...)
}

// fatalCode is the exit-code-aware variant of fatal for call sites whose W1
// category is known statically from the failing layer (config/CLI validation
// → 2) regardless of the underlying error's taxonomy kind.
func fatalCode(code int, f string, args ...any) {
	exitFatal(code, f, args...)
}

func exitFatal(code int, f string, args ...any) {
	// §N11: even fatal diagnostics pass the credential redactor.
	fmt.Fprint(os.Stderr, "omnilsp: "+security.RedactString(fmt.Sprintf(f, args...))+"\n")
	os.Exit(code)
}

// sessionMeta assembles the §P9 header record: config digest plus the
// backend/toolchain inventory discovered at serve time.
func sessionMeta(cfg config.Config, srv *server.Server) replay.Meta {
	tool := map[string]string{}
	if out, err := exec.Command("go", "version").Output(); err == nil {
		tool["go"] = firstLine(string(out))
	}
	if out, err := exec.Command("clangd", "--version").Output(); err == nil {
		tool["clangd"] = firstLine(string(out))
	}
	backends := map[string]string{}
	for _, b := range cfg.Backends {
		if b.Enabled {
			backends[b.LanguageID] = b.BinaryPath
		}
	}
	return replay.Meta{
		ConfigHash:   replay.ConfigHash(cfg),
		Toolchain:    tool,
		Backends:     backends,
		WorkspaceDir: cfg.WorkspaceDir,
	}
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			return s[:i]
		}
	}
	return s
}

// §N11: every CLI log line passes the credential redactor before hitting
// stderr — workspace names, config paths and backend output can carry tokens.
// warn renders f with args first (same shape as info), then redacts; the
// rendered string must never be fed back through f as a format argument.
func warn(f string, args ...any) {
	fmt.Fprint(os.Stderr, "omnilsp: WARN: "+security.RedactString(fmt.Sprintf(f, args...))+"\n")
}

func info(f string, args ...any) {
	fmt.Fprint(os.Stderr, "omnilsp: "+security.RedactString(fmt.Sprintf(f, args...))+"\n")
}

// sharedCore is the HTTP read API's view of the active session server. The
// TCP accept loop swaps it per reconnect; the HTTP listener outlives sessions.
var sharedCore = &httpCore{}
