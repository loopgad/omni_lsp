package ccls

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/nested"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

type lifecycleWriter struct{ frames chan string }

func (w *lifecycleWriter) Write(p []byte) (int, error) {
	w.frames <- string(p)
	return len(p), nil
}

func (w *lifecycleWriter) Close() error { return nil }

func readLifecycleMessage(t *testing.T, frames <-chan string) map[string]json.RawMessage {
	t.Helper()
	select {
	case frame := <-frames:
		separator := strings.Index(frame, "\r\n\r\n")
		if separator < 0 {
			t.Fatalf("malformed LSP frame %q", frame)
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal([]byte(frame[separator+4:]), &message); err != nil {
			t.Fatalf("decode LSP frame: %v", err)
		}
		return message
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for child notification")
		return nil
	}
}

func TestDidCloseDocumentFencesOldSnapshotsAndReopensNewer(t *testing.T) {
	stdout, child := io.Pipe()
	stdin := &lifecycleWriter{frames: make(chan string, 4)}
	conn := nested.New(nested.Config{Name: "clangd", Lang: "cpp", WorkDir: t.TempDir()})
	conn.Attach(nil, stdin, stdout)
	b := &Backend{conn: conn}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = child.Close()
	})

	const uri = "file:///workspace/main.cpp"
	b.didOpen(uri, []byte("int older;\n"), 5)
	opened := readLifecycleMessage(t, stdin.frames)
	var openMethod string
	if err := json.Unmarshal(opened["method"], &openMethod); err != nil || openMethod != "textDocument/didOpen" {
		t.Fatalf("initial sync method = %q, err %v", openMethod, err)
	}

	if err := b.DidCloseDocument(uri, 5); err != nil {
		t.Fatalf("DidCloseDocument: %v", err)
	}
	closed := readLifecycleMessage(t, stdin.frames)
	var closeMethod string
	if err := json.Unmarshal(closed["method"], &closeMethod); err != nil || closeMethod != "textDocument/didClose" {
		t.Fatalf("close method = %q, err %v", closeMethod, err)
	}
	if _, err := conn.SyncDocumentAtRevision("cpp", uri, []byte("int stale;\n"), 5); err == nil {
		t.Fatal("stale C++ snapshot was accepted after didClose")
	}

	result := make(chan error, 1)
	go func() {
		_, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
			URI: uri, Content: []byte("int newer;\n"), SnapshotRev: 6,
		})
		result <- err
	}()
	reopened := readLifecycleMessage(t, stdin.frames)
	var reopenMethod string
	if err := json.Unmarshal(reopened["method"], &reopenMethod); err != nil || reopenMethod != "textDocument/didOpen" {
		t.Fatalf("reopen method = %q, err %v", reopenMethod, err)
	}
	var params struct {
		TextDocument struct {
			Version int32  `json:"version"`
			Text    string `json:"text"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(reopened["params"], &params); err != nil {
		t.Fatal(err)
	}
	if params.TextDocument.Version != 2 || params.TextDocument.Text != "int newer;\n" {
		t.Fatalf("reopened child document = %+v, want newer content at version 2", params.TextDocument)
	}
	request := readLifecycleMessage(t, stdin.frames)
	var method string
	if err := json.Unmarshal(request["method"], &method); err != nil || method != "textDocument/documentSymbol" {
		t.Fatalf("document symbol request method = %q, err %v", method, err)
	}
	var id int64
	if err := json.Unmarshal(request["id"], &id); err != nil || id == 0 {
		t.Fatalf("document symbol request id = %d, err %v", id, err)
	}
	response, err := json.Marshal(&jsonrpc.Message{
		JSONRPC: jsonrpc.Version,
		ID:      &jsonrpc.RequestID{Num: id},
		Result:  json.RawMessage("[]"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(child, "Content-Length: %d\r\n\r\n%s", len(response), response); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("DocumentSymbols after reopen: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("document symbol request did not complete")
	}
}

func TestDeferredDiagnosticsSyncsCloseRevisionWithoutClaimingResults(t *testing.T) {
	stdout, child := io.Pipe()
	stdin := &lifecycleWriter{frames: make(chan string, 4)}
	conn := nested.New(nested.Config{Name: "clangd", Lang: "cpp", WorkDir: t.TempDir()})
	conn.Attach(nil, stdin, stdout)
	b := &Backend{conn: conn}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = child.Close()
	})

	const uri = "file:///workspace/diagnostics.cpp"
	items, err := b.DiagnosticsWithEncoding(context.Background(), uri, []byte("int before;\n"), 20, 0)
	if err != nil || len(items) != 0 {
		t.Fatalf("deferred diagnostics = (%v, %v), want no diagnostics and no transport error", items, err)
	}
	open := readLifecycleMessage(t, stdin.frames)
	var openMethod string
	_ = json.Unmarshal(open["method"], &openMethod)
	if openMethod != "textDocument/didOpen" {
		t.Fatalf("initial diagnostics sync method = %q", openMethod)
	}
	if err := b.DidCloseDocument(uri, 20); err != nil {
		t.Fatal(err)
	}
	closed := readLifecycleMessage(t, stdin.frames)
	var closeMethod string
	_ = json.Unmarshal(closed["method"], &closeMethod)
	if closeMethod != "textDocument/didClose" {
		t.Fatalf("close method = %q", closeMethod)
	}

	if items, err := b.DiagnosticsWithEncoding(context.Background(), uri, []byte("int stale;\n"), 20, 0); err == nil || len(items) != 0 {
		t.Fatalf("stale deferred diagnostics = (%v, %v), want revision error", items, err)
	}
	items, err = b.DiagnosticsWithEncoding(context.Background(), uri, []byte("int after;\n"), 21, 0)
	if err != nil || len(items) != 0 {
		t.Fatalf("newer deferred diagnostics = (%v, %v), want empty deferred result", items, err)
	}
	reopened := readLifecycleMessage(t, stdin.frames)
	var reopenMethod string
	_ = json.Unmarshal(reopened["method"], &reopenMethod)
	if reopenMethod != "textDocument/didOpen" {
		t.Fatalf("newer diagnostics sync method = %q, want didOpen", reopenMethod)
	}
}
