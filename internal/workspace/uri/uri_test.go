package uri

// Tests for goal.md §D2/§D3 and §S2 property: parse -> canonical -> parse
// preserves identity; path conversion roundtrips for well-formed inputs.

import (
	"runtime"
	"testing"
)

func TestParseCanonicalIdempotent(t *testing.T) {
	inputs := []string{
		"file:///c:/Users/test/main.go",
		"file:///C:/Users/test/main.go",
		"file:///home/user/main.go",
		"file://server/share/proj/a.cpp",
		"file:///home/user/%E4%B8%AD%E6%96%87.go",
		"file:///home/user/中文.go",
		"https://example.com/x.txt",
	}
	for _, in := range inputs {
		u1, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		u2, err := Parse(u1.Canonical())
		if err != nil {
			t.Fatalf("re-Parse(%q): %v", u1.Canonical(), err)
		}
		if u1.Canonical() != u2.Canonical() {
			t.Errorf("idempotence broken: %q -> %q vs %q", in, u1.Canonical(), u2.Canonical())
		}
	}
}

func TestDriveLetterCaseCanonicalizes(t *testing.T) {
	a, err := Parse("file:///C:/x/y.go")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse("file:///c:/x/y.go")
	if err != nil {
		t.Fatal(err)
	}
	if a.Canonical() != b.Canonical() {
		t.Errorf("drive case must not affect identity:\n %q\n %q", a.Canonical(), b.Canonical())
	}
}

func TestEncodedDriveColonCanonicalizesWithoutChangingEscapedPathData(t *testing.T) {
	literal, err := Parse("file:///C:/Users/test/main.go")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Parse("file:///c%3A/Users/test/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if literal.Canonical() != encoded.Canonical() {
		t.Errorf("encoded drive colon must preserve Windows file identity:\n %q\n %q", literal.Canonical(), encoded.Canonical())
	}
	withEscapedUnreserved, err := Parse("file:///c:/Users/A%20B/%7Espace.py")
	if err != nil {
		t.Fatal(err)
	}
	withLiteralUnreserved, err := Parse("file:///C:/Users/A%20B/~space.py")
	if err != nil {
		t.Fatal(err)
	}
	if withEscapedUnreserved.Canonical() != withLiteralUnreserved.Canonical() {
		t.Errorf("encoded unreserved path characters must preserve identity:\n %q\n %q", withEscapedUnreserved.Canonical(), withLiteralUnreserved.Canonical())
	}

	withEscapedSlash, err := Parse("file:///C:/Users/a%2Fb/main.go")
	if err != nil {
		t.Fatal(err)
	}
	withPathSeparator, err := Parse("file:///C:/Users/a/b/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if withEscapedSlash.Canonical() == withPathSeparator.Canonical() {
		t.Errorf("canonicalization must preserve escaped path data: %q", withEscapedSlash.Canonical())
	}
}

func TestDisplaySpellingPreserved(t *testing.T) {
	in := "file:///C:/Users/Test/Main.GO"
	u, err := Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != in {
		t.Errorf("display corrupted: %q", u.String())
	}
}

func TestFromPathRoundTrip(t *testing.T) {
	var paths []string
	if runtimeIsWindows() {
		paths = []string{`C:\x\y\main.go`, `C:\中文\测试.go`, `\\server\share\a.cpp`}
	} else {
		paths = []string{"/x/y/main.go", "/中文/测试.go"}
	}
	for _, p := range paths {
		u := FromPath(p)
		got, err := u.Path()
		if err != nil {
			t.Fatalf("Path(%q): %v", p, err)
		}
		if got != p {
			t.Errorf("path roundtrip: got %q, want %q (uri=%s)", got, p, u.String())
		}
	}
}

func TestNonFileSchemeRejected(t *testing.T) {
	u, err := Parse("untitled:Untitled-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Path(); err == nil {
		t.Error("non-file URI must not produce a filesystem path")
	}
	if u.IsFile() {
		t.Error("IsFile should be false for untitled scheme")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "no-scheme-here", "main.go"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) should fail", in)
		}
	}
}

func TestWindowsPathSemantics(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only assertions")
	}
	u, err := Parse("file:///d:/proj/main.go")
	if err != nil {
		t.Fatal(err)
	}
	p, err := u.Path()
	if err != nil {
		t.Fatal(err)
	}
	if p != `D:\proj\main.go` {
		t.Errorf("got %q, want D:\\proj\\main.go", p)
	}

	unc, err := Parse("file://SERVER/share/f.h")
	if err != nil {
		t.Fatal(err)
	}
	up, err := unc.Path()
	if err != nil {
		t.Fatal(err)
	}
	want := `\\SERVER\share\f.h`
	if up != want && up != `\\server\share\f.h` {
		t.Errorf("UNC path = %q, want %q family", up, want)
	}
}
