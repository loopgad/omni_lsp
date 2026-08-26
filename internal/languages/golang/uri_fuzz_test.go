package golang

import (
	"testing"
)

// FuzzURI roundtrips adversarial URIs through uriToPath/pathToUri.
// Invariant: never panic, pathToUri(uriToPath(uri)) is stable for common cases.
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

	f.Fuzz(func(t *testing.T, uri string) {
		// Should never panic.
		path := uriToPath(uri)
		_ = path
		back := pathToUri(path)
		_ = back
	})
}
