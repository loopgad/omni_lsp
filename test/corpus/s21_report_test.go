package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const s21ReportPathEnv = "OMNILSP_S21_REPORT"

// s21GateEnv mirrors OMNILSP_SOAK_GATE (docs/soak-nightly.md): a release-evidence
// invocation explicitly opts in with OMNILSP_S21_GATE=required. Under the opt-in
// a missing candidate or a -short invocation is a hard failure, never a skip;
// without it the test stays an environmental skip so form-only CI invocations
// do not fail on preconditions they cannot satisfy.
const s21GateEnv = "OMNILSP_S21_GATE"

func s21GateRequired() bool {
	return strings.TrimSpace(os.Getenv(s21GateEnv)) == "required"
}

const (
	s21Passed      s21Decision = "passed"
	s21NotVerified s21Decision = "not_verified"
	s21Failed      s21Decision = "failed"
)

type s21Decision string

type s21Evidence struct {
	requiredLanguages []string
	runID             string
	candidateBinary   string
	candidateSHA256   string
	environment       s21Environment
	startedAt         time.Time
	finishedAt        time.Time
	corpusSHA256      string
	corpusHashError   string
	testedLanguage    map[string]bool
	skippedLanguages  map[string]string
	failedLanguages   map[string]string
	testedCases       []s21TestedCase
	skippedCases      []s21SkippedCase
	buckets           ErrorBuckets
	classifierFailed  bool
}

// s21Environment deliberately contains only runtime facts observed by the Go
// process producing this report. Tool versions belong to the acceptance
// harness' locked inventory; copying unobserved values here would make the
// S21 claim appear stronger than its evidence.
type s21Environment struct {
	GOOS      string `json:"goos"`
	GOARCH    string `json:"goarch"`
	GoVersion string `json:"go_version"`
}

type s21TestedCase struct {
	Language       string            `json:"language"`
	Case           string            `json:"case"`
	Operations     int               `json:"operations"`
	OperationCount s21OperationCount `json:"operation_counts"`
}

type s21SkippedCase struct {
	Language string `json:"language"`
	Case     string `json:"case"`
	Reason   string `json:"reason"`
}

type s21OperationCount struct {
	Hover      int `json:"hover"`
	Definition int `json:"definition"`
	References int `json:"references"`
	Rename     int `json:"rename"`
	Total      int `json:"total"`
}

func (c *s21OperationCount) add(operation string) {
	switch operation {
	case "hover":
		c.Hover++
	case "definition":
		c.Definition++
	case "references":
		c.References++
	case "rename":
		c.Rename++
	default:
		panic("unknown S21 operation " + operation)
	}
	c.Total++
}

type s21ErrorBucketCounts struct {
	WrongEdit               int64 `json:"wrong_edit"`
	StaleEdit               int64 `json:"stale_edit"`
	WrongFileLocation       int64 `json:"wrong_file_location"`
	PositionMappingError    int64 `json:"position_mapping_error"`
	ProtocolInvalidResponse int64 `json:"protocol_invalid_response"`
	SnapshotMixing          int64 `json:"snapshot_mixing"`
}

type s21Report struct {
	SchemaVersion   int              `json:"schema_version"`
	Kind            string           `json:"kind"`
	RunID           string           `json:"run_id"`
	CandidateBinary string           `json:"candidate_binary"`
	CandidateSHA256 string           `json:"candidate_sha256"`
	Environment     s21Environment   `json:"environment"`
	CorpusSHA256    string           `json:"corpus_sha256"`
	StartedAt       time.Time        `json:"started_at"`
	FinishedAt      time.Time        `json:"finished_at"`
	Errors          []string         `json:"errors"`
	Skips           []string         `json:"skips"`
	Decision        s21Decision      `json:"decision"`
	Checks          []s21ReportCheck `json:"checks"`
	GeneratedAt     time.Time        `json:"generated_at"`
}

type s21ReportCheck struct {
	ID       string            `json:"id"`
	Status   s21Decision       `json:"status"`
	Observed s21ReportObserved `json:"observed"`
}

type s21ReportObserved struct {
	RequiredLanguages []string             `json:"required_languages"`
	TestedLanguages   []string             `json:"tested_languages"`
	SkippedLanguages  map[string]string    `json:"skipped_languages"`
	FailedLanguages   map[string]string    `json:"failed_languages"`
	TestedCases       []s21TestedCase      `json:"tested_cases"`
	SkippedCases      []s21SkippedCase     `json:"skipped_cases"`
	OperationCounts   s21OperationCount    `json:"operation_counts"`
	ErrorBuckets      s21ErrorBucketCounts `json:"error_buckets"`
}

func newS21Evidence(required []string, runID, candidateSHA256 string) *s21Evidence {
	candidateBinary := strings.TrimSpace(os.Getenv("OMNILSP_BIN"))
	if candidateBinary != "" {
		if absolute, err := filepath.Abs(candidateBinary); err == nil {
			candidateBinary = absolute
		}
	}
	corpusSHA256, corpusHashErr := s21CorpusSHA256()
	corpusHashError := ""
	if corpusHashErr != nil {
		corpusHashError = corpusHashErr.Error()
	}
	now := time.Now().UTC()
	return &s21Evidence{
		requiredLanguages: append([]string(nil), required...),
		runID:             runID,
		candidateBinary:   candidateBinary,
		candidateSHA256:   candidateSHA256,
		environment:       s21Environment{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version()},
		startedAt:         now,
		corpusSHA256:      corpusSHA256,
		corpusHashError:   corpusHashError,
		testedLanguage:    make(map[string]bool),
		skippedLanguages:  make(map[string]string),
		failedLanguages:   make(map[string]string),
	}
}

// s21CorpusSHA256 follows the acceptance harness' corpus identity algorithm:
// each file contributes its normalized relative path and content SHA-256,
// entries are sorted by path, and the resulting UTF-8 manifest is hashed.
// Hashing the manifest keeps the report independent of absolute checkout
// paths while changing either a path or its bytes changes the identity.
func s21CorpusSHA256() (string, error) {
	root := "testdata"
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		root = filepath.Join("test", "corpus", "testdata")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve corpus root: %w", err)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("not a directory")
		}
		return "", fmt.Errorf("corpus root %q: %w", root, err)
	}

	type corpusEntry struct {
		relative string
		content  string
	}
	entries := make([]corpusEntry, 0)
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, corpusEntry{
			relative: filepath.ToSlash(relative),
			content:  hex.EncodeToString(digest[:]),
		})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("read corpus files: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].relative < entries[j].relative })
	h := sha256.New()
	for _, entry := range entries {
		if _, err := fmt.Fprintf(h, "%s %s\n", entry.relative, entry.content); err != nil {
			return "", fmt.Errorf("hash corpus manifest: %w", err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (e *s21Evidence) beginCase(language, name string) *s21TestedCase {
	e.testedCases = append(e.testedCases, s21TestedCase{Language: language, Case: name})
	return &e.testedCases[len(e.testedCases)-1]
}

func (e *s21Evidence) skipCase(language, name, reason string) {
	e.skippedCases = append(e.skippedCases, s21SkippedCase{Language: language, Case: name, Reason: reason})
}

func (e *s21Evidence) report() s21Report {
	testedLanguages := make([]string, 0, len(e.testedLanguage))
	for language, tested := range e.testedLanguage {
		if tested {
			testedLanguages = append(testedLanguages, language)
		}
	}
	sort.Strings(testedLanguages)
	requiredLanguages := append([]string(nil), e.requiredLanguages...)
	sort.Strings(requiredLanguages)
	testedCases := append([]s21TestedCase{}, e.testedCases...)
	sort.Slice(testedCases, func(i, j int) bool {
		if testedCases[i].Language == testedCases[j].Language {
			return testedCases[i].Case < testedCases[j].Case
		}
		return testedCases[i].Language < testedCases[j].Language
	})
	skippedCases := append([]s21SkippedCase{}, e.skippedCases...)
	sort.Slice(skippedCases, func(i, j int) bool {
		if skippedCases[i].Language == skippedCases[j].Language {
			return skippedCases[i].Case < skippedCases[j].Case
		}
		return skippedCases[i].Language < skippedCases[j].Language
	})

	var operations s21OperationCount
	for _, testCase := range testedCases {
		operations.Hover += testCase.OperationCount.Hover
		operations.Definition += testCase.OperationCount.Definition
		operations.References += testCase.OperationCount.References
		operations.Rename += testCase.OperationCount.Rename
		operations.Total += testCase.OperationCount.Total
	}
	buckets := s21ErrorBucketCounts{
		WrongEdit:               e.buckets.WrongEdit,
		StaleEdit:               e.buckets.StaleEdit,
		WrongFileLocation:       e.buckets.WrongFileLocation,
		PositionMappingError:    e.buckets.PositionMappingError,
		ProtocolInvalidResponse: e.buckets.ProtocolInvalidResponse,
		SnapshotMixing:          e.buckets.SnapshotMixing,
	}
	// Completion is observed after the report's evidence has been assembled.
	// The start time remains the evidence construction time so a report cannot
	// claim a completed interval without a corresponding start marker.
	e.finishedAt = time.Now().UTC()
	decision := e.decision(operations)
	checkID := e.runID + "/S21/zero-error-classification"
	return s21Report{
		SchemaVersion:   1,
		Kind:            "s21",
		RunID:           e.runID,
		CandidateBinary: e.candidateBinary,
		CandidateSHA256: e.candidateSHA256,
		Environment:     e.environment,
		CorpusSHA256:    e.corpusSHA256,
		StartedAt:       e.startedAt,
		FinishedAt:      e.finishedAt,
		Errors:          e.reportErrors(),
		Skips:           e.reportSkips(),
		Decision:        decision,
		Checks: []s21ReportCheck{{
			ID:     checkID,
			Status: decision,
			Observed: s21ReportObserved{
				RequiredLanguages: requiredLanguages,
				TestedLanguages:   testedLanguages,
				SkippedLanguages:  cloneStringMap(e.skippedLanguages),
				FailedLanguages:   cloneStringMap(e.failedLanguages),
				TestedCases:       testedCases,
				SkippedCases:      skippedCases,
				OperationCounts:   operations,
				ErrorBuckets:      buckets,
			},
		}},
		GeneratedAt: time.Now().UTC(),
	}
}

func (e *s21Evidence) decision(operations s21OperationCount) s21Decision {
	if e.buckets.Total() > 0 || e.classifierFailed || len(e.failedLanguages) > 0 {
		return s21Failed
	}
	if !e.metadataComplete() {
		return s21NotVerified
	}
	if len(e.skippedLanguages) > 0 || len(e.skippedCases) > 0 {
		return s21NotVerified
	}
	for _, language := range e.requiredLanguages {
		if !e.testedLanguage[language] {
			return s21NotVerified
		}
		caseCount := 0
		for _, testCase := range e.testedCases {
			if testCase.Language != language {
				continue
			}
			caseCount++
			if testCase.Operations != 4 || testCase.OperationCount.Total != 4 ||
				testCase.OperationCount.Hover != 1 || testCase.OperationCount.Definition != 1 ||
				testCase.OperationCount.References != 1 || testCase.OperationCount.Rename != 1 {
				return s21NotVerified
			}
		}
		if caseCount == 0 {
			return s21NotVerified
		}
	}
	if operations.Total == 0 || operations.Hover != operations.Total/4 ||
		operations.Definition != operations.Hover || operations.References != operations.Hover ||
		operations.Rename != operations.Hover || operations.Total%4 != 0 {
		return s21NotVerified
	}
	return s21Passed
}

func (e *s21Evidence) metadataComplete() bool {
	if strings.TrimSpace(e.runID) == "" || strings.TrimSpace(e.candidateBinary) == "" ||
		!filepath.IsAbs(e.candidateBinary) || !validCandidateHash(e.candidateSHA256) ||
		e.startedAt.IsZero() || e.finishedAt.IsZero() || e.finishedAt.Before(e.startedAt) ||
		strings.TrimSpace(e.environment.GOOS) == "" || strings.TrimSpace(e.environment.GOARCH) == "" ||
		strings.TrimSpace(e.environment.GoVersion) == "" || !validCandidateHash(e.corpusSHA256) ||
		strings.TrimSpace(e.corpusHashError) != "" {
		return false
	}
	return true
}

func (e *s21Evidence) reportErrors() []string {
	errors := make([]string, 0)
	if strings.TrimSpace(e.corpusHashError) != "" {
		errors = append(errors, "corpus hash: "+e.corpusHashError)
	}
	failedLanguages := make([]string, 0, len(e.failedLanguages))
	for language := range e.failedLanguages {
		failedLanguages = append(failedLanguages, language)
	}
	sort.Strings(failedLanguages)
	for _, language := range failedLanguages {
		errors = append(errors, language+": "+e.failedLanguages[language])
	}
	if e.classifierFailed {
		errors = append(errors, "classifier failure")
	}
	for _, bucket := range []struct {
		name  string
		value int64
	}{
		{name: "wrong_edit", value: e.buckets.WrongEdit},
		{name: "stale_edit", value: e.buckets.StaleEdit},
		{name: "wrong_file_location", value: e.buckets.WrongFileLocation},
		{name: "position_mapping_error", value: e.buckets.PositionMappingError},
		{name: "protocol_invalid_response", value: e.buckets.ProtocolInvalidResponse},
		{name: "snapshot_mixing", value: e.buckets.SnapshotMixing},
	} {
		if bucket.value > 0 {
			errors = append(errors, fmt.Sprintf("error bucket %s: %d", bucket.name, bucket.value))
		}
	}
	return errors
}

func (e *s21Evidence) reportSkips() []string {
	skips := make([]string, 0, len(e.skippedLanguages)+len(e.skippedCases))
	languages := make([]string, 0, len(e.skippedLanguages))
	for language := range e.skippedLanguages {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	for _, language := range languages {
		skips = append(skips, language+": "+e.skippedLanguages[language])
	}
	cases := append([]s21SkippedCase{}, e.skippedCases...)
	sort.Slice(cases, func(i, j int) bool {
		if cases[i].Language == cases[j].Language {
			return cases[i].Case < cases[j].Case
		}
		return cases[i].Language < cases[j].Language
	})
	for _, testCase := range cases {
		skips = append(skips, fmt.Sprintf("%s/%s: %s", testCase.Language, testCase.Case, testCase.Reason))
	}
	return skips
}

func TestS21CorpusHashIsStableAndComplete(t *testing.T) {
	first, err := s21CorpusSHA256()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s21CorpusSHA256()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("corpus hash changed between identical reads: %q != %q", first, second)
	}
	if !validCandidateHash(first) {
		t.Fatalf("corpus hash is not a lowercase SHA-256 digest: %q", first)
	}
}

func validCandidateHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return map[string]string{}
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func writeS21Report(path string, report s21Report) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("S21 report path is empty")
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encode S21 report: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create S21 report directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".s21-report-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary S21 report: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary S21 report: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary S21 report: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary S21 report: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish S21 report: %w", err)
	}
	return nil
}

func TestS21EvidenceDecisionRequiresAttributableCompleteCoverage(t *testing.T) {
	newPassingEvidence := func() *s21Evidence {
		evidence := newS21Evidence([]string{"rust", "python", "typescript"}, "run-123", strings.Repeat("a", sha256.Size*2))
		candidatePath, err := filepath.Abs(filepath.Join("testdata", "candidate"))
		if err != nil {
			t.Fatal(err)
		}
		evidence.candidateBinary = candidatePath
		evidence.startedAt = time.Now().UTC().Add(-time.Second)
		evidence.environment = s21Environment{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version()}
		evidence.corpusSHA256 = strings.Repeat("b", sha256.Size*2)
		evidence.corpusHashError = ""
		for _, language := range evidence.requiredLanguages {
			evidence.testedLanguage[language] = true
			caseEvidence := evidence.beginCase(language, "basic")
			for _, operation := range []string{"hover", "definition", "references", "rename"} {
				caseEvidence.OperationCount.add(operation)
				caseEvidence.Operations++
			}
		}
		return evidence
	}

	t.Run("zero_error_complete_coverage_passes", func(t *testing.T) {
		evidence := newPassingEvidence()
		report := evidence.report()
		if got := report.Decision; got != s21Passed {
			t.Fatalf("decision = %q, want %q", got, s21Passed)
		}
		if report.SchemaVersion != 1 || report.Kind != "s21" || len(report.Checks) != 1 ||
			report.Checks[0].ID != "run-123/S21/zero-error-classification" || report.Checks[0].Status != s21Passed {
			t.Fatalf("unexpected S21 report contract: %+v", report)
		}
		if report.CandidateBinary == "" || !filepath.IsAbs(report.CandidateBinary) ||
			report.CandidateSHA256 == "" || !validCandidateHash(report.CandidateSHA256) ||
			report.CorpusSHA256 == "" || !validCandidateHash(report.CorpusSHA256) ||
			report.Environment.GOOS != runtime.GOOS || report.Environment.GOARCH != runtime.GOARCH ||
			report.Environment.GoVersion == "" || report.StartedAt.IsZero() || report.FinishedAt.IsZero() ||
			report.FinishedAt.Before(report.StartedAt) || report.StartedAt.Location() != time.UTC ||
			report.FinishedAt.Location() != time.UTC {
			t.Fatalf("incomplete S21 metadata: %+v", report)
		}
		var decoded map[string]any
		reportPath := filepath.Join(t.TempDir(), "s21.json")
		if err := writeS21Report(reportPath, report); err != nil {
			t.Fatal(err)
		}
		encoded, err := os.ReadFile(reportPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		checks, ok := decoded["checks"].([]any)
		if !ok || len(checks) != 1 {
			t.Fatalf("checks were not encoded: %v", decoded["checks"])
		}
		check, ok := checks[0].(map[string]any)
		if !ok {
			t.Fatalf("check was not an object: %T", checks[0])
		}
		observed, ok := check["observed"].(map[string]any)
		if !ok {
			t.Fatalf("observed was not an object: %T", check["observed"])
		}
		for _, key := range []string{"required_languages", "tested_languages", "skipped_languages", "tested_cases", "operation_counts", "error_buckets"} {
			if _, exists := observed[key]; !exists {
				t.Errorf("report is missing observed.%s", key)
			}
		}
		for _, key := range []string{"candidate_binary", "candidate_sha256", "environment", "corpus_sha256", "started_at", "finished_at", "errors", "skips"} {
			if _, exists := decoded[key]; !exists {
				t.Errorf("report is missing top-level %s", key)
			}
		}
		for _, key := range []string{"errors", "skips"} {
			values, ok := decoded[key].([]any)
			if !ok || values == nil || len(values) != 0 {
				t.Errorf("report top-level %s = %#v, want an explicit empty array", key, decoded[key])
			}
		}
	})

	t.Run("nonzero_bucket_fails", func(t *testing.T) {
		evidence := newPassingEvidence()
		evidence.buckets.WrongEdit = 1
		if got := evidence.report().Decision; got != s21Failed {
			t.Fatalf("decision = %q, want %q", got, s21Failed)
		}
	})

	t.Run("skipped_language_is_not_verified", func(t *testing.T) {
		evidence := newPassingEvidence()
		delete(evidence.testedLanguage, "python")
		evidence.skippedLanguages["python"] = "toolchain not installed"
		if got := evidence.report().Decision; got != s21NotVerified {
			t.Fatalf("decision = %q, want %q", got, s21NotVerified)
		}
	})

	t.Run("missing_identity_or_operation_is_not_verified", func(t *testing.T) {
		evidence := newPassingEvidence()
		evidence.candidateSHA256 = ""
		if got := evidence.report().Decision; got != s21NotVerified {
			t.Fatalf("missing candidate hash decision = %q, want %q", got, s21NotVerified)
		}
		evidence = newPassingEvidence()
		evidence.testedCases[0].Operations = 3
		evidence.testedCases[0].OperationCount.Rename = 0
		evidence.testedCases[0].OperationCount.Total = 3
		if got := evidence.report().Decision; got != s21NotVerified {
			t.Fatalf("incomplete operation decision = %q, want %q", got, s21NotVerified)
		}
	})

	t.Run("missing_report_metadata_is_not_verified", func(t *testing.T) {
		for name, clear := range map[string]func(*s21Evidence){
			"candidate path":  func(e *s21Evidence) { e.candidateBinary = "" },
			"environment":     func(e *s21Evidence) { e.environment.GoVersion = "" },
			"corpus hash":     func(e *s21Evidence) { e.corpusSHA256 = "" },
			"start timestamp": func(e *s21Evidence) { e.startedAt = time.Time{} },
		} {
			t.Run(name, func(t *testing.T) {
				evidence := newPassingEvidence()
				clear(evidence)
				if got := evidence.report().Decision; got != s21NotVerified {
					t.Fatalf("missing %s decision = %q, want %q", name, got, s21NotVerified)
				}
			})
		}
	})
}
