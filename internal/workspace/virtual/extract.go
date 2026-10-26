package virtual

// Region extraction — the §D12 inbound half of the virtual-document
// pipeline (Host Document → Region Extraction → Virtual Document). The
// outbound half (coordinate projection) lives in projection.go.

import (
	"fmt"

	ierrors "github.com/omnilsp/omni/internal/errors"
)

// Piece is one region proposal for the inbound pipeline: either a half-open
// host byte range [HostStart, HostEnd) copied verbatim into the virtual
// document (mapped, QualityExact), or a literal generated chunk with no host
// provenance (QualityUnmappedGenerated — §Y0: S3 must not cross it).
type Piece struct {
	// HostStart/HostEnd delimit the mapped host slice. Ignored when
	// Generated is set.
	HostStart, HostEnd uint32
	// Generated marks the piece as generated content: Text is appended to
	// the virtual document and mapped as an UnmappedGenerated region with an
	// empty host range.
	Generated bool
	// Text carries the generated content; only read when Generated is set.
	Text []byte
}

// Extract assembles the inbound pipeline from host content: pieces are
// concatenated in the given order into the virtual content and contribute
// one source-map segment each — QualityExact for mapped host slices,
// QualityUnmappedGenerated for generated chunks. Generated segments record
// an empty host range at the host offset reached so far (the convention the
// mapping tests pin), so reverse lookups can never land inside them.
//
// Fail-closed: mapped pieces must be in bounds of hostContent and strictly
// ordered (each HostStart >= the previous mapped piece's HostEnd — overlap,
// reordering and collapsing are the registry's ManyToOne/OneToMany grades
// and are not guessed here); empty pieces of either kind are rejected
// because a zero-width region can neither carry provenance nor be probed.
func Extract(hostContent []byte, pieces []Piece) ([]byte, []Segment, error) {
	const op = "virtual.Extract"
	virt := make([]byte, 0, len(hostContent))
	segs := make([]Segment, 0, len(pieces))
	hostCursor := uint32(0) // host offset reached so far
	virtLen := uint32(0)
	for i, p := range pieces {
		if p.Generated {
			if len(p.Text) == 0 {
				return nil, nil, ierrors.New(ierrors.ErrInvalidArgument, op,
					fmt.Sprintf("piece %d: empty generated region", i))
			}
			segs = append(segs, Segment{
				HostStart: hostCursor, HostEnd: hostCursor,
				VirtStart: virtLen, VirtEnd: virtLen + uint32(len(p.Text)),
				Quality: QualityUnmappedGenerated,
			})
			virt = append(virt, p.Text...)
			virtLen += uint32(len(p.Text))
			continue
		}
		if p.HostStart > p.HostEnd {
			return nil, nil, ierrors.New(ierrors.ErrInvalidArgument, op,
				fmt.Sprintf("piece %d: host range start %d exceeds end %d", i, p.HostStart, p.HostEnd))
		}
		if p.HostStart == p.HostEnd {
			return nil, nil, ierrors.New(ierrors.ErrInvalidArgument, op,
				fmt.Sprintf("piece %d: empty host range", i))
		}
		if p.HostEnd > uint32(len(hostContent)) {
			return nil, nil, ierrors.New(ierrors.ErrInvalidArgument, op,
				fmt.Sprintf("piece %d: host range [%d,%d) out of bounds (host is %d bytes)",
					i, p.HostStart, p.HostEnd, len(hostContent)))
		}
		if p.HostStart < hostCursor {
			return nil, nil, ierrors.New(ierrors.ErrInvalidArgument, op,
				fmt.Sprintf("piece %d: host range [%d,%d) overlaps or reorders the previous mapped piece (host cursor %d)",
					i, p.HostStart, p.HostEnd, hostCursor))
		}
		segs = append(segs, Segment{
			HostStart: p.HostStart, HostEnd: p.HostEnd,
			VirtStart: virtLen, VirtEnd: virtLen + (p.HostEnd - p.HostStart),
			Quality: QualityExact,
		})
		virt = append(virt, hostContent[p.HostStart:p.HostEnd]...)
		virtLen += p.HostEnd - p.HostStart
		hostCursor = p.HostEnd
	}
	return virt, segs, nil
}

// RegisterExtracted runs the inbound pipeline end to end: Extract builds the
// virtual slice and its source map from hostContent, then the mapping is
// registered under virtualURI → hostURI (replacing any previous mapping for
// that virtual URI, per RegisterVirtual's contract). Returns the virtual
// content — the registry stores mappings, not text, so the caller keeps the
// slice and passes it back with the host content to build a Projector.
func (r *Registry) RegisterExtracted(virtualURI, hostURI string, hostContent []byte, pieces []Piece) ([]byte, error) {
	virt, segs, err := Extract(hostContent, pieces)
	if err != nil {
		return nil, err
	}
	if err := r.RegisterVirtual(virtualURI, hostURI, segs); err != nil {
		return nil, err
	}
	return virt, nil
}
