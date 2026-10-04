package ccls

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/nested"
)

func TestBeginWorkspaceSnapshotNormalizesCLanguageIDs(t *testing.T) {
	stdout, child := io.Pipe()
	stdin := &lifecycleWriter{frames: make(chan string, 8)}
	conn := nested.New(nested.Config{Name: "clangd", Lang: "cpp", WorkDir: t.TempDir()})
	conn.Attach(nil, stdin, stdout)
	b := &Backend{conn: conn}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = child.Close()
	})

	const revision = 7
	documents := []languages.WorkspaceDocument{
		{URI: "file:///workspace/a.c", LanguageID: "c", Content: []byte("int c_value;\n")},
		{URI: "file:///workspace/b.cpp", LanguageID: "cpp", Content: []byte("int cpp_value;\n")},
	}
	leaseCtx, finish, err := b.BeginWorkspaceSnapshot(context.Background(), languages.WorkspaceSnapshot{
		Revision: revision, Documents: documents,
	})
	if err != nil {
		t.Fatalf("begin workspace snapshot: %v", err)
	}
	defer finish()

	wantDocuments := map[string]bool{
		documents[0].URI: true,
		documents[1].URI: true,
	}
	for range documents {
		message := readLifecycleMessage(t, stdin.frames)
		var method string
		if err := json.Unmarshal(message["method"], &method); err != nil {
			t.Fatalf("decode didOpen method: %v", err)
		}
		var params struct {
			TextDocument struct {
				URI        string `json:"uri"`
				LanguageID string `json:"languageId"`
			} `json:"textDocument"`
		}
		if err := json.Unmarshal(message["params"], &params); err != nil {
			t.Fatalf("decode didOpen params: %v", err)
		}
		if method != "textDocument/didOpen" || params.TextDocument.LanguageID != "cpp" || !wantDocuments[params.TextDocument.URI] {
			t.Fatalf("snapshot open = method %q, URI %q, language %q; want a C/C++ didOpen using cpp", method, params.TextDocument.URI, params.TextDocument.LanguageID)
		}
		delete(wantDocuments, params.TextDocument.URI)
	}
	if len(wantDocuments) != 0 {
		t.Fatalf("snapshot omitted C/C++ documents: %v", wantDocuments)
	}

	for _, doc := range documents {
		if _, err := b.DiagnosticsWithEncoding(leaseCtx, doc.URI, doc.Content, revision, 1); err != nil {
			t.Fatalf("diagnostics for %s using the snapshot lease: %v", doc.URI, err)
		}
	}
}

func TestHoverRejectsOlderRevisionBeforeChildRequest(t *testing.T) {
	stdout, child := io.Pipe()
	stdin := &lifecycleWriter{frames: make(chan string, 4)}
	conn := nested.New(nested.Config{Name: "clangd", Lang: "cpp", WorkDir: t.TempDir()})
	conn.Attach(nil, stdin, stdout)
	b := &Backend{conn: conn}
	t.Cleanup(func() {
		_ = conn.Close()
		_ = child.Close()
	})

	const uri = "file:///workspace/main.cpp"
	if err := b.didOpen(uri, []byte("int newer;\n"), 2); err != nil {
		t.Fatalf("initial document sync: %v", err)
	}
	select {
	case <-stdin.frames:
	case <-time.After(time.Second):
		t.Fatal("initial document sync was not sent")
	}

	result, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: uri, Content: []byte("int older;\n"), SnapshotRev: 1,
	})
	if !ierrors.IsKind(err, ierrors.ErrContentModified) {
		t.Fatalf("stale hover error = %v, want ErrContentModified", err)
	}
	if result.Status == identity.ResultExact || result.Value != nil {
		t.Fatalf("stale hover returned a usable result: %+v", result)
	}
	select {
	case frame := <-stdin.frames:
		t.Fatalf("stale hover sent child frame %q", frame)
	case <-time.After(200 * time.Millisecond):
	}
}
