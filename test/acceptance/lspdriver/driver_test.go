package lspdriver

import (
	"fmt"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

func TestEventsSinceDetectsEvictedHistory(t *testing.T) {
	session := &Session{}
	start := session.EventCursor()
	for i := 0; i < eventLimit+1; i++ {
		session.mu.Lock()
		session.appendEventLocked(&jsonrpc.Message{Method: fmt.Sprintf("event-%d", i)})
		session.mu.Unlock()
	}

	if events, _, overflow := session.EventsSince(start); !overflow || events != nil {
		t.Fatalf("EventsSince(old cursor) = (%d events, overflow=%t), want (nil, true)", len(events), overflow)
	}

	cursor := session.EventCursor()
	events, next, overflow := session.EventsSince(cursor - 2)
	if overflow {
		t.Fatal("EventsSince(recent cursor) reported false overflow")
	}
	if next != cursor || len(events) != 2 {
		t.Fatalf("EventsSince returned %d events and cursor %d; want 2 events and cursor %d", len(events), next, cursor)
	}
	if events[0].Method != fmt.Sprintf("event-%d", eventLimit-1) || events[1].Method != fmt.Sprintf("event-%d", eventLimit) {
		t.Fatalf("retained events = %q, %q; want final two sequence entries", events[0].Method, events[1].Method)
	}
}
