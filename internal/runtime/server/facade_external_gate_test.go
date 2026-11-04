package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// TestARCH_CallMethodExternalChangeGated pins bug round 23 A / BUG-1: the four
// external workspace-change notifications (didChangeWatchedFiles, didCreate-
// Files, didRenameFiles, didDeleteFiles) are VFS-mutating methods — they run
// invalidateExternalSources (AdvanceRevision + publishSnapshot) — yet they were
// missing from facadeMutatingMethods, so CallMethod dispatched them raw: in the
// Uninitialized state they mutated VFS state and published snapshots, exactly
// the bypass the C2 lifecycle gate exists to prevent (§C2: CallMethod must
// admit a mutating method only in the state the LSP surface would).
//
// Uninitialized: all four must be rejected and nothing may publish.
// Running: the same calls must be admitted (the facade is a legitimate
// surface; the gate only mirrors lifecycle, it must not break it).
func TestARCH_CallMethodExternalChangeGated(t *testing.T) {
	s := New(DefaultConfig())
	ctx := context.Background()
	uri := "file:///gated-external/main.go"

	createParams := func(u string) map[string]any {
		return map[string]any{"files": []map[string]any{{"uri": u}}}
	}
	watchedParams := func(u string) map[string]any {
		return map[string]any{"changes": []map[string]any{{"uri": u, "type": 1}}}
	}
	renameParams := map[string]any{"files": []map[string]any{{"oldUri": uri, "newUri": uri + ".new"}}}

	calls := []struct {
		method string
		params any
	}{
		{"workspace/didChangeWatchedFiles", watchedParams(uri)},
		{"workspace/didCreateFiles", createParams(uri)},
		{"workspace/didRenameFiles", renameParams},
		{"workspace/didDeleteFiles", createParams(uri)},
	}

	// Uninitialized: lifecycle gate must refuse every one of them.
	for _, c := range calls {
		_, err := s.CallMethod(ctx, c.method, c.params)
		if err == nil || !strings.Contains(err.Error(), "not allowed in state") {
			t.Errorf("%s in state %s: want lifecycle gate rejection, got %v", c.method, s.State(), err)
		}
	}
	if rev := s.vfs.Revision(); rev != 0 {
		t.Errorf("VFS revision advanced through ungated facade in Uninitialized state: %d", rev)
	}
	if cur := s.snapMgr.Current(); cur != nil {
		t.Errorf("snapshot published through ungated facade in Uninitialized state: rev %d", cur.Revision())
	}

	// Running: the four notifications are legitimate facade traffic and must
	// succeed (gating is lifecycle-only, not an admission ban).
	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()
	for _, c := range calls {
		if _, err := s.CallMethod(ctx, c.method, c.params); err != nil {
			t.Errorf("%s via facade in Running state: %v", c.method, err)
		}
	}
	if s.snapMgr.Current() == nil {
		t.Error("external change notifications in Running state published no snapshot")
	}
}

// TestFacadeExternalChangeCannotRegressPublishedSnapshot pins the concurrency
// half of BUG-1: in the Running state the read loop applies didChange inline
// (dispatchInline holds mutationMu) while CallMethod dispatched the external
// change notifications on the caller goroutine WITHOUT that mutex. Both paths
// end in publishSnapshot, whose read-then-publish (rev := vfs.Revision() …
// snapMgr.Publish) is not atomic; an interleaving publishes an older revision
// over a newer one and the published snapshot regresses — stale captured
// revisions can then pass verifyEnvelopeFresh.
//
// The pin drives both surfaces concurrently per round with an observer
// sampling the published revision; the published revision must be monotone
// non-decreasing (each publish under the shared writer lock observes a
// revision ≥ every previously published one).
func TestFacadeExternalChangeCannotRegressPublishedSnapshot(t *testing.T) {
	s := New(DefaultConfig())
	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()
	ctx := context.Background()

	const docs = 24
	for i := 0; i < docs; i++ {
		uri := fmt.Sprintf("file:///race-ext/doc%d.go", i)
		if _, err := s.CallMethod(ctx, "textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": 1, "text": "seed\n"},
		}); err != nil {
			t.Fatalf("didOpen doc %d via facade: %v", i, err)
		}
	}

	// Observer: sample the published snapshot revision continuously; any value
	// strictly below the high-water mark is a regression window.
	var regressions, samples atomic.Int64
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		high := uint64(0)
		for !stop.Load() {
			if cur := s.snapMgr.Current(); cur != nil {
				rev := cur.Revision()
				samples.Add(1)
				if rev < high {
					regressions.Add(1)
				}
				if rev > high {
					high = rev
				}
			}
		}
	}()

	const rounds = 300
	for round := 0; round < rounds; round++ {
		uri := fmt.Sprintf("file:///race-ext/doc%d.go", round%docs)
		rawChange, err := json.Marshal(map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": 5},
			"contentChanges": []map[string]any{{"text": fmt.Sprintf("edit-%d\n", round)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		loopMsg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "read-loop", IsStr: true}, "textDocument/didChange", rawChange)

		createURI := fmt.Sprintf("file:///race-ext/created-%d.go", round)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			// Read-loop surface: inline didChange under mutationMu.
			s.dispatchInline(loopMsg)
		}()
		go func() {
			defer wg.Done()
			// Facade surface: external change notification.
			_, _ = s.CallMethod(ctx, "workspace/didCreateFiles", map[string]any{
				"files": []map[string]any{{"uri": createURI}},
			})
		}()
		wg.Wait()
	}

	stop.Store(true)
	<-done

	if n := regressions.Load(); n != 0 {
		t.Fatalf("published snapshot revision regressed in %d of %d sampled observations across %d rounds", n, samples.Load(), rounds)
	}
	if samples.Load() == 0 {
		t.Fatal("observer sampled nothing")
	}
}
