package report

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func stableS18CheckIDs(runID string) []string {
	return []string{
		runID + "/S18/c/completion_first_usable",
		runID + "/S18/cpp/completion_first_usable",
		runID + "/S18/c/syntax_update_after_edit",
		runID + "/S18/cpp/syntax_update_after_edit",
	}
}

func stableEvidenceReport(runID string, ids ...string) Report {
	r := New(runID)
	r.ABBAEvidence = make(map[string]ABBAEvidence, len(ids))
	for _, id := range ids {
		limits, ok := stableS18LimitsForCheckID(runID, id)
		if !ok {
			continue
		}
		candidateNS := (limits.strictMS[0] + 5) * 1e6
		upstreamNS := candidateNS - 2e6
		writeNS := 1e6
		waitNS := candidateNS - 4e6
		sample := SampleSet{ID: id, Unit: "ns/op", Warmup: 32}
		evidence := ABBAEvidence{
			CheckID: id, FixtureID: limits.fixtureID, Operation: limits.operation, Method: limits.method,
			Rounds: make([]ABBARound, 0, 3),
		}
		for roundIndex := 0; roundIndex < 3; roundIndex++ {
			round := ABBARound{
				Index: roundIndex,
				Order: []string{"candidate", "upstream", "upstream", "candidate"},
				Legs:  make([]ABBALeg, 0, 4),
			}
			for position, role := range round.Order {
				leg := ABBALeg{Position: position, Role: role}
				if role == "candidate" {
					leg.SampleStart = len(sample.Raw)
					leg.SampleCount = 1000
					for index := 0; index < leg.SampleCount; index++ {
						sample.Raw = append(sample.Raw, candidateNS)
						leg.RequestIDs = append(leg.RequestIDs, fmt.Sprintf("%s-r%d-l%d-q%d", id, roundIndex, position, index))
						leg.WriteRawNS = append(leg.WriteRawNS, writeNS)
						leg.WaitRawNS = append(leg.WaitRawNS, waitNS)
					}
				} else {
					leg.RawNS = make([]float64, 1000)
					for index := range leg.RawNS {
						leg.RawNS[index] = upstreamNS
					}
				}
				round.Legs = append(round.Legs, leg)
			}
			evidence.Rounds = append(evidence.Rounds, round)
		}
		percentiles := s18Percentiles(sample.Raw)
		sample.P50, sample.P95, sample.P99 = percentiles.p50, percentiles.p95, percentiles.p99
		check := Check{
			ID: id, Status: Failed, Summary: "strict latency threshold exceeded",
			Threshold: map[string]any{
				"p50_ms": limits.strictMS[0], "p95_ms": limits.strictMS[1], "p99_ms": limits.strictMS[2],
			},
			Observed: map[string]any{
				"sample_count": len(sample.Raw), "p50_ns": percentiles.p50, "p95_ns": percentiles.p95, "p99_ns": percentiles.p99,
				"failure_type": "latency", "failure_types": []string{"latency"},
				"validation": map[string]any{
					"candidate_status": "passed", "candidate_normalization_status": "passed",
					"upstream_status": "passed", "upstream_normalization_status": "passed",
				},
				"upstream_differential": map[string]any{
					"status": "passed", "matched": true, "target_candidate_sample_status": "passed",
				},
				"edit_update": map[string]any{"status": s18EditUpdateStatus(limits.operation)},
			},
		}
		r.Checks = append(r.Checks, check)
		r.Samples = append(r.Samples, sample)
		r.ABBAEvidence[id] = evidence
	}
	r.Finalize(time.Now())
	return r
}

func s18EditUpdateStatus(operation string) string {
	if operation == "completion_first_usable" {
		return "not_applicable"
	}
	return "passed"
}

func strictPassingStableEvidenceReport(runID string) Report {
	ids := stableS18CheckIDs(runID)
	r := stableEvidenceReport(runID, ids...)
	for _, id := range ids {
		limits, _ := stableS18LimitsForCheckID(runID, id)
		candidateNS := (limits.strictMS[0] - 1) * 1e6
		upstreamNS := candidateNS - 2e6
		sampleIndex := -1
		for index := range r.Samples {
			if r.Samples[index].ID == id {
				sampleIndex = index
				break
			}
		}
		if sampleIndex < 0 {
			continue
		}
		sample := r.Samples[sampleIndex]
		for index := range sample.Raw {
			sample.Raw[index] = candidateNS
		}
		percentiles := s18Percentiles(sample.Raw)
		sample.P50, sample.P95, sample.P99 = percentiles.p50, percentiles.p95, percentiles.p99
		r.Samples[sampleIndex] = sample
		for checkIndex := range r.Checks {
			if r.Checks[checkIndex].ID != id {
				continue
			}
			check := r.Checks[checkIndex]
			check.Status = Passed
			check.Observed = cloneReportMap(check.Observed)
			delete(check.Observed, "failure_type")
			delete(check.Observed, "failure_types")
			check.Observed["p50_ns"] = percentiles.p50
			check.Observed["p95_ns"] = percentiles.p95
			check.Observed["p99_ns"] = percentiles.p99
			check.Summary = "strict thresholds met"
			r.Checks[checkIndex] = check
		}
		evidence := r.ABBAEvidence[id]
		for roundIndex := range evidence.Rounds {
			for legIndex := range evidence.Rounds[roundIndex].Legs {
				leg := &evidence.Rounds[roundIndex].Legs[legIndex]
				if leg.Role == "candidate" {
					for sampleOffset := range leg.WriteRawNS {
						leg.WriteRawNS[sampleOffset] = 1e6
						leg.WaitRawNS[sampleOffset] = candidateNS - 4e6
					}
				} else {
					for sampleOffset := range leg.RawNS {
						leg.RawNS[sampleOffset] = upstreamNS
					}
				}
			}
		}
		r.ABBAEvidence[id] = evidence
	}
	r.Finalize(time.Now())
	r.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "completed", Decision: ReleasePassed,
	}
	return r
}

func TestStableV2ABBAEvidenceQualifiesAllFourAllowlistedChecks(t *testing.T) {
	const runID = "stable-v2-run"
	ids := stableS18CheckIDs(runID)
	r := stableEvidenceReport(runID, ids...)
	r.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "completed",
		Decision:                 ReleasePassedWithPerformanceException,
		BoundedExceptionCheckIDs: ids, ExceptionCheckIDs: ids,
	}
	if r.Decision != Failed {
		t.Fatalf("strict report decision = %q, want failed", r.Decision)
	}
	if err := validateReleaseAssessment(r); err != nil {
		t.Fatalf("validate all four qualified S18 exceptions: %v", err)
	}

	for _, id := range ids {
		limits, _ := stableS18LimitsForCheckID(runID, id)
		check := findCheck(t, r, id)
		metrics, err := validateS18ABBAEvidence(id, check, findSample(t, r, id), r.ABBAEvidence[id], limits)
		if err != nil || !metrics.Eligible {
			t.Fatalf("%s eligible=%t err=%v", id, metrics.Eligible, err)
		}
	}
}

func TestStableV2ABBAEvidenceRejectsForgedOrIncompleteProof(t *testing.T) {
	const runID = "stable-v2-negative"
	const checkID = runID + "/S18/c/syntax_update_after_edit"
	tests := []struct {
		name   string
		mutate func(*Report)
	}{
		{name: "old policy id", mutate: func(r *Report) { r.ReleaseAssessment.PolicyID = "s18-c-cpp-stable-v1" }},
		{name: "wrong evidence operation", mutate: func(r *Report) {
			evidence := r.ABBAEvidence[checkID]
			evidence.Operation = "hot_hover"
			r.ABBAEvidence[checkID] = evidence
		}},
		{name: "incomplete rounds", mutate: func(r *Report) {
			evidence := r.ABBAEvidence[checkID]
			evidence.Rounds = evidence.Rounds[:2]
			r.ABBAEvidence[checkID] = evidence
		}},
		{name: "wrong ABBA order", mutate: func(r *Report) {
			evidence := r.ABBAEvidence[checkID]
			evidence.Rounds[0].Order[1] = "candidate"
			r.ABBAEvidence[checkID] = evidence
		}},
		{name: "missing leg sample", mutate: func(r *Report) {
			evidence := r.ABBAEvidence[checkID]
			evidence.Rounds[0].Legs[1].RawNS = evidence.Rounds[0].Legs[1].RawNS[:999]
			r.ABBAEvidence[checkID] = evidence
		}},
		{name: "candidate range gap", mutate: func(r *Report) {
			evidence := r.ABBAEvidence[checkID]
			evidence.Rounds[0].Legs[3].SampleStart++
			r.ABBAEvidence[checkID] = evidence
		}},
		{name: "empty request id", mutate: func(r *Report) {
			evidence := r.ABBAEvidence[checkID]
			evidence.Rounds[0].Legs[0].RequestIDs[0] = ""
			r.ABBAEvidence[checkID] = evidence
		}},
		{name: "candidate percentile claim", mutate: func(r *Report) { r.Samples[0].P99++ }},
		{name: "check percentile claim", mutate: func(r *Report) { r.Checks[0].Observed["p95_ns"] = 1.0 }},
		{name: "semantic failure", mutate: func(r *Report) {
			validation := r.Checks[0].Observed["validation"].(map[string]any)
			validation["candidate_status"] = "failed"
		}},
		{name: "overhead exceeds cap", mutate: func(r *Report) {
			evidence := r.ABBAEvidence[checkID]
			for roundIndex := range evidence.Rounds {
				for legIndex := range evidence.Rounds[roundIndex].Legs {
					leg := &evidence.Rounds[roundIndex].Legs[legIndex]
					if leg.Role != "candidate" {
						continue
					}
					for index := range leg.WaitRawNS {
						leg.WaitRawNS[index] -= 3e6
					}
				}
			}
			r.ABBAEvidence[checkID] = evidence
		}},
		{name: "negative overhead", mutate: func(r *Report) {
			evidence := r.ABBAEvidence[checkID]
			for index := range evidence.Rounds[0].Legs[0].WaitRawNS {
				evidence.Rounds[0].Legs[0].WaitRawNS[index] += 4e6
			}
			r.ABBAEvidence[checkID] = evidence
		}},
		{name: "missing ABBA evidence", mutate: func(r *Report) { delete(r.ABBAEvidence, checkID) }},
		{name: "out-of-scope language", mutate: func(r *Report) {
			id := runID + "/S18/go/syntax_update_after_edit"
			r.ReleaseAssessment.BoundedExceptionCheckIDs = []string{id}
			r.ReleaseAssessment.ExceptionCheckIDs = []string{id}
		}},
		{name: "too many exception IDs", mutate: func(r *Report) {
			r.ReleaseAssessment.BoundedExceptionCheckIDs = append(stableS18CheckIDs(runID), checkID)
			r.ReleaseAssessment.ExceptionCheckIDs = append(stableS18CheckIDs(runID), checkID)
		}},
		{name: "incomplete execution", mutate: func(r *Report) { r.ReleaseAssessment.ExecutionStatus = "incomplete" }},
		{name: "semantic failure in another gate", mutate: func(r *Report) {
			r.Checks = append(r.Checks, Check{ID: runID + "/S18/go/hot_hover", Status: Failed, Observed: map[string]any{"failure_type": "semantic", "failure_types": []string{"semantic"}}})
		}},
		{name: "unverified gate", mutate: func(r *Report) { r.Checks = append(r.Checks, Check{ID: runID + "/S19/progress", Status: NotVerified}) }},
		{name: "report error", mutate: func(r *Report) { r.Errors = []string{"process exit status 1"} }},
		{name: "skipped gate", mutate: func(r *Report) { r.Skips = []Skip{{ID: "tool", Reason: "not installed"}} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := stableEvidenceReport(runID, checkID)
			r.ReleaseAssessment = &ReleaseAssessment{
				PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "completed",
				Decision:                 ReleasePassedWithPerformanceException,
				BoundedExceptionCheckIDs: []string{checkID}, ExceptionCheckIDs: []string{checkID},
			}
			test.mutate(&r)
			if err := validateReleaseAssessment(r); err == nil {
				t.Fatal("accepted forged or incomplete S18 exception evidence")
			}
		})
	}
}

func TestStableV2BoundedCandidateDoesNotUpgradeUncoveredFailure(t *testing.T) {
	const runID = "stable-v2-hard-gate"
	const checkID = runID + "/S18/c/completion_first_usable"
	r := stableEvidenceReport(runID, checkID)
	r.Checks = append(r.Checks, Check{
		ID: runID + "/S18/rust/hot_hover", Status: Failed,
		Observed: map[string]any{"failure_type": "semantic", "failure_types": []string{"semantic"}},
	})
	r.Finalize(time.Now())
	r.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "completed", Decision: ReleaseFailed,
		BoundedExceptionCheckIDs: []string{checkID},
	}
	if err := validateReleaseAssessment(r); err != nil {
		t.Fatalf("bounded candidate may remain diagnostic in failed assessment: %v", err)
	}
	r.ReleaseAssessment.Decision = ReleasePassedWithPerformanceException
	r.ReleaseAssessment.ExceptionCheckIDs = []string{checkID}
	if err := validateReleaseAssessment(r); err == nil {
		t.Fatal("accepted a latency exception alongside an uncovered semantic failure")
	}
}

func TestStableV2PassedRequiresAllFourCompleteABBARecords(t *testing.T) {
	const runID = "stable-v2-pass"
	r := strictPassingStableEvidenceReport(runID)
	if r.Decision != Passed {
		t.Fatalf("strict report decision = %q, want passed", r.Decision)
	}
	if err := validateReleaseAssessment(r); err != nil {
		t.Fatalf("complete strict evidence should satisfy stable v2: %v", err)
	}
	delete(r.ABBAEvidence, stableS18CheckIDs(runID)[0])
	if err := validateReleaseAssessment(r); err == nil {
		t.Fatal("stable v2 passed without required ABBA evidence")
	}
}

func TestStableV2ExceptionRequiresEveryScopedEvidenceSetAndSemanticGate(t *testing.T) {
	const runID = "stable-v2-all-scope"
	ids := stableS18CheckIDs(runID)
	for _, test := range []struct {
		name   string
		mutate func(*Report)
	}{
		{name: "missing non-exception evidence", mutate: func(r *Report) {
			delete(r.ABBAEvidence, ids[1])
		}},
		{name: "semantic failure on another scoped check", mutate: func(r *Report) {
			for index := range r.Checks {
				if r.Checks[index].ID == ids[1] {
					observed := cloneReportMap(r.Checks[index].Observed)
					validation := cloneReportMap(observed["validation"].(map[string]any))
					validation["upstream_status"] = "failed"
					observed["validation"] = validation
					r.Checks[index].Observed = observed
					return
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := stableEvidenceReport(runID, ids...)
			r.ReleaseAssessment = &ReleaseAssessment{
				PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "completed",
				Decision: ReleasePassedWithPerformanceException, BoundedExceptionCheckIDs: ids, ExceptionCheckIDs: ids,
			}
			test.mutate(&r)
			if err := validateReleaseAssessment(r); err == nil {
				t.Fatal("accepted exception assessment with incomplete scoped proof")
			}
		})
	}
}

func TestStableV2ExceptionRejectsStrictFailureForgedAsPassed(t *testing.T) {
	const runID = "stable-v2-forged-pass"
	ids := stableS18CheckIDs(runID)
	r := stableEvidenceReport(runID, ids...)
	accepted := append([]string(nil), ids[1:]...)
	r.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "completed",
		Decision: ReleasePassedWithPerformanceException, BoundedExceptionCheckIDs: accepted, ExceptionCheckIDs: accepted,
	}
	for index := range r.Checks {
		if r.Checks[index].ID == ids[0] {
			check := r.Checks[index]
			check.Status = Passed
			check.Observed = cloneReportMap(check.Observed)
			delete(check.Observed, "failure_type")
			delete(check.Observed, "failure_types")
			r.Checks[index] = check
			break
		}
	}
	if err := validateReleaseAssessment(r); err == nil {
		t.Fatal("accepted a raw strict-over-threshold check forged as passed")
	}
}

func TestS18ABBAAggregatesAllRoundsBeforeApplyingLimits(t *testing.T) {
	const runID = "stable-v2-aggregate"
	const id = runID + "/S18/c/syntax_update_after_edit"
	r := stableEvidenceReport(runID, id)
	limits, _ := stableS18LimitsForCheckID(runID, id)
	evidence := r.ABBAEvidence[id]
	for legIndex := range evidence.Rounds[0].Legs {
		leg := &evidence.Rounds[0].Legs[legIndex]
		if leg.Role == "upstream" {
			for sampleIndex := range leg.RawNS {
				leg.RawNS[sampleIndex] = 14e6
			}
		}
	}
	metrics, err := validateS18ABBAEvidence(id, findCheck(t, r, id), findSample(t, r, id), evidence, limits)
	if err != nil {
		t.Fatalf("validate aggregate evidence: %v", err)
	}
	if !metrics.Eligible {
		t.Fatal("aggregate-qualified evidence was rejected because one round alone misses the strict comparator")
	}
}

func TestStableV2ExceptionAllowsExceptionCapOverageOnStrictPassingQuantile(t *testing.T) {
	const runID = "stable-v2-strict-passing-over-cap"
	const id = runID + "/S18/c/completion_first_usable"
	r := strictPassingStableEvidenceReport(runID)

	var sampleIndex, checkIndex = -1, -1
	for index := range r.Samples {
		if r.Samples[index].ID == id {
			sampleIndex = index
			break
		}
	}
	for index := range r.Checks {
		if r.Checks[index].ID == id {
			checkIndex = index
			break
		}
	}
	if sampleIndex < 0 || checkIndex < 0 {
		t.Fatalf("fixture is missing target check %q", id)
	}

	sample := r.Samples[sampleIndex]
	for index := range sample.Raw {
		if index < len(sample.Raw)/2 {
			sample.Raw[index] = 45e6
		} else {
			sample.Raw[index] = 110e6
		}
	}
	percentiles := s18Percentiles(sample.Raw)
	sample.P50, sample.P95, sample.P99 = percentiles.p50, percentiles.p95, percentiles.p99
	r.Samples[sampleIndex] = sample

	evidence := r.ABBAEvidence[id]
	for roundIndex := range evidence.Rounds {
		for legIndex := range evidence.Rounds[roundIndex].Legs {
			leg := &evidence.Rounds[roundIndex].Legs[legIndex]
			if leg.Role == "candidate" {
				for offset := range leg.RequestIDs {
					outer := sample.Raw[leg.SampleStart+offset]
					leg.WriteRawNS[offset] = 1e6
					leg.WaitRawNS[offset] = outer - 4e6
				}
			} else {
				for offset := range leg.RawNS {
					leg.RawNS[offset] = 42e6
				}
			}
		}
	}
	r.ABBAEvidence[id] = evidence

	check := r.Checks[checkIndex]
	check.Status = Failed
	check.Summary = "strict p50 threshold exceeded"
	check.Observed = cloneReportMap(check.Observed)
	check.Observed["p50_ns"] = percentiles.p50
	check.Observed["p95_ns"] = percentiles.p95
	check.Observed["p99_ns"] = percentiles.p99
	check.Observed["failure_type"] = "latency"
	check.Observed["failure_types"] = []string{"latency"}
	r.Checks[checkIndex] = check
	r.Finalize(time.Now())
	r.ReleaseAssessment = &ReleaseAssessment{
		PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "completed",
		Decision:                 ReleasePassedWithPerformanceException,
		BoundedExceptionCheckIDs: []string{id}, ExceptionCheckIDs: []string{id},
	}

	limits, _ := stableS18LimitsForCheckID(runID, id)
	metrics, err := validateS18ABBAEvidence(id, check, sample, evidence, limits)
	if err != nil {
		t.Fatalf("validate candidate exception evidence: %v", err)
	}
	if metrics.candidate.p95 <= limits.absMaxMS[1]*1e6 || metrics.candidate.p95 > limits.strictMS[1]*1e6 {
		t.Fatalf("fixture p95=%g does not exceed only the exception cap", metrics.candidate.p95)
	}
	if !metrics.Eligible {
		t.Fatal("candidate exceeding an exception-only cap at a strict-passing quantile was rejected")
	}
	if err := validateReleaseAssessment(r); err != nil {
		t.Fatalf("valid mixed strict/exception quantiles should pass release assessment: %v", err)
	}
}

func TestS18ABBARejectsZeroOuterOrUpstreamDurations(t *testing.T) {
	const runID = "stable-v2-zero-duration"
	const id = runID + "/S18/c/syntax_update_after_edit"
	for _, test := range []struct {
		name   string
		mutate func(*Report)
	}{
		{name: "candidate outer", mutate: func(r *Report) { r.Samples[0].Raw[0] = 0 }},
		{name: "upstream", mutate: func(r *Report) {
			evidence := r.ABBAEvidence[id]
			evidence.Rounds[0].Legs[1].RawNS[0] = 0
			r.ABBAEvidence[id] = evidence
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := stableEvidenceReport(runID, id)
			test.mutate(&r)
			limits, _ := stableS18LimitsForCheckID(runID, id)
			if _, err := validateS18ABBAEvidence(id, findCheck(t, r, id), findSample(t, r, id), r.ABBAEvidence[id], limits); err == nil {
				t.Fatal("accepted a zero-duration candidate or upstream sample")
			}
		})
	}
}

func TestStableV2PassedRejectsOverBudgetSameRequestOverhead(t *testing.T) {
	const runID = "stable-v2-overhead-pass"
	const id = runID + "/S18/c/completion_first_usable"
	r := strictPassingStableEvidenceReport(runID)
	evidence := r.ABBAEvidence[id]
	for roundIndex := range evidence.Rounds {
		for legIndex := range evidence.Rounds[roundIndex].Legs {
			leg := &evidence.Rounds[roundIndex].Legs[legIndex]
			if leg.Role != "candidate" {
				continue
			}
			for offset := range leg.RequestIDs {
				outer := r.Samples[0].Raw[leg.SampleStart+offset]
				leg.WriteRawNS[offset] = 0
				leg.WaitRawNS[offset] = outer - 8e6
			}
		}
	}
	r.ABBAEvidence[id] = evidence

	limits, _ := stableS18LimitsForCheckID(runID, id)
	metrics, err := validateS18ABBAEvidence(id, findCheck(t, r, id), findSample(t, r, id), evidence, limits)
	if err != nil {
		t.Fatalf("validate complete strict-passing evidence: %v", err)
	}
	if s18WithinOverheadBudget(metrics.overhead, limits) {
		t.Fatal("fixture did not exceed the aggregate same-request overhead budget")
	}
	if err := validateReleaseAssessment(r); err == nil {
		t.Fatal("accepted stable ReleasePassed with an over-budget ABBA overhead quantile")
	}
}

func TestWritePreservesIncompleteABBAEvidenceForFailedOrUnverifiedRuns(t *testing.T) {
	const runID = "stable-v2-incomplete"
	const checkID = runID + "/S18/c/syntax_update_after_edit"
	for _, test := range []struct {
		name       string
		decision   Status
		assessment ReleaseAssessment
	}{
		{name: "failed", decision: Failed, assessment: ReleaseAssessment{PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "completed", Decision: ReleaseFailed}},
		{name: "not verified", decision: NotVerified, assessment: ReleaseAssessment{PolicyID: s18EvidenceQualifiedPolicyID, ExecutionStatus: "incomplete", Decision: ReleaseNotVerified}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := New(runID)
			r.Decision = test.decision
			r.ReleaseAssessment = &test.assessment
			r.ABBAEvidence = map[string]ABBAEvidence{
				checkID: {CheckID: checkID, FixtureID: "c", Operation: "syntax_update_after_edit", Method: "textDocument/documentSymbol", Rounds: []ABBARound{{Index: 0}}},
			}
			path := t.TempDir() + "/incomplete.json"
			if err := Write(path, r); err != nil {
				t.Fatalf("write diagnostic report: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got Report
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if got.Decision != test.decision || len(got.ABBAEvidence[checkID].Rounds) != 1 {
				t.Fatalf("incomplete evidence was not preserved: decision=%q evidence=%+v", got.Decision, got.ABBAEvidence[checkID])
			}
		})
	}
}

func findCheck(t *testing.T, r Report, id string) Check {
	t.Helper()
	for _, check := range r.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("missing check %q", id)
	return Check{}
}

func findSample(t *testing.T, r Report, id string) SampleSet {
	t.Helper()
	for _, sample := range r.Samples {
		if sample.ID == id {
			return sample
		}
	}
	t.Fatalf("missing sample set %q", id)
	return SampleSet{}
}
