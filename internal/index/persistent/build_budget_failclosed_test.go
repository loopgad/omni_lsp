package persistent

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

// Guards the audit finding "budget walk is fail-open": walkLiveBytes used to
// swallow every filepath.Walk error (`if err != nil { return nil }`), so an
// entry the filesystem refuses to report (ACL, AV interference) shrank the
// counted `used` total and let a write through that the §L17 budget could not
// actually verify. §L17 says refuse, never fill the disk: an unaccounted
// subtree must refuse the write (fail-closed), not undercount it.
//
// The error is injected through the budgetWalkFS seam because no portable way
// exists to make ReadDir/Lstat fail on a live directory across Windows ACLs
// and POSIX permission bits; filepath.Walk's error delivery itself is stdlib
// behavior.

func TestBudgetWalkErrorRefusesWrite(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, Config{DiskBudgetBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	orig := budgetWalkFS
	t.Cleanup(func() { budgetWalkFS = orig })
	walks := 0
	budgetWalkFS = func(root string, fn filepath.WalkFunc) error {
		walks++
		// Same shape a real walk delivers for an unreadable entry.
		return fn(filepath.Join(root, "unreadable-entry"), nil, fs.ErrPermission)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := s.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Abort()
	if _, err := b.WriteSegment([]byte("payload")); !errors.Is(err, ErrDiskBudgetExceeded) {
		t.Fatalf("walk error must refuse the write (§L17 fail-closed), got %v", err)
	}
	// The refusal is cached with the walk itself: the build's later segments
	// are refused too, without paying a walk per segment (circle-2 finding 6).
	if _, err := b.WriteSegment([]byte("payload")); !errors.Is(err, ErrDiskBudgetExceeded) {
		t.Fatalf("cached walk error must refuse later segments too, got %v", err)
	}
	if walks != 1 {
		t.Fatalf("budget walk ran %d times for one build, want 1", walks)
	}
}

// TestBudgetWalkSkippedWhenBudgetUnlimited pins the other accepted half: with
// DiskBudgetBytes = 0 there is nothing to compare the walk against, so the
// walk (and its error) must not block the write.
func TestBudgetWalkSkippedWhenBudgetUnlimited(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, Config{DiskBudgetBytes: 0})
	if err != nil {
		t.Fatal(err)
	}
	orig := budgetWalkFS
	t.Cleanup(func() { budgetWalkFS = orig })
	budgetWalkFS = func(root string, fn filepath.WalkFunc) error {
		return fn(filepath.Join(root, "unreadable-entry"), nil, fs.ErrPermission)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := s.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Abort()
	if _, err := b.WriteSegment([]byte("payload")); err != nil {
		t.Fatalf("unlimited budget must skip the walk entirely, got %v", err)
	}
}
