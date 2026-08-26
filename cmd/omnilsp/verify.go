package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/omnilsp/omni/internal/conformance"
)

// defaultScoreFloor mirrors the committed regression baseline
// (test/conformance/testdata/baseline.json) so the CLI can gate standalone.
// defaultScoreFloor mirrors the committed regression baseline
// (test/conformance/testdata/baseline.json) so the CLI can gate standalone.
const defaultScoreFloor = 90.0

// verifyReport is the versioned machine shape for `omnilsp verify`.
type verifyReport struct {
	Schema      string              `json:"schema"`
	Mode        string              `json:"mode"`
	Floor       float64             `json:"floor"`
	Passed      bool                `json:"passed"`
	Conformance *conformance.Report `json:"conformance"`
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON (schema omnilsp.verify.v1)")
	full := fs.Bool("full", false, "execute AUTO probes and GATE commands (slow)")
	minScore := fs.Float64("min", defaultScoreFloor, "minimum core score to pass")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var (
		rep *conformance.Report
		err error
	)
	if *full {
		rep, err = conformance.FullReport("", "600s")
	} else {
		rep, err = conformance.FastReport("")
	}
	if err != nil {
		return err
	}

	passed := rep.CoreScore >= *minScore

	if *jsonOut {
		out := verifyReport{
			Schema:      "omnilsp.verify.v1",
			Mode:        rep.Mode,
			Floor:       *minScore,
			Passed:      passed,
			Conformance: rep,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		fmt.Print(conformance.RenderText(rep))
		fmt.Printf("\nfloor %.1f%%: ", *minScore)
		if passed {
			fmt.Println("PASS")
		} else {
			fmt.Println("FAIL — score below floor")
		}
	}

	if !passed {
		// W1 extension (ADR-0005): verification failure exits 1 until first
		// stable release fixes the category permanently.
		os.Exit(1)
	}
	return nil
}
