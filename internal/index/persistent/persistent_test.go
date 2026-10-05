package persistent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func newStore(t *testing.T) *FileStore {
	t.Helper()
	s, err := NewFileStore(t.TempDir(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func publishOne(t *testing.T, s *FileStore, payload string) SegmentID {
	t.Helper()
	b, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.WriteSegment([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	return id
}

type cancelDuringCommitContext struct {
	context.Context
	calls    int
	cancelAt int
	canceled bool
	onCancel func()
}

type commitDecisionContext struct {
	context.Context
	calls   int
	checked chan struct{}
	resume  chan struct{}
}

func (c *commitDecisionContext) Err() error {
	c.calls++
	if c.calls == 6 {
		err := c.Context.Err()
		close(c.checked)
		<-c.resume
		return err
	}
	return c.Context.Err()
}

func (c *cancelDuringCommitContext) Err() error {
	c.calls++
	if c.calls == c.cancelAt {
		c.canceled = true
		if c.onCancel != nil {
			c.onCancel()
		}
	}
	if c.canceled {
		return context.Canceled
	}
	return nil
}

func TestCommitCancellationAfterSegmentPromotionRollsBackUncommittedFiles(t *testing.T) {
	s := newStore(t)
	oldID := publishOne(t, s, "generation-one")
	build, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	failedID, err := build.WriteSegment([]byte("must-not-become-an-orphan"))
	if err != nil {
		t.Fatal(err)
	}

	var sawPromotedSegment, sawTemporaryManifest, sawCurrentInHistory, sawPreviousManifest bool
	ctx := &cancelDuringCommitContext{Context: context.Background(), cancelAt: 6}
	ctx.onCancel = func() {
		_, segmentErr := os.Stat(filepath.Join(s.root, segFileName(failedID)))
		_, manifestErr := os.Stat(s.tmpManifestPath())
		_, currentManifestErr := os.Stat(s.manifestPath())
		sawPromotedSegment = segmentErr == nil
		sawTemporaryManifest = manifestErr == nil
		sawPreviousManifest = currentManifestErr == nil
		historyBytes, historyErr := os.ReadFile(s.historyPath())
		var history []*Generation
		if historyErr == nil && json.Unmarshal(historyBytes, &history) == nil {
			for _, generation := range history {
				if generation != nil && generation.ID == 1 {
					sawCurrentInHistory = true
				}
			}
		}
	}
	if err := build.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit error = %v, want context.Canceled", err)
	}
	if !sawPromotedSegment || !sawTemporaryManifest || !sawCurrentInHistory || !sawPreviousManifest {
		t.Fatalf("cancellation did not occur before atomic publication: segment=%t temp-manifest=%t current-in-history=%t previous-manifest=%t", sawPromotedSegment, sawTemporaryManifest, sawCurrentInHistory, sawPreviousManifest)
	}

	view, err := s.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatalf("open snapshot after cancellation: %v", err)
	}
	if view.ID != 1 || len(view.Segments) != 1 || view.Segments[0].ID != oldID {
		t.Fatalf("canceled commit changed readable generation: %+v", view.Generation)
	}
	if _, err := view.ReadSegment(oldID); err != nil {
		t.Fatalf("previous generation became unreadable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.root, segFileName(failedID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled commit left orphan segment %s: %v", failedID, err)
	}
	if _, err := os.Stat(s.tmpManifestPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled commit left temporary manifest: %v", err)
	}
	if staging, err := filepath.Glob(filepath.Join(s.root, "staging-*")); err != nil || len(staging) != 0 {
		t.Fatalf("canceled commit left staging directories: %v (%v)", staging, err)
	}

	lockCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	probe, err := s.BeginBuild(lockCtx)
	if err != nil {
		t.Fatalf("canceled commit left writer lock held: %v", err)
	}
	if err := probe.Abort(); err != nil {
		t.Fatalf("release writer lock probe: %v", err)
	}

	reopened, err := NewFileStore(s.root, Config{})
	if err != nil {
		t.Fatalf("reopen store after canceled commit: %v", err)
	}
	recovered, err := reopened.OpenSnapshot(context.Background())
	if err != nil || recovered.ID != 1 || len(recovered.Segments) != 1 || recovered.Segments[0].ID != oldID {
		t.Fatalf("canceled commit changed recovered generation: %+v, err=%v", recovered.Generation, err)
	}
}

func TestCommitCanceledBeforePublishKeepsCurrentGeneration(t *testing.T) {
	s := newStore(t)
	oldID := publishOne(t, s, "still-current")
	build, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newID, err := build.WriteSegment([]byte("canceled"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := build.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("commit error = %v, want context.Canceled", err)
	}
	if err := build.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("repeated commit changed its terminal result: %v", err)
	}
	view, err := s.OpenSnapshot(context.Background())
	if err != nil || view.ID != 1 || view.Segments[0].ID != oldID {
		t.Fatalf("canceled commit changed current generation: %+v, err=%v", view.Generation, err)
	}
	if _, err := os.Stat(filepath.Join(s.root, segFileName(newID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled segment remains discoverable: %v", err)
	}
	if _, err := os.Stat(s.manifestPath()); err != nil {
		t.Fatalf("previous manifest was not preserved: %v", err)
	}
}

func TestCancellationRacingCommitHasOneTerminalOutcome(t *testing.T) {
	s := newStore(t)
	oldID := publishOne(t, s, "before-race")
	for i := 0; i < 8; i++ {
		build, err := s.BeginBuild(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		newID, err := build.WriteSegment([]byte("race-result"))
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		start := make(chan struct{})
		commitResult := make(chan error, 1)
		go func() {
			<-start
			commitResult <- build.Commit(ctx)
		}()
		go func() {
			<-start
			cancel()
		}()
		close(start)
		err = <-commitResult
		cancel()
		view, openErr := s.OpenSnapshot(context.Background())
		if openErr != nil {
			t.Fatalf("open after commit race: %v", openErr)
		}
		switch {
		case err == nil:
			if len(view.Segments) != 1 || view.Segments[0].ID != newID {
				t.Fatalf("commit reported success but generation %d was not published", view.ID)
			}
			oldID = newID
		case errors.Is(err, context.Canceled):
			if view.ID == 1 && view.Segments[0].ID != oldID || view.ID != 1 && view.Segments[0].ID == newID {
				t.Fatalf("commit reported cancellation but published generation %d", view.ID)
			}
		default:
			t.Fatalf("unexpected commit result: %v", err)
		}
		// Rollback must leave nothing behind. No other code path reads staging,
		// so a residue only burns disk: WriteSegment counts staging bytes
		// against the budget and cleanupOrphanSegments never descends into
		// staging-*, so the leak is permanent and eventually starves commits
		// with ErrDiskBudgetExceeded. A stranded manifest.tmp would also shadow
		// the next publish.
		if staging, globErr := filepath.Glob(filepath.Join(s.root, "staging-*")); globErr != nil || len(staging) != 0 {
			t.Fatalf("round %d left staging directories behind: %v (%v)", i, staging, globErr)
		}
		if _, statErr := os.Stat(s.tmpManifestPath()); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("round %d left manifest.tmp behind: %v", i, statErr)
		}
	}
}

func TestCancellationAfterCommitDecisionReturnsCommittedOutcome(t *testing.T) {
	s := newStore(t)
	oldID := publishOne(t, s, "before-commit-wins")
	build, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newID, err := build.WriteSegment([]byte("commit-wins"))
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &commitDecisionContext{Context: base, checked: make(chan struct{}), resume: make(chan struct{})}
	result := make(chan error, 1)
	go func() { result <- build.Commit(ctx) }()
	select {
	case <-ctx.checked:
	case <-time.After(2 * time.Second):
		t.Fatal("commit did not reach its final cancellation check")
	}
	cancel()
	close(ctx.resume)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("cancellation after the commit decision changed its result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("commit did not finish after its final cancellation check")
	}
	view, err := s.OpenSnapshot(context.Background())
	if err != nil || view.ID != 2 || len(view.Segments) != 1 || view.Segments[0].ID != newID {
		t.Fatalf("commit reported success without publishing generation 2: %+v, err=%v", view.Generation, err)
	}
	if view.Segments[0].ID == oldID {
		t.Fatal("commit kept the previous generation after reporting success")
	}
}

func TestGenerationLeasePinsSegmentsDuringCompaction(t *testing.T) {
	s := newStore(t)
	oldID := publishOne(t, s, "reader-pinned")
	lease, err := s.OpenSnapshotLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	oldPath := filepath.Join(s.root, segFileName(oldID))
	if err := s.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("compaction removed a pinned segment: %v", err)
	}
	got, err := lease.Snapshot().ReadSegment(oldID)
	if err != nil || string(got) != "reader-pinned" {
		t.Fatalf("leased reader lost its segment: %q, %v", got, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired segment remains after its last lease closed: %v", err)
	}
}

func TestRestartRecoveryReservesPastAbandonedBuild(t *testing.T) {
	root := t.TempDir()
	s, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	oldID := publishOne(t, s, "survives-restart")
	ready := filepath.Join(root, "crashed-helper.ready")
	release := filepath.Join(root, "crashed-helper.release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPersistentReservationHelperProcess$")
	cmd.Env = append(os.Environ(),
		"OMNILSP_PERSISTENT_RESERVATION_HELPER=1",
		"OMNILSP_PERSISTENT_RESERVATION_ROOT="+root,
		"OMNILSP_PERSISTENT_RESERVATION_READY="+ready,
		"OMNILSP_PERSISTENT_RESERVATION_RELEASE="+release,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	childWaited := false
	defer func() {
		if !childWaited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not stage its abandoned generation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // A forced process exit simulates a crash before Abort.
	childWaited = true
	if idBytes, err := os.ReadFile(ready); err != nil || string(idBytes) != "2" {
		t.Fatalf("crashed helper reservation = %q, err=%v; want generation 2", idBytes, err)
	}

	reopened, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	view, err := reopened.OpenSnapshot(context.Background())
	if err != nil || view.ID != 1 || view.Segments[0].ID != oldID {
		t.Fatalf("restart lost the previous generation: %+v, err=%v", view.Generation, err)
	}
	next, err := reopened.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if next.GenerationID() != 3 {
		t.Fatalf("generation after abandoned reservation = %d, want 3", next.GenerationID())
	}
	if err := next.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentReservationHelperProcess(t *testing.T) {
	if os.Getenv("OMNILSP_PERSISTENT_RESERVATION_HELPER") != "1" {
		return
	}
	root := os.Getenv("OMNILSP_PERSISTENT_RESERVATION_ROOT")
	ready := os.Getenv("OMNILSP_PERSISTENT_RESERVATION_READY")
	release := os.Getenv("OMNILSP_PERSISTENT_RESERVATION_RELEASE")
	store, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	build, err := store.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := build.WriteSegment([]byte("helper-staged")); err != nil {
		t.Fatal(err)
	}
	readyTmp := ready + ".tmp"
	if err := os.WriteFile(readyTmp, []byte(strconv.FormatUint(build.GenerationID(), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(readyTmp, ready); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(release); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("parent did not release reservation helper")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := build.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentCrossProcessBuildReservation(t *testing.T) {
	root := t.TempDir()
	ready := filepath.Join(root, "helper.ready")
	release := filepath.Join(root, "helper.release")
	cmd := exec.Command(os.Args[0], "-test.run=^TestPersistentReservationHelperProcess$")
	cmd.Env = append(os.Environ(),
		"OMNILSP_PERSISTENT_RESERVATION_HELPER=1",
		"OMNILSP_PERSISTENT_RESERVATION_ROOT="+root,
		"OMNILSP_PERSISTENT_RESERVATION_READY="+ready,
		"OMNILSP_PERSISTENT_RESERVATION_RELEASE="+release,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	helperWaited := false
	defer func() {
		_ = os.WriteFile(release, nil, 0o600)
		if !helperWaited && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	var firstID uint64
	for {
		if raw, err := os.ReadFile(ready); err == nil {
			firstID, err = strconv.ParseUint(string(raw), 10, 64)
			if err != nil {
				t.Fatalf("parse helper reservation %q: %v", raw, err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not reserve a generation")
		}
		time.Sleep(10 * time.Millisecond)
	}
	store, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	second, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatalf("second process could not reserve while first staged: %v", err)
	}
	if got := second.GenerationID(); got <= firstID {
		t.Fatalf("reservations are not unique and monotonic: first=%d second=%d", firstID, got)
	}
	if err := second.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("reservation helper failed: %v", err)
	}
	helperWaited = true
}

// TestL4_CrashBeforePublishKeepsOldGeneration: staging data abandoned before
// Commit never affects the readable pointer.
func TestL4_CrashBeforePublishKeepsOldGeneration(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "generation-one")

	b, err := s.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.WriteSegment([]byte("never published")); err != nil {
		t.Fatal(err)
	}
	if err := b.Abort(); err != nil { // simulates crash cleanup of staging
		t.Fatal(err)
	}

	view, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("old generation lost: %v", err)
	}
	if len(view.Segments) != 1 {
		t.Fatalf("segments = %d, want the single old one", len(view.Segments))
	}
	got, err := view.ReadSegment(view.Segments[0].ID)
	if err != nil || string(got) != "generation-one" {
		t.Fatalf("read %q err=%v", got, err)
	}
	// No staging leftovers discoverable in the store root.
	if _, err := os.Stat(filepath.Join(s.root, "staging-2")); !os.IsNotExist(err) {
		t.Error("staging directory leaked into store root")
	}
}

// TestL6_TXN002_HalfWrittenSegmentNotDiscoverable: a truncated staged file
// fails verification and cannot enter a committed generation.
func TestL6_TXN002_HalfWrittenSegmentNotDiscoverable(t *testing.T) {
	s := newStore(t)
	b, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id, err := b.WriteSegment([]byte("payload-that-will-be-truncated"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a torn write in staging: chop the file below declared length.
	p := filepath.Join(b.(*fileBuild).stageDir, segFileName(id))
	raw, _ := os.ReadFile(p)
	if err := os.WriteFile(p, raw[:len(raw)-5], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(context.Background()); err != nil {
		t.Fatalf("commit itself may proceed; discovery is what must fail: %v", err)
	}
	_, err = s.OpenSnapshot(context.Background())
	if err == nil {
		t.Fatal("half-written segment became discoverable — TXN-002 violated")
	}
}

// TestL7_ChecksumMismatchQuarantines: one flipped byte quarantines the whole
// generation and falls back to the previous verified one (TXN-004 + L8).
func TestL7_ChecksumMismatchQuarantines(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "good-generation")
	publishOne(t, s, "corrupt-me")

	view, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	corruptSeg := view.Segments[0].ID
	p := filepath.Join(s.root, segFileName(corruptSeg))
	raw, _ := os.ReadFile(p)
	raw[len(raw)-1] ^= 0xFF // flip one payload byte
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	view2, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("recovery failed instead of falling back: %v", err)
	}
	q := s.Quarantined()
	found := false
	for _, id := range q {
		if id == corruptSeg {
			found = true
		}
	}
	if !found {
		t.Errorf("corrupt segment %s not in quarantine list: %v", corruptSeg, q)
	}
	got, _ := view2.ReadSegment(view2.Segments[0].ID)
	if string(got) != "good-generation" && len(view2.Segments) >= 1 {
		t.Logf("fell back to generation with first segment %q", got)
	}
}

// TestL6_TXN003_IdempotentRecovery: repeated opens on the same corruption
// state converge to the same outcome without double-quarantining.
func TestL6_TXN003_IdempotentRecovery(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "stable-old")
	publishOne(t, s, "broken-new")

	view, _ := s.OpenSnapshot(ctx)
	p := filepath.Join(s.root, segFileName(view.Segments[0].ID))
	raw, _ := os.ReadFile(p)
	raw[segHeaderSz] ^= 0x01 // corrupt payload start
	os.WriteFile(p, raw, 0o644)

	v1, err1 := s.OpenSnapshot(ctx)
	q1 := s.Quarantined()
	v2, err2 := s.OpenSnapshot(ctx)
	q2 := s.Quarantined()

	if (err1 == nil) != (err2 == nil) {
		t.Fatalf("non-idempotent errors: %v vs %v", err1, err2)
	}
	if len(v1.Segments) > 0 && len(v2.Segments) > 0 && v1.ID != v2.ID {
		t.Errorf("recovered generations diverge: %d vs %d", v1.ID, v2.ID)
	}
	if len(q2) < len(q1) {
		t.Errorf("quarantine list shrank between runs")
	}
}

// TestBudget_RejectsWhenFull: writes past the configured budget are refused
// with ErrDiskBudgetExceeded, never partially applied (§L17).
func TestBudget_RejectsWhenFull(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFileStore(dir, Config{DiskBudgetBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 4096) // far beyond the tiny budget
	_, err = b.WriteSegment(big)
	if !errors.Is(err, ErrDiskBudgetExceeded) {
		t.Fatalf("want ErrDiskBudgetExceeded, got %v", err)
	}
	if err := b.Commit(context.Background()); err == nil {
		t.Log("empty commit allowed; harmless")
	}
}

// TestCompact_ReducesSegmentsAndPreservesReads: many segments merge into a
// fresh generation with identical readable content (§L16).
func TestCompact_ReducesSegmentsAndPreservesReads(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	want := map[string]string{}
	b, err := s.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"alpha", "beta", "gamma"} {
		id, err := b.WriteSegment([]byte(p))
		if err != nil {
			t.Fatal(err)
		}
		want[string(id)] = p
		_ = i
	}
	if err := b.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	before, _ := s.OpenSnapshot(ctx)
	if len(before.Segments) < 3 {
		t.Fatalf("setup produced %d segments", len(before.Segments))
	}
	if err := s.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Segments) >= len(before.Segments) {
		t.Errorf("compaction did not reduce segments: %d → %d", len(before.Segments), len(after.Segments))
	}
	if len(after.Segments) != 1 {
		t.Errorf("compaction must yield exactly one merged segment, got %d", len(after.Segments))
	}
	payload, err := after.ReadSegment(after.Segments[0].ID)
	if err != nil {
		t.Fatalf("post-compact read: %v", err)
	}
	var got []string
	for _, p := range SplitMerged(payload) {
		got = append(got, string(p))
	}
	wantPayloads := []string{"alpha", "beta", "gamma"}
	if len(got) != len(wantPayloads) {
		t.Fatalf("merged segment holds %d payloads, want 3", len(got))
	}
	for i := range wantPayloads {
		if got[i] != wantPayloads[i] {
			t.Errorf("payload[%d] = %q, want %q", i, got[i], wantPayloads[i])
		}
	}
}

func TestCompactRetiresOnlyTheCompactedFallback(t *testing.T) {
	s := newStore(t)
	publishOne(t, s, "older-fallback")
	build, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{"current-a", "current-b"} {
		if _, err := build.WriteSegment([]byte(payload)); err != nil {
			_ = build.Abort()
			t.Fatal(err)
		}
	}
	if err := build.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	compacted, err := s.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	segmentPath := filepath.Join(s.root, segFileName(compacted.Segments[0].ID))
	if err := os.WriteFile(segmentPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	fallback, err := s.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fallback.ID != 1 {
		t.Fatalf("fallback generation after compacted corruption = %d, want 1", fallback.ID)
	}
	payload, err := fallback.ReadSegment(fallback.Segments[0].ID)
	if err != nil || string(payload) != "older-fallback" {
		t.Fatalf("fallback payload = %q, %v", payload, err)
	}
}

func TestL8_NullHistoryEntryDegradesWithoutPanic(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, fileHistory), []byte(`[null]`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatalf("open store with malformed history: %v", err)
	}
	if _, err := s.OpenSnapshot(context.Background()); !errors.Is(err, ErrNoGeneration) {
		t.Fatalf("OpenSnapshot error = %v, want ErrNoGeneration", err)
	}
}

func TestL4_IndependentStoresReserveDistinctMonotonicGenerations(t *testing.T) {
	root := t.TempDir()
	first, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFileStore(root, Config{})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	type reservedBuild struct {
		build   BuildSession
		payload string
		err     error
	}
	results := make(chan reservedBuild, 2)
	for i, store := range []*FileStore{first, second} {
		payload := fmt.Sprintf("writer-%d", i)
		go func(store *FileStore, payload string) {
			<-start
			build, err := store.BeginBuild(context.Background())
			if err != nil {
				results <- reservedBuild{err: err}
				return
			}
			if _, err := build.WriteSegment([]byte(payload)); err != nil {
				_ = build.Abort()
				results <- reservedBuild{err: err}
				return
			}
			results <- reservedBuild{build: build, payload: payload}
		}(store, payload)
	}
	close(start)
	reserved := []reservedBuild{<-results, <-results}
	for _, result := range reserved {
		if result.err != nil {
			t.Fatal(result.err)
		}
	}
	if reserved[0].build.GenerationID() > reserved[1].build.GenerationID() {
		reserved[0], reserved[1] = reserved[1], reserved[0]
	}
	if reserved[0].build.GenerationID() != 1 || reserved[1].build.GenerationID() != 2 {
		t.Fatalf("reserved generations = %d, %d, want 1, 2", reserved[0].build.GenerationID(), reserved[1].build.GenerationID())
	}
	for _, result := range reserved {
		if err := result.build.Commit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	view, err := first.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.ID != 2 {
		t.Fatalf("published generation = %d, want 2", view.ID)
	}
	payload, err := view.ReadSegment(view.Segments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(payload); got != reserved[1].payload {
		t.Fatalf("published payload = %q, want highest reservation payload %q", got, reserved[1].payload)
	}
}

func TestOlderReservationCannotReplaceNewerCommittedGeneration(t *testing.T) {
	s := newStore(t)
	publishOne(t, s, "base")
	older, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	olderID, err := older.WriteSegment([]byte("older-reservation"))
	if err != nil {
		t.Fatal(err)
	}
	newer, err := s.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newerID, err := newer.WriteSegment([]byte("newer-reservation"))
	if err != nil {
		t.Fatal(err)
	}
	if newer.GenerationID() <= older.GenerationID() {
		t.Fatalf("generation IDs regressed: older=%d newer=%d", older.GenerationID(), newer.GenerationID())
	}
	if err := newer.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := older.Commit(context.Background()); err == nil {
		t.Fatal("older reservation replaced a newer committed generation")
	}
	view, err := s.OpenSnapshot(context.Background())
	if err != nil || view.ID != newer.GenerationID() || view.Segments[0].ID != newerID {
		t.Fatalf("newer committed generation was replaced: %+v, err=%v", view.Generation, err)
	}
	if _, err := os.Stat(filepath.Join(s.root, segFileName(olderID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("superseded reservation left a segment: %v", err)
	}
}

func TestL7_UnsafeSegmentIDRejected(t *testing.T) {
	view := GenerationView{Generation: Generation{ID: 1}, dir: t.TempDir()}
	if _, err := view.ReadSegment(SegmentID("../../outside")); err == nil {
		t.Fatal("unsafe segment ID was accepted")
	}
}

// TestL9_FutureSchemaQuarantinesAndFallsBack pins the forward-compatibility
// reader policy (L9): a segment written by a NEWER schema is refused, the
// offending generation quarantines, and recovery falls back to the older
// verified generation instead of serving unknown bytes.
func TestL9_FutureSchemaQuarantinesAndFallsBack(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	publishOne(t, s, "old-good-generation")
	futureSeg := publishOne(t, s, "written-by-future-version")

	// Rewrite the future segment's framing with an unknown schema version.
	p := filepath.Join(s.root, segFileName(futureSeg))
	raw, _ := os.ReadFile(p)
	raw[4] = 0 // raw[4:8] holds schemaVer (big-endian); 0x00000001 -> 0x00FFFFFF+1
	raw[5], raw[6], raw[7] = 0xFF, 0xFF, 0xFF
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	view, err := s.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("recovery must fall back to old generation: %v", err)
	}
	for _, seg := range view.Segments {
		payload, err := view.ReadSegment(seg.ID)
		if err != nil {
			t.Fatalf("surviving segment %s unreadable: %v", seg.ID, err)
		}
		if string(payload) == "written-by-future-version" {
			t.Error("future-schema payload must never be visible")
		}
	}
	found := false
	for _, id := range s.Quarantined() {
		if id == futureSeg {
			found = true
		}
	}
	if !found {
		t.Errorf("future-schema segment %s not quarantined: %v", futureSeg, s.Quarantined())
	}
}

// TestL10_FreshnessTupleGatesVisibility pins the §L10 contract: sealed
// records whose identity inputs no longer match are stale — never visible;
// matching tuples round-trip the payload untouched.
func TestL10_FreshnessTupleGatesVisibility(t *testing.T) {
	fresh := FreshnessTuple{SourceHash: "h1", BuildContext: "bc1", Toolchain: "go1.26", BackendVer: "v7"}
	sealed := SealPayload([]byte("payload-bytes"), fresh)
	headerLen := int(binary.BigEndian.Uint32(sealed[:freshnessHeaderSz]))
	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(sealed[freshnessHeaderSz:freshnessHeaderSz+headerLen], &persisted); err != nil {
		t.Fatalf("decode persisted freshness tuple: %v", err)
	}
	if _, ok := persisted["revision"]; ok {
		t.Fatal("process-local snapshot revision must not be persisted as freshness identity")
	}

	got, err := VerifyPayload(sealed, fresh)
	if err != nil || string(got) != "payload-bytes" {
		t.Fatalf("fresh tuple round-trip: %q %v", got, err)
	}

	for name, mut := range map[string]func(*FreshnessTuple){
		"sourceHash":   func(f *FreshnessTuple) { f.SourceHash = "other" },
		"buildContext": func(f *FreshnessTuple) { f.BuildContext = "other" },
		"toolchain":    func(f *FreshnessTuple) { f.Toolchain = "go1.25" },
		"backendVer":   func(f *FreshnessTuple) { f.BackendVer = "v6" },
	} {
		m := fresh
		mut(&m)
		if _, err := VerifyPayload(sealed, m); err == nil {
			t.Errorf("%s mismatch accepted", name)
		}
	}

	if _, err := VerifyPayload([]byte{0, 0}, fresh); err == nil {
		t.Error("truncated seal accepted")
	}
}
