package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// TestFacadeDidChangeCannotDoubleApplyAgainstReadLoop pins bug round 20 A#9
// (圈 3): CallMethod admits the VFS-mutating notifications (didOpen/didChange/
// didSave/didClose) once the server is Running, but dispatched them on the
// caller goroutine WITHOUT mutationMu — while the LSP read loop applies the
// same notifications under mutationMu (dispatchInline, F1 single writer).
// Two same-version full-text edits arriving through both surfaces could then
// both pass the version gate's check-then-act (handlers.go: newVersion <=
// cur.Version) and both reach VFS.Update: one edit silently lost, the exact
// PROT-SYNC-001 shape the inlineMethods comment claims was eliminated.
//
// The pin drives both surfaces concurrently per round and requires the
// version gate to admit exactly one of the two same-version edits.
func TestFacadeDidChangeCannotDoubleApplyAgainstReadLoop(t *testing.T) {
	s := New(DefaultConfig())
	s.mu.Lock()
	s.state = StateRunning
	s.mu.Unlock()
	ctx := context.Background()

	changeParams := func(uri, text string) map[string]any {
		return map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": 5},
			"contentChanges": []map[string]any{{"text": text}},
		}
	}

	for round := 0; round < 200; round++ {
		uri := fmt.Sprintf("file:///race/doc%d.go", round)
		if _, err := s.CallMethod(ctx, "textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": 1, "text": "seed\n"},
		}); err != nil {
			t.Fatalf("round %d: didOpen via facade: %v", round, err)
		}

		rawB, err := json.Marshal(changeParams(uri, "BBBB\n"))
		if err != nil {
			t.Fatal(err)
		}
		loopMsg := jsonrpc.NewRequest(jsonrpc.RequestID{Str: "read-loop", IsStr: true}, "textDocument/didChange", rawB)

		before := s.vfs.Revision()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = s.CallMethod(ctx, "textDocument/didChange", changeParams(uri, "AAAA\n")) // façade surface
		}()
		go func() {
			defer wg.Done()
			s.dispatchInline(loopMsg) // read-loop surface (F1 single writer)
		}()
		wg.Wait()

		if delta := s.vfs.Revision() - before; delta != 1 {
			t.Fatalf("round %d: same-version didChange pair produced %d VFS updates, want exactly 1 (version gate must reject the loser)", round, delta)
		}
		if content := string(s.vfs.Content(uri)); content != "AAAA\n" && content != "BBBB\n" {
			t.Fatalf("round %d: doc content is neither edit: %q", round, content)
		}

		if _, err := s.CallMethod(ctx, "textDocument/didClose", map[string]any{
			"textDocument": map[string]any{"uri": uri},
		}); err != nil {
			t.Fatalf("round %d: didClose: %v", round, err)
		}
	}
}
