//go:build soak

package soak

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

type fakeSuspendResumeGuard struct {
	events           suspendResumeEvents
	closeCalled      bool
	checkBeforeClose bool
	checks           int
	eventOnClose     uint32
	closeErr         error
}

func (g *fakeSuspendResumeGuard) Check() error {
	g.checks++
	if !g.closeCalled {
		g.checkBeforeClose = true
	}
	return g.events.check()
}

func (g *fakeSuspendResumeGuard) Close() error {
	g.closeCalled = true
	if g.eventOnClose != 0 {
		g.events.record(g.eventOnClose)
	}
	return g.closeErr
}

func TestFinalizeSuspendResumeGuardChecksAfterClose(t *testing.T) {
	guard := &fakeSuspendResumeGuard{eventOnClose: 4}
	closeErr, checkErr := finalizeSuspendResumeGuard(guard)
	if closeErr != nil {
		t.Fatalf("close monitor: %v", closeErr)
	}
	if checkErr == nil || !strings.Contains(checkErr.Error(), "1 event") || !strings.Contains(checkErr.Error(), "0x4") {
		t.Fatalf("event delivered during close was not observed by final check: %v", checkErr)
	}
	if !guard.closeCalled || guard.checkBeforeClose || guard.checks != 1 {
		t.Fatalf("finalization order/count = close:%v check-before-close:%v checks:%d", guard.closeCalled, guard.checkBeforeClose, guard.checks)
	}
}

func TestFinalizeSuspendResumeGuardChecksEvenWhenCloseFails(t *testing.T) {
	closeFailure := errors.New("unregister failed")
	guard := &fakeSuspendResumeGuard{eventOnClose: 18, closeErr: closeFailure}
	closeErr, checkErr := finalizeSuspendResumeGuard(guard)
	if !errors.Is(closeErr, closeFailure) {
		t.Fatalf("close error = %v, want %v", closeErr, closeFailure)
	}
	if checkErr == nil || !strings.Contains(checkErr.Error(), "0x12") {
		t.Fatalf("final check did not run after close failure: %v", checkErr)
	}
	if guard.checks != 1 {
		t.Fatalf("final check count = %d, want 1", guard.checks)
	}
}

func TestSuspendResumeEventsRemainInvalidAfterObservation(t *testing.T) {
	var events suspendResumeEvents
	if err := events.check(); err != nil {
		t.Fatalf("empty event state rejected: %v", err)
	}
	events.record(4) // PBT_APMSUSPEND
	if err := events.check(); err == nil || !strings.Contains(err.Error(), "1 event") || !strings.Contains(err.Error(), "0x4") {
		t.Fatalf("suspend event was not reported: %v", err)
	}
	if err := events.check(); err == nil {
		t.Fatal("observed suspend event stopped invalidating the run")
	}
	events.record(18) // PBT_APMRESUMEAUTOMATIC
	if err := events.check(); err == nil || !strings.Contains(err.Error(), "2 event") || !strings.Contains(err.Error(), "0x12") {
		t.Fatalf("resume event was not reported: %v", err)
	}
}

func TestSuspendResumeEventsConcurrentCallbacksAreCounted(t *testing.T) {
	var events suspendResumeEvents
	const callbacks = 128
	var workers sync.WaitGroup
	workers.Add(callbacks)
	for i := 0; i < callbacks; i++ {
		go func(event uint32) {
			defer workers.Done()
			events.record(event)
		}(uint32(i))
	}
	workers.Wait()
	if err := events.check(); err == nil || !strings.Contains(err.Error(), "128 event") {
		t.Fatalf("concurrent callback events were lost: %v", err)
	}
}

func TestSuspendResumeCallbackGateDrainsCallbacksBeforeFinalCheck(t *testing.T) {
	gate := newSuspendResumeCallbackGate()
	var events suspendResumeEvents
	if !gate.enter() {
		t.Fatal("initial callback was rejected")
	}

	callbackRelease := make(chan struct{})
	callbackDone := make(chan struct{})
	go func() {
		defer close(callbackDone)
		defer gate.leave()
		<-callbackRelease
		events.record(4)
	}()

	closeDone := make(chan struct{})
	go func() {
		gate.closeAndWait()
		close(closeDone)
	}()
	<-gate.sealed

	gate.mu.Lock()
	if gate.active != 1 || gate.waiters != 1 {
		active, waiters := gate.active, gate.waiters
		gate.mu.Unlock()
		t.Fatalf("close did not wait for the admitted callback: active=%d waiters=%d", active, waiters)
	}
	gate.mu.Unlock()

	if gate.enter() {
		gate.leave()
		t.Fatal("callback admission remained open after close began")
	}
	select {
	case <-closeDone:
		t.Fatal("callback gate closed before its admitted callback completed")
	default:
	}

	close(callbackRelease)
	<-callbackDone
	<-closeDone
	if err := events.check(); err == nil || !strings.Contains(err.Error(), "1 event") {
		t.Fatalf("final event check missed callback drained during close: %v", err)
	}
	if gate.enter() {
		gate.leave()
		t.Fatal("callback admission reopened after close completed")
	}
}
