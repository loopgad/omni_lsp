package server

// WS-D14 release evidence. Two coverage gaps remain after the earlier rounds:
// complete external file-operation transitions (create/modify/rename/delete)
// with semantic invalidation and untouched editor buffers at every step, and
// candidate-wide external-dependency coverage — external changes in any
// candidate dependency location (a directory created later, a nested imported
// subpackage, a non-source dependency tree) must invalidate semantic state
// through the polling watcher without ever overwriting editor buffers.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

func TestExternalCreateModifyRenameDeleteTransitionsInvalidateSemanticsKeepBuffer(t *testing.T) {
	setExternalChangeGoEnvironment(t)
	root := t.TempDir()
	writeExternalChangeFile(t, filepath.Join(root, "go.mod"), `module transitionevidence

go 1.26.1
`)
	mainPath := filepath.Join(root, "main.go")
	diskSource := "package transitionevidence\n\nfunc caller() {\n\tTarget()\n}\n"
	dirtySource := diskSource + "// editor-only line\n"
	writeExternalChangeFile(t, mainPath, diskSource)
	mainURI := uri.FromPath(mainPath).String()

	s, tr := startWatchedExternalServer(t, root)
	openWatchedExternalDocument(t, tr, root, mainURI, dirtySource)
	waitForExternalWatchReady(t, s, mainURI)

	line, column := definitionQueryPosition(dirtySource, "Target()")
	if got := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 20); len(got) != 0 {
		t.Fatalf("definition before external create = %+v; want no result", got)
	}
	assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)

	depPath := filepath.Join(root, "dep.go")
	depURI := uri.FromPath(depPath).String()
	before := s.VFS().Revision()
	writeExternalChangeFile(t, depPath, "package transitionevidence\n\nfunc Target() {}\n")
	waitForExternalSync(t, s, before)
	created := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 21)
	if len(created) != 1 || created[0].URI != depURI {
		t.Fatalf("definition after disk create = %+v; want %s", created, depURI)
	}
	assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)

	// External modify: push Target down one line on disk. The next definition
	// must move with it instead of serving the warmed pre-modify target.
	before = s.VFS().Revision()
	writeExternalChangeFile(t, depPath, "package transitionevidence\n\n\nfunc Target() {}\n")
	waitForExternalSync(t, s, before)
	modified := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 22)
	if len(modified) != 1 || modified[0].URI != depURI || modified[0].StartLine != created[0].StartLine+1 {
		t.Fatalf("definition after disk modify = %+v; want %s with the target line advanced by one from %+v", modified, depURI, created)
	}
	assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)

	renamedPath := filepath.Join(root, "moved.go")
	renamedURI := uri.FromPath(renamedPath).String()
	before = s.VFS().Revision()
	if err := os.Rename(depPath, renamedPath); err != nil {
		t.Fatal(err)
	}
	waitForExternalSync(t, s, before)
	renamed := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 23)
	if len(renamed) != 1 || renamed[0].URI != renamedURI || renamed[0].URI == depURI {
		t.Fatalf("definition after disk rename = %+v; want %s and no stale %s", renamed, renamedURI, depURI)
	}
	assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)
	assertExternalCandidatesStayClosed(t, s, depURI, renamedURI)

	before = s.VFS().Revision()
	if err := os.Remove(renamedPath); err != nil {
		t.Fatal(err)
	}
	waitForExternalSync(t, s, before)
	deleted := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 24)
	if len(deleted) != 0 {
		t.Fatalf("definition after disk delete = %+v; want no stale exact target", deleted)
	}
	assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)
	assertExternalCandidatesStayClosed(t, s, depURI, renamedURI)
}

// writeNewExternalChangeFile creates the parent directory chain first, for
// candidate locations below the workspace root (a directory created later, a
// nested subpackage, a non-source dependency tree): writeExternalChangeFile
// itself only writes into directories that already exist.
func writeNewExternalChangeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir parent of %s: %v", path, err)
	}
	writeExternalChangeFile(t, path, content)
}

func TestExternalDependencyCandidatesAcrossLocations(t *testing.T) {
	t.Run("created directory candidate", func(t *testing.T) {
		setExternalChangeGoEnvironment(t)
		root := t.TempDir()
		writeExternalChangeFile(t, filepath.Join(root, "go.mod"), `module candidatecoverage

go 1.26.1
`)
		mainPath := filepath.Join(root, "main.go")
		// The open buffer already imports the not-yet-existing subpackage, so
		// only the external create can turn the query from unresolved into an
		// exact location inside the candidate directory.
		diskSource := "package main\n\nimport \"candidatecoverage/generated\"\n\nfunc main() {\n\tgenerated.ExtraTarget()\n}\n"
		dirtySource := diskSource + "// editor-only line\n"
		writeExternalChangeFile(t, mainPath, diskSource)
		mainURI := uri.FromPath(mainPath).String()

		s, tr := startWatchedExternalServer(t, root)
		openWatchedExternalDocument(t, tr, root, mainURI, dirtySource)
		waitForExternalWatchReady(t, s, mainURI)

		line, column := definitionQueryPosition(dirtySource, "ExtraTarget()")
		if got := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 30); len(got) != 0 {
			t.Fatalf("definition before candidate directory create = %+v; want no result", got)
		}
		assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)

		createdPath := filepath.Join(root, "generated", "extra.go")
		createdURI := uri.FromPath(createdPath).String()
		before := s.VFS().Revision()
		writeNewExternalChangeFile(t, createdPath, "package generated\n\nfunc ExtraTarget() {}\n")
		waitForExternalSync(t, s, before)
		created := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 31)
		if len(created) != 1 || created[0].URI != createdURI {
			t.Fatalf("definition after candidate directory create = %+v; want %s", created, createdURI)
		}
		assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)
		assertExternalCandidatesStayClosed(t, s, createdURI)
	})

	t.Run("nested subpackage modify rename delete", func(t *testing.T) {
		setExternalChangeGoEnvironment(t)
		root := t.TempDir()
		writeExternalChangeFile(t, filepath.Join(root, "go.mod"), `module candidatecoverage

go 1.26.1
`)
		depPath := filepath.Join(root, "nested", "dep.go")
		writeNewExternalChangeFile(t, depPath, "package nested\n\nfunc NestedTarget() {}\n")
		mainPath := filepath.Join(root, "main.go")
		diskSource := "package main\n\nimport \"candidatecoverage/nested\"\n\nfunc main() {\n\tnested.NestedTarget()\n}\n"
		dirtySource := diskSource + "// editor-only line\n"
		writeExternalChangeFile(t, mainPath, diskSource)
		mainURI := uri.FromPath(mainPath).String()
		depURI := uri.FromPath(depPath).String()

		s, tr := startWatchedExternalServer(t, root)
		openWatchedExternalDocument(t, tr, root, mainURI, dirtySource)
		waitForExternalWatchReady(t, s, mainURI)

		line, column := definitionQueryPosition(dirtySource, "NestedTarget()")
		baseline := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 40)
		if len(baseline) != 1 || baseline[0].URI != depURI {
			t.Fatalf("nested subpackage baseline = %+v; want %s", baseline, depURI)
		}
		assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)

		before := s.VFS().Revision()
		writeNewExternalChangeFile(t, depPath, "package nested\n\n\nfunc NestedTarget() {}\n")
		waitForExternalSync(t, s, before)
		modified := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 41)
		if len(modified) != 1 || modified[0].URI != depURI || modified[0].StartLine != baseline[0].StartLine+1 {
			t.Fatalf("definition after nested modify = %+v; want %s with the target line advanced by one from %+v", modified, depURI, baseline)
		}
		assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)

		movedPath := filepath.Join(root, "nested", "moved.go")
		movedURI := uri.FromPath(movedPath).String()
		before = s.VFS().Revision()
		if err := os.Rename(depPath, movedPath); err != nil {
			t.Fatal(err)
		}
		waitForExternalSync(t, s, before)
		renamed := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 42)
		if len(renamed) != 1 || renamed[0].URI != movedURI || renamed[0].URI == depURI {
			t.Fatalf("definition after nested rename = %+v; want %s and no stale %s", renamed, movedURI, depURI)
		}
		assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)
		assertExternalCandidatesStayClosed(t, s, depURI, movedURI)

		before = s.VFS().Revision()
		if err := os.Remove(movedPath); err != nil {
			t.Fatal(err)
		}
		waitForExternalSync(t, s, before)
		deleted := dispatchDefinitionLocationsForExternalChange(t, s, mainURI, line, column, 43)
		if len(deleted) != 0 {
			t.Fatalf("definition after nested delete = %+v; want no stale exact target", deleted)
		}
		assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)
		assertExternalCandidatesStayClosed(t, s, depURI, movedURI)
	})

	t.Run("non-source dependency candidate", func(t *testing.T) {
		setExternalChangeGoEnvironment(t)
		root := t.TempDir()
		writeExternalChangeFile(t, filepath.Join(root, "go.mod"), `module candidatecoverage

go 1.26.1
`)
		mainPath := filepath.Join(root, "main.go")
		diskSource := "package main\n\nfunc main() {\n}\n"
		dirtySource := diskSource + "// editor-only line\n"
		writeExternalChangeFile(t, mainPath, diskSource)
		mainURI := uri.FromPath(mainPath).String()

		s, tr := startWatchedExternalServer(t, root)
		openWatchedExternalDocument(t, tr, root, mainURI, dirtySource)
		waitForExternalWatchReady(t, s, mainURI)

		// Dependency trees outside any Go package (here a JavaScript bundle)
		// are still workspace candidates: external edits must invalidate the
		// published revision even though no Go symbol can move.
		scriptPath := filepath.Join(root, "node_modules", "pkg", "dep.js")
		before := s.VFS().Revision()
		writeNewExternalChangeFile(t, scriptPath, "export const before = 1;\n")
		waitForExternalSync(t, s, before)
		assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)

		before = s.VFS().Revision()
		if err := os.Remove(scriptPath); err != nil {
			t.Fatal(err)
		}
		waitForExternalSync(t, s, before)
		assertExternalTransitionBufferIntact(t, s, mainURI, dirtySource)
		assertExternalCandidatesStayClosed(t, s, uri.FromPath(scriptPath).String())
	})
}

// startWatchedExternalServer runs a server with the polling watcher enabled
// against a Go backend rooted at root, and stops Run on cleanup.
func startWatchedExternalServer(t *testing.T, root string) (*Server, *queuedWatchTransport) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.WatchInterval = 5 * time.Millisecond
	s := New(cfg)
	s.RegisterBackend("go", golang.New(root))
	tr := &queuedWatchTransport{fakeTransport: newFakeTransport(), in: make(chan *jsonrpc.Message, 8)}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx, tr) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop")
		}
	})
	return s, tr
}

// openWatchedExternalDocument performs the initialize handshake and opens
// mainURI with the given dirty editor content (editor version 11).
func openWatchedExternalDocument(t *testing.T, tr *queuedWatchTransport, root, mainURI, dirtySource string) {
	t.Helper()
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
}

// assertExternalTransitionBufferIntact requires the open document to keep its
// editor version, dirty flag and exact content after every external step.
func assertExternalTransitionBufferIntact(t *testing.T, s *Server, mainURI, dirtySource string) {
	t.Helper()
	file := s.VFS().Get(mainURI)
	if file == nil || file.Version != 11 || !file.Dirty || string(file.Content) != dirtySource {
		t.Fatalf("external transition disturbed the open editor buffer: %+v", file)
	}
}

// assertExternalCandidatesStayClosed requires every dependency candidate to
// stay outside the VFS: the watcher invalidates, it never opens buffers.
func assertExternalCandidatesStayClosed(t *testing.T, s *Server, uris ...string) {
	t.Helper()
	for _, candidate := range uris {
		if file := s.VFS().Get(candidate); file != nil {
			t.Fatalf("external file operation opened a closed dependency in the VFS: %s", candidate)
		}
	}
}
