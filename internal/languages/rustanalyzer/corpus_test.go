package rustanalyzer

// Differential corpus runner (§S7/S8): basic.rs must yield an exact hover
// carrying contents plus §B4 evidence with a digest-backed build context;
// broken.rs must survive the same queries — no panic, a legal four-state
// status, internal diagnostics when unknown. Each file runs in its OWN
// backend/workspace. Files live under testdata so the go tool never
// compiles them as part of this package.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/uri"
)

const corpusDir = "../../../test/corpus/testdata/rust"

func TestCorpus_RustFilesProduceGroundedSemantics(t *testing.T) {
	if testing.Short() {
		t.Skip("requires rust-analyzer toolchain")
	}
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		t.Fatalf("corpus dir: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("corpus too thin: %d files", len(entries))
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".rs") {
			continue
		}
		path := filepath.Join(corpusDir, e.Name())
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("%s: %v", path, rerr)
		}
		t.Run(e.Name(), func(t *testing.T) {
			// Scaffold BEFORE New(): rust-analyzer discovers the cargo
			// workspace during initialize, and this RA version refuses to
			// ground semantics for detached files (FetchWorkspaceError).
			// Making the corpus file itself the [[bin]] crate root keeps its
			// corpus filename while attaching it to a real crate.
			dir := t.TempDir()
			fpath := filepath.Join(dir, e.Name())
			if werr := os.WriteFile(fpath, src, 0o644); werr != nil {
				t.Fatal(werr)
			}
			manifest := "[package]\nname = \"corpus\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[[bin]]\nname = \"corpus\"\npath = \"" + e.Name() + "\"\n"
			if werr := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte(manifest), 0o644); werr != nil {
				t.Fatal(werr)
			}
			b := corpusBackend(t, dir) // isolated workspace per corpus file
			defer b.Close()
			bcID := b.BuildContextID() // E7: caller injects the build context
			furi := uri.FromPath(fpath).Canonical()

			line, col, ok := findDeclSymbol(src, "fn ")
			if !ok {
				t.Skip("no fn declaration in corpus file")
			}

			var hres identity.SemanticResult[*languages.HoverResult]
			var herr error
			broken := strings.Contains(e.Name(), "broken")
			hoverRequest := languages.HoverRequest{
				URI: furi, Content: src, Line: uint32(line), Column: uint32(col),
				SnapshotRev: 1, BuildContext: bcID,
			}
			if broken {
				// Keep deliberately broken input as a single resilience query.
				hres, herr = b.Hover(t.Context(), hoverRequest)
			} else {
				probeCtx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
				hres, herr = probePositiveHover(probeCtx, b, hoverRequest)
				cancel()
			}
			if herr != nil {
				t.Fatalf("hover semantic probe failed: %v", herr)
			}
			if !broken {
				if epoch, ready := b.SemanticReadiness(); epoch != identity.BackendEpoch(b.SupervisorEpoch()) || !ready {
					t.Fatalf("positive corpus hover did not establish readiness for current supervisor epoch: (%d, %t), supervisor=%d", epoch, ready, b.SupervisorEpoch())
				}
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

// probePositiveHover waits for real semantic content on a known declaration
// in the corpus fixture. Only empty/unknown results are retried. Transport,
// ContentModified, and other backend errors are returned immediately.
func probePositiveHover(ctx context.Context, backend *Backend, request languages.HoverRequest) (identity.SemanticResult[*languages.HoverResult], error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := backend.Hover(ctx, request)
		if err != nil {
			return result, err
		}
		if result.Status == identity.ResultExact && result.Value != nil && strings.TrimSpace(result.Value.Contents) != "" {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("timed out waiting for positive rust-analyzer hover: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// corpusBackend wires a real rust-analyzer against the scaffolded workspace.
// A missing binary skips the run instead of failing machines without the
// toolchain installed.
func corpusBackend(t *testing.T, dir string) *Backend {
	t.Helper()
	b, err := New(dir)
	if errors.Is(err, ErrToolchainMissing) {
		t.Skip("toolchain not installed")
	}
	if err != nil {
		t.Fatalf("backend start: %v", err)
	}
	return b
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
