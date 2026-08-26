package uri

import (
	"testing"
)

// FuzzURIParse verifies no panic and canonical idempotence on arbitrary input
// (goal.md §S3 fuzz target "URI parsing", §S2 property).
func FuzzURIParse(f *testing.F) {
	seeds := []string{
		"file:///c:/x/y.go",
		"file:///C:/x/y.go",
		"file://server/share/x.cpp",
		"file:///home/%E4%B8%AD.go",
		"untitled:Untitled-1",
		"https://example.com/a?b#c",
		"file:///a b/spaces.txt",
		"file:///incomplete%",
		"main.go",
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		u, err := Parse(in)
		if err != nil {
			return // rejection is fine
		}
		// Canonical idempotence: re-parsing the canonical form must be stable.
		u2, err := Parse(u.Canonical())
		if err != nil {
			t.Fatalf("canonical form not parseable: %q -> %q: %v", in, u.Canonical(), err)
		}
		if u2.Canonical() != u.Canonical() {
			t.Fatalf("idempotence broken: %q -> %q vs %q", in, u.Canonical(), u2.Canonical())
		}
		// Display spelling is never corrupted for accepted inputs.
		if u.String() != in {
			t.Fatalf("display corrupted: %q != %q", u.String(), in)
		}
		// Path() must never panic; file URIs with hosts on non-Windows are
		// allowed to error but only with a message, never a crash.
		_, _ = u.Path()
	})
}
