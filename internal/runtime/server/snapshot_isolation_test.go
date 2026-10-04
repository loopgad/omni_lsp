package server

// Tests closing the P0 loop found in audit: the scheduler path must never
// drop accepted state transitions (PROT-SYNC-001/C5/F12), read queries must
// serve their captured snapshot (INV-SNAPSHOT-002), and S3 rename must get
// ContentModified when its snapshot aged (C6/D11).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/protocol/lsp"
	"github.com/omnilsp/omni/internal/runtime/scheduler"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

func didChangeNotification(uri string, version int64, text string) *jsonrpc.Message {
	raw, _ := json.Marshal(DidChangeTextDocumentParams{
		TextDocument: lsp.VersionedTextDocumentIdentifier{URI: uri, Version: version},
		ContentChanges: []lsp.TextDocumentContentChangeEvent{
			{Text: text}, // full replacement
		},
	})
	return jsonrpc.NewNotification("textDocument/didChange", raw)
}

// TestP1_RapidEditsOrderedUnderConcurrency drives the production admission
// path with the default worker pool: N rapid edits must ALL apply, strictly
// in arrival order (F1 single-writer). Under the old architecture any worker
// pool wider than one reordered concurrent applications and the VFS
// version-regression guard silently dropped edits — PROT-SYNC-001 data loss
// through a second door, masked until now by MaxConcurrent=1 test configs.
func TestP1_RapidEditsOrderedUnderConcurrency(t *testing.T) {
	s := New(DefaultConfig()) // default MaxConcurrent (8)
	attachTransport(s)
	stop := startScheduler(s)
	defer stop()
	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()

	openDoc(s, "file:///x.go", 1, "L0\n")

	const n = 20
	want := "L0\n"
	for i := 1; i <= n; i++ {
		want += fmt.Sprintf("E%02d\n", i)
		s.scheduleMessage(context.Background(),
			didChangeNotification("file:///x.go", int64(i+1), want))
	}

	waitFor(t, func() bool {
		return string(s.vfs.Content("file:///x.go")) == want
	})
	if r := s.syncRejects.Load(); r != 0 {
		t.Errorf("syncRejects = %d, want 0 (no edit may be dropped)", r)
	}
}

// TestP0_SyncBypassesSaturatedScheduler proves the F1 single-writer property
// end to end: with every scheduler worker occupied by blocked queries, state
// transitions still apply immediately — dispatchInline executes on the read
// loop, so scheduleMessage returning means the edit IS applied (no polling,
// no queue, no drop). The old architecture queued edits behind the blockers.
func TestP0_SyncBypassesSaturatedScheduler(t *testing.T) {
	const workers = 8 // scheduler.DefaultConfig MaxConcurrent
	s := New(DefaultConfig())
	attachTransport(s)
	stop := startScheduler(s)
	defer stop()
	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()

	openDoc(s, "file:///x.go", 1, "L0\n")

	// Occupy every worker with a blocked query-class request.
	release := make(chan struct{})
	started := make(chan struct{}, workers)
	for i := 0; i < workers; i++ {
		if res := s.scheduler.Submit(&scheduler.Request{
			Priority:   scheduler.PriorityHover,
			EnqueuedAt: time.Now(),
			Execute: func(ctx context.Context, _ *snapshot.Snapshot) (any, error) {
				started <- struct{}{}
				<-release
				return nil, nil
			},
		}); res != scheduler.Admitted {
			t.Fatalf("blocker %d admission: %v", i, res)
		}
	}
	for i := 0; i < workers; i++ {
		<-started
	}

	// Inline sync must land synchronously despite total worker saturation.
	s.scheduleMessage(context.Background(), didChangeNotification("file:///x.go", 2, "second\n"))
	if got := string(s.vfs.Content("file:///x.go")); got != "second\n" {
		t.Fatalf("sync notification waited behind saturated queries: content = %q", got)
	}
	close(release)
}

// TestINV_SNAPSHOT_002_HoverServesCapturedSnapshot verifies a query bound to
// snapshot N reads version-N content even when newer edits exist.
func TestINV_SNAPSHOT_002_HoverServesCapturedSnapshot(t *testing.T) {
	var gotContent []byte
	be := &capturingHoverBackend{onHover: func(content []byte) { gotContent = content }}
	s := New(DefaultConfig())
	s.RegisterBackend("go", be)

	s.vfs.Open("file:///x.go", "go", 1, []byte("version-one"), vfs.SourceEditor)
	s.publishSnapshot()
	oldSnap := s.snapMgr.Current()

	// A newer edit lands after the query was (conceptually) captured.
	s.vfs.Open("file:///x.go", "go", 2, []byte("version-two"), vfs.SourceEditor)
	s.publishSnapshot()

	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/hover", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(withSnapshot(context.Background(), oldSnap), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("hover failed: %v", resp.Error)
	}
	if string(gotContent) != "version-one" {
		t.Errorf("hover served %q, want captured \"version-one\"", gotContent)
	}
}

func TestINV_SNAPSHOT_002_MissingCapturedDocumentDoesNotReadLiveVFS(t *testing.T) {
	var gotContent []byte
	be := &capturingHoverBackend{onHover: func(content []byte) { gotContent = content }}
	s := New(DefaultConfig())
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///captured.go", "go", 1, []byte("captured"), vfs.SourceEditor)
	s.publishSnapshot()
	oldSnap := s.snapMgr.Current()

	// This URI was opened after capture. Reading its current VFS bytes would
	// combine the old request snapshot with a newer document state.
	s.vfs.Open("file:///later.go", "go", 1, []byte("live-only"), vfs.SourceEditor)
	params := `{"textDocument":{"uri":"file:///later.go"},"position":{"line":0,"character":0}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 2}, "textDocument/hover", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(withSnapshot(context.Background(), oldSnap), msg)
	if resp == nil || resp.Error == nil || resp.Error.Code != jsonrpc.ContentModified {
		t.Fatalf("missing captured document response = %#v, want ContentModified", resp)
	}
	if gotContent != nil {
		t.Fatalf("backend saw live-only content %q for an older snapshot", gotContent)
	}
}

func TestINV_SNAPSHOT_002_EmptyCapturedDocumentIsAuthoritative(t *testing.T) {
	var gotContent []byte
	be := &capturingHoverBackend{onHover: func(content []byte) { gotContent = append([]byte(nil), content...) }}
	s := New(DefaultConfig())
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///empty.go", "go", 1, []byte{}, vfs.SourceEditor)
	s.publishSnapshot()
	oldSnap := s.snapMgr.Current()
	s.vfs.Open("file:///empty.go", "go", 2, []byte("newer"), vfs.SourceEditor)
	s.publishSnapshot()

	params := `{"textDocument":{"uri":"file:///empty.go"},"position":{"line":0,"character":0}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 3}, "textDocument/hover", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(withSnapshot(context.Background(), oldSnap), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("hover failed: %v", resp.Error)
	}
	if len(gotContent) != 0 {
		t.Fatalf("backend saw %q, want empty content from the captured document", gotContent)
	}
}

type workspaceLeaseContextKey struct{}

type leaseAwareHoverBackend struct {
	mockBackend
	gotLeaseContext atomic.Bool
}

func (b *leaseAwareHoverBackend) BeginWorkspaceSnapshot(ctx context.Context, _ languages.WorkspaceSnapshot) (context.Context, func() error, error) {
	return context.WithValue(ctx, workspaceLeaseContextKey{}, true), func() error { return nil }, nil
}

func (b *leaseAwareHoverBackend) WorkspaceSnapshotGeneration() uint64 { return 1 }

func (b *leaseAwareHoverBackend) Hover(ctx context.Context, req languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.gotLeaseContext.Store(ctx.Value(workspaceLeaseContextKey{}) == true)
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: "scoped"},
	}, nil
}

func TestINV_SNAPSHOT_002_PassesLeaseContextToBackend(t *testing.T) {
	be := &leaseAwareHoverBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s := New(DefaultConfig())
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///scoped.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()

	params := `{"textDocument":{"uri":"file:///scoped.go"},"position":{"line":0,"character":0}}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 4}, "textDocument/hover", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(withSnapshot(context.Background(), s.snapMgr.Current()), msg)
	if resp == nil || resp.Error != nil {
		t.Fatalf("hover failed: %v", resp.Error)
	}
	if !be.gotLeaseContext.Load() {
		t.Fatal("backend did not receive the snapshot-scoped context")
	}
}

type failingFinishHoverBackend struct{ leaseAwareHoverBackend }

func (b *failingFinishHoverBackend) BeginWorkspaceSnapshot(ctx context.Context, _ languages.WorkspaceSnapshot) (context.Context, func() error, error) {
	return context.WithValue(ctx, workspaceLeaseContextKey{}, true), func() error {
		return errors.New("snapshot generation changed before lease finish")
	}, nil
}

func TestExplainOmitsEvidenceWhenWorkspaceLeaseFinishRejectsResponse(t *testing.T) {
	be := &failingFinishHoverBackend{leaseAwareHoverBackend: leaseAwareHoverBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
	}}
	s := New(DefaultConfig())
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///stale.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()

	params := `{"textDocument":{"uri":"file:///stale.go"},"position":{"line":0,"character":0}}`
	resp := s.Dispatcher().Dispatch(withSnapshot(context.Background(), s.snapMgr.Current()), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 5}, "textDocument/hover", json.RawMessage(params)))
	if resp == nil || resp.Error == nil {
		t.Fatalf("hover with failed lease finish = %+v, want an error", resp)
	}
	for _, record := range s.recentEvidence() {
		if record.Method == "textDocument/hover" && record.URI == "file:///stale.go" {
			t.Fatalf("failed/stale response left explain evidence: %+v", record)
		}
	}
}

// TestD11_RenameContentModifiedOnAgedSnapshot verifies the S3 freshness gate:
// rename against an aged snapshot refuses with LSP ContentModified.
func TestD11_RenameContentModifiedOnAgedSnapshot(t *testing.T) {
	be := &mockBackend{langID: "go", exts: []string{".go"}, renResult: provenRename()}
	s := New(DefaultConfig())
	s.RegisterBackend("go", be)

	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	oldSnap := s.snapMgr.Current()

	s.vfs.Open("file:///x.go", "go", 2, []byte("package main // edited\n"), vfs.SourceEditor)
	s.publishSnapshot()

	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":7},"newName":"bar"}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "rn", IsStr: true}, "textDocument/rename", json.RawMessage(params))
	resp := s.dispatcher.Dispatch(withSnapshot(context.Background(), oldSnap), msg)
	if resp == nil || resp.Error == nil {
		t.Fatal("aged rename must be refused")
	}
	if resp.Error.Code != jsonrpc.ContentModified {
		t.Errorf("code = %d, want ContentModified (%d)", resp.Error.Code, jsonrpc.ContentModified)
	}
}

type blockingRenameBackend struct {
	mockBackend
	entered chan struct{}
	release chan struct{}
}

func (b *blockingRenameBackend) Rename(context.Context, languages.RenameRequest) (identity.SemanticResult[languages.ValidatedEdit], error) {
	close(b.entered)
	<-b.release
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status: identity.ResultExact,
		Value:  b.renResult,
	}, nil
}

// TestD11_RenameRejectsEditWhenWorkspaceChangesDuringBackend verifies the
// post-computation freshness gate and its request-attributable queryTrace
// counters. This closes the race left by checking only before backend work.
func TestD11_RenameRejectsEditWhenWorkspaceChangesDuringBackend(t *testing.T) {
	be := &blockingRenameBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}, renResult: provenRename()},
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	s := New(DefaultConfig())
	s.RegisterBackend("go", be)
	s.vfs.Open("file:///x.go", "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	oldSnap := s.snapMgr.Current()

	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":7},"newName":"bar"}`
	msg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "rn-race", IsStr: true}, "textDocument/rename", json.RawMessage(params))
	response := make(chan *jsonrpc.Message, 1)
	go func() {
		response <- s.dispatcher.Dispatch(withSnapshot(context.Background(), oldSnap), msg)
	}()
	select {
	case <-be.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("rename did not reach backend")
	}

	s.vfs.Open("file:///x.go", "go", 2, []byte("package main // edited\n"), vfs.SourceEditor)
	s.publishSnapshot()
	close(be.release)

	var resp *jsonrpc.Message
	select {
	case resp = <-response:
	case <-time.After(2 * time.Second):
		t.Fatal("rename did not finish")
	}
	if resp == nil || resp.Error == nil || resp.Error.Code != jsonrpc.ContentModified {
		t.Fatalf("in-flight stale rename response = %#v, want ContentModified", resp)
	}

	traceMsg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 17}, "omnilsp/queryTrace", json.RawMessage(`{}`))
	traceResp := s.dispatcher.Dispatch(context.Background(), traceMsg)
	if traceResp == nil || traceResp.Error != nil {
		t.Fatalf("queryTrace failed: %#v", traceResp)
	}
	var trace struct {
		RenameRequestsStarted uint64
		RenameStaleRejected   uint64
	}
	if err := json.Unmarshal(traceResp.Result, &trace); err != nil {
		t.Fatalf("decode queryTrace: %v", err)
	}
	if trace.RenameRequestsStarted != 1 || trace.RenameStaleRejected != 1 {
		t.Fatalf("rename trace = %+v, want started=1 staleRejected=1", trace)
	}
}

func provenRename() languages.ValidatedEdit {
	return languages.ValidatedEdit{
		Complete: true,
		Edits: []languages.TextEdit{{
			URI: "file:///x.go", StartLine: 0, StartChar: 7,
			EndLine: 0, EndChar: 11, NewText: "bar",
		}},
	}
}

// TestF11_IdenticalHoverJoinsSingleExecution proves the server wires
// CoalesceKey end to end: two concurrent identical hovers share ONE backend
// execution and both receive the result (J6 single-flight, F11).
func TestF11_IdenticalHoverJoinsSingleExecution(t *testing.T) {
	var execs int32
	release := make(chan struct{})
	be := &capturingHoverBackend{onHover: func([]byte) {
		atomic.AddInt32(&execs, 1)
		<-release
	}}
	s := New(DefaultConfig())
	s.RegisterBackend("go", be)
	ft := attachTransport(s)
	stop := startScheduler(s)
	defer stop()
	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()

	openDoc(s, "file:///x.go", 1, "package main\n")

	params := `{"textDocument":{"uri":"file:///x.go"},"position":{"line":0,"character":0}}`
	send := func(id int64) {
		s.scheduleMessage(context.Background(),
			jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, "textDocument/hover", json.RawMessage(params)))
	}
	send(1) // leader
	send(2) // must join the leader's in-flight computation

	waitFor(t, func() bool { return s.scheduler.Stats().TotalJoined == 1 })
	close(release)

	waitFor(t, func() bool {
		resp := ft.responses()
		return len(resp) >= 2
	})
	if n := atomic.LoadInt32(&execs); n != 1 {
		t.Errorf("backend executions = %d, want exactly 1", n)
	}
}

// capturingHoverBackend records the content slice the server handed it.
type capturingHoverBackend struct {
	mockBackend
	onHover func(content []byte)
}

func (c *capturingHoverBackend) Hover(_ context.Context, req languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	c.onHover(req.Content)
	return identity.SemanticResult[*languages.HoverResult]{
		Status:   identity.ResultExact,
		Value:    &languages.HoverResult{Contents: "hover"},
		Evidence: []identity.Evidence{{}},
	}, nil
}
