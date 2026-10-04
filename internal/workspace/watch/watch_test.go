package watch

import (
	"os"
	"path/filepath"
	"strings"
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

func TestWatcherSkipsRepositoryArtifactsButIncludesDependencies(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		".tmp-gocache/cache.bin",
		"dist/app.js",
		"dist-smoke/app.js",
		"test/acceptance/evidence/report.json",
		"test/acceptance/tools/bin/runner.exe",
		"test/acceptance/tools/node_modules/pkg/index.js",
		"test/acceptance/tools/.npm-cache/index.json",
		"editors/vscode/node_modules/pkg/index.js",
		"node_modules/pkg/index.js",
	}
	for _, rel := range paths {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	p := New(root, time.Hour, nil)
	p.Scan()
	for _, rel := range paths {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.WriteFile(path, []byte("after!"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	wantPath := filepath.Join("node_modules", "pkg", "index.js")
	evs := p.Scan()
	if len(evs) != 1 || evs[0].Path != wantPath || evs[0].Kind != Modified {
		t.Fatalf("unexpected artifact/dependency changes: %+v", evs)
	}
}

func TestWatcherDetectsSameSizeSameMtimeContentChange(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "same.txt")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.Scan()

	if err := os.WriteFile(path, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if evs := p.Scan(); !has(evs, "same.txt", Modified) {
		t.Fatalf("same-size, same-mtime content change was missed: %+v", evs)
	}
}

func TestIncompleteScanKeepsPreviousBaseline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.Scan()

	movedRoot := root + "-moved"
	if err := os.Rename(root, movedRoot); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := os.Stat(movedRoot); err == nil {
			_ = os.Rename(movedRoot, root)
		}
	}()
	if evs := p.Scan(); len(evs) != 0 {
		t.Fatalf("incomplete scan reported false changes: %+v", evs)
	}
	if err := p.LastScanError(); err == nil {
		t.Fatal("missing root scan did not report an error")
	}
	if err := os.Rename(movedRoot, root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if evs := p.Scan(); !has(evs, "existing.txt", Modified) || has(evs, "existing.txt", Created) {
		t.Fatalf("incomplete scan replaced the baseline: %+v", evs)
	}
	if err := p.LastScanError(); err != nil {
		t.Fatalf("successful scan retained previous error: %v", err)
	}
}

func TestSizeLimitsReportIncompleteAndPreserveBaseline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.maxFileBytes = 4
	p.maxTreeBytes = 6
	p.Scan()

	oversized := filepath.Join(root, "oversized.txt")
	if err := os.WriteFile(oversized, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if evs := p.Scan(); len(evs) != 0 {
		t.Fatalf("oversized-file scan reported events: %+v", evs)
	}
	if err := p.LastScanError(); err == nil || !strings.Contains(err.Error(), "file size") {
		t.Fatalf("oversized-file scan error = %v", err)
	}
	if !p.HasBaseline() {
		t.Fatal("oversized-file scan discarded the valid baseline")
	}
	if err := os.Remove(oversized); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if evs := p.Scan(); !has(evs, "existing.txt", Modified) {
		t.Fatalf("file-limit recovery lost the valid baseline: %+v", evs)
	}

	p.maxTreeBytes = 4
	other := filepath.Join(root, "other.txt")
	if err := os.WriteFile(other, []byte("xy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if evs := p.Scan(); len(evs) != 0 {
		t.Fatalf("oversized-tree scan reported events: %+v", evs)
	}
	if err := p.LastScanError(); err == nil || !strings.Contains(err.Error(), "workspace exceeds") {
		t.Fatalf("oversized-tree scan error = %v", err)
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("end"), 0o644); err != nil {
		t.Fatal(err)
	}
	if evs := p.Scan(); !has(evs, "existing.txt", Modified) {
		t.Fatalf("tree-limit recovery lost the valid baseline: %+v", evs)
	}
}

func TestDepthLimitReportsIncompleteScan(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "existing.txt")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.maxDepth = 2
	p.Scan()

	deep := root
	for i := 0; i <= p.maxDepth; i++ {
		deep = filepath.Join(deep, "level")
		if err := os.Mkdir(deep, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if evs := p.Scan(); len(evs) != 0 {
		t.Fatalf("depth-limited scan reported events: %+v", evs)
	}
	if err := p.LastScanError(); err == nil {
		t.Fatal("depth-limited scan did not report an error")
	}
	if err := os.RemoveAll(filepath.Join(root, "level")); err != nil {
		t.Fatal(err)
	}
	if evs := p.Scan(); !has(evs, "existing.txt", Modified) {
		t.Fatalf("depth-limited scan lost the valid baseline: %+v", evs)
	}
}

func TestStartEstablishesBaselineAndImmediateCloseWaits(t *testing.T) {
	p := New(t.TempDir(), time.Hour, nil)
	p.Start()
	defer p.CloseAndWait()
	waitForBaseline(t, p)
	p.CloseAndWait()

	closing := New(t.TempDir(), time.Hour, nil)
	closing.Start()
	done := make(chan struct{})
	go func() {
		closing.CloseAndWait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("CloseAndWait did not finish during initial scan")
	}
}

func TestErrorHandlerDeduplicatesAndReportsRecovery(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.Scan()
	reported := make(chan error, 4)
	p.SetErrorHandler(func(err error) { reported <- err })

	movedRoot := root + "-moved"
	if err := os.Rename(root, movedRoot); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := os.Stat(movedRoot); err == nil {
			_ = os.Rename(movedRoot, root)
		}
	}()
	p.Scan()
	select {
	case err := <-reported:
		if err == nil {
			t.Fatal("error callback received nil for failed scan")
		}
	case <-time.After(time.Second):
		t.Fatal("scan error was not reported")
	}
	p.Scan()
	select {
	case err := <-reported:
		t.Fatalf("unchanged error was reported again: %v", err)
	default:
	}

	if err := os.Rename(movedRoot, root); err != nil {
		t.Fatal(err)
	}
	p.Scan()
	select {
	case err := <-reported:
		if err != nil {
			t.Fatalf("recovery callback error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("scan recovery was not reported")
	}
	p.Scan()
	select {
	case err := <-reported:
		t.Fatalf("unchanged success was reported again: %v", err)
	default:
	}
}

func TestCloseAndWaitWaitsForErrorHandler(t *testing.T) {
	root := t.TempDir()
	p := New(root, time.Hour, nil)
	p.Scan()
	entered := make(chan struct{})
	release := make(chan struct{})
	p.SetErrorHandler(func(err error) {
		if err != nil {
			close(entered)
			<-release
		}
	})

	movedRoot := root + "-moved"
	if err := os.Rename(root, movedRoot); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := os.Stat(movedRoot); err == nil {
			_ = os.Rename(movedRoot, root)
		}
	}()
	scanDone := make(chan struct{})
	go func() {
		p.Scan()
		close(scanDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("scan error callback did not start")
	}

	closeDone := make(chan struct{})
	go func() {
		p.CloseAndWait()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("CloseAndWait returned before error callback completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("CloseAndWait did not finish after error callback completed")
	}
	<-scanDone
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
	if evs := p.Scan(); len(evs) != 0 {
		t.Fatalf("scan after close reported events: %+v", evs)
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

func waitForBaseline(t *testing.T, p *Poller) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer timer.Stop()
	defer ticker.Stop()
	for !p.HasBaseline() {
		select {
		case <-timer.C:
			t.Fatal("initial scan did not establish a baseline")
		case <-ticker.C:
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
