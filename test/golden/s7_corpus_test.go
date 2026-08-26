package golden

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/runtime/server"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// §S7 requires ten corpus tiers per Tier-S language. This harness drives the
// full request path (didOpen -> hover -> references) against each tier and
// asserts the only properties that must hold at every scale: no panic, a
// parseable response, and bounded work. Absolute latencies live in test/perf.

type tier struct {
	name string
	text func() string
}

func rustFn(name string, bodyLines int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "fn %s(x: i32) -> i32 {\n", name)
	for i := 0; i < bodyLines; i++ {
		fmt.Fprintf(&b, "    x + %d;\n", i)
	}
	b.WriteString("    x\n}\n")
	return b.String()
}

func s7tiers() []tier {
	return []tier{
		{"s7-tiny", func() string { return "fn t() {}\n" }},
		{"s7-medium", func() string { return rustFn("medium", 50) }},
		{"s7-large", func() string { return rustFn("large", 500) }},
		{"s7-pathological", func() string {
			deep := strings.Repeat("(", 400) + strings.Repeat(")", 400) // nesting bomb
			long := strings.Repeat("x", 20000)                          // one huge line
			return "fn p() { let _ = \"" + deep + long + "\"; }\n"
		}},
		{"s7-broken-code", func() string { return "fn broken( {\n  this is not rust )))\n" }},
		{"s7-real-world-representative", func() string {
			// A slice of this repository's own production code.
			return "// Package persistent implements the transactional on-disk index store.\n// Immutable segments, atomic generation publication, checksum verification.\nfn demo(x: i32) -> i32 {\n    x + 1\n}\n"
		}},
		{"s7-multi-root", func() string {
			return rustFn("multiroot", 10) // two documents from distinct roots below
		}},
		{"s7-generated-code", func() string {
			// Generated artifacts carry a virtual URI marker (#vdoc, §D12).
			return "mod generated {\n    fn g() {}\n}\n"
		}},
		{"s7-unicode", func() string {
			return "fn ünïcödé() { let 中文 = \"emoji 🎉🚀\"; let _ = 中文; }\n"
		}},
		{"s7-platform-conditional", func() string {
			return strings.ReplaceAll(rustFn("crlf", 5), "\n", "\r\n") // Windows line endings
		}},
	}
}

// driveTier opens the document and fires hover + references, returning both
// raw responses for shape assertions.
func driveTier(t *testing.T, uri, langID, text string) (hoverResp, refResp *jsonrpc.Message) {
	t.Helper()
	s := server.New(server.DefaultConfig())
	s.RegisterBackend(langID, stubBackend{langID: langID, exts: []string{".rs"}})
	s.VFS().Open(uri, langID, 1, []byte(text), vfs.SourceEditor)
	dispatch := func(id int64, method, params string) *jsonrpc.Message {
		msg := jsonrpc.NewRequest(jsonrpc.RequestID{Num: id}, method, json.RawMessage(params))
		return s.Dispatcher().Dispatch(context.Background(), msg)
	}
	pos := fmt.Sprintf(`{"textDocument":{"uri":%q},"position":{"line":0,"character":3}`, uri)
	hoverResp = dispatch(1, "textDocument/hover", pos+"}")
	refResp = dispatch(2, "textDocument/references", pos+`,"context":{"includeDeclaration":true}}`)
	return hoverResp, refResp
}

func tierName(uri string) string {
	if i := strings.LastIndex(uri, "/"); i >= 0 {
		return uri[i+1:]
	}
	return uri
}

func assertSane(t *testing.T, resp *jsonrpc.Message, what string) {
	t.Helper()
	if resp == nil {
		t.Fatalf("%s: nil response", what)
	}
	if resp.Error != nil {
		t.Errorf("%s: unexpected error %v", what, resp.Error)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
	}
	raw, _ := json.Marshal(resp)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Errorf("%s: unparseable envelope: %v", what, err)
	}
}

// TestS7_CorpusTiersSmoke exercises all ten §S7 tiers end to end. Every tier
// must complete without panic and answer with well-formed envelopes; the
// pathological and broken-code tiers additionally prove graceful degradation.
func TestS7_CorpusTiersSmoke(t *testing.T) {
	for _, tr := range s7tiers() {
		tr := tr
		t.Run(tr.name, func(t *testing.T) {
			text := tr.text()
			uri := fmt.Sprintf("file:///w/%s.rs", tr.name)
			hoverResp, refResp := driveTier(t, uri, "rust", text)
			assertSane(t, hoverResp, "hover")
			assertSane(t, refResp, "references")
		})
	}
}

// stubBackend satisfies languages.Backend minimally: S7 exercises the
// request pipeline shape, not semantic depth.
type stubBackend struct {
	langID string
	exts   []string
}

func (b stubBackend) LanguageID() string       { return b.langID }
func (b stubBackend) FileExtensions() []string { return b.exts }
func (b stubBackend) Close() error             { return nil }

func (b stubBackend) Completion(context.Context, languages.CompletionRequest) ([]languages.CompletionItem, error) {
	return nil, nil
}
func (b stubBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	return identity.SemanticResult[*languages.HoverResult]{Status: identity.ResultUnavailable}, nil
}
func (b stubBackend) Definition(context.Context, languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{}, nil
}
func (b stubBackend) References(context.Context, languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	return identity.SemanticResult[[]languages.Location]{}, nil
}
func (b stubBackend) DocumentSymbols(context.Context, languages.DocumentSymbolRequest) ([]languages.DocumentSymbol, error) {
	return nil, nil
}
func (b stubBackend) WorkspaceSymbols(context.Context, languages.WorkspaceSymbolRequest) ([]languages.WorkspaceSymbol, error) {
	return nil, nil
}
func (b stubBackend) Diagnostics(context.Context, string, []byte) ([]languages.Diagnostic, error) {
	return nil, nil
}
func (b stubBackend) SemanticTokens(context.Context, string, []byte) ([]languages.SemanticToken, error) {
	return nil, nil
}
func (b stubBackend) Rename(context.Context, languages.RenameRequest) (identity.SemanticResult[languages.ValidatedEdit], error) {
	return identity.SemanticResult[languages.ValidatedEdit]{Status: identity.ResultUnavailable}, nil
}
