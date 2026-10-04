package server

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// hoverCountingBackend counts hover invocations to prove memo semantics.
type hoverCountingBackend struct {
	mockBackend
	hovers int64
}

func (b *hoverCountingBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.hovers++
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: "hit"},
	}, nil
}

type contentHoverBackend struct {
	mockBackend
	calls atomic.Int64
}

func (b *contentHoverBackend) Hover(_ context.Context, req languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.calls.Add(1)
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: string(req.Content)},
	}, nil
}

type controlledSemanticFingerprintBackend struct {
	mockBackend
	fingerprint string
}

func (b *controlledSemanticFingerprintBackend) SemanticInputFingerprint(context.Context, *snapshot.Snapshot, string) (string, error) {
	return b.fingerprint, nil
}

type epochHoverBackend struct {
	hoverCountingBackend
	epoch uint64
}

func (b *epochHoverBackend) SupervisorEpoch() uint64 { return b.epoch }

type sharedComputeHoverBackend struct {
	mockBackend
	started       chan context.Context
	release       chan struct{}
	calls         atomic.Int64
	leaseFinishes atomic.Int64
	gotLease      atomic.Bool
}

type panicThenRecoveringHoverBackend struct {
	mockBackend
	calls         atomic.Int64
	leaseFinishes atomic.Int64
	leaseActive   atomic.Bool
}

func (b *panicThenRecoveringHoverBackend) BeginWorkspaceSnapshot(ctx context.Context, _ languages.WorkspaceSnapshot) (context.Context, func() error, error) {
	if !b.leaseActive.CompareAndSwap(false, true) {
		return nil, nil, context.Canceled
	}
	return ctx, func() error {
		b.leaseFinishes.Add(1)
		b.leaseActive.Store(false)
		return nil
	}, nil
}

func (*panicThenRecoveringHoverBackend) WorkspaceSnapshotGeneration() uint64 { return 1 }

func (b *panicThenRecoveringHoverBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	if b.calls.Add(1) == 1 {
		panic("backend panic for lease cleanup regression")
	}
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: "recovered"},
	}, nil
}

func (b *sharedComputeHoverBackend) BeginWorkspaceSnapshot(ctx context.Context, _ languages.WorkspaceSnapshot) (context.Context, func() error, error) {
	leaseCtx := context.WithValue(ctx, workspaceLeaseContextKey{}, true)
	return leaseCtx, func() error {
		b.leaseFinishes.Add(1)
		return nil
	}, nil
}

func (*sharedComputeHoverBackend) WorkspaceSnapshotGeneration() uint64 { return 1 }

func (b *sharedComputeHoverBackend) Hover(ctx context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.calls.Add(1)
	b.gotLease.Store(ctx.Value(workspaceLeaseContextKey{}) == true)
	b.started <- ctx
	select {
	case <-ctx.Done():
		return identity.SemanticResult[*languages.HoverResult]{Status: identity.ResultUnknown}, nil
	case <-b.release:
		return identity.SemanticResult[*languages.HoverResult]{
			Status: identity.ResultExact,
			Value:  &languages.HoverResult{Contents: "shared result"},
		}, nil
	}
}

// failingHoverBackend returns a timeout-kind error for the first N hovers.
type failingHoverBackend struct {
	mockBackend
	failUntil int64
	calls     int64
}

func (b *failingHoverBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.calls++
	if b.calls <= b.failUntil {
		return identity.SemanticResult[*languages.HoverResult]{},
			ierrors.New(ierrors.ErrTimeout, "go", "timed out")
	}
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: "recovered"},
	}, nil
}

// staleThenRecoveringHoverBackend reports a nested snapshot-generation race
// once, then serves a fresh result for the same semantic query key.
type staleThenRecoveringHoverBackend struct {
	mockBackend
	calls int64
}

func (b *staleThenRecoveringHoverBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.calls++
	if b.calls == 1 {
		return identity.SemanticResult[*languages.HoverResult]{},
			ierrors.New(ierrors.ErrContentModified, "nested.request", "document generation is no longer current")
	}
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: "recovered"},
	}, nil
}

// TestJ_EngineMemoizesSemanticReads pins the §J wiring end to end.
func TestJ_EngineMemoizesSemanticReads(t *testing.T) {
	const uri = "file:///w/main.go"

	t.Run("same revision computes once", func(t *testing.T) {
		s := New(DefaultConfig())
		be := &hoverCountingBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
		s.RegisterBackend("go", be)
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		s.publishSnapshot()

		for i := 0; i < 3; i++ {
			resp := dispatchHoverRaw(s, uri)
			if resp == nil || resp.Error != nil {
				t.Fatalf("hover %d failed: %+v", i, resp)
			}
		}
		if n := be.hovers; n != 1 {
			t.Fatalf("backend hovered %d times for same revision, want 1", n)
		}
	})

	t.Run("revision advance recomputes", func(t *testing.T) {
		s := New(DefaultConfig())
		be := &hoverCountingBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
		s.RegisterBackend("go", be)
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		s.publishSnapshot()

		dispatchHoverOK(t, s, uri)
		s.vfs.Update(uri, 2, []byte("package main // edited\n"))
		s.publishSnapshot()
		dispatchHoverOK(t, s, uri)

		if n := be.hovers; n != 2 {
			t.Fatalf("backend hovered %d times across revisions, want 2", n)
		}
	})

	t.Run("transient failure is not memoized then recovers", func(t *testing.T) {
		s := New(DefaultConfig())
		be := &failingHoverBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}, failUntil: 1}
		s.RegisterBackend("go", be)
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		s.publishSnapshot()

		resp1 := dispatchHoverRaw(s, uri) // fails with timeout kind
		if resp1 == nil || resp1.Error == nil {
			t.Fatal("expected first hover to fail")
		}
		var stats = s.queries.Stats()
		if stats.Computations != 1 {
			t.Fatalf("unexpected computation count %d", stats.Computations)
		}
		resp2 := dispatchHoverRaw(s, uri) // retries and succeeds
		if resp2 == nil || resp2.Error != nil {
			t.Fatalf("second hover should recover: %+v", resp2)
		}
		if be.calls != 2 {
			t.Fatalf("backend calls = %d, want 2 (failure must not be cached)", be.calls)
		}
	})

	t.Run("content modified failure is not memoized then recovers", func(t *testing.T) {
		s := New(DefaultConfig())
		be := &staleThenRecoveringHoverBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
		s.RegisterBackend("go", be)
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		s.publishSnapshot()

		resp1 := dispatchHoverRaw(s, uri)
		if resp1 == nil || resp1.Error == nil || resp1.Error.Code != jsonrpc.ContentModified {
			t.Fatalf("first hover error = %+v, want ContentModified", resp1)
		}

		resp2 := dispatchHoverRaw(s, uri)
		if resp2 == nil || resp2.Error != nil {
			t.Fatalf("same-key hover should recover after content modification: %+v", resp2)
		}
		if be.calls != 2 {
			t.Fatalf("backend calls = %d, want 2 (content-modified failure must not be cached)", be.calls)
		}

		resp3 := dispatchHoverRaw(s, uri)
		if resp3 == nil || resp3.Error != nil {
			t.Fatalf("memoized recovered hover failed: %+v", resp3)
		}
		if be.calls != 2 {
			t.Fatalf("backend calls after recovery = %d, want 2 (success should remain memoized)", be.calls)
		}
	})
}

func TestJ_FingerprintChangeDuringSemanticComputeDoesNotPublishExact(t *testing.T) {
	s := New(DefaultConfig())
	be := &controlledSemanticFingerprintBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		fingerprint: "before",
	}
	computations := 0
	compute := func(context.Context) (identity.SemanticResult[int], error) {
		computations++
		if computations == 1 {
			be.fingerprint = "changed-during-compute"
		}
		return identity.NewExactResult(computations, nil), nil
	}
	call := func() (identity.SemanticResult[int], error) {
		return semanticViaEngine[int](s, context.Background(), be, "hover", "file:///w/main.go", 1, "build", "options", compute)
	}

	if _, err := call(); !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("first result error = %v, want content modified", err)
	}
	if computations != 1 {
		t.Fatalf("compute calls after first request = %d, want 1", computations)
	}
	be.fingerprint = "before"
	second, err := call()
	if err != nil || second.Status != identity.ResultExact || second.Value != 2 {
		t.Fatalf("retry result = %+v, %v; want freshly computed exact value 2", second, err)
	}
	if computations != 2 {
		t.Fatalf("compute calls after retry = %d, want 2; stale Exact result was probably published", computations)
	}
}

func TestJ_WarmMemoRecomputesWhenCurrentInputFingerprintChanges(t *testing.T) {
	s := New(DefaultConfig())
	be := &controlledSemanticFingerprintBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		fingerprint: "source-v1",
	}
	computations := 0
	compute := func(context.Context) (identity.SemanticResult[int], error) {
		computations++
		return identity.NewExactResult(computations, nil), nil
	}
	call := func() (identity.SemanticResult[int], error) {
		return semanticViaEngine[int](s, context.Background(), be, "hover", "file:///w/main.go", 1, "build", "options", compute)
	}

	first, err := call()
	if err != nil || first.Value != 1 {
		t.Fatalf("cold result = %+v, %v; want 1", first, err)
	}
	warm, err := call()
	if err != nil || warm.Value != 1 || computations != 1 {
		t.Fatalf("warm result = %+v, %v after %d computes; want cached 1 after one compute", warm, err, computations)
	}

	be.fingerprint = "source-v2"
	changed, err := call()
	if err != nil || changed.Status != identity.ResultExact || changed.Value != 2 || computations != 2 {
		t.Fatalf("changed-fingerprint result = %+v, %v after %d computes; want fresh exact 2 after two computes", changed, err, computations)
	}
}

func TestJ_CanonicalURIAliasesShareSemanticMemoEntry(t *testing.T) {
	const alias, canonical = "file:///w/%6Dain.go", "file:///w/main.go"
	if canonicalDocumentURI(alias) != canonicalDocumentURI(canonical) {
		t.Fatalf("test URIs are not aliases: %q != %q", canonicalDocumentURI(alias), canonicalDocumentURI(canonical))
	}
	s := New(DefaultConfig())
	be := &controlledSemanticFingerprintBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		fingerprint: "source-v1",
	}
	computations := 0
	call := func(documentURI string) (identity.SemanticResult[int], error) {
		return semanticViaEngine[int](s, context.Background(), be, "hover", documentURI, 1, "build", "options", func(context.Context) (identity.SemanticResult[int], error) {
			computations++
			return identity.NewExactResult(computations, nil), nil
		})
	}
	if _, err := call(alias); err != nil {
		t.Fatalf("alias request: %v", err)
	}
	warm, err := call(canonical)
	if err != nil || warm.Value != 1 || computations != 1 {
		t.Fatalf("canonical alias result = %+v, %v after %d computes; want shared memo value 1", warm, err, computations)
	}
}

func TestJ_BackendEpochSeparatesSemanticCacheEntries(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	be := &epochHoverBackend{hoverCountingBackend: hoverCountingBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
	}, epoch: 1}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
	s.publishSnapshot()

	dispatchHoverOK(t, s, uri)
	be.epoch = 2
	dispatchHoverOK(t, s, uri)
	dispatchHoverOK(t, s, uri)
	if got := be.hovers; got != 2 {
		t.Fatalf("backend hover calls = %d, want one computation per backend epoch", got)
	}
}

func TestJ1_DifferentSnapshotsCannotAliasOnSameRevision(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	be := &contentHoverBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("version one"), 0)
	s.publishSnapshot()
	firstSnapshot := s.snapMgr.Current()

	request := func(snap *snapshot.Snapshot, id int64) *jsonrpc.Message {
		params := json.RawMessage(`{"textDocument":{"uri":"` + uri + `"},"position":{"line":0,"character":0}}`)
		return s.Dispatcher().Dispatch(withSnapshot(context.Background(), snap), jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, "textDocument/hover", params))
	}
	if resp := request(firstSnapshot, 1); resp == nil || resp.Error != nil {
		t.Fatalf("first snapshot hover = %+v", resp)
	}

	secondSnapshot := snapshot.New(firstSnapshot.ID().WorkspaceID, firstSnapshot.ID().Revision, map[string]snapshot.DocumentSnapshot{
		uri: {URI: uri, LanguageID: "go", Version: 2, Content: []byte("version two")},
	})
	if secondSnapshot.InstanceID() == firstSnapshot.InstanceID() {
		t.Fatal("separate snapshot publications have the same instance identity")
	}
	resp := request(secondSnapshot, 2)
	if resp == nil || resp.Error != nil {
		t.Fatalf("second snapshot hover = %+v", resp)
	}
	var hover struct {
		Contents struct {
			Value string `json:"value"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(resp.Result, &hover); err != nil {
		t.Fatalf("decode second snapshot hover: %v", err)
	}
	if hover.Contents.Value != "version two" || be.calls.Load() != 2 {
		t.Fatalf("second snapshot served %q after %d backend calls, want version two / 2", hover.Contents.Value, be.calls.Load())
	}
}

func TestJ6_ServerLeaderCancelKeepsSharedSemanticComputeAndLease(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	be := &sharedComputeHoverBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".go"}},
		started:     make(chan context.Context, 1), release: make(chan struct{}),
	}
	defer func() {
		select {
		case <-be.release:
		default:
			close(be.release)
		}
	}()
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
	s.publishSnapshot()
	snapshot := s.snapMgr.Current()
	params := json.RawMessage(`{"textDocument":{"uri":"` + uri + `"},"position":{"line":0,"character":0}}`)
	request := func(ctx context.Context, id int64) *jsonrpc.Message {
		return s.Dispatcher().Dispatch(withSnapshot(ctx, snapshot), jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, "textDocument/hover", params))
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leaderDone := make(chan *jsonrpc.Message, 1)
	go func() { leaderDone <- request(leaderCtx, 1) }()
	backendCtx := <-be.started

	waiterDone := make(chan *jsonrpc.Message, 1)
	go func() { waiterDone <- request(context.Background(), 2) }()
	waitFor(t, func() bool { return s.queries.InFlightWaiters() == 2 })

	cancelLeader()
	select {
	case resp := <-leaderDone:
		if resp == nil || resp.Error == nil {
			t.Fatalf("canceled leader response = %+v, want cancellation error", resp)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled leader did not return independently")
	}
	if be.leaseFinishes.Load() != 0 {
		t.Fatal("shared backend snapshot lease ended when only the leader canceled")
	}
	select {
	case <-backendCtx.Done():
		t.Fatal("leader cancellation reached the backend computation while a waiter remained")
	default:
	}

	close(be.release)
	select {
	case resp := <-waiterDone:
		if resp == nil || resp.Error != nil {
			t.Fatalf("remaining waiter response = %+v", resp)
		}
		var hover struct {
			Contents struct {
				Value string `json:"value"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(resp.Result, &hover); err != nil {
			t.Fatalf("decode remaining waiter hover: %v", err)
		}
		if hover.Contents.Value != "shared result" {
			t.Fatalf("remaining waiter hover = %q, want shared result", hover.Contents.Value)
		}
	case <-time.After(time.Second):
		t.Fatal("remaining waiter did not receive the shared result")
	}
	if be.calls.Load() != 1 || !be.gotLease.Load() || be.leaseFinishes.Load() != 1 {
		t.Fatalf("compute calls=%d gotLease=%v leaseFinishes=%d, want 1/true/1", be.calls.Load(), be.gotLease.Load(), be.leaseFinishes.Load())
	}
}

func TestJ6_BackendPanicReleasesSharedSemanticLeaseForRetry(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	be := &panicThenRecoveringHoverBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
	s.publishSnapshot()

	first := dispatchHoverRaw(s, uri)
	if first == nil || first.Error == nil {
		t.Fatalf("first hover response = %+v, want recovered panic error", first)
	}
	if be.leaseActive.Load() || be.leaseFinishes.Load() != 1 {
		t.Fatalf("lease active=%v finishes=%d after panic, want false/1", be.leaseActive.Load(), be.leaseFinishes.Load())
	}

	second := dispatchHoverRaw(s, uri)
	if second == nil || second.Error != nil {
		t.Fatalf("same-key retry after panic = %+v, want success", second)
	}
	if be.calls.Load() != 2 || be.leaseActive.Load() || be.leaseFinishes.Load() != 2 {
		t.Fatalf("calls=%d lease active=%v finishes=%d after retry, want 2/false/2", be.calls.Load(), be.leaseActive.Load(), be.leaseFinishes.Load())
	}
}

func dispatchHoverOK(t *testing.T, s *Server, uri string) {
	t.Helper()
	resp := dispatchHoverRaw(s, uri)
	if resp == nil || resp.Error != nil {
		t.Fatalf("hover failed: %+v", resp)
	}
}

func dispatchHoverRaw(s *Server, uri string) *jsonrpc.Message {
	return s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 42}, "textDocument/hover",
		json.RawMessage(`{"textDocument":{"uri":"`+uri+`"},"position":{"line":0,"character":0}}`)))
}

func TestI12_ValidateEditSet(t *testing.T) {
	mk := func(uri string, sl, sc, el, ec uint32) languages.TextEdit {
		return languages.TextEdit{URI: uri, StartLine: sl, StartChar: sc, EndLine: el, EndChar: ec}
	}
	t.Run("empty and single pass", func(t *testing.T) {
		if err := ValidateEditSet(nil); err != nil {
			t.Fatal(err)
		}
		if err := ValidateEditSet([]languages.TextEdit{mk("a.go", 0, 0, 1, 0)}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("same-file overlap refused", func(t *testing.T) {
		err := ValidateEditSet([]languages.TextEdit{
			mk("a.go", 2, 0, 5, 10),
			mk("a.go", 5, 5, 7, 0), // overlaps at line 5
		})
		if err == nil || !contains(err.Error(), "overlapping") {
			t.Fatalf("expected overlap refusal, got %v", err)
		}
	})
	t.Run("canonical URI aliases overlap refused", func(t *testing.T) {
		err := ValidateEditSet([]languages.TextEdit{
			mk("file:///w/%6Dain.go", 0, 1, 0, 6),
			mk("file:///w/main.go", 0, 4, 0, 8),
		})
		if err == nil || !contains(err.Error(), "overlapping") {
			t.Fatalf("expected overlapping edits through URI aliases to be refused, got %v", err)
		}
	})
	t.Run("adjacent same-file edits pass", func(t *testing.T) {
		if err := ValidateEditSet([]languages.TextEdit{
			mk("a.go", 2, 0, 3, 0),
			mk("a.go", 3, 0, 4, 0), // boundary-touching is not overlapping
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("same-position insertions refused", func(t *testing.T) {
		err := ValidateEditSet([]languages.TextEdit{
			mk("a.go", 2, 4, 2, 4),
			mk("a.go", 2, 4, 2, 4),
		})
		if err == nil || !contains(err.Error(), "overlapping") {
			t.Fatalf("expected coincident insertions to be refused, got %v", err)
		}
	})
	t.Run("malformed range refused", func(t *testing.T) {
		if err := ValidateEditSet([]languages.TextEdit{mk("a.go", 3, 0, 2, 0)}); err == nil {
			t.Fatal("expected reversed range to be refused")
		}
	})
	t.Run("missing URI refused", func(t *testing.T) {
		if err := ValidateEditSet([]languages.TextEdit{mk("", 0, 0, 0, 1)}); err == nil {
			t.Fatal("expected empty URI to be refused")
		}
	})
	t.Run("cross-file overlap impossible", func(t *testing.T) {
		if err := ValidateEditSet([]languages.TextEdit{
			mk("a.go", 2, 0, 5, 10),
			mk("b.go", 2, 0, 5, 10),
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func contains(s, sub string) bool {
	return len(sub) > 0 && indexOfBytes([]byte(s), []byte(sub)) >= 0
}

func indexOfBytes(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
