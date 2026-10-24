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

// cacheStatsBackend opts a mock into the optional §K0 cache-metrics interface
// with canned counters; it locks the aggregation arithmetic, not any real
// backend's accounting.
type cacheStatsBackend struct {
	mockBackend
	hits, misses int64
}

func (b *cacheStatsBackend) CacheStats() (int64, int64) { return b.hits, b.misses }

func TestK_StatusAggregatesCacheStatsAcrossBackends(t *testing.T) {
	s := New(DefaultConfig())
	s.RegisterBackend("go", &cacheStatsBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}}, hits: 11, misses: 3,
	})
	s.RegisterBackend("rust", &cacheStatsBackend{
		mockBackend: mockBackend{langID: "rust", exts: []string{".rs"}}, hits: 5, misses: 7,
	})
	// A backend without the interface must neither contribute nor break the
	// aggregation loop.
	s.RegisterBackend("python", &mockBackend{langID: "python", exts: []string{".py"}})

	resp := s.dispatcher.Dispatch(context.Background(),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 8}, "omnilsp/status", nil))
	if resp == nil || resp.Error != nil {
		t.Fatalf("status failed: %v", resp.Error)
	}
	var out struct {
		CacheHits   int64 `json:"CacheHits"`
		CacheMisses int64 `json:"CacheMisses"`
	}
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("unmarshal status result: %v", err)
	}
	if out.CacheHits != 16 || out.CacheMisses != 10 {
		t.Errorf("cache counters = (%d,%d), want aggregated (16,10) from the two providers", out.CacheHits, out.CacheMisses)
	}
}
