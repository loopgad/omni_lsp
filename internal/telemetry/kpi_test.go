package telemetry

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestS20_KPIFormulas pins all nine §S20 formulas against sampled outcomes,
// including both positive and negative classification examples.
func TestS20_KPIFormulas(t *testing.T) {
	record := NewRecord(Dimension{Feature: "references", Language: "go", Backend: "golang-native", Oracle: "gopls"}, Inputs{
		TruePositive: Sampled(8), FalsePositive: Sampled(2), FalseNegative: Sampled(2), TrueNegative: Sampled(88),
		Refusals: Sampled(3), Requests: Sampled(100),
		StaleRejected: Sampled(7), StaleCandidates: Sampled(10),
		WrongFileLocations: Sampled(1), LocationResults: Sampled(10),
		PositionMappingFailures: Sampled(2), PositionMappingSamples: Sampled(20),
		EditValidationFailures: Sampled(1), EditValidationAttempts: Sampled(5),
	})
	assertMetric := func(name string, metric Metric, formula string, numerator, denominator int64, want float64) {
		t.Helper()
		if metric.Status != MetricObserved || metric.Formula != formula {
			t.Fatalf("%s status/formula = %q/%q, want observed/%q", name, metric.Status, metric.Formula, formula)
		}
		if metric.Numerator == nil || *metric.Numerator != numerator || metric.Denominator == nil || *metric.Denominator != denominator {
			t.Fatalf("%s counts = %v/%v, want %d/%d", name, metric.Numerator, metric.Denominator, numerator, denominator)
		}
		if metric.Value == nil || *metric.Value != want {
			t.Fatalf("%s value = %v, want %v", name, metric.Value, want)
		}
	}
	assertMetric("precision", record.Metrics.Precision, "TP/(TP+FP)", 8, 10, 0.8)
	assertMetric("recall", record.Metrics.Recall, "TP/(TP+FN)", 8, 10, 0.8)
	assertMetric("false positive rate", record.Metrics.FalsePositiveRate, "FP/(FP+TN)", 2, 90, 2.0/90.0)
	assertMetric("false negative rate", record.Metrics.FalseNegativeRate, "FN/(FN+TP)", 2, 10, 0.2)
	assertMetric("refusal rate", record.Metrics.RefusalRate, "refusals/requests", 3, 100, 0.03)
	assertMetric("stale result rejection rate", record.Metrics.StaleResultRejectionRate, "stale_results_rejected/stale_results_presented_to_freshness_gate", 7, 10, 0.7)
	assertMetric("wrong file rate", record.Metrics.WrongFileRate, "wrong_file_locations/returned_locations", 1, 10, 0.1)
	assertMetric("position mapping failure rate", record.Metrics.PositionMappingFailureRate, "position_mapping_failures/position_mapping_samples", 2, 20, 0.1)
	assertMetric("edit validation failure rate", record.Metrics.EditValidationFailureRate, "edit_validation_failures/edit_validation_attempts", 1, 5, 0.2)
	if !record.Complete() || record.Invalid() {
		t.Fatalf("fully sampled record not complete: %+v", record)
	}
}

// TestS20_KPIZeroDenominatorSafe requires undefined/unsampled rates to remain
// not_verified without a zero value that could look like a passing result.
func TestS20_KPIZeroDenominatorSafe(t *testing.T) {
	record := NewRecord(Dimension{Feature: "hover", Language: "go", Backend: "golang-native"}, Inputs{
		TruePositive: Sampled(0), FalsePositive: Sampled(0), FalseNegative: Sampled(0), TrueNegative: Sampled(0),
		Refusals: Sampled(0), Requests: Sampled(0),
	})
	for name, metric := range map[string]Metric{
		"precision":           record.Metrics.Precision,
		"recall":              record.Metrics.Recall,
		"false-positive rate": record.Metrics.FalsePositiveRate,
		"false-negative rate": record.Metrics.FalseNegativeRate,
		"refusal rate":        record.Metrics.RefusalRate,
	} {
		if metric.Status != MetricNotVerified || metric.Value != nil || metric.Reason == "" {
			t.Errorf("%s empty window must be not_verified without value; got %+v", name, metric)
		}
	}

	unobserved := NewRecord(Dimension{Feature: "references", Language: "python", Backend: "pyright"}, Inputs{})
	encoded, err := json.Marshal(unobserved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"value":0`) || strings.Contains(string(encoded), `"numerator":0`) {
		t.Fatalf("unobserved KPI serialized a synthetic zero: %s", encoded)
	}
	if unobserved.Metrics.StaleResultRejectionRate.Status != MetricNotVerified || unobserved.Metrics.EditValidationFailureRate.Status != MetricNotVerified {
		t.Fatalf("uncollected stale/edit metrics must be not_verified: %+v", unobserved.Metrics)
	}
}

func TestS20_KPIRejectsImpossibleCounts(t *testing.T) {
	record := NewRecord(Dimension{Feature: "definition", Language: "go", Backend: "golang-native"}, Inputs{
		TruePositive: Sampled(3), FalsePositive: Sampled(-1), FalseNegative: Sampled(0), TrueNegative: Sampled(2),
		Refusals: Sampled(0), Requests: Sampled(4),
	})
	if record.Metrics.RefusalRate.Status != MetricObserved {
		t.Fatalf("known zero refusal rate should be observed: %+v", record.Metrics.RefusalRate)
	}
	if record.Metrics.Precision.Status != MetricInvalid || !record.Invalid() {
		t.Fatalf("invalid inputs were not surfaced: %+v", record.Metrics.Precision)
	}
}

func TestS20_KPINotApplicableIsExplicitAndComplete(t *testing.T) {
	record := NewRecord(Dimension{Feature: "definition", Language: "Go", Backend: "golang"}, Inputs{
		TruePositive: Sampled(1), FalsePositive: Sampled(0), FalseNegative: Sampled(0), TrueNegative: Sampled(1),
		Refusals: Sampled(0), Requests: Sampled(1),
		StaleRejected: Sampled(1), StaleCandidates: Sampled(1),
		WrongFileLocations: Sampled(0), LocationResults: Sampled(1),
		PositionMappingFailures: Sampled(0), PositionMappingSamples: Sampled(1),
	})
	record.Metrics.EditValidationFailureRate = NotApplicable(
		"edit_validation_failures/edit_validation_attempts",
		"definition returns read-only locations and produces no WorkspaceEdit",
	)
	if !record.Complete() || record.Invalid() {
		t.Fatalf("explicit not-applicable metric should complete the record: %+v", record)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"status":"not_applicable"`) || !strings.Contains(string(encoded), `"reason":"definition returns read-only locations and produces no WorkspaceEdit"`) {
		t.Fatalf("not-applicable status or reason missing from JSON: %s", encoded)
	}
	if record.Metrics.EditValidationFailureRate.Value != nil || record.Metrics.EditValidationFailureRate.Numerator != nil || record.Metrics.EditValidationFailureRate.Denominator != nil {
		t.Fatalf("not-applicable metric must not invent samples: %+v", record.Metrics.EditValidationFailureRate)
	}

	record.Metrics.EditValidationFailureRate = NotApplicable("edit validation", "")
	if record.Complete() || !record.Invalid() {
		t.Fatalf("not-applicable metric without a reason must remain invalid/incomplete: %+v", record.Metrics.EditValidationFailureRate)
	}
	reasonedButSampled := NotApplicable("edit validation", "no edit result is produced")
	zero := int64(0)
	reasonedButSampled.Numerator = &zero
	record.Metrics.EditValidationFailureRate = reasonedButSampled
	if record.Complete() || !record.Invalid() {
		t.Fatalf("not-applicable metric carrying synthetic counts must remain invalid/incomplete: %+v", record.Metrics.EditValidationFailureRate)
	}
}
