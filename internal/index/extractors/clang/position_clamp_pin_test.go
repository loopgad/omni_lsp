package clang

import (
	"testing"

	"github.com/omnilsp/omni/internal/index/model"
)

// Pin for round20/bugB bug 1: helper events may omit the end column
// (type_relation and implementation edges carry no end_column; declaration
// occurrences whose clang_getRangeEnd is a null location decode endColumn=0).
// sourcePosition then returns 0 for the end while StartChar is positive, so
// the same-line range violated the validated sink invariant
// (internal/index/semantic/semantic.go validPosition: same-line requires
// EndChar >= StartChar) and every cpp/c typed-fact export died with
// ErrMalformedPayload "invalid edge record". Both position paths must clamp a
// degenerate same-line range to a zero-width range at the start:
//   - convertPosition, the file-backed path used when the position cache
//     holds the file;
//   - approximatePosition, the fallback used when the file is over
//     maxPositionCache, unreadable, or not valid UTF-8.
func sameLineRangeValid(pos model.Position) bool {
	// Mirrors semantic.validPosition (internal/index/semantic/semantic.go:628)
	// for the same-line shapes produced here; cannot import the semantic
	// package from the extractor without dragging the sink into the test.
	return pos.EndLine > pos.StartLine || (pos.EndLine == pos.StartLine && pos.EndChar >= pos.StartChar)
}

func TestConvertPositionClampsMissingEndColumnToStart(t *testing.T) {
	data := []byte("hello world\nsecond line\n")
	ws := &workspace{
		root:         t.TempDir(),
		uriRel:       map[string]string{"file:///ws/a.cpp": "a.cpp"},
		positionData: map[string][]byte{"a.cpp": data},
	}
	file := model.File{URI: "file:///ws/a.cpp", Size: int64(len(data))}

	pos := convertPosition(ws, file, 1, 7, 1, 0)
	if pos.StartChar != 6 {
		t.Fatalf("convertPosition StartChar = %d, want 6", pos.StartChar)
	}
	if !sameLineRangeValid(pos) {
		t.Fatalf("convertPosition emitted a same-line range the validated sink rejects (EndChar=%d < StartChar=%d): %+v", pos.EndChar, pos.StartChar, pos)
	}
	if pos.EndChar != pos.StartChar {
		t.Fatalf("convertPosition missing end column should clamp to a zero-width range at the start, got %+v", pos)
	}
}

func TestConvertPositionClampsMissingEndLineAndColumn(t *testing.T) {
	data := []byte("hello world\nsecond line\n")
	ws := &workspace{
		root:         t.TempDir(),
		uriRel:       map[string]string{"file:///ws/a.cpp": "a.cpp"},
		positionData: map[string][]byte{"a.cpp": data},
	}
	file := model.File{URI: "file:///ws/a.cpp", Size: int64(len(data))}

	// endLine=0 with endColumn=0 is the helper's "no extent" shape: endLine
	// defaults to the start line, so the degenerate range is same-line.
	pos := convertPosition(ws, file, 1, 7, 0, 0)
	if pos.StartLine != 0 || pos.EndLine != 0 {
		t.Fatalf("convertPosition lines = (%d, %d), want (0, 0)", pos.StartLine, pos.EndLine)
	}
	if !sameLineRangeValid(pos) {
		t.Fatalf("convertPosition emitted a same-line range the validated sink rejects (EndChar=%d < StartChar=%d): %+v", pos.EndChar, pos.StartChar, pos)
	}
}

func TestApproximatePositionClampsMissingEndColumn(t *testing.T) {
	// approximatePosition is the degraded path (position cache over budget,
	// read failure, non-UTF-8 file): it must satisfy the same invariant.
	pos := approximatePosition(1, 7, 1, 0)
	if pos.StartChar != 6 {
		t.Fatalf("approximatePosition StartChar = %d, want 6", pos.StartChar)
	}
	if !sameLineRangeValid(pos) {
		t.Fatalf("approximatePosition emitted a same-line range the validated sink rejects (EndChar=%d < StartChar=%d): %+v", pos.EndChar, pos.StartChar, pos)
	}
	if pos.EndChar != pos.StartChar {
		t.Fatalf("approximatePosition missing end column should clamp to a zero-width range at the start, got %+v", pos)
	}
}

func TestConvertPositionKeepsLegitimateRanges(t *testing.T) {
	data := []byte("hello world\nsecond line\n")
	ws := &workspace{
		root:         t.TempDir(),
		uriRel:       map[string]string{"file:///ws/a.cpp": "a.cpp"},
		positionData: map[string][]byte{"a.cpp": data},
	}
	file := model.File{URI: "file:///ws/a.cpp", Size: int64(len(data))}

	// A real same-line range must keep its own end.
	wide := convertPosition(ws, file, 1, 1, 1, 12)
	if wide.StartChar != 0 || wide.EndChar != 11 {
		t.Fatalf("convertPosition legitimate same-line range = %+v, want StartChar=0 EndChar=11", wide)
	}
	// A cross-line range is valid regardless of its end column; the clamp
	// must not rewrite it.
	cross := convertPosition(ws, file, 1, 1, 2, 0)
	if cross.StartLine != 0 || cross.EndLine != 1 {
		t.Fatalf("convertPosition cross-line range = %+v, want lines (0, 1)", cross)
	}
	if !sameLineRangeValid(cross) {
		t.Fatalf("convertPosition cross-line range rejected by invariant: %+v", cross)
	}
	// Same for the approximate path.
	approxWide := approximatePosition(1, 1, 1, 12)
	if approxWide.StartChar != 0 || approxWide.EndChar != 11 {
		t.Fatalf("approximatePosition legitimate same-line range = %+v, want StartChar=0 EndChar=11", approxWide)
	}
}
