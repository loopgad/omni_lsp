// Package kpi computes the §S20 accuracy metrics per feature/language/backend
// from confusion-count inputs. Pure functions: callers own collection; this
// package owns the canonical formulas so every surface reports identically.
package telemetry

import "fmt"

// Counts is one feature's confusion tally over an evaluation window.
type Counts struct {
	TruePositive  int64
	FalsePositive int64
	FalseNegative int64
	TrueNegative  int64

	// Refusals are honest Unknown results (§A3 Zero-Wrong-Result): reported
	// separately, never folded into precision/recall.
	Refusals int64

	// StaleRejected counts results refused by the freshness gate (§D11) —
	// rejections are the system working, tracked to prove the gate fires.
	StaleRejected int64

	// TotalAnswers is every answer surfaced (correct + wrong + stale-accepted).
	TotalAnswers int64
}

func ratio(num, den int64) float64 {
	if den == 0 {
		return 0 // undefined metric reports as zero, never NaN into JSON
	}
	return float64(num) / float64(den)
}

// Precision = TP / (TP + FP).
func (c Counts) Precision() float64 { return ratio(c.TruePositive, c.TruePositive+c.FalsePositive) }

// Recall = TP / (TP + FN).
func (c Counts) Recall() float64 { return ratio(c.TruePositive, c.TruePositive+c.FalseNegative) }

// FalsePositiveRate = FP / (FP + TN).
func (c Counts) FalsePositiveRate() float64 {
	return ratio(c.FalsePositive, c.FalsePositive+c.TrueNegative)
}

// FalseNegativeRate = FN / (FN + TP).
func (c Counts) FalseNegativeRate() float64 {
	return ratio(c.FalseNegative, c.FalseNegative+c.TruePositive)
}

// RefusalRate is the honest-unknown share of all answers.
func (c Counts) RefusalRate() float64 { return ratio(c.Refusals, c.TotalAnswers) }

// StaleRejectionRate is the share of answers caught by the freshness gate.
func (c Counts) StaleRejectionRate() float64 {
	return ratio(c.StaleRejected, c.TotalAnswers)
}

// Report renders the canonical §S20 five-line block.
func (c Counts) Report(feature string) string {
	return fmt.Sprintf(
		"%s precision=%.4f recall=%.4f fpr=%.4f fnr=%.4f refusal=%.4f stale_rejected=%d",
		feature, c.Precision(), c.Recall(), c.FalsePositiveRate(),
		c.FalseNegativeRate(), c.RefusalRate(), c.StaleRejected)
}
