package conformance

import (
	"math"
	"slices"
	"testing"
)

// knownUnweighted lists core-domain categories deliberately excluded from the
// gate denominator. A new core category must be added here explicitly, which
// is the point: an unlisted category silently scores 0 for every one of its
// checks instead of failing.
//
// As of this test, 7 categories (59 of 78 core checks) carry weight and these
// 8 (19 checks, UX alone is 10) do not. That predates this file and is
// recorded, not endorsed: score() adds 0 for a missing key on both the
// weighted and unweighted branches, so every check below is invisible to
// coreScore while still being listed as satisfied in docs/conformance.md.
// Weights are user-approved, so changing them is a decision, not a cleanup —
// this list exists so the gap stays visible and cannot grow unnoticed.
var knownUnweighted = map[string]bool{
	"ACC":       true,
	"CONFIG":    true,
	"MEMO":      true,
	"PLUGIN":    true,
	"STAB":      true,
	"TESTING":   true,
	"UX":        true,
	"WORKSPACE": true,
}

var knownStatuses = []Status{StatusAuto, StatusPartial, StatusGate, StatusDeferred}

// TestRegistry_DataShape guards the registry's own invariants. These are data
// invariants, not behavioural ones, which is why they were unguarded: score()
// renormalizes by weightSum, so a misweighted registry still produces a
// plausible-looking number. Only a whole-string docs comparison notices, and
// that one is satisfied by regenerating the document — "edit the weight, then
// regenerate" stays green end to end.
func TestRegistry_DataShape(t *testing.T) {
	seenID := make(map[string]bool, len(Registry.Checks))
	coreCategories := map[string]bool{}

	for i := range Registry.Checks {
		c := &Registry.Checks[i]

		if c.ID == "" {
			t.Errorf("registry[%d]: empty ID", i)
		}
		if seenID[c.ID] {
			t.Errorf("duplicate ID %q: results would be ambiguous and docs double-count it", c.ID)
		}
		seenID[c.ID] = true

		if !slices.Contains(knownStatuses, c.Status) {
			t.Errorf("%s: status %q is not one of AUTO/PARTIAL/GATE/DEFERRED", c.ID, c.Status)
		}
		if c.Weight <= 0 {
			t.Errorf("%s: weight %v must be positive", c.ID, c.Weight)
		}
		if c.Text == "" {
			t.Errorf("%s: empty Text claims nothing and scores like a satisfied check", c.ID)
		}

		switch c.Status {
		case StatusAuto:
			if c.Probe == nil {
				t.Errorf("%s: AUTO means verified by running the referenced tests, so it needs a Probe", c.ID)
			}
			if c.Reason != "" {
				t.Errorf("%s: AUTO must not carry a Reason; that is what PARTIAL is for", c.ID)
			}
		case StatusPartial, StatusDeferred:
			if c.Reason == "" {
				t.Errorf("%s: %s requires a Reason explaining the gap", c.ID, c.Status)
			}
		}
		if c.Status == StatusGate && c.Gate == nil {
			t.Errorf("%s: GATE requires a Gate command", c.ID)
		}

		if c.Domain == "core" {
			coreCategories[c.Category] = true
		}
	}

	// Zero-weight core categories are the failure mode worth surfacing: score()
	// adds 0 for them on both the weighted and unweighted paths, so every check
	// in that category drops out of the gate denominator with no warning.
	for _, cat := range sortedKeys(coreCategories) {
		if _, ok := coreWeights[cat]; ok {
			continue
		}
		if knownUnweighted[cat] {
			continue
		}
		t.Errorf("core category %q has no coreWeights entry; its checks score 0. "+
			"Give it a weight, or list it in knownUnweighted with a comment saying why "+
			"it is excluded from the gate denominator", cat)
	}
	for cat := range coreWeights {
		if !coreCategories[cat] {
			t.Errorf("coreWeights has %q but no core check declares that category", cat)
		}
	}

	// The only weight invariant that actually holds: within-category weights are
	// relative and normalized by score(), but the category weights themselves
	// are documented as summing to 1.0 "so no renormalization is needed".
	var sum float64
	for cat, w := range coreWeights {
		if w <= 0 {
			t.Errorf("coreWeights[%s] = %v must be positive", cat, w)
		}
		sum += w
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("coreWeights sum = %v, want 1", sum)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
