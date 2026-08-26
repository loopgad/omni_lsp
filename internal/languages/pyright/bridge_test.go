package pyright

// X4/S3 protocol tests for the pyright bridge. All run against an injected
// fake server — no real toolchain required. The fake answers the exact
// request ids the bridge generates, so nothing races the pending table
// under -race.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		Name:           serverName,
		Lang:           langID,
		WorkDir:        t.TempDir(),
		RequestTimeout: 2 * time.Second,
	})
	conn.Attach(nil, stdin, pr)
	b := &Backend{conn: conn, workDir: t.TempDir(), cfgFile: "pyproject.toml"}
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

func TestX4_HoverProjectsEnvelope(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	go serve(stdin, pw, map[string]any{
		"textDocument/hover": map[string]any{"contents": map[string]string{"value": "def hello() -> str"}},
	})

	res, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: "file:///x/main.py", Content: []byte("print('hi')\n"),
		SnapshotRev: 7, BuildContext: identity.BuildContextID("python:sha256:test"),
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
			"uri": "file:///x/lib.py",
			"range": map[string]any{
				"start": map[string]uint32{"line": 1, "character": 2},
				"end":   map[string]uint32{"line": 1, "character": 8},
			},
		}},
	})

	res, err := b.Definition(context.Background(), languages.DefinitionRequest{
		URI: "file:///x/main.py", Content: []byte("print('hi')\n"),
		SnapshotRev: 3, BuildContext: identity.BuildContextID("python:sha256:test"),
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
	if loc.URI != "file:///x/lib.py" {
		t.Fatalf("uri = %q", loc.URI)
	}
	if loc.Range.StartLine != 1 || loc.Range.StartCharacter != 2 ||
		loc.Range.EndLine != 1 || loc.Range.EndCharacter != 8 {
		t.Fatalf("range = %+v", loc.Range)
	}
	if res.Evidence[0].Assurance != identity.AssuranceCompilerResolved {
		t.Fatalf("assurance = %v, want compilerResolved", res.Evidence[0].Assurance)
	}
}

func TestX4_ErrorResponseSurfacesUnavailable(t *testing.T) {
	b, stdin, pw := newTestBackend(t)
	// No canned hover key → the fake answers every request with an error frame.
	go serve(stdin, pw, nil)

	res, err := b.Hover(context.Background(), languages.HoverRequest{
		URI: "file:///x/main.py", Content: []byte("print('hi')\n"),
		SnapshotRev: 1, BuildContext: identity.BuildContextID("python:sha256:test"),
	})
	if err != nil {
		t.Fatalf("semantic refusal leaked into Go error: %v", err)
	}
	if res.Status == identity.ResultExact {
		t.Fatal("error response must not project an exact envelope")
	}
	if len(res.InternalDiagnostics) == 0 {
		t.Fatal("internal diagnostics must record the upstream refusal")
	}
}

func TestS3_RenameFailClosedWithoutBuildFile(t *testing.T) {
	b, _, _ := newTestBackend(t) // workdir is fresh — pyproject.toml absent

	res, err := b.Rename(context.Background(), languages.RenameRequest{
		URI: "file:///x/main.py", Content: []byte("print('hi')\n"),
		SnapshotRev: 1, BuildContext: identity.BuildContextID("python:sha256:test"),
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
		if strings.Contains(d, "pyproject.toml") {
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
