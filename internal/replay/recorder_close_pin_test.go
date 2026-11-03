package replay

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// Pin for round20/bugB bug 1/7: RecorderTransport.Close sets the writer to
// nil, and a late Write/Read from a worker still in flight (scheduler
// Shutdown does not join admitted workers; the session closes the recorder
// after Run returns) hit json.NewEncoder(r.w).Encode on the nil writer and
// panicked — a recording lifecycle problem aborting the live session,
// against the §P9 contract ("a recording write failure never aborts or
// corrupts the live session"). Late entries must degrade to a retained
// firstErr and the live transport must keep flowing.
func TestRecorderLateMessagesAfterCloseDegradeWithoutPanic(t *testing.T) {
	ft := newFeedTransport(&jsonrpc.Message{Method: "late-in"})
	rec, err := NewRecorder(ft, filepath.Join(t.TempDir(), "session.jsonl"), Meta{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	// Late inbound message: the inner transport still delivers.
	if msg, err := rec.Read(context.Background()); err != nil || msg == nil {
		t.Fatalf("Read after Close must still serve the live session: msg=%v err=%v", msg, err)
	}

	// Late outbound message: must not panic and must still reach the live
	// transport.
	outbound := &jsonrpc.Message{Method: "late-out"}
	if err := rec.Write(context.Background(), outbound); err != nil {
		t.Fatalf("Write after Close must not fail the live session: %v", err)
	}
	if ft.written() != 1 {
		t.Fatalf("late write must still reach the inner transport, got %d writes", ft.written())
	}

	if rec.Err() == nil || !errors.Is(rec.Err(), ErrRecordedAfterClose) {
		t.Fatalf("late entries must be retained as ErrRecordedAfterClose, got %v", rec.Err())
	}
}
