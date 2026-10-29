package nested

// Round-17 product-domain wire-shape normalization (§Fencing-adjacent
// discretion item): a child language server answering the LSP-standard
// ContentModified code (-32801) had its error kind dropped by Conn.SendRequest
// (plain fmt.Errorf), so the dispatcher relayed the refusal as InternalError
// -32603 "<name>: content modified" — two wire shapes for one semantic,
// forcing consumers into a dual predicate (the round-17 VS Code and Neovim
// acceptance drivers, which must keep matching for backward compatibility with
// older candidate telemetry). These guards pin the normalized behavior:
//
//  1. a child of every bridged backend class that answers -32801 surfaces
//     typed ierrors.ErrContentModified, so the dispatcher answers -32801;
//  2. the relayed text keeps the legacy "<name>: <child message>" form, so the
//     message-contains half of the harness dual predicate still matches;
//  3. non-content-modified child errors keep their legacy untyped shape and
//     wire as -32603 exactly as before.
//
// rust-analyzer's refusal text ("content modified") is attested in-repo by the
// acceptance harness docs (test/acceptance/clients/{vscode,test-entry.js,
// neovim/run.lua} guard:contentModified blocks and commit 8cfdbbb). No in-repo
// recording pins pyright's or typescript-language-server's exact text, so the
// normalization keys on the standard wire code and the message column below is
// representative, not observed.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

func newChildErrorConn(t *testing.T, name, lang string) (*Conn, *io.PipeWriter, *notificationRecorder) {
	t.Helper()
	stdout, child := io.Pipe()
	recorder := newNotificationRecorder()
	conn := New(Config{Name: name, Lang: lang, WorkDir: t.TempDir(), RequestTimeout: 2 * time.Second})
	conn.Attach(nil, recorder, stdout)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = child.Close()
	})
	return conn, child, recorder
}

// answerChildWithErrorResponse observes one outgoing request frame and answers
// it with the given JSON-RPC error.
func answerChildWithErrorResponse(t *testing.T, child io.Writer, recorder *notificationRecorder, wantMethod string, code int, message string) {
	t.Helper()
	request := decodeRecordedNotification(t, <-recorder.frames)
	if request.Method != wantMethod || request.ID == nil {
		t.Fatalf("outgoing request = %+v, want an identified %s request", request, wantMethod)
	}
	raw, err := json.Marshal(&jsonrpc.Message{
		JSONRPC: jsonrpc.Version,
		ID:      request.ID,
		Error:   &jsonrpc.ResponseError{Code: code, Message: message},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(child, "Content-Length: %d\r\n\r\n%s", len(raw), raw); err != nil {
		t.Fatal(err)
	}
}

func TestSendRequestNormalizesChildContentModifiedToTypedKind(t *testing.T) {
	for _, tc := range []struct {
		backend string // backend class served over the shared stdio bridge
		lang    string
		message string // child's content-modified refusal text (representative; see file comment)
	}{
		{backend: "rust-analyzer", lang: "rust", message: "content modified"},
		{backend: "pyright", lang: "python", message: "Request failed: content modified"},
		{backend: "typescript-language-server", lang: "typescript", message: "content modified"},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			conn, child, recorder := newChildErrorConn(t, tc.backend, tc.lang)
			type reply struct {
				result json.RawMessage
				err    error
			}
			resultCh := make(chan reply, 1)
			go func() {
				result, err := conn.SendRequest(context.Background(), "textDocument/hover", nil)
				resultCh <- reply{result: result, err: err}
			}()

			answerChildWithErrorResponse(t, child, recorder, "textDocument/hover", jsonrpc.ContentModified, tc.message)

			select {
			case got := <-resultCh:
				if !ierrors.IsKind(got.err, ierrors.ErrContentModified) {
					t.Fatalf("child -32801 surfaced as %v (%T), want typed ierrors.ErrContentModified", got.err, got.err)
				}
				if got.result != nil {
					t.Fatalf("child refusal carried a result: %s", got.result)
				}
				legacyText := tc.backend + ": " + tc.message
				if !strings.Contains(got.err.Error(), legacyText) {
					t.Fatalf("typed error text %q lost the legacy relay text %q the harness dual predicate matches", got.err.Error(), legacyText)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("child content-modified response never surfaced")
			}
		})
	}
}

func TestDispatcherSurfacesNestedChildContentModifiedAsWire32801(t *testing.T) {
	conn, child, recorder := newChildErrorConn(t, "rust-analyzer", "rust")

	dispatcher := jsonrpc.NewDispatcher()
	respCh := make(chan *jsonrpc.Message, 1)
	dispatcher.Register("test/relay", func(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
		// Product shape: the semantic handler returns the backend error to the
		// dispatcher (internal/runtime/server handlers.go returns backend err).
		_, err := conn.SendRequest(ctx, "textDocument/hover", nil)
		return nil, err
	})
	go func() {
		respCh <- dispatcher.Dispatch(context.Background(), &jsonrpc.Message{
			JSONRPC: jsonrpc.Version,
			ID:      &jsonrpc.RequestID{Num: 7},
			Method:  "test/relay",
		})
	}()

	answerChildWithErrorResponse(t, child, recorder, "textDocument/hover", jsonrpc.ContentModified, "content modified")

	select {
	case resp := <-respCh:
		if resp == nil || resp.Error == nil {
			t.Fatalf("dispatch response = %+v, want an error response", resp)
		}
		if resp.Error.Code != jsonrpc.ContentModified {
			t.Fatalf("wire code = %d, want ContentModified (%d); the child refusal must not relay as InternalError", resp.Error.Code, jsonrpc.ContentModified)
		}
		// Dual-predicate halves, exactly as the round-17 drivers check them:
		// the code branch above, and the message-contains branch for older
		// candidate telemetry that only matches the text.
		if !strings.Contains(strings.ToLower(resp.Error.Message), "content modified") {
			t.Fatalf("wire message %q lost the %q text the legacy predicate matches", resp.Error.Message, "content modified")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dispatcher round trip never completed")
	}
}

func TestSendRequestKeepsNonContentModifiedChildErrorsUntyped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		message string
	}{
		{name: "server internal error", code: jsonrpc.InternalError, message: "boom"},
		{name: "typed refusal", code: -32803, message: "request failed: semantic refusal"},
		{name: "server-defined code", code: -50001, message: "server-specific failure"},
		{name: "parse error", code: -32700, message: "malformed request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, child, recorder := newChildErrorConn(t, "fake-lsp", "test")
			resultCh := make(chan error, 1)
			go func() {
				_, err := conn.SendRequest(context.Background(), "textDocument/hover", nil)
				resultCh <- err
			}()

			answerChildWithErrorResponse(t, child, recorder, "textDocument/hover", tc.code, tc.message)

			var handlerErr error
			select {
			case got := <-resultCh:
				handlerErr = got
				if ierrors.IsKind(got, ierrors.ErrContentModified) {
					t.Fatalf("non-content-modified child error %v was retyped as ErrContentModified", got)
				}
				// Legacy shape, exact: plain "<name>: <child message>".
				if want := "fake-lsp: " + tc.message; got.Error() != want {
					t.Fatalf("untyped error text = %q, want the exact legacy form %q", got.Error(), want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("child error response never surfaced")
			}

			// Wire shape unchanged: an untyped error dispatches as InternalError
			// (-32603) with the identical text — the legacy relay for every
			// non-content-modified failure.
			dispatcher := jsonrpc.NewDispatcher()
			dispatcher.Register("test/relay", func(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
				return nil, handlerErr
			})
			resp := dispatcher.Dispatch(context.Background(), &jsonrpc.Message{
				JSONRPC: jsonrpc.Version,
				ID:      &jsonrpc.RequestID{Num: 1},
				Method:  "test/relay",
			})
			if resp == nil || resp.Error == nil {
				t.Fatalf("dispatch response = %+v, want an error response", resp)
			}
			if resp.Error.Code != jsonrpc.InternalError {
				t.Fatalf("wire code = %d, want InternalError (%d); non-content-modified relay shape must not change", resp.Error.Code, jsonrpc.InternalError)
			}
			if resp.Error.Message != "fake-lsp: "+tc.message {
				t.Fatalf("wire message = %q, want the legacy text unchanged", resp.Error.Message)
			}
		})
	}
}
