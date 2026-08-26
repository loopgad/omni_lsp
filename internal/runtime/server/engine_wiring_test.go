package server

import (
	"context"
	"encoding/json"
	"testing"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// hoverCountingBackend counts hover invocations to prove memo semantics.
type hoverCountingBackend struct {
	mockBackend
	hovers int64
}

func (b *hoverCountingBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.hovers++
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: "hit"},
	}, nil
}

// failingHoverBackend returns a timeout-kind error for the first N hovers.
type failingHoverBackend struct {
	mockBackend
	failUntil int64
	calls     int64
}

func (b *failingHoverBackend) Hover(_ context.Context, _ languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	b.calls++
	if b.calls <= b.failUntil {
		return identity.SemanticResult[*languages.HoverResult]{},
			ierrors.New(ierrors.ErrTimeout, "go", "timed out")
	}
	return identity.SemanticResult[*languages.HoverResult]{
		Status: identity.ResultExact,
		Value:  &languages.HoverResult{Contents: "recovered"},
	}, nil
}

// TestJ_EngineMemoizesSemanticReads pins the §J wiring end to end.
func TestJ_EngineMemoizesSemanticReads(t *testing.T) {
	const uri = "file:///w/main.go"

	t.Run("same revision computes once", func(t *testing.T) {
		s := New(DefaultConfig())
		be := &hoverCountingBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
		s.RegisterBackend("go", be)
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		s.publishSnapshot()

		for i := 0; i < 3; i++ {
			resp := dispatchHoverRaw(s, uri)
			if resp == nil || resp.Error != nil {
				t.Fatalf("hover %d failed: %+v", i, resp)
			}
		}
		if n := be.hovers; n != 1 {
			t.Fatalf("backend hovered %d times for same revision, want 1", n)
		}
	})

	t.Run("revision advance recomputes", func(t *testing.T) {
		s := New(DefaultConfig())
		be := &hoverCountingBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
		s.RegisterBackend("go", be)
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		s.publishSnapshot()

		dispatchHoverOK(t, s, uri)
		s.vfs.Update(uri, 2, []byte("package main // edited\n"))
		s.publishSnapshot()
		dispatchHoverOK(t, s, uri)

		if n := be.hovers; n != 2 {
			t.Fatalf("backend hovered %d times across revisions, want 2", n)
		}
	})

	t.Run("transient failure is not memoized then recovers", func(t *testing.T) {
		s := New(DefaultConfig())
		be := &failingHoverBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}, failUntil: 1}
		s.RegisterBackend("go", be)
		s.vfs.Open(uri, "go", 1, []byte("package main\n"), 0)
		s.publishSnapshot()

		resp1 := dispatchHoverRaw(s, uri) // fails with timeout kind
		if resp1 == nil || resp1.Error == nil {
			t.Fatal("expected first hover to fail")
		}
		var stats = s.queries.Stats()
		if stats.Computations != 1 {
			t.Fatalf("unexpected computation count %d", stats.Computations)
		}
		resp2 := dispatchHoverRaw(s, uri) // retries and succeeds
		if resp2 == nil || resp2.Error != nil {
			t.Fatalf("second hover should recover: %+v", resp2)
		}
		if be.calls != 2 {
			t.Fatalf("backend calls = %d, want 2 (failure must not be cached)", be.calls)
		}
	})
}

func dispatchHoverOK(t *testing.T, s *Server, uri string) {
	t.Helper()
	resp := dispatchHoverRaw(s, uri)
	if resp == nil || resp.Error != nil {
		t.Fatalf("hover failed: %+v", resp)
	}
}

func dispatchHoverRaw(s *Server, uri string) *jsonrpc.Message {
	return s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 42}, "textDocument/hover",
		json.RawMessage(`{"textDocument":{"uri":"`+uri+`"},"position":{"line":0,"character":0}}`)))
}

func TestI12_ValidateEditSet(t *testing.T) {
	mk := func(uri string, sl, sc, el, ec uint32) languages.TextEdit {
		return languages.TextEdit{URI: uri, StartLine: sl, StartChar: sc, EndLine: el, EndChar: ec}
	}
	t.Run("empty and single pass", func(t *testing.T) {
		if err := ValidateEditSet(nil); err != nil {
			t.Fatal(err)
		}
		if err := ValidateEditSet([]languages.TextEdit{mk("a.go", 0, 0, 1, 0)}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("same-file overlap refused", func(t *testing.T) {
		err := ValidateEditSet([]languages.TextEdit{
			mk("a.go", 2, 0, 5, 10),
			mk("a.go", 5, 5, 7, 0), // overlaps at line 5
		})
		if err == nil || !contains(err.Error(), "overlapping") {
			t.Fatalf("expected overlap refusal, got %v", err)
		}
	})
	t.Run("adjacent same-file edits pass", func(t *testing.T) {
		if err := ValidateEditSet([]languages.TextEdit{
			mk("a.go", 2, 0, 3, 0),
			mk("a.go", 3, 0, 4, 0), // boundary-touching is not overlapping
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cross-file overlap impossible", func(t *testing.T) {
		if err := ValidateEditSet([]languages.TextEdit{
			mk("a.go", 2, 0, 5, 10),
			mk("b.go", 2, 0, 5, 10),
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func contains(s, sub string) bool {
	return len(sub) > 0 && indexOfBytes([]byte(s), []byte(sub)) >= 0
}

func indexOfBytes(hay, needle []byte) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
