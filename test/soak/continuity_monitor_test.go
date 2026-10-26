package soak

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// stubSuspendResumeGuard stands in for the Windows suspend/resume monitor so
// the continuity monitor's tick logic can be driven without OS registrations.
type stubSuspendResumeGuard struct {
	checkErr error
	closeErr error
	closes   atomic.Int64
}

func (g *stubSuspendResumeGuard) Check() error { return g.checkErr }

func (g *stubSuspendResumeGuard) Close() error {
	g.closes.Add(1)
	return g.closeErr
}

func newMonitorUnderTest(completed *atomic.Uint64, tickBuffer int) (*soakContinuityMonitor, chan time.Time, chan struct{}) {
	monitor := &soakContinuityMonitor{
		ticks: make(chan struct{}, 1),
		fatal: make(chan soakMonitorFatal, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	ticks := make(chan time.Time, tickBuffer)
	runDone := make(chan struct{})
	go func() {
		monitor.run(&stubSuspendResumeGuard{}, completed, ticks)
		close(runDone)
	}()
	return monitor, ticks, runDone
}

func syntheticTicks(start time.Time, count int) []time.Time {
	ticks := make([]time.Time, count)
	for i := range ticks {
		// Wall-only times keep every Sub in run() on the wall clock, which is
		// equivalent for synthetic spacing and avoids manufacturing monotonic
		// readings the runtime did not produce.
		ticks[i] = start.Add(time.Duration(i+1) * soakPollPeriod)
	}
	return ticks
}

// TestSoakContinuityMonitorAdvancesWithoutTickConsumers pins the harness fix
// for gate-20261025T1404Z: the continuity tick must advance the active
// duration while the main loop is busy and nobody consumes the tick
// notifications. Under the previous arrangement the tick shared the main
// goroutine with blocking request bodies, so a busy loop starved every tick
// and the run aborted with "active duration monitor did not advance".
func TestSoakContinuityMonitorAdvancesWithoutTickConsumers(t *testing.T) {
	completed := &atomic.Uint64{}
	completed.Store(7)
	monitor, ticks, runDone := newMonitorUnderTest(completed, 64)
	base := time.Now()

	for _, tick := range syntheticTicks(base, 6) {
		select {
		case ticks <- tick:
		case <-time.After(2 * time.Second):
			t.Fatal("monitor stopped consuming ticks before the run was advancing")
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for monitor.activeDuration() < 6*soakPollPeriod {
		if time.Now().After(deadline) {
			t.Fatalf("monitor did not advance active to 30s while the main loop was busy: got %s", monitor.activeDuration())
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case fatal := <-monitor.fatal:
		t.Fatalf("a busy main loop must not fatal the monitor: %+v", fatal)
	default:
	}
	// The main loop's watchdog only needs the heartbeat to be fresh: Windows
	// wall-clock granularity makes sub-millisecond strict monotonicity
	// unobservable, so assert the property the guard actually relies on.
	if stale := time.Since(time.Unix(0, monitor.heartbeat.Load())); stale > monitorAdvanceAbandonLimit {
		t.Fatalf("monitor heartbeat stale after processed ticks: %s", stale)
	}
	if consumed := len(monitor.ticks); consumed != 1 {
		t.Fatalf("tick notification buffer = %d, want at most one pending notification", consumed)
	}

	close(monitor.stop)
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not stop after halt")
	}
}

func TestSoakContinuityMonitorFatalOnSuspendResume(t *testing.T) {
	monitor := &soakContinuityMonitor{
		ticks: make(chan struct{}, 1),
		fatal: make(chan soakMonitorFatal, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	guard := &stubSuspendResumeGuard{checkErr: errors.New("Windows suspend/resume notification observed (1 event(s))")}
	ticks := make(chan time.Time, 1)
	runDone := make(chan struct{})
	go func() {
		monitor.run(guard, &atomic.Uint64{}, ticks)
		close(runDone)
	}()

	ticks <- time.Now()
	select {
	case fatal := <-monitor.fatal:
		if fatal.checkID != "soak/continuity" || !fatal.notVerified || !fatal.continuity {
			t.Fatalf("suspend/resume fatal = %+v, want soak/continuity not_verified continuity", fatal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("suspend/resume event did not fatal the monitor")
	}
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not stop after a suspend/resume fatal")
	}
}

func TestSoakContinuityMonitorDetectsSampleGap(t *testing.T) {
	monitor := &soakContinuityMonitor{
		ticks: make(chan struct{}, 1),
		fatal: make(chan soakMonitorFatal, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	ticks := make(chan time.Time, 2)
	runDone := make(chan struct{})
	go func() {
		monitor.run(&stubSuspendResumeGuard{}, &atomic.Uint64{}, ticks)
		close(runDone)
	}()

	// A tick arriving more than 2*soakPollPeriod after the previous one means
	// the monitor goroutine itself could not run — true starvation — and must
	// abort with the unchanged sleep/interruption message.
	ticks <- time.Now()
	ticks <- time.Now().Add(3 * soakPollPeriod)
	select {
	case fatal := <-monitor.fatal:
		if fatal.checkID != "soak/continuity" || !fatal.continuity {
			t.Fatalf("sample gap fatal = %+v, want soak/continuity continuity", fatal)
		}
		if want := "sleep/interruption invalidated the soak"; len(fatal.message) < len(want) || fatal.message[:len(want)] != want {
			t.Fatalf("sample gap message = %q, want prefix %q", fatal.message, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a monitor tick gap over 2*soakPollPeriod did not fatal the monitor")
	}
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not stop after a sample gap fatal")
	}
}

func TestSoakContinuityMonitorDetectsProgressStall(t *testing.T) {
	completed := &atomic.Uint64{}
	completed.Store(3)
	monitor := &soakContinuityMonitor{
		ticks: make(chan struct{}, 1),
		fatal: make(chan soakMonitorFatal, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	ticks := make(chan time.Time, 64)
	runDone := make(chan struct{})
	go func() {
		monitor.run(&stubSuspendResumeGuard{}, completed, ticks)
		close(runDone)
	}()

	// Two minutes of advancing ticks with a frozen completed counter must
	// abort with the unchanged query-progress message.
	for _, tick := range syntheticTicks(time.Now(), 40) {
		select {
		case ticks <- tick:
		case <-time.After(2 * time.Second):
			t.Fatal("monitor stopped consuming ticks before the stall threshold")
		}
	}
	select {
	case fatal := <-monitor.fatal:
		if fatal.checkID != "soak/query-progress" || fatal.notVerified || fatal.continuity {
			t.Fatalf("progress stall fatal = %+v, want soak/query-progress failed non-continuity", fatal)
		}
		if want := "query progress stalled for"; len(fatal.message) < len(want) || fatal.message[:len(want)] != want {
			t.Fatalf("progress stall message = %q, want prefix %q", fatal.message, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a frozen completed counter over two minutes of active time did not fatal the monitor")
	}
	close(monitor.stop)
	<-runDone
}

func TestSoakContinuityMonitorSuppressesProgressStallDuringMaintenance(t *testing.T) {
	completed := &atomic.Uint64{}
	completed.Store(3)
	// Unbuffered ticks ensure every maintenance-window tick is processed while
	// the flag is still set; with a buffered channel the producer could call
	// endMaintenance first, and a later stall would make the test pass without
	// ever exercising suppression.
	monitor, ticks, runDone := newMonitorUnderTest(completed, 0)
	base := time.Now()
	monitor.beginMaintenance()
	for _, tick := range syntheticTicks(base, 40) {
		select {
		case ticks <- tick:
		case fatal := <-monitor.fatal:
			t.Fatalf("maintenance window produced a fatal: %+v", fatal)
		case <-time.After(2 * time.Second):
			t.Fatal("monitor stopped consuming ticks during maintenance")
		}
	}
	select {
	case fatal := <-monitor.fatal:
		t.Fatalf("maintenance window produced a fatal: %+v", fatal)
	default:
	}

	// After the recorded block ends, a frozen completion counter must again
	// trip the unchanged two-minute progress-stall guard. The send select also
	// watches fatal because the monitor exits as soon as it trips.
	monitor.endMaintenance()
	var stalled *soakMonitorFatal
	for _, tick := range syntheticTicks(base.Add(40*soakPollPeriod), 40) {
		select {
		case ticks <- tick:
		case fatal := <-monitor.fatal:
			stalled = &fatal
		case <-time.After(2 * time.Second):
			t.Fatal("monitor stopped consuming ticks after maintenance")
		}
		if stalled != nil {
			break
		}
	}
	if stalled == nil {
		select {
		case fatal := <-monitor.fatal:
			stalled = &fatal
		case <-time.After(2 * time.Second):
			t.Fatal("a frozen completed counter after maintenance did not fatal")
		}
	}
	if stalled.checkID != "soak/query-progress" {
		t.Fatalf("post-maintenance fatal = %+v, want soak/query-progress", *stalled)
	}
	close(monitor.stop)
	<-runDone
}

func TestSoakContinuityMonitorProgressingWorkloadDoesNotStall(t *testing.T) {
	completed := &atomic.Uint64{}
	monitor := &soakContinuityMonitor{
		ticks: make(chan struct{}, 1),
		fatal: make(chan soakMonitorFatal, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	// Unbuffered: the producer must not run ahead of the monitor, or all ticks
	// are queued with the completion counter already at its final value and the
	// monitor correctly sees the counter frozen for the whole synthetic window.
	ticks := make(chan time.Time)
	runDone := make(chan struct{})
	go func() {
		monitor.run(&stubSuspendResumeGuard{}, completed, ticks)
		close(runDone)
	}()

	// The same tick cadence with continuously advancing completions must stay
	// quiet well past the stall threshold: only true starvation aborts.
	base := time.Now()
	for i, tick := range syntheticTicks(base, 40) {
		completed.Store(uint64(i+1) * 11)
		select {
		case ticks <- tick:
		case fatal := <-monitor.fatal:
			t.Fatalf("progressing workload produced a monitor fatal: %+v", fatal)
		case <-time.After(2 * time.Second):
			t.Fatal("monitor stopped consuming ticks on a healthy workload")
		}
	}
	select {
	case fatal := <-monitor.fatal:
		t.Fatalf("progressing workload produced a monitor fatal: %+v", fatal)
	default:
	}
	close(monitor.stop)
	<-runDone
}
