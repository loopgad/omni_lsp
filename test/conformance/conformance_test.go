// Package conformance_test locks the closed loop: the generated conformance
// document must match the registry, and the core score must never regress
// below the committed baseline without an explicit update.
package conformance_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/conformance"
)

const docPath = "../../docs/conformance.md"

func updateRequested() bool { return os.Getenv("OMNISP_UPDATE_CONFORMANCE") == "1" }

func writeDoc(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Clean(docPath), []byte(content), 0o644); err != nil {
		t.Fatalf("write conformance.md: %v", err)
	}
}

const baselinePath = "testdata/baseline.json"

// TestGenerateDocs keeps docs/conformance.md a pure projection of the
// registry (S6 explicit-update rule).
func TestGenerateDocs(t *testing.T) {
	want := conformance.GenerateDocs()
	got, err := os.ReadFile(docPath)
	if os.IsNotExist(err) || updateRequested() {
		writeDoc(t, want)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		if updateRequested() {
			writeDoc(t, want)
			return
		}
		t.Error("docs/conformance.md drifted from registry; regenerate with OMNISP_UPDATE_CONFORMANCE=1")
	}
}

// TestConformanceNoRegression is the closed-loop gate: fast-mode core score
// must meet or beat every recorded category floor. Improvements are adopted
// explicitly via OMNISP_UPDATE_CONFORMANCE=1 so any silent drop is loud.
func TestConformanceNoRegression(t *testing.T) {
	rep, err := conformance.FastReport("../..")
	if err != nil {
		t.Fatal(err)
	}

	var cur = conformance.Baseline{Categories: map[string]float64{}}
	for _, d := range rep.Domains {
		if !d.Gated {
			continue
		}
		for _, c := range d.Categories {
			cur.Categories[c.Category] = c.Score
		}
	}
	cur.CoreScore = rep.CoreScore

	prevData, err := os.ReadFile(baselinePath)
	switch {
	case os.IsNotExist(err) || updateRequested():
		if mkErr := os.MkdirAll(filepath.Dir(baselinePath), 0o755); mkErr != nil {
			t.Fatal(mkErr)
		}
		if wErr := conformance.WriteBaselineFile(baselinePath, cur.CoreScore, cur.Categories); wErr != nil {
			t.Fatal(wErr)
		}
		return
	case err != nil:
		t.Fatal(err)
	}
	prev, err := conformance.ParseBaseline(prevData)
	if err != nil {
		t.Fatalf("baseline.json corrupt: %v", err)
	}

	for cat, score := range prev.Categories {
		if cur.Categories[cat]+1e-9 < score {
			t.Errorf("%s regressed: %.3f < baseline %.3f (fix or OMNISP_UPDATE_CONFORMANCE=1)", cat, cur.Categories[cat], score)
		}
	}
	if cur.CoreScore+1e-9 < prev.CoreScore {
		t.Errorf("core score regressed: %.2f%% < baseline %.2f%%", cur.CoreScore, prev.CoreScore)
	}
}
