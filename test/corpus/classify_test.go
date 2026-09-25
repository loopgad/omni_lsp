package corpus

// §S21 accuracy verification (goal.md 行 4474-4487): every response produced
// while serving the qualified corpus is inspected and sorted into exactly the
// six release-blocking error buckets. The qualified-corpus goal is that all
// six counters stay zero:
//
//	Wrong Edit Rate            = 0
//	Stale Edit Applied         = 0
//	Wrong-file Location        = 0
//	Position Mapping Error     = 0
//	Protocol-invalid Response  = 0
//	Snapshot Mixing            = 0
//
// rust really runs when rust-analyzer is installed; python/typescript follow
// the existing corpus-runner pattern and skip on ErrToolchainMissing.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/pyright"
	"github.com/omnilsp/omni/internal/languages/rustanalyzer"
	"github.com/omnilsp/omni/internal/languages/typescript"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

// contextedBackend is what every concrete backend offers on top of the
// canonical interface: a caller-injected build-context ID (§E7).
type contextedBackend interface {
	languages.Backend
	BuildContextID() identity.BuildContextID
}

// --- negative fixtures: prove every bucket can fire ---

const fixtureSrc = "package m\n\nfunc greet() {}\n"

// fixtureBase builds a clean rename response over fixtureSrc: one edit whose
// range covers "greet" (line 2, UTF-16 cols 5..10) and renames it greet2.
func fixtureBase() (ClassifyRequest, LSPResponse) {
	req := ClassifyRequest{
		URI:     "file:///m.go",
		Content: []byte(fixtureSrc),
		Symbol:  "greet",
		NewName: "greet2",
		Rev:     7,
		Epoch:   3,
	}
	resp := LSPResponse{
		Feature:     "rename",
		Status:      identity.ResultExact,
		URIs:        []string{req.URI},
		Ranges:      []languages.Range{{StartLine: 2, StartCharacter: 5, EndLine: 2, EndCharacter: 10}},
		EditNewText: []string{"greet2"},
		Rev:         7,
		Epoch:       3,
	}
	return req, resp
}

// TestS21_ZeroErrorClassification runs hover/definition/references/rename
// across the qualified corpus (basic+broken per language), sorts every
// response into the six §S21 buckets, prints the table, and fails if any
// bucket is non-zero. A subtest first proves the classifier itself fires on
// six known-bad responses — otherwise all-zero would prove nothing.
func TestS21_ZeroErrorClassification(t *testing.T) {
	if testing.Short() {
		t.Skip("requires language toolchains")
	}

	t.Run("classifier_fires_on_known_bad_responses", func(t *testing.T) {
		// Astral content: col 6 lands inside the 🦀 surrogate pair, so the
		// engine must reject the mapping (and skip text extraction).
		astralContent := "package m\n\nfunc \U0001f980greet() {}\n"

		fixtures := []struct {
			name string
			mut  func(*ClassifyRequest, *LSPResponse)
			want []string
		}{
			{"clean_response_fires_nothing", func(*ClassifyRequest, *LSPResponse) {}, nil},
			{"transport_error", func(_ *ClassifyRequest, r *LSPResponse) { r.Err = "dial failed" },
				[]string{"protocolInvalidResponse"}},
			{"illegal_status_shape", func(_ *ClassifyRequest, r *LSPResponse) { r.Status = 99 },
				[]string{"protocolInvalidResponse"}},
			{"foreign_uri", func(_ *ClassifyRequest, r *LSPResponse) { r.URIs[0] = "file:///elsewhere.go" },
				[]string{"wrongFileLocation"}},
			{"edit_covers_wrong_text", func(_ *ClassifyRequest, r *LSPResponse) {
				r.Ranges[0].StartCharacter = 6
			}, []string{"wrongEdit"}},
			{"edit_renames_to_wrong_name", func(_ *ClassifyRequest, r *LSPResponse) {
				r.EditNewText[0] = "totally_other"
			}, []string{"wrongEdit"}},
			{"surrogate_midpoint_offset", func(rq *ClassifyRequest, r *LSPResponse) {
				rq.Content = []byte(astralContent)
				r.Ranges[0] = languages.Range{StartLine: 2, StartCharacter: 6, EndLine: 2, EndCharacter: 12}
				r.EditNewText = nil
			}, []string{"positionMappingError"}},
			{"stale_revision", func(_ *ClassifyRequest, r *LSPResponse) { r.Rev = 99 },
				[]string{"staleEdit", "snapshotMixing"}},
			{"epoch_mismatch", func(_ *ClassifyRequest, r *LSPResponse) { r.Epoch = 999 },
				[]string{"staleEdit", "snapshotMixing"}},
		}
		for _, fx := range fixtures {
			t.Run(fx.name, func(t *testing.T) {
				req, resp := fixtureBase()
				fx.mut(&req, &resp)
				got := ClassifyResponse(req, resp)
				sort.Strings(got)
				want := append([]string(nil), fx.want...)
				sort.Strings(want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("buckets = %v, want %v", got, want)
				}
			})
		}
	})

	var buckets ErrorBuckets
	skipped := map[string]string{}

	langs := []struct {
		name           string
		ext            string
		keyword        string
		start          func(t *testing.T, dir string) (contextedBackend, error)
		scaffoldBefore bool // rust needs Cargo.toml before New() (workspace fetch)
	}{
		{name: "rust", ext: ".rs", keyword: "fn ", scaffoldBefore: true,
			start: func(t *testing.T, dir string) (contextedBackend, error) {
				return rustanalyzer.New(dir)
			}},
		{name: "python", ext: ".py", keyword: "def ",
			start: func(t *testing.T, dir string) (contextedBackend, error) {
				return pyright.New(dir)
			}},
		{name: "typescript", ext: ".ts", keyword: "function ",
			start: func(t *testing.T, dir string) (contextedBackend, error) {
				return typescript.New(dir)
			}},
	}

	for _, lang := range langs {
		t.Run(lang.name, func(t *testing.T) {
			dir := t.TempDir()
			files, err := filepath.Glob(filepath.Join("testdata", lang.name, "*"+lang.ext))
			if err != nil || len(files) < 2 {
				t.Fatalf("corpus too thin for %s: %v (%d files)", lang.name, err, len(files))
			}

			if lang.scaffoldBefore {
				manifest := "[package]\nname = \"corpus\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"
				for _, f := range files {
					base := strings.TrimSuffix(filepath.Base(f), lang.ext)
					manifest += fmt.Sprintf("\n[[bin]]\nname = \"%s\"\npath = \"%s\"\n",
						base, filepath.Base(f))
				}
				if werr := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(manifest), 0o644); werr != nil {
					t.Fatal(werr)
				}
			}

			b, err := lang.start(t, dir)
			if errors.Is(err, rustanalyzer.ErrToolchainMissing) ||
				errors.Is(err, pyright.ErrToolchainMissing) ||
				errors.Is(err, typescript.ErrToolchainMissing) {
				skipped[lang.name] = "toolchain not installed"
				t.Skip("toolchain not installed")
			}
			if err != nil {
				t.Fatalf("backend start: %v", err)
			}
			defer b.Close()
			bc := b.BuildContextID()

			for _, f := range files {
				src, rerr := os.ReadFile(f)
				if rerr != nil {
					t.Fatalf("%s: %v", f, rerr)
				}
				t.Run(filepath.Base(f), func(t *testing.T) {
					line, col, sym, ok := findDeclSymbol(src, lang.keyword)
					if !ok {
						t.Skipf("no %q declaration in corpus file", lang.keyword)
					}
					furi := writeCorpusFile(t, dir, filepath.Base(f), src)
					runQueriesAndClassify(t, b, bc, furi, src, sym, line, col, &buckets)
				})
			}
		})
	}

	if n := len(skipped); n > 0 {
		names := make([]string, 0, n)
		for k := range skipped {
			names = append(names, k+" ("+skipped[k]+")")
		}
		sort.Strings(names)
		t.Logf("skipped languages: %s", strings.Join(names, ", "))
	}
	t.Logf("\n%sall six buckets must be zero (§S21)", &buckets)
	if buckets.Total() != 0 {
		t.Fatalf("§S21 violated: %d error-bucket hits across the qualified corpus", buckets.Total())
	}
}

// runQueriesAndClassify drives hover → definition → references → rename for
// one corpus file and records every response into buckets. The backend epoch
// observed on the first (hover) response becomes the baseline the remaining
// responses are compared against — within one live session it cannot change.
func runQueriesAndClassify(t *testing.T, b contextedBackend, bc identity.BuildContextID,
	furi string, src []byte, sym string, line, col int, buckets *ErrorBuckets) {
	t.Helper()
	const rev = uint64(1)
	ctx := t.Context()

	// Language servers answer null until their first analysis pass settles;
	// poll until hover grounds (broken files never ground — first try wins).
	broken := strings.Contains(sym, "half_written") ||
		strings.Contains(string(src), "unfinished")
	var hres identity.SemanticResult[*languages.HoverResult]
	var herr error
	deadline := time.Now().Add(45 * time.Second)
	for {
		hres, herr = b.Hover(ctx, languages.HoverRequest{
			URI: furi, Content: src, Line: uint32(line), Column: uint32(col),
			SnapshotRev: rev, BuildContext: bc,
		})
		if herr != nil {
			break
		}
		if broken || hres.Value != nil && hres.Value.Contents != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond) // ponytail: poll — no readiness signal exposed
	}

	req := ClassifyRequest{URI: furi, Content: src, Symbol: sym, Rev: rev}
	hresp := HoverEnvelope(hres, herr)
	req.Epoch = hresp.Epoch // baseline from first response of this session
	logHits(t, "hover", buckets.Record(req, hresp))

	dres, derr := b.Definition(ctx, languages.DefinitionRequest{
		URI: furi, Content: src, Line: uint32(line), Column: uint32(col),
		SnapshotRev: rev, BuildContext: bc,
	})
	logHits(t, "definition", buckets.Record(req,
		LocationsEnvelope("definition", dres, derr)))

	rres, rerr := b.References(ctx, languages.ReferencesRequest{
		URI: furi, Content: src, Line: uint32(line), Column: uint32(col),
		SnapshotRev: rev, BuildContext: bc, IncludeDecl: true,
	})
	logHits(t, "references", buckets.Record(req,
		LocationsEnvelope("references", rres, rerr)))

	nres, nerr := b.Rename(ctx, languages.RenameRequest{
		URI: furi, Content: src, Line: uint32(line), Column: uint32(col),
		SnapshotRev: rev, BuildContext: bc, NewName: sym + "_renamed",
	})
	nreq := req
	nreq.NewName = sym + "_renamed"
	logHits(t, "rename", buckets.Record(nreq, RenameEnvelope(nres, nerr)))
}

func logHits(t *testing.T, feature string, hits []string) {
	t.Helper()
	if len(hits) > 0 {
		t.Errorf("%s: hit buckets %v", feature, hits)
	}
}

// writeCorpusFile materializes a corpus source inside the backend workspace
// and returns its canonical file URI (§D2).
func writeCorpusFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	fpath := filepath.Join(dir, name)
	if err := os.WriteFile(fpath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return uri.FromPath(fpath).Canonical()
}

// findDeclSymbol locates the identifier after a top-level declaration keyword
// ("fn ", "def ", "function ") and returns its last-char position plus the
// identifier text itself (needed to verify ranges cover the right symbol).
func findDeclSymbol(src []byte, kw string) (line, col int, symbol string, ok bool) {
	for i, raw := range strings.Split(string(src), "\n") {
		l := strings.TrimSpace(raw)
		if strings.HasPrefix(l, kw) {
			rest := strings.TrimPrefix(l, kw)
			if idx := strings.IndexFunc(rest, func(r rune) bool { return r == '(' }); idx > 0 {
				return i, len(kw) + idx - 1, strings.TrimSpace(rest[:idx]), true
			}
		}
	}
	return 0, 0, "", false
}
