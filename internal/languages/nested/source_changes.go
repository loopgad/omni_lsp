package nested

import (
	"context"
	"fmt"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/runtime/supervisor"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

// SourceChange is one external file event to forward to a nested LSP server.
// Kind matches the LSP FileChangeType values.
type SourceChange struct {
	URI  string `json:"uri"`
	Kind int    `json:"type"`
}

// SourceChangeWriteUncertainError reports a notification whose transport
// write may have reached the child only partially. The affected child epoch
// has been detached and its streams closed before this error is returned.
type SourceChangeWriteUncertainError struct {
	Epoch     uint64
	Cause     error
	WriteErr  error
	RetireErr error
}

func (e *SourceChangeWriteUncertainError) Error() string {
	cause := e.Cause
	if cause == nil {
		cause = e.WriteErr
	}
	message := fmt.Sprintf("source change write outcome uncertain (child epoch %d fenced): %v", e.Epoch, cause)
	if e.WriteErr != nil {
		message += fmt.Sprintf(" (write: %v)", e.WriteErr)
	}
	if e.RetireErr != nil {
		message += fmt.Sprintf(" (child termination: %v)", e.RetireErr)
	}
	return message
}

func (e *SourceChangeWriteUncertainError) Unwrap() []error {
	causes := make([]error, 0, 3)
	if e.Cause != nil {
		causes = append(causes, e.Cause)
	}
	if e.WriteErr != nil {
		causes = append(causes, e.WriteErr)
	}
	if e.RetireErr != nil {
		causes = append(causes, e.RetireErr)
	}
	return causes
}

// NotifySourceChanges sends external file events while holding the exclusive
// workspace lease. This waits for active child queries, invalidates the
// generation used by later leases, and preserves event order and duplicates.
func (c *Conn) NotifySourceChanges(ctx context.Context, changes []SourceChange) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil {
		return ierrors.New(ierrors.ErrBackendUnavailable, "nested.workspace", "backend connection is unavailable")
	}
	normalized, err := normalizeSourceChanges(changes)
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		return nil
	}
	if c.closed.Load() {
		return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}
	if err := c.workspaceLease.acquireWriteContext(ctx); err != nil {
		return err
	}
	defer c.workspaceLease.releaseWrite()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed.Load()
	neverStarted := !closed && c.readerEpoch.Load() == 0 && c.stdin == nil
	c.mu.Unlock()
	if closed {
		return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}
	if neverStarted {
		// The child has no prior state to invalidate. Its first initialize will
		// read the current filesystem after the external change.
		return nil
	}

	// Invalidation runs once this operation owns the transport write turn. A
	// canceled write-lock waiter therefore leaves the current child untouched.
	return c.notifyWithContext(ctx, "workspace/didChangeWatchedFiles", struct {
		Changes []SourceChange `json:"changes"`
	}{Changes: normalized}, func() {
		// A write failure may have reached the child only partially, so its
		// workspace lease marker must remain invalid either way.
		c.workspaceLease.invalidate()
		c.mu.Lock()
		c.workspaceGeneration++
		c.mu.Unlock()
	})
}

// RecoverSourceChanges retires the current supervised child and waits for a
// later, successfully initialized supervisor epoch. It never treats a start
// attempt as recovery: only Attach followed by MarkReady advances the epoch.
func (c *Conn) RecoverSourceChanges(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c == nil {
		return ierrors.New(ierrors.ErrBackendUnavailable, "nested.workspace", "backend connection is unavailable")
	}
	if c.closed.Load() {
		return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}

	c.mu.Lock()
	sup := c.sup
	readerEpoch := c.readerEpoch.Load()
	stdin, stdout, cmd := c.stdin, c.stdout, c.cmd
	neverStarted := readerEpoch == 0 && stdin == nil && stdout == nil && cmd == nil
	ready := c.ready && c.readyReaderEpoch == readerEpoch && !c.exitHandled
	readyBackendEpoch := c.readyBackendEpoch
	exiting := c.exitHandled
	if c.sourceRecoveryPending {
		recoveryID := c.sourceRecoveryID
		targetEpoch := c.sourceRecoveryEpoch
		c.mu.Unlock()
		if sup == nil {
			return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend is not supervised")
		}
		return c.waitForSourceRecovery(ctx, sup, targetEpoch, recoveryID)
	}
	c.mu.Unlock()
	if sup == nil {
		if neverStarted {
			return nil
		}
		return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend is not supervised")
	}
	targetEpoch := sup.Epoch()
	ready = ready && readyBackendEpoch == targetEpoch
	state := sup.State()
	if state == supervisor.StateQuarantined {
		return fmt.Errorf("%s source change recovery: %w", c.cfg.Name, supervisor.ErrQuarantined)
	}
	if state == supervisor.StateDisabled {
		if neverStarted {
			return nil
		}
		return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend is disabled")
	}
	if neverStarted && (state == supervisor.StateReady || state == supervisor.StateDegraded) {
		return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "supervisor is ready without an attached child")
	}
	if (state == supervisor.StateReady || state == supervisor.StateDegraded) && !ready && !exiting {
		return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "supervisor is ready without a ready child connection")
	}

	c.mu.Lock()
	if c.sourceRecoveryPending {
		recoveryID := c.sourceRecoveryID
		targetEpoch = c.sourceRecoveryEpoch
		c.mu.Unlock()
		return c.waitForSourceRecovery(ctx, sup, targetEpoch, recoveryID)
	}
	c.sourceRecoveryID++
	recoveryID := c.sourceRecoveryID
	c.sourceRecoveryEpoch = targetEpoch
	c.sourceRecoveryPending = true
	c.mu.Unlock()

	if ready && (state == supervisor.StateReady || state == supervisor.StateDegraded) {
		reason := fmt.Errorf("%s source change synchronization requested child recovery", c.cfg.Name)
		if err := c.retireProcessEpoch(readerEpoch, stdin, stdout, cmd, reason); err != nil {
			return fmt.Errorf("%s source change recovery could not retire child epoch %d: %w", c.cfg.Name, readerEpoch, err)
		}
	}
	return c.waitForSourceRecovery(ctx, sup, targetEpoch, recoveryID)
}

func (c *Conn) waitForSourceRecovery(ctx context.Context, sup *supervisor.Supervisor, targetEpoch, recoveryID uint64) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.closed.Load() {
			return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed during source change recovery")
		}
		state := sup.State()
		if state == supervisor.StateQuarantined {
			return fmt.Errorf("%s source change recovery: %w", c.cfg.Name, supervisor.ErrQuarantined)
		}
		if state == supervisor.StateDisabled {
			return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend is disabled")
		}
		backendEpoch := sup.Epoch()
		c.mu.Lock()
		ready := c.ready && c.readyReaderEpoch == c.readerEpoch.Load() &&
			c.readyBackendEpoch == backendEpoch && !c.exitHandled
		if backendEpoch > targetEpoch && ready && c.sourceRecoveryPending && c.sourceRecoveryID == recoveryID {
			c.sourceRecoveryPending = false
		}
		c.mu.Unlock()
		if backendEpoch > targetEpoch && ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func normalizeSourceChanges(changes []SourceChange) ([]SourceChange, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	result := make([]SourceChange, len(changes))
	for i, change := range changes {
		if change.Kind < 1 || change.Kind > 3 {
			return nil, ierrors.New(ierrors.ErrInvalidArgument, "nested.workspace", fmt.Sprintf("invalid source change kind %d", change.Kind))
		}
		parsed, err := workspaceuri.Parse(change.URI)
		if err != nil {
			return nil, ierrors.New(ierrors.ErrInvalidArgument, "nested.workspace", fmt.Sprintf("invalid source change URI %q: %v", change.URI, err))
		}
		if !parsed.IsFile() {
			return nil, ierrors.New(ierrors.ErrInvalidArgument, "nested.workspace", fmt.Sprintf("source change URI %q is not a file URI", change.URI))
		}
		result[i] = SourceChange{URI: parsed.Canonical(), Kind: change.Kind}
	}
	return result, nil
}
