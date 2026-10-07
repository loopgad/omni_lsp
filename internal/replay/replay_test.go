package replay

// Golden round-trip: record a live session against a server, replay the
// "in" stream into a fresh identical server, and require the produced "out"
// stream to match the recording message-for-message (§P9 deterministic
// replay; volatile fields stripped by normalization).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/server"
)

// stubBackend answers deterministically — no toolchain required.
type stubBackend struct{}

func (s *stubBackend) LanguageID() string       { return "go" }
func (s *stubBackend) FileExtensions() []string { return []string{".go"} }
func (s *stubBackend) Close() error             { return nil }

func (s *stubBackend) Completion(_ context.Context, _ languages.CompletionRequest) ([]languages.CompletionItem, error) {
	return nil, nil
}

func (s *stubBackend) Hover(_ context.Context, req languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: "stub hover"},
	}, nil
}

func (s *stubBackend) Definition(_ context.Context, _ languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{Status: identity.ResultExact, Value: []languages.Location{}}, nil
}

func (s *stubBackend) References(_ context.Context, _ languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{Status: identity.ResultExact, Value: []languages.Location{}}, nil
}

func (s *stubBackend) DocumentSymbols(_ context.Context, _ languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	return nil, nil
}

func (s *stubBackend) WorkspaceSymbols(_ context.Context, _ languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	return nil, nil
}

func (s *stubBackend) SemanticTokens(_ context.Context, _ string, _ []byte) ([]languages.SemanticToken, error) {
	return nil, nil
}

func (s *stubBackend) Rename(_ context.Context, _ languages.RenameRequest) (identity.SemanticResult[languages.ValidatedEdit], error) {
	return identity.SemanticResult[languages.ValidatedEdit]{
		Status: identity.ResultUnavailable,
	}, nil
}

func (s *stubBackend) Diagnostics(_ context.Context, _ string, _ []byte) ([]languages.Diagnostic, error) {
	return nil, nil
}

// feedTransport is a bidirectional fake: Read serves scripted messages,
// Write records everything the server produces.
type feedTransport struct {
	in      chan *jsonrpc.Message
	writesM sync.Mutex
	writes  []*jsonrpc.Message
	done    chan struct{}
	closed  chan struct{}
}

func newFeedTransport(in ...*jsonrpc.Message) *feedTransport {
	f := &feedTransport{
		in:     make(chan *jsonrpc.Message, len(in)),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
	for _, m := range in {
		f.in <- m
	}
	close(f.in) // EOF after the script drains
	return f
}

func (f *feedTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	select {
	case msg, ok := <-f.in:
		if !ok {
			// Script drained: hold Run open until the test closes or cancels.
			select {
			case <-f.closed:
				return nil, context.Canceled
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *feedTransport) Write(_ context.Context, msg *jsonrpc.Message) error {
	f.writesM.Lock()
	f.writes = append(f.writes, msg)
	f.writesM.Unlock()
	return nil
}

func (f *feedTransport) Close() error { close(f.closed); return nil }

// Done satisfies transport.Transport: closed on Close, ending Run.
func (f *feedTransport) Done() <-chan struct{} { return f.closed }

func (f *feedTransport) written() int {
	f.writesM.Lock()
	defer f.writesM.Unlock()
	return len(f.writes)
}

func scriptedIn() []*jsonrpc.Message {
	initParams := json.RawMessage(`{"processId":null,"rootUri":"file:///w","capabilities":{}}`)
	openParams := json.RawMessage(`{"textDocument":{"uri":"file:///w/a.go","languageId":"go","version":1,"text":"package a"}}`)
	hoverParams := json.RawMessage(`{"textDocument":{"uri":"file:///w/a.go"},"position":{"line":0,"character":0}}`)
	shutdownParams := json.RawMessage(`null`)
	return []*jsonrpc.Message{
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 1}, "initialize", initParams),
		jsonrpc.NewNotification("initialized", json.RawMessage(`{}`)),
		jsonrpc.NewNotification("textDocument/didOpen", openParams),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 2}, "textDocument/hover", hoverParams),
		jsonrpc.NewRequest(jsonrpc.RequestID{Num: 3}, "shutdown", shutdownParams),
	}
}

const wantResponses = 3 // initialize + hover + shutdown

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached in time")
}

func TestP9_RecordReplayRoundTrip(t *testing.T) {
	dir := t.TempDir()

	// --- Phase 1: record a session against a live server. ---
	live := server.New(server.DefaultConfig())
	live.RegisterBackend("go", &stubBackend{})
	script := scriptedIn()
	ft := newFeedTransport(script...)
	rec, err := NewRecorder(ft, dir+"/session.jsonl", Meta{
		ConfigHash: ConfigHash(map[string]string{"transport": "stdio"}),
		Toolchain:  map[string]string{"go": "stub-1"},
	}, live.SnapshotRevision)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}

	sessionDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { sessionDone <- live.Run(ctx, rec) }()

	waitFor(t, func() bool { return ft.written() >= wantResponses })
	cancel()
	select {
	case <-sessionDone:
	case <-time.After(3 * time.Second):
		t.Fatal("live run did not stop")
	}
	if err := rec.Close(); err != nil {
		t.Fatalf("recorder close: %v", err)
	}

	// --- Phase 2: load and replay into a fresh server. ---
	sess, err := LoadSession(dir + "/session.jsonl")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if sess.Meta.ConfigHash == "" || len(sess.Entries) == 0 {
		t.Fatal("session meta/entries missing")
	}

	fresh := server.New(server.DefaultConfig())
	fresh.RegisterBackend("go", &stubBackend{})
	player := NewPlayer()
	for _, e := range sess.Entries {
		if e.Dir != "in" {
			continue
		}
		var msg jsonrpc.Message
		if err := json.Unmarshal(e.Payload, &msg); err != nil {
			t.Fatalf("entry %d unparsable: %v", e.Seq, err)
		}
		player.Feed(&msg)
	}
	go func() {
		// End the feed only after every scripted response reached the player:
		// an early EOF makes the server cancel in-flight requests, which would
		// diverge the replay for scheduling reasons instead of content ones.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && len(player.Out()) < wantResponses {
			time.Sleep(2 * time.Millisecond)
		}
		player.EndFeed()
	}()

	replayCtx, replayCancel := context.WithCancel(context.Background())
	runErr := fresh.Run(replayCtx, player.Transport())
	replayCancel()
	if runErr != nil && runErr != context.Canceled {
		t.Fatalf("replay run: %v", runErr)
	}

	if err := player.CompareOut(sess.Entries); err != nil {
		t.Fatal(err)
	}
}

func TestPlayerTransportReadEOFThenCloseIsIdempotent(t *testing.T) {
	player := NewPlayer()
	player.EndFeed()
	if _, err := player.Transport().Read(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("read after EndFeed = %v, want EOF", err)
	}

	const closeCallers = 8
	var wg sync.WaitGroup
	errs := make(chan error, closeCallers)
	for i := 0; i < closeCallers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- player.Transport().Close()
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("repeated concurrent transport close: %v", err)
		}
	}

	select {
	case <-player.Transport().Done():
	default:
		t.Fatal("transport Done channel remains open after EOF and Close")
	}
}

func TestPlayerTransportCloseUnblocksRead(t *testing.T) {
	player := NewPlayer()
	readStarted := make(chan struct{})
	readResult := make(chan error, 1)
	go func() {
		close(readStarted)
		_, err := player.Transport().Read(context.Background())
		readResult <- err
	}()
	<-readStarted

	if err := player.Transport().Close(); err != nil {
		t.Fatalf("close transport: %v", err)
	}
	select {
	case err := <-readResult:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("blocked read after Close = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read")
	}
}

func TestPlayerReplayPreservesResponseBeforeShutdown(t *testing.T) {
	player := NewPlayer()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	entries := []Entry{
		{Seq: 1, Dir: "in", Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"query"}`)},
		{Seq: 2, Dir: "out", Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":true}`)},
		{Seq: 3, Dir: "in", Payload: json.RawMessage(`{"jsonrpc":"2.0","method":"exit"}`)},
	}
	done := make(chan error, 1)
	go func() { done <- player.Replay(ctx, entries) }()
	request, err := player.Transport().Read(ctx)
	if err != nil || request.Method != "query" {
		t.Fatalf("first request: %v, %v", request, err)
	}
	if len(player.t.in) != 0 {
		t.Fatal("later exit was fed before its recorded response barrier")
	}
	if err := player.Transport().Write(ctx, jsonrpc.NewResponse(*request.ID, json.RawMessage(`true`))); err != nil {
		t.Fatal(err)
	}
	exit, err := player.Transport().Read(ctx)
	if err != nil || exit.Method != "exit" {
		t.Fatalf("exit: %v, %v", exit, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPlayerDrainNotificationsCannotAnswerRequests(t *testing.T) {
	player := NewPlayer()
	ctx := context.Background()
	var request jsonrpc.Message
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":1,"method":"query"}`), &request); err != nil {
		t.Fatal(err)
	}
	player.Feed(&request)
	if _, err := player.Transport().Read(ctx); err != nil {
		t.Fatal(err)
	}
	player.t.drainWait = time.Millisecond
	player.EndFeed()
	if err := player.Transport().Write(ctx, &jsonrpc.Message{Method: "notification"}); err != nil {
		t.Fatal(err)
	}
	if _, err := player.Transport().Read(ctx); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("unanswered request was treated as successful EOF: %v", err)
	}
}

func TestReplayNormalizeEvidenceClockPreservesIdentityAndUserFields(t *testing.T) {
	raw := json.RawMessage(`{"method":"workspace/symbol","status":"exact","completeness":"complete","Timestamp":"user-value","user":{"Timestamp":"2026-10-02T10:43:40Z","Kind":3,"Snapshot":{},"Assurance":2},"evidence":[{"Timestamp":"2026-10-02T10:43:39.065346Z","Kind":3,"Snapshot":{"Revision":7},"Assurance":2,"IndexGen":9,"SourceHash":"sha256:source"}]}`)
	got := string(normalize(&jsonrpc.Message{Result: raw}).Result)
	for _, field := range []string{`"Timestamp":"user-value"`, `"IndexGen":9`, `"SourceHash":"sha256:source"`, `"Revision":7`} {
		if !strings.Contains(got, field) {
			t.Fatalf("normalization removed identity or user field %s: %s", field, got)
		}
	}
	if !strings.Contains(got, "2026-10-02T10:43:40Z") {
		t.Fatalf("opaque user timestamp was removed: %s", got)
	}
	if strings.Contains(got, "2026-10-02T10:43:39") {
		t.Fatalf("evidence clock was retained: %s", got)
	}
}

func TestPlayerClosedTransportRejectsBufferedInputAndFeed(t *testing.T) {
	player := NewPlayer()
	player.Feed(&jsonrpc.Message{Method: "queued"})
	if err := player.Transport().Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := player.Transport().Read(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("closed transport delivered buffered input: %v", err)
	}
	if err := player.FeedContext(context.Background(), &jsonrpc.Message{Method: "later"}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("closed transport accepted input: %v", err)
	}
}

func TestLegacySessionLoadsAsPartialUnverified(t *testing.T) {
	path := t.TempDir() + "/legacy.jsonl"
	legacy := &Session{
		Meta:    Meta{FormatVersion: legacyFormatVersion, ConfigHash: "legacy-config"},
		Entries: []Entry{{Seq: 1, Dir: "in", Payload: json.RawMessage(`{"jsonrpc":"2.0"}`)}},
	}
	if err := Save(path, legacy); err != nil {
		t.Fatalf("save legacy session: %v", err)
	}

	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("legacy session should remain readable: %v", err)
	}
	if len(loaded.Entries) != 1 {
		t.Fatalf("loaded entries = %d, want 1", len(loaded.Entries))
	}
	if got := loaded.SemanticReproductionStatus(); got != ReproductionPartialUnverified {
		t.Fatalf("legacy semantic status = %q, want %q", got, ReproductionPartialUnverified)
	}
	got, err := loaded.VerifySemanticReproduction(nil)
	if got != ReproductionPartialUnverified || !errors.Is(err, ErrSemanticIdentityUnverified) {
		t.Fatalf("legacy verification = (%q, %v), want partial/unverified", got, err)
	}
}

func TestLoadSessionRequiresFirstUniqueMetaHeader(t *testing.T) {
	validMeta := `{"seq":0,"dir":"meta","payload":{"formatVersion":2}}`
	request := `{"seq":1,"dir":"in","payload":{"jsonrpc":"2.0","id":1,"method":"initialize"}}`
	cases := []struct {
		name string
		data string
	}{
		{name: "missing", data: request + "\n"},
		{name: "late", data: request + "\n" + validMeta + "\n"},
		{name: "duplicate", data: validMeta + "\n" + validMeta + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/session.jsonl"
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatalf("write session: %v", err)
			}
			if _, err := LoadSession(path); err == nil {
				t.Fatal("LoadSession succeeded without exactly one first-line meta header")
			}
		})
	}
}

func TestLegacyFormatCannotUseNewResponseIdentityEvidence(t *testing.T) {
	identity := replayTestIdentity(9, "sha256:"+strings.Repeat("a", 64))
	id := jsonrpc.RequestID{Num: 7}
	request, err := json.Marshal(jsonrpc.NewRequest(id, "textDocument/hover", nil))
	if err != nil {
		t.Fatal(err)
	}
	response, err := json.Marshal(jsonrpc.NewResponse(id, json.RawMessage(`null`)))
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{
		Meta: Meta{FormatVersion: legacyFormatVersion, SemanticIdentity: identity},
		Entries: []Entry{
			{Seq: 1, Dir: "identity", Payload: mustJSON(identity)},
			{Seq: 2, Dir: "in", Payload: request},
			{Seq: 3, Dir: "out", Payload: response, SemanticBinding: &SemanticResponseBinding{
				RequestID: id, Kind: SemanticBindingGeneration, Generation: identity.Generation,
				IndexContentDigest: identity.IndexContentDigest,
			}},
		},
	}
	if err := session.VerifyResponseBindings(identity); !errors.Is(err, ErrSemanticResponseBindingUnverified) {
		t.Fatalf("v1 response evidence error = %v, want fail-closed unverified", err)
	}
}

func replayTestIdentity(generation uint64, digest string) *SemanticIdentity {
	return &SemanticIdentity{
		Generation:         generation,
		IndexContentDigest: digest,
		BuildContexts:      map[string]string{"go:workspace": "go:sha256:" + strings.Repeat("b", 32)},
		Tools:              []ToolIdentity{{Name: "go", Path: "C:/tools/go.exe", Version: "go1.26", SHA256: strings.Repeat("c", 64)}},
	}
}

func TestSemanticResponseBindingRoundTripCompletesReplay(t *testing.T) {
	path := t.TempDir() + "/bound.jsonl"
	identity := replayTestIdentity(23, "sha256:"+strings.Repeat("a", 64))
	id := jsonrpc.RequestID{Num: 19}
	request := jsonrpc.NewRequest(id, "textDocument/hover", json.RawMessage(`{"textDocument":{"uri":"file:///w/a.go"}}`))
	response := jsonrpc.NewResponse(id, json.RawMessage(`{"contents":"ok"}`))

	recorder, err := NewRecorder(newFeedTransport(request), path, Meta{}, nil)
	if err != nil {
		t.Fatalf("create recorder: %v", err)
	}
	if _, err := recorder.Read(context.Background()); err != nil {
		t.Fatalf("record request: %v", err)
	}
	if err := recorder.RegisterSemanticIdentity(id, *identity); err != nil {
		t.Fatalf("register recorded generation identity: %v", err)
	}
	if err := recorder.BindSemanticResponse(id, true, identity.Generation, identity.IndexContentDigest); err != nil {
		t.Fatalf("bind recorded response: %v", err)
	}
	if err := recorder.Write(context.Background(), response); err != nil {
		t.Fatalf("record response: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("close recorder: %v", err)
	}

	session, err := LoadSession(path)
	if err != nil {
		t.Fatalf("load recording: %v", err)
	}
	if session.Meta.FormatVersion != FormatVersion {
		t.Fatalf("recorded format = %d, want %d", session.Meta.FormatVersion, FormatVersion)
	}
	if err := session.VerifyResponseBindings(identity); err != nil {
		t.Fatalf("verify recorded bindings: %v", err)
	}
	if !session.hasIdentityEvent() {
		t.Fatal("recording did not persist a response-time identity event")
	}

	player := NewPlayer()
	player.Feed(request)
	player.EndFeed()
	if err := player.RegisterSemanticIdentity(id, *identity); err != nil {
		t.Fatalf("register replay generation identity: %v", err)
	}
	if err := player.BindSemanticResponse(id, true, identity.Generation, identity.IndexContentDigest); err != nil {
		t.Fatalf("bind replayed response: %v", err)
	}
	if err := player.Transport().Write(context.Background(), response); err != nil {
		t.Fatalf("capture replayed response: %v", err)
	}
	status, err := player.CompareSession(session, identity)
	if err != nil || status != ReproductionComplete {
		t.Fatalf("matching response provenance = (%q, %v), want complete", status, err)
	}
}

func TestSemanticResponseBindingRejectsMissingMismatchedAndPrunedProvenance(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	identity := replayTestIdentity(31, digest)
	id := jsonrpc.RequestID{Str: "hover-31", IsStr: true}
	request, _ := json.Marshal(jsonrpc.NewRequest(id, "textDocument/hover", nil))
	response, _ := json.Marshal(jsonrpc.NewResponse(id, json.RawMessage(`{"contents":"ok"}`)))
	base := &Session{Meta: Meta{FormatVersion: FormatVersion, SemanticIdentity: identity}, Entries: []Entry{
		{Seq: 1, Dir: "in", Payload: request},
		{Seq: 2, Dir: "out", Payload: response, SemanticBinding: &SemanticResponseBinding{
			RequestID: id, Kind: SemanticBindingGeneration, Generation: identity.Generation, IndexContentDigest: digest,
		}},
	}}

	tests := []struct {
		name    string
		mutate  func(*Session)
		wantErr error
	}{
		{
			name:    "legacy missing binding",
			mutate:  func(s *Session) { s.Entries[1].SemanticBinding = nil },
			wantErr: ErrSemanticResponseBindingUnverified,
		},
		{
			name: "response binding ID mismatch",
			mutate: func(s *Session) {
				s.Entries[1].SemanticBinding.RequestID = jsonrpc.RequestID{Num: 99}
			},
			wantErr: ErrSemanticResponseBindingMismatch,
		},
		{
			name:    "generation pruned from available state",
			mutate:  func(*Session) {},
			wantErr: ErrSemanticGenerationUnavailable,
		},
		{
			name: "content digest mismatch",
			mutate: func(s *Session) {
				s.Entries[1].SemanticBinding.IndexContentDigest = "sha256:" + strings.Repeat("d", 64)
			},
			wantErr: ErrSemanticIdentityMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			copy := *base
			copy.Entries = append([]Entry(nil), base.Entries...)
			binding := *base.Entries[1].SemanticBinding
			copy.Entries[1].SemanticBinding = &binding
			tt.mutate(&copy)
			available := identity
			if tt.name == "generation pruned from available state" {
				available = replayTestIdentity(identity.Generation+1, digest)
			}
			err := copy.VerifyResponseBindings(available)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("verify response binding error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestLiveSemanticResponseRequiresExplicitNoneAndMatchingTools(t *testing.T) {
	baseIdentity := replayTestIdentity(0, "")
	identity := &SemanticIdentity{Tools: append([]ToolIdentity(nil), baseIdentity.Tools...)}
	id := jsonrpc.RequestID{Str: "live-hover", IsStr: true}
	request, _ := json.Marshal(jsonrpc.NewRequest(id, "textDocument/hover", json.RawMessage(`{"textDocument":{"uri":"file:///w/a.go"}}`)))
	response, _ := json.Marshal(jsonrpc.NewResponse(id, json.RawMessage(`{"contents":"from live backend"}`)))
	session := &Session{Meta: Meta{FormatVersion: FormatVersion}, Entries: []Entry{
		{Seq: 1, Dir: "identity", Payload: mustJSON(identity)},
		{Seq: 2, Dir: "in", Payload: request},
		{Seq: 3, Dir: "out", Payload: response, SemanticBinding: &SemanticResponseBinding{RequestID: id, Kind: SemanticBindingNone, ToolIdentityDigest: toolIdentityDigest(identity.Tools)}},
	}}
	if err := session.VerifyResponseBindings(identity); err != nil {
		t.Fatalf("explicit live-path binding should verify without an index generation: %v", err)
	}

	player := NewPlayer()
	if err := player.RegisterSemanticIdentity(id, *identity); err != nil {
		t.Fatalf("register replay tool identity: %v", err)
	}
	if err := player.BindSemanticResponse(id, false, 0, ""); err != nil {
		t.Fatalf("bind live replay response: %v", err)
	}
	if err := player.Transport().Write(context.Background(), jsonrpc.NewResponse(id, json.RawMessage(`{"contents":"from live backend"}`))); err != nil {
		t.Fatalf("write live replay response: %v", err)
	}
	status, err := player.CompareSession(session, identity)
	if err != nil || status != ReproductionComplete {
		t.Fatalf("matching live-tool provenance = (%q, %v), want complete", status, err)
	}

	changedTools := &SemanticIdentity{Tools: append([]ToolIdentity(nil), identity.Tools...)}
	changedTools.Tools[0].SHA256 = strings.Repeat("f", 64)
	status, err = player.CompareSession(session, changedTools)
	if status != ReproductionPartialUnverified || !errors.Is(err, ErrSemanticIdentityMismatch) {
		t.Fatalf("changed live backend identity = (%q, %v), want identity mismatch", status, err)
	}
}

func TestPinnedSemanticGenerationMustBeAvailableForIdentityVerification(t *testing.T) {
	path := t.TempDir() + "/pinned.jsonl"
	recorded := SemanticIdentity{
		Generation:         17,
		IndexContentDigest: "sha256:" + strings.Repeat("a", 64),
		BuildContexts: map[string]string{
			"go:workspace": "go:sha256:" + strings.Repeat("b", 32),
		},
		Tools: []ToolIdentity{
			{Name: "go", Path: "C:/tools/go.exe", Version: "go1.26", SHA256: strings.Repeat("c", 64)},
			{Name: "extractor", Path: "C:/tools/extractor.exe", Version: "v2", SHA256: strings.Repeat("d", 64)},
		},
	}
	recorder, err := NewRecorder(newFeedTransport(), path, Meta{
		FormatVersion:    FormatVersion,
		ConfigHash:       "pinned-config",
		SemanticIdentity: &recorded,
	}, nil)
	if err != nil {
		t.Fatalf("create pinned recording: %v", err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatalf("close pinned recording: %v", err)
	}
	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("load pinned session: %v", err)
	}
	if loaded.Meta.SemanticIdentity == nil || loaded.Meta.SemanticIdentity.Generation != recorded.Generation {
		t.Fatalf("semantic generation was not preserved: %+v", loaded.Meta.SemanticIdentity)
	}
	if got := loaded.SemanticReproductionStatus(); got != ReproductionIdentityPinned {
		t.Fatalf("pinned semantic status = %q, want %q", got, ReproductionIdentityPinned)
	}

	missing := *loaded.Meta.SemanticIdentity
	missing.Generation = recorded.Generation + 1 // another generation is present, but the recorded one is missing
	got, err := loaded.VerifySemanticReproduction(&missing)
	if got != ReproductionPartialUnverified || !errors.Is(err, ErrSemanticGenerationUnavailable) {
		t.Fatalf("missing generation verification = (%q, %v), want partial/unavailable", got, err)
	}
	got, err = NewPlayer().CompareSession(loaded, &missing)
	if got != ReproductionPartialUnverified || !errors.Is(err, ErrSemanticGenerationUnavailable) {
		t.Fatalf("missing generation comparison = (%q, %v), want partial/refused", got, err)
	}

	available := *loaded.Meta.SemanticIdentity
	available.Tools = []ToolIdentity{recorded.Tools[1], recorded.Tools[0]}
	got, err = loaded.VerifySemanticReproduction(&available)
	if err != nil || got != ReproductionIdentityVerified {
		t.Fatalf("matching semantic identity = (%q, %v), want identity-verified", got, err)
	}
	got, err = NewPlayer().CompareSession(loaded, &available)
	if got != ReproductionComplete || err != nil {
		t.Fatalf("empty nonsemantic recording = (%q, %v), want complete", got, err)
	}

	available.IndexContentDigest = "sha256:" + strings.Repeat("e", 64)
	got, err = loaded.VerifySemanticReproduction(&available)
	if got != ReproductionPartialUnverified || !errors.Is(err, ErrSemanticIdentityMismatch) {
		t.Fatalf("content mismatch verification = (%q, %v), want partial/mismatch", got, err)
	}
}

func TestCompareSessionDoesNotClaimCompleteWithoutResponseGenerationBinding(t *testing.T) {
	identity := &SemanticIdentity{
		Generation:         3,
		IndexContentDigest: "sha256:" + strings.Repeat("a", 64),
		BuildContexts:      map[string]string{"go:workspace": "go:sha256:" + strings.Repeat("b", 32)},
		Tools:              []ToolIdentity{{Name: "go", Path: "C:/tools/go.exe", Version: "go1.26", SHA256: strings.Repeat("c", 64)}},
	}
	id := jsonrpc.RequestID{Num: 9}
	request := jsonrpc.NewRequest(id, "textDocument/hover", json.RawMessage(`{"textDocument":{"uri":"file:///w/a.go"}}`))
	response := jsonrpc.NewResponse(id, json.RawMessage(`{"contents":"ok"}`))
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	session := &Session{
		Meta: Meta{FormatVersion: FormatVersion, SemanticIdentity: identity},
		Entries: []Entry{
			{Seq: 1, Dir: "in", Payload: mustJSON(request)},
			{Seq: 2, Dir: "out", Payload: payload},
		},
	}
	player := NewPlayer()
	if err := player.Transport().Write(context.Background(), response); err != nil {
		t.Fatalf("write replayed response: %v", err)
	}

	status, err := player.CompareSession(session, identity)
	if status != ReproductionPartialUnverified || !errors.Is(err, ErrSemanticResponseBindingUnverified) {
		t.Fatalf("matching response without semantic binding = (%q, %v), want partial/unverified", status, err)
	}
}

func TestCompareSessionAllowsLegacyNonsemanticResponseWithoutBinding(t *testing.T) {
	identity := replayTestIdentity(41, "sha256:"+strings.Repeat("a", 64))
	id := jsonrpc.RequestID{Num: 1}
	request := jsonrpc.NewRequest(id, "initialize", json.RawMessage(`{"capabilities":{}}`))
	response := jsonrpc.NewResponse(id, json.RawMessage(`{"capabilities":{}}`))
	requestPayload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	responsePayload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{Meta: Meta{FormatVersion: FormatVersion, SemanticIdentity: identity}, Entries: []Entry{
		{Seq: 1, Dir: "in", Payload: requestPayload},
		{Seq: 2, Dir: "out", Payload: responsePayload},
	}}
	player := NewPlayer()
	if err := player.Transport().Write(context.Background(), response); err != nil {
		t.Fatal(err)
	}
	status, err := player.CompareSession(session, identity)
	if err != nil || status != ReproductionComplete {
		t.Fatalf("legacy nonsemantic response = (%q, %v), want complete", status, err)
	}
}

// TestRecorder_WriteFailureSurfaces: a recording write failure used to be
// swallowed by `_ = r.log(...)`. It must now be retained and exposed via
// Err() while the live session keeps flowing untouched (pass-through intact,
// later writes still attempted, first error never overwritten).
func TestRecorder_WriteFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	// Payload larger than the bufio.Writer buffer forces an actual write to
	// the (closed) file instead of lingering in the buffer.
	bulk := json.RawMessage(`{"pad":"` + strings.Repeat("x", 16*1024) + `"}`)
	ft := newFeedTransport(jsonrpc.NewRequest(jsonrpc.RequestID{Num: 7}, "textDocument/hover", bulk))
	rec, err := NewRecorder(ft, dir+"/session.jsonl", Meta{}, nil)
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	defer rec.Close()

	// Sabotage the sink: close the file under the recorder so every real disk
	// write fails from here on.
	if err := rec.f.Close(); err != nil {
		t.Fatalf("sabotage close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	gotMsg, readErr := rec.Read(ctx)
	if gotMsg == nil || readErr != nil {
		t.Fatalf("Read pass-through broken: msg=%v err=%v", gotMsg, readErr)
	}
	if rec.Err() == nil {
		t.Fatal("Err() is nil after failed recording write")
	}
	first := rec.Err()

	if err := rec.Write(ctx, gotMsg); err != nil {
		t.Fatalf("Write pass-through broken: %v", err)
	}
	if ft.written() != 1 {
		t.Fatalf("inner transport writes = %d, want 1", ft.written())
	}

	// Later failures must still be attempted but must not overwrite the first.
	rec.log("out", raw(gotMsg))
	if rec.Err() != first {
		t.Fatal("first recorded error was overwritten by a later failure")
	}
}
