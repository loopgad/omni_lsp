package jsonrpc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// Tests in this file pin the 2026-10-25 batch ruling (docs/audit-longterm-spec.md
// 终局裁决第 1 条) at the codec layer: framing integrity and element validity
// are two layers. ReadFrame classifies frames; element faults inside a
// parseable array are isolated, frame faults are not. The transport-side
// half of the ruling lives in internal/transport (batch survival + per-element
// replies on the wire; unparseable frames still per-stream fatal).

// TestReadFrameClassifiesSinglePinsSingleMessagePath: a non-array body takes
// the old single-message path unchanged — ReadFrame is additive, not a
// behavior change for singles.
func TestReadFrameClassifiesSinglePinsSingleMessagePath(t *testing.T) {
	c := NewCodec()
	fr, err := c.ReadFrame(strings.NewReader(frame(`{"jsonrpc":"2.0","id":1,"method":"x"}`)))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if fr.Msg == nil || fr.Msg.Method != "x" {
		t.Fatalf("single body classified as %+v, want Msg with method x", fr)
	}
	if fr.Elements != nil {
		t.Errorf("single body must not carry Elements: %+v", fr.Elements)
	}

	// Unparseable single body → typed ParseError, the per-stream fatal class.
	fr, err = c.ReadFrame(strings.NewReader(frame(`{not js`)))
	if err == nil {
		t.Fatalf("garbage body classified as %+v, want an error", fr)
	}
	var codecErr *CodecError
	if !errors.As(err, &codecErr) || codecErr.Code != ParseError {
		t.Fatalf("error = %v (%T), want *CodecError{ParseError}", err, err)
	}
}

// TestReadFrameClassifiesBatchElements: a parseable array is a §6 batch.
// Valid elements decode; invalid ones get exactly one Reply each, with the
// element's id echoed when detectable and null otherwise.
func TestReadFrameClassifiesBatchElements(t *testing.T) {
	c := NewCodec()
	fr, err := c.ReadFrame(strings.NewReader(frame(
		`[{"jsonrpc":"2.0","id":2,"method":"batch/a"},1,{"id":5,"method":123},[1],"str",null]`)))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if fr.Msg != nil {
		t.Fatalf("batch body must not set Msg: %+v", fr.Msg)
	}
	if len(fr.Elements) != 6 {
		t.Fatalf("elements = %d, want 6", len(fr.Elements))
	}

	// Valid elements keep their decoded shape and exact raw bytes.
	if fr.Elements[0].Msg == nil || fr.Elements[0].Msg.Method != "batch/a" {
		t.Errorf("element 0 = %+v, want decoded request", fr.Elements[0])
	}
	if fr.Elements[0].Reply != nil {
		t.Errorf("element 0 must not carry a Reply: %+v", fr.Elements[0].Reply)
	}
	if string(fr.Elements[0].Raw) != `{"jsonrpc":"2.0","id":2,"method":"batch/a"}` {
		t.Errorf("element 0 raw = %s, want exact wire bytes", fr.Elements[0].Raw)
	}

	// Every invalid element: Reply set, Msg nil, code InvalidRequest.
	for i, wantIDNull := range map[int]bool{1: true, 2: false, 3: true, 4: true} {
		el := fr.Elements[i]
		if el.Msg != nil {
			t.Errorf("element %d decoded to %+v, want nil Msg for an invalid element", i, el.Msg)
		}
		if el.Reply == nil {
			t.Fatalf("element %d has no Reply", i)
		}
		if el.Reply.Error == nil || el.Reply.Error.Code != InvalidRequest {
			t.Errorf("element %d reply = %+v, want InvalidRequest", i, el.Reply)
		}
		if wantIDNull {
			if el.Reply.ID == nil || !el.Reply.ID.IsNull {
				t.Errorf("element %d reply id = %+v, want null (nothing detectable)", i, el.Reply.ID)
			}
		} else if el.Reply.ID == nil || !el.Reply.ID.Equals(RequestID{Num: 5}) {
			t.Errorf("element %d reply id = %+v, want 5 (detectable despite the decode failure)", i, el.Reply.ID)
		}
	}

	// `null` decodes into the zero Message — valid JSON that is not a valid
	// message. Bug round 20 A#15 (圈 5) replaced the old "delivered like any
	// standalone null body" ruling with the element-isolation contract the
	// BatchElement.Reply declaration always stated: an invalid element gets
	// exactly one InvalidRequest reply (id null — nothing detectable), the
	// same outcome Dispatcher.Dispatch now gives a standalone tri-state-none
	// message.
	if fr.Elements[5].Msg != nil || fr.Elements[5].Reply == nil {
		t.Errorf("element 5 (null) = msg %v reply %v, want one InvalidRequest reply and no delivery", fr.Elements[5].Msg, fr.Elements[5].Reply)
	}
	if fr.Elements[5].Reply.Error == nil || fr.Elements[5].Reply.Error.Code != InvalidRequest {
		t.Errorf("element 5 (null) reply = %+v, want InvalidRequest", fr.Elements[5].Reply)
	}
}

// TestReadFrameEmptyBatchIsSilent: an empty array is a whole, parseable frame
// with zero elements — nothing to deliver, nothing to answer, session intact.
// (§6 example answers `[]` with `[]` on a request/response server; LSP 3.17
// deprecates batches, so the silent reading is the one the ruling's "elements
// as usual" extends to: no elements, no traffic.)
func TestReadFrameEmptyBatchIsSilent(t *testing.T) {
	c := NewCodec()
	fr, err := c.ReadFrame(strings.NewReader(frame(`[]`)))
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if fr.Msg != nil {
		t.Fatalf("empty batch must not set Msg: %+v", fr.Msg)
	}
	if fr.Elements == nil || len(fr.Elements) != 0 {
		t.Fatalf("empty batch Elements = %#v, want non-nil and empty", fr.Elements)
	}
}

// TestReadFrameBrokenArrayIsFrameFault is the codec-side counter-example the
// ruling requires: a body that opens as an array but does not parse is an
// UNPARSEABLE FRAME — the per-stream fatal class — not a batch with bad
// elements. This is the cut line between element isolation and framing
// integrity.
func TestReadFrameBrokenArrayIsFrameFault(t *testing.T) {
	c := NewCodec()
	fr, err := c.ReadFrame(strings.NewReader(frame(`[{"jsonrpc":"2.0","id":1,"method":"x"}, oops]`)))
	if err == nil {
		t.Fatalf("broken array classified as %+v, want a framing fault", fr)
	}
	var codecErr *CodecError
	if !errors.As(err, &codecErr) || codecErr.Code != ParseError {
		t.Fatalf("error = %v (%T), want *CodecError{ParseError}", err, err)
	}
}

// TestReadFrameAdvancesStream: a batch consumes exactly its frame; the next
// frame on the same stream reads cleanly afterwards. A regression here is how
// the old per-stream fatal used to strand post-batch traffic.
func TestReadFrameAdvancesStream(t *testing.T) {
	c := NewCodec()
	var stream strings.Builder
	stream.WriteString(frame(`[{"jsonrpc":"2.0","id":2,"method":"batch/a"}]`))
	stream.WriteString(frame(`{"jsonrpc":"2.0","id":7,"method":"after"}`))
	r := strings.NewReader(stream.String())

	if _, err := c.ReadFrame(r); err != nil {
		t.Fatalf("ReadFrame batch: %v", err)
	}
	fr, err := c.ReadFrame(r)
	if err != nil {
		t.Fatalf("ReadFrame after batch: %v", err)
	}
	if fr.Msg == nil || fr.Msg.Method != "after" {
		t.Fatalf("post-batch frame = %+v, want Msg with method after", fr)
	}
}

// TestBatchElementReplyMarshaledShape: the reply's wire form — jsonrpc 2.0,
// id as echoed or null, error object with the InvalidRequest code.
func TestBatchElementReplyMarshaledShape(t *testing.T) {
	raw := json.RawMessage(`42`)
	m := batchElementReply(raw, errors.New("decode failed"))
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *struct {
			Code int `json:"code"`
		} `json:"error"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if probe.JSONRPC != Version {
		t.Errorf("jsonrpc = %q, want %q", probe.JSONRPC, Version)
	}
	if string(probe.ID) != "null" {
		t.Errorf("id = %s, want literal null", probe.ID)
	}
	if probe.Error == nil || probe.Error.Code != InvalidRequest {
		t.Errorf("error = %+v, want InvalidRequest", probe.Error)
	}
	if probe.Method != "" {
		t.Errorf("reply must not carry a method: %q", probe.Method)
	}
}
