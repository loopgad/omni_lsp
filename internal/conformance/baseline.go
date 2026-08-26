package conformance

import (
	"encoding/json"
	"os"
)

// Baseline is the committed regression floor for the core domain.
type Baseline struct {
	CoreScore  float64            `json:"coreScore"`
	Categories map[string]float64 `json:"categories"`
}

// WriteBaselineFile persists a new regression floor (explicit update path).
func WriteBaselineFile(path string, core float64, cats map[string]float64) error {
	data, err := json.MarshalIndent(Baseline{CoreScore: core, Categories: cats}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

// ParseBaseline reads a committed floor.
func ParseBaseline(data []byte) (Baseline, error) {
	var b Baseline
	err := json.Unmarshal(data, &b)
	return b, err
}

// CoreSummary is the doctor-visible projection of the core domain.
type CoreSummary struct {
	Score      float64            `json:"score"`
	Categories map[string]float64 `json:"categories"`
}

// FastCoreSummary computes the fast-mode core scorecard summary.
func FastCoreSummary(root string) (*CoreSummary, error) {
	rep, err := FastReport(root)
	if err != nil {
		return nil, err
	}
	sum := &CoreSummary{Categories: map[string]float64{}}
	for _, d := range rep.Domains {
		if !d.Gated {
			continue
		}
		sum.Score = d.Score * 100
		for _, c := range d.Categories {
			sum.Categories[c.Category] = c.Score
		}
	}
	return sum, nil
}
