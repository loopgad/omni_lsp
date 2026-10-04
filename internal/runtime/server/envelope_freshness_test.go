package server

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"
	"time"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

type blockingEnvelopeBackend struct {
	mockBackend
	started chan struct{}
	release <-chan struct{}
}

func (b *blockingEnvelopeBackend) block() {
	b.started <- struct{}{}
	<-b.release
}

func (b *blockingEnvelopeBackend) Hover(context.Context, languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.block()
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: "exact"},
	}, nil
}

func (b *blockingEnvelopeBackend) Definition(context.Context, languages.DefinitionRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.block()
	return identity.SemanticResult[[]languages.Location]{Status: identity.ResultExact}, nil
}

func (b *blockingEnvelopeBackend) References(context.Context, languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.block()
	return identity.SemanticResult[[]languages.Location]{Status: identity.ResultExact}, nil
}

type partialBlockingReferencesBackend struct{ blockingEnvelopeBackend }

func (b *partialBlockingReferencesBackend) References(context.Context, languages.ReferencesRequest) (identity.SemanticResult[[]languages.Location], error) {
	b.block()
	return identity.SemanticResult[[]languages.Location]{
		Status:       identity.ResultPartial,
		Completeness: identity.IncompleteKnownSubset,
		Value: []languages.Location{{
			URI: "file:///stale.go",
			Range: languages.Range{
				StartLine: 7, StartCharacter: 3, EndLine: 7, EndCharacter: 18,
			},
		}},
	}, nil
}

type facadeEnvelopeCall func(context.Context, *Server, string) (bool, error)

func facadeEnvelopeCalls() map[string]facadeEnvelopeCall {
	return map[string]facadeEnvelopeCall{
		"hover": func(ctx context.Context, s *Server, documentURI string) (bool, error) {
			result, err := s.HoverEnvelope(ctx, documentURI, 0, 0)
			return result.Status == identity.ResultExact, err
		},
		"definition": func(ctx context.Context, s *Server, documentURI string) (bool, error) {
			result, err := s.DefinitionEnvelope(ctx, documentURI, 0, 0)
			return result.Status == identity.ResultExact, err
		},
		"references": func(ctx context.Context, s *Server, documentURI string) (bool, error) {
			result, err := s.ReferencesEnvelope(ctx, documentURI, 0, 0, true)
			return result.Status == identity.ResultExact, err
		},
	}
}

func newBlockedFacadeCall(t *testing.T, call facadeEnvelopeCall) (*Server, string, chan struct{}, <-chan struct{}, <-chan facadeEnvelopeOutcome) {
	t.Helper()
	const documentURI = "file:///x.facade"
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	backend := &blockingEnvelopeBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".facade"}},
		started:     started,
		release:     release,
	}
	s := New(DefaultConfig())
	s.RegisterBackend("go", backend)
	s.vfs.Open(documentURI, "go", 1, []byte("initial"), vfs.SourceEditor)
	s.publishSnapshot()
	ctx := withSnapshot(context.Background(), s.snapMgr.Current())
	result := make(chan facadeEnvelopeOutcome, 1)
	go func() {
		exact, err := call(ctx, s, documentURI)
		result <- facadeEnvelopeOutcome{exact: exact, err: err}
	}()
	return s, documentURI, release, started, result
}

type facadeEnvelopeOutcome struct {
	exact bool
	err   error
}

func TestTypedEnvelopeCancellationDoesNotReturnExact(t *testing.T) {
	for name, call := range facadeEnvelopeCalls() {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			started := make(chan struct{}, 1)
			backend := &blockingEnvelopeBackend{
				mockBackend: mockBackend{langID: "go", exts: []string{".facade"}},
				started:     started,
				release:     release,
			}
			s := New(DefaultConfig())
			s.RegisterBackend("go", backend)
			const documentURI = "file:///cancel.facade"
			s.vfs.Open(documentURI, "go", 1, []byte("initial"), vfs.SourceEditor)
			s.publishSnapshot()
			capturedCtx := withSnapshot(ctx, s.snapMgr.Current())
			result := make(chan facadeEnvelopeOutcome, 1)
			go func() {
				exact, err := call(capturedCtx, s, documentURI)
				result <- facadeEnvelopeOutcome{exact: exact, err: err}
			}()

			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("typed facade did not reach the backend")
			}
			cancel()
			releaseOnce.Do(func() { close(release) })
			select {
			case got := <-result:
				if !stderrors.Is(got.err, context.Canceled) || got.exact {
					t.Fatalf("canceled facade result exact=%t err=%v; want context.Canceled without Exact", got.exact, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("typed facade did not return after backend completion")
			}
		})
	}
}

func TestTypedEnvelopeRejectsExactResultAfterWorkspaceRevisionChanges(t *testing.T) {
	for name, call := range facadeEnvelopeCalls() {
		t.Run(name, func(t *testing.T) {
			s, documentURI, release, started, result := newBlockedFacadeCall(t, call)
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("typed facade did not reach the backend")
			}
			s.vfs.Open(documentURI, "go", 2, []byte("edited after capture"), vfs.SourceEditor)
			s.publishSnapshot()
			releaseOnce.Do(func() { close(release) })
			select {
			case got := <-result:
				if !ierrors.IsKind(got.err, ierrors.ErrContentModified) || got.exact {
					t.Fatalf("stale facade result exact=%t err=%v; want content-modified without Exact", got.exact, got.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("typed facade did not return after backend completion")
			}
		})
	}
}

func TestTypedReferencesRejectsPartialLocationsAfterWorkspaceRevisionChanges(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	started := make(chan struct{}, 1)
	backend := &partialBlockingReferencesBackend{blockingEnvelopeBackend{
		mockBackend: mockBackend{langID: "go", exts: []string{".facade"}},
		started:     started,
		release:     release,
	}}
	s := New(DefaultConfig())
	s.RegisterBackend("go", backend)
	const documentURI = "file:///partial.facade"
	s.vfs.Open(documentURI, "go", 1, []byte("initial"), vfs.SourceEditor)
	s.publishSnapshot()
	captured := withSnapshot(context.Background(), s.snapMgr.Current())
	type outcome struct {
		result identity.SemanticResult[[]languages.Location]
		err    error
	}
	result := make(chan outcome, 1)
	go func() {
		envelope, err := s.ReferencesEnvelope(captured, documentURI, 0, 0, true)
		result <- outcome{result: envelope, err: err}
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("typed references facade did not reach the backend")
	}
	s.vfs.Open(documentURI, "go", 2, []byte("edited after capture"), vfs.SourceEditor)
	s.publishSnapshot()
	releaseOnce.Do(func() { close(release) })
	select {
	case got := <-result:
		if !ierrors.IsKind(got.err, ierrors.ErrContentModified) {
			t.Fatalf("stale partial references error = %v, want content-modified", got.err)
		}
		if got.result.Status == identity.ResultExact || got.result.Status == identity.ResultPartial || len(got.result.Value) != 0 {
			t.Fatalf("stale partial locations escaped freshness check: %+v", got.result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("typed references facade did not return after backend completion")
	}
}
