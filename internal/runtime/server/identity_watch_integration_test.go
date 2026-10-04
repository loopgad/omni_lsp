package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/protocol/lsp"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

func TestEquivalentDocumentURIsShareLifecycle(t *testing.T) {
	s := New(DefaultConfig())
	defer s.diag.Close()
	s.RegisterBackend("go", &contentHoverBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}})
	if _, err := s.CallMethod(context.Background(), "initialize", InitializeParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CallMethod(context.Background(), "initialized", nil); err != nil {
		t.Fatal(err)
	}
	first := "file:///C:/workspace/%6dain.go"
	alias := "file:///c%3A/workspace/main.go"
	notify := func(method string, params any) {
		t.Helper()
		_, err := s.CallMethod(context.Background(), method, params)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	notify("textDocument/didOpen", DidOpenTextDocumentParams{TextDocument: textDocumentItem(first, 1, "package p\n")})
	before := s.vfs.Revision()
	notify("textDocument/didOpen", DidOpenTextDocumentParams{TextDocument: textDocumentItem(alias, 1, "package p\n")})
	if s.vfs.Revision() != before {
		t.Fatal("idempotent alias open advanced revision")
	}
	if got := s.snapMgr.Current().Document(alias); got == nil || got.URI != first {
		t.Fatalf("snapshot alias = %+v", got)
	}
	_, err := s.CallMethod(context.Background(), "textDocument/didOpen", DidOpenTextDocumentParams{TextDocument: textDocumentItem(alias, 2, "package changed\n")})
	if err == nil || s.vfs.Revision() != before {
		t.Fatal("conflicting open was not rejected atomically")
	}
	notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": alias, "version": 2}, "contentChanges": []map[string]any{{"text": "package edited\n"}}})
	if got := string(s.vfs.Content(first)); got != "package edited\n" {
		t.Fatalf("alias change = %q", got)
	}
	if response := dispatchHoverRaw(s, alias); response == nil || response.Error != nil || !strings.Contains(string(response.Result), "package edited") {
		t.Fatalf("alias query did not consume updated snapshot: %+v", response)
	}
	s.diag.mu.Lock()
	for key := range s.diag.timers {
		if key != canonicalDocumentURI(first) {
			t.Errorf("diagnostics retained alias timer %q", key)
		}
	}
	s.diag.mu.Unlock()
	notify("textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": alias}})
	if s.vfs.Get(first) != nil || s.snapMgr.Current().Document(alias) != nil {
		t.Fatal("alias close left editor state")
	}
	s.diag.mu.Lock()
	defer s.diag.mu.Unlock()
	if len(s.diag.timers) != 0 || len(s.diag.runs) != 0 || len(s.diag.cache) != 0 {
		t.Fatal("alias close retained diagnostic state")
	}
}

func textDocumentItem(uri string, version int64, text string) lsp.TextDocumentItem {
	return lsp.TextDocumentItem{URI: uri, LanguageID: "go", Version: version, Text: text}
}

type queuedWatchTransport struct {
	*fakeTransport
	in chan *jsonrpc.Message
}

func (t *queuedWatchTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	select {
	case msg := <-t.in:
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRunStartsPollerAfterInitializeAndStopsOnExit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "dependency.go")
	if err := os.WriteFile(path, []byte("package first\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rootURI := workspaceuri.FromPath(root).String()
	// Escaped drive colon also exercises the on-disk root conversion.
	rootURI = "file:///" + strings.Replace(strings.TrimPrefix(rootURI, "file:///"), ":", "%3A", 1)
	cfg := DefaultConfig()
	cfg.WatchInterval = 5 * time.Millisecond
	s := New(cfg)
	tr := &queuedWatchTransport{fakeTransport: newFakeTransport(), in: make(chan *jsonrpc.Message)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, tr) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop")
		}
	}()
	initParams, _ := json.Marshal(InitializeParams{RootURI: rootURI})
	tr.in <- jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", initParams)
	tr.in <- jsonrpc.NewNotification("initialized", nil)
	// Wait for the asynchronous complete baseline before mutating disk.
	openParams, _ := json.Marshal(DidOpenTextDocumentParams{TextDocument: textDocumentItem(workspaceuri.FromPath(path).String(), 1, "package unsaved\n")})
	tr.in <- jsonrpc.NewNotification("textDocument/didOpen", openParams)
	deadline := time.Now().Add(5 * time.Second)
	for s.vfs.Revision() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	before := s.vfs.Revision()
	if before == 0 {
		t.Fatal("didOpen did not complete")
	}
	for time.Now().Before(deadline) {
		s.mu.RLock()
		poller := s.workspacePoller
		s.mu.RUnlock()
		if poller != nil && poller.HasBaseline() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s.mu.RLock()
	poller := s.workspacePoller
	s.mu.RUnlock()
	if poller == nil || !poller.HasBaseline() {
		t.Fatal("poller did not establish a complete baseline")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package other\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	for s.vfs.Revision() == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.vfs.Revision() == before {
		t.Fatal("external same-size same-time change never invalidated snapshot")
	}
	if got := string(s.vfs.Content(workspaceuri.FromPath(path).String())); got != "package unsaved\n" {
		t.Fatalf("poller overwrote editor buffer: %q", got)
	}
}
