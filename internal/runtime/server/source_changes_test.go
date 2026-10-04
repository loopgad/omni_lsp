package server

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

type publicationOrderBackend struct {
	*sourceChangeBackend
	first   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (b *publicationOrderBackend) SupervisorEpoch() uint64 {
	if b.first.CompareAndSwap(false, true) {
		close(b.entered)
		<-b.release
	}
	return b.epoch
}

func TestExternalChangeQueuesBeforePublishingRevision(t *testing.T) {
	s := New(DefaultConfig())
	b := &publicationOrderBackend{
		sourceChangeBackend: &sourceChangeBackend{mockBackend: &mockBackend{langID: "typescript", exts: []string{".ts"}}, epoch: 1},
		entered:             make(chan struct{}), release: make(chan struct{}),
	}
	s.languages["typescript"] = b
	s.publishSnapshot()
	before := s.currentRevision()
	done := make(chan error, 1)
	go func() {
		done <- s.applyExternalSourceChanges(context.Background(), []languages.SourceChange{{URI: "file:///C:/workspace/a.ts", Kind: languages.SourceChangeChanged}})
	}()
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		close(b.release)
		t.Fatal("source writer did not reach enqueue boundary")
	}
	publishedEarly := s.currentRevision() != before
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if publishedEarly {
		t.Fatal("new revision became visible before child invalidation was queued")
	}
	if s.currentRevision() == before {
		t.Fatal("external source change did not advance revision")
	}
	if err := s.flushExternalSourceChanges(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if b.notifications != 1 || len(b.changes) != 1 {
		t.Fatalf("published revision lacked its child invalidation: notifications=%d changes=%v", b.notifications, b.changes)
	}
}

type overflowRecoveryBackend struct {
	*sourceChangeBackend
	recoveries int
	recover    func(context.Context) error
}

func (b *overflowRecoveryBackend) RecoverSourceChanges(ctx context.Context) error {
	b.recoveries++
	if b.recover != nil {
		return b.recover(ctx)
	}
	b.epoch++ // a new initialized fake child, not an arbitrary test-side bump
	return nil
}

func newOverflowRecoveryServer(t *testing.T) (*Server, *overflowRecoveryBackend) {
	t.Helper()
	s := New(DefaultConfig())
	b := &overflowRecoveryBackend{sourceChangeBackend: &sourceChangeBackend{mockBackend: &mockBackend{langID: "typescript"}, epoch: 1}}
	s.languages["typescript"] = b
	changes := make([]languages.SourceChange, maxPendingSourceChanges+1)
	for i := range changes {
		changes[i] = languages.SourceChange{URI: "file:///C:/workspace/a.ts", Kind: languages.SourceChangeChanged}
	}
	if err := s.applyExternalSourceChanges(context.Background(), changes); err == nil {
		t.Fatal("oversized source event batch was accepted")
	}
	return s, b
}

func TestExternalChangeOverflowRequiresVerifiedRecovery(t *testing.T) {
	s, b := newOverflowRecoveryServer(t)
	if b.recoveries != 0 {
		t.Fatal("document writer must not restart a child")
	}
	if err := s.flushExternalSourceChanges(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if b.recoveries != 1 || b.epoch != 2 || b.notifications != 0 || s.externalSourceSyncError(b) != nil {
		t.Fatalf("overflow recovery = attempts %d, epoch %d, notifications %d, fence %v", b.recoveries, b.epoch, b.notifications, s.externalSourceSyncError(b))
	}
	if err := s.flushExternalSourceChanges(context.Background(), b); err != nil || b.recoveries != 1 {
		t.Fatalf("restarted recovered child again: attempts %d, err %v", b.recoveries, err)
	}
}

func TestExternalChangeOverflowRecoveryFailureKeepsFence(t *testing.T) {
	s, b := newOverflowRecoveryServer(t)
	failed := errors.New("child initialization failed")
	b.recover = func(context.Context) error { return failed }
	if err := s.flushExternalSourceChanges(context.Background(), b); !errors.Is(err, failed) {
		t.Fatalf("recovery error = %v, want %v", err, failed)
	}
	if s.externalSourceSyncError(b) == nil || b.notifications != 0 {
		t.Fatal("failed recovery cleared freshness fence or used old child")
	}
}

func TestExternalChangeOverflowDuringRecoveryRetainsFence(t *testing.T) {
	s, b := newOverflowRecoveryServer(t)
	b.recover = func(context.Context) error {
		changes := make([]languages.SourceChange, maxPendingSourceChanges+1)
		for i := range changes {
			changes[i] = languages.SourceChange{URI: "file:///C:/workspace/b.ts", Kind: languages.SourceChangeChanged}
		}
		if err := s.applyExternalSourceChanges(context.Background(), changes); err == nil {
			t.Fatal("second oversized batch was accepted")
		}
		b.epoch++
		return nil
	}
	if err := s.flushExternalSourceChanges(context.Background(), b); err == nil {
		t.Fatal("recovery accepted another batch lost during initialization")
	}
	if b.recoveries != 1 || b.notifications != 0 || s.externalSourceSyncError(b) == nil {
		t.Fatalf("new overflow fence was lost: attempts %d, notifications %d, fence %v", b.recoveries, b.notifications, s.externalSourceSyncError(b))
	}
}

type sourceChangeBackend struct {
	*mockBackend
	epoch         uint64
	changes       []languages.SourceChange
	notifications int
	err           error
}

func (b *sourceChangeBackend) SupervisorEpoch() uint64 { return b.epoch }
func (b *sourceChangeBackend) NotifySourceChanges(_ context.Context, changes []languages.SourceChange) error {
	b.notifications++
	b.changes = append([]languages.SourceChange(nil), changes...)
	return b.err
}

func TestExternalChangesForwardOrderedLifecycleOnceAndPreserveOverlay(t *testing.T) {
	s := New(DefaultConfig())
	b := &sourceChangeBackend{mockBackend: &mockBackend{langID: "typescript", exts: []string{".ts"}}, epoch: 1}
	s.languages["typescript"], s.languages["javascript"] = b, b
	uri := "file:///C:/workspace/a.ts"
	s.vfs.Open(uri, "typescript", 7, []byte("unsaved content"), vfs.SourceEditor)
	params, _ := json.Marshal(map[string]any{"files": []map[string]string{{"oldUri": uri, "newUri": uri}}})
	_, err := s.handleDidRenameFiles(context.Background(), &jsonrpc.Message{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if b.notifications != 0 {
		t.Fatal("writer must not perform child I/O")
	}
	if err := s.flushExternalSourceChanges(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	want := []languages.SourceChange{{URI: canonicalDocumentURI(uri), Kind: languages.SourceChangeDeleted}, {URI: canonicalDocumentURI(uri), Kind: languages.SourceChangeCreated}}
	if b.notifications != 1 || !reflect.DeepEqual(b.changes, want) {
		t.Fatalf("notifications=%d changes=%v want=%v", b.notifications, b.changes, want)
	}
	if doc := s.vfs.Get(uri); doc.Version != 7 || string(doc.Content) != "unsaved content" {
		t.Fatalf("external hint replaced dirty editor document: %+v", doc)
	}
}

func TestFailedExternalSynchronizationFencesReadsUntilNewChildEpoch(t *testing.T) {
	s := New(DefaultConfig())
	b := &sourceChangeBackend{mockBackend: &mockBackend{langID: "typescript"}, epoch: 1, err: errors.New("child notification failed")}
	s.languages["typescript"] = b
	changes := []languages.SourceChange{{URI: "file:///C:/workspace/dependency.ts", Kind: languages.SourceChangeChanged}}
	if err := s.applyExternalSourceChanges(context.Background(), changes); err != nil {
		t.Fatal(err)
	}
	if err := s.flushExternalSourceChanges(context.Background(), b); err == nil {
		t.Fatal("failed notification must surface on the consuming read")
	}
	if _, _, err := s.beginBackendWorkspaceSnapshot(context.Background(), b, nil); err == nil {
		t.Fatal("failed synchronization must fence semantic reads")
	}
	b.err = nil
	if err := s.applyExternalSourceChanges(context.Background(), changes); err != nil {
		t.Fatal(err)
	}
	if s.externalSourceSyncError(b) == nil {
		t.Fatal("a later partial hint cannot erase missing-event failure")
	}
	b.epoch++
	if s.externalSourceSyncError(b) != nil {
		t.Fatal("a new child process must be allowed to recover from disk")
	}
}

func TestCanceledExternalSynchronizationRetainsPendingBatch(t *testing.T) {
	s := New(DefaultConfig())
	b := &sourceChangeBackend{mockBackend: &mockBackend{langID: "typescript"}, epoch: 1, err: context.Canceled}
	s.languages["typescript"] = b
	changes := []languages.SourceChange{{URI: "file:///C:/workspace/dependency.ts", Kind: languages.SourceChangeChanged}}
	if err := s.applyExternalSourceChanges(context.Background(), changes); err != nil {
		t.Fatal(err)
	}
	if err := s.flushExternalSourceChanges(context.Background(), b); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if s.externalSourceSyncError(b) != nil {
		t.Fatal("canceled unsent notification must not fence the child epoch")
	}
	b.err = nil
	if err := s.flushExternalSourceChanges(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if b.notifications != 2 || !reflect.DeepEqual(b.changes, []languages.SourceChange{{URI: canonicalDocumentURI(changes[0].URI), Kind: changes[0].Kind}}) {
		t.Fatalf("pending delivery: calls=%d changes=%v", b.notifications, b.changes)
	}
	if err := s.flushExternalSourceChanges(context.Background(), b); err != nil || b.notifications != 2 {
		t.Fatalf("committed batch repeated: calls=%d err=%v", b.notifications, err)
	}
}
