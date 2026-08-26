package corpus

// §S22 metamorphic tests (goal.md 行 4489-4500): transformations that cannot
// change semantics — added whitespace/comments, changed line endings — MUST
// leave results invariant modulo locations. These probes run pure VFS +
// UTF-16 position engine flows over embedded synthetic sources: no language
// toolchain, no disk access beyond nothing at all, fully deterministic.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// baseSrc is the shared synthetic document (~30 lines). The 🦀 astral char
// forces surrogate-pair UTF-16 columns into every mapping check.
const baseSrc = `package metamorphic

import "fmt"

// Payload carries a 🦀 astral char so surrogate-pair columns are exercised.
type Payload struct {
	name  string
	steps int
}

func newPayload(name string) *Payload {
	return &Payload{name: name}
}

func (p *Payload) advance() int {
	p.steps++
	return p.steps
}

func report(p *Payload) string {
	return fmt.Sprintf("%s advanced %d steps", p.name, p.steps)
}

func main() {
	p := newPayload("probe")
	p.advance()
	p.advance()
	fmt.Println(report(p))
}
`

var metamorphicSymbols = []string{"newPayload", "advance", "steps", "report", "\U0001f980"}

// anchor is one symbol occurrence resolved through the UTF-16 engine.
type anchor struct {
	symbol    string
	line, col uint32 // UTF-16 line:character of the first code unit
	start     uint32 // byte offset of first byte
}

// anchorsOf locates every occurrence of the symbols in content.
func anchorsOf(content []byte) []anchor {
	var out []anchor
	for _, sym := range metamorphicSymbols {
		for from := 0; ; {
			i := bytes.Index(content[from:], []byte(sym))
			if i < 0 {
				break
			}
			off := uint32(from + i)
			line, col, err := position.LineCharAt(content, off)
			if err != nil {
				panic("anchor inside multi-byte rune: " + err.Error())
			}
			out = append(out, anchor{symbol: sym, line: line, col: col, start: off})
			from += i + len(sym)
		}
	}
	return out
}

// checkAnchor asserts the engine round-trips a transformed-content anchor
// (both span boundaries) and that the mapped bytes still spell the symbol:
// the semantic payload is invariant, only its location may move.
func checkAnchor(t *testing.T, content []byte, a anchor) {
	t.Helper()
	off, err := position.OffsetOfLineChar(content, a.line, a.col)
	if err != nil || off != a.start {
		t.Fatalf("%q @ %d:%d -> offset %d (want %d, err %v): mapping not self-consistent",
			a.symbol, a.line, a.col, off, a.start, err)
	}
	end := a.start + uint32(len(a.symbol))
	el, ec, err := position.LineCharAt(content, end)
	if err != nil {
		t.Fatalf("%q: span end offset %d unmappable: %v", a.symbol, end, err)
	}
	eo, err := position.OffsetOfLineChar(content, el, ec)
	if err != nil || eo != end {
		t.Fatalf("%q: span end %d:%d -> %d (want %d, err %v)", a.symbol, el, ec, eo, end, err)
	}
	if got := string(content[a.start:end]); got != a.symbol {
		t.Fatalf("%q @ %d:%d: mapped span carries %q", a.symbol, a.line, a.col, got)
	}
}

// docStream drives the VFS didChange stream: open the base document once,
// then push successive versions through Update and hand the stored content
// (what a query would see) back to the caller.
type docStream struct {
	t       *testing.T
	v       *vfs.VFS
	uri     string
	ver     int64
	rev     uint64
	content []byte
}

func openDoc(t *testing.T, content []byte) *docStream {
	t.Helper()
	v := vfs.New()
	const docURI = "file:///s22/metamorphic.go"
	v.Open(docURI, "go", 1, content, vfs.SourceEditor)
	return &docStream{t: t, v: v, uri: docURI, ver: 1, rev: v.Revision(), content: v.Content(docURI)}
}

// push applies one semantic-preserving transformation through didChange.
func (d *docStream) push(transformed string) []byte {
	d.t.Helper()
	d.ver++
	d.v.Update(d.uri, d.ver, []byte(transformed))
	if d.v.Revision() <= d.rev {
		d.t.Fatal("vfs revision must advance on didChange")
	}
	d.rev = d.v.Revision()
	d.content = d.v.Content(d.uri)
	if string(d.content) != transformed {
		d.t.Fatal("didChange stream altered the pushed content")
	}
	return d.content
}

// TestS22_MetamorphicWhitespaceInvariance: adding leading indentation and
// trailing whitespace shifts columns by exactly the padding width (or not at
// all) and leaves every symbol's mapping self-consistent modulo locations.
func TestS22_MetamorphicWhitespaceInvariance(t *testing.T) {
	base := openDoc(t, []byte(baseSrc))
	baseline := anchorsOf(base.content)
	if len(baseline) == 0 {
		t.Fatal("fixture lost its anchors")
	}

	const pad = "    "
	indented := base.push(indentEachLine(baseSrc, pad))
	indentedAnchors := anchorsOf(indented)
	if len(indentedAnchors) != len(baseline) {
		t.Fatalf("indent changed anchor count: %d -> %d", len(baseline), len(indentedAnchors))
	}
	for i, a := range indentedAnchors {
		want := baseline[i]
		want.col += uint32(position.UTF16Len([]byte(pad))) // lines shifted right by pad width
		if a.line != want.line || a.col != want.col {
			t.Fatalf("indent moved %q from %d:%d to %d:%d, want %d:%d",
				a.symbol, want.line, want.col, a.line, a.col, want.line, want.col)
		}
		checkAnchor(t, indented, a) // byte offsets come straight from the transformed content
	}

	const tail = "   "
	spaced := base.push(trailEachLine(baseSrc, tail))
	spacedAnchors := anchorsOf(spaced)
	if len(spacedAnchors) != len(baseline) {
		t.Fatalf("trailing whitespace changed anchor count: %d -> %d", len(baseline), len(spacedAnchors))
	}
	for i, a := range spacedAnchors {
		w := baseline[i]
		if a.line != w.line || a.col != w.col {
			t.Fatalf("trailing whitespace moved %q: (%d:%d) -> (%d:%d); logical positions must stay put",
				a.symbol, w.line, w.col, a.line, a.col)
		}
		checkAnchor(t, spaced, a)
	}
}

// TestS22_MetamorphicLineEndingInvariance: converting LF ↔ CRLF ↔ CR must
// keep every logical line:column position identical and every mapping
// self-consistent within each encoding.
func TestS22_MetamorphicLineEndingInvariance(t *testing.T) {
	lfSrc := strings.ReplaceAll(baseSrc, "\r\n", "\n")
	stream := openDoc(t, []byte(lfSrc))
	baseline := anchorsOf(stream.content)

	variants := []struct {
		name    string
		content string
	}{
		{"crlf", strings.ReplaceAll(lfSrc, "\n", "\r\n")},
		{"lone-cr", strings.ReplaceAll(lfSrc, "\n", "\r")},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			content := stream.push(v.content)
			anchors := anchorsOf(content)
			if len(anchors) != len(baseline) {
				t.Fatalf("line-ending conversion changed anchor count: %d -> %d",
					len(baseline), len(anchors))
			}
			for i, a := range anchors {
				w := baseline[i]
				if a.line != w.line || a.col != w.col {
					t.Fatalf("%s: terminator style moved %q from %d:%d to %d:%d",
						v.name, a.symbol, w.line, w.col, a.line, a.col)
				}
				checkAnchor(t, content, a)
			}
			idx := position.NewIndex([]byte(content), position.UTF16)
			if idx.LineCount() != position.NewIndex([]byte(lfSrc), position.UTF16).LineCount() {
				t.Fatalf("%s: line count changed under terminator conversion", v.name)
			}
		})
	}
}

// TestS22_MetamorphicCommentInjectionInvariance: injecting leading comment
// lines must shift every original position down by exactly the injected
// delta (old position + delta) and keep mappings self-consistent.
func TestS22_MetamorphicCommentInjectionInvariance(t *testing.T) {
	const probe = "// s22 metamorphic probe\n"
	const injections = 2
	injected := strings.Repeat(probe, injections) + baseSrc

	stream := openDoc(t, []byte(baseSrc))
	baseline := anchorsOf(stream.content)
	content := stream.push(injected)
	anchors := anchorsOf(content)
	if len(anchors) != len(baseline) {
		t.Fatalf("comment injection changed anchor count: %d -> %d", len(baseline), len(anchors))
	}

	deltaLines := uint32(injections)
	deltaBytes := uint32(len(probe) * injections)
	for i, a := range anchors {
		w := baseline[i]
		if a.line != w.line+deltaLines || a.col != w.col {
			t.Fatalf("%q moved from %d:%d to %d:%d, want %d:%d (same column)",
				a.symbol, w.line, w.col, a.line, a.col, w.line+deltaLines, w.col)
		}
		if a.start != w.start+deltaBytes {
			t.Fatalf("%q byte offset moved %d -> %d, want +delta=%d",
				a.symbol, w.start, a.start, w.start+deltaBytes)
		}
		// The delta formula itself must be exact: mapping the OLD position
		// plus delta through the engine lands on old offset + delta bytes.
		off, err := position.OffsetOfLineChar(content, w.line+deltaLines, w.col)
		if err != nil || off != w.start+deltaBytes {
			t.Fatalf("%q: old-position+delta maps to %d (err %v), want %d",
				a.symbol, off, err, w.start+deltaBytes)
		}
		checkAnchor(t, content, a)
	}
}

// indentEachLine prefixes every line (empty ones included — trailing
// whitespace is semantically inert by construction here).
func indentEachLine(src, pad string) string {
	lines := strings.Split(src, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

// trailEachLine appends tail to every non-empty line.
func trailEachLine(src, tail string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = l + tail
		}
	}
	return strings.Join(lines, "\n")
}
