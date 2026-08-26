package corpus

// §S6 triage tests (goal.md 行 4142-4156).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestS6_ClassifyFourBuckets(t *testing.T) {
	cases := []struct {
		name     string
		keywords []string
		want     Classification
	}{
		{"upstream", []string{"crash inside gopls"}, ClassUpstreamBug},
		{"upstream-pyright", []string{"pyright disagrees"}, ClassUpstreamBug},
		{"upstream-tsserver", []string{"tsserver timeout"}, ClassUpstreamBug},
		{"upstream-rust-analyzer", []string{"rust-analyzer panic"}, ClassUpstreamBug},
		{"build-context-version", []string{"toolchain version drift"}, ClassBuildContext},
		{"build-context-build", []string{"build context differs"}, ClassBuildContext},
		{"semantic", []string{"behavior is by design"}, ClassSemanticDiff},
		{"semantic-documented", []string{"documented deviation"}, ClassSemanticDiff},
		{"default-omnilsp", []string{"wrong edit location"}, ClassOmnilspBug},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.keywords); got != tc.want {
				t.Fatalf("Classify(%v) = %q, want %q", tc.keywords, got, tc.want)
			}
		})
	}
}

func TestS6_ReportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := NewMismatchReport("go", "test/corpus/testdata/minimal/definition.go")
	r.Symptom = "definition target differs from gopls"
	r.Expected = "target at decl.go:10"
	r.Actual = "no result"
	r.Classification = ClassUpstreamBug
	r.RegressionTest = "TestS6_ClassifyFourBuckets"

	path, err := r.WriteTo(dir)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(path), "triage-go-") {
		t.Fatalf("unexpected filename %q", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var got MismatchReport
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Schema != SchemaVersion {
		t.Errorf("schema = %q, want %q", got.Schema, SchemaVersion)
	}
	if got.Backend != r.Backend || got.FixtureRef != r.FixtureRef ||
		got.Classification != r.Classification || got.RegressionTest != r.RegressionTest {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, *r)
	}
	if got.Env.OS == "" || got.Env.GoVersion == "" || got.Env.TimestampUTC == "" {
		t.Errorf("env snapshot incomplete: %+v", got.Env)
	}
}

func TestS6_UnexplainedMismatchRejected(t *testing.T) {
	r := NewMismatchReport("python", "testdata/minimal/hover.py")
	r.Classification = ClassOmnilspBug
	if err := r.Validate(); err == nil {
		t.Fatal("expected Validate to reject report with neither regression test nor waiver")
	}
}

func TestS6_WaiverRequiresReference(t *testing.T) {
	waived := NewMismatchReport("typescript", "testdata/minimal/completions.ts")
	waived.Classification = ClassSemanticDiff
	waived.WaivedByADR = "docs/adr/0007-semantic-diff.md"
	if err := waived.Validate(); err != nil {
		t.Fatalf("waived report should validate: %v", err)
	}

	unwaivered := NewMismatchReport("rust", "testdata/minimal/rename.rs")
	unwaivered.Classification = ClassSemanticDiff
	if err := unwaivered.Validate(); err == nil {
		t.Fatal("waiver without reference must not count as explanation")
	}
}
