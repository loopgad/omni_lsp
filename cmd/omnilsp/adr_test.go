package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestADRFilesExist enforces U9/Y5-3: the four closeout ADRs exist and carry
// a Status line.
func TestADRFilesExist(t *testing.T) {
	// Keyword fragments matched case-insensitively against each ADR's body.
	want := map[string]string{
		"0001": "in-process go backend",
		"0002": "jsonrpc",
		"0003": "pkgcache",
		"0004": "replay",
	}
	// ADR-0005 has no keyword contract yet; presence is enough.
	dir := filepath.Join("..", "..", "docs", "adr")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read ADR dir: %v", err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatal(rerr)
		}
		txt := string(data)
		prefix := e.Name()[:4]
		found[prefix] = true
		if !strings.Contains(txt, "Status") {
			t.Errorf("%s: missing Status field", e.Name())
		}
		if kw, ok := want[prefix]; ok && !strings.Contains(strings.ToLower(txt), kw) {
			t.Errorf("%s: expected keyword %q", e.Name(), kw)
		}
	}
	for prefix := range want {
		if !found[prefix] {
			t.Errorf("missing ADR %sxxx", prefix)
		}
	}
}
