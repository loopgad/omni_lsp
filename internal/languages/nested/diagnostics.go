package nested

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/runtime/supervisor"
	"github.com/omnilsp/omni/internal/workspace/position"
)

// Diagnostic is the normalized subset of an upstream LSP diagnostic that the
// language adapters publish through the frozen Backend interface.
type Diagnostic struct {
	StartLine uint32
	StartChar uint32
	EndLine   uint32
	EndChar   uint32
	Severity  int
	Code      string
	Source    string
	Message   string
}

// DocumentVersion identifies one synchronized document generation. The
// opaque generation and worker epoch prevent a late notification from a
// prior document revision or restarted process from satisfying a new waiter.
type DocumentVersion struct {
	URI                 string
	Version             int32
	epoch               uint64
	serial              uint64
	workspaceGeneration uint64
}

type documentState struct {
	langID                       string
	contentHash                  [32]byte
	open                         bool
	clientClosed                 bool
	version                      int32
	epoch                        uint64
	serial                       uint64
	snapshotRevision             uint64
	highestRevision              uint64
	allowUnversioned             bool
	diagnosticsRefreshGeneration uint64
	diagnostics                  []Diagnostic
	diagnosticsReceived          bool
	diagnosticsPublishing        chan struct{}
	diagnosticsReady             chan struct{}
	diagnosticsClosed            bool
	diagnosticsErr               error
}

type publishDiagnosticsParams struct {
	URI         string          `json:"uri"`
	Version     *int32          `json:"version"`
	Diagnostics json.RawMessage `json:"diagnostics"`
}

type wireDiagnostic struct {
	Range    *wireRange      `json:"range"`
	Severity *int            `json:"severity"`
	Code     json.RawMessage `json:"code"`
	Source   string          `json:"source"`
	Message  *string         `json:"message"`
}

type wireRange struct {
	Start *wirePosition `json:"start"`
	End   *wirePosition `json:"end"`
}

type wirePosition struct {
	Line      *uint32 `json:"line"`
	Character *uint32 `json:"character"`
}

// SyncDocument opens a document once and sends full-text didChange updates
// for later content. Repeated calls with the same content reuse its version.
func (c *Conn) SyncDocument(langID, uri string, content []byte) (DocumentVersion, error) {
	return c.SyncDocumentAtRevision(langID, uri, content, 0)
}

// SyncDocumentAtRevision synchronizes one source snapshot with the child LSP.
// A revision-aware caller cannot roll the child back to an older snapshot.
func (c *Conn) SyncDocumentAtRevision(langID, uri string, content []byte, snapshotRevision uint64) (DocumentVersion, error) {
	c.workspaceLease.acquireWrite()
	defer c.workspaceLease.releaseWrite()
	token, err := c.syncDocumentAtRevision(langID, uri, content, snapshotRevision)
	c.workspaceLease.invalidate()
	return token, err
}

// DidSaveDocumentAtRevision synchronizes a saved source snapshot and forwards
// didSave to an already-open child document. The workspace write lease keeps
// didChange and didSave ordered as one lifecycle transition.
func (c *Conn) DidSaveDocumentAtRevision(langID, uri string, content []byte, snapshotRevision uint64) error {
	if langID == "" || uri == "" {
		return ierrors.New(ierrors.ErrInvalidArgument, "nested.didSave", "language id and document URI are required")
	}
	c.workspaceLease.acquireWrite()
	defer c.workspaceLease.releaseWrite()
	if c.closed.Load() {
		return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}

	// didSave is valid only after didOpen. A source document that the nested
	// backend has not opened will be synchronized on its first actual request.
	c.mu.Lock()
	documentURI, state := c.findDocumentURI(uri)
	open := state != nil && state.open && state.epoch == c.readerEpoch.Load()
	c.mu.Unlock()
	if !open {
		return nil
	}
	if _, err := c.syncDocumentAtRevision(langID, documentURI, content, snapshotRevision); err != nil {
		c.workspaceLease.invalidate()
		return err
	}
	c.workspaceLease.invalidate()
	return c.Notify("textDocument/didSave", map[string]any{
		"textDocument": map[string]string{"uri": documentURI},
	})
}

// SyncDocumentAtRevisionContext reuses the document token carried by a
// workspace snapshot context. It is safe to call while the corresponding
// shared lease is active; without one it delegates to the exclusive sync API.
func (c *Conn) SyncDocumentAtRevisionContext(ctx context.Context, langID, uri string, content []byte, snapshotRevision uint64) (DocumentVersion, error) {
	if lease, ok := workspaceLeaseFromContext(ctx); ok {
		if lease.conn != c {
			return DocumentVersion{}, ierrors.New(ierrors.ErrInvalidArgument, "nested.didOpen", "workspace snapshot lease belongs to a different child connection")
		}
		if err := lease.beginUse(); err != nil {
			return DocumentVersion{}, err
		}
		defer lease.endUse()
		token, err := lease.document(uri, langID, content, snapshotRevision)
		if err != nil {
			return DocumentVersion{}, err
		}
		if err := c.validateDocumentVersion(token); err != nil {
			return DocumentVersion{}, err
		}
		return token, nil
	}
	return c.SyncDocumentAtRevision(langID, uri, content, snapshotRevision)
}

// syncDocumentAtRevision performs the document mutation while the caller owns
// the workspace write lease. Workspace snapshots use it to reconcile several
// documents as one transaction with respect to backend queries.
func (c *Conn) syncDocumentAtRevision(langID, uri string, content []byte, snapshotRevision uint64) (DocumentVersion, error) {
	if langID == "" || uri == "" {
		return DocumentVersion{}, ierrors.New(ierrors.ErrInvalidArgument, "nested.didOpen", "language id and document URI are required")
	}
	if c.closed.Load() {
		return DocumentVersion{}, ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}
	contentHash := sha256.Sum256(content)

	c.documentMu.Lock()
	defer c.documentMu.Unlock()

	epoch := c.readerEpoch.Load()
	c.mu.Lock()
	if snapshotRevision != 0 && snapshotRevision < c.workspaceRevision {
		current := c.workspaceRevision
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrContentModified, "nested.didOpen", fmt.Sprintf("snapshot revision %d is older than workspace revision %d", snapshotRevision, current))
	}
	previous := c.documents[uri]
	if previous != nil && previous.epoch != epoch {
		previous = nil
	}
	canonicalPrevious, activeAlias := c.canonicalDocumentForSync(uri, epoch)
	if activeAlias {
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrInvalidArgument, "nested.didOpen", "canonical-equivalent document URI is already open")
	}
	if canonicalPrevious != nil && previous != nil && previous.langID != "" && canonicalPrevious.langID != "" && previous.langID != canonicalPrevious.langID {
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrInvalidArgument, "nested.didOpen", "canonical-equivalent document URI has a different language id")
	}
	if previous == nil {
		previous = canonicalPrevious
	} else if !previous.open {
		previous = mergeDocumentHistory(previous, canonicalPrevious)
	}
	if previous != nil && previous.langID != langID {
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrInvalidArgument, "nested.didOpen", "document language changed for an open URI")
	}
	if previous != nil && !previous.open && previous.clientClosed &&
		(snapshotRevision == 0 || snapshotRevision <= previous.highestRevision) {
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrContentModified, "nested.didOpen", "document was closed after the requested snapshot")
	}
	if previous != nil && snapshotRevision != 0 && snapshotRevision < previous.highestRevision {
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrContentModified, "nested.didOpen", fmt.Sprintf("snapshot revision %d is older than synchronized revision %d", snapshotRevision, previous.highestRevision))
	}
	unchanged := previous != nil && previous.contentHash == contentHash
	if unchanged {
		if snapshotRevision != 0 {
			if snapshotRevision > c.workspaceRevision {
				c.workspaceRevision = snapshotRevision
				c.workspaceGeneration++
			}
			if snapshotRevision != previous.snapshotRevision || snapshotRevision > previous.highestRevision {
				if !previous.diagnosticsClosed {
					previous.diagnosticsErr = ierrors.New(ierrors.ErrContentModified, "nested.diagnostics", "workspace snapshot advanced before diagnostics arrived")
					previous.diagnosticsClosed = true
					close(previous.diagnosticsReady)
				}
				c.nextDocumentGeneration++
				updated := *previous
				updated.serial = c.nextDocumentGeneration
				updated.snapshotRevision = snapshotRevision
				if snapshotRevision > updated.highestRevision {
					updated.highestRevision = snapshotRevision
				}
				updated.allowUnversioned = false
				updated.diagnostics = nil
				updated.diagnosticsPublishing = nil
				updated.diagnosticsReady = make(chan struct{})
				updated.diagnosticsClosed = false
				updated.diagnosticsErr = nil
				previous = &updated
				c.storeDocumentLocked(uri, previous)
			}
		}
		token := DocumentVersion{URI: uri, Version: previous.version, epoch: previous.epoch, serial: previous.serial, workspaceGeneration: c.workspaceGeneration}
		c.mu.Unlock()
		return token, nil
	}
	if previous != nil && previous.open && snapshotRevision != 0 && snapshotRevision == previous.highestRevision &&
		previous.snapshotRevision == snapshotRevision && !unchanged {
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrContentModified, "nested.didOpen", "different document content has the same snapshot revision")
	}
	if previous != nil && previous.version == int32(^uint32(0)>>1) {
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrInvalidDocumentVersion, "nested.didOpen", "document version exhausted")
	}
	if snapshotRevision > c.workspaceRevision {
		c.workspaceRevision = snapshotRevision
	}
	// LSP workers keep a mutable workspace, not historical snapshots. Any
	// didOpen/didChange therefore invalidates responses captured against the
	// previous workspace generation, including requests for other URIs.
	c.workspaceGeneration++
	workspaceGeneration := c.workspaceGeneration

	version := int32(1)
	method := "textDocument/didOpen"
	highestRevision := snapshotRevision
	if previous != nil {
		version = previous.version + 1
		if previous.open {
			method = "textDocument/didChange"
		}
		if previous.highestRevision > highestRevision {
			highestRevision = previous.highestRevision
		}
		if !previous.diagnosticsClosed {
			previous.diagnosticsErr = ierrors.New(ierrors.ErrContentModified, "nested.diagnostics", "document changed before diagnostics arrived")
			previous.diagnosticsClosed = true
			close(previous.diagnosticsReady)
		}
	}
	c.nextDocumentGeneration++
	state := &documentState{
		langID:                       langID,
		contentHash:                  contentHash,
		open:                         true,
		version:                      version,
		epoch:                        epoch,
		serial:                       c.nextDocumentGeneration,
		snapshotRevision:             snapshotRevision,
		highestRevision:              highestRevision,
		diagnosticsRefreshGeneration: workspaceGeneration,
		// The first open in a worker epoch has no older in-epoch document
		// notification to confuse with this report. Later changes require a
		// version because an unversioned report cannot be tied to current text.
		allowUnversioned: previous == nil || (previous != nil && !previous.open && !previous.clientClosed),
		diagnosticsReady: make(chan struct{}),
	}
	c.storeDocumentLocked(uri, state)
	c.mu.Unlock()

	params := any(map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": langID, "version": version, "text": string(content),
		},
	})
	if method == "textDocument/didChange" {
		params = map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []any{map[string]string{"text": string(content)}},
		}
	}
	if err := c.Notify(method, params); err != nil {
		c.mu.Lock()
		if c.documents[uri] == state {
			state.open = false
			state.contentHash = [32]byte{}
			state.diagnostics = nil
			state.diagnosticsReceived = false
			state.allowUnversioned = false
			state.diagnosticsErr = err
			if !state.diagnosticsClosed {
				state.diagnosticsClosed = true
				close(state.diagnosticsReady)
			}
		}
		c.mu.Unlock()
		return DocumentVersion{}, err
	}
	return DocumentVersion{URI: uri, Version: version, epoch: epoch, serial: state.serial, workspaceGeneration: workspaceGeneration}, nil
}

// CloseDocument sends didClose and retains a small revision tombstone. The
// tombstone prevents a queued request from an older snapshot from reopening
// stale content after the client has closed the document.
func (c *Conn) CloseDocument(uri string, snapshotRevision uint64) error {
	c.workspaceLease.acquireWrite()
	defer c.workspaceLease.releaseWrite()
	err := c.closeDocument(uri, snapshotRevision)
	c.workspaceLease.invalidate()
	return err
}

// RefreshDiagnosticsAtRevision synchronizes a document and forces a fresh
// same-content didChange so the child publishes diagnostics for a new token.
// With a snapshot-scoped context, the refresh stays inside that shared source
// lease and changes only the child's per-document version.
func (c *Conn) RefreshDiagnosticsAtRevision(ctx context.Context, langID, uri string, content []byte, snapshotRevision uint64) (DocumentVersion, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if langID == "" || uri == "" {
		return DocumentVersion{}, ierrors.New(ierrors.ErrInvalidArgument, "nested.didChange", "language id and document URI are required")
	}
	if err := ctx.Err(); err != nil {
		return DocumentVersion{}, err
	}
	if lease, ok := workspaceLeaseFromContext(ctx); ok {
		if lease.conn != c {
			return DocumentVersion{}, ierrors.New(ierrors.ErrInvalidArgument, "nested.didChange", "workspace snapshot lease belongs to a different child connection")
		}
		if err := lease.beginUse(); err != nil {
			return DocumentVersion{}, err
		}
		defer lease.endUse()
		document, err := lease.document(uri, langID, content, snapshotRevision)
		if err != nil {
			return DocumentVersion{}, err
		}
		token, err := c.refreshDiagnosticsDocument(langID, document.URI, content, snapshotRevision, lease.generation, lease.epoch)
		if err == nil {
			lease.updateDocument(uri, token)
		}
		return token, err
	}

	if err := c.workspaceLease.acquireWriteContext(ctx); err != nil {
		return DocumentVersion{}, err
	}
	defer c.workspaceLease.releaseWrite()
	// Cancellation may race with the gate opening after its wait callback
	// wakes. Check again while holding the exclusive lease before changing the
	// child workspace or advancing a document version.
	if err := ctx.Err(); err != nil {
		return DocumentVersion{}, err
	}
	if c.closed.Load() {
		return DocumentVersion{}, ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}
	c.workspaceLease.invalidate()
	if _, err := c.syncDocumentAtRevision(langID, uri, content, snapshotRevision); err != nil {
		return DocumentVersion{}, err
	}
	return c.refreshDiagnosticsDocument(langID, uri, content, snapshotRevision, c.WorkspaceSnapshotGeneration(), c.readerEpoch.Load())
}

func (c *Conn) refreshDiagnosticsDocument(langID, uri string, content []byte, snapshotRevision, workspaceGeneration, epoch uint64) (DocumentVersion, error) {
	hash := sha256.Sum256(content)
	c.documentMu.Lock()
	defer c.documentMu.Unlock()
	c.mu.Lock()
	state := c.documents[uri]
	if c.closed.Load() {
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}
	if state == nil || !state.open || state.epoch != epoch || state.contentHash != hash ||
		state.langID != langID || c.workspaceGeneration != workspaceGeneration ||
		(snapshotRevision != 0 && (state.snapshotRevision != snapshotRevision || c.workspaceRevision != snapshotRevision)) {
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrContentModified, "nested.didChange", "document changed before diagnostics refresh")
	}
	if state.diagnosticsRefreshGeneration == workspaceGeneration {
		token := DocumentVersion{
			URI: uri, Version: state.version, epoch: state.epoch,
			serial: state.serial, workspaceGeneration: workspaceGeneration,
		}
		c.mu.Unlock()
		return token, nil
	}
	if state.version == int32(^uint32(0)>>1) {
		c.mu.Unlock()
		return DocumentVersion{}, ierrors.New(ierrors.ErrInvalidDocumentVersion, "nested.didChange", "document version exhausted")
	}
	if !state.diagnosticsClosed {
		state.diagnosticsErr = ierrors.New(ierrors.ErrContentModified, "nested.diagnostics", "document refreshed before diagnostics arrived")
		state.diagnosticsClosed = true
		close(state.diagnosticsReady)
	}
	c.nextDocumentGeneration++
	refreshed := &documentState{
		langID:                       state.langID,
		contentHash:                  state.contentHash,
		open:                         true,
		version:                      state.version + 1,
		epoch:                        epoch,
		serial:                       c.nextDocumentGeneration,
		snapshotRevision:             state.snapshotRevision,
		highestRevision:              state.highestRevision,
		diagnosticsRefreshGeneration: workspaceGeneration,
		allowUnversioned:             false,
		diagnosticsReady:             make(chan struct{}),
	}
	c.storeDocumentLocked(uri, refreshed)
	c.mu.Unlock()

	if err := c.Notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": refreshed.version},
		"contentChanges": []any{map[string]string{"text": string(content)}},
	}); err != nil {
		c.mu.Lock()
		if c.documents[uri] == refreshed {
			refreshed.diagnosticsErr = err
			refreshed.diagnosticsClosed = true
			close(refreshed.diagnosticsReady)
		}
		c.mu.Unlock()
		return DocumentVersion{}, err
	}
	return DocumentVersion{
		URI: uri, Version: refreshed.version, epoch: refreshed.epoch,
		serial: refreshed.serial, workspaceGeneration: workspaceGeneration,
	}, nil
}

// closeDocument sends didClose while the caller owns the workspace write
// lease. The supplied revision is the revision after this close was applied.
func (c *Conn) closeDocument(uri string, snapshotRevision uint64) error {
	if uri == "" {
		return ierrors.New(ierrors.ErrInvalidArgument, "nested.didClose", "document URI is required")
	}
	if c.closed.Load() {
		return ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}
	c.documentMu.Lock()
	defer c.documentMu.Unlock()
	c.mu.Lock()
	epoch := c.readerEpoch.Load()
	documentURI := uri
	state := c.documents[uri]
	if state == nil || !state.open || state.epoch != epoch {
		if aliasURI, aliasState := c.findDocumentURI(uri); aliasState != nil {
			documentURI, state = aliasURI, aliasState
		}
	}
	if snapshotRevision != 0 && snapshotRevision < c.workspaceRevision {
		current := c.workspaceRevision
		c.mu.Unlock()
		return ierrors.New(ierrors.ErrContentModified, "nested.didClose", fmt.Sprintf("snapshot revision %d is older than workspace revision %d", snapshotRevision, current))
	}
	if state != nil && snapshotRevision != 0 && snapshotRevision < state.highestRevision {
		current := state.highestRevision
		c.mu.Unlock()
		return ierrors.New(ierrors.ErrContentModified, "nested.didClose", fmt.Sprintf("snapshot revision %d is older than document revision %d", snapshotRevision, current))
	}
	wasOpen := state != nil && state.open && state.epoch == epoch
	if state == nil {
		state = &documentState{langID: c.cfg.Lang, epoch: epoch, diagnosticsClosed: true, diagnosticsReady: make(chan struct{})}
		close(state.diagnosticsReady)
		c.storeDocumentLocked(uri, state)
	}
	if state.highestRevision < snapshotRevision {
		state.highestRevision = snapshotRevision
	}
	if snapshotRevision > c.workspaceRevision {
		c.workspaceRevision = snapshotRevision
	}
	c.workspaceGeneration++
	state.snapshotRevision = snapshotRevision
	state.contentHash = [32]byte{}
	state.diagnostics = nil
	state.diagnosticsReceived = false
	state.open = false
	state.clientClosed = true
	state.epoch = epoch
	c.nextDocumentGeneration++
	state.serial = c.nextDocumentGeneration
	state.diagnosticsErr = ierrors.New(ierrors.ErrContentModified, "nested.didClose", "document closed before diagnostics completed")
	if !state.diagnosticsClosed {
		state.diagnosticsClosed = true
		close(state.diagnosticsReady)
	}
	c.mu.Unlock()
	if wasOpen {
		return c.Notify("textDocument/didClose", map[string]any{
			"textDocument": map[string]string{"uri": documentURI},
		})
	}
	return nil
}

// WaitForDiagnostics returns only a report for the exact synchronized
// document generation. A missing report is an error after the configured
// request timeout; it is never represented as a successful empty list.
func (c *Conn) WaitForDiagnostics(ctx context.Context, token DocumentVersion) ([]Diagnostic, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if lease, ok := workspaceLeaseFromContext(ctx); ok {
		if lease.conn != c {
			return nil, ierrors.New(ierrors.ErrInvalidArgument, "nested.diagnostics", "workspace snapshot lease belongs to a different child connection")
		}
		if err := lease.beginUse(); err != nil {
			return nil, err
		}
		defer lease.endUse()
	}
	timer := time.NewTimer(c.cfg.RequestTimeout)
	defer timer.Stop()
	for {
		c.mu.Lock()
		state := c.documents[token.URI]
		if !sameWorkspaceDocumentVersion(state, token, c.workspaceGeneration) {
			c.mu.Unlock()
			return nil, ierrors.New(ierrors.ErrContentModified, "nested.diagnostics", "document generation is no longer current")
		}
		ready := state.diagnosticsReady
		publishing := state.diagnosticsPublishing
		c.mu.Unlock()

		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, ierrors.New(ierrors.ErrTimeout, c.cfg.Name, fmt.Sprintf("diagnostics for %s version %d timed out after %s", token.URI, token.Version, c.cfg.RequestTimeout))
		}
		if publishing != nil {
			select {
			case <-publishing:
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timer.C:
				return nil, ierrors.New(ierrors.ErrTimeout, c.cfg.Name, fmt.Sprintf("diagnostics for %s version %d timed out after %s", token.URI, token.Version, c.cfg.RequestTimeout))
			}
		}

		c.mu.Lock()
		if state.diagnosticsErr != nil {
			err := state.diagnosticsErr
			c.mu.Unlock()
			return nil, err
		}
		if !sameWorkspaceDocumentVersion(c.documents[token.URI], token, c.workspaceGeneration) {
			c.mu.Unlock()
			return nil, ierrors.New(ierrors.ErrContentModified, "nested.diagnostics", "document changed while waiting for diagnostics")
		}
		// A replacement can begin after the channels above are captured. If
		// so, wait for its runtime invalidation callback and state commit too.
		if state.diagnosticsPublishing != nil {
			c.mu.Unlock()
			continue
		}
		if !state.diagnosticsClosed {
			c.mu.Unlock()
			return nil, fmt.Errorf("%s diagnostics completed without a report", c.cfg.Name)
		}
		result := append(make([]Diagnostic, 0, len(state.diagnostics)), state.diagnostics...)
		c.mu.Unlock()
		return result, nil
	}
}

func sameDocumentVersion(state *documentState, token DocumentVersion) bool {
	return state != nil && state.version == token.Version && state.epoch == token.epoch && state.serial == token.serial
}

func sameWorkspaceDocumentVersion(state *documentState, token DocumentVersion, workspaceGeneration uint64) bool {
	return sameDocumentVersion(state, token) && token.workspaceGeneration == workspaceGeneration
}

// SendRequestAtRevision synchronizes a source snapshot, issues a child
// request for that exact document generation, and validates the generation
// again after the response arrives. The RPC itself is deliberately performed
// without holding documentMu or mu, so unrelated documents can continue
// syncing and requests can run concurrently.
func (c *Conn) SendRequestAtRevision(ctx context.Context, langID, uri string, content []byte, snapshotRevision uint64, method string, params interface{}, parentRequestIDs ...json.RawMessage) (json.RawMessage, error) {
	result, _, err := c.sendRequestAtRevision(ctx, langID, uri, content, snapshotRevision, method, params, false, parentRequestIDs...)
	return result, err
}

// SendRequestAtRevisionWithEpoch returns the supervisor epoch that remained
// attached to the synchronized document for the successful request. A process
// replacement invalidates the document token before a result can be accepted.
func (c *Conn) SendRequestAtRevisionWithEpoch(ctx context.Context, langID, uri string, content []byte, snapshotRevision uint64, method string, params interface{}, parentRequestIDs ...json.RawMessage) (json.RawMessage, uint64, error) {
	return c.sendRequestAtRevision(ctx, langID, uri, content, snapshotRevision, method, params, true, parentRequestIDs...)
}

func (c *Conn) sendRequestAtRevision(ctx context.Context, langID, uri string, content []byte, snapshotRevision uint64, method string, params interface{}, requireReadyEpoch bool, parentRequestIDs ...json.RawMessage) (json.RawMessage, uint64, error) {
	if len(parentRequestIDs) > 0 && c.requestTiming != nil {
		ctx = withRequestTimingParentID(ctx, parentRequestIDs[0])
	}
	if lease, ok := workspaceLeaseFromContext(ctx); ok {
		if lease.conn != c {
			return nil, 0, ierrors.New(ierrors.ErrInvalidArgument, "nested.request", "workspace snapshot lease belongs to a different child connection")
		}
		if err := lease.beginUse(); err != nil {
			return nil, 0, err
		}
		defer lease.endUse()
		token, err := lease.document(uri, langID, content, snapshotRevision)
		if err != nil {
			return nil, 0, err
		}
		if requireReadyEpoch {
			return c.sendRequestForDocumentWithEpoch(ctx, token, method, params)
		}
		result, err := c.sendRequestForDocument(ctx, token, method, params)
		return result, 0, err
	}
	token, err := c.SyncDocumentAtRevision(langID, uri, content, snapshotRevision)
	if err != nil {
		return nil, 0, err
	}
	if requireReadyEpoch {
		return c.sendRequestForDocumentWithEpoch(ctx, token, method, params)
	}
	result, err := c.sendRequestForDocument(ctx, token, method, params)
	return result, 0, err
}

func (c *Conn) sendRequestForDocumentWithEpoch(ctx context.Context, token DocumentVersion, method string, params interface{}) (json.RawMessage, uint64, error) {
	epoch, err := c.readyEpochForDocument(token)
	if err != nil {
		return nil, 0, err
	}
	result, requestErr := c.SendRequest(ctx, method, params)
	currentEpoch, validationErr := c.readyEpochForDocument(token)
	if validationErr != nil {
		return nil, epoch, validationErr
	}
	if currentEpoch != epoch {
		return nil, epoch, ierrors.New(ierrors.ErrContentModified, "nested.request", "backend epoch changed while request was in flight")
	}
	if requestErr != nil {
		return nil, epoch, requestErr
	}
	return result, epoch, nil
}

func (c *Conn) sendRequestForDocument(ctx context.Context, token DocumentVersion, method string, params interface{}) (json.RawMessage, error) {
	if err := c.validateDocumentVersion(token); err != nil {
		return nil, err
	}
	result, err := c.SendRequest(ctx, method, params)
	if validationErr := c.validateDocumentVersion(token); validationErr != nil {
		return nil, validationErr
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Conn) readyEpochForDocument(token DocumentVersion) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.documents[token.URI]
	if !sameWorkspaceDocumentVersion(state, token, c.workspaceGeneration) {
		return 0, ierrors.New(ierrors.ErrContentModified, "nested.request", "document generation is no longer current")
	}
	if c.closed.Load() || c.sup == nil || !c.ready || c.exitHandled || c.readyReaderEpoch != token.epoch {
		return 0, ierrors.New(ierrors.ErrBackendUnavailable, "nested.request", "document belongs to a worker that is not ready")
	}
	epoch := c.readyBackendEpoch
	supState := c.sup.State()
	if (supState != supervisor.StateReady && supState != supervisor.StateDegraded) || c.sup.Epoch() != epoch {
		return 0, ierrors.New(ierrors.ErrBackendUnavailable, "nested.request", "worker readiness changed for document generation")
	}
	return epoch, nil
}

func (c *Conn) validateDocumentVersion(token DocumentVersion) error {
	c.mu.Lock()
	current := c.documents[token.URI]
	valid := sameWorkspaceDocumentVersion(current, token, c.workspaceGeneration)
	c.mu.Unlock()
	if !valid {
		return ierrors.New(ierrors.ErrContentModified, "nested.request", "document generation is no longer current")
	}
	return nil
}

func (c *Conn) receiveDiagnostics(epoch uint64, raw json.RawMessage) {
	var params publishDiagnosticsParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return
	}
	if params.URI == "" {
		return
	}
	if len(params.Diagnostics) == 0 || params.Diagnostics[0] != '[' {
		return
	}
	var wire []wireDiagnostic
	if err := json.Unmarshal(params.Diagnostics, &wire); err != nil {
		return
	}
	diagnostics := make([]Diagnostic, 0, len(wire))
	for _, item := range wire {
		if item.Range == nil || item.Range.Start == nil || item.Range.End == nil ||
			item.Range.Start.Line == nil || item.Range.Start.Character == nil ||
			item.Range.End.Line == nil || item.Range.End.Character == nil || item.Message == nil {
			return
		}
		diagnostic := Diagnostic{
			StartLine: *item.Range.Start.Line, StartChar: *item.Range.Start.Character,
			EndLine: *item.Range.End.Line, EndChar: *item.Range.End.Character,
			Source: item.Source, Message: *item.Message, Code: normalizeDiagnosticCode(item.Code),
		}
		if item.Severity != nil {
			diagnostic.Severity = *item.Severity
		}
		diagnostics = append(diagnostics, diagnostic)
	}

	c.diagnosticsMu.Lock()
	defer c.diagnosticsMu.Unlock()
	c.mu.Lock()
	if epoch != c.readerEpoch.Load() {
		c.mu.Unlock()
		return
	}
	documentURI, state := c.findDocumentURI(params.URI)
	if state == nil || state.epoch != epoch || !state.open {
		c.mu.Unlock()
		return
	}
	if params.Version != nil {
		if *params.Version != state.version {
			c.mu.Unlock()
			return
		}
	} else if !state.allowUnversioned && !c.cfg.AllowUnversionedDiagnostics {
		// LSP versions are optional, but after a document update an
		// unversioned report cannot be proven to belong to current text.
		c.mu.Unlock()
		return
	}
	changed := !state.diagnosticsReceived || !equalDiagnostics(state.diagnostics, diagnostics)
	handler := c.diagnosticsUpdateHandler
	var publishing chan struct{}
	if changed && handler != nil {
		publishing = make(chan struct{})
		state.diagnosticsPublishing = publishing
	}
	c.mu.Unlock()
	if changed && handler != nil {
		handler(documentURI)
	}
	// Do not expose a replacement until its runtime invalidation callback has
	// completed. WaitForDiagnostics observes diagnosticsPublishing so a pull
	// cannot return a stale cache entry or the previous same-version report in
	// the callback-to-commit window.
	c.mu.Lock()
	if current := c.documents[documentURI]; current == state && state.open && state.epoch == epoch {
		state.diagnostics = diagnostics
		state.diagnosticsReceived = true
		if !state.diagnosticsClosed {
			state.diagnosticsClosed = true
			close(state.diagnosticsReady)
		}
	}
	if publishing != nil {
		if state.diagnosticsPublishing == publishing {
			state.diagnosticsPublishing = nil
		}
		close(publishing)
	}
	c.mu.Unlock()
}

func equalDiagnostics(left, right []Diagnostic) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func normalizeDiagnosticCode(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		return strconv.FormatInt(number, 10)
	}
	return strings.TrimSpace(string(raw))
}

// ConvertDiagnosticPositions converts child-LSP UTF-16 ranges into the
// negotiated parent position encoding. Nested clients currently leave child
// position encoding at the LSP default (UTF-16).
func ConvertDiagnosticPositions(content []byte, diagnostics []Diagnostic, targetEncoding int) ([]Diagnostic, error) {
	target := position.UTF16
	switch targetEncoding {
	case int(position.UTF8):
		target = position.UTF8
	case int(position.UTF32):
		target = position.UTF32
	}
	idx := position.NewIndex(content, position.UTF16)
	result := make([]Diagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		start, err := idx.ConvertPosition(content, position.LSPPosition{Line: diagnostic.StartLine, Character: diagnostic.StartChar}, position.UTF16, target)
		if err != nil {
			return nil, fmt.Errorf("convert diagnostic start: %w", err)
		}
		end, err := idx.ConvertPosition(content, position.LSPPosition{Line: diagnostic.EndLine, Character: diagnostic.EndChar}, position.UTF16, target)
		if err != nil {
			return nil, fmt.Errorf("convert diagnostic end: %w", err)
		}
		diagnostic.StartLine, diagnostic.StartChar = start.Line, start.Character
		diagnostic.EndLine, diagnostic.EndChar = end.Line, end.Character
		result = append(result, diagnostic)
	}
	return result, nil
}

// invalidateDocumentsLocked wakes all waiters before dropping per-process
// document state. The caller holds c.mu and the document serialization lock.
func (c *Conn) invalidateDocumentsLocked(err error) {
	c.workspaceGeneration++
	for _, state := range c.documents {
		if !state.diagnosticsClosed {
			state.diagnosticsErr = err
			state.diagnosticsClosed = true
			close(state.diagnosticsReady)
		}
	}
	c.documents = make(map[string]*documentState)
	c.canonicalDocuments = make(map[string]map[string]struct{})
}

// invalidateDocumentsForRestartLocked drops child-open content while keeping
// per-URI revision tombstones. In-flight requests captured before the restart
// must not reopen older snapshots into the replacement worker.
func (c *Conn) invalidateDocumentsForRestartLocked(err error, epoch uint64) {
	c.workspaceGeneration++
	for _, state := range c.documents {
		state.open = false
		state.contentHash = [32]byte{}
		state.diagnostics = nil
		state.epoch = epoch
		c.nextDocumentGeneration++
		state.serial = c.nextDocumentGeneration
		state.diagnosticsErr = err
		if !state.diagnosticsClosed {
			state.diagnosticsClosed = true
			close(state.diagnosticsReady)
		}
	}
}
