package server

import (
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/scheduler"
)

// TestInflightKeysSeparateNullFromZeroID pins bug round 20 A#7 (圈 3):
// requestIDKey folded the JSON-RPC id null and the numeric id 0 into the same
// inflight key "n:0" (IsNull's Num is the zero value). Two concurrent in-flight
// requests with those IDs then overwrite each other's inflight entry, the
// first completion's unregisterInflight deletes the survivor's entry early,
// $/cancelRequest misroutes, and drainInflight undercounts. Null ids are not
// conformant JSON-RPC but they decode into a non-nil Message.ID and reach the
// scheduler, so the keys must still be injective.
func TestInflightKeysSeparateNullFromZeroID(t *testing.T) {
	if requestIDKey(jsonrpc.RequestID{IsNull: true}) == requestIDKey(jsonrpc.RequestID{Num: 0}) {
		t.Fatalf("requestIDKey folds id null and id 0 into one key %q", requestIDKey(jsonrpc.RequestID{Num: 0}))
	}

	s := New(DefaultConfig())
	zeroReq := &scheduler.Request{}
	nullReq := &scheduler.Request{}
	s.mu.Lock()
	s.inflight[requestIDKey(jsonrpc.RequestID{Num: 0})] = zeroReq
	s.inflight[requestIDKey(jsonrpc.RequestID{IsNull: true})] = nullReq
	n := len(s.inflight)
	s.mu.Unlock()
	if n != 2 {
		t.Fatalf("concurrent id 0 and id null requests collapsed into %d inflight entry, want 2", n)
	}

	// $/cancelRequest for id 0 must not hit the id null entry.
	s.cancelRequest(jsonrpc.RequestID{Num: 0})
	if nullReq.Cancelled() {
		t.Fatal("$/cancelRequest for id 0 cancelled the id null inflight request")
	}
	if !zeroReq.Cancelled() {
		t.Fatal("$/cancelRequest for id 0 did not reach its own inflight request")
	}

	// And the reverse routing.
	s.cancelRequest(jsonrpc.RequestID{IsNull: true})
	if !nullReq.Cancelled() {
		t.Fatal("$/cancelRequest for id null did not reach its own inflight request")
	}
}
