// X5-7 replay half: a typed semantic generation committed to the persistent
// store is recorded into a replay session; a fresh process reopens the store
// and the recording, serves the recorded query from the CURRENT restored
// generation (fresh-only), and full session verification binds the replayed
// response to the generation that was actually recovered. After a recommit the
// stale recording must stop verifying against fresh state while a lease pinned
// to the old generation keeps serving exactly its own facts.
package replay

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

const x5ReplayRootURI = "file:///x5-replay"

// x5ReplayTool is the pinned extractor tool identity. A replay generation
// identity is only complete with at least one full tool identity.
var x5ReplayTool = model.ToolIdentity{
	Name: "x5-replay-extractor", Path: "C:/tools/x5-replay-extractor.exe",
	Version: "1", SHA256: strings.Repeat("a", 64),
}

// x5ScriptTransport feeds one scripted client request to the recorder.
type x5ScriptTransport struct {
	in        chan *jsonrpc.Message
	closed    chan struct{}
	closeOnce sync.Once
}

func newX5ScriptTransport(msg *jsonrpc.Message) *x5ScriptTransport {
	t := &x5ScriptTransport{in: make(chan *jsonrpc.Message, 1), closed: make(chan struct{})}
	t.in <- msg
	return t
}

func (t *x5ScriptTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	select {
	case msg := <-t.in:
		return msg, nil
	case <-t.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *x5ScriptTransport) Write(_ context.Context, _ *jsonrpc.Message) error { return nil }

func (t *x5ScriptTransport) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

// Done satisfies transport.Transport: closed on Close, ending any Run.
func (t *x5ScriptTransport) Done() <-chan struct{} { return t.closed }

// x5ReplayView is an immutable model.WorkspaceView over one source file whose
// symbol name changes between generations, so a reader serving superseded
// facts is detectable by name.
type x5ReplayView struct {
	identity model.Identity
	file     model.File
	content  []byte
}

func (v *x5ReplayView) Identity() model.Identity { return v.identity }

func (v *x5ReplayView) Walk(_ context.Context, _ string, visit func(model.File) error) error {
	return visit(v.file)
}

func (v *x5ReplayView) Read(_ context.Context, uri string) (io.ReadCloser, error) {
	if uri != v.file.URI {
		return nil, fmt.Errorf("x5 replay view: no file %q", uri)
	}
	return io.NopCloser(strings.NewReader(string(v.content))), nil
}

// x5NthOccurrence locates the (skip+1)-th occurrence of needle and returns its
// line and column.
func x5NthOccurrence(content, needle string, skip int) (line, col int) {
	offset := -1
	for i := 0; i <= skip; i++ {
		next := strings.Index(content[offset+1:], needle)
		if next < 0 {
			panic("x5 replay fixture: occurrence missing from source")
		}
		offset += next + 1
	}
	prefix := content[:offset]
	line = strings.Count(prefix, "\n")
	col = offset - strings.LastIndex(prefix, "\n") - 1
	return line, col
}

// commitX5ReplayGeneration writes one complete typed semantic generation
// (symbol + definition/reference occurrences + coverage) through
// semantic.NewSink and commits it. The provenance pins a full tool identity so
// the generation carries a replayable SemanticIdentity.
func commitX5ReplayGeneration(t *testing.T, store *persistent.FileStore, symbolName string) uint64 {
	t.Helper()
	ctx := context.Background()
	mainContent := []byte("package replay\n\nfunc " + symbolName + "() {}\n\nfunc Caller() { " + symbolName + "() }\n")
	sum := sha256.Sum256(mainContent)
	fileHash := identity.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
	diskSum := sha256.Sum256([]byte("x5-replay-disk-" + symbolName))
	view := &x5ReplayView{
		identity: model.Identity{
			Workspace:  identity.WorkspaceID(x5ReplayRootURI),
			DiskDigest: identity.ContentHash("sha256:" + hex.EncodeToString(diskSum[:])),
		},
		file:    model.File{URI: x5ReplayRootURI + "/main.go", LanguageID: "go", Size: int64(len(mainContent)), SHA256: fileHash},
		content: mainContent,
	}
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatalf("begin replay build: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	scope := model.Scope{ID: "x5-replay-go", Language: "go", RootURI: x5ReplayRootURI, Build: model.BuildInputs{Options: map[string]string{}}}
	scope.BuildContext = model.ComputeBuildContextID(scope, "x5-replay-extractor", "1", "x5-replay-toolchain", []model.ToolIdentity{x5ReplayTool})
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion,
		Identity:      view.Identity(),
		Scope:         scope,
		Extractor:     "x5-replay-extractor",
		ExtractorVer:  "1",
		Backend:       identity.BackendID{Language: "go", Name: "x5-replay-backend"},
		Toolchain:     "x5-replay-toolchain",
		Tools:         []model.ToolIdentity{x5ReplayTool},
	}
	request := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	sink, err := semantic.NewSink(ctx, build, request)
	if err != nil {
		t.Fatalf("open replay semantic sink: %v", err)
	}
	defer func() { _ = sink.Close() }()
	symbolID := identity.SymbolID("go:x5-replay." + symbolName)
	defLine, defCol := x5NthOccurrence(string(mainContent), symbolName, 0)
	refLine, refCol := x5NthOccurrence(string(mainContent), symbolName, 1)
	if err := sink.WriteSymbols(ctx, []model.Symbol{{ID: symbolID, ScopeID: scope.ID, Name: symbolName, Kind: "function"}}); err != nil {
		t.Fatalf("write replay symbols: %v", err)
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{
		{SymbolID: symbolID, ScopeID: scope.ID, URI: view.file.URI,
			Range: model.Position{StartLine: uint32(defLine), StartChar: uint32(defCol), EndLine: uint32(defLine), EndChar: uint32(defCol + len(symbolName))},
			Role:  "definition", SourceHash: fileHash, BuildContext: scope.BuildContext},
		{SymbolID: symbolID, ScopeID: scope.ID, URI: view.file.URI,
			Range: model.Position{StartLine: uint32(refLine), StartChar: uint32(refCol), EndLine: uint32(refLine), EndChar: uint32(refCol + len(symbolName))},
			Role:  "reference", SourceHash: fileHash, BuildContext: scope.BuildContext},
	}); err != nil {
		t.Fatalf("write replay occurrences: %v", err)
	}
	report := model.Report{
		Identity:  view.Identity(),
		UsedTools: map[string][]model.ToolIdentity{scope.ID: {x5ReplayTool}},
	}
	for _, fact := range model.RequiredFactKinds {
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: model.Complete})
	}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatalf("finalize replay generation: %v", err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatalf("commit replay generation: %v", err)
	}
	committed = true
	return build.GenerationID()
}

// x5GenerationContentDigest mirrors the server's generation digest: SHA-256
// over length-prefixed segment IDs and payloads in manifest order.
func x5GenerationContentDigest(ctx context.Context, view persistent.GenerationView) (string, error) {
	h := sha256.New()
	for _, ref := range view.Segments {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		payload, err := view.ReadSegment(ref.ID)
		if err != nil {
			return "", err
		}
		x5DigestPart(h, []byte(ref.ID))
		x5DigestPart(h, payload)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func x5DigestPart(h hash.Hash, part []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(part)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(part)
}

// x5AvailableIdentity derives the replay SemanticIdentity of a generation
// restored from disk: generation, content digest, build contexts and tool
// identities — exactly the fields VerifySemanticReproduction checks.
func x5AvailableIdentity(ctx context.Context, view persistent.GenerationView) (SemanticIdentity, error) {
	reader, err := semantic.OpenReader(ctx, view)
	if err != nil {
		return SemanticIdentity{}, err
	}
	defer reader.Close()
	metadata := reader.Metadata()
	digest, err := x5GenerationContentDigest(ctx, view)
	if err != nil {
		return SemanticIdentity{}, err
	}
	contexts := make(map[string]string, len(metadata.Scopes))
	toolsByKey := make(map[string]ToolIdentity)
	for _, scope := range metadata.Scopes {
		if scope.ID == "" || scope.BuildContext == "" {
			return SemanticIdentity{}, fmt.Errorf("x5 replay: scope %q has no build identity", scope.ID)
		}
		contexts[scope.ID] = string(scope.BuildContext)
		for _, tool := range metadata.UsedTools[scope.ID] {
			toolsByKey[tool.Name+"\x00"+tool.Path+"\x00"+tool.Version+"\x00"+tool.SHA256] = ToolIdentity{
				Name: tool.Name, Path: tool.Path, Version: tool.Version, SHA256: tool.SHA256,
			}
		}
	}
	if len(contexts) == 0 || len(toolsByKey) == 0 {
		return SemanticIdentity{}, fmt.Errorf("x5 replay: generation %d lacks scopes or tool identity", view.ID)
	}
	keys := make([]string, 0, len(toolsByKey))
	for key := range toolsByKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	tools := make([]ToolIdentity, 0, len(keys))
	for _, key := range keys {
		tools = append(tools, toolsByKey[key])
	}
	return SemanticIdentity{Generation: view.ID, IndexContentDigest: digest, BuildContexts: contexts, Tools: tools}, nil
}

// x5SymbolNames reads the sorted symbol display names of a generation — the
// fresh-only query surface the replayed response is built on.
func x5SymbolNames(ctx context.Context, view persistent.GenerationView) ([]string, error) {
	reader, err := semantic.OpenReader(ctx, view)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var names []string
	for {
		batch, readErr := reader.Next(ctx)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		for _, symbol := range batch.Symbols {
			names = append(names, symbol.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// x5SemanticResponseBinder is the recorder/player seam the fresh-only query
// path binds its provenance through.
type x5SemanticResponseBinder interface {
	RegisterSemanticIdentity(id jsonrpc.RequestID, identity SemanticIdentity) error
	BindSemanticResponse(id jsonrpc.RequestID, semantic bool, generation uint64, indexContentDigest string) error
}

// serveX5HoverFromCurrentGeneration answers one hover request from the store's
// CURRENT committed generation — never a cached view — and binds the response
// to the identity of the generation it actually served.
func serveX5HoverFromCurrentGeneration(ctx context.Context, store *persistent.FileStore, binder x5SemanticResponseBinder, write func(context.Context, *jsonrpc.Message) error, msg *jsonrpc.Message) error {
	if !msg.IsRequest() || msg.Method != "textDocument/hover" || msg.ID == nil {
		return fmt.Errorf("x5 replay: unexpected message %q", msg.Method)
	}
	view, err := store.OpenSnapshot(ctx)
	if err != nil {
		return err
	}
	available, err := x5AvailableIdentity(ctx, view)
	if err != nil {
		return err
	}
	names, err := x5SymbolNames(ctx, view)
	if err != nil {
		return err
	}
	if len(names) != 1 {
		return fmt.Errorf("x5 replay: generation %d serves %d symbols, want exactly the recorded one", view.ID, len(names))
	}
	result, err := json.Marshal(map[string]string{"contents": "x5-replay:" + names[0]})
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

// TestPersistedSemanticGenerationReplayServesFreshOnlyAcrossProcesses closes
// the replay half of X5-7: the recording of a query served from a committed
// typed generation is restored across process boundaries together with the
// store, the replayed response is produced from the current restored
// generation and binds to its exact identity, CompareSession reports complete
// reproduction, and after another process recommits, the stale recording stops
// verifying (fresh-only) while the pinned generation keeps its own facts.
func TestPersistedSemanticGenerationReplayServesFreshOnlyAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sessionPath := root + "/session.jsonl"

	// --- Process A: commit the typed generation and record a hover session
	// whose response is produced from the current generation's facts. ---
	store, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	gen1 := commitX5ReplayGeneration(t, store, "RecordedOne")
	view1, err := store.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recordedIdentity, err := x5AvailableIdentity(ctx, view1)
	if err != nil {
		t.Fatalf("derive recorded identity: %v", err)
	}

	request := jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "textDocument/hover",
		json.RawMessage(`{"textDocument":{"uri":"file:///x5-replay/main.go"},"position":{"line":2,"character":5}}`))
	script := newX5ScriptTransport(request)
	recorder, err := NewRecorder(script, sessionPath, Meta{
		ConfigHash:       ConfigHash(map[string]string{"x5": "replay"}),
		SemanticIdentity: &recordedIdentity,
	}, nil)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	msg, err := recorder.Read(ctx)
	if err != nil {
		t.Fatalf("record request: %v", err)
	}
	if err := serveX5HoverFromCurrentGeneration(ctx, store, recorder, recorder.Write, msg); err != nil {
		t.Fatalf("record hover response: %v", err)
	}
	if err := script.Close(); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("recorder close: %v", err)
	}

	// --- Process B: reopen the store and the recording the way a fresh
	// process would, verify the restored generation satisfies the recorded
	// identity, then replay the session with responses served fresh-only. ---
	reopened, err := persistent.NewFileStore(root, persistent.Config{})
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
		t.Fatalf("reopened store lost generation %d: %v", gen1, err)
	}
	if restoredView.ID != gen1 {
		t.Fatalf("reopened store serves generation %d, want the committed %d", restoredView.ID, gen1)
	}
	restoredIdentity, err := x5AvailableIdentity(ctx, restoredView)
	if err != nil {
		t.Fatalf("derive restored identity: %v", err)
	}
	if !sameSemanticIdentity(&recordedIdentity, &restoredIdentity) {
		t.Fatalf("restored generation identity differs from the recording: %+v vs %+v", recordedIdentity, restoredIdentity)
	}
	if status, err := session.VerifySemanticReproduction(&restoredIdentity); status != ReproductionIdentityVerified || err != nil {
		t.Fatalf("restored generation fails recording verification: status=%s err=%v", status, err)
	}
	if err := session.VerifyResponseBindings(&restoredIdentity); err != nil {
		t.Fatalf("recorded response bindings reject the restored generation: %v", err)
	}

	player := NewPlayer()
	replayCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() {
		for {
			incoming, readErr := player.Transport().Read(replayCtx)
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					served <- nil
				} else {
					served <- readErr
				}
				return
			}
			if err := serveX5HoverFromCurrentGeneration(replayCtx, reopened, player, player.Transport().Write, incoming); err != nil {
				served <- err
				return
			}
		}
	}()
	if err := player.Replay(replayCtx, session.Entries); err != nil {
		t.Fatalf("replay recorded session: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatalf("fresh-only replay server: %v", err)
	}
	status, err := player.CompareSession(session, &restoredIdentity)
	if err != nil || status != ReproductionComplete {
		t.Fatalf("replay against the restored generation = %s, %v; want complete reproduction", status, err)
	}

	// --- Fresh-only enforcement: after a second process recommits, the stale
	// recording stops verifying against fresh state while the pinned
	// generation still serves exactly its own facts. ---
	lease, err := reopened.OpenSnapshotLease(ctx)
	if err != nil {
		t.Fatalf("pin generation %d: %v", gen1, err)
	}
	defer lease.Close()
	committer, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatalf("open committing store: %v", err)
	}
	gen2 := commitX5ReplayGeneration(t, committer, "RecordedTwo")
	if gen2 <= gen1 {
		t.Fatalf("generation IDs did not advance: gen1=%d gen2=%d", gen1, gen2)
	}
	currentView, err := reopened.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("open current generation: %v", err)
	}
	if currentView.ID != gen2 {
		t.Fatalf("fresh reader serves generation %d, want the committed %d", currentView.ID, gen2)
	}
	currentNames, err := x5SymbolNames(ctx, currentView)
	if err != nil {
		t.Fatal(err)
	}
	if len(currentNames) != 1 || currentNames[0] != "RecordedTwo" {
		t.Fatalf("fresh reader facts after recommit = %v, want only [RecordedTwo]", currentNames)
	}
	freshIdentity, err := x5AvailableIdentity(ctx, currentView)
	if err != nil {
		t.Fatalf("derive fresh identity: %v", err)
	}
	if _, err := session.VerifySemanticReproduction(&freshIdentity); !errors.Is(err, ErrSemanticGenerationUnavailable) {
		t.Fatalf("stale recording verified against the recommitted store: err=%v, want %v", err, ErrSemanticGenerationUnavailable)
	}
	if err := session.VerifyResponseBindings(&freshIdentity); !errors.Is(err, ErrSemanticGenerationUnavailable) {
		t.Fatalf("stale response bindings verified against the recommitted store: err=%v, want %v", err, ErrSemanticGenerationUnavailable)
	}
	if pinned := lease.Snapshot(); pinned.ID != gen1 {
		t.Fatalf("lease pinned generation %d, want %d", pinned.ID, gen1)
	}
	pinnedNames, err := x5SymbolNames(ctx, lease.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if len(pinnedNames) != 1 || pinnedNames[0] != "RecordedOne" {
		t.Fatalf("pinned reader facts after recommit = %v, want unchanged [RecordedOne]", pinnedNames)
	}
}
