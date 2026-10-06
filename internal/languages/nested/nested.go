// Package nested is the shared bridge for out-of-process language servers
// that speak LSP over stdio (goal.md §G1 "compiler/language-service worker"
// class): clangd, rust-analyzer, pyright, typescript-language-server.
//
// Responsibility:
//
//	Process lifecycle (supervised restart with backoff and quarantine),
//	LSP initialize handshake, request/response correlation with bounded
//	waiting, notification writing, and digest-backed BuildContext identity.
//
// Owned mutable state:
//
//	cmd/stdin/stdout pipes (installed via Attach), the pending-response
//	table, and the supervisor. All guarded by mu or atomics; see F0/F2.
//
// Concurrency model:
//
//	One reader goroutine owns stdout decoding (F15); sendRequest blocks on
//	a per-call channel with ctx/timeout escape. Restarts are driven by the
//	supervisor's StateBackoff callback on a fresh goroutine.
//
// Invariants:
//
//  1. A crashed process fails all pending requests fast instead of letting
//     them ride out their timeouts against a dead pipe (§G2/G5).
//  2. Each successful (re)start increments the supervisor epoch, which
//     evidence consumers use to detect mid-session backend replacement
//     (INV-BACKEND-001).
//  3. Process spawning uses argument arrays — never a shell string (§G6).
//     This package never executes the workspace's own files.
//
// Allowed dependencies:
//
//	internal/{jsonrpc,supervisor,errors,buildctx,identity,trust,uri,workspace/position}
//	only. No LSP semantic types beyond position/textDocument envelopes it must
//	forward. trust and workspace/position were missing from this list while
//	being imported.
//
// Failure behavior:
//
//	Every method returns typed errors (errors package); semantic refusal is
//	the caller's (backend adapter's) concern.
package nested

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/supervisor"
	"github.com/omnilsp/omni/internal/trust"
	"github.com/omnilsp/omni/internal/workspace/buildctx"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

const defaultRequestTimeout = 30 * time.Second
const shutdownWriteTimeout = 100 * time.Millisecond
const shutdownGracePeriod = 5 * time.Second
const forcedExitWait = 1 * time.Second
const closeLeaseWait = 1 * time.Second

// Config wires a concrete language server into the shared bridge. Start is
// injectable so supervision can be tested without a real toolchain.
type Config struct {
	Name    string // server name for diagnostics, e.g. "clangd"
	Lang    string // build-context language tag, e.g. "cpp"
	WorkDir string

	// Start spawns (or re-spawns) the process and must Attach its pipes,
	// run the Initialize handshake, then call MarkReady. Production
	// factories use argument arrays per §G6.
	Start func(c *Conn) error

	// VersionProbe + ParseVersion derive the digest-backed build context
	// from the toolchain identity (§E0). ParseVersion receives the probe
	// output verbatim.
	VersionProbe func() (string, error)
	ParseVersion func(output string, err error) (string, error)

	RequestTimeout time.Duration // zero → defaultRequestTimeout
	// CaptureDiagnostics enables version-checked storage of upstream
	// textDocument/publishDiagnostics notifications for adapters that expose
	// compiler diagnostics. It is opt-in so explicitly deferred backends do
	// not accidentally claim support by receiving ignored worker output. When
	// enabled, Initialize also advertises the publishDiagnostics capability.
	CaptureDiagnostics bool
	// AllowUnversionedDiagnostics accepts versionless reports for the current
	// open document and worker epoch. Use only for servers such as TSLS that
	// omit the optional version field; explicit mismatched versions remain
	// rejected. A versionless report cannot prove freshness across edits.
	AllowUnversionedDiagnostics bool

	// Sup overrides the supervisor policy; nil → DefaultConfig. Tests use
	// it for millisecond backoffs instead of the production schedule.
	Sup *supervisor.Config
}

// writeMutex serializes child writes and supports cancellation while waiting
// for the write turn. Its zero value is ready for use.
type writeMutex struct {
	init    sync.Once
	token   chan struct{}
	waiters atomic.Int32
}

func (m *writeMutex) initialize() {
	m.init.Do(func() {
		m.token = make(chan struct{}, 1)
		m.token <- struct{}{}
	})
}

func (m *writeMutex) Lock() {
	m.initialize()
	<-m.token
}

func (m *writeMutex) Unlock() {
	m.initialize()
	m.token <- struct{}{}
}

func (m *writeMutex) LockContext(ctx context.Context) error {
	m.initialize()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.waiters.Add(1)
	defer m.waiters.Add(-1)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		if err := ctx.Err(); err != nil {
			m.token <- struct{}{}
			return err
		}
		return nil
	}
}

// Conn is the process-side bridge shared by all nested-LSP backends.
type Conn struct {
	cfg Config

	mu                     sync.Mutex
	writeMu                writeMutex
	documentMu             sync.Mutex
	diagnosticsMu          sync.Mutex // serializes publishDiagnostics processing
	workspaceLease         *workspaceLeaseGate
	cmd                    *exec.Cmd
	stdin                  io.WriteCloser
	stdout                 io.ReadCloser
	pending                map[int64]chan *jsonrpc.Message
	documents              map[string]*documentState
	canonicalDocuments     map[string]map[string]struct{}
	nextDocumentGeneration uint64
	// workspaceRevision is the latest immutable source snapshot observed by
	// this child. workspaceGeneration invalidates in-flight requests whenever
	// any document changes the mutable workspace view held by the child.
	workspaceRevision        uint64
	workspaceGeneration      uint64
	diagnosticsUpdateHandler func(uri string)
	readerEpoch              atomic.Uint64
	readyReaderEpoch         uint64 // protected by mu; paired with readyBackendEpoch
	readyBackendEpoch        uint64 // protected by mu; zero is a valid fixed epoch
	ready                    bool   // protected by mu
	exitHandled              bool

	nextID atomic.Int64
	closed atomic.Bool

	// lastActivity (unix nanos) feeds the hung-worker watchdog (G5): a
	// process with in-flight requests but no traffic for HungGrace is
	// presumed wedged and restarted instead of waiting out full timeouts.
	lastActivity atomic.Int64

	requestTiming         *requestTimingRecorder
	requestTimingWG       sync.WaitGroup
	processWaitMu         sync.Mutex
	processWaits          map[*exec.Cmd]*processWaitState
	closeOnce             sync.Once
	closeDone             chan struct{}
	closeErr              error
	sourceRecoveryID      uint64 // protected by mu; identifies an in-progress recovery request
	sourceRecoveryEpoch   uint64 // protected by mu; supervised epoch retired by that request
	sourceRecoveryPending bool   // protected by mu; cleared only after a newer ready epoch

	// Build context identity (§E0), derived lazily once.
	buildCtxOnce sync.Once
	buildCtxID   identity.BuildContextID

	sup *supervisor.Supervisor
}

type processWaitState struct {
	done       chan struct{}
	outputDone <-chan struct{}
	startOnce  sync.Once
	err        error
}

// New builds an idle Conn. It does not spawn anything: production callers
// follow up with StartSupervised; identity-only tests may stop here.
func New(cfg Config) *Conn {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	return &Conn{
		cfg:            cfg,
		pending:        make(map[int64]chan *jsonrpc.Message),
		documents:      make(map[string]*documentState),
		workspaceLease: newWorkspaceLeaseGate(),
		requestTiming:  newRequestTimingRecorder(cfg.Lang),
		processWaits:   make(map[*exec.Cmd]*processWaitState),
		closeDone:      make(chan struct{}),
	}
}

// StartSupervised wires the supervisor to the configured factory and starts
// the first process. A failed initial start is fatal for the backend (the
// caller surfaces it); later crashes enter the backoff/quarantine cycle.
//
// Trust gate (§N1/N3): OMNILSP_TRUST=untrusted|restricted restricts backend
// process starts. When the variable is unset entirely, local product policy
// treats an explicit editor launch as the user's trust signal (§N1 SHOULD
// elasticity); setting the variable to anything always wins.
func (c *Conn) StartSupervised() error {
	supCfg := supervisor.DefaultConfig()
	if c.cfg.Sup != nil {
		supCfg = *c.cfg.Sup
	}
	pol, err := trust.PolicyForBackend(c.cfg.WorkDir, c.cfg.Name)
	if err != nil {
		return err
	}
	supCfg.Trust = pol
	sup := supervisor.New(supCfg)
	c.mu.Lock()
	c.sup = sup
	c.mu.Unlock()
	c.wireSupervisor(sup)
	sup.Start()
	if sup.State() == supervisor.StateDisabled {
		return fmt.Errorf("%s start blocked by workspace trust (OMNILSP_TRUST=%s)",
			c.cfg.Name, pol.State())
	}
	if err := c.cfg.Start(c); err != nil {
		c.failPending(c.cfg.Name + " start failed")
		c.closed.Store(true)
		c.closeCurrentProcess()
		return fmt.Errorf("%s start: %w", c.cfg.Name, err)
	}
	if c.closed.Load() {
		return errors.New(errors.ErrBackendUnavailable, c.cfg.Name, "backend closed during start")
	}
	c.lastActivity.Store(time.Now().UnixNano())
	// The watchdog must never fire on a request still inside its legitimate
	// window: a project-wide analysis can legitimately run for many seconds.
	// Align the silence threshold with the per-request timeout so only a
	// backend that is silent BEYOND its own deadlines gets recycled.
	c.startWatchdog(maxDuration(supCfg.HungGrace, c.cfg.RequestTimeout))
	return nil
}

// wireSupervisor connects the restart policy to the process factory: when a
// backoff elapses (notified as StateBackoff — StateStarting is set silently
// by the supervisor and never notified), spawn a replacement process.
func (c *Conn) wireSupervisor(sup *supervisor.Supervisor) {
	sup.RegisterCallbacks(
		func(from, to supervisor.State, _ error) {
			if to == supervisor.StateBackoff && !c.closed.Load() {
				go func() {
					// A panicking Start factory must not take the whole
					// process down; treat it like any restart failure.
					defer func() {
						if r := recover(); r != nil {
							c.HandleProcessExit(fmt.Errorf("restart panic: %v", r))
						}
					}()
					if err := c.cfg.Start(c); err != nil {
						c.HandleProcessExit(fmt.Errorf("restart: %w", err))
					}
				}()
			}
		},
		nil,
	)
}

// Attach installs the pipes of a freshly spawned process and resets the
// pending table (fresh epoch, fresh table). Called by Start factories.
//
// Invariant §G2: requests still parked in the old table — registered during
// the crash→backoff→restart window — are failed fast here, never dropped;
// the replacement table only carries requests from the new epoch on.
func (c *Conn) Attach(cmd *exec.Cmd, stdin io.WriteCloser, stdout io.ReadCloser) {
	c.workspaceLease.acquireWrite()
	defer c.workspaceLease.releaseWrite()
	c.documentMu.Lock()
	defer c.documentMu.Unlock()
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		_ = c.stopProcessNow(stdin, stdout, cmd)
		return
	}
	epoch := c.readerEpoch.Add(1)
	c.ready = false
	c.readyReaderEpoch = 0
	c.readyBackendEpoch = 0
	var outputDone chan struct{}
	if cmd != nil && cmd.Process != nil {
		outputDone = make(chan struct{})
		c.processWaitMu.Lock()
		c.processWaits[cmd] = &processWaitState{
			done: make(chan struct{}), outputDone: outputDone,
		}
		c.processWaitMu.Unlock()
	}
	c.exitHandled = false
	oldStdin, oldStdout, oldCmd := c.stdin, c.stdout, c.cmd
	c.cmd, c.stdin, c.stdout = cmd, stdin, stdout
	orphaned := c.pending
	c.pending = make(map[int64]chan *jsonrpc.Message)
	c.invalidateDocumentsForRestartLocked(fmt.Errorf("%s worker restarted", c.cfg.Name), epoch)
	c.mu.Unlock()
	c.workspaceLease.invalidate()
	failAll(orphaned, c.cfg.Name+" process terminated")
	_ = c.stopProcessNow(oldStdin, oldStdout, oldCmd)
	if outputDone == nil {
		outputDone = make(chan struct{})
	}
	if stdout == nil {
		close(outputDone)
		return
	}
	go c.readLoop(stdout, epoch, outputDone)
}

// MarkReady promotes the supervisor into Ready after a successful handshake.
func (c *Conn) MarkReady() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() || c.sup == nil || c.exitHandled {
		return
	}
	readerEpoch := c.readerEpoch.Load()
	backendEpoch := c.sup.MarkReady()
	// Attach and MarkReady are serialized by mu. The binding therefore names
	// the exact pipe generation whose initialize handshake just succeeded.
	c.readyReaderEpoch = readerEpoch
	c.readyBackendEpoch = backendEpoch
	c.ready = true
}

// SupervisorMessage exposes the supervisor's human-facing lifecycle state
// (§Q2 error projection): crash counts, epoch, recovery guidance.
func (c *Conn) SupervisorMessage() string {
	c.mu.Lock()
	sup := c.sup
	c.mu.Unlock()
	if sup == nil {
		return "Backend has not been started."
	}
	return sup.UserMessage()
}

// WorkDir exposes the configured working directory to Start factories.
func (c *Conn) WorkDir() string { return c.cfg.WorkDir }

// SetDiagnosticsUpdateHandler registers a callback for each changed,
// version-validated diagnostic set from the current child connection.
func (c *Conn) SetDiagnosticsUpdateHandler(handler func(uri string)) {
	c.mu.Lock()
	c.diagnosticsUpdateHandler = handler
	c.mu.Unlock()
}

// storeDocumentLocked tracks the URI's canonical identity alongside its
// original spelling. Child language servers may normalize equivalent file
// URIs differently in publishDiagnostics.
func (c *Conn) storeDocumentLocked(uriStr string, state *documentState) {
	c.documents[uriStr] = state
	parsed, err := uri.Parse(uriStr)
	if err != nil {
		return
	}
	canonical := parsed.Canonical()
	if c.canonicalDocuments == nil {
		c.canonicalDocuments = make(map[string]map[string]struct{})
	}
	aliases := c.canonicalDocuments[canonical]
	if aliases == nil {
		aliases = make(map[string]struct{}, 1)
		c.canonicalDocuments[canonical] = aliases
	}
	aliases[uriStr] = struct{}{}
}

// findDocumentURI accepts a child's equivalent URI spelling only when it
// resolves to one tracked document. An exact spelling remains unambiguous.
// The caller holds c.mu.
func (c *Conn) findDocumentURI(uriStr string) (string, *documentState) {
	if state := c.documents[uriStr]; state != nil && state.open && state.epoch == c.readerEpoch.Load() {
		return uriStr, state
	}
	parsed, err := uri.Parse(uriStr)
	if err != nil {
		return "", nil
	}
	aliases := c.canonicalDocuments[parsed.Canonical()]
	var activeURI string
	var activeState *documentState
	for alias := range aliases {
		state := c.documents[alias]
		if state == nil || !state.open || state.epoch != c.readerEpoch.Load() {
			continue
		}
		if activeState != nil {
			return "", nil
		}
		activeURI, activeState = alias, state
	}
	return activeURI, activeState
}

// canonicalDocumentForSync carries the document high-water mark across an
// equivalent URI spelling after close. A second concurrently open spelling
// is rejected to avoid ambiguous snapshot ownership.
func (c *Conn) canonicalDocumentForSync(uriStr string, epoch uint64) (*documentState, bool) {
	parsed, err := uri.Parse(uriStr)
	if err != nil {
		return nil, false
	}
	aliases := c.canonicalDocuments[parsed.Canonical()]
	var previous *documentState
	for alias := range aliases {
		if alias == uriStr {
			continue
		}
		state := c.documents[alias]
		if state == nil || state.epoch != epoch {
			continue
		}
		if state.open {
			return nil, true
		}
		previous = mergeDocumentHistory(previous, state)
	}
	return previous, false
}

func mergeDocumentHistory(current, candidate *documentState) *documentState {
	if current == nil {
		if candidate == nil {
			return nil
		}
		merged := *candidate
		return &merged
	}
	if candidate == nil {
		return current
	}
	merged := *current
	if candidate.version > merged.version {
		merged.version = candidate.version
	}
	if candidate.highestRevision > merged.highestRevision {
		merged.highestRevision = candidate.highestRevision
	}
	merged.clientClosed = merged.clientClosed || candidate.clientClosed
	return &merged
}

// HandleProcessExit is the single crash entry point: fail pending requests
// fast, then hand the lifecycle to the supervisor (backoff → restart or
// quarantine). Manual Close never routes here.
func (c *Conn) HandleProcessExit(reason error) {
	if c.closed.Load() {
		return
	}
	c.mu.Lock()
	epoch := c.readerEpoch.Load()
	c.mu.Unlock()
	c.handleProcessExit(epoch, reason, false)
}

func (c *Conn) handleProcessExit(epoch uint64, reason error, deduplicate bool) {
	if c.closed.Load() {
		return
	}
	c.mu.Lock()
	if c.readerEpoch.Load() != epoch || (deduplicate && c.exitHandled) {
		c.mu.Unlock()
		return
	}
	c.exitHandled = true
	c.ready = false
	pending := c.pending
	c.pending = make(map[int64]chan *jsonrpc.Message)
	sup := c.sup
	c.mu.Unlock()
	failAll(pending, c.cfg.Name+" process terminated")
	if sup != nil {
		sup.MarkUnhealthy(context.Background(), reason)
	}
}

func (c *Conn) failPending(msg string) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan *jsonrpc.Message)
	c.mu.Unlock()
	failAll(pending, msg)
}

// failAll delivers a fast-fail error to every parked response channel.
// Non-blocking send: senders that already timed out must not be blocked.
func failAll(pending map[int64]chan *jsonrpc.Message, msg string) {
	for _, ch := range pending {
		select {
		case ch <- &jsonrpc.Message{Error: &jsonrpc.ResponseError{
			Code: jsonrpc.InternalError, Message: msg,
		}}:
		default:
		}
	}
}

// SupervisorEpoch reports the current lifecycle epoch (§G2).
func (c *Conn) SupervisorEpoch() uint64 {
	c.mu.Lock()
	sup := c.sup
	c.mu.Unlock()
	if sup == nil {
		return 0
	}
	return sup.Epoch()
}

// Initialize performs the standard LSP initialize/initialized handshake
// rooted at the working directory. A failed handshake returns an error so
// callers can refuse MarkReady — a half-initialized process must never be
// promoted into the Ready rotation.
func (c *Conn) Initialize() error {
	rootURI := uri.FromPath(c.cfg.WorkDir).Canonical()
	ctx := context.Background()
	capabilities := map[string]interface{}{}
	if c.cfg.CaptureDiagnostics {
		capabilities["textDocument"] = map[string]interface{}{
			"publishDiagnostics": map[string]interface{}{"relatedInformation": true},
		}
	}
	if _, err := c.SendRequest(ctx, "initialize", map[string]interface{}{
		"processId":    os.Getpid(),
		"rootUri":      rootURI,
		"capabilities": capabilities,
	}); err != nil {
		return fmt.Errorf("%s initialize: %w", c.cfg.Name, err)
	}
	c.Notify("initialized", map[string]interface{}{})
	return nil
}

// readLoop is the single reader goroutine for server output (F15).
// It owns the pipe snapshot it was started with; later Attach calls install
// new pipes without touching this goroutine's view (no cross-epoch race).
// Framing and decoding are delegated to the shared jsonrpc.Codec so message
// size limits and header parsing stay in one place (C1).
func (c *Conn) readLoop(stdout io.ReadCloser, epoch uint64, done chan<- struct{}) {
	defer close(done)
	codec := jsonrpc.NewCodec()
	r := bufio.NewReader(stdout)
	for {
		msg, err := codec.ReadMessage(r)
		if err != nil {
			break
		}
		c.lastActivity.Store(time.Now().UnixNano())
		if !c.closed.Load() && c.cfg.CaptureDiagnostics && msg.Method == "textDocument/publishDiagnostics" {
			c.receiveDiagnostics(epoch, msg.Params)
		}
		if msg.ID == nil || msg.ID.IsStr {
			continue // server notifications are handled above; string IDs are unsupported
		}
		c.mu.Lock()
		if c.readerEpoch.Load() != epoch {
			c.mu.Unlock()
			return
		}
		ch, ok := c.pending[msg.ID.Num]
		if ok {
			delete(c.pending, msg.ID.Num)
		}
		c.mu.Unlock()
		if ok {
			select {
			case ch <- msg:
			default: // sender timed out already; drop to avoid goroutine leak
			}
		}
	}
	if c.readerEpoch.Load() != epoch {
		return
	}
	c.handleProcessExit(epoch, fmt.Errorf("%s output stream ended", c.cfg.Name), true)
}

func (c *Conn) writeMessage(msg *jsonrpc.Message) error {
	return c.writeMessageDuringClose(msg, false)
}

func (c *Conn) writeMessageDuringClose(msg *jsonrpc.Message, duringClose bool) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	if c.closed.Load() && !duringClose {
		c.mu.Unlock()
		return fmt.Errorf("%s backend closed", c.cfg.Name)
	}
	if c.stdin == nil {
		c.mu.Unlock()
		return errors.New(errors.ErrBackendUnavailable, c.cfg.Name, "backend is not started")
	}
	stdin := c.stdin
	c.mu.Unlock()

	_, err = fmt.Fprintf(stdin, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
}

func (c *Conn) notifyWithContext(ctx context.Context, method string, params interface{}, beforeWrite func()) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.writeMessageWithContext(ctx, jsonrpc.NewNotification(method, mustRaw(params)), beforeWrite)
}

// writeMessageWithContext is used only for source-change notifications. It
// takes a cancellable write turn, and if cancellation or a transport error
// occurs after Write starts, it closes and retires that exact child epoch
// before returning. The writer goroutine is always joined before the write
// turn is released, so it cannot later mutate a replacement child's stream.
func (c *Conn) writeMessageWithContext(ctx context.Context, msg *jsonrpc.Message, beforeWrite func()) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := c.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer c.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return fmt.Errorf("%s backend closed", c.cfg.Name)
	}
	stdin, stdout, cmd := c.stdin, c.stdout, c.cmd
	epoch := c.readerEpoch.Load()
	c.mu.Unlock()

	if beforeWrite != nil {
		beforeWrite()
	}
	if stdin == nil {
		err := errors.New(errors.ErrBackendUnavailable, c.cfg.Name, "backend is not started")
		c.handleProcessExit(epoch, fmt.Errorf("%s notification %s write failed: %w", c.cfg.Name, msg.Method, err), true)
		return err
	}

	started := make(chan struct{})
	writeDone := make(chan error, 1)
	go func() {
		if err := ctx.Err(); err != nil {
			writeDone <- err
			return
		}
		close(started)
		_, err := fmt.Fprintf(stdin, "Content-Length: %d\r\n\r\n%s", len(data), data)
		writeDone <- err
	}()

	select {
	case writeErr := <-writeDone:
		if writeErr == nil {
			return nil
		}
		if !writeStarted(started) && ctx.Err() != nil {
			return ctx.Err()
		}
		return c.fenceUncertainSourceWrite(ctx, msg.Method, epoch, stdin, stdout, cmd, writeErr)
	case <-ctx.Done():
		// Prefer a completed full write over a cancellation that raced after
		// commit. If the child had started the write, retiring it will close
		// stdin and release the writer before this method returns.
		select {
		case writeErr := <-writeDone:
			if writeErr == nil {
				return nil
			}
			if !writeStarted(started) {
				return ctx.Err()
			}
			return c.fenceUncertainSourceWrite(ctx, msg.Method, epoch, stdin, stdout, cmd, writeErr)
		default:
		}
		select {
		case <-started:
			retireErr := c.retireProcessEpoch(epoch, stdin, stdout, cmd,
				fmt.Errorf("%s notification %s write canceled: %w", c.cfg.Name, msg.Method, ctx.Err()))
			writeErr := <-writeDone
			if writeErr == nil {
				return nil
			}
			return &SourceChangeWriteUncertainError{
				Epoch: epoch, Cause: ctx.Err(), WriteErr: writeErr, RetireErr: retireErr,
			}
		case writeErr := <-writeDone:
			if writeErr == nil {
				return nil
			}
			if !writeStarted(started) {
				return ctx.Err()
			}
			return c.fenceUncertainSourceWrite(ctx, msg.Method, epoch, stdin, stdout, cmd, writeErr)
		}
	}
}

func writeStarted(started <-chan struct{}) bool {
	select {
	case <-started:
		return true
	default:
		return false
	}
}

func (c *Conn) fenceUncertainSourceWrite(ctx context.Context, method string, epoch uint64, stdin io.WriteCloser, stdout io.ReadCloser, cmd *exec.Cmd, writeErr error) error {
	cause := writeErr
	var distinctWriteErr error
	if ctxErr := ctx.Err(); ctxErr != nil {
		cause = ctxErr
		distinctWriteErr = writeErr
	}
	retireErr := c.retireProcessEpoch(epoch, stdin, stdout, cmd,
		fmt.Errorf("%s notification %s write failed: %w", c.cfg.Name, method, writeErr))
	return &SourceChangeWriteUncertainError{
		Epoch: epoch, Cause: cause, WriteErr: distinctWriteErr, RetireErr: retireErr,
	}
}

// retireProcessEpoch detaches and closes only the captured process generation.
// Marking exitHandled before closing stdout prevents its reader from scheduling
// a second restart for the same failure. A replacement Attach has a larger
// reader epoch and is never detached or killed by a stale writer.
func (c *Conn) retireProcessEpoch(epoch uint64, stdin io.WriteCloser, stdout io.ReadCloser, cmd *exec.Cmd, reason error) error {
	var pending map[int64]chan *jsonrpc.Message
	var sup *supervisor.Supervisor
	markUnhealthy := false
	c.mu.Lock()
	current := c.readerEpoch.Load() == epoch
	if current {
		if c.stdin != nil {
			stdin = c.stdin
		}
		if c.stdout != nil {
			stdout = c.stdout
		}
		if c.cmd != nil {
			cmd = c.cmd
		}
		c.stdin, c.stdout, c.cmd = nil, nil, nil
		c.ready = false
		pending = c.pending
		c.pending = make(map[int64]chan *jsonrpc.Message)
		if !c.exitHandled {
			c.exitHandled = true
			if !c.closed.Load() {
				sup = c.sup
				markUnhealthy = sup != nil
			}
		}
	}
	c.mu.Unlock()

	if pending != nil {
		failAll(pending, c.cfg.Name+" process terminated")
	}
	stopErr := c.stopProcessNow(stdin, stdout, cmd)
	if markUnhealthy && !c.closed.Load() {
		sup.MarkUnhealthy(context.Background(), reason)
	}
	return stopErr
}

// RegisterPending creates the response channel for an id. Exported for
// supervision tests that pre-register requests before simulating a crash.
func (c *Conn) RegisterPending(id int64) <-chan *jsonrpc.Message {
	ch := make(chan *jsonrpc.Message, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	return ch
}

// SendRequest issues one request and waits for the response.
// C7/F10: ctx cancellation and the bounded timeout both terminate the wait;
// the pending entry is always cleaned up on abandonment.
func (c *Conn) SendRequest(ctx context.Context, method string, params interface{}) (json.RawMessage, error) {
	if c.closed.Load() {
		return nil, errors.New(errors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}
	id := c.nextID.Add(1)
	request := jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, method, mustRaw(params))
	var timingURI string
	if c.requestTiming != nil && isNestedRPCTimingMethod(method) {
		timingURI = documentURIFromParams(request.Params)
	}
	var parentRequestID json.RawMessage
	if timingURI != "" {
		parentRequestID = requestTimingParentIDFromContext(ctx)
	}
	ch := make(chan *jsonrpc.Message, 1)
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return nil, errors.New(errors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}
	c.pending[id] = ch
	if timingURI != "" {
		c.requestTimingWG.Add(1)
	}
	c.mu.Unlock()
	var timingStarted time.Time
	if timingURI != "" {
		defer c.requestTimingWG.Done()
		timingStarted = time.Now()
	}
	if err := c.writeMessage(request); err != nil {
		if timingURI != "" {
			finished := time.Now()
			writeDuration := finished.Sub(timingStarted)
			c.requestTiming.record(method, timingURI, writeDuration, writeDuration, 0, parentRequestID, requestTimingWriteError)
		}
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s write: %w", c.cfg.Name, err)
	}
	var writeDuration time.Duration
	var waitStarted time.Time
	if timingURI != "" {
		writeDuration = time.Since(timingStarted)
		waitStarted = time.Now()
	}
	c.lastActivity.Store(time.Now().UnixNano())

	timer := time.NewTimer(c.cfg.RequestTimeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		if timingURI != "" {
			finished := time.Now()
			outcome := requestTimingResponse
			if resp.Error != nil {
				outcome = requestTimingErrorResponse
			}
			c.requestTiming.record(method, timingURI, finished.Sub(timingStarted), writeDuration, finished.Sub(waitStarted), parentRequestID, outcome)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", c.cfg.Name, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		if timingURI != "" {
			finished := time.Now()
			c.requestTiming.record(method, timingURI, finished.Sub(timingStarted), writeDuration, finished.Sub(waitStarted), parentRequestID, requestTimingCanceled)
		}
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-timer.C:
		if timingURI != "" {
			finished := time.Now()
			c.requestTiming.record(method, timingURI, finished.Sub(timingStarted), writeDuration, finished.Sub(waitStarted), parentRequestID, requestTimingTimeout)
		}
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, errors.New(errors.ErrTimeout, c.cfg.Name,
			fmt.Sprintf("%s request %s timed out after %s", c.cfg.Name, method, c.cfg.RequestTimeout))
	}
}

// hasPending reports whether any in-flight requests await responses.
func (c *Conn) hasPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending) > 0
}

// startWatchdog launches the G5 hung-worker detector. Every tick it checks:
// pending requests present AND no traffic for HungGrace ⇒ wedged process.
// HandleHungWorker fails the parked requests immediately and kills the
// process; the read-loop exit then drives the normal restart cycle.
func (c *Conn) startWatchdog(grace time.Duration) {
	if grace <= 0 {
		grace = 5 * time.Second
	}
	go func() {
		ticker := time.NewTicker(grace / 2)
		defer ticker.Stop()
		for range ticker.C {
			if c.closed.Load() {
				return
			}
			if !c.hasPending() {
				continue // idle: silence is normal
			}
			idle := time.Since(time.Unix(0, c.lastActivity.Load()))
			if idle < grace {
				continue
			}
			sup := c.sup
			go func() {
				sup.HandleHungWorker(
					func() { c.failPending(c.cfg.Name + " worker hung; requests failed") },
					c.killProcess,
				)
			}()
			return // process death routes through HandleProcessExit → restart
		}
	}()
}

func mustRaw(params interface{}) json.RawMessage {
	if params == nil {
		return nil
	}
	data, err := json.Marshal(params)
	if err != nil {
		return nil
	}
	return data
}

// Notify sends a server-bound notification (no id, no response).
func (c *Conn) Notify(method string, params interface{}) error {
	if err := c.writeMessage(jsonrpc.NewNotification(method, mustRaw(params))); err != nil {
		c.handleProcessExit(c.readerEpoch.Load(), fmt.Errorf("%s notification %s write failed: %w", c.cfg.Name, method, err), true)
		return err
	}
	return nil
}

// DidOpen synchronizes the current document text with the child LSP.
func (c *Conn) DidOpen(langID, uriStr string, content []byte) {
	_, _ = c.SyncDocument(langID, uriStr, content)
}

// BuildContextID returns the digest-backed identity of this connection's
// build context (§E0). Derived once from the injected version probe; on
// probe failure a stable "<lang>:sha256:unavailable" ID is returned so
// evidence never fabricates a real context.
func (c *Conn) BuildContextID() identity.BuildContextID {
	c.buildCtxOnce.Do(func() {
		const unavailable = "unavailable"
		if c.cfg.VersionProbe == nil || c.cfg.ParseVersion == nil {
			c.buildCtxID = identity.BuildContextID(c.cfg.Lang + ":sha256:" + unavailable)
			return
		}
		ver, err := c.cfg.ParseVersion(c.cfg.VersionProbe())
		if err != nil || ver == "" {
			c.buildCtxID = identity.BuildContextID(c.cfg.Lang + ":sha256:" + unavailable)
			return
		}
		ctx := buildctx.Context{
			Language: c.cfg.Lang,
			Toolchain: buildctx.ToolchainIdentity{
				Kind:    c.cfg.Name,
				Version: ver,
			},
			WorkingDir: c.cfg.WorkDir,
		}
		c.buildCtxID = ctx.ID()
	})
	return c.buildCtxID
}

// killProcess terminates the worker without marking the Conn permanently
// closed: unlike Close (manual shutdown), a watchdog kill must let the read
// loop observe EOF so the supervisor's crash/restart cycle engages.
func (c *Conn) killProcess() {
	c.mu.Lock()
	stdin, stdout, cmd := c.stdin, c.stdout, c.cmd
	c.mu.Unlock()
	_ = c.stopProcessNow(stdin, stdout, cmd)
}

func (c *Conn) stopProcessNow(stdin io.WriteCloser, stdout io.ReadCloser, cmd *exec.Cmd) error {
	if stdin != nil {
		_ = stdin.Close()
	}
	if stdout != nil {
		_ = stdout.Close()
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Kill()
	state := c.processWaitState(cmd)
	timer := time.NewTimer(forcedExitWait)
	defer timer.Stop()
	select {
	case <-state.done:
		return nil
	case <-timer.C:
		return fmt.Errorf("%s process did not exit within %s after forced termination", c.cfg.Name, forcedExitWait)
	}
}

func (c *Conn) processWaitState(cmd *exec.Cmd) *processWaitState {
	c.processWaitMu.Lock()
	state := c.processWaits[cmd]
	if state == nil {
		state = &processWaitState{done: make(chan struct{})}
		c.processWaits[cmd] = state
	}
	c.processWaitMu.Unlock()
	state.startOnce.Do(func() {
		go func() {
			if state.outputDone != nil {
				<-state.outputDone
			}
			state.err = cmd.Wait()
			close(state.done)
		}()
	})
	return state
}

func (c *Conn) closeCurrentProcess() {
	c.mu.Lock()
	stdin, stdout, cmd := c.stdin, c.stdout, c.cmd
	c.stdin, c.stdout, c.cmd = nil, nil, nil
	c.mu.Unlock()
	_ = c.stopProcessNow(stdin, stdout, cmd)
}

// Close shuts the process down in LSP order and releases the pipes. It is safe
// to call more than once.
func (c *Conn) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		// Wake requests first so any snapshot read leases they own can finish.
		c.failPending(c.cfg.Name + " backend closed")
		// Shut down the child before waiting for workspace leases. A write
		// blocked in a child pipe is released by the bounded terminate fallback.
		c.closeErr = c.shutdownCurrentProcess()

		leaseCtx, cancel := context.WithTimeout(context.Background(), closeLeaseWait)
		leaseErr := c.workspaceLease.acquireWriteContext(leaseCtx)
		cancel()
		if leaseErr != nil {
			if c.closeErr == nil {
				c.closeErr = fmt.Errorf("%s timed out waiting for workspace leases: %w", c.cfg.Name, leaseErr)
			}
			// A canceled workspace writer is removed from the queue. Invalidate
			// the marker even when a leaked read lease prevents exclusive access;
			// closed connections must not expose a reusable snapshot.
			c.workspaceLease.invalidate()
		}
		c.documentMu.Lock()
		c.mu.Lock()
		c.invalidateDocumentsLocked(fmt.Errorf("%s backend closed", c.cfg.Name))
		c.mu.Unlock()
		c.documentMu.Unlock()
		c.workspaceLease.invalidate()
		if leaseErr == nil {
			c.workspaceLease.releaseWrite()
		}
		if c.requestTiming != nil {
			c.requestTimingWG.Wait()
			if err := c.requestTiming.flush(); c.closeErr == nil {
				c.closeErr = err
			}
		}
		close(c.closeDone)
	})
	<-c.closeDone
	return c.closeErr
}

func (c *Conn) shutdownCurrentProcess() error {
	c.mu.Lock()
	stdin, stdout, cmd := c.stdin, c.stdout, c.cmd
	c.mu.Unlock()
	if stdin == nil {
		if cmd != nil && cmd.Process != nil {
			forceErr := c.stopProcessNow(stdin, stdout, cmd)
			return forcedShutdownError(c.cfg.Name,
				fmt.Errorf("%s graceful shutdown unavailable: child stdin is missing", c.cfg.Name), forceErr)
		}
		if stdout != nil {
			_ = stdout.Close()
		}
		if stdout != nil {
			return fmt.Errorf("%s graceful shutdown unavailable: child stdin is missing", c.cfg.Name)
		}
		return nil
	}
	timeout := shutdownWriteTimeout
	if cmd != nil && cmd.Process != nil {
		timeout = shutdownGracePeriod
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	responded, shutdownErr := c.requestShutdown(ctx)
	var lifecycleErr error
	if shutdownErr != nil {
		lifecycleErr = fmt.Errorf("%s shutdown request: %w", c.cfg.Name, shutdownErr)
	}
	exitSent := false
	if responded {
		if err := c.writeCloseNotification(ctx, "exit"); err != nil {
			lifecycleErr = appendLifecycleError(lifecycleErr, fmt.Errorf("%s exit notification: %w", c.cfg.Name, err))
		} else {
			exitSent = true
		}
	}
	processReaped := false
	if cmd != nil && cmd.Process != nil {
		if responded && exitSent {
			state := c.processWaitState(cmd)
			select {
			case <-state.done:
				processReaped = true
				if state.err != nil {
					lifecycleErr = appendLifecycleError(lifecycleErr,
						fmt.Errorf("%s graceful process wait: %w", c.cfg.Name, state.err))
				}
			case <-ctx.Done():
				lifecycleErr = appendLifecycleError(lifecycleErr,
					fmt.Errorf("%s graceful shutdown timed out waiting for process exit: %w", c.cfg.Name, ctx.Err()))
			}
		}
		if !processReaped {
			if lifecycleErr == nil {
				lifecycleErr = fmt.Errorf("%s graceful shutdown did not complete", c.cfg.Name)
			}
			forceErr := c.stopProcessNow(stdin, stdout, cmd)
			lifecycleErr = forcedShutdownError(c.cfg.Name, lifecycleErr, forceErr)
		} else {
			if stdin != nil {
				_ = stdin.Close()
			}
			if stdout != nil {
				_ = stdout.Close()
			}
		}
	} else {
		if stdin != nil {
			_ = stdin.Close()
		}
		if stdout != nil {
			_ = stdout.Close()
		}
	}
	c.mu.Lock()
	if c.cmd == cmd {
		c.stdin, c.stdout, c.cmd = nil, nil, nil
	}
	c.mu.Unlock()
	return lifecycleErr
}

func appendLifecycleError(current, next error) error {
	if current == nil {
		return next
	}
	if next == nil {
		return current
	}
	return fmt.Errorf("%v; %w", current, next)
}

func forcedShutdownError(name string, cause, forceErr error) error {
	if cause == nil {
		cause = fmt.Errorf("graceful shutdown did not complete")
	}
	if forceErr != nil {
		return fmt.Errorf("%s lifecycle: %w; forced termination/reaping failed: %v", name, cause, forceErr)
	}
	return fmt.Errorf("%s lifecycle: %w; process was force-terminated", name, cause)
}

func (c *Conn) requestShutdown(ctx context.Context) (bool, error) {
	id := c.nextID.Add(1)
	ch := make(chan *jsonrpc.Message, 1)
	request := jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, "shutdown", nil)
	c.mu.Lock()
	if c.stdin == nil {
		c.mu.Unlock()
		return false, fmt.Errorf("%s backend is not started", c.cfg.Name)
	}
	c.pending[id] = ch
	c.mu.Unlock()

	writeDone := make(chan error, 1)
	go func() { writeDone <- c.writeMessageDuringClose(request, true) }()
	select {
	case err := <-writeDone:
		if err != nil {
			c.removePending(id)
			return false, err
		}
	case <-ctx.Done():
		c.removePending(id)
		return false, ctx.Err()
	}
	select {
	case response := <-ch:
		if response.Error != nil {
			return true, fmt.Errorf("%s shutdown response: %s", c.cfg.Name, response.Error.Message)
		}
		return true, nil
	case <-ctx.Done():
		c.removePending(id)
		return false, ctx.Err()
	}
}

func (c *Conn) writeCloseNotification(ctx context.Context, method string) error {
	notification := jsonrpc.NewNotification(method, nil)
	writeDone := make(chan error, 1)
	go func() { writeDone <- c.writeMessageDuringClose(notification, true) }()
	timer := time.NewTimer(shutdownWriteTimeout)
	defer timer.Stop()
	select {
	case err := <-writeDone:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("%s %s notification write exceeded %s", c.cfg.Name, method, shutdownWriteTimeout)
	}
}

func (c *Conn) removePending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// maxDuration returns the larger of two durations.
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
