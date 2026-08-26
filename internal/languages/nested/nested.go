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
//	internal/{jsonrpc,supervisor,errors,buildctx,identity,uri} only. No LSP
//	semantic types beyond position/textDocument envelopes it must forward.
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

	// Sup overrides the supervisor policy; nil → DefaultConfig. Tests use
	// it for millisecond backoffs instead of the production schedule.
	Sup *supervisor.Config
}

// Conn is the process-side bridge shared by all nested-LSP backends.
type Conn struct {
	cfg Config

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	pending map[int64]chan *jsonrpc.Message

	nextID atomic.Int64
	closed atomic.Bool

	// lastActivity (unix nanos) feeds the hung-worker watchdog (G5): a
	// process with in-flight requests but no traffic for HungGrace is
	// presumed wedged and restarted instead of waiting out full timeouts.
	lastActivity atomic.Int64

	// Build context identity (§E0), derived lazily once.
	buildCtxOnce sync.Once
	buildCtxID   identity.BuildContextID

	sup *supervisor.Supervisor
}

// New builds an idle Conn. It does not spawn anything: production callers
// follow up with StartSupervised; identity-only tests may stop here.
func New(cfg Config) *Conn {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	return &Conn{cfg: cfg, pending: make(map[int64]chan *jsonrpc.Message)}
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
	c.sup = supervisor.New(supCfg)
	c.wireSupervisor()
	c.sup.Start()
	if c.sup.State() == supervisor.StateDisabled {
		return fmt.Errorf("%s start blocked by workspace trust (OMNILSP_TRUST=%s)",
			c.cfg.Name, pol.State())
	}
	if err := c.cfg.Start(c); err != nil {
		return fmt.Errorf("%s start: %w", c.cfg.Name, err)
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
func (c *Conn) wireSupervisor() {
	c.sup.RegisterCallbacks(
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
	c.mu.Lock()
	c.cmd, c.stdin, c.stdout = cmd, stdin, stdout
	orphaned := c.pending
	c.pending = make(map[int64]chan *jsonrpc.Message)
	c.mu.Unlock()
	failAll(orphaned, c.cfg.Name+" process terminated")
	go c.readLoop(stdout)
}

// MarkReady promotes the supervisor into Ready after a successful handshake.
func (c *Conn) MarkReady() { c.sup.MarkReady() }

// SupervisorMessage exposes the supervisor's human-facing lifecycle state
// (§Q2 error projection): crash counts, epoch, recovery guidance.
func (c *Conn) SupervisorMessage() string { return c.sup.UserMessage() }

// WorkDir exposes the configured working directory to Start factories.
func (c *Conn) WorkDir() string { return c.cfg.WorkDir }

// HandleProcessExit is the single crash entry point: fail pending requests
// fast, then hand the lifecycle to the supervisor (backoff → restart or
// quarantine). Manual Close never routes here.
func (c *Conn) HandleProcessExit(reason error) {
	if c.closed.Load() {
		return
	}
	c.failPending(c.cfg.Name + " process terminated")
	c.sup.MarkUnhealthy(context.Background(), reason)
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
func (c *Conn) SupervisorEpoch() uint64 { return c.sup.Epoch() }

// Initialize performs the standard LSP initialize/initialized handshake
// rooted at the working directory. A failed handshake returns an error so
// callers can refuse MarkReady — a half-initialized process must never be
// promoted into the Ready rotation.
func (c *Conn) Initialize() error {
	rootURI := uri.FromPath(c.cfg.WorkDir).Canonical()
	ctx := context.Background()
	if _, err := c.SendRequest(ctx, "initialize", map[string]interface{}{
		"processId":    os.Getpid(),
		"rootUri":      rootURI,
		"capabilities": map[string]interface{}{},
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
func (c *Conn) readLoop(stdout io.ReadCloser) {
	codec := jsonrpc.NewCodec()
	r := bufio.NewReader(stdout)
	for {
		msg, err := codec.ReadMessage(r)
		if err != nil {
			break
		}
		c.lastActivity.Store(time.Now().UnixNano())
		if msg.ID == nil || msg.ID.IsStr {
			continue // server notifications / string IDs unsupported by this bridge
		}
		c.mu.Lock()
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
	// Process died: fail all pending requests fast instead of letting them
	// ride out their timeouts against a dead pipe, then let the supervisor
	// decide between backoff-restart and quarantine (§G2).
	c.failPending(c.cfg.Name + " process terminated")
	if !c.closed.Load() {
		c.HandleProcessExit(fmt.Errorf("%s output stream ended", c.cfg.Name))
	}
}

func (c *Conn) writeMessage(msg *jsonrpc.Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = fmt.Fprintf(c.stdin, "Content-Length: %d\r\n\r\n%s", len(data), data)
	return err
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
	ch := make(chan *jsonrpc.Message, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.writeMessage(jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, method, mustRaw(params))); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s write: %w", c.cfg.Name, err)
	}
	c.lastActivity.Store(time.Now().UnixNano())

	timer := time.NewTimer(c.cfg.RequestTimeout)
	defer timer.Stop()
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", c.cfg.Name, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-timer.C:
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
	return c.writeMessage(jsonrpc.NewNotification(method, mustRaw(params)))
}

// DidOpen forwards a textDocument/didOpen for the bridge's language.
func (c *Conn) DidOpen(langID, uriStr string, content []byte) {
	_ = c.Notify("textDocument/didOpen", map[string]interface{}{
		"textDocument": map[string]interface{}{
			"uri": uriStr, "languageId": langID, "version": 1, "text": string(content),
		},
	})
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

// Close shuts the process down in LSP order and releases the pipes.
// killProcess terminates the worker without marking the Conn permanently
// closed: unlike Close (manual shutdown), a watchdog kill must let the read
// loop observe EOF so the supervisor's crash/restart cycle engages.
func (c *Conn) killProcess() {
	c.mu.Lock()
	stdin, stdout, cmd := c.stdin, c.stdout, c.cmd
	c.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if stdout != nil {
		_ = stdout.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}

func (c *Conn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		_ = c.Notify("shutdown", nil)
		c.mu.Lock()
		stdin, stdout, cmd := c.stdin, c.stdout, c.cmd
		c.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
		if stdout != nil {
			_ = stdout.Close()
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	return nil
}

// maxDuration returns the larger of two durations.
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
