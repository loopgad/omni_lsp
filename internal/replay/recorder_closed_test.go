package replay

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// TestRecorderOperationsAfterCloseReturnNotPanic pins bug round 20 A#3:
// Close() flushes, sets r.w = nil and closes the file, but logLocked kept
// dereferencing the writer — so any late Write/Read after the session closed
// panicked (nil bufio.Writer) instead of returning an error, violating the
// transport contract ("a closed transport returns errors on all subsequent
// operations", stdio.go). The window is reachable in production: drainResult
// releases srv.Run after the drain window while a still-running backend
// request can complete and s.send its late response through the closed
// recorder, and the diagnostics debounce timer callback has no recover — a
// single late write was a whole-process crash. After the fix the live
// message still flows (the wrapped transport is untouched; a recording
// failure never aborts the session) and the gap is visible via Err().
func TestRecorderOperationsAfterCloseReturnNotPanic(t *testing.T) {
	msg := &jsonrpc.Message{
		JSONRPC: "2.0",
		Method:  "window/workDoneProgress",
		Params:  json.RawMessage(`{"kind":"begin"}`),
	}
	rec, err := NewRecorder(newFeedTransport(msg), filepath.Join(t.TempDir(), "session.jsonl"), Meta{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Write must not panic; the inner transport stays authoritative for the
	// live session even though the recording leg is gone.
	if werr := rec.Write(context.Background(), msg); werr != nil {
		t.Fatalf("Write after Close must not fail the live session, got: %v", werr)
	}
	// Read must not panic either: the scripted message still flows.
	if got, rerr := rec.Read(context.Background()); rerr != nil || got == nil {
		t.Fatalf("Read after Close: msg=%v err=%v, want the scripted message and nil error", got, rerr)
	}

	// The failed recording legs must be detectable, not silent.
	if err := rec.Err(); !errors.Is(err, errRecorderClosed) {
		t.Fatalf("Err() after post-Close writes = %v, want errRecorderClosed", err)
	}
}
