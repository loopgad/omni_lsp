package rustanalyzer

// X4/S3 protocol tests for the rust-analyzer bridge. All run against an
// injected fake server — no real toolchain required. The fake answers the
// exact request ids the bridge generates, so nothing races the pending
// table under -race.

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
		t.Fatalf("start fake rust-analyzer connection: %v", err)
	}
	b := &Backend{conn: conn, workDir: t.TempDir(), cfgFile: "Cargo.toml"}
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
	const uri = "file:///workspace/main.rs"
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
			"severity": 1, "code": "E0425", "source": "rust-analyzer", "message": "cannot find value",
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
		if diagnostic.StartChar != 4 || diagnostic.EndChar != 7 || diagnostic.Code != "E0425" || diagnostic.Source != "rust-analyzer" || diagnostic.Message != "cannot find value" {
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
				"label": "println", "kind": 3, "detail": "macro",
				"documentation": "println docs", "insertText": "println!($0)", "sortText": "01", "filterText": "pri",
			}},
			documentation: "println docs",
		},
		{
			name: "completion list",
			result: map[string]any{
				"isIncomplete": true,
				"items": []map[string]any{{
					"label": "String", "kind": 7, "detail": "struct std::string::String",
					"documentation": map[string]any{"kind": "markdown", "value": "**String**"},
					"insertText":    "String", "sortText": "02", "filterText": "String",
				}},
			},
			incomplete: true, documentation: "**String**",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, stdin, pw := newTestBackend(t)
			go serve(stdin, pw, map[string]any{"textDocument/completion": tc.result})

			result, err := b.CompletionList(context.Background(), languages.CompletionRequest{
				URI: "file:///x/main.rs", Content: []byte("pri\n"), SnapshotRev: 2,
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
			URI: "file:///x/main.rs", Content: []byte("let value = 1;\n"), SnapshotRev: 3,
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
					"uri": "file:///x/main.rs",
					"range": map[string]any{
						"start": map[string]uint32{"line": 5, "character": 6},
						"end":   map[string]uint32{"line": 5, "character": 10},
					},
				},
			}},
		})

		syms, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
			URI: "file:///x/main.rs", Content: []byte("let value = 1;\n"), SnapshotRev: 3,
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
		"textDocument/hover": map[string]any{"contents": map[string]string{"value": "fn hello(&self) -> u32"}},
	})

	res, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: "file:///x/main.rs", Content: []byte("fn main() {}\n"),
		SnapshotRev: 7, BuildContext: identity.BuildContextID("rust:sha256:test"),
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

func TestX4_NullHoverIsUnknownAndCanRecover(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	if epoch, ready := b.SemanticReadiness(); epoch != 1 || ready {
		t.Fatalf("initial semantic readiness = (%d, %t), want cold supervisor epoch 1", epoch, ready)
	}
	go func() {
		replies := 0
		for frame := range stdin.frames {
			var request struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal([]byte(frameBody(frame)), &request) != nil || request.ID == nil || request.Method != "textDocument/hover" {
				continue
			}
			replies++
			if replies == 1 {
				writeFrame(pw, *request.ID, json.RawMessage("null"))
				continue
			}
			writeFrame(pw, *request.ID, map[string]any{
				"contents": map[string]string{"kind": "markdown", "value": "fn main()"},
			})
		}
	}()

	request := languages.HoverRequest{
		URI: "file:///x/main.rs", Content: []byte("fn main() {}\n"),
		SnapshotRev: 7, BuildContext: identity.BuildContextID("rust:sha256:test"),
	}
	first, err := b.Hover(context.Background(), request)
	if err != nil {
		t.Fatalf("null hover returned Go error: %v", err)
	}
	if first.Status != identity.ResultUnknown || first.Value != nil || first.Completeness != identity.CompletenessUnknown {
		t.Fatalf("null hover = %+v; want unknown, retryable result", first)
	}
	if epoch, ready := b.SemanticReadiness(); epoch != 1 || ready {
		t.Fatalf("null hover semantic readiness = (%d, %t), want cold supervisor epoch 1", epoch, ready)
	}
	if len(first.Evidence) != 1 || first.Evidence[0].DetailCode != "rust-analyzer returned null hover; readiness or a negative result is not proven" {
		t.Fatalf("null hover evidence = %+v; want readiness ambiguity", first.Evidence)
	}

	second, err := b.Hover(context.Background(), request)
	if err != nil {
		t.Fatalf("recovered hover returned Go error: %v", err)
	}
	if second.Status != identity.ResultExact || second.Value == nil || second.Value.Contents != "fn main()" {
		t.Fatalf("recovered hover = %+v; want exact hover after transient null", second)
	}
	if epoch, ready := b.SemanticReadiness(); epoch != 1 || !ready {
		t.Fatalf("positive hover semantic readiness = (%d, %t), want ready supervisor epoch 1", epoch, ready)
	}
}

func TestX4_EmptyDefinitionAndReferencesAreUnknownAndCanRecover(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	var definitions, references int
	location := []map[string]any{{
		"uri": "file:///x/lib.rs",
		"range": map[string]any{
			"start": map[string]uint32{"line": 1, "character": 2},
			"end":   map[string]uint32{"line": 1, "character": 8},
		},
	}}
	go func() {
		for frame := range stdin.frames {
			var request struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal([]byte(frameBody(frame)), &request) != nil || request.ID == nil {
				continue
			}
			switch request.Method {
			case "textDocument/definition":
				definitions++
				if definitions == 1 {
					writeFrame(pw, *request.ID, json.RawMessage("null"))
				} else {
					writeFrame(pw, *request.ID, location)
				}
			case "textDocument/references":
				references++
				if references == 1 {
					writeFrame(pw, *request.ID, json.RawMessage("[]"))
				} else {
					writeFrame(pw, *request.ID, location)
				}
			}
		}
	}()

	const uri = "file:///x/main.rs"
	content := []byte("fn main() { call(); }\n")
	buildContext := identity.BuildContextID("rust:sha256:test")
	definitionRequest := languages.DefinitionRequest{
		URI: uri, Content: content, SnapshotRev: 7, BuildContext: buildContext,
		Line: 0, Column: 14,
	}
	firstDefinition, err := b.Definition(context.Background(), definitionRequest)
	if err != nil {
		t.Fatalf("null definition returned Go error: %v", err)
	}
	if firstDefinition.Status != identity.ResultUnknown || len(firstDefinition.Value) != 0 || firstDefinition.Completeness != identity.CompletenessUnknown {
		t.Fatalf("null definition = %+v; want unknown with no locations", firstDefinition)
	}
	if len(firstDefinition.InternalDiagnostics) != 1 || firstDefinition.InternalDiagnostics[0] != "rust-analyzer returned no definition locations; readiness or a negative result is not proven" {
		t.Fatalf("null definition diagnostics = %v; want readiness ambiguity", firstDefinition.InternalDiagnostics)
	}
	secondDefinition, err := b.Definition(context.Background(), definitionRequest)
	if err != nil {
		t.Fatalf("recovered definition returned Go error: %v", err)
	}
	if secondDefinition.Status != identity.ResultExact || len(secondDefinition.Value) != 1 || secondDefinition.Value[0].URI != "file:///x/lib.rs" {
		t.Fatalf("recovered definition = %+v; want exact location after null", secondDefinition)
	}

	referencesRequest := languages.ReferencesRequest{
		URI: uri, Content: content, SnapshotRev: 7, BuildContext: buildContext,
		Line: 0, Column: 14, IncludeDecl: true,
	}
	firstReferences, err := b.References(context.Background(), referencesRequest)
	if err != nil {
		t.Fatalf("empty references returned Go error: %v", err)
	}
	if firstReferences.Status != identity.ResultUnknown || len(firstReferences.Value) != 0 || firstReferences.Completeness != identity.CompletenessUnknown {
		t.Fatalf("empty references = %+v; want unknown with no locations", firstReferences)
	}
	if len(firstReferences.InternalDiagnostics) != 1 || firstReferences.InternalDiagnostics[0] != "rust-analyzer returned no reference locations; readiness or a negative result is not proven" {
		t.Fatalf("empty references diagnostics = %v; want readiness ambiguity", firstReferences.InternalDiagnostics)
	}
	secondReferences, err := b.References(context.Background(), referencesRequest)
	if err != nil {
		t.Fatalf("recovered references returned Go error: %v", err)
	}
	if secondReferences.Status != identity.ResultPartial || len(secondReferences.Value) != 1 || secondReferences.Value[0].URI != "file:///x/lib.rs" || secondReferences.Completeness != identity.IncompleteKnownSubset {
		t.Fatalf("recovered references = %+v; want partial location after empty result", secondReferences)
	}
}

func TestX4_DefinitionParsesLocations(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	go serve(stdin, pw, map[string]any{
		"textDocument/definition": []map[string]any{{
			"uri": "file:///x/lib.rs",
			"range": map[string]any{
				"start": map[string]uint32{"line": 1, "character": 2},
				"end":   map[string]uint32{"line": 1, "character": 8},
			},
		}},
	})

	res, err := b.Definition(context.Background(), languages.DefinitionRequest{
		URI: "file:///x/main.rs", Content: []byte("fn main() {}\n"),
		SnapshotRev: 3, BuildContext: identity.BuildContextID("rust:sha256:test"),
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
	if loc.URI != "file:///x/lib.rs" {
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

func TestX4_ErrorResponseSurfacesUnavailable(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	// No canned hover key → the fake answers every request with an error frame.
	go serve(stdin, pw, nil)

	res, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: "file:///x/main.rs", Content: []byte("fn main() {}\n"),
		SnapshotRev: 1, BuildContext: identity.BuildContextID("rust:sha256:test"),
	})
	if err == nil || !strings.Contains(err.Error(), "textDocument/hover refused by fake server") {
		t.Fatalf("upstream refusal error was not preserved: %v", err)
	}
	if res.Status != identity.ResultUnknown {
		t.Fatalf("error response status = %v, want unknown with error surfaced", res.Status)
	}
	if len(res.InternalDiagnostics) == 0 {
		t.Fatal("internal diagnostics must record the upstream refusal")
	}
}

func TestS3_RenameFailClosedWithoutBuildFile(t *testing.T) {
	b, _, _ := newTestBackend(t) // workdir is fresh — Cargo.toml absent

	res, err := b.Rename(context.Background(), languages.RenameRequest{
		URI: "file:///x/main.rs", Content: []byte("fn main() {}\n"),
		SnapshotRev: 1, BuildContext: identity.BuildContextID("rust:sha256:test"),
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
		if strings.Contains(d, "Cargo.toml") {
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

func TestS3_RenameNullIsRefused(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	if err := os.WriteFile(filepath.Join(b.workDir, "Cargo.toml"), []byte("[package]\nname='test'\nversion='0.1.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	go serve(stdin, pw, map[string]any{"textDocument/rename": json.RawMessage("null")})

	res, err := b.Rename(context.Background(), languages.RenameRequest{
		URI: "file:///x/main.rs", Content: []byte("fn main() {}\n"), SnapshotRev: 1,
		BuildContext: identity.BuildContextID("rust:sha256:test"), NewName: "renamed",
	})
	if err != nil {
		t.Fatalf("null rename returned Go error: %v", err)
	}
	if res.Status != identity.ResultUnavailable || res.Value.Complete {
		t.Fatalf("null rename = status %v, complete %t; want unavailable and incomplete", res.Status, res.Value.Complete)
	}
	if len(res.InternalDiagnostics) == 0 || res.InternalDiagnostics[0] != "upstream language service refused rename" {
		t.Fatalf("null rename diagnostics = %v; want upstream refusal", res.InternalDiagnostics)
	}
}

func TestS3_RenameEmptyWorkspaceEditIsRefused(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	if err := os.WriteFile(filepath.Join(b.workDir, "Cargo.toml"), []byte("[package]\nname='test'\nversion='0.1.0'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	go serve(stdin, pw, map[string]any{"textDocument/rename": map[string]any{"changes": nil}})

	res, err := b.Rename(context.Background(), languages.RenameRequest{
		URI: "file:///x/main.rs", Content: []byte("fn main() {}\n"), SnapshotRev: 1,
		BuildContext: identity.BuildContextID("rust:sha256:test"), NewName: "renamed",
	})
	if err != nil {
		t.Fatalf("empty rename returned Go error: %v", err)
	}
	if res.Status != identity.ResultUnavailable || res.Value.Complete || len(res.Value.Edits) != 0 {
		t.Fatalf("empty rename = status %v, complete %t, edits %v; want unavailable refusal", res.Status, res.Value.Complete, res.Value.Edits)
	}
	if len(res.Evidence) != 1 || res.Evidence[0].DetailCode != "rust-analyzer-refused" {
		t.Fatalf("empty rename evidence = %+v; want upstream refusal", res.Evidence)
	}
	if len(res.InternalDiagnostics) != 1 || res.InternalDiagnostics[0] != "upstream language service refused rename" {
		t.Fatalf("empty rename diagnostics = %v; want upstream refusal", res.InternalDiagnostics)
	}
}
