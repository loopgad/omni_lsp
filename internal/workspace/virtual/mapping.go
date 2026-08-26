package virtual

import (
	stderrors "errors"

	ierrors "github.com/omnilsp/omni/internal/errors"
)

// Sentinel errors: callers compare with stdlib errors.Is. Returned values
// are internal/errors structured errors whose Cause chain terminates at the
// sentinel, so both errors.Is and Kind-based comparison work.
var (
	// ErrUnmappedRegion — the offset lies in (or a span crosses) generated
	// content with no source provenance.
	ErrUnmappedRegion = stderrors.New("virtual: offset lies in an unmapped generated region")
	// ErrUnsafeGeneratedEdit — §Y0/SEM-SAFE-001: an edit would touch an
	// UnmappedGenerated region; fail closed.
	ErrUnsafeGeneratedEdit = stderrors.New("virtual: edit touches an unmapped generated region")
	// ErrUnsafeEditSpan — the span is not provably contained in one Exact
	// segment, so its host projection is ambiguous for S3.
	ErrUnsafeEditSpan = stderrors.New("virtual: edit span is not wholly inside a single Exact segment")
	// ErrUnknownVirtual — the URI is not a registered virtual document.
	ErrUnknownVirtual = stderrors.New("virtual: unknown virtual document")
	// ErrOffsetOutOfRange — the offset is outside the document bounds.
	ErrOffsetOutOfRange = stderrors.New("virtual: offset outside document bounds")
)

// terr builds the structured error for a sentinel cause.
func terr(kind ierrors.ErrorKind, op, code string, cause error) *ierrors.Error {
	e := ierrors.Wrap(kind, op, cause)
	e.Code = code
	return e
}

// ToHostOffset maps a virtual byte offset to its host byte offset.
//
// Offsets inside an Exact/ManyToOne/OneToMany segment map linearly and the
// segment quality is reported so callers can decide whether to trust the
// result; offsets inside an UnmappedGenerated segment (or in a gap between
// segments) return ErrUnmappedRegion — absence of proof is not projection
// (SEM-SAFE-001).
//
// ponytail: linear segment scan; switch to binary search if profiles show
// large segment counts matter.
func ToHostOffset(reg *Registry, virtualURI string, virtOff uint32) (hostOff uint32, q MappingQuality, err error) {
	const op = "virtual.ToHostOffset"
	d, ok := reg.doc(virtualURI)
	if !ok {
		return 0, 0, terr(ierrors.ErrNotFound, op, "UNKNOWN_VIRTUAL", ErrUnknownVirtual)
	}
	for _, s := range d.segs {
		if virtOff >= s.VirtStart && virtOff < s.VirtEnd {
			if s.Quality == QualityUnmappedGenerated {
				return 0, s.Quality, terr(ierrors.ErrInvalidPosition, op, "UNMAPPED_REGION", ErrUnmappedRegion)
			}
			off := s.HostStart + (virtOff - s.VirtStart)
			// Collapsing grades (ManyToOne/OneToMany) have a larger virtual
			// than host span; clamp the linear projection into the host
			// segment instead of returning an out-of-range offset.
			if off >= s.HostEnd {
				if s.HostEnd > s.HostStart {
					off = s.HostEnd - 1
				} else {
					off = s.HostStart
				}
			}
			return off, s.Quality, nil
		}
	}
	return 0, 0, terr(ierrors.ErrInvalidPosition, op, "OUT_OF_RANGE", ErrOffsetOutOfRange)
}

// ToVirtualOffset is ToHostOffset's inverse: host byte offset → virtual byte
// offset, with the same quality semantics and failure modes.
func ToVirtualOffset(reg *Registry, hostURI string, hostOff uint32) (virtOff uint32, q MappingQuality, err error) {
	const op = "virtual.ToVirtualOffset"
	// A host may back several virtual documents; pick deterministically
	// (LookupVirtual is sorted) rather than guessing by map order.
	virts := reg.LookupVirtual(hostURI)
	if len(virts) == 0 {
		return 0, 0, terr(ierrors.ErrNotFound, op, "UNKNOWN_VIRTUAL", ErrUnknownVirtual)
	}
	d, ok := reg.doc(virts[0])
	if !ok {
		return 0, 0, terr(ierrors.ErrNotFound, op, "UNKNOWN_VIRTUAL", ErrUnknownVirtual)
	}
	for _, s := range d.segs {
		if hostOff >= s.HostStart && hostOff < s.HostEnd {
			if s.Quality == QualityUnmappedGenerated {
				return 0, s.Quality, terr(ierrors.ErrInvalidPosition, op, "UNMAPPED_REGION", ErrUnmappedRegion)
			}
			return s.VirtStart + (hostOff - s.HostStart), s.Quality, nil
		}
	}
	return 0, 0, terr(ierrors.ErrInvalidPosition, op, "OUT_OF_RANGE", ErrOffsetOutOfRange)
}
