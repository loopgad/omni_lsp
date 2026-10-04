package golang

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

func TestJ0_PackageLoadHonorsCanceledContext(t *testing.T) {
	b := newTestBackend(t)
	defer b.Close()
	src := "package main\nfunc main() {}\n"
	uri := writeGoFile(t, b, "cancel.go", src)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := b.loadPackage(ctx, uri, []byte(src), 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("package load returned %v, want context.Canceled", err)
	}
}

func TestGoFunctionHoverSignatureDoesNotRepeatFuncKeyword(t *testing.T) {
	got := formatGoFunctionHoverSignature("sym050", "func(x int) int")
	if want := "func sym050(x int) int"; got != want {
		t.Fatalf("formatted hover signature = %q, want %q", got, want)
	}
}

func TestJ0_PackageLoadPassesContextToGoPackages(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	src := "package main\nfunc main() {}\n"
	uri := writeGoFile(t, b, "cancel_during_load.go", src)
	ctx := &backendCancelOnDoneContext{Context: context.Background(), done: make(chan struct{})}
	_, err := b.loadPackage(ctx, uri, []byte(src), 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("package load returned %v, want cancellation from packages.Load context", err)
	}
	if !ctx.activated.Load() {
		t.Fatal("packages.Load never consulted the request context")
	}
}

func TestJ0_BackendLockWaitHonorsCancellation(t *testing.T) {
	b := newTestBackend(t)
	defer b.Close()
	<-b.gate // hold the serialized backend state while the request waits
	defer func() { b.gate <- struct{}{} }()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &backendObservedContext{Context: base, observed: make(chan struct{}), observeAt: 1}
	uri := writeGoFile(t, b, "lock.go", "package main\nfunc main() {}\n")
	done := make(chan error, 1)
	go func() {
		_, err := b.Completion(ctx, languages.CompletionRequest{URI: uri, Content: []byte("package main\n")})
		done <- err
	}()
	select {
	case <-ctx.observed:
	case <-time.After(time.Second):
		t.Fatal("request did not wait on the backend lock")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Completion err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("backend request stayed blocked after cancellation")
	}
}

func TestJ0_ReferencesTraversalHonorsCancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	var src strings.Builder
	src.WriteString("package main\nvar target int\nfunc use() {\n")
	for i := 0; i < 4096; i++ {
		src.WriteString("_ = target\n")
	}
	src.WriteString("}\n")
	content := []byte(src.String())
	uri := writeGoFile(t, b, "many_refs.go", string(content))
	req := languages.ReferencesRequest{URI: uri, Content: content, Line: 1, Column: 5, SnapshotRev: 1}
	if _, err := b.References(context.Background(), req); err != nil {
		t.Fatalf("warm references: %v", err)
	}
	ctx := &backendCancelAfterErrContext{Context: context.Background(), done: make(chan struct{}), cancelAt: 6}
	if _, err := b.References(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("References err = %v, want cancellation during traversal", err)
	}
	if calls := ctx.calls.Load(); calls < ctx.cancelAt {
		t.Fatalf("context checked %d times, want traversal polling to reach %d", calls, ctx.cancelAt)
	}
}

func TestReferencesIncludeDeclarationDeduplicatesRenameEdits(t *testing.T) {
	b := newTestBackend(t)
	defer b.Close()

	declarationSource := []byte("package main\nvar localScopeValue = 1\n")
	declarationURI := writeGoFile(t, b, "declaration.go", string(declarationSource))
	lineText := "func use() int { return localScopeValue }"
	source := []byte("package main\n" + lineText + "\n")
	fileURI := writeGoFile(t, b, "references.go", string(source))
	line := uint32(1)
	column := uint32(strings.Index(lineText, "localScopeValue"))
	nameEnd := column + uint32(len("localScopeValue"))

	references, err := b.References(context.Background(), languages.ReferencesRequest{
		URI: fileURI, Content: source, SnapshotRev: 1,
		Line: line, Column: column, IncludeDecl: true,
	})
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	declaration := languages.Location{
		URI: declarationURI,
		Range: languages.Range{
			StartLine: 1, StartCharacter: 4,
			EndLine: 1, EndCharacter: 4 + uint32(len("localScopeValue")),
		},
	}
	use := languages.Location{
		URI: fileURI,
		Range: languages.Range{
			StartLine: line, StartCharacter: column,
			EndLine: line, EndCharacter: nameEnd,
		},
	}
	declarationCount := 0
	useCount := 0
	for _, reference := range references.Value {
		if reference == declaration {
			declarationCount++
		}
		if reference == use {
			useCount++
		}
	}
	if declarationCount != 1 {
		t.Fatalf("references contain declaration %d times; want exactly once: %+v", declarationCount, references.Value)
	}
	if useCount != 1 || len(references.Value) != 2 {
		t.Fatalf("references contain use %d times and total %d locations; want one declaration and one use across files: %+v", useCount, len(references.Value), references.Value)
	}

	renamed, err := b.Rename(context.Background(), languages.RenameRequest{
		URI: fileURI, Content: source, SnapshotRev: 1,
		Line: line, Column: column, NewName: "renamedLocalScopeValue",
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Status != identity.ResultExact || !renamed.Value.Complete {
		t.Fatalf("Rename status/complete = %v/%t, diagnostics %v", renamed.Status, renamed.Value.Complete, renamed.InternalDiagnostics)
	}
	if len(renamed.Value.Edits) != 2 {
		t.Fatalf("rename edits = %+v; want one edit for declaration and one for use", renamed.Value.Edits)
	}
	editURIs := map[string]bool{}
	for i, edit := range renamed.Value.Edits {
		editURIs[edit.URI] = true
		for j := i + 1; j < len(renamed.Value.Edits); j++ {
			other := renamed.Value.Edits[j]
			if edit.URI == other.URI && editRangesOverlap(edit, other) {
				t.Fatalf("rename edits overlap: %+v and %+v", edit, other)
			}
		}
	}
	if !editURIs[declarationURI] || !editURIs[fileURI] {
		t.Fatalf("rename edits did not preserve both declaration/use URIs: %+v", renamed.Value.Edits)
	}
}

func TestDiagnosticsKeepPrimaryGoSyntaxErrorAndSuppressEOFCascades(t *testing.T) {
	b := newTestBackend(t)
	defer b.Close()
	content := []byte("package broken\nvar wanted int\nfunc use() {\n    _ = wanted\n}\nfunc broken( {\n")
	uri := writeGoFile(t, b, "broken.go", string(content))

	diags, err := b.DiagnosticsWithEncoding(context.Background(), uri, content, 1, 1)
	if err != nil {
		t.Fatalf("DiagnosticsWithEncoding: %v", err)
	}
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want only the primary syntax error: %+v", len(diags), diags)
	}
	got := diags[0]
	if got.StartLine != 5 || got.StartChar != 13 || got.EndLine != got.StartLine || got.EndChar != got.StartChar {
		t.Fatalf("syntax diagnostic range = %d:%d-%d:%d, want zero-width 5:13 point", got.StartLine, got.StartChar, got.EndLine, got.EndChar)
	}
	if got.Message != "expected ')', found '{'" {
		t.Fatalf("syntax diagnostic message = %q, want primary parser error", got.Message)
	}
}

func TestDiagnosticsCoverWholeGoIdentifiersInClientEncoding(t *testing.T) {
	b := newTestBackend(t)
	defer b.Close()
	content := []byte("package main\nvar _ = missingASCII\nvar _ = missingλName\n")
	uri := writeGoFile(t, b, "undefined.go", string(content))

	for _, tc := range []struct {
		name         string
		encoding     int
		unicodeWidth uint32
	}{
		{name: "UTF-8", encoding: 0, unicodeWidth: 13},
		{name: "UTF-16", encoding: 1, unicodeWidth: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diags, err := b.DiagnosticsWithEncoding(context.Background(), uri, content, 1, tc.encoding)
			if err != nil {
				t.Fatalf("DiagnosticsWithEncoding: %v", err)
			}
			if len(diags) != 2 {
				t.Fatalf("got %d diagnostics, want 2 undefined identifiers: %+v", len(diags), diags)
			}

			want := map[string]struct {
				line  uint32
				width uint32
			}{
				"missingASCII": {line: 1, width: 12},
				"missingλName": {line: 2, width: tc.unicodeWidth},
			}
			for _, d := range diags {
				var identifier string
				for name := range want {
					if strings.Contains(d.Message, name) {
						identifier = name
						break
					}
				}
				expected, ok := want[identifier]
				if !ok {
					t.Fatalf("unexpected diagnostic: %+v", d)
				}
				if d.StartLine != expected.line || d.StartChar != 8 || d.EndLine != expected.line || d.EndChar != 8+expected.width {
					t.Errorf("%s range = %d:%d-%d:%d, want %d:8-%d:%d", identifier, d.StartLine, d.StartChar, d.EndLine, d.EndChar, expected.line, expected.line, 8+expected.width)
				}
				delete(want, identifier)
			}
			if len(want) != 0 {
				t.Errorf("missing diagnostics for identifiers: %v", want)
			}
		})
	}
}

func editRangesOverlap(left, right languages.TextEdit) bool {
	positionBefore := func(line, character, otherLine, otherCharacter uint32) bool {
		return line < otherLine || (line == otherLine && character < otherCharacter)
	}
	return positionBefore(left.StartLine, left.StartChar, right.EndLine, right.EndChar) &&
		positionBefore(right.StartLine, right.StartChar, left.EndLine, left.EndChar)
}

type backendObservedContext struct {
	context.Context
	observed  chan struct{}
	observeAt int32
	calls     atomic.Int32
	once      sync.Once
}

func (c *backendObservedContext) Err() error {
	if c.calls.Add(1) == c.observeAt {
		c.once.Do(func() { close(c.observed) })
	}
	return c.Context.Err()
}

type backendCancelAfterErrContext struct {
	context.Context
	done     chan struct{}
	cancelAt int32
	calls    atomic.Int32
	once     sync.Once
}

type backendCancelOnDoneContext struct {
	context.Context
	done      chan struct{}
	activated atomic.Bool
	once      sync.Once
}

func (c *backendCancelOnDoneContext) Done() <-chan struct{} {
	c.once.Do(func() {
		c.activated.Store(true)
		close(c.done)
	})
	return c.done
}

func (c *backendCancelOnDoneContext) Err() error {
	if c.activated.Load() {
		return context.Canceled
	}
	return nil
}

func (c *backendCancelAfterErrContext) Done() <-chan struct{} { return c.done }
func (c *backendCancelAfterErrContext) Err() error {
	if c.calls.Add(1) >= c.cancelAt {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}

func newTestBackend(t *testing.T) *Backend {
	t.Helper()
	dir := t.TempDir()
	goMod := `module testmod
go 1.26`
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	return New(dir)
}

func writeGoFile(t *testing.T, b *Backend, name, content string) string {
	t.Helper()
	fpath := filepath.Join(b.workDir, name)
	if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fpath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return pathToUri(fpath)
}

func TestBackendLocked(t *testing.T) {
	b := New(t.TempDir())
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLanguageID(t *testing.T) {
	b := New(t.TempDir())
	if b.LanguageID() != "go" {
		t.Errorf("LanguageID() = %q, want go", b.LanguageID())
	}
}

func TestFileExtensions(t *testing.T) {
	b := New(t.TempDir())
	exts := b.FileExtensions()
	if len(exts) != 1 || exts[0] != ".go" {
		t.Errorf("FileExtensions() = %v, want [.go]", exts)
	}
}

func TestDocumentSymbols_Parseable(t *testing.T) {
	b := New(t.TempDir())
	src := `package main
func foo(x int) {}
type Bar struct{ X int }`
	syms, err := b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
		URI:     "file:///tmp/test.go",
		Content: []byte(src),
	})
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(syms) == 0 {
		t.Fatal("expected at least one symbol")
	}
	found := false
	for _, s := range syms {
		if s.Name == "foo" {
			found = true
			if !s.SelectionRangeSet || s.SelectionLine != 1 || s.SelectionCharacter != 5 || s.SelectionEndLine != 1 || s.SelectionEndCharacter != 8 {
				t.Fatalf("foo selection range = %+v, want AST identifier range 1:5-1:8", s)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected foo symbol, got: %v", syms)
	}
}

func TestURIPathConversion(t *testing.T) {
	uri := "file:///hello/world.go"
	got := uriToPath(uri)
	if got == "" {
		t.Error("uriToPath should not return empty")
	}
	back := pathToUri(got)
	if back == "" {
		t.Error("pathToUri should not return empty")
	}
}

func TestH_CompletionNoDeadlock(t *testing.T) {
	b := newTestBackend(t)
	done := make(chan error, 1)
	go func() {
		// 不存在的子目录让 packages.Load 快速失败，走语法回退路径；
		// 无论返回结果还是错误，都必须在锁未被重入时及时返回。
		_, err := b.Completion(context.Background(), languages.CompletionRequest{
			URI:     pathToUri(filepath.Join(b.workDir, "missing", "x.go")),
			Content: []byte("package p\n"),
		})
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Completion 挂起：b.mu 在持有状态下被 locked() 重入死锁")
	}
}

func TestConcurrentAccess(t *testing.T) {
	b := New(t.TempDir())
	done := make(chan struct{}, 5)
	for i := 0; i < 5; i++ {
		go func() {
			defer func() { recover() }()
			_, _ = b.DocumentSymbols(context.Background(), languages.DocumentSymbolRequest{
				URI:     "file:///tmp/x.go",
				Content: []byte("package main\n"),
			})
			done <- struct{}{}
		}()
	}
	for i := 0; i < 5; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("goroutine leak")
		}
	}
}
