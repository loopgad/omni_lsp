package virtual

import (
	"errors"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/workspace/position"
)

// The registry mapping behind vURI can be hot-swapped between:
//   - A: one Exact segment covering [0,20) of the 20-byte single-line
//     document — the §Y0 gate admits the whole span;
//   - B: Exact [0,10) plus UnmappedGenerated [10,20) — the gate rejects the
//     whole span with ErrUnsafeGeneratedEdit.
//
// The only distinguishable outcomes for the full span are therefore success
// (gate and lookup both saw A) and ErrUnsafeGeneratedEdit (both saw B).
// ErrUnsafeEditSpan is unreachable when the gate and the segment lookup
// observe the SAME mapping — its appearance proves they saw different
// generations, which is the read-judge-reread TOCTOU of audit finding 4 that
// turned an admitted edit into a pseudo-rejection.

func registerToctouMapping(reg *Registry, b bool) {
	segs := []Segment{
		{HostStart: 0, HostEnd: 20, VirtStart: 0, VirtEnd: 20, Quality: QualityExact},
	}
	if b {
		segs = []Segment{
			{HostStart: 0, HostEnd: 10, VirtStart: 0, VirtEnd: 10, Quality: QualityExact},
			{HostStart: 0, HostEnd: 0, VirtStart: 10, VirtEnd: 20, Quality: QualityUnmappedGenerated},
		}
	}
	if err := reg.RegisterVirtual(vURI, hURI, segs); err != nil {
		panic(err)
	}
}

// TestVirtualToHostRangeGateAndLookupShareOneSnapshot hammers the range
// projection against a concurrent re-registration of the same virtual URI.
func TestVirtualToHostRangeGateAndLookupShareOneSnapshot(t *testing.T) {
	const doc = "0123456789abcdefghij"
	for attempt := 0; attempt < 3; attempt++ {
		reg := New()
		registerToctouMapping(reg, false)
		proj, err := NewProjector(reg, vURI, []byte(doc), []byte(doc), position.UTF8)
		if err != nil {
			t.Fatalf("NewProjector: %v", err)
		}

		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				registerToctouMapping(reg, false)
				registerToctouMapping(reg, true)
			}
		}()

		start := position.LSPPosition{Line: 0, Character: 0}
		end := position.LSPPosition{Line: 0, Character: 20}
		deadline := time.Now().Add(300 * time.Millisecond)
		for time.Now().Before(deadline) {
			_, _, err := proj.VirtualToHostRange(start, end)
			switch {
			case err == nil:
			case errors.Is(err, ErrUnsafeGeneratedEdit):
			default:
				close(stop)
				<-done
				t.Fatalf("range admitted by the gate was pseudo-rejected: %v", err)
			}
		}
		close(stop)
		<-done
	}
}
