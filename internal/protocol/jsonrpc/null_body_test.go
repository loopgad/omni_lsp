package jsonrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Bug round 20 A#15 (圈 5): a literal null body or batch element decoded
// "successfully" into the zero Message — methodPresent=false, so
// IsRequest/IsNotification/IsResponse are all false and the dispatcher
// silently dropped it. The element-isolation contract (BatchElement.Reply:
// one InvalidRequest per invalid element) was thereby escaped, and the
// single-message path had no terminal outcome at all. Per the codec's
// two-layer ruling (framing vs message validity) the fix is layered too:
// decodeBatchFrame answers a tri-state-none ELEMENT with its InvalidRequest
// reply, and Dispatcher.Dispatch gives a standalone tri-state-none message
// the same terminal outcome — the framing layer stays payload-agnostic.

func framed(body string) string {
	return fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
}

func TestBatchNullElementGetsInvalidRequestReply(t *testing.T) {
	codec := NewCodec()
	body := `[null, {"jsonrpc":"2.0","id":1,"method":"m"}]`
	fr, err := codec.ReadFrame(strings.NewReader(framed(body)))
	if err != nil {
		t.Fatalf("batch frame: %v", err)
	}
	if len(fr.Elements) != 2 {
		t.Fatalf("elements = %d, want 2", len(fr.Elements))
	}
	nullEl := fr.Elements[0]
	if nullEl.Msg != nil {
		t.Fatalf("null element decoded into a message: %+v", nullEl.Msg)
	}
	if nullEl.Reply == nil {
		t.Fatal("null batch element silently dropped — element isolation owes it exactly one InvalidRequest reply")
	}
	if nullEl.Reply.Error == nil || nullEl.Reply.Error.Code != InvalidRequest {
		t.Fatalf("null element reply = %+v, want an InvalidRequest error response", nullEl.Reply)
	}
	if nullEl.Reply.ID == nil || !nullEl.Reply.ID.IsNull {
		t.Fatalf("null element reply id = %+v, want null (nothing detectable)", nullEl.Reply.ID)
	}
	valid := fr.Elements[1]
	if valid.Msg == nil || valid.Reply != nil {
		t.Fatalf("valid sibling element misclassified: %+v", valid)
	}
}

func TestBatchEmptyObjectElementGetsInvalidRequestReply(t *testing.T) {
	codec := NewCodec()
	body := `[{}]`
	fr, err := codec.ReadFrame(strings.NewReader(framed(body)))
	if err != nil {
		t.Fatalf("batch frame: %v", err)
	}
	if len(fr.Elements) != 1 || fr.Elements[0].Reply == nil {
		t.Fatalf("empty-object element not answered with a reply: %+v", fr.Elements)
	}
	if fr.Elements[0].Reply.Error == nil || fr.Elements[0].Reply.Error.Code != InvalidRequest {
		t.Fatalf("empty-object element reply = %+v, want InvalidRequest", fr.Elements[0].Reply)
	}
}

func TestDispatchTriStateNoneGetsInvalidRequest(t *testing.T) {
	d := NewDispatcher()
	// The zero Message is what a literal null body decodes into.
	resp := d.Dispatch(context.Background(), &Message{})
	if resp == nil {
		t.Fatal("tri-state-none message silently dropped — the sender gets no terminal outcome")
	}
	if resp.Error == nil || resp.Error.Code != InvalidRequest {
		t.Fatalf("tri-state-none dispatch = %+v, want an InvalidRequest error response", resp)
	}
	if resp.ID == nil || !resp.ID.IsNull {
		t.Fatalf("reply id = %+v, want null (id undetectable, §4.2)", resp.ID)
	}

	// Real responses still route to nil (the caller owns them).
	if resp := d.Dispatch(context.Background(), NewResponse(RequestID{Num: 1}, nil)); resp != nil {
		t.Fatalf("response dispatched: %+v", resp)
	}
}
