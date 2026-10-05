package position

// Package position -- tests for the Position Engine per goal.md section D7-D8.
//
// Invariants tested:
//
//	INV-POS-001: valid position roundtrips preserve exact boundary
//	INV-POS-002: invalid positions return typed error, never panic

import (
	"testing"
	"unicode/utf8"
)

func TestNewIndexEmpty(t *testing.T) {
	idx := NewIndex([]byte{}, UTF8)
	if idx.LineCount() != 1 {
		t.Errorf("line count = %d, want 1", idx.LineCount())
	}
}

func TestNewIndexSingleLine(t *testing.T) {
	idx := NewIndex([]byte("hello"), UTF8)
	if idx.LineCount() != 1 {
		t.Errorf("line count = %d, want 1", idx.LineCount())
	}
	start, err := idx.LineStart(0)
	if err != nil {
		t.Fatalf("LineStart(0): %v", err)
	}
	if start != 0 {
		t.Errorf("LineStart(0) = %d, want 0", start)
	}
}

func TestNewIndexMultipleLines(t *testing.T) {
	content := []byte("a\nb\nc")
	idx := NewIndex(content, UTF8)
	if idx.LineCount() != 3 {
		t.Errorf("line count = %d, want 3", idx.LineCount())
	}
}

func TestOffsetToPosition(t *testing.T) {
	content := []byte("hello world")
	idx := NewIndex(content, UTF8)
	pos, err := idx.OffsetToPosition(content, 6)
	if err != nil {
		t.Fatalf("OffsetToPosition(6): %v", err)
	}
	if pos.Line != 0 || pos.Col != 6 {
		t.Errorf("pos = (%d, %d), want (0, 6)", pos.Line, pos.Col)
	}
}

func TestPositionToOffset(t *testing.T) {
	content := []byte("hello world")
	idx := NewIndex(content, UTF8)
	off, err := idx.PositionToOffset(content, 0, 6, UTF8)
	if err != nil {
		t.Fatalf("PositionToOffset(0,6): %v", err)
	}
	if off != 6 {
		t.Errorf("offset = %d, want 6", off)
	}
}

func TestCJKCharacters(t *testing.T) {
	content := []byte("\u4f60\u597d\u4e16\u754c")
	idx := NewIndex(content, UTF8)
	pos, err := idx.OffsetToPosition(content, 3)
	if err != nil {
		t.Fatalf("OffsetToPosition(3): %v", err)
	}
	_ = pos
}

func TestEmojiEncoding(t *testing.T) {
	content := []byte("a\xf0\x9f\x98\x80b")
	idx8 := NewIndex(content, UTF8)
	idx16 := NewIndex(content, UTF16)
	if idx8.LineCount() != 1 {
		t.Error("UTF-8: expected 1 line")
	}
	if idx16.LineCount() != 1 {
		t.Error("UTF-16: expected 1 line")
	}
	pos, err := idx16.OffsetToPosition(content, 1)
	if err != nil {
		t.Fatalf("UTF-16 OffsetToPosition(1): %v", err)
	}
	if pos.Col != 1 {
		t.Errorf("UTF-16 col at byte 1 = %d, want 1", pos.Col)
	}
}

func TestRoundTripProperty(t *testing.T) {
	content := []byte("hello world\n\u7b2c\u4e8c\u884c\n")
	for _, enc := range []Encoding{UTF8, UTF16} {
		idx := NewIndex(content, enc)
		for off := uint32(0); off <= idx.totalBytes; {
			pos, err := idx.OffsetToPosition(content, off)
			if err != nil {
				t.Fatalf("offset %d enc=%s: %v", off, enc, err)
			}
			got, err := idx.PositionToOffset(content, pos.Line, pos.Col, enc)
			if err != nil {
				t.Fatalf("roundtrip offset %d enc=%s: %v", off, enc, err)
			}
			if got != off {
				t.Errorf("offset %d -> pos(%d,%d) -> %d (want %d) enc=%s", off, pos.Line, pos.Col, got, off, enc)
			}
			// Advance to next rune boundary for INV-POS-001.
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
	}
}

func TestClamp(t *testing.T) {
	content := []byte("hello\nworld")
	idx := NewIndex(content, UTF8)
	pos := idx.Clamp(content, LSPPosition{Line: 0, Character: 999}, UTF8)
	if pos.Character != 5 {
		t.Errorf("clamp col = %d, want 5", pos.Character)
	}
}

func TestConvertPosition(t *testing.T) {
	content := []byte("abc")
	idx := NewIndex(content, UTF8)
	converted, err := idx.ConvertPosition(content, LSPPosition{Line: 0, Character: 2}, UTF8, UTF16)
	if err != nil {
		t.Fatalf("ConvertPosition: %v", err)
	}
	if converted.Character != 2 {
		t.Errorf("converted col = %d, want 2", converted.Character)
	}
}

func TestOutOfBoundsDoesNotPanic(t *testing.T) {
	content := []byte("hello")
	idx := NewIndex(content, UTF8)
	_, err := idx.OffsetToPosition(content, 999)
	if err == nil {
		t.Error("expected error for out-of-range offset")
	}
	_, err = idx.LineStart(999)
	if err == nil {
		t.Error("expected error for out-of-range line")
	}
}

func TestPropertyRoundTripDiverse(t *testing.T) {
	texts := [][]byte{
		[]byte("hello world"),
		[]byte("\u4f60\u597d\u4e16\u754c"),
		[]byte("hello \xf0\x9f\x98\x80 world"),
		[]byte("line1\rnline2\rn"),
		[]byte(""),
		[]byte("x"),
		[]byte("a\tb\tc\n"),
	}
	for _, text := range texts {
		for _, enc := range []Encoding{UTF8, UTF16} {
			idx := NewIndex(text, enc)
			for off := uint32(0); off <= idx.totalBytes; {
				pos, err := idx.OffsetToPosition(text, off)
				if err != nil {
					t.Fatalf("off=%d enc=%s: %v", off, enc, err)
				}
				got, err := idx.PositionToOffset(text, pos.Line, pos.Col, enc)
				if err != nil {
					t.Fatalf("roundtrip off=%d enc=%s: %v", off, enc, err)
				}
				if got != off {
					t.Errorf("off=%d -> pos(%d,%d) -> %d (want %d) enc=%s", off, pos.Line, pos.Col, got, off, enc)
				}
				// Advance to next rune boundary.
				if off < idx.totalBytes {
					_, s := utf8.DecodeRune(text[off:])
					if s == 0 {
						s = 1
					}
					off += uint32(s)
				} else {
					break
				}
			}
		}
	}
}

// TestRuneLength covers the previously-untested RuneLength function.
func TestRuneLength(t *testing.T) {
	// ASCII.
	if got := RuneLength([]byte("hello")); got != 5 {
		t.Errorf("RuneLength(\"hello\") = %d, want 5", got)
	}
	// Empty.
	if got := RuneLength([]byte("")); got != 0 {
		t.Errorf("RuneLength(\"\") = %d, want 0", got)
	}
	// CJK.
	if got := RuneLength([]byte("中文")); got != 2 {
		t.Errorf("RuneLength(\"中文\") = %d, want 2", got)
	}
	// Emoji (surrogate pair in UTF-16, but 1 rune in UTF-8).
	if got := RuneLength([]byte("\xf0\x9f\x98\x80")); got != 1 {
		t.Errorf("RuneLength(emoji) = %d, want 1", got)
	}
	// Mixed.
	if got := RuneLength([]byte("a\xc3\xa9\xe2\x82\xac")); got != 3 {
		t.Errorf("RuneLength(\"a\xc3\xa9\xe2\x82\xac\") = %d, want 3", got)
	}
}

// TestUTF16Len covers the previously-untested UTF16Len function.
func TestUTF16Len(t *testing.T) {
	// ASCII.
	if got := UTF16Len([]byte("hello")); got != 5 {
		t.Errorf("UTF16Len(\"hello\") = %d, want 5", got)
	}
	// Empty.
	if got := UTF16Len([]byte("")); got != 0 {
		t.Errorf("UTF16Len(\"\") = %d, want 0", got)
	}
	// Basic Multilingual Plane (no surrogate).
	if got := UTF16Len([]byte("\xc3\xa9")); got != 1 {
		t.Errorf("UTF16Len(\"e-acute\") = %d, want 1", got)
	}
	// Supplementary Plane (surrogate pair, e.g. emoji).
	if got := UTF16Len([]byte("\xf0\x9f\x98\x80")); got != 2 {
		t.Errorf("UTF16Len(emoji) = %d, want 2", got)
	}
	// Mixed ASCII + emoji.
	if got := UTF16Len([]byte("a\xf0\x9f\x98\x80b")); got != 4 {
		t.Errorf("UTF16Len(\"a[emoji]b\") = %d, want 4", got)
	}
	// CJK.
	if got := UTF16Len([]byte("中文")); got != 2 {
		t.Errorf("UTF16Len(\"中文\") = %d, want 2", got)
	}
}

// TestPositionToOffsetPastLineEndClampsWithinLine locks the clamp that keeps a
// past-the-end column inside its own line. Returning the next line's start made
// golang's validSourceMapRange accept an out-of-range end as legal, weakening
// the Y0-9/D12 gate that blocks edits on unmapped generated regions.
func TestPositionToOffsetPastLineEndClampsWithinLine(t *testing.T) {
	for _, enc := range []Encoding{UTF8, UTF16} {
		t.Run(enc.String(), func(t *testing.T) {
			content := []byte("hello\nworld")
			idx := NewIndex(content, enc)
			got, err := idx.PositionToOffset(content, 0, 6, enc)
			if err != nil {
				t.Fatalf("PositionToOffset(0,6): %v", err)
			}
			if got != 5 {
				t.Errorf("PositionToOffset(0,6) = %d, want 5 (end of line 0 content)", got)
			}
			if got >= uint32(len(content)) && content[got-1] == '\n' {
				t.Errorf("PositionToOffset(0,6) landed on the line terminator at %d", got)
			}
		})
	}
}
