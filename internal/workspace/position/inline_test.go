package position

// Guard for the index-free inline conversion path introduced to keep
// per-call full-document Index builds off the hot paths (§S18 ranged
// didChange, persistent reference result loops): OffsetOfLineCharInline must
// be indistinguishable from the Index-backed OffsetOfLineCharEncoding —
// same offsets, same errors, same error text — for every position of every
// sample, including out-of-range lines and columns.

import (
	"fmt"
	"testing"
)

func TestOffsetOfLineCharInlineMatchesIndexPath(t *testing.T) {
	encodings := []Encoding{UTF8, UTF16, UTF32}
	for _, src := range roundtripSamples() {
		content := []byte(src)
		// Probe two lines past the end and two columns past any width so the
		// error paths are compared too, not just the successes.
		for line := uint32(0); line <= uint32(len(src))+2; line++ {
			for col := uint32(0); col <= uint32(len(src))+2; col++ {
				for _, enc := range encodings {
					wantOff, wantErr := OffsetOfLineCharEncoding(content, line, col, enc)
					gotOff, gotErr := OffsetOfLineCharInline(content, line, col, enc)
					if (wantErr == nil) != (gotErr == nil) {
						t.Fatalf("%q %v %d:%d: index path err=%v, inline err=%v",
							src, enc, line, col, wantErr, gotErr)
					}
					if wantErr != nil && wantErr.Error() != gotErr.Error() {
						t.Fatalf("%q %v %d:%d: error text drift: index %q vs inline %q",
							src, enc, line, col, wantErr, gotErr)
					}
					if wantErr == nil && wantOff != gotOff {
						t.Fatalf("%q %v %d:%d: index path offset %d, inline offset %d",
							src, enc, line, col, wantOff, gotOff)
					}
				}
			}
		}
	}
}

// TestOffsetOfLineCharInlineRoundTrip pins INV-POS-001 on the inline path
// itself: every scalar boundary resolves back to its offset without an Index.
func TestOffsetOfLineCharInlineRoundTrip(t *testing.T) {
	for _, src := range roundtripSamples() {
		for off := 0; off <= len(src); off++ {
			line, col, err := LineCharAt([]byte(src), uint32(off))
			if err != nil {
				if !isInvalidBoundary(src, off) {
					t.Fatalf("LineCharAt(%q, %d): unexpected error: %v", src, off, err)
				}
				continue
			}
			back, err := OffsetOfLineCharInline([]byte(src), line, col, UTF16)
			if err != nil {
				t.Fatalf("OffsetOfLineCharInline(%q, %d:%d) from offset %d: %v", src, line, col, off, err)
			}
			if back != uint32(off) {
				t.Errorf("inline roundtrip %q offset %d -> %d:%d -> %d", src, off, line, col, back)
			}
		}
	}
}

// BenchmarkIndexOffsetOfLineChar is the pre-fix shape: one NewIndex build
// (full scan + 4 bytes of column map per content byte) per conversion.
func BenchmarkIndexOffsetOfLineChar(b *testing.B) {
	content := benchmarkDocument()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := OffsetOfLineCharEncoding(content, 400, 10, UTF16); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOffsetOfLineCharInline is the post-fix shape: scan to the target
// line, resolve the column within it, allocate nothing per content byte.
func BenchmarkOffsetOfLineCharInline(b *testing.B) {
	content := benchmarkDocument()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := OffsetOfLineCharInline(content, 400, 10, UTF16); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkDocument() []byte {
	// 1000 lines x ~64 bytes: large enough that a per-call full-document
	// build is visible, small enough for a fast benchmark.
	line := fmt.Sprintf("func example%04d(ctx context.Context, argA string, argB int) (*Result, error) {\n", 0)
	doc := make([]byte, 0, len(line)*1000)
	for i := 0; i < 1000; i++ {
		doc = append(doc, fmt.Sprintf("func example%04d(ctx context.Context, argA string, argB int) (*Result, error) {\n", i)...)
	}
	return doc
}
