// Package scheduler implements the canonical request admission, prioritization,
// and execution engine for OmniLSP.
//
// Invariants:
//  1. Every queue channel is bounded (F6).
//  2. Cancellation is propagated via context.Context from LSP request to worker (C7).
//  3. Stale writes cannot publish results (stale snapshot rejection in handler).
//  4. P0 requests are never starved by P4/P5 background work (F7 fairness).
//  5. Per-class concurrency caps bind at the pick point (F8): a request is
//     dequeued only while its class holds a token, so a worker never blocks
//     behind another class; Large/VeryLarge run under strictly smaller caps.
//     Unclassified requests resolve to DefaultCostClass at Submit (fail-closed).
//
// Corresponds to goal.md §F3–F12.
package scheduler

// Invariants:
//  1. All requests routed through bounded queues per F6.
//  2. Priority aging prevents starvation per F7.
//  3. Shutdown drains queued work but cancels in-flight Execute: s.cancel()
//     runs before the queues close, so anything already executing sees a done
//     context immediately. Queued requests do not run after cancellation: a
//     draining worker that dequeues one rolls it back with ctx.Err()
//     (runRequest). The drain guarantee is that every admitted request still
//     receives exactly one terminal Result — the aging fence (agingMu /
//     agingPending) keeps priority-aging passes from holding a request
//     between lanes while the last draining worker scans, which would
//     otherwise strand it with no terminal outcome at all.
//
// F2 Locking policy:
//   - Protected: queues[NumCostClasses][9], config, closed, inFlight, stats
//   - No nested locks; atomic ops for inFlight/stats counters
//   - No blocking I/O under lock
//   - Goroutines owned by Start/Shutdown lifecycle
//   - Lock order: agingMu before queueMu (Shutdown and the aging pass both
//     take agingMu first; trySend only ever takes queueMu).

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// CostClass classifies a request's resource cost for goal.md §F8's per-class
// concurrency caps ("Large work MUST have stricter concurrency caps"). The
// worker pool enforces one cap per class at the pick point; ClassifyCost maps
// LSP method families to classes at the dispatch point.
//
// The zero value is CostUnset, NOT CostTiny: an unclassified request must
// never silently land in the widest lane (fail-closed). Submit resolves
// CostUnset to DefaultCostClass before admission, so queues and the worker
// pool only ever see resolved classes.
type CostClass int

const (
	// CostUnset is the zero value of CostClass: "not classified". It is
	// resolved to DefaultCostClass on admission and never reaches a queue.
	CostUnset     CostClass = iota
	CostTiny                // local lookup / cache hit
	CostSmall               // single-file parse / query
	CostMedium              // package-level semantic work
	CostLarge               // workspace references / index query
	CostVeryLarge           // full indexing / rebuild
)

// NumCostClasses counts CostClass values including the CostUnset slot. Queue
// lanes and class caps are indexed by raw CostClass value, so slot 0 exists
// but carries no traffic (see resolveClass).
const NumCostClasses = CostVeryLarge + 1

// DefaultCostClass is what Submit resolves an unclassified request to.
// Medium is the safe middle: its derived cap (¾ of the pool) bounds an
// unclassified flood below the full worker pool, so classified interactive
// work always keeps workers, and unknown work can neither hog the Tiny/Small
// lanes nor dodge the strict Large/VeryLarge caps (§F8 fail-closed).
const DefaultCostClass = CostMedium

func (c CostClass) String() string {
	switch c {
	case CostUnset:
		return "unset"
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

// Priority defines request priority levels per goal.md §F3.
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
	// CostClass is the §F8 resource cost classification. The zero value
	// CostUnset means "not classified"; Submit resolves it to DefaultCostClass
	// before admission (fail-closed). ClassifyCost provides the method-family
	// mapping the server uses at its dispatch point.
	CostClass   CostClass
	Priority    Priority
	Deadline    time.Time // zero means no deadline
	CoalesceKey string
	ClientClass string
	EnqueuedAt  time.Time
	Execute     func(ctx context.Context, snap *snapshot.Snapshot) (any, error)
	Result      chan Result

	// Cancellation support (C7/F11). cancelMu guards cancel/cancelled so a
	// Cancel() racing with worker start is not lost.
	cancelMu          sync.Mutex
	cancel            context.CancelFunc
	cancelled         atomic.Bool
	cancelErr         error
	finished          chan struct{}
	finishedOnce      sync.Once
	finishedFlag      bool // guarded by cancelMu
	sharedCall        *sharedCall
	sharedRelease     func()
	sharedReleaseOnce sync.Once
	resultOnce        sync.Once

	// F11: lazily created broadcast channel closed on Cancel so waiters that
	// joined a shared computation wake up instead of waiting for its result.
	wakeOnce  sync.Once
	wake      chan struct{}
	closeOnce sync.Once
}

// Cancel completes this request independently. Non-shared work receives the
// cancellation directly; shared work continues until its final waiter leaves.
// Safe to call multiple times and from any goroutine.
func (r *Request) Cancel() {
	r.cancelWithError(context.Canceled)
}

func (r *Request) cancelWithError(err error) {
	r.cancelMu.Lock()
	if r.finishedFlag || r.cancelled.Load() {
		r.cancelMu.Unlock()
		return
	}
	r.cancelled.Store(true)
	r.cancelErr = err
	cancel := r.cancel
	r.cancel = nil
	r.finishLocked(Result{Err: err})
	r.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}

	r.wakeOnce.Do(func() { r.wake = make(chan struct{}) })
	r.closeWake()
}

func (r *Request) setCancel(cancel context.CancelFunc) {
	r.cancelMu.Lock()
	if r.finishedFlag || r.cancelled.Load() {
		r.cancelMu.Unlock()
		cancel()
		return
	}
	r.cancel = cancel
	r.cancelMu.Unlock()
}

func (r *Request) clearCancel() {
	r.cancelMu.Lock()
	r.cancel = nil
	r.cancelMu.Unlock()
}

func (r *Request) setSharedCall(sc *sharedCall, release func()) {
	releaseOnce := func() { r.sharedReleaseOnce.Do(release) }
	r.cancelMu.Lock()
	r.sharedCall = sc
	r.sharedRelease = releaseOnce
	if r.finishedFlag || r.cancelled.Load() {
		r.cancelMu.Unlock()
		releaseOnce()
		return
	}
	r.cancel = releaseOnce
	r.cancelMu.Unlock()
}

func (r *Request) releaseSharedWaiter() {
	r.cancelMu.Lock()
	release := r.sharedRelease
	r.cancelMu.Unlock()
	if release != nil {
		release()
	}
}

func (r *Request) cancelError() error {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	if r.cancelErr != nil {
		return r.cancelErr
	}
	return context.Canceled
}

func (r *Request) finish(result Result) {
	r.cancelMu.Lock()
	r.finishLocked(result)
	r.cancelMu.Unlock()
}

func (r *Request) finishLocked(result Result) {
	if r.finishedFlag || r.Result == nil {
		return
	}
	r.finishedFlag = true
	if r.finished != nil {
		r.finishedOnce.Do(func() { close(r.finished) })
	}
	r.resultOnce.Do(func() {
		select {
		case r.Result <- result:
		default:
		}
	})
}

func (r *Request) watchDeadline() {
	delay := time.Until(r.Deadline)
	if delay <= 0 {
		r.cancelWithError(context.DeadlineExceeded)
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-r.finished:
	case <-timer.C:
		r.cancelWithError(context.DeadlineExceeded)
	}
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
	PriorityAgingMs int // milliseconds; after this long queued, boost a request one priority (consumed as time.Duration(x)*time.Millisecond)

	// ClassCaps optionally overrides the per-class concurrency caps (§F8).
	// Indexed by CostClass value; the CostUnset slot is ignored. A nil map or
	// a non-positive entry derives the cap from MaxConcurrent (see
	// derivedClassCap): Tiny/Small span the pool, Medium ¾, Large ½,
	// VeryLarge ¼ — "Large work MUST have stricter concurrency caps". An
	// explicit positive entry wins, clamped to [1, MaxConcurrent].
	ClassCaps map[CostClass]int
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
//   - Separate bounded channel per priority (P0–P5) inside per-class §F8 lanes.
//   - Worker pool fetches from the highest non-empty priority whose class
//     still holds concurrency capacity (with aging boost).
//   - Admission control rejects when in-flight or queue budget exceeded.
type Scheduler struct {
	config Config
	// queues is indexed [CostClass][Priority] (§F8 lanes inside §F3 lanes).
	// The CostUnset row exists to keep the index identical to the class
	// value; it carries no traffic — resolveClass guarantees every enqueued
	// request carries a resolved class.
	queues [NumCostClasses][9]chan *Request
	closed atomic.Bool
	cancel context.CancelFunc

	// §F8 per-class concurrency tokens: classTokens[c] holds cap(c) slots; a
	// request is dequeued only while its class token is held (acquired at
	// pick time, released when execution ends), so a class at its cap simply
	// stops being picked from — workers never block behind another class.
	classTokens [NumCostClasses]chan struct{}

	// queueMu arbitrates queue send (trySend, RLock) against Shutdown's
	// close(queues) (Lock): a bare closed-flag double-check cannot make
	// chansend/closechan race-free at the runtime level — the race detector
	// correctly flags any send that overlaps the close window.
	queueMu sync.RWMutex

	// agingMu is the priority-aging fence (audit 复证圈 1, findings 1/2/3/5):
	// boostStaleRequests holds it for an entire scan so a request can never
	// sit in limbo between lanes while Shutdown closes the queues or the
	// last draining worker scans. Shutdown takes the same fence before it
	// cancels and closes, and pickRequest's final drain scan takes it so an
	// empty scan is a stable empty. agingPending counts passes waiting on or
	// holding the fence; a draining worker that scans empty while a pass is
	// pending yields and rescans instead of exiting.
	agingMu      sync.Mutex
	agingPending atomic.Int64

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
	for c := range s.queues {
		for i := range s.queues[c] {
			s.queues[c][i] = make(chan *Request, cfg.MaxQueueSize)
		}
	}
	for c := CostTiny; c <= CostVeryLarge; c++ {
		s.classTokens[c] = make(chan struct{}, classCapFor(cfg, c))
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

// trySend enqueues a request into class c's pq lane without racing Shutdown:
// the closed flag is re-checked inside the select window, and a send that
// would hit a closed queue is dropped instead of panicking. The caller owns
// the "request never reaches a worker" consequence (report rejection / fail
// the Result). c is resolved through resolveClass so the dead CostUnset row
// can never receive traffic.
func (s *Scheduler) trySend(c CostClass, pq int, req *Request) bool {
	c = resolveClass(c)
	if s.closed.Load() {
		return false
	}
	s.queueMu.RLock()
	defer s.queueMu.RUnlock()
	if s.closed.Load() {
		return false
	}
	select {
	case s.queues[c][pq] <- req:
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

	// §F8 fail-closed default: resolve an unclassified request before
	// admission so it can never reach the dead CostUnset lane or dodge the
	// per-class caps. Explicit classes pass through untouched.
	req.CostClass = resolveClass(req.CostClass)

	// Create result channel if not provided.
	if req.Result == nil {
		req.Result = make(chan Result, 1)
	}
	if req.finished == nil {
		req.finished = make(chan struct{})
	}
	if req.Cancelled() {
		req.finish(Result{Err: req.cancelError()})
		return RejectedCancelled
	}
	if !req.Deadline.IsZero() {
		go req.watchDeadline()
	}

	// F11/J6: identical coalescable requests join the in-flight twin instead
	// of duplicating work. Waiters do not occupy worker or in-flight budget.
	if req.CoalesceKey != "" {
		sc, leader := s.tracker.acquire(req.CoalesceKey)
		req.setSharedCall(sc, func() { s.tracker.release(sc) })
		if !leader {
			s.totalJoined.Add(1)
			go func() {
				select {
				case <-sc.done:
					req.releaseSharedWaiter()
					req.finish(Result{Value: sc.value, Err: sc.err})
				case <-req.wakeCh():
					req.releaseSharedWaiter()
					req.finish(Result{Err: req.cancelError()})
				}
			}()
			return Admitted
		}
	}

	// In-flight budget check (F5 admission control).
	if int(s.inFlight.Load()) >= s.config.MaxInFlight {
		s.totalRejected.Add(1)
		err := fmt.Errorf("scheduler: %w: in-flight budget exceeded", errRejected)
		s.deliverRollback(req, err)
		req.releaseSharedWaiter()
		req.finish(Result{Err: err})
		return RejectedQueueFull
	}

	// Per-priority queue full check.
	pq := int(req.Priority)
	if pq >= len(s.queues[req.CostClass]) {
		pq = len(s.queues[req.CostClass]) - 1
	}
	if s.trySend(req.CostClass, pq, req) {
		s.totalEnqueued.Add(1)
		select { // wake an idle worker immediately (no polling delay)
		case s.notify <- struct{}{}:
		default:
		}
		return Admitted
	}
	s.totalRejected.Add(1)
	err := fmt.Errorf("scheduler: %w: queue full", errRejected)
	s.deliverRollback(req, err)
	req.releaseSharedWaiter()
	req.finish(Result{Err: err})
	return RejectedQueueFull
}

// deliverResult completes the leader's shared call, broadcasting (val, err)
// to all joined waiters. Called on EVERY worker exit path (F11 duty).
func (s *Scheduler) deliverResult(req *Request, val any, err error) {
	if req.sharedCall != nil {
		s.tracker.Deliver(req.sharedCall, val, err)
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
		// pickRequest returned this request holding its §F8 class token;
		// runRequest releases it on every exit path.
		s.runRequest(ctx, req)
	}
}

// runRequest carries one dequeued request from the cancellation checks
// through execution and result delivery. The §F8 class token for
// req.CostClass was acquired by pickClassAware before the dequeue; it is
// released here on every exit path, and a wake token is posted so workers
// sleeping on class capacity re-scan the queues.
func (s *Scheduler) runRequest(ctx context.Context, req *Request) {
	defer s.releaseClass(req.CostClass)

	// Check cancellation before executing.
	select {
	case <-ctx.Done():
		req.releaseSharedWaiter()
		s.deliverRollback(req, ctx.Err())
		req.finish(Result{Err: ctx.Err()})
		return
	default:
	}

	// Deadlines belong to each request. A timed-out leader still runs for
	// its live joiners; shared work is skipped only after its last waiter
	// has left.
	if !req.Deadline.IsZero() && time.Now().After(req.Deadline) {
		req.cancelWithError(context.DeadlineExceeded)
	}
	if req.Cancelled() && (req.sharedCall == nil || s.tracker.waiterCount(req.sharedCall) == 0) {
		err := req.cancelError()
		req.releaseSharedWaiter()
		s.deliverRollback(req, err)
		req.finish(Result{Err: err})
		return
	}

	s.inFlight.Add(1)

	execCtx, cancelExec := context.WithCancel(ctx)
	if req.sharedCall != nil {
		s.tracker.setCancel(req.sharedCall, cancelExec)
	} else {
		req.setCancel(cancelExec)
	}
	if req.Cancelled() && (req.sharedCall == nil || s.tracker.waiterCount(req.sharedCall) == 0) {
		err := req.cancelError()
		cancelExec()
		req.clearCancel()
		s.inFlight.Add(-1)
		req.releaseSharedWaiter()
		s.deliverRollback(req, err)
		req.finish(Result{Err: err})
		return
	}

	s.totalExecuted.Add(1)

	// Shared execution uses a context canceled only when its final waiter
	// leaves. Panics are converted into a terminal error so worker and
	// singleflight cleanup always run.
	val, err := executeSafely(req, execCtx)
	cancelExec()
	req.clearCancel()
	// Note: cancelExec() cancels execCtx after Execute returns. We must NOT
	// check execCtx.Err() here — it will always be cancelled at this point.
	// The only case where err should be ctx.Err() is if Execute itself
	// observed the parent ctx cancellation during execution.

	s.deliverResult(req, val, err) // F11: broadcast real values to joined waiters
	req.finish(Result{Value: val, Err: err})
	req.releaseSharedWaiter()
	s.inFlight.Add(-1)
}

// releaseClass returns the request's §F8 class token and wakes one worker:
// queued same-class requests become runnable the moment a token frees, and
// sleeping workers only ever wait on s.notify.
func (s *Scheduler) releaseClass(c CostClass) {
	<-s.classTokens[resolveClass(c)]
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func executeSafely(req *Request, ctx context.Context) (value any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			value = nil
			err = fmt.Errorf("scheduler: request execution panicked (%T)", recovered)
		}
	}()
	return req.Execute(ctx, req.Snapshot)
}

// pickRequest selects the highest-priority ready request whose §F8 class
// still has capacity (see pickClassAware). It skips closed empty lanes so
// shutdown drains buffered requests at every class and priority before
// workers exit. Idle workers block on the notify signal instead of polling —
// Submit and releaseClass each wake one.
func (s *Scheduler) pickRequest(ctx context.Context) *Request {
	draining := false
	for {
		if req := s.pickClassAware(); req != nil {
			return req
		}
		if draining {
			// Final drain scan under the aging fence: with the fence held, a
			// priority-aging pass can neither hold a request between lanes
			// during this scan nor re-queue one after it. A pass that started
			// before cancellation either completed (its requeues are visible
			// to this scan) or is still pending, in which case the worker
			// yields and rescans instead of exiting on a false empty. Any
			// pass that runs later refuses at its fence entry (cancelled
			// context or closed), so an empty fenced scan is a stable empty.
			s.agingMu.Lock()
			if req := s.pickClassAware(); req != nil {
				s.agingMu.Unlock()
				return req
			}
			pending := s.agingPending.Load()
			s.agingMu.Unlock()
			if pending != 0 {
				runtime.Gosched()
				continue
			}
			return nil
		}
		// Prefer an already-canceled context before waiting. If cancellation
		// races the wait below, either selected case loops through one final
		// queue scan before the worker exits.
		select {
		case <-ctx.Done():
			draining = true
			continue
		default:
		}
		// No requests ready; sleep until a Submit signals or shutdown.
		select {
		case <-ctx.Done():
			draining = true
		case <-s.notify:
		}
	}
}

// pickClassAware scans priorities outer and classes inner, returning the
// highest-priority request whose class holds a free §F8 concurrency token.
// The token is acquired BEFORE the dequeue (both non-blocking), so a request
// is never pulled out of a queue without capacity to run it: the worker
// holding it goes straight to execution and never blocks behind another
// class, which is what keeps P0 interactive work moving while Large lanes
// sit at their cap (F7). At shutdown, closed lanes drain: a closed-empty
// receive reports !ok, the token goes back, and the scan continues.
func (s *Scheduler) pickClassAware() *Request {
	for pq := 0; pq < 9; pq++ {
		for c := CostTiny; c <= CostVeryLarge; c++ {
			select {
			case s.classTokens[c] <- struct{}{}:
				select {
				case req, ok := <-s.queues[c][pq]:
					if ok && req != nil {
						return req // token held for the duration of execution
					}
					<-s.classTokens[c] // lane empty or closed: give the slot back
				default:
					<-s.classTokens[c]
				}
			default:
				// class at cap: skip its lanes entirely
			}
		}
	}
	return nil
}

// agingLoop boosts priorities of long-queued requests to prevent starvation (F7).
func (s *Scheduler) agingLoop(ctx context.Context) {
	// PriorityAgingMs is documented in milliseconds (round20/bugC): read it
	// as time.Duration(x)*time.Millisecond, never bare time.Duration(x) —
	// the latter made the production default 5000 a 5µs threshold, boosting
	// every queued request each tick and flooding the P0 lane with P5 work.
	interval := time.Duration(s.config.PriorityAgingMs) * time.Millisecond / 2
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
			s.boostStaleRequests(ctx)
		}
	}
}

// requeueAged puts an aging-extracted request back into its §F8 class lane:
// first choice, then the lane it came from, then any sibling priority lane
// with room. An aging pass extracts at most one request per lane, so the
// origin lane has room unless a concurrent Submit refilled it; the sibling
// fallback (F3 audit fix) keeps a saturated destination from turning an
// admitted request into a pseudo-rejection. Returns the priority the request
// landed in, or -1 when every lane of the class is full.
func (s *Scheduler) requeueAged(c CostClass, first, origin int, req *Request) int {
	if s.trySend(c, first, req) {
		return first
	}
	if origin != first && s.trySend(c, origin, req) {
		return origin
	}
	for pq := 0; pq < len(s.queues[resolveClass(c)]); pq++ {
		if pq != first && pq != origin && s.trySend(c, pq, req) {
			return pq
		}
	}
	return -1
}

// boostStaleRequests lifts requests that have been queued longer than
// PriorityAgingMs. Aging moves requests between priorities within their own
// §F8 class lane — boosting changes §F3 order, never §F8 cost.
//
// The pass holds the aging fence (agingMu) for its entire duration and is
// registered in agingPending before taking it: Shutdown waits on the fence
// before it cancels and closes the queues, and a draining worker's final
// scan holds it too, so an extracted request can never be in lane limbo
// across a close or a last scan (audit findings 1/2/5 — the panic on a
// drained closed lane — and finding 2 — stranded admitted requests).
func (s *Scheduler) boostStaleRequests(ctx context.Context) {
	s.agingPending.Add(1)
	s.agingMu.Lock()
	defer func() {
		s.agingMu.Unlock()
		s.agingPending.Add(-1)
	}()
	// Refuse to scan once shutdown started or the session context is gone:
	// from that point the drain owns every queued request, and a pass past
	// this point would extract requests from lanes that are closing under it
	// and turn them into pseudo-rejections (F3) or stragglers (F2).
	if s.closed.Load() || ctx.Err() != nil {
		return
	}
	boosted := false
	for pq := PriorityMaintenance; pq > PriorityCompletion; pq-- {
		for c := CostTiny; c <= CostVeryLarge; c++ {
			src := s.queues[c][pq]
			if len(src) == 0 {
				continue
			}
			destPq := pq - 1
			// Drain one stale request and re-queue at higher priority. The
			// receive must be the binary form: a lane closed mid-scan that a
			// draining worker just emptied reports the zero value with
			// ok=false, and receiving that as a bare *Request nil-
			// dereferenced at the EnqueuedAt read below, panicking the
			// agingLoop goroutine and the process with it.
			select {
			case req, ok := <-src:
				if !ok || req == nil {
					continue
				}
				if time.Since(req.EnqueuedAt) <= time.Duration(s.config.PriorityAgingMs)*time.Millisecond {
					// Not stale yet, put back; never drop silently.
					if s.requeueAged(c, int(pq), int(pq), req) < 0 {
						s.rejectDropped(req, "scheduler: priority aging could not return the request; every lane of its class is full")
					}
					continue
				}
				if s.requeueAged(c, int(destPq), int(pq), req) >= 0 {
					boosted = true
				} else {
					// Destination full AND no sibling lane had room: fail
					// fast instead of vanishing (historical double-default
					// silent drop).
					s.rejectDropped(req, "scheduler: priority aging boost failed; every lane of its class is full")
				}
			default:
			}
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
	err := fmt.Errorf("scheduler: %w: %s", errRejected, msg)
	s.totalRejected.Add(1)
	s.deliverRollback(req, err)
	req.releaseSharedWaiter()
	req.finish(Result{Err: err})
}

// Shutdown gracefully stops the scheduler.
func (s *Scheduler) Shutdown() {
	// Aging fence first: a priority-aging pass must never overlap the
	// cancel+close window. A pass holding an extracted request at that
	// moment would either pseudo-reject it (trySend refuses closed lanes) or
	// re-queue it after the last draining worker scan — a stranded admitted
	// request with no terminal result. Taking the same fence the pass holds
	// makes Shutdown wait for any in-flight pass and block later ones; every
	// later pass refuses at its own entry check below.
	s.agingMu.Lock()
	defer s.agingMu.Unlock()

	// Linearize shutdown with queue sends: a sender holding RLock must finish
	// its closed check and send before closed becomes visible or workers are
	// cancelled. Later senders acquire RLock after this and reject on closed.
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	if s.cancel != nil {
		s.cancel()
	}
	for c := range s.queues {
		for i := range s.queues[c] {
			close(s.queues[c][i])
		}
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
	for i := range st.QueueLengths {
		for c := range s.queues {
			st.QueueLengths[i] += len(s.queues[c][i])
		}
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
	for c := range s.queues {
		for i := range s.queues[c] {
			n += len(s.queues[c][i])
		}
	}
	return n
}
