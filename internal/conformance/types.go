// Package conformance implements the closed-loop verification scorecard:
// goal.md acceptance items become machine-checkable probes whose weighted
// results form the omnilsp.conformance.v1 report.
//
// Design contract (user decision, 2026-08):
//   - core domain (Y0-Y5 + PERF) is gated; deferred domains (x5/x7/x8/x9)
//     carry their own independent denominators and are reported, never
//     merged into the core score — no cross-domain coupling.
//   - fast mode is pure AST existence scanning (no test execution); full
//     mode batch-executes referenced tests and gate commands.
//
// Invariants:
//  1. Every AUTO check MUST reference existing Test symbols or it scores 0 —
//     a stale reference is a loud failure, not silent credit.
//  2. Deferred items never influence the core denominator.
//  3. The schema string is compatibility surface (R4): changes require a
//     version bump.
package conformance

// Status classifies how a check earns points.
type Status string

const (
	// StatusAuto: verified by running/finding the referenced Test symbols.
	StatusAuto Status = "AUTO"
	// StatusPartial: partially satisfied; fixed credit, reason required.
	StatusPartial Status = "PARTIAL"
	// StatusGate: heavyweight release command (race/fuzz/soak/golden).
	// fast mode pays existence credit only; full mode executes it.
	StatusGate Status = "GATE"
	// StatusDeferred: belongs to a future milestone domain.
	StatusDeferred Status = "DEFERRED"
)

// Probe names the tests that protect a check. A check may span multiple
// packages via Groups; every symbol must exist (fast) and pass (full).
type Probe struct {
	Groups []ProbeGroup `json:"groups"`
}

// ProbeGroup binds test symbols to their declaring package.
type ProbeGroup struct {
	Pkg   string   `json:"pkg"`
	Tests []string `json:"tests"`
}

// Gate describes a heavyweight verification command.
type Gate struct {
	Cmd        string `json:"cmd"`       // documented invocation
	TargetPkg  string `json:"targetPkg"` // existence-check anchor
	TargetTest string `json:"targetTest,omitempty"`
}

// Check is one scored acceptance item.
type Check struct {
	ID        string  `json:"id"`       // e.g. "Y0-1"
	Domain    string  `json:"domain"`   // "core", "x5", "x7", ...
	Category  string  `json:"category"` // "Y0".."Y5", "PERF"
	Clause    string  `json:"clause"`   // goal.md clause references
	Text      string  `json:"text"`     // checklist item wording
	Status    Status  `json:"status"`
	Weight    float64 `json:"weight,omitempty"` // within-category weight
	Reason    string  `json:"reason,omitempty"` // why PARTIAL / DEFERRED
	Milestone string  `json:"milestone,omitempty"`
	Probe     *Probe  `json:"probe,omitempty"`
	Gate      *Gate   `json:"gate,omitempty"`
}

// CategoryScore aggregates one category inside a domain.
type CategoryScore struct {
	Category string   `json:"category"`
	Score    float64  `json:"score"` // 0..1 weighted mean of eligible checks
	Items    int      `json:"items"`
	Failed   []string `json:"failed,omitempty"` // IDs of zero-scoring checks
}

// DomainScore is an independently-scored slice of the checklist.
type DomainScore struct {
	Name       string          `json:"name"`
	Score      float64         `json:"score"`
	Gated      bool            `json:"gated"` // true only for core
	Categories []CategoryScore `json:"categories"`
}

// CheckResult records one check's outcome in a run.
type CheckResult struct {
	ID     string  `json:"id"`
	Status Status  `json:"status"`
	Result float64 `json:"result"` // earned fraction 0..1
	Mode   string  `json:"mode"`   // fast | full
	Detail string  `json:"detail,omitempty"`
}

// Report is the omnilsp.conformance.v1 document.
type Report struct {
	Schema    string        `json:"schema"` // omnilsp.conformance.v1
	Mode      string        `json:"mode"`
	CoreScore float64       `json:"coreScore"` // 0..100
	Domains   []DomainScore `json:"domains"`
	Checks    []CheckResult `json:"checks"`
}
