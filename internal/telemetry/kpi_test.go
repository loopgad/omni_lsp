package telemetry

import (
	"strings"
	"testing"
)

// TestS20_KPIFormulas pins the §S20 formulas against a hand-computed window.
func TestS20_KPIFormulas(t *testing.T) {
	c := Counts{
		TruePositive: 80, FalsePositive: 20, FalseNegative: 20, TrueNegative: 880,
		Refusals: 30, StaleRejected: 7, TotalAnswers: 1000,
	}
	if got := c.Precision(); got != 0.8 {
		t.Errorf("precision = %v, want 0.8", got)
	}
	if got := c.Recall(); got != 0.8 {
		t.Errorf("recall = %v, want 0.8", got)
	}
	if got := c.FalsePositiveRate(); got != 20.0/900.0 {
		t.Errorf("fpr = %v", got)
	}
	if got := c.FalseNegativeRate(); got != 0.2 {
		t.Errorf("fnr = %v, want 0.2", got)
	}
	if got := c.RefusalRate(); got != 0.03 {
		t.Errorf("refusal rate = %v, want 0.03", got)
	}
	if got := c.StaleRejectionRate(); got != 0.007 {
		t.Errorf("stale rejection rate = %v, want 0.007", got)
	}
}

// TestS20_KPIZeroDenominatorSafe: empty windows report zeros, never NaN
// (NaN would poison downstream JSON encoders).
func TestS20_KPIZeroDenominatorSafe(t *testing.T) {
	var zero Counts
	rep := zero.Report("hover")
	if strings.Contains(rep, "NaN") {
		t.Fatalf("report contains NaN: %s", rep)
	}
	if !strings.Contains(rep, "precision=0.0000") {
		t.Errorf("unexpected empty-window report: %s", rep)
	}
}
