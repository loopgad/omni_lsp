package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPollerExcludedNestedRootOmitsItsEntireSubtree(t *testing.T) {
	root := t.TempDir()
	indexRoot := filepath.Join(root, "build", "cache", "semantic-index")
	indexedFile := filepath.Join(indexRoot, "nested", "facts.bin")
	if err := os.MkdirAll(filepath.Dir(indexedFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexedFile, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := New(root, time.Hour, nil)
	if err := p.SetExcludedRoots(indexRoot); err != nil {
		t.Fatalf("configure index exclusion: %v", err)
	}
	p.Scan()
	if !p.HasBaseline() {
		t.Fatal("initial complete scan did not establish a baseline")
	}

	if err := os.WriteFile(indexedFile, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}
	newIndexFile := filepath.Join(indexRoot, "nested", "deeper", "more-facts.bin")
	if err := os.MkdirAll(filepath.Dir(newIndexFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newIndexFile, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if events := p.Scan(); len(events) != 0 {
		t.Fatalf("writes below configured index root escaped exclusion: %+v", events)
	}
	if err := p.LastScanError(); err != nil {
		t.Fatalf("excluded subtree made scan incomplete: %v", err)
	}
}

func TestPollerExcludedRootUsesPathBoundariesAndKeepsPeerDirectories(t *testing.T) {
	root := t.TempDir()
	indexRoot := filepath.Join(root, "cache", "nested", "index")
	paths := []string{
		filepath.Join(indexRoot, "facts.json"),
		filepath.Join(root, "packages", "index", "source.ts"),
		filepath.Join(root, "cache", "nested", "index-backup", "manifest.json"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	p := New(root, time.Hour, nil)
	if err := p.SetExcludedRoots(indexRoot); err != nil {
		t.Fatalf("configure index exclusion: %v", err)
	}
	p.Scan()

	for _, path := range paths {
		if err := os.WriteFile(path, []byte("after!"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	events := p.Scan()
	want := map[string]Kind{
		filepath.Join("packages", "index", "source.ts"):                   Modified,
		filepath.Join("cache", "nested", "index-backup", "manifest.json"): Modified,
	}
	if len(events) != len(want) {
		t.Fatalf("unexpected events around excluded root: got %+v, want exactly %+v", events, want)
	}
	for _, event := range events {
		if kind, ok := want[event.Path]; !ok || kind != event.Kind {
			t.Errorf("unexpected event: %+v", event)
		}
		delete(want, event.Path)
	}
	if len(want) != 0 {
		t.Errorf("missing source changes: %+v", want)
	}
}

func TestSetExcludedRootsRejectsWorkspaceAndMustPrecedeScanning(t *testing.T) {
	root := t.TempDir()
	ancestor := filepath.Dir(root)
	for _, excluded := range []string{root, ancestor} {
		p := New(root, time.Hour, nil)
		if err := p.SetExcludedRoots(excluded); err == nil {
			t.Errorf("SetExcludedRoots(%q) succeeded and would hide the workspace", excluded)
		}
	}

	scanned := New(root, time.Hour, nil)
	scanned.Scan()
	if err := scanned.SetExcludedRoots(filepath.Join(root, "index")); err == nil {
		t.Error("SetExcludedRoots succeeded after the first Scan")
	}

	started := New(root, time.Hour, nil)
	started.Start()
	defer started.CloseAndWait()
	if err := started.SetExcludedRoots(filepath.Join(root, "index")); err == nil {
		t.Error("SetExcludedRoots succeeded after Start")
	}
}
