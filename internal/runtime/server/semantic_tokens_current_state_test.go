package server

// Pins the CURRENT semanticTokens behavior chain before the §3.3 ruling
// lands. Today buildCapabilities declares semanticTokensProvider
// unconditionally, while the ccls bridge answers textDocument/semanticTokens
// with an explicit empty list (its stub comment admits push-based tokens are
// not consumed yet). The semantics are still undecided (§7): either stop
// declaring the capability or make it conditional on the backend actually
// serving tokens. Until that ruling this test only protects the status quo
// from drifting silently — flipping either end must fail it.

import (
	"context"
	"testing"

	"github.com/omnilsp/omni/internal/languages/ccls"
)

func TestQ3_SemanticTokensDeclaredButCclsServesEmpty(t *testing.T) {
	caps := buildCapabilities("utf-16")
	if caps.SemanticTokensProvider == nil {
		t.Fatal("semanticTokensProvider is no longer declared — if this is the §7 ruling landing, update this pin together with its comment")
	}
	if !caps.SemanticTokensProvider.Full {
		t.Error("semanticTokensProvider lost the full-document flag")
	}

	// The served end of the chain for C/C++: SemanticTokens reads no backend
	// state, so a zero-value Backend is enough to pin the stub's answer.
	be := &ccls.Backend{}
	tokens, err := be.SemanticTokens(context.Background(), "file:///w/main.cpp", []byte("int main() {}\n"))
	if err != nil {
		t.Fatalf("ccls SemanticTokens error: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("ccls SemanticTokens = %d tokens, want the explicit empty stub list", len(tokens))
	}
}
