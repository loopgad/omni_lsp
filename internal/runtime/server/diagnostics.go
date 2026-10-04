package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// §C11/§I17/§I18 diagnostic pipeline. Push (publishDiagnostics) is debounced
// per URI; pull (textDocument/diagnostic) shares the same bounded cache. The
// cache key carries every input the spec mandates: snapshot revision, content
// hash, BuildContextID, backend epoch, and a diagnostics-configuration hash.

const (
	diagDebounce      = 150 * time.Millisecond
	diagCacheLimit    = 512 // §K0: every cache is bounded (FIFO)
	diagConfigVersion = "v1"
	diagRetryLimit    = 3
)

var errDiagnosticDocumentClosed = errors.New("diagnostic document is closed")

// diagKey is the §C11 cache identity for one document's diagnostic set.
type diagKey struct {
	URI                   string
	SnapshotRev           uint64
	ContentHash           string // first 16 hex chars of sha256
	BuildContext          string
	BackendEpoch          uint64
	WorkspaceGeneration   uint64
	DiagnosticsGeneration uint64
	ConfigHash            string
}

func computeDiagKey(uri string, snapRev uint64, content []byte, bc string, epoch, workspaceGeneration, diagnosticsGeneration uint64) diagKey {
	sum := sha256.Sum256(content)
	return diagKey{
		URI:                   uri,
		SnapshotRev:           snapRev,
		ContentHash:           hex.EncodeToString(sum[:8]),
		BuildContext:          bc,
		BackendEpoch:          epoch,
		WorkspaceGeneration:   workspaceGeneration,
		DiagnosticsGeneration: diagnosticsGeneration,
		ConfigHash:            diagConfigVersion,
	}
}

// diagEntry pairs the cached set with its key so pull can revalidate.
type diagEntry struct {
	key     diagKey
	items   []languages.Diagnostic
	version int64
}

// diagCoordinator owns debounce timers, the bounded cache, and wire emission.
type diagCoordinator struct {
	mu       sync.Mutex
	cache    map[string]diagEntry // keyed by URI; latest set wins (FIFO order below)
	order    []string
	updates  map[string]uint64
	timers   map[string]*time.Timer
	runs     map[string]*diagRun
	pullRuns map[string]map[*diagRun]struct{}
	server   *Server
	closed   bool
}

type diagRun struct {
	cancel context.CancelFunc
}

func newDiagCoordinator(s *Server) *diagCoordinator {
	return &diagCoordinator{
		cache:    make(map[string]diagEntry),
		updates:  make(map[string]uint64),
		timers:   make(map[string]*time.Timer),
		runs:     make(map[string]*diagRun),
		pullRuns: make(map[string]map[*diagRun]struct{}),
		server:   s,
	}
}

// request schedules a debounced push for the URI (§C11 push path).
func (d *diagCoordinator) request(uri string) {
	d.schedule(uri, false)
}

// diagnosticsUpdated invalidates cached results and schedules a push when a
// nested server publishes a newer complete set for the current document.
func (d *diagCoordinator) diagnosticsUpdated(uri string) {
	d.schedule(uri, true)
}

func (d *diagCoordinator) schedule(uri string, childUpdate bool) {
	uri = canonicalDocumentURI(uri)
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	if childUpdate {
		d.updates[uri]++
		delete(d.cache, uri)
		filtered := d.order[:0]
		for _, queued := range d.order {
			if queued != uri {
				filtered = append(filtered, queued)
			}
		}
		d.order = filtered
	}
	defer d.mu.Unlock()
	if t := d.timers[uri]; t != nil {
		t.Stop()
	}
	if run := d.runs[uri]; run != nil {
		run.cancel()
		delete(d.runs, uri)
	}
	ready := make(chan struct{})
	var timer *time.Timer
	timer = time.AfterFunc(diagDebounce, func() {
		<-ready
		d.mu.Lock()
		if d.timers[uri] != timer {
			d.mu.Unlock()
			return
		}
		delete(d.timers, uri)
		ctx, cancel := context.WithCancel(context.Background())
		run := &diagRun{cancel: cancel}
		d.runs[uri] = run
		d.mu.Unlock()
		d.computeAndPush(ctx, uri)
		cancel()
		d.mu.Lock()
		if d.runs[uri] == run {
			delete(d.runs, uri)
		}
		d.mu.Unlock()
	})
	d.timers[uri] = timer
	close(ready)
}

// didClose cancels pending and in-flight push work and removes the bounded
// per-URI diagnostic cache before the VFS drops the editor document.
func (d *diagCoordinator) didClose(uri string) {
	d.didCloseDocument(uri, nil)
}

// didCloseDocument serializes the VFS close with diagnostic cache commits.
// A computation either commits before this transition and is cleared here, or
// observes the closed VFS entry and cannot repopulate the cache afterward.
func (d *diagCoordinator) didCloseDocument(uri string, closeDocument func()) {
	uri = canonicalDocumentURI(uri)
	d.mu.Lock()
	defer d.mu.Unlock()
	if timer := d.timers[uri]; timer != nil {
		timer.Stop()
		delete(d.timers, uri)
	}
	if run := d.runs[uri]; run != nil {
		run.cancel()
		delete(d.runs, uri)
	}
	for run := range d.pullRuns[uri] {
		run.cancel()
		delete(d.pullRuns[uri], run)
	}
	delete(d.pullRuns, uri)
	if closeDocument != nil {
		closeDocument()
	}
	delete(d.cache, uri)
	delete(d.updates, uri)
	filtered := d.order[:0]
	for _, queued := range d.order {
		if queued != uri {
			filtered = append(filtered, queued)
		}
	}
	d.order = filtered
}

// Close cancels pending debounce timers (session teardown).
func (d *diagCoordinator) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	for _, t := range d.timers {
		t.Stop()
	}
	d.timers = map[string]*time.Timer{}
	for _, run := range d.runs {
		run.cancel()
	}
	d.runs = map[string]*diagRun{}
	for _, runs := range d.pullRuns {
		for run := range runs {
			run.cancel()
		}
	}
	d.pullRuns = map[string]map[*diagRun]struct{}{}
}

// computeAndPush runs diagnostics and emits publishDiagnostics.
func (d *diagCoordinator) computeAndPush(ctx context.Context, uri string) {
	uri = canonicalDocumentURI(uri)
	items, err := d.compute(ctx, uri)
	if err != nil || ctx.Err() != nil {
		if err != nil && ctx.Err() == nil {
			d.server.recordEvidence(ctx, "textDocument/publishDiagnostics", uri, identity.ResultUnavailable, identity.CompletenessUnknown, nil, []string{err.Error()})
		}
		return
	}
	be := d.server.findWorkspaceBackendFor(uri)
	if be == nil {
		return
	}
	if currentKey := d.currentKey(uri, be); currentKey != (diagKey{}) {
		d.mu.Lock()
		cached, ok := d.cache[uri]
		d.mu.Unlock()
		if !ok || cached.key != currentKey {
			return
		}
	}
	d.mu.Lock()
	cached := d.cache[uri]
	d.mu.Unlock()
	version := cached.version
	wire := projectDiagnostics(items)
	displayURI := uri
	if file := d.server.vfs.Get(uri); file != nil {
		displayURI = file.URI
	}
	payload, merr := json.Marshal(map[string]any{
		"uri":         displayURI,
		"version":     version,
		"diagnostics": wire,
	})
	if merr != nil {
		return
	}
	if currentKey := d.currentKey(uri, be); currentKey != cached.key {
		return
	}
	d.server.notifyClient("textDocument/publishDiagnostics", json.RawMessage(payload))
}

func (d *diagCoordinator) currentKey(uri string, be languages.Backend) diagKey {
	uri = canonicalDocumentURI(uri)
	src := d.server.vfs.Content(uri)
	if src == nil {
		src = []byte{}
	}
	rev := uint64(0)
	if snap := d.server.snapMgr.Current(); snap != nil {
		rev = snap.ID().Revision
	}
	d.mu.Lock()
	diagnosticsGeneration := d.updates[uri]
	d.mu.Unlock()
	return computeDiagKey(uri, rev, src, string(backendBuildContext(be)), backendEpoch(be), backendWorkspaceGeneration(be), diagnosticsGeneration)
}

// compute resolves diagnostics through the bounded §C11 cache.
func (d *diagCoordinator) compute(ctx context.Context, uri string) (result []languages.Diagnostic, resultErr error) {
	uri = canonicalDocumentURI(uri)
	if d.server.vfs.Get(uri) == nil {
		return nil, errDiagnosticDocumentClosed
	}
	be := d.server.findWorkspaceBackendFor(uri)
	if be == nil {
		return nil, fmt.Errorf("no backend for %s", uri)
	}
	captured := snapshotFromCtx(ctx)
	if captured == nil && d.server.snapMgr != nil {
		captured = d.server.snapMgr.Current()
	}
	var src []byte
	snapRev := uint64(0)
	if captured != nil {
		snapRev = captured.ID().Revision
		doc := captured.Document(uri)
		if doc == nil {
			return nil, ierrors.New(ierrors.ErrContentModified, "server.diagnostics", "requested document is absent from the captured workspace snapshot")
		}
		src = doc.Content
	} else {
		src = d.server.vfs.Content(uri)
	}
	if src == nil {
		src = []byte{}
	}
	leaseCtx, finishSnapshot, err := d.server.beginBackendWorkspaceSnapshot(ctx, be, captured)
	if err != nil {
		return nil, err
	}
	defer func() {
		if finishSnapshot != nil {
			if finishErr := finishSnapshot(); finishErr != nil {
				result, resultErr = nil, finishErr
			}
		}
	}()
	workspaceGeneration := backendWorkspaceGeneration(be)
	epoch := backendEpoch(be)
	buildContext := string(backendBuildContext(be))

	d.mu.Lock()
	diagnosticsGeneration := d.updates[uri]
	d.mu.Unlock()
	key := computeDiagKey(uri, snapRev, src, buildContext, epoch, workspaceGeneration, diagnosticsGeneration)

	d.mu.Lock()
	if en, ok := d.cache[uri]; ok && en.key == key {
		d.mu.Unlock()
		return en.items, nil
	}
	d.mu.Unlock()

	// Both in-process overlays and nested diagnostic refresh must consume the
	// captured workspace context. Nested refresh can update its diagnostic
	// version inside this lease without changing the source generation.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var items []languages.Diagnostic
		if encoded, ok := be.(languages.EncodedDiagnosticsProvider); ok {
			items, err = encoded.DiagnosticsWithEncoding(leaseCtx, uri, src, snapRev, d.server.negotiatedEncodingInt())
		} else {
			items, err = be.Diagnostics(leaseCtx, uri, src)
		}
		if err != nil {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if d.server.vfs.Get(uri) == nil {
			return nil, errDiagnosticDocumentClosed
		}
		if string(backendBuildContext(be)) != buildContext || backendEpoch(be) != epoch {
			return nil, ierrors.New(ierrors.ErrContentModified, "server.diagnostics", "backend context changed while diagnostics were computed")
		}
		if backendWorkspaceGeneration(be) != workspaceGeneration {
			return nil, ierrors.New(ierrors.ErrContentModified, "server.diagnostics", "child workspace changed while diagnostics were computed")
		}
		if string(backendBuildContext(be)) != buildContext || backendEpoch(be) != epoch {
			return nil, ierrors.New(ierrors.ErrContentModified, "server.diagnostics", "backend context changed before diagnostics could be cached")
		}
		if backendWorkspaceGeneration(be) != workspaceGeneration {
			return nil, ierrors.New(ierrors.ErrContentModified, "server.diagnostics", "child workspace changed before diagnostics could be cached")
		}

		d.mu.Lock()
		if ctx.Err() != nil {
			d.mu.Unlock()
			return nil, ctx.Err()
		}
		if d.server.vfs.Get(uri) == nil {
			d.mu.Unlock()
			return nil, errDiagnosticDocumentClosed
		}
		if d.updates[uri] != diagnosticsGeneration {
			// A child report can replace the data after Diagnostics has copied
			// its result but before this commit. The result is then ambiguous:
			// fetch again under the new generation instead of caching it as new.
			diagnosticsGeneration = d.updates[uri]
			d.mu.Unlock()
			if attempt+1 == diagRetryLimit {
				return nil, ierrors.New(ierrors.ErrContentModified, "server.diagnostics", "child diagnostics kept changing while this report was computed")
			}
			continue
		}
		if _, seen := d.cache[uri]; !seen {
			d.order = append(d.order, uri)
			for len(d.cache) >= diagCacheLimit && len(d.order) > 0 { // FIFO eviction
				old := d.order[0]
				d.order = d.order[1:]
				delete(d.cache, old)
			}
		}
		version := int64(0)
		if captured := snapshotFromCtx(ctx); captured != nil {
			if doc := captured.Document(uri); doc != nil {
				version = doc.Version
			}
		} else if f := d.server.vfs.Get(uri); f != nil {
			version = f.Version
		}
		resultKey := computeDiagKey(uri, snapRev, src, buildContext, epoch, workspaceGeneration, diagnosticsGeneration)
		d.cache[uri] = diagEntry{key: resultKey, items: items, version: version}
		d.mu.Unlock()
		return items, nil
	}
}

// computePull tracks request-scoped diagnostic computation so didClose can
// cancel it and guarantee that a pull started for a closing document cannot
// refill the cache after the close transition.
func (d *diagCoordinator) computePull(parent context.Context, uri string) ([]languages.Diagnostic, error) {
	uri = canonicalDocumentURI(uri)
	ctx, cancel := context.WithCancel(parent)
	run := &diagRun{cancel: cancel}
	d.mu.Lock()
	if d.server.vfs.Get(uri) == nil {
		d.mu.Unlock()
		cancel()
		return nil, errDiagnosticDocumentClosed
	}
	if d.pullRuns[uri] == nil {
		d.pullRuns[uri] = make(map[*diagRun]struct{})
	}
	d.pullRuns[uri][run] = struct{}{}
	d.mu.Unlock()
	defer func() {
		cancel()
		d.mu.Lock()
		delete(d.pullRuns[uri], run)
		if len(d.pullRuns[uri]) == 0 {
			delete(d.pullRuns, uri)
		}
		d.mu.Unlock()
	}()

	items, err := d.compute(ctx, uri)
	if err != nil {
		if errors.Is(err, context.Canceled) && d.server.vfs.Get(uri) == nil {
			return nil, errDiagnosticDocumentClosed
		}
		return nil, err
	}
	if d.server.vfs.Get(uri) == nil {
		return nil, errDiagnosticDocumentClosed
	}
	return items, nil
}

// findWorkspaceBackendFor resolves diagnostics through the same URI-to-backend
// path used by semantic requests. In particular, language IDs such as
// javascriptreact and typescriptreact are routed by their file extension to
// the TypeScript backend. An unresolved URI must not fall through to an
// arbitrary registered backend.
func (s *Server) findWorkspaceBackendFor(uri string) languages.Backend {
	be, err := s.resolveBackend(uri)
	if err != nil {
		return nil
	}
	return be
}

// backendEpoch reports the backend's restart epoch when it exposes one
// (nested bridges); in-process backends are stable at 0.
func backendEpoch(be languages.Backend) uint64 {
	type epoker interface{ SupervisorEpoch() uint64 }
	if e, ok := be.(epoker); ok {
		return e.SupervisorEpoch()
	}
	return 0
}

// resultID derives the §I18 stable identity from semantic fields — source,
// code, message, and the intra-document range. Two diagnostics with the same
// semantic content keep the same ID across line shifts, reducing editor flicker.
func resultID(item languages.Diagnostic) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%d:%d:%d:%d",
		item.Source, item.Code, item.Message,
		item.StartLine, item.StartChar, item.EndLine, item.EndChar)))
	return hex.EncodeToString(h[:12])
}

func diagnosticsResultID(items []languages.Diagnostic) string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, resultID(item))
	}
	sort.Strings(ids)
	h := sha256.New()
	for _, id := range ids {
		_, _ = h.Write([]byte(id))
	}
	return hex.EncodeToString(h.Sum(nil)[:12])
}

// projectDiagnostics converts internal diagnostics to the LSP wire shape.
func projectDiagnostics(items []languages.Diagnostic) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		entry := map[string]any{
			"range": map[string]any{
				"start": map[string]any{"line": it.StartLine, "character": it.StartChar},
				"end":   map[string]any{"line": it.EndLine, "character": it.EndChar},
			},
			"severity": it.Severity,
			"source":   it.Source,
			"message":  it.Message,
			"code":     it.Code,
		}
		out = append(out, entry)
	}
	return out
}

// handlePullDiagnostics serves textDocument/diagnostic (§C11 pull path).
func (s *Server) handlePullDiagnostics(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid diagnostic params: %w", err)
	}
	items, err := s.diag.computePull(ctx, params.TextDocument.URI)
	if err != nil {
		if ierrors.IsKind(err, ierrors.ErrContentModified) {
			return nil, err
		}
		// §Q4 no silent fallback: an unanalyzable document answers empty-full,
		// never stale data; the failure stays visible in metrics/logs.
		if !errors.Is(err, errDiagnosticDocumentClosed) && !errors.Is(err, context.Canceled) {
			s.metrics.BackendFails.Inc(1)
		}
		items = nil
	}
	payload, _ := json.Marshal(map[string]any{
		"kind":     "full",
		"resultId": diagnosticsResultID(items),
		"items":    projectDiagnostics(items),
	})
	return payload, nil
}
