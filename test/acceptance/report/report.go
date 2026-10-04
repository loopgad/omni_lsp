// Package report defines the test-only, machine-readable release evidence
// contract shared by acceptance, performance, client, and soak tests.
package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Status string

const (
	Passed      Status = "passed"
	Failed      Status = "failed"
	NotVerified Status = "not_verified"
	Running     Status = "running"
)

type ReleaseDecision string

const (
	ReleasePassed                         ReleaseDecision = "passed"
	ReleasePassedWithPerformanceException ReleaseDecision = "passed_with_performance_exception"
	ReleaseFailed                         ReleaseDecision = "failed"
	ReleaseNotVerified                    ReleaseDecision = "not_verified"
)

type ReleaseAssessment struct {
	PolicyID                 string          `json:"policy_id"`
	ExecutionStatus          string          `json:"execution_status"`
	Decision                 ReleaseDecision `json:"decision"`
	BoundedExceptionCheckIDs []string        `json:"bounded_exception_check_ids"`
	ExceptionCheckIDs        []string        `json:"exception_check_ids"`
}

type Candidate struct {
	Binary   string `json:"binary,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
	Revision string `json:"revision,omitempty"`
}

type Corpus struct {
	Name   string `json:"name,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

type Check struct {
	ID        string         `json:"id"`
	Status    Status         `json:"status"`
	Summary   string         `json:"summary,omitempty"`
	Threshold map[string]any `json:"threshold,omitempty"`
	Observed  map[string]any `json:"observed,omitempty"`
}

type Skip struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type SampleSet struct {
	ID        string    `json:"id"`
	Unit      string    `json:"unit"`
	Raw       []float64 `json:"raw"`
	P50       float64   `json:"p50,omitempty"`
	P95       float64   `json:"p95,omitempty"`
	P99       float64   `json:"p99,omitempty"`
	Warmup    int       `json:"warmup,omitempty"`
	SampledAt time.Time `json:"sampled_at,omitempty"`
}

// ABBAEvidence stores the raw upstream arms and request-correlated nested
// phase timings for a stable S18 candidate. Candidate outer durations are
// referenced from the operation's SampleSet.Raw to avoid duplicating them.
type ABBAEvidence struct {
	CheckID   string      `json:"check_id"`
	FixtureID string      `json:"fixture_id"`
	Operation string      `json:"operation"`
	Method    string      `json:"method"`
	Rounds    []ABBARound `json:"rounds"`
}

type ABBARound struct {
	Index int       `json:"index"`
	Order []string  `json:"order"`
	Legs  []ABBALeg `json:"legs"`
}

type ABBALeg struct {
	Position    int       `json:"position"`
	Role        string    `json:"role"`
	SampleStart int       `json:"sample_start"`
	SampleCount int       `json:"sample_count"`
	RequestIDs  []string  `json:"request_ids,omitempty"`
	RawNS       []float64 `json:"raw_ns,omitempty"`
	WriteRawNS  []float64 `json:"write_raw_ns,omitempty"`
	WaitRawNS   []float64 `json:"wait_raw_ns,omitempty"`
}

// Report is versioned independently of public product protocols. Any skipped
// or missing gate must appear as NotVerified; only explicit all-pass evidence
// can produce a Passed decision.
type Report struct {
	SchemaVersion     int                     `json:"schema_version"`
	RunID             string                  `json:"run_id"`
	StartedAt         time.Time               `json:"started_at"`
	FinishedAt        *time.Time              `json:"finished_at,omitempty"`
	Decision          Status                  `json:"decision"`
	Candidate         Candidate               `json:"candidate"`
	Environment       map[string]string       `json:"environment,omitempty"`
	Corpus            Corpus                  `json:"corpus,omitempty"`
	Limits            map[string]any          `json:"limits,omitempty"`
	Checks            []Check                 `json:"checks"`
	Samples           []SampleSet             `json:"samples,omitempty"`
	ABBAEvidence      map[string]ABBAEvidence `json:"abba_evidence,omitempty"`
	Errors            []string                `json:"errors,omitempty"`
	Skips             []Skip                  `json:"skips,omitempty"`
	ReleaseAssessment *ReleaseAssessment      `json:"release_assessment,omitempty"`
}

func New(runID string) Report {
	return Report{
		SchemaVersion: 1,
		RunID:         runID,
		StartedAt:     time.Now().UTC(),
		Decision:      Running,
		Environment:   make(map[string]string),
		Limits:        make(map[string]any),
	}
}

// Finalize derives the decision from explicit checks and errors. An empty
// report, any running check, or any not-verified check cannot pass.
func (r *Report) Finalize(now time.Time) {
	r.FinishedAt = timePtr(now.UTC())
	switch {
	case len(r.Errors) > 0:
		r.Decision = Failed
		return
	}
	if len(r.Checks) == 0 {
		r.Decision = NotVerified
		return
	}
	failed := false
	running := false
	unverified := len(r.Skips) > 0
	for _, check := range r.Checks {
		switch check.Status {
		case Failed:
			failed = true
		case Running:
			running = true
		case NotVerified:
			unverified = true
		case Passed:
		default:
			unverified = true
		}
	}
	if failed {
		r.Decision = Failed
		return
	}
	if running {
		r.Decision = Running
		return
	}
	if unverified {
		r.Decision = NotVerified
		return
	}
	r.Decision = Passed
}

func validateReleaseAssessment(r Report) error {
	a := r.ReleaseAssessment
	if a == nil {
		return nil
	}
	if a.PolicyID == "" || a.ExecutionStatus == "" {
		return errors.New("release assessment requires policy_id and execution_status")
	}
	if a.PolicyID != "s18-strict-v1" && a.PolicyID != s18EvidenceQualifiedPolicyID {
		return fmt.Errorf("unsupported release policy %q", a.PolicyID)
	}
	if a.PolicyID == "s18-strict-v1" && len(a.BoundedExceptionCheckIDs) != 0 {
		return errors.New("strict release assessment cannot list bounded performance exceptions")
	}
	checks := make(map[string]Check, len(r.Checks))
	for _, check := range r.Checks {
		if _, duplicate := checks[check.ID]; duplicate {
			return fmt.Errorf("duplicate performance report check %q", check.ID)
		}
		checks[check.ID] = check
	}
	bounded := make(map[string]struct{}, len(a.BoundedExceptionCheckIDs))
	if len(a.BoundedExceptionCheckIDs) > 4 {
		return errors.New("release assessment has too many bounded performance exception checks")
	}
	for _, id := range a.BoundedExceptionCheckIDs {
		if _, ok := stableS18LimitsForCheckID(r.RunID, id); !ok {
			return fmt.Errorf("bounded performance exception check %q is outside the stable S18 scope", id)
		}
		if _, duplicate := bounded[id]; duplicate {
			return fmt.Errorf("duplicate bounded performance exception check %q", id)
		}
		bounded[id] = struct{}{}
	}
	switch a.Decision {
	case ReleasePassed:
		if r.Decision != Passed || a.ExecutionStatus != "completed" || len(a.BoundedExceptionCheckIDs) != 0 || len(a.ExceptionCheckIDs) != 0 {
			return errors.New("release assessment cannot pass without completed strict evidence or when it has exceptions")
		}
		if a.PolicyID == s18EvidenceQualifiedPolicyID {
			if err := validateStableS18PassedEvidence(r, checks); err != nil {
				return err
			}
		}
	case ReleasePassedWithPerformanceException:
		if a.PolicyID != s18EvidenceQualifiedPolicyID || r.Decision != Failed || a.ExecutionStatus != "completed" || len(a.ExceptionCheckIDs) == 0 || len(a.ExceptionCheckIDs) > 4 || len(r.Errors) != 0 || len(r.Skips) != 0 {
			return errors.New("performance exception requires a completed execution, strict failure, and exception checks")
		}
		seen := make(map[string]struct{}, len(a.ExceptionCheckIDs))
		if len(a.ExceptionCheckIDs) != len(a.BoundedExceptionCheckIDs) {
			return errors.New("accepted performance exception IDs must match bounded exception IDs")
		}
		for _, id := range a.ExceptionCheckIDs {
			if _, ok := stableS18LimitsForCheckID(r.RunID, id); !ok {
				return fmt.Errorf("performance exception check %q is outside the stable S18 scope", id)
			}
			if _, ok := seen[id]; ok {
				return fmt.Errorf("duplicate performance exception check %q", id)
			}
			if _, ok := bounded[id]; !ok {
				return fmt.Errorf("accepted performance exception check %q is not listed as bounded", id)
			}
			seen[id] = struct{}{}
			check, ok := checks[id]
			if !ok || check.Status != Failed {
				return fmt.Errorf("performance exception check %q is not a strict failure", id)
			}
			if !isLatencyOnlyFailure(check) {
				return fmt.Errorf("performance exception check %q is not a latency-only failure", id)
			}
		}
		for _, check := range r.Checks {
			switch check.Status {
			case Passed:
			case Failed:
				if _, covered := seen[check.ID]; !covered {
					return fmt.Errorf("performance exception does not cover failed check %q", check.ID)
				}
			default:
				return fmt.Errorf("performance exception cannot cover %q check %q", check.Status, check.ID)
			}
		}
		if err := validateStableS18ExceptionEvidence(r, checks, seen); err != nil {
			return err
		}
	case ReleaseFailed:
		if r.Decision != Failed || a.ExecutionStatus != "completed" || len(a.ExceptionCheckIDs) != 0 {
			return errors.New("failed release assessment requires completed strict failure and no accepted exception IDs")
		}
	case ReleaseNotVerified:
		if a.ExecutionStatus == "completed" || (r.Decision != NotVerified && r.Decision != Running && r.Decision != Failed) || len(a.BoundedExceptionCheckIDs) != 0 || len(a.ExceptionCheckIDs) != 0 {
			return errors.New("not_verified release assessment requires incomplete strict evidence")
		}
	default:
		return fmt.Errorf("invalid release assessment decision %q", a.Decision)
	}
	return nil
}

func isLatencyOnlyFailure(check Check) bool {
	if check.Observed["failure_type"] != "latency" {
		return false
	}
	switch types := check.Observed["failure_types"].(type) {
	case []string:
		return len(types) == 1 && types[0] == "latency"
	case []any:
		return len(types) == 1 && types[0] == "latency"
	default:
		return false
	}
}

// Write validates and atomically writes a complete evidence document.
func Write(path string, r Report) error {
	if path == "" {
		return errors.New("acceptance report path is empty")
	}
	if r.SchemaVersion != 1 {
		return fmt.Errorf("unsupported acceptance report schema %d", r.SchemaVersion)
	}
	if r.RunID == "" {
		return errors.New("acceptance report run_id is empty")
	}
	if r.Decision == "" {
		return errors.New("acceptance report decision is empty")
	}
	if r.ReleaseAssessment != nil {
		if r.ReleaseAssessment.BoundedExceptionCheckIDs == nil {
			r.ReleaseAssessment.BoundedExceptionCheckIDs = []string{}
		}
		if r.ReleaseAssessment.ExceptionCheckIDs == nil {
			r.ReleaseAssessment.ExceptionCheckIDs = []string{}
		}
	}
	if err := validateReleaseAssessment(r); err != nil {
		return err
	}
	if r.Decision == Passed {
		if len(r.Checks) == 0 || len(r.Errors) > 0 || len(r.Skips) > 0 {
			return errors.New("acceptance report cannot pass with no checks, errors, or skips")
		}
		for _, check := range r.Checks {
			if check.ID == "" || check.Status != Passed {
				return fmt.Errorf("acceptance report cannot pass: check %q has status %q", check.ID, check.Status)
			}
		}
	}
	for _, check := range r.Checks {
		if check.ID == "" || (check.Status != Passed && check.Status != Failed && check.Status != NotVerified && check.Status != Running) {
			return fmt.Errorf("invalid acceptance check: id=%q status=%q", check.ID, check.Status)
		}
	}
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode acceptance report: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create acceptance report directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".acceptance-report-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary acceptance report: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write acceptance report: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync acceptance report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close acceptance report: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish acceptance report: %w", err)
	}
	return nil
}

func timePtr(t time.Time) *time.Time { return &t }
