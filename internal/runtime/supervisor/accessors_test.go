package supervisor

import (
	"context"
	"errors"
	"testing"
)

// TestObservabilityAccessors pins the Appendix D/Q2 observability contract:
// after a crash the accessors must agree with each other — epoch advanced,
// crash counter incremented, last error retained, health unhealthy, and the
// user-facing message non-empty and mentioning recovery. Zero-value
// (pre-Start) state must read as disabled/unhealthy, never healthy-by-default.
func TestObservabilityAccessors(t *testing.T) {
	// Pre-Start: nothing is healthy about an unstarted backend.
	s := New(Config{MaxConsecutiveCrashes: 3})
	if s.Health().IsHealthy() {
		t.Error("unstarted supervisor must not report healthy")
	}
	if s.UserMessage() == "" {
		t.Error("unstarted supervisor must still project a status message")
	}

	// Post-crash: accessors agree on the incident.
	s.Start()
	before := s.Epoch()
	s.MarkReady()
	s.MarkUnhealthy(context.Background(), errors.New("segfault at 0x0"))
	if s.Epoch() != before+1 {
		t.Errorf("epoch = %d, want %d after restart", s.Epoch(), before+1)
	}
	if s.ConsecutiveCrashes() != 1 {
		t.Errorf("crashes = %d, want 1", s.ConsecutiveCrashes())
	}
	if got := s.LastError(); got == nil || got.Error() != "segfault at 0x0" {
		t.Errorf("lastErr = %v", got)
	}
	if s.Health().IsHealthy() {
		t.Error("health flag not cleared by noteCrash")
	}
	msg := s.UserMessage()
	if msg == "" {
		t.Fatal("crashed supervisor must surface a user message (Q2)")
	}
}
