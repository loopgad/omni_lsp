package upstream

import (
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

func TestNotificationsSinceAndCursorCoversAtomicSnapshot(t *testing.T) {
	const total = 128
	session := &Session{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			session.captureNotification(&jsonrpc.Message{
				Method: "$/progress",
				Params: json.RawMessage(fmt.Sprintf(`{"token":"%d","value":{"kind":"begin"}}`, i)),
			})
		}
	}()

	// Bound the wait. This loop spins until the producer's notifications show
	// up, and when they never do -- which is exactly what happened when
	// IsNotification stopped recognising a struct-literal Message -- the test
	// burned the whole package timeout before saying anything. A regression
	// here should read as one line, not as a ninety-minute CI run.
	deadline := time.After(30 * time.Second)
	var cursor uint64
	for cursor < total {
		select {
		case <-deadline:
			t.Fatalf("only %d of %d notifications became visible; the producer stopped being recognised", cursor, total)
		default:
		}
		notifications, nextCursor, overflow := session.NotificationsSinceAndCursor(cursor)
		if overflow {
			t.Fatal("notification history overflowed below its configured capacity")
		}
		if nextCursor < cursor || nextCursor > total {
			t.Fatalf("snapshot cursor = %d, want [%d,%d]", nextCursor, cursor, total)
		}
		if got, want := len(notifications), int(nextCursor-cursor); got != want {
			t.Fatalf("snapshot returned %d events for cursor span [%d,%d], want %d", got, cursor, nextCursor, want)
		}
		for i, notification := range notifications {
			wantSequence := cursor + uint64(i) + 1
			if notification.Sequence != wantSequence {
				t.Fatalf("event %d sequence = %d, want %d", i, notification.Sequence, wantSequence)
			}
		}
		if nextCursor == cursor {
			runtime.Gosched()
			continue
		}
		cursor = nextCursor
	}
	<-done
}
