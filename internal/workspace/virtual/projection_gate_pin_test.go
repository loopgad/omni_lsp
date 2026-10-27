package virtual

import (
	"errors"
	"testing"

	"github.com/omnilsp/omni/internal/workspace/position"
)

// TestProjectorEditGateMatchesValidateEditSpan pins the snapshot gate used by
// VirtualToHostRange against the public §Y0/SEM-SAFE-001 gate
// (ValidateEditSpan in safety.go): on identical mapping generations both must
// agree on every span, so the projector-local copy of the rule cannot drift
// from the public one.
func TestProjectorEditGateMatchesValidateEditSpan(t *testing.T) {
	const doc = "0123456789abcdefghij"
	reg := New()
	registerToctouMapping(reg, false)
	registerToctouMapping(reg, true) // current generation: B
	proj, err := NewProjector(reg, vURI, []byte(doc), []byte(doc), position.UTF8)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	d, ok := reg.doc(vURI)
	if !ok {
		t.Fatal("mapping missing")
	}
	idx := position.NewIndex([]byte(doc), position.UTF8)
	for _, span := range [][2]uint32{{0, 20}, {0, 10}, {5, 12}, {10, 20}, {20, 20}, {12, 5}} {
		startPos, startErr := idx.OffsetToPosition([]byte(doc), span[0])
		endPos, endErr := idx.OffsetToPosition([]byte(doc), span[1])
		if startErr != nil || endErr != nil {
			t.Fatalf("offsets %v: %v %v", span, startErr, endErr)
		}
		start := position.LSPPosition{Line: startPos.Line, Character: startPos.Col}
		end := position.LSPPosition{Line: endPos.Line, Character: endPos.Col}
		want := ValidateEditSpan(reg, vURI, span[0], span[1])
		got := validateEditSpanDoc("virtual.Projector.VirtualToHostRange", d, span[0], span[1])
		_, _, rangeErr := proj.VirtualToHostRange(start, end)
		if !sameGateOutcome(want, got) {
			t.Errorf("span %v: snapshot gate = %v, public gate = %v (divergence)", span, got, want)
		}
		if !sameGateOutcome(want, rangeErr) {
			t.Errorf("span %v: VirtualToHostRange = %v, public gate = %v (divergence)", span, rangeErr, want)
		}
	}
}

func sameGateOutcome(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return errors.Is(a, ErrUnsafeGeneratedEdit) == errors.Is(b, ErrUnsafeGeneratedEdit) &&
		errors.Is(a, ErrUnsafeEditSpan) == errors.Is(b, ErrUnsafeEditSpan)
}
