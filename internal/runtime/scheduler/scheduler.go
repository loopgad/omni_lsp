// Package scheduler implements the canonical request admission, prioritization,
// and execution engine for OmniLSP.
//
// Invariants:
//  1. Every queue channel is bounded (F6).
//  2. Cancellation is propagated via context.Context from LSP request to worker (C7).
//  3. Stale writes cannot publish results (stale snapshot rejection in handler).
//  4. P0 requests are never starved by P4/P5 background work (F7 fairness).
//
// Corresponds to goal.md §F3–F12.
package scheduler

// Invariants:
//  1. All requests routed through bounded queues per F6.
//  2. Priority aging prevents starvation per F7.
//  3. Shutdown is graceful: in-flight work completes, new work rejected (F15).
//
// F2 Locking policy:
//   - Protected: queues[9], config, closed, inFlight, stats
//   - No nested locks; atomic ops for inFlight/stats counters
//   - No blocking I/O under lock
//   - Goroutines owned by Start/Shutdown lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// Priority defines request priority levels per goal.md §F3.
// CostClass classifies requests by resource cost per goal.md §F8.
// Large work MUST have stricter concurrency caps than Tiny work.
type CostClass int

const (
	CostTiny      CostClass = iota // local lookup / cache hit
	CostSmall                      // single-file parse / query
	CostMedium                     // package-level semantic work
	CostLarge                      // workspace references / index query
	CostVeryLarge                  // full indexing / rebuild
)

func (c CostClass) String() string {
	switch c {
	case CostTiny:
		return "tiny"
	case CostSmall:
		return "small"
	case CostMedium:
		return "medium"
	case CostLarge:
		return "large"
	case CostVeryLarge:
		return "very_large"
	default:
		return "unknown"
	}
}

type Priority int

const (
	PriorityCompletion Priority = iota // P0: immediate interaction
	PriorityHover
	PrioritySignature
	PriorityDefinition // P1: navigation
	PriorityReferences
	PriorityDiagnostics    // P2: correctness feedback
	PrioritySemanticTokens // P3: rich editor data
	PriorityIndexing       // P4: background throughput
	PriorityMaintenance    // P5: opportunistic
)

func (p Priority) String() string {
	switch p {
	case PriorityCompletion:
		return "P0-completion"
	case PriorityHover:
		return "P1-hover"
	case PrioritySignature:
		return "P1-signature"
	case PriorityDefinition:
		return "P1-definition"
	case PriorityReferences:
		return "P1-references"
	case PriorityDiagnostics:
		return "P2-diagnostics"
	case PrioritySemanticTokens:
		return "P3-semantic-tokens"
	case PriorityIndexing:
		return "P4-indexing"
	case PriorityMaintenance:
		return "P5-maintenance"
	default:
		return fmt.Sprintf("P%d", int(p))
	}
}

// ClientID mirrors the wire JSON-RPC request-ID triple without importing the
// protocol package (INV-ARCH-002: the scheduler is core). The server layer
// converts to/from jsonrpc.RequestID at the boundary.
type ClientID struct {
	Str    string
	Num    int64
	IsStr  bool
	IsNull bool
}

// Request represents a scheduled request.
// Corresponds to goal.md §F4 (Scheduler request record).
type Request struct {
	RequestID identity.RequestID
	Workspace identity.WorkspaceID

	// ClientID mirrors the wire request-ID triple without importing the
	// protocol package (INV-ARCH-002: scheduler is core). The server layer
	// converts to/from jsonrpc.RequestID at the boundary.
	// OriginalID is the client-visible JSON-RPC request ID (set by the server
	// layer; nil for notifications). Error responses MUST echo it instead of
	// the internal RequestID string, which is not a wire-format ID.
	OriginalID   *ClientID
	Snapshot     *snapshot.Snapshot
	SnapshotID   identity.SnapshotID
	BuildContext identity.BuildContextID
	CostClass    CostClass // F8: resource cost classification
	Priority     Priority
	Deadline     time.Time // zero means no deadline
	CoalesceKey  string
	ClientClass  string
	EnqueuedAt   time.Time
	Execute      func(ctx context.Context, snap *snapshot.Snapshot) (any, error)
	Result       chan Result

	// Cancellation support (C7/F11). cancelMu guards cancel/cancelled so a
	// Cancel() racing with worker start is not lost.
	cancelMu  sync.Mutex
	cancel    context.CancelFunc
	cancelled atomic.Bool

	// F11: lazily created broadcast channel closed on Cancel so waiters that
	// joined a shared computation wake up instead of waiting for its result.
	wakeOnce  sync.Once
	wake      chan struct{}
	closeOnce sync.Once
}

// Cancel cancels the request. If it is still queued the worker will skip it;
// if it is running its context is cancelled; if it joined a shared
// computation (F11) its waiter goroutine wakes with Canceled. Safe to call
// multiple times and from any goroutine.
func (r *Request) Cancel() {
	r.cancelMu.Lock()
	r.cancelled.Store(true)
	if r.cancel != nil {
		r.cancel()
	}
	r.cancelMu.Unlock()

	r.wakeOnce.Do(func() { r.wake = make(chan struct{}) })
	r.closeWake()
}

// wakeCh returns the lazily created cancellation broadcast channel (F11).
func (r *Request) wakeCh() <-chan struct{} {
	r.wakeOnce.Do(func() { r.wake = make(chan struct{}) })
	return r.wake
}

// closeWake closes the wake channel exactly once (caller of Cancel only).
func (r *Request) closeWake() {
	ch := r.wake
	r.closeOnce.Do(func() { close(ch) })
}

// Cancelled reports whether Cancel has been called.
func (r *Request) Cancelled() bool { return r.cancelled.Load() }

// Result is the outcome of a scheduled request.
type Result struct {
	Value any
	Err   error
}

// AdmissionResult is the outcome of admission control.
type AdmissionResult int

const (
	Admitted AdmissionResult = iota
	RejectedQueueFull
	RejectedCancelled
	RejectedStaleSnapshot
	RejectedDuplicateCoalesced
)

func (r AdmissionResult) String() string {
	switch r {
	case Admitted:
		return "admitted"
	case RejectedQueueFull:
		return "rejected:queue-full"
	case RejectedCancelled:
		return "rejected:cancelled"
	case RejectedStaleSnapshot:
		return "rejected:stale-snapshot"
	case RejectedDuplicateCoalesced:
		return "rejected:duplicate-coalesced"
	default:
		return "unknown"
	}
}

// Config configures the scheduler.
type Config struct {
	MaxConcurrent   int
	MaxQueueSize    int // per-priority, total max = MaxQueueSize * numPriorities
	MaxInFlight     int // global in-flight cap (admission control)
	PriorityAgingMs int // after this many ms, boost a queued request by one priority
}

func DefaultConfig() Config {
	return Config{
		MaxConcurrent:   8,
		MaxQueueSize:    64,
		MaxInFlight:     256,
		PriorityAgingMs: 5000,
	}
}

// Scheduler manages request admission, prioritization, and execution.
//
// Architecture:
//   - Separate bounded channel per priority (P0–P5).
//   - Worker pool fetches from highest non-empty priority (with aging boost).
//   - Admission control rejects when in-flight or queue budget exceeded.
type Scheduler struct {
	config Config
	queues [9]chan *Request // indexed by Priority
	closed atomic.Bool
	cancel context.CancelFunc

	// queueMu arbitrates queue send (trySend, RLock) against Shutdown's
	// close(queues) (Lock): a bare closed-flag double-check cannot make
	// chansend/closechan race-free at the runtime level — the race detector
	// correctly flags any send that overlaps the close window.
	queueMu sync.RWMutex

	// F11/J6: single-flight joining for coalescable read-only requests.
	tracker *InFlightTracker

	// Wake-up signal for idle workers (replaces 10ms busy-polling): Submit
	// sends a non-blocking token after enqueue; workers block on it.
	notify chan struct{}

	inFlight      atomic.Int64
	totalEnqueued atomic.Int64
	totalRejected atomic.Int64
	totalExecuted atomic.Int64
	totalJoined   atomic.Int64 // F11: requests served by an in-flight twin
}

func New(cfg Config) *Scheduler {
	s := &Scheduler{
		config:  cfg,
		tracker: NewInFlightTracker(),
		notify:  make(chan struct{}, 1),
	}
	for i := range s.queues {
		s.queues[i] = make(chan *Request, cfg.MaxQueueSize)
	}
	return s
}

// Start launches the worker pool and background aging goroutine.
func (s *Scheduler) Start(ctx context.Context) {
	ctx, s.cancel = context.WithCancel(ctx)

	// Launch workers.
	for i := 0; i < s.config.MaxConcurrent; i++ {
		go s.worker(ctx)
	}

	// Background priority aging.
	go s.agingLoop(ctx)
}

// trySend enqueues a request without racing Shutdown: the closed flag is
// re-checked inside the select window, and a send that would hit a closed
// queue is dropped instead of panicking. The caller owns the "request never
// reaches a worker" consequence (report rejection / fail the Result).
func (s *Scheduler) trySend(pq int, req *Request) bool {
	if s.closed.Load() {
		return false
	}
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	select {
	case s.queues[pq] <- req:
		return true
	default:
		return false
	}
}

// Submit admits and queues a request. Returns the admission result.
// If Admitted, the caller MUST read from req.Result to avoid goroutine leak.
// Submit creates the Result channel if not already set.
func (s *Scheduler) Submit(req *Request) AdmissionResult {
	if s.closed.Load() {
		return RejectedCancelled
	}

	// Create result channel if not provided.
	if req.Result == nil {
		req.Result = make(chan Result, 1)
	}

	// F11/J6: identical coalescable requests join the in-flight twin instead
	// of duplicating work. Waiters do not occupy worker or in-flight budget.
	if req.CoalesceKey != "" {
		sc, leader := s.tracker.acquire(req.CoalesceKey)
		if !leader {
			s.totalJoined.Add(1)
			go func() {
				select {
				case <-sc.done:
					req.Result <- Result{Value: sc.value, Err: sc.err}
				case <-req.wakeCh():
					req.Result <- Result{Err: context.Canceled}
				}
			}()
			return Admitted
		}
	}

	// In-flight budget check (F5 admission control).
	if int(s.inFlight.Load()) >= s.config.MaxInFlight {
		s.totalRejected.Add(1)
		s.deliverResult(req, nil, fmt.Errorf("scheduler: in-flight budget exceeded"))
		return RejectedQueueFull
	}

	// Per-priority queue full check.
	pq := int(req.Priority)
	if pq >= len(s.queues) {
		pq = len(s.queues) - 1
	}
	if s.trySend(pq, req) {
		s.totalEnqueued.Add(1)
		select { // wake an idle worker immediately (no polling delay)
		case s.notify <- struct{}{}:
		default:
		}
		return Admitted
	}
	s.totalRejected.Add(1)
	s.deliverResult(req, nil, fmt.Errorf("scheduler: queue full"))
	return RejectedQueueFull
}

// deliverResult completes the leader's shared call, broadcasting (val, err)
// to all joined waiters. Called on EVERY worker exit path (F11 duty).
func (s *Scheduler) deliverResult(req *Request, val any, err error) {
	if req.CoalesceKey != "" {
		s.tracker.Deliver(req.CoalesceKey, val, err)
	}
}

// deliverRollback is the failure-path form used before execution: waiters
// receive err instead of the leader's (never-produced) value.
func (s *Scheduler) deliverRollback(req *Request, err error) {
	s.deliverResult(req, nil, err)
}

func (s *Scheduler) worker(ctx context.Context) {
	for {
		req := s.pickRequest(ctx)
		if req == nil {
			return
		}

		// Check cancellation before executing.
		select {
		case <-ctx.Done():
			s.deliverRollback(req, ctx.Err())
			select { // non-blocking: caller may have abandoned req.Result
			case req.Result <- Result{Err: ctx.Err()}:
			default:
			}
			continue
		default:
		}

		// Check deadline.
		if !req.Deadline.IsZero() && time.Now().After(req.Deadline) {
			err := fmt.Errorf("scheduler: request deadline exceeded")
			s.deliverRollback(req, err)
			select { // non-blocking: caller may have abandoned req.Result
			case req.Result <- Result{Err: err}:
			default:
			}
			continue
		}

		s.inFlight.Add(1)

		execCtx, cancelExec := context.WithCancel(ctx)
		req.cancelMu.Lock()
		if req.cancelled.Load() {
			// Cancelled while queued (C7): skip execution entirely.
			req.cancelMu.Unlock()
			cancelExec()
			s.inFlight.Add(-1)
			s.deliverRollback(req, context.Canceled)
			select {
			case req.Result <- Result{Err: context.Canceled}:
			default:
			}
			continue
		}
		req.cancel = cancelExec
		req.cancelMu.Unlock()

		s.totalExecuted.Add(1)

		// Execute with request-scoped context for cancellation propagation (C7).
		val, err := req.Execute(execCtx, req.Snapshot)
		cancelExec()
		// Note: cancelExec() cancels execCtx after Execute returns. We must NOT
		// check execCtx.Err() here — it will always be cancelled at this point.
		// The only case where err should be ctx.Err() is if Execute itself
		// observed the parent ctx cancellation during execution.

		s.deliverResult(req, val, err) // F11: broadcast real values to joined waiters

		res := Result{Value: val, Err: err}
		// Try buffered send first (non-blocking), then blocking with context awareness.
		select {
		case req.Result <- res:
		default:
			// Result channel already consumed or full; request is done.
		}
		s.inFlight.Add(-1)
	}
}

// pickRequest selects the highest-priority ready request. Idle workers block
// on the notify signal instead of polling — Submit wakes one immediately.
func (s *Scheduler) pickRequest(ctx context.Context) *Request {
	for {
		for pq := 0; pq < len(s.queues); pq++ {
			select {
			case req := <-s.queues[pq]:
				return req
			default:
			}
		}
		// No requests ready; sleep until a Submit signals or shutdown.
		select {
		case <-ctx.Done():
			return nil
		case <-s.notify:
		}
	}
}

// agingLoop boosts priorities of long-queued requests to prevent starvation (F7).
func (s *Scheduler) agingLoop(ctx context.Context) {
	interval := time.Duration(s.config.PriorityAgingMs) / 2
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond // minimum sane interval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.boostStaleRequests()
		}
	}
}

// boostStaleRequests lifts requests that have been queued longer than PriorityAgingMs.
func (s *Scheduler) boostStaleRequests() {
	boosted := false
	for pq := PriorityMaintenance; pq > PriorityCompletion; pq-- {
		src := s.queues[pq]
		if len(src) == 0 {
			continue
		}
		destPq := pq - 1
		// Drain one stale request and re-queue at higher priority.
		select {
		case req := <-src:
			if time.Since(req.EnqueuedAt) <= time.Duration(s.config.PriorityAgingMs) {
				// Not stale yet, put back; never drop silently.
				if !s.trySend(int(pq), req) {
					s.rejectDropped(req, "scheduler: dropped during priority aging")
				}
				continue
			}
			if s.trySend(int(destPq), req) {
				boosted = true
			} else if !s.trySend(int(pq), req) {
				// Destination full AND origin refused: fail fast instead of
				// vanishing (historical double-default silent drop).
				s.rejectDropped(req, "scheduler: queues saturated during priority aging")
			}
		default:
		}
	}
	if boosted {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

// rejectDropped terminates a request that could not be re-queued: it writes
// the rejection to req.Result (non-blocking — Result is buffered with
// capacity 1 and the submitter may already be gone) and rolls back any
// coalescing group so joined waiters share the same terminal outcome.
func (s *Scheduler) rejectDropped(req *Request, msg string) {
	err := errors.New(msg)
	s.totalRejected.Add(1)
	s.deliverRollback(req, err)
	select {
	case req.Result <- Result{Err: err}:
	default:
	}
}

// Shutdown gracefully stops the scheduler.
func (s *Scheduler) Shutdown() {
	if s.closed.CompareAndSwap(false, true) {
		if s.cancel != nil {
			s.cancel()
		}
		// Close all queues to unblock workers. The write lock excludes
		// concurrent trySend senders, making close-vs-send race-free.
		s.queueMu.Lock()
		for i := range s.queues {
			close(s.queues[i])
		}
		s.queueMu.Unlock()
	}
}

// Stats returns current scheduler statistics.
type Stats struct {
	InFlight      int64
	TotalEnqueued int64
	TotalRejected int64
	TotalExecuted int64
	TotalJoined   int64 // F11: served by an in-flight twin
	QueueLengths  [9]int
}

func (s *Scheduler) Stats() Stats {
	st := Stats{}
	for i := range s.queues {
		st.QueueLengths[i] = len(s.queues[i])
	}
	st.InFlight = s.inFlight.Load()
	st.TotalEnqueued = s.totalEnqueued.Load()
	st.TotalRejected = s.totalRejected.Load()
	st.TotalExecuted = s.totalExecuted.Load()
	st.TotalJoined = s.totalJoined.Load()
	return st
}

// QueueLen returns the total number of requests currently queued.
func (s *Scheduler) QueueLen() int {
	n := 0
	for i := range s.queues {
		n += len(s.queues[i])
	}
	return n
}
