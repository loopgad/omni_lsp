package watch

// §D14 file-operation transition evidence. The clause requires the watcher
// model to tolerate rename represented as delete+create, editor writes via
// atomic replace, and duplicate/missing events. These tests pin the exact
// event surface of each file-operation transition (create, modify, rename,
// delete, directory rename, atomic replace) so the semantic layer above can
// rely on delete+create for renames and a single Modified for atomic
// replaces.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherRenameSurfacesAsDeletePlusCreate(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "before.txt")
	if err := os.WriteFile(original, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.Scan()

	if err := os.Rename(original, filepath.Join(root, "after.txt")); err != nil {
		t.Fatal(err)
	}
	assertTransitionEvents(t, root, p.Scan(), map[string]Kind{
		"before.txt": Deleted,
		"after.txt":  Created,
	})

	// A rename must leave no stale fingerprint behind: the next quiet scan
	// reports neither a duplicate create nor a phantom modify.
	if again := p.Scan(); len(again) != 0 {
		t.Fatalf("rename left stale fingerprint entries: %+v", again)
	}
}

func TestWatcherDirectoryRenameSurfacesAsPerFileDeletePlusCreate(t *testing.T) {
	root := t.TempDir()
	oldDir := filepath.Join(root, "staging")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one.go", "two.go"} {
		if err := os.WriteFile(filepath.Join(oldDir, name), []byte("body of "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := New(root, time.Hour, nil)
	p.Scan()

	newDir := filepath.Join(root, "promoted")
	if err := os.Rename(oldDir, newDir); err != nil {
		t.Fatal(err)
	}
	assertTransitionEvents(t, root, p.Scan(), map[string]Kind{
		"staging/one.go":  Deleted,
		"staging/two.go":  Deleted,
		"promoted/one.go": Created,
		"promoted/two.go": Created,
	})
}

func TestWatcherAtomicReplaceSurfacesAsModifyNotDeletePlusCreate(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "editor.go")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.Scan()

	// Editor atomic replace (§D14): write a temporary sibling, then rename it
	// over the open file. The watcher must see one Modified for the same path,
	// never a delete+create pair that would read as buffer churn.
	temp := filepath.Join(root, "editor.go.tmp")
	if err := os.WriteFile(temp, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Windows timestamp coalescing (bug round 20 A#1): the temp file is
	// written milliseconds after the target, so NTFS lands both writes in
	// the same timestamp tick and the replacement's mtime equals the
	// baseline's (measured 19% of atomic replaces on the bug host). Force
	// that exact shape — same mtime, same size — so the guard is
	// deterministic instead of a ~5% flake.
	baseline, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(temp, baseline.ModTime(), baseline.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, target); err != nil {
		t.Fatal(err)
	}
	if replaced, rerr := os.Stat(target); rerr != nil {
		t.Fatal(rerr)
	} else if !replaced.ModTime().Equal(baseline.ModTime()) || replaced.Size() != baseline.Size() {
		t.Fatalf("fixture drifted: replaced (size %d, mtime %v) vs baseline (size %d, mtime %v)",
			replaced.Size(), replaced.ModTime(), baseline.Size(), baseline.ModTime())
	}
	assertTransitionEvents(t, root, p.Scan(), map[string]Kind{"editor.go": Modified})
}

func TestWatcherFullTransitionSequenceCreateModifyRenameDelete(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "anchor.txt"), []byte("anchor"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.Scan()

	created := filepath.Join(root, "candidate.txt")
	if err := os.WriteFile(created, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertTransitionEvents(t, root, p.Scan(), map[string]Kind{"candidate.txt": Created})

	if err := os.WriteFile(created, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertTransitionEvents(t, root, p.Scan(), map[string]Kind{"candidate.txt": Modified})

	renamed := filepath.Join(root, "renamed.txt")
	if err := os.Rename(created, renamed); err != nil {
		t.Fatal(err)
	}
	assertTransitionEvents(t, root, p.Scan(), map[string]Kind{
		"candidate.txt": Deleted,
		"renamed.txt":   Created,
	})

	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	assertTransitionEvents(t, root, p.Scan(), map[string]Kind{"renamed.txt": Deleted})

	// The full create→modify→rename→delete migration ends in silence, and no
	// transition along the way invalidated the scan.
	if quiet := p.Scan(); len(quiet) != 0 {
		t.Fatalf("completed transitions left residual events: %+v", quiet)
	}
	if err := p.LastScanError(); err != nil {
		t.Fatalf("transition scans were incomplete: %v", err)
	}
}

// assertTransitionEvents requires the scan to report exactly the wanted
// root-relative path→kind pairs (resolved to the absolute event paths): no
// missing events, no duplicates, no extras.
func assertTransitionEvents(t *testing.T, root string, events []Event, want map[string]Kind) {
	t.Helper()
	absoluteWant := make(map[string]Kind, len(want))
	for rel, kind := range want {
		absoluteWant[filepath.Join(root, rel)] = kind
	}
	want = absoluteWant
	got := make(map[string]Kind, len(events))
	for _, event := range events {
		if _, duplicate := got[event.Path]; duplicate {
			t.Errorf("duplicate event for %q: %+v", event.Path, event)
		}
		got[event.Path] = event.Kind
	}
	for path, kind := range want {
		gotKind, ok := got[path]
		if !ok {
			t.Errorf("missing kind %d event for %q; got %+v", kind, path, events)
			continue
		}
		if gotKind != kind {
			t.Errorf("event %q kind = %d, want %d", path, gotKind, kind)
		}
		delete(got, path)
	}
	for path, kind := range got {
		t.Errorf("unexpected kind %d event for %q", kind, path)
	}
}
