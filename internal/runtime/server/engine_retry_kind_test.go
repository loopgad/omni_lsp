package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

// allErrorKinds lists every ErrorKind the errors package declares. The retry
// table below is walked against this list rather than against the four kinds
// the switch names, so adding a kind to the enum cannot silently fall into the
// default branch with nobody noticing which side it landed on.
var allErrorKinds = []ierrors.ErrorKind{
	ierrors.ErrProtocol,
	ierrors.ErrInvalidPosition,
	ierrors.ErrInvalidDocumentVersion,
	ierrors.ErrStaleSnapshot,
	ierrors.ErrContentModified,
	ierrors.ErrBackendUnavailable,
	ierrors.ErrBackendCrashed,
	ierrors.ErrBuildContext,
	ierrors.ErrParse,
	ierrors.ErrSemantic,
	ierrors.ErrIndex,
	ierrors.ErrIndexCorrupt,
	ierrors.ErrTimeout,
	ierrors.ErrCancelled,
	ierrors.ErrOverloaded,
	ierrors.ErrPermission,
	ierrors.ErrUntrustedOperation,
	ierrors.ErrUnsupported,
	ierrors.ErrInternal,
	ierrors.ErrNotFound,
	ierrors.ErrInvalidArgument,
}

// TestQ4_IsRetryableKindClassification pins which failure classes are worth
// retrying. Misreading this is silent in both directions: a deterministic
// compute error marked retryable re-runs the backend on every request, and a
// transient one marked permanent memoizes a failure that would have cleared.
func TestQ4_IsRetryableKindClassification(t *testing.T) {
	retryable := map[ierrors.ErrorKind]bool{
		ierrors.ErrContentModified:    true,
		ierrors.ErrBackendUnavailable: true,
		ierrors.ErrTimeout:            true,
		ierrors.ErrOverloaded:         true,
	}
	for _, kind := range allErrorKinds {
		want := retryable[kind]
		if got := isRetryableKind(ierrors.New(kind, "op", "msg")); got != want {
			t.Errorf("isRetryableKind(%s) = %v, want %v", kind, got, want)
		}
	}
}

// TestQ4_IsRetryableKindNonBackendErrors covers the inputs that never reach the
// kind switch: errors.As fails, so they are all permanent by construction.
func TestQ4_IsRetryableKindNonBackendErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"nil", nil},
		{"plain error", errors.New("boom")},
		{"context canceled", context.Canceled},
		{"context deadline", context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isRetryableKind(tc.err) {
				t.Errorf("isRetryableKind(%v) = true, want false", tc.err)
			}
		})
	}

	// A wrapper must not hide the kind: a backend that returns a wrapped
	// timeout would otherwise have its failure memoized and never retried.
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"wrapped timeout", fmt.Errorf("hover: %w", ierrors.New(ierrors.ErrTimeout, "go", "slow"))},
		{"double wrapped timeout", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", ierrors.New(ierrors.ErrTimeout, "go", "slow")))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !isRetryableKind(tc.err) {
				t.Errorf("isRetryableKind(%v) = false, want true; the wrapper hid the kind", tc.err)
			}
		})
	}
}

// permanentHoverBackend fails every hover with a deterministic compute error.
type permanentHoverBackend struct {
	mockBackend
	calls int64
}

func (b *permanentHoverBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.calls++
	return identity.SemanticResult[*languages.HoverResult]{},
		ierrors.New(ierrors.ErrSemantic, "go", "deterministic type error")
}

// TestQ4_PermanentFailureIsMemoized covers the other half of the contract:
// a failure that will not clear on its own is remembered, so a client asking
// again does not re-run the same doomed computation. Nothing asserted this --
// only the retryable side had coverage.
func TestQ4_PermanentFailureIsMemoized(t *testing.T) {
	const uri = "file:///w/main.go"
	s := New(DefaultConfig())
	be := &permanentHoverBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
	s.publishSnapshot()

	for i := 1; i <= 2; i++ {
		resp := dispatchHoverRaw(s, uri)
		if resp == nil || resp.Error == nil {
			t.Fatalf("hover %d = %+v, want an error", i, resp)
		}
	}
	if be.calls != 1 {
		t.Errorf("backend calls = %d, want 1 (a deterministic failure must be memoized, not recomputed)", be.calls)
	}
}
