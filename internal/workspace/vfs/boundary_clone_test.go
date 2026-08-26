package vfs

import "testing"

// TestD_BoundaryClone pins the write-boundary cloning contract: content
// handed to Open/Update is cloned once, so callers may freely mutate their
// slice afterwards without corrupting VFS state. This contract is what
// allows snapshot publication to share slices without copying.
func TestD_BoundaryClone(t *testing.T) {
	v := New()

	mine := []byte("caller-owned")
	v.Open("file:///a.go", "go", 1, mine, SourceEditor)
	mine[0] = 'X' // caller reuses its buffer — must not leak in
	if got := string(v.Content("file:///a.go")); got != "caller-owned" {
		t.Errorf("Open leaked caller mutation: %q", got)
	}

	mine = []byte("second-write")
	v.Update("file:///a.go", 2, mine)
	mine[0] = 'X'
	if got := string(v.Content("file:///a.go")); got != "second-write" {
		t.Errorf("Update leaked caller mutation: %q", got)
	}

	// Content() read boundary still copies (external mutation impossible).
	out := v.Content("file:///a.go")
	out[0] = 'X'
	if got := string(v.Content("file:///a.go")); got != "second-write" {
		t.Errorf("Content() leaked external mutation: %q", got)
	}
}
