package nested

import (
	"testing"

	"github.com/omnilsp/omni/internal/workspace/position"
)

// TestConvertDiagnosticPositions pins the diagnostic range conversion that
// every nested bridge applies before publishing positions.
//
// Two things made this worth locking. First, the production default is
// UTF-16, and ConvertPosition returns early when from == to, so on the default
// path the ranges are forwarded verbatim -- including out-of-range columns. Only
// an explicit UTF-8 or UTF-32 negotiation reaches the byte-offset path. Second,
// ConvertPosition keeps pos.Line and recomputes only the column, so a range
// that ends on a later line must stay on that line: a clamped end that jumped to
// the next line would make the range cover source the diagnostic never mentioned,
// which is the §Y0-9/D12 gate on unmapped generated regions.
//
// Line 0 of content is "héllo": 6 UTF-8 bytes, 5 UTF-16 code units. Line 1
// starts at byte 7.
func TestConvertDiagnosticPositions(t *testing.T) {
	content := []byte("héllo\nworld")
	const bmpLine0End = 5  // UTF-16 units on line 0
	const utf8Line0End = 6 // UTF-8 bytes on line 0

	for _, tc := range []struct {
		name        string
		target      int
		in          Diagnostic
		wantStartCh uint32
		wantEndLine uint32
		wantEndChar uint32
	}{
		{
			name: "utf16 target is identity, the production default",
			// targetEncoding 1 == position.UTF16; from == to returns early, so
			// even an out-of-range column survives untouched.
			target:      int(position.UTF16),
			in:          Diagnostic{StartLine: 0, StartChar: 2, EndLine: 0, EndChar: 999},
			wantStartCh: 2,
			wantEndLine: 0,
			wantEndChar: 999,
		},
		{
			name:        "utf8 target converts columns",
			target:      int(position.UTF8),
			in:          Diagnostic{StartLine: 0, StartChar: 2, EndLine: 0, EndChar: bmpLine0End},
			wantStartCh: 3,
			wantEndLine: 0,
			wantEndChar: utf8Line0End,
		},
		{
			name: "out of range column clamps inside its own line",
			// The end must land at the end of line 0, not at the start of line 1.
			target:      int(position.UTF8),
			in:          Diagnostic{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 999},
			wantStartCh: 0,
			wantEndLine: 0,
			wantEndChar: utf8Line0End,
		},
		{
			name:        "a cross line range keeps both endpoints on their own lines",
			target:      int(position.UTF8),
			in:          Diagnostic{StartLine: 0, StartChar: 2, EndLine: 1, EndChar: 3},
			wantStartCh: 3,
			wantEndLine: 1,
			wantEndChar: 3,
		},
		{
			name:        "utf32 target counts runes",
			target:      int(position.UTF32),
			in:          Diagnostic{StartLine: 0, StartChar: 2, EndLine: 0, EndChar: bmpLine0End},
			wantStartCh: 2,
			wantEndLine: 0,
			wantEndChar: 5,
		},
		{
			name: "an unrecognised encoding falls back to utf16",
			// No default case in the switch, so target stays position.UTF16 and
			// the early return applies. Anything else would be a silent guess.
			target:      99,
			in:          Diagnostic{StartLine: 0, StartChar: 2, EndLine: 1, EndChar: 3},
			wantStartCh: 2,
			wantEndLine: 1,
			wantEndChar: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ConvertDiagnosticPositions(content, []Diagnostic{tc.in}, tc.target)
			if err != nil {
				t.Fatalf("ConvertDiagnosticPositions: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("got %d diagnostics, want 1", len(got))
			}
			if got[0].StartLine != tc.in.StartLine || got[0].StartChar != tc.wantStartCh {
				t.Errorf("start = %d:%d, want %d:%d",
					got[0].StartLine, got[0].StartChar, tc.in.StartLine, tc.wantStartCh)
			}
			if got[0].EndLine != tc.wantEndLine || got[0].EndChar != tc.wantEndChar {
				t.Errorf("end = %d:%d, want %d:%d",
					got[0].EndLine, got[0].EndChar, tc.wantEndLine, tc.wantEndChar)
			}
		})
	}
}

// TestConvertDiagnosticPositionsAstralColumns covers a surrogate pair, the one
// case where UTF-16 and UTF-8 disagree by more than a per-rune factor: the emoji
// is 2 UTF-16 code units and 4 UTF-8 bytes, so every column after it shifts by 2.
func TestConvertDiagnosticPositionsAstralColumns(t *testing.T) {
	content := []byte("a\U0001F600b\nx")
	// Line 0 is a + emoji + b: 3 UTF-16 code units, 6 UTF-8 bytes, line 1 at byte 7.
	for _, tc := range []struct {
		name        string
		utf16Col    uint32
		wantUTF8Col uint32
	}{
		{"before the pair", 1, 1},
		{"after the pair", 3, 5},
		{"end of line", 5, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Diagnostic{StartLine: 0, StartChar: tc.utf16Col, EndLine: 0, EndChar: tc.utf16Col}
			got, err := ConvertDiagnosticPositions(content, []Diagnostic{in}, int(position.UTF8))
			if err != nil {
				t.Fatalf("ConvertDiagnosticPositions: %v", err)
			}
			if got[0].StartChar != tc.wantUTF8Col {
				t.Errorf("utf16 col %d became utf8 col %d, want %d",
					tc.utf16Col, got[0].StartChar, tc.wantUTF8Col)
			}
		})
	}
}

// TestConvertDiagnosticPositionsEmptyInput pins that no diagnostics yields an
// allocated empty slice: the result is copied into the published payload, and a
// nil slice there would serialise as null instead of [].
func TestConvertDiagnosticPositionsEmptyInput(t *testing.T) {
	got, err := ConvertDiagnosticPositions([]byte("x\ny"), nil, int(position.UTF8))
	if err != nil {
		t.Fatalf("ConvertDiagnosticPositions: %v", err)
	}
	if got == nil {
		t.Error("nil result serialises as null, not []")
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}
