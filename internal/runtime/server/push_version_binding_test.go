package server

import (
	"context"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// Bug round 20 A#12 (圈 4) pins, in two legs, that push-lane diagnostics
// never pair one generation's items with another generation's version.

// blockingDiagBackend parks Diagnostics so the test can land a didChange
// (vfs.Update + publishSnapshot) inside compute's in-flight window — the
// exact window between "backend Diagnostics returned" and "schedule cancels
// the run" that the push lane's timer callback lives in.
type blockingDiagBackend struct {
	mockBackend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingDiagBackend) Diagnostics(context.Context, string, []byte) ([]languages.Diagnostic, error) {
	close(b.entered)
	<-b.release
	return []languages.Diagnostic{{
		StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 4,
		Severity: 1, Code: "STALE-V1", Source: "go", Message: "stale v1 content diagnostic",
	}}, nil
}

// Leg 1: compute's commit segment must take the entry version from the SAME
// snapshot its src/snapRev came from. It used to re-derive the snapshot from
// ctx — nil on the push lane (timer callback runs on context.Background) —
// and fell back to the live VFS, so an edit landing after the backend call
// returned committed {key=old generation, version=new generation}. The
// mismatched entry then went on the wire through publishPluginDiagnostics as
// a full-replacement frame claiming the NEW document version for OLD
// diagnostics.
func TestPushComputeCommitsVersionFromCapturedSnapshot(t *testing.T) {
	const uri = "file:///w/pushver.go"
	s := New(DefaultConfig())
	be := &blockingDiagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		entered: make(chan struct{}), release: make(chan struct{})}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("v1-content"), vfs.SourceEditor)
	s.publishSnapshot() // rev1 snapshot carries doc v1

	committed := make(chan error, 1)
	go func() {
		_, _, err := s.diag.compute(context.Background(), uri) // push-lane shape: no snapshot in ctx
		committed <- err
	}()
	<-be.entered // diagnostics in flight against the rev-1 snapshot

	// The didChange shape lands in the window — vfs.Update/publishSnapshot
	// hold no diag mutex (handlers.go request-after-update sequence).
	s.vfs.Update(uri, 2, []byte("v2-content!!"))
	s.publishSnapshot() // rev2

	close(be.release)
	if err := <-committed; err != nil {
		t.Fatalf("compute: %v", err)
	}

	s.diag.mu.Lock()
	en := s.diag.cache[uri]
	s.diag.mu.Unlock()
	if en.key.SnapshotRev != 1 {
		t.Fatalf("entry key binds SnapshotRev %d, want the compute run's own generation 1", en.key.SnapshotRev)
	}
	if en.version != 1 {
		t.Fatalf("entry pairs version %d (live VFS) with rev-1 items — version/items generations mispaired on commit", en.version)
	}
}

// Leg 2: publishPluginDiagnostics must not put a version claim on the wire
// for an entry that is no longer the current generation — the same discipline
// publishComputed enforces. The convergence semantics stay intact: the frame
// still carries the (consistently stale) merged items WITHOUT a version
// claim, and the next generation's push delivers the fresh versioned frame.
func TestPushPluginDiagnosticsOmitsVersionForStaleGeneration(t *testing.T) {
	const uri = "file:///w/pluginver.go"
	s, be, rt := newRecordingServer(t, "go")

	s.vfs.Open(uri, "go", 1, []byte("v1"), vfs.SourceEditor)
	s.publishSnapshot()
	oldGen := s.diag.currentKey(uri, be)
	s.vfs.Update(uri, 2, []byte("v2"))
	s.publishSnapshot()

	// The mispaired entry shape bug A#12 used to commit: old-generation key
	// and items, new-generation version.
	seedDiagCache(s, uri, oldGen, boundItems("stale v1 content diagnostic"), 2)
	s.diag.publishPluginDiagnostics(uri, map[string]pluginDiagSet{
		"plug": {items: boundItems("plugin diagnostic")},
	})

	pubs := pushPayloads(t, rt)
	if len(pubs) != 1 {
		t.Fatalf("plugin push produced %d frames, want exactly 1: %+v", len(pubs), pubs)
	}
	if pubs[0].Version != 0 {
		t.Fatalf("stale-generation frame claimed version %d on the wire — a full-replacement publishDiagnostics must not pair a newer version with older items", pubs[0].Version)
	}
	if len(pubs[0].Diagnostics) == 0 {
		t.Fatal("stale-generation frame dropped its items; convergence semantics require the merged set to flow")
	}

	// Non-regression: a current-generation entry keeps its version claim.
	seedDiagCache(s, uri, s.diag.currentKey(uri, be), boundItems("current diagnostic"), 2)
	s.diag.publishPluginDiagnostics(uri, map[string]pluginDiagSet{
		"plug": {items: boundItems("plugin diagnostic")},
	})
	pubs = pushPayloads(t, rt)
	if len(pubs) != 2 {
		t.Fatalf("second push produced %d frames, want 2: %+v", len(pubs), pubs)
	}
	if pubs[1].Version != 2 {
		t.Fatalf("current-generation frame version = %d, want 2 (normal path must keep its version claim)", pubs[1].Version)
	}
}
