package ccls

import (
	"errors"
	"testing"
)

// TestParseClangdVersion pins the version-probe contract: the token after
// "version" wins; errors propagate; unparseable output refuses.
func TestParseClangdVersion(t *testing.T) {
	cases := []struct {
		out     string
		err     error
		want    string
		wantErr bool
	}{
		{"clangd version 18.1.3\n  features: linux", nil, "18.1.3", false},
		{"clangd version 19\n", nil, "19", false},
		{"garbage output entirely", nil, "", true},
		{"version", nil, "", true}, // "version" as the last field: nothing follows
		{"", errBoom, "", true},
	}
	for _, c := range cases {
		got, err := parseClangdVersion(c.out, c.err)
		if (err != nil) != c.wantErr {
			t.Errorf("parseClangdVersion(%q): err=%v wantErr=%v", c.out, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("parseClangdVersion(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

var errBoom = errors.New("probe failed")

// TestMacroHeuristicBoundaries pins §H2.4's v1 heuristic: ALL_CAPS-with-digit/
// underscore names of length>=3 are macro suspects; mixed-case and short
// names are not; out-of-range positions yield nothing (never panic).
func TestMacroHeuristicBoundaries(t *testing.T) {
	src := []byte("int MAX_SIZE = 1;\nfloat ratio = MAX_SIZE;\n")
	if d := macroSuspectDiag(src, 0, 4); len(d) != 1 {
		t.Errorf("MAX_SIZE should be suspect, got %v", d)
	}
	if d := macroSuspectDiag(src, 1, 14); len(d) != 1 {
		t.Errorf("heuristic is name-shape based, not position based: %v", d)
	}
	if d := macroSuspectDiag(src, 0, 0); len(d) != 0 {
		t.Errorf("'int' is lowercase, got %v", d)
	}
	// Out-of-range positions must not panic and must not flag.
	for _, pos := range [][2]uint32{{99, 0}, {0, 9999}} {
		if d := macroSuspectDiag(src, pos[0], pos[1]); len(d) != 0 {
			t.Errorf("pos %v out of range flagged %v", pos, d)
		}
	}
	for name, want := range map[string]bool{"AB": false, "abc": false, "_X": false, "MAX": true, "A_1": true} {
		if got := isMacroName(name); got != want {
			t.Errorf("isMacroName(%q) = %v, want %v", name, got, want)
		}
	}
}
