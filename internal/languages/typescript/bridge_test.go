package typescript

// X4/S3 protocol tests for the tsserver bridge. All run against an injected
// fake server — no real toolchain required. The fake answers the exact
// request ids the bridge generates, so nothing races the pending table
// under -race.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/nested"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// fakeStdin captures outgoing frames so tests respond to the real request
// ids instead of blind pre-writes that readLoop would drop as unmatched.
type fakeStdin struct{ frames chan string }

func newFakeStdin() *fakeStdin { return &fakeStdin{frames: make(chan string, 32)} }

func (f *fakeStdin) Write(p []byte) (int, error) { f.frames <- string(p); return len(p), nil }
func (f *fakeStdin) Close() error                { return nil }

// frameBody strips the Content-Length header from a captured outgoing write.
func frameBody(frame string) string {
	if i := strings.Index(frame, "\r\n\r\n"); i >= 0 {
		return frame[i+4:]
	}
	return frame
}

// newTestBackend wires a Backend onto injected pipes with no supervisor and
// no initialize handshake: SendRequest only needs the pending table plus
// stdin/stdout, both installed by Attach.
func newTestBackend(t *testing.T) (*Backend, *fakeStdin, io.WriteCloser) {
	t.Helper()
	stdin := newFakeStdin()
	pr, pw := io.Pipe()
	conn := nested.New(nested.Config{
		Name:               serverName,
		Lang:               langID,
		WorkDir:            t.TempDir(),
		RequestTimeout:     2 * time.Second,
		CaptureDiagnostics: true,
		Start: func(c *nested.Conn) error {
			c.Attach(nil, stdin, pr)
			c.MarkReady()
			return nil
		},
	})
	if err := conn.StartSupervised(); err != nil {
		t.Fatalf("start fake typescript connection: %v", err)
	}
	b := &Backend{conn: conn, workDir: t.TempDir(), cfgFile: "tsconfig.json"}
	t.Cleanup(func() {
		conn.Close() // closed flag stops readLoop's exit path before sup is touched
		pw.Close()
		close(stdin.frames)
	})
	return b, stdin, pw
}

// serve replies to every incoming request using canned results keyed by
// method; a missing key answers with a JSON-RPC error frame.
func serve(stdin *fakeStdin, out io.Writer, canned map[string]any) {
	for f := range stdin.frames {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(frameBody(f)), &req) != nil || req.ID == nil || req.Method == "" {
			continue // didOpen/shutdown notifications carry no id
		}
		if resp, ok := canned[req.Method]; ok {
			writeFrame(out, *req.ID, resp)
		} else {
			writeErrorFrame(out, *req.ID, req.Method+" refused by fake server")
		}
	}
}

// writeFrame sends one Content-Length-framed result response for id.
func writeFrame(w io.Writer, id int64, result any) {
	raw, _ := json.Marshal(result)
	frameMessage(w, &jsonrpc.Message{
		JSONRPC: jsonrpc.Version,
		ID:      &jsonrpc.RequestID{Num: id},
		Result:  raw,
	})
}

// writeErrorFrame sends one Content-Length-framed JSON-RPC error response.
func writeErrorFrame(w io.Writer, id int64, msg string) {
	frameMessage(w, &jsonrpc.Message{
		JSONRPC: jsonrpc.Version,
		ID:      &jsonrpc.RequestID{Num: id},
		Error:   &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: msg},
	})
}

func frameMessage(w io.Writer, msg *jsonrpc.Message) {
	data, _ := json.Marshal(msg)
	fmt.Fprintf(w, "Content-Length: %d\r\n\r\n%s", len(data), data)
}

func TestDiagnosticsForwardUpstreamUsingParentEncoding(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	const uri = "file:///workspace/main.ts"
	content := []byte("😀bad\n")
	type result struct {
		items []languages.Diagnostic
		err   error
	}
	got := make(chan result, 1)
	go func() {
		items, err := b.DiagnosticsWithEncoding(context.Background(), uri, content, 1, 0)
		got <- result{items: items, err: err}
	}()

	var outgoing struct {
		Method string `json:"method"`
		Params struct {
			TextDocument struct {
				Version int32 `json:"version"`
			} `json:"textDocument"`
		} `json:"params"`
	}
	select {
	case frame := <-stdin.frames:
		if err := json.Unmarshal([]byte(frameBody(frame)), &outgoing); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("didOpen was not sent")
	}
	if outgoing.Method != "textDocument/didOpen" || outgoing.Params.TextDocument.Version != 1 {
		t.Fatalf("initial document sync = %+v", outgoing)
	}
	params, _ := json.Marshal(map[string]any{
		"uri": uri, "version": 1,
		"diagnostics": []any{map[string]any{
			"range": map[string]any{
				"start": map[string]any{"line": 0, "character": 2},
				"end":   map[string]any{"line": 0, "character": 5},
			},
			"severity": 1, "code": 2322, "source": "ts", "message": "type mismatch",
		}},
	})
	frameMessage(pw, &jsonrpc.Message{JSONRPC: jsonrpc.Version, Method: "textDocument/publishDiagnostics", Params: params})

	select {
	case response := <-got:
		if response.err != nil {
			t.Fatalf("diagnostics failed: %v", response.err)
		}
		if len(response.items) != 1 {
			t.Fatalf("diagnostics = %+v, want one upstream item", response.items)
		}
		diagnostic := response.items[0]
		if diagnostic.StartChar != 4 || diagnostic.EndChar != 7 || diagnostic.Code != "2322" || diagnostic.Source != "ts" || diagnostic.Message != "type mismatch" {
			t.Fatalf("projected diagnostic = %+v", diagnostic)
		}
	case <-time.After(time.Second):
		t.Fatal("publishDiagnostics did not reach the backend")
	}
}

func TestX4_CompletionAcceptsArrayAndCompletionList(t *testing.T) {
	cases := []struct {
		name          string
		result        any
		incomplete    bool
		documentation string
	}{
		{
			name: "array",
			result: []map[string]any{{
				"label": "console", "kind": 9, "detail": "typeof console",
				"documentation": "console docs", "insertText": "console", "sortText": "01", "filterText": "con",
			}},
			documentation: "console docs",
		},
		{
			name: "completion list",
			result: map[string]any{
				"isIncomplete": true,
				"items": []map[string]any{{
					"label": "map", "kind": 2, "detail": "<U>(callbackfn: Function) => U[]",
					"documentation": map[string]any{"kind": "markdown", "value": "**map**"},
					"insertText":    "map($0)", "sortText": "02", "filterText": "map",
				}},
			},
			incomplete: true, documentation: "**map**",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, stdin, pw := newTestBackend(t)
			go serve(stdin, pw, map[string]any{"textDocument/completion": tc.result})

			result, err := b.CompletionList(context.Background(), languages.CompletionRequest{
				URI: "file:///x/main.ts", Content: []byte("con\n"), SnapshotRev: 2,
			})
			if err != nil {
				t.Fatalf("completion returned Go error: %v", err)
			}
			if len(result.Items) != 1 || result.Items[0].Label == "" || result.Items[0].Kind == 0 || result.Items[0].Detail == "" {
				t.Fatalf("completion = %+v, want one projected item", result)
			}
			item := result.Items[0]
			if result.IsIncomplete != tc.incomplete || item.Documentation != tc.documentation || item.InsertText == "" || item.SortText == "" || item.FilterText == "" {
				t.Fatalf("completion metadata was not preserved: %+v", result)
			}
		})
	}
}

func TestX4_DocumentSymbolsPreserveSelectionChildrenAndFlatRanges(t *testing.T) {
	t.Run("document symbols", func(t *testing.T) {
		b, stdin, pw := newTestBackend(t)
		go serve(stdin, pw, map[string]any{
			"textDocument/documentSymbol": []map[string]any{{
				"name": "Module", "detail": "module", "kind": 2,
				"range": map[string]any{
					"start": map[string]uint32{"line": 1, "character": 0},
					"end":   map[string]uint32{"line": 8, "character": 0},
				},
				"selectionRange": map[string]any{
					"start": map[string]uint32{"line": 1, "character": 4},
					"end":   map[string]uint32{"line": 1, "character": 10},
				},
				"children": []map[string]any{{
					"name": "value", "kind": 13,
					"range": map[string]any{
						"start": map[string]uint32{"line": 3, "character": 2},
						"end":   map[string]uint32{"line": 3, "character": 7},
					},
					"selectionRange": map[string]any{
						"start": map[string]uint32{"line": 3, "character": 2},
						"end":   map[string]uint32{"line": 3, "character": 7},
					},
				}},
			}},
		})

		syms, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
			URI: "file:///x/main.ts", Content: []byte("const value = 1;\n"), SnapshotRev: 3,
		})
		if err != nil {
			t.Fatalf("document symbols returned Go error: %v", err)
		}
		if len(syms) != 1 || syms[0].Detail != "module" || syms[0].SelectionLine != 1 || syms[0].SelectionCharacter != 4 {
			t.Fatalf("document symbols = %+v, want preserved selection", syms)
		}
		if !syms[0].SelectionRangeSet || syms[0].SelectionEndLine != 1 || syms[0].SelectionEndCharacter != 10 {
			t.Fatalf("document symbol selection end = %+v, want upstream end 1:10", syms[0])
		}
		if len(syms[0].Children) != 1 || syms[0].Children[0].Name != "value" || syms[0].Children[0].SelectionCharacter != 2 || syms[0].Children[0].SelectionEndCharacter != 7 {
			t.Fatalf("document symbol children = %+v, want nested child and selection", syms[0].Children)
		}
	})

	t.Run("symbol information", func(t *testing.T) {
		b, stdin, pw := newTestBackend(t)
		go serve(stdin, pw, map[string]any{
			"textDocument/documentSymbol": []map[string]any{{
				"name": "Flat", "kind": 12,
				"location": map[string]any{
					"uri": "file:///x/main.ts",
					"range": map[string]any{
						"start": map[string]uint32{"line": 5, "character": 6},
						"end":   map[string]uint32{"line": 5, "character": 10},
					},
				},
			}},
		})

		syms, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
			URI: "file:///x/main.ts", Content: []byte("const value = 1;\n"), SnapshotRev: 3,
		})
		if err != nil {
			t.Fatalf("flat document symbols returned Go error: %v", err)
		}
		if len(syms) != 1 || syms[0].Name != "Flat" || syms[0].StartLine != 5 || syms[0].SelectionCharacter != 6 || syms[0].SelectionEndCharacter != 10 {
			t.Fatalf("flat document symbols = %+v, want location range as selection", syms)
		}
	})
}

func TestX4_HoverProjectsEnvelope(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	go serve(stdin, pw, map[string]any{
		"textDocument/hover": map[string]any{"contents": map[string]string{"value": "const x: number"}},
	})

	res, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: "file:///x/main.ts", Content: []byte("let x = 1;\n"),
		SnapshotRev: 7, BuildContext: identity.BuildContextID("typescript:sha256:test"),
	})
	if err != nil {
		t.Fatalf("hover returned Go error: %v", err)
	}
	if res.Status != identity.ResultExact {
		t.Fatalf("status = %v, want exact", res.Status)
	}
	if res.Value == nil || res.Value.Contents == "" {
		t.Fatal("hover contents empty")
	}
	if len(res.Evidence) != 1 {
		t.Fatalf("evidence items = %d, want 1", len(res.Evidence))
	}
	ev := res.Evidence[0]
	if ev.BackendEpoch != 1 {
		t.Fatalf("backend epoch = %d, want the request's ready epoch 1", ev.BackendEpoch)
	}
	if ev.Kind != identity.EvidenceCompiler || ev.Assurance != identity.AssuranceCompilerResolved {
		t.Fatalf("evidence kind/assurance = %v/%v, want compiler/compilerResolved", ev.Kind, ev.Assurance)
	}
	if !strings.Contains(ev.DetailCode, serverName) {
		t.Fatalf("detail code %q lacks bridge name %q", ev.DetailCode, serverName)
	}
	if ev.Snapshot.Revision != 7 {
		t.Fatalf("snapshot revision = %d, want 7", ev.Snapshot.Revision)
	}
	if ev.Backend.Name != serverName || ev.Backend.Language != langID {
		t.Fatalf("backend id = %+v", ev.Backend)
	}
}

func TestX4_DefinitionParsesLocations(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	go serve(stdin, pw, map[string]any{
		"textDocument/definition": []map[string]any{{
			"uri": "file:///x/lib.ts",
			"range": map[string]any{
				"start": map[string]uint32{"line": 1, "character": 2},
				"end":   map[string]uint32{"line": 1, "character": 8},
			},
		}},
	})

	res, err := b.Definition(context.Background(), languages.DefinitionRequest{
		URI: "file:///x/main.ts", Content: []byte("let x = 1;\n"),
		SnapshotRev: 3, BuildContext: identity.BuildContextID("typescript:sha256:test"),
	})
	if err != nil {
		t.Fatalf("definition returned Go error: %v", err)
	}
	if res.Status != identity.ResultExact {
		t.Fatalf("status = %v, want exact", res.Status)
	}
	if len(res.Value) != 1 {
		t.Fatalf("locations = %d, want 1", len(res.Value))
	}
	loc := res.Value[0]
	if loc.URI != "file:///x/lib.ts" {
		t.Fatalf("uri = %q", loc.URI)
	}
	if loc.Range.StartLine != 1 || loc.Range.StartCharacter != 2 ||
		loc.Range.EndLine != 1 || loc.Range.EndCharacter != 8 {
		t.Fatalf("range = %+v", loc.Range)
	}
	if res.Evidence[0].Assurance != identity.AssuranceCompilerResolved {
		t.Fatalf("assurance = %v, want compilerResolved", res.Evidence[0].Assurance)
	}
	if res.Evidence[0].BackendEpoch != 1 {
		t.Fatalf("backend epoch = %d, want the request's ready epoch 1", res.Evidence[0].BackendEpoch)
	}
}

func TestX4_ErrorResponsePropagatesAsUnknown(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	// No canned hover key → the fake answers every request with an error frame.
	go serve(stdin, pw, nil)

	res, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: "file:///x/main.ts", Content: []byte("let x = 1;\n"),
		SnapshotRev: 1, BuildContext: identity.BuildContextID("typescript:sha256:test"),
	})
	if err == nil || !strings.Contains(err.Error(), "refused by fake server") {
		t.Fatalf("hover request error = %v, want child error preserved", err)
	}
	if res.Status != identity.ResultUnknown {
		t.Fatalf("error response status = %v, want unknown", res.Status)
	}
	if len(res.InternalDiagnostics) == 0 {
		t.Fatal("internal diagnostics must record the upstream refusal")
	}
}

func TestS3_RenameFailClosedWithoutBuildFile(t *testing.T) {
	b, _, _ := newTestBackend(t) // workdir is fresh — tsconfig.json absent

	res, err := b.Rename(context.Background(), languages.RenameRequest{
		URI: "file:///x/main.ts", Content: []byte("let x = 1;\n"),
		SnapshotRev: 1, BuildContext: identity.BuildContextID("typescript:sha256:test"),
		NewName: "renamed",
	})
	if err != nil {
		t.Fatalf("rename gate returned Go error: %v", err)
	}
	if res.Status != identity.ResultUnavailable {
		t.Fatalf("status = %v, want unavailable", res.Status)
	}
	found := false
	for _, d := range res.InternalDiagnostics {
		if strings.Contains(d, "tsconfig.json") {
			found = true
		}
	}
	if !found {
		t.Fatalf("diagnostics must name the missing manifest, got %v", res.InternalDiagnostics)
	}
	if res.Value.Complete {
		t.Fatal("unprovable rename must not claim completeness")
	}
}

func enableTSRenameForProtocolTest(t *testing.T, b *Backend) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.workDir, "tsconfig.json"), []byte(`{"compilerOptions":{}}`), 0o600); err != nil {
		t.Fatalf("write tsconfig: %v", err)
	}
}

func replyToTSRenameRequest(t *testing.T, frames <-chan string, out io.Writer, method string, result any) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case frame := <-frames:
			var msg jsonrpc.Message
			if err := json.Unmarshal([]byte(frameBody(frame)), &msg); err != nil {
				t.Fatalf("decode child request: %v", err)
			}
			if msg.ID == nil || msg.Method == "" {
				continue // didOpen/didChange notifications are not requests
			}
			if msg.Method != method {
				t.Fatalf("unexpected child request %q while waiting for %q", msg.Method, method)
			}
			writeFrame(out, msg.ID.Num, result)
			return
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", method)
		}
	}
}

func startTSRenameProtocolTest(t *testing.T, uri string, content []byte, token languages.Range, kind languages.SymbolKind) (*Backend, *fakeStdin, io.WriteCloser, <-chan struct {
	result identity.SemanticResult[languages.ValidatedEdit]
	err    error
}) {
	t.Helper()
	b, stdin, child := newTestBackend(t)
	enableTSRenameForProtocolTest(t, b)
	got := make(chan struct {
		result identity.SemanticResult[languages.ValidatedEdit]
		err    error
	}, 1)
	go func() {
		result, err := b.Rename(context.Background(), languages.RenameRequest{
			URI: uri, Content: content, SnapshotRev: 3,
			BuildContext: identity.BuildContextID("typescript:sha256:test"),
			Line:         token.StartLine, Column: token.StartCharacter, NewName: "Renamed",
		})
		got <- struct {
			result identity.SemanticResult[languages.ValidatedEdit]
			err    error
		}{result: result, err: err}
	}()
	definition := []map[string]any{{
		"uri": uri,
		"range": map[string]any{
			"start": map[string]uint32{"line": token.StartLine, "character": token.StartCharacter},
			"end":   map[string]uint32{"line": token.EndLine, "character": token.EndCharacter},
		},
	}}
	replyToTSRenameRequest(t, stdin.frames, child, "textDocument/definition", definition)
	documentSymbols := []map[string]any{{
		"name": "Target", "kind": int(kind),
		"range": map[string]any{
			"start": map[string]uint32{"line": 0, "character": 0},
			"end":   map[string]uint32{"line": token.EndLine, "character": token.EndCharacter + 4},
		},
		"selectionRange": map[string]any{
			"start": map[string]uint32{"line": token.StartLine, "character": token.StartCharacter},
			"end":   map[string]uint32{"line": token.EndLine, "character": token.EndCharacter},
		},
	}}
	replyToTSRenameRequest(t, stdin.frames, child, "textDocument/documentSymbol", documentSymbols)
	return b, stdin, child, got
}

func waitTSRenameResult(t *testing.T, got <-chan struct {
	result identity.SemanticResult[languages.ValidatedEdit]
	err    error
}) struct {
	result identity.SemanticResult[languages.ValidatedEdit]
	err    error
} {
	t.Helper()
	select {
	case result := <-got:
		return result
	case <-time.After(time.Second):
		t.Fatal("rename did not complete")
		return struct {
			result identity.SemanticResult[languages.ValidatedEdit]
			err    error
		}{}
	}
}

func assertNoTSRenameRequest(t *testing.T, frames <-chan string) {
	t.Helper()
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case frame := <-frames:
			var msg jsonrpc.Message
			if err := json.Unmarshal([]byte(frameBody(frame)), &msg); err != nil {
				t.Fatalf("decode outgoing frame: %v", err)
			}
			if msg.Method == "textDocument/rename" {
				t.Fatal("function-like rename reached typescript-language-server")
			}
		case <-timer.C:
			return
		}
	}
}

func TestS3_FunctionLikeRenameFailsClosedBeforeUpstreamRename(t *testing.T) {
	const uri = "file:///x/main.ts"
	content := []byte("export function SoakTarget() {}\n")
	token := languages.Range{StartLine: 0, StartCharacter: 16, EndLine: 0, EndCharacter: 26}
	for _, tc := range []struct {
		name string
		kind languages.SymbolKind
	}{
		{name: "function", kind: languages.SymbolFunction},
		{name: "method", kind: languages.SymbolMethod},
		{name: "constructor", kind: languages.SymbolConstructor},
		{name: "operator", kind: tsFunctionLikeSymbolOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stdin, _, got := startTSRenameProtocolTest(t, uri, content, token, tc.kind)
			response := waitTSRenameResult(t, got)
			if response.err != nil {
				t.Fatalf("rename returned Go error: %v", response.err)
			}
			if response.result.Status != identity.ResultUnavailable || response.result.Value.Complete {
				t.Fatalf("function-like rename = %+v; want unavailable and incomplete", response.result)
			}
			if len(response.result.Evidence) != 1 || response.result.Evidence[0].DetailCode != "rename-function-collision-unproven" {
				t.Fatalf("function-like rename evidence = %+v; want collision-proof refusal", response.result.Evidence)
			}
			const want = "rename refused: TypeScript/JavaScript function/method collision analysis is not proven (SEM-SAFE-001)"
			if len(response.result.InternalDiagnostics) != 1 || response.result.InternalDiagnostics[0] != want {
				t.Fatalf("function-like rename diagnostics = %v; want [%q]", response.result.InternalDiagnostics, want)
			}
			assertNoTSRenameRequest(t, stdin.frames)
		})
	}
}

func TestS3_ClassifiedVariableRenameAndEmptyResponses(t *testing.T) {
	const uri = "file:///x/main.ts"
	content := []byte("const Target = 1;\n")
	token := languages.Range{StartLine: 0, StartCharacter: 6, EndLine: 0, EndCharacter: 12}
	t.Run("variable keeps supported rename path", func(t *testing.T) {
		_, stdin, child, got := startTSRenameProtocolTest(t, uri, content, token, languages.SymbolVariable)
		edit := map[string]any{"changes": map[string]any{uri: []map[string]any{{
			"range": map[string]any{
				"start": map[string]uint32{"line": 0, "character": 6},
				"end":   map[string]uint32{"line": 0, "character": 12},
			},
			"newText": "Renamed",
		}}}}
		replyToTSRenameRequest(t, stdin.frames, child, "textDocument/rename", edit)
		response := waitTSRenameResult(t, got)
		if response.err != nil || response.result.Status != identity.ResultExact || !response.result.Value.Complete {
			t.Fatalf("classified variable rename = %+v, err=%v; want exact complete edit", response.result, response.err)
		}
		if len(response.result.Value.Edits) != 1 || response.result.Value.Edits[0].NewText != "Renamed" {
			t.Fatalf("classified variable edits = %+v; want one upstream edit", response.result.Value.Edits)
		}
	})

	t.Run("JSON null is refused", func(t *testing.T) {
		_, stdin, child, got := startTSRenameProtocolTest(t, uri, content, token, languages.SymbolVariable)
		replyToTSRenameRequest(t, stdin.frames, child, "textDocument/rename", json.RawMessage("null"))
		assertTSRenameRefused(t, waitTSRenameResult(t, got))
	})

	t.Run("empty WorkspaceEdit is refused", func(t *testing.T) {
		_, stdin, child, got := startTSRenameProtocolTest(t, uri, content, token, languages.SymbolVariable)
		replyToTSRenameRequest(t, stdin.frames, child, "textDocument/rename", map[string]any{"changes": nil})
		assertTSRenameRefused(t, waitTSRenameResult(t, got))
	})
}

func assertTSRenameRefused(t *testing.T, response struct {
	result identity.SemanticResult[languages.ValidatedEdit]
	err    error
}) {
	t.Helper()
	if response.err != nil {
		t.Fatalf("refused rename returned Go error: %v", response.err)
	}
	if response.result.Status != identity.ResultUnavailable || response.result.Value.Complete || len(response.result.Value.Edits) != 0 {
		t.Fatalf("refused rename = %+v; want unavailable and incomplete", response.result)
	}
	if len(response.result.Evidence) != 1 || response.result.Evidence[0].DetailCode != serverName+"-refused" {
		t.Fatalf("refused rename evidence = %+v; want upstream refusal", response.result.Evidence)
	}
	if len(response.result.InternalDiagnostics) != 1 || response.result.InternalDiagnostics[0] != "upstream language service refused rename" {
		t.Fatalf("refused rename diagnostics = %v; want upstream refusal", response.result.InternalDiagnostics)
	}
}
