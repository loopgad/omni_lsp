package conformance

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// categoryWeights are the core-domain category weights (user-approved):
// correctness and security dominate; they sum to 1.0 across the seven
// categories so no renormalization is needed.
var coreWeights = map[string]float64{
	"Y0":   0.24,
	"Y3":   0.18,
	"Y1":   0.18,
	"Y2":   0.13,
	"Y4":   0.09,
	"Y5":   0.09,
	"PERF": 0.09,
}

// Status credit values.
const (
	creditFull     = 1.0
	creditPartial  = 0.5
	creditGateFast = 0.5 // existence-only credit for heavyweight gates in fast mode
)

// FastReport scores every check via AST existence only (no test execution).
func FastReport(root string) (*Report, error) {
	if root == "" {
		var err error
		if root, err = RequireModuleRoot(); err != nil {
			return nil, err
		}
	}
	sym, err := testSymbols(root)
	if err != nil {
		return nil, err
	}
	return score(sym, nil, "fast"), nil
}

// FullReport additionally batch-executes AUTO probes per package and runs
// gate commands; gates earn full credit only here.
func FullReport(root string, timeout string) (*Report, error) {
	// Recursion guard: the race-all gate re-runs this package's tests, and
	// without this marker TestFullReport_RunsAutoProbes would execute a
	// second full probe+gate pass inside the first (2x wall clock, and the
	// outer verify then times out). Engine tests set the same var themselves.
	if os.Getenv("OMNILSP_CONFORMANCE_NESTED") == "" {
		os.Setenv("OMNILSP_CONFORMANCE_NESTED", "1")
	}
	if root == "" {
		var err error
		if root, err = RequireModuleRoot(); err != nil {
			return nil, err
		}
	}
	sym, err := testSymbols(root)
	if err != nil {
		return nil, err
	}
	exec := map[string]error{}
	for _, c := range Registry.Checks {
		if c.Probe == nil {
			continue
		}
		for _, g := range c.Probe.Groups {
			key := g.Pkg + "\x00" + strings.Join(g.Tests, "\x00")
			if _, done := exec[key]; done {
				continue
			}
			exec[key] = runTestsBatch(g.Pkg, g.Tests, timeout)
		}
	}
	for _, c := range Registry.Checks {
		if c.Gate == nil {
			continue
		}
		key := "gate\x00" + c.ID
		if _, done := exec[key]; done {
			continue
		}
		exec[key] = runGateCmd(c.Gate.Cmd)
	}
	return score(sym, exec, "full"), nil
}

func runGateCmd(cmd string) error {
	// ponytail: shell-split by spaces is enough — gate commands are
	// repo-controlled literals, never user input.
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return fmt.Errorf("empty gate command")
	}
	return execCommand(fields[0], fields[1:]...)
}

func score(sym map[string][]string, exec map[string]error, mode string) *Report {
	rep := &Report{Schema: "omnilsp.conformance.v1", Mode: mode}
	domains := map[string][]*Check{}
	for i := range Registry.Checks {
		c := &Registry.Checks[i]
		domains[c.Domain] = append(domains[c.Domain], c)
		// Milestone-deferred items living in the core checklist are also
		// surfaced inside their own milestone domain (independent score).
		if c.Domain == "core" && c.Status == StatusDeferred && c.Milestone != "" {
			domains[c.Milestone] = append(domains[c.Milestone], c)
		}
	}

	names := make([]string, 0, len(domains))
	for n := range domains {
		names = append(names, n)
	}
	sort.Strings(names)
	// Core first, then deferred domains alphabetically.
	order := []string{"core"}
	for _, n := range names {
		if n != "core" {
			order = append(order, n)
		}
	}

	for _, dom := range order {
		checks := domains[dom]
		cats := map[string][]*Check{}
		var catOrder []string
		for _, c := range checks {
			if _, seen := cats[c.Category]; !seen {
				catOrder = append(catOrder, c.Category)
			}
			cats[c.Category] = append(cats[c.Category], c)
		}

		ds := DomainScore{Name: dom, Gated: dom == "core"}
		var weighted, weightSum float64

		ws := make([]string, len(catOrder))
		copy(ws, catOrder)
		sort.Strings(ws)
		if dom == "core" {
			sort.Slice(ws, func(i, j int) bool { // spec order Y0..Y5 then PERF
				return coreWeightRank(ws[i]) < coreWeightRank(ws[j])
			})
		}

		for _, cat := range ws {
			cs := CategoryScore{Category: cat}
			var sum, wsum float64
			deferredOut := 0 // milestone-deferred items excluded from the denominator
			for _, c := range cats[cat] {
				r := checkResult(c, sym, exec, mode)
				rep.Checks = append(rep.Checks, CheckResult{
					ID: c.ID, Status: c.Status, Result: r.Result, Mode: mode, Detail: r.Detail,
				})
				if dom == "core" && c.Status == StatusDeferred {
					// Anti-coupling rule (user decision): a milestone-deferred
					// item never drags the gated core score down — it is scored
					// inside its own milestone domain below.
					deferredOut++
					continue
				}
				sum += c.Weight * r.Result
				wsum += c.Weight
				if r.Result <= 0 {
					cs.Failed = append(cs.Failed, c.ID)
				}
			}
			cs.Items = len(cats[cat]) - deferredOut
			if wsum > 0 {
				cs.Score = sum / wsum
			} else {
				cs.Score = 1 // nothing eligible yet in this category
			}
			if cs.Items > 0 { // categories with zero eligible items add noise
				ds.Categories = append(ds.Categories, cs)
			}

			if dom == "core" && wsum > 0 {
				weighted += coreWeights[cat] * (sum / wsum)
				weightSum += coreWeights[cat]
			} else if wsum > 0 {
				weighted += (sum / wsum) * float64(cs.Items)
				weightSum += float64(cs.Items)
			}
		}
		if weightSum > 0 {
			ds.Score = weighted / weightSum
		}
		rep.Domains = append(rep.Domains, ds)
		if dom == "core" {
			rep.CoreScore = ds.Score * 100
		}
	}
	return rep
}

func coreWeightRank(cat string) int {
	switch cat {
	case "Y0":
		return 0
	case "Y1":
		return 1
	case "Y2":
		return 2
	case "Y3":
		return 3
	case "Y4":
		return 4
	case "Y5":
		return 5
	case "PERF":
		return 6
	default:
		return 7
	}
}

type res struct {
	Result float64
	Detail string
}

func checkResult(c *Check, sym map[string][]string, exec map[string]error, mode string) res {
	switch c.Status {
	case StatusPartial:
		return res{Result: creditPartial, Detail: c.Reason}
	case StatusDeferred:
		return res{Result: 0, Detail: c.Reason}
	case StatusGate:
		g := c.Gate
		if g == nil {
			return res{Result: 0, Detail: "gate without target"}
		}
		groups := []ProbeGroup{{Pkg: g.TargetPkg}}
		if g.TargetTest != "" {
			groups[0].Tests = []string{g.TargetTest}
		}
		if missing := hasAllGroups(sym, groups); mode == "fast" || g.TargetTest == "" {
			if len(missing) > 0 {
				return res{Result: 0, Detail: "missing symbols: " + strings.Join(missing, ",")}
			}
			if mode == "fast" {
				return res{Result: creditGateFast, Detail: "existence (fast); full runs: " + g.Cmd}
			}
		}
		if err := exec["gate\x00"+c.ID]; err != nil {
			return res{Result: 0, Detail: truncate(err.Error(), 200)}
		}
		return res{Result: creditFull, Detail: g.Cmd}
	case StatusAuto:
		p := c.Probe
		if p == nil || len(p.Groups) == 0 {
			return res{Result: 0, Detail: "auto check without probe"}
		}
		if missing := hasAllGroups(sym, p.Groups); len(missing) > 0 {
			return res{Result: 0, Detail: "missing symbols: " + strings.Join(missing, ",")}
		}
		if mode == "full" {
			for _, g := range p.Groups {
				key := g.Pkg + "\x00" + strings.Join(g.Tests, "\x00")
				if err := exec[key]; err != nil {
					return res{Result: 0, Detail: truncate(err.Error(), 200)}
				}
			}
		}
		return res{Result: creditFull}
	default:
		return res{Result: 0, Detail: "unknown status " + string(c.Status)}
	}
}

func testsOr(one string) []string {
	if one == "" {
		return nil
	}
	return []string{one}
}

func truncate(s string, n int) string {
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// RenderText produces the human-readable scorecard.
func RenderText(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "omnilsp conformance %s (%s mode)\n", r.Schema, r.Mode)
	for _, d := range r.Domains {
		tag := ""
		if d.Gated {
			tag = " [gated]"
		}
		fmt.Fprintf(&b, "\n%s%s score: %.1f%%\n", d.Name, tag, d.Score*100)
		for _, c := range d.Categories {
			bar := failedMark(c.Score)
			fmt.Fprintf(&b, "  %-5s %5.1f%%  (%d items)%s\n", c.Category, c.Score*100, c.Items, bar)
			for _, id := range c.Failed {
				fmt.Fprintf(&b, "        FAIL %s\n", id)
			}
		}
	}
	return b.String()
}

func failedMark(score float64) string {
	switch {
	case score >= 0.999:
		return ""
	case score > 0:
		return "  <- partial"
	default:
		return "  <- FAILED"
	}
}
