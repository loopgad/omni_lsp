package ccls

// round23 C: a workspace/symbol reply that failed to decode used to fall
// through to `return syms, nil` — a broken child reply was presented as a
// successful empty result, while the same bridge surfaces an explicit error
// for document symbols. This pin reproduces the verifier's scenario: a
// well-formed JSON reply that is not an array.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
)

func TestWorkspaceSymbolsDecodeFailureIsSurfaced(t *testing.T) {
	b, stdin, child := newCclsProtocolBackend(t)
	type workspaceSymbolsReply struct {
		syms []languages.WorkspaceSymbol
		err  error
	}
	got := make(chan workspaceSymbolsReply, 1)
	go func() {
		syms, err := b.WorkspaceSymbols(context.Background(), languages.WorkspaceSymbolRequest{Query: "Target"})
		got <- workspaceSymbolsReply{syms: syms, err: err}
	}()
	replyToCclsRequest(t, stdin.frames, child, "workspace/symbol", "harmless")
	select {
	case response := <-got:
		if response.err == nil {
			t.Fatalf("decode-failure workspace/symbol reply = %d syms, err=<nil>; want an explicit decode error", len(response.syms))
		}
		if !strings.Contains(response.err.Error(), "workspace symbol decode") {
			t.Fatalf("err = %v, want the workspace symbol decode wording", response.err)
		}
		if len(response.syms) != 0 {
			t.Fatalf("syms = %d, want empty on error", len(response.syms))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workspace/symbol request did not complete after its reply")
	}
}
