package jsonrpc

import (
	"errors"
	"runtime"
	"strings"
	"testing"
)

// Guard for audit circle 2 finding 1: a batch frame's element count must be
// separately capped (§C1 — no unbounded allocation from attacker-controlled
// shape). Pre-fix a frame of tiny invalid elements amplified memory hundreds
// of times (measured 621x cumulative / 253x live on a 1MiB frame) and owed
// one outbound reply per invalid element (108x output amplification). The
// cap turns an oversize batch into a whole-frame ParseError — the same
// per-stream fatal class as a broken array — which also bounds replies per
// frame to the element cap.

// buildBatch returns a framed batch body of n tiny invalid elements.
func buildBatch(n int) string {
	return "[" + strings.TrimSuffix(strings.Repeat(`1,`, n), `,`) + `]`
}

func TestReadFrameOversizeBatchIsWholeFrameParseError(t *testing.T) {
	c := NewCodec()
	fr, err := c.ReadFrame(strings.NewReader(frame(buildBatch(DefaultMaxBatchElements + 1))))
	if err == nil {
		t.Fatalf("batch with %d elements classified as %+v, want a whole-frame rejection", DefaultMaxBatchElements+1, fr)
	}
	var codecErr *CodecError
	if !errors.As(err, &codecErr) || codecErr.Code != ParseError {
		t.Fatalf("error = %v (%T), want *CodecError{ParseError} — the per-stream fatal class", err, err)
	}
	if fr != nil {
		t.Fatalf("oversize batch must not produce a frame: %+v", fr)
	}

	// The cap is inclusive: exactly the cap still decodes as a batch.
	fr, err = c.ReadFrame(strings.NewReader(frame(buildBatch(DefaultMaxBatchElements))))
	if err != nil {
		t.Fatalf("batch with exactly %d elements: %v", DefaultMaxBatchElements, err)
	}
	if fr.Msg != nil || len(fr.Elements) != DefaultMaxBatchElements {
		t.Fatalf("cap-sized batch classified as msg=%v elements=%d", fr.Msg, len(fr.Elements))
	}
}

// TestReadFrameOversizeBatchAllocationBounded pins the §C1 bound: processing
// an oversize batch must cost memory proportional to the frame, not to the
// element count times per-element reply machinery. The bound is generous
// (200x the frame bytes — the codec still must unmarshal the array once to
// count it); pre-fix the per-element Message/Reply/Sprintf amplification ran
// ~250x per element batch and ~600x cumulative on dense frames.
func TestReadFrameOversizeBatchAllocationBounded(t *testing.T) {
	const elements = 50000
	framed := frame(buildBatch(elements))
	inSize := len(framed)

	c := NewCodec()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 5; i++ {
		_, err := c.ReadFrame(strings.NewReader(framed))
		var codecErr *CodecError
		if !errors.As(err, &codecErr) || codecErr.Code != ParseError {
			t.Fatalf("iteration %d: error = %v, want whole-frame ParseError", i, err)
		}
	}
	runtime.ReadMemStats(&after)
	amp := after.TotalAlloc - before.TotalAlloc
	if amp > 200*uint64(inSize) {
		t.Fatalf("oversize batch frame of %d bytes caused %d bytes cumulative allocation (%dx) — element cap must bound decode work",
			inSize, amp, amp/uint64(inSize))
	}
}

func TestReadFrameBatchCapIsConfigurable(t *testing.T) {
	c := &Codec{MaxBatchElements: 4}
	if _, err := c.ReadFrame(strings.NewReader(frame(buildBatch(5)))); err == nil {
		t.Fatal("frame over the configured cap must be rejected")
	} else {
		var codecErr *CodecError
		if !errors.As(err, &codecErr) || codecErr.Code != ParseError {
			t.Fatalf("error = %v (%T), want *CodecError{ParseError}", err, err)
		}
	}
	fr, err := c.ReadFrame(strings.NewReader(frame(buildBatch(4))))
	if err != nil {
		t.Fatalf("frame at the configured cap: %v", err)
	}
	if len(fr.Elements) != 4 {
		t.Fatalf("elements = %d, want 4", len(fr.Elements))
	}
}
