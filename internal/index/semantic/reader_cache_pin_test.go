package semantic

// Pin for round 23 OPT-4 (reader half): OpenReader retains the decoded,
// validated batches and Next serves from that cache — a generation is read
// and decoded once per reader, and the iterator never touches the segment
// files again after open (even corrupted-on-disk segments cannot surface).
// Rewind + Next replay the identical batches.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
)

func TestReaderNextServesCachedBatchesWithoutRereadingDisk(t *testing.T) {
	ctx := context.Background()
	store, root, req, report, sourceHash := fixture(t)

	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	sink, err := NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	scopeID := req.Scopes[0].ID
	if err := sink.WriteSymbols(ctx, []model.Symbol{
		{ID: "go:pkg.Func", ScopeID: scopeID, Name: "Func", Kind: "function"},
		{ID: "go:pkg.Type", ScopeID: scopeID, Name: "Type", Kind: "type"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{{
		SymbolID: "go:pkg.Func", ScopeID: scopeID, URI: "file:///repo/main.go",
		Range: model.Position{StartLine: 1, StartChar: 0, EndLine: 1, EndChar: 4}, Role: "reference",
		SourceHash: sourceHash, BuildContext: req.Scopes[0].BuildContext,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatal(err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	committed = true

	reopened, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	view, err := reopened.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(ctx, view)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer func() { _ = reader.Close() }()

	var firstPass []Batch
	for {
		batch, err := reader.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		firstPass = append(firstPass, batch)
	}
	if len(firstPass) == 0 {
		t.Fatal("no batches served")
	}

	// Corrupt the on-disk segment files after open: Next must serve from the
	// retained cache and never re-read.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := 0
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "seg-") && strings.HasSuffix(name, ".bin") {
			if err := os.WriteFile(filepath.Join(root, name), []byte("corrupted"), 0o600); err != nil {
				t.Fatal(err)
			}
			corrupted++
		}
	}
	if corrupted == 0 {
		t.Fatal("no segment files found to corrupt — fixture layout changed")
	}

	if err := reader.Rewind(); err != nil {
		t.Fatal(err)
	}
	var secondPass []Batch
	for {
		batch, err := reader.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("next after rewind with corrupted on-disk segments: %v (reader must serve cached batches)", err)
		}
		secondPass = append(secondPass, batch)
	}
	if !reflect.DeepEqual(firstPass, secondPass) {
		t.Fatal("rewind replay differs from the first pass")
	}
}
