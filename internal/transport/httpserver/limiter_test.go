package httpserver

import (
	"fmt"
	"testing"
)

// TestP3_PerSourceLimiterWindowResets covers the part of Allow that the HTTP
// test cannot reach: TestX6_PerSourceRateLimitBucket runs against the real
// clock, so it can only ever exercise one window. Whether the counter resets
// when the window rolls over is the whole reason this is a fixed-window
// limiter, and nothing pinned it -- a limiter that never reset would reject
// every source forever after its first minute and still pass that test.
func TestP3_PerSourceLimiterWindowResets(t *testing.T) {
	lim := newPerSourceLimiter(2)
	if !lim.Allow("a", 100) || !lim.Allow("a", 100) {
		t.Fatal("first two requests in a window must be admitted")
	}
	if lim.Allow("a", 100) {
		t.Error("third request in the same window was admitted; the limit is not being applied")
	}
	if !lim.Allow("a", 101) {
		t.Error("first request in the next window was rejected; the counter never reset")
	}
	// A different key keeps its own count rather than sharing the reset.
	if !lim.Allow("b", 100) {
		t.Error("an untouched source was rejected; counters are not per key")
	}
}

// TestP3_PerSourceLimiterBoundsCardinality covers the bounded-cardinality
// guard. Unbounded per-source buckets are a memory-growth
// vector: every distinct source address earns a permanent entry, so an
// attacker rotating source keys would otherwise grow the map without limit.
// Nothing tested that the map is actually capped.
func TestP3_PerSourceLimiterBoundsCardinality(t *testing.T) {
	lim := newPerSourceLimiter(1)
	// Keys spanning the cap, all inside one window so nothing else resets.
	for i := 0; i < perSourceLimiterMaxBuckets+1; i++ {
		lim.Allow(fmt.Sprintf("10.0.%d.%d", i/256, i%256), 7)
	}
	lim.mu.Lock()
	size := len(lim.buckets)
	lim.mu.Unlock()
	if size > perSourceLimiterMaxBuckets {
		t.Errorf("bucket map holds %d entries, want at most %d", size, perSourceLimiterMaxBuckets)
	}
}
