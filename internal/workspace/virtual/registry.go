// Concurrency model: RWMutex-guarded registry; segment slices are copied
// on registration so callers cannot mutate published mappings.
//
// Invariants:
//  1. Fail closed: any edit span touching an UnmappedGenerated region is rejected (Y0).
//  2. Mappings are immutable once registered; cleanup only via explicit unregister.
//  3. Coordinate mapping never guesses — ambiguous spans report typed errors.
//
// Package virtual implements goal.md §D12 source maps for virtual
// (embedded/generated) documents: an explicit registry of host↔virtual
// offset mappings graded by quality, bidirectional coordinate mapping, and
// the §Y0/SEM-SAFE-001 fail-closed rule that UnmappedGenerated regions block
// unsafe S3 edits. It also carries the minimal §D13 notebook cell model.
//
// Zero third-party dependencies; stdlib only.
package virtual

import (
	"fmt"
	"slices"
	"sync"

	ierrors "github.com/omnilsp/omni/internal/errors"
)

// MappingQuality grades how faithfully a segment maps between virtual and
// host coordinates (§D12: source-map segments MUST specify quality).
type MappingQuality uint8

const (
	QualityExact             MappingQuality = iota + 1 // 1:1 in both directions; safe for S3.
	QualityManyToOne                                   // Several virtual regions collapse into one host region.
	QualityOneToMany                                   // One host region expands into several virtual regions.
	QualityUnmappedGenerated                           // Generated content with no provenance; S3 must not cross it.
)

func (q MappingQuality) String() string {
	switch q {
	case QualityExact:
		return "exact"
	case QualityManyToOne:
		return "many_to_one"
	case QualityOneToMany:
		return "one_to_many"
	case QualityUnmappedGenerated:
		return "unmapped_generated"
	default:
		return fmt.Sprintf("unknown(%d)", uint8(q))
	}
}

// Segment maps one half-open virtual byte range [VirtStart, VirtEnd) onto a
// host byte range [HostStart, HostEnd). Offsets are bytes, not runes —
// callers convert from LSP line/character at their own edge.
type Segment struct {
	HostStart, HostEnd uint32 // byte offsets into the host document
	VirtStart, VirtEnd uint32 // byte offsets into the virtual document
	Quality            MappingQuality
}

// virtualDoc is the immutable record behind one registered virtual URI.
// Segments are never mutated after publication (copy-on-write on register),
// so readers may traverse them after acquiring the pointer under RLock.
type virtualDoc struct {
	hostURI string
	segs    []Segment
}

// Registry tracks virtual documents, their hosts and their source-map
// segments. Safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	virtual   map[string]*virtualDoc // virtual URI -> mapping record
	notebooks map[string]Notebook    // notebook URI -> ordered cells (D13)
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		virtual:   make(map[string]*virtualDoc),
		notebooks: make(map[string]Notebook),
	}
}

// RegisterVirtual records virtualURI as a view over hostURI described by
// segments. Re-registering the same virtual URI replaces its mapping.
func (r *Registry) RegisterVirtual(virtualURI, hostURI string, segments []Segment) error {
	if virtualURI == "" || hostURI == "" {
		return ierrors.New(ierrors.ErrInvalidArgument, "virtual.RegisterVirtual", "empty virtual or host URI")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	segs := make([]Segment, len(segments))
	copy(segs, segments)
	r.virtual[virtualURI] = &virtualDoc{hostURI: hostURI, segs: segs}
	return nil
}

// ResolveHost returns the host document URI backing a virtual document.
func (r *Registry) ResolveHost(virtualURI string) (string, error) {
	if d, ok := r.doc(virtualURI); ok {
		return d.hostURI, nil
	}
	return "", terr(ierrors.ErrNotFound, "virtual.ResolveHost", "UNKNOWN_VIRTUAL", ErrUnknownVirtual)
}

// LookupVirtual lists virtual URIs derived from hostURI (sorted, possibly empty).
func (r *Registry) LookupVirtual(hostURI string) []string {
	r.mu.RLock()
	var out []string
	for vuri, d := range r.virtual {
		if d.hostURI == hostURI {
			out = append(out, vuri)
		}
	}
	r.mu.RUnlock()
	slices.Sort(out)
	return out
}

// IsVirtual reports whether uri names a registered virtual document.
func (r *Registry) IsVirtual(uri string) bool {
	_, ok := r.doc(uri)
	return ok
}

// Segments returns a copy of the mapping segments for virtualURI.
func (r *Registry) Segments(virtualURI string) ([]Segment, bool) {
	d, ok := r.doc(virtualURI)
	if !ok {
		return nil, false
	}
	segs := make([]Segment, len(d.segs))
	copy(segs, d.segs)
	return segs, true
}

// UnregisterVirtual removes a single virtual document.
func (r *Registry) UnregisterVirtual(virtualURI string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.virtual, virtualURI)
}

// UnregisterHost removes every virtual document derived from hostURI — the
// cleanup hook for host-document close (didClose / workspace teardown).
func (r *Registry) UnregisterHost(hostURI string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for vuri, d := range r.virtual {
		if d.hostURI == hostURI {
			delete(r.virtual, vuri)
		}
	}
}

// doc acquires the immutable record pointer; segs are read-only afterwards.
func (r *Registry) doc(virtualURI string) (*virtualDoc, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.virtual[virtualURI]
	return d, ok
}
