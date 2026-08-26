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
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof" // §P6: registers pprof handlers on DefaultServeMux
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	mcpserver "github.com/omnilsp/omni/internal/protocol/mcp"
	"github.com/omnilsp/omni/internal/security"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/languages"
	ccls "github.com/omnilsp/omni/internal/languages/ccls"
	"github.com/omnilsp/omni/internal/languages/golang"
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
			os.Exit(1)
		}
	case "verify":
		if err := cmdVerify(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "verify: %v\n", err)
			os.Exit(1)
		}
	case "replay":
		if err := cmdReplay(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "replay: %v\n", err)
			os.Exit(1)
		}
	case "repro":
		if err := cmdRepro(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "repro: %v\n", err)
			os.Exit(1)
		}
	case "plugins":
		if err := cmdPlugins(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "plugins: %v\n", err)
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Println("omnilsp v" + version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println("omnilsp - OmniLSP Language Intelligence Platform")
	fmt.Println()
	fmt.Println("Usage: omnilsp <command> [flags]")
	fmt.Println()
	fmt.Println("Commands: serve | doctor | verify | replay | version")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  serve    Start the LSP server (--config, --transport stdio|tcp, --addr, --workspace)")
	fmt.Println("  doctor   Probe the environment (PASS/WARN/FAIL/SKIP)")
	fmt.Println("  replay   Replay a recorded session file")
	fmt.Println("  repro    Build a reproducible bug-report bundle (.zip)")
	fmt.Println("  plugins  Manage out-of-process plugins (list/validate)")
	fmt.Println("  version  Show version information")
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
	recordPath := fs.String("record", "", "record the session to this .jsonl file (§P9)")
	_ = fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("config: %v", err)
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
		fatal("%v", err)
	}

	srvCfg := server.DefaultConfig()
	srvCfg.Scheduler.MaxConcurrent = cfg.MaxConcurrentRequests
	srvCfg.Scheduler.MaxQueueSize = cfg.MaxQueueSize
	srv := server.New(srvCfg)

	registerBackends(srv, cfg)

	// §X6: optional read-only HTTP API alongside the main transport. Loopback
	// enforced by ValidateAddr (N8 v1); mutating endpoints do not exist.
	if *httpAddr != "" {
		if verr := httpserver.ValidateAddr(*httpAddr); verr != nil {
			fatal("http-addr: %v", verr)
		}
		sharedCore.srv.Store(srv)
		hs := httpserver.New(sharedCore, httpserver.Options{Addr: *httpAddr})
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
			fatal("debug: %v", verr)
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
		info("serving MCP %s over stdio", mcpserver.ProtocolRevision)
		if err := mcpserver.Serve(ctx, os.Stdin, os.Stdout, mcpCore{srv: srv}); err != nil && ctx.Err() == nil {
			fatal("mcp server error: %v", err)
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
			fatal("tcp-addr: %v", verr)
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
			sessionSrv := server.New(srvCfg)
			registerBackends(sessionSrv, cfg)
			sharedCore.srv.Store(sessionSrv)
			tr = transport.NewTCPTransport(conn)
			if rerr := sessionSrv.Run(ctx, tr); rerr != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "omnilsp: session %d ended: %v\n", session, rerr)
			}
			fmt.Fprintln(os.Stderr, "omnilsp: client disconnected; waiting for reconnect")
		}
	default:
		tr = transport.NewStdioTransportFromOS()
	}

	// §P9 session recording wraps whichever transport was selected.
	if *recordPath != "" {
		rec, rerr := replay.NewRecorder(tr, *recordPath, sessionMeta(cfg, srv), srv.SnapshotRevision)
		if rerr != nil {
			fatal("record: %v", rerr)
		}
		defer rec.Close()
		tr = rec
		info("recording session to %s", *recordPath)
	}

	if err := srv.Run(ctx, tr); err != nil && ctx.Err() == nil {
		fatal("server error: %v", err)
	}
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
		info("go backend registered (workdir=%s)", workDir)
	}
	if enabled("cpp") || enabled("c") {
		cb, cerr := ccls.New(workDir)
		if cerr != nil {
			warn("ccls backend unavailable: %v", cerr)
		} else {
			srv.RegisterBackend(cb.LanguageID(), cb)
			srv.RegisterBackend("c", cb)
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
	// Registration happens after all factories settle, keeping map writes on
	// one goroutine.
	type settled struct {
		e  nestedEntry
		be languages.Backend
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
			results <- settled{e: e, be: be}
		}(e)
	}
	wg.Wait()
	close(results)
	for st := range results {
		srv.RegisterBackend(st.be.LanguageID(), st.be)
		if st.be.LanguageID() != st.e.short {
			srv.RegisterBackend(st.e.short, st.be) // short alias, e.g. "py" → python
		}
		info("%s backend registered (%s bridge)", st.e.langID, st.e.binary)
	}
}

func fatal(f string, args ...any) {
	// §N11: even fatal diagnostics pass the credential redactor.
	fmt.Fprint(os.Stderr, "omnilsp: "+security.RedactString(fmt.Sprintf(f, args...))+"\n")
	os.Exit(1)
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
func warn(f string, args ...any) {
	fmt.Fprintf(os.Stderr, "omnilsp: WARN: "+f+"\n", security.RedactString(fmt.Sprintf(f, args...)))
}

func info(f string, args ...any) {
	fmt.Fprint(os.Stderr, "omnilsp: "+security.RedactString(fmt.Sprintf(f, args...))+"\n")
}

// sharedCore is the HTTP read API's view of the active session server. The
// TCP accept loop swaps it per reconnect; the HTTP listener outlives sessions.
var sharedCore = &httpCore{}
