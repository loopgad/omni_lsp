package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// fakeCore returns hardcoded canonical results for e2e protocol tests.
type fakeCore struct{}

func testEvidence() identity.Evidence {
	return identity.Evidence{
		Kind:         identity.EvidenceCompiler,
		Assurance:    identity.AssuranceCompilerResolved,
		Snapshot:     identity.SnapshotID{Workspace: "w1", Revision: 42},
		BuildContext: "bc-test",
		Backend:      identity.BackendID{Language: "go", Name: "native"},
		DetailCode:   "EXACT",
	}
}

func (fakeCore) Hover(_ context.Context, _ string, _, _ uint32) (identity.SemanticResult[*languages.HoverResult], error) {
	return identity.NewExactResult(
		&languages.HoverResult{Contents: "func Foo(x int) int"},
		[]identity.Evidence{testEvidence()},
	), nil
}

func (fakeCore) Definition(_ context.Context, _ string, _, _ uint32) (identity.SemanticResult[[]languages.Location], error) {
	return identity.NewExactResult(
		[]languages.Location{{URI: "file:///a.go", Range: languages.Range{StartLine: 3}}},
		[]identity.Evidence{testEvidence()},
	), nil
}

func (fakeCore) References(_ context.Context, _ string, _, _ uint32, _ bool) (identity.SemanticResult[[]languages.Location], error) {
	return identity.NewPartialResult(
		[]languages.Location{{URI: "file:///a.go", Range: languages.Range{StartLine: 7}}},
		[]identity.Evidence{testEvidence()},
	), nil
}

func (fakeCore) WorkspaceSymbols(_ context.Context, _ string) (identity.SemanticResult[[]languages.WorkspaceSymbol], error) {
	return identity.NewPartialResult(
		[]languages.WorkspaceSymbol{{Name: "Foo", URI: "file:///a.go"}},
		[]identity.Evidence{testEvidence()},
	), nil
}

func (fakeCore) IndexStatus(_ context.Context) (map[string]any, error) {
	return map[string]any{"documents": 1, "symbols": 2}, nil
}

// runServe drives Serve over an in-memory pipe of newline-delimited requests
// and indexes the emitted responses by their numeric id.
func runServe(t *testing.T, requests ...string) map[int64]*jsonrpc.Message {
	t.Helper()
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	if err := Serve(context.Background(), in, &out, fakeCore{}); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	responses := map[int64]*jsonrpc.Message{}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg jsonrpc.Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("unparseable response line %q: %v", line, err)
		}
		if msg.ID == nil {
			t.Fatalf("response without id: %s", line)
		}
		responses[msg.ID.Num] = &msg
	}
	return responses
}

func resultRaw(t *testing.T, resp *jsonrpc.Message) map[string]any {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	var m map[string]any
	if err := json.Unmarshal(resp.Result, &m); err != nil {
		t.Fatalf("unparseable result %s: %v", resp.Result, err)
	}
	return m
}

func TestInitializePinsProtocolRevision(t *testing.T) {
	resp := runServe(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)[1]
	m := resultRaw(t, resp)

	rev, ok := m["protocolRevision"].(string)
	if !ok || rev != "2026-07-28" {
		t.Errorf("protocolRevision = %v, want pinned %q", m["protocolRevision"], ProtocolRevision)
	}
	caps, ok := m["capabilities"].(map[string]any)
	if !ok || caps["tools"] == nil {
		t.Errorf("capabilities missing tools entry: %v", m["capabilities"])
	}
	info, ok := m["serverInfo"].(map[string]any)
	if !ok || info["name"] != ServerName || info["version"] == "" {
		t.Errorf("serverInfo incomplete: %v", m["serverInfo"])
	}

	// Ping stays stateless: answered without any prior session handshake.
	resp = runServe(t, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)[9]
	resultRaw(t, resp)
}

func TestToolsListIsFiveReadOnly(t *testing.T) {
	resp := runServe(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)[1]
	raw, err := json.Marshal(resultRaw(t, resp)["tools"])
	if err != nil {
		t.Fatal(err)
	}
	var tools []toolDef
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 5 {
		t.Fatalf("got %d tools, want exactly 5", len(tools))
	}
	want := map[string]bool{
		"hover": true, "find_definition": true, "find_references": true,
		"workspace_symbols": true, "index_status": true,
	}
	descriptions := make(map[string]string, len(tools))
	for _, tl := range tools {
		if !want[tl.Name] {
			t.Errorf("unexpected tool %q (mutating tools must stay unregistered)", tl.Name)
		}
		delete(want, tl.Name)
		if tl.Description == "" {
			t.Errorf("tool %q missing description", tl.Name)
		}
		descriptions[tl.Name] = tl.Description
		if tl.InputSchema["type"] != "object" {
			t.Errorf("tool %q schema not an object schema: %v", tl.Name, tl.InputSchema)
		}
	}
	for name := range want {
		t.Errorf("missing tool %q", name)
	}
	if !strings.Contains(descriptions["workspace_symbols"], "does not query the persistent file-inventory index") {
		t.Errorf("workspace_symbols description blurs semantic and persistent inventory indexes: %q", descriptions["workspace_symbols"])
	}
	if !strings.Contains(descriptions["index_status"], "does not contain semantic symbols") {
		t.Errorf("index_status description overstates index contents: %q", descriptions["index_status"])
	}
}

func TestHoverCallTextAndMetaProjection(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hover","arguments":{"uri":"file:///x.go","line":0,"column":4}}}`
	resp := runServe(t, req)[2]
	m := resultRaw(t, resp)

	content, ok := m["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %v, want one text item", m["content"])
	}
	item := content[0].(map[string]any)
	if item["type"] != "text" {
		t.Errorf("content type = %v, want text", item["type"])
	}
	if item["text"] != "func Foo(x int) int" {
		t.Errorf("hover text = %v", item["text"])
	}

	meta, ok := m["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta missing: %v", m["_meta"])
	}
	ev := testEvidence()
	want := map[string]any{
		"status":       "exact",
		"evidenceKind": ev.Kind.String(),
		"assurance":    float64(ev.Assurance),
		"snapshotRev":  float64(ev.Snapshot.Revision),
		"buildContext": string(ev.BuildContext),
		"backend":      "go/native",
		"detailCode":   ev.DetailCode,
	}
	for k, v := range want {
		if meta[k] != v {
			t.Errorf("_meta[%q] = %v (%T), want %v", k, meta[k], meta[k], v)
		}
	}
}

func TestUnknownToolAndMethodReturnErrors(t *testing.T) {
	responses := runServe(t,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rename","arguments":{"uri":"file:///x.go"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"workspace/willRename","params":{}}`,
	)
	if err := responses[1].Error; err == nil {
		t.Error("unknown tool 'rename' must return an error")
	} else if err.Code != jsonrpc.InvalidParams {
		t.Errorf("unknown tool code = %d, want %d", err.Code, jsonrpc.InvalidParams)
	}
	if err := responses[2].Error; err == nil {
		t.Error("unknown method must return -32601")
	} else if err.Code != jsonrpc.MethodNotFound {
		t.Errorf("unknown method code = %d, want %d", err.Code, jsonrpc.MethodNotFound)
	}
}

func TestWorkspaceSymbolsHonestStatus(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"workspace_symbols","arguments":{"query":"Fo"}}}`
	resp := runServe(t, req)[3]
	m := resultRaw(t, resp)

	meta := m["_meta"].(map[string]any)
	if meta["status"] != "partial" {
		t.Errorf("_meta.status = %v, want honest partial", meta["status"])
	}
	text := m["content"].([]any)[0].(map[string]any)["text"]
	if !strings.Contains(text.(string), "Foo") {
		t.Errorf("symbol text = %v, want Foo listed", text)
	}
}

func TestIndexStatusHasNoEnvelopeMeta(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"index_status"}}`
	resp := runServe(t, req)[4]
	m := resultRaw(t, resp)
	if _, claimed := m["_meta"]; claimed {
		t.Error("index_status must not claim evidence _meta without a canonical envelope")
	}
	text := m["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "documents") {
		t.Errorf("index_status text = %q", text)
	}
}

func TestMalformedJSONDoesNotPanic(t *testing.T) {
	requests := []string{
		`{definitely not json`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`, // notification: ignored
		`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
	}
	var out bytes.Buffer
	in := strings.NewReader(strings.Join(requests, "\n") + "\n")
	if err := Serve(context.Background(), in, &out, fakeCore{}); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 2 {
		t.Fatalf("got %d response lines, want exactly 2 (error + ping): %q", len(lines), out.String())
	}
	var parseResp jsonrpc.Message
	if err := json.Unmarshal([]byte(lines[0]), &parseResp); err != nil {
		t.Fatal(err)
	}
	if parseResp.Error == nil || parseResp.Error.Code != jsonrpc.ParseError {
		t.Errorf("malformed JSON error = %+v, want code %d", parseResp.Error, jsonrpc.ParseError)
	}
	var pingResp jsonrpc.Message
	if err := json.Unmarshal([]byte(lines[1]), &pingResp); err != nil {
		t.Fatal(err)
	}
	if pingResp.Error != nil || pingResp.ID == nil || pingResp.ID.Num != 7 {
		t.Errorf("post-malformed ping mishandled: %+v", pingResp)
	}
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
