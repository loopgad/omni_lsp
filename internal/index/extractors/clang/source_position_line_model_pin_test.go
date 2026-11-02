package clang

import (
	"testing"

	"github.com/omnilsp/omni/internal/index/model"
)

// Pin for round20/bugB bug 2: sourcePosition's (line,column)→UTF-16 column
// scan counted only '\n' as a line boundary, while libclang and the canonical
// position engine (internal/workspace/position lineSpan) treat '\r' and
// "\r\n" as line breaks too. On CR-only sources every line ≥ 2 event position
// was measured against line 1's byte prefix — a legal-looking but wrong
// position that sailed through the validated sink. Columns are relative to
// their own line's start, so the defect shows wherever line 1's prefix width
// differs from line 2's (here: the range reaches past line 1's end).
//
// Note on the round-3 evidence probe: convertPosition(2,1,2,5) over
// "alpha\rbeta\r" yields the same relative columns before and after the fix
// ("alph" and "beta" have equal UTF-16 width); its absolute-offset assertion
// contradicted the function's within-line column semantics. The cases below
// pin the actual manifestation.
func newCRWorkspace(data []byte) (*workspace, model.File) {
	uri := "file:///ws/classic.cpp"
	ws := &workspace{
		uriRel:       map[string]string{uri: "classic.cpp"},
		positionData: map[string][]byte{"classic.cpp": data},
	}
	return ws, model.File{URI: uri, Size: int64(len(data))}
}

func TestSourcePositionTreatsCRAsLineBoundary(t *testing.T) {
	ws, file := newCRWorkspace([]byte("abcdefghij\rxy\r"))
	// Line 2 is "xy": a range covering columns 1..5 (exclusive end 5) ends
	// after "xy" + the line break window — measured on line 2's own bytes the
	// end column is 3 ("xy\r" is 3 units from line 2's start), not 4 ("abcd",
	// line 1's prefix).
	pos := convertPosition(ws, file, 2, 1, 2, 5)
	if pos.StartLine != 1 || pos.EndLine != 1 {
		t.Fatalf("lines = (%d, %d), want (1, 1)", pos.StartLine, pos.EndLine)
	}
	if pos.StartChar != 0 {
		t.Fatalf("StartChar = %d, want 0 (column 1 of line 2)", pos.StartChar)
	}
	if pos.EndChar != 3 {
		t.Fatalf("EndChar = %d, want 3 measured from line 2's own start (byte prefix of line 1 gave %d)", pos.EndChar, pos.EndChar)
	}
}

func TestSourcePositionKeepsCRLFAndLFLineModels(t *testing.T) {
	crlf, crlfFile := newCRWorkspace([]byte("alpha\r\nbeta\r\n"))
	pos := convertPosition(crlf, crlfFile, 2, 1, 2, 5)
	if pos.StartChar != 0 || pos.EndChar != 4 {
		t.Fatalf("CRLF line 2 range = %+v, want StartChar=0 EndChar=4 (unchanged)", pos)
	}
	lf, lfFile := newCRWorkspace([]byte("alpha\nbeta\n"))
	pos = convertPosition(lf, lfFile, 2, 1, 2, 5)
	if pos.StartChar != 0 || pos.EndChar != 4 {
		t.Fatalf("LF line 2 range = %+v, want StartChar=0 EndChar=4 (unchanged)", pos)
	}
}
