package server

// Tests closing the P0 loop found in audit: the scheduler path must never
// drop accepted state transitions (PROT-SYNC-001/C5/F12), read queries must
// serve their captured snapshot (INV-SNAPSHOT-002), and S3 rename must get
// ContentModified when its snapshot aged (C6/D11).

import (
	"context"
	"encoding/json"
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
