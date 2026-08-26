package server

import (
	"context"
	"errors"
	"fmt"

	ierrors "github.com/omnilsp/omni/internal/errors"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/semantic/query"
)

// semanticViaEngine routes a semantic read through the §J memo engine: the
// same revision + build context + options returns the cached envelope without
// touching the backend; a revision advance invalidates via InvalidateSnapshot
// (wired into publishSnapshot). Transient backend errors stay uncached and
// retryable; stable errors are memoized as FailedStable.
func semanticViaEngine[T any](s *Server, ctx context.Context, kind, uri string, snapRev uint64, bc identity.BuildContextID, opts string, fn func() (identity.SemanticResult[T], error)) (identity.SemanticResult[T], error) {
	res, err := s.queries.Query(ctx, query.Key{
		Kind:         kind,
		Workspace:    "default",
		SnapshotRev:  snapRev,
		BuildContext: string(bc),
		Subject:      uri,
		OptionsHash:  opts,
	}, query.DepSet{
		{Kind: "file", ID: fmt.Sprintf("%s@%d", uri, snapRev)}: {},
		{Kind: "backendEpoch", ID: string(bc)}:                 {},
	}, func(_ context.Context, _ query.Bindings) (any, query.DepSet, error) {
		r, ferr := fn()
		return r, nil, wrapTransient(ferr)
	})
	if err != nil {
		// A captured older snapshot is still a legitimate answer
		// (INV-SNAPSHOT-002): on a §J7 stale-publish rejection, answer by
		// calling the backend directly — just don't memoize the result.
		if errors.Is(err, query.ErrStalePublish) {
			return fn()
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
// request rather than memoizing: crashed/restarting, timed out, or overloaded
// backends may succeed next call; a deterministic compute error would not.
func isRetryableKind(err error) bool {
	var e *ierrors.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Kind {
	case ierrors.ErrBackendUnavailable, ierrors.ErrTimeout, ierrors.ErrOverloaded:
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
		byFile[e.URI] = append(byFile[e.URI], e)
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
	if a.URI != b.URI {
		return false
	}
	aStart, aEnd := posKey(a.StartLine, a.StartChar), posKey(a.EndLine, a.EndChar)
	bStart, bEnd := posKey(b.StartLine, b.StartChar), posKey(b.EndLine, b.EndChar)
	// Half-open ranges [start,end): boundary-touching edits do not overlap.
	return aStart < bEnd && bStart < aEnd
}

func posKey(line, char uint32) uint64 {
	return uint64(line)<<32 | uint64(char)
}
