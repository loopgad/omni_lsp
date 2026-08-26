package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// TestB_WakeupLatency proves idle workers wake on the Submit signal instead
// of polling. One hundred strictly serial round-trips each pay one full
// "idle → wake" transition. Under the old 10ms polling loop the expected
// total was ≥500ms (avg 5ms per wake); with notify signaling it is well
// under 50ms. The 250ms bound is generous for slow CI yet a real guard:
// polling reliably exceeded it.
func TestB_WakeupLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive")
	}
	s := New(DefaultConfig())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Shutdown()

	const n = 100
	start := time.Now()
	for i := 0; i < n; i++ {
		req := &Request{
			Priority: PriorityHover,
			Result:   make(chan Result, 1),
			Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
				return i, nil //nolint:loopclosure // read after Result received? no — value copied at send time below
			},
		}
		if res := s.Submit(req); res != Admitted {
			t.Fatalf("submit %d: %v", i, res)
		}
		select {
		case <-req.Result:
		case <-time.After(5 * time.Second):
			t.Fatalf("request %d never completed", i)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 250*time.Millisecond {
		t.Errorf("100 serial idle-wake round-trips took %v — workers appear to be polling again", elapsed)
	}
}
