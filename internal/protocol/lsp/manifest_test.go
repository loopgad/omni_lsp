package lsp

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestMethodLinesForSkipsNonLiteralRegistrations pins the fingerprint's
// contract: only a direct `x.dispatcher.Register("literal", ...)` contributes a
// method line. Every other shape must be skipped silently, because a manifest
// built from a missed registration is worse than one missing it — the
// registered-method-set equality guard then passes against a fingerprint that
// no longer names what the server serves.
//
// The four shapes below map one-to-one onto the four skip branches.
func TestMethodLinesForSkipsNonLiteralRegistrations(t *testing.T) {
	src := `package p

func bad(d *Dispatcher, h Handler) {
	d.Register(h)                        // no arguments at all
	other.Register("textDocument/nope1") // receiver is not an identifier chain
	notDispatcher.Register("textDocument/nope2")
	s.dispatcher.Register(methodConst, h) // first argument is a named constant
	s.dispatcher.Register(fmt.Sprintf("textDocument/nope3"), h)
	d.nonRegister("textDocument/nope4")
}

func good(s *Server, h Handler) {
	s.dispatcher.Register("textDocument/hover", h)
	s.dispatcher.Register("textDocument/definition", h)
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "server.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, err := methodLinesFor(path)
	if err != nil {
		t.Fatalf("methodLinesFor: %v", err)
	}
	// methodLinesFor returns AST order; sorting is BuildMethodManifest's job,
	// so sort here rather than pinning the walk order.
	slices.Sort(got)
	want := []string{`method "textDocument/definition"`, `method "textDocument/hover"`}
	if !slices.Equal(got, want) {
		t.Fatalf("method lines = %q, want exactly %q", got, want)
	}
}

// TestMethodLinesForRejectsUnparsableSource keeps the error path honest: a file
// that does not parse must surface the parse error rather than an empty
// fingerprint that would look like "no methods registered".
func TestMethodLinesForRejectsUnparsableSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.go")
	if err := os.WriteFile(path, []byte("package p\nfunc ("), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if _, err := methodLinesFor(path); err == nil {
		t.Fatal("unparsable source returned no error; an empty fingerprint is indistinguishable from zero methods")
	}
}
