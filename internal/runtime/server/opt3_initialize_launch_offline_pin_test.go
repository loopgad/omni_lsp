package server

// Pin for round 23 OPT-3: plugin launch must not run on the initialize
// critical path. launchReadyPlugins verifies every plugin entrypoint with a
// whole-file SHA-256 (budgeted 30s each) before spawning; that work used to
// run synchronously inside the mutationMu-critical dispatchInline(initialize),
// delaying the initialize response by the whole launch chain. It now runs in
// a background goroutine with the ready-gate semantics unchanged.
//
// The pin gives initialize a payload that CANNOT pass if the launch chain is
// synchronous again: eight plugins with 32MB entrypoints cost ~200ms+ of pure
// SHA-256 on the launch path, while the backgrounded initialize answers in
// single-digit milliseconds. The bound must stay far above any plausible
// non-launch initialize cost and far below the blob-hashing cost of a sync
// regression. After the response, the pin still requires the launch phase to
// run to completion (no lost launches).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/plugin"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

func TestInitializeRespondsBeforePluginLaunchCompletes(t *testing.T) {
	const plugins = 8
	const blobMB = 32
	root := t.TempDir()
	pluginsDir := filepath.Join(root, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, plugins)
	for i := 0; i < plugins; i++ {
		id := fmt.Sprintf("opt3-pin-%d", i)
		ids = append(ids, id)
		writeBlobPluginDir(t, pluginsDir, id, blobMB)
	}

	s := New(Config{PluginsDir: pluginsDir, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()

	// Mounted but not launched: pending until the trust policy exists.
	if len(s.pluginPending) != plugins {
		t.Fatalf("pending plugins = %d, want %d", len(s.pluginPending), plugins)
	}

	params, err := json.Marshal(InitializeParams{RootURI: uri.FromPath(root).String()})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	s.dispatchInline(jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", params))
	elapsed := time.Since(start)
	// dispatchInline sends the response synchronously (send inside the
	// mutex); a failed initialize would have logged via the transport. The
	// settle check below proves the pipeline ran.
	if elapsed >= 120*time.Millisecond {
		t.Fatalf("initialize took %v with %d x %dMB entrypoints pending — plugin launch must not block the initialize response (round-23 OPT-3)", elapsed, plugins, blobMB)
	}

	// The launch phase must still run to completion: every plugin's launch
	// attempt ends in Running (helper executes) or Verified (launch refused —
	// these blob entrypoints are not executable). Nothing may stay pending or
	// enabled once the launch goroutine has drained.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		allSettled := true
		for _, id := range ids {
			st, err := s.pluginMgr.State(id)
			if err != nil || (st != plugin.StateRunning && st != plugin.StateVerified) {
				allSettled = false
				break
			}
		}
		if allSettled {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, id := range ids {
		st, err := s.pluginMgr.State(id)
		if err != nil || (st != plugin.StateRunning && st != plugin.StateVerified) {
			t.Fatalf("plugin %s never settled after the background launch: state=%v err=%v", id, st, err)
		}
	}
}

// writeBlobPluginDir assembles one plugin directory whose entrypoint is a
// deterministic blob of the requested size, checksum declared correctly so
// the trust chain accepts it and the LAUNCH-side SHA-256 re-verification is
// what pays.
func writeBlobPluginDir(t *testing.T, pluginsDir, id string, mb int64) {
	t.Helper()
	dir := filepath.Join(pluginsDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(dir, "entrypoint.bin")
	f, err := os.Create(blob)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	for written := int64(0); written < mb<<20; written += int64(len(chunk)) {
		if _, err := f.Write(chunk); err != nil {
			f.Close()
			t.Fatal(err)
		}
		_, _ = h.Write(chunk)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"id":           id,
		"version":      "0.1.0",
		"apiVersion":   "omnilsp.plugin.v1",
		"entrypoint":   "entrypoint.bin",
		"languages":    []string{"go"},
		"capabilities": []string{"diagnostics.emit"},
		"checksum":     "sha256:" + hex.EncodeToString(h.Sum(nil)),
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
