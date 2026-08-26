// Package position implements the canonical position engine (goal.md D7-D8):
// UTF-8/UTF-16/UTF-32 conversions over document text with strict round-trip
// guarantees (INV-POS-001).
//
// Concurrency model: pure functions over immutable inputs; no shared state.
//
// Invariants:
//  1. Round trips are lossless: offset->line:col->offset is identity for
//     every valid position in every supported encoding.
//  2. Out-of-range positions fail closed with typed errors, never clamp silently.
//  3. Surrogate pairs and multi-byte runes are never split by conversion.
package position

import (
	"fmt"
	"unicode/utf8"
)

type Encoding int

const (
	UTF8 Encoding = iota
	UTF16
	UTF32
)

func (e Encoding) String() string {
	switch e {
	case UTF8:
		return "utf-8"
	case UTF16:
		return "utf-16"
	case UTF32:
		return "utf-32"
	default:
		return fmt.Sprintf("unknown(%d)", int(e))
	}
}

type Pos struct {
	Line   uint32
	Col    uint32
	Offset uint32
}

type Range struct{ Start, End Pos }

type LSPPosition struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

type LSPRange struct {
	Start LSPPosition `json:"start"`
	End   LSPPosition `json:"end"`
}

// Index holds precomputed line start offsets and an offset-to-column mapping.
type Index struct {
	lines      []uint32
	totalBytes uint32
	encoding   Encoding
	// offsetCol maps each byte offset to its column number (0-indexed).
	// Built during NewIndex, one entry per byte of content.
	offsetCol []uint32
}

func NewIndex(content []byte, enc Encoding) *Index {
	idx := &Index{
		lines:      []uint32{0},
		totalBytes: uint32(len(content)),
		encoding:   enc,
		offsetCol:  make([]uint32, len(content)),
	}
	col := uint32(0)
	for i := 0; i < len(content); {
		if content[i] == 10 {
			idx.offsetCol[i] = col
			col = 0
			i++
			idx.lines = append(idx.lines, uint32(i))
		} else if content[i] == 13 {
			idx.offsetCol[i] = col
			col = 0
			i++
			if i < len(content) && content[i] == 10 {
				idx.offsetCol[i] = 0
				i++
			}
			idx.lines = append(idx.lines, uint32(i))
		} else {
			r, s := utf8.DecodeRune(content[i:])
			var w uint32
			switch enc {
			case UTF8:
				w = uint32(s)
			case UTF16:
				if r <= 0xFFFF {
					w = 1
				} else {
					w = 2
				}
			case UTF32:
				w = 1
			default:
				w = uint32(s)
			}
			// Assign column to each byte of this rune.
			for j := 0; j < s && i+j < len(content); j++ {
				idx.offsetCol[i+j] = col
			}
			col += w
			i += s
		}
	}
	return idx
}

func (idx *Index) LineCount() int { return len(idx.lines) }

// UTF16ColumnAt returns the UTF-16 column of a byte offset within its line.
// Returns 0 for out-of-range offsets.
func (idx *Index) UTF16ColumnAt(offset uint32) uint32 {
	if int(offset) < len(idx.offsetCol) {
		return idx.offsetCol[offset]
	}
	return 0
}

func (idx *Index) LineStart(line uint32) (uint32, error) {
	if int(line) >= len(idx.lines) {
		return 0, fmt.Errorf("line %d out of range (total %d lines)", line, len(idx.lines))
	}
	return idx.lines[line], nil
}

func (idx *Index) OffsetToPosition(content []byte, offset uint32) (Pos, error) {
	if offset > idx.totalBytes {
		return Pos{}, fmt.Errorf("offset %d out of range (total %d bytes)", offset, idx.totalBytes)
	}
	if offset == idx.totalBytes {
		// EOF: position after last character.
		line := idx.findLine(offset)
		if line >= len(idx.lines) {
			line = len(idx.lines) - 1
		}
		var col uint32
		if offset > 0 {
			col = idx.offsetCol[offset-1]
			// Advance past the last character.
			_, s := utf8.DecodeRune(content[offset-uint32(1):])
			if s > 1 {
				// Multi-byte char at end; col already set to char start.
			}
			// Column after last char is the start col of last char + its width.
			// We need to compute it.
			r2, _ := utf8.DecodeRune(content[offset-uint32(1):])
			var w uint32
			switch idx.encoding {
			case UTF8:
				_, s2 := utf8.DecodeRune(content[offset-uint32(1):])
				w = uint32(s2)
			case UTF16:
				if r2 <= 0xFFFF {
					w = 1
				} else {
					w = 2
				}
			case UTF32:
				w = 1
			default:
				_, s2 := utf8.DecodeRune(content[offset-uint32(1):])
				w = uint32(s2)
			}
			col = idx.offsetCol[offset-1] + w
		}
		return Pos{Line: uint32(line), Col: col, Offset: offset}, nil
	}
	line := idx.findLine(offset)
	col := idx.offsetCol[offset]
	// Snap to first byte of the UTF-8 rune containing this offset.
	// Only snaps for actual multi-byte runes (s > 1); single-byte chars
	// and invalid sequences have col==offsetCol[prev] but should not snap.
	if offset > uint32(idx.lines[line]) {
		_, s := utf8.DecodeRune(content[offset-1:])
		if s > 1 {
			firstByte := offset
			for firstByte > uint32(idx.lines[line]) {
				_, ps := utf8.DecodeRune(content[firstByte-uint32(1):])
				if ps <= 1 {
					break
				}
				firstByte--
			}
			return Pos{Line: uint32(line), Col: col, Offset: firstByte}, nil
		}
	}
	return Pos{Line: uint32(line), Col: col, Offset: offset}, nil
}

func (idx *Index) PositionToOffset(content []byte, line, col uint32, enc Encoding) (uint32, error) {
	if int(line) >= len(idx.lines) {
		return 0, fmt.Errorf("line %d out of range (total %d lines)", line, len(idx.lines))
	}
	ls := idx.lines[line]
	var le uint32
	if int(line)+1 < len(idx.lines) {
		le = idx.lines[line+1]
	} else {
		le = uint32(len(content))
	}

	// Find first byte offset in [ls, le) with column == col.
	for i := ls; i < le; i++ {
		if idx.offsetCol[i] == col {
			// Snap to first byte of the UTF-8 rune, but only for multi-byte runes.
			// Single-byte chars (including null) that coincidentally share col
			// must NOT snap, or roundtrip fails for those offsets.
			_, s := utf8.DecodeRune(content[i:])
			if s > 1 {
				firstByte := uint32(i)
				for firstByte > ls {
					_, ps := utf8.DecodeRune(content[firstByte-uint32(1):])
					if ps <= 1 {
						break
					}
					firstByte--
				}
				return firstByte, nil
			}
			return uint32(i), nil
		}
	}
	// col is past the end of the line: return le.
	return le, nil
}

func (idx *Index) ConvertPosition(content []byte, pos LSPPosition, from, to Encoding) (LSPPosition, error) {
	if from == to {
		return pos, nil
	}
	// First convert from source encoding to byte offset.
	offset, err := idx.PositionToOffset(content, pos.Line, pos.Character, from)
	if err != nil {
		return LSPPosition{}, err
	}
	// Then compute column in target encoding.
	line := idx.findLine(offset)
	ls := idx.lines[line]
	col := uint32(0)
	i := int(ls)
	for i < int(offset) && i < len(content) {
		r, s := utf8.DecodeRune(content[i:])
		i += s
		switch to {
		case UTF8:
			col += uint32(s)
		case UTF16:
			if r <= 0xFFFF {
				col++
			} else {
				col += 2
			}
		case UTF32:
			col++
		default:
			col += uint32(s)
		}
	}
	return LSPPosition{Line: pos.Line, Character: col}, nil
}

func (idx *Index) Clamp(content []byte, pos LSPPosition, enc Encoding) LSPPosition {
	if len(idx.lines) == 0 {
		return LSPPosition{}
	}
	if int(pos.Line) >= len(idx.lines) {
		pos.Line = uint32(len(idx.lines) - 1)
	}
	ls := idx.lines[pos.Line]
	var le uint32
	if int(pos.Line)+1 < len(idx.lines) {
		le = idx.lines[pos.Line+1]
	} else {
		le = uint32(len(content))
	}
	lc := content[ls:le]
	if len(lc) > 0 && lc[len(lc)-1] == 10 {
		lc = lc[:len(lc)-1]
	}
	if len(lc) > 0 && lc[len(lc)-1] == 13 {
		lc = lc[:len(lc)-1]
	}
	ll := idx.columnWidth(lc, enc)
	if pos.Character > ll {
		pos.Character = ll
	}
	return pos
}

func (idx *Index) findLine(offset uint32) int {
	lo, hi := 0, len(idx.lines)-1
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if idx.lines[mid] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func (idx *Index) columnWidth(text []byte, enc Encoding) uint32 {
	switch enc {
	case UTF8:
		return uint32(len(text))
	case UTF16:
		var w uint32
		for len(text) > 0 {
			r, s := utf8.DecodeRune(text)
			text = text[s:]
			if r <= 0xFFFF {
				w++
			} else {
				w += 2
			}
		}
		return w
	case UTF32:
		var w uint32
		for len(text) > 0 {
			_, s := utf8.DecodeRune(text)
			text = text[s:]
			w++
		}
		return w
	default:
		return uint32(len(text))
	}
}

func RuneLength(b []byte) int { return utf8.RuneCount(b) }

func UTF16Len(b []byte) int {
	var n int
	for len(b) > 0 {
		r, s := utf8.DecodeRune(b)
		b = b[s:]
		if r <= 0xFFFF {
			n++
		} else {
			n += 2
		}
	}
	return n
}

// OffsetOfLineChar converts an LSP-style UTF-16 line/character position into a
// byte offset within content. The character must lie on a scalar boundary
// within the line content (terminator excluded); out-of-range positions
// return an error — never clamp (goal.md §D6, INV-POS-002).
func OffsetOfLineChar(content []byte, line, character uint32) (uint32, error) {
	idx := NewIndex(content, UTF16)
	return idx.OffsetOfLineChar(content, line, character)
}

// OffsetOfLineChar is the method form of OffsetOfLineChar using a prebuilt index.
func (idx *Index) OffsetOfLineChar(content []byte, line, character uint32) (uint32, error) {
	lineStart, err := idx.LineStart(line)
	if err != nil {
		return 0, err
	}
	lineEnd := uint32(len(content))
	if next, err := idx.LineStart(line + 1); err == nil {
		lineEnd = next
	}
	lc := content[lineStart:lineEnd]
	// Strip the line terminator (\n, \r\n, or lone \r).
	if n := len(lc); n > 0 && lc[n-1] == '\n' {
		lc = lc[:n-1]
		if n = len(lc); n > 0 && lc[n-1] == '\r' {
			lc = lc[:n-1]
		}
	} else if n > 0 && lc[n-1] == '\r' {
		lc = lc[:n-1]
	}
	off, err := utf16ColToOffset(lc, character)
	if err != nil {
		return 0, err
	}
	return lineStart + off, nil
}

// LineCharAt converts a byte offset within content to an LSP-style UTF-16
// line/character position (INV-POS-001 roundtrip direction). Offsets that
// fall inside a multi-byte rune are not logical boundaries and are rejected.
func LineCharAt(content []byte, offset uint32) (line, character uint32, err error) {
	if offset > uint32(len(content)) {
		return 0, 0, fmt.Errorf("offset %d out of range (total %d bytes)", offset, len(content))
	}
	if int(offset) < len(content) && content[offset]&0xC0 == 0x80 {
		return 0, 0, fmt.Errorf("offset %d falls inside a multi-byte rune", offset)
	}
	// The \n of a CRLF pair shares one line break with its \r: no unique
	// position exists, so it is not a valid logical boundary.
	if int(offset) < len(content) && content[offset] == '\n' && offset > 0 && content[offset-1] == '\r' {
		return 0, 0, fmt.Errorf("offset %d falls inside a CRLF terminator", offset)
	}
	idx := NewIndex(content, UTF16)
	line = uint32(idx.findLine(offset))
	if offset < uint32(len(content)) {
		return line, idx.offsetCol[offset], nil
	}
	// EOF: column is the UTF-16 width of the final line's content.
	start := idx.lines[line]
	end := uint32(len(content))
	lc := content[start:end]
	if n := len(lc); n > 0 && lc[n-1] == '\n' {
		lc = lc[:n-1]
		if n = len(lc); n > 0 && lc[n-1] == '\r' {
			lc = lc[:n-1]
		}
	} else if n > 0 && lc[n-1] == '\r' {
		lc = lc[:n-1]
	}
	return line, idx.columnWidth(lc, UTF16), nil
}

// utf16ColToOffset converts a UTF-16 code-unit column to a byte offset within
// one line's content. Errors when col falls inside a surrogate pair or past
// the end of the line.
func utf16ColToOffset(lineContent []byte, col uint32) (uint32, error) {
	var w uint32
	i := 0
	for i < len(lineContent) {
		if w == col {
			return uint32(i), nil
		}
		r, s := utf8.DecodeRune(lineContent[i:])
		if r <= 0xFFFF {
			w++
		} else {
			w += 2
		}
		i += s
	}
	if w == col {
		return uint32(i), nil
	}
	return 0, fmt.Errorf("character %d out of range (line width %d UTF-16 units)", col, w)
}
