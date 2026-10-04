//go:build soak

// In-process soak coverage for goal.md §S16/Y1. The local release pipeline
// separately runs a continuous one-hour real-stdio workload; this build-tagged
// suite keeps a short mixed-load regression available to regular test runs,
// with hard bounds on goroutines and heap.
//
// Run explicitly:
//
//	go test -tags soak -timeout 600s ./test/soak/
package soak

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages/golang"
	"github.com/omnilsp/omni/internal/runtime/server"
	"github.com/omnilsp/omni/internal/workspace/uri"
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
	dir := t.TempDir()
	b := golang.New(dir)
	defer b.Close()
	srv.RegisterBackend(b.LanguageID(), b)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module soak\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "soak.go")
	content := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	docURI := uri.FromPath(file).String()

	ctx, cancel := context.WithTimeout(context.Background(), soakDuration())
	defer cancel()

	// Facade mutating methods are lifecycle-gated (C2): initialize first.
	for _, call := range []struct {
		method string
		params any
	}{
		{"initialize", map[string]any{"processId": 1, "rootUri": uri.FromPath(dir).String()}},
		{"initialized", map[string]any{}},
		{"textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri": docURI, "languageId": "go", "version": 1, "text": content,
		}}},
	} {
		if _, err := srv.CallMethod(ctx, call.method, call.params); err != nil {
			t.Fatalf("%s: %v", call.method, err)
		}
	}

	runtime.GC()
	baseGo := runtime.NumGoroutine()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	done := make(chan error, 1)
	go func() {
		completed := 0
		for i := 0; ctx.Err() == nil; i++ {
			// Mixed workload: edit stream + queries + periodic churn.
			content = fmt.Sprintf("package main\n\nfunc main() {}\n// tick %d\n", i)
			if _, err := srv.CallMethod(ctx, "textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": docURI, "version": i + 2},
				"contentChanges": []map[string]any{{"text": content}},
			}); err != nil {
				if ctx.Err() != nil {
					break
				}
				done <- fmt.Errorf("didChange iteration %d: %w", i, err)
				return
			}
			res, err := srv.CallMethod(ctx, "textDocument/hover", map[string]any{
				"textDocument": map[string]any{"uri": docURI},
				"position":     map[string]int{"line": 2, "character": 6},
			})
			if err != nil || len(res) == 0 || string(res) == "null" {
				if ctx.Err() != nil {
					break
				}
				done <- fmt.Errorf("hover iteration %d: result=%s err=%v", i, res, err)
				return
			}
			completed++
			if i%50 == 49 {
				time.Sleep(10 * time.Millisecond) // let supervision/caches breathe
			}
		}
		if completed == 0 {
			done <- fmt.Errorf("soak completed no edit/query cycles")
			return
		}
		done <- nil
	}()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

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
