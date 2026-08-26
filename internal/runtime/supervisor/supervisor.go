// Package supervisor implements the backend lifecycle state machine per goal.md section G2-G5.
//
// F2 Locking policy:
//   - Protected: state, epoch, crashes, lastErr, health, onStateChange, onEpochChange
//   - All public methods acquire mu; no nested locks
//   - retryAfter goroutine owns its context; cancelled on parent ctx Done
//   - UserMessageLocked assumes mu is already held (called from AllowRequest under lock)
//
// F15 Goroutine policy:
//   - retryAfter goroutine: owned by MarkUnhealthy, terminated by parentCtx cancellation
//   - No goroutine per request; one retry goroutine per crash

// Invariants:
//  1. BackendEpoch increments on each successful start (INV-BACKEND-001).
//  2. A result from epoch N MUST NOT be published as epoch N+1.
//  3. Restart delays are bounded; crash loops transition to Quarantined.
//  4. Every goroutine has a clear owner and termination condition (F15).
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omnilsp/omni/internal/trust"
)

type State int

const (
	StateDisabled State = iota
	StateStarting
	StateReady
	StateDegraded
	StateUnhealthy
	StateBackoff
	StateQuarantined
)

func (s State) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateDegraded:
		return "degraded"
	case StateUnhealthy:
		return "unhealthy"
	case StateBackoff:
		return "backoff"
	case StateQuarantined:
		return "quarantined"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

type BackendHealth struct {
	ProcessAlive      bool
	RPCResponsive     bool
	WorkspaceLoaded   bool
	BuildContextValid bool
	IndexReady        bool
	LastSuccess       time.Time
	FailureClass      string
}

func (h BackendHealth) IsHealthy() bool {
	return h.ProcessAlive && h.RPCResponsive && h.WorkspaceLoaded && h.BuildContextValid
}

type Config struct {
	MaxConsecutiveCrashes int
	InitialBackoff        time.Duration
	MaxBackoff            time.Duration
	HungGrace             time.Duration
	// Trust, when non-nil, gates backend start on workspace trust (§N1/N3).
	// A refused start keeps the supervisor Disabled and reports the reason
	// through the state-change callback.
	Trust *trust.Policy
}

func DefaultConfig() Config {
	return Config{
		MaxConsecutiveCrashes: 5,
		InitialBackoff:        100 * time.Millisecond,
		MaxBackoff:            10 * time.Second,
		HungGrace:             5 * time.Second,
	}
}

type Supervisor struct {
	mu            sync.Mutex
	cfg           Config
	state         State
	epoch         atomic.Uint64
	crashes       int
	lastErr       error
	startAt       time.Time
	health        BackendHealth
	onStateChange func(State, State, error)
	onEpochChange func(uint64)
}

func New(cfg Config) *Supervisor {
	if cfg.MaxConsecutiveCrashes <= 0 {
		cfg.MaxConsecutiveCrashes = 5
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = 100 * time.Millisecond
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = 10 * time.Second
	}
	return &Supervisor{cfg: cfg, state: StateDisabled}
}

func (s *Supervisor) Start() uint64 {
	s.mu.Lock()
	old := s.state
	if old != StateDisabled && old != StateQuarantined && old != StateUnhealthy {
		ep := s.epoch.Load()
		s.mu.Unlock()
		return ep
	}
	// Workspace trust gate (§N1/N3): an untrusted workspace refuses backend
	// process starts. The supervisor stays Disabled; the refusal is surfaced
	// via the state-change callback.
	if s.cfg.Trust != nil {
		if err := s.cfg.Trust.CanStartBackend("supervised-backend"); err != nil {
			s.mu.Unlock()
			s.notify(old, StateDisabled, err)
			return s.epoch.Load()
		}
	}
	s.state = StateStarting
	s.startAt = time.Now()
	s.mu.Unlock()
	// Callbacks fire outside the lock: a callback calling State()/Health()
	// would otherwise self-deadlock (F2).
	s.notify(old, StateStarting, nil)
	return s.epoch.Load()
}

func (s *Supervisor) MarkReady() uint64 {
	s.mu.Lock()
	s.epoch.Add(1)
	ep := s.epoch.Load()
	s.state = StateReady
	s.crashes = 0
	s.lastErr = nil
	s.health = BackendHealth{
		ProcessAlive:      true,
		RPCResponsive:     true,
		WorkspaceLoaded:   true,
		BuildContextValid: true,
		LastSuccess:       time.Now().UTC(),
	}
	s.mu.Unlock()
	s.notify(StateStarting, StateReady, nil)
	s.notifyEpoch(ep)
	return ep
}

func (s *Supervisor) MarkDegraded(class string) {
	s.mu.Lock()
	s.state = StateDegraded
	s.health.FailureClass = class
	s.health.LastSuccess = time.Now().UTC()
	s.mu.Unlock()
	s.notify(StateReady, StateDegraded, nil)
}

func (s *Supervisor) MarkUnhealthy(ctx context.Context, err error) {
	s.mu.Lock()
	s.crashes++
	s.lastErr = err
	prev := s.state
	s.state = StateUnhealthy
	s.health.ProcessAlive = false
	s.health.RPCResponsive = false
	// Quarantine decision and backoff derivation both read s.crashes — they
	// must happen under the lock or concurrent crash events race (F2).
	shouldQuarantine := s.crashes >= s.cfg.MaxConsecutiveCrashes
	delay := s.nextBackoffLocked()
	s.mu.Unlock()

	// Zero callbacks while holding mu (see Start).
	s.notify(prev, StateUnhealthy, err)

	if shouldQuarantine {
		s.mu.Lock()
		s.state = StateQuarantined
		s.mu.Unlock()
		s.notify(StateUnhealthy, StateQuarantined, err)
		return
	}

	go s.retryAfter(ctx, delay, err)
}

func (s *Supervisor) retryAfter(parentCtx context.Context, delay time.Duration, lastErr error) {
	select {
	case <-parentCtx.Done():
		return
	case <-time.After(delay):
	}

	s.mu.Lock()
	if s.state != StateUnhealthy && s.state != StateDegraded {
		s.mu.Unlock()
		return
	}
	s.state = StateBackoff
	s.mu.Unlock()
	s.notify(StateUnhealthy, StateBackoff, lastErr)

	s.mu.Lock()
	s.state = StateStarting
	s.mu.Unlock()

	_ = s.epoch.Load() // epoch does not change on restart; just start the cycle
}

// nextBackoffLocked derives the restart delay from the crash count; caller
// must hold mu.
func (s *Supervisor) nextBackoffLocked() time.Duration {
	base := time.Duration(s.cfg.InitialBackoff)
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	maxBase := time.Duration(s.cfg.MaxBackoff)
	if maxBase <= 0 {
		maxBase = 10 * time.Second
	}
	exp := s.crashes - 1
	if exp < 0 {
		exp = 0
	}
	delay := base
	multiplier := 1
	ratio := int(maxBase / base)
	for i := 0; i < exp; i++ {
		multiplier *= 2
		if multiplier > ratio {
			multiplier = ratio
			break
		}
	}
	delay = time.Duration(int64(base) * int64(multiplier))
	if delay > maxBase {
		delay = maxBase
	}
	jitter := int64(delay) / 4
	delay = delay + time.Duration(rand.Int63n(2*jitter)) - time.Duration(jitter)
	if delay < base {
		delay = base
	}
	return delay
}

// HandleHungWorker cancels and terminates a hung worker. cancelFn aborts the
// worker's in-flight RPC; terminateFn must block until the worker process has
// actually exited. HandleHungWorker returns once termination is confirmed.
func (s *Supervisor) HandleHungWorker(cancelFn func(), terminateFn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		cancelFn()
		if terminateFn != nil {
			terminateFn()
		}
	}()
	<-done
}

func (s *Supervisor) Epoch() uint64           { return s.epoch.Load() }
func (s *Supervisor) State() State            { s.mu.Lock(); defer s.mu.Unlock(); return s.state }
func (s *Supervisor) Health() BackendHealth   { s.mu.Lock(); defer s.mu.Unlock(); return s.health }
func (s *Supervisor) ConsecutiveCrashes() int { s.mu.Lock(); defer s.mu.Unlock(); return s.crashes }
func (s *Supervisor) LastError() error        { s.mu.Lock(); defer s.mu.Unlock(); return s.lastErr }

func (s *Supervisor) UserMessage() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.userMessageLocked()
}

func (s *Supervisor) userMessageLocked() string {
	st, cr, le := s.state, s.crashes, s.lastErr
	switch st {
	case StateDisabled:
		return "Backend disabled"
	case StateStarting:
		return "Backend starting..."
	case StateReady:
		return fmt.Sprintf("Backend ready (epoch %d)", s.epoch.Load())
	case StateDegraded:
		return fmt.Sprintf("Backend degraded: %s (epoch %d)", s.health.FailureClass, s.epoch.Load())
	case StateUnhealthy:
		msg := fmt.Sprintf("Backend unhealthy, restarting (crash #%d, epoch %d)", cr, s.epoch.Load())
		if le != nil {
			msg += fmt.Sprintf(": %v", le)
		}
		return msg + ". Recovery in progress."
	case StateBackoff:
		return "Backend backing off before restart..."
	case StateQuarantined:
		msg := fmt.Sprintf("Backend quarantined: too many consecutive crashes (%d)", cr)
		if le != nil {
			msg += fmt.Sprintf(". Last error: %v", le)
		}
		return msg + ". Manual intervention required."
	default:
		return "Unknown state"
	}
}

func (s *Supervisor) RegisterCallbacks(onChange func(State, State, error), onEpoch func(uint64)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onStateChange = onChange
	s.onEpochChange = onEpoch
}

func (s *Supervisor) notify(from, to State, err error) {
	// Appendix D: "Any transition away from Ready MUST be observable." The
	// stderr line is the always-on observer — structured consumers attach
	// via RegisterCallbacks.
	if from == StateReady && to != StateReady {
		fmt.Fprintf(os.Stderr, "omnilsp: supervisor: %s -> %s (%v)\n", from, to, err)
	}
	if s.onStateChange != nil {
		s.onStateChange(from, to, err)
	}
}

func (s *Supervisor) notifyEpoch(ep uint64) {
	if s.onEpochChange != nil {
		s.onEpochChange(ep)
	}
}

var ErrQuarantined = errors.New("backend quarantined: too many consecutive failures, manual intervention required")

func (s *Supervisor) AllowRequest() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case StateReady, StateDegraded:
		return nil
	case StateQuarantined:
		msg := s.userMessageLocked()
		return fmt.Errorf("%w: %s", ErrQuarantined, msg)
	case StateDisabled:
		return errors.New("backend not started")
	default:
		return fmt.Errorf("backend in transitional state %s", s.state)
	}
}
