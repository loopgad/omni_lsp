package nested

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

func TestBeginWorkspaceSnapshotSyncsEveryDocumentBeforeLease(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	docs := []SnapshotDocument{
		{URI: "file:///workspace/b.go", LangID: "go", Content: []byte("package b\n")},
		{URI: "file:///workspace/a.go", LangID: "go", Content: []byte("package a\n")},
	}

	leaseCtx, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 12, docs)
	if err != nil {
		t.Fatal(err)
	}
	if leaseCtx == nil {
		t.Fatal("snapshot lease did not return a derived request context")
	}
	defer finish()

	for _, wantURI := range []string{"file:///workspace/a.go", "file:///workspace/b.go"} {
		message := decodeRecordedNotification(t, <-recorder.frames)
		if message.Method != "textDocument/didOpen" {
			t.Fatalf("snapshot sync method = %q, want didOpen", message.Method)
		}
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			t.Fatal(err)
		}
		if params.TextDocument.URI != wantURI {
			t.Fatalf("snapshot opened %q, want %q", params.TextDocument.URI, wantURI)
		}
	}
	if got := conn.WorkspaceSnapshotGeneration(); got == 0 {
		t.Fatal("workspace generation was not advanced while reconciling the snapshot")
	}
}

func TestBeginWorkspaceSnapshotFastPathAllowsConcurrentReaders(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	docs := []SnapshotDocument{{URI: "file:///workspace/a.go", LangID: "go", Content: []byte("package a\n")}}

	_, firstFinish, err := conn.BeginWorkspaceSnapshot(context.Background(), 4, docs)
	if err != nil {
		t.Fatal(err)
	}
	defer firstFinish()
	_ = <-recorder.frames

	type result struct {
		finish func() error
		err    error
	}
	second := make(chan result, 1)
	go func() {
		_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 4, docs)
		second <- result{finish: finish, err: err}
	}()

	select {
	case got := <-second:
		if got.err != nil {
			t.Fatalf("concurrent same-revision snapshot: %v", got.err)
		}
		defer got.finish()
	case <-time.After(time.Second):
		_ = firstFinish()
		got := <-second
		if got.finish != nil {
			_ = got.finish()
		}
		t.Fatal("same-revision fast path waited for the existing query lease")
	}
}

func TestBeginWorkspaceSnapshotClosesDocumentsAbsentFromSnapshot(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	initial := []SnapshotDocument{
		{URI: "file:///workspace/a.go", LangID: "go", Content: []byte("package a\n")},
		{URI: "file:///workspace/b.go", LangID: "go", Content: []byte("package b\n")},
	}
	_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 20, initial)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames
	_ = <-recorder.frames
	if err := finish(); err != nil {
		t.Fatal(err)
	}

	_, finish, err = conn.BeginWorkspaceSnapshot(context.Background(), 21, initial[:1])
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	closeNotice := decodeRecordedNotification(t, <-recorder.frames)
	if closeNotice.Method != "textDocument/didClose" {
		t.Fatalf("snapshot reconcile method = %q, want didClose", closeNotice.Method)
	}
	var params struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(closeNotice.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.TextDocument.URI != "file:///workspace/b.go" {
		t.Fatalf("closed URI = %q, want B", params.TextDocument.URI)
	}
	select {
	case frame := <-recorder.frames:
		t.Fatalf("unchanged A received an unnecessary child notification: %s", frame)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestBeginWorkspaceSnapshotAdvancesRevisionWithoutUnchangedDidChanges(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	docs := []SnapshotDocument{
		{URI: "file:///workspace/a.go", LangID: "go", Content: []byte("package a\n")},
		{URI: "file:///workspace/b.go", LangID: "go", Content: []byte("package b\n")},
	}
	_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 22, docs)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames
	_ = <-recorder.frames
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	firstGeneration := conn.WorkspaceSnapshotGeneration()

	_, finish, err = conn.BeginWorkspaceSnapshot(context.Background(), 23, docs)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if got := conn.WorkspaceSnapshotGeneration(); got <= firstGeneration {
		t.Fatalf("workspace generation = %d after revision advanced, want > %d", got, firstGeneration)
	}
	select {
	case frame := <-recorder.frames:
		t.Fatalf("unchanged document received an unnecessary child notification: %s", frame)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestWorkspaceSnapshotLeaseCanonicalizesLookupButPreservesChildURI(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	const childURI = "FILE:///workspace/%6dain.py"
	const canonicalURI = "file:///workspace/main.py"
	content := []byte("value = 1\n")

	_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 89, []SnapshotDocument{{
		URI: childURI, LangID: "python", Content: content,
	}})
	if err != nil {
		t.Fatal(err)
	}
	initialFinish := finish
	defer initialFinish()
	open := decodeRecordedNotification(t, <-recorder.frames)
	if open.Method != "textDocument/didOpen" {
		t.Fatalf("open method = %q, want didOpen", open.Method)
	}
	var openParams struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(open.Params, &openParams); err != nil {
		t.Fatal(err)
	}
	if openParams.TextDocument.URI != childURI {
		t.Fatalf("child opened URI %q, want preserved display URI %q", openParams.TextDocument.URI, childURI)
	}
	if err := initialFinish(); err != nil {
		t.Fatal(err)
	}

	leaseCtx, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 90, []SnapshotDocument{
		{URI: childURI, LangID: "python", Content: content},
		{URI: "file:///workspace/other.py", LangID: "python", Content: []byte("other = 2\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	otherOpen := decodeRecordedNotification(t, <-recorder.frames)
	if otherOpen.Method != "textDocument/didOpen" {
		t.Fatalf("unrelated document method = %q, want didOpen", otherOpen.Method)
	}

	first, err := conn.RefreshDiagnosticsAtRevision(leaseCtx, "python", canonicalURI, content, 90)
	if err != nil {
		t.Fatalf("refresh by canonical URI alias: %v", err)
	}
	if first.URI != childURI {
		t.Fatalf("refresh token URI = %q, want child display URI %q", first.URI, childURI)
	}
	change := decodeRecordedNotification(t, <-recorder.frames)
	if change.Method != "textDocument/didChange" {
		t.Fatalf("refresh method = %q, want didChange", change.Method)
	}
	var changeParams struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(change.Params, &changeParams); err != nil {
		t.Fatal(err)
	}
	if changeParams.TextDocument.URI != childURI {
		t.Fatalf("refresh sent URI %q, want the actual child URI %q", changeParams.TextDocument.URI, childURI)
	}

	lease, ok := workspaceLeaseFromContext(leaseCtx)
	if !ok {
		t.Fatal("workspace lease missing from derived context")
	}
	updated, err := lease.document(canonicalURI, "python", content, 90)
	if err != nil {
		t.Fatalf("lookup refreshed token by canonical URI: %v", err)
	}
	if updated.URI != childURI || updated.Version != first.Version || updated.serial != first.serial {
		t.Fatalf("lease token after refresh = %+v, want updated child token %+v", updated, first)
	}
	second, err := conn.RefreshDiagnosticsAtRevision(leaseCtx, "python", canonicalURI, content, 90)
	if err != nil || second.Version != first.Version || second.serial != first.serial {
		t.Fatalf("repeat alias refresh = %+v, %v; want same token", second, err)
	}
	select {
	case frame := <-recorder.frames:
		t.Fatalf("idempotent alias refresh sent another notification: %s", frame)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestPrepareWorkspaceSnapshotRejectsCanonicalAliases(t *testing.T) {
	const displayURI = "FILE:///workspace/%6dain.py"
	snapshot, _, err := prepareWorkspaceSnapshot([]SnapshotDocument{{
		URI: displayURI, LangID: "python", Content: []byte("value = 1\n"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].URI != displayURI {
		t.Fatalf("prepared snapshot = %+v; want the original URI spelling preserved", snapshot)
	}
	if got := workspaceDocumentKey("opaque-document-id"); got != "opaque-document-id" {
		t.Fatalf("opaque document identity changed to %q", got)
	}

	_, _, err = prepareWorkspaceSnapshot([]SnapshotDocument{
		{URI: displayURI, LangID: "python", Content: []byte("same\n")},
		{URI: "file:///workspace/main.py", LangID: "python", Content: []byte("same\n")},
	})
	if !ierrors.IsKind(err, ierrors.ErrInvalidArgument) {
		t.Fatalf("snapshot with canonical URI aliases error = %v, want ErrInvalidArgument", err)
	}
}

func TestCloseDocumentRevisionRejectsOlderWorkspaceSnapshot(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	docs := []SnapshotDocument{{URI: "file:///workspace/b.go", LangID: "go", Content: []byte("package b\n")}}
	_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 30, docs)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames
	if err := finish(); err != nil {
		t.Fatal(err)
	}

	if err := conn.CloseDocument(docs[0].URI, 31); err != nil {
		t.Fatal(err)
	}
	closeNotice := decodeRecordedNotification(t, <-recorder.frames)
	if closeNotice.Method != "textDocument/didClose" {
		t.Fatalf("close method = %q, want didClose", closeNotice.Method)
	}
	if _, _, err := conn.BeginWorkspaceSnapshot(context.Background(), 30, docs); !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("older snapshot after close error = %v, want ErrContentModified", err)
	}
}

func TestBeginWorkspaceSnapshotReplaysAllDocumentsAfterRestart(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	docs := []SnapshotDocument{
		{URI: "file:///workspace/a.rs", LangID: "rust", Content: []byte("fn a() {}\n")},
		{URI: "file:///workspace/b.rs", LangID: "rust", Content: []byte("fn b() {}\n")},
	}
	_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 50, docs)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames
	_ = <-recorder.frames
	if err := finish(); err != nil {
		t.Fatal(err)
	}

	stdout, child := io.Pipe()
	replacementRecorder := newNotificationRecorder()
	conn.Attach(nil, replacementRecorder, stdout)
	t.Cleanup(func() { _ = child.Close() })

	_, finish, err = conn.BeginWorkspaceSnapshot(context.Background(), 50, docs)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	for range 2 {
		message := decodeRecordedNotification(t, <-replacementRecorder.frames)
		if message.Method != "textDocument/didOpen" {
			t.Fatalf("replay method = %q, want didOpen", message.Method)
		}
	}
}

func TestWorkspaceSnapshotFinishRejectsChangedGeneration(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	docs := []SnapshotDocument{{URI: "file:///workspace/a.go", LangID: "go", Content: []byte("package a\n")}}
	_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 70, docs)
	if err != nil {
		t.Fatal(err)
	}
	_ = <-recorder.frames

	// Simulate a generation change reported from outside the serialized
	// mutation path so finish's defensive stale check is exercised directly.
	conn.mu.Lock()
	conn.workspaceGeneration++
	conn.mu.Unlock()
	if err := finish(); !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("stale workspace lease finish error = %v, want ErrContentModified", err)
	}
}

func TestSendRequestAtRevisionUsesSnapshotScopedContext(t *testing.T) {
	conn, child, recorder := newDiagnosticsConn(t, time.Second)
	const uri = "file:///workspace/main.go"
	content := []byte("package main\n")
	leaseCtx, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 80, []SnapshotDocument{{
		URI: uri, LangID: "go", Content: content,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	_ = <-recorder.frames // didOpen

	type response struct {
		result json.RawMessage
		err    error
	}
	resultCh := make(chan response, 1)
	go func() {
		result, err := conn.SendRequestAtRevision(leaseCtx, "go", uri, content, 80, "textDocument/hover", nil)
		resultCh <- response{result: result, err: err}
	}()

	var request jsonrpc.Message
	select {
	case frame := <-recorder.frames:
		request = decodeRecordedNotification(t, frame)
	case <-time.After(time.Second):
		t.Fatal("request under the workspace lease tried to resynchronize under an exclusive lease")
	}
	if request.Method != "textDocument/hover" || request.ID == nil {
		t.Fatalf("snapshot-scoped request = %+v, want an identified hover", request)
	}
	responseFrame, err := json.Marshal(&jsonrpc.Message{
		JSONRPC: jsonrpc.Version,
		ID:      request.ID,
		Result:  json.RawMessage(`{"contents":"ok"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(child, "Content-Length: %d\r\n\r\n%s", len(responseFrame), responseFrame); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got.err != nil || string(got.result) != `{"contents":"ok"}` {
			t.Fatalf("snapshot-scoped request result = %s, %v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("snapshot-scoped request did not complete")
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.SendRequestAtRevision(leaseCtx, "go", uri, content, 80, "textDocument/hover", nil); !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("request after snapshot lease finish error = %v, want ErrContentModified", err)
	}
	if _, err := conn.SyncDocumentAtRevisionContext(leaseCtx, "go", uri, content, 80); !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("sync after snapshot lease finish error = %v, want ErrContentModified", err)
	}
}

func TestRefreshDiagnosticsAtRevisionRunsInsideSnapshotLease(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	const uri = "file:///workspace/main.py"
	content := []byte("value = 1\n")
	initialLeaseCtx, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 90, []SnapshotDocument{{
		URI: uri, LangID: "python", Content: content,
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	open := decodeRecordedNotification(t, <-recorder.frames)
	if open.Method != "textDocument/didOpen" {
		t.Fatalf("open method = %q, want didOpen", open.Method)
	}
	initial, err := conn.RefreshDiagnosticsAtRevision(initialLeaseCtx, "python", uri, content, 90)
	if err != nil || initial.Version != 1 {
		t.Fatalf("diagnostics immediately after didOpen = %+v, %v; want didOpen version 1", initial, err)
	}
	select {
	case frame := <-recorder.frames:
		t.Fatalf("refresh immediately after didOpen sent a redundant notification: %s", frame)
	case <-time.After(20 * time.Millisecond):
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}

	leaseCtx, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 91, []SnapshotDocument{
		{URI: uri, LangID: "python", Content: content},
		{URI: "file:///workspace/other.py", LangID: "python", Content: []byte("other = 2\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	otherOpen := decodeRecordedNotification(t, <-recorder.frames)
	if otherOpen.Method != "textDocument/didOpen" {
		t.Fatalf("unrelated new document method = %q, want didOpen", otherOpen.Method)
	}
	generation := conn.WorkspaceSnapshotGeneration()

	first, err := conn.RefreshDiagnosticsAtRevision(leaseCtx, "python", uri, content, 91)
	if err != nil {
		t.Fatal(err)
	}
	change := decodeRecordedNotification(t, <-recorder.frames)
	if change.Method != "textDocument/didChange" || first.Version != 2 {
		t.Fatalf("first refresh = method %q version %d; want didChange v2", change.Method, first.Version)
	}
	if first.WorkspaceGeneration() != generation || conn.WorkspaceSnapshotGeneration() != generation {
		t.Fatalf("same-content diagnostics refresh changed workspace generation: token=%d conn=%d want %d", first.WorkspaceGeneration(), conn.WorkspaceSnapshotGeneration(), generation)
	}

	second, err := conn.RefreshDiagnosticsAtRevision(leaseCtx, "python", uri, content, 91)
	if err != nil || second.Version != first.Version || second.serial != first.serial {
		t.Fatalf("duplicate refresh = %+v, %v; want the same per-generation token", second, err)
	}
	select {
	case frame := <-recorder.frames:
		t.Fatalf("idempotent refresh sent another notification: %s", frame)
	case <-time.After(20 * time.Millisecond):
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.RefreshDiagnosticsAtRevision(leaseCtx, "python", uri, content, 91); !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("refresh after lease finish error = %v, want ErrContentModified", err)
	}
	if _, err := conn.WaitForDiagnostics(leaseCtx, second); !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("diagnostics wait after lease finish error = %v, want ErrContentModified", err)
	}
}

func TestRefreshDiagnosticsAtRevisionCancellationWhileWaitingForSnapshotLease(t *testing.T) {
	conn, _, recorder := newDiagnosticsConn(t, time.Second)
	const uri = "file:///workspace/main.py"
	content := []byte("value = 1\n")
	_, finish, err := conn.BeginWorkspaceSnapshot(context.Background(), 92, []SnapshotDocument{{
		URI: uri, LangID: "python", Content: content,
	}})
	if err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = finish()
		}
	}()
	open := decodeRecordedNotification(t, <-recorder.frames)
	if open.Method != "textDocument/didOpen" {
		t.Fatalf("initial sync method = %q, want didOpen", open.Method)
	}

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		token DocumentVersion
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		token, err := conn.RefreshDiagnosticsAtRevision(ctx, "python", uri, content, 92)
		resultCh <- result{token: token, err: err}
	}()

	// Wait until the refresh is definitely queued behind the held snapshot
	// lease before canceling it. This avoids relying on scheduler timing.
	deadline := time.NewTimer(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	queued := false
waitForQueue:
	for {
		conn.workspaceLease.mu.Lock()
		queued = conn.workspaceLease.waitingWriters > 0
		conn.workspaceLease.mu.Unlock()
		if queued {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			break waitForQueue
		}
	}
	ticker.Stop()
	deadline.Stop()
	if !queued {
		cancel()
		t.Fatal("refresh did not queue behind the held workspace snapshot lease")
	}
	cancel()

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("canceled refresh remained blocked behind the held snapshot lease")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("canceled refresh error = %v, want context.Canceled", got.err)
	}

	conn.mu.Lock()
	version := conn.documents[uri].version
	conn.mu.Unlock()
	if version != 1 {
		t.Fatalf("document version while snapshot lease is held = %d, want unchanged version 1", version)
	}

	if err := finish(); err != nil {
		t.Fatal(err)
	}
	finished = true
	conn.mu.Lock()
	version = conn.documents[uri].version
	conn.mu.Unlock()
	if version != 1 {
		t.Fatalf("document version after releasing the lease = %d, want unchanged version 1", version)
	}
	select {
	case frame := <-recorder.frames:
		t.Fatalf("canceled refresh sent a child notification after the lease was released: %s", frame)
	case <-time.After(20 * time.Millisecond):
	}
}
