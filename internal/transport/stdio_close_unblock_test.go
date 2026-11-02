package transport

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// TestStdioCloseUnblocksParkedRead pins bug round 20 A#8 (圈 3): Close used
// to only flip the closed flag, but a Read already parked in the select (the
// shape server.Run lives in) is woken only by msgCh/errCh/ctx.Done/done — and
// done closes only when readLoop exits, which it never does while blocked in
// codec.ReadFrame(stdin). handleExit's contract comment ("transport closed so
// the Run loop returns", C2) therefore failed on the stdio transport: after
// an exit notification, with the client not closing stdin and no signal, the
// session — including plugin/backend teardown — hung indefinitely. The TCP
// leg already held because TCPTransport.Close closes the conn; stdio must
// behave the same: Close closes the underlying stdin (when closable), the
// parked ReadFrame faults out, readLoop feeds errCh and closes done, and the
// parked Read returns.
func TestStdioCloseUnblocksParkedRead(t *testing.T) {
	client, serverEnd := net.Pipe()
	defer serverEnd.Close()
	tr := NewStdioTransport(client, io.Discard)

	readReturned := make(chan error, 1)
	go func() {
		_, err := tr.Read(context.Background())
		readReturned <- err
	}()
	// Give the read goroutine time to park inside ReadFrame on the pipe.
	time.Sleep(100 * time.Millisecond)

	tr.Close()
	select {
	case err := <-readReturned:
		if err == nil {
			t.Fatal("Read returned a message after Close; want the parked read to fault out, not succeed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CONFIRMED stdio: Read still blocked 2s after Close — Close must unblock a parked Read (handleExit→Run-return handshake, C2)")
	}

	// readLoop must have exited with it: Done closes exactly then.
	select {
	case <-tr.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done() did not close after Close unblocked the read loop")
	}

	// Close stays idempotent.
	if err := tr.Close(); err != nil {
		t.Fatalf("duplicate Close: %v", err)
	}
}
