package nested

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"sort"
	"sync"
	"sync/atomic"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

// SnapshotDocument is one document in an immutable workspace snapshot.
type SnapshotDocument struct {
	URI     string
	LangID  string
	Content []byte
}

type workspaceSnapshotMarker struct {
	valid       bool
	revision    uint64
	generation  uint64
	epoch       uint64
	fingerprint [32]byte
}

type workspaceSnapshotContextKey struct{}

type workspaceLeaseDocument struct {
	langID      string
	contentHash [32]byte
	token       DocumentVersion
}

type workspaceRequestLease struct {
	conn       *Conn
	revision   uint64
	generation uint64
	epoch      uint64
	active     atomic.Bool
	lifeMu     sync.RWMutex
	mu         sync.RWMutex
	documents  map[string]workspaceLeaseDocument
}

func (l *workspaceRequestLease) beginUse() error {
	l.lifeMu.RLock()
	if !l.active.Load() {
		l.lifeMu.RUnlock()
		return ierrors.New(ierrors.ErrContentModified, "nested.workspace", "workspace snapshot lease is no longer active")
	}
	return nil
}

func (l *workspaceRequestLease) endUse() { l.lifeMu.RUnlock() }

func (l *workspaceRequestLease) end() {
	l.lifeMu.Lock()
	l.active.Store(false)
	l.lifeMu.Unlock()
}

func (l *workspaceRequestLease) document(uri, langID string, content []byte, revision uint64) (DocumentVersion, error) {
	if l == nil || !l.active.Load() {
		return DocumentVersion{}, ierrors.New(ierrors.ErrContentModified, "nested.workspace", "workspace snapshot lease is no longer active")
	}
	if revision != l.revision {
		return DocumentVersion{}, ierrors.New(ierrors.ErrContentModified, "nested.workspace", "request revision does not match its workspace snapshot lease")
	}
	key := workspaceDocumentKey(uri)
	l.mu.RLock()
	doc, ok := l.documents[key]
	l.mu.RUnlock()
	if !ok || doc.langID != langID || doc.contentHash != sha256.Sum256(content) {
		return DocumentVersion{}, ierrors.New(ierrors.ErrContentModified, "nested.workspace", "request document does not match its workspace snapshot lease")
	}
	return doc.token, nil
}

func (l *workspaceRequestLease) updateDocument(uri string, token DocumentVersion) {
	l.mu.Lock()
	key := workspaceDocumentKey(uri)
	if doc, ok := l.documents[key]; ok {
		doc.token = token
		l.documents[key] = doc
	}
	l.mu.Unlock()
}

// workspaceDocumentKey uses the URI package's identity for parseable URIs,
// while keeping non-URI and malformed opaque identifiers byte-for-byte intact.
func workspaceDocumentKey(raw string) string {
	parsed, err := uri.Parse(raw)
	if err != nil {
		return raw
	}
	return parsed.Canonical()
}

// workspaceLeaseGate lets same-revision queries share a stable child
// workspace while mutations wait until those queries finish.
type workspaceLeaseGate struct {
	mu             sync.Mutex
	cond           *sync.Cond
	readers        int
	writer         bool
	waitingWriters int
	marker         workspaceSnapshotMarker
}

func newWorkspaceLeaseGate() *workspaceLeaseGate {
	g := &workspaceLeaseGate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *workspaceLeaseGate) acquireWrite() {
	_ = g.acquireWriteContext(context.Background())
}

// acquireWriteContext waits for exclusive workspace access and returns when
// the caller's context is canceled. A canceled writer is removed from the
// queue so it cannot mutate the child after a later reader releases its lease.
func (g *workspaceLeaseGate) acquireWriteContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	g.waitingWriters++
	waiting := true
	stopWake := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer stopWake()
	defer func() {
		if waiting {
			g.waitingWriters--
			g.cond.Broadcast()
		}
		g.mu.Unlock()
	}()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !g.writer && g.readers == 0 {
			g.waitingWriters--
			waiting = false
			g.writer = true
			g.cond.Broadcast()
			return nil
		}
		g.cond.Wait()
	}
}

func (g *workspaceLeaseGate) releaseWrite() {
	g.mu.Lock()
	g.writer = false
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *workspaceLeaseGate) invalidate() {
	g.mu.Lock()
	g.marker.valid = false
	g.mu.Unlock()
}

// acquireSnapshot returns with either an exclusive reconciliation slot or a
// shared lease for an already reconciled snapshot. Context cancellation wakes
// callers waiting behind active queries or another reconciliation.
func (g *workspaceLeaseGate) acquireSnapshot(ctx context.Context, revision uint64, fingerprint [32]byte) (writer bool, generation, epoch uint64, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	g.waitingWriters++
	waiting := true
	stopWake := context.AfterFunc(ctx, func() {
		g.mu.Lock()
		g.cond.Broadcast()
		g.mu.Unlock()
	})
	defer stopWake()
	defer func() {
		if waiting {
			g.waitingWriters--
			g.cond.Broadcast()
		}
		g.mu.Unlock()
	}()

	for {
		if err := ctx.Err(); err != nil {
			return false, 0, 0, err
		}
		if !g.writer {
			if g.marker.valid && g.marker.revision == revision {
				if g.marker.fingerprint != fingerprint {
					return false, 0, 0, ierrors.New(ierrors.ErrContentModified, "nested.workspace", "different workspace content has the same snapshot revision")
				}
				if g.waitingWriters == 1 {
					g.readers++
					waiting = false
					g.waitingWriters--
					g.cond.Broadcast()
					return false, g.marker.generation, g.marker.epoch, nil
				}
			}
			if g.readers == 0 {
				g.writer = true
				waiting = false
				g.waitingWriters--
				g.cond.Broadcast()
				return true, 0, 0, nil
			}
		}
		g.cond.Wait()
	}
}

// publishSnapshot converts the current exclusive reconciliation slot into a
// shared query lease without opening a gap where another mutation can enter.
func (g *workspaceLeaseGate) publishSnapshot(revision, generation, epoch uint64, fingerprint [32]byte) {
	g.mu.Lock()
	g.marker = workspaceSnapshotMarker{
		valid: true, revision: revision, generation: generation,
		epoch: epoch, fingerprint: fingerprint,
	}
	g.writer = false
	g.readers++
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *workspaceLeaseGate) releaseRead() {
	g.mu.Lock()
	g.readers--
	g.cond.Broadcast()
	g.mu.Unlock()
}

// BeginWorkspaceSnapshot reconciles every desired document with the child,
// closes child-open documents absent from the snapshot, and returns a derived
// context carrying the synchronized document tokens. The caller holds the
// lease through its backend query and then calls finish.
func (c *Conn) BeginWorkspaceSnapshot(ctx context.Context, snapshotRevision uint64, docs []SnapshotDocument) (leaseCtx context.Context, finish func() error, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.closed.Load() {
		return nil, nil, ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}
	snapshot, fingerprint, err := prepareWorkspaceSnapshot(docs)
	if err != nil {
		return nil, nil, err
	}

	writer, generation, epoch, err := c.workspaceLease.acquireSnapshot(ctx, snapshotRevision, fingerprint)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		if writer {
			c.workspaceLease.releaseWrite()
		} else {
			c.workspaceLease.releaseRead()
		}
		return nil, nil, err
	}
	if c.closed.Load() {
		if writer {
			c.workspaceLease.releaseWrite()
		} else {
			c.workspaceLease.releaseRead()
		}
		return nil, nil, ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
	}

	if writer {
		complete := false
		defer func() {
			if !complete {
				c.workspaceLease.releaseWrite()
			}
		}()

		c.mu.Lock()
		currentRevision := c.workspaceRevision
		startingGeneration := c.workspaceGeneration
		c.mu.Unlock()
		if snapshotRevision < currentRevision {
			return nil, nil, ierrors.New(ierrors.ErrContentModified, "nested.workspace", "snapshot revision is older than the child workspace")
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		c.workspaceLease.invalidate()

		for _, doc := range snapshot {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if _, err := c.syncDocumentAtRevision(doc.LangID, doc.URI, doc.Content, snapshotRevision); err != nil {
				return nil, nil, err
			}
		}

		c.mu.Lock()
		openURIs := make([]string, 0, len(c.documents))
		epoch = c.readerEpoch.Load()
		for uri, state := range c.documents {
			if state.open && state.epoch == epoch {
				if !snapshotContainsURI(snapshot, uri) {
					openURIs = append(openURIs, uri)
				}
			}
		}
		c.mu.Unlock()
		sort.Strings(openURIs)
		for _, uri := range openURIs {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if err := c.closeDocument(uri, snapshotRevision); err != nil {
				return nil, nil, err
			}
		}

		c.mu.Lock()
		if snapshotRevision > c.workspaceRevision {
			c.workspaceRevision = snapshotRevision
			if c.workspaceGeneration == startingGeneration {
				c.workspaceGeneration++
			}
		}
		generation = c.workspaceGeneration
		epoch = c.readerEpoch.Load()
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if c.closed.Load() {
			return nil, nil, ierrors.New(ierrors.ErrBackendUnavailable, c.cfg.Name, "backend closed")
		}

		c.workspaceLease.publishSnapshot(snapshotRevision, generation, epoch, fingerprint)
		complete = true
	}

	lease, err := c.captureWorkspaceRequestLease(snapshot, snapshotRevision, generation, epoch)
	if err != nil {
		c.workspaceLease.releaseRead()
		return nil, nil, err
	}
	leaseCtx = context.WithValue(ctx, workspaceSnapshotContextKey{}, lease)

	var once sync.Once
	var finishErr error
	finish = func() error {
		once.Do(func() {
			lease.end()
			c.mu.Lock()
			changed := c.workspaceRevision != snapshotRevision ||
				c.workspaceGeneration != generation || c.readerEpoch.Load() != epoch
			c.mu.Unlock()
			if changed {
				finishErr = ierrors.New(ierrors.ErrContentModified, "nested.workspace", "child workspace changed during the backend query")
				c.workspaceLease.invalidate()
			}
			c.workspaceLease.releaseRead()
		})
		return finishErr
	}
	return leaseCtx, finish, nil
}

func (c *Conn) captureWorkspaceRequestLease(docs []SnapshotDocument, revision, generation, epoch uint64) (*workspaceRequestLease, error) {
	lease := &workspaceRequestLease{
		conn: c, revision: revision, generation: generation, epoch: epoch,
		documents: make(map[string]workspaceLeaseDocument, len(docs)),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.workspaceRevision != revision || c.workspaceGeneration != generation || c.readerEpoch.Load() != epoch {
		return nil, ierrors.New(ierrors.ErrContentModified, "nested.workspace", "child workspace changed while capturing its snapshot lease")
	}
	for _, doc := range docs {
		state := c.documents[doc.URI]
		contentHash := sha256.Sum256(doc.Content)
		if state == nil || !state.open || state.epoch != epoch || state.langID != doc.LangID ||
			state.contentHash != contentHash || (revision != 0 && state.snapshotRevision != revision) {
			return nil, ierrors.New(ierrors.ErrContentModified, "nested.workspace", "document is not synchronized to the requested workspace snapshot")
		}
		lease.documents[workspaceDocumentKey(doc.URI)] = workspaceLeaseDocument{
			langID: doc.LangID, contentHash: contentHash,
			token: DocumentVersion{
				URI: doc.URI, Version: state.version, epoch: state.epoch,
				serial: state.serial, workspaceGeneration: generation,
			},
		}
	}
	lease.active.Store(true)
	return lease, nil
}

func workspaceLeaseFromContext(ctx context.Context) (*workspaceRequestLease, bool) {
	if ctx == nil {
		return nil, false
	}
	lease, ok := ctx.Value(workspaceSnapshotContextKey{}).(*workspaceRequestLease)
	return lease, ok
}

func prepareWorkspaceSnapshot(docs []SnapshotDocument) ([]SnapshotDocument, [32]byte, error) {
	snapshot := make([]SnapshotDocument, len(docs))
	seen := make(map[string]struct{}, len(docs))
	for i, doc := range docs {
		if doc.URI == "" || doc.LangID == "" {
			return nil, [32]byte{}, ierrors.New(ierrors.ErrInvalidArgument, "nested.workspace", "language id and document URI are required")
		}
		key := workspaceDocumentKey(doc.URI)
		if _, exists := seen[key]; exists {
			return nil, [32]byte{}, ierrors.New(ierrors.ErrInvalidArgument, "nested.workspace", "snapshot contains duplicate canonical document URIs")
		}
		seen[key] = struct{}{}
		snapshot[i] = SnapshotDocument{URI: doc.URI, LangID: doc.LangID, Content: append([]byte(nil), doc.Content...)}
	}
	sort.Slice(snapshot, func(i, j int) bool { return snapshot[i].URI < snapshot[j].URI })

	h := sha256.New()
	for _, doc := range snapshot {
		writeSnapshotField(h, []byte(doc.URI))
		writeSnapshotField(h, []byte(doc.LangID))
		writeSnapshotField(h, doc.Content)
	}
	var fingerprint [32]byte
	copy(fingerprint[:], h.Sum(nil))
	return snapshot, fingerprint, nil
}

func writeSnapshotField(h hash.Hash, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(value)
}

func snapshotContainsURI(docs []SnapshotDocument, uri string) bool {
	index := sort.Search(len(docs), func(i int) bool { return docs[i].URI >= uri })
	return index < len(docs) && docs[index].URI == uri
}

// WorkspaceSnapshotGeneration reports the child workspace generation used to
// detect mutations while backend queries are in flight.
func (c *Conn) WorkspaceSnapshotGeneration() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.workspaceGeneration
}

// WorkspaceGeneration is the workspace generation captured by this document
// version token.
func (v DocumentVersion) WorkspaceGeneration() uint64 {
	return v.workspaceGeneration
}
