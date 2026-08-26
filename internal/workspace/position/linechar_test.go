package position

// Roundtrip tests for goal.md §D7/§D8 and INV-POS-001:
// valid position conversions must preserve the exact logical boundary;
// invalid positions must return typed errors, never panic (INV-POS-002).

import (
	"testing"
)

func roundtripSamples() []string {
	return []string{
		"",
		"ascii only\n",
		"中文注释\nfunc main() {}\n",
		"emoji 😀 pair\nsecond\n",
		"crlf line one\r\ncrlf line two\r\n",
		"mixed 中文 + emoji 😀 end\n",
		"trailing no newline",
		"\n\n\n",
		"tab\there\n",
	}
}

// TestOffsetLineCharRoundTrip verifies offset -> LineCharAt -> OffsetOfLineChar
// == original offset for every scalar boundary in each sample. Offsets without
// a unique position representation (mid-rune bytes, the \n of a CRLF pair)
// are not logical boundaries and must be rejected (INV-POS-002).
func TestOffsetLineCharRoundTrip(t *testing.T) {
	for _, src := range roundtripSamples() {
		for off := 0; off <= len(src); off++ {
			line, col, err := LineCharAt([]byte(src), uint32(off))
			if err != nil {
				if !isInvalidBoundary(src, off) {
					t.Fatalf("LineCharAt(%q, %d): unexpected error: %v", src, off, err)
				}
				continue // no position representation for this byte
			}
			back, err := OffsetOfLineChar([]byte(src), line, col)
			if err != nil {
				t.Fatalf("OffsetOfLineChar(%q, %d:%d) from offset %d: %v", src, line, col, off, err)
			}
			if back != uint32(off) {
				t.Errorf("roundtrip %q offset %d -> %d:%d -> %d", src, off, line, col, back)
			}
		}
	}
}

// isInvalidBoundary reports whether an offset lacks a unique logical position.
func isInvalidBoundary(src string, off int) bool {
	if off >= len(src) {
		return false
	}
	if src[off]&0xC0 == 0x80 {
		return true // mid-rune
	}
	return src[off] == '\n' && off > 0 && src[off-1] == '\r' // inside CRLF
}

// TestOffsetOfLineCharRejectsInvalid verifies INV-POS-002 for inbound conversion.
func TestOffsetOfLineCharRejectsInvalid(t *testing.T) {
	src := []byte("ab😀中\ncd\n")
	cases := []struct {
		name string
		line uint32
		col  uint32
	}{
		{"line out of range", 9, 0},
		{"column past ascii width", 0, 100},
		{"inside surrogate pair", 0, 3}, // a(1) b(2) [😀 spans 2..4]
		{"column past cjk width", 0, 6}, // width = 5 units
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := OffsetOfLineChar(src, tc.line, tc.col)
			if err == nil {
				t.Errorf("position %d:%d should be rejected", tc.line, tc.col)
			}
		})
	}
}

// TestLineCharAtKnownValues pins exact UTF-16 semantics.
func TestLineCharAtKnownValues(t *testing.T) {
	src := "a中😀b\ncd"
	cases := []struct {
		offset   uint32
		wantLine uint32
		wantCol  uint32
	}{
		{0, 0, 0}, // a
		{1, 0, 1}, // 中 starts at UTF-16 col 1
		{4, 0, 2}, // 😀 starts at col 2 (surrogate pair start)
		{8, 0, 4}, // b after emoji: 1+1+2 = 4 units
		{9, 0, 5}, // \n position = end of line content
		{10, 1, 0},
		{11, 1, 1},
		{12, 1, 2}, // EOF
	}
	for _, tc := range cases {
		line, col, err := LineCharAt([]byte(src), tc.offset)
		if err != nil {
			t.Fatalf("offset %d: %v", tc.offset, err)
		}
		if line != tc.wantLine || col != tc.wantCol {
			t.Errorf("offset %d = %d:%d, want %d:%d", tc.offset, line, col, tc.wantLine, tc.wantCol)
		}
	}
}

// TestCRLFLineStartsMatchFullScanner compares incremental line detection with
// a naive full scan (property from §D8).
func TestCRLFLineStartsMatchFullScanner(t *testing.T) {
	src := "one\r\ntwo\nthree\rfour\r\n"
	idx := NewIndex([]byte(src), UTF16)

	var wantLines []uint32
	wantLines = append(wantLines, 0)
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			wantLines = append(wantLines, uint32(i+1))
		} else if src[i] == '\r' && (i+1 >= len(src) || src[i+1] != '\n') {
			wantLines = append(wantLines, uint32(i+1))
		}
	}
	if len(idx.lines) != len(wantLines) {
		t.Fatalf("lines = %v, want %v", idx.lines, wantLines)
	}
	for i := range wantLines {
		if idx.lines[i] != wantLines[i] {
			t.Errorf("line[%d] = %d, want %d", i, idx.lines[i], wantLines[i])
		}
	}
}
