package virtual

// Tests for goal.md §D12 (virtual documents / source maps), §Y0
// ("Generated/source-map gaps block unsafe edits") and the minimal §D13
// notebook registry support.

import (
	stderrors "errors"
	"reflect"
	"testing"
)

// fixture: one virtual doc over "file:///h.txt" with all four quality grades.
//
//	Exact              virt [0,13)   <-> host [0,13)
//	ManyToOne          virt [13,40)  ->  host [13,20)
//	OneToMany          virt [40,60)  <-  host [20,26)
//	UnmappedGenerated  virt [60,90)      (no provenance)
func fixture(t *testing.T) *Registry {
	t.Helper()
	reg := New()
	segs := []Segment{
		{HostStart: 0, HostEnd: 13, VirtStart: 0, VirtEnd: 13, Quality: QualityExact},
		{HostStart: 13, HostEnd: 20, VirtStart: 13, VirtEnd: 40, Quality: QualityManyToOne},
		{HostStart: 20, HostEnd: 26, VirtStart: 40, VirtEnd: 60, Quality: QualityOneToMany},
		{HostStart: 26, HostEnd: 26, VirtStart: 60, VirtEnd: 90, Quality: QualityUnmappedGenerated},
	}
	if err := reg.RegisterVirtual("file:///v.tmpl", "file:///h.txt", segs); err != nil {
		t.Fatalf("RegisterVirtual: %v", err)
	}
	return reg
}

// TestD12_RoundTripExactMapping — inside an Exact segment the bidirectional
// mapping is the identity, and out-of-bounds/unmapped probes report typed errors.
func TestD12_RoundTripExactMapping(t *testing.T) {
	reg := fixture(t)
	for _, vo := range []uint32{0, 1, 7, 12} {
		hostOff, q, err := ToHostOffset(reg, "file:///v.tmpl", vo)
		if err != nil || q != QualityExact {
			t.Fatalf("ToHostOffset(%d): off=%d q=%v err=%v", vo, hostOff, q, err)
		}
		if hostOff != vo {
			t.Errorf("ToHostOffset(%d) = %d, want identity", vo, hostOff)
		}
		back, qb, err := ToVirtualOffset(reg, "file:///h.txt", hostOff)
		if err != nil || qb != QualityExact {
			t.Fatalf("ToVirtualOffset(%d): off=%d q=%v err=%v", hostOff, back, qb, err)
		}
		if back != vo {
			t.Errorf("round trip %d -> %d -> %d, want identity", vo, hostOff, back)
		}
	}
	if _, _, err := ToHostOffset(reg, "file:///v.tmpl", 100); !stderrors.Is(err, ErrOffsetOutOfRange) {
		t.Errorf("out-of-bounds err = %v, want ErrOffsetOutOfRange", err)
	}
	if _, _, err := ToHostOffset(reg, "file:///v.tmpl", 70); !stderrors.Is(err, ErrUnmappedRegion) {
		t.Errorf("unmapped-region err = %v, want ErrUnmappedRegion", err)
	}
	if _, _, err := ToHostOffset(reg, "file:///nope.tmpl", 0); !stderrors.Is(err, ErrUnknownVirtual) {
		t.Errorf("unknown doc err = %v, want ErrUnknownVirtual", err)
	}
}

// TestD12_UnmappedGeneratedBlocksEdit — §Y0: edits touching an
// UnmappedGenerated segment must fail closed with ErrUnsafeGeneratedEdit;
// spans fully inside an Exact segment are allowed.
func TestD12_UnmappedGeneratedBlocksEdit(t *testing.T) {
	reg := fixture(t)
	cases := []struct {
		name       string
		start, end uint32
		want       error // nil means allowed
	}{
		{"inside generated", 61, 70, ErrUnsafeGeneratedEdit},
		{"crosses into generated", 55, 65, ErrUnsafeGeneratedEdit},
		{"exact interior", 2, 7, nil},
		{"exact full segment", 0, 13, nil},
		{"many-to-one interior", 14, 20, ErrUnsafeEditSpan},
		{"spans two segments", 5, 15, ErrUnsafeEditSpan},
	}
	for _, tc := range cases {
		err := ValidateEditSpan(reg, "file:///v.tmpl", tc.start, tc.end)
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s [%d,%d): err = %v, want allowed", tc.name, tc.start, tc.end, err)
		case tc.want != nil && !stderrors.Is(err, tc.want):
			t.Errorf("%s [%d,%d): err = %v, want %v", tc.name, tc.start, tc.end, err, tc.want)
		}
	}
}

// TestD12_QualityGradesReported — ManyToOne/OneToMany mappings carry their
// grade in both directions; UnmappedGenerated reports itself and refuses.
// Non-Exact grades make no identity guarantee (that is the point of the
// grading), so only the quality label and in-range results are asserted.
func TestD12_QualityGradesReported(t *testing.T) {
	reg := fixture(t)

	hostOff, q, err := ToHostOffset(reg, "file:///v.tmpl", 14)
	if err != nil || q != QualityManyToOne {
		t.Fatalf("many->one forward: off=%d q=%v err=%v", hostOff, q, err)
	}
	if hostOff < 13 || hostOff >= 20 {
		t.Errorf("many->one forward off = %d, want within host segment [13,20)", hostOff)
	}
	vo, q, err := ToVirtualOffset(reg, "file:///h.txt", 14)
	if err != nil || q != QualityManyToOne {
		t.Fatalf("many->one reverse: off=%d q=%v err=%v", vo, q, err)
	}

	hostOff, q, err = ToHostOffset(reg, "file:///v.tmpl", 41)
	if err != nil || q != QualityOneToMany {
		t.Fatalf("one->many forward: off=%d q=%v err=%v", hostOff, q, err)
	}
	if hostOff < 20 || hostOff >= 26 {
		t.Errorf("one->many forward off = %d, want within host segment [20,26)", hostOff)
	}
	vo, q, err = ToVirtualOffset(reg, "file:///h.txt", 21)
	if err != nil || q != QualityOneToMany {
		t.Fatalf("one->many reverse: off=%d q=%v err=%v", vo, q, err)
	}
	if vo < 40 || vo >= 60 {
		t.Errorf("one->many reverse vo = %d, want within virtual segment [40,60)", vo)
	}

	// Collapsing grades clamp into the host segment instead of overflowing it.
	hostOff, _, err = ToHostOffset(reg, "file:///v.tmpl", 39)
	if err != nil || hostOff != 19 {
		t.Errorf("clamped tail = %d err=%v, want 19", hostOff, err)
	}

	hoff, q, err := ToHostOffset(reg, "file:///v.tmpl", 75)
	if !stderrors.Is(err, ErrUnmappedRegion) || q != QualityUnmappedGenerated || hoff != 0 {
		t.Errorf("unmapped probe: off=%d q=%v err=%v", hoff, q, err)
	}
}

// TestD12_HostCloseCleansVirtual — UnregisterHost removes every virtual
// document derived from the host while unrelated documents survive.
func TestD12_HostCloseCleansVirtual(t *testing.T) {
	reg := New()
	for _, v := range []string{"file:///a.gen", "file:///b.gen"} {
		if err := reg.RegisterVirtual(v, "file:///host.go", nil); err != nil {
			t.Fatalf("RegisterVirtual(%s): %v", v, err)
		}
	}
	if err := reg.RegisterVirtual("file:///other.gen", "file:///else.go", nil); err != nil {
		t.Fatalf("RegisterVirtual(other): %v", err)
	}

	reg.UnregisterHost("file:///host.go")

	if _, err := reg.ResolveHost("file:///a.gen"); !stderrors.Is(err, ErrUnknownVirtual) {
		t.Errorf("ResolveHost(a.gen) err = %v, want ErrUnknownVirtual after host close", err)
	}
	if _, err := reg.ResolveHost("file:///b.gen"); !stderrors.Is(err, ErrUnknownVirtual) {
		t.Errorf("ResolveHost(b.gen) err = %v, want ErrUnknownVirtual after host close", err)
	}
	if got := reg.LookupVirtual("file:///host.go"); len(got) != 0 {
		t.Errorf("LookupVirtual(host.go) = %v, want empty", got)
	}
	if host, err := reg.ResolveHost("file:///other.gen"); err != nil || host != "file:///else.go" {
		t.Errorf("unrelated virtual lost: host=%q err=%v", host, err)
	}

	// Single-document unregister still works.
	reg.UnregisterVirtual("file:///other.gen")
	if reg.IsVirtual("file:///other.gen") {
		t.Error("other.gen still registered after UnregisterVirtual")
	}
}

// TestD13_NotebookRegistryRoundTrip — minimal §D13 model: ordered cells are
// stored and returned as given (no semantic guessing, no execution metadata).
func TestD13_NotebookRegistryRoundTrip(t *testing.T) {
	reg := New()
	nb := Notebook{OrderedCells: []Cell{
		{URI: "file:///nb.ipynb#cell0", LanguageID: "python", Version: 1},
		{URI: "file:///nb.ipynb#cell1", LanguageID: "markdown", Version: 3},
	}}
	if err := reg.RegisterNotebook("file:///nb.ipynb", nb); err != nil {
		t.Fatalf("RegisterNotebook: %v", err)
	}
	got, ok := reg.Notebook("file:///nb.ipynb")
	if !ok || !reflect.DeepEqual(got.OrderedCells, nb.OrderedCells) {
		t.Errorf("Notebook = %+v ok=%v, want stored cells", got, ok)
	}
	if _, ok := reg.Notebook("file:///missing.ipynb"); ok {
		t.Error("unknown notebook reported found")
	}
	reg.UnregisterNotebook("file:///nb.ipynb")
	if _, ok := reg.Notebook("file:///nb.ipynb"); ok {
		t.Error("notebook survived UnregisterNotebook")
	}
}
