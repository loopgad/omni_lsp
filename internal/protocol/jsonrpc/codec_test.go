package jsonrpc

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWriteReadRequest(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec()

	req := NewRequest(RequestID{Num: 1}, "test/method", json.RawMessage(`{"key":"value"}`))

	err := c.WriteMessage(&buf, req)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	msg, err := c.ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	if !msg.IsRequest() {
		t.Fatal("expected request")
	}
	if msg.Method != "test/method" {
		t.Errorf("expected method test/method, got %s", msg.Method)
	}
}

func TestWriteReadResponse(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec()

	resp := NewResponse(RequestID{Num: 1}, json.RawMessage(`{"status":"ok"}`))

	err := c.WriteMessage(&buf, resp)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	msg, err := c.ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	if !msg.IsResponse() {
		t.Fatal("expected response")
	}
	if msg.Error != nil {
		t.Errorf("unexpected error: %v", msg.Error)
	}
}

func TestWriteReadNotification(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec()

	notif := NewNotification("test/notify", json.RawMessage(`{"data":42}`))

	err := c.WriteMessage(&buf, notif)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	msg, err := c.ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	if !msg.IsNotification() {
		t.Fatal("expected notification")
	}
	if msg.Method != "test/notify" {
		t.Errorf("expected method test/notify, got %s", msg.Method)
	}
}

func TestReadEmptyBuffer(t *testing.T) {
	c := NewCodec()
	var buf bytes.Buffer
	_, err := c.ReadMessage(&buf)
	if err == nil {
		t.Error("expected error for empty buffer")
	}
}

func TestWriteReadError(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec()

	resp := NewErrorResponse(RequestID{Num: 1}, MethodNotFound, "method not found", nil)

	err := c.WriteMessage(&buf, resp)
	if err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	msg, err := c.ReadMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}

	if !msg.IsError() {
		t.Fatal("expected error response")
	}
	if msg.Error.Code != MethodNotFound {
		t.Errorf("expected code %d, got %d", MethodNotFound, msg.Error.Code)
	}
}

func TestMultipleMessages(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec()

	for i := 0; i < 5; i++ {
		notif := NewNotification("test/notify", json.RawMessage(`{}`))
		c.WriteMessage(&buf, notif)
	}

	for i := 0; i < 5; i++ {
		_, err := c.ReadMessage(&buf)
		if err != nil {
			t.Fatalf("ReadMessage %d: %v", i, err)
		}
	}
}

func TestRequestIDTypes(t *testing.T) {
	var buf bytes.Buffer
	c := NewCodec()

	// Numeric ID.
	req := NewRequest(RequestID{Num: 42}, "test", nil)
	c.WriteMessage(&buf, req)
	msg, _ := c.ReadMessage(&buf)
	if msg.ID.Num != 42 {
		t.Errorf("expected ID 42, got %d", msg.ID.Num)
	}

	// String ID.
	buf.Reset()
	req2 := NewRequest(RequestID{Str: "abc-123", IsStr: true}, "test", nil)
	c.WriteMessage(&buf, req2)
	msg2, _ := c.ReadMessage(&buf)
	if msg2.ID.Str != "abc-123" {
		t.Errorf("expected ID abc-123, got %s", msg2.ID.Str)
	}
}

// TestC1_OversizeMessageRejectedBeforeAllocation verifies the C1 invariant:
// a declared Content-Length above the codec cap fails fast without reading
// or preallocating the body.
func TestC1_OversizeMessageRejectedBeforeAllocation(t *testing.T) {
	c := &Codec{MaxMessageSize: 64}
	header := "Content-Length: 1048576\r\n\r\n"
	_, err := c.ReadMessage(bytes.NewReader([]byte(header)))
	if err == nil {
		t.Fatal("oversize Content-Length must be rejected")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("too large")) {
		t.Errorf("error should mention size limit: %v", err)
	}
}

func TestC1_DefaultLimitApplies(t *testing.T) {
	c := NewCodec()
	if c.maxMessageSize() != DefaultMaxMessageSize {
		t.Errorf("default = %d, want %d", c.maxMessageSize(), DefaultMaxMessageSize)
	}
	zero := &Codec{}
	if zero.maxMessageSize() != DefaultMaxMessageSize {
		t.Errorf("zero-value fallback = %d, want %d", zero.maxMessageSize(), DefaultMaxMessageSize)
	}
}
