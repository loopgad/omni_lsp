package virtual

// Swept range-level round-trip and §Y0-9 gate parity in all three negotiated
// encodings. pipeline_test.go pins the absolute failure classes with
// hand-picked ranges at UTF-16 only; this file makes the property exhaustive:
// for EVERY ordered pair of LSP-reachable positions of the fixture, the
// projector and the edit gates must agree class-for-class, and every admitted
// range must project onto exactly the host bytes of its containing Exact
// segment and round-trip back identically — in UTF-8, UTF-16 and UTF-32
// alike. Unmapped-region behavior at range level (generated overlap →
// ErrUnsafeGeneratedEdit, gaps → ErrUnsafeEditSpan) is exercised implicitly
// by the class parity against the gates, whose absolute taxonomy the other
// files pin.

import (
	"testing"

	"github.com/omnilsp/omni/internal/workspace/position"
)

// sweepPos is one LSP-reachable position with its byte offset.
type sweepPos struct {
	line, char, off uint32
}

// reachablePositions collects every LSP-reachable position of content in enc.
func reachablePositions(t *testing.T, content []byte, enc position.Encoding) []sweepPos {
	t.Helper()
	var out []sweepPos
	sweepPositions(t, content, enc, func(line, char, off uint32) {
		out = append(out, sweepPos{line: line, char: char, off: off})
	})
	return out
}

func lspPos(sp sweepPos) position.LSPPosition {
	return position.LSPPosition{Line: sp.line, Character: sp.char}
}

// TestD12_RangeSweepParityAndRoundTripAllEncodings walks every ordered pair
// of LSP-reachable positions on each side of the mapping and asserts, per
// pair:
//
//  1. the projector's verdict class equals the corresponding edit gate's
//     (§Y0-9: an edit is projectable exactly when the gate admits it);
//  2. for admitted spans, the projection is the linear image of the
//     containing Exact segment — the exact host (virtual) bytes, not merely
//     a plausible position — and the reverse projection round-trips
//     identically.
func TestD12_RangeSweepParityAndRoundTripAllEncodings(t *testing.T) {
	for _, enc := range []position.Encoding{position.UTF8, position.UTF16, position.UTF32} {
		p := pipelineFixture(t, enc)
		t.Run(enc.String(), func(t *testing.T) {
			t.Run("virtual direction agrees with ValidateEditSpan", func(t *testing.T) {
				vps := reachablePositions(t, p.virt, enc)
				for _, start := range vps {
					for _, end := range vps {
						if start.off > end.off {
							continue // LSP ranges are ordered; inverted input is pinned in pipeline_test.go
						}
						gate := classifyErr(t, ValidateEditSpan(p.reg, p.vuri, start.off, end.off))
						got, q, err := p.proj.VirtualToHostRange(lspPos(start), lspPos(end))
						if cls := classifyErr(t, err); cls != gate {
							t.Fatalf("virtual [%d,%d) (%d,%d)-(%d,%d): gate class %q, projector class %q (err=%v)",
								start.off, end.off, start.line, start.char, end.line, end.char, gate, cls, err)
						}
						if gate != "ok" {
							continue
						}
						if q != QualityExact {
							t.Fatalf("virtual [%d,%d): quality = %v, want Exact", start.off, end.off, q)
						}
						seg, ok := exactVirtualSpanContaining(p.segs, start.off, end.off)
						if !ok {
							t.Fatalf("virtual [%d,%d): gate admitted but no Exact segment contains it", start.off, end.off)
						}
						wantStart := seg.HostStart + (start.off - seg.VirtStart)
						wantEnd := seg.HostStart + (end.off - seg.VirtStart)
						hstart, err := position.OffsetOfLineCharEncoding(p.host, got.Start.Line, got.Start.Character, enc)
						if err != nil || hstart != wantStart {
							t.Fatalf("virtual [%d,%d): host start = %v (off %d, err %v), want offset %d",
								start.off, end.off, got.Start, hstart, err, wantStart)
						}
						hend, err := position.OffsetOfLineCharEncoding(p.host, got.End.Line, got.End.Character, enc)
						if err != nil || hend != wantEnd {
							t.Fatalf("virtual [%d,%d): host end = %v (off %d, err %v), want offset %d",
								start.off, end.off, got.End, hend, err, wantEnd)
						}
						if string(p.host[wantStart:wantEnd]) != string(p.virt[start.off:end.off]) {
							t.Fatalf("virtual [%d,%d): projected host bytes %q != virtual bytes %q",
								start.off, end.off, p.host[wantStart:wantEnd], p.virt[start.off:end.off])
						}
						back, q, err := p.proj.HostToVirtualRange(got.Start, got.End)
						if err != nil || q != QualityExact {
							t.Fatalf("virtual [%d,%d): reverse range: %v q=%v", start.off, end.off, err, q)
						}
						if back.Start != lspPos(start) || back.End != lspPos(end) {
							t.Fatalf("virtual [%d,%d): round trip (%v,%v) -> (%v,%v) -> (%v,%v)",
								start.off, end.off, lspPos(start), lspPos(end), got.Start, got.End, back.Start, back.End)
						}
					}
				}
			})

			t.Run("host direction agrees with the host-span gate", func(t *testing.T) {
				d, ok := p.reg.doc(p.vuri)
				if !ok {
					t.Fatalf("virtual document %s missing from registry", p.vuri)
				}
				hps := reachablePositions(t, p.host, enc)
				for _, start := range hps {
					for _, end := range hps {
						if start.off > end.off {
							continue
						}
						gate := classifyErr(t, validateHostSpan("test", d, start.off, end.off))
						got, q, err := p.proj.HostToVirtualRange(lspPos(start), lspPos(end))
						if cls := classifyErr(t, err); cls != gate {
							t.Fatalf("host [%d,%d) (%d,%d)-(%d,%d): gate class %q, projector class %q (err=%v)",
								start.off, end.off, start.line, start.char, end.line, end.char, gate, cls, err)
						}
						if gate != "ok" {
							continue
						}
						if q != QualityExact {
							t.Fatalf("host [%d,%d): quality = %v, want Exact", start.off, end.off, q)
						}
						seg, ok := exactHostSpanContaining(p.segs, start.off, end.off)
						if !ok {
							t.Fatalf("host [%d,%d): gate admitted but no Exact segment contains it", start.off, end.off)
						}
						wantStart := seg.VirtStart + (start.off - seg.HostStart)
						wantEnd := seg.VirtStart + (end.off - seg.HostStart)
						vstart, err := position.OffsetOfLineCharEncoding(p.virt, got.Start.Line, got.Start.Character, enc)
						if err != nil || vstart != wantStart {
							t.Fatalf("host [%d,%d): virtual start = %v (off %d, err %v), want offset %d",
								start.off, end.off, got.Start, vstart, err, wantStart)
						}
						vend, err := position.OffsetOfLineCharEncoding(p.virt, got.End.Line, got.End.Character, enc)
						if err != nil || vend != wantEnd {
							t.Fatalf("host [%d,%d): virtual end = %v (off %d, err %v), want offset %d",
								start.off, end.off, got.End, vend, err, wantEnd)
						}
						if string(p.virt[wantStart:wantEnd]) != string(p.host[start.off:end.off]) {
							t.Fatalf("host [%d,%d): projected virtual bytes %q != host bytes %q",
								start.off, end.off, p.virt[wantStart:wantEnd], p.host[start.off:end.off])
						}
						back, q, err := p.proj.VirtualToHostRange(got.Start, got.End)
						if err != nil || q != QualityExact {
							t.Fatalf("host [%d,%d): reverse range: %v q=%v", start.off, end.off, err, q)
						}
						if back.Start != lspPos(start) || back.End != lspPos(end) {
							t.Fatalf("host [%d,%d): round trip (%v,%v) -> (%v,%v) -> (%v,%v)",
								start.off, end.off, lspPos(start), lspPos(end), got.Start, got.End, back.Start, back.End)
						}
					}
				}
			})
		})
	}
}
