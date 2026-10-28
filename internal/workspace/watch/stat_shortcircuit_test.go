package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Guards for audit circle 2 finding 5: the poller used to open + read +
// SHA-256 every regular file on EVERY interval (O(total workspace bytes)
// every 2s by default) even though it already records (mtime,size) in its
// fingerprints. The walk now reuses the previous digest whenever the stat
// signature is unchanged and only re-reads files whose fingerprint moved.
//
// Contract cut line: the stat signature IS the change signal. A content edit
// that preserves BOTH mtime and size is invisible to the poller — the same
// tradeoff every stat-based poller makes (TestWatcherDetectsSameSizeSameMtime
//ContentChange pinned the opposite, content-hash semantics, and is replaced
// with the new contract in watch_test.go).

// TestWatcherSkipsRereadForUnchangedStatSignature: content drift under an
// identical (mtime,size) signature must NOT produce an event (the digest is
// reused, the file is not re-read) — pre-fix the poller re-hashed content
// and DID report this as Modified.
func TestWatcherSkipsRereadForUnchangedStatSignature(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "quiet.go")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.Scan()

	baseline, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Restore the BASELINE mtime: the stat signature is then identical to
	// the baseline's (same size 3, same mtime), which is exactly the shape
	// the digest reuse must not re-read or report.
	if err := os.Chtimes(file, baseline.ModTime(), baseline.ModTime()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 3 || !info.ModTime().Equal(baseline.ModTime()) {
		t.Fatalf("fixture drifted: (size %d, mtime %v) vs baseline (size 3, mtime %v)", info.Size(), info.ModTime(), baseline.ModTime())
	}

	if evs := p.Scan(); len(evs) != 0 {
		t.Fatalf("content drift under an identical stat signature produced events %+v — the poller must reuse the fingerprint without re-reading", evs)
	}
}

// TestWatcherRereadsWhenStatSignatureMoves: a real content change that moves
// mtime (the editor/atomic-replace shape) is still detected, including the
// same-size case the old content-hash pin covered by legitimate means.
func TestWatcherRereadsWhenStatSignatureMoves(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "edited.go")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.Scan()

	if err := os.WriteFile(file, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(file, future, future); err != nil {
		t.Fatal(err)
	}

	evs := p.Scan()
	if len(evs) != 1 || evs[0].Kind != Modified {
		t.Fatalf("same-size content change with a moved mtime = %+v, want exactly one Modified", evs)
	}
}
