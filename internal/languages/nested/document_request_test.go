package nested

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/supervisor"
)

func TestSendRequestAtRevisionRejectsResponseAfterDocumentAdvances(t *testing.T) {
	stdout, child := io.Pipe()
	recorder := newNotificationRecorder()
	conn := New(Config{
		Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(),
		RequestTimeout: time.Second,
	})
	conn.Attach(nil, recorder, stdout)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = child.Close()
	})

	const uri = "file:///workspace/main.go"
	if _, err := conn.SyncDocumentAtRevision("go", uri, []byte("package old\n"), 1); err != nil {
		t.Fatal(err)
	}
	_ = decodeRecordedNotification(t, <-recorder.frames) // rev1 didOpen

	type response struct {
		result json.RawMessage
		err    error
	}
	resultCh := make(chan response, 1)
	go func() {
		result, err := conn.SendRequestAtRevision(context.Background(), "go", uri, []byte("package old\n"), 1, "textDocument/hover", nil)
		resultCh <- response{result: result, err: err}
	}()

	request := decodeRecordedNotification(t, <-recorder.frames)
	if request.Method != "textDocument/hover" || request.ID == nil {
		t.Fatalf("request = %+v, want an identified hover request", request)
	}

	if _, err := conn.SyncDocumentAtRevision("go", uri, []byte("package new\n"), 2); err != nil {
		t.Fatal(err)
	}
	change := decodeRecordedNotification(t, <-recorder.frames)
	if change.Method != "textDocument/didChange" {
		t.Fatalf("revision 2 sync method = %q, want didChange", change.Method)
	}

	rawResponse, err := json.Marshal(&jsonrpc.Message{
		JSONRPC: jsonrpc.Version,
		ID:      request.ID,
		Result:  json.RawMessage(`{"contents":{"value":"stale"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(child, "Content-Length: %d\r\n\r\n%s", len(rawResponse), rawResponse); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-resultCh:
		if !ierrors.IsKind(got.err, ierrors.ErrContentModified) {
			t.Fatalf("stale request error = %v, want ErrContentModified", got.err)
		}
		if got.result != nil {
			t.Fatalf("stale child result was accepted: %s", got.result)
		}
	case <-time.After(time.Second):
		t.Fatal("stale request did not complete after child response")
	}
}

func TestSendRequestAtRevisionWithEpochRequiresReadyReaderBinding(t *testing.T) {
	firstStdout, firstChild := io.Pipe()
	firstRecorder := newNotificationRecorder()
	conn := New(Config{Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(), RequestTimeout: time.Second})
	conn.sup = supervisor.New(supervisor.DefaultConfig())
	conn.Attach(nil, firstRecorder, firstStdout)
	conn.MarkReady()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = firstChild.Close()
	})

	const uri = "file:///workspace/main.go"
	firstToken, err := conn.SyncDocumentAtRevision("go", uri, []byte("package main\n"), 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = decodeRecordedNotification(t, <-firstRecorder.frames)
	if epoch, err := conn.readyEpochForDocument(firstToken); err != nil || epoch != 1 {
		t.Fatalf("first ready binding = (%d, %v), want (1, nil)", epoch, err)
	}

	secondStdout, secondChild := io.Pipe()
	secondRecorder := newNotificationRecorder()
	conn.Attach(nil, secondRecorder, secondStdout)
	t.Cleanup(func() { _ = secondChild.Close() })
	if _, err := conn.SyncDocumentAtRevision("go", uri, []byte("package main\n"), 1); err != nil {
		t.Fatal(err)
	}
	_ = decodeRecordedNotification(t, <-secondRecorder.frames)
	if _, epoch, err := conn.SendRequestAtRevisionWithEpoch(context.Background(), "go", uri, []byte("package main\n"), 1, "textDocument/hover", nil); !ierrors.IsKind(err, ierrors.ErrBackendUnavailable) || epoch != 0 {
		t.Fatalf("unready replacement request = epoch %d, err %v; want unavailable with no claimed epoch", epoch, err)
	}
	select {
	case frame := <-secondRecorder.frames:
		t.Fatalf("unready replacement sent a request: %s", frame)
	default:
	}

	conn.MarkReady()
	type requestResult struct {
		raw   json.RawMessage
		epoch uint64
		err   error
	}
	resultCh := make(chan requestResult, 1)
	go func() {
		raw, epoch, err := conn.SendRequestAtRevisionWithEpoch(context.Background(), "go", uri, []byte("package main\n"), 1, "textDocument/hover", nil)
		resultCh <- requestResult{raw: raw, epoch: epoch, err: err}
	}()
	request := decodeRecordedNotification(t, <-secondRecorder.frames)
	if request.Method != "textDocument/hover" || request.ID == nil {
		t.Fatalf("ready request = %+v, want an identified hover request", request)
	}
	rawResponse, err := json.Marshal(&jsonrpc.Message{JSONRPC: jsonrpc.Version, ID: request.ID, Result: json.RawMessage(`{"contents":{"value":"ready"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(secondChild, "Content-Length: %d\r\n\r\n%s", len(rawResponse), rawResponse); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-resultCh:
		if got.err != nil || got.epoch != 2 || string(got.raw) != `{"contents":{"value":"ready"}}` {
			t.Fatalf("ready replacement result = (raw %s, epoch %d, err %v), want epoch 2 response", got.raw, got.epoch, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("ready replacement request did not complete")
	}

	type staleResult struct {
		epoch uint64
		err   error
	}
	staleCh := make(chan staleResult, 1)
	go func() {
		_, epoch, err := conn.SendRequestAtRevisionWithEpoch(context.Background(), "go", uri, []byte("package main\n"), 1, "textDocument/hover", nil)
		staleCh <- staleResult{epoch: epoch, err: err}
	}()
	staleRequest := decodeRecordedNotification(t, <-secondRecorder.frames)
	if staleRequest.Method != "textDocument/hover" || staleRequest.ID == nil {
		t.Fatalf("in-flight request = %+v, want an identified hover request", staleRequest)
	}
	thirdStdout, thirdChild := io.Pipe()
	thirdRecorder := newNotificationRecorder()
	conn.Attach(nil, thirdRecorder, thirdStdout)
	t.Cleanup(func() { _ = thirdChild.Close() })
	conn.MarkReady()
	select {
	case got := <-staleCh:
		if !ierrors.IsKind(got.err, ierrors.ErrContentModified) || got.epoch != 2 {
			t.Fatalf("replaced in-flight result epoch = %d, err = %v; want original epoch 2 and ContentModified", got.epoch, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement did not reject the old in-flight request")
	}
}

func TestSendRequestAtRevisionRejectsResponseAfterOtherDocumentAdvancesWorkspace(t *testing.T) {
	stdout, child := io.Pipe()
	recorder := newNotificationRecorder()
	conn := New(Config{
		Name: "fake-lsp", Lang: "test", WorkDir: t.TempDir(),
		RequestTimeout: time.Second,
	})
	conn.Attach(nil, recorder, stdout)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = child.Close()
	})

	const uriA = "file:///workspace/a.go"
	const uriB = "file:///workspace/b.go"
	if _, err := conn.SyncDocumentAtRevision("go", uriA, []byte("package a\n"), 1); err != nil {
		t.Fatal(err)
	}
	_ = decodeRecordedNotification(t, <-recorder.frames) // rev1 didOpen for A

	type response struct {
		result json.RawMessage
		err    error
	}
	resultCh := make(chan response, 1)
	go func() {
		result, err := conn.SendRequestAtRevision(context.Background(), "go", uriA, []byte("package a\n"), 1, "textDocument/references", nil)
		resultCh <- response{result: result, err: err}
	}()

	request := decodeRecordedNotification(t, <-recorder.frames)
	if request.Method != "textDocument/references" || request.ID == nil {
		t.Fatalf("request = %+v, want an identified references request", request)
	}

	// A child LSP owns one mutable workspace. Syncing another URI at a later
	// workspace snapshot invalidates the in-flight result for A as well.
	if _, err := conn.SyncDocumentAtRevision("go", uriB, []byte("package b\n"), 2); err != nil {
		t.Fatal(err)
	}
	otherDocumentSync := decodeRecordedNotification(t, <-recorder.frames)
	if otherDocumentSync.Method != "textDocument/didOpen" {
		t.Fatalf("other document sync method = %q, want didOpen", otherDocumentSync.Method)
	}

	rawResponse, err := json.Marshal(&jsonrpc.Message{
		JSONRPC: jsonrpc.Version,
		ID:      request.ID,
		Result:  json.RawMessage(`[{"uri":"file:///workspace/b.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":5}}}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(child, "Content-Length: %d\r\n\r\n%s", len(rawResponse), rawResponse); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-resultCh:
		if !ierrors.IsKind(got.err, ierrors.ErrContentModified) {
			t.Fatalf("stale cross-document request error = %v, want ErrContentModified", got.err)
		}
		if got.result != nil {
			t.Fatalf("mixed-workspace child result was accepted: %s", got.result)
		}
	case <-time.After(time.Second):
		t.Fatal("cross-document stale request did not complete after child response")
	}

	if _, err := conn.SyncDocumentAtRevision("go", uriA, []byte("package a\n"), 1); !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("old workspace snapshot sync error = %v, want ErrContentModified", err)
	}
}
