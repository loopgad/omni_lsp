// Package telemetry defines the test-only, machine-readable §S20 KPI contract.
// Callers provide explicit evidence counts; unsampled counts remain unknown so
// an empty measurement can never be mistaken for a zero error rate.
package telemetry

const KPIReportSchema = "omnilsp.s20.accuracy-kpi.v2"

type MetricStatus string

const (
	MetricObserved      MetricStatus = "observed"
	MetricNotVerified   MetricStatus = "not_verified"
	MetricNotApplicable MetricStatus = "not_applicable"
	MetricInvalid       MetricStatus = "invalid"
)

// Count distinguishes an observed zero from a value that was never sampled.
type Count struct {
	Value    int64 `json:"value,omitempty"`
	Observed bool  `json:"observed"`
}

// Sampled marks a count as measured, including when its measured value is zero.
func Sampled(value int64) Count { return Count{Value: value, Observed: true} }

// NotApplicable marks a metric outside the operation represented by its
// dimension. The caller must supply a precise reason; it is not a substitute
// for missing evidence on an applicable metric.
func NotApplicable(formula, reason string) Metric {
	return Metric{Formula: formula, Status: MetricNotApplicable, Reason: reason}
}

// Inputs contains only evidence gathered by the caller. A zero-value Count is
// unknown; callers must use Sampled(0) to report a measured zero.
type Inputs struct {
	TruePositive  Count
	FalsePositive Count
	FalseNegative Count
	TrueNegative  Count

	Refusals Count
	Requests Count

	StaleRejected   Count
	StaleCandidates Count

	WrongFileLocations Count
	LocationResults    Count

	PositionMappingFailures Count
	PositionMappingSamples  Count

	EditValidationFailures Count
	EditValidationAttempts Count
}

type Dimension struct {
	Feature  string `json:"feature"`
	Language string `json:"language"`
	Backend  string `json:"backend"`
	Oracle   string `json:"oracle,omitempty"`
}

// Metric keeps the formula alongside its evidence. Numerator, denominator and
// value are omitted whenever the corresponding evidence is not available.
type Metric struct {
	Formula     string       `json:"formula"`
	Status      MetricStatus `json:"status"`
	Numerator   *int64       `json:"numerator,omitempty"`
	Denominator *int64       `json:"denominator,omitempty"`
	Value       *float64     `json:"value,omitempty"`
	Reason      string       `json:"reason,omitempty"`
}

// Metrics is the complete nine-field S20 KPI set.
type Metrics struct {
	Precision                  Metric `json:"precision"`
	Recall                     Metric `json:"recall"`
	FalsePositiveRate          Metric `json:"falsePositiveRate"`
	FalseNegativeRate          Metric `json:"falseNegativeRate"`
	RefusalRate                Metric `json:"refusalRate"`
	StaleResultRejectionRate   Metric `json:"staleResultRejectionRate"`
	WrongFileRate              Metric `json:"wrongFileRate"`
	PositionMappingFailureRate Metric `json:"positionMappingFailureRate"`
	EditValidationFailureRate  Metric `json:"editValidationFailureRate"`
}

type Record struct {
	Dimension Dimension `json:"dimension"`
	Metrics   Metrics   `json:"metrics"`
}

// NewRecord calculates the canonical S20 formulas from explicitly sampled
// counts. Formula units are query outcomes for confusion rates and individual
// semantic results for wrong-file/position checks; callers must not combine
// incompatible sampling windows in one Inputs value.
func NewRecord(dimension Dimension, in Inputs) Record {
	tpFP := add(in.TruePositive, in.FalsePositive)
	tpFN := add(in.TruePositive, in.FalseNegative)
	fpTN := add(in.FalsePositive, in.TrueNegative)
	return Record{
		Dimension: dimension,
		Metrics: Metrics{
			Precision:                  ratio(in.TruePositive, tpFP, "TP/(TP+FP)"),
			Recall:                     ratio(in.TruePositive, tpFN, "TP/(TP+FN)"),
			FalsePositiveRate:          ratio(in.FalsePositive, fpTN, "FP/(FP+TN)"),
			FalseNegativeRate:          ratio(in.FalseNegative, tpFN, "FN/(FN+TP)"),
			RefusalRate:                ratio(in.Refusals, in.Requests, "refusals/requests"),
			StaleResultRejectionRate:   ratio(in.StaleRejected, in.StaleCandidates, "stale_results_rejected/stale_results_presented_to_freshness_gate"),
			WrongFileRate:              ratio(in.WrongFileLocations, in.LocationResults, "wrong_file_locations/returned_locations"),
			PositionMappingFailureRate: ratio(in.PositionMappingFailures, in.PositionMappingSamples, "position_mapping_failures/position_mapping_samples"),
			EditValidationFailureRate:  ratio(in.EditValidationFailures, in.EditValidationAttempts, "edit_validation_failures/edit_validation_attempts"),
		},
	}
}

func ratio(numerator, denominator Count, formula string) Metric {
	metric := Metric{Formula: formula, Status: MetricNotVerified}
	if numerator.Observed {
		v := numerator.Value
		metric.Numerator = &v
	}
	if denominator.Observed {
		v := denominator.Value
		metric.Denominator = &v
	}
	if !numerator.Observed || !denominator.Observed {
		metric.Reason = "numerator or denominator was not sampled"
		return metric
	}
	if numerator.Value < 0 || denominator.Value < 0 || numerator.Value > denominator.Value {
		metric.Status = MetricInvalid
		metric.Reason = "sampled counts are negative or numerator exceeds denominator"
		return metric
	}
	if denominator.Value == 0 {
		metric.Reason = "observed denominator is zero; rate is undefined"
		return metric
	}
	value := float64(numerator.Value) / float64(denominator.Value)
	metric.Value = &value
	metric.Status = MetricObserved
	return metric
}

func add(left, right Count) Count {
	if !left.Observed || !right.Observed {
		return Count{}
	}
	return Sampled(left.Value + right.Value)
}

func (r Record) Complete() bool {
	for _, metric := range []Metric{
		r.Metrics.Precision, r.Metrics.Recall, r.Metrics.FalsePositiveRate,
		r.Metrics.FalseNegativeRate, r.Metrics.RefusalRate,
		r.Metrics.StaleResultRejectionRate, r.Metrics.WrongFileRate,
		r.Metrics.PositionMappingFailureRate, r.Metrics.EditValidationFailureRate,
	} {
		if metric.Status != MetricObserved && (metric.Status != MetricNotApplicable || metric.Reason == "" || metric.Numerator != nil || metric.Denominator != nil || metric.Value != nil) {
			return false
		}
	}
	return true
}

func (r Record) Invalid() bool {
	for _, metric := range []Metric{
		r.Metrics.Precision, r.Metrics.Recall, r.Metrics.FalsePositiveRate,
		r.Metrics.FalseNegativeRate, r.Metrics.RefusalRate,
		r.Metrics.StaleResultRejectionRate, r.Metrics.WrongFileRate,
		r.Metrics.PositionMappingFailureRate, r.Metrics.EditValidationFailureRate,
	} {
		if metric.Status == MetricInvalid || (metric.Status == MetricNotApplicable && (metric.Reason == "" || metric.Numerator != nil || metric.Denominator != nil || metric.Value != nil)) {
			return true
		}
	}
	return false
}
