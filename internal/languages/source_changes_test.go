package languages

import "testing"

func TestNormalizeSourceChangesCanonicalizesWithoutReordering(t *testing.T) {
	changes := []SourceChange{
		{URI: "FILE:///workspace/%61.py", Kind: SourceChangeDeleted},
		{URI: "file:///workspace/a.py", Kind: SourceChangeCreated},
		{URI: "file:///workspace/other.py", Kind: SourceChangeChanged},
	}
	got, err := NormalizeSourceChanges(changes)
	if err != nil {
		t.Fatalf("NormalizeSourceChanges: %v", err)
	}
	if len(got) != len(changes) {
		t.Fatalf("normalized changes = %d, want %d", len(got), len(changes))
	}
	want := []SourceChange{
		{URI: "file:///workspace/a.py", Kind: SourceChangeDeleted},
		{URI: "file:///workspace/a.py", Kind: SourceChangeCreated},
		{URI: "file:///workspace/other.py", Kind: SourceChangeChanged},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalized changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestNormalizeSourceChangesRejectsInvalidURIOrKind(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change SourceChange
	}{
		{name: "non-file uri", change: SourceChange{URI: "untitled:main.py", Kind: SourceChangeChanged}},
		{name: "invalid kind", change: SourceChange{URI: "file:///main.py", Kind: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizeSourceChanges([]SourceChange{tc.change}); err == nil {
				t.Fatal("invalid source change unexpectedly accepted")
			}
		})
	}
}
