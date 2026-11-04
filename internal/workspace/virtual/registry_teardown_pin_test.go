package virtual

import "testing"

// TestUnregisterAllDropsNotebooksToo pins bug round 23 A / BUG-2 at the
// registry level: UnregisterAll is documented as the CloseSession backstop —
// "no stale mapping survives into the next session" (bug round 20 A#11, 圈 4)
// — but it reset only the virtual table and left r.notebooks untouched, so a
// notebook registered through the embedder API outlived the session sweep.
// The teardown backstop must sweep BOTH tables.
func TestUnregisterAllDropsNotebooksToo(t *testing.T) {
	r := New()
	if err := r.RegisterNotebook("nb:///probe.ipynb", Notebook{
		OrderedCells: []Cell{{URI: "nb:///probe.ipynb#cell-1", LanguageID: "python", Version: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterVirtual("virtual:///probe.go#0", "file:///probe.go", []Segment{
		{HostStart: 0, HostEnd: 4, VirtStart: 0, VirtEnd: 4, Quality: QualityExact},
	}); err != nil {
		t.Fatal(err)
	}

	r.UnregisterAll()

	// Round-20 contract half: the virtual table is swept.
	if got := r.LookupVirtual("file:///probe.go"); len(got) != 0 {
		t.Errorf("virtual mapping survived UnregisterAll: %v", got)
	}
	// The half that was missed: the notebook table must be swept too.
	if _, ok := r.Notebook("nb:///probe.ipynb"); ok {
		t.Error("notebook SURVIVED UnregisterAll — teardown backstop must cover both tables")
	}
}
