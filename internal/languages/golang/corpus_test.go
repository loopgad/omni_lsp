package golang

// Differential corpus runner (§X2 exit criterion): every corpus file must
// yield non-empty hover, resolvable definitions, and evidence carrying a
// digest-backed build context. Each file runs in its OWN backend/workspace —
// corpus files intentionally use different package names and must not share
// a compilation directory. Files live under testdata so the go tool never
// compiles them as part of this package.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/test/corpus"
)

const corpusDir = "../../../test/corpus/testdata/go"

func TestCorpus_GoFilesProduceGroundedSemantics(t *testing.T) {
	if testing.Short() {
		t.Skip("requires go toolchain")
	}
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("corpus dir: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("corpus too thin: %d files", len(entries))
	}

	var buckets corpus.ErrorBuckets
	const rev = uint64(1)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(corpusDir, e.Name())
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("%s: %v", path, rerr)
		}
		t.Run(e.Name(), func(t *testing.T) {
			b := newTestBackend(t) // isolated workspace per corpus file
			defer b.Close()
			uri := writeGoFile(t, b, e.Name(), string(src))
			bcID := b.BuildContextID() // E7: caller injects the build context

			line, col, sym, ok := findFuncSymbol(src)
			if !ok {
				t.Skip("no func declaration in corpus file")
			}

			hres, herr := b.Hover(t.Context(), languages.HoverRequest{
				URI: uri, Content: src, Line: uint32(line), Column: uint32(col),
				SnapshotRev: rev, BuildContext: bcID,
			})
			if herr != nil {
				t.Fatalf("hover error: %v", herr)
			}
			if hres.Status != identity.ResultExact {
				t.Errorf("hover on grounded corpus must be exact, got %v (diag=%v)",
					hres.Status, hres.InternalDiagnostics)
			}
			if hres.Value == nil || hres.Value.Contents == "" {
				t.Fatalf("hover must be non-empty at func symbol (status=%v diag=%v)",
					hres.Status, hres.InternalDiagnostics)
			}
			if len(hres.Evidence) == 0 {
				t.Error("§B4: hover evidence missing")
			} else {
				for _, ev := range hres.Evidence {
					if ev.BuildContext == "" || ev.BuildContext == "unavailable" {
						t.Errorf("evidence lacks digest-backed build context: %+v", ev)
					}
				}
			}

			dres, derr := b.Definition(t.Context(), languages.DefinitionRequest{
				URI: uri, Content: src, Line: uint32(line), Column: uint32(col),
				SnapshotRev: rev, BuildContext: bcID,
			})
			if derr != nil {
				t.Fatalf("definition error: %v", derr)
			}
			if dres.Value == nil {
				t.Errorf("definition must return a location list (status=%v diag=%v)",
					dres.Status, dres.InternalDiagnostics)
			}

			// §S21: sort every response into the six release-blocking error
			// buckets — Go now rides the same classifier as TestS21.
			req := corpus.ClassifyRequest{URI: uri, Content: src, Symbol: sym, Rev: rev}
			hresp := corpus.HoverEnvelope(hres, herr)
			req.Epoch = hresp.Epoch // baseline from first response of this session
			logBucketHits(t, "hover", buckets.Record(req, hresp))
			logBucketHits(t, "definition", buckets.Record(req,
				corpus.LocationsEnvelope("definition", dres, derr)))

			rres, rerr := b.References(t.Context(), languages.ReferencesRequest{
				URI: uri, Content: src, Line: uint32(line), Column: uint32(col),
				SnapshotRev: rev, BuildContext: bcID, IncludeDecl: true,
			})
			logBucketHits(t, "references", buckets.Record(req,
				corpus.LocationsEnvelope("references", rres, rerr)))

			nres, nerr := b.Rename(t.Context(), languages.RenameRequest{
				URI: uri, Content: src, Line: uint32(line), Column: uint32(col),
				SnapshotRev: rev, BuildContext: bcID, NewName: sym + "_renamed",
			})
			nreq := req
			nreq.NewName = sym + "_renamed"
			logBucketHits(t, "rename", buckets.Record(nreq,
				corpus.RenameEnvelope(nres, nerr)))
		})
	}

	t.Logf("\n%sall six buckets must be zero (§S21)", &buckets)
	if buckets.Total() != 0 {
		t.Fatalf("§S21 violated: %d error-bucket hits across the go corpus", buckets.Total())
	}
}

// logBucketHits fails the subtest when a §S21 bucket fires on a response.
func logBucketHits(t *testing.T, feature string, hits []string) {
	t.Helper()
	if len(hits) > 0 {
		t.Errorf("%s: hit buckets %v", feature, hits)
	}
}

// findFuncSymbol locates the identifier after a top-level "func " keyword —
// a position guaranteed to carry real semantic information. The generic type
// parameter list ("Add[...]") is skipped so the returned name is the plain
// identifier the backend ranges cover.
func findFuncSymbol(src []byte) (line, col int, name string, ok bool) {
	for i, raw := range strings.Split(string(src), "\n") {
		l := strings.TrimSpace(raw)
		if strings.HasPrefix(l, "func ") {
			rest := strings.TrimPrefix(l, "func ")
			idx := strings.IndexAny(rest, "([")
			if idx > 0 {
				return i, 5 + idx - 1, rest[:idx], true // last char of the name
			}
		}
	}
	return 0, 0, "", false
}
