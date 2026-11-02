package clang

import (
	"testing"

	"github.com/omnilsp/omni/internal/index/model"
)

// Pin for round20/bugB round-4 (boundary & truncation sweep): sourcePosition
// on a line number beyond the file's content must pin to EOF deterministically.
// Before the fix, the byte-scan loop left `start` wherever the last line
// break put it: a trailing newline advanced it past the final break (zero
// columns at EOF), while a file without one kept the last real line's start —
// the same out-of-range helper event then decoded the last line's content
// into a phantom line, differing between two files that differ only in their
// final byte. Both shapes now agree on StartChar=0 at EOF; every in-range
// line keeps its existing column semantics.
func newEofProbeWorkspace(data string) (*workspace, model.File) {
	uri := "file:///ws/a.cpp"
	ws := &workspace{
		uriRel:       map[string]string{uri: "a.cpp"},
		positionData: map[string][]byte{"a.cpp": []byte(data)},
	}
	return ws, model.File{URI: uri, Size: int64(len(data))}
}

func TestSourcePositionBeyondEOFPinsToEOF(t *testing.T) {
	wsTrailing, fileTrailing := newEofProbeWorkspace("ab\ncd\n")
	wsBare, fileBare := newEofProbeWorkspace("ab\ncd")
	for _, tc := range []struct {
		name string
		ws   *workspace
		file model.File
	}{
		{"trailing newline", wsTrailing, fileTrailing},
		{"no trailing newline", wsBare, fileBare},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pos := convertPosition(tc.ws, tc.file, 9, 2, 9, 4)
			if pos.StartLine != 8 || pos.EndLine != 8 {
				t.Fatalf("out-of-range line decoded to lines (%d,%d), want (8,8): %+v", pos.StartLine, pos.EndLine, pos)
			}
			if pos.StartChar != 0 || pos.EndChar != 0 {
				t.Fatalf("out-of-range line must pin to a zero-width position at EOF, got %+v", pos)
			}
		})
	}
}

func TestSourcePositionInRangeUnchangedByEOFPin(t *testing.T) {
	ws, file := newEofProbeWorkspace("ab\ncd")
	pos := convertPosition(ws, file, 2, 2, 2, 4)
	if pos.StartLine != 1 || pos.EndLine != 1 || pos.StartChar != 1 || pos.EndChar != 2 {
		t.Fatalf("in-range line 2 range = %+v, want start (1,1) end (1,2)", pos)
	}
	// The phantom EOF line of a trailing-newline file keeps its zero-width
	// position (line count includes the empty line after the final break).
	ws2, file2 := newEofProbeWorkspace("ab\n")
	pos2 := convertPosition(ws2, file2, 2, 1, 2, 1)
	if pos2.StartLine != 1 || pos2.EndLine != 1 || pos2.StartChar != 0 || pos2.EndChar != 0 {
		t.Fatalf("trailing-newline phantom line 2 range = %+v, want zero-width at (1,0)", pos2)
	}
	// An empty file pins every line to the zero-width position at offset 0.
	ws3, file3 := newEofProbeWorkspace("")
	pos3 := convertPosition(ws3, file3, 1, 1, 1, 1)
	if pos3.StartChar != 0 || pos3.EndChar != 0 {
		t.Fatalf("empty-file line 1 range = %+v, want zero chars", pos3)
	}
}
