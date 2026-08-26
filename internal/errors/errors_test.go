package errors

import (
	"errors"
	"testing"
)

func TestErrorKindString(t *testing.T) {
	cases := []struct {
		kind ErrorKind
		want string
	}{
		{ErrProtocol, "protocol"},
		{ErrInvalidPosition, "invalid_position"},
		{ErrStaleSnapshot, "stale_snapshot"},
		{ErrContentModified, "content_modified"},
		{ErrBackendUnavailable, "backend_unavailable"},
		{ErrBackendCrashed, "backend_crashed"},
		{ErrBuildContext, "build_context"},
		{ErrParse, "parse"},
		{ErrSemantic, "semantic"},
		{ErrIndex, "index"},
		{ErrIndexCorrupt, "index_corrupt"},
		{ErrTimeout, "timeout"},
		{ErrCancelled, "cancelled"},
		{ErrOverloaded, "overloaded"},
		{ErrPermission, "permission"},
		{ErrUntrustedOperation, "untrusted_operation"},
		{ErrUnsupported, "unsupported"},
		{ErrInternal, "internal"},
		{ErrNotFound, "not_found"},
		{ErrInvalidArgument, "invalid_argument"},
		{ErrorKind(999), "unknown(999)"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			got := tc.kind.String()
			if got != tc.want {
				t.Errorf("ErrorKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

func TestOmniErrorString(t *testing.T) {
	e := New(ErrInternal, "op1", "something failed").
		WithWorkspace("ws1").
		WithSnapshot(42).
		WithRecoverable(true).
		WithCode("E001")
	s := e.Error()
	if s == "" {
		t.Error("expected non-empty error string")
	}
	if !contains(s, "ws1") {
		t.Errorf("expected error string to contain workspace, got: %s", s)
	}

	e2 := New(ErrTimeout, "op2", "timed out").WithRecoverable(true)
	s2 := e2.Error()
	if s2 != "[timeout] op2: timed out" {
		t.Errorf("got %q, want '[timeout] op2: timed out'", s2)
	}

	// Non-recoverable with cause.
	e3 := New(ErrInternal, "op3", "crashed")
	e3.Recoverable = false
	e3.Cause = errors.New("OOM")
	s3 := e3.Error()
	if !contains(s3, "non-recoverable") {
		t.Errorf("expected 'non-recoverable' in %q", s3)
	}
	if !contains(s3, "OOM") {
		t.Errorf("expected 'OOM' in %q", s3)
	}
}

func contains(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestOmniErrorIs(t *testing.T) {
	e1 := New(ErrTimeout, "op", "timeout")
	e3 := New(ErrInternal, "op", "internal")

	target1 := New(ErrTimeout, "x", "x")
	if !e1.Is(target1) {
		t.Error("e1.Is(same-kind error) should be true")
	}
	if e1.Is(e3) {
		t.Error("e1.Is(different-kind) should be false")
	}
	var plainErr error = errors.New("plain")
	if e1.Is(plainErr) {
		t.Error("e1.Is(plain error) should be false")
	}
}

func TestOmniErrorUnwrap(t *testing.T) {
	cause := errors.New("root cause")
	e := New(ErrInternal, "op", "wrapped")
	e.Cause = cause
	if !errors.Is(e, cause) {
		t.Error("expected Is to find root cause")
	}
}

func TestOmniErrorNilSafe(t *testing.T) {
	var nilErr *Error
	s := nilErr.Error()
	if s != "<nil>" {
		t.Errorf("nil Error().Error() = %q, want '<nil>'", s)
	}
}

func TestIsKind(t *testing.T) {
	e := New(ErrBackendCrashed, "backend", "crashed")
	if !IsKind(e, ErrBackendCrashed) {
		t.Error("IsKind should match")
	}
	if IsKind(e, ErrTimeout) {
		t.Error("IsKind should not match different kind")
	}
	if IsKind(errors.New("plain"), ErrTimeout) {
		t.Error("IsKind on plain error should be false")
	}
}

func TestOmniErrorBuilderChain(t *testing.T) {
	e := New(ErrBackendCrashed, "backend", "crashed").
		WithWorkspace("ws1").
		WithLanguage("go").
		WithSnapshot(10).
		WithBackend("gopls").
		WithTraceID("trace-1").
		WithRecoverable(true).
		WithCode("CRASH-001")
	e.Cause = errors.New("OOM")

	if e.Kind != ErrBackendCrashed {
		t.Errorf("Kind = %v, want ErrBackendCrashed", e.Kind)
	}
	if e.Code != "CRASH-001" {
		t.Errorf("Code = %q, want 'CRASH-001'", e.Code)
	}
	if e.Workspace != "ws1" {
		t.Errorf("Workspace = %q, want 'ws1'", e.Workspace)
	}
	if e.Language != "go" {
		t.Errorf("Language = %q, want 'go'", e.Language)
	}
	if e.TraceID != "trace-1" {
		t.Errorf("TraceID = %q, want 'trace-1'", e.TraceID)
	}
	if e.Cause == nil {
		t.Error("expected non-nil Cause")
	}
}

func TestWrap(t *testing.T) {
	cause := errors.New("inner")
	e := Wrap(ErrParse, "parse failed", cause)
	if e == nil {
		t.Fatal("Wrap returned nil")
	}
	if e.Kind != ErrParse {
		t.Errorf("Kind = %v, want ErrParse", e.Kind)
	}
	if e.Op != "parse failed" {
		t.Errorf("Op = %q, want 'parse failed'", e.Op)
	}
	if !errors.Is(e, cause) {
		t.Error("Wrap should preserve cause via Is")
	}
}
