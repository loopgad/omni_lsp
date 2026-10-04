package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

func indexRequest(s *Server, ctx context.Context, method string) *jsonrpc.Message {
	return s.dispatcher.Dispatch(ctx, jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, method, nil))
}

type indexMutationContext struct {
	context.Context
	trigger     func() bool
	mutate      func() error
	mutationErr error
	mutated     bool
}

func (c *indexMutationContext) Err() error {
	if !c.mutated && c.mutate != nil && c.trigger != nil && c.trigger() {
		c.mutated = true
		c.mutationErr = c.mutate()
	}
	return c.Context.Err()
}

func stagedBuildHasSegment(indexDir string) bool {
	staging, err := filepath.Glob(filepath.Join(indexDir, "staging-*"))
	if err != nil {
		return false
	}
	for _, dir := range staging {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.Type().IsRegular() {
				return true
			}
		}
	}
	return false
}

func TestFirstInventoryChange(t *testing.T) {
	base := []fileRecord{{Path: "a.go", Size: 1, SHA256: "a"}, {Path: "c.go", Size: 1, SHA256: "c"}}
	for _, tc := range []struct {
		name  string
		after []fileRecord
		want  string
	}{
		{"added", []fileRecord{{Path: "a.go", Size: 1, SHA256: "a"}, {Path: "b.go", Size: 1, SHA256: "b"}, {Path: "c.go", Size: 1, SHA256: "c"}}, "added b.go"},
		{"removed", []fileRecord{{Path: "c.go", Size: 1, SHA256: "c"}}, "removed a.go"},
		{"changed", []fileRecord{{Path: "a.go", Size: 1, SHA256: "different"}, {Path: "c.go", Size: 1, SHA256: "c"}}, "changed a.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstInventoryChange(base, tc.after); got != tc.want {
				t.Fatalf("firstInventoryChange = %q, want %q", got, tc.want)
			}
		})
	}
}

func indexStats(t *testing.T, s *Server) IndexStats {
	t.Helper()
	resp := indexRequest(s, context.Background(), "omnilsp/indexStats")
	if resp == nil || resp.Error != nil {
		t.Fatalf("indexStats: %+v", resp)
	}
	var stats IndexStats
	if err := json.Unmarshal(resp.Result, &stats); err != nil {
		t.Fatal(err)
	}
	return stats
}

func initializedIndexServer(t *testing.T, root string) *Server {
	t.Helper()
	cfg := DefaultConfig()
	cfg.IndexDir = filepath.Join(t.TempDir(), "index")
	s := New(cfg)
	params, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
	if err != nil {
		t.Fatal(err)
	}
	resp := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", params))
	if resp == nil || resp.Error != nil {
		t.Fatalf("initialize: %+v", resp)
	}
	return s
}

func TestC12_IndexStatsDisabled(t *testing.T) {
	withoutRoot := New(DefaultConfig())
	initResp := withoutRoot.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", json.RawMessage(`{}`)))
	if initResp == nil || initResp.Error != nil {
		t.Fatalf("initialize without root: %+v", initResp)
	}
	stats := indexStats(t, withoutRoot)
	if stats.Enabled || stats.Reason == "" {
		t.Fatalf("disabled stats = %+v", stats)
	}
	root := t.TempDir()
	blocked := filepath.Join(t.TempDir(), "index-file")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.IndexDir = blocked
	s := New(cfg)
	params, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
	if err != nil {
		t.Fatal(err)
	}
	resp := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", params))
	if resp == nil || resp.Error != nil {
		t.Fatalf("degraded initialize: %+v", resp)
	}
	if got := indexStats(t, s); got.Enabled || got.Reason == "" {
		t.Fatalf("degraded stats = %+v", got)
	}
}

func TestC12_IndexStatsAndReindexLifecycle(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	rootWithIndex := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootWithIndex, "source.go"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	insideCfg := DefaultConfig()
	insideCfg.IndexDir = filepath.Join(rootWithIndex, "index")
	inside := New(insideCfg)
	insideParams, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(rootWithIndex).String()})
	if err != nil {
		t.Fatal(err)
	}
	insideInit := inside.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", insideParams))
	if insideInit == nil || insideInit.Error != nil {
		t.Fatalf("initialize with internal index: %+v", insideInit)
	}
	if resp := indexRequest(inside, context.Background(), "omnilsp/reindex"); resp == nil || resp.Error != nil {
		t.Fatalf("reindex with internal index: %+v", resp)
	}
	if got := indexStats(t, inside).LastRebuild.Files; got != 1 {
		t.Fatalf("index counted its own files: %d", got)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "名字.go"), []byte("package 名字\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "skip.js"), []byte("skip"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := initializedIndexServer(t, root)
	for generation := uint64(1); generation <= 2; generation++ {
		resp := indexRequest(s, context.Background(), "omnilsp/reindex")
		if resp == nil || resp.Error != nil {
			t.Fatalf("reindex %d: %+v", generation, resp)
		}
		stats := indexStats(t, s)
		if !stats.Enabled || !stats.Fresh || stats.Generation != generation || stats.LastRebuild.Files != 1 || len(stats.Segments) != 1 {
			t.Fatalf("generation %d stats = %+v", generation, stats)
		}
		view, err := s.idx.store.OpenSnapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := view.ReadSegment(view.Segments[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		recordsExpected, err := s.idx.inventory(context.Background(), &IndexRebuildStats{})
		if err != nil {
			t.Fatal(err)
		}
		_, inventoryHash, err := inventoryPayload(recordsExpected)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := persistent.VerifyPayload(sealed, persistent.FreshnessTuple{
			SourceHash: inventorySourceHash(s.idx.workspaceID, inventoryHash), BackendVer: inventoryVersion,
		})
		if err != nil {
			t.Fatal(err)
		}
		var records []fileRecord
		if err := json.Unmarshal(payload, &records); err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 || records[0].Path != "名字.go" {
			t.Fatalf("records = %+v", records)
		}
	}
	s.snapMgr.Publish(snapshot.New(string(s.workspaceID), 1, nil))
	if stats := indexStats(t, s); !stats.Fresh || stats.Generation != 2 {
		t.Fatalf("snapshot revision alone must not stale the disk inventory: %+v", stats)
	}
}

func TestC12_IndexStatsFreshnessSurvivesServerRestart(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	root := t.TempDir()
	indexDir := filepath.Join(t.TempDir(), "persistent-index")
	sourcePath := filepath.Join(root, "source.go")
	if err := os.WriteFile(sourcePath, []byte("package original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newServer := func() *Server {
		cfg := DefaultConfig()
		cfg.IndexDir = indexDir
		s := New(cfg)
		params, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
		if err != nil {
			t.Fatal(err)
		}
		resp := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", params))
		if resp == nil || resp.Error != nil {
			t.Fatalf("initialize: %+v", resp)
		}
		return s
	}
	first := newServer()
	first.snapMgr.Publish(snapshot.New(string(first.workspaceID), 7, nil))
	if resp := indexRequest(first, context.Background(), "omnilsp/reindex"); resp == nil || resp.Error != nil {
		t.Fatalf("initial reindex: %+v", resp)
	}
	if stats := indexStats(t, first); !stats.Fresh || stats.Generation != 1 || stats.Revision != 7 {
		t.Fatalf("initial index stats = %+v", stats)
	}
	second := newServer()
	second.snapMgr.Publish(snapshot.New(string(second.workspaceID), 2, nil))
	if stats := indexStats(t, second); !stats.Enabled || !stats.Fresh || stats.Generation != 1 || stats.Revision != 2 {
		t.Fatalf("unchanged tree must stay fresh after restart with a different snapshot revision: %+v", stats)
	}
	if err := os.WriteFile(sourcePath, []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stats := indexStats(t, second); !stats.Enabled || stats.Fresh || stats.Generation != 1 {
		t.Fatalf("recovered stale index stats = %+v", stats)
	}
	if resp := indexRequest(second, context.Background(), "omnilsp/reindex"); resp == nil || resp.Error != nil {
		t.Fatalf("rebuild after restart: %+v", resp)
	}
	if stats := indexStats(t, second); !stats.Fresh || stats.Generation != 2 {
		t.Fatalf("rebuilt index stats = %+v", stats)
	}
}

func TestC12_IndexStatsTracksDiskInventoryChanges(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.go")
	if err := os.WriteFile(sourcePath, []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := initializedIndexServer(t, root)
	assertFresh := func(generation uint64) {
		t.Helper()
		if resp := indexRequest(s, context.Background(), "omnilsp/reindex"); resp == nil || resp.Error != nil {
			t.Fatalf("reindex generation %d: %+v", generation, resp)
		}
		if stats := indexStats(t, s); !stats.Fresh || stats.Generation != generation {
			t.Fatalf("fresh generation %d stats = %+v", generation, stats)
		}
	}
	assertStale := func(generation uint64, change string) {
		t.Helper()
		if stats := indexStats(t, s); stats.Fresh || stats.Generation != generation {
			t.Fatalf("%s must stale generation %d without replacing it: %+v", change, generation, stats)
		}
	}
	assertFresh(1)
	if err := os.WriteFile(sourcePath, []byte("package edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertStale(1, "disk edit")
	assertFresh(2)

	addedPath := filepath.Join(root, "added.go")
	if err := os.WriteFile(addedPath, []byte("package added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertStale(2, "disk add")
	assertFresh(3)

	if err := os.Remove(addedPath); err != nil {
		t.Fatal(err)
	}
	assertStale(3, "disk delete")
}

func TestC12_IndexFreshnessBindsWorkspaceIdentity(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	indexDir := filepath.Join(t.TempDir(), "shared-index")
	newServer := func(root string) *Server {
		t.Helper()
		cfg := DefaultConfig()
		cfg.IndexDir = indexDir
		s := New(cfg)
		params, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
		if err != nil {
			t.Fatal(err)
		}
		resp := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", params))
		if resp == nil || resp.Error != nil {
			t.Fatalf("initialize %s: %+v", root, resp)
		}
		return s
	}
	rootA, rootB := t.TempDir(), t.TempDir()
	for _, root := range []string{rootA, rootB} {
		if err := os.WriteFile(filepath.Join(root, "same.go"), []byte("package same\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	first := newServer(rootA)
	if resp := indexRequest(first, context.Background(), "omnilsp/reindex"); resp == nil || resp.Error != nil {
		t.Fatalf("first workspace reindex: %+v", resp)
	}
	second := newServer(rootB)
	if stats := indexStats(t, second); stats.Fresh || stats.Generation != 1 {
		t.Fatalf("identical file inventory from another workspace must be stale: %+v", stats)
	}
}

func TestC12_IndexStatsRebuildsLegacyInventory(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := initializedIndexServer(t, root)
	records, err := s.idx.inventory(context.Background(), &IndexRebuildStats{})
	if err != nil {
		t.Fatal(err)
	}
	data, oldSourceHash, err := inventoryPayload(records)
	if err != nil {
		t.Fatal(err)
	}
	build, err := s.idx.store.BeginBuild(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	if _, err := build.WriteSegment(persistent.SealPayload(data, persistent.FreshnessTuple{
		SourceHash: oldSourceHash, BackendVer: "file-inventory-v1",
	})); err != nil {
		t.Fatal(err)
	}
	if err := build.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	committed = true
	if stats := indexStats(t, s); stats.Fresh || stats.Generation != 1 {
		t.Fatalf("legacy inventory must be retained but stale: %+v", stats)
	}
	if resp := indexRequest(s, context.Background(), "omnilsp/reindex"); resp == nil || resp.Error != nil {
		t.Fatalf("rebuild legacy inventory: %+v", resp)
	}
	if stats := indexStats(t, s); !stats.Fresh || stats.Generation != 2 {
		t.Fatalf("rebuilt inventory = %+v", stats)
	}
}

func TestC12_ReindexMutationAbortsWithoutReplacingGeneration(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	for _, tc := range []struct {
		name      string
		mutate    func(*Server, string) func() error
		want      string
		wantFresh bool
	}{
		{
			name: "snapshot revision",
			mutate: func(s *Server, _ string) func() error {
				return func() error {
					s.snapMgr.Publish(snapshot.New(string(s.workspaceID), 1, nil))
					return nil
				}
			},
			want:      "workspace changed during reindex",
			wantFresh: true,
		},
		{
			name: "disk content",
			mutate: func(_ *Server, sourcePath string) func() error {
				return func() error {
					return os.WriteFile(sourcePath, []byte("package altered\n"), 0o644)
				}
			},
			want:      "workspace files changed during reindex: changed source.go",
			wantFresh: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			sourcePath := filepath.Join(root, "source.go")
			if err := os.WriteFile(sourcePath, []byte("package original\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			s := initializedIndexServer(t, root)
			if resp := indexRequest(s, context.Background(), "omnilsp/reindex"); resp == nil || resp.Error != nil {
				t.Fatalf("initial reindex: %+v", resp)
			}
			before := indexStats(t, s)
			if !before.Fresh || before.Generation != 1 {
				t.Fatalf("initial generation = %+v", before)
			}
			ctx := &indexMutationContext{
				Context: context.Background(),
				trigger: func() bool { return stagedBuildHasSegment(s.idx.dir) },
				mutate:  tc.mutate(s, sourcePath),
			}
			if _, err := s.handleReindex(ctx, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reindex error = %v, want substring %q", err, tc.want)
			}
			if ctx.mutationErr != nil {
				t.Fatalf("injected mutation failed: %v", ctx.mutationErr)
			}
			after := indexStats(t, s)
			if after.Generation != before.Generation || len(after.Segments) != len(before.Segments) {
				t.Fatalf("failed reindex replaced last generation: before=%+v after=%+v", before, after)
			}
			if after.Fresh != tc.wantFresh {
				t.Fatalf("freshness after %s mutation = %t, want %t: %+v", tc.name, after.Fresh, tc.wantFresh, after)
			}
		})
	}
}

func TestC12_CorruptHistoryDoesNotBlockInitialize(t *testing.T) {
	root := t.TempDir()
	indexDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(indexDir, "history.json"), []byte(`[null]`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.IndexDir = indexDir
	s := New(cfg)
	params, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
	if err != nil {
		t.Fatal(err)
	}
	resp := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", params))
	if resp == nil || resp.Error != nil {
		t.Fatalf("initialize with malformed history: %+v", resp)
	}
	stats := indexStats(t, s)
	if !stats.Enabled || stats.Generation != 0 {
		t.Fatalf("degraded index status = %+v", stats)
	}
}

func TestC12_SharedIndexDirSerializesServers(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	root := t.TempDir()
	indexDir := filepath.Join(t.TempDir(), "shared-index")
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newServer := func() *Server {
		cfg := DefaultConfig()
		cfg.IndexDir = indexDir
		s := New(cfg)
		params, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
		if err != nil {
			t.Fatal(err)
		}
		resp := s.dispatcher.Dispatch(context.Background(), jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", params))
		if resp == nil || resp.Error != nil {
			t.Fatalf("initialize shared-index server: %+v", resp)
		}
		return s
	}
	servers := []*Server{newServer(), newServer()}
	start := make(chan struct{})
	results := make(chan *jsonrpc.Message, len(servers))
	for _, srv := range servers {
		go func(srv *Server) {
			<-start
			results <- indexRequest(srv, context.Background(), "omnilsp/reindex")
		}(srv)
	}
	close(start)
	seen := map[uint64]bool{}
	for range len(servers) {
		resp := <-results
		if resp == nil || resp.Error != nil {
			t.Fatalf("shared-index reindex: %+v", resp)
		}
		var result struct {
			Generation uint64 `json:"generation"`
		}
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			t.Fatal(err)
		}
		seen[result.Generation] = true
	}
	if !seen[1] || !seen[2] || len(seen) != 2 {
		t.Fatalf("published generations = %+v, want distinct 1 and 2", seen)
	}
	for i, srv := range servers {
		if stats := indexStats(t, srv); !stats.Enabled || !stats.Fresh || stats.Generation != 2 {
			t.Fatalf("server %d shared index stats = %+v", i, stats)
		}
	}
}

func TestC12_ReindexTrustGate(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "untrusted")
	s := initializedIndexServer(t, t.TempDir())
	resp := indexRequest(s, context.Background(), "omnilsp/reindex")
	if resp == nil || resp.Error == nil || resp.Error.Code != jsonrpc.RequestFailed || !strings.Contains(resp.Error.Message, "untrusted") {
		t.Fatalf("reindex gate = %+v", resp)
	}
	if string(resp.Error.Data) != `{"kind":"untrusted_operation"}` {
		t.Fatalf("untrusted error data = %s", resp.Error.Data)
	}
	if stats := indexStats(t, s); !stats.Enabled {
		t.Fatalf("indexStats gated: %+v", stats)
	}
}

type cancelAfterContext struct {
	context.Context
	cancelWhen func() bool
	onCancel   func()
	canceled   atomic.Bool
}

func (c *cancelAfterContext) Err() error {
	if !c.canceled.Load() && c.cancelWhen != nil && c.cancelWhen() {
		if c.canceled.CompareAndSwap(false, true) && c.onCancel != nil {
			c.onCancel()
		}
	}
	if c.canceled.Load() {
		return context.Canceled
	}
	return nil
}

func TestC12_ReindexCancel(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.go"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := initializedIndexServer(t, root)
	if resp := indexRequest(s, context.Background(), "omnilsp/reindex"); resp.Error != nil {
		t.Fatal(resp.Error)
	}
	before, err := s.idx.store.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sawStagedSegment := false
	ctx := &cancelAfterContext{Context: context.Background(), cancelWhen: func() bool { return stagedBuildHasSegment(s.idx.dir) }}
	ctx.onCancel = func() {
		staging, err := filepath.Glob(filepath.Join(s.idx.dir, "staging-*"))
		if err != nil || len(staging) != 1 {
			return
		}
		entries, err := os.ReadDir(staging[0])
		sawStagedSegment = err == nil && len(entries) == 1 && !entries[0].IsDir()
	}
	resp := indexRequest(s, ctx, "omnilsp/reindex")
	if resp == nil || resp.Error == nil || resp.Error.Code != jsonrpc.RequestCancelled {
		t.Fatalf("cancel response = %+v", resp)
	}
	if !sawStagedSegment {
		t.Fatal("cancellation was not injected after a staged segment had been written")
	}
	after, err := s.idx.store.OpenSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID {
		t.Fatalf("cancel published generation %d over %d", after.ID, before.ID)
	}
	if _, err := after.ReadSegment(after.Segments[0].ID); err != nil {
		t.Fatal(err)
	}
	if staging, err := filepath.Glob(filepath.Join(s.idx.dir, "staging-*")); err != nil || len(staging) != 0 {
		t.Fatalf("cancel left staging directories: %v (%v)", staging, err)
	}
	if _, err := os.Stat(filepath.Join(s.idx.dir, "manifest.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel left a temporary manifest: %v", err)
	}
	lockCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	build, err := s.idx.store.BeginBuild(lockCtx)
	if err != nil {
		t.Fatalf("cancel left the persistent writer lock held: %v", err)
	}
	if err := build.Abort(); err != nil {
		t.Fatalf("abort lock-release probe: %v", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("cancellation was not exercised")
	}
}
