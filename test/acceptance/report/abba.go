package report

import (
	"fmt"
	"math"
	"sort"
)

const s18EvidenceQualifiedPolicyID = "s18-evidence-qualified-v2"

type s18OperationLimits struct {
	fixtureID string
	operation string
	method    string
	strictMS  [3]float64
	absMaxMS  [3]float64
	overhead  [3]float64
}

type s18Quantiles struct {
	p50 float64
	p95 float64
	p99 float64
}

type s18ABBAMetrics struct {
	candidate s18Quantiles
	upstream  s18Quantiles
	overhead  s18Quantiles
	Eligible  bool
}

var s18StableOperations = []s18OperationLimits{
	{fixtureID: "c", operation: "completion_first_usable", method: "textDocument/completion", strictMS: [3]float64{40, 120, 250}, absMaxMS: [3]float64{80, 100, 125}, overhead: [3]float64{5, 10, 20}},
	{fixtureID: "cpp", operation: "completion_first_usable", method: "textDocument/completion", strictMS: [3]float64{40, 120, 250}, absMaxMS: [3]float64{80, 100, 125}, overhead: [3]float64{5, 10, 20}},
	{fixtureID: "c", operation: "syntax_update_after_edit", method: "textDocument/documentSymbol", strictMS: [3]float64{15, 50, 100}, absMaxMS: [3]float64{60, 100, 150}, overhead: [3]float64{5, 10, 20}},
	{fixtureID: "cpp", operation: "syntax_update_after_edit", method: "textDocument/documentSymbol", strictMS: [3]float64{15, 50, 100}, absMaxMS: [3]float64{60, 100, 150}, overhead: [3]float64{5, 10, 20}},
}

func stableS18LimitsForCheckID(runID, id string) (s18OperationLimits, bool) {
	for _, limits := range s18StableOperations {
		if id == fmt.Sprintf("%s/S18/%s/%s", runID, limits.fixtureID, limits.operation) {
			return limits, true
		}
	}
	return s18OperationLimits{}, false
}

func validateStableS18PassedEvidence(r Report, checks map[string]Check) error {
	for id := range r.ABBAEvidence {
		if _, ok := stableS18LimitsForCheckID(r.RunID, id); !ok {
			return fmt.Errorf("ABBA evidence check %q is outside the stable S18 scope", id)
		}
	}
	for _, limits := range s18StableOperations {
		id := fmt.Sprintf("%s/S18/%s/%s", r.RunID, limits.fixtureID, limits.operation)
		check, ok := checks[id]
		if !ok || check.Status != Passed {
			return fmt.Errorf("stable S18 release pass is missing passed check %q", id)
		}
		evidence, ok := r.ABBAEvidence[id]
		if !ok {
			return fmt.Errorf("stable S18 release pass is missing complete ABBA evidence for %q", id)
		}
		sample, ok := findSampleSet(r.Samples, id)
		if !ok {
			return fmt.Errorf("stable S18 release pass is missing a unique sample set for %q", id)
		}
		metrics, err := validateS18ABBAEvidence(id, check, sample, evidence, limits)
		if err != nil {
			return err
		}
		if !s18WithinStrictLimits(metrics.candidate, limits) || !s18WithinOverheadBudget(metrics.overhead, limits) || hasLatencyFailureType(check) || !s18SemanticEvidencePassed(check, limits) {
			return fmt.Errorf("stable S18 release pass has unverified or over-threshold evidence for %q", id)
		}
	}
	return nil
}

func findSampleSet(samples []SampleSet, id string) (SampleSet, bool) {
	var found SampleSet
	count := 0
	for _, sample := range samples {
		if sample.ID == id {
			found = sample
			count++
		}
	}
	return found, count == 1
}

func s18WithinStrictLimits(candidate s18Quantiles, limits s18OperationLimits) bool {
	for index, value := range quantileArray(candidate) {
		if value > limits.strictMS[index]*1e6 {
			return false
		}
	}
	return true
}

func s18WithinOverheadBudget(overhead s18Quantiles, limits s18OperationLimits) bool {
	for index, value := range quantileArray(overhead) {
		if value > limits.overhead[index]*1e6 {
			return false
		}
	}
	return true
}

func validateStableS18ExceptionEvidence(r Report, checks map[string]Check, accepted map[string]struct{}) error {
	for id := range r.ABBAEvidence {
		if _, ok := stableS18LimitsForCheckID(r.RunID, id); !ok {
			return fmt.Errorf("ABBA evidence check %q is outside the stable S18 scope", id)
		}
	}
	for _, limits := range s18StableOperations {
		id := fmt.Sprintf("%s/S18/%s/%s", r.RunID, limits.fixtureID, limits.operation)
		check, ok := checks[id]
		if !ok {
			return fmt.Errorf("stable S18 exception assessment is missing check %q", id)
		}
		evidence, ok := r.ABBAEvidence[id]
		if !ok {
			return fmt.Errorf("stable S18 exception assessment is missing ABBA evidence for %q", id)
		}
		sample, ok := findSampleSet(r.Samples, id)
		if !ok {
			return fmt.Errorf("stable S18 exception assessment is missing a unique sample set for %q", id)
		}
		metrics, err := validateS18ABBAEvidence(id, check, sample, evidence, limits)
		if err != nil {
			return err
		}
		if !s18WithinOverheadBudget(metrics.overhead, limits) {
			return fmt.Errorf("stable S18 check %q exceeds the same-request overhead budget", id)
		}
		if !s18SemanticEvidencePassed(check, limits) {
			return fmt.Errorf("stable S18 exception assessment has unverified semantic evidence for %q", id)
		}
		if s18StrictExceeded(metrics.candidate, limits) {
			if check.Status != Failed || !isLatencyOnlyFailure(check) {
				return fmt.Errorf("strict-over-threshold check %q must remain a latency-only failure", id)
			}
			if _, ok := accepted[id]; !ok {
				return fmt.Errorf("strict-over-threshold check %q is not listed as an accepted exception", id)
			}
			if !metrics.Eligible {
				return fmt.Errorf("strict-over-threshold check %q is outside the evidence-qualified limits", id)
			}
			continue
		}
		if check.Status != Passed || hasLatencyFailureType(check) {
			return fmt.Errorf("strict-passing check %q has inconsistent status or latency failure type", id)
		}
		if _, ok := accepted[id]; ok {
			return fmt.Errorf("strict-passing check %q cannot be an accepted performance exception", id)
		}
	}
	return nil
}

func s18StrictExceeded(candidate s18Quantiles, limits s18OperationLimits) bool {
	for index, value := range quantileArray(candidate) {
		if value > limits.strictMS[index]*1e6 {
			return true
		}
	}
	return false
}

func hasLatencyFailureType(check Check) bool {
	if check.Observed["failure_type"] == "latency" {
		return true
	}
	switch types := check.Observed["failure_types"].(type) {
	case []string:
		for _, failureType := range types {
			if failureType == "latency" {
				return true
			}
		}
	case []any:
		for _, failureType := range types {
			if failureType == "latency" {
				return true
			}
		}
	}
	return false
}

func validateS18ABBAEvidence(id string, check Check, sample SampleSet, evidence ABBAEvidence, limits s18OperationLimits) (s18ABBAMetrics, error) {
	if evidence.CheckID != id || evidence.FixtureID != limits.fixtureID || evidence.Operation != limits.operation || evidence.Method != limits.method {
		return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence for %q has inconsistent check, fixture, operation, or method metadata", id)
	}
	if sample.ID != id || sample.Unit != "ns/op" || len(sample.Raw) < 6000 {
		return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q requires at least 6000 candidate ns/op samples", id)
	}
	if err := validateS18StrictThreshold(check, limits); err != nil {
		return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q: %w", id, err)
	}
	for index, value := range sample.Raw {
		if !validPositiveDurationNS(value) {
			return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q has invalid candidate raw sample %d", id, index)
		}
	}

	if len(evidence.Rounds) != 3 {
		return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q requires exactly three rounds", id)
	}
	const candidateRole = "candidate"
	const upstreamRole = "upstream"
	expectedOrder := [...]string{candidateRole, upstreamRole, upstreamRole, candidateRole}
	var candidateRaw, upstreamRaw, overheadRaw []float64
	candidateStart := 0
	seenRequestIDs := make(map[string]struct{})
	for roundIndex, round := range evidence.Rounds {
		if round.Index != roundIndex || len(round.Order) != len(expectedOrder) || len(round.Legs) != len(expectedOrder) {
			return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q round %d has invalid index, order, or leg count", id, roundIndex)
		}
		for legIndex, leg := range round.Legs {
			wantRole := expectedOrder[legIndex]
			if round.Order[legIndex] != wantRole || leg.Position != legIndex || leg.Role != wantRole {
				return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q round %d leg %d is out of C/U/U/C order", id, roundIndex, legIndex)
			}
			if wantRole == candidateRole {
				if leg.SampleStart != candidateStart || leg.SampleCount < 1000 || len(leg.RequestIDs) != leg.SampleCount || len(leg.WriteRawNS) != leg.SampleCount || len(leg.WaitRawNS) != leg.SampleCount || len(leg.RawNS) != 0 {
					return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q round %d candidate leg %d has invalid candidate sample reference or correlated phase samples", id, roundIndex, legIndex)
				}
				end := leg.SampleStart + leg.SampleCount
				if end < leg.SampleStart || end > len(sample.Raw) {
					return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q round %d candidate sample range is outside its sample set", id, roundIndex)
				}
				for offset := 0; offset < leg.SampleCount; offset++ {
					requestID := leg.RequestIDs[offset]
					if requestID == "" {
						return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q round %d candidate leg has an empty request ID", id, roundIndex)
					}
					if _, duplicate := seenRequestIDs[requestID]; duplicate {
						return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q has duplicate candidate request ID %q", id, requestID)
					}
					seenRequestIDs[requestID] = struct{}{}
					outer := sample.Raw[leg.SampleStart+offset]
					write := leg.WriteRawNS[offset]
					wait := leg.WaitRawNS[offset]
					overhead := outer - write - wait
					if !validDurationNS(write) || !validDurationNS(wait) || !validDurationNS(overhead) {
						return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q round %d has invalid same-request overhead inputs", id, roundIndex)
					}
					candidateRaw = append(candidateRaw, outer)
					overheadRaw = append(overheadRaw, overhead)
				}
				candidateStart = end
			} else {
				if leg.SampleStart != 0 || leg.SampleCount != 0 || len(leg.RequestIDs) != 0 || len(leg.WriteRawNS) != 0 || len(leg.WaitRawNS) != 0 || len(leg.RawNS) < 1000 {
					return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q round %d upstream leg %d has invalid raw samples", id, roundIndex, legIndex)
				}
				for sampleIndex, value := range leg.RawNS {
					if !validPositiveDurationNS(value) {
						return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q round %d has invalid upstream raw sample %d", id, roundIndex, sampleIndex)
					}
				}
				upstreamRaw = append(upstreamRaw, leg.RawNS...)
			}
		}
	}
	if candidateStart != len(sample.Raw) {
		return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q candidate ranges do not partition the complete operation sample set", id)
	}
	if err := validateS18CandidateSummaries(check, sample, candidateRaw); err != nil {
		return s18ABBAMetrics{}, fmt.Errorf("ABBA evidence check %q: %w", id, err)
	}
	metrics := s18ABBAMetrics{
		candidate: s18Percentiles(candidateRaw),
		upstream:  s18Percentiles(upstreamRaw),
		overhead:  s18Percentiles(overheadRaw),
	}
	metrics.Eligible = s18ExceptionWithinLimits(metrics, limits)
	if !s18SemanticEvidencePassed(check, limits) {
		metrics.Eligible = false
	}
	return metrics, nil
}

func s18ExceptionWithinLimits(metrics s18ABBAMetrics, limits s18OperationLimits) bool {
	candidate := quantileArray(metrics.candidate)
	upstream := quantileArray(metrics.upstream)
	overhead := quantileArray(metrics.overhead)
	strictOverage := false
	for index := range candidate {
		strictNS := limits.strictMS[index] * 1e6
		absoluteNS := limits.absMaxMS[index] * 1e6
		overheadNS := limits.overhead[index] * 1e6
		if overhead[index] > overheadNS {
			return false
		}
		if candidate[index] > strictNS {
			strictOverage = true
			if upstream[index] <= strictNS || candidate[index] > absoluteNS || candidate[index] > upstream[index]+overheadNS {
				return false
			}
		}
	}
	return strictOverage
}

func quantileArray(values s18Quantiles) [3]float64 {
	return [3]float64{values.p50, values.p95, values.p99}
}

func s18Percentiles(samples []float64) s18Quantiles {
	return s18Quantiles{
		p50: nearestRankPercentile(samples, .50),
		p95: nearestRankPercentile(samples, .95),
		p99: nearestRankPercentile(samples, .99),
	}
}

func nearestRankPercentile(samples []float64, quantile float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	ordered := append([]float64(nil), samples...)
	sort.Float64s(ordered)
	index := int(math.Ceil(quantile*float64(len(ordered)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}

func validateS18StrictThreshold(check Check, limits s18OperationLimits) error {
	for index, name := range []string{"p50_ms", "p95_ms", "p99_ms"} {
		value, ok := reportNumber(check.Threshold[name])
		if !ok || value != limits.strictMS[index] {
			return fmt.Errorf("strict %s threshold does not match policy", name)
		}
	}
	return nil
}

func validateS18CandidateSummaries(check Check, sample SampleSet, raw []float64) error {
	computed := s18Percentiles(raw)
	values := quantileArray(computed)
	sampleValues := [3]float64{sample.P50, sample.P95, sample.P99}
	observedNames := [3]string{"p50_ns", "p95_ns", "p99_ns"}
	for index, value := range values {
		if sampleValues[index] != value {
			return fmt.Errorf("candidate %s sample percentile does not match raw samples", observedNames[index])
		}
		observed, ok := reportNumber(check.Observed[observedNames[index]])
		if !ok || observed != value {
			return fmt.Errorf("candidate %s check percentile does not match raw samples", observedNames[index])
		}
	}
	count, ok := reportNumber(check.Observed["sample_count"])
	if !ok || count != float64(len(raw)) {
		return fmt.Errorf("candidate sample_count does not match raw samples")
	}
	return nil
}

func s18SemanticEvidencePassed(check Check, limits s18OperationLimits) bool {
	validation, ok := check.Observed["validation"].(map[string]any)
	if !ok || validation["candidate_status"] != "passed" || validation["candidate_normalization_status"] != "passed" ||
		validation["upstream_status"] != "passed" || validation["upstream_normalization_status"] != "passed" {
		return false
	}
	differential, ok := check.Observed["upstream_differential"].(map[string]any)
	if !ok || differential["status"] != "passed" || differential["matched"] != true {
		return false
	}
	if limits.operation == "completion_first_usable" && differential["target_candidate_sample_status"] != "passed" {
		return false
	}
	editUpdate, ok := check.Observed["edit_update"].(map[string]any)
	if !ok {
		return false
	}
	wantEditStatus := "passed"
	if limits.operation == "completion_first_usable" {
		wantEditStatus = "not_applicable"
	}
	return editUpdate["status"] == wantEditStatus
}

func validDurationNS(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func validPositiveDurationNS(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}

func reportNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case float32:
		return float64(typed), !math.IsNaN(float64(typed)) && !math.IsInf(float64(typed), 0)
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	default:
		return 0, false
	}
}
