package virtual

// Coordinate projection — the §D12 outbound half of the virtual-document
// pipeline (Virtual Result → Reverse Source Map → Host Result) and its
// inverse. Byte-offset mapping lives in mapping.go; this file adds the LSP
// line/character layer (position ↔ offset via internal/workspace/position,
// the §D7/D8 canonical engine) and the edit-range gates.

import (
	"bytes"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/workspace/position"
)

// Projector projects positions and edit ranges between one host document and
// one registered virtual document. Positions are LSP line/character pairs in
// the encoding fixed at construction (§C4 negotiation decides which); the
// registry underneath works in byte offsets.
//
// Fail-closed (registry invariant 3 — coordinate mapping never guesses):
// points inside UnmappedGenerated regions or mapping gaps report
// ErrUnmappedRegion / ErrOffsetOutOfRange; byte offsets without a logical
// line/character boundary report ErrNotABoundary; edit ranges are gated by
// the same §Y0/SEM-SAFE-001 rule as ValidateEditSpan (one Exact segment,
// whole-span). Point projection is half-open like the registry's segments —
// a position exactly on a segment's closing boundary has no provenance and
// fails, while range endpoints may sit on it (the gate admits them).
type Projector struct {
	reg        *Registry
	virtualURI string
	hostURI    string
	host, virt []byte
	hostIdx    *position.Index
	virtIdx    *position.Index
	enc        position.Encoding
}

// NewProjector binds a registered virtual document to its current host and
// virtual content. The registered Exact segments are verified against the
// two contents: a map that disagrees with the text it claims to describe
// would project coordinates that do not exist, so construction fails closed.
// ManyToOne/OneToMany grades make no byte-equality claim and are not
// verified.
func NewProjector(reg *Registry, virtualURI string, hostContent, virtualContent []byte, enc position.Encoding) (*Projector, error) {
	const op = "virtual.NewProjector"
	d, ok := reg.doc(virtualURI)
	if !ok {
		return nil, terr(ierrors.ErrNotFound, op, "UNKNOWN_VIRTUAL", ErrUnknownVirtual)
	}
	for _, s := range d.segs {
		if s.Quality != QualityExact {
			continue
		}
		if s.HostStart > s.HostEnd || s.VirtStart > s.VirtEnd ||
			s.HostEnd > uint32(len(hostContent)) || s.VirtEnd > uint32(len(virtualContent)) ||
			!bytes.Equal(hostContent[s.HostStart:s.HostEnd], virtualContent[s.VirtStart:s.VirtEnd]) {
			return nil, ierrors.New(ierrors.ErrInvalidArgument, op,
				"virtual content does not match the registered Exact mapping for "+virtualURI)
		}
	}
	return &Projector{
		reg:        reg,
		virtualURI: virtualURI,
		hostURI:    d.hostURI,
		host:       hostContent,
		virt:       virtualContent,
		hostIdx:    position.NewIndex(hostContent, enc),
		virtIdx:    position.NewIndex(virtualContent, enc),
		enc:        enc,
	}, nil
}

// VirtualToHostPosition maps a virtual LSP position to the host position
// holding the same content. The containing segment's quality is returned so
// callers can grade the result; UnmappedGenerated points and mapping gaps
// fail with ErrUnmappedRegion / ErrOffsetOutOfRange.
func (p *Projector) VirtualToHostPosition(line, character uint32) (position.LSPPosition, MappingQuality, error) {
	const op = "virtual.Projector.VirtualToHostPosition"
	voff, err := p.virtIdx.OffsetOfLineChar(p.virt, line, character)
	if err != nil {
		return position.LSPPosition{}, 0, err
	}
	hoff, q, err := ToHostOffset(p.reg, p.virtualURI, voff)
	if err != nil {
		return position.LSPPosition{}, q, err
	}
	hline, hchar, err := p.positionAt(op, p.host, p.hostIdx, hoff)
	if err != nil {
		return position.LSPPosition{}, q, err
	}
	return position.LSPPosition{Line: hline, Character: hchar}, q, nil
}

// HostToVirtualPosition is the inverse projection into this projector's
// virtual document (ToVirtualOffset resolves the host's first virtual
// document, which is not necessarily this one). Host offsets outside every
// mapped region — prose between extracted regions, for instance — fail with
// ErrOffsetOutOfRange: the virtual view simply has no coordinate there.
func (p *Projector) HostToVirtualPosition(line, character uint32) (position.LSPPosition, MappingQuality, error) {
	const op = "virtual.Projector.HostToVirtualPosition"
	hoff, err := p.hostIdx.OffsetOfLineChar(p.host, line, character)
	if err != nil {
		return position.LSPPosition{}, 0, err
	}
	d, ok := p.reg.doc(p.virtualURI)
	if !ok { // the mapping was unregistered after construction
		return position.LSPPosition{}, 0, terr(ierrors.ErrNotFound, op, "UNKNOWN_VIRTUAL", ErrUnknownVirtual)
	}
	voff, q, err := toVirtualOffset(d, op, hoff)
	if err != nil {
		return position.LSPPosition{}, q, err
	}
	vline, vchar, err := p.positionAt(op, p.virt, p.virtIdx, voff)
	if err != nil {
		return position.LSPPosition{}, q, err
	}
	return position.LSPPosition{Line: vline, Character: vchar}, q, nil
}

// VirtualToHostRange maps a half-open virtual edit range to host coordinates.
// The §Y0/SEM-SAFE-001 gate is applied unchanged — ValidateEditSpan runs on
// the resolved byte span — so the projector and the Y0-9 gate cannot
// disagree: an edit crossing an UnmappedGenerated region fails with
// ErrUnsafeGeneratedEdit, any other span not wholly inside one Exact segment
// fails with ErrUnsafeEditSpan. Both endpoints then map linearly inside that
// segment, so a range ending on the segment's closing boundary (which the
// gate admits) projects to the host segment's end.
func (p *Projector) VirtualToHostRange(start, end position.LSPPosition) (position.LSPRange, MappingQuality, error) {
	const op = "virtual.Projector.VirtualToHostRange"
	vstart, err := p.virtIdx.OffsetOfLineChar(p.virt, start.Line, start.Character)
	if err != nil {
		return position.LSPRange{}, 0, err
	}
	vend, err := p.virtIdx.OffsetOfLineChar(p.virt, end.Line, end.Character)
	if err != nil {
		return position.LSPRange{}, 0, err
	}
	if err := ValidateEditSpan(p.reg, p.virtualURI, vstart, vend); err != nil {
		return position.LSPRange{}, 0, err
	}
	d, ok := p.reg.doc(p.virtualURI)
	if !ok { // the mapping was unregistered after construction
		return position.LSPRange{}, 0, terr(ierrors.ErrNotFound, op, "UNKNOWN_VIRTUAL", ErrUnknownVirtual)
	}
	seg, ok := exactVirtualSpanContaining(d.segs, vstart, vend)
	if !ok { // unreachable: ValidateEditSpan just admitted the span
		return position.LSPRange{}, 0, terr(ierrors.ErrUntrustedOperation, op, "UNSAFE_EDIT_SPAN", ErrUnsafeEditSpan)
	}
	hstart := seg.HostStart + (vstart - seg.VirtStart)
	hend := seg.HostStart + (vend - seg.VirtStart)
	startLine, startChar, err := p.positionAt(op, p.host, p.hostIdx, hstart)
	if err != nil {
		return position.LSPRange{}, seg.Quality, err
	}
	endLine, endChar, err := p.positionAt(op, p.host, p.hostIdx, hend)
	if err != nil {
		return position.LSPRange{}, seg.Quality, err
	}
	return position.LSPRange{
		Start: position.LSPPosition{Line: startLine, Character: startChar},
		End:   position.LSPPosition{Line: endLine, Character: endChar},
	}, seg.Quality, nil
}

// HostToVirtualRange mirrors the projection for edits landing on the HOST
// document: the host span must lie wholly inside one Exact segment's host
// range (validateHostSpan — the same fail-closed taxonomy) and maps linearly
// into virtual coordinates. Host spans over unmapped territory (host text
// the virtual view was not extracted from) fail with ErrUnsafeEditSpan
// rather than guessing a partial projection.
func (p *Projector) HostToVirtualRange(start, end position.LSPPosition) (position.LSPRange, MappingQuality, error) {
	const op = "virtual.Projector.HostToVirtualRange"
	hstart, err := p.hostIdx.OffsetOfLineChar(p.host, start.Line, start.Character)
	if err != nil {
		return position.LSPRange{}, 0, err
	}
	hend, err := p.hostIdx.OffsetOfLineChar(p.host, end.Line, end.Character)
	if err != nil {
		return position.LSPRange{}, 0, err
	}
	d, ok := p.reg.doc(p.virtualURI)
	if !ok {
		return position.LSPRange{}, 0, terr(ierrors.ErrNotFound, op, "UNKNOWN_VIRTUAL", ErrUnknownVirtual)
	}
	if err := validateHostSpan(op, d, hstart, hend); err != nil {
		return position.LSPRange{}, 0, err
	}
	seg, ok := exactHostSpanContaining(d.segs, hstart, hend)
	if !ok { // unreachable: validateHostSpan just admitted the span
		return position.LSPRange{}, 0, terr(ierrors.ErrUntrustedOperation, op, "UNSAFE_EDIT_SPAN", ErrUnsafeEditSpan)
	}
	vstart := seg.VirtStart + (hstart - seg.HostStart)
	vend := seg.VirtStart + (hend - seg.HostStart)
	startLine, startChar, err := p.positionAt(op, p.virt, p.virtIdx, vstart)
	if err != nil {
		return position.LSPRange{}, seg.Quality, err
	}
	endLine, endChar, err := p.positionAt(op, p.virt, p.virtIdx, vend)
	if err != nil {
		return position.LSPRange{}, seg.Quality, err
	}
	return position.LSPRange{
		Start: position.LSPPosition{Line: startLine, Character: startChar},
		End:   position.LSPPosition{Line: endLine, Character: endChar},
	}, seg.Quality, nil
}

// positionAt converts a byte offset back into an LSP position, failing
// closed when the offset carries no logical boundary. The position engine's
// OffsetToPosition snaps mid-rune offsets to the rune start and gives a
// position past a trailing terminator at EOF, so the result is verified by
// converting it back: a mismatch means no line/character coordinate denotes
// the offset exactly (mid-rune, or the LF of a CRLF pair — LineCharAt
// rejects both).
func (p *Projector) positionAt(op string, content []byte, idx *position.Index, off uint32) (line, character uint32, err error) {
	pos, err := idx.OffsetToPosition(content, off)
	if err != nil {
		return 0, 0, err
	}
	if back, err := idx.OffsetOfLineChar(content, pos.Line, pos.Col); err == nil && back == off {
		return pos.Line, pos.Col, nil
	}
	if off == uint32(len(content)) {
		// The true EOF coordinate is the last line's (possibly empty)
		// content width after any trailing terminator — the convention
		// LineCharAt pins for end-of-document positions.
		line := uint32(idx.LineCount() - 1)
		start, err := idx.LineStart(line)
		if err != nil {
			return 0, 0, err
		}
		end := uint32(len(content))
		for end > start && (content[end-1] == '\n' || content[end-1] == '\r') {
			end--
		}
		char := widthOf(content[start:end], p.enc)
		if back, err := idx.OffsetOfLineChar(content, line, char); err == nil && back == off {
			return line, char, nil
		}
	}
	return 0, 0, terr(ierrors.ErrInvalidPosition, op, "NOT_A_BOUNDARY", ErrNotABoundary)
}

// widthOf measures text in LSP position units for the given encoding — the
// same measure the position engine's Index uses to build columns.
func widthOf(text []byte, enc position.Encoding) uint32 {
	switch enc {
	case position.UTF8:
		return uint32(len(text))
	case position.UTF32:
		return uint32(position.RuneLength(text))
	default:
		return uint32(position.UTF16Len(text))
	}
}

// exactVirtualSpanContaining returns the Exact segment whose VIRTUAL span
// contains [start, end] with the gate's inclusive-end rule for ranges.
func exactVirtualSpanContaining(segs []Segment, start, end uint32) (Segment, bool) {
	for _, s := range segs {
		if s.Quality == QualityExact && start >= s.VirtStart && end <= s.VirtEnd {
			return s, true
		}
	}
	return Segment{}, false
}

// exactHostSpanContaining is exactVirtualSpanContaining's mirror on the
// segments' host spans.
func exactHostSpanContaining(segs []Segment, start, end uint32) (Segment, bool) {
	for _, s := range segs {
		if s.Quality == QualityExact && start >= s.HostStart && end <= s.HostEnd {
			return s, true
		}
	}
	return Segment{}, false
}
