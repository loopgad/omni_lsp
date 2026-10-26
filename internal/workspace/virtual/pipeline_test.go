package virtual

// Tests for the §D12 pipeline as a whole: region extraction on the way in
// (Extract / RegisterExtracted), bidirectional coordinate projection on the
// way out (Projector — positions and edit ranges, in all three negotiated
// encodings), and the requirement that projection and the §Y0-9 edit gates
// agree everywhere (an edit is projectable exactly when the gate admits it,
// with the same failure class).

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/workspace/position"
)

// Fixture: a notebook-ish host (prose, two fenced code regions, prose) whose
// second code region carries π (2 bytes, 1 UTF-16 unit) and 𝕏 (4 bytes,
// 2 UTF-16 units), so the UTF-8/UTF-16/UTF-32 column units genuinely
// disagree and the sweeps exercise all three encodings.
const (
	vURI = "file:///v.go"
	hURI = "file:///host.md"

	hostProse1 = "# title\n"
	hostFence1 = "```go\n"
	hostCode1  = "package main\n\nfunc main() {}\n"
	hostProse2 = "some prose\n"
	hostFence2 = "```go\n"
	hostCode2  = "var π𝕏 = 1\n"
	hostFence3 = "```\n"

	hostDoc = hostProse1 + hostFence1 + hostCode1 + hostProse2 + hostFence2 + hostCode2 + hostFence3

	// Generated chunks: virtual-only content with no host provenance.
	gen1 = "//! vdoc header\n"
	gen2 = "//! --- cell ---\n"
)

func spanOf(doc, sub string) (uint32, uint32) {
	i := strings.Index(doc, sub)
	if i < 0 {
		panic("test fixture: substring not found: " + sub)
	}
	return uint32(i), uint32(i + len(sub))
}

func pipelinePieces() []Piece {
	c1s, c1e := spanOf(hostDoc, hostCode1)
	c2s, c2e := spanOf(hostDoc, hostCode2)
	return []Piece{
		{Generated: true, Text: []byte(gen1)},
		{HostStart: c1s, HostEnd: c1e},
		{Generated: true, Text: []byte(gen2)},
		{HostStart: c2s, HostEnd: c2e},
	}
}

// pipe bundles the registered fixture with its projector and texts.
type pipe struct {
	reg  *Registry
	proj *Projector
	vuri string
	host []byte
	virt []byte
	segs []Segment
}

func pipelineFixture(t *testing.T, enc position.Encoding) *pipe {
	t.Helper()
	host := []byte(hostDoc)
	reg := New()
	virt, err := reg.RegisterExtracted(vURI, hURI, host, pipelinePieces())
	if err != nil {
		t.Fatalf("RegisterExtracted: %v", err)
	}
	proj, err := NewProjector(reg, vURI, host, virt, enc)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	segs, ok := reg.Segments(vURI)
	if !ok {
		t.Fatalf("Segments(%s) missing after registration", vURI)
	}
	return &pipe{reg: reg, proj: proj, vuri: vURI, host: host, virt: virt, segs: segs}
}

// sweepPositions enumerates every LSP-reachable position of content in enc —
// every scalar boundary of every line including end-of-line and the final
// (possibly empty) line — and visits each with its byte offset.
func sweepPositions(t *testing.T, content []byte, enc position.Encoding, visit func(line, char, off uint32)) {
	t.Helper()
	idx := position.NewIndex(content, enc)
	for line := 0; line < idx.LineCount(); line++ {
		start, err := idx.LineStart(uint32(line))
		if err != nil {
			t.Fatalf("LineStart(%d): %v", line, err)
		}
		end := uint32(len(content))
		if next, err := idx.LineStart(uint32(line + 1)); err == nil {
			end = next
		}
		lc := content[start:end]
		for len(lc) > 0 && (lc[len(lc)-1] == '\n' || lc[len(lc)-1] == '\r') {
			lc = lc[:len(lc)-1]
		}
		for char := uint32(0); char <= widthOf(lc, enc); char++ {
			off, err := idx.OffsetOfLineChar(content, uint32(line), char)
			if err != nil {
				continue // mid-surrogate-pair column: no scalar boundary
			}
			visit(uint32(line), char, off)
		}
	}
}

// virtualSpanAt / hostSpanAt: half-open point containment per side.
func virtualSpanAt(segs []Segment, off uint32) (Segment, bool) {
	for _, s := range segs {
		if off >= s.VirtStart && off < s.VirtEnd {
			return s, true
		}
	}
	return Segment{}, false
}

func hostSpanAt(segs []Segment, off uint32) (Segment, bool) {
	for _, s := range segs {
		if off >= s.HostStart && off < s.HostEnd {
			return s, true
		}
	}
	return Segment{}, false
}

// classifyErr buckets an outcome by the sentinel it carries so gate and
// projector decisions can be compared class-for-class.
func classifyErr(t *testing.T, err error) string {
	t.Helper()
	switch {
	case err == nil:
		return "ok"
	case stderrors.Is(err, ErrUnsafeGeneratedEdit):
		return "generated"
	case stderrors.Is(err, ErrUnsafeEditSpan):
		return "span"
	case stderrors.Is(err, ErrUnmappedRegion):
		return "unmapped"
	case stderrors.Is(err, ErrOffsetOutOfRange):
		return "range"
	case stderrors.Is(err, ErrUnknownVirtual):
		return "unknown"
	case stderrors.Is(err, ErrNotABoundary):
		return "boundary"
	default:
		return "other"
	}
}

// TestD12_PipelineExtractBuildsVirtualSlice — the inbound half: pieces
// concatenate into the virtual slice with one correctly-bounded segment per
// piece, generated chunks record empty host ranges, and every malformed
// piece shape is rejected.
func TestD12_PipelineExtractBuildsVirtualSlice(t *testing.T) {
	c1s, c1e := spanOf(hostDoc, hostCode1)
	c2s, c2e := spanOf(hostDoc, hostCode2)
	virt, segs, err := Extract([]byte(hostDoc), pipelinePieces())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if want := gen1 + hostCode1 + gen2 + hostCode2; string(virt) != want {
		t.Fatalf("virtual slice = %q, want %q", virt, want)
	}
	if len(segs) != 4 {
		t.Fatalf("segments = %d, want 4", len(segs))
	}
	abs := len(gen1) + len(hostCode1) + len(gen2)
	wantSegs := []Segment{
		{HostStart: 0, HostEnd: 0, VirtStart: 0, VirtEnd: uint32(len(gen1)), Quality: QualityUnmappedGenerated},
		{HostStart: c1s, HostEnd: c1e, VirtStart: uint32(len(gen1)), VirtEnd: uint32(len(gen1) + len(hostCode1)), Quality: QualityExact},
		{HostStart: c1e, HostEnd: c1e, VirtStart: uint32(len(gen1) + len(hostCode1)), VirtEnd: uint32(abs), Quality: QualityUnmappedGenerated},
		{HostStart: c2s, HostEnd: c2e, VirtStart: uint32(abs), VirtEnd: uint32(len(virt)), Quality: QualityExact},
	}
	for i, w := range wantSegs {
		if segs[i] != w {
			t.Errorf("segment %d = %+v, want %+v", i, segs[i], w)
		}
	}

	invalid := []struct {
		name   string
		pieces []Piece
	}{
		{"out of bounds", []Piece{{HostStart: 0, HostEnd: uint32(len(hostDoc)) + 1}}},
		{"inverted range", []Piece{{HostStart: 5, HostEnd: 2}}},
		{"empty mapped", []Piece{{HostStart: 3, HostEnd: 3}}},
		{"empty generated", []Piece{{Generated: true}}},
		{"overlap", []Piece{{HostStart: 0, HostEnd: 10}, {HostStart: 5, HostEnd: 20}}},
		{"reorder", []Piece{{HostStart: 10, HostEnd: 20}, {HostStart: 0, HostEnd: 5}}},
	}
	for _, tc := range invalid {
		if _, _, err := Extract([]byte(hostDoc), tc.pieces); err == nil {
			t.Errorf("%s: Extract accepted invalid pieces", tc.name)
		}
	}
	// Adjacent (non-overlapping) host slices are legal.
	if _, segs, err := Extract([]byte("0123456789"), []Piece{{HostStart: 0, HostEnd: 5}, {HostStart: 5, HostEnd: 10}}); err != nil || len(segs) != 2 {
		t.Errorf("adjacent pieces rejected: segs=%d err=%v", len(segs), err)
	}
}

// TestD12_PipelinePositionRoundTrip — the outbound half at position level,
// in every negotiated encoding: every LSP-reachable position of the virtual
// slice either round-trips identically through an Exact segment (with byte
// identity in both texts) or fails closed with exactly the class the mapping
// dictates (generated → ErrUnmappedRegion, boundary/EOF → ErrOffsetOutOfRange);
// likewise for host positions into the virtual view.
func TestD12_PipelinePositionRoundTrip(t *testing.T) {
	for _, enc := range []position.Encoding{position.UTF8, position.UTF16, position.UTF32} {
		p := pipelineFixture(t, enc)
		t.Run(enc.String(), func(t *testing.T) {
			sweepPositions(t, p.virt, enc, func(line, char, off uint32) {
				seg, ok := virtualSpanAt(p.segs, off)
				if ok && seg.Quality == QualityExact {
					hpos, q, err := p.proj.VirtualToHostPosition(line, char)
					if err != nil || q != QualityExact {
						t.Fatalf("virtual (%d,%d) off %d: %v q=%v", line, char, off, err, q)
					}
					hoff, err := position.OffsetOfLineCharEncoding(p.host, hpos.Line, hpos.Character, enc)
					if err != nil {
						t.Fatalf("host position %v not resolvable: %v", hpos, err)
					}
					if want := seg.HostStart + (off - seg.VirtStart); hoff != want {
						t.Fatalf("virtual off %d projected to host off %d, want %d", off, hoff, want)
					}
					if p.virt[off] != p.host[hoff] {
						t.Fatalf("content mismatch at virtual off %d", off)
					}
					vpos, q, err := p.proj.HostToVirtualPosition(hpos.Line, hpos.Character)
					if err != nil || q != QualityExact {
						t.Fatalf("reverse of virtual (%d,%d): %v q=%v", line, char, err, q)
					}
					if vpos.Line != line || vpos.Character != char {
						t.Fatalf("round trip (%d,%d) -> %v -> %v", line, char, hpos, vpos)
					}
					return
				}
				_, _, err := p.proj.VirtualToHostPosition(line, char)
				if err == nil {
					t.Fatalf("virtual (%d,%d) off %d projected despite no Exact provenance", line, char, off)
				}
				if ok && seg.Quality == QualityUnmappedGenerated {
					if !stderrors.Is(err, ErrUnmappedRegion) {
						t.Fatalf("generated point (%d,%d): err = %v, want ErrUnmappedRegion", line, char, err)
					}
					return
				}
				if !stderrors.Is(err, ErrOffsetOutOfRange) {
					t.Fatalf("boundary point (%d,%d) off %d: err = %v, want ErrOffsetOutOfRange", line, char, off, err)
				}
			})
			sweepPositions(t, p.host, enc, func(line, char, off uint32) {
				seg, ok := hostSpanAt(p.segs, off)
				if ok && seg.Quality == QualityExact {
					vpos, q, err := p.proj.HostToVirtualPosition(line, char)
					if err != nil || q != QualityExact {
						t.Fatalf("host (%d,%d) off %d: %v q=%v", line, char, off, err, q)
					}
					voff, err := position.OffsetOfLineCharEncoding(p.virt, vpos.Line, vpos.Character, enc)
					if err != nil {
						t.Fatalf("virtual position %v not resolvable: %v", vpos, err)
					}
					if want := seg.VirtStart + (off - seg.HostStart); voff != want {
						t.Fatalf("host off %d projected to virtual off %d, want %d", off, voff, want)
					}
					if p.host[off] != p.virt[voff] {
						t.Fatalf("content mismatch at host off %d", off)
					}
					hpos, q, err := p.proj.VirtualToHostPosition(vpos.Line, vpos.Character)
					if err != nil || q != QualityExact {
						t.Fatalf("reverse of host (%d,%d): %v q=%v", line, char, err, q)
					}
					if hpos.Line != line || hpos.Character != char {
						t.Fatalf("round trip (%d,%d) -> %v -> %v", line, char, vpos, hpos)
					}
					return
				}
				_, _, err := p.proj.HostToVirtualPosition(line, char)
				if !stderrors.Is(err, ErrOffsetOutOfRange) {
					t.Fatalf("host (%d,%d) off %d outside every segment: err = %v, want ErrOffsetOutOfRange", line, char, off, err)
				}
			})
		})
	}
}

// TestD12_PipelineRangeProjectionMatchesY0Gate — edit ranges: the projector
// must refuse exactly the spans the §Y0-9 gates refuse, with the same failure
// class, and every admitted range must round-trip across both directions.
func TestD12_PipelineRangeProjectionMatchesY0Gate(t *testing.T) {
	p := pipelineFixture(t, position.UTF16)
	c1s, c1e := spanOf(hostDoc, hostCode1)
	c2s, _ := spanOf(hostDoc, hostCode2)
	p2s, _ := spanOf(hostDoc, hostProse2)
	f1s, f1e := spanOf(hostDoc, hostFence1)
	e1 := p.segs[1] // Exact over code-1
	e2 := p.segs[3] // Exact over code-2
	genMid := p.segs[2]
	vposOf := func(off uint32) position.LSPPosition {
		line, char, err := position.LineCharAt(p.virt, off)
		if err != nil {
			t.Fatalf("LineCharAt(virt, %d): %v", off, err)
		}
		return position.LSPPosition{Line: line, Character: char}
	}
	hposOf := func(off uint32) position.LSPPosition {
		line, char, err := position.LineCharAt(p.host, off)
		if err != nil {
			t.Fatalf("LineCharAt(host, %d): %v", off, err)
		}
		return position.LSPPosition{Line: line, Character: char}
	}

	t.Run("virtual direction agrees with ValidateEditSpan", func(t *testing.T) {
		cases := []struct {
			name   string
			vs, ve uint32
		}{
			{"inside code-1", e1.VirtStart + 3, e1.VirtStart + 9},
			{"full code-1", e1.VirtStart, e1.VirtEnd},
			{"closing boundary", e1.VirtEnd - 4, e1.VirtEnd},
			{"zero-width at generated start", e1.VirtEnd, e1.VirtEnd},
			{"crosses into generated", e1.VirtEnd - 3, e1.VirtEnd + 3},
			{"inside generated", genMid.VirtStart + 4, genMid.VirtStart + 9},
			{"spans both cells over the gap", e1.VirtEnd - 2, e2.VirtStart + 2},
			{"whole document", 0, uint32(len(p.virt))},
			{"inverted", e1.VirtStart + 5, e1.VirtStart + 2},
		}
		for _, tc := range cases {
			gate := classifyErr(t, ValidateEditSpan(p.reg, p.vuri, tc.vs, tc.ve))
			got, q, err := p.proj.VirtualToHostRange(vposOf(tc.vs), vposOf(tc.ve))
			if cls := classifyErr(t, err); cls != gate {
				t.Fatalf("%s: gate class %q, projector class %q (err=%v)", tc.name, gate, cls, err)
			}
			if gate != "ok" {
				continue
			}
			if q != QualityExact {
				t.Fatalf("%s: quality = %v, want Exact", tc.name, q)
			}
			back, q, err := p.proj.HostToVirtualRange(got.Start, got.End)
			if err != nil || q != QualityExact {
				t.Fatalf("%s: reverse range: %v q=%v", tc.name, err, q)
			}
			if back.Start != vposOf(tc.vs) || back.End != vposOf(tc.ve) {
				t.Fatalf("%s: range round trip %v -> %v -> %v", tc.name, vposOf(tc.vs), got, back)
			}
		}
	})

	t.Run("host direction agrees with the host-span gate", func(t *testing.T) {
		d, ok := p.reg.doc(p.vuri)
		if !ok {
			t.Fatalf("virtual document %s missing from registry", p.vuri)
		}
		cases := []struct {
			name   string
			hs, he uint32
		}{
			{"inside code-1", c1s + 3, c1s + 9},
			{"full code-1", c1s, c1e},
			{"zero-width inside code-1", c1s + 4, c1s + 4},
			{"prose between cells", p2s + 2, p2s + 6},
			{"zero-width in prose", p2s + 3, p2s + 3},
			{"prose into code-2", p2s, c2s + 4},
			{"fence line", f1s, f1e},
			{"inverted", c1s + 5, c1s + 2},
		}
		for _, tc := range cases {
			gate := classifyErr(t, validateHostSpan("test", d, tc.hs, tc.he))
			got, q, err := p.proj.HostToVirtualRange(hposOf(tc.hs), hposOf(tc.he))
			if cls := classifyErr(t, err); cls != gate {
				t.Fatalf("%s: gate class %q, projector class %q (err=%v)", tc.name, gate, cls, err)
			}
			if gate != "ok" {
				continue
			}
			if q != QualityExact {
				t.Fatalf("%s: quality = %v, want Exact", tc.name, q)
			}
			back, q, err := p.proj.VirtualToHostRange(got.Start, got.End)
			if err != nil || q != QualityExact {
				t.Fatalf("%s: reverse range: %v q=%v", tc.name, err, q)
			}
			if back.Start != hposOf(tc.hs) || back.End != hposOf(tc.he) {
				t.Fatalf("%s: range round trip %v -> %v -> %v", tc.name, hposOf(tc.hs), got, back)
			}
		}
	})
}

// TestD12_PipelineProjectorFailsClosed — construction never binds a projector
// whose contents disagree with the registered Exact segments.
func TestD12_PipelineProjectorFailsClosed(t *testing.T) {
	p := pipelineFixture(t, position.UTF16)

	t.Run("content disagreeing with Exact segments", func(t *testing.T) {
		bad := append([]byte(nil), p.virt...)
		bad[p.segs[1].VirtStart+2] ^= 0x20
		if _, err := NewProjector(p.reg, p.vuri, p.host, bad, position.UTF16); err == nil {
			t.Fatal("NewProjector accepted virtual content contradicting the Exact mapping")
		}
		// Truncating inside the last mapped host region breaks the segment's
		// bounds and must be refused...
		e2 := p.segs[3]
		if _, err := NewProjector(p.reg, p.vuri, p.host[:e2.HostEnd-1], p.virt, position.UTF16); err == nil {
			t.Fatal("NewProjector accepted host content shorter than the mapping")
		}
		// ...but Exact segments make no claim about unmapped host territory,
		// so losing the trailing fence line (never mapped) still verifies.
		if _, err := NewProjector(p.reg, p.vuri, p.host[:len(p.host)-1], p.virt, position.UTF16); err != nil {
			t.Fatalf("NewProjector rejected a host whose unmapped tail changed: %v", err)
		}
		// The virtual slice is fully covered by segments, so ANY truncation
		// contradicts the map.
		if _, err := NewProjector(p.reg, p.vuri, p.host, p.virt[:len(p.virt)-1], position.UTF16); err == nil {
			t.Fatal("NewProjector accepted virtual content shorter than the mapping")
		}
	})

	t.Run("unknown virtual URI", func(t *testing.T) {
		if _, err := NewProjector(p.reg, "file:///absent", p.host, p.virt, position.UTF16); !stderrors.Is(err, ErrUnknownVirtual) {
			t.Fatalf("err = %v, want ErrUnknownVirtual", err)
		}
	})

	t.Run("inverted Exact segment rejected", func(t *testing.T) {
		reg := New()
		if err := reg.RegisterVirtual("file:///v.bad", "file:///h.bad", []Segment{
			{HostStart: 9, HostEnd: 3, VirtStart: 0, VirtEnd: 6, Quality: QualityExact},
		}); err != nil {
			t.Fatalf("RegisterVirtual: %v", err)
		}
		if _, err := NewProjector(reg, "file:///v.bad", []byte("0123456789"), []byte("abcdef"), position.UTF16); err == nil {
			t.Fatal("NewProjector accepted an inverted Exact segment")
		}
	})
}

// TestD12_PipelineRegisterExtractedReplacesMapping — re-registering a virtual
// URI replaces its mapping (registry contract), after which a projector over
// the stale slice fails closed and one over the fresh slice works.
func TestD12_PipelineRegisterExtractedReplacesMapping(t *testing.T) {
	const hostText = "hello world"
	reg := New()
	v1, err := reg.RegisterExtracted("file:///v.txt", "file:///h.txt", []byte(hostText), []Piece{{HostStart: 0, HostEnd: 5}})
	if err != nil || string(v1) != "hello" {
		t.Fatalf("first extract: v=%q err=%v", v1, err)
	}
	v2, err := reg.RegisterExtracted("file:///v.txt", "file:///h.txt", []byte(hostText), []Piece{{HostStart: 6, HostEnd: 11}})
	if err != nil || string(v2) != "world" {
		t.Fatalf("second extract: v=%q err=%v", v2, err)
	}
	segs, ok := reg.Segments("file:///v.txt")
	if !ok || len(segs) != 1 || segs[0].HostStart != 6 || segs[0].Quality != QualityExact {
		t.Fatalf("segments after re-register = %+v ok=%v", segs, ok)
	}
	if _, err := NewProjector(reg, "file:///v.txt", []byte(hostText), v1, position.UTF16); err == nil {
		t.Fatal("stale virtual slice passed Exact verification")
	}
	if _, err := NewProjector(reg, "file:///v.txt", []byte(hostText), v2, position.UTF16); err != nil {
		t.Fatalf("fresh slice rejected: %v", err)
	}
}

// TestD12_PipelineRejectsUnprojectableCoordinates — coordinates that exist as
// bytes but carry no LSP position, or no host/virtual provenance, fail closed.
func TestD12_PipelineRejectsUnprojectableCoordinates(t *testing.T) {
	t.Run("linear ManyToOne projection into a multi-byte rune", func(t *testing.T) {
		reg := New()
		if err := reg.RegisterVirtual("file:///v.m2o", "file:///h.txt", []Segment{
			{HostStart: 0, HostEnd: 3, VirtStart: 0, VirtEnd: 4, Quality: QualityManyToOne},
		}); err != nil {
			t.Fatalf("RegisterVirtual: %v", err)
		}
		proj, err := NewProjector(reg, "file:///v.m2o", []byte("a𝕏b"), []byte("abcd"), position.UTF8)
		if err != nil {
			t.Fatalf("NewProjector: %v", err)
		}
		// {0,2} is a valid UTF-8 boundary in "abcd", but its linear host
		// projection (offset 2) falls inside 𝕏 — no host coordinate exists.
		_, q, err := proj.VirtualToHostPosition(0, 2)
		if !stderrors.Is(err, ErrNotABoundary) {
			t.Fatalf("err = %v, want ErrNotABoundary", err)
		}
		if q != QualityManyToOne {
			t.Fatalf("quality = %v, want ManyToOne", q)
		}
	})

	t.Run("host-side generated span", func(t *testing.T) {
		reg := New()
		if err := reg.RegisterVirtual("file:///v.gen", "file:///h2.txt", []Segment{
			{HostStart: 0, HostEnd: 5, VirtStart: 0, VirtEnd: 5, Quality: QualityExact},
			{HostStart: 5, HostEnd: 10, VirtStart: 5, VirtEnd: 8, Quality: QualityUnmappedGenerated},
		}); err != nil {
			t.Fatalf("RegisterVirtual: %v", err)
		}
		proj, err := NewProjector(reg, "file:///v.gen", []byte("aaaaabbbbb"), []byte("aaaaaggg"), position.UTF16)
		if err != nil {
			t.Fatalf("NewProjector: %v", err)
		}
		if _, _, err := proj.HostToVirtualPosition(0, 7); !stderrors.Is(err, ErrUnmappedRegion) {
			t.Fatalf("host point in generated span: err = %v, want ErrUnmappedRegion", err)
		}
		hr := position.LSPRange{
			Start: position.LSPPosition{Line: 0, Character: 6},
			End:   position.LSPPosition{Line: 0, Character: 8},
		}
		if _, _, err := proj.HostToVirtualRange(hr.Start, hr.End); !stderrors.Is(err, ErrUnsafeGeneratedEdit) {
			t.Fatalf("host range over generated span: err = %v, want ErrUnsafeGeneratedEdit", err)
		}
	})

	t.Run("virtual EOF has no host provenance", func(t *testing.T) {
		p := pipelineFixture(t, position.UTF16)
		line, char, err := position.LineCharAt(p.virt, uint32(len(p.virt)))
		if err != nil {
			t.Fatalf("LineCharAt(EOF): %v", err)
		}
		if _, _, err := p.proj.VirtualToHostPosition(line, char); !stderrors.Is(err, ErrOffsetOutOfRange) {
			t.Fatalf("virtual EOF: err = %v, want ErrOffsetOutOfRange (point projection is half-open)", err)
		}
	})
}
