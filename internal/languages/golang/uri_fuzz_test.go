package golang

import (
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/workspace/uri"
)

// FuzzURI roundtrips adversarial URIs through uriToPath/pathToUri.
// Invariant: never panic; for well-formed file URIs the uri→path→uri
// roundtrip is stable and the rebuilt URI is a canonical identity.
func FuzzURI(f *testing.F) {
	seeds := []string{
		"file:///hello/world.go",
		"file:///C:/Users/test/main.go",
		"file:///usr/local/bin/foo.go",
		"",
		"file:///",
		"file:///path%20with%20spaces.go",
		"file:///path/with/中文.go",
		"not-a-uri",
		"file:///",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		// Should never panic.
		path := uriToPath(raw)
		back := pathToUri(path)

		u, err := uri.Parse(raw)
		if err != nil || !u.IsFile() {
			return
		}
		enginePath, perr := u.Path()
		if perr != nil {
			return
		}
		// uriToPath must decode through the engine, not the string-trim
		// fallback: a percent-encoded path would come back still-encoded.
		if path != enginePath {
			t.Fatalf("uriToPath(%q) = %q, want engine-decoded path %q", raw, path, enginePath)
		}
		// The path→URI roundtrip is only lossless for paths whose characters
		// FromPath re-encodes losslessly: control bytes are parked in a
		// non-reparseable opaque form, and '?', '#' and '%' change meaning
		// (query/fragment split, escape prefix) when spelled raw in a URI.
		if strings.ContainsAny(enginePath, "?#%") ||
			strings.IndexFunc(enginePath, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
			return
		}
		// The rebuilt URI must parse and already be in canonical form (D2/S2
		// idempotence) — otherwise maps keyed by canonical identity would
		// miss documents this roundtrip just produced.
		bu, berr := uri.Parse(back)
		if berr != nil {
			t.Fatalf("pathToUri(%q) = %q does not re-parse: %v", path, back, berr)
		}
		if bu.Canonical() != back {
			t.Fatalf("pathToUri(%q) = %q is not canonical (canonical form %q)",
				path, back, bu.Canonical())
		}
		// Stability: re-running the roundtrip on its own output is a fixed
		// point.
		if again := pathToUri(uriToPath(back)); again != back {
			t.Fatalf("uri→path→uri roundtrip unstable: %q → %q → %q", raw, back, again)
		}
	})
}
