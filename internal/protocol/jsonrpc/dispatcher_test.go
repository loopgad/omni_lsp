package jsonrpc

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"testing"

	ierrors "github.com/omnilsp/omni/internal/errors"
)

func TestDispatcherMapsContentModifiedToWireCode(t *testing.T) {
	stale := ierrors.New(ierrors.ErrContentModified, "nested.request", "document changed")
	cases := []struct {
		name string
		err  error
		code int
	}{
		{name: "typed stale result", err: stale, code: ContentModified},
		{name: "wrapped stale result", err: fmt.Errorf("backend request failed: %w", stale), code: ContentModified},
		{name: "ordinary error remains internal", err: stderrors.New("backend failed"), code: InternalError},
		{name: "explicit response error is preserved", err: &ResponseError{Code: RequestCancelled, Message: "cancelled"}, code: RequestCancelled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDispatcher()
			d.Register("test/request", func(context.Context, *Message) (json.RawMessage, error) {
				return nil, tc.err
			})

			id := RequestID{Num: 42}
			resp := d.Dispatch(context.Background(), NewRequest(id, "test/request", nil))
			if resp == nil || resp.Error == nil {
				t.Fatal("expected JSON-RPC error response")
			}

			wire, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			var decoded Message
			if err := json.Unmarshal(wire, &decoded); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if decoded.Error == nil {
				t.Fatal("wire response has no error object")
			}
			if decoded.Error.Code != tc.code {
				t.Errorf("wire error code = %d, want %d", decoded.Error.Code, tc.code)
			}
			if decoded.ID == nil || !decoded.ID.Equals(id) {
				t.Errorf("wire response ID = %#v, want %#v", decoded.ID, id)
			}
		})
	}
}
