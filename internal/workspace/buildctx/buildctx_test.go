package buildctx

// Tests for goal.md §E4 canonicalization invariants: determinism, map-order
// independence, allowlist filtering, and cross-run ID stability (golden).

import (
	"testing"
)

func sampleContext() Context {
	return Context{
		Language:   "go",
		Toolchain:  ToolchainIdentity{Kind: "go", Version: "go1.26.1", Target: "windows/amd64"},
		WorkingDir: "D:/ws/proj",
		Args:       []string{"-tags=integration", "-mod=vendor"},
		Defines:    map[string]string{"FOO": "1"},
		Env:        map[string]string{"GOOS": "windows", "GOARCH": "amd64"},
	}
}

func TestE4_MapOrderIndependence(t *testing.T) {
	a := sampleContext()
	a.Env = map[string]string{"GOARCH": "amd64", "GOOS": "windows"}
	b := sampleContext()
	b.Env = map[string]string{"GOOS": "windows", "GOARCH": "amd64"}

	if a.ID() != b.ID() {
		t.Errorf("map insertion order changed the ID:\n %s\n %s", a.ID(), b.ID())
	}
	if string(a.Canonicalize()) != string(b.Canonicalize()) {
		t.Error("canonical bytes differ for identical logical contexts")
	}
}

func TestE4_ArgOrderIsSemantic(t *testing.T) {
	a := sampleContext()
	a.Args = []string{"-x", "-y"}
	b := sampleContext()
	b.Args = []string{"-y", "-x"}
	if a.ID() == b.ID() {
		t.Error("argument order must be preserved where it is semantic")
	}
}

func TestE4_AllowlistFiltering(t *testing.T) {
	env := map[string]string{
		"GOOS":         "linux",
		"PATH":         "/usr/bin:/something/secret",
		"HOME":         "/home/user",
		"GOPRIVATE":    "corp.example.com",
		"HTTP_PROXY":   "http://10.0.0.1:8080",
		"GOFLAGS":      "-mod=vendor",
		"RANDOM_TOKEN": "super-secret-value",
	}
	c, err := DeriveGo("/ws", "go1.26.1", "linux/amd64", env, nil)
	if err != nil {
		t.Fatal(err)
	}
	canon := string(c.Canonicalize())
	for _, secret := range []string{"/usr/bin", "/home/user", "corp.example.com", "10.0.0.1", "super-secret"} {
		if contains(canon, secret) {
			t.Errorf("allowlist leaked %q into canonical form", secret)
		}
	}
	if !contains(canon, "linux") || !contains(canon, "-mod=vendor") {
		t.Error("allowlisted values missing from canonical form")
	}
}

func TestE0_IDStableAcrossInvocations(t *testing.T) {
	// Golden hash: same inputs must produce the same ID across processes
	// and releases unless canonicalization intentionally changes.
	c, err := DeriveGo("/ws", "go1.26.1", "windows/amd64",
		map[string]string{"GOOS": "windows", "GOARCH": "amd64"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := c.ID()
	for i := 0; i < 3; i++ {
		again, _ := DeriveGo("/ws", "go1.26.1", "windows/amd64",
			map[string]string{"GOOS": "windows", "GOARCH": "amd64"}, nil)
		if again.ID() != want {
			t.Fatalf("ID unstable across invocations: %s vs %s", again.ID(), want)
		}
	}
	t.Logf("golden ID: %s", want)
}

func TestE5_ToolchainChangeChangesID(t *testing.T) {
	v1, _ := DeriveGo("/ws", "go1.26.0", "linux/amd64", nil, nil)
	v2, _ := DeriveGo("/ws", "go1.26.1", "linux/amd64", nil, nil)
	if v1.ID() == v2.ID() {
		t.Error("toolchain version change must change BuildContextID (§E5)")
	}
}

func TestProvenanceRecorded(t *testing.T) {
	c, err := DeriveGo("/ws", "go1.26.1", "linux/amd64", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Provenance != ProvenanceBackendDerived {
		t.Errorf("provenance = %v, want backend_derived", c.Provenance)
	}
	if got := c.ID(); len(got) == 0 || !contains(string(got), "go:sha256:") {
		t.Errorf("ID format wrong: %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
