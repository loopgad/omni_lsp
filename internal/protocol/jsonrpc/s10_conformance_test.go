// Package jsonrpc — S10 protocol conformance tests.
package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRequestIDPreserved(t *testing.T) {
	id := RequestID{Str: "req-42", IsStr: true}
	msg := NewRequest(id, "test/method", json.RawMessage("{}"))
	if msg.ID == nil || !msg.ID.Equals(id) {
		t.Fatal("request ID not preserved")
	}
}

func TestNotificationHasNoID(t *testing.T) {
	msg := NewNotification("method", json.RawMessage("{}"))
	if msg.ID != nil {
		t.Error("notification should have nil ID")
	}
	if !msg.IsNotification() {
		t.Error("should be recognized as notification")
	}
}

func TestResponseHasNoMethod(t *testing.T) {
	id := RequestID{Num: 1}
	msg := NewResponse(id, json.RawMessage("null"))
	if msg.Method != "" {
		t.Error("response should have empty method")
	}
	if !msg.IsResponse() {
		t.Error("should be recognized as response")
	}
}

func TestMalformedJSONDoesNotPanic(t *testing.T) {
	// Wire-level path: a frame whose body is not JSON must come back as a
	// typed ParseError CodecError — never a panic, never a silent success.
	t.Run("codec body not json", func(t *testing.T) {
		msg, err := NewCodec().ReadMessage(strings.NewReader(frame("not json")))
		if err == nil {
			t.Fatalf("malformed body must be rejected, got message %+v", msg)
		}
		if msg != nil {
			t.Fatalf("malformed body must not yield a message: %+v", msg)
		}
		var codecErr *CodecError
		if !errors.As(err, &codecErr) {
			t.Fatalf("error = %v (%T), want *CodecError", err, err)
		}
		if codecErr.Code != ParseError {
			t.Errorf("code = %d, want ParseError (%d)", codecErr.Code, ParseError)
		}
	})

	// Original case, kept as its own subtest: a request for an unknown
	// method whose params happen to be malformed still yields an error
	// response instead of a panic.
	t.Run("unknown method malformed params", func(t *testing.T) {
		d := NewDispatcher()
		msg := NewRequest(RequestID{Str: "x", IsStr: true}, "unknown", json.RawMessage("not json"))
		resp := d.Dispatch(nil, msg)
		if resp == nil {
			t.Fatal("expected error response")
		}
		if resp.Error == nil {
			t.Fatal("expected error")
		}
	})
}

func TestUnknownMethodReturnsError(t *testing.T) {
	d := NewDispatcher()
	msg := NewRequest(RequestID{Str: "u", IsStr: true}, "nonexistent", json.RawMessage("{}"))
	resp := d.Dispatch(nil, msg)
	if resp == nil || resp.Error == nil {
		t.Fatal("expected error for unknown method")
	}
	if resp.Error.Code != MethodNotFound {
		t.Errorf("code = %d, want MethodNotFound", resp.Error.Code)
	}
}

func TestUnknownNotificationSilentlyIgnored(t *testing.T) {
	d := NewDispatcher()
	msg := NewNotification("nonexistent", json.RawMessage("{}"))
	resp := d.Dispatch(nil, msg)
	if resp != nil {
		t.Error("unknown notification should return nil")
	}
}

func TestResponsePassthrough(t *testing.T) {
	d := NewDispatcher()
	msg := NewResponse(RequestID{Num: 99}, json.RawMessage(`{"ok":true}`))
	resp := d.Dispatch(nil, msg)
	if resp != nil {
		t.Error("response passthrough should return nil")
	}
}

func TestStructuredErrorResponse(t *testing.T) {
	// S10 contract over the real dispatch path: a handler that fails must
	// get a typed error response carrying code and message — not just a
	// constructed one.
	t.Run("dispatch typed error response", func(t *testing.T) {
		d := NewDispatcher()
		d.Register("test/fail", func(ctx context.Context, msg *Message) (json.RawMessage, error) {
			return nil, &ResponseError{
				Code:    InvalidParams,
				Message: "bad input",
				Data:    json.RawMessage(`{"hint":"check params"}`),
			}
		})
		req := NewRequest(RequestID{Str: "e", IsStr: true}, "test/fail", json.RawMessage("{}"))
		resp := d.Dispatch(context.Background(), req)
		if resp == nil {
			t.Fatal("dispatch must answer a failing handler with a response")
		}
		if resp.Error == nil {
			t.Fatal("expected error in response")
		}
		if resp.Error.Code != InvalidParams {
			t.Errorf("code = %d, want InvalidParams (%d)", resp.Error.Code, InvalidParams)
		}
		if resp.Error.Message != "bad input" {
			t.Errorf("message = %q, want %q", resp.Error.Message, "bad input")
		}
		if string(resp.Error.Data) != `{"hint":"check params"}` {
			t.Errorf("data = %s, want the handler's data preserved", resp.Error.Data)
		}
		if resp.ID == nil || !resp.ID.Equals(RequestID{Str: "e", IsStr: true}) {
			t.Error("request ID must be preserved on the error response")
		}
	})

	// A plain (non-*ResponseError) error maps to InternalError with the
	// error text as the message.
	t.Run("dispatch plain error maps to InternalError", func(t *testing.T) {
		d := NewDispatcher()
		d.Register("test/plain", func(ctx context.Context, msg *Message) (json.RawMessage, error) {
			return nil, errors.New("boom")
		})
		req := NewRequest(RequestID{Num: 3}, "test/plain", nil)
		resp := d.Dispatch(context.Background(), req)
		if resp == nil || resp.Error == nil {
			t.Fatal("expected error response")
		}
		if resp.Error.Code != InternalError {
			t.Errorf("code = %d, want InternalError (%d)", resp.Error.Code, InternalError)
		}
		if resp.Error.Message != "boom" {
			t.Errorf("message = %q, want %q", resp.Error.Message, "boom")
		}
	})

	// Constructor-level check kept from the original test.
	t.Run("constructor", func(t *testing.T) {
		id := RequestID{Str: "e", IsStr: true}
		err := NewErrorResponse(id, InternalError, "boom", nil)
		if err == nil || err.Error == nil {
			t.Fatal("expected error response")
		}
		if err.Error.Code != InternalError {
			t.Errorf("code = %d, want InternalError", err.Error.Code)
		}
	})
}

func TestRequestIDNumberAndString(t *testing.T) {
	numID := RequestID{Num: 42}
	strID := RequestID{Str: "abc", IsStr: true}
	nullID := RequestID{IsNull: true}

	if !numID.Equals(RequestID{Num: 42}) {
		t.Error("number IDs should be equal")
	}
	if !strID.Equals(RequestID{Str: "abc", IsStr: true}) {
		t.Error("string IDs should be equal")
	}
	if numID.Equals(strID) {
		t.Error("number and string IDs should not be equal")
	}
	if !nullID.Equals(RequestID{IsNull: true}) {
		t.Error("null IDs should be equal")
	}
}

func TestMessageKindDetection(t *testing.T) {
	cases := []struct {
		msg     *Message
		isReq   bool
		isNotif bool
		isResp  bool
	}{
		{NewRequest(RequestID{Str: "1", IsStr: true}, "method", nil), true, false, false},
		{NewNotification("method", nil), false, true, false},
		{NewResponse(RequestID{Str: "1", IsStr: true}, nil), false, false, true},
		{&Message{}, false, false, false},
	}
	for _, c := range cases {
		if c.msg.IsRequest() != c.isReq {
			t.Errorf("IsRequest = %v, want %v", c.msg.IsRequest(), c.isReq)
		}
		if c.msg.IsNotification() != c.isNotif {
			t.Errorf("IsNotification = %v, want %v", c.msg.IsNotification(), c.isNotif)
		}
		if c.msg.IsResponse() != c.isResp {
			t.Errorf("IsResponse = %v, want %v", c.msg.IsResponse(), c.isResp)
		}
	}
}
