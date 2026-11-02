package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// TestTCPAcceptLoopReturnsOnContextCancel pins bug round 20 A#2: cmdServe
// installs signal.NotifyContext (SIGINT/SIGTERM), so default termination is
// replaced by cancellation — and the TCP accept loop must therefore observe
// ctx. Before the fix the loop blocked in Accept forever after a signal: the
// process neither exited nor ran any teardown (nested language-server
// backends survived only until an operator's SIGKILL). The loop must (a)
// still serve clients while the context is live, (b) return once ctx is
// cancelled, and (c) release the listener so the port does not stay bound.
func TestTCPAcceptLoopReturnsOnContextCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Deadline guard: an Accept that outlives the test must fail with a
	// timeout error (clear assertion) rather than hang the package.
	if tcp, ok := ln.(*net.TCPListener); ok {
		if derr := tcp.SetDeadline(time.Now().Add(10 * time.Second)); derr != nil {
			t.Fatal(derr)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan int, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tcpAcceptLoop(ctx, ln, func(session int, conn net.Conn) {
			served <- session
			conn.Close()
		})
	}()

	// While the context is live, a connecting client is served.
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("tcpAcceptLoop never served the connected client")
	}
	client.Close()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tcpAcceptLoop ignored context cancellation: the accept loop never returned after ctx.Done")
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener was not closed when the loop returned: %v", err)
	}
}
