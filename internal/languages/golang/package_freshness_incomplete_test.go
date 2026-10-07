package golang

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/workspace/snapshot"
	"golang.org/x/tools/go/packages"
)

// TestPackageErrorsCacheableClassifiesByErrorKind locks the fingerprint gate's
// error classification without a toolchain: go list layout verdicts
// (packages.ListError) may back a cache entry keyed by the input closure,
// while genuine type/parse faults, unclassifiable errors, module-graph errors,
// and IllTyped packages without a local error all fail closed.
func TestPackageErrorsCacheableClassifiesByErrorKind(t *testing.T) {
	layout := packages.Error{
		Pos:  "-",
		Msg:  "C source files not allowed when not using cgo or SWIG: c-perf.c",
		Kind: packages.ListError,
	}
	cases := []struct {
		name string
		pkg  *packages.Package
		want bool
	}{
		{"clean", &packages.Package{}, true},
		{"ill-typed without local error", &packages.Package{IllTyped: true}, false},
		{"layout list error", &packages.Package{Errors: []packages.Error{layout}}, true},
		{"ill-typed layout list error", &packages.Package{IllTyped: true, Errors: []packages.Error{layout}}, true},
		{"type error", &packages.Package{Errors: []packages.Error{{Kind: packages.TypeError}}}, false},
		{"parse error", &packages.Package{Errors: []packages.Error{{Kind: packages.ParseError}}}, false},
		{"unknown error", &packages.Package{Errors: []packages.Error{{Kind: packages.UnknownError}}}, false},
		{"layout error mixed with type error", &packages.Package{Errors: []packages.Error{layout, {Kind: packages.TypeError}}}, false},
		{"module error", &packages.Package{Module: &packages.Module{Error: &packages.ModuleError{Err: "missing go.sum entry"}}}, false},
	}
	for _, testCase := range cases {
		if got := packageErrorsCacheable(testCase.pkg); got != testCase.want {
			t.Errorf("%s: packageErrorsCacheable = %t, want %t", testCase.name, got, testCase.want)
		}
	}
}

// TestLayoutListErrorPackageWithCGOIsCachedAndRevalidates locks the PERF-2
// backend-side fix end to end: with CGO_ENABLED=1 and a C source next to the
// Go package (the S18 mixed layout), go list reports the package Incomplete
// through a ListError, the input-fingerprint gate must release it, the package
// cache must fill on the first request, and a second identical request must be
// a cache hit that never re-runs packages.Load.
func TestLayoutListErrorPackageWithCGOIsCachedAndRevalidates(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	t.Setenv("CGO_ENABLED", "1")
	b := newTestBackend(t)
	defer b.Close()
	ctx := context.Background()

	mainContent := []byte("package main\n\nfunc main() {}\n")
	mainURI := writeGoFile(t, b, "main.go", string(mainContent))
	if err := os.WriteFile(filepath.Join(b.workDir, "c-perf.c"), []byte("int omnilsp_perf_marker;\n"), 0o644); err != nil {
		t.Fatalf("write C source: %v", err)
	}

	pkg, err := b.loadPackage(ctx, mainURI, mainContent, 1)
	if err != nil {
		t.Fatalf("initial package load: %v", err)
	}
	if len(pkg.Errors) != 1 || pkg.Errors[0].Kind != packages.ListError {
		t.Fatalf("mixed-layout fixture errors = %+v, want exactly one go list layout error (fixture drifted)", pkg.Errors)
	}
	hits, misses := b.CacheStats()
	if hits != 0 || misses != 1 {
		t.Fatalf("cache stats after first load = %d hits / %d misses, want 0/1", hits, misses)
	}

	fingerprint, complete, err := packageInputFingerprint(ctx, pkg, uriToPath(mainURI), packageOverlays{}, b.BuildContextID(), b.goWorkPath, b.goFlags)
	if err != nil {
		t.Fatalf("packageInputFingerprint: %v", err)
	}
	if !complete || fingerprint == "" {
		t.Fatal("input-fingerprint gate rejected a layout-incomplete package; the cache would never fill")
	}

	captured := snapshot.New("go-package-cache-layout-test", 1, map[string]snapshot.DocumentSnapshot{
		mainURI: {URI: mainURI, LanguageID: "go", Content: mainContent},
	})
	semanticIdentity, err := b.SemanticInputFingerprint(ctx, captured, mainURI)
	if err != nil {
		t.Fatalf("SemanticInputFingerprint: %v", err)
	}
	if semanticIdentity == "" {
		t.Fatal("SemanticInputFingerprint returned empty for a cached layout-incomplete package")
	}

	second, err := b.loadPackage(ctx, mainURI, mainContent, 1)
	if err != nil {
		t.Fatalf("second package load: %v", err)
	}
	if second != pkg {
		t.Fatal("second identical request did not reuse the cached package")
	}
	hits, misses = b.CacheStats()
	if hits != 1 || misses != 1 {
		t.Fatalf("cache stats after second load = %d hits / %d misses, want 1/1 (second request must not re-run packages.Load)", hits, misses)
	}
}

// assertRealErrorPackageStaysUncacheable grounds the fixture as a genuine
// toolchain-reported fault of the wanted kind, then locks the rejection from
// both sides: the input-fingerprint gate must refuse the package and two
// identical requests must both miss the package cache.
func assertRealErrorPackageStaysUncacheable(t *testing.T, content []byte, want packages.ErrorKind) {
	t.Helper()
	b := newTestBackend(t)
	defer b.Close()
	ctx := context.Background()
	mainURI := writeGoFile(t, b, "main.go", string(content))
	pkg, err := b.loadPackage(ctx, mainURI, content, 1)
	if err != nil {
		t.Fatalf("initial package load: %v", err)
	}
	// On the Windows x/tools overlay compile path Errors[0] is the go list
	// wrapper output; type/parse errors land in later entries, so match on
	// any error carrying the wanted kind rather than the first one.
	hasWant := false
	for _, e := range pkg.Errors {
		if e.Kind == want {
			hasWant = true
			break
		}
	}
	if len(pkg.Errors) == 0 || !hasWant {
		t.Fatalf("fixture errors = %+v, want an error of kind %v (fixture drifted)", pkg.Errors, want)
	}
	fingerprint, complete, err := packageInputFingerprint(ctx, pkg, uriToPath(mainURI), packageOverlays{}, b.BuildContextID(), b.goWorkPath, b.goFlags)
	if err != nil {
		t.Fatalf("packageInputFingerprint: %v", err)
	}
	if complete || fingerprint != "" {
		t.Fatalf("real %v package produced a cacheable fingerprint %q", want, fingerprint)
	}
	if _, err := b.loadPackage(ctx, mainURI, content, 1); err != nil {
		t.Fatalf("second package load: %v", err)
	}
	hits, misses := b.CacheStats()
	if hits != 0 || misses != 2 {
		t.Fatalf("cache stats after two loads = %d hits / %d misses, want 0/2 (real errors must never fill the cache)", hits, misses)
	}
}

func TestTypeErrorPackageStaysOutOfPackageCache(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	assertRealErrorPackageStaysUncacheable(t, []byte("package main\n\nfunc main() { undefinedSymbol() }\n"), packages.TypeError)
}

func TestParseErrorPackageStaysOutOfPackageCache(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	assertRealErrorPackageStaysUncacheable(t, []byte("package main\n\nfunc {{{ }\n"), packages.ParseError)
}
