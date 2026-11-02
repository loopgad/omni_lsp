package main

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/runtime/server"
)

// TestMCPTransportReturnsOnContextCancel pins bug round 20 A#6 (圈 3): on the
// --transport mcp path signal.NotifyContext owns SIGINT/SIGTERM, but
// mcpserver.Serve only checks ctx.Err() AFTER a line arrives — on an idle
// stdin the scanner blocks forever and the cancellation is never observed.
// serveMCP then neither returns nor runs srv.CloseSession(), so the
// closeBackends/diag.Close/scheduler.Shutdown/ClosePlugins teardown audited
// in 批 2 is unreachable on the signal path — the same failure family fixed
// for the TCP accept loop and HTTP listener in 3f32aa0, third leg. serveMCP
// must (a) return promptly once ctx is cancelled on an idle stdin and (b)
// still run the session teardown (observable via the registered backend's
// Close).
func TestMCPTransportReturnsOnContextCancel(t *testing.T) {
	cfg := config.Default()
	cfg.WorkspaceDir = t.TempDir()
	srv := server.New(serveConfig(cfg, ""))

	be := &mcpTeardownBackend{}
	srv.RegisterBackend("testlang", be)

	// net.Pipe: reads block until a write happens on the other end — a
	// faithful stand-in for an idle os.Stdin.
	client, serverEnd := net.Pipe()
	defer client.Close()
	defer serverEnd.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveMCP(ctx, srv, serverEnd, io.Discard)
	}()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("serveMCP after ctx cancel: %v, want an error wrapping context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WEDGE CONFIRMED: serveMCP still blocked 2s after ctx cancel (idle stdin) — the MCP branch does not observe the signal ctx")
	}

	// The cancellation path must not skip session teardown.
	if got := be.closeCalls.Load(); got == 0 {
		t.Fatal("serveMCP returned on ctx cancel without running CloseSession teardown (backend Close never called)")
	}
}
