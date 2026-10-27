package main

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/runtime/server"
)

// TestTCPSessionClosesConnWhenRunReturns pins the session teardown contract
// (audit finding 7): the accept loop is designed for hot reconnects, and a
// TCP session that ends WITHOUT the exit handshake — clean EOF, transport
// fault, cancellation — used to leave the accepted net.Conn open and the
// transport's readLoop goroutine blocked in ReadFrame forever. One leaked fd
// and one leaked goroutine per reconnect. Whatever way the session ends, the
// accepted connection must be closed by the time runTCPSession returns.
func TestTCPSessionClosesConnWhenRunReturns(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	serverConn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTCPSession(ctx, 0, serverConn, server.New(server.DefaultConfig()), config.Config{}, "")
	}()

	// Let the session settle into its read loop, then cancel the context:
	// Run returns ctx.Err() — a non-exit session end.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runTCPSession did not return after cancellation")
	}

	// If the accepted conn was closed on teardown, the client sees the
	// connection close (EOF or reset) immediately; if it leaked, the read
	// runs into the deadline instead.
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	_, rerr := client.Read(buf)
	if rerr == nil {
		t.Fatal("accepted connection still open after the session ended: fd and readLoop leak per reconnect")
	}
	var nerr net.Error
	if errors.As(rerr, &nerr) && nerr.Timeout() {
		t.Fatalf("accepted connection still open after the session ended (read timed out): fd and readLoop leak per reconnect")
	}
}
