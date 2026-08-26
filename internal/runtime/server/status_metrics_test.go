package server

// Tests for §K0 observability closure: omnilsp/status must surface the
// scheduler's joined counter and aggregate backend cache hit/miss.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

func TestK_StatusReportsJoinedAndCacheMetrics(t *testing.T) {
	s := New(DefaultConfig())
	s.RegisterBackend("go", &mockBackend{langID: "go", exts: []string{".go"}})
	ft := attachTransport(s)

	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 7}, "omnilsp/status", nil)
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("status failed: %v", resp.Error)
	}
	if err := ft.Write(context.Background(), resp); err != nil {
		t.Fatal(err)
	}
	writes := ft.responses()
	if len(writes) == 0 {
		t.Fatal("no response captured")
	}

	var out struct {
		State       string `json:"State"`
		TotalJoined int64  `json:"TotalJoined"`
		CacheHits   int64  `json:"CacheHits"`
		CacheMisses int64  `json:"CacheMisses"`
	}
	if err := json.Unmarshal(writes[len(writes)-1].Result, &out); err != nil {
		t.Fatalf("unmarshal status result: %v", err)
	}
	if out.TotalJoined != 0 {
		t.Errorf("TotalJoined = %d, want 0 (nothing joined yet)", out.TotalJoined)
	}
	// mockBackend implements no CacheStats — counters stay zero, proving the
	// optional-interface aggregation tolerates backends without metrics.
	if out.CacheHits != 0 || out.CacheMisses != 0 {
		t.Errorf("cache counters = (%d,%d), want zeros without a provider", out.CacheHits, out.CacheMisses)
	}
}
