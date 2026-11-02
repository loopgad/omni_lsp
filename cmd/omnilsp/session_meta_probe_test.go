package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/replay"
	"github.com/omnilsp/omni/internal/runtime/server"
)

// TestSessionMetaProbeIsBoundedWhenToolchainHangs pins bug round 20 A#10
// (圈 4): sessionMeta probed toolchain versions with bare
// exec.Command().Output() — no deadline, no ctx. On the serve --record path
// sessionMeta runs synchronously during startSessionRecorder argument
// evaluation, BEFORE Run, while signal.NotifyContext already owns SIGINT: a
// hung clangd stalled startup forever with Ctrl-C swallowed (reproduced with
// a 120s-hanging fake clangd on PATH: killed only by an external 8s timeout,
// "recording session to" never printed). The probe must be bounded: a hung
// toolchain degrades to a missing entry (the same shape as a missing
// toolchain), never an unbounded wait.
func TestSessionMetaProbeIsBoundedWhenToolchainHangs(t *testing.T) {
	dir := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		// A compiled sleeper is the only reliable hang fixture in headless
		// runs: batch primitives (ping/timeout) fail fast without a console
		// or network. The fixture self-terminates after 8s so a leaked probe
		// process cannot outlive the test run.
		src := filepath.Join(dir, "sleepmain.go")
		if err := os.WriteFile(src, []byte("package main\n\nimport \"time\"\n\nfunc main() { time.Sleep(8 * time.Second) }\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "clangd.exe"), src)
		if out, berr := build.CombinedOutput(); berr != nil {
			t.Fatalf("build hang fixture: %v: %s", berr, out)
		}
	default:
		sh := filepath.Join(dir, "clangd")
		if err := os.WriteFile(sh, []byte("#!/bin/sh\nsleep 8\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)

	old := versionProbeTimeout
	versionProbeTimeout = 500 * time.Millisecond
	t.Cleanup(func() { versionProbeTimeout = old })

	cfg := config.Default()
	cfg.WorkspaceDir = t.TempDir()
	srv := server.New(serveConfig(cfg, ""))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan replay.Meta, 1)
	go func() { done <- sessionMeta(ctx, cfg, srv) }()
	select {
	case meta := <-done:
		if v, ok := meta.Toolchain["clangd"]; ok {
			t.Fatalf("hung clangd probe still produced a version entry: %q", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sessionMeta still blocked 5s on a hung clangd probe — the serve --record startup path has no deadline and swallows SIGINT")
	}
}
