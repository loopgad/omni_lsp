package supervisor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestSupervisor_HandleHungWorker_Terminates 证明 HandleHungWorker 会调用
// terminateFn 终止 hung worker，并且函数本身会返回（修复前 done channel
// 无人关闭导致永久阻塞、terminateFn 被忽略）。
func TestSupervisor_HandleHungWorker_Terminates(t *testing.T) {
	var cancelled, terminated atomic.Bool
	cancelFn := func() { cancelled.Store(true) }
	terminateFn := func() { terminated.Store(true) }

	s := New(DefaultConfig())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.HandleHungWorker(cancelFn, terminateFn)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleHungWorker did not return")
	}
	if !cancelled.Load() {
		t.Error("cancelFn was not called")
	}
	if !terminated.Load() {
		t.Error("terminateFn was not called")
	}
}

// TestMarkDegraded locks the G3 health transition: a Ready supervisor that
// reports degradation moves to Degraded with the callback observing the
// Ready->Degraded edge, and AllowRequest still admits (degraded != dead).
func TestMarkDegraded(t *testing.T) {
	s := New(Config{MaxConsecutiveCrashes: 3})
	s.Start()
	s.MarkReady()

	var edges []string
	s.RegisterCallbacks(func(from, to State, err error) {
		edges = append(edges, from.String()+"->"+to.String())
	}, nil)

	s.MarkDegraded("slow-worker")
	if got := s.State(); got != StateDegraded {
		t.Fatalf("state = %v, want degraded", got)
	}
	if len(edges) == 0 || edges[len(edges)-1] != "ready->degraded" {
		t.Errorf("callback edges = %v, want last ready->degraded", edges)
	}
}

// TestQuarantineAfterMaxCrashes locks the G4 isolation policy: exactly
// MaxConsecutiveCrashes unhealthy events flip the supervisor into
// Quarantined, where requests are refused.
func TestQuarantineAfterMaxCrashes(t *testing.T) {
	const max = 3
	s := New(Config{MaxConsecutiveCrashes: max})
	s.Start()
	s.MarkReady()

	for i := 0; i < max; i++ {
		s.MarkUnhealthy(context.Background(), errors.New("boom"))
	}
	if got := s.State(); got != StateQuarantined {
		t.Fatalf("state after %d crashes = %v, want quarantined", max, got)
	}
	if err := s.AllowRequest(); err == nil {
		t.Error("quarantined supervisor admitted a request")
	}

	// One more crash must not corrupt state or panic (already isolated).
	s.MarkUnhealthy(context.Background(), errors.New("again"))
	if got := s.State(); got != StateQuarantined {
		t.Errorf("state = %v, want quarantined to stick", got)
	}
}
