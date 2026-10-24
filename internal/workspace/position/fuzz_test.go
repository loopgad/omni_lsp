// Package position — fuzz targets per goal.md §S3 (position conversion).
//
// Fuzz invariants: no panic, no out-of-range access, INV-POS-001 roundtrip.
package position

import (
	"fmt"
	"testing"
	"unicode/utf8"
)

// FuzzPositionConversion fuzzes OffsetToPosition / PositionToOffset
// with random byte sequences, verifying INV-POS-001 roundtrips.
func FuzzPositionConversion(f *testing.F) {
	seeds := [][]byte{
		[]byte(""),
		[]byte("a"),
		[]byte("hello world"),
		[]byte("\u4f60\u597d"),
		[]byte("a\xf0\x9f\x98\x80b\xc3\xa9"),
		[]byte("line1\nline2\nline3\n"),
		[]byte("line1\r\nline2\r\n"),
		[]byte("\t\t\t"),
		[]byte("x\x00y\x01z"),
	}
	for _, s := range seeds {
		for _, enc := range []Encoding{UTF8, UTF16, UTF32} {
			f.Add(string(s), int(enc))
		}
	}

	f.Fuzz(func(t *testing.T, seed string, encInt int) {
		enc := Encoding(encInt % 3)
		if enc < UTF8 || enc > UTF32 {
			enc = UTF8
		}
		content := []byte(seed)
		idx := NewIndex(content, enc)

		// INV-POS-001: roundtrip is guaranteed for strict logical boundaries.
		// We verify: (1) OffsetToPosition never errors for valid offsets,
		// (2) the resulting position is well-formed, (3) the roundtrip result
		// matches the pinned expectation for every offset class below.
		for off := uint32(0); off <= idx.totalBytes; {
			pos, err := idx.OffsetToPosition(content, off)
			if err != nil {
				t.Fatalf("offset %d enc=%s: unexpected error: %v", off, enc, err)
			}
			// Verify position is well-formed.
			if pos.Line >= uint32(idx.LineCount()) {
				t.Fatalf("INV-POS-001: line %d out of range (total %d)", pos.Line, idx.LineCount())
			}
			back, err := idx.PositionToOffset(content, pos.Line, pos.Col, enc)
			if err != nil {
				t.Fatalf("roundtrip from (%d,%d): %v", pos.Line, pos.Col, err)
			}
			// Roundtrip expectations are pinned per case (no assertion-free
			// path), matching what INV-POS-001 actually guarantees:
			//  - the LF of a CRLF pair shares (line, col 0) with the line
			//    start and is not a strict boundary; it resolves exactly to
			//    the line start (tolerance pinned below as back == ls).
			//  - every other boundary roundtrips exactly, including EOF:
			//    an empty last line cannot strip its terminator (the clamp
			//    never crosses the line start), and a non-empty last line
			//    carries no byte at the EOF column, so the resolver returns
			//    the content end unchanged.
			ls, err := idx.LineStart(pos.Line)
			if err != nil {
				t.Fatalf("LineStart(%d) for roundtrip check: %v", pos.Line, err)
			}
			if off > 0 && off < idx.totalBytes && content[off] == '\n' && content[off-1] == '\r' {
				if back != ls {
					t.Fatalf("CRLF-LF offset %d enc=%s: got %d, want line start %d",
						off, enc, back, ls)
				}
			} else if back != off {
				t.Fatalf("roundtrip failed at offset %d enc=%s: got %d (want %d)",
					off, enc, back, off)
			}
			// Advance to next rune boundary.
			if off < idx.totalBytes {
				_, s := utf8.DecodeRune(content[off:])
				if s == 0 {
					s = 1
				}
				off += uint32(s)
			} else {
				break
			}
		}

		// Test Clamp with extreme column values.
		for line := uint32(0); line < uint32(idx.LineCount())+10; line++ {
			_ = idx.Clamp(content, LSPPosition{Line: line, Character: 0}, enc)
			_ = idx.Clamp(content, LSPPosition{Line: line, Character: 1 << 20}, enc)
		}

		// Test ConvertPosition between encodings.
		for _, from := range []Encoding{UTF8, UTF16, UTF32} {
			for _, to := range []Encoding{UTF8, UTF16, UTF32} {
				if from == to {
					continue
				}
				_, _ = idx.ConvertPosition(content, LSPPosition{Line: 0, Character: 0}, from, to)
			}
		}

		// Test PositionToOffset with extreme column values.
		for line := uint32(0); line < uint32(idx.LineCount())+5; line++ {
			_, _ = idx.PositionToOffset(content, line, 1<<20, enc)
		}
	})
}

// FuzzPositionEdgeCases exercises pathological inputs.
func FuzzPositionEdgeCases(f *testing.F) {
	f.Add("")
	f.Add("a")
	f.Add("hello\nworld")
	f.Add("\xff\xfe\xfd") // invalid UTF-8 sequence
	f.Add("αβγδε")
	f.Add("\xf0\x90\x80\x80") // overlong surrogate

	f.Fuzz(func(t *testing.T, text string) {
		content := []byte(text)
		for _, enc := range []Encoding{UTF8, UTF16, UTF32} {
			idx := NewIndex(content, enc)
			// Every in-range offset (including EOF) is legal: OffsetToPosition
			// must convert it without error — non-rune-boundary offsets snap
			// inside OffsetToPosition instead of failing.
			for off := uint32(0); off <= idx.totalBytes; off++ {
				if _, err := idx.OffsetToPosition(content, off); err != nil {
					t.Fatalf("enc=%s: OffsetToPosition(%d) on %d bytes: unexpected error: %v",
						enc, off, idx.totalBytes, err)
				}
			}
			// Out-of-range offsets must fail closed with an error (never clamp
			// silently).
			for _, off := range []uint32{idx.totalBytes + 1, idx.totalBytes + 100, 1 << 24} {
				if _, err := idx.OffsetToPosition(content, off); err == nil {
					t.Fatalf("enc=%s: OffsetToPosition(%d) on %d bytes returned nil error, want out-of-range error",
						enc, off, idx.totalBytes)
				}
			}
			// Try out-of-range line numbers.
			for _, line := range []uint32{uint32(idx.LineCount()), uint32(idx.LineCount()) + 100, 1 << 30} {
				_, err := idx.LineStart(line)
				if err == nil {
					t.Fatalf("LineStart(%d) should error", line)
				}
			}
		}
	})
}

// FuzzMultiByteBoundary verifies INV-POS-001 specifically for multi-byte runes.
func FuzzMultiByteBoundary(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("abc"))
	f.Add([]byte("\xe4\xb8\xad\xe6\x96\x87"))
	f.Add([]byte("\xf0\x9f\x98\x80\xf0\x9f\x98\x81"))
	f.Add([]byte("a\xe4\xb8\xadm"))
	f.Add([]byte("\xef\xbf\xbd")) // replacement character

	f.Fuzz(func(t *testing.T, content []byte) {
		idx := NewIndex(content, UTF8)
		for off := uint32(0); off <= idx.totalBytes; {
			// The \n of a CRLF pair shares one line break with its \r: no
			// (line,col) pair maps back uniquely, so the exact-roundtrip
			// property does not apply there (strict-boundary policy).
			if off < idx.totalBytes && content[off] == '\n' && off > 0 && content[off-1] == '\r' {
				off++
				continue
			}
			pos, err := idx.OffsetToPosition(content, off)
			if err != nil {
				t.Fatalf("OffsetToPosition(%d): %v", off, err)
			}
			back, err := idx.PositionToOffset(content, pos.Line, pos.Col, UTF8)
			if err != nil {
				t.Fatalf("PositionToOffset(%d,%d): %v", pos.Line, pos.Col, err)
			}
			if back != off {
				t.Fatalf("roundtrip failed at offset %d: got %d (want %d)", off, back, off)
			}
			if off < idx.totalBytes {
				_, s := utf8.DecodeRune(content[off:])
				if s == 0 {
					s = 1
				}
				off += uint32(s)
			} else {
				break
			}
		}
		// Verify no panic on large column values.
		_ = fmt.Sprintf("col=%d", idx.columnWidth(content, UTF8))
	})
}
