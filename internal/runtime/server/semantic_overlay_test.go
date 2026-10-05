package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	golang "github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

type overlayTestSink struct {
	symbols     []model.Symbol
	occurrences []model.Occurrence
	edges       []model.Edge
}

func (s *overlayTestSink) WriteSymbols(_ context.Context, values []model.Symbol) error {
	s.symbols = append(s.symbols, values...)
	return nil
}
func (s *overlayTestSink) WriteOccurrences(_ context.Context, values []model.Occurrence) error {
	s.occurrences = append(s.occurrences, values...)
	return nil
}
func (s *overlayTestSink) WriteEdges(_ context.Context, values []model.Edge) error {
	s.edges = append(s.edges, values...)
	return nil
}

func TestGoSnapshotSemanticOverlayExportsCurrentFactsAndRejectsStaleSnapshot(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOENV", "off")
	t.Setenv("GOTOOLCHAIN", "local")

	root := t.TempDir()
	mod := []byte("module overlay.test\n\ngo 1.22\n")
	diskSource := []byte("package overlaytest\n\nfunc target() {}\n\nfunc use() { target() }\n")
	openSource := []byte("// unsaved line\npackage overlaytest\n\nfunc renamed() {}\n\nfunc use() { renamed() }\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), mod, 0o600); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(root, "main.go")
	if err := os.WriteFile(filePath, diskSource, 0o600); err != nil {
		t.Fatal(err)
	}
	rootURI := uri.FromPath(root).Canonical()
	fileURI := uri.FromPath(filePath).Canonical()
	s := New(DefaultConfig())
	s.workspaceID = identity.WorkspaceID(rootURI)
	s.vfs.Open(fileURI, "go", 2, openSource, vfs.SourceEditor)
	revision := s.vfs.Revision()
	captured := snapshot.New(string(s.workspaceID), revision, map[string]snapshot.DocumentSnapshot{
		fileURI: {URI: fileURI, LanguageID: "go", Version: 2, Content: openSource},
	})
	s.snapMgr.Publish(captured)
	ctx := withSnapshot(context.Background(), captured)

	diskView, err := captureSemanticView(ctx, root, s.workspaceID, revision, "")
	if err != nil {
		t.Fatalf("capture disk view: %v", err)
	}
	privateSnapshotPath := diskView.snapshot
	view, err := captureGoSnapshotSemanticView(s, ctx, diskView)
	if err != nil {
		_ = diskView.Close()
		t.Fatalf("capture Go request overlay: %v", err)
	}
	if !view.HasLanguageChanges("go") || !view.StillCurrent(s, ctx) {
		t.Fatal("captured overlay did not identify current Go changes")
	}

	baseBackend := golang.New(root)
	baseRequest, err := baseBackend.BuildIndexRequest(ctx, diskView, rootURI)
	if err != nil {
		t.Fatalf("plan disk Go scopes: %v", err)
	}
	baseSink := &overlayTestSink{}
	baseReport, err := baseBackend.ExportIndex(ctx, baseRequest, baseSink)
	if err != nil {
		t.Fatalf("export disk Go facts: %v", err)
	}
	if err := model.ValidateReport(baseRequest, baseReport); err != nil {
		t.Fatalf("validate disk Go report: %v", err)
	}

	overlayBackend := golang.New(root)
	overlaySink := &overlayTestSink{}
	request, report, err := exportGoSnapshotOverlay(ctx, s, view, overlayBackend, overlayBackend, overlaySink, "go")
	if err != nil {
		t.Fatalf("export snapshot Go overlay: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("validate overlay report: %v", err)
	}
	if len(request.Scopes) != 1 {
		t.Fatalf("overlay scopes = %d, want one", len(request.Scopes))
	}
	baseID := overlaySymbolID(baseSink.symbols, "target")
	newID := overlaySymbolID(overlaySink.symbols, "renamed")
	if baseID == "" || newID == "" {
		t.Fatalf("disk/overlay symbol IDs missing: old=%q new=%q", baseID, newID)
	}
	if got := overlayOccurrenceLine(baseSink.occurrences, baseID, "definition"); got != 2 {
		t.Fatalf("disk definition line = %d, want 2", got)
	}
	if got := overlayOccurrenceLine(baseSink.occurrences, baseID, "reference"); got != 4 {
		t.Fatalf("disk reference line = %d, want 4", got)
	}
	if got := overlayOccurrenceLine(overlaySink.occurrences, newID, "definition"); got != 3 {
		t.Fatalf("overlay definition line = %d, want shifted line 3", got)
	}
	if got := overlayOccurrenceLine(overlaySink.occurrences, newID, "reference"); got != 5 {
		t.Fatalf("overlay reference line = %d, want shifted line 5", got)
	}
	if overlaySymbolID(overlaySink.symbols, "target") != "" {
		t.Fatal("overlay still contains the renamed-away disk symbol")
	}
	if err := view.Close(); err != nil {
		t.Fatalf("close overlay and private snapshot: %v", err)
	}
	if _, err := os.Stat(privateSnapshotPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private snapshot remains after close, stat error = %v", err)
	}
	if got, err := os.ReadFile(filePath); err != nil || string(got) != string(diskSource) {
		t.Fatalf("overlay modified workspace source: content=%q err=%v", got, err)
	}

	s.vfs.Update(fileURI, 3, []byte("// newer\npackage overlaytest\n"))
	nextRevision := s.vfs.Revision()
	nextSnapshot := snapshot.New(string(s.workspaceID), nextRevision, map[string]snapshot.DocumentSnapshot{
		fileURI: {URI: fileURI, LanguageID: "go", Version: 3, Content: []byte("// newer\npackage overlaytest\n")},
	})
	s.snapMgr.Publish(nextSnapshot)
	if view.StillCurrent(s, ctx) {
		t.Fatal("overlay remained current after the request snapshot advanced")
	}
	if _, _, err := exportGoSnapshotOverlay(ctx, s, view, overlayBackend, overlayBackend, &overlayTestSink{}, "go"); !errors.Is(err, errGoSemanticOverlayStale) {
		t.Fatalf("export after snapshot advance error = %v, want stale overlay", err)
	}
}

func overlaySymbolID(symbols []model.Symbol, name string) identity.SymbolID {
	for _, symbol := range symbols {
		if symbol.Name == name {
			return symbol.ID
		}
	}
	return ""
}

func overlayOccurrenceLine(occurrences []model.Occurrence, symbolID identity.SymbolID, role string) uint32 {
	for _, occurrence := range occurrences {
		if occurrence.SymbolID == symbolID && occurrence.Role == role {
			return occurrence.Range.StartLine
		}
	}
	return ^uint32(0)
}

func TestGoOverlayPositionLookupDoesNotSortBySymbolName(t *testing.T) {
	fileURI := "file:///repo/main.go"
	view := &goSnapshotSemanticView{identity: model.Identity{Workspace: "test", DiskDigest: "sha256:disk", SnapshotRev: 1}}
	request := model.Request{View: view, Scopes: []model.Scope{{ID: "go", Language: "go", BuildContext: "context"}}}
	sink := &boundedGoOverlayFactsSink{
		symbols: []model.Symbol{{ID: "z", ScopeID: "go", Name: "early"}, {ID: "a", ScopeID: "go", Name: "late"}},
		occurrences: []model.Occurrence{
			{SymbolID: "z", ScopeID: "go", URI: fileURI, Role: "reference", SourceHash: "sha256:source", BuildContext: "context", Range: model.Position{StartLine: 0, StartChar: 1, EndLine: 0, EndChar: 2}},
			{SymbolID: "a", ScopeID: "go", URI: fileURI, Role: "reference", SourceHash: "sha256:source", BuildContext: "context", Range: model.Position{StartLine: 8, StartChar: 1, EndLine: 8, EndChar: 2}},
		},
	}
	facts, err := buildGoSnapshotSemanticFacts(view, request, model.Report{}, sink, "go")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := facts.symbolAt(fileURI, 8, 1); !ok || got != "a" {
		t.Fatalf("position lookup after a lexically later symbol: got %q, found %v", got, ok)
	}
}
