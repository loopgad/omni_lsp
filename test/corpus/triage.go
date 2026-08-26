package corpus

// §S6 differential mismatch triage (goal.md 行 4142-4156):
//
//	detect mismatch
//	 -> record exact environment
//	 -> minimize fixture
//	 -> classify:
//	      OmniLSP bug
//	      upstream bug
//	      supported semantic difference
//	      build-context mismatch
//	 -> add regression
//	 -> fix/waive via explicit issue/ADR
//
// "No unexplained golden update to hide regressions."

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Classification is the four-bucket §S6 triage outcome.
type Classification string

const (
	ClassOmnilspBug   Classification = "omnilsp_bug"
	ClassUpstreamBug  Classification = "upstream_bug"
	ClassSemanticDiff Classification = "supported_semantic_difference"
	ClassBuildContext Classification = "build_context_mismatch"
)

// SchemaVersion is stamped on every report; bump when fields change.
const SchemaVersion = "omnilsp.triage.v1"

// EnvSnapshot records the exact environment a mismatch was observed in.
type EnvSnapshot struct {
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	GoVersion     string `json:"go_version"`
	ModuleVersion string `json:"module_version"`
	TimestampUTC  string `json:"timestamp_utc"`
}

// MismatchReport is the structured artifact of one triaged mismatch.
type MismatchReport struct {
	Schema         string         `json:"schema"`
	Backend        string         `json:"backend"`
	FixtureRef     string         `json:"fixture_ref"`
	Symptom        string         `json:"symptom"`
	Expected       string         `json:"expected"`
	Actual         string         `json:"actual"`
	Env            EnvSnapshot    `json:"env"`
	Classification Classification `json:"classification"`
	RegressionTest string         `json:"regression_test"`
	WaivedByADR    string         `json:"waived_by_adr"`
}

// NewMismatchReport fills Schema and the exact environment automatically.
func NewMismatchReport(backend, fixtureRef string) *MismatchReport {
	moduleVersion := "unknown"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		moduleVersion = bi.Main.Version
	}
	return &MismatchReport{
		Schema:     SchemaVersion,
		Backend:    backend,
		FixtureRef: fixtureRef,
		Env: EnvSnapshot{
			OS:            runtime.GOOS,
			Arch:          runtime.GOARCH,
			GoVersion:     runtime.Version(),
			ModuleVersion: moduleVersion,
			TimestampUTC:  time.Now().UTC().Format(time.RFC3339),
		},
	}
}

// Classify maps symptom keywords onto the four §S6 buckets. Pure heuristic:
// upstream tool names first, then build-context markers, then documented
// semantic differences, defaulting to an omnilsp bug.
func Classify(symptomKeywords []string) Classification {
	upstream := []string{"gopls", "pyright", "tsserver", "rust-analyzer"}
	buildCtx := []string{"version", "flags", "env", "build context"}
	semantic := []string{"by design", "documented"}
	for _, kw := range symptomKeywords {
		k := strings.ToLower(kw)
		for _, u := range upstream {
			if strings.Contains(k, u) {
				return ClassUpstreamBug
			}
		}
	}
	for _, kw := range symptomKeywords {
		k := strings.ToLower(kw)
		for _, b := range buildCtx {
			if strings.Contains(k, b) {
				return ClassBuildContext
			}
		}
	}
	for _, kw := range symptomKeywords {
		k := strings.ToLower(kw)
		for _, s := range semantic {
			if strings.Contains(k, s) {
				return ClassSemanticDiff
			}
		}
	}
	return ClassOmnilspBug
}

// WriteTo persists the report as triage-<backend>-<timestamp>.json under dir
// (colons replaced so the name is Windows-safe) and returns the path.
func (r *MismatchReport) WriteTo(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("triage: create dir: %w", err)
	}
	stamp := strings.ReplaceAll(r.Env.TimestampUTC, ":", "-")
	path := filepath.Join(dir, fmt.Sprintf("triage-%s-%s.json", r.Backend, stamp))
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", fmt.Errorf("triage: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("triage: write: %w", err)
	}
	return path, nil
}

// Validate enforces §S6 red line: every mismatch is either covered by a
// registered regression test or explicitly waived by an ADR/issue reference.
func (r *MismatchReport) Validate() error {
	if r.Schema != SchemaVersion {
		return fmt.Errorf("triage: schema %q, want %q", r.Schema, SchemaVersion)
	}
	switch r.Classification {
	case ClassOmnilspBug, ClassUpstreamBug, ClassSemanticDiff, ClassBuildContext:
	default:
		return fmt.Errorf("triage: unknown classification %q", r.Classification)
	}
	if r.RegressionTest == "" && r.WaivedByADR == "" {
		return errors.New("triage: unexplained mismatch: needs regression test or waiver reference")
	}
	return nil
}
