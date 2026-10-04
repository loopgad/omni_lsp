package ccls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

func TestDefinitionAndReferencesPropagateChildErrors(t *testing.T) {
	const uri = "file:///x/main.cpp"
	content := []byte("int value = 1;\n")
	lookups := []cclsLookupCase{
		{
			name:          "definition",
			method:        "textDocument/definition",
			successStatus: identity.ResultExact,
			call: func(b *Backend) (identity.SemanticResult[[]languages.Location], error) {
				return b.Definition(context.Background(), languages.DefinitionRequest{URI: uri, Content: content, SnapshotRev: 1})
			},
		},
		{
			name:          "references",
			method:        "textDocument/references",
			successStatus: identity.ResultPartial,
			call: func(b *Backend) (identity.SemanticResult[[]languages.Location], error) {
				return b.References(context.Background(), languages.ReferencesRequest{URI: uri, Content: content, SnapshotRev: 1})
			},
		},
	}

	for _, lookup := range lookups {
		t.Run(lookup.name, func(t *testing.T) {
			t.Run("request error", func(t *testing.T) {
				got := runCclsLookupCase(t, lookup, "request")
				if got.err == nil || !strings.Contains(got.err.Error(), "fake child request failed") {
					t.Fatalf("request error = %v, want child failure preserved", got.err)
				}
				assertUnknownCclsLookup(t, got.result)
			})

			t.Run("decode error", func(t *testing.T) {
				got := runCclsLookupCase(t, lookup, "decode")
				var decodeErr *json.UnmarshalTypeError
				if !errors.As(got.err, &decodeErr) {
					t.Fatalf("decode error = %v, want original JSON unmarshal error", got.err)
				}
				assertUnknownCclsLookup(t, got.result)
			})

			t.Run("valid empty result", func(t *testing.T) {
				got := runCclsLookupCase(t, lookup, "empty")
				if got.err != nil || got.result.Status != lookup.successStatus || len(got.result.Value) != 0 {
					t.Fatalf("empty result = (%+v, %v), want successful empty result", got.result, got.err)
				}
			})
		})
	}
}

type cclsLookupCase struct {
	name          string
	method        string
	successStatus identity.ResultStatus
	call          func(*Backend) (identity.SemanticResult[[]languages.Location], error)
}

type cclsLookupOutcome struct {
	result identity.SemanticResult[[]languages.Location]
	err    error
}

func runCclsLookupCase(t *testing.T, lookup cclsLookupCase, responseKind string) cclsLookupOutcome {
	t.Helper()
	b, stdin, child := newCclsProtocolBackend(t)
	got := make(chan cclsLookupOutcome, 1)
	go func() {
		result, err := lookup.call(b)
		got <- cclsLookupOutcome{result: result, err: err}
	}()

	switch responseKind {
	case "request":
		replyCclsRequestError(t, stdin.frames, child, lookup.method)
	case "decode":
		replyToCclsRequest(t, stdin.frames, child, lookup.method, map[string]any{"unexpected": true})
	case "empty":
		replyToCclsRequest(t, stdin.frames, child, lookup.method, nil)
	default:
		t.Fatalf("unknown fake response kind %q", responseKind)
	}
	return <-got
}

func replyCclsRequestError(t *testing.T, frames <-chan string, child io.Writer, method string) {
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
		writeCclsRPCResponse(t, child, &jsonrpc.Message{
			JSONRPC: jsonrpc.Version,
			ID:      &jsonrpc.RequestID{Num: id},
			Error:   &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "fake child request failed"},
		})
		return
	}
}

func assertUnknownCclsLookup(t *testing.T, got identity.SemanticResult[[]languages.Location]) {
	t.Helper()
	if got.Status != identity.ResultUnknown || got.Completeness != identity.CompletenessUnknown {
		t.Fatalf("error result = %+v, want unknown", got)
	}
}

func writeCclsRPCResponse(t *testing.T, child io.Writer, response *jsonrpc.Message) {
	t.Helper()
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(child, "Content-Length: %d\r\n\r\n%s", len(raw), raw); err != nil {
		t.Fatalf("write child response: %v", err)
	}
}

func TestHoverPropagatesChildErrors(t *testing.T) {
	const uri = "file:///x/main.cpp"
	for _, responseKind := range []string{"request", "decode"} {
		t.Run(responseKind, func(t *testing.T) {
			b, stdin, child := newCclsProtocolBackend(t)
			got := make(chan struct {
				result identity.SemanticResult[*languages.HoverResult]
				err    error
			}, 1)
			go func() {
				result, err := b.Hover(context.Background(), languages.HoverRequest{
					URI: uri, Content: []byte("int value = 1;\n"), SnapshotRev: 1,
				})
				got <- struct {
					result identity.SemanticResult[*languages.HoverResult]
					err    error
				}{result: result, err: err}
			}()
			if responseKind == "request" {
				replyCclsRequestError(t, stdin.frames, child, "textDocument/hover")
			} else {
				replyToCclsRequest(t, stdin.frames, child, "textDocument/hover", map[string]any{
					"contents": map[string]any{"value": true},
				})
			}
			response := <-got
			if responseKind == "request" {
				if response.err == nil || !strings.Contains(response.err.Error(), "fake child request failed") {
					t.Fatalf("hover request error = %v, want child failure preserved", response.err)
				}
			} else {
				var decodeErr *json.UnmarshalTypeError
				if !errors.As(response.err, &decodeErr) {
					t.Fatalf("hover decode error = %v, want original JSON unmarshal error", response.err)
				}
			}
			if response.result.Status != identity.ResultUnknown {
				t.Fatalf("hover error result = %+v, want unknown", response.result)
			}
		})
	}
}

func TestRenamePropagatesChildRequestAndDecodeErrors(t *testing.T) {
	for _, responseKind := range []string{"request", "decode"} {
		t.Run(responseKind, func(t *testing.T) {
			_, stdin, child, got, uri := startCclsRenameProtocolTest(t, languages.SymbolVariable)
			if responseKind == "request" {
				replyCclsRequestError(t, stdin.frames, child, "textDocument/rename")
			} else {
				replyToCclsRequest(t, stdin.frames, child, "textDocument/rename", map[string]any{
					"changes": map[string]any{uri: "not-an-edit-list"},
				})
			}
			response := awaitCclsRenameResult(t, got)
			if responseKind == "request" {
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
	const uri = "file:///x/main.cpp"
	b, stdin, child := newCclsProtocolBackend(t)
	enableCclsRenameForProtocolTest(t, b)
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
	replyCclsRequestError(t, stdin.frames, child, "textDocument/definition")
	response := awaitCclsRenameResult(t, got)
	if response.err == nil || !strings.Contains(response.err.Error(), "fake child request failed") {
		t.Fatalf("rename classification error = %v, want definition failure preserved", response.err)
	}
	if response.result.Status != identity.ResultUnavailable {
		t.Fatalf("rename classification result = %+v, want unavailable", response.result)
	}
}
