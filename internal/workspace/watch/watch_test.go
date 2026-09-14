package watch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWatcherDetectsCreateModifyDelete(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, 20*time.Millisecond, nil)
	p.Scan() // baseline

	var mu sync.Mutex
	var evs []Event
	done := make(chan struct{})
	p.onChange = func(list []Event) {
		mu.Lock()
		evs = append(evs, list...)
		n := len(evs)
		mu.Unlock()
		select {
		case done <- struct{}{}:
		default:
		}
		_ = n
	}
	p.Start()
	defer p.Close()

	// modify + create
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("two-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, done, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return has(evs, "b.txt", Created) && has(evs, "a.txt", Modified)
	}, "create+modify")

	// delete
	if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, done, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return has(evs, "b.txt", Deleted)
	}, "delete")
}

func TestWatcherIgnoresVendoredAndBaselineSilence(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, 10*time.Millisecond, nil)
	p.Scan() // baseline (HEAD excluded)
	time.Sleep(30 * time.Millisecond)
	if evs := p.Scan(); len(evs) != 0 {
		t.Fatalf("quiet tree produced events: %+v", evs)
	}
}

func TestPollerStartAndCloseAreIdempotent(t *testing.T) {
	p := New(t.TempDir(), 10*time.Millisecond, nil)

	p.Start()
	p.Start()
	p.Close()
	p.Close()
}

func TestPollerCloseFromCallbackDoesNotDeadlock(t *testing.T) {
	root := t.TempDir()
	p := New(root, 10*time.Millisecond, nil)
	p.Scan()
	done := make(chan struct{})
	p.onChange = func([]Event) {
		p.Close()
		close(done)
	}
	p.Start()
	defer p.Close()

	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close from callback deadlocked")
	}
}

func TestPollerCloseAndWaitWaitsForManualScanCallback(t *testing.T) {
	p := New(t.TempDir(), 10*time.Millisecond, nil)
	p.Scan()
	entered := make(chan struct{})
	release := make(chan struct{})
	p.onChange = func([]Event) {
		close(entered)
		<-release
	}

	if err := os.WriteFile(filepath.Join(p.root, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	scanDone := make(chan struct{})
	go func() {
		p.Scan()
		close(scanDone)
	}()
	<-entered

	closeDone := make(chan struct{})
	go func() {
		p.CloseAndWait()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("CloseAndWait returned before callback completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("CloseAndWait did not return after callback completed")
	}
	<-scanDone
}

func waitFor(t *testing.T, ch chan struct{}, cond func() bool, what string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("timeout waiting for %s", what)
		}
	}
}

func has(evs []Event, path string, kind Kind) bool {
	for _, e := range evs {
		if filepath.Base(e.Path) == path && e.Kind == kind {
			return true
		}
	}
	return false
}
