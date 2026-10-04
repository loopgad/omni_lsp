package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

func TestWatchedExternalDependencyInvalidatesGoDefinitionCache(t *testing.T) {
	testExternalDependencyInvalidatesGoDefinitionCache(t, true)
}

func TestUnnotifiedExternalDependencyInvalidatesGoDefinitionCache(t *testing.T) {
	testExternalDependencyInvalidatesGoDefinitionCache(t, false)
}

func TestWatchedCreateRenameDeleteRefreshesDefinitionWithoutClientHints(t *testing.T) {
	setExternalChangeGoEnvironment(t)
	root := t.TempDir()
	writeExternalChangeFile(t, filepath.Join(root, "go.mod"), `module externalchanges

go 1.26.1
`)
	mainPath := filepath.Join(root, "main.go")
	diskSource := "package externalchanges\n\nfunc caller() {\n\tTarget()\n}\n"
	dirtySource := diskSource + "// editor-only\n"
	writeExternalChangeFile(t, mainPath, diskSource)
	mainURI := uri.FromPath(mainPath).String()

	cfg := DefaultConfig()
	cfg.WatchInterval = 5 * time.Millisecond
	s := New(cfg)
	s.RegisterBackend("go", golang.New(root))
	tr := &queuedWatchTransport{fakeTransport: newFakeTransport(), in: make(chan *jsonrpc.Message, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx, tr) }()
	runFinished := false
	t.Cleanup(func() {
		cancel()
		if runFinished {
			return
		}
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop")
		}
	})

	initParams, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
	if err != nil {
		t.Fatal(err)
	}
	tr.in <- jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", initParams)
	tr.in <- jsonrpc.NewNotification("initialized", nil)
	openParams, err := json.Marshal(map[string]any{
		"textDocument": map[string]any{
			"uri": mainURI, "languageId": "go", "version": 11, "text": dirtySource,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	tr.in <- jsonrpc.NewNotification("textDocument/didOpen", openParams)
	waitForExternalWatchReady(t, s, mainURI)

	line, column := definitionQueryPosition(dirtySource, "Target()")
	if got := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 10); len(got) != 0 {
		t.Fatalf("definition before dependency create = %+v; want no result", got)
	}

	createdPath := filepath.Join(root, "created.go")
	createdURI := uri.FromPath(createdPath).String()
	before := s.VFS().Revision()
	writeExternalChangeFile(t, createdPath, "package externalchanges\n\nfunc Target() {}\n")
	waitForExternalSync(t, s, before)
	created := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 11)
	if len(created) != 1 || created[0].URI != createdURI {
		t.Fatalf("definition after disk create = %+v; want %s", created, createdURI)
	}

	renamedPath := filepath.Join(root, "renamed.go")
	renamedURI := uri.FromPath(renamedPath).String()
	before = s.VFS().Revision()
	if err := os.Rename(createdPath, renamedPath); err != nil {
		t.Fatal(err)
	}
	waitForExternalSync(t, s, before)
	renamed := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 12)
	if len(renamed) != 1 || renamed[0].URI != renamedURI || renamed[0].URI == createdURI {
		t.Fatalf("definition after disk rename = %+v; want %s and no stale %s", renamed, renamedURI, createdURI)
	}

	before = s.VFS().Revision()
	if err := os.Remove(renamedPath); err != nil {
		t.Fatal(err)
	}
	waitForExternalSync(t, s, before)
	deleted := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 13)
	if len(deleted) != 0 {
		t.Fatalf("definition after disk delete = %+v; want no stale exact target", deleted)
	}
	if file := s.VFS().Get(mainURI); file == nil || file.Version != 11 || !file.Dirty || string(file.Content) != dirtySource {
		t.Fatalf("disk watcher replaced the dirty open buffer: %+v", file)
	}
	if s.VFS().Get(createdURI) != nil || s.VFS().Get(renamedURI) != nil {
		t.Fatal("external file operation opened a closed dependency in the VFS")
	}

	// Force a scan error while its evidence callback waits on the shared mutation
	// lock, then stop Run. CloseAndWait must release once the lock is released.
	s.mutationMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mutationMu.Unlock()
		}
	}()
	movedRoot := root + "-moved"
	if err := os.Rename(root, movedRoot); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := os.Stat(movedRoot); err == nil {
			_ = os.Rename(movedRoot, root)
		}
	}()
	waitForExternalScanError(t, s)
	cancel()
	select {
	case err := <-runDone:
		t.Fatalf("Run stopped before the scan error callback left mutation lock: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	s.mutationMu.Unlock()
	locked = false
	select {
	case err := <-runDone:
		runFinished = true
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run shutdown error = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run shutdown deadlocked after releasing mutation lock")
	}
}

func testExternalDependencyInvalidatesGoDefinitionCache(t *testing.T, notify bool) {
	setExternalChangeGoEnvironment(t)
	root := t.TempDir()
	writeExternalChangeFile(t, filepath.Join(root, "go.mod"), "module externalchange\n\ngo 1.26.1\n")
	depPath := filepath.Join(root, "dep.go")
	depBefore := "package externalchange\n\n  func Target() {}\n"
	writeExternalChangeFile(t, depPath, depBefore)

	mainPath := filepath.Join(root, "main.go")
	mainSource := "package externalchange\n\nfunc caller() {\n\tTarget()\n}\n"
	writeExternalChangeFile(t, mainPath, mainSource)
	depURI := uri.FromPath(depPath).String()
	mainURI := uri.FromPath(mainPath).String()

	backend := golang.New(root)
	defer backend.Close()
	s := New(DefaultConfig())
	s.RegisterBackend("go", backend)
	s.VFS().Open(mainURI, "go", 11, []byte(mainSource), vfs.SourceEditor)
	s.publishSnapshot()
	if s.VFS().Get(depURI) != nil {
		t.Fatal("dependency file must remain closed in the VFS")
	}

	line, column := definitionQueryPosition(mainSource, "Target()")
	first := dispatchDefinitionForExternalChange(t, s, mainURI, line, column, 1)
	second := dispatchDefinitionForExternalChange(t, s, mainURI, line, column, 2)
	if first.URI != depURI || second.URI != depURI || first.StartLine != second.StartLine {
		t.Fatalf("warm definition = (%+v), then (%+v); want the same dependency target", first, second)
	}

	file := s.VFS().Get(mainURI)
	if file == nil {
		t.Fatal("open main.go disappeared before external change")
	}
	openContent := string(file.Content)
	openVersion := file.Version
	beforeRevision := s.VFS().Revision()
	beforeSnapshotRevision := s.SnapshotRevision()
	depAfter := "package externalchange\n\n\n func Target() {}\n"
	stat, err := os.Stat(depPath)
	if err != nil {
		t.Fatal(err)
	}
	writeExternalChangeFile(t, depPath, depAfter)
	if err := os.Chtimes(depPath, stat.ModTime(), stat.ModTime()); err != nil {
		t.Fatal(err)
	}

	params, err := json.Marshal(map[string]any{
		"changes": []map[string]any{{"uri": depURI, "type": 2}},
	})
	if err != nil {
		t.Fatalf("marshal watched-file notification: %v", err)
	}
	if notify {
		s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewNotification("workspace/didChangeWatchedFiles", params))
	}

	if got := s.VFS().Revision(); notify && got <= beforeRevision {
		t.Fatalf("VFS revision after external change = %d, want > %d", got, beforeRevision)
	}
	if got := s.SnapshotRevision(); notify && (got <= beforeSnapshotRevision || got != s.VFS().Revision()) {
		t.Fatalf("published snapshot revision after external change = %d, VFS revision = %d (before %d)", got, s.VFS().Revision(), beforeSnapshotRevision)
	}
	file = s.VFS().Get(mainURI)
	if file == nil || file.Version != openVersion || string(file.Content) != openContent {
		t.Fatalf("open main.go changed during external invalidation: file=%+v", file)
	}
	if s.VFS().Get(depURI) != nil {
		t.Fatal("watched dependency notification opened the closed dependency in the VFS")
	}

	updated := dispatchDefinitionForExternalChange(t, s, mainURI, line, column, 3)
	if updated.URI != depURI || updated.StartLine != first.StartLine+1 {
		t.Fatalf("definition after dependency edit = %+v; before=%+v; want target line to advance by one", updated, first)
	}
}

func TestExternalFileHintsValidateBeforeAdvancingRevision(t *testing.T) {
	uriA := uri.FromPath(filepath.Join(t.TempDir(), "before.go")).String()
	uriB := uri.FromPath(filepath.Join(t.TempDir(), "after.go")).String()
	tests := []struct {
		name    string
		method  string
		valid   any
		invalid any
		empty   any
	}{
		{
			name:   "watched changes",
			method: "workspace/didChangeWatchedFiles",
			valid: struct {
				Changes []struct {
					URI  string `json:"uri"`
					Type int    `json:"type"`
				} `json:"changes"`
			}{Changes: []struct {
				URI  string `json:"uri"`
				Type int    `json:"type"`
			}{{URI: uriA, Type: 2}}},
			invalid: struct {
				Changes []struct {
					URI  string `json:"uri"`
					Type int    `json:"type"`
				} `json:"changes"`
			}{Changes: []struct {
				URI  string `json:"uri"`
				Type int    `json:"type"`
			}{{URI: uriA, Type: 2}, {URI: "not-a-file-uri", Type: 3}}},
			empty: struct {
				Changes []struct {
					URI  string `json:"uri"`
					Type int    `json:"type"`
				} `json:"changes"`
			}{},
		},
		{
			name:   "create files",
			method: "workspace/didCreateFiles",
			valid: struct {
				Files []struct {
					URI string `json:"uri"`
				} `json:"files"`
			}{Files: []struct {
				URI string `json:"uri"`
			}{{URI: uriA}}},
			invalid: struct {
				Files []struct {
					URI string `json:"uri"`
				} `json:"files"`
			}{Files: []struct {
				URI string `json:"uri"`
			}{{URI: uriA}, {URI: "not-a-file-uri"}}},
			empty: struct {
				Files []struct {
					URI string `json:"uri"`
				} `json:"files"`
			}{},
		},
		{
			name:   "rename files",
			method: "workspace/didRenameFiles",
			valid: struct {
				Files []struct {
					OldURI string `json:"oldUri"`
					NewURI string `json:"newUri"`
				} `json:"files"`
			}{Files: []struct {
				OldURI string `json:"oldUri"`
				NewURI string `json:"newUri"`
			}{{OldURI: uriA, NewURI: uriB}}},
			invalid: struct {
				Files []struct {
					OldURI string `json:"oldUri"`
					NewURI string `json:"newUri"`
				} `json:"files"`
			}{Files: []struct {
				OldURI string `json:"oldUri"`
				NewURI string `json:"newUri"`
			}{{OldURI: uriA, NewURI: uriB}, {OldURI: uriA}}},
			empty: struct {
				Files []struct {
					OldURI string `json:"oldUri"`
					NewURI string `json:"newUri"`
				} `json:"files"`
			}{},
		},
		{
			name:   "delete files",
			method: "workspace/didDeleteFiles",
			valid: struct {
				Files []struct {
					URI string `json:"uri"`
				} `json:"files"`
			}{Files: []struct {
				URI string `json:"uri"`
			}{{URI: uriA}}},
			invalid: struct {
				Files []struct {
					URI string `json:"uri"`
				} `json:"files"`
			}{Files: []struct {
				URI string `json:"uri"`
			}{{URI: uriA}, {URI: "not-a-file-uri"}}},
			empty: struct {
				Files []struct {
					URI string `json:"uri"`
				} `json:"files"`
			}{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(DefaultConfig())
			dispatchExternalFileHint(t, s, tc.method, tc.invalid)
			if got := s.VFS().Revision(); got != 0 {
				t.Fatalf("malformed batch advanced VFS revision to %d", got)
			}
			dispatchExternalFileHint(t, s, tc.method, tc.empty)
			if got := s.VFS().Revision(); got != 0 {
				t.Fatalf("empty batch advanced VFS revision to %d", got)
			}
			dispatchExternalFileHint(t, s, tc.method, tc.valid)
			if got := s.VFS().Revision(); got != 1 || s.SnapshotRevision() != got {
				t.Fatalf("valid batch revision = VFS %d, snapshot %d; want both 1", got, s.SnapshotRevision())
			}
		})
	}
}

func setExternalChangeGoEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOENV", "off")
	t.Setenv("GOTOOLCHAIN", "local")
}

func writeExternalChangeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func definitionQueryPosition(source, needle string) (uint32, uint32) {
	offset := strings.Index(source, needle)
	line := uint32(strings.Count(source[:offset], "\n"))
	lineStart := strings.LastIndex(source[:offset], "\n") + 1
	return line, uint32(offset - lineStart)
}

func waitForExternalWatchReady(t *testing.T, s *Server, mainURI string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		poller := s.workspacePoller
		s.mu.RUnlock()
		if s.VFS().Get(mainURI) != nil && poller != nil && poller.HasBaseline() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	s.mu.RLock()
	poller := s.workspacePoller
	s.mu.RUnlock()
	if poller == nil {
		t.Fatal("workspace poller was not started")
	}
	t.Fatalf("workspace poller did not establish a baseline: scan error=%v", poller.LastScanError())
}

func waitForExternalSync(t *testing.T, s *Server, before uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		revision := s.VFS().Revision()
		if revision > before && s.SnapshotRevision() == revision {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("external change did not publish a new snapshot: VFS=%d snapshot=%d before=%d", s.VFS().Revision(), s.SnapshotRevision(), before)
}

func waitForExternalScanError(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.RLock()
		poller := s.workspacePoller
		s.mu.RUnlock()
		if poller != nil && poller.LastScanError() != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("workspace poller did not report the missing-root scan error")
}

func dispatchDefinitionForExternalChange(t *testing.T, s *Server, fileURI string, line, column uint32, requestID int) struct {
	URI       string `json:"uri"`
	StartLine uint32
} {
	locations := dispatchDefinitionLocationsForExternalChange(t, s, fileURI, line, column, requestID)
	if len(locations) != 1 {
		t.Fatalf("definition locations = %+v; want one target", locations)
	}
	return locations[0]
}

func dispatchDefinitionLocationsForExternalChange(t *testing.T, s *Server, fileURI string, line, column uint32, requestID int) []struct {
	URI       string `json:"uri"`
	StartLine uint32
} {
	t.Helper()
	params, err := json.Marshal(struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position struct {
			Line      uint32 `json:"line"`
			Character uint32 `json:"character"`
		} `json:"position"`
	}{TextDocument: struct {
		URI string `json:"uri"`
	}{URI: fileURI}, Position: struct {
		Line      uint32 `json:"line"`
		Character uint32 `json:"character"`
	}{Line: line, Character: column}})
	if err != nil {
		t.Fatalf("marshal definition request: %v", err)
	}
	response := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: int64(requestID)}, "textDocument/definition", params))
	if response == nil || response.Error != nil {
		t.Fatalf("definition response = %+v", response)
	}
	var locations []struct {
		URI   string `json:"uri"`
		Range struct {
			Start struct {
				Line uint32 `json:"line"`
			} `json:"start"`
		} `json:"range"`
	}
	if err := json.Unmarshal(response.Result, &locations); err != nil {
		t.Fatalf("unmarshal definition response: %v", err)
	}
	results := make([]struct {
		URI       string `json:"uri"`
		StartLine uint32
	}, 0, len(locations))
	for _, location := range locations {
		results = append(results, struct {
			URI       string `json:"uri"`
			StartLine uint32
		}{URI: location.URI, StartLine: location.Range.Start.Line})
	}
	return results
}

func dispatchExternalFileHint(t *testing.T, s *Server, method string, params any) {
	t.Helper()
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal %s params: %v", method, err)
	}
	s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewNotification(method, data))
}
