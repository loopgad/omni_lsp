package jsonrpc

import (
	"encoding/json"
	"testing"
)

// TestMessageKindDetectionFromWire pins the wire-level contract that
// Message.IsRequest / IsNotification / IsResponse depend on.
//
// Method is a plain string, so after decoding it cannot say whether the wire
// carried a method field at all. A response omits it; a malformed request such
// as {"id":1,"method":""} carries it empty. Collapsing those two cases is what
// made the server's read loop drop a malformed request on the floor: it looked
// exactly like a response, and the client waited forever for a reply that JSON-RPC
// requires. The kind predicates therefore track field presence, not emptiness.
func TestMessageKindDetectionFromWire(t *testing.T) {
	for _, tc := range []struct {
		name         string
		raw          string
		request      bool
		notification bool
		response     bool
	}{
		{"request", `{"jsonrpc":"2.0","id":1,"method":"textDocument/hover"}`, true, false, false},
		{"request with empty params", `{"jsonrpc":"2.0","id":1,"method":"x","params":{}}`, true, false, false},
		{"notification", `{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{}}`, false, true, false},
		{"notification with empty method", `{"jsonrpc":"2.0","method":"","params":{}}`, false, true, false},
		// The two cases that must not be confused.
		{"malformed request with empty method", `{"jsonrpc":"2.0","id":1,"method":""}`, true, false, false},
		{"response carrying a result", `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`, false, false, true},
		{"response carrying an error", `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"no"}}`, false, false, true},
		// A response is allowed to have a null result, so presence of result is
		// not the signal -- absence of method is.
		{"response with null result", `{"jsonrpc":"2.0","id":1,"result":null}`, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var msg Message
			if err := json.Unmarshal([]byte(tc.raw), &msg); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.raw, err)
			}
			if got := msg.IsRequest(); got != tc.request {
				t.Errorf("IsRequest() = %v, want %v (method=%q present=%v)",
					got, tc.request, msg.Method, msg.methodPresent)
			}
			if got := msg.IsNotification(); got != tc.notification {
				t.Errorf("IsNotification() = %v, want %v (method=%q present=%v)",
					got, tc.notification, msg.Method, msg.methodPresent)
			}
			if got := msg.IsResponse(); got != tc.response {
				t.Errorf("IsResponse() = %v, want %v (method=%q present=%v)",
					got, tc.response, msg.Method, msg.methodPresent)
			}
		})
	}
}

// TestEmptyMethodRequestGetsMethodNotFound is the regression this whole
// mechanism exists for. The server read loop drops anything IsResponse reports,
// so a request whose method was empty used to vanish with no reply. It has to
// come back as a normal method-not-found error, which is what a request naming
// no method means.
func TestEmptyMethodRequestGetsMethodNotFound(t *testing.T) {
	var msg Message
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":7,"method":""}`), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.IsResponse() {
		t.Fatal("an empty-method request is still classified as a response, so the read loop would discard it")
	}
	d := NewDispatcher()
	resp := d.dispatchRequest(t.Context(), &msg)
	if resp == nil {
		t.Fatal("no response; the client waits forever")
	}
	if resp.Error == nil {
		t.Fatalf("got result %s, want an error", string(resp.Result))
	}
	if resp.Error.Code != MethodNotFound {
		t.Errorf("code = %d, want MethodNotFound (%d)", resp.Error.Code, MethodNotFound)
	}
	if resp.ID == nil || resp.ID.Num != 7 {
		t.Errorf("response id = %v, want the request id 7", resp.ID)
	}
}

// TestConstructorsSetMethodPresence keeps the Go-side constructors and the
// wire decoder telling the same story. A Message built in process takes the
// same classification path as a decoded one.
func TestConstructorsSetMethodPresence(t *testing.T) {
	if got := NewRequest(RequestID{Num: 1}, "m", nil); !got.IsRequest() || got.methodPresent != true {
		t.Errorf("NewRequest: request=%v present=%v, want true/true", got.IsRequest(), got.methodPresent)
	}
	if got := NewNotification("m", nil); !got.IsNotification() || got.methodPresent != true {
		t.Errorf("NewNotification: notification=%v present=%v, want true/true", got.IsNotification(), got.methodPresent)
	}
	// NewResponse with a nil result is still a response -- existing tests rely
	// on that, and result presence is not the discriminator.
	if got := NewResponse(RequestID{Num: 1}, nil); !got.IsResponse() || got.methodPresent != false {
		t.Errorf("NewResponse: response=%v present=%v, want true/false", got.IsResponse(), got.methodPresent)
	}
	if got := NewErrorResponse(RequestID{Num: 1}, MethodNotFound, "m", nil); !got.IsResponse() {
		t.Errorf("NewErrorResponse: response=%v, want true", got.IsResponse())
	}
	// A zero Message has no id and no method, so it is none of the three.
	var zero Message
	if zero.IsRequest() || zero.IsNotification() || zero.IsResponse() {
		t.Errorf("a zero Message must classify as none of the three kinds, got request=%v notification=%v response=%v",
			zero.IsRequest(), zero.IsNotification(), zero.IsResponse())
	}
}
