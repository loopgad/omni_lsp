//go:build soak

// Accelerated soak entry (goal.md §S16/Y1). The spec's release bar is a 24h
// representative workload — that belongs to the release pipeline, not to
// every `go test ./...` run. This build-tagged suite is the always-available
// short form: sustained mixed load with hard bounds on goroutines and heap,
// so monotonic leaks surface in minutes instead of days.
//
// Run explicitly:
//
//	go test -tags soak -timeout 600s ./test/soak/
package soak

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/runtime/server"
)

// soakDuration honors SOAK_DURATION (time.Duration syntax, e.g. "10s"/"1m")
// for quick local runs; the default stays at the commit-gate 30s.
func soakDuration() time.Duration {
	dur := 30 * time.Second
	if v := os.Getenv("SOAK_DURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return dur
}

func TestSoak_SustainedMixedLoadBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	srvCfg := server.DefaultConfig()
	srv := server.New(srvCfg)
	b := golang.New(t.TempDir())
	srv.RegisterBackend(b.LanguageID(), b)

	ctx, cancel := context.WithTimeout(context.Background(), soakDuration())
	defer cancel()

	// Facade mutating methods are lifecycle-gated (C2): initialize first.
	_, _ = srv.CallMethod(ctx, "initialize", map[string]any{"processId": 1, "rootUri": "file:///w"})
	_, _ = srv.CallMethod(ctx, "initialized", map[string]any{})

	runtime.GC()
	baseGo := runtime.NumGoroutine()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	done := make(chan struct{})
	go func() {
		defer close(done)
		uri := "file://" + t.TempDir() + "/soak.go"
		content := []byte("package main\n\nfunc main() {}\n")
		for i := 0; ctx.Err() == nil; i++ {
			// Mixed workload: edit stream + queries + periodic churn.
			content = append(content, []byte("// tick\n")...)
			_, _ = srv.CallMethod(ctx, "textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": uri, "version": i + 2},
				"contentChanges": []map[string]any{{"text": string(content)}},
			})
			_, _ = srv.CallMethod(ctx, "textDocument/hover", map[string]any{
				"textDocument": map[string]any{"uri": uri},
				"position":     map[string]int{"line": 2, "character": 6},
			})
			if i%50 == 49 {
				time.Sleep(10 * time.Millisecond) // let supervision/caches breathe
			}
		}
	}()
	<-done

	deadline := time.Now().Add(10 * time.Second)
	for {
		runtime.GC()
		goNow := runtime.NumGoroutine()
		var now runtime.MemStats
		runtime.ReadMemStats(&now)
		grow := int64(goNow) - int64(baseGo)
		heapGrow := int64(now.HeapAlloc) - int64(base.HeapAlloc)
		if grow <= 50 && heapGrow < 256<<20 {
			break // within bounds: no monotonic goroutine/heap leak (Y1)
		}
		if time.Now().After(deadline) {
			t.Fatalf("resource bounds exceeded after soak: goroutines %d->%d, heap %d->%d",
				baseGo, goNow, base.HeapAlloc, now.HeapAlloc)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
