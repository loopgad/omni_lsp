package persistent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Pin for round20/bugB bugs 2/6: a build session that dies between BeginBuild
// and Commit/Abort leaves staging-<id>/ with fsynced segments, and nothing
// ever reclaimed it — cleanupOrphanSegments only sweeps root seg-*.bin files
// — while walkLiveBytes counts everything under root except quarantine, so
// the orphan bytes were permanently billed against the §L17 disk budget: one
// crash shrank publishable capacity forever and a budgeted store eventually
// refused every WriteSegment with ErrDiskBudgetExceeded.
//
// Reclamation needs liveness: builders do not hold the writer lock across
// WriteSegment, so a live concurrent build looks exactly like crash residue
// by directory age alone. Each staging directory therefore carries an
// owner.lock byte-range lock held by its building process for the whole
// session (created inside BeginBuild's writer-lock critical section, freed
// by the OS on death): a marker whose lock is still acquirable has no live
// owner and is reclaimed; a busy marker is a live build and stays.

func TestBeginBuildHoldsStagingOwnerMarker(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, fmt.Sprintf("staging-%d", b.GenerationID()), "owner.lock")
	f, err := os.Open(marker)
	if err != nil {
		t.Fatalf("BeginBuild must create an owner marker inside its staging directory: %v", err)
	}
	unlock, lockErr := tryGenerationLock(f, true)
	if lockErr == nil {
		_ = unlock()
		t.Fatal("a live build's owner marker must be held: a second exclusive lock must not be acquirable")
	}
	_ = f.Close()
	if err := b.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(marker)); !os.IsNotExist(err) {
		t.Fatalf("Abort must remove the staging directory including the marker: %v", err)
	}
}

func TestCleanupReclaimsCrashedStagingDirectories(t *testing.T) {
	root := t.TempDir()
	// Crashed build residue: staging-7 with a released owner marker (the OS
	// frees the byte-range lock when the owner dies) and a staged segment.
	crash := filepath.Join(root, "staging-7")
	if err := os.MkdirAll(crash, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crash, "owner.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crash, "seg-7-deadbeef.bin"), encodeSegment(7, []byte("staged bytes")), 0o600); err != nil {
		t.Fatal(err)
	}
	// Live build: staging-8 whose owner marker is byte-range locked.
	live := filepath.Join(root, "staging-8")
	if err := os.MkdirAll(live, 0o700); err != nil {
		t.Fatal(err)
	}
	liveFile, err := os.OpenFile(filepath.Join(live, "owner.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	unlock, lockErr := tryGenerationLock(liveFile, true)
	if lockErr != nil {
		t.Fatal(lockErr)
	}
	defer func() { _ = unlock(); _ = liveFile.Close() }()
	if err := writeGenerationSequence(root, 8); err != nil {
		t.Fatal(err)
	}

	if _, err := NewFileStore(root, Config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(crash); !os.IsNotExist(err) {
		t.Fatalf("crash-orphaned staging-7 must be reclaimed at open, stat err: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live build's staging-8 must survive the sweep: %v", err)
	}
}

func TestCrashedStagingBytesStopStarvingBudget(t *testing.T) {
	root := t.TempDir()
	const payloadSize = 64 << 10
	// Crash residue: 64 KiB of staged bytes in staging-7, owner dead.
	crash := filepath.Join(root, "staging-7")
	if err := os.MkdirAll(crash, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crash, "owner.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crash, "seg-7-deadbeef.bin"), encodeSegment(7, make([]byte, payloadSize)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeGenerationSequence(root, 8); err != nil {
		t.Fatal(err)
	}

	// Budget equals the post-reclamation footprint plus exactly one more
	// segment: the write is only possible once the orphan staging bytes
	// stop being billed.
	var live int64
	err := filepath.Walk(root, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if fi.IsDir() {
			if strings.HasPrefix(fi.Name(), "staging-") {
				return filepath.SkipDir
			}
			return nil
		}
		live += fi.Size()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	budget := live + payloadSize + int64(segHeaderSz) + 16

	s, err := NewFileStore(root, Config{DiskBudgetBytes: budget})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Abort() }() // release the owner marker so TempDir cleanup can remove staging
	if _, err := b.WriteSegment(make([]byte, payloadSize)); err != nil {
		t.Fatalf("WriteSegment must succeed once orphan staging bytes are reclaimed (budget %d): %v", budget, err)
	}
}
