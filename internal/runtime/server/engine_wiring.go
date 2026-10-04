package server

import (
	"context"
	"errors"
	"fmt"

	ierrors "github.com/omnilsp/omni/internal/errors"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/semantic/query"
	"github.com/omnilsp/omni/internal/workspace/snapshot"
)

// semanticViaEngine routes a semantic read through the §J memo engine: the
// same revision + build context + options returns the cached envelope without
// touching the backend; a revision advance invalidates via InvalidateSnapshot
// (wired into publishSnapshot). Transient backend errors stay uncached and
// retryable; stable errors are memoized as FailedStable.
func semanticViaEngine[T any](s *Server, ctx context.Context, be languages.Backend, kind, uri string, snapRev uint64, bc identity.BuildContextID, opts string, fn func(context.Context) (identity.SemanticResult[T], error)) (result identity.SemanticResult[T], resultErr error) {
	if err := s.flushExternalSourceChanges(ctx, be); err != nil {
		return result, err
	}
	uri = canonicalDocumentURI(uri)
	backendGeneration := backendEpoch(be)
	captured := snapshotFromCtx(ctx)
	// A snapshot revision only identifies editor state. Closed dependencies
	// need independent content identity before a semantic memo can be reused.
	input, identified := be.(interface {
		SemanticInputFingerprint(context.Context, *snapshot.Snapshot, string) (string, error)
	})
	if !identified {
		return withBackendWorkspaceSnapshot(ctx, s, be, captured, fn)
	}
	fingerprint, inputErr := input.SemanticInputFingerprint(ctx, captured, uri)
	if inputErr != nil {
		var zero identity.SemanticResult[T]
		return zero, inputErr
	}
	if fingerprint == "" {
		return withBackendWorkspaceSnapshot(ctx, s, be, captured, fn)
	}
	defer func() {
		if resultErr != nil || result.Status == identity.ResultUnknown || result.Status == identity.ResultUnavailable {
			return
		}
		currentFingerprint, err := input.SemanticInputFingerprint(ctx, captured, uri)
		if err == nil && currentFingerprint != fingerprint {
			err = ierrors.New(ierrors.ErrContentModified, "semantic.memo", "semantic inputs changed during the request")
		}
		if err != nil {
			s.queries.Invalidate(query.Dep{Kind: "file", ID: fmt.Sprintf("%s@%d", uri, snapRev)})
			result = identity.SemanticResult[T]{}
			resultErr = err
		}
	}()
	opts = fmt.Sprintf("%d:%s%s", len(fingerprint), fingerprint, opts)
	snapshotInstance := uint64(0)
	if captured != nil {
		snapshotInstance = captured.InstanceID()
	}
	res, err := s.queries.Query(ctx, query.Key{
		Kind:             kind,
		Workspace:        "default",
		SnapshotRev:      snapRev,
		SnapshotInstance: snapshotInstance,
		BuildContext:     string(bc),
		BackendEpoch:     backendGeneration,
		Subject:          uri,
		OptionsHash:      opts,
	}, query.DepSet{
		{Kind: "file", ID: fmt.Sprintf("%s@%d", uri, snapRev)}:                  {},
		{Kind: "backendEpoch", ID: fmt.Sprintf("%s@%d", bc, backendGeneration)}: {},
	}, func(computeCtx context.Context, _ query.Bindings) (any, query.DepSet, error) {
		r, ferr := withBackendWorkspaceSnapshot(computeCtx, s, be, captured, fn)
		if ferr == nil && r.Status != identity.ResultUnknown && r.Status != identity.ResultUnavailable {
			current, err := input.SemanticInputFingerprint(computeCtx, captured, uri)
			if err == nil && current != fingerprint {
				err = ierrors.New(ierrors.ErrContentModified, "semantic.memo", "semantic inputs changed before cache publication")
			}
			if err != nil {
				return nil, nil, &query.TransientError{Err: err}
			}
		}
		// §B6/§J4: an in-band Unknown/Unavailable envelope (err == nil, the
		// TS-bridge convention for upstream refusal) is an honest but
		// non-terminal answer. It must NOT be memoized as Ready for the rest
		// of the snapshot — a recovered backend must be consulted again.
		// Surface it as transient so the engine drops it, then project it.
		if ferr == nil && (r.Status == identity.ResultUnknown || r.Status == identity.ResultUnavailable) {
			return r, nil, &query.TransientError{Err: errUnknownEnvelope}
		}
		return r, nil, wrapTransient(ferr)
	})
	if err != nil {
		// A captured older snapshot is still a legitimate answer
		// (INV-SNAPSHOT-002): on a §J7 stale-publish rejection, answer by
		// calling the backend directly — just don't memoize the result.
		if errors.Is(err, query.ErrStalePublish) {
			return withBackendWorkspaceSnapshot(ctx, s, be, captured, fn)
		}
		// A transient Unknown/Unavailable envelope (§B6) is an honest answer
		// (§A3): project it as-is. It was left uncached (transient dropped in
		// the engine), so a recovered backend is consulted on the next request.
		if errors.Is(err, errUnknownEnvelope) {
			if envelope, ok := res.Value.(identity.SemanticResult[T]); ok {
				return envelope, nil
			}
		}
		var zero identity.SemanticResult[T]
		return zero, err
	}
	envelope, ok := res.Value.(identity.SemanticResult[T])
	if !ok {
		var zero identity.SemanticResult[T]
		return zero, fmt.Errorf("query engine cached unexpected type for %s", kind)
	}
	return envelope, nil
}

// withBackendWorkspaceSnapshot makes the backend snapshot lease live for the
// duration of the actual computation. Memoized work may outlive the request
// that started it while another waiter remains, so that work must own its
// lease instead of borrowing the initiating request's lease.
func withBackendWorkspaceSnapshot[T any](ctx context.Context, s *Server, be languages.Backend, captured *snapshot.Snapshot, fn func(context.Context) (T, error)) (result T, requestErr error) {
	var zero T
	leaseCtx, finish, err := s.beginBackendWorkspaceSnapshot(ctx, be, captured)
	if err != nil {
		return zero, err
	}
	if finish == nil {
		return fn(leaseCtx)
	}
	defer func() {
		originalPanic := recover()
		finishErr, finishPanic := finishBackendWorkspaceSnapshot(finish)
		if originalPanic != nil {
			panic(originalPanic)
		}
		if finishPanic != nil {
			panic(finishPanic)
		}
		if finishErr != nil {
			result = zero
			requestErr = finishErr
		}
	}()
	return fn(leaseCtx)
}

func finishBackendWorkspaceSnapshot(finish func() error) (err error, panicValue any) {
	defer func() { panicValue = recover() }()
	err = finish()
	return err, nil
}

// errUnknownEnvelope marks an in-band Unknown/Unavailable result as retryable
// (never memoized); the caller still projects the envelope, so the wire answer
// is unchanged — only the memoization policy differs.
var errUnknownEnvelope = ierrors.New(ierrors.ErrBackendUnavailable, "semantic", "backend returned unknown/unavailable envelope")

// wrapTransient marks retryable backend failures so the memo engine drops
// them instead of caching (a crashed-but-restarting backend must not have its
// failure pinned to the revision).
func wrapTransient(err error) error {
	if err == nil {
		return nil
	}
	if isRetryableKind(err) {
		return &query.TransientError{Err: err}
	}
	return err
}

// isRetryableKind reports backend failure classes worth retrying on a later
// request rather than memoizing: stale content, crashed/restarting, timed out,
// or overloaded backends may succeed next call; a deterministic compute error
// would not.
func isRetryableKind(err error) bool {
	var e *ierrors.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Kind {
	case ierrors.ErrContentModified, ierrors.ErrBackendUnavailable, ierrors.ErrTimeout, ierrors.ErrOverloaded:
		return true
	default:
		return false
	}
}

// ValidateEditSet performs the §I12 cross-file edit-set preflight: overlapping
// ranges within one file are ambiguous (apply order changes the outcome), so
// the whole set is refused rather than applied in an arbitrary order. Empty
// and single-edit sets trivially pass.
func ValidateEditSet(edits []languages.TextEdit) error {
	byFile := make(map[string][]languages.TextEdit)
	for _, e := range edits {
		if e.URI == "" {
			return fmt.Errorf("edit set validation failed: edit has an empty URI")
		}
		if posKey(e.StartLine, e.StartChar) > posKey(e.EndLine, e.EndChar) {
			return fmt.Errorf("edit set validation failed for %s: range start is after range end", e.URI)
		}
		key := canonicalDocumentURI(e.URI)
		byFile[key] = append(byFile[key], e)
	}
	for uri, list := range byFile {
		if len(list) < 2 {
			continue
		}
		for i := 0; i < len(list); i++ {
			for j := i + 1; j < len(list); j++ {
				a, b := list[i], list[j]
				if rangesOverlap(a, b) {
					return fmt.Errorf("edit set validation failed for %s: overlapping ranges [%d:%d-%d:%d] and [%d:%d-%d:%d]",
						uri, a.StartLine, a.StartChar, a.EndLine, a.EndChar,
						b.StartLine, b.StartChar, b.EndLine, b.EndChar)
				}
			}
		}
	}
	return nil
}

func rangesOverlap(a, b languages.TextEdit) bool {
	if canonicalDocumentURI(a.URI) != canonicalDocumentURI(b.URI) {
		return false
	}
	aStart, aEnd := posKey(a.StartLine, a.StartChar), posKey(a.EndLine, a.EndChar)
	bStart, bEnd := posKey(b.StartLine, b.StartChar), posKey(b.EndLine, b.EndChar)
	// Half-open ranges [start,end): boundary-touching edits do not overlap.
	// Two insertions at the same offset are still ambiguous because applying
	// them in either order changes the resulting text.
	if aStart == aEnd && bStart == bEnd && aStart == bStart {
		return true
	}
	return aStart < bEnd && bStart < aEnd
}

func posKey(line, char uint32) uint64 {
	return uint64(line)<<32 | uint64(char)
}
