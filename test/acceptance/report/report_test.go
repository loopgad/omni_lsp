package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFinalizeRequiresEveryGateToPass(t *testing.T) {
	tests := []struct {
		name   string
		checks []Check
		skips  []Skip
		want   Status
	}{
		{name: "all pass", checks: []Check{{ID: "S1", Status: Passed}}, want: Passed},
		{name: "skip blocks pass", checks: []Check{{ID: "S1", Status: Passed}}, skips: []Skip{{ID: "S2", Reason: "tool missing"}}, want: NotVerified},
		{name: "failure dominates skip", checks: []Check{{ID: "S1", Status: Failed}}, skips: []Skip{{ID: "S2", Reason: "tool missing"}}, want: Failed},
		{name: "running remains running", checks: []Check{{ID: "S1", Status: Passed}, {ID: "S2", Status: Running}}, want: Running},
		{name: "empty is unverified", want: NotVerified},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := New("run-1")
			report.Checks = test.checks
			report.Skips = test.skips
			report.Finalize(time.Now())
			if report.Decision != test.want {
				t.Fatalf("decision = %q, want %q", report.Decision, test.want)
			}
		})
	}
}

func TestWriteProducesReadableEvidence(t *testing.T) {
	report := New("run-2")
	report.Candidate.SHA256 = "abc123"
	report.Checks = []Check{{ID: "S18-hover", Status: Passed}}
	report.Samples = []SampleSet{{ID: "hover", Unit: "ms", Raw: []float64{1, 2, 3}}}
	report.Finalize(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))
	path := filepath.Join(t.TempDir(), "run", "report.json")
	if err := Write(path, report); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.RunID != report.RunID || got.Decision != Passed || len(got.Samples) != 1 || len(got.Samples[0].Raw) != 3 {
		t.Fatalf("read report does not preserve evidence: %+v", got)
	}
}

func TestWriteRejectsUnprovenPass(t *testing.T) {
	report := New("run-3")
	report.Decision = Passed
	if err := Write(filepath.Join(t.TempDir(), "report.json"), report); err == nil {
		t.Fatal("Write accepted a passed report without explicit passing checks")
	}
}

func TestReleaseAssessmentKeepsStrictFailureVisible(t *testing.T) {
	ids := stableS18CheckIDs("exception-run")
	r := stableEvidenceReport("exception-run", ids...)
	r.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "completed",
		Decision:                 ReleasePassedWithPerformanceException,
		BoundedExceptionCheckIDs: ids,
		ExceptionCheckIDs:        ids,
	}
	if r.Decision != Failed {
		t.Fatalf("strict decision = %q, want %q", r.Decision, Failed)
	}
	if err := validateReleaseAssessment(r); err != nil {
		t.Fatalf("valid bounded performance exception: %v", err)
	}
	path := filepath.Join(t.TempDir(), "performance.json")
	if err := Write(path, r); err != nil {
		t.Fatalf("write exception evidence: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Decision != Failed || got.ReleaseAssessment == nil || got.ReleaseAssessment.Decision != ReleasePassedWithPerformanceException || len(got.ReleaseAssessment.BoundedExceptionCheckIDs) != 4 {
		t.Fatalf("strict and release decisions were conflated: %+v", got)
	}
}

func TestReleaseAssessmentRecordsBoundedCandidatesWithoutGrantingPass(t *testing.T) {
	const runID = "bounded-candidate-run"
	const candidateID = runID + "/S18/c/syntax_update_after_edit"
	r := stableEvidenceReport(runID, candidateID)
	r.Checks = append(r.Checks, Check{ID: runID + "/S18/go/hot_hover", Status: Failed, Observed: map[string]any{"failure_type": "semantic", "failure_types": []string{"semantic"}}})
	r.Finalize(time.Now())
	r.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "completed", Decision: ReleaseFailed,
		BoundedExceptionCheckIDs: []string{candidateID},
	}
	if err := validateReleaseAssessment(r); err != nil {
		t.Fatalf("valid bounded candidate in failed assessment: %v", err)
	}
	if r.ReleaseAssessment.Decision != ReleaseFailed || len(r.ReleaseAssessment.ExceptionCheckIDs) != 0 {
		t.Fatalf("bounded candidate upgraded the overall failure: %+v", r.ReleaseAssessment)
	}

	path := filepath.Join(t.TempDir(), "failed-with-bounded-candidate.json")
	if err := Write(path, r); err != nil {
		t.Fatalf("write bounded candidate assessment: %v", err)
	}
	persistedBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted Report
	if err := json.Unmarshal(persistedBytes, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.ReleaseAssessment == nil || persisted.ReleaseAssessment.BoundedExceptionCheckIDs == nil || persisted.ReleaseAssessment.ExceptionCheckIDs == nil || len(persisted.ReleaseAssessment.ExceptionCheckIDs) != 0 {
		t.Fatalf("serialized assessment must distinguish bounded candidates from an explicit empty accepted-ID list: %+v", persisted.ReleaseAssessment)
	}

	for _, test := range []struct {
		name string
		ids  []string
	}{
		{name: "out of scope", ids: []string{runID + "/S18/c/hot_hover"}},
		{name: "another run", ids: []string{"other-run/S18/c/syntax_update_after_edit"}},
		{name: "duplicate", ids: []string{candidateID, candidateID}},
		{name: "too many", ids: append(stableS18CheckIDs(runID), candidateID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			forged := r
			assessment := *r.ReleaseAssessment
			assessment.BoundedExceptionCheckIDs = test.ids
			forged.ReleaseAssessment = &assessment
			if err := validateReleaseAssessment(forged); err == nil {
				t.Fatal("accepted invalid bounded exception candidate IDs")
			}
		})
	}
	acceptedDespiteBlocker := r
	assessment := *r.ReleaseAssessment
	assessment.ExceptionCheckIDs = []string{candidateID}
	acceptedDespiteBlocker.ReleaseAssessment = &assessment
	if err := validateReleaseAssessment(acceptedDespiteBlocker); err == nil {
		t.Fatal("failed assessment reported a bounded candidate as an accepted release exception")
	}
	strictPolicy := r
	assessment = *r.ReleaseAssessment
	assessment.PolicyID = "s18-strict-v1"
	strictPolicy.ReleaseAssessment = &assessment
	if err := validateReleaseAssessment(strictPolicy); err == nil {
		t.Fatal("strict policy reported a bounded performance exception")
	}
}

func TestReleaseAssessmentRejectsForgedOrIncompleteException(t *testing.T) {
	const candidateID = "exception-run/S18/c/syntax_update_after_edit"
	base := stableEvidenceReport("exception-run", candidateID)
	tests := []struct {
		name       string
		assessment ReleaseAssessment
	}{
		{name: "execution incomplete", assessment: ReleaseAssessment{PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "incomplete", Decision: ReleasePassedWithPerformanceException, ExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"}}},
		{name: "no recorded exception", assessment: ReleaseAssessment{PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "completed", Decision: ReleasePassedWithPerformanceException}},
		{name: "unfailed check", assessment: ReleaseAssessment{PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "completed", Decision: ReleasePassedWithPerformanceException, ExceptionCheckIDs: []string{"exception-run/S18/cpp/syntax_update_after_edit"}}},
		{name: "duplicate check", assessment: ReleaseAssessment{PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "completed", Decision: ReleasePassedWithPerformanceException, ExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit", "exception-run/S18/c/syntax_update_after_edit"}}},
		{name: "exception from another run", assessment: ReleaseAssessment{PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "completed", Decision: ReleasePassedWithPerformanceException, ExceptionCheckIDs: []string{"other-run/S18/c/syntax_update_after_edit"}}},
	}
	withError := base
	withError.Errors = []string{"p50 strict latency threshold exceeded"}
	withError.ReleaseAssessment = &ReleaseAssessment{PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "completed", Decision: ReleasePassedWithPerformanceException, BoundedExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"}, ExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"}}
	if err := validateReleaseAssessment(withError); err == nil {
		t.Fatal("accepted a report with errors despite the latency-only exception")
	}
	withSkip := base
	withSkip.Skips = []Skip{{ID: "cpp", Reason: "upstream tool missing"}}
	withSkip.ReleaseAssessment = &ReleaseAssessment{PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "completed", Decision: ReleasePassedWithPerformanceException, BoundedExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"}, ExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"}}
	if err := validateReleaseAssessment(withSkip); err == nil {
		t.Fatal("accepted a performance exception with a skipped check")
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := base
			r.ReleaseAssessment = &test.assessment
			if err := validateReleaseAssessment(r); err == nil {
				t.Fatal("accepted invalid performance exception")
			}
		})
	}

	wrongFailureType := base
	wrongFailureType.Checks = append([]Check(nil), base.Checks...)
	wrongFailureCheck := wrongFailureType.Checks[0]
	wrongFailureCheck.Observed = cloneReportMap(wrongFailureCheck.Observed)
	wrongFailureCheck.Observed["failure_type"] = "semantic"
	wrongFailureCheck.Observed["failure_types"] = []string{"semantic"}
	wrongFailureType.Checks[0] = wrongFailureCheck
	wrongFailureType.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "completed",
		Decision: ReleasePassedWithPerformanceException, BoundedExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"}, ExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"},
	}
	if err := validateReleaseAssessment(wrongFailureType); err == nil {
		t.Fatal("accepted a semantic failure as a performance exception")
	}

	uncoveredFailure := base
	uncoveredFailure.Checks = append(uncoveredFailure.Checks, Check{
		ID: "exception-run/S18/go/hot_hover", Status: Failed,
		Observed: map[string]any{"failure_type": "semantic", "failure_types": []string{"semantic"}},
	})
	uncoveredFailure.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "completed",
		Decision: ReleasePassedWithPerformanceException, BoundedExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"}, ExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"},
	}
	if err := validateReleaseAssessment(uncoveredFailure); err == nil {
		t.Fatal("accepted an uncovered semantic failure alongside a latency exception")
	}

	unverifiedCheck := base
	unverifiedCheck.Checks = append(unverifiedCheck.Checks, Check{ID: "exception-run/S19/progress", Status: NotVerified})
	unverifiedCheck.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "completed",
		Decision: ReleasePassedWithPerformanceException, BoundedExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"}, ExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"},
	}
	if err := validateReleaseAssessment(unverifiedCheck); err == nil {
		t.Fatal("accepted a performance exception with an unverified check")
	}

	duplicateCheck := base
	duplicateCheck.Checks = append(duplicateCheck.Checks, base.Checks[0])
	duplicateCheck.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: "s18-evidence-qualified-v2", ExecutionStatus: "completed",
		Decision: ReleasePassedWithPerformanceException, BoundedExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"}, ExceptionCheckIDs: []string{"exception-run/S18/c/syntax_update_after_edit"},
	}
	if err := validateReleaseAssessment(duplicateCheck); err == nil {
		t.Fatal("accepted duplicate report check IDs")
	}

	passed := New("passed-run")
	passed.Checks = []Check{{ID: "passed-run/S18/go/hot_hover", Status: Passed}}
	passed.Finalize(time.Now())
	passed.ReleaseAssessment = &ReleaseAssessment{PolicyID: "s18-strict-v1", ExecutionStatus: "incomplete", Decision: ReleasePassed}
	if err := validateReleaseAssessment(passed); err == nil {
		t.Fatal("accepted an incomplete execution as a passed release assessment")
	}
}

func latencyFailureObservation() map[string]any {
	return map[string]any{"failure_type": "latency", "failure_types": []string{"latency"}}
}

func cloneReportMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}
