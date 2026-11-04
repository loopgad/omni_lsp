package typescript

// round23 C: a workspace/symbol reply that failed to decode used to fall
// through to `return syms, nil` — a broken child reply was presented as a
// successful empty result, while the same bridge surfaces an explicit error
// for document symbols. This pin reproduces the verifier's scenario: a
// well-formed JSON reply that is not an array.

import (
	"context"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
)

func TestWorkspaceSymbolsDecodeFailureIsSurfaced(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	go serve(stdin, pw, map[string]any{"workspace/symbol": "harmless"})
	syms, err := b.WorkspaceSymbols(context.Background(), languages.WorkspaceSymbolRequest{Query: "Selected"})
	if err == nil {
		t.Fatalf("decode-failure workspace/symbol reply = %d syms, err=<nil>; want an explicit decode error", len(syms))
	}
	if !strings.Contains(err.Error(), "workspace symbol decode") {
		t.Fatalf("err = %v, want the workspace symbol decode wording", err)
	}
	if len(syms) != 0 {
		t.Fatalf("syms = %d, want empty on error", len(syms))
	}
}
