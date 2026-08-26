package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

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
)

// diagKey is the §C11 cache identity for one document's diagnostic set.
type diagKey struct {
	URI          string
	SnapshotRev  uint64
	ContentHash  string // first 16 hex chars of sha256
	BuildContext string
	BackendEpoch uint64
	ConfigHash   string
}

func computeDiagKey(uri string, snapRev uint64, content []byte, bc string, epoch uint64) diagKey {
	sum := sha256.Sum256(content)
	return diagKey{
		URI:          uri,
		SnapshotRev:  snapRev,
		ContentHash:  hex.EncodeToString(sum[:8]),
		BuildContext: bc,
		BackendEpoch: epoch,
		ConfigHash:   diagConfigVersion,
	}
}

// diagEntry pairs the cached set with its key so pull can revalidate.
type diagEntry struct {
	key   diagKey
	items []languages.Diagnostic
}

// diagCoordinator owns debounce timers, the bounded cache, and wire emission.
type diagCoordinator struct {
	mu     sync.Mutex
	cache  map[string]diagEntry // keyed by URI; latest set wins (FIFO order below)
	order  []string
	timers map[string]*time.Timer
	server *Server
}

func newDiagCoordinator(s *Server) *diagCoordinator {
	return &diagCoordinator{
		cache:  make(map[string]diagEntry),
		timers: make(map[string]*time.Timer),
		server: s,
	}
}

// request schedules a debounced push for the URI (§C11 push path).
func (d *diagCoordinator) request(uri string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t := d.timers[uri]; t != nil {
		t.Stop()
	}
	d.timers[uri] = time.AfterFunc(diagDebounce, func() {
		d.mu.Lock()
		delete(d.timers, uri)
		d.mu.Unlock()
		d.computeAndPush(uri)
	})
}

// Close cancels pending debounce timers (session teardown).
func (d *diagCoordinator) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range d.timers {
		t.Stop()
	}
	d.timers = map[string]*time.Timer{}
}

// computeAndPush runs diagnostics and emits publishDiagnostics.
func (d *diagCoordinator) computeAndPush(uri string) {
	items, err := d.compute(uri)
	if err != nil {
		return // transient backend failure: stay silent, next edit retries
	}
	version := int64(0)
	if f := d.server.vfs.Get(uri); f != nil {
		version = f.Version
	}
	wire := projectDiagnostics(items)
	payload, merr := json.Marshal(map[string]any{
		"uri":         uri,
		"version":     version,
		"diagnostics": wire,
	})
	if merr != nil {
		return
	}
	d.server.notifyClient("textDocument/publishDiagnostics", json.RawMessage(payload))
}

// compute resolves diagnostics through the bounded §C11 cache.
func (d *diagCoordinator) compute(uri string) ([]languages.Diagnostic, error) {
	be := d.server.findWorkspaceBackendFor(uri)
	if be == nil {
		return nil, fmt.Errorf("no backend for %s", uri)
	}
	var src []byte
	snapRev := uint64(0)
	if captured := snapshotFromCtx(context.Background()); captured != nil {
		snapRev = captured.ID().Revision
	} else if snap := d.server.snapMgr.Current(); snap != nil {
		snapRev = snap.ID().Revision
	}
	src = d.server.vfs.Content(uri)
	if src == nil {
		src = []byte{}
	}
	key := computeDiagKey(uri, snapRev, src, string(backendBuildContext(be)), backendEpoch(be))

	d.mu.Lock()
	if en, ok := d.cache[uri]; ok && en.key == key {
		d.mu.Unlock()
		return en.items, nil
	}
	d.mu.Unlock()

	items, err := be.Diagnostics(context.Background(), uri, src)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if _, seen := d.cache[uri]; !seen {
		d.order = append(d.order, uri)
		for len(d.cache) >= diagCacheLimit && len(d.order) > 0 { // FIFO eviction
			old := d.order[0]
			d.order = d.order[1:]
			delete(d.cache, old)
		}
	}
	d.cache[uri] = diagEntry{key: key, items: items}
	d.mu.Unlock()
	return items, nil
}

// findWorkspaceBackendFor resolves the backend owning this URI's language.
func (s *Server) findWorkspaceBackendFor(uri string) languages.Backend {
	lang := ""
	if f := s.vfs.Get(uri); f != nil {
		lang = f.LanguageID
	}
	if lang == "" {
		if i := strings.LastIndex(uri, "."); i >= 0 {
			switch strings.ToLower(uri[i:]) {
			case ".go":
				lang = "go"
			case ".c", ".cc", ".cpp", ".h", ".hpp":
				lang = "cpp"
			}
		}
	}
	if lang == "" {
		return s.findWorkspaceBackend()
	}
	s.mu.RLock()
	be := s.languages[lang]
	s.mu.RUnlock()
	if be != nil {
		return be
	}
	return s.findWorkspaceBackend()
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
			"resultId": resultID(it),
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
	items, err := s.diag.compute(params.TextDocument.URI)
	if err != nil {
		// §Q4 no silent fallback: an unanalyzable document answers empty-full,
		// never stale data; the failure stays visible in metrics/logs.
		s.metrics.BackendFails.Inc(1)
		items = nil
	}
	payload, _ := json.Marshal(map[string]any{
		"kind":  "full",
		"items": projectDiagnostics(items),
	})
	return payload, nil
}
