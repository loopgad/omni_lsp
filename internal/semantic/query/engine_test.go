package query

import (
	"context"
	"fmt"
	"testing"
)

// TestK0_QueryMemoBounded pins §K0: the memo table never exceeds
// maxQueryEntries regardless of insertion volume.
func TestK0_QueryMemoBounded(t *testing.T) {
	e := NewEngine(1)
	for i := 0; i < maxQueryEntries+500; i++ {
		_, err := e.Query(context.Background(), Key{Kind: "hover", Workspace: "w", SnapshotRev: 1, BuildContext: "b", Subject: fmt.Sprintf("s%d", i)},
			DepSet{}, func(_ context.Context, _ Bindings) (any, DepSet, error) { return i, nil, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := len(e.entries); got > maxQueryEntries {
		t.Fatalf("memo table grew to %d (cap %d)", got, maxQueryEntries)
	}
	if e.Stats().Evictions == 0 {
		t.Fatal("expected eviction counter to advance")
	}
}
