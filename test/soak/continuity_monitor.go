package soak

// This file is deliberately untagged: the continuity monitor is pure harness
// logic (no OS registrations, no server processes), so its form checks run in
// the default suite per the 2026-10-25 ruling 3. The platform
// newSuspendResumeGuard constructors and the real-stdio soak workload stay
// behind the soak build tag.

import (
	"fmt"
	"sync/atomic"
	"time"
)

const (
	soakPollPeriod = 5 * time.Second

	// monitorAdvanceAbandonLimit replaces the previous in-loop
	// "active == 0 && time.Since(startAt) > 30s" check: the dedicated monitor
	// goroutine advances active duration now, so the main loop instead aborts
	// when the monitor heartbeat (updated on every processed tick) is this
	// stale. The 30s threshold and the abort message are unchanged.
	monitorAdvanceAbandonLimit = 30 * time.Second
)

// suspendResumeGuard is the platform continuity monitor contract. The real
// constructors live in the tagged platform files: soak && windows registers
// PowerRegisterSuspendResumeNotification, soak && !windows refuses so a
// non-Windows run cannot silently skip continuity monitoring.
type suspendResumeGuard interface {
	Check() error
	Close() error
}

// soakMonitorFatal is a continuity or liveness fatality detected by the
// dedicated monitor goroutine. The main test goroutine turns it into evidence
// entries and t.Fatal, because testing.T may only be touched from the goroutine
// that started the test.
type soakMonitorFatal struct {
	checkID     string
	message     string
	notVerified bool
	continuity  bool
}

// soakContinuityMonitor owns the soak's 5s continuity tick. It runs on its own
// goroutine so active-duration advancement, the suspend/resume guard, and the
// wall/monotonic sampling no longer queue behind blocking request bodies on
// the main goroutine (gate-20261025T1404Z: the tick shared the main goroutine
// with those bodies, starved for the whole run, and the harness aborted a
// healthy run with "active duration monitor did not advance").
//
// True starvation still aborts: a monitor tick delayed past 2*soakPollPeriod,
// a suspend/resume event, or a query-progress stall is fatal, and the main
// loop additionally watches the heartbeat for a monitor that stopped
// advancing. Heavy per-tick work (scheduler sampling, minute telemetry,
// restarts) stays on the main loop and is triggered by dropped-when-busy tick
// notifications, matching the old time.Ticker buffer-one drop semantics.
type soakContinuityMonitor struct {
	ticks       chan struct{}
	fatal       chan soakMonitorFatal
	heartbeat   atomic.Int64 // UnixNano of the last processed tick
	active      atomic.Int64 // active elapsed duration in nanoseconds
	maintenance atomic.Int32 // known blocking blocks: progress stalls are suspended
	stop        chan struct{}
	done        chan struct{}
}

func startSoakContinuityMonitor(guard suspendResumeGuard, completed *atomic.Uint64) *soakContinuityMonitor {
	monitor := &soakContinuityMonitor{
		ticks: make(chan struct{}, 1),
		fatal: make(chan soakMonitorFatal, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	monitor.heartbeat.Store(time.Now().UnixNano())
	ticker := time.NewTicker(soakPollPeriod)
	go func() {
		defer close(monitor.done)
		defer ticker.Stop()
		monitor.run(guard, completed, ticker.C)
	}()
	return monitor
}

func (m *soakContinuityMonitor) activeDuration() time.Duration {
	return time.Duration(m.active.Load())
}

// beginMaintenance marks a known blocking block. The monitor keeps sampling
// wall/monotonic time, but query-progress stalls are suppressed and the
// progress baseline is reset so the stall clock starts after the block.
func (m *soakContinuityMonitor) beginMaintenance() { m.maintenance.Add(1) }

func (m *soakContinuityMonitor) endMaintenance() { m.maintenance.Add(-1) }

// halt stops the monitor goroutine and waits for it. Call only once, from the
// main test goroutine.
func (m *soakContinuityMonitor) halt() {
	close(m.stop)
	<-m.done
}

// run processes ticks until stopped or a guard fires. It is synchronous so
// focused tests can drive the tick channel directly.
func (m *soakContinuityMonitor) run(guard suspendResumeGuard, completed *atomic.Uint64, ticks <-chan time.Time) {
	lastMono, lastWall := time.Now(), time.Now().UTC()
	lastProgressCount := completed.Load()
	lastProgressAt := lastMono
	for {
		select {
		case <-m.stop:
			return
		case now := <-ticks:
			if err := guard.Check(); err != nil {
				m.fatal <- soakMonitorFatal{checkID: "soak/continuity", message: err.Error(), notVerified: true, continuity: true}
				return
			}
			monoDelta, wallDelta := now.Sub(lastMono), now.UTC().Sub(lastWall)
			if wallDelta < 0 || wallDelta > 2*soakPollPeriod || wallDelta-monoDelta > 20*time.Second {
				message := fmt.Sprintf("sleep/interruption invalidated the soak: wall sample gap=%s monotonic gap=%s", wallDelta, monoDelta)
				m.fatal <- soakMonitorFatal{checkID: "soak/continuity", message: message, notVerified: true, continuity: true}
				return
			}
			m.active.Add(int64(monoDelta))
			lastMono, lastWall = now, now.UTC()
			if m.maintenance.Load() > 0 {
				// A recorded maintenance block legitimately stops query
				// completions; reset the baseline so the stall clock starts
				// when the block ends.
				lastProgressCount = completed.Load()
				lastProgressAt = now
			} else {
				if progressed := completed.Load(); progressed > lastProgressCount {
					lastProgressCount = progressed
					lastProgressAt = now
				}
				if active := m.activeDuration(); active >= 2*time.Minute && now.Sub(lastProgressAt) > 2*time.Minute {
					message := fmt.Sprintf("query progress stalled for %s with %d completed requests", now.Sub(lastProgressAt), lastProgressCount)
					m.fatal <- soakMonitorFatal{checkID: "soak/query-progress", message: message, notVerified: false, continuity: false}
					return
				}
			}
			m.heartbeat.Store(time.Now().UnixNano())
			select {
			case m.ticks <- struct{}{}:
			default:
			}
		}
	}
}
