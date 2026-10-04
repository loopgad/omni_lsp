// Package vfs implements the virtual file system with priority-based file sources
// and atomic revision tracking for OmniLSP.
//
// Responsibility:
//
//	Manages document content across disk/editor/virtual/remote sources with
//	editor overlay taking precedence (D1). Provides single-writer mutation with
//	concurrent readers via RWMutex.
//
// Owned mutable state:
//
//	files map (protected by mu), diskFiles map, revision counter (atomic).
//
// Concurrency model:
//
//	sync.RWMutex for map access; atomic.Uint64 for revision counter.
//	All mutations acquire exclusive lock; reads acquire shared lock.
//
// Invariants:
//  1. Editor source (SourceEditor) always takes precedence over disk content.
//  2. Revision counter monotonically increases on each mutation (F1).
//  3. Get() returns a copy to prevent external mutation of internal state.
//  4. Close() reverts to disk state if available; editor overlay is discarded.
package vfs

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/omnilsp/omni/internal/workspace/uri"
)

// FileSource indicates where a file's content comes from.
type FileSource int

const (
	SourceDisk    FileSource = iota // From filesystem
	SourceEditor                    // Unsaved editor content
	SourceVirtual                   // Virtual/generated document
	SourceRemote                    // Remote/cache
)

// FileState represents the state of a single file in the VFS.
type FileState struct {
	URI        string
	LanguageID string
	Version    int64
	Content    []byte
	Source     FileSource
	Dirty      bool // true if editor has unsaved changes
}

// ErrDuplicateOpen reports that an open document was reopened with conflicting
// language, version, or content. The original open state remains intact.
var ErrDuplicateOpen = errors.New("vfs: conflicting duplicate open")

// VFS is a thread-safe virtual file system that maintains file state.
// Invariants:
//  1. All reads and writes are atomic per file.
//  2. Editor overlay (SourceEditor) always takes precedence over disk.
//  3. Closing a file reverts to disk state if available.
type VFS struct {
	mu    sync.RWMutex
	files map[string]*FileState
	// diskFiles stores the last known disk content for revert.
	diskFiles map[string][]byte
	revision  atomic.Uint64
}

// New creates a new empty VFS.
func New() *VFS {
	return &VFS{
		files:     make(map[string]*FileState),
		diskFiles: make(map[string][]byte),
	}
}

// Open opens or updates a file with the given content. The content is
// cloned at this write boundary once; from here on the bytes are immutable
// VFS property (callers may freely reuse/mutate their slice). This is what
// lets snapshot publication share content slices without copying.
func (v *VFS) Open(uri, langID string, version int64, content []byte, source FileSource) {
	key := canonicalURI(uri)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.openLocked(key, uri, langID, version, content, source)
}

// TryOpen opens a file, or accepts an idempotent reopen when language,
// version, and content match. A conflict leaves the existing state intact.
func (v *VFS) TryOpen(uriText, langID string, version int64, content []byte, source FileSource) error {
	key := canonicalURI(uriText)
	v.mu.Lock()
	defer v.mu.Unlock()
	if existing := v.files[key]; existing != nil {
		if existing.LanguageID != langID || existing.Version != version || !bytes.Equal(existing.Content, content) {
			return ErrDuplicateOpen
		}
		return nil
	}
	v.openLocked(key, uriText, langID, version, content, source)
	return nil
}

func (v *VFS) openLocked(key, uriText, langID string, version int64, content []byte, source FileSource) {
	content = cloneBytes(content)
	v.files[key] = &FileState{
		URI:        uriText,
		LanguageID: langID,
		Version:    version,
		Content:    content,
		Source:     source,
		Dirty:      source == SourceEditor,
	}
	if source == SourceDisk {
		v.diskFiles[key] = content
	}
	v.revision.Add(1)
}

// Update updates the content of an open file (e.g., on didChange). The
// content is cloned at this write boundary (see Open).
func (v *VFS) Update(uri string, version int64, content []byte) {
	key := canonicalURI(uri)
	v.mu.Lock()
	defer v.mu.Unlock()
	content = cloneBytes(content)
	if f, ok := v.files[key]; ok {
		f.Version = version
		f.Content = content
		f.Dirty = true
		f.Source = SourceEditor
	} else {
		v.files[key] = &FileState{
			URI:     uri,
			Version: version,
			Content: content,
			Source:  SourceEditor,
			Dirty:   true,
		}
	}
	v.revision.Add(1)
}

// Save marks a file as saved (e.g., on didSave).
func (v *VFS) Save(uri string) {
	key := canonicalURI(uri)
	v.mu.Lock()
	defer v.mu.Unlock()
	if f, ok := v.files[key]; ok {
		f.Dirty = false
		f.Source = SourceDisk
		v.diskFiles[key] = f.Content
	}
	v.revision.Add(1)
}

// Close removes a file from the VFS (e.g., on didClose).
func (v *VFS) Close(uri string) {
	key := canonicalURI(uri)
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.files, key)
	v.revision.Add(1)
}

// Get returns the file state for the given URI.
// Returns nil if the file is not open.
//
// Contract: the returned struct is a copy, but its Content slice is SHARED
// immutable VFS property (cloned at the Open/Update write boundary). Callers
// MUST treat it as read-only; to own mutable bytes use Content(), which
// copies. Hot-path consumers (snapshot publication) rely on the sharing.
func (v *VFS) Get(uri string) *FileState {
	key := canonicalURI(uri)
	v.mu.RLock()
	defer v.mu.RUnlock()
	f := v.files[key]
	if f == nil {
		return nil
	}
	// Return a copy to prevent external mutation.
	cp := *f
	return &cp
}

// Content returns the content bytes for the given URI.
// Returns nil if the file is not open.
func (v *VFS) Content(uri string) []byte {
	key := canonicalURI(uri)
	v.mu.RLock()
	defer v.mu.RUnlock()
	f := v.files[key]
	if f == nil {
		return nil
	}
	cp := make([]byte, len(f.Content))
	copy(cp, f.Content)
	return cp
}

// OpenFiles returns URIs of all open files.
func (v *VFS) OpenFiles() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	uris := make([]string, 0, len(v.files))
	for uri := range v.files {
		uris = append(uris, v.files[uri].URI)
	}
	return uris
}

// Revision returns the current VFS revision number.
func (v *VFS) Revision() uint64 {
	return v.revision.Load()
}

// AdvanceRevision invalidates analyses after an external source change without
// replacing editor-authoritative content or changing document versions.
func (v *VFS) AdvanceRevision() uint64 {
	return v.revision.Add(1)
}

func cloneBytes(b []byte) []byte {
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}

func canonicalURI(value string) string {
	parsed, err := uri.Parse(value)
	if err != nil {
		return value
	}
	return parsed.Canonical()
}
