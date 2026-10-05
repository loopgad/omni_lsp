package jsonrpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// F16 requires exactly one terminal outcome per request: a handler panic
// must become a structured InternalError carrying the request ID, never a
// lost response and never a process crash. The notification case has no ID to
// answer on, so a panic there must produce no response at all.
//
// Nothing reached this recover() before: s10_conformance_test.go registers a
// "test/panic" method whose handler returns cleanly, so deleting the entire
// defer block left the suite green.
func TestF16_RequestHandlerPanicYieldsInternalError(t *testing.T) {
	d := NewDispatcher()
	d.Register("test/panic", func(ctx context.Context, msg *Message) (json.RawMessage, error) {
		panic("boom")
	})
	id := RequestID{Num: 42}
	resp := d.Dispatch(context.Background(), NewRequest(id, "test/panic", nil))
	if resp == nil {
		t.Fatal("panicking request produced no response; the client waits forever")
	}
	if resp.ID == nil || *resp.ID != id {
		t.Fatalf("response ID = %+v, want %+v echoed back", resp.ID, id)
	}
	if resp.Error == nil {
		t.Fatal("panicking request produced a success response")
	}
	if resp.Error.Code != InternalError {
		t.Errorf("code = %d, want InternalError (%d)", resp.Error.Code, InternalError)
	}
	if !strings.Contains(resp.Error.Message, "boom") {
		t.Errorf("message = %q, want it to name the panic value", resp.Error.Message)
	}
	if resp.Result != nil {
		t.Errorf("panicking request also produced a result: %s", resp.Result)
	}
}

func TestF16_NotificationPanicProducesNoResponse(t *testing.T) {
	d := NewDispatcher()
	d.Register("test/panic", func(ctx context.Context, msg *Message) (json.RawMessage, error) {
		panic("boom")
	})
	resp := d.Dispatch(context.Background(), NewNotification("test/panic", nil))
	if resp != nil {
		t.Fatalf("notification panic produced a response: %+v", resp)
	}
}

// A panic must not poison the dispatcher: the next request on the same
// instance still routes, which is what "preserve process integrity" means in
// F16 — the handler is at fault, not the server.
func TestF16_DispatcherSurvivesHandlerPanic(t *testing.T) {
	d := NewDispatcher()
	d.Register("test/panic", func(ctx context.Context, msg *Message) (json.RawMessage, error) {
		panic("boom")
	})
	d.Register("test/ok", func(ctx context.Context, msg *Message) (json.RawMessage, error) {
		return json.RawMessage(`{"fine":true}`), nil
	})
	d.Dispatch(context.Background(), NewRequest(RequestID{Num: 1}, "test/panic", nil))

	resp := d.Dispatch(context.Background(), NewRequest(RequestID{Num: 2}, "test/ok", nil))
	if resp == nil || resp.Error != nil {
		t.Fatalf("follow-up request failed after a panic: %+v", resp)
	}
	if string(resp.Result) != `{"fine":true}` {
		t.Errorf("follow-up result = %s", resp.Result)
	}
}
