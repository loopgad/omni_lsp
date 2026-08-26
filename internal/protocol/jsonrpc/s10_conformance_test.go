// Package jsonrpc — S10 protocol conformance tests.
package jsonrpc

import (
	"context"
	"encoding/json"
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
	d := NewDispatcher()
	msg := NewRequest(RequestID{Str: "x", IsStr: true}, "unknown", json.RawMessage("not json"))
	resp := d.Dispatch(nil, msg)
	if resp == nil {
		t.Fatal("expected error response")
	}
	if resp.Error == nil {
		t.Fatal("expected error")
	}
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
	d := NewDispatcher()
	d.Register("test/panic", func(ctx context.Context, msg *Message) (json.RawMessage, error) {
		return nil, nil
	})
	// Verify error response construction
	id := RequestID{Str: "e", IsStr: true}
	err := NewErrorResponse(id, InternalError, "boom", nil)
	if err == nil || err.Error == nil {
		t.Fatal("expected error response")
	}
	if err.Error.Code != InternalError {
		t.Errorf("code = %d, want InternalError", err.Error.Code)
	}
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
