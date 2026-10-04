package typescript

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

func TestBeginWorkspaceSnapshotPreservesDocumentLanguageIDs(t *testing.T) {
	tests := []struct {
		name       string
		languageID string
		wantID     string
		uri        string
	}{
		{name: "typescript", languageID: "typescript", wantID: "typescript", uri: "file:///workspace/main.ts"},
		{name: "javascript", languageID: "javascript", wantID: "javascript", uri: "file:///workspace/main.js"},
		{name: "tsx", languageID: "typescriptreact", wantID: "typescriptreact", uri: "file:///workspace/main.tsx"},
		{name: "jsx", languageID: "javascriptreact", wantID: "javascriptreact", uri: "file:///workspace/main.jsx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, stdin, pw := newTestBackend(t)
			const revision = 11
			content := []byte("const value = 1;\n")
			leaseCtx, finish, err := b.BeginWorkspaceSnapshot(context.Background(), languages.WorkspaceSnapshot{
				Revision: revision,
				Documents: []languages.WorkspaceDocument{{
					URI: tt.uri, LanguageID: tt.languageID, Content: content,
				}},
			})
			if err != nil {
				t.Fatalf("begin workspace snapshot: %v", err)
			}
			defer finish()

			var opened struct {
				Method string `json:"method"`
				Params struct {
					TextDocument struct {
						URI        string `json:"uri"`
						LanguageID string `json:"languageId"`
						Version    int32  `json:"version"`
					} `json:"textDocument"`
				} `json:"params"`
			}
			select {
			case frame := <-stdin.frames:
				if err := json.Unmarshal([]byte(frameBody(frame)), &opened); err != nil {
					t.Fatalf("decode didOpen: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("snapshot didOpen was not sent")
			}
			if opened.Method != "textDocument/didOpen" || opened.Params.TextDocument.URI != tt.uri || opened.Params.TextDocument.LanguageID != tt.wantID || opened.Params.TextDocument.Version != 1 {
				t.Fatalf("snapshot didOpen = %+v, want language ID %q", opened, tt.wantID)
			}

			type diagnosticsResult struct {
				items []languages.Diagnostic
				err   error
			}
			got := make(chan diagnosticsResult, 1)
			go func() {
				items, err := b.DiagnosticsWithEncoding(leaseCtx, tt.uri, content, revision, 0)
				got <- diagnosticsResult{items: items, err: err}
			}()

			params, err := json.Marshal(map[string]any{
				"uri": tt.uri, "version": opened.Params.TextDocument.Version,
				"diagnostics": []any{},
			})
			if err != nil {
				t.Fatalf("encode publishDiagnostics: %v", err)
			}
			frameMessage(pw, &jsonrpc.Message{JSONRPC: jsonrpc.Version, Method: "textDocument/publishDiagnostics", Params: params})
			select {
			case result := <-got:
				if result.err != nil {
					t.Fatalf("diagnostics for %s: %v", tt.languageID, result.err)
				}
				if len(result.items) != 0 {
					t.Fatalf("diagnostics = %+v, want empty result", result.items)
				}
			case <-time.After(time.Second):
				t.Fatal("diagnostics did not finish after publishDiagnostics")
			}
		})
	}
}

func TestDocumentLanguageIDUsesURIExtension(t *testing.T) {
	for _, tc := range []struct {
		uri  string
		want string
	}{
		{"file:///workspace/main.ts", "typescript"},
		{"file:///workspace/main.TSX", "typescriptreact"},
		{"file:///workspace/main.js", "javascript"},
		{"file:///workspace/main.JSX", "javascriptreact"},
		{"file:///workspace/types.d.ts", "typescript"},
		{"untitled:Untitled-1", "typescript"},
	} {
		t.Run(tc.uri, func(t *testing.T) {
			if got := documentLanguageID(tc.uri); got != tc.want {
				t.Fatalf("documentLanguageID(%q) = %q, want %q", tc.uri, got, tc.want)
			}
		})
	}
}

func TestDocumentSyncUsesLanguageIDForDocumentExtension(t *testing.T) {
	b, stdin, _ := newTestBackend(t)
	for _, tc := range []struct {
		uri  string
		want string
	}{
		{"file:///workspace/main.ts", "typescript"},
		{"file:///workspace/main.tsx", "typescriptreact"},
		{"file:///workspace/main.js", "javascript"},
		{"file:///workspace/main.jsx", "javascriptreact"},
	} {
		if err := b.didOpen(tc.uri, []byte("const value = 1;\n"), 1); err != nil {
			t.Fatalf("didOpen %s: %v", tc.uri, err)
		}
		var opened struct {
			Method string `json:"method"`
			Params struct {
				TextDocument struct {
					URI        string `json:"uri"`
					LanguageID string `json:"languageId"`
				} `json:"textDocument"`
			} `json:"params"`
		}
		select {
		case frame := <-stdin.frames:
			if err := json.Unmarshal([]byte(frameBody(frame)), &opened); err != nil {
				t.Fatalf("decode didOpen for %s: %v", tc.uri, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("didOpen was not sent for %s", tc.uri)
		}
		if opened.Method != "textDocument/didOpen" || opened.Params.TextDocument.URI != tc.uri || opened.Params.TextDocument.LanguageID != tc.want {
			t.Fatalf("didOpen = %+v, want language ID %q", opened, tc.want)
		}
	}
}

func TestHoverRejectsOlderRevisionBeforeChildRequest(t *testing.T) {
	b, stdin, _ := newTestBackend(t)
	const uri = "file:///workspace/main.ts"

	if err := b.didOpen(uri, []byte("const newer = 1;\n"), 2); err != nil {
		t.Fatalf("initial document sync: %v", err)
	}
	select {
	case <-stdin.frames:
	case <-time.After(time.Second):
		t.Fatal("initial document sync was not sent")
	}

	result, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: uri, Content: []byte("const older = 1;\n"), SnapshotRev: 1,
	})
	if !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("stale hover error = %v, want ErrContentModified", err)
	}
	if result.Status == identity.ResultExact || result.Value != nil {
		t.Fatalf("stale hover returned a usable result: %+v", result)
	}
	select {
	case frame := <-stdin.frames:
		t.Fatalf("stale hover sent child frame %q", frameBody(frame))
	case <-time.After(200 * time.Millisecond):
	}
}
