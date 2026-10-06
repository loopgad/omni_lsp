// Package server implements the LSP JSON-RPC server for OmniLSP.
//
// Responsibility:
//
//	Accepts JSON-RPC messages via a transport, enforces the C2 lifecycle gate,
//	classifies priority (F3), admits queries through the scheduler (F5/F6),
//	dispatches to registered handlers, and projects canonical results to
//	protocol responses.
//
// Owned mutable state:
//
//	state (State enum, protected by mu), transport (set at Run), languages map,
//	inflight table, evidence ring (both mu-protected), syncRejects (atomic).
//
// Concurrency model:
//
//	sync.RWMutex for state/transport/inflight/ring; scheduler handles request
//	goroutines; no nested locks between server and scheduler.
//
// Invariants:
//  1. Every LSP request is admitted through the C2 lifecycle gate and the
//     scheduler (F3/F5).
//  2. didOpen/didChange/didSave/didClose update VFS and publish Snapshot (F1).
//  3. Every semantic request serves from exactly one captured Snapshot
//     (INV-SNAPSHOT-002); state transitions are never dropped for staleness
//     (PROT-SYNC-001).
//  4. The VFS is the single source of truth for open-document content.
package server

import (
	"context"
	"encoding/json"
	"errors"
	stderrors "errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/scheduler"
	"github.com/omnilsp/omni/internal/semantic/query"
	"github.com/omnilsp/omni/internal/telemetry"
	"github.com/omnilsp/omni/internal/transport"
	"github.com/omnilsp/omni/internal/trust"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/internal/workspace/vfs"
	"github.com/omnilsp/omni/internal/workspace/virtual"
	"github.com/omnilsp/omni/internal/workspace/watch"
)

// State represents the server lifecycle state (goal.md §C2).
type State int

const (
	StateUninitialized State = iota
	StateInitializing
	StateRunning
	StateShuttingDown
	StateExited
)

func (st State) String() string {
	switch st {
	case StateUninitialized:
		return "uninitialized"
	case StateInitializing:
		return "initializing"
	case StateRunning:
		return "running"
	case StateShuttingDown:
		return "shutting_down"
	case StateExited:
		return "exited"
	default:
		return "unknown"
	}
}

// Server is the main LSP server.
type Server struct {
	mu sync.RWMutex
	// mutationMu serializes client state transitions and external invalidation.
	mutationMu       sync.Mutex
	backendCloseOnce sync.Once
	backendCloseErr  error
	sourceSyncStates []*sourceSyncState
	shutdownErr      error         // protected by mu; remains fatal after a later exit
	workspacePoller  *watch.Poller // protected by mu; owns the current scan baseline
	state            State
	transport        transport.Transport
	dispatcher       *jsonrpc.Dispatcher
	scheduler        *scheduler.Scheduler
	snapMgr          *snapshot.Manager
	vfs              *vfs.VFS
	virtualReg       *virtual.Registry            // §D12 source-map registry for virtual documents
	languages        map[string]languages.Backend // keyed by LanguageID
	semanticIndexes  map[string]semanticIndexBinding
	overlayFacts     *goSnapshotSemanticFactsCache
	queries          *query.Engine    // §J memo engine for semantic read paths
	diag             *diagCoordinator // §C11/§I17 push/pull diagnostics
	positionEncoding string           // §C4 negotiated: utf-8|utf-16|utf-32
	config           Config
	workspaceID      identity.WorkspaceID
	idx              *indexService
	idxReason        string
	trust            *trust.Policy

	// syncRejects counts rejected didChange notifications (invalid range or
	// version) for observability (D6: rejection must be visible, not silent).
	syncRejects atomic.Int64

	// Rename freshness counters delimit requests that entered backend analysis
	// and were rejected because the workspace advanced before the edit was
	// returned. They make stale-result safety measurable without changing the
	// LSP rename response contract.
	renameRequestsStarted atomic.Uint64
	renameStaleRejected   atomic.Uint64

	// inflight maps JSON-RPC request IDs to scheduled requests for
	// $/cancelRequest lookup (C7). Protected by mu.
	inflight map[string]*scheduler.Request
	// completionPhaseSpans links opt-in trace samples across scheduler and
	// response-write stages. Protected by mu; nil when tracing is disabled.
	completionPhaseSpans map[string]*completionPhaseTraceSpan

	// evRing is a bounded ring of recent §B4 evidence records for the
	// omnilsp/explain API (§I25). Protected by mu.
	evRing     [64]evidenceRecord
	evRingNext int

	// metrics are the §P2 telemetry counters (§P3: low-cardinality labels only).
	metrics serverMetrics

	completionPhaseTrace *completionPhaseTraceRecorder
	// semanticResponseObserver is an optional session-recorder hook. It receives
	// request-local evidence before the matching response is written.
	semanticResponseObserver func(jsonrpc.RequestID, []identity.Evidence)

	// clientWantsDocumentChanges records the §C9 negotiation: the client
	// declared workspace.workspaceEdit.documentChanges at initialize. Set
	// once during handleInitialize, read-only afterwards.
	clientWantsDocumentChanges atomic.Bool
}

// wantsDocumentChanges reports whether the client negotiated the version-aware
// documentChanges WorkspaceEdit form (§C9).
func (s *Server) wantsDocumentChanges() bool { return s.clientWantsDocumentChanges.Load() }

// parseClientEditCapability inspects raw initialize capabilities for
// workspace.workspaceEdit.documentChanges == true.
func parseClientEditCapability(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var caps struct {
		Workspace struct {
			WorkspaceEdit struct {
				DocumentChanges bool `json:"documentChanges"`
			} `json:"workspaceEdit"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &caps); err != nil {
		return false // malformed capability payload: legacy form, fail safe
	}
	return caps.Workspace.WorkspaceEdit.DocumentChanges
}

// serverMetrics aggregates the canonical counters (§P2/§P3): every rejection,
// backend failure, and snapshot epoch must be visible, never silent.
type serverMetrics struct {
	RequestsTotal  *telemetry.Counter // omnilsp.requests.total
	RejectsTotal   *telemetry.Counter // admission/lifecycle rejections
	SyncRejects    *telemetry.Counter // didChange content rejections
	ExternalSyncs  *telemetry.Counter // workspace/didChange* external notifications
	BackendFails   *telemetry.Counter // semantic dispatch errors by language
	SnapshotEpochs *telemetry.Counter // publishes (D9)
}

func newServerMetrics() serverMetrics {
	return serverMetrics{
		RequestsTotal:  telemetry.NewCounter("omnilsp.requests.total"),
		RejectsTotal:   telemetry.NewCounter("omnilsp.rejects.total"),
		SyncRejects:    telemetry.NewCounter("omnilsp.sync.rejects"),
		ExternalSyncs:  telemetry.NewCounter("omnilsp.sync.external"),
		BackendFails:   telemetry.NewCounter("omnilsp.backend.failures"),
		SnapshotEpochs: telemetry.NewCounter("omnilsp.snapshots.epochs"),
	}
}

// evidenceRecord is one entry of the explain evidence ring.
type evidenceRecord struct {
	Method       string
	URI          string
	Ev           []identity.Evidence
	Diag         []string
	At           time.Time
	Status       identity.ResultStatus
	Completeness identity.Completeness
}

// Config holds server configuration.
type Config struct {
	Scheduler            scheduler.Config
	IndexDir             string
	IndexDiskBudgetBytes int64
	// WatchInterval > 0 enables the §D14 workspace poller (external file
	// changes are rescanned on this cadence). Zero disables it: editors that
	// drive didChangeWatchedFiles themselves need no second opinion.
	WatchInterval time.Duration
}

// DefaultConfig returns default server configuration.
func DefaultConfig() Config {
	return Config{Scheduler: scheduler.DefaultConfig(), IndexDiskBudgetBytes: 256 << 20}
}

// New creates a new LSP server.
func New(cfg Config) *Server {
	s := &Server{
		state:                StateUninitialized,
		config:               cfg,
		dispatcher:           jsonrpc.NewDispatcher(),
		snapMgr:              snapshot.NewManager(),
		vfs:                  vfs.New(),
		virtualReg:           virtual.New(),
		languages:            make(map[string]languages.Backend),
		semanticIndexes:      make(map[string]semanticIndexBinding),
		overlayFacts:         newGoSnapshotSemanticFactsCache(),
		inflight:             make(map[string]*scheduler.Request),
		metrics:              newServerMetrics(),
		queries:              query.NewEngine(0),
		completionPhaseTrace: newCompletionPhaseTraceFromEnv(),
	}
	s.diag = newDiagCoordinator(s)
	s.positionEncoding = "utf-16" // LSP baseline default until negotiated
	s.scheduler = scheduler.New(cfg.Scheduler)
	s.registerHandlers()
	return s
}

// RegisterBackend registers a language backend for the given language ID.
// VFS exposes the virtual filesystem for tooling and tests.
func (s *Server) VFS() *vfs.VFS { return s.vfs }

// Dispatcher exposes the JSON-RPC dispatcher for tooling and tests.
func (s *Server) Dispatcher() *jsonrpc.Dispatcher { return s.dispatcher }

type diagnosticUpdateSource interface {
	SetDiagnosticsUpdateHandler(func(uri string))
}

func (s *Server) RegisterBackend(langID string, backend languages.Backend) {
	s.mu.Lock()
	s.languages[langID] = backend
	if provider, ok := backend.(languages.SemanticIndexProvider); ok {
		if planner, ok := backend.(languages.SemanticIndexRequestBuilder); ok {
			// Backend aliases (for example "c" for a canonical "cpp" backend)
			// must not create a second semantic job with a different language key.
			// Request builders derive scope language from their canonical backend
			// identity, so bind the capability under that same identity.
			semanticLanguage := backend.LanguageID()
			if semanticLanguage == "" {
				semanticLanguage = langID
			}
			s.semanticIndexes[semanticLanguage] = semanticIndexBinding{provider: provider, planner: planner}
		}
	}
	s.mu.Unlock()
	if source, ok := backend.(diagnosticUpdateSource); ok {
		source.SetDiagnosticsUpdateHandler(s.diag.diagnosticsUpdated)
	}
}

// RegisterSemanticIndexProvider binds an optional external semantic exporter
// and request planner to a language key. It is used by language stacks where
// the exporter is separate from the frozen Backend implementation.
func (s *Server) RegisterSemanticIndexProvider(langID string, provider languages.SemanticIndexProvider, planner languages.SemanticIndexRequestBuilder) {
	if langID == "" || provider == nil || planner == nil {
		return
	}
	s.mu.Lock()
	s.semanticIndexes[langID] = semanticIndexBinding{provider: provider, planner: planner}
	s.mu.Unlock()
}

// Run runs the server with the given transport. Blocks until shutdown.
// All incoming messages are admitted through the lifecycle gate (C2) and the
// scheduler (F3/F5). The Running state is entered only via the initialize
// handshake, never implicitly.
func (s *Server) Run(ctx context.Context, t transport.Transport) (runErr error) {
	s.mu.Lock()
	s.transport = t
	s.mu.Unlock()

	s.scheduler.Start(ctx)
	var watcher *watch.Poller
	defer func() {
		if watcher != nil {
			watcher.CloseAndWait()
		}
		s.scheduler.Shutdown()
		runErr = stderrors.Join(runErr, s.closeBackends())
		s.mu.RLock()
		runErr = stderrors.Join(runErr, s.shutdownErr)
		s.mu.RUnlock()
		s.diag.Close()
		if err := s.completionPhaseTrace.close(); err != nil {
			runErr = stderrors.Join(runErr, err)
		}
	}()

	for {
		// C2/Q3: transport termination must surface its cause. Done alone is
		// not consulted here — Read reports either the stream error (e.g.
		// truncated frame, §S13) or a clean EOF; skipping straight to nil
		// would swallow the difference.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, err := t.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// LSP exit closes the transport deliberately. In that one state,
			// Read may return io.ErrClosedPipe instead of EOF; this is the
			// successful completion of the lifecycle handshake, not a transport
			// fault. Unexpected read failures still surface below.
			if s.State() == StateExited {
				return s.drainInflight(2 * time.Second)
			}
			// Clean EOF is a normal stream end (peer closed after finishing,
			// replay drain); any other error is a transport fault that must
			// surface — a truncated frame must never masquerade as done (S13).
			if stderrors.Is(err, io.EOF) {
				return s.drainInflight(2 * time.Second)
			}
			return stderrors.Join(err, s.drainInflight(2*time.Second))
		}

		if msg == nil {
			continue
		}

		if msg.IsResponse() {
			continue
		}

		// Route through scheduler for admission control and prioritization.
		s.scheduleMessage(ctx, msg)
		// The ordinary LSP root is only available after initialize. The
		// callback shares the document writer, including while Read blocks.
		if watcher == nil && s.config.WatchInterval > 0 && s.State() == StateRunning {
			if root := s.workspaceRoot(); root != "" {
				watcher = watch.New(root, s.config.WatchInterval, func(events []watch.Event) {
					if len(events) == 0 {
						return
					}
					s.mutationMu.Lock()
					defer s.mutationMu.Unlock()
					if s.State() != StateRunning {
						return
					}
					changes := make([]languages.SourceChange, 0, len(events))
					for _, event := range events {
						changes = append(changes, languages.SourceChange{
							URI: workspaceuri.FromPath(event.Path).Canonical(), Kind: languages.SourceChangeKind(event.Kind + 1),
						})
					}
					if err := s.applyExternalSourceChanges(ctx, changes); err != nil {
						s.recordEvidence(ctx, "workspace/poll", "", identity.ResultUnavailable, identity.CompletenessUnknown, nil, []string{err.Error()})
					}
				})
				if idx, _, _ := s.indexState(); idx != nil && idx.dir != "" {
					if err := watcher.SetExcludedRoots(idx.dir); err != nil {
						watcher.CloseAndWait()
						return fmt.Errorf("configure workspace watcher index exclusion: %w", err)
					}
				}
				watcher.SetErrorHandler(func(err error) {
					s.mutationMu.Lock()
					defer s.mutationMu.Unlock()
					if s.State() != StateRunning {
						return
					}
					if err != nil {
						s.recordEvidence(context.Background(), "workspace/poll", "", identity.ResultUnavailable, identity.CompletenessUnknown, nil, []string{err.Error()})
					} else {
						s.recordEvidence(context.Background(), "workspace/poll", "", identity.ResultExact, identity.Complete, nil, []string{"complete scan recovered"})
					}
				})
				s.mu.Lock()
				s.workspacePoller = watcher
				s.mu.Unlock()
				watcher.Start()
			}
		}
	}
}

// closeBackends reaps each unique child service once. Shutdown calls it before
// acknowledging the request so exit does not race the client's process cleanup.
func (s *Server) closeBackends() error {
	s.backendCloseOnce.Do(func() {
		s.diag.Close()
		backends := s.workspaceBackends()
		errorsByBackend := make([]error, len(backends))
		var workers sync.WaitGroup
		limit := make(chan struct{}, 8)
		for i, be := range backends {
			workers.Add(1)
			go func(i int, be languages.Backend) {
				defer workers.Done()
				limit <- struct{}{}
				defer func() { <-limit }()
				if err := be.Close(); err != nil {
					errorsByBackend[i] = fmt.Errorf("close %s backend: %w", be.LanguageID(), err)
				}
			}(i, be)
		}
		workers.Wait()
		s.backendCloseErr = stderrors.Join(errorsByBackend...)
	})
	return s.backendCloseErr
}

// inlineMethods are handled directly on the read-loop goroutine instead of
// the scheduler pool (F1 single-writer). State transitions must be applied
// strictly in arrival order and never dropped: routing them through the
// worker pool let concurrent workers apply didChanges out of order, and the
// VFS version-regression guard silently dropped the losers (PROT-SYNC-001).
// They are also cheap (string splices, state flips), so inlining costs the
// loop nothing while making cancel/lifecycle handling immediate (A3).
var inlineMethods = map[string]bool{
	"initialize":                      true,
	"initialized":                     true,
	"shutdown":                        true,
	"exit":                            true,
	"$/cancelRequest":                 true,
	"textDocument/didOpen":            true,
	"textDocument/didChange":          true,
	"textDocument/didSave":            true,
	"textDocument/didClose":           true,
	"workspace/didChangeWatchedFiles": true,
	"workspace/didCreateFiles":        true,
	"workspace/didRenameFiles":        true,
	"workspace/didDeleteFiles":        true,
}

// scheduleMessage admits the message: lifecycle/state-transition methods run
// inline on the caller goroutine (the read loop — a natural single writer);
// everything else goes through the scheduler for admission control and
// prioritization (F3/F5). The scheduler is therefore a query engine only.
func (s *Server) scheduleMessage(parentCtx context.Context, msg *jsonrpc.Message) {
	s.metrics.RequestsTotal.Inc(1)
	if inlineMethods[msg.Method] {
		s.dispatchInline(msg)
		return
	}

	if !s.lifecycleAllows(msg.Method) {
		s.respondLifecycleError(msg)
		return
	}
	priority := s.classifyPriority(msg)
	phaseSpan := s.beginScheduledCompletionTrace(msg)

	// Track in-flight requests for $/cancelRequest mapping (C7).
	var reqID string
	if msg.ID != nil {
		reqID = requestIDKey(*msg.ID)
	}

	snap := s.snapMgr.Current()
	// Preserve the client-visible wire ID: error responses must echo it, not
	// the internal RequestID string (which is not a JSON-RPC id).
	var origID *scheduler.ClientID
	if msg.ID != nil {
		origID = &scheduler.ClientID{Str: msg.ID.Str, Num: msg.ID.Num, IsStr: msg.ID.IsStr, IsNull: msg.ID.IsNull}
	}
	enqueuedAt := time.Now()
	req := &scheduler.Request{
		RequestID:   identity.RequestID(fmt.Sprintf("%s-%v", msg.Method, msg.ID)),
		OriginalID:  origID,
		Workspace:   s.workspaceID,
		Priority:    priority,
		Snapshot:    snap,
		CoalesceKey: coalesceKeyFor(msg, snap),
		EnqueuedAt:  enqueuedAt,
		Execute: func(ctx context.Context, snap *snapshot.Snapshot) (any, error) {
			// INV-SNAPSHOT-002: the request serves from exactly this captured
			// snapshot. D11 staleness is NOT checked here — a queued request
			// behind a newer edit is still valid (C6 offers ContentModified
			// only where a mutating result must be fresh; see handleRename).
			// Gating state transitions here would drop accepted didChange
			// edits and violate PROT-SYNC-001.
			if phaseSpan != nil {
				phaseSpan.sample.SchedulerQueueWaitNS = elapsedNanoseconds(enqueuedAt)
				ctx = withCompletionPhaseSpan(ctx, phaseSpan)
			}
			dispatchStarted := time.Time{}
			if phaseSpan != nil {
				dispatchStarted = time.Now()
			}
			resp := s.dispatcher.Dispatch(withSnapshot(ctx, snap), msg)
			if phaseSpan != nil {
				phaseSpan.sample.DispatcherCallNS = elapsedNanoseconds(dispatchStarted)
				phaseSpan.dispatcherReturnedAt = time.Now()
			}
			return resp, nil
		},
	}

	if reqID != "" {
		s.mu.Lock()
		s.inflight[reqID] = req
		if phaseSpan != nil {
			if s.completionPhaseSpans == nil {
				s.completionPhaseSpans = make(map[string]*completionPhaseTraceSpan)
			}
			s.completionPhaseSpans[reqID] = phaseSpan
		}
		s.mu.Unlock()
	}

	submitStarted := time.Time{}
	if phaseSpan != nil {
		phaseSpan.sample.SchedulePrepareNS = elapsedNanoseconds(phaseSpan.serverStartedAt)
		submitStarted = time.Now()
	}
	result := s.scheduler.Submit(req)
	if phaseSpan != nil {
		phaseSpan.sample.SchedulerSubmitNS = elapsedNanoseconds(submitStarted)
	}
	switch result {
	case scheduler.Admitted:
		// Read result in background goroutine to avoid blocking the main loop.
		go s.drainResult(req, reqID)
	case scheduler.RejectedQueueFull:
		s.unregisterInflight(reqID)
		s.respondRejected(msg, "scheduler queue full")
		s.finishQueuedCompletionTrace(reqID, "rejected_queue_full")
	case scheduler.RejectedCancelled:
		s.unregisterInflight(reqID)
		s.respondRejected(msg, "scheduler cancelled")
		s.finishQueuedCompletionTrace(reqID, "rejected_cancelled")
	default:
		s.unregisterInflight(reqID)
		s.respondRejected(msg, "scheduler rejected")
		s.finishQueuedCompletionTrace(reqID, "rejected")
	}
}

func (s *Server) beginScheduledCompletionTrace(msg *jsonrpc.Message) *completionPhaseTraceSpan {
	if s.completionPhaseTrace == nil || msg == nil || msg.ID == nil || msg.Method != "textDocument/completion" {
		return nil
	}
	var params CompletionParams
	if err := json.Unmarshal(msg.Params, &params); err != nil || params.TextDocument.URI == "" {
		return nil
	}
	span := s.completionPhaseTrace.begin("", params.TextDocument.URI, rawJSONRPCRequestID(msg.ID))
	if span != nil {
		span.serverStartedAt = time.Now()
		span.serverManaged = true
	}
	return span
}

func (s *Server) takeCompletionPhaseTrace(reqID string) *completionPhaseTraceSpan {
	if reqID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	span := s.completionPhaseSpans[reqID]
	delete(s.completionPhaseSpans, reqID)
	return span
}

func (s *Server) finishQueuedCompletionTrace(reqID, outcome string) {
	span := s.takeCompletionPhaseTrace(reqID)
	if span == nil {
		return
	}
	span.sample.ResponseOutcome = outcome
	span.sample.ServerRoundTripNS = elapsedNanoseconds(span.serverStartedAt)
	span.finish()
}

func (s *Server) sendCompletionPhaseResponse(span *completionPhaseTraceSpan, msg *jsonrpc.Message) {
	if span == nil {
		if msg != nil {
			_ = s.send(msg)
		}
		return
	}
	if msg == nil {
		span.sample.ResponseOutcome = "missing_response"
		span.sample.ServerRoundTripNS = elapsedNanoseconds(span.serverStartedAt)
		span.finish()
		return
	}
	writeStarted := time.Now()
	err := s.send(msg)
	span.sample.ResponseWriteNS = elapsedNanoseconds(writeStarted)
	span.sample.ServerRoundTripNS = elapsedNanoseconds(span.serverStartedAt)
	switch {
	case err != nil:
		span.sample.ResponseOutcome = "write_failed"
	case msg.Error != nil:
		span.sample.ResponseOutcome = "error_response"
	default:
		span.sample.ResponseOutcome = "success"
	}
	span.finish()
}

// dispatchInline executes a state-transition or lifecycle message directly
// on the read-loop goroutine, preserving strict arrival order (F1). The C2
// lifecycle gate still applies; responses are sent synchronously.
func (s *Server) dispatchInline(msg *jsonrpc.Message) {
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()
	if !s.lifecycleAllows(msg.Method) {
		s.respondLifecycleError(msg)
		return
	}
	resp := s.dispatcher.Dispatch(context.Background(), msg)
	if resp != nil {
		_ = s.send(resp)
	}
}

// coalescableMethods are read-only, idempotent queries whose result depends
// solely on (method, document position, snapshot revision) — safe to share
// one execution between concurrent identical requests (F11/J6). Mutating or
// side-effecting methods are deliberately excluded.
var coalescableMethods = map[string]bool{
	"textDocument/hover":      true,
	"textDocument/definition": true,
	"textDocument/references": true,
}

// coalesceParams is the minimal params projection needed for the key.
type coalesceParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"position"`
}

// coalesceKeyFor builds the F11 single-flight key for a coalescable query,
// or "" when the request must never join. Unparseable params yield "" (the
// request still executes; it just cannot share work).
func coalesceKeyFor(msg *jsonrpc.Message, snap *snapshot.Snapshot) string {
	if !coalescableMethods[msg.Method] || snap == nil || len(msg.Params) == 0 {
		return ""
	}
	var p coalesceParams
	if err := json.Unmarshal(msg.Params, &p); err != nil || p.TextDocument.URI == "" {
		return ""
	}
	return fmt.Sprintf("%s|%s|%d|%d|%d",
		msg.Method, p.TextDocument.URI, snap.Revision(), p.Position.Line, p.Position.Character)
}

// lifecycleAllows enforces the C2 lifecycle state machine at admission.
// Before initialize completes only initialization-safe messages are accepted;
// after shutdown only exit; after exit nothing.
func (s *Server) lifecycleAllows(method string) bool {
	s.mu.RLock()
	st := s.state
	s.mu.RUnlock()
	switch st {
	case StateUninitialized, StateInitializing:
		switch method {
		case "initialize", "initialized", "exit":
			return true
		}
		return false
	case StateRunning:
		return true
	case StateShuttingDown:
		return method == "exit"
	default: // StateExited and unknown states accept nothing.
		return false
	}
}

// respondLifecycleError rejects a request that violates lifecycle ordering.
// Notifications are silently dropped per LSP semantics — but counted (§K).
func (s *Server) respondLifecycleError(msg *jsonrpc.Message) {
	s.metrics.RejectsTotal.Inc(1)
	if !msg.IsRequest() || msg.ID == nil {
		return
	}
	resp := jsonrpc.NewErrorResponse(*msg.ID, jsonrpc.InvalidRequest,
		fmt.Sprintf("method %q not allowed in state %s", msg.Method, s.State()), nil)
	_ = s.send(resp)
}

// drainResult reads the scheduled request result and sends the response.
// reqID is the JSON-RPC request ID key used for $/cancelRequest mapping (C7).
// drainInflight waits (bounded) for scheduled requests whose responses are
// still being written back by drainResult goroutines. Without this, a client
// that closes the stream right after its last request can race the response
// write: the session ends before "result" hits the wire (observed as a replay
// divergence under load). A timeout is an incomplete lifecycle, not success.
func (s *Server) drainInflight(maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		n := len(s.inflight)
		s.mu.RUnlock()
		if n == 0 {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.RLock()
	n := len(s.inflight)
	s.mu.RUnlock()
	if n == 0 {
		return nil
	}
	return fmt.Errorf("shutdown: %d requests remain after %s", n, maxWait)
}

func (s *Server) drainResult(req *scheduler.Request, reqID string) {
	defer s.unregisterInflight(reqID)
	phaseSpan := s.takeCompletionPhaseTrace(reqID)
	phaseTraceFinished := false
	// F16: a panicking result path must not kill the process; the request
	// simply never gets a protocol response (logged below).
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "omnilsp: drainResult panic for %s: %v\n", reqID, r)
		}
		if phaseSpan != nil && !phaseTraceFinished {
			phaseSpan.sample.ResponseOutcome = "drain_result_panic"
			phaseSpan.sample.ServerRoundTripNS = elapsedNanoseconds(phaseSpan.serverStartedAt)
			phaseSpan.finish()
		}
	}()
	res := <-req.Result
	if phaseSpan != nil && !phaseSpan.dispatcherReturnedAt.IsZero() {
		phaseSpan.sample.SchedulerResultDeliveryNS = elapsedNanoseconds(phaseSpan.dispatcherReturnedAt)
	}
	if res.Err != nil {
		// Echo the client's wire ID when we captured it; fall back to the
		// internal RequestID string only for requests that had none.
		respID := parseRequestID(req.RequestID)
		if req.OriginalID != nil {
			cid := *req.OriginalID
			respID = jsonrpc.RequestID{Str: cid.Str, Num: cid.Num, IsStr: cid.IsStr, IsNull: cid.IsNull}
		}
		if ierrors.IsKind(res.Err, ierrors.ErrCancelled) || ctxErr(res.Err) {
			// LSP cancellation is advisory: the original request still needs a
			// terminal response. RequestCancelled tells the client that work was
			// stopped without leaving its request ID pending forever.
			msg := jsonrpc.NewErrorResponse(respID, jsonrpc.RequestCancelled, "request cancelled", nil)
			s.sendCompletionPhaseResponse(phaseSpan, msg)
			phaseTraceFinished = phaseSpan != nil
			return
		}
		errText := projectBackendError(s, res.Err)
		msg := jsonrpc.NewErrorResponse(
			respID,
			jsonrpc.InternalError,
			errText,
			nil,
		)
		s.sendCompletionPhaseResponse(phaseSpan, msg)
		phaseTraceFinished = phaseSpan != nil
		return
	}
	// res.Value is *jsonrpc.Message from dispatcher.Dispatch
	if respMsg, ok := res.Value.(*jsonrpc.Message); ok && respMsg != nil {
		s.sendCompletionPhaseResponse(phaseSpan, respMsg)
	} else {
		s.sendCompletionPhaseResponse(phaseSpan, nil)
	}
	phaseTraceFinished = phaseSpan != nil
}

func ctxErr(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func (s *Server) unregisterInflight(reqID string) {
	if reqID == "" {
		return
	}
	s.mu.Lock()
	delete(s.inflight, reqID)
	s.mu.Unlock()
}

// cancelRequest implements the server side of $/cancelRequest (C7): it looks
// up the in-flight scheduled request and cancels it. Unknown IDs are ignored.
func (s *Server) cancelRequest(id jsonrpc.RequestID) {
	key := requestIDKey(id)
	s.mu.RLock()
	req := s.inflight[key]
	s.mu.RUnlock()
	if req != nil {
		req.Cancel()
	}
}

// requestIDKey builds a map key from a JSON-RPC request ID.
func requestIDKey(id jsonrpc.RequestID) string {
	if id.IsStr {
		return "s:" + id.Str
	}
	return fmt.Sprintf("n:%d", id.Num)
}

// snapshotCtxKey carries the request's captured snapshot through the handler
// chain (INV-SNAPSHOT-002: exactly one snapshot per request).
type snapshotCtxKey struct{}

type evidenceStageCtxKey struct{}

type evidenceStage struct {
	mu      sync.Mutex
	records []evidenceRecord
}

func withEvidenceStage(ctx context.Context, stage *evidenceStage) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, evidenceStageCtxKey{}, stage)
}

func (s *evidenceStage) append(record evidenceRecord) {
	s.mu.Lock()
	s.records = append(s.records, record)
	s.mu.Unlock()
}

func (s *evidenceStage) evidence() []identity.Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []identity.Evidence
	for _, record := range s.records {
		out = append(out, record.Ev...)
	}
	return out
}

// SetSemanticResponseObserver installs a recording hook. The hook must not
// mutate the provided evidence slice and must be safe for concurrent calls.
func (s *Server) SetSemanticResponseObserver(observer func(jsonrpc.RequestID, []identity.Evidence)) {
	s.mu.Lock()
	s.semanticResponseObserver = observer
	s.mu.Unlock()
}

func (s *Server) observeSemanticResponse(id jsonrpc.RequestID, evidence []identity.Evidence) {
	s.mu.RLock()
	observer := s.semanticResponseObserver
	s.mu.RUnlock()
	if observer != nil {
		observer(id, evidence)
	}
}

func (s *evidenceStage) commit(server *Server) {
	s.mu.Lock()
	records := append([]evidenceRecord(nil), s.records...)
	s.records = nil
	s.mu.Unlock()
	for _, record := range records {
		server.appendEvidence(record)
	}
}

func withSnapshot(ctx context.Context, snap *snapshot.Snapshot) context.Context {
	return context.WithValue(ctx, snapshotCtxKey{}, snap)
}

// snapshotFromCtx returns the captured snapshot, or nil when a handler runs
// outside the scheduler path (direct Dispatch in tests).
func snapshotFromCtx(ctx context.Context) *snapshot.Snapshot {
	if v, ok := ctx.Value(snapshotCtxKey{}).(*snapshot.Snapshot); ok {
		return v
	}
	return nil
}

// recordEvidence appends one §B4 evidence record to the explain ring.
// Callers: envelope-unwrapping semantic handlers.
func (s *Server) recordEvidence(ctx context.Context, method, uri string, status identity.ResultStatus, completeness identity.Completeness, ev []identity.Evidence, diag []string) {
	record := evidenceRecord{
		Method: method, URI: canonicalDocumentURI(uri), Ev: ev, Diag: diag, At: time.Now().UTC(),
		Status: status, Completeness: completeness,
	}
	if stage, ok := ctx.Value(evidenceStageCtxKey{}).(*evidenceStage); ok && stage != nil {
		stage.append(record)
		return
	}
	s.appendEvidence(record)
}

func (s *Server) appendEvidence(record evidenceRecord) {
	s.mu.Lock()
	s.evRing[s.evRingNext] = record
	s.evRingNext = (s.evRingNext + 1) % len(s.evRing)
	s.mu.Unlock()
}

// recentEvidence returns all live ring entries oldest-first.
func (s *Server) recentEvidence() []evidenceRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]evidenceRecord, 0, len(s.evRing))
	for i := 0; i < len(s.evRing); i++ {
		idx := (s.evRingNext + i) % len(s.evRing)
		r := s.evRing[idx]
		if r.At.IsZero() {
			continue
		}
		out = append(out, r)
	}
	return out
}

// classifyPriority maps LSP methods to scheduler priority classes (F3).
func (s *Server) classifyPriority(msg *jsonrpc.Message) scheduler.Priority {
	switch msg.Method {
	case "textDocument/completion", "textDocument/signatureHelp":
		return scheduler.PriorityCompletion
	case "textDocument/hover":
		return scheduler.PriorityHover
	case "textDocument/definition", "textDocument/declaration",
		"textDocument/typeDefinition", "textDocument/implementation":
		return scheduler.PriorityDefinition
	case "textDocument/references", "textDocument/rename":
		return scheduler.PriorityReferences
	case "textDocument/diagnostics", "textDocument/codeAction":
		return scheduler.PriorityDiagnostics
	case "textDocument/semanticTokens/full", "textDocument/semanticTokens/range",
		"textDocument/inlayHint":
		return scheduler.PrioritySemanticTokens
	case "$/progress":
		return scheduler.PriorityMaintenance
	default:
		return scheduler.PriorityMaintenance
	}
}

func (s *Server) send(msg *jsonrpc.Message) error {
	s.mu.RLock()
	t := s.transport
	s.mu.RUnlock()
	if t == nil {
		return fmt.Errorf("no transport")
	}
	return t.Write(context.Background(), msg)
}

func (s *Server) respondRejected(msg *jsonrpc.Message, reason string) {
	s.metrics.RejectsTotal.Inc(1)
	if !msg.IsRequest() || msg.ID == nil {
		return
	}
	resp := jsonrpc.NewErrorResponse(*msg.ID, jsonrpc.RequestFailed, reason, nil)
	_ = s.send(resp)
}

func parseRequestID(id identity.RequestID) jsonrpc.RequestID {
	return jsonrpc.RequestID{Str: string(id), IsStr: len(id) > 0}
}

// registerHandlers registers all LSP method handlers.
func (s *Server) registerHandlers() {
	s.dispatcher.Register("initialize", s.handleInitialize)
	s.dispatcher.Register("initialized", s.handleInitialized)
	s.dispatcher.Register("shutdown", s.handleShutdown)
	s.dispatcher.Register("exit", s.handleExit)
	s.dispatcher.Register("$/cancelRequest", s.handleCancelRequest)
	s.dispatcher.Register("textDocument/didOpen", s.handleDidOpen)
	s.dispatcher.Register("textDocument/didChange", s.handleDidChange)
	s.dispatcher.Register("textDocument/didSave", s.handleDidSave)
	s.dispatcher.Register("textDocument/didClose", s.handleDidClose)
	s.dispatcher.Register("textDocument/hover", s.handleHover)
	s.dispatcher.Register("textDocument/completion", s.handleCompletion)
	s.dispatcher.Register("textDocument/definition", s.handleDefinition)
	s.dispatcher.Register("textDocument/declaration", s.handleDeclaration)
	s.dispatcher.Register("textDocument/documentSymbol", s.handleDocumentSymbol)
	s.dispatcher.Register("textDocument/references", s.handleReferences)
	s.dispatcher.Register("textDocument/rename", s.handleRename)
	s.dispatcher.Register("textDocument/semanticTokens/full", s.handleSemanticTokens)
	s.dispatcher.Register("textDocument/prepareRename", s.handlePrepareRename)
	s.dispatcher.Register("textDocument/diagnostic", s.handlePullDiagnostics)
	s.dispatcher.Register("workspace/didChangeWatchedFiles", s.handleDidChangeWatchedFiles)
	s.dispatcher.Register("workspace/didCreateFiles", s.handleDidCreateFiles)
	s.dispatcher.Register("workspace/didRenameFiles", s.handleDidRenameFiles)
	s.dispatcher.Register("workspace/didDeleteFiles", s.handleDidDeleteFiles)
	s.dispatcher.Register("textDocument/codeAction", s.handleCodeAction)
	s.dispatcher.Register("textDocument/signatureHelp", s.handleSignatureHelp)
	s.dispatcher.Register("textDocument/formatting", s.handleFormatting)
	s.dispatcher.Register("textDocument/inlayHint", s.handleInlayHints)
	s.dispatcher.Register("workspace/symbol", s.handleWorkspaceSymbol)
	// Custom extension namespace per goal.md §C12 (omnilsp/* only).
	s.dispatcher.Register("omnilsp/status", s.handleOmnilspStatus)
	s.dispatcher.Register("omnilsp/explain", s.handleOmnilspExplain)
	s.dispatcher.Register("omnilsp/backendStatus", s.handleOmnilspBackendStatus)
	s.dispatcher.Register("omnilsp/resultMeta", s.handleOmnilspResultMeta)
	s.dispatcher.Register("omnilsp/queryTrace", s.handleOmnilspQueryTrace)
	s.dispatcher.Register("omnilsp/indexStats", s.handleIndexStats)
	s.dispatcher.Register("omnilsp/reindex", s.handleReindex)
}

// State returns the current server state.
func (s *Server) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

// facadeMutatingMethods are the VFS-mutating notifications registered in the
// dispatcher. The CallMethod facade admits them only in the same state the C2
// lifecycle gate would admit them over LSP — otherwise an MCP/HTTP adapter
// could mutate the VFS from any goroutine, bypassing lifecycle ordering.
var facadeMutatingMethods = map[string]bool{
	"textDocument/didOpen":   true,
	"textDocument/didChange": true,
	"textDocument/didSave":   true,
	"textDocument/didClose":  true,
}

// CallMethod is the canonical read-only facade for non-LSP protocol adapters
// (MCP tools, HTTP API). It routes through the same dispatcher — lifecycle
// gate, snapshot capture, backend resolution, evidence recording — so a
// query over MCP/HTTP behaves exactly like the same query over LSP
// (§C0: semantic behavior MUST NOT differ by transport; §C14: no snapshot
// bypass). Mutating methods are gated to the Ready (Running) state with C2-gate
// semantics; anything unregistered returns a typed method-not-found error.
func (s *Server) CallMethod(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if facadeMutatingMethods[method] && s.State() != StateRunning {
		return nil, fmt.Errorf("method %q not allowed in state %s", method, s.State())
	}
	rawParams, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "facade", IsStr: true}, method, rawParams)
	resp := s.dispatcher.Dispatch(ctx, msg)
	if resp.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
	}
	return resp.Result, nil
}

// HoverEnvelope returns the raw canonical hover envelope (§B5) for adapters
// that project evidence themselves (C14). Same routing as textDocument/hover.
func (s *Server) HoverEnvelope(ctx context.Context, uri string, line, col uint32) (identity.SemanticResult[*languages.HoverResult], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[*languages.HoverResult](), err
	}
	uri = canonicalDocumentURI(uri)
	be, err := s.resolveBackend(uri)
	if err != nil {
		return unavailableEnvelope[*languages.HoverResult](), err
	}
	src := s.semanticSource(uri)
	snapRev, bc := s.queryContext(be, uri)
	ctx, src, snapRev = s.captureEnvelopeInputs(ctx, uri, src, snapRev)
	result, err := withBackendWorkspaceSnapshot(ctx, s, be, snapshotFromCtx(ctx), func(ctx context.Context) (identity.SemanticResult[*languages.HoverResult], error) {
		return be.Hover(ctx, languages.HoverRequest{
			URI: uri, Content: src, SnapshotRev: snapRev, BuildContext: bc, Line: line, Column: col,
		})
	})
	if err != nil {
		return unavailableEnvelope[*languages.HoverResult](), err
	}
	result, err = verifyEnvelopeFresh(s, ctx, result)
	if err != nil {
		return unavailableEnvelope[*languages.HoverResult](), err
	}
	s.recordEvidence(ctx, "textDocument/hover", uri, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
	return result, nil
}

// DefinitionEnvelope is HoverEnvelope's counterpart for definitions.
func (s *Server) DefinitionEnvelope(ctx context.Context, uri string, line, col uint32) (identity.SemanticResult[[]languages.Location], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	uri = canonicalDocumentURI(uri)
	ctx, _, revision := s.captureEnvelopeInputs(ctx, uri, nil, 0)
	if result, ok := s.indexedLocationEnvelope(ctx, uri, line, col, false, persistentDefinition, revision); ok {
		var err error
		result, err = verifyEnvelopeFresh(s, ctx, result)
		if err != nil {
			return unavailableEnvelope[[]languages.Location](), err
		}
		s.recordEvidence(ctx, "textDocument/definition", uri, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	be, err := s.resolveBackend(uri)
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	src := s.semanticSource(uri)
	snapRev, bc := s.queryContext(be, uri)
	ctx, src, snapRev = s.captureEnvelopeInputs(ctx, uri, src, snapRev)
	result, err := withBackendWorkspaceSnapshot(ctx, s, be, snapshotFromCtx(ctx), func(ctx context.Context) (identity.SemanticResult[[]languages.Location], error) {
		return be.Definition(ctx, languages.DefinitionRequest{
			URI: uri, Content: src, SnapshotRev: snapRev, BuildContext: bc, Line: line, Column: col,
		})
	})
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	result, err = verifyEnvelopeFresh(s, ctx, result)
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	s.recordEvidence(ctx, "textDocument/definition", uri, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
	return result, nil
}

// ReferencesEnvelope is HoverEnvelope's counterpart for references.
func (s *Server) ReferencesEnvelope(ctx context.Context, uri string, line, col uint32, includeDecl bool) (identity.SemanticResult[[]languages.Location], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	uri = canonicalDocumentURI(uri)
	ctx, _, revision := s.captureEnvelopeInputs(ctx, uri, nil, 0)
	if result, ok := s.indexedLocationEnvelope(ctx, uri, line, col, includeDecl, persistentReferences, revision); ok {
		var err error
		result, err = verifyEnvelopeFresh(s, ctx, result)
		if err != nil {
			return unavailableEnvelope[[]languages.Location](), err
		}
		s.recordEvidence(ctx, "textDocument/references", uri, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	be, err := s.resolveBackend(uri)
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	src := s.semanticSource(uri)
	snapRev, bc := s.queryContext(be, uri)
	ctx, src, snapRev = s.captureEnvelopeInputs(ctx, uri, src, snapRev)
	result, err := withBackendWorkspaceSnapshot(ctx, s, be, snapshotFromCtx(ctx), func(ctx context.Context) (identity.SemanticResult[[]languages.Location], error) {
		return be.References(ctx, languages.ReferencesRequest{
			URI: uri, Content: src, SnapshotRev: snapRev, BuildContext: bc,
			Line: line, Column: col, IncludeDecl: includeDecl,
		})
	})
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	result, err = verifyEnvelopeFresh(s, ctx, result)
	if err != nil {
		return unavailableEnvelope[[]languages.Location](), err
	}
	s.recordEvidence(ctx, "textDocument/references", uri, result.Status, result.Completeness, result.Evidence, result.InternalDiagnostics)
	return result, nil
}

func unavailableEnvelope[T any]() identity.SemanticResult[T] {
	return identity.SemanticResult[T]{
		Status:       identity.ResultUnavailable,
		Completeness: identity.CompletenessUnknown,
	}
}

func verifyEnvelopeFresh[T any](s *Server, ctx context.Context, result identity.SemanticResult[T]) (identity.SemanticResult[T], error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return unavailableEnvelope[T](), err
	}
	captured := snapshotFromCtx(ctx)
	if captured == nil || s.snapMgr == nil {
		return result, nil
	}
	current := s.snapMgr.Current()
	if current != nil && current.ID().Revision != captured.ID().Revision {
		return unavailableEnvelope[T](), ierrors.New(ierrors.ErrContentModified, "server.envelope",
			fmt.Sprintf("workspace changed since request (snapshot %d, current %d)", captured.ID().Revision, current.ID().Revision))
	}
	return result, nil
}

func (s *Server) indexedLocationEnvelope(ctx context.Context, uri string, line, col uint32, includeDecl bool, query persistentSemanticQuery, revision uint64) (identity.SemanticResult[[]languages.Location], bool) {
	if ctx.Err() != nil {
		return identity.SemanticResult[[]languages.Location]{}, false
	}
	if result, ok := s.goSnapshotSemanticLocations(ctx, uri, line, col, 1, revision, query, includeDecl); ok {
		return result, true
	}
	return s.persistentSemanticLocations(ctx, uri, line, col, 1, revision, query, includeDecl)
}

func (s *Server) captureEnvelopeInputs(ctx context.Context, uri string, source []byte, revision uint64) (context.Context, []byte, uint64) {
	if ctx == nil {
		ctx = context.Background()
	}
	captured := snapshotFromCtx(ctx)
	if captured == nil && s.snapMgr != nil {
		captured = s.snapMgr.Current()
		if captured != nil {
			ctx = withSnapshot(ctx, captured)
		}
	}
	if captured != nil {
		revision = captured.ID().Revision
		if document := captured.Document(uri); document != nil {
			source = document.Content
		}
	}
	return ctx, source, revision
}

// semanticSource mirrors dispatchSemanticRequest's content resolution:
// captured snapshot document wins, then VFS overlay, then empty.
//
// §D12 hook: a registered virtual URI reads through to its host content so
// semantic calls never see an empty source for a virtual document.
// TODO(D12): full pipeline — region extraction on the way in (host content →
// virtual slice) and reverse-mapped positions on the way out (virtual result
// → host result) — requires coordinate projection threaded through every
// Envelope call site and the languages request/response structs; deferred
// until a backend actually emits virtual documents. The registry accessor
// below is the seam those call sites will use.
func (s *Server) semanticSource(uri string) []byte {
	if s.virtualReg != nil {
		if host, err := s.virtualReg.ResolveHost(uri); err == nil {
			uri = host
		}
	}
	if snap := s.snapMgr.Current(); snap != nil {
		if doc := snap.Document(uri); doc != nil && doc.Content != nil {
			return doc.Content
		}
	}
	if src := s.vfs.Content(uri); src != nil {
		return src
	}
	return []byte{}
}

// VirtualRegistry exposes the §D12 virtual-document/source-map registry so
// adapters can register host↔virtual mappings and S3 gates can consult them.
func (s *Server) VirtualRegistry() *virtual.Registry { return s.virtualReg }

// queryContext pairs the current snapshot revision with the backend's build
// context — the two identities every canonical request must carry.
func (s *Server) queryContext(be languages.Backend, uri string) (uint64, identity.BuildContextID) {
	snapRev := uint64(0)
	if snap := s.snapMgr.Current(); snap != nil {
		snapRev = snap.ID().Revision
	}
	return snapRev, backendBuildContext(be)
}

// SnapshotRevision returns the current snapshot revision for observability
// consumers such as the §P9 session recorder. Zero before first publish.
func (s *Server) SnapshotRevision() uint64 {
	if snap := s.snapMgr.Current(); snap != nil {
		return snap.Revision()
	}
	return 0
}

// projectBackendError enriches a failed request's error text with the
// backend's supervisor lifecycle message (§Q2): a crash becomes "Recovery in
// progress (crash #2, epoch 3)" instead of a bare "process terminated".
func projectBackendError(s *Server, err error) string {
	text := err.Error()
	var be *ierrors.Error
	if !errors.As(err, &be) || be.Op == "" {
		return text
	}
	s.mu.RLock()
	backend := s.languages[be.Op]
	s.mu.RUnlock()
	rep, ok := backend.(languages.StatusReporter)
	if !ok {
		return text
	}
	if status := rep.BackendStatusMessage(); status != "" {
		return text + " [" + status + "]"
	}
	return text
}
