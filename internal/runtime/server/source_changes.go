package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/security"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

const maxPendingSourceChanges = 4096
const maxPendingSourceBytes = 8 << 20

type sourceSyncState struct {
	backend          languages.Backend
	gate             chan struct{}
	pending          []languages.SourceChange // guarded by Server.mu
	bytes            int
	epoch            uint64
	err              error
	overflow         bool
	overflowSequence uint64
}

func (s *Server) sourceState(backend languages.Backend, create bool) *sourceSyncState {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.sourceSyncStates {
		if sameSemanticCapability(state.backend, backend) {
			return state
		}
	}
	if !create {
		return nil
	}
	state := &sourceSyncState{backend: backend, gate: make(chan struct{}, 1)}
	state.gate <- struct{}{}
	s.sourceSyncStates = append(s.sourceSyncStates, state)
	return state
}

// containExternalChanges executes the §N7 boundary on the external-change
// funnel (audit circle 2, finding 3): every change URI must resolve inside
// the session's workspace root before anything is queued for the child
// backends — this is the production caller the containment gate had been
// missing. A root-less session (no workspace root configured) has no domain
// to enforce and keeps prior behavior; the lifecycle gate already blocks
// external-change traffic until initialize.
//
// Fail-closed: one rejected URI refuses the whole batch, and the error text
// names only the client-supplied URI — never the resolved filesystem layout
// (N7's no-path-leak rule).
func (s *Server) containExternalChanges(changes []languages.SourceChange) error {
	root := s.workspaceRoot()
	if root == "" {
		return nil
	}
	for _, change := range changes {
		parsed, err := workspaceuri.Parse(change.URI)
		if err != nil || !parsed.IsFile() {
			return fmt.Errorf("external change URI %q failed workspace containment", change.URI)
		}
		path := parsed.PathOr()
		if path == "" {
			// A cross-host authority (file://evil-host/share/x) has no local
			// path on this platform — there is nothing inside the root it
			// could name, so refuse.
			return fmt.Errorf("external change URI %q failed workspace containment", change.URI)
		}
		if _, err := security.ResolveInWorkspace(root, path); err != nil {
			return fmt.Errorf("external change URI %q failed workspace containment", change.URI)
		}
	}
	return nil
}

// The document writer only invalidates and queues bounded events. Child I/O
// runs on the next semantic/diagnostic read, outside mutationMu, so a slow
// child cannot prevent the main reader from accepting shutdown or cancellation.
func (s *Server) applyExternalSourceChanges(ctx context.Context, changes []languages.SourceChange) error {
	changes, err := languages.NormalizeSourceChanges(changes)
	if err != nil || len(changes) == 0 {
		return err
	}
	if err := s.containExternalChanges(changes); err != nil {
		return err
	}
	bytes := 0
	for _, change := range changes {
		bytes += len(change.URI) + 1
	}
	var failures []error
	for _, backend := range s.workspaceBackends() {
		if _, ok := backend.(languages.SourceChangeSynchronizer); !ok {
			continue
		}
		state := s.sourceState(backend, true)
		epoch := backendEpoch(backend)
		s.mu.Lock()
		if len(state.pending)+len(changes) > maxPendingSourceChanges || state.bytes+bytes > maxPendingSourceBytes {
			state.err = fmt.Errorf("external source queue (%s) exceeded its bounded budget", backend.LanguageID())
			state.epoch = epoch
			state.overflow = true
			state.overflowSequence++
			failures = append(failures, state.err)
		} else {
			state.pending = append(state.pending, changes...)
			state.bytes += bytes
		}
		s.mu.Unlock()
	}
	// Every child must have either a queued invalidation or a freshness fence
	// before a reader can capture the new workspace revision. Publishing first
	// lets a concurrent watcher/read interleave query the old child state under
	// the new revision, even when both query-side flushes succeed.
	s.invalidateExternalSources()
	if s.diag != nil && s.State() == StateRunning {
		for _, uri := range s.vfs.OpenFiles() {
			s.diag.request(uri)
		}
	}
	return errors.Join(failures...)
}

// A read must deliver every queued event before using the child's memoized
// state. Failed delivery fences that epoch; a later partial hint cannot erase it.
func (s *Server) flushExternalSourceChanges(ctx context.Context, backend languages.Backend) error {
	state := s.sourceState(backend, false)
	if state == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-state.gate:
	}
	defer func() { state.gate <- struct{}{} }()
	epoch := backendEpoch(backend)
	s.mu.Lock()
	if state.overflow {
		fence := state.err
		sequence, queued := state.overflowSequence, len(state.pending)
		queuedBytes := state.bytes
		s.mu.Unlock()
		recoverer, ok := backend.(languages.SourceChangeRecoverer)
		if !ok {
			return fence
		}
		bounded, cancel := context.WithTimeout(ctx, s.externalSyncWindow())
		err := recoverer.RecoverSourceChanges(bounded)
		cancel()
		if err != nil {
			return errors.Join(fence, err)
		}
		epoch = backendEpoch(backend)
		s.mu.Lock()
		if state.overflowSequence != sequence {
			err := state.err
			s.mu.Unlock()
			return err // another event was lost while the new child initialized
		}
		state.pending = append([]languages.SourceChange(nil), state.pending[queued:]...)
		state.bytes -= queuedBytes
		state.overflow, state.err = false, nil
	}
	if state.err != nil && (state.epoch == 0 || state.epoch == epoch) {
		err := state.err
		s.mu.Unlock()
		return err
	}
	state.err = nil
	changes := append([]languages.SourceChange(nil), state.pending...)
	s.mu.Unlock()
	if len(changes) == 0 {
		return nil
	}
	bounded, cancel := context.WithTimeout(ctx, s.externalSyncWindow())
	err := backend.(languages.SourceChangeSynchronizer).NotifySourceChanges(bounded, changes)
	cancel()
	epoch = backendEpoch(backend)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err // the pending batch remains available to a later read
		}
		state.err = fmt.Errorf("external source synchronization (%s): %w", backend.LanguageID(), err)
		state.epoch = epoch
		return state.err
	}
	for _, change := range changes {
		state.bytes -= len(change.URI) + 1
	}
	state.pending = append([]languages.SourceChange(nil), state.pending[len(changes):]...)
	return nil
}
