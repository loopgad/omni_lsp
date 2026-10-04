//go:build soak

package soak

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// suspendResumeCallbackGate tracks callback bodies that have entered before
// notification unregistration. Closing the gate rejects later callbacks and
// waits for every admitted callback to finish before the final event check.
type suspendResumeCallbackGate struct {
	mu      sync.Mutex
	drained *sync.Cond
	active  int
	waiters int
	closed  bool
	sealed  chan struct{}
}

func newSuspendResumeCallbackGate() *suspendResumeCallbackGate {
	gate := &suspendResumeCallbackGate{sealed: make(chan struct{})}
	gate.drained = sync.NewCond(&gate.mu)
	return gate
}

func (g *suspendResumeCallbackGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.active++
	return true
}

func (g *suspendResumeCallbackGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	if g.closed && g.active == 0 {
		g.drained.Broadcast()
	}
}

func (g *suspendResumeCallbackGate) closeAndWait() {
	g.mu.Lock()
	if !g.closed {
		g.closed = true
		close(g.sealed)
	}
	for g.active != 0 {
		g.waiters++
		g.drained.Wait()
		g.waiters--
	}
	g.mu.Unlock()
}

// finalizeSuspendResumeGuard unregisters the OS notification source before the
// final latch read. Close implementations must drain callbacks already in
// flight before returning so the final check observes every event delivered
// before unregistration completed.
func finalizeSuspendResumeGuard(guard interface {
	Check() error
	Close() error
}) (closeErr, checkErr error) {
	closeErr = guard.Close()
	checkErr = guard.Check()
	return closeErr, checkErr
}

// suspendResumeEvents is shared by the platform monitor and focused tests.
// Power callbacks only update atomics so they stay safe in the OS callback
// context; every observed event permanently invalidates this run.
type suspendResumeEvents struct {
	count atomic.Uint64
	last  atomic.Uint32
}

func (s *suspendResumeEvents) record(event uint32) {
	s.last.Store(event)
	s.count.Add(1)
}

func (s *suspendResumeEvents) eventCount() uint64 {
	return s.count.Load()
}

func (s *suspendResumeEvents) check() error {
	count := s.eventCount()
	if count == 0 {
		return nil
	}
	return fmt.Errorf("Windows suspend/resume notification observed (%d event(s), last type %#x); uninterrupted soak is invalid", count, s.last.Load())
}
