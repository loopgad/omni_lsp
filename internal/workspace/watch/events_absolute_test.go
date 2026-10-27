package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPollerEventsCarryAbsoluteWorkspacePaths pins the D14 event contract the
// server relies on (audit finding 6): every event Path must be an absolute
// path. The server turns each event into a file URI directly, and a relative
// path parses as a UNC host (file://pkg/a.go -> \\pkg\a.go), silently losing
// the per-file invalidation for the real file.
func TestPollerEventsCarryAbsoluteWorkspacePaths(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a.go")
	if err := os.WriteFile(file, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := New(root, time.Hour, nil)
	p.Scan()
	if err := os.WriteFile(file, []byte("after!"), 0o644); err != nil {
		t.Fatal(err)
	}
	events := p.Scan()
	if len(events) != 1 {
		t.Fatalf("unexpected events: %+v", events)
	}
	event := events[0]
	if !filepath.IsAbs(event.Path) {
		t.Fatalf("event path %q is not absolute; the server would parse it as a UNC host", event.Path)
	}
	if event.Path != file {
		t.Fatalf("event path %q, want %q", event.Path, file)
	}
	if event.Kind != Modified {
		t.Fatalf("event kind %d, want Modified (%d)", event.Kind, Modified)
	}
}
