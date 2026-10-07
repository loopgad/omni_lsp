package main

// omnilsp doctor — environment probes per goal.md §P8.
//
// v1 probe set (spec says "as applicable"): version, os/arch, config parse,
// workspace readability, Go toolchain, clangd bridge, compile database,
// cache directory writability, TCP bind check, disk space. The disk probe
// shells out to a per-platform syscall (doctor_disk_windows.go /
// doctor_disk_unix.go) and SKIPs only when that call itself fails.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/omnilsp/omni/internal/conformance"

	"github.com/omnilsp/omni/internal/config"
)

const (
	statusPass = "PASS"
	statusWarn = "WARN"
	statusFail = "FAIL"
	statusSkip = "SKIP"
)

type probe struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// doctorReport is the versioned machine-readable shape (goal.md W2).
type doctorReport struct {
	Schema      string                   `json:"schema"`
	Checks      []probe                  `json:"checks"`
	Conformance *conformance.CoreSummary `json:"conformance,omitempty"`
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := fs.String("config", "", "path to omnilsp.json")
	workspace := fs.String("workspace", "", "workspace root to probe")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON (schema omnilsp.doctor.v1)")
	_ = fs.Parse(args)

	var failed bool
	var probes []probe
	collect := func(p probe) {
		if p.Status == statusFail {
			failed = true
		}
		probes = append(probes, p)
	}

	collect(probe{"version", statusPass, "omnilsp v" + version})
	collect(probe{"os/arch", statusPass, runtime.GOOS + "/" + runtime.GOARCH})

	cfg, err := config.Load(*configPath)
	if err != nil {
		collect(probe{"config", statusFail, err.Error()})
	} else if verr := cfg.Validate(); verr != nil {
		collect(probe{"config", statusFail, verr.Error()})
	} else {
		collect(probe{"config", statusPass, "transport=" + cfg.Transport})
	}

	// Workspace trust/readability.
	ws := *workspace
	if ws == "" {
		if cfg.WorkspaceDir != "" {
			ws = cfg.WorkspaceDir
		} else {
			ws, _ = os.Getwd()
		}
	}
	if st, serr := os.Stat(ws); serr != nil || !st.IsDir() {
		collect(probe{"workspace", statusFail, fmt.Sprintf("%s unreadable: %v", ws, serr)})
	} else {
		collect(probe{"workspace", statusPass, ws})
	}

	// Go toolchain.
	if goPath, lerr := exec.LookPath("go"); lerr != nil {
		collect(probe{"go toolchain", statusWarn, "go not found — Go semantics disabled"})
	} else {
		out, rerr := exec.Command(goPath, "version").Output()
		if rerr != nil {
			collect(probe{"go toolchain", statusWarn, "go version failed: " + rerr.Error()})
		} else {
			collect(probe{"go toolchain", statusPass, trimLine(string(out))})
		}
	}

	// clangd bridge.
	if _, lerr := exec.LookPath("clangd"); lerr != nil {
		collect(probe{"clangd", statusWarn, "clangd not found — C/C++ semantics disabled"})
	} else {
		out, rerr := exec.Command("clangd", "--version").Output()
		if rerr != nil {
			collect(probe{"clangd", statusWarn, "clangd --version failed"})
		} else {
			collect(probe{"clangd", statusPass, trimLine(string(out))})
		}
	}

	// Rust toolchain (X4): rust-analyzer rides on rustc's identity.
	if _, lerr := exec.LookPath("rustc"); lerr != nil {
		collect(probe{"rust toolchain", statusSkip, "rustc not found — Rust language pack inactive"})
	} else {
		out, rerr := exec.Command("rustc", "--version").Output()
		if rerr != nil {
			collect(probe{"rust toolchain", statusWarn, "rustc --version failed"})
		} else {
			collect(probe{"rust toolchain", statusPass, trimLine(string(out))})
		}
	}

	// Python (X4): pyright probes the interpreter; some distros ship python3 only.
	pyBin := "python"
	if _, lerr := exec.LookPath(pyBin); lerr != nil {
		pyBin = "python3"
	}
	if _, lerr := exec.LookPath(pyBin); lerr != nil {
		collect(probe{"python", statusSkip, "python/python3 not found — Python language pack inactive"})
	} else {
		out, rerr := exec.Command(pyBin, "--version").CombinedOutput()
		if rerr != nil {
			collect(probe{"python", statusWarn, pyBin + " --version failed"})
		} else {
			collect(probe{"python", statusPass, trimLine(string(out))})
		}
	}

	// Node/tsc (X4): typescript-language-server needs node; tsc is its identity probe.
	if _, lerr := exec.LookPath("node"); lerr != nil {
		collect(probe{"node/tsc", statusSkip, "node not found — TypeScript language pack inactive"})
	} else {
		out, rerr := exec.Command("node", "--version").Output()
		if rerr != nil {
			collect(probe{"node/tsc", statusWarn, "node --version failed"})
		} else {
			detail := trimLine(string(out))
			if tout, terr := exec.Command("tsc", "--version").CombinedOutput(); terr == nil {
				detail += ", tsc " + trimLine(string(tout))
			} else {
				detail += ", tsc not found"
			}
			collect(probe{"node/tsc", statusPass, detail})
		}
	}

	// Compile database presence (C/C++ only meaningful when clangd exists).
	ccdb := filepath.Join(ws, "build", "compile_commands.json")
	switch _, serr := os.Stat(ccdb); {
	case serr == nil:
		collect(probe{"compile db", statusPass, ccdb})
	case os.IsNotExist(serr):
		collect(probe{"compile db", statusWarn, ccdb + " missing — C/C++ rename stays fail-closed"})
	default:
		collect(probe{"compile db", statusWarn, ccdb + ": " + serr.Error()})
	}

	// Cache directory writability.
	if cd, cerr := os.UserCacheDir(); cerr != nil {
		collect(probe{"cache dir", statusWarn, "no user cache dir: " + cerr.Error()})
	} else {
		dir := filepath.Join(cd, "omnilsp")
		if merr := os.MkdirAll(dir, 0o755); merr != nil {
			collect(probe{"cache dir", statusFail, dir + ": " + merr.Error()})
		} else if terr := touchFile(filepath.Join(dir, ".probe")); terr != nil {
			collect(probe{"cache dir", statusFail, dir + " not writable: " + terr.Error()})
		} else {
			collect(probe{"cache dir", statusPass, dir})
		}
	}

	// TCP bind conflict check (only when tcp transport configured).
	if cfg.Transport == "tcp" {
		listenAddr := cfg.TCPAddr
		if listenAddr == "" {
			listenAddr = "127.0.0.1:9301"
		}
		ln, berr := net.Listen("tcp", listenAddr)
		if berr != nil {
			collect(probe{"port", statusFail, listenAddr + ": " + berr.Error()})
		} else {
			ln.Close()
			collect(probe{"port", statusPass, listenAddr + " bindable"})
		}
	} else {
		collect(probe{"port", statusSkip, "stdio transport — no port needed"})
	}

	// Disk space (X9): real free/total bytes via a per-platform syscall
	// (doctor_disk_windows.go / doctor_disk_unix.go). Probed on the cache
	// volume when resolvable — the index cache is what grows unbounded —
	// falling back to the workspace volume. A failed probe is SKIP (the
	// environment limits us), not FAIL: the disk could be fine.
	diskPath := ws
	if cd, cerr := os.UserCacheDir(); cerr == nil {
		diskPath = filepath.Join(cd, "omnilsp")
	}
	if free, total, derr := diskSpace(diskPath); derr != nil {
		collect(probe{"disk space", statusSkip, fmt.Sprintf("%s: %v", diskPath, derr)})
	} else {
		collect(probe{"disk space", statusPass, fmt.Sprintf("%d bytes free of %d (%s)", free, total, diskPath)})
	}

	if *jsonOut {
		summary, sumErr := conformance.FastCoreSummary(".")
		if sumErr != nil {
			summary = nil // doctor's primary duty is env probes; scoring is additive
		}
		if err := renderJSON(os.Stdout, probes, summary); err != nil {
			return err
		}
	} else {
		renderText(os.Stdout, probes)
	}
	if failed {
		return fmt.Errorf("doctor: at least one probe FAILED")
	}
	return nil
}

// renderText writes the human-readable one-line-per-probe report.
func renderText(w io.Writer, probes []probe) {
	for _, p := range probes {
		fmt.Fprintf(w, "%-4s %-22s %s\n", p.Status, p.Name, p.Detail)
	}
}

// renderJSON writes the W2 versioned report. Pure JSON on w; any human
// chatter must go to stderr upstream to keep stdout machine-parseable.
func renderJSON(w io.Writer, probes []probe, conf *conformance.CoreSummary) error {
	b, err := json.MarshalIndent(doctorReport{Schema: "omnilsp.doctor.v1", Checks: probes, Conformance: conf}, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func touchFile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return f.Close()
}

func trimLine(s string) string {
	for i, r := range s {
		if r == '\n' || r == '\r' {
			return s[:i]
		}
	}
	return s
}
