package golang

import (
	"context"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

// TestC4_RenamePropagatesEncodingSetToReferences pins that the rename
// request's negotiated encoding reaches the internal References hop.
//
// §C4 requires one position-encoding decision per request. Rename walks to
// References internally; when that hop dropped EncodingSet it fell back to
// the UTF-16 default while the caller had agreed UTF-8, so the returned
// edits were in the wrong unit on any line preceded by non-ASCII text.
//
// Line 2 below is 19 ASCII bytes followed by three CJK chars, which puts the
// use site at byte column 32 (UTF-8) but code-unit column 26 (UTF-16).
func TestC4_RenamePropagatesEncodingSetToReferences(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	b := newTestBackend(t)
	defer b.Close()
	const src = "package main\n\nfunc main() { _ = \"日本語\" + 変数 }\n\nvar 変数 = 1\n"
	uri := writeGoFile(t, b, "main.go", src)

	res, err := b.Rename(context.Background(), languages.RenameRequest{
		URI: uri, Content: []byte(src), SnapshotRev: 1,
		Line: 4, Column: 4, Encoding: 0, EncodingSet: true,
		NewName: "renamed",
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if res.Status != identity.ResultExact || !res.Value.Complete {
		t.Fatalf("status/complete = %v/%t, diagnostics %v", res.Status, res.Value.Complete, res.InternalDiagnostics)
	}
	var use *languages.TextEdit
	for i := range res.Value.Edits {
		if res.Value.Edits[i].StartLine == 2 {
			use = &res.Value.Edits[i]
		}
	}
	if use == nil {
		t.Fatalf("no edit on the use line 2: %+v", res.Value.Edits)
	}
	if use.StartChar != 32 || use.EndChar != 38 {
		t.Errorf("use-site edit columns = %d..%d, want 32..38 (UTF-8 byte units); "+
			"26..28 means the internal References hop fell back to UTF-16", use.StartChar, use.EndChar)
	}
}
