// Read-API adapters: bridge the MCP tools surface and the HTTP read API to
// the server's canonical envelope facades. Both adapters live at the
// protocol boundary (cmd) so INV-ARCH-002 holds: the semantic core never
// imports mcp/http packages — these files import the core, one way only.
package main

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	mcpserver "github.com/omnilsp/omni/internal/protocol/mcp"
	"github.com/omnilsp/omni/internal/runtime/server"
	httpserver "github.com/omnilsp/omni/internal/transport/httpserver"
)

// mcpCore implements protocol/mcp's Core over the server facades.
type mcpCore struct{ srv *server.Server }

func (c mcpCore) Hover(ctx context.Context, uri string, line, col uint32) (identity.SemanticResult[*languages.HoverResult], error) {
	return c.srv.HoverEnvelope(ctx, uri, line, col)
}

func (c mcpCore) Definition(ctx context.Context, uri string, line, col uint32) (identity.SemanticResult[[]languages.Location], error) {
	return c.srv.DefinitionEnvelope(ctx, uri, line, col)
}

func (c mcpCore) References(ctx context.Context, uri string, line, col uint32, includeDecl bool) (identity.SemanticResult[[]languages.Location], error) {
	return c.srv.ReferencesEnvelope(ctx, uri, line, col, includeDecl)
}

func (c mcpCore) WorkspaceSymbols(ctx context.Context, query string) (identity.SemanticResult[[]languages.WorkspaceSymbol], error) {
	raw, err := c.srv.CallMethod(ctx, "workspace/symbol", map[string]any{"query": query})
	if err != nil {
		return identity.SemanticResult[[]languages.WorkspaceSymbol]{}, err
	}
	var lspSyms []struct {
		Name     string `json:"name"`
		Kind     int    `json:"kind"`
		Location struct {
			URI   string `json:"uri"`
			Range struct {
				Start struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"start"`
			} `json:"range"`
		} `json:"location"`
	}
	if err := json.Unmarshal(raw, &lspSyms); err != nil {
		return identity.SemanticResult[[]languages.WorkspaceSymbol]{
			Status:              identity.ResultUnknown,
			InternalDiagnostics: []string{"workspace symbol decode failed: " + err.Error()},
		}, nil
	}
	syms := make([]languages.WorkspaceSymbol, len(lspSyms))
	for i, s := range lspSyms {
		syms[i] = languages.WorkspaceSymbol{
			Name: s.Name, Kind: languages.SymbolKind(s.Kind),
			URI:       s.Location.URI,
			StartLine: s.Location.Range.Start.Line, StartCol: s.Location.Range.Start.Character,
		}
	}
	return identity.SemanticResult[[]languages.WorkspaceSymbol]{
		Status:       identity.ResultExact,
		Value:        syms,
		Evidence:     []identity.Evidence{{Kind: identity.EvidenceIndex, Assurance: identity.AssuranceIndexedExact}},
		Completeness: identity.Complete,
	}, nil
}

func (c mcpCore) IndexStatus(ctx context.Context) (map[string]any, error) {
	raw, err := c.srv.CallMethod(ctx, "omnilsp/indexStats", map[string]any{})
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// httpCore implements transport/httpserver's Core over the same facades.
// httpCore dereferences the active session server through an atomic pointer:
// TCP accept-loop reconnects swap the instance while the HTTP listener and
// its handlers stay resident (stability: warm admin surface across sessions).
type httpCore struct {
	srv atomic.Pointer[server.Server]
}

func (c *httpCore) Hover(ctx context.Context, uri string, line, column uint32) (identity.SemanticResult[*languages.HoverResult], error) {
	return c.srv.Load().HoverEnvelope(ctx, uri, line, column)
}

func (c *httpCore) Definition(ctx context.Context, uri string, line, column uint32) (identity.SemanticResult[[]languages.Location], error) {
	return c.srv.Load().DefinitionEnvelope(ctx, uri, line, column)
}

func (c *httpCore) References(ctx context.Context, uri string, line, column uint32, includeDeclaration bool) (identity.SemanticResult[[]languages.Location], error) {
	return c.srv.Load().ReferencesEnvelope(ctx, uri, line, column, includeDeclaration)
}

func (c *httpCore) Status() map[string]any {
	raw, err := c.srv.Load().CallMethod(context.Background(), "omnilsp/status", map[string]any{})
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (c *httpCore) Ready() bool { return c.srv.Load().State() == server.StateRunning }

var (
	_ mcpserver.Core  = mcpCore{}
	_ httpserver.Core = (*httpCore)(nil)
)
