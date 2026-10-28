package jsonrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Frame is one read of a Content-Length framed body, classified per the
// 2026-10-25 ruling on JSON-RPC 2.0 §6 batch semantics (docs/audit-longterm-spec.md
// 终局裁决第 1 条): framing integrity and element validity are two layers.
//
// Either Msg is set (single-object body) or Elements is non-nil (array body,
// possibly empty). A body that cannot be parsed at all — non-JSON, or an
// array that breaks mid-parse — yields a typed ParseError instead, the
// per-stream fatal class the ruling keeps for framing faults.
type Frame struct {
	// Msg is the decoded message of a single-object body; nil for batch
	// frames.
	Msg *Message

	// Elements carries the elements of an array (batch) body in wire order.
	// Nil for single-object bodies; non-nil (possibly empty) for array
	// bodies, so an empty batch is distinguishable from a single message.
	Elements []BatchElement
}

// BatchElement is one element of a batch frame and its per-element outcome.
// §6 isolates elements: a decode failure is answered by Reply and never
// affects the frame's siblings or the session.
type BatchElement struct {
	// Raw is the element's exact JSON bytes as they arrived.
	Raw json.RawMessage

	// Msg is the decoded element; nil exactly when the element failed to
	// decode into a Message.
	Msg *Message

	// Reply is the single error response owed to an invalid element:
	// InvalidRequest (the array parsed, so the element is valid JSON that is
	// merely not a valid message — §5.1's ParseError is for unparseable
	// text), echoing the element's id when one is detectable and null
	// otherwise. Nil for valid elements.
	Reply *Message
}

// ReadFrame reads the next framed body and classifies it.
//
// Array bodies are JSON-RPC 2.0 §6 batches: each element is decoded in
// isolation, an element that fails to decode gets exactly one error response
// (id null when undetectable) and its siblings are unaffected. Any other
// body follows the single-message path, where an unparseable body yields a
// typed ParseError — the framing-fault class that stays per-stream fatal
// upstream. A body that starts as an array but breaks mid-parse is a framing
// fault too and lands there, not in element isolation.
//
// The body size cap (MaxMessageSize) bounds a batch's total size exactly as
// it bounds a single message, and the element count is separately capped
// (MaxBatchElements): a batch over the cap is rejected whole as a ParseError
// — the per-stream fatal class — instead of being amplified per element
// (audit circle 2, finding 1: a dense 1MiB batch used to cost 621x its size
// in cumulative allocations and owed one reply per invalid element). The
// element cap doubles as the per-frame reply budget: replies per frame can
// never exceed it. Delivery backpressure remains the transport's existing
// policy (F6).
func (c *Codec) ReadFrame(r io.Reader) (*Frame, error) {
	body, err := c.readBody(r)
	if err != nil {
		return nil, err
	}
	// Tolerate leading whitespace before the array opener, exactly as the
	// single-message path tolerates it before an object.
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		return decodeBatchFrame(trimmed, c.maxBatchElements())
	}
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, &CodecError{
			Code:    ParseError,
			Message: fmt.Sprintf("parse error: %v", err),
		}
	}
	return &Frame{Msg: &msg}, nil
}

// decodeBatchFrame splits an array body and decodes each element in
// isolation. The array itself must parse: a body that opens with '[' but is
// not valid JSON is an unparseable frame (ParseError) — the ruling's
// per-stream fatal class — not a batch with bad elements. So is an array
// with more than maxElements elements: fail closed at the pre-count, before
// json.Unmarshal materializes every element and before any per-element
// decode or reply work.
func decodeBatchFrame(body []byte, maxElements int) (*Frame, error) {
	if countTopLevelArrayElements(body) > maxElements {
		return nil, &CodecError{
			Code: ParseError,
			Message: fmt.Sprintf("parse error: batch frame exceeds the element cap (%d elements max)",
				maxElements),
		}
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(body, &raws); err != nil {
		return nil, &CodecError{
			Code:    ParseError,
			Message: fmt.Sprintf("parse error: %v", err),
		}
	}
	if len(raws) > maxElements {
		return nil, &CodecError{
			Code: ParseError,
			Message: fmt.Sprintf("parse error: batch frame exceeds the element cap: %d elements (max %d)",
				len(raws), maxElements),
		}
	}
	f := &Frame{Elements: make([]BatchElement, 0, len(raws))}
	for _, raw := range raws {
		el := BatchElement{Raw: raw}
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil {
			el.Reply = batchElementReply(raw, err)
		} else {
			el.Msg = &m
		}
		f.Elements = append(f.Elements, el)
	}
	return f, nil
}

// countTopLevelArrayElements is an allocation-free conservative pre-count of
// the elements in an array body: string-, escape- and depth-aware, so it is
// exact for well-formed JSON and an upper bound for malformed text (which
// the real unmarshal rejects as ParseError afterwards anyway). It exists so
// an oversize batch is refused before json.Unmarshal materializes every
// element (§C1: the count check itself must not amplify the frame).
func countTopLevelArrayElements(body []byte) int {
	count := 1 // the first element; commas at depth 1 add the rest
	depth := 0
	inString := false
	escaped := false
	for _, b := range body {
		if escaped {
			escaped = false
			continue
		}
		switch {
		case inString:
			switch b {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case b == '"':
			inString = true
		case b == '[' || b == '{':
			depth++
		case b == ']' || b == '}':
			depth--
		case b == ',' && depth == 1:
			count++
		}
	}
	return count
}

// batchElementReply builds the §6 single error response for one invalid
// batch element. The id is echoed when the element still carries a
// detectable one (e.g. {"id":5,"method":123}) and null otherwise (spec
// §4.2: an id that cannot be detected MUST be Null) — the ruling's "id
// 未知则 null". The detection probe is deliberately lenient: it decodes only
// the id field and ignores everything else about the broken element.
func batchElementReply(raw json.RawMessage, cause error) *Message {
	id := RequestID{IsNull: true}
	var probe struct {
		ID *RequestID `json:"id"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && probe.ID != nil {
		id = *probe.ID
	}
	return NewErrorResponse(id, InvalidRequest,
		fmt.Sprintf("invalid request: batch element is not a valid JSON-RPC message: %v", cause), nil)
}
