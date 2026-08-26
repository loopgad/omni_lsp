package pyright

// Differential corpus runner (§S7/S8): basic.py must yield an exact hover
// carrying contents plus §B4 evidence with a digest-backed build context;
// broken.py must survive the same queries — no panic, a legal four-state
// status, internal diagnostics when unknown. Each file runs in its OWN
// backend/workspace. Files live under testdata so the go tool never
// compiles them as part of this package.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

const corpusDir = "../../../test/corpus/testdata/python"

func TestCorpus_PythonFilesProduceGroundedSemantics(t *testing.T) {
	if testing.Short() {
		t.Skip("requires pyright toolchain")
	}
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("corpus dir: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("corpus too thin: %d files", len(entries))
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".py") {
			continue
		}
		path := filepath.Join(corpusDir, e.Name())
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("%s: %v", path, rerr)
		}
		t.Run(e.Name(), func(t *testing.T) {
			b := corpusBackend(t) // isolated workspace per corpus file
			defer b.Close()
			bcID := b.BuildContextID() // E7: caller injects the build context

			line, col, ok := findDeclSymbol(src, "def ")
			if !ok {
				t.Skip("no def declaration in corpus file")
			}
			furi := writeCorpusFile(t, b.workDir, e.Name(), string(src))

			// Servers answer null/empty until their first analysis pass
			// settles (pyright cold start, tsserver loading lib.d.ts).
			// Editors wait for publishDiagnostics; the nested bridge does
			// not surface it, so poll until hover grounds or give up.
			var hres identity.SemanticResult[*languages.HoverResult]
			var herr error
			broken := strings.Contains(e.Name(), "broken")
			deadline := time.Now().Add(45 * time.Second)
			for {
				hres, herr = b.Hover(t.Context(), languages.HoverRequest{
					URI: furi, Content: src, Line: uint32(line), Column: uint32(col),
					SnapshotRev: 1, BuildContext: bcID,
				})
				if herr != nil {
					t.Fatalf("hover error: %v", herr)
				}
				if broken || (hres.Value != nil && hres.Value.Contents != "") || time.Now().After(deadline) {
					break
				}
				time.Sleep(200 * time.Millisecond) // ponytail: poll — bridge exposes no readiness signal
			}
			assertHoverEnvelope(t, e.Name(), hres.Status, hres.Value, hres.InternalDiagnostics)
			if len(hres.Evidence) == 0 || hres.Evidence[0].BuildContext == "" {
				t.Error("§B4: hover evidence lacks build context")
			}

			dres, derr := b.Definition(t.Context(), languages.DefinitionRequest{
				URI: furi, Content: src, Line: uint32(line), Column: uint32(col),
				SnapshotRev: 1, BuildContext: bcID,
			})
			if derr != nil {
				t.Fatalf("definition error: %v", derr)
			}
			if dres.Status > identity.ResultUnavailable {
				t.Errorf("illegal definition status %d", dres.Status)
			}
		})
	}
}

// corpusBackend wires a real pyright-langserver against a fresh temp
// workspace. A missing binary skips the run instead of failing machines
// without the toolchain installed.
func corpusBackend(t *testing.T) *Backend {
	t.Helper()
	b, err := New(t.TempDir())
	if errors.Is(err, ErrToolchainMissing) {
		t.Skip("toolchain not installed")
	}
	if err != nil {
		t.Fatalf("backend start: %v", err)
	}
	return b
}

// writeCorpusFile materializes the corpus source inside the backend
// workspace and returns its canonical file URI (§D2).
func writeCorpusFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	fpath := filepath.Join(dir, name)
	if err := os.WriteFile(fpath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return uri.FromPath(fpath).Canonical()
}

// assertHoverEnvelope enforces the §S7/S8 contract: exact results carry
// contents; unknown carries diagnostics; any state is survivable for
// deliberately broken corpora but normal files must be exact.
func assertHoverEnvelope(t *testing.T, name string, st identity.ResultStatus, v *languages.HoverResult, diags []string) {
	t.Helper()
	broken := strings.Contains(name, "broken")
	switch st {
	case identity.ResultExact:
		if v == nil || v.Contents == "" {
			// null upstream results map to exact-with-no-value; that is a
			// legal envelope for deliberately broken corpora only.
			if !broken {
				t.Fatal("exact hover must carry non-empty contents")
			}
		}
	case identity.ResultUnknown:
		if len(diags) == 0 {
			t.Error("unknown status must record internal diagnostics")
		}
	case identity.ResultPartial, identity.ResultUnavailable:
		// legal degradation states (§S8)
	default:
		t.Fatalf("illegal result status %d", st)
	}
	if !broken && st != identity.ResultExact {
		t.Fatalf("normal corpus file must resolve exact, got %v (diag=%v)", st, diags)
	}
}

// findDeclSymbol locates the identifier after a top-level declaration
// keyword ("fn ", "def ", "function ") — a position guaranteed to carry
// real semantic information even in half-written sources.
func findDeclSymbol(src []byte, kw string) (line, col int, ok bool) {
	for i, raw := range strings.Split(string(src), "\n") {
		l := strings.TrimSpace(raw)
		if strings.HasPrefix(l, kw) {
			rest := strings.TrimPrefix(l, kw)
			if idx := strings.IndexFunc(rest, func(r rune) bool { return r == '(' }); idx > 0 {
				return i, len(kw) + idx - 1, true // last char of the name
			}
		}
	}
	return 0, 0, false
}
