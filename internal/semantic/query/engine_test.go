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

func TestK2_DepIndexCleansEvictedKeys(t *testing.T) {
	dep := Dep{Kind: "file", ID: "hash-1"}
	e := NewEngine(1)
	for i := 0; i < maxQueryEntries+32; i++ {
		_, err := e.Query(context.Background(), Key{Kind: "hover", Workspace: "w", SnapshotRev: 1, BuildContext: "b", Subject: fmt.Sprintf("s%d", i)},
			DepSet{dep: {}}, func(_ context.Context, _ Bindings) (any, DepSet, error) { return i, nil, nil })
		if err != nil {
			t.Fatal(err)
		}
	}

	for d, keys := range e.depIndex {
		for k := range keys {
			if _, ok := e.entries[k]; !ok {
				t.Fatalf("stale dependency index entry %q for dep %s after eviction", k, d)
			}
		}
	}

	for i := 0; i < 10; i++ {
		_, err := e.Query(context.Background(), Key{Kind: "hover", Workspace: "w", SnapshotRev: 1, BuildContext: "b", Subject: fmt.Sprintf("old-%d", i)},
			DepSet{dep: {}}, func(_ context.Context, _ Bindings) (any, DepSet, error) { return i, nil, nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := e.InvalidateSnapshot(1); n == 0 {
		t.Fatal("expected snapshot invalidation to evict at least one entry for revision 1")
	}
	for d, keys := range e.depIndex {
		for k := range keys {
			if _, ok := e.entries[k]; !ok {
				t.Fatalf("stale dependency index entry %q for dep %s after snapshot invalidation", k, d)
			}
		}
	}
}

func TestInvalidateSnapshotDoesNotMatchSerializedFields(t *testing.T) {
	e := NewEngine(0)
	keyWithEmbeddedRevision := Key{
		Kind: "hover", Workspace: "w", SnapshotRev: 2,
		BuildContext: "b", Subject: "file|1|embedded",
	}
	if _, err := e.Query(context.Background(), keyWithEmbeddedRevision, nil,
		func(_ context.Context, _ Bindings) (any, DepSet, error) { return "value", nil, nil }); err != nil {
		t.Fatal(err)
	}
	if got := e.InvalidateSnapshot(1); got != 0 {
		t.Fatalf("invalidated %d unrelated entries, want 0", got)
	}
	if _, err := e.Query(context.Background(), keyWithEmbeddedRevision, nil,
		func(_ context.Context, _ Bindings) (any, DepSet, error) { return "recomputed", nil, nil }); err != nil {
		t.Fatal(err)
	}
	if stats := e.Stats(); stats.Computations != 1 {
		t.Fatalf("collision caused recomputation: computations=%d, want 1", stats.Computations)
	}
}
