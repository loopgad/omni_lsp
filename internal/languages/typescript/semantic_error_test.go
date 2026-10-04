package typescript

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

func TestDefinitionAndReferencesPropagateChildErrors(t *testing.T) {
	const uri = "file:///x/main.ts"
	lookups := []struct {
		name          string
		method        string
		successStatus identity.ResultStatus
		call          func(*Backend) (identity.SemanticResult[[]languages.Location], error)
	}{
		{
			name:          "definition",
			method:        "textDocument/definition",
			successStatus: identity.ResultExact,
			call: func(b *Backend) (identity.SemanticResult[[]languages.Location], error) {
				return b.Definition(context.Background(), languages.DefinitionRequest{URI: uri, Content: []byte("const value = 1;\n"), SnapshotRev: 1})
			},
		},
		{
			name:          "references",
			method:        "textDocument/references",
			successStatus: identity.ResultPartial,
			call: func(b *Backend) (identity.SemanticResult[[]languages.Location], error) {
				return b.References(context.Background(), languages.ReferencesRequest{URI: uri, Content: []byte("const value = 1;\n"), SnapshotRev: 1})
			},
		},
	}

	for _, lookup := range lookups {
		t.Run(lookup.name, func(t *testing.T) {
			t.Run("request error", func(t *testing.T) {
				b, stdin, child := newTestBackend(t)
				go serve(stdin, child, map[string]any{})

				got, err := lookup.call(b)
				if err == nil || !strings.Contains(err.Error(), "refused by fake server") {
					t.Fatalf("request error = %v, want child failure preserved", err)
				}
				assertUnknownLookup(t, got)
			})

			t.Run("decode error", func(t *testing.T) {
				b, stdin, child := newTestBackend(t)
				go serve(stdin, child, map[string]any{lookup.method: map[string]any{"unexpected": true}})

				got, err := lookup.call(b)
				var decodeErr *json.UnmarshalTypeError
				if !errors.As(err, &decodeErr) {
					t.Fatalf("decode error = %v, want original JSON unmarshal error", err)
				}
				assertUnknownLookup(t, got)
			})

			t.Run("valid empty result", func(t *testing.T) {
				b, stdin, child := newTestBackend(t)
				go serve(stdin, child, map[string]any{lookup.method: nil})

				got, err := lookup.call(b)
				if err != nil || got.Status != lookup.successStatus || len(got.Value) != 0 {
					t.Fatalf("empty result = (%+v, %v), want successful empty result", got, err)
				}
			})
		})
	}
}

func assertUnknownLookup(t *testing.T, got identity.SemanticResult[[]languages.Location]) {
	t.Helper()
	if got.Status != identity.ResultUnknown || got.Completeness != identity.CompletenessUnknown {
		t.Fatalf("error result = %+v, want unknown", got)
	}
}

func TestHoverPropagatesChildErrors(t *testing.T) {
	const uri = "file:///x/main.ts"
	for _, tc := range []struct {
		name    string
		result  any
		request bool
	}{
		{name: "request error", request: true},
		{name: "decode error", result: map[string]any{"contents": map[string]any{"value": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, stdin, child := newTestBackend(t)
			if tc.request {
				go serve(stdin, child, map[string]any{})
			} else {
				go serve(stdin, child, map[string]any{"textDocument/hover": tc.result})
			}
			got, err := b.Hover(context.Background(), languages.HoverRequest{
				URI: uri, Content: []byte("const value = 1;\n"), SnapshotRev: 1,
			})
			if tc.request {
				if err == nil || !strings.Contains(err.Error(), "refused by fake server") {
					t.Fatalf("hover request error = %v, want child failure preserved", err)
				}
			} else {
				var decodeErr *json.UnmarshalTypeError
				if !errors.As(err, &decodeErr) {
					t.Fatalf("hover decode error = %v, want original JSON unmarshal error", err)
				}
			}
			assertUnknownHover(t, got)
		})
	}
}

func TestRenamePropagatesChildRequestAndDecodeErrors(t *testing.T) {
	const uri = "file:///x/main.ts"
	content := []byte("const Target = 1;\n")
	token := languages.Range{StartLine: 0, StartCharacter: 6, EndLine: 0, EndCharacter: 12}
	for _, tc := range []struct {
		name    string
		request bool
	}{
		{name: "request error", request: true},
		{name: "decode error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stdin, child, got := startTSRenameProtocolTest(t, uri, content, token, languages.SymbolVariable)
			if tc.request {
				replyToTSRequestError(t, stdin.frames, child, "textDocument/rename")
			} else {
				replyToTSRenameRequest(t, stdin.frames, child, "textDocument/rename", map[string]any{
					"changes": map[string]any{uri: "not-an-edit-list"},
				})
			}
			response := waitTSRenameResult(t, got)
			if tc.request {
				if response.err == nil || !strings.Contains(response.err.Error(), "fake child request failed") {
					t.Fatalf("rename request error = %v, want child failure preserved", response.err)
				}
			} else {
				var decodeErr *json.UnmarshalTypeError
				if !errors.As(response.err, &decodeErr) {
					t.Fatalf("rename decode error = %v, want original JSON unmarshal error", response.err)
				}
			}
			if response.result.Status != identity.ResultUnavailable {
				t.Fatalf("rename error result = %+v, want unavailable", response.result)
			}
		})
	}
}

func TestRenameClassificationRequestErrorPropagates(t *testing.T) {
	const uri = "file:///x/main.ts"
	b, stdin, child := newTestBackend(t)
	enableTSRenameForProtocolTest(t, b)
	got := make(chan struct {
		result identity.SemanticResult[languages.ValidatedEdit]
		err    error
	}, 1)
	go func() {
		result, err := b.Rename(context.Background(), languages.RenameRequest{
			URI: uri, Content: []byte("const Target = 1;\n"), SnapshotRev: 1,
			Line: 0, Column: 6, NewName: "Renamed",
		})
		got <- struct {
			result identity.SemanticResult[languages.ValidatedEdit]
			err    error
		}{result: result, err: err}
	}()
	replyToTSRequestError(t, stdin.frames, child, "textDocument/definition")
	response := waitTSRenameResult(t, got)
	if response.err == nil || !strings.Contains(response.err.Error(), "fake child request failed") {
		t.Fatalf("rename classification error = %v, want definition failure preserved", response.err)
	}
	if response.result.Status != identity.ResultUnavailable {
		t.Fatalf("rename classification result = %+v, want unavailable", response.result)
	}
}

func replyToTSRequestError(t *testing.T, frames <-chan string, out io.Writer, method string) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case frame := <-frames:
			var request struct {
				ID     *int64 `json:"id"`
				Method string `json:"method"`
			}
			if err := json.Unmarshal([]byte(frameBody(frame)), &request); err != nil {
				t.Fatalf("decode child request: %v", err)
			}
			if request.ID == nil || request.Method == "" {
				continue
			}
			if request.Method != method {
				t.Fatalf("child request = %+v, want %s", request, method)
			}
			writeErrorFrame(out, *request.ID, "fake child request failed")
			return
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", method)
		}
	}
}

func assertUnknownHover(t *testing.T, got identity.SemanticResult[*languages.HoverResult]) {
	t.Helper()
	if got.Status != identity.ResultUnknown || got.Completeness != identity.CompletenessUnknown {
		t.Fatalf("hover error result = %+v, want unknown", got)
	}
}
