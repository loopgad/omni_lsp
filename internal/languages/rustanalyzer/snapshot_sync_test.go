package rustanalyzer

import (
	"context"
	"testing"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

func TestHoverSurfacesBackendErrorsToBoundedProbe(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	go serve(stdin, pw, map[string]any{})

	result, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: "file:///workspace/main.rs", Content: []byte("fn known() {}\n"), SnapshotRev: 1,
	})
	if err == nil {
		t.Fatal("backend JSON-RPC error was hidden from bounded semantic probe")
	}
	if result.Status != identity.ResultUnknown || result.Value != nil {
		t.Fatalf("backend error returned a usable semantic value: %+v", result)
	}
}

func TestHoverRejectsOlderRevisionBeforeChildRequest(t *testing.T) {
	b, stdin, _ := newTestBackend(t)
	const uri = "file:///workspace/main.rs"

	if err := b.didOpen(uri, []byte("fn newer() {}\n"), 2); err != nil {
		t.Fatalf("initial document sync: %v", err)
	}
	select {
	case <-stdin.frames:
	case <-time.After(time.Second):
		t.Fatal("initial document sync was not sent")
	}

	result, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: uri, Content: []byte("fn older() {}\n"), SnapshotRev: 1,
	})
	if !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("stale hover error = %v, want ErrContentModified", err)
	}
	if result.Status == identity.ResultExact || result.Value != nil {
		t.Fatalf("stale hover returned a usable result: %+v", result)
	}
	select {
	case frame := <-stdin.frames:
		t.Fatalf("stale hover sent child frame %q", frameBody(frame))
	case <-time.After(200 * time.Millisecond):
	}
}
