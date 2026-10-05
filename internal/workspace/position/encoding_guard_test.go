package position

import (
	"strings"
	"testing"
)

// TestPositionToOffsetRejectsForeignEncoding covers the guard that makes the
// enc argument mean something.
//
// PositionToOffset used to accept an Encoding and ignore it: the function
// resolves columns against offsetCol, which records the unit the Index was
// built with. Passing any other unit produced an offset for the wrong column,
// with no error, because the argument was decorative. Now a mismatched unit is
// rejected. No production caller did this -- the two in golang build the Index
// with UTF16 and ask for UTF16 -- so the guard is for the next caller, and a
// guard nobody exercises is not a guard.
func TestPositionToOffsetRejectsForeignEncoding(t *testing.T) {
	// "é" is one UTF-16 unit and two UTF-8 bytes, so column 2 is the boundary
	// where the two units disagree and a silent mismatch would be visible.
	content := []byte("héllo\nworld")

	for _, built := range []Encoding{UTF8, UTF16, UTF32} {
		t.Run("index built for "+built.String(), func(t *testing.T) {
			idx := NewIndex(content, built)
			for _, asked := range []Encoding{UTF8, UTF16, UTF32} {
				if asked == built {
					continue
				}
				got, err := idx.PositionToOffset(content, 0, 2, asked)
				if err == nil {
					t.Errorf("PositionToOffset(0,2,%v) on a %v index returned %d, "+
						"want an error rather than an offset in the wrong unit",
						asked, built, got)
					continue
				}
				if !strings.Contains(err.Error(), built.String()) {
					t.Errorf("error %q does not name the index encoding %v, "+
						"so the caller cannot tell which unit to rebuild with",
						err, built)
				}
			}
			// The matching unit still works, so the guard is not simply
			// rejecting everything.
			if _, err := idx.PositionToOffset(content, 0, 2, built); err != nil {
				t.Errorf("PositionToOffset(0,2,%v) on a matching %v index: %v",
					built, built, err)
			}
		})
	}
}

// TestConvertPositionRejectsForeignSource pins the same contract one level up:
// ConvertPosition forwards `from` to PositionToOffset, so a source encoding
// that disagrees with the index cannot be converted either.
func TestConvertPositionRejectsForeignSource(t *testing.T) {
	content := []byte("héllo\nworld")
	idx := NewIndex(content, UTF16)

	if _, err := idx.ConvertPosition(content,
		LSPPosition{Line: 0, Character: 2}, UTF8, UTF16); err == nil {
		t.Error("ConvertPosition from UTF8 on a UTF16 index succeeded; " +
			"the source column would have been read in the wrong unit")
	}
	// from == to still short-circuits before any of this, so it is unaffected.
	if _, err := idx.ConvertPosition(content,
		LSPPosition{Line: 0, Character: 2}, UTF16, UTF16); err != nil {
		t.Errorf("ConvertPosition from UTF16 to UTF16 on a UTF16 index: %v", err)
	}
}
