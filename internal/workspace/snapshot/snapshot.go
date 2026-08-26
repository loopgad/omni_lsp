// Package snapshot implements the immutable snapshot engine for OmniLSP.
//
// Responsibility:
//
//	Publishes logically immutable Snapshot objects containing VFS root, build set,
//	and workspace model references. Single-writer publication model with atomic
//	pointer swap for lock-free reads (D9).
//
// Owned mutable state:
//
//	current (atomic.Pointer[Snapshot]), revisions counter.
//
// Concurrency model:
//
//	sync/atomic for snapshot publication; readers acquire pointer once and never refresh.
//
// Invariants:
//  1. INV-SNAPSHOT-001: After publication, a Snapshot is logically immutable.
//  2. INV-SNAPSHOT-002: A request MUST NOT read from two Snapshot revisions.
//  3. INV-SNAPSHOT-003: Publishing a newer Snapshot MUST NOT mutate objects reachable from older Snapshots.
package snapshot

import (
	"sync/atomic"
)

// ID uniquely identifies a snapshot.
type ID struct {
	WorkspaceID string
	Revision    uint64
}

// Snapshot is an immutable view of workspace state at a point in time.
// Invariants:
//  1. Once published, a snapshot is never modified.
//  2. Each request is bound to exactly one snapshot.
//  3. Old snapshots remain valid until garbage collected.
type Snapshot struct {
	id        ID
	vfsRev    uint64
	documents map[string]DocumentSnapshot
}

// DocumentSnapshot is an immutable view of a single document.
type DocumentSnapshot struct {
	URI        string
	LanguageID string
	Version    int64
	Content    []byte
}

// New creates a new snapshot from the given documents.
//
// The document map is copied, but content byte slices are SHARED, not
// copied: VFS clones content once at its write boundary (Open/Update), so
// stored slices are immutable VFS property and safe to share across
// revisions. This makes publication O(#docs) instead of O(#docs × size) —
// the per-keystroke publish path never touches file bytes. Document()
// still copies on its read boundary.
//
// INV-SNAPSHOT-003 rests on this discipline: nobody may mutate a slice
// after handing it to the VFS.
func New(wsID string, rev uint64, docs map[string]DocumentSnapshot) *Snapshot {
	cp := make(map[string]DocumentSnapshot, len(docs))
	for k, v := range docs {
		cp[k] = v // shares Content — immutable by VFS write-boundary contract
	}
	return &Snapshot{
		id:        ID{WorkspaceID: wsID, Revision: rev},
		vfsRev:    rev,
		documents: cp,
	}
}

// ID returns the snapshot identifier.
func (s *Snapshot) ID() ID { return s.id }

// Revision returns the monotonic revision of this snapshot (J6: part of the
// coalescing key — requests on different revisions must never join).
func (s *Snapshot) Revision() uint64 { return s.id.Revision }

// Document returns the document snapshot for the given URI.
// Returns nil if the document is not in this snapshot.
func (s *Snapshot) Document(uri string) *DocumentSnapshot {
	d, ok := s.documents[uri]
	if !ok {
		return nil
	}
	contentCopy := make([]byte, len(d.Content))
	copy(contentCopy, d.Content)
	d.Content = contentCopy
	return &d
}

// Documents returns all document URIs in this snapshot.
func (s *Snapshot) Documents() []string {
	uris := make([]string, 0, len(s.documents))
	for uri := range s.documents {
		uris = append(uris, uri)
	}
	return uris
}

// Manager manages snapshot lifecycle.
// Invariants:
//  1. Current() always returns the latest published snapshot.
//  2. Publish() is called from a single writer (scheduler).
type Manager struct {
	current atomic.Pointer[Snapshot]
}

// NewManager creates a new snapshot manager.
func NewManager() *Manager {
	return &Manager{}
}

// Publish publishes a new snapshot as the current one.
func (m *Manager) Publish(s *Snapshot) {
	m.current.Store(s)
}

// Current returns the current snapshot. Never returns nil after first Publish.
func (m *Manager) Current() *Snapshot {
	return m.current.Load()
}
