// Invariants:
//  1. Read-only projection: mutating LSP methods are unreachable from MCP tools.
//  2. ProtocolRevision is a pinned compatibility constant; changes require a version bump.
//  3. Evidence projection never invents data beyond _meta passthrough.
//
// Package mcp exposes OmniLSP read-only code intelligence over the MCP stdio
// protocol, pinned to revision 2026-07-28.
//
// Design constraints (goal.md §A7/§C14):
//
//   - Stateless request model: every request is handled independently; the
//     historical initialize/session lifecycle is not assumed.
//   - Read-only by omission: no mutating tool is registered (fail-closed).
//   - Honest projection: tool results carry the canonical result status and
//     evidence provenance in _meta; descriptions never claim stronger
//     guarantees than the canonical result provides.
//
// Dependency direction (INV-ARCH-002/U3): this package defines its own narrow
// Core interface and MUST NOT import internal/runtime/server.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// ProtocolRevision is the pinned MCP protocol revision served by this adapter.
// It is fixed in source control per §A7 and never fetched at build time.
const ProtocolRevision = "2026-07-28"

const (
	ServerName    = "omnilsp"
	ServerVersion = "0.2.0"
	maxFrameSize  = 8 << 20 // 8 MiB guard against unbounded lines
)

// Core is the narrow consumer-defined interface the adapter needs
// (INV-ARCH-002/U3: consumers define their own interfaces). The semantic core
// adapts this onto its Backend implementations; the MCP package never sees them.
type Core interface {
	Hover(ctx context.Context, uri string, line, col uint32) (identity.SemanticResult[*languages.HoverResult], error)
	Definition(ctx context.Context, uri string, line, col uint32) (identity.SemanticResult[[]languages.Location], error)
	References(ctx context.Context, uri string, line, col uint32, includeDecl bool) (identity.SemanticResult[[]languages.Location], error)
	WorkspaceSymbols(ctx context.Context, query string) (identity.SemanticResult[[]languages.WorkspaceSymbol], error)
	IndexStatus(ctx context.Context) (map[string]any, error)
}

// wireRequest is the subset of JSON-RPC 2.0 a stateless server consumes.
// ID is raw so any id shape (string/number/null/absent) round-trips untouched.
type wireRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// Serve reads newline-delimited JSON-RPC requests from in (the MCP stdio
// transport framing: one JSON object per line), handles each request serially
// against core, and writes one response line per request to out.
//
// Malformed input produces an error response, never a panic (§C1). Requests
// without an id are notifications and are ignored. Serve returns on EOF or
// scanner failure.
func Serve(ctx context.Context, in io.Reader, out io.Writer, core Core) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrameSize)
	for sc.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		resp := handle(ctx, line, core)
		if resp == nil {
			continue // notification: no response in the stateless model
		}
		b, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		b = append(b, '\n')
		if _, err := out.Write(b); err != nil {
			return err
		}
	}
	return sc.Err()
}

// handle routes one request line to a response message. A nil return means
// the input was a notification.
func handle(ctx context.Context, line []byte, core Core) *jsonrpc.Message {
	var req wireRequest
	nullID := jsonrpc.RequestID{IsNull: true}
	if err := json.Unmarshal(line, &req); err != nil {
		return jsonrpc.NewErrorResponse(nullID, jsonrpc.ParseError, "parse error", nil)
	}
	id := nullID
	if req.ID != nil && string(req.ID) != "null" {
		if err := json.Unmarshal(req.ID, &id); err != nil {
			return jsonrpc.NewErrorResponse(nullID, jsonrpc.InvalidRequest, "invalid request id", nil)
		}
	} else if req.ID == nil {
		return nil // notification (no id member): nothing to answer
	}
	if req.Method == "" {
		return jsonrpc.NewErrorResponse(id, jsonrpc.InvalidRequest, "missing method", nil)
	}

	result, rpcErr := dispatch(ctx, req.Method, req.Params, core)
	if rpcErr != nil {
		return jsonrpc.NewErrorResponse(id, rpcErr.code, rpcErr.message, nil)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return jsonrpc.NewErrorResponse(id, jsonrpc.InternalError, "result encoding failed", nil)
	}
	return jsonrpc.NewResponse(id, raw)
}

type rpcErr struct {
	code    int
	message string
}

func dispatch(ctx context.Context, method string, params json.RawMessage, core Core) (any, *rpcErr) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolRevision": ProtocolRevision,
			"capabilities":     map[string]any{"tools": map[string]any{}},
			"serverInfo":       map[string]any{"name": ServerName, "version": ServerVersion},
		}, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return map[string]any{"tools": toolDefs()}, nil

	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
			return nil, &rpcErr{jsonrpc.InvalidParams, "invalid params"}
		}
		args := p.Arguments
		if len(args) == 0 {
			args = []byte("{}")
		}
		return callTool(ctx, p.Name, args, core)

	default:
		return nil, &rpcErr{jsonrpc.MethodNotFound, "method not found: " + method}
	}
}

// posArgs are the shared position arguments of the navigation tools.
type posArgs struct {
	URI    string `json:"uri"`
	Line   uint32 `json:"line"`
	Column uint32 `json:"column"`
}

// callTool executes one registered tool. Only read-only tools exist here;
// mutating operations stay unregistered by omission (§C14 fail-closed).
func callTool(ctx context.Context, name string, args json.RawMessage, core Core) (any, *rpcErr) {
	switch name {
	case "hover":
		var a posArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, &rpcErr{jsonrpc.InvalidParams, "invalid arguments for hover"}
		}
		res, err := core.Hover(ctx, a.URI, a.Line, a.Column)
		if err != nil {
			return nil, &rpcErr{jsonrpc.InternalError, err.Error()}
		}
		text := "no hover information available"
		if res.Value != nil && res.Value.Contents != "" {
			text = res.Value.Contents
		}
		return envelope(res.Status, res.Evidence, text), nil

	case "find_definition":
		var a posArgs
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, &rpcErr{jsonrpc.InvalidParams, "invalid arguments for find_definition"}
		}
		res, err := core.Definition(ctx, a.URI, a.Line, a.Column)
		if err != nil {
			return nil, &rpcErr{jsonrpc.InternalError, err.Error()}
		}
		return envelope(res.Status, res.Evidence, renderLocations(res.Value)), nil

	case "find_references":
		var a struct {
			posArgs
			IncludeDeclaration bool `json:"includeDeclaration"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, &rpcErr{jsonrpc.InvalidParams, "invalid arguments for find_references"}
		}
		res, err := core.References(ctx, a.URI, a.Line, a.Column, a.IncludeDeclaration)
		if err != nil {
			return nil, &rpcErr{jsonrpc.InternalError, err.Error()}
		}
		return envelope(res.Status, res.Evidence, renderLocations(res.Value)), nil

	case "workspace_symbols":
		var a struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, &rpcErr{jsonrpc.InvalidParams, "invalid arguments for workspace_symbols"}
		}
		res, err := core.WorkspaceSymbols(ctx, a.Query)
		if err != nil {
			return nil, &rpcErr{jsonrpc.InternalError, err.Error()}
		}
		return envelope(res.Status, res.Evidence, renderSymbols(res.Value)), nil

	case "index_status":
		m, err := core.IndexStatus(ctx)
		if err != nil {
			return nil, &rpcErr{jsonrpc.InternalError, err.Error()}
		}
		if m == nil {
			m = map[string]any{}
		}
		raw, merr := json.Marshal(m)
		if merr != nil {
			return nil, &rpcErr{jsonrpc.InternalError, "index status encoding failed"}
		}
		// ponytail: index_status has no canonical envelope yet, so no _meta is claimed
		return map[string]any{
			"content": []map[string]string{{"type": "text", "text": string(raw)}},
		}, nil

	default:
		// Unknown tool names are rejected rather than guessed at.
		return nil, &rpcErr{jsonrpc.InvalidParams, "unknown tool: " + name}
	}
}

// envelope builds the MCP tool result: human-readable content plus the honest
// _meta projection of the canonical result status and evidence provenance
// (§C14: never claim stronger guarantees than the canonical result carries).
func envelope(status identity.ResultStatus, evs []identity.Evidence, text string) map[string]any {
	meta := map[string]any{"status": status.String()}
	if len(evs) > 0 {
		ev := evs[0]
		meta["evidenceKind"] = ev.Kind.String()
		meta["assurance"] = int(ev.Assurance)
		meta["snapshotRev"] = uint64(ev.Snapshot.Revision)
		if ev.BuildContext != "" {
			meta["buildContext"] = string(ev.BuildContext)
		}
		if ev.Backend.Language != "" || ev.Backend.Name != "" {
			meta["backend"] = strings.Trim(ev.Backend.Language+"/"+ev.Backend.Name, "/")
		}
		if ev.DetailCode != "" {
			meta["detailCode"] = ev.DetailCode
		}
	} else {
		// No evidence recorded: say so instead of implying certainty.
		meta["evidenceKind"] = "none"
	}
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
		"_meta":   meta,
	}
}

// renderLocations formats locations as URI:line:column (displayed 1-based).
func renderLocations(locs []languages.Location) string {
	if len(locs) == 0 {
		return "no results"
	}
	parts := make([]string, len(locs))
	for i, l := range locs {
		parts[i] = fmt.Sprintf("%s:%d:%d", l.URI, l.Range.StartLine+1, l.Range.StartCharacter+1)
	}
	return strings.Join(parts, "\n")
}

// renderSymbols formats workspace symbols as NAME URI:line:column (1-based).
func renderSymbols(syms []languages.WorkspaceSymbol) string {
	if len(syms) == 0 {
		return "no results"
	}
	parts := make([]string, len(syms))
	for i, s := range syms {
		parts[i] = fmt.Sprintf("%s %s:%d:%d", s.Name, s.URI, s.StartLine+1, s.StartCol+1)
	}
	return strings.Join(parts, "\n")
}

// toolDef is an MCP tool descriptor.
type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func uintProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "minimum": 0, "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func objectSchema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func posProps() map[string]any {
	return map[string]any{
		"uri":    strProp("Document URI"),
		"line":   uintProp("Zero-based line"),
		"column": uintProp("Zero-based column"),
	}
}

// toolDefs lists exactly the five read-only tools. Descriptions deliberately
// avoid promising exactness: strength of each answer travels in _meta (§C14).
func toolDefs() []toolDef {
	reqPos := []string{"uri", "line", "column"}
	return []toolDef{
		{
			Name:        "hover",
			Description: "Hover information for the symbol at a document position. The answer's status and evidence level are reported in the result _meta; it may be partial, unknown, or unavailable.",
			InputSchema: objectSchema(reqPos, posProps()),
		},
		{
			Name:        "find_definition",
			Description: "Candidate definition location(s) of the symbol at a document position. Locations are candidates graded by the evidence in _meta, which may report partial or unknown status.",
			InputSchema: objectSchema(reqPos, posProps()),
		},
		{
			Name:        "find_references",
			Description: "References to the symbol at a document position. Set includeDeclaration to also include the declaration site. Completeness depends on backend evidence; consult _meta.",
			InputSchema: objectSchema(reqPos, func() map[string]any {
				p := posProps()
				p["includeDeclaration"] = boolProp("Include the declaration site among references")
				return p
			}()),
		},
		{
			Name:        "workspace_symbols",
			Description: "Ask the configured language backend for workspace symbols matching a query substring. Results depend on backend support and coverage; _meta reports result status and evidence. This does not query the persistent file-inventory index.",
			InputSchema: objectSchema([]string{"query"}, map[string]any{
				"query": strProp("Substring to match against symbol names"),
			}),
		},
		{
			Name:        "index_status",
			Description: "Report persistent file-inventory index status, including freshness, generation, and rebuild counters when available. Read-only and informational; this index does not contain semantic symbols.",
			InputSchema: objectSchema(nil, map[string]any{}),
		},
	}
}
