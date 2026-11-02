package clang

import (
	"testing"
	"unicode/utf8"
)

// Pin for round20/bugB bug 4: sourcePosition computed the end offset as
// start + int(column-1) with no upper clamp. Helper event JSON is untrusted
// input (decoded straight into rawEvent.Line/Column), and column=0xFFFFFFFF
// wrapped int(column-1) negative on 32-bit GOARCH, so data[start:end] sliced
// with a negative bound and panicked:
//
//	GOARCH=386 go test ./internal/index/extractors/clang/ -run TestSourcePositionHugeColumnStaysBounded
//	panic: runtime error: slice bounds out of range [:-2]  (pre-fix, at extractor.go sourcePosition)
//
// On 64-bit GOARCH the same input did not panic but silently measured the
// whole remaining file. The column is clamped with 64-bit math so the slice
// end always stays in [start, len(data)] on every GOARCH.
func TestSourcePositionHugeColumnStaysBounded(t *testing.T) {
	data := []byte("alpha\nbeta\n")
	total := uint32(utf8.RuneCountInString(string(data)))
	for _, column := range []uint32{0, 1, 2, 0xFFFFFFFF} {
		got := sourcePosition(data, 1, column)
		if got > total {
			t.Fatalf("sourcePosition(line=1, column=%#x) = %d exceeds the file's %d UTF-16 units", column, got, total)
		}
	}
	// Preserved edge semantics: column 0 is the "no position" sentinel and
	// column 1 is the line's first character (zero-width offset 0).
	if got := sourcePosition(data, 1, 0); got != 0 {
		t.Fatalf("sourcePosition(column=0) = %d, want 0", got)
	}
	if got := sourcePosition(data, 1, 1); got != 0 {
		t.Fatalf("sourcePosition(column=1) = %d, want 0", got)
	}
	if got := sourcePosition(data, 1, 2); got != 1 {
		t.Fatalf("sourcePosition(column=2) = %d, want 1", got)
	}
}
