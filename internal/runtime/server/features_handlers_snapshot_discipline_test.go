package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// disciplineSpyBackend implements the three optional capabilities
// (§I16/§I20/§I22) plus the external-change and workspace-snapshot sync
// interfaces, recording the phase order around each capability call.
type disciplineSpyBackend struct {
	mockBackend

	mu         sync.Mutex
	notifySeen int // NotifySourceChanges deliveries before the latest capability call
	leaseSeen  int // BeginWorkspaceSnapshot leases before the latest capability call

	content map[string]string // method -> content the capability call received
	rev     map[string]uint64 // method -> snapshot revision the capability call received
	// flushed / leased record whether the sync phase had run at least once
	// before that method's capability call.
	flushed map[string]bool
	leased  map[string]bool
}

func (b *disciplineSpyBackend) record(method string, content []byte, rev uint64, notify, lease int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.content == nil {
		b.content = map[string]string{}
		b.rev = map[string]uint64{}
		b.flushed = map[string]bool{}
		b.leased = map[string]bool{}
	}
	b.content[method] = string(content)
	b.rev[method] = rev
	b.flushed[method] = notify > 0
	b.leased[method] = lease > 0
}

func (b *disciplineSpyBackend) SignatureHelp(_ context.Context, req languages.SignatureHelpRequest) (identity.SemanticResult[*languages.SignatureHelpResult], error) {
	b.mu.Lock()
	notify, lease := b.notifySeen, b.leaseSeen
	b.mu.Unlock()
	b.record("textDocument/signatureHelp", req.Content, req.SnapshotRev, notify, lease)
	return identity.SemanticResult[*languages.SignatureHelpResult]{
		Status: identity.ResultPartial,
		Value:  &languages.SignatureHelpResult{Signatures: []languages.SignatureInformation{{Label: "demo(a int)"}}},
	}, nil
}

func (b *disciplineSpyBackend) Formatting(_ context.Context, req languages.FormattingRequest) ([]languages.TextEdit, error) {
	b.mu.Lock()
	notify, lease := b.notifySeen, b.leaseSeen
	b.mu.Unlock()
	b.record("textDocument/formatting", req.Content, 0, notify, lease)
	return nil, nil
}

func (b *disciplineSpyBackend) InlayHints(_ context.Context, req languages.InlayHintRequest) ([]languages.InlayHint, error) {
	b.mu.Lock()
	notify, lease := b.notifySeen, b.leaseSeen
	b.mu.Unlock()
	b.record("textDocument/inlayHint", req.Content, req.SnapshotRev, notify, lease)
	return nil, nil
}

func (b *disciplineSpyBackend) NotifySourceChanges(_ context.Context, _ []languages.SourceChange) error {
	b.mu.Lock()
	b.notifySeen++
	b.mu.Unlock()
	return nil
}

func (b *disciplineSpyBackend) BeginWorkspaceSnapshot(ctx context.Context, _ languages.WorkspaceSnapshot) (context.Context, func() error, error) {
	b.mu.Lock()
	b.leaseSeen++
	b.mu.Unlock()
	return ctx, func() error { return nil }, nil
}

func (b *disciplineSpyBackend) WorkspaceSnapshotGeneration() uint64 { return 0 }

// TestOptionalCapabilitiesFollowSnapshotDiscipline pins bug round 23 A /
// BUG-3: handleSignatureHelp, handleFormatting and handleInlayHints read
// s.vfs.Content and s.currentRevision() as two independent reads and never
// consumed the dispatcher-captured snapshot, never flushed the queued
// external-source batch, and never took a BeginWorkspaceSnapshot lease —
// breaking the one-request-one-snapshot discipline (INV-SNAPSHOT-002) that
// every other semantic feature follows through dispatchSemanticRequest.
//
// The scenario freezes the exact interleaving the two-read window exposes:
// a snapshot is published (rev 2, content "A-content"), an external change is
// queued for the backend, and the live VFS is then edited to "B-content"
// without publishing. The capability call must
//  1. see the queued external batch delivered first (flush-before-read),
//  2. hold a workspace snapshot lease (lease-before-read),
//  3. receive the captured snapshot's content at the captured revision —
//     "A-content" @ rev 2, never the un-published live VFS text.
func TestOptionalCapabilitiesFollowSnapshotDiscipline(t *testing.T) {
	const uri = "file:///w/discipline.go"
	ctx := context.Background()

	for _, method := range []string{
		"textDocument/signatureHelp",
		"textDocument/formatting",
		"textDocument/inlayHint",
	} {
		t.Run(method, func(t *testing.T) {
			s := New(DefaultConfig())
			s.mu.Lock()
			s.state = StateRunning
			s.mu.Unlock()
			spy := &disciplineSpyBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
			s.RegisterBackend("go", spy)

			s.vfs.Open(uri, "go", 1, []byte("A-content"), 0)
			s.publishSnapshot() // rev 1

			// Queue an external change for this backend: the flush discipline
			// requires it to be delivered before any child-backed read.
			if err := s.applyExternalSourceChanges(ctx, []languages.SourceChange{
				{URI: uri, Kind: languages.SourceChangeCreated},
			}); err != nil {
				t.Fatalf("queue external change: %v", err)
			}

			// Cross-generation window: the live VFS now differs from the
			// published snapshot. INV-SNAPSHOT-002 says the request must serve
			// the captured generation, not this un-published text.
			s.vfs.Update(uri, 2, []byte("B-content"))

			params := map[string]any{"textDocument": map[string]any{"uri": uri}}
			switch method {
			case "textDocument/signatureHelp":
				params["position"] = map[string]any{"line": 0, "character": 0}
			case "textDocument/formatting":
				params["options"] = map[string]any{"tabSize": 4, "insertSpaces": true}
			case "textDocument/inlayHint":
				params["range"] = map[string]any{
					"start": map[string]any{"line": 0, "character": 0},
					"end":   map[string]any{"line": 1, "character": 0},
				}
			}
			rawParams, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			resp := s.Dispatcher().Dispatch(ctx, jsonrpc.NewRequest(
				jsonrpc.RequestID{Num: 1}, method, rawParams))
			if resp == nil || resp.Error != nil {
				t.Fatalf("%s failed: %+v", method, resp)
			}

			spy.mu.Lock()
			flushed, leased := spy.flushed[method], spy.leased[method]
			content, rev := spy.content[method], spy.rev[method]
			spy.mu.Unlock()

			if !flushed {
				t.Errorf("%s: backend read served while the queued external-source batch was never flushed (flush-before-read violated)", method)
			}
			if !leased {
				t.Errorf("%s: backend read served without a BeginWorkspaceSnapshot lease (INV-SNAPSHOT-002)", method)
			}
			if method != "textDocument/formatting" {
				// rev 2 = published snapshot after the external-change invalidation.
				if rev != 2 {
					t.Errorf("%s: SnapshotRev = %d, want 2 (the captured generation)", method, rev)
				}
			}
			if !strings.Contains(content, "A-content") || strings.Contains(content, "B-content") {
				t.Errorf("%s: backend received cross-generation content %q; want the captured snapshot's \"A-content\"", method, content)
			}
		})
	}
}
