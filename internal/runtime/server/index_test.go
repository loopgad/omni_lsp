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

	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

func indexRequest(s *Server, ctx context.Context, method string) *jsonrpc.Message {
	return s.dispatcher.Dispatch(ctx, jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, method, nil))
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
		payload, err := persistent.VerifyPayload(sealed, persistent.FreshnessTuple{BackendVer: inventoryVersion, Revision: stats.Revision})
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
	if stats := indexStats(t, s); stats.Fresh || stats.Generation != 2 {
		t.Fatalf("stale generation must remain visible but not fresh: %+v", stats)
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
	checks atomic.Int32
	limit  int32
}

func (c *cancelAfterContext) Err() error {
	if c.checks.Add(1) >= c.limit {
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
	ctx := &cancelAfterContext{Context: context.Background(), limit: 4}
	resp := indexRequest(s, ctx, "omnilsp/reindex")
	if resp == nil || resp.Error == nil || resp.Error.Code != jsonrpc.RequestFailed {
		t.Fatalf("cancel response = %+v", resp)
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
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("cancellation was not exercised")
	}
}
