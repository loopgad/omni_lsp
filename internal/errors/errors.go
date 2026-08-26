// Package errors provides the structured error taxonomy for the OmniLSP platform.
//
// Invariants:
//  1. Every error carries a Kind, Operation, and optional Context.
//  2. Errors are never silently swallowed — they propagate with full metadata.
//  3. Error comparison uses Kind, not string matching.
//  4. Errors are safe for concurrent use (immutable after creation).
package errors

import (
	stderrors "errors"
	"fmt"
)

// ErrorKind categorizes errors into well-defined classes.
type ErrorKind uint16

const (
	ErrProtocol ErrorKind = iota + 1
	ErrInvalidPosition
	ErrInvalidDocumentVersion
	ErrStaleSnapshot
	ErrContentModified
	ErrBackendUnavailable
	ErrBackendCrashed
	ErrBuildContext
	ErrParse
	ErrSemantic
	ErrIndex
	ErrIndexCorrupt
	ErrTimeout
	ErrCancelled
	ErrOverloaded
	ErrPermission
	ErrUntrustedOperation
	ErrUnsupported
	ErrInternal
	ErrNotFound
	ErrInvalidArgument
)

var kindNames = map[ErrorKind]string{
	ErrProtocol:               "protocol",
	ErrInvalidPosition:        "invalid_position",
	ErrInvalidDocumentVersion: "invalid_document_version",
	ErrStaleSnapshot:          "stale_snapshot",
	ErrContentModified:        "content_modified",
	ErrBackendUnavailable:     "backend_unavailable",
	ErrBackendCrashed:         "backend_crashed",
	ErrBuildContext:           "build_context",
	ErrParse:                  "parse",
	ErrSemantic:               "semantic",
	ErrIndex:                  "index",
	ErrIndexCorrupt:           "index_corrupt",
	ErrTimeout:                "timeout",
	ErrCancelled:              "cancelled",
	ErrOverloaded:             "overloaded",
	ErrPermission:             "permission",
	ErrUntrustedOperation:     "untrusted_operation",
	ErrUnsupported:            "unsupported",
	ErrInternal:               "internal",
	ErrNotFound:               "not_found",
	ErrInvalidArgument:        "invalid_argument",
}

func (k ErrorKind) String() string {
	if name, ok := kindNames[k]; ok {
		return name
	}
	return fmt.Sprintf("unknown(%d)", uint16(k))
}

// Error is the platform's structured error type.
type Error struct {
	Kind        ErrorKind
	Code        string // machine-readable sub-code (e.g. "EAGAIN")
	Op          string
	Message     string
	Workspace   string
	Language    string
	Snapshot    uint64
	Backend     string
	TraceID     string
	Recoverable bool // true if the caller should retry or fall back
	Cause       error
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := fmt.Sprintf("[%s] %s: %s", e.Kind, e.Op, e.Message)
	if e.Workspace != "" {
		msg += fmt.Sprintf(" (workspace=%s)", e.Workspace)
	}
	if e.Language != "" {
		msg += fmt.Sprintf(" (language=%s)", e.Language)
	}
	if e.Backend != "" {
		msg += fmt.Sprintf(" (backend=%s)", e.Backend)
	}
	if e.TraceID != "" {
		msg += fmt.Sprintf(" (trace=%s)", e.TraceID)
	}
	if !e.Recoverable {
		msg += " (non-recoverable)"
	}
	if e.Cause != nil {
		msg += fmt.Sprintf(": %v", e.Cause)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

func IsKind(err error, kind ErrorKind) bool {
	var e *Error
	if stderrors.As(err, &e) {
		return e.Kind == kind
	}
	return false
}

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	if !ok {
		return false
	}
	return e.Kind == t.Kind
}

func New(kind ErrorKind, op, message string) *Error {
	return &Error{Kind: kind, Op: op, Message: message}
}

func Wrap(kind ErrorKind, op string, cause error) *Error {
	return &Error{Kind: kind, Op: op, Message: cause.Error(), Cause: cause}
}

func (e *Error) WithWorkspace(ws string) *Error  { e.Workspace = ws; return e }
func (e *Error) WithLanguage(lang string) *Error { e.Language = lang; return e }
func (e *Error) WithSnapshot(id uint64) *Error   { e.Snapshot = id; return e }
func (e *Error) WithBackend(id string) *Error    { e.Backend = id; return e }
func (e *Error) WithTraceID(id string) *Error    { e.TraceID = id; return e }
func (e *Error) WithRecoverable(v bool) *Error   { e.Recoverable = v; return e }
func (e *Error) WithCode(code string) *Error     { e.Code = code; return e }
