package virtual

import (
	ierrors "github.com/omnilsp/omni/internal/errors"
)

// ValidateEditSpan enforces the §Y0/§D12 S3 rule: an S3 edit may proceed
// only when its half-open virtual span [start, end) lies wholly inside one
// Exact segment. Any overlap with an UnmappedGenerated segment fails closed
// with ErrUnsafeGeneratedEdit; spans graded ManyToOne/OneToMany (ambiguous
// host projection) fail with ErrUnsafeEditSpan. Unknown URIs return
// ErrUnknownVirtual — callers that serve plain host documents must gate with
// Registry.IsVirtual first.
func ValidateEditSpan(reg *Registry, virtualURI string, start, end uint32) error {
	const op = "virtual.ValidateEditSpan"
	if start > end {
		return ierrors.New(ierrors.ErrInvalidArgument, op, "edit start exceeds end")
	}
	d, ok := reg.doc(virtualURI)
	if !ok {
		return terr(ierrors.ErrNotFound, op, "UNKNOWN_VIRTUAL", ErrUnknownVirtual)
	}
	// Fail closed on generated gaps first — the strongest, cheapest check.
	for _, s := range d.segs {
		if s.Quality == QualityUnmappedGenerated && start < s.VirtEnd && end > s.VirtStart {
			return terr(ierrors.ErrUntrustedOperation, op, "UNSAFE_GENERATED_EDIT", ErrUnsafeGeneratedEdit)
		}
	}
	for _, s := range d.segs {
		if s.Quality == QualityExact && start >= s.VirtStart && end <= s.VirtEnd {
			return nil
		}
	}
	return terr(ierrors.ErrUntrustedOperation, op, "UNSAFE_EDIT_SPAN", ErrUnsafeEditSpan)
}
