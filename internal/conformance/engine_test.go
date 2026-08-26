package conformance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFastReport_ScoreBoundsAndShape runs the engine entry on this module
// (go test CWD is the package dir, inside go.mod) and pins report shape:
// schema string, core-score bounds, gated core domain, per-check [0,1].
func TestFastReport_ScoreBoundsAndShape(t *testing.T) {
	rep, err := FastReport("")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Schema != "omnilsp.conformance.v1" {
		t.Errorf("Schema = %q, want omnilsp.conformance.v1", rep.Schema)
	}
	if rep.CoreScore < 0 || rep.CoreScore > 100 {
		t.Errorf("CoreScore = %v outside [0,100]", rep.CoreScore)
	}
	if len(rep.Domains) == 0 {
		t.Fatal("no domains scored")
	}
	core := (*DomainScore)(nil)
	for i := range rep.Domains {
		if rep.Domains[i].Gated {
			if core != nil {
				t.Fatalf("multiple gated domains: %q and %q", core.Name, rep.Domains[i].Name)
			}
			core = &rep.Domains[i]
		}
	}
	if core == nil || core.Name != "core" {
		t.Fatalf("core domain missing or not gated; domains %+v", rep.Domains)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("no check results")
	}
	for _, c := range rep.Checks {
		if c.Result < 0 || c.Result > 1 {
			t.Errorf("check %s Result = %v outside [0,1]", c.ID, c.Result)
		}
	}
}

// TestRenderText_ContainsDomainsAndScores checks the human scorecard on a
// hand-built Report: a zero category must render FAILED, a half category
// must render partial.
func TestRenderText_ContainsDomainsAndScores(t *testing.T) {
	rep := &Report{
		Schema:    "omnilsp.conformance.v1",
		Mode:      "fast",
		CoreScore: 50,
		Domains: []DomainScore{{
			Name: "demo", Score: 0.5, Gated: true,
			Categories: []CategoryScore{
				{Category: "Y0", Score: 0, Items: 2, Failed: []string{"Y0-1"}},
				{Category: "Y1", Score: 0.5, Items: 1},
			},
		}},
	}
	out := RenderText(rep)
	for _, want := range []string{
		"demo", "50.0%", "%",
		"Y0", "<- FAILED", "FAIL Y0-1",
		"Y1", "<- partial",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderText output missing %q:\n%s", want, out)
		}
	}
}

// TestRequireModuleRoot_OutsideModuleFails: no go.mod above a temp dir.
func TestRequireModuleRoot_OutsideModuleFails(t *testing.T) {
	t.Chdir(t.TempDir())
	root, err := RequireModuleRoot()
	if err == nil {
		t.Fatalf("RequireModuleRoot unexpectedly returned %q outside module", root)
	}
}

// TestBaselineRoundTrip: write -> read -> identical numbers.
func TestBaselineRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	cats := map[string]float64{"Y0": 1, "Y2": 0.25, "PERF": 0.75}
	if err := WriteBaselineFile(path, 97.5, cats); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseBaseline(data)
	if err != nil {
		t.Fatal(err)
	}
	if b.CoreScore != 97.5 {
		t.Errorf("CoreScore round-trip mismatch: got %v", b.CoreScore)
	}
	if len(b.Categories) != len(cats) {
		t.Fatalf("Categories count mismatch: got %d, want %d", len(b.Categories), len(cats))
	}
	for k, v := range cats {
		if b.Categories[k] != v {
			t.Errorf("Categories[%s]: got %v, want %v", k, b.Categories[k], v)
		}
	}
}

// TestFullReport_RunsAutoProbes executes every AUTO probe batch and gate
// command on this module — slow (minutes) by design. All AUTO checks must
// earn full credit on a green tree.
func TestFullReport_RunsAutoProbes(t *testing.T) {
	// ponytail: executes the whole tree (~20min warm, longer under -race);
	// run explicitly via OMNILSP_CONFORMANCE_FULL=1 go test ./internal/conformance/
	if os.Getenv("OMNILSP_CONFORMANCE_FULL") == "" {
		t.Skip("set OMNILSP_CONFORMANCE_FULL=1 to execute full-probe verification")
	}
	if os.Getenv("OMNILSP_CONFORMANCE_NESTED") != "" {
		t.Skip("nested via race-all gate")
	}
	t.Setenv("OMNILSP_CONFORMANCE_NESTED", "1") // inherited by gates: stops recursion
	rep, err := FullReport("", "120s")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Checks {
		if c.Status != StatusAuto || c.Result == 1 {
			continue
		}
		t.Errorf("AUTO check %s Result=%v detail=%q", c.ID, c.Result, c.Detail)
	}
}
