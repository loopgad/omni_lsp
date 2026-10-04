package pyright

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

func TestDefinitionAndReferencesPropagateChildErrors(t *testing.T) {
	const uri = "file:///x/main.py"
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
				return b.Definition(context.Background(), languages.DefinitionRequest{URI: uri, Content: []byte("value = 1\n"), SnapshotRev: 1})
			},
		},
		{
			name:          "references",
			method:        "textDocument/references",
			successStatus: identity.ResultPartial,
			call: func(b *Backend) (identity.SemanticResult[[]languages.Location], error) {
				return b.References(context.Background(), languages.ReferencesRequest{URI: uri, Content: []byte("value = 1\n"), SnapshotRev: 1})
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
	const uri = "file:///x/main.py"
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
				URI: uri, Content: []byte("value = 1\n"), SnapshotRev: 1,
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
	const uri = "file:///x/main.py"
	for _, tc := range []struct {
		name    string
		result  any
		request bool
	}{
		{name: "request error", request: true},
		{name: "decode error", result: map[string]any{"changes": map[string]any{uri: "not-an-edit-list"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, stdin, child := newTestBackend(t)
			if err := os.WriteFile(filepath.Join(b.workDir, b.cfgFile), []byte("[tool.pyright]\n"), 0o600); err != nil {
				t.Fatalf("write project marker: %v", err)
			}
			if tc.request {
				go serve(stdin, child, map[string]any{})
			} else {
				go serve(stdin, child, map[string]any{"textDocument/rename": tc.result})
			}
			got, err := b.Rename(context.Background(), languages.RenameRequest{
				URI: uri, Content: []byte("value = original\n"), SnapshotRev: 1,
				Line: 0, Column: 8, NewName: "replacement",
			})
			if tc.request {
				if err == nil || !strings.Contains(err.Error(), "refused by fake server") {
					t.Fatalf("rename request error = %v, want child failure preserved", err)
				}
			} else {
				var decodeErr *json.UnmarshalTypeError
				if !errors.As(err, &decodeErr) {
					t.Fatalf("rename decode error = %v, want original JSON unmarshal error", err)
				}
			}
			if got.Status != identity.ResultUnavailable {
				t.Fatalf("rename error result = %+v, want unavailable", got)
			}
		})
	}
}

func assertUnknownHover(t *testing.T, got identity.SemanticResult[*languages.HoverResult]) {
	t.Helper()
	if got.Status != identity.ResultUnknown || got.Completeness != identity.CompletenessUnknown {
		t.Fatalf("hover error result = %+v, want unknown", got)
	}
}
