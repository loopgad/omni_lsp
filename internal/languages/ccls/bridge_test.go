package ccls

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

type cclsProtocolWriter struct{ frames chan string }

func (w *cclsProtocolWriter) Write(p []byte) (int, error) {
	w.frames <- string(p)
	return len(p), nil
}

func (w *cclsProtocolWriter) Close() error { return nil }

func readCclsProtocolMessage(t *testing.T, frames <-chan string) map[string]json.RawMessage {
	t.Helper()
	select {
	case frame := <-frames:
		separator := strings.Index(frame, "\r\n\r\n")
		if separator < 0 {
			t.Fatalf("malformed LSP frame %q", frame)
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal([]byte(frame[separator+4:]), &message); err != nil {
			t.Fatalf("decode LSP frame: %v", err)
		}
		return message
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for child request")
		return nil
	}
}

func newCclsProtocolBackend(t *testing.T) (*Backend, *cclsProtocolWriter, io.WriteCloser) {
	t.Helper()
	stdout, child := io.Pipe()
	workDir := t.TempDir()
	stdin := &cclsProtocolWriter{frames: make(chan string, 16)}
	conn := nested.New(nested.Config{
		Name: "clangd", Lang: "cpp", WorkDir: workDir, RequestTimeout: 2 * time.Second,
		Start: func(c *nested.Conn) error {
			c.Attach(nil, stdin, stdout)
			c.MarkReady()
			return nil
		},
	})
	if err := conn.StartSupervised(); err != nil {
		t.Fatalf("start fake clangd connection: %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = child.Close()
	})
	return &Backend{conn: conn, workDir: workDir}, stdin, child
}

func replyToCclsRequest(t *testing.T, frames <-chan string, child io.Writer, method string, result any) {
	t.Helper()
	for {
		message := readCclsProtocolMessage(t, frames)
		var gotMethod string
		if err := json.Unmarshal(message["method"], &gotMethod); err != nil {
			t.Fatalf("decode child method: %v", err)
		}
		if gotMethod != method {
			continue
		}
		var id int64
		if err := json.Unmarshal(message["id"], &id); err != nil || id == 0 {
			t.Fatalf("decode %s request id: %d, err %v", method, id, err)
		}
		response, err := json.Marshal(&jsonrpc.Message{
			JSONRPC: jsonrpc.Version,
			ID:      &jsonrpc.RequestID{Num: id},
			Result:  mustCclsRaw(t, result),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintf(child, "Content-Length: %d\r\n\r\n%s", len(response), response); err != nil {
			t.Fatalf("write %s response: %v", method, err)
		}
		return
	}
}

func closeCclsProtocolBackend(t *testing.T, b *Backend, stdin *cclsProtocolWriter, child io.Writer) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- b.conn.Close() }()

	shutdown := readCclsProtocolMessage(t, stdin.frames)
	var method string
	if err := json.Unmarshal(shutdown["method"], &method); err != nil || method != "shutdown" {
		t.Fatalf("shutdown request method = %q, err %v", method, err)
	}
	requestID := shutdown["id"]
	if len(requestID) == 0 {
		t.Fatal("shutdown request has no id")
	}
	response, err := json.Marshal(map[string]any{"jsonrpc": jsonrpc.Version, "id": requestID, "result": nil})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(child, "Content-Length: %d\r\n\r\n%s", len(response), response); err != nil {
		t.Fatalf("write shutdown response: %v", err)
	}

	exit := readCclsProtocolMessage(t, stdin.frames)
	if err := json.Unmarshal(exit["method"], &method); err != nil || method != "exit" {
		t.Fatalf("exit notification method = %q, err %v", method, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after shutdown response and exit notification")
	}
}

func mustCclsRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func enableCclsRenameForProtocolTest(t *testing.T, b *Backend) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.workDir, "compile_commands.json"), []byte("[]"), 0o644); err != nil {
		t.Fatalf("write compile database: %v", err)
	}
}

func replyToCclsRenamePreflight(t *testing.T, frames <-chan string, child io.Writer, uri string, kind languages.SymbolKind) {
	t.Helper()
	// The current file defines `Target` at 0:4-10. The definition response and
	// document-symbol selection range must point to that exact token.
	definition := []map[string]any{{
		"uri": uri,
		"range": map[string]any{
			"start": map[string]uint32{"line": 0, "character": 4},
			"end":   map[string]uint32{"line": 0, "character": 10},
		},
	}}
	replyToCclsRequest(t, frames, child, "textDocument/definition", definition)
	documentSymbols := []map[string]any{{
		"name": "Target", "kind": int(kind),
		"range": map[string]any{
			"start": map[string]uint32{"line": 0, "character": 0},
			"end":   map[string]uint32{"line": 0, "character": 18},
		},
		"selectionRange": map[string]any{
			"start": map[string]uint32{"line": 0, "character": 4},
			"end":   map[string]uint32{"line": 0, "character": 10},
		},
	}}
	replyToCclsRequest(t, frames, child, "textDocument/documentSymbol", documentSymbols)
}

func startCclsRenameProtocolTest(t *testing.T, kind languages.SymbolKind) (*Backend, *cclsProtocolWriter, io.WriteCloser, <-chan struct {
	result identity.SemanticResult[languages.ValidatedEdit]
	err    error
}, string) {
	t.Helper()
	b, stdin, child := newCclsProtocolBackend(t)
	enableCclsRenameForProtocolTest(t, b)
	const uri = "file:///x/main.cpp"
	got := make(chan struct {
		result identity.SemanticResult[languages.ValidatedEdit]
		err    error
	}, 1)
	go func() {
		result, err := b.Rename(context.Background(), languages.RenameRequest{
			URI: uri, Content: []byte("int Target(void);\n"), SnapshotRev: 3,
			Line: 0, Column: 5, NewName: "Renamed",
		})
		got <- struct {
			result identity.SemanticResult[languages.ValidatedEdit]
			err    error
		}{result: result, err: err}
	}()
	replyToCclsRenamePreflight(t, stdin.frames, child, uri, kind)
	return b, stdin, child, got, uri
}

func assertNoCclsRenameRequest(t *testing.T, frames <-chan string) {
	t.Helper()
	select {
	case frame := <-frames:
		separator := strings.Index(frame, "\r\n\r\n")
		if separator < 0 {
			t.Fatalf("malformed LSP frame %q", frame)
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal([]byte(frame[separator+4:]), &message); err != nil {
			t.Fatalf("decode LSP frame: %v", err)
		}
		var method string
		_ = json.Unmarshal(message["method"], &method)
		if method == "textDocument/rename" {
			t.Fatal("unsafe function-like rename reached clangd")
		}
		t.Fatalf("unexpected child message after rename refusal: method=%q", method)
	case <-time.After(100 * time.Millisecond):
	}
}

func awaitCclsRenameResult(t *testing.T, got <-chan struct {
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
		t.Fatal("rename request did not complete")
		return struct {
			result identity.SemanticResult[languages.ValidatedEdit]
			err    error
		}{}
	}
}

func TestRenameFunctionLikeTargetsFailClosedBeforeRename(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind languages.SymbolKind
	}{
		{name: "function", kind: languages.SymbolFunction},
		{name: "method", kind: languages.SymbolMethod},
		{name: "constructor", kind: languages.SymbolConstructor},
		{name: "operator", kind: cclsFunctionLikeSymbolOperator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stdin, _, got, _ := startCclsRenameProtocolTest(t, tc.kind)
			response := awaitCclsRenameResult(t, got)
			if response.err != nil {
				t.Fatalf("rename returned Go error: %v", response.err)
			}
			if response.result.Status != identity.ResultUnavailable || response.result.Value.Complete {
				t.Fatalf("rename result = %+v, want unavailable and incomplete", response.result)
			}
			if len(response.result.Evidence) != 1 || response.result.Evidence[0].DetailCode != "rename-function-collision-unproven" {
				t.Fatalf("rename evidence = %+v, want typed function-collision refusal", response.result.Evidence)
			}
			const want = "rename refused: C/C++ function/method collision analysis is not proven (SEM-SAFE-001)"
			if len(response.result.InternalDiagnostics) != 1 || response.result.InternalDiagnostics[0] != want {
				t.Fatalf("rename diagnostics = %v, want [%q]", response.result.InternalDiagnostics, want)
			}
			assertNoCclsRenameRequest(t, stdin.frames)
		})
	}
}

func TestRenameClassifiedNonFunctionAndNullResponse(t *testing.T) {
	t.Run("classified variable keeps supported rename path", func(t *testing.T) {
		_, stdin, child, got, uri := startCclsRenameProtocolTest(t, languages.SymbolVariable)
		edit := map[string]any{"changes": map[string]any{uri: []map[string]any{{
			"range": map[string]any{
				"start": map[string]uint32{"line": 0, "character": 4},
				"end":   map[string]uint32{"line": 0, "character": 10},
			},
			"newText": "Renamed",
		}}}}
		replyToCclsRequest(t, stdin.frames, child, "textDocument/rename", edit)
		response := awaitCclsRenameResult(t, got)
		if response.err != nil || response.result.Status != identity.ResultExact || !response.result.Value.Complete {
			t.Fatalf("classified non-function rename = %+v, err=%v; want exact complete edit", response.result, response.err)
		}
		if len(response.result.Value.Edits) != 1 || response.result.Value.Edits[0].NewText != "Renamed" {
			t.Fatalf("classified non-function edits = %+v, want one upstream edit", response.result.Value.Edits)
		}
	})

	t.Run("JSON null is unavailable", func(t *testing.T) {
		_, stdin, child, got, _ := startCclsRenameProtocolTest(t, languages.SymbolVariable)
		replyToCclsRequest(t, stdin.frames, child, "textDocument/rename", json.RawMessage("null"))
		response := awaitCclsRenameResult(t, got)
		if response.err != nil {
			t.Fatalf("null rename returned Go error: %v", response.err)
		}
		if response.result.Status != identity.ResultUnavailable || response.result.Value.Complete {
			t.Fatalf("null rename result = %+v, want unavailable and incomplete", response.result)
		}
		if len(response.result.Evidence) != 1 || response.result.Evidence[0].DetailCode != "clangd-refused" {
			t.Fatalf("null rename evidence = %+v, want clangd-refused", response.result.Evidence)
		}
		if len(response.result.InternalDiagnostics) != 1 || response.result.InternalDiagnostics[0] != "upstream language service refused rename" {
			t.Fatalf("null rename diagnostics = %v, want explicit upstream refusal", response.result.InternalDiagnostics)
		}
	})
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
				"label": "printf", "kind": 3, "detail": "int (const char *, ...)",
				"documentation": "printf docs", "insertText": "printf($0)", "sortText": "01", "filterText": "pr"},
			},
			documentation: "printf docs",
		},
		{
			name: "completion list",
			result: map[string]any{
				"isIncomplete": true,
				"items": []map[string]any{{
					"label": "std", "kind": 9, "detail": "namespace std",
					"documentation": map[string]any{"kind": "markdown", "value": "**namespace std**"},
					"insertText":    "std::", "sortText": "02", "filterText": "std"},
				},
			},
			incomplete: true, documentation: "**namespace std**",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, stdin, child := newCclsProtocolBackend(t)
			got := make(chan struct {
				list languages.CompletionList
				err  error
			}, 1)
			go func() {
				list, err := b.CompletionList(context.Background(), languages.CompletionRequest{
					URI: "file:///x/main.cpp", Content: []byte("pri\n"), SnapshotRev: 2,
				})
				got <- struct {
					list languages.CompletionList
					err  error
				}{list: list, err: err}
			}()
			replyToCclsRequest(t, stdin.frames, child, "textDocument/completion", tc.result)
			select {
			case response := <-got:
				if response.err != nil {
					t.Fatalf("completion returned Go error: %v", response.err)
				}
				if len(response.list.Items) != 1 || response.list.Items[0].Label == "" || response.list.Items[0].Kind == 0 || response.list.Items[0].Detail == "" {
					t.Fatalf("completion = %+v, want one projected item", response.list)
				}
				item := response.list.Items[0]
				if response.list.IsIncomplete != tc.incomplete || item.Documentation != tc.documentation || item.InsertText == "" || item.SortText == "" || item.FilterText == "" {
					t.Fatalf("completion metadata was not preserved: %+v", response.list)
				}
			case <-time.After(time.Second):
				t.Fatal("completion request did not complete")
			}
		})
	}
}

func TestX4_DocumentSymbolsPreserveSelectionChildrenAndFlatRanges(t *testing.T) {
	t.Run("document symbols", func(t *testing.T) {
		b, stdin, child := newCclsProtocolBackend(t)
		got := make(chan struct {
			syms []languages.DocumentSymbol
			err  error
		}, 1)
		go func() {
			syms, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
				URI: "file:///x/main.cpp", Content: []byte("int value = 1;\n"), SnapshotRev: 3,
			})
			got <- struct {
				syms []languages.DocumentSymbol
				err  error
			}{syms: syms, err: err}
		}()
		replyToCclsRequest(t, stdin.frames, child, "textDocument/documentSymbol", []map[string]any{{
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
		}})
		select {
		case response := <-got:
			if response.err != nil {
				t.Fatalf("document symbols returned Go error: %v", response.err)
			}
			if len(response.syms) != 1 || response.syms[0].Detail != "module" || response.syms[0].SelectionLine != 1 || response.syms[0].SelectionCharacter != 4 {
				t.Fatalf("document symbols = %+v, want preserved selection", response.syms)
			}
			if !response.syms[0].SelectionRangeSet || response.syms[0].SelectionEndLine != 1 || response.syms[0].SelectionEndCharacter != 10 {
				t.Fatalf("document symbol selection end = %+v, want upstream end 1:10", response.syms[0])
			}
			if len(response.syms[0].Children) != 1 || response.syms[0].Children[0].Name != "value" || response.syms[0].Children[0].SelectionCharacter != 2 || response.syms[0].Children[0].SelectionEndCharacter != 7 {
				t.Fatalf("document symbol children = %+v, want nested child and selection", response.syms[0].Children)
			}
		case <-time.After(time.Second):
			t.Fatal("document symbol request did not complete")
		}
	})

	t.Run("symbol information", func(t *testing.T) {
		b, stdin, child := newCclsProtocolBackend(t)
		got := make(chan struct {
			syms []languages.DocumentSymbol
			err  error
		}, 1)
		go func() {
			syms, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
				URI: "file:///x/main.cpp", Content: []byte("int value = 1;\n"), SnapshotRev: 3,
			})
			got <- struct {
				syms []languages.DocumentSymbol
				err  error
			}{syms: syms, err: err}
		}()
		replyToCclsRequest(t, stdin.frames, child, "textDocument/documentSymbol", []map[string]any{{
			"name": "Flat", "kind": 12,
			"location": map[string]any{
				"uri": "file:///x/main.cpp",
				"range": map[string]any{
					"start": map[string]uint32{"line": 5, "character": 6},
					"end":   map[string]uint32{"line": 5, "character": 10},
				},
			},
		}})
		select {
		case response := <-got:
			if response.err != nil {
				t.Fatalf("flat document symbols returned Go error: %v", response.err)
			}
			if len(response.syms) != 1 || response.syms[0].Name != "Flat" || response.syms[0].StartLine != 5 || response.syms[0].SelectionCharacter != 6 || response.syms[0].SelectionEndCharacter != 10 {
				t.Fatalf("flat document symbols = %+v, want location range as selection", response.syms)
			}
		case <-time.After(time.Second):
			t.Fatal("flat document symbol request did not complete")
		}
	})
}

func TestS18NestedTraceCorrelatesParentRequestIDs(t *testing.T) {
	tracePath := filepath.Join(t.TempDir(), "nested-rpc-timing.json")
	t.Setenv("OMNILSP_S18_NESTED_RPC_TRACE", tracePath)
	b, stdin, child := newCclsProtocolBackend(t)

	const uri = "file:///x/main.cpp"
	completionDone := make(chan error, 1)
	go func() {
		_, err := b.CompletionList(context.Background(), languages.CompletionRequest{
			URI: uri, Content: []byte("int first;\n"), SnapshotRev: 1,
			ParentRequestID: json.RawMessage("41"),
		})
		completionDone <- err
	}()
	replyToCclsRequest(t, stdin.frames, child, "textDocument/completion", []map[string]any{})
	if err := <-completionDone; err != nil {
		t.Fatalf("CompletionList: %v", err)
	}

	documentSymbolsDone := make(chan error, 1)
	go func() {
		_, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
			URI: uri, Content: []byte("int first;\nint second;\n"), SnapshotRev: 2,
			ParentRequestID: json.RawMessage(`"syntax-42"`),
		})
		documentSymbolsDone <- err
	}()
	change := readCclsProtocolMessage(t, stdin.frames)
	var changeMethod string
	if err := json.Unmarshal(change["method"], &changeMethod); err != nil || changeMethod != "textDocument/didChange" {
		t.Fatalf("document-symbol snapshot change method = %q, err %v", changeMethod, err)
	}
	replyToCclsRequest(t, stdin.frames, child, "textDocument/documentSymbol", []map[string]any{})
	if err := <-documentSymbolsDone; err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	closeCclsProtocolBackend(t, b, stdin, child)

	data, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read nested trace: %v", err)
	}
	var trace struct {
		Language         string `json:"language"`
		SampleLimit      int    `json:"sample_limit"`
		SamplesTruncated bool   `json:"samples_truncated"`
		Samples          []struct {
			Method          string          `json:"method"`
			ParentRequestID json.RawMessage `json:"parent_request_id"`
			Outcome         string          `json:"outcome"`
		} `json:"samples"`
	}
	if err := json.Unmarshal(data, &trace); err != nil {
		t.Fatalf("decode nested trace: %v", err)
	}
	if trace.Language != "cpp" || trace.SampleLimit < 15000 || trace.SamplesTruncated {
		t.Fatalf("trace metadata = language %q limit %d truncated %t", trace.Language, trace.SampleLimit, trace.SamplesTruncated)
	}
	if len(trace.Samples) != 2 {
		t.Fatalf("trace samples = %+v, want completion and documentSymbol", trace.Samples)
	}
	got := make(map[string]struct {
		parentID json.RawMessage
		outcome  string
	})
	for _, sample := range trace.Samples {
		got[sample.Method] = struct {
			parentID json.RawMessage
			outcome  string
		}{parentID: sample.ParentRequestID, outcome: sample.Outcome}
	}
	if sample := got["textDocument/completion"]; string(sample.parentID) != "41" || sample.outcome != "response" {
		t.Errorf("completion trace = parent %s outcome %q, want 41 response", sample.parentID, sample.outcome)
	}
	if sample := got["textDocument/documentSymbol"]; string(sample.parentID) != `"syntax-42"` || sample.outcome != "response" {
		t.Errorf("documentSymbol trace = parent %s outcome %q, want \"syntax-42\" response", sample.parentID, sample.outcome)
	}
}

func TestX4_ReferencesDeduplicateExactLocations(t *testing.T) {
	b, stdin, child := newCclsProtocolBackend(t)
	if b.SupervisorEpoch() != 1 {
		t.Fatalf("supervisor epoch = %d, want ready epoch 1", b.SupervisorEpoch())
	}
	got := make(chan struct {
		result identity.SemanticResult[[]languages.Location]
		err    error
	}, 1)
	go func() {
		result, err := b.References(context.Background(), languages.ReferencesRequest{
			URI: "file:///x/main.cpp", Content: []byte("int value = 1;\n"), SnapshotRev: 3,
		})
		got <- struct {
			result identity.SemanticResult[[]languages.Location]
			err    error
		}{result: result, err: err}
	}()
	replyToCclsRequest(t, stdin.frames, child, "textDocument/references", []map[string]any{
		{
			"uri": "file:///x/a.cpp",
			"range": map[string]any{
				"start": map[string]uint32{"line": 1, "character": 2},
				"end":   map[string]uint32{"line": 1, "character": 5},
			},
		},
		{
			"uri": "file:///x/a.cpp",
			"range": map[string]any{
				"start": map[string]uint32{"line": 1, "character": 2},
				"end":   map[string]uint32{"line": 1, "character": 5},
			},
		},
		{
			"uri": "file:///x/a.cpp",
			"range": map[string]any{
				"start": map[string]uint32{"line": 1, "character": 3},
				"end":   map[string]uint32{"line": 1, "character": 5},
			},
		},
		{
			"uri": "file:///x/b.cpp",
			"range": map[string]any{
				"start": map[string]uint32{"line": 1, "character": 2},
				"end":   map[string]uint32{"line": 1, "character": 5},
			},
		},
	})
	select {
	case response := <-got:
		if response.err != nil {
			t.Fatalf("references returned Go error: %v", response.err)
		}
		if len(response.result.Value) != 3 {
			t.Fatalf("references = %+v, want three distinct locations", response.result.Value)
		}
		if len(response.result.Evidence) == 0 || response.result.Evidence[0].BackendEpoch != 1 {
			t.Fatalf("references evidence = %+v, want request epoch 1", response.result.Evidence)
		}
		if response.result.Value[0] == response.result.Value[1] || response.result.Value[1] == response.result.Value[2] || response.result.Value[0] == response.result.Value[2] {
			t.Fatalf("distinct reference locations were collapsed: %+v", response.result.Value)
		}
	case <-time.After(time.Second):
		t.Fatal("references request did not complete")
	}
}
