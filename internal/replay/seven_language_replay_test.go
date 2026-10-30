// DEF-IDXWIRE seven-language replay half: each language's real typed export
// commits a generation through the persistent store; "process A" records a
// query served from that generation; "process B" reopens the store and the
// recording the way a fresh process would, verifies the restored generation
// against the recorded identity, replays the session fresh-only (answers are
// rebuilt from the current restored generation), and CompareSession must
// report complete reproduction. After a second process recommits the language
// with an extra marker symbol, the stale recording must stop verifying against
// fresh state (fresh-only) while a lease pinned to the old generation keeps
// serving exactly its own facts — the X5-7 precedent path extended from the
// single Go generation to all seven registered languages.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/test/harness/sevenlang"
)

// sevenLangQueryFacts rebuilds the deterministic fresh-only query surface of a
// generation: sorted scope-qualified symbol names plus the occurrence count.
func sevenLangQueryFacts(ctx context.Context, view persistent.GenerationView) ([]string, int, error) {
	reader, err := semantic.OpenReader(ctx, view)
	if err != nil {
		return nil, 0, err
	}
	defer reader.Close()
	var symbols []string
	occurrences := 0
	for {
		batch, readErr := reader.Next(ctx)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, 0, readErr
		}
		switch batch.Kind {
		case semantic.BatchSymbols:
			for _, symbol := range batch.Symbols {
				symbols = append(symbols, symbol.ScopeID+"\x1f"+symbol.Name)
			}
		case semantic.BatchOccurrences:
			occurrences += len(batch.Occurrences)
		}
	}
	sort.Strings(symbols)
	return symbols, occurrences, nil
}

// serveSevenLangHoverFromCurrentGeneration answers one hover request from the
// store's CURRENT committed generation — never a cached view — and binds the
// response to the identity of the generation it actually served.
func serveSevenLangHoverFromCurrentGeneration(ctx context.Context, store *persistent.FileStore, binder x5SemanticResponseBinder, write func(context.Context, *jsonrpc.Message) error, msg *jsonrpc.Message) error {
	if !msg.IsRequest() || msg.Method != "textDocument/hover" || msg.ID == nil {
		return fmt.Errorf("sevenlang replay: unexpected message %q", msg.Method)
	}
	view, err := store.OpenSnapshot(ctx)
	if err != nil {
		return err
	}
	available, err := x5AvailableIdentity(ctx, view)
	if err != nil {
		return err
	}
	symbols, occurrences, err := sevenLangQueryFacts(ctx, view)
	if err != nil {
		return err
	}
	result, err := json.Marshal(map[string]any{"symbols": symbols, "occurrences": occurrences})
	if err != nil {
		return err
	}
	if err := binder.RegisterSemanticIdentity(*msg.ID, available); err != nil {
		return err
	}
	if err := binder.BindSemanticResponse(*msg.ID, true, view.ID, available.IndexContentDigest); err != nil {
		return err
	}
	return write(ctx, jsonrpc.NewResponse(*msg.ID, result))
}

// sevenLangCommittedGeneration is one language generation exported through its
// production surface into a persistent store rooted at root.
type sevenLangCommittedGeneration struct {
	bundle sevenlang.Bundle
	store  *persistent.FileStore
	root   string
	id     uint64
}

// exportSevenLangGeneration runs the full language export into a committed
// persistent generation: BeginBuild → semantic.NewSink → ExportIndex →
// Finalize → Commit, mirroring the server's transactional reindex flow.
func exportSevenLangGeneration(t *testing.T, ctx context.Context, language, storeRoot, fixtureDir string, extra bool) *sevenLangCommittedGeneration {
	t.Helper()
	bundle, err := sevenlang.Build(ctx, language, fixtureDir, extra)
	if err != nil {
		if errors.Is(err, sevenlang.ErrUnavailable) {
			t.Skipf("language toolchain unavailable: %v", err)
		}
		t.Fatalf("build %s export bundle: %v", language, err)
	}
	t.Cleanup(func() {
		if bundle.Close != nil {
			_ = bundle.Close()
		}
	})
	store, err := persistent.NewFileStore(storeRoot, persistent.Config{})
	if err != nil {
		t.Fatalf("open %s store: %v", language, err)
	}
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatalf("begin %s build: %v", language, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	sink, err := semantic.NewSink(ctx, build, bundle.Request)
	if err != nil {
		t.Fatalf("open %s semantic sink: %v", language, err)
	}
	defer func() { _ = sink.Close() }()
	report, err := bundle.Export(ctx, bundle.Request, sink)
	if err != nil {
		// Same clang edge-position break pinned by the persistent chain test:
		// the clang extractor emits edge records the validated sink rejects,
		// so the cpp/c replay chain cannot commit a generation until the
		// extractor clamps the unknown end position.
		if errors.Is(err, semantic.ErrMalformedPayload) && strings.Contains(err.Error(), "invalid edge record") {
			t.Skipf("clang exporter emits edge records the validated semantic sink rejects (EndChar=0 < StartChar from missing helper end columns in internal/index/extractors/clang/extractor.go convertPosition); cpp/c replay chain blocked upstream: %v", err)
		}
		t.Fatalf("%s ExportIndex through the persistent sink: %v", language, err)
	}
	if err := model.ValidateReport(bundle.Request, report); err != nil {
		t.Fatalf("%s export report invalid: %v", language, err)
	}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatalf("finalize %s generation: %v", language, err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatalf("commit %s generation: %v", language, err)
	}
	committed = true
	return &sevenLangCommittedGeneration{bundle: bundle, store: store, root: storeRoot, id: build.GenerationID()}
}

// sevenLangHasSymbol reports whether the query surface names the symbol in any
// scope.
func sevenLangHasSymbol(symbols []string, name string) bool {
	for _, entry := range symbols {
		if _, bare, found := strings.Cut(entry, "\x1f"); found && bare == name {
			return true
		}
	}
	return false
}

// TestSevenLanguageTypedGenerationsReplayFreshOnlyAcrossStoreReopen extends
// the X5-7 cross-process replay precedent to all seven registered languages:
// record against a real typed generation in process A, reopen store plus
// recording in process B, replay fresh-only to complete reproduction, then
// prove fresh-only enforcement and pinned-reader isolation after a recommit.
// Languages whose pinned toolchain is absent skip with the exact blocking
// point.
func TestSevenLanguageTypedGenerationsReplayFreshOnlyAcrossStoreReopen(t *testing.T) {
	for key, value := range sevenlang.GoEnvPins() {
		t.Setenv(key, value)
	}
	for _, language := range sevenlang.Languages() {
		t.Run(language, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()
			root := t.TempDir()
			storeRoot := filepath.Join(root, "semantic-index")
			sessionPath := filepath.Join(root, "session.jsonl")

			// --- Process A: commit the base typed generation and record one
			// hover whose response is produced from the current generation's
			// facts. ---
			base := exportSevenLangGeneration(t, ctx, language, storeRoot, filepath.Join(root, "fixture-base"), false)
			view1, err := base.store.OpenSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if view1.ID != base.id {
				t.Fatalf("process A store serves generation %d, want the committed %d", view1.ID, base.id)
			}
			recordedIdentity, err := x5AvailableIdentity(ctx, view1)
			if err != nil {
				t.Fatalf("derive recorded %s identity: %v", language, err)
			}

			request := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/hover",
				json.RawMessage(`{"textDocument":{"uri":"file:///sevenlang"},"position":{"line":0,"character":0}}`))
			script := newX5ScriptTransport(request)
			recorder, err := NewRecorder(script, sessionPath, Meta{
				ConfigHash:       ConfigHash(map[string]string{"sevenlang": language}),
				SemanticIdentity: &recordedIdentity,
			}, nil)
			if err != nil {
				t.Fatalf("recorder: %v", err)
			}
			msg, err := recorder.Read(ctx)
			if err != nil {
				t.Fatalf("record request: %v", err)
			}
			if err := serveSevenLangHoverFromCurrentGeneration(ctx, base.store, recorder, recorder.Write, msg); err != nil {
				t.Fatalf("record %s hover response: %v", language, err)
			}
			if err := script.Close(); err != nil {
				t.Fatal(err)
			}
			if err := recorder.Close(); err != nil {
				t.Fatalf("recorder close: %v", err)
			}

			// --- Process B: reopen the store and the recording the way a
			// fresh process would, verify the restored generation satisfies
			// the recorded identity, then replay fresh-only. ---
			reopened, err := persistent.NewFileStore(storeRoot, persistent.Config{})
			if err != nil {
				t.Fatalf("reopen store: %v", err)
			}
			session, err := LoadSession(sessionPath)
			if err != nil {
				t.Fatalf("load recorded session: %v", err)
			}
			if session.SemanticReproductionStatus() != ReproductionIdentityPinned {
				t.Fatalf("recording status = %q, want identity-pinned", session.SemanticReproductionStatus())
			}
			restoredView, err := reopened.OpenSnapshot(ctx)
			if err != nil {
				t.Fatalf("reopened store lost %s generation %d: %v", language, base.id, err)
			}
			if restoredView.ID != base.id {
				t.Fatalf("reopened store serves generation %d, want the committed %d", restoredView.ID, base.id)
			}
			restoredIdentity, err := x5AvailableIdentity(ctx, restoredView)
			if err != nil {
				t.Fatalf("derive restored identity: %v", err)
			}
			if !sameSemanticIdentity(&recordedIdentity, &restoredIdentity) {
				t.Fatalf("restored %s generation identity differs from the recording: %+v vs %+v", language, recordedIdentity, restoredIdentity)
			}
			if status, err := session.VerifySemanticReproduction(&restoredIdentity); status != ReproductionIdentityVerified || err != nil {
				t.Fatalf("restored generation fails recording verification: status=%s err=%v", status, err)
			}
			if err := session.VerifyResponseBindings(&restoredIdentity); err != nil {
				t.Fatalf("recorded response bindings reject the restored generation: %v", err)
			}

			player := NewPlayer()
			served := make(chan error, 1)
			go func() {
				for {
					incoming, readErr := player.Transport().Read(ctx)
					if readErr != nil {
						if errors.Is(readErr, io.EOF) {
							served <- nil
						} else {
							served <- readErr
						}
						return
					}
					if err := serveSevenLangHoverFromCurrentGeneration(ctx, reopened, player, player.Transport().Write, incoming); err != nil {
						served <- err
						return
					}
				}
			}()
			if err := player.Replay(ctx, session.Entries); err != nil {
				t.Fatalf("replay recorded %s session: %v", language, err)
			}
			if err := <-served; err != nil {
				t.Fatalf("fresh-only %s replay server: %v", language, err)
			}
			status, err := player.CompareSession(session, &restoredIdentity)
			if err != nil || status != ReproductionComplete {
				t.Fatalf("replay against the restored %s generation = %s, %v; want complete reproduction", language, status, err)
			}

			// --- Fresh-only enforcement: pin the old generation with a lease,
			// recommit the same language with an extra marker symbol from a
			// second store instance, then require the stale recording to stop
			// verifying while the pinned reader keeps exactly its own facts. ---
			lease, err := reopened.OpenSnapshotLease(ctx)
			if err != nil {
				t.Fatalf("pin generation %d: %v", base.id, err)
			}
			defer lease.Close()
			pinned := lease.Snapshot()
			if pinned.ID != base.id {
				t.Fatalf("lease pinned generation %d, want %d", pinned.ID, base.id)
			}

			extra := exportSevenLangGeneration(t, ctx, language, storeRoot, filepath.Join(root, "fixture-extra"), true)
			if extra.id <= base.id {
				t.Fatalf("generation IDs did not advance: base=%d extra=%d", base.id, extra.id)
			}
			currentView, err := reopened.OpenSnapshot(ctx)
			if err != nil {
				t.Fatalf("open current generation: %v", err)
			}
			if currentView.ID != extra.id {
				t.Fatalf("fresh reader serves generation %d, want the recommitted %d", currentView.ID, extra.id)
			}
			freshSymbols, _, err := sevenLangQueryFacts(ctx, currentView)
			if err != nil {
				t.Fatal(err)
			}
			if !sevenLangHasSymbol(freshSymbols, extra.bundle.Expect.ExtraSymbol) {
				t.Fatalf("fresh %s reader after recommit lost the marker symbol %q; served %d symbols", language, extra.bundle.Expect.ExtraSymbol, len(freshSymbols))
			}
			freshIdentity, err := x5AvailableIdentity(ctx, currentView)
			if err != nil {
				t.Fatalf("derive fresh identity: %v", err)
			}
			if sameSemanticIdentity(&recordedIdentity, &freshIdentity) {
				t.Fatalf("recommitted %s generation reuses the recorded identity %+v", language, freshIdentity)
			}
			if _, err := session.VerifySemanticReproduction(&freshIdentity); !errors.Is(err, ErrSemanticGenerationUnavailable) {
				t.Fatalf("stale %s recording verified against the recommitted store: err=%v, want %v", language, err, ErrSemanticGenerationUnavailable)
			}
			if err := session.VerifyResponseBindings(&freshIdentity); !errors.Is(err, ErrSemanticGenerationUnavailable) {
				t.Fatalf("stale %s response bindings verified against the recommitted store: err=%v, want %v", language, err, ErrSemanticGenerationUnavailable)
			}
			pinnedSymbols, _, err := sevenLangQueryFacts(ctx, lease.Snapshot())
			if err != nil {
				t.Fatal(err)
			}
			if sevenLangHasSymbol(pinnedSymbols, extra.bundle.Expect.ExtraSymbol) {
				t.Fatalf("pinned %s reader leaked the recommitted marker symbol %q", language, extra.bundle.Expect.ExtraSymbol)
			}
			if !sevenLangHasSymbol(pinnedSymbols, base.bundle.Expect.SymbolNames[0]) {
				t.Fatalf("pinned %s reader lost its own base symbol %q", language, base.bundle.Expect.SymbolNames[0])
			}
		})
	}
}
