package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

// TestRunPollerForwardsRealFileURIsToBackends drives the D14 poller through
// the real Run loop and captures the SourceChange URIs the watcher callback
// hands to a SourceChangeSynchronizer backend (audit finding 6). The poller
// reported events relative to the workspace root and the callback converted
// them with workspaceuri.FromPath directly, producing file://pkg/a.go — a URI
// whose Path() resolves to \\pkg\a.go, a UNC host — so the per-file
// invalidation never reached the real file. Every forwarded URI must parse
// back to the actual absolute path on disk.
func TestRunPollerForwardsRealFileURIsToBackends(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "dependency.go")
	if err := os.WriteFile(file, []byte("package first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := workspaceuri.FromPath(root).String()
	// Escaped drive colon also exercises the on-disk root conversion.
	rootURI = "file:///" + strings.Replace(strings.TrimPrefix(rootURI, "file:///"), ":", "%3A", 1)

	cfg := DefaultConfig()
	cfg.WatchInterval = 5 * time.Millisecond
	s := New(cfg)
	backend := &sourceChangeBackend{mockBackend: &mockBackend{langID: "go", exts: []string{".go"}}, epoch: 1}
	s.RegisterBackend("go", backend)

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

	deadline := time.Now().Add(5 * time.Second)
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

	if err := os.WriteFile(file, []byte("package second\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for time.Now().Before(deadline) {
		_ = s.flushExternalSourceChanges(context.Background(), backend)
		if len(backend.changes) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(backend.changes) == 0 {
		t.Fatal("poller change never reached the backend synchronizer")
	}
	for _, change := range backend.changes {
		if !externalChangeURIValid(change.URI) {
			t.Errorf("forwarded URI %q failed externalChangeURIValid", change.URI)
			continue
		}
		parsed, err := workspaceuri.Parse(change.URI)
		if err != nil {
			t.Errorf("forwarded URI %q does not parse: %v", change.URI, err)
			continue
		}
		if got := parsed.PathOr(); got != file {
			t.Errorf("forwarded URI %q resolves to path %q, want the real file %q", change.URI, got, file)
		}
		if change.Kind != languages.SourceChangeChanged {
			t.Errorf("forwarded kind %d, want SourceChangeChanged", change.Kind)
		}
	}
}
