package perf

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	cclsbackend "github.com/omnilsp/omni/internal/languages/ccls"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/test/acceptance/lspdriver"
	acceptreport "github.com/omnilsp/omni/test/acceptance/report"
	"github.com/omnilsp/omni/test/acceptance/toolversion"
	"github.com/omnilsp/omni/test/acceptance/upstream"
)

const (
	acceptanceModeEnv             = "OMNILSP_ACCEPTANCE"
	acceptanceBinEnv              = "OMNILSP_BIN"
	candidateSHAEnv               = "OMNILSP_CANDIDATE_SHA256"
	performanceOutEnv             = "OMNILSP_PERF_REPORT"
	acceptanceRunIDEnv            = "OMNILSP_RUN_ID"
	e2eS18PolicyEnv               = "OMNILSP_S18_POLICY"
	e2eS18ModeEnv                 = "OMNILSP_S18_MODE"
	e2eS18StableModeEnv           = "OMNILSP_S18_STABLE_MODE"
	e2eS18PhaseTimingEnv          = "OMNILSP_S18_PHASE_TIMING"
	e2eS18CompletionPhaseTraceEnv = "OMNILSP_S18_COMPLETION_PHASE_TRACE"
	e2eS18FilterEnv               = "OMNILSP_S18_FILTER"
	performanceSamples            = 1000
	e2eS18UpstreamReadyTimeout    = 45 * time.Second
	e2eS18ReadyStableCount        = 3
	e2eS18ReadyPollInterval       = 200 * time.Millisecond
)

const (
	e2eS18StrictPolicyID = "s18-strict-v1"
	e2eS18StablePolicyID = "s18-evidence-qualified-v2"
)

var e2eS18PolicyFlag = flag.String("S18Policy", "strict", "S18 release policy: strict or stable")

var e2eS19ReturnedLocationScales = [...]int{200, 800, 3200}

var errE2ENotVerified = errors.New("not verified")

// e2eFailureKind is deliberately independent of the human-readable error
// text. The release exception may only consume a typed latency failure; all
// other kinds remain hard acceptance failures.
type e2eFailureKind string

const (
	e2eFailureLatency       e2eFailureKind = "latency"
	e2eFailureSemantic      e2eFailureKind = "semantic"
	e2eFailureSampleMissing e2eFailureKind = "sample_missing"
	e2eFailureProtocol      e2eFailureKind = "protocol"
	e2eFailureProcess       e2eFailureKind = "process"
	e2eFailureTool          e2eFailureKind = "tool"
	e2eFailureIncomplete    e2eFailureKind = "incomplete"
)

type e2eTypedFailure struct {
	kind e2eFailureKind
	err  error
}

func (f *e2eTypedFailure) Error() string {
	if f == nil || f.err == nil {
		return string(f.kind)
	}
	return f.err.Error()
}

func (f *e2eTypedFailure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.err
}

func e2eWrapFailure(kind e2eFailureKind, err error) error {
	if err == nil {
		return nil
	}
	var typed *e2eTypedFailure
	if errors.As(err, &typed) {
		return err
	}
	return &e2eTypedFailure{kind: kind, err: err}
}

func e2eFailureKindOf(err error) e2eFailureKind {
	if err == nil {
		return ""
	}
	var typed *e2eTypedFailure
	if errors.As(err, &typed) && typed != nil {
		return typed.kind
	}
	var responseErr *jsonrpc.ResponseError
	if errors.As(err, &responseErr) {
		return e2eFailureProtocol
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return e2eFailureProcess
	}
	return e2eFailureProcess
}

func e2eS18Policy() string {
	for _, value := range []string{os.Getenv(e2eS18PolicyEnv), os.Getenv(e2eS18ModeEnv), os.Getenv(e2eS18StableModeEnv)} {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			if value == "1" || value == "true" || value == "stable" || value == e2eS18StablePolicyID {
				return "stable"
			}
			return "strict"
		}
	}
	value := strings.ToLower(strings.TrimSpace(*e2eS18PolicyFlag))
	if value == "stable" || value == "1" || value == "true" || value == e2eS18StablePolicyID {
		return "stable"
	}
	return "strict"
}

func e2eS18StableModeEnabled() bool { return e2eS18Policy() == "stable" }

func e2eS18PolicyID() string {
	if e2eS18StableModeEnabled() {
		return e2eS18StablePolicyID
	}
	return e2eS18StrictPolicyID
}

type e2ePerformanceReport struct {
	Schema            string                          `json:"schema"`
	RunID             string                          `json:"runId"`
	CreatedUTC        time.Time                       `json:"createdUtc"`
	CandidateBin      string                          `json:"candidateBinary"`
	CandidateSHA      string                          `json:"candidateSha256"`
	GitRevision       string                          `json:"gitRevision"`
	CorpusSHA         string                          `json:"corpusSha256"`
	CorpusContentSHA  string                          `json:"corpusContentSha256"`
	CorpusFiles       int                             `json:"corpusFiles"`
	ToolVersions      map[string]string               `json:"toolVersions"`
	Environment       map[string]string               `json:"environment"`
	S18               e2eS18Report                    `json:"s18"`
	S19               e2eS19Report                    `json:"s19"`
	Decision          string                          `json:"decision"`
	ReleaseAssessment *acceptreport.ReleaseAssessment `json:"release_assessment"`
	ReportPath        string                          `json:"reportPath,omitempty"`
}

type e2eS18Report struct {
	Status       string                               `json:"status"`
	Thresholds   map[string]e2eThresholds             `json:"strictThresholds"`
	Operations   map[string]e2eSampleSet              `json:"operations"`
	ABBAEvidence map[string]acceptreport.ABBAEvidence `json:"abbaEvidence,omitempty"`
	FailureTypes []string                             `json:"failureTypes,omitempty"`
	Errors       []string                             `json:"errors,omitempty"`
	NotVerified  []string                             `json:"notVerified,omitempty"`
}

type e2eThresholds struct {
	P50Millis int64 `json:"p50Millis"`
	P95Millis int64 `json:"p95Millis"`
	P99Millis int64 `json:"p99Millis"`
}

var (
	e2eS18StableCompletionThreshold = e2eThresholds{P50Millis: 80, P95Millis: 100, P99Millis: 125}
	e2eS18StableSyntaxThreshold     = e2eThresholds{P50Millis: 60, P95Millis: 100, P99Millis: 150}
)

const (
	e2eS18ABBARounds          = 3
	e2eS18ABBALegsPerRound    = 4
	e2eS18ABBASamplesPerLeg   = performanceSamples
	e2eS18ABBARequiredSamples = e2eS18ABBARounds * 2 * e2eS18ABBASamplesPerLeg
)

func e2eRecordS18Failure(sampleSet *e2eSampleSet, status string, kind e2eFailureKind, err error) {
	if sampleSet == nil {
		return
	}
	sampleSet.Status = status
	if kind != "" {
		kindString := string(kind)
		seen := false
		for _, existing := range sampleSet.FailureTypes {
			if existing == kindString {
				seen = true
				break
			}
		}
		if !seen {
			sampleSet.FailureTypes = append(sampleSet.FailureTypes, kindString)
		}
		if sampleSet.FailureType == "" {
			sampleSet.FailureType = kindString
		} else if sampleSet.FailureType != kindString {
			sampleSet.FailureType = "multiple"
		}
	}
	if err != nil {
		sampleSet.Errors = append(sampleSet.Errors, err.Error())
	}
}

func e2eSampleFailureType(sampleSet e2eSampleSet) string {
	if sampleSet.FailureType != "" {
		return sampleSet.FailureType
	}
	if len(sampleSet.FailureTypes) == 1 {
		return sampleSet.FailureTypes[0]
	}
	if len(sampleSet.FailureTypes) > 1 {
		return "multiple"
	}
	return ""
}

func e2eS18ExceptionThreshold(fixtureID, operation string) (e2eThresholds, bool) {
	if fixtureID != "c" && fixtureID != "cpp" {
		return e2eThresholds{}, false
	}
	switch operation {
	case "completion_first_usable":
		return e2eS18StableCompletionThreshold, true
	case "syntax_update_after_edit":
		return e2eS18StableSyntaxThreshold, true
	default:
		return e2eThresholds{}, false
	}
}

func e2eS18ExceptionCheckIDs(runID string) []string {
	ids := make([]string, 0, 4)
	for _, fixtureID := range []string{"c", "cpp"} {
		ids = append(ids, runID+"/S18/"+fixtureID+"/completion_first_usable")
		ids = append(ids, runID+"/S18/"+fixtureID+"/syntax_update_after_edit")
	}
	return ids
}

func e2eS18StrictLatencyExceeded(sampleSet e2eSampleSet) bool {
	threshold := sampleSet.Thresholds
	return sampleSet.P50NS > threshold.P50Millis*int64(time.Millisecond) ||
		sampleSet.P95NS > threshold.P95Millis*int64(time.Millisecond) ||
		sampleSet.P99NS > threshold.P99Millis*int64(time.Millisecond)
}

func e2eS18DifferentialEvidencePassed(sampleSet e2eSampleSet) bool {
	evidence := sampleSet.SemanticEvidence
	return evidence != nil && evidence.CandidateObservationStatus == "passed" &&
		evidence.CandidateValidationStatus == "passed" && evidence.UpstreamObservationStatus == "passed" &&
		evidence.UpstreamValidationStatus == "passed" && evidence.DifferentialMatched &&
		evidence.DifferentialStatus == "passed"
}

func e2eS18CompletionSemanticEligible(sampleSet e2eSampleSet) bool {
	evidence := sampleSet.SemanticEvidence
	if sampleSet.Method != "textDocument/completion" || !e2eS18DifferentialEvidencePassed(sampleSet) || evidence.TargetSampleStatus != "passed" {
		return false
	}
	return evidence.FullListSnapshotStatus == "passed" && evidence.FullListSampleStatus == "passed"
}

func e2eS18StrictCompletionSamplesComplete(sampleSet e2eSampleSet) bool {
	if sampleSet.SampleCount != performanceSamples || len(sampleSet.SamplesNS) != performanceSamples ||
		sampleSet.SemanticEvidence == nil || sampleSet.SemanticEvidence.FullListSnapshotStatus != "pending" ||
		sampleSet.SemanticEvidence.FullListSampleStatus != "pending" || sampleSet.SemanticEvidence.TargetSampleStatus != "pending" {
		return false
	}
	if sampleSet.Status == "pass" {
		return sampleSet.FailureType == "" && len(sampleSet.FailureTypes) == 0
	}
	return sampleSet.Status == "failed" && e2eSampleFailureType(sampleSet) == string(e2eFailureLatency) &&
		len(sampleSet.FailureTypes) == 1 && sampleSet.FailureTypes[0] == string(e2eFailureLatency)
}

func e2eS18ABBACompletionSamplesSemanticallyComplete(sampleSet e2eSampleSet) bool {
	if sampleSet.Method != "textDocument/completion" || sampleSet.SampleCount != e2eS18ABBARequiredSamples ||
		len(sampleSet.SamplesNS) != e2eS18ABBARequiredSamples {
		return false
	}
	if sampleSet.Status == "pass" {
		return sampleSet.FailureType == "" && len(sampleSet.FailureTypes) == 0
	}
	return sampleSet.Status == "failed" && e2eSampleFailureType(sampleSet) == string(e2eFailureLatency) &&
		len(sampleSet.FailureTypes) == 1 && sampleSet.FailureTypes[0] == string(e2eFailureLatency)
}

func e2eS18RequiresCompletionFullList(fixtureID, method string) bool {
	return method == "textDocument/completion" && (fixtureID == "c" || fixtureID == "cpp")
}

func e2eS18SyntaxSemanticEligible(sampleSet e2eSampleSet) bool {
	editUpdate := e2eS18EditUpdateObserved(sampleSet.Method, sampleSet.EditUpdate)
	return sampleSet.Method == "textDocument/documentSymbol" && e2eS18DifferentialEvidencePassed(sampleSet) &&
		editUpdate["status"] == "passed"
}

func e2eS18SemanticEligible(operation string, sampleSet e2eSampleSet) bool {
	switch operation {
	case "completion_first_usable":
		return e2eS18CompletionSemanticEligible(sampleSet)
	case "syntax_update_after_edit":
		return e2eS18SyntaxSemanticEligible(sampleSet)
	default:
		return false
	}
}

func e2eS18StableSampleCount(operationID string) int {
	if strings.HasSuffix(operationID, "/completion_first_usable") || strings.HasSuffix(operationID, "/syntax_update_after_edit") {
		return e2eS18ABBARequiredSamples
	}
	return performanceSamples
}

func e2eS18StableThresholdForID(operationID string) (e2eThresholds, bool) {
	parts := strings.Split(operationID, "/")
	if len(parts) < 4 || parts[len(parts)-3] != "S18" {
		return e2eThresholds{}, false
	}
	return e2eS18ExceptionThreshold(parts[len(parts)-2], parts[len(parts)-1])
}

func e2eS18ABBAEvidenceComplete(operationID string, sampleSet e2eSampleSet, evidence acceptreport.ABBAEvidence) bool {
	_, inScope := e2eS18StableThresholdForID(operationID)
	if !inScope || evidence.CheckID != operationID || evidence.Method != sampleSet.Method ||
		sampleSet.SampleCount != e2eS18ABBARequiredSamples || len(sampleSet.SamplesNS) != e2eS18ABBARequiredSamples ||
		len(evidence.Rounds) != e2eS18ABBARounds {
		return false
	}
	parts := strings.Split(operationID, "/")
	if len(parts) < 4 || evidence.FixtureID != parts[len(parts)-2] || evidence.Operation != parts[len(parts)-1] {
		return false
	}
	if (evidence.Operation == "completion_first_usable" && evidence.Method != "textDocument/completion") ||
		(evidence.Operation == "syntax_update_after_edit" && evidence.Method != "textDocument/documentSymbol") {
		return false
	}
	if !e2eS18SemanticEligible(evidence.Operation, sampleSet) {
		return false
	}

	const perRequest = e2eS18ABBASamplesPerLeg
	wantOrder := [...]string{"candidate", "upstream", "upstream", "candidate"}
	var candidate, upstream, overhead []int64
	seenRequestIDs := make(map[string]struct{}, e2eS18ABBARequiredSamples)
	nextSampleStart := 0
	for roundIndex, round := range evidence.Rounds {
		if round.Index != roundIndex || len(round.Order) != e2eS18ABBALegsPerRound || len(round.Legs) != e2eS18ABBALegsPerRound {
			return false
		}
		for position := 0; position < e2eS18ABBALegsPerRound; position++ {
			if round.Order[position] != wantOrder[position] {
				return false
			}
			leg := round.Legs[position]
			if leg.Position != position || leg.Role != wantOrder[position] {
				return false
			}
			if leg.Role == "candidate" {
				if leg.SampleStart != nextSampleStart || leg.SampleCount != perRequest ||
					len(leg.RequestIDs) != perRequest || len(leg.WriteRawNS) != perRequest || len(leg.WaitRawNS) != perRequest ||
					len(leg.RawNS) != 0 {
					return false
				}
				for i, id := range leg.RequestIDs {
					parsed, err := strconv.ParseInt(id, 10, 64)
					if err != nil || parsed <= 0 {
						return false
					}
					if _, duplicate := seenRequestIDs[id]; duplicate {
						return false
					}
					seenRequestIDs[id] = struct{}{}
					outer := sampleSet.SamplesNS[leg.SampleStart+i]
					writeValue, waitValue := leg.WriteRawNS[i], leg.WaitRawNS[i]
					if outer <= 0 || !e2eValidABBAPhaseNS(writeValue) || !e2eValidABBAPhaseNS(waitValue) ||
						float64(outer) < writeValue || float64(outer)-writeValue < waitValue {
						return false
					}
					overhead = append(overhead, int64(float64(outer)-writeValue-waitValue))
				}
				candidate = append(candidate, sampleSet.SamplesNS[leg.SampleStart:leg.SampleStart+leg.SampleCount]...)
				nextSampleStart += leg.SampleCount
			} else {
				if leg.SampleStart != 0 || leg.SampleCount != 0 || len(leg.RequestIDs) != 0 ||
					len(leg.WriteRawNS) != 0 || len(leg.WaitRawNS) != 0 || len(leg.RawNS) != perRequest {
					return false
				}
				for _, value := range leg.RawNS {
					if !e2eValidABBAPhaseNS(value) || value <= 0 {
						return false
					}
					upstream = append(upstream, int64(value))
				}
			}
		}
	}
	if nextSampleStart != len(sampleSet.SamplesNS) || len(candidate) != e2eS18ABBARequiredSamples ||
		len(upstream) != e2eS18ABBARequiredSamples || len(overhead) != e2eS18ABBARequiredSamples ||
		len(seenRequestIDs) != e2eS18ABBARequiredSamples {
		return false
	}
	if sampleSet.P50NS != e2ePercentileNS(candidate, .50) || sampleSet.P95NS != e2ePercentileNS(candidate, .95) ||
		sampleSet.P99NS != e2ePercentileNS(candidate, .99) {
		return false
	}
	for i, q := range []float64{.50, .95, .99} {
		overheadNS := e2ePercentileNS(overhead, q)
		budgetNS := []int64{5, 10, 20}[i] * int64(time.Millisecond)
		if overheadNS > budgetNS {
			return false
		}
	}
	return true
}

func e2eValidABBAPhaseNS(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value < math.Exp2(63) && math.Trunc(value) == value
}

func e2eS18ABBAExceptionEligible(operationID string, sampleSet e2eSampleSet, evidence acceptreport.ABBAEvidence) bool {
	threshold, inScope := e2eS18StableThresholdForID(operationID)
	if !inScope || !e2eS18ABBAEvidenceComplete(operationID, sampleSet, evidence) {
		return false
	}
	var upstream []int64
	for _, round := range evidence.Rounds {
		for _, leg := range round.Legs {
			if leg.Role != "upstream" {
				continue
			}
			for _, value := range leg.RawNS {
				upstream = append(upstream, int64(value))
			}
		}
	}
	strictOverage := false
	for i, q := range []float64{.50, .95, .99} {
		candidateNS := e2ePercentileNS(sampleSet.SamplesNS, q)
		upstreamNS := e2ePercentileNS(upstream, q)
		strictNS := []int64{sampleSet.Thresholds.P50Millis, sampleSet.Thresholds.P95Millis, sampleSet.Thresholds.P99Millis}[i] * int64(time.Millisecond)
		budgetNS := []int64{5, 10, 20}[i] * int64(time.Millisecond)
		stableNS := []int64{threshold.P50Millis, threshold.P95Millis, threshold.P99Millis}[i] * int64(time.Millisecond)
		if candidateNS > strictNS {
			if upstreamNS <= strictNS || candidateNS > upstreamNS+budgetNS || candidateNS > stableNS {
				return false
			}
			strictOverage = true
		}
	}
	return strictOverage
}

// e2eS18LatencyExceptionEligible validates the complete, run-qualified
// boundary for an evidence-backed stable-policy exception. Strict sample
// status and the original failure classification remain authoritative.
func e2eS18LatencyExceptionEligible(runID, operationID string, sampleSet e2eSampleSet, evidence acceptreport.ABBAEvidence) bool {
	allowed := false
	for _, id := range e2eS18ExceptionCheckIDs(runID) {
		if operationID == id {
			allowed = true
			break
		}
	}
	if !allowed || sampleSet.Status != "failed" || e2eSampleFailureType(sampleSet) != string(e2eFailureLatency) ||
		len(sampleSet.FailureTypes) != 1 || sampleSet.FailureTypes[0] != string(e2eFailureLatency) ||
		sampleSet.SampleCount != e2eS18StableSampleCount(operationID) || len(sampleSet.SamplesNS) != e2eS18StableSampleCount(operationID) ||
		!e2eS18StrictLatencyExceeded(sampleSet) ||
		!e2eS18ABBAExceptionEligible(operationID, sampleSet, evidence) {
		return false
	}
	return true
}

func e2eS18ExceptionSet(report e2ePerformanceReport) []string {
	if !e2eS18StableModeEnabled() {
		return nil
	}
	allowedIDs := make(map[string]struct{}, 4)
	for _, id := range e2eS18ExceptionCheckIDs(report.RunID) {
		allowedIDs[id] = struct{}{}
	}
	exceptions := make([]string, 0, len(allowedIDs))
	for id, sampleSet := range report.S18.Operations {
		switch sampleSet.Status {
		case "pass":
			if len(sampleSet.FailureTypes) != 0 || sampleSet.FailureType != "" {
				return nil
			}
		case "failed":
			evidence, hasEvidence := report.S18.ABBAEvidence[id]
			if _, ok := allowedIDs[id]; !ok || !hasEvidence || !e2eS18LatencyExceptionEligible(report.RunID, id, sampleSet, evidence) {
				return nil
			}
			exceptions = append(exceptions, id)
		default:
			return nil
		}
	}
	if len(exceptions) == 0 || report.S19.Status != "pass" {
		return nil
	}
	for _, kind := range report.S18.FailureTypes {
		if kind != string(e2eFailureLatency) {
			return nil
		}
	}
	sort.Strings(exceptions)
	return exceptions
}

// e2eS18BoundedExceptionSet records eligible C/C++ latency rows even when a
// different strict failure keeps the overall release decision failed. The
// rows are evidence candidates only; e2eS18ExceptionSet remains the stricter
// all-gates predicate used for passed_with_performance_exception.
func e2eS18BoundedExceptionSet(report e2ePerformanceReport) []string {
	if !e2eS18StableModeEnabled() || !e2eS18ExecutionComplete(report) {
		return nil
	}
	bounded := make([]string, 0, 4)
	for _, id := range e2eS18ExceptionCheckIDs(report.RunID) {
		sampleSet, ok := report.S18.Operations[id]
		evidence, hasEvidence := report.S18.ABBAEvidence[id]
		if ok && hasEvidence && e2eS18LatencyExceptionEligible(report.RunID, id, sampleSet, evidence) {
			bounded = append(bounded, id)
		}
	}
	sort.Strings(bounded)
	return bounded
}

func e2eS18ExpectedOperationIDs(runID string) map[string]struct{} {
	const operationsPerFixture = 4
	fixtureIDs := [...]string{"go", "c", "cpp", "rust", "python", "typescript", "javascript"}
	operationNames := [...]string{"hot_hover", "hot_definition", "completion_first_usable", "syntax_update_after_edit"}
	expected := make(map[string]struct{}, len(fixtureIDs)*operationsPerFixture)
	for _, fixtureID := range fixtureIDs {
		for _, operation := range operationNames {
			expected[runID+"/S18/"+fixtureID+"/"+operation] = struct{}{}
		}
	}
	return expected
}

func e2eS18ExecutionComplete(report e2ePerformanceReport) bool {
	if (report.S18.Status != "pass" && report.S18.Status != "failed") ||
		len(report.S18.Operations) != len(e2eS18ExpectedOperationIDs(report.RunID)) ||
		(report.S19.Status != "pass" && report.S19.Status != "failed") ||
		report.Decision != e2eOverallDecision(report.S18.Status, report.S19.Status) {
		return false
	}
	for id := range e2eS18ExpectedOperationIDs(report.RunID) {
		if _, ok := report.S18.Operations[id]; !ok {
			return false
		}
	}
	for id, sampleSet := range report.S18.Operations {
		wantSamples := performanceSamples
		if e2eS18StableModeEnabled() {
			if _, scoped := e2eS18StableThresholdForID(id); scoped {
				wantSamples = e2eS18ABBARequiredSamples
			}
		}
		if sampleSet.SampleCount != wantSamples || len(sampleSet.SamplesNS) != wantSamples ||
			(sampleSet.Status != "pass" && sampleSet.Status != "failed") {
			return false
		}
		if sampleSet.Status == "pass" && (sampleSet.FailureType != "" || len(sampleSet.FailureTypes) != 0) {
			return false
		}
		parts := strings.Split(id, "/")
		if len(parts) >= 4 && parts[len(parts)-3] == "S18" &&
			e2eS18RequiresCompletionFullList(parts[len(parts)-2], sampleSet.Method) &&
			!e2eS18CompletionSemanticEligible(sampleSet) {
			return false
		}
	}
	if e2eS18StableModeEnabled() {
		if len(report.S18.ABBAEvidence) != len(e2eS18ExceptionCheckIDs(report.RunID)) {
			return false
		}
		for _, id := range e2eS18ExceptionCheckIDs(report.RunID) {
			sample, ok := report.S18.Operations[id]
			evidence, evidenceOK := report.S18.ABBAEvidence[id]
			if !ok || !evidenceOK || !e2eS18ABBAEvidenceComplete(id, sample, evidence) {
				return false
			}
		}
	}
	if len(report.S19.Scaling) != len(e2eS19ReturnedLocationScales) || len(report.S19.Resources) != 5 {
		return false
	}
	for _, scale := range report.S19.Scaling {
		if len(scale.SamplesNS) != 3 || (scale.Status != "pass" && scale.Status != "failed") {
			return false
		}
	}
	return report.S19.Cancellation.Attempted && report.S19.Progress.Status != "not_verified" &&
		report.S19.NoStarvation.Status != "not_verified"
}

func e2eReleaseAssessment(report e2ePerformanceReport) *acceptreport.ReleaseAssessment {
	assessment := &acceptreport.ReleaseAssessment{
		PolicyID:                 e2eS18PolicyID(),
		ExecutionStatus:          "incomplete",
		Decision:                 acceptreport.ReleaseNotVerified,
		BoundedExceptionCheckIDs: []string{},
		ExceptionCheckIDs:        []string{},
	}
	if e2eS18ExecutionComplete(report) {
		assessment.ExecutionStatus = "completed"
	}
	if assessment.ExecutionStatus == "completed" {
		assessment.BoundedExceptionCheckIDs = e2eS18BoundedExceptionSet(report)
	}
	if report.Decision == "pass" && assessment.ExecutionStatus == "completed" {
		assessment.Decision = acceptreport.ReleasePassed
		return assessment
	}
	if exceptions := e2eS18ExceptionSet(report); len(exceptions) > 0 && assessment.ExecutionStatus == "completed" {
		assessment.Decision = acceptreport.ReleasePassedWithPerformanceException
		assessment.ExceptionCheckIDs = exceptions
		return assessment
	}
	if report.Decision == "failed" && assessment.ExecutionStatus == "completed" {
		assessment.Decision = acceptreport.ReleaseFailed
	}
	return assessment
}

type e2eSampleSet struct {
	Language            string                   `json:"language"`
	Differential        string                   `json:"upstreamDifferential"`
	Presentation        string                   `json:"presentationComparison,omitempty"`
	SemanticEvidence    *e2eS18SemanticEvidence  `json:"semanticEvidence,omitempty"`
	UpstreamReadiness   *e2eS18ReadinessEvidence `json:"upstreamReadiness,omitempty"`
	EditUpdate          map[string]any           `json:"editUpdate,omitempty"`
	Status              string                   `json:"status"`
	Method              string                   `json:"method"`
	SampleCount         int                      `json:"sampleCount"`
	SamplesNS           []int64                  `json:"samplesNanoseconds"`
	PhaseTiming         *e2eS18PhaseTiming       `json:"phaseTiming,omitempty"`
	P50NS               int64                    `json:"p50Nanoseconds"`
	P95NS               int64                    `json:"p95Nanoseconds"`
	P99NS               int64                    `json:"p99Nanoseconds"`
	Thresholds          e2eThresholds            `json:"strictThresholdsMillis"`
	ExceptionThresholds *e2eThresholds           `json:"exceptionThresholdsMillis,omitempty"`
	FailureType         string                   `json:"failureType,omitempty"`
	FailureTypes        []string                 `json:"failureTypes,omitempty"`
	Validation          string                   `json:"semanticValidation"`
	Errors              []string                 `json:"errors,omitempty"`
}

type e2eS18PhaseTiming struct {
	DidChangeNotifyNS       []int64 `json:"didChangeNotifyNanoseconds"`
	DocumentSymbolRequestNS []int64 `json:"documentSymbolRequestNanoseconds"`
}

type e2eS18ReadinessEvidence struct {
	Status                 string   `json:"status"`
	ProbeMethod            string   `json:"probeMethod,omitempty"`
	AttemptCount           int      `json:"attemptCount"`
	StableObservationCount int      `json:"stableObservationCount"`
	ProgressBeginCount     int      `json:"progressBeginCount"`
	ProgressEndCount       int      `json:"progressEndCount"`
	ActiveProgressTokens   []string `json:"activeProgressTokens,omitempty"`
	ElapsedMillis          int64    `json:"elapsedMillis"`
	LastObservationProblem string   `json:"lastObservationProblem,omitempty"`
}

type e2eS18SemanticEvidence struct {
	Method                     string `json:"method"`
	ExpectedSymbol             string `json:"expected_symbol"`
	CandidateNormalized        any    `json:"candidate_normalized"`
	CandidateObservationStatus string `json:"candidate_observation_status"`
	CandidateValidationStatus  string `json:"candidate_validation_status"`
	UpstreamNormalized         any    `json:"upstream_normalized"`
	UpstreamObservationStatus  string `json:"upstream_observation_status"`
	UpstreamValidationStatus   string `json:"upstream_validation_status"`
	DifferentialMatched        bool   `json:"differential_matched"`
	DifferentialStatus         string `json:"differential_status"`
	FullListSnapshotStatus     string `json:"full_list_snapshot_status,omitempty"`
	FullListSampleStatus       string `json:"full_list_sample_status,omitempty"`
	TargetSampleStatus         string `json:"target_candidate_sample_status,omitempty"`
}

type e2eS19Report struct {
	Status       string                 `json:"status"`
	Scaling      []e2eReferenceScale    `json:"referencesScaling"`
	Cancellation e2eCancellationOutcome `json:"activeCancellation"`
	Progress     e2eProgressOutcome     `json:"progress"`
	NoStarvation e2eNoStarvationOutcome `json:"noStarvation"`
	Resources    []e2eResourceSnapshot  `json:"processTreeResources"`
	Errors       []string               `json:"errors,omitempty"`
	NotVerified  []string               `json:"notVerified,omitempty"`
}

type e2eReferenceScale struct {
	ReturnedLocationTotalIncludingDeclaration          int     `json:"returnedLocationTotalIncludingDeclaration"`
	ExpectedReturnedLocationCountIncludingDeclaration  int     `json:"expectedReturnedLocationCountIncludingDeclaration"`
	ObservedReturnedLocationCountsIncludingDeclaration []int   `json:"observedReturnedLocationCountsIncludingDeclaration"`
	SamplesNS                                          []int64 `json:"samplesNanoseconds"`
	P50NS                                              int64   `json:"p50Nanoseconds"`
	P95NS                                              int64   `json:"p95Nanoseconds"`
	P99NS                                              int64   `json:"p99Nanoseconds"`
	Status                                             string  `json:"status"`
	Error                                              string  `json:"error,omitempty"`
}

type e2eCancellationOutcome struct {
	Attempted       bool   `json:"attempted"`
	RequestID       int64  `json:"requestId,omitempty"`
	ProgressToken   string `json:"progressToken,omitempty"`
	ProgressBegan   bool   `json:"progressBegan"`
	CallerOutcome   string `json:"callerOutcome,omitempty"`
	ServerTerminal  string `json:"serverTerminalOutcome,omitempty"`
	ServerErrorCode int    `json:"serverErrorCode,omitempty"`
	Status          string `json:"status"`
}

type e2eProgressOutcome struct {
	BeginObserved bool   `json:"beginObserved"`
	EndObserved   bool   `json:"endObserved"`
	Status        string `json:"status"`
}

type e2eNoStarvationOutcome struct {
	ReferenceWasActive           bool   `json:"referenceWasActiveForHighPriorityRequests"`
	ReferenceProgressBegan       bool   `json:"referenceProgressBegan"`
	ReferenceProgressToken       string `json:"referenceProgressToken,omitempty"`
	ReferenceRequestID           int64  `json:"referenceRequestId,omitempty"`
	CompletionRequestID          int64  `json:"completionRequestId,omitempty"`
	HoverRequestID               int64  `json:"hoverRequestId,omitempty"`
	ReferenceBeginIndex          int    `json:"referenceBeginEventIndex,omitempty"`
	ReferenceEndIndex            int    `json:"referenceEndEventIndex,omitempty"`
	CompletionResponseIndex      int    `json:"completionResponseEventIndex,omitempty"`
	HoverResponseIndex           int    `json:"hoverResponseEventIndex,omitempty"`
	ReferenceResponseIndex       int    `json:"referenceResponseEventIndex,omitempty"`
	ReferenceTerminal            string `json:"referenceTerminalOutcome,omitempty"`
	ReferenceTerminalSuccess     bool   `json:"referenceTerminalSuccess"`
	ReferenceTerminalErrorCode   int    `json:"referenceTerminalErrorCode,omitempty"`
	ReferenceTerminalResultCount int    `json:"referenceTerminalResultCount"`
	ReferenceResultCount         int    `json:"referenceResultCount"`
	ExpectedReferenceResultCount int    `json:"expectedReferenceResultCount"`
	ReferenceCallerOutcome       string `json:"referenceCallerOutcome,omitempty"`
	ReferenceCompletedNaturally  bool   `json:"referenceCompletedNaturally"`
	CompletionNS                 int64  `json:"completionNanoseconds"`
	HoverNS                      int64  `json:"hoverNanoseconds"`
	BudgetMillis                 int64  `json:"perRequestBudgetMillis"`
	CompletionStatus             string `json:"completionStatus"`
	HoverStatus                  string `json:"hoverStatus"`
	Status                       string `json:"status"`
}

type e2eWireLocation struct {
	URI   string `json:"uri"`
	Range struct {
		Start struct {
			Line      uint32 `json:"line"`
			Character uint32 `json:"character"`
		} `json:"start"`
		End struct {
			Line      uint32 `json:"line"`
			Character uint32 `json:"character"`
		} `json:"end"`
	} `json:"range"`
}

// TestS18S19_RealProcessPerformanceAcceptance measures the real OmniLSP stdio
// process. It is opt-in because it requires the acceptance tool mode and emits
// a raw report; when enabled, missing tools and unsupported behavior are
// recorded as blocked/failed instead of being counted as a pass.
func TestS18S19_RealProcessPerformanceAcceptance(t *testing.T) {
	if os.Getenv(acceptanceModeEnv) != "1" {
		t.Skip("not run: set OMNILSP_ACCEPTANCE=1 to enable real-process stdio/LSP acceptance")
	}

	now := time.Now().UTC()
	runID := strings.TrimSpace(os.Getenv(acceptanceRunIDEnv))
	if runID == "" {
		runID = fmt.Sprintf("%d", now.UnixNano())
	}
	stableMode := e2eS18StableModeEnabled()
	policyID := e2eS18PolicyID()
	phaseTimingValue := os.Getenv(e2eS18PhaseTimingEnv)
	phaseTimingEnabled := strings.TrimSpace(phaseTimingValue) == "1"
	reportedPhaseTimingValue := phaseTimingValue
	if reportedPhaseTimingValue == "" {
		reportedPhaseTimingValue = "unset"
	}
	completionPhaseTraceValue := strings.TrimSpace(os.Getenv(e2eS18CompletionPhaseTraceEnv))
	if completionPhaseTraceValue == "" {
		completionPhaseTraceValue = "unset"
	} else {
		completionPhaseTraceValue = "enabled"
	}
	report := e2ePerformanceReport{
		Schema:     "omnilsp-performance-acceptance/v1",
		RunID:      runID,
		CreatedUTC: now,
		S18:        e2eS18Report{Status: "blocked", Thresholds: e2eS18Thresholds(), Operations: map[string]e2eSampleSet{}},
		S19:        e2eS19Report{Status: "blocked"},
		Environment: map[string]string{
			"goos": runtime.GOOS, "goarch": runtime.GOARCH,
			"acceptanceRunID":             runID,
			"logicalCores":                fmt.Sprintf("%d", runtime.NumCPU()),
			"cpu":                         firstNonEmpty(os.Getenv("OMNILSP_CPU_MODEL"), os.Getenv("PROCESSOR_IDENTIFIER"), "unavailable"),
			"filesystem":                  e2eFilesystemType(os.TempDir()),
			"workspaceRoot":               os.TempDir(),
			"client":                      "test/acceptance/lspdriver; LSP 3.17; UTF-16",
			"goRuntime":                   runtime.Version(),
			"osVersion":                   e2eOSVersion(),
			"memoryLimit":                 firstNonEmpty(os.Getenv("GOMEMLIMIT"), "unlimited-or-runtime-default"),
			"workerCount":                 fmt.Sprintf("runner GOMAXPROCS=%d; candidate GOMAXPROCS=8", runtime.GOMAXPROCS(0)),
			"candidateMaxConcurrent":      "8",
			"candidateMaxQueue":           "64",
			"candidateGOMAXPROCS":         "8",
			e2eS18PhaseTimingEnv:          reportedPhaseTimingValue,
			"s18PhaseTimingEnabled":       strconv.FormatBool(phaseTimingEnabled),
			e2eS18CompletionPhaseTraceEnv: completionPhaseTraceValue,
			e2eS18FilterEnv:               firstNonEmpty(strings.TrimSpace(os.Getenv(e2eS18FilterEnv)), "all"),
			"s18PolicyID":                 policyID,
			"releasePolicyID":             policyID,
			"s18PolicyMode":               map[bool]string{true: "stable", false: "strict"}[stableMode],
			"s18StableExceptionScope":     "c/cpp completion_first_usable and syntax_update_after_edit only; raw single-request samples plus per-request candidate overhead required; strict thresholds remain authoritative",
			"s19ResourceRoot":             "Go test process (os.Getpid) and measurable descendants; five S19 stages",
			"benchmarkState.S18":          map[bool]string{true: "32 semantic warm-ups; qualified C/C++ completion and syntax rows use 3 ABBA rounds (candidate/upstream/upstream/candidate), 1000 requests per leg", false: "32 warm-up requests followed by 1000 individual measured requests per language and operation"}[stableMode],
			"benchmarkState.S19":          "exact 200/800/3200 returned-location totals including declaration; full-document didChange invalidates snapshots between each of 3 samples",
		},
	}
	defer func() {
		if report.S18.Status == "blocked" && len(report.S18.Operations) == 0 {
			report.S18.Errors = append(report.S18.Errors, "S18 did not start; required process/tooling was unavailable")
		}
		if report.S19.Status == "blocked" && len(report.S19.Scaling) == 0 {
			report.S19.Errors = append(report.S19.Errors, "S19 did not start; required process/tooling was unavailable")
		}
		report.Decision = e2eOverallDecision(report.S18.Status, report.S19.Status)
		report.ReleaseAssessment = e2eReleaseAssessment(report)
		path, err := writeE2EPerformanceReport(report)
		if err != nil {
			t.Errorf("write raw performance report: %v", err)
			return
		}
		t.Logf("raw S18/S19 JSON report: %s", path)
	}()

	binary, err := e2eResolveBinary(t)
	if err != nil {
		report.S18.Errors = append(report.S18.Errors, err.Error())
		report.S19.Errors = append(report.S19.Errors, err.Error())
		t.Errorf("candidate unavailable; structured acceptance decision will remain not_verified: %v", err)
		return
	}
	binaryBytes, err := os.ReadFile(binary)
	if err != nil {
		report.S18.Errors = append(report.S18.Errors, "read candidate binary: "+err.Error())
		report.S19.Errors = append(report.S19.Errors, "read candidate binary: "+err.Error())
		t.Errorf("candidate binary unreadable; structured acceptance decision will remain not_verified: %v", err)
		return
	}
	binaryDigest := sha256.Sum256(binaryBytes)
	report.CandidateBin = binary
	report.CandidateSHA = "sha256:" + hex.EncodeToString(binaryDigest[:])
	report.GitRevision = e2eGitRevision(t)
	report.ToolVersions = e2eToolVersions()

	s18Files := buildE2ES18Corpus()
	report.CorpusContentSHA, report.CorpusFiles = digestE2ECorpus(s18Files)
	fixtureManifest, manifestErr := json.Marshal(e2eBuildS18AuditManifest(s18Files))
	if manifestErr != nil {
		report.S18.Errors = append(report.S18.Errors, "encode S18 representative fixture manifest: "+manifestErr.Error())
	} else {
		manifestDigest := sha256.Sum256(fixtureManifest)
		report.Environment["s18FixtureManifest"] = string(fixtureManifest)
		report.Environment["s18FixtureManifestSHA256"] = "sha256:" + hex.EncodeToString(manifestDigest[:])
	}
	report.Environment["s18CoverageScope"] = "representative Tier S performance fixtures; not exhaustive language or tool conformance"
	corpusRunDigest := sha256.Sum256([]byte(report.CorpusContentSHA + "\nrun-id:" + runID))
	report.CorpusSHA = "sha256:" + hex.EncodeToString(corpusRunDigest[:])
	t.Run("S18", func(st *testing.T) {
		report.S18 = runE2ES18(st, binary, s18Files, runID, report.ToolVersions, phaseTimingEnabled)
		for name, samples := range report.S18.Operations {
			evidence, hasEvidence := report.S18.ABBAEvidence[name]
			acceptedLatencyException := stableMode && hasEvidence && e2eS18LatencyExceptionEligible(runID, name, samples, evidence)
			if samples.Status == "failed" && !acceptedLatencyException {
				st.Errorf("S18 %s: %s", name, strings.Join(samples.Errors, "; "))
			} else if samples.Status == "failed" {
				st.Logf("S18 %s: strict latency failure retained; accepted under %s", name, e2eS18StablePolicyID)
			} else if samples.Status != "pass" {
				st.Errorf("S18 %s not verified: %s", name, strings.Join(samples.Errors, "; "))
			}
		}
	})

	t.Run("S19", func(st *testing.T) {
		report.S19 = runE2ES19(st, binary)
		if report.S19.Status == "failed" {
			st.Errorf("S19 status=%s: %s", report.S19.Status, strings.Join(report.S19.Errors, "; "))
		} else if report.S19.Status != "pass" {
			st.Errorf("S19 status=%s; structured acceptance decision will remain not_verified: %s", report.S19.Status, strings.Join(report.S19.NotVerified, "; "))
		}
	})
}

func runE2ES18(t *testing.T, binary string, files map[string]string, runID string, toolVersions map[string]string, phaseTimingEnabled bool) e2eS18Report {
	t.Helper()
	result := e2eS18Report{Status: "running", Thresholds: e2eS18Thresholds(), Operations: map[string]e2eSampleSet{}, ABBAEvidence: map[string]acceptreport.ABBAEvidence{}}
	workspace := t.TempDir()
	if err := writeE2EFiles(workspace, files); err != nil {
		result.Status = "failed"
		result.Errors = append(result.Errors, err.Error())
		t.Errorf("prepare S18 corpus: %v", err)
		return result
	}
	toolPath := e2eAcceptanceToolPath()
	session := lspdriver.Start(t, binary, workspace, e2eS18CandidateEnv(toolPath, "", workspace))
	defer func() {
		if session != nil {
			session.Close(t)
		}
	}()
	closeCandidate := func() bool {
		if session == nil {
			return true
		}
		cleanExit := e2eCloseS18Candidate(t, session)
		session = nil
		return cleanExit
	}
	session.Initialize(t)

	fixtures := e2eS18Fixtures(workspace, files)
	filter := e2eParseS18Filter(os.Getenv(e2eS18FilterEnv))
	for _, fixture := range fixtures {
		fixture := fixture
		operationNames := []string{"hot_hover", "hot_definition", "completion_first_usable", "syntax_update_after_edit"}
		selectedCount := 0
		for _, name := range operationNames {
			if e2eS18FilterSelects(filter, fixture.id, name) {
				selectedCount++
			}
		}
		if selectedCount == 0 {
			for _, name := range operationNames {
				check := e2eS18Operation(fixture, workspace, files, name)
				key := runID + "/S18/" + fixture.id + "/" + name
				if e2eS18StableModeEnabled() && e2eS18ExceptionScope(fixture.id, name) {
					result.ABBAEvidence[key] = e2eNewS18ABBAEvidence(key, fixture, name, check.method)
				}
				result.Operations[key] = e2eSampleSet{
					Language: fixture.name, Status: "not_verified", Method: check.method,
					Thresholds: check.threshold, Validation: "not run; excluded by diagnostic S18 filter",
					SemanticEvidence: e2eNewS18SemanticEvidence(fixture, check.method),
					FailureType:      string(e2eFailureIncomplete), FailureTypes: []string{string(e2eFailureIncomplete)},
					Errors: []string{"not run: excluded by diagnostic S18 filter"},
				}
			}
			result.NotVerified = append(result.NotVerified, fixture.name+": excluded by diagnostic S18 filter")
			continue
		}
		fixtureOpened := false
		if session != nil {
			e2eOpenS18FixtureFiles(t, session, fixture, files, workspace)
			fixtureOpened = true
		}
		prefix := runID + "/S18/" + fixture.id
		missing := e2eMissingFixtureTools(fixture.toolNames)
		missing = append(missing, e2eLockedToolVersionProblems(fixture.toolNames, toolVersions)...)
		missing = uniqueE2EStrings(missing)
		upstreamBinary := ""
		if len(missing) == 0 {
			var err error
			upstreamBinary, err = e2eLookPath(fixture.upstreamBinary)
			if err != nil {
				missing = append(missing, fixture.upstreamBinary+": "+err.Error())
			}
		}
		for _, name := range operationNames {
			key := prefix + "/" + name
			check := e2eS18Operation(fixture, workspace, files, name)
			if e2eS18StableModeEnabled() && e2eS18ExceptionScope(fixture.id, name) {
				result.ABBAEvidence[key] = e2eNewS18ABBAEvidence(key, fixture, name, check.method)
			}
			if !e2eS18FilterSelects(filter, fixture.id, name) {
				result.Operations[key] = e2eSampleSet{
					Language: fixture.name, Status: "not_verified", Method: check.method,
					Thresholds: check.threshold, Validation: "not run; excluded by diagnostic S18 filter",
					SemanticEvidence: e2eNewS18SemanticEvidence(fixture, check.method),
					FailureType:      string(e2eFailureIncomplete), FailureTypes: []string{string(e2eFailureIncomplete)},
					Errors: []string{"not run: excluded by diagnostic S18 filter"},
				}
				result.NotVerified = append(result.NotVerified, fixture.name+"/"+name+": excluded by diagnostic S18 filter")
				continue
			}
			if len(missing) > 0 {
				result.NotVerified = append(result.NotVerified, fixture.name+": "+strings.Join(missing, ", "))
				result.Operations[key] = e2eSampleSet{
					Language: fixture.name, Status: "not_verified", Method: check.method,
					Thresholds: check.threshold, Validation: "not run; pinned/required tool unavailable",
					SemanticEvidence: e2eNewS18SemanticEvidence(fixture, check.method),
					FailureType:      string(e2eFailureTool), FailureTypes: []string{string(e2eFailureTool)},
					Errors: []string{"blocked: required acceptance tools missing: " + strings.Join(missing, ", ")},
				}
				continue
			}
			if e2eS18StableModeEnabled() && e2eS18ExceptionScope(fixture.id, name) {
				if session != nil {
					closeCandidate()
					session = nil
					fixtureOpened = false
				}
				tracePath := filepath.Join(t.TempDir(), "nested-rpc-timing.json")
				candidate := lspdriver.Start(t, binary, workspace, e2eS18CandidateEnv(toolPath, tracePath, workspace))
				candidate.Initialize(t)
				e2eOpenS18FixtureFiles(t, candidate, fixture, files, workspace)
				baseline, readiness, err := e2eStartS18Upstream(upstreamBinary, fixture, files, workspace, toolPath, check)
				if err != nil {
					candidate.Close(t)
					message := fixture.upstreamBinary + " initialization/readiness failed: " + err.Error()
					result.NotVerified = append(result.NotVerified, fixture.name+" upstream readiness: "+err.Error())
					result.Operations[key] = e2eSampleSet{
						Language: fixture.name, Status: "not_verified", Method: check.method,
						Thresholds: check.threshold, Validation: "pinned upstream did not become semantically ready",
						SemanticEvidence: e2eNewS18SemanticEvidence(fixture, check.method), UpstreamReadiness: &readiness,
						FailureType: string(e2eFailureProcess), FailureTypes: []string{string(e2eFailureProcess)},
						Errors: []string{"blocked: " + message},
					}
					continue
				}
				sampleSet, evidence := e2eRunS18ABBASamples(t, candidate, baseline, fixture, files, workspace, check, key, phaseTimingEnabled)
				sampleSet.UpstreamReadiness = &readiness
				candidateCleanExit := e2eCloseS18Candidate(t, candidate)
				traceErr := e2eCompleteS18ABBATiming(&evidence, sampleSet, tracePath, fixture, workspace)
				if traceErr != nil {
					if sampleSet.SemanticEvidence != nil && e2eS18RequiresCompletionFullList(fixture.id, check.method) {
						sampleSet.SemanticEvidence.FullListSnapshotStatus = "not_verified"
						sampleSet.SemanticEvidence.FullListSampleStatus = "not_verified"
						sampleSet.SemanticEvidence.TargetSampleStatus = "not_verified"
					}
					e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureIncomplete, fmt.Errorf("ABBA nested timing correlation: %w", traceErr))
					result.NotVerified = append(result.NotVerified, fixture.name+"/"+name+": "+traceErr.Error())
				}
				if !candidateCleanExit {
					if sampleSet.SemanticEvidence != nil && e2eS18RequiresCompletionFullList(fixture.id, check.method) {
						sampleSet.SemanticEvidence.FullListSnapshotStatus = "not_verified"
						sampleSet.SemanticEvidence.FullListSampleStatus = "not_verified"
						sampleSet.SemanticEvidence.TargetSampleStatus = "not_verified"
					}
					e2eRecordS18Failure(&sampleSet, "failed", e2eFailureProcess, errors.New("candidate process did not exit cleanly after ABBA sampling"))
				} else if traceErr == nil && e2eS18RequiresCompletionFullList(fixture.id, check.method) && e2eS18ABBACompletionSamplesSemanticallyComplete(sampleSet) {
					sampleSet.SemanticEvidence.FullListSnapshotStatus = "passed"
					sampleSet.SemanticEvidence.FullListSampleStatus = "passed"
					sampleSet.SemanticEvidence.TargetSampleStatus = "passed"
				}
				result.ABBAEvidence[key] = evidence
				result.Operations[key] = sampleSet
				continue
			}
			if session == nil {
				session = lspdriver.Start(t, binary, workspace, e2eS18CandidateEnv(toolPath, "", workspace))
				session.Initialize(t)
				fixtureOpened = false
			}
			if !fixtureOpened {
				e2eOpenS18FixtureFiles(t, session, fixture, files, workspace)
				fixtureOpened = true
			}
			baseline, readiness, err := e2eStartS18Upstream(upstreamBinary, fixture, files, workspace, toolPath, check)
			if err != nil {
				message := fixture.upstreamBinary + " initialization/readiness failed: " + err.Error()
				result.NotVerified = append(result.NotVerified, fixture.name+" upstream readiness: "+err.Error())
				result.Operations[key] = e2eSampleSet{
					Language: fixture.name, Status: "not_verified", Method: check.method,
					Thresholds: check.threshold, Validation: "pinned upstream did not become semantically ready",
					SemanticEvidence:  e2eNewS18SemanticEvidence(fixture, check.method),
					UpstreamReadiness: &readiness,
					FailureType:       string(e2eFailureProcess), FailureTypes: []string{string(e2eFailureProcess)},
					Errors: []string{"blocked: " + message},
				}
				continue
			}
			sampleSet := e2eRunS18Samples(t, session, baseline, fixture, files, workspace, check, phaseTimingEnabled)
			sampleSet.UpstreamReadiness = &readiness
			result.Operations[key] = sampleSet
		}
	}
	candidateCleanExit := closeCandidate()
	for id, samples := range result.Operations {
		evidence := samples.SemanticEvidence
		if evidence == nil || evidence.FullListSnapshotStatus != "pending" || evidence.FullListSampleStatus != "pending" || evidence.TargetSampleStatus != "pending" {
			continue
		}
		if candidateCleanExit && e2eS18StrictCompletionSamplesComplete(samples) {
			evidence.FullListSnapshotStatus = "passed"
			evidence.FullListSampleStatus = "passed"
			evidence.TargetSampleStatus = "passed"
		} else {
			evidence.FullListSnapshotStatus = "not_verified"
			evidence.FullListSampleStatus = "not_verified"
			evidence.TargetSampleStatus = "not_verified"
			if !candidateCleanExit {
				e2eRecordS18Failure(&samples, "failed", e2eFailureProcess, errors.New("candidate process did not exit cleanly after completion sampling"))
			} else {
				e2eRecordS18Failure(&samples, "not_verified", e2eFailureIncomplete, errors.New("completion samples were incomplete when finalizing full-list evidence"))
			}
		}
		result.Operations[id] = samples
	}
	result.Status = "pass"
	for _, samples := range result.Operations {
		for _, kind := range samples.FailureTypes {
			seen := false
			for _, existing := range result.FailureTypes {
				if existing == kind {
					seen = true
					break
				}
			}
			if !seen {
				result.FailureTypes = append(result.FailureTypes, kind)
			}
		}
		if samples.Status == "failed" {
			result.Status = "failed"
			result.Errors = append(result.Errors, samples.Language+": "+strings.Join(samples.Errors, "; "))
		} else if samples.Status != "pass" && result.Status == "pass" {
			result.Status = "partial"
			result.NotVerified = append(result.NotVerified, samples.Language+"/"+samples.Method+": "+strings.Join(samples.Errors, "; "))
		}
	}
	if !candidateCleanExit {
		result.Status = "failed"
		result.FailureTypes = append(result.FailureTypes, string(e2eFailureProcess))
		result.Errors = append(result.Errors, "candidate process did not exit cleanly after S18 sampling")
	}
	return result
}

type e2eNestedTimingSample struct {
	Method          string          `json:"method"`
	URI             string          `json:"uri"`
	ParentRequestID json.RawMessage `json:"parent_request_id"`
	DurationNS      uint64          `json:"duration_ns"`
	WriteDurationNS uint64          `json:"write_duration_ns"`
	WaitDurationNS  uint64          `json:"wait_duration_ns"`
	Outcome         string          `json:"outcome"`
}

type e2eNestedTimingReport struct {
	Version          int                     `json:"version"`
	Language         string                  `json:"language"`
	SampleLimit      int                     `json:"sample_limit"`
	SamplesTruncated bool                    `json:"samples_truncated"`
	Samples          []e2eNestedTimingSample `json:"samples"`
}

type e2eS18ABBAPathState struct {
	version int64
	marker  byte
}

func e2eRunS18ABBASamples(t *testing.T, candidate *lspdriver.Session, oracle *upstream.Session, f e2eS18Fixture, files map[string]string, workspace string, check e2eS18OperationSpec, operationID string, phaseTimingEnabled bool) (e2eSampleSet, acceptreport.ABBAEvidence) {
	t.Helper()
	fullListCheck := e2eS18RequiresCompletionFullList(f.id, check.method)
	sampleSet := e2eSampleSet{
		Language: f.name, Status: "running", Method: check.method, Thresholds: check.threshold,
		Validation: "not checked", Differential: "not checked", SemanticEvidence: e2eNewS18SemanticEvidence(f, check.method),
	}
	if fullListCheck {
		sampleSet.SemanticEvidence.FullListSnapshotStatus = "not_verified"
		sampleSet.SemanticEvidence.FullListSampleStatus = "not_verified"
	}
	threshold, _ := e2eS18StableThresholdForID(operationID)
	sampleSet.ExceptionThresholds = &threshold
	operation := operationID[strings.LastIndex(operationID, "/")+1:]
	evidence := e2eNewS18ABBAEvidence(operationID, f, operation, check.method)
	defer func() {
		sampleSet.SampleCount = len(sampleSet.SamplesNS)
		if sampleSet.SampleCount > 0 {
			sampleSet.P50NS = e2ePercentileNS(sampleSet.SamplesNS, .50)
			sampleSet.P95NS = e2ePercentileNS(sampleSet.SamplesNS, .95)
			sampleSet.P99NS = e2ePercentileNS(sampleSet.SamplesNS, .99)
		}
	}()
	defer func() {
		if oracle != nil {
			if err := oracle.Close(); err != nil {
				if fullListCheck && sampleSet.SemanticEvidence != nil && sampleSet.SemanticEvidence.FullListSnapshotStatus == "passed" {
					sampleSet.SemanticEvidence.FullListSnapshotStatus = "not_verified"
					sampleSet.SemanticEvidence.FullListSampleStatus = "not_verified"
				}
				e2eRecordS18Failure(&sampleSet, "failed", e2eFailureProcess, fmt.Errorf("pinned upstream process shutdown: %w", err))
			}
		}
	}()
	docURI := uri.FromPath(filepath.Join(workspace, filepath.FromSlash(f.queryFile))).String()
	queryText := files[f.queryFile]
	checkBaseline := func(request func(context.Context) (json.RawMessage, error), label string) (json.RawMessage, bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		raw, err := request(ctx)
		if err != nil {
			return nil, false, fmt.Errorf("%s baseline request: %w", label, err)
		}
		if err := check.validate(raw); err != nil {
			return nil, true, fmt.Errorf("%s baseline validation: %w", label, err)
		}
		return raw, false, nil
	}
	candidateRequest := func(ctx context.Context) (json.RawMessage, error) {
		_, raw, err := candidate.RequestIDContext(ctx, check.method, check.params)
		return raw, err
	}
	upstreamRequest := func(ctx context.Context) (json.RawMessage, error) {
		return oracle.RequestContext(ctx, check.method, check.params)
	}
	candidateBaseline, candidateBaselineValidationFailed, err := checkBaseline(candidateRequest, "candidate")
	if err != nil {
		if fullListCheck && candidateBaselineValidationFailed {
			sampleSet.SemanticEvidence.FullListSnapshotStatus = "failed"
			sampleSet.SemanticEvidence.FullListSampleStatus = "failed"
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, err)
		} else {
			e2eRecordS18Failure(&sampleSet, e2eErrorStatus(err), e2eFailureKindOf(err), err)
		}
		return sampleSet, evidence
	}
	if fullListCheck {
		_, decodeErr := e2eDecodeCompletionSnapshot(candidateBaseline)
		if decodeErr != nil {
			sampleSet.SemanticEvidence.FullListSnapshotStatus = "failed"
			sampleSet.SemanticEvidence.FullListSampleStatus = "failed"
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, fmt.Errorf("candidate full-list baseline validation: %v", decodeErr))
			return sampleSet, evidence
		}
	}
	verifyCandidateFullList := func(raw json.RawMessage) error {
		if !fullListCheck {
			return nil
		}
		if err := e2eS18ValidateFullCompletionResponse(check, raw); err != nil {
			return fmt.Errorf("candidate completion full-list validation: %v", err)
		}
		return nil
	}
	recordCandidateFullListFailure := func(label string, err error) {
		sampleSet.SemanticEvidence.FullListSnapshotStatus = "failed"
		sampleSet.SemanticEvidence.FullListSampleStatus = "failed"
		e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, fmt.Errorf("%s: %v", label, err))
	}
	upstreamBaseline, _, err := checkBaseline(upstreamRequest, "upstream")
	if err != nil {
		e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureKindOf(err), fmt.Errorf("blocked: %w", err))
		return sampleSet, evidence
	}
	currentSymbol := f.expectedSymbol
	var syntaxNames []string
	if check.method == "textDocument/documentSymbol" {
		beforeNames, namesErr := e2eS18SymbolNames(candidateBaseline, f, currentSymbol)
		if namesErr != nil {
			e2eRecordS18Failure(&sampleSet, e2eErrorStatus(namesErr), e2eFailureSemantic, namesErr)
			return sampleSet, evidence
		}
		upstreamNames, namesErr := e2eS18SymbolNames(upstreamBaseline, f, currentSymbol)
		if namesErr != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureSemantic, fmt.Errorf("blocked: upstream symbols: %w", namesErr))
			return sampleSet, evidence
		}
		if !reflect.DeepEqual(beforeNames, upstreamNames) {
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, fmt.Errorf("normalized document-symbol baseline differs: candidate=%v upstream=%v", beforeNames, upstreamNames))
			return sampleSet, evidence
		}
		updatedText, updatedSymbol, renamed := e2eRenameFunctionSymbol(queryText, f.expectedSymbol)
		if !renamed {
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, fmt.Errorf("fixture has no function declaration for edit target %q", f.expectedSymbol))
			return sampleSet, evidence
		}
		updatedCheck := check
		updatedCheck.validate = func(raw json.RawMessage) error { return validateE2ESymbolsFor(raw, updatedSymbol) }
		updatedCheck.compare = func(left, right json.RawMessage) error {
			return e2eCompareS18DocumentSymbols(left, right, f, updatedSymbol)
		}
		updatedCheck.compareSample = nil
		candidate.Notify(t, "textDocument/didChange", map[string]any{
			"textDocument": map[string]any{"uri": docURI, "version": 2}, "contentChanges": []map[string]string{{"text": updatedText}},
		})
		if err := oracle.Notify("textDocument/didChange", map[string]any{
			"textDocument": map[string]any{"uri": docURI, "version": 2}, "contentChanges": []map[string]string{{"text": updatedText}},
		}); err != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureProcess, fmt.Errorf("upstream didChange: %w", err))
			return sampleSet, evidence
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		candidateUpdated, requestErr := candidate.RequestContext(ctx, check.method, check.params)
		cancel()
		if requestErr != nil {
			e2eRecordS18Failure(&sampleSet, e2eErrorStatus(requestErr), e2eFailureKindOf(requestErr), fmt.Errorf("candidate didChange freshness query: %w", requestErr))
			return sampleSet, evidence
		}
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		upstreamUpdated, requestErr := oracle.RequestContext(ctx, check.method, check.params)
		cancel()
		if requestErr != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureKindOf(requestErr), fmt.Errorf("upstream didChange freshness query: %w", requestErr))
			return sampleSet, evidence
		}
		candidateNames, namesErr := e2eS18SymbolNames(candidateUpdated, f, updatedSymbol)
		if namesErr != nil {
			e2eRecordS18Failure(&sampleSet, e2eErrorStatus(namesErr), e2eFailureSemantic, namesErr)
			return sampleSet, evidence
		}
		upstreamNames, namesErr = e2eS18SymbolNames(upstreamUpdated, f, updatedSymbol)
		if namesErr != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureSemantic, namesErr)
			return sampleSet, evidence
		}
		expected := append([]string(nil), beforeNames...)
		renamedExpected := false
		for i, name := range expected {
			if name == f.expectedSymbol {
				expected[i] = updatedSymbol
				renamedExpected = true
			}
		}
		if !renamedExpected || !reflect.DeepEqual(candidateNames, expected) || !reflect.DeepEqual(candidateNames, upstreamNames) {
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, fmt.Errorf("document-symbol response was stale or differs after unsaved rename: candidate=%v upstream=%v expected=%v", candidateNames, upstreamNames, expected))
			return sampleSet, evidence
		}
		compareErr := updatedCheck.compare(candidateUpdated, upstreamUpdated)
		semantic, semanticErr := e2eBuildS18SemanticEvidence(f, updatedCheck, updatedSymbol, candidateUpdated, upstreamUpdated, compareErr)
		if semanticErr != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureSemantic, semanticErr)
			return sampleSet, evidence
		}
		sampleSet.SemanticEvidence = semantic
		sampleSet.EditUpdate = map[string]any{
			"changed": true, "oldSymbol": f.expectedSymbol, "newSymbol": updatedSymbol,
			"beforeSymbols": beforeNames, "afterSymbols": candidateNames,
			"upstreamAfterSymbols": upstreamNames, "freshnessVerified": true,
		}
		sampleSet.Differential = "normalized document symbols match pinned upstream before and after an unsaved function rename"
		queryText, currentSymbol, syntaxNames = updatedText, updatedSymbol, candidateNames
		check = updatedCheck
	} else {
		compareErr := check.compare(candidateBaseline, upstreamBaseline)
		semantic, semanticErr := e2eBuildS18SemanticEvidence(f, check, f.queryToken, candidateBaseline, upstreamBaseline, compareErr)
		if semanticErr != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureSemantic, semanticErr)
			return sampleSet, evidence
		}
		sampleSet.SemanticEvidence = semantic
		sampleSet.Differential = "target candidate semantics are compared with pinned upstream on each paired sample; every C/C++ candidate response is validated as a full list, while same-response order, edit, and isIncomplete pass-through is covered by the completion projection regression"
		sampleSet.SemanticEvidence.TargetSampleStatus = "not_verified"
	}
	if !e2eS18DifferentialEvidencePassed(sampleSet) {
		e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, errors.New("candidate and upstream semantic baseline did not match"))
		return sampleSet, evidence
	}

	queryTextBytes := []byte(queryText)
	markerOffset := e2eEditMarkerOffset(queryText)
	if (check.method == "textDocument/documentSymbol" || check.method == "textDocument/completion") && (markerOffset < 0 || markerOffset >= len(queryTextBytes)) {
		e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, errors.New("fixture is missing a safe comment/string edit marker"))
		return sampleSet, evidence
	}
	initialVersion := int64(1)
	if check.method == "textDocument/documentSymbol" {
		initialVersion = 2
	}
	candidateState := e2eS18ABBAPathState{version: initialVersion, marker: queryTextBytes[markerOffset]}
	upstreamState := candidateState
	updateForOperation := func(target *e2eS18ABBAPathState, notify func(string, any) error) (int64, error) {
		target.version++
		if target.marker == 'A' {
			target.marker = 'B'
		} else {
			target.marker = 'A'
		}
		updated := append([]byte(nil), queryTextBytes...)
		updated[markerOffset] = target.marker
		started := time.Now()
		err := notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": docURI, "version": target.version},
			"contentChanges": []map[string]string{{"text": string(updated)}},
		})
		return time.Since(started).Nanoseconds(), err
	}
	candidateOp := func() (json.RawMessage, int64, int64, int64, int64, error) {
		var didChangeNS int64
		if check.method == "textDocument/completion" {
			didChangeNS, _ = updateForOperation(&candidateState, func(method string, params any) error {
				candidate.Notify(t, method, params)
				return nil
			})
		}
		started := time.Now()
		if check.method == "textDocument/documentSymbol" {
			var notifyErr error
			didChangeNS, notifyErr = updateForOperation(&candidateState, func(method string, params any) error {
				candidate.Notify(t, method, params)
				return nil
			})
			if notifyErr != nil {
				return nil, 0, 0, 0, 0, notifyErr
			}
		}
		requestStarted := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		id, raw, requestErr := candidate.RequestIDContext(ctx, check.method, check.params)
		cancel()
		requestNS := time.Since(requestStarted).Nanoseconds()
		return raw, id, time.Since(started).Nanoseconds(), didChangeNS, requestNS, requestErr
	}
	upstreamOp := func() (json.RawMessage, int64, error) {
		if check.method == "textDocument/completion" {
			if _, err := updateForOperation(&upstreamState, oracle.Notify); err != nil {
				return nil, 0, err
			}
		}
		started := time.Now()
		if check.method == "textDocument/documentSymbol" {
			if _, err := updateForOperation(&upstreamState, oracle.Notify); err != nil {
				return nil, 0, err
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		raw, requestErr := oracle.RequestContext(ctx, check.method, check.params)
		cancel()
		return raw, time.Since(started).Nanoseconds(), requestErr
	}
	comparePair := func(candidateRaw, upstreamRaw json.RawMessage) error {
		if err := check.validate(candidateRaw); err != nil {
			return fmt.Errorf("candidate sample validation: %w", err)
		}
		if err := check.validate(upstreamRaw); err != nil {
			return fmt.Errorf("upstream sample validation: %w", err)
		}
		if err := check.compare(candidateRaw, upstreamRaw); err != nil {
			return fmt.Errorf("candidate/upstream sample differential: %w", err)
		}
		if check.method == "textDocument/documentSymbol" {
			if err := e2eSameS18SymbolNames(candidateRaw, f, currentSymbol, syntaxNames); err != nil {
				return fmt.Errorf("candidate document-symbol sample freshness: %w", err)
			}
			if err := e2eSameS18SymbolNames(upstreamRaw, f, currentSymbol, syntaxNames); err != nil {
				return fmt.Errorf("upstream document-symbol sample freshness: %w", err)
			}
		}
		return nil
	}
	for i := 0; i < 32; i++ {
		candidateRaw, _, _, _, _, candidateErr := candidateOp()
		if candidateErr != nil {
			e2eRecordS18Failure(&sampleSet, e2eErrorStatus(candidateErr), e2eFailureKindOf(candidateErr), fmt.Errorf("warm-up %d candidate: %w", i, candidateErr))
			return sampleSet, evidence
		}
		if err := verifyCandidateFullList(candidateRaw); err != nil {
			recordCandidateFullListFailure(fmt.Sprintf("warm-up %d candidate full-list semantic validation", i), err)
			return sampleSet, evidence
		}
		upstreamRaw, _, upstreamErr := upstreamOp()
		if upstreamErr != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureKindOf(upstreamErr), fmt.Errorf("warm-up %d upstream: %w", i, upstreamErr))
			return sampleSet, evidence
		}
		if err := comparePair(candidateRaw, upstreamRaw); err != nil {
			if check.method == "textDocument/completion" {
				e2eRecordS18CompletionTargetSampleFailure(&sampleSet, err)
			}
			e2eRecordS18Failure(&sampleSet, e2eErrorStatus(err), e2eFailureSemantic, fmt.Errorf("warm-up %d: %w", i, err))
			return sampleSet, evidence
		}
	}

	sampleSet.SamplesNS = make([]int64, 0, e2eS18ABBARequiredSamples)
	for roundIndex := 0; roundIndex < e2eS18ABBARounds; roundIndex++ {
		round := acceptreport.ABBARound{Index: roundIndex, Order: []string{"candidate", "upstream", "upstream", "candidate"}, Legs: make([]acceptreport.ABBALeg, 0, e2eS18ABBALegsPerRound)}
		flushPartialRound := func(leg acceptreport.ABBALeg) {
			round.Legs = append(round.Legs, leg)
			evidence.Rounds = append(evidence.Rounds, round)
			sampleSet.SampleCount = len(sampleSet.SamplesNS)
		}
		candidateResponses := make([][]json.RawMessage, 2)
		upstreamResponses := make([][]json.RawMessage, 2)
		candidatePair := 0
		upstreamPair := 0
		for position, role := range round.Order {
			leg := acceptreport.ABBALeg{Position: position, Role: role}
			if role == "candidate" {
				leg.SampleStart = len(sampleSet.SamplesNS)
				responses := make([]json.RawMessage, 0, e2eS18ABBASamplesPerLeg)
				for i := 0; i < e2eS18ABBASamplesPerLeg; i++ {
					raw, requestID, outerNS, didChangeNS, requestNS, requestErr := candidateOp()
					if requestErr != nil {
						e2eRecordS18Failure(&sampleSet, e2eErrorStatus(requestErr), e2eFailureKindOf(requestErr), fmt.Errorf("ABBA round %d candidate sample %d: %w", roundIndex, i, requestErr))
						flushPartialRound(leg)
						return sampleSet, evidence
					}
					if requestID <= 0 || outerNS <= 0 {
						e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureIncomplete, errors.New("candidate LSP request ID or outer duration is invalid"))
						flushPartialRound(leg)
						return sampleSet, evidence
					}
					sampleSet.SamplesNS = append(sampleSet.SamplesNS, outerNS)
					leg.RequestIDs = append(leg.RequestIDs, strconv.FormatInt(requestID, 10))
					leg.SampleCount++
					responses = append(responses, raw)
					if phaseTimingEnabled && check.method == "textDocument/documentSymbol" {
						if sampleSet.PhaseTiming == nil {
							sampleSet.PhaseTiming = &e2eS18PhaseTiming{}
						}
						sampleSet.PhaseTiming.DidChangeNotifyNS = append(sampleSet.PhaseTiming.DidChangeNotifyNS, didChangeNS)
						sampleSet.PhaseTiming.DocumentSymbolRequestNS = append(sampleSet.PhaseTiming.DocumentSymbolRequestNS, requestNS)
					}
					if fullListCheck {
						if err := verifyCandidateFullList(raw); err != nil {
							recordCandidateFullListFailure(fmt.Sprintf("ABBA round %d candidate sample %d full-list semantic validation", roundIndex, i), err)
							flushPartialRound(leg)
							return sampleSet, evidence
						}
					} else if err := check.validate(raw); err != nil {
						e2eRecordS18Failure(&sampleSet, e2eErrorStatus(err), e2eFailureSemantic, fmt.Errorf("ABBA candidate sample %d validation: %w", i, err))
						flushPartialRound(leg)
						return sampleSet, evidence
					}
				}
				candidateResponses[candidatePair] = responses
				candidatePair++
			} else {
				responses := make([]json.RawMessage, 0, e2eS18ABBASamplesPerLeg)
				for i := 0; i < e2eS18ABBASamplesPerLeg; i++ {
					raw, durationNS, requestErr := upstreamOp()
					if requestErr != nil {
						e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureKindOf(requestErr), fmt.Errorf("ABBA round %d upstream sample %d: %w", roundIndex, i, requestErr))
						flushPartialRound(leg)
						return sampleSet, evidence
					}
					if durationNS <= 0 {
						e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureIncomplete, errors.New("upstream outer duration is invalid"))
						flushPartialRound(leg)
						return sampleSet, evidence
					}
					responses = append(responses, raw)
					leg.RawNS = append(leg.RawNS, float64(durationNS))
					if err := check.validate(raw); err != nil {
						e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureSemantic, fmt.Errorf("ABBA upstream sample %d validation: %w", i, err))
						flushPartialRound(leg)
						return sampleSet, evidence
					}
				}
				upstreamResponses[upstreamPair] = responses
				upstreamPair++
			}
			round.Legs = append(round.Legs, leg)
		}
		for pair := 0; pair < 2; pair++ {
			for i := 0; i < e2eS18ABBASamplesPerLeg; i++ {
				if err := comparePair(candidateResponses[pair][i], upstreamResponses[pair][i]); err != nil {
					if check.method == "textDocument/completion" {
						e2eRecordS18CompletionTargetSampleFailure(&sampleSet, err)
					}
					e2eRecordS18Failure(&sampleSet, e2eErrorStatus(err), e2eFailureSemantic, fmt.Errorf("ABBA round %d pair %d sample %d: %w", roundIndex, pair, i, err))
					evidence.Rounds = append(evidence.Rounds, round)
					sampleSet.SampleCount = len(sampleSet.SamplesNS)
					return sampleSet, evidence
				}
			}
		}
		evidence.Rounds = append(evidence.Rounds, round)
	}
	sampleSet.SampleCount = len(sampleSet.SamplesNS)
	if sampleSet.SampleCount > 0 {
		sampleSet.P50NS = e2ePercentileNS(sampleSet.SamplesNS, .50)
		sampleSet.P95NS = e2ePercentileNS(sampleSet.SamplesNS, .95)
		sampleSet.P99NS = e2ePercentileNS(sampleSet.SamplesNS, .99)
	}
	if sampleSet.SampleCount != e2eS18ABBARequiredSamples {
		e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSampleMissing, fmt.Errorf("collected %d of %d ABBA candidate request samples", sampleSet.SampleCount, e2eS18ABBARequiredSamples))
	} else if e2eS18StrictLatencyExceeded(sampleSet) {
		e2eRecordS18Failure(&sampleSet, "failed", e2eFailureLatency, errors.New("one or more P50/P95/P99 values exceed the strict goal.md §S18 threshold"))
	} else {
		sampleSet.Status = "pass"
		sampleSet.Validation = "candidate and upstream semantic predicates passed for all paired ABBA samples"
	}
	return sampleSet, evidence
}

func e2eCompleteS18ABBATiming(evidence *acceptreport.ABBAEvidence, sampleSet e2eSampleSet, tracePath string, fixture e2eS18Fixture, workspace string) error {
	if evidence == nil {
		return errors.New("ABBA evidence is missing")
	}
	data, err := os.ReadFile(tracePath)
	if err != nil {
		return fmt.Errorf("read nested timing trace: %w", err)
	}
	var trace e2eNestedTimingReport
	if err := json.Unmarshal(data, &trace); err != nil {
		return fmt.Errorf("decode nested timing trace: %w", err)
	}
	if trace.Version != 1 || trace.Language != e2eS18NestedTraceBackendLanguage(fixture) || trace.SampleLimit < e2eS18ABBARequiredSamples ||
		trace.SamplesTruncated || len(trace.Samples) == 0 || len(trace.Samples) > trace.SampleLimit {
		return fmt.Errorf("nested trace header/retention invalid (version=%d language=%q limit=%d truncated=%t samples=%d)", trace.Version, trace.Language, trace.SampleLimit, trace.SamplesTruncated, len(trace.Samples))
	}
	byParentID := make(map[string]e2eNestedTimingSample)
	duplicateParentIDs := make(map[string]struct{})
	var correlationErr error
	for _, sample := range trace.Samples {
		if len(sample.ParentRequestID) == 0 || string(sample.ParentRequestID) == "null" {
			continue
		}
		id, err := e2eNumericJSONRPCID(sample.ParentRequestID)
		if err != nil {
			if correlationErr == nil {
				correlationErr = fmt.Errorf("nested timing trace has malformed parent request ID: %w", err)
			}
			continue
		}
		if _, exists := byParentID[id]; exists {
			delete(byParentID, id)
			duplicateParentIDs[id] = struct{}{}
			continue
		}
		if _, duplicate := duplicateParentIDs[id]; duplicate {
			continue
		}
		byParentID[id] = sample
	}
	wantURI := uri.FromPath(filepath.Join(workspace, filepath.FromSlash(fixture.queryFile))).String()
	for roundIndex := range evidence.Rounds {
		for legIndex := range evidence.Rounds[roundIndex].Legs {
			leg := &evidence.Rounds[roundIndex].Legs[legIndex]
			if leg.Role != "candidate" {
				continue
			}
			if leg.SampleStart < 0 || leg.SampleStart+leg.SampleCount > len(sampleSet.SamplesNS) || len(leg.RequestIDs) != leg.SampleCount {
				if correlationErr == nil {
					correlationErr = fmt.Errorf("candidate leg %d/%d has invalid request/sample references", roundIndex, legIndex)
				}
				continue
			}
			leg.WriteRawNS = nil
			leg.WaitRawNS = nil
			for i, id := range leg.RequestIDs {
				if _, duplicate := duplicateParentIDs[id]; duplicate {
					if correlationErr == nil {
						correlationErr = fmt.Errorf("nested trace duplicated candidate parent request ID %s", id)
					}
					continue
				}
				sample, ok := byParentID[id]
				if !ok {
					if correlationErr == nil {
						correlationErr = fmt.Errorf("nested trace omitted candidate parent request ID %s", id)
					}
					continue
				}
				if sample.Method != evidence.Method || sample.URI != wantURI || sample.Outcome != "response" {
					if correlationErr == nil {
						correlationErr = fmt.Errorf("nested trace request %s method/URI/outcome mismatch: %s %s %s", id, sample.Method, sample.URI, sample.Outcome)
					}
					continue
				}
				if sample.DurationNS > uint64(math.MaxInt64) || sample.WriteDurationNS > uint64(math.MaxInt64) || sample.WaitDurationNS > uint64(math.MaxInt64) {
					if correlationErr == nil {
						correlationErr = fmt.Errorf("nested trace request %s duration exceeds int64", id)
					}
					continue
				}
				outer := sampleSet.SamplesNS[leg.SampleStart+i]
				durationNS := int64(sample.DurationNS)
				writeNS, waitNS := int64(sample.WriteDurationNS), int64(sample.WaitDurationNS)
				if outer <= 0 || durationNS < writeNS || durationNS-writeNS < waitNS || outer < writeNS || outer-writeNS < waitNS {
					if correlationErr == nil {
						correlationErr = fmt.Errorf("candidate request %s has invalid child phases or negative outer-minus-child overhead", id)
					}
					continue
				}
				leg.WriteRawNS = append(leg.WriteRawNS, float64(writeNS))
				leg.WaitRawNS = append(leg.WaitRawNS, float64(waitNS))
			}
		}
	}
	if correlationErr != nil {
		return correlationErr
	}
	if len(evidence.Rounds) != e2eS18ABBARounds {
		return errors.New("ABBA candidate/upstream rounds are incomplete")
	}
	return nil
}

func e2eS18NestedTraceBackendLanguage(fixture e2eS18Fixture) string {
	// C and C++ share ccls, whose nested RPC identity is "cpp" for both
	// document language IDs. The per-request URI check below distinguishes the
	// two fixtures and prevents a C++ trace sample from being attributed to C.
	if fixture.id == "c" || fixture.id == "cpp" {
		return "cpp"
	}
	return fixture.id
}

func e2eS18UpstreamInitializeCapabilities(fixture e2eS18Fixture) (map[string]any, bool) {
	if fixture.id == "c" || fixture.id == "cpp" {
		// ccls initializes its clangd child with an empty capabilities object.
		return map[string]any{}, true
	}
	return nil, false
}

func e2eNumericJSONRPCID(raw json.RawMessage) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	var text string
	switch typed := value.(type) {
	case json.Number:
		text = typed.String()
	case string:
		text = typed
	default:
		return "", fmt.Errorf("request ID is not numeric: %s", raw)
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parsed <= 0 {
		return "", fmt.Errorf("invalid numeric request ID %q", text)
	}
	return strconv.FormatInt(parsed, 10), nil
}

func e2eS18ExceptionScope(fixtureID, operation string) bool {
	_, ok := e2eS18ExceptionThreshold(fixtureID, operation)
	return ok
}

func e2eNewS18ABBAEvidence(checkID string, fixture e2eS18Fixture, operation, method string) acceptreport.ABBAEvidence {
	return acceptreport.ABBAEvidence{
		CheckID: checkID, FixtureID: fixture.id, Operation: operation, Method: method,
		Rounds: []acceptreport.ABBARound{},
	}
}

func e2eS18CandidateEnv(toolPath, tracePath, workspace string) []string {
	env := []string{
		"OMNILSP_TRUST=trusted", "OMNILSP_LOG_LEVEL=error", "OMNILSP_MAX_CONCURRENT=8",
		"OMNILSP_MAX_QUEUE=64", "GOMAXPROCS=8", "PATH=" + toolPath,
		"OMNILSP_S18_NESTED_RPC_TRACE=" + tracePath,
	}
	if phaseTracePath := strings.TrimSpace(os.Getenv(e2eS18CompletionPhaseTraceEnv)); phaseTracePath != "" {
		env = append(env, e2eS18CompletionPhaseTraceEnv+"="+phaseTracePath)
		for _, name := range []string{acceptanceRunIDEnv, candidateSHAEnv} {
			if value := strings.TrimSpace(os.Getenv(name)); value != "" {
				env = append(env, name+"="+value)
			}
		}
	}
	if workspace != "" {
		env = append(env, "OMNILSP_INDEX_DIR="+filepath.Join(workspace, ".omnilsp-index"))
		candidateTarget, _ := e2eS18CargoTargetDirs(workspace)
		env = append(env, "CARGO_TARGET_DIR="+candidateTarget)
	}
	return env
}

func e2eCloseS18Candidate(t testing.TB, candidate *lspdriver.Session) bool {
	t.Helper()
	if candidate == nil {
		return true
	}
	failedBeforeClose := t.Failed()
	candidate.Close(t)
	return !failedBeforeClose && !t.Failed()
}

func e2eS18CargoTargetDirs(workspace string) (candidate, upstream string) {
	parent := filepath.Dir(workspace)
	return filepath.Join(parent, "candidate-cargo-target"), filepath.Join(parent, "upstream-cargo-target")
}

func e2eOpenS18FixtureFiles(t *testing.T, session *lspdriver.Session, fixture e2eS18Fixture, files map[string]string, workspace string) {
	t.Helper()
	for _, rel := range fixture.openFiles {
		text, ok := files[rel]
		if !ok {
			continue
		}
		session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri":        uri.FromPath(filepath.Join(workspace, filepath.FromSlash(rel))).String(),
			"languageId": fixture.languageIDFor(rel), "version": 1, "text": text,
		}})
	}
}

// e2eParseS18Filter accepts comma-separated fixture IDs or fixture/operation
// pairs for focused diagnosis. An empty filter means the full S18 matrix.
func e2eParseS18Filter(value string) map[string]struct{} {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	selected := make(map[string]struct{})
	for _, item := range strings.Split(value, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		if item != "" {
			selected[item] = struct{}{}
		}
	}
	return selected
}

func e2eS18FilterSelects(filter map[string]struct{}, fixtureID, operation string) bool {
	if len(filter) == 0 {
		return true
	}
	_, languageSelected := filter[strings.ToLower(fixtureID)]
	_, operationSelected := filter[strings.ToLower(fixtureID+"/"+operation)]
	return languageSelected || operationSelected
}

func e2eStartS18Upstream(binary string, fixture e2eS18Fixture, files map[string]string, workspace, toolPath string, readinessProbe e2eS18OperationSpec) (*upstream.Session, e2eS18ReadinessEvidence, error) {
	env := e2eS18UpstreamEnv(fixture, toolPath, workspace)
	session, err := upstream.Start(binary, fixture.upstreamArgs, workspace, env)
	if err != nil {
		return nil, e2eS18ReadinessEvidence{Status: "not_verified"}, err
	}
	progressCursor := session.NotificationCursor()
	closeOnError := func(evidence e2eS18ReadinessEvidence, err error) (*upstream.Session, e2eS18ReadinessEvidence, error) {
		if closeErr := session.Close(); closeErr != nil {
			return nil, evidence, fmt.Errorf("%w; stop failed upstream: %v", err, closeErr)
		}
		return nil, evidence, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	if capabilities, ok := e2eS18UpstreamInitializeCapabilities(fixture); ok {
		err = session.InitializeWithCapabilities(ctx, workspace, capabilities)
	} else {
		err = session.Initialize(ctx, workspace)
	}
	cancel()
	if err != nil {
		return closeOnError(e2eS18ReadinessEvidence{Status: "not_verified"}, err)
	}
	for _, rel := range fixture.openFiles {
		text, ok := files[rel]
		if !ok {
			continue
		}
		if err := session.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri":        uri.FromPath(filepath.Join(workspace, filepath.FromSlash(rel))).String(),
			"languageId": fixture.languageIDFor(rel), "version": 1, "text": text,
		}}); err != nil {
			return closeOnError(e2eS18ReadinessEvidence{Status: "not_verified"}, fmt.Errorf("open %s: %w", rel, err))
		}
	}
	evidence, err := e2eWaitForS18UpstreamReady(session, fixture, readinessProbe, progressCursor, e2eS18UpstreamReadyTimeout)
	if err != nil {
		return closeOnError(evidence, err)
	}
	return session, evidence, nil
}

func e2eS18UpstreamEnv(fixture e2eS18Fixture, toolPath, workspace string) []string {
	env := []string{"PATH=" + toolPath}
	if fixture.id == "rust" {
		_, upstreamTarget := e2eS18CargoTargetDirs(workspace)
		env = append(env, "CARGO_TARGET_DIR="+upstreamTarget)
	}
	return env
}

type e2eS18ProgressState struct {
	active map[string]struct{}
	begin  int
	end    int
}

func e2eS18ProgressStateFromNotifications(notifications []upstream.Notification) (e2eS18ProgressState, error) {
	state := e2eS18ProgressState{active: make(map[string]struct{})}
	for _, notification := range notifications {
		if notification.Method != "$/progress" {
			continue
		}
		var params struct {
			Token json.RawMessage `json:"token"`
			Value struct {
				Kind string `json:"kind"`
			} `json:"value"`
		}
		if err := json.Unmarshal(notification.Params, &params); err != nil {
			return state, fmt.Errorf("decode upstream $/progress notification: %w", err)
		}
		if len(params.Token) == 0 || string(params.Token) == "null" {
			return state, errors.New("upstream $/progress notification omitted token")
		}
		token := string(params.Token)
		switch params.Value.Kind {
		case "begin":
			state.active[token] = struct{}{}
			state.begin++
		case "end":
			delete(state.active, token)
			state.end++
		case "report":
		default:
			return state, fmt.Errorf("upstream $/progress notification has unsupported kind %q", params.Value.Kind)
		}
	}
	return state, nil
}

func e2eS18ReadinessParams(params any, token string) any {
	requestParams, ok := params.(map[string]any)
	if !ok {
		return params
	}
	copy := make(map[string]any, len(requestParams)+1)
	for key, value := range requestParams {
		copy[key] = value
	}
	copy["workDoneToken"] = token
	return copy
}

func e2eS18NextStableObservationCount(previous, current any, count int) int {
	if count == 0 || !reflect.DeepEqual(previous, current) {
		return 1
	}
	return count + 1
}

// e2eWaitForS18UpstreamReady does not infer readiness from initialize or a
// fixed delay. It requires three consecutive validated, normalized semantic
// observations to match and refuses readiness while reported work is active.
func e2eWaitForS18UpstreamReady(session *upstream.Session, fixture e2eS18Fixture, check e2eS18OperationSpec, progressCursor uint64, timeout time.Duration) (e2eS18ReadinessEvidence, error) {
	started := time.Now()
	evidence := e2eS18ReadinessEvidence{Status: "not_verified", ProbeMethod: check.method}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ticker := time.NewTicker(e2eS18ReadyPollInterval)
	defer ticker.Stop()
	var previous any
	stableCount := 0
	for {
		if err := ctx.Err(); err != nil {
			evidence.StableObservationCount = stableCount
			evidence.ElapsedMillis = time.Since(started).Milliseconds()
			active := append([]string(nil), evidence.ActiveProgressTokens...)
			return evidence, fmt.Errorf("%w: pinned %s upstream was not semantically ready within %s (attempts=%d stable=%d/%d active_progress=%v last=%s)", errE2ENotVerified, fixture.name, timeout, evidence.AttemptCount, stableCount, e2eS18ReadyStableCount, active, evidence.LastObservationProblem)
		}
		notifications, overflow := session.NotificationsSince(progressCursor)
		if overflow {
			evidence.ElapsedMillis = time.Since(started).Milliseconds()
			return evidence, fmt.Errorf("%w: pinned %s upstream progress history overflowed before readiness could be established", errE2ENotVerified, fixture.name)
		}
		progress, progressErr := e2eS18ProgressStateFromNotifications(notifications)
		if progressErr != nil {
			evidence.ElapsedMillis = time.Since(started).Milliseconds()
			return evidence, fmt.Errorf("%w: pinned %s upstream progress was malformed: %v", errE2ENotVerified, fixture.name, progressErr)
		}
		e2eRecordS18ProgressEvidence(&evidence, progress)

		evidence.AttemptCount++
		requestParams := e2eS18ReadinessParams(check.params, fmt.Sprintf("omnilsp-s18-ready-%s-%d", fixture.id, evidence.AttemptCount))
		requestCtx, requestCancel := context.WithTimeout(ctx, 10*time.Second)
		raw, requestErr := session.RequestContext(requestCtx, check.method, requestParams)
		requestCancel()
		var normalized any
		if requestErr == nil {
			requestErr = check.validate(raw)
		}
		if requestErr == nil {
			normalized, requestErr = e2eS18ReadinessObservation(fixture, check.method, e2eExpectedS18Symbol(fixture, check.method), raw)
		}
		if requestErr != nil {
			evidence.LastObservationProblem = requestErr.Error()
			stableCount = 0
		} else {
			evidence.LastObservationProblem = ""
		}

		// Re-read progress after each response: indexing may have begun while
		// the request was running, and a result during that work is not ready.
		notifications, overflow = session.NotificationsSince(progressCursor)
		if overflow {
			evidence.ElapsedMillis = time.Since(started).Milliseconds()
			return evidence, fmt.Errorf("%w: pinned %s upstream progress history overflowed before readiness could be established", errE2ENotVerified, fixture.name)
		}
		progress, progressErr = e2eS18ProgressStateFromNotifications(notifications)
		if progressErr != nil {
			evidence.ElapsedMillis = time.Since(started).Milliseconds()
			return evidence, fmt.Errorf("%w: pinned %s upstream progress was malformed: %v", errE2ENotVerified, fixture.name, progressErr)
		}
		e2eRecordS18ProgressEvidence(&evidence, progress)
		if requestErr == nil && len(progress.active) == 0 {
			stableCount = e2eS18NextStableObservationCount(previous, normalized, stableCount)
			previous = normalized
		} else {
			stableCount = 0
			previous = nil
		}
		evidence.StableObservationCount = stableCount
		evidence.ElapsedMillis = time.Since(started).Milliseconds()
		if stableCount >= e2eS18ReadyStableCount {
			evidence.Status = "passed"
			return evidence, nil
		}

		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

func e2eS18ReadinessObservation(f e2eS18Fixture, method, expectedSymbol string, raw json.RawMessage) (any, error) {
	if method == "textDocument/completion" && (f.id == "c" || f.id == "cpp") {
		snapshot, err := e2eDecodeCompletionSnapshot(raw)
		if err != nil {
			return nil, err
		}
		if err := validateE2ECompletionFor(raw, expectedSymbol); err != nil {
			return nil, err
		}
		return snapshot.Semantics, nil
	}
	return e2eNormalizeS18Observation(f, method, expectedSymbol, raw)
}

func e2eRecordS18ProgressEvidence(evidence *e2eS18ReadinessEvidence, state e2eS18ProgressState) {
	evidence.ProgressBeginCount = state.begin
	evidence.ProgressEndCount = state.end
	evidence.ActiveProgressTokens = evidence.ActiveProgressTokens[:0]
	for token := range state.active {
		evidence.ActiveProgressTokens = append(evidence.ActiveProgressTokens, token)
	}
	sort.Strings(evidence.ActiveProgressTokens)
}

type e2eS18Fixture struct {
	id             string
	name           string
	languageID     string
	queryFile      string
	targetFile     string
	queryToken     string
	expectedSymbol string
	openFiles      []string
	toolNames      []string
	upstreamBinary string
	upstreamArgs   []string
}

type e2eS18AuditManifest struct {
	CoverageScope string                `json:"coverageScope"`
	CorpusFiles   []string              `json:"corpusFiles"`
	Fixtures      []e2eS18FixtureRecord `json:"representativeFixtures"`
}

type e2eS18FixtureRecord struct {
	ID             string   `json:"id"`
	Language       string   `json:"language"`
	LanguageID     string   `json:"languageId"`
	QueryFile      string   `json:"queryFile"`
	QueryToken     string   `json:"queryToken"`
	TargetFile     string   `json:"targetFile"`
	ExpectedSymbol string   `json:"expectedSymbol"`
	OpenFiles      []string `json:"openFiles"`
	ToolNames      []string `json:"toolNames"`
	UpstreamBinary string   `json:"upstreamBinary"`
	UpstreamArgs   []string `json:"upstreamArgs"`
}

func e2eBuildS18AuditManifest(files map[string]string) e2eS18AuditManifest {
	corpusFiles := make([]string, 0, len(files))
	for path := range files {
		corpusFiles = append(corpusFiles, filepath.ToSlash(path))
	}
	sort.Strings(corpusFiles)
	manifest := e2eS18AuditManifest{
		CoverageScope: "representative Tier S performance fixtures; not exhaustive language or tool conformance",
		CorpusFiles:   corpusFiles,
		Fixtures:      make([]e2eS18FixtureRecord, 0, len(e2eS18Fixtures("", files))),
	}
	for _, fixture := range e2eS18Fixtures("", files) {
		manifest.Fixtures = append(manifest.Fixtures, e2eS18FixtureRecord{
			ID: fixture.id, Language: fixture.name, LanguageID: fixture.languageID,
			QueryFile: fixture.queryFile, QueryToken: fixture.queryToken,
			TargetFile: fixture.targetFile, ExpectedSymbol: fixture.expectedSymbol,
			OpenFiles: append([]string(nil), fixture.openFiles...), ToolNames: append([]string(nil), fixture.toolNames...),
			UpstreamBinary: fixture.upstreamBinary, UpstreamArgs: append([]string(nil), fixture.upstreamArgs...),
		})
	}
	return manifest
}

func e2eS18Fixtures(workspace string, files map[string]string) []e2eS18Fixture {
	clangdArgs := cclsbackend.CommandArgs(workspace)
	return []e2eS18Fixture{
		{id: "go", name: "Go", languageID: "go", queryFile: "use.go", targetFile: "src01.go", queryToken: "sym050", expectedSymbol: "use", openFiles: []string{"src01.go", "use.go"}, toolNames: []string{"go", "gopls"}, upstreamBinary: "gopls", upstreamArgs: []string{"serve"}},
		{id: "c", name: "C", languageID: "c", queryFile: "c-perf.c", targetFile: "c-perf.c", queryToken: "targetPerf", expectedSymbol: "usePerf", openFiles: []string{"c-perf.c"}, toolNames: []string{"clangd"}, upstreamBinary: "clangd", upstreamArgs: append([]string(nil), clangdArgs...)},
		{id: "cpp", name: "C++", languageID: "cpp", queryFile: "cpp-perf.cpp", targetFile: "cpp-perf.cpp", queryToken: "targetPerf", expectedSymbol: "usePerf", openFiles: []string{"cpp-perf.cpp"}, toolNames: []string{"clangd"}, upstreamBinary: "clangd", upstreamArgs: append([]string(nil), clangdArgs...)},
		{id: "rust", name: "Rust", languageID: "rust", queryFile: "rust-perf/src/lib.rs", targetFile: "rust-perf/src/lib.rs", queryToken: "target_perf", expectedSymbol: "use_perf", openFiles: []string{"rust-perf/src/lib.rs"}, toolNames: []string{"rust-analyzer", "cargo"}, upstreamBinary: "rust-analyzer"},
		{id: "python", name: "Python", languageID: "python", queryFile: "python-perf.py", targetFile: "python-perf.py", queryToken: "target_perf", expectedSymbol: "use_perf", openFiles: []string{"python-perf.py"}, toolNames: []string{"pyright-langserver", "node"}, upstreamBinary: "pyright-langserver", upstreamArgs: []string{"--stdio"}},
		{id: "typescript", name: "TypeScript", languageID: "typescript", queryFile: "typescript-perf.ts", targetFile: "typescript-perf.ts", queryToken: "targetPerf", expectedSymbol: "usePerf", openFiles: []string{"typescript-perf.ts"}, toolNames: []string{"typescript-language-server", "tsc", "node"}, upstreamBinary: "typescript-language-server", upstreamArgs: []string{"--stdio"}},
		{id: "javascript", name: "JavaScript", languageID: "javascript", queryFile: "javascript-perf.js", targetFile: "javascript-perf.js", queryToken: "targetPerf", expectedSymbol: "usePerf", openFiles: []string{"javascript-perf.js"}, toolNames: []string{"typescript-language-server", "tsc", "node"}, upstreamBinary: "typescript-language-server", upstreamArgs: []string{"--stdio"}},
	}
}

func (f e2eS18Fixture) languageIDFor(rel string) string { return f.languageID }

type e2eS18OperationSpec struct {
	method                 string
	params                 any
	threshold              e2eThresholds
	validate               func(json.RawMessage) error
	compare                func(json.RawMessage, json.RawMessage) error
	compareSample          func(json.RawMessage, json.RawMessage) error
	presentationDifference func(json.RawMessage, json.RawMessage) (bool, error)
}

func e2eS18ValidateFullCompletionResponse(check e2eS18OperationSpec, raw json.RawMessage) error {
	if check.method != "textDocument/completion" {
		return fmt.Errorf("full-list validation requires textDocument/completion, got %q", check.method)
	}
	if check.validate != nil {
		if err := check.validate(raw); err != nil {
			return err
		}
	}
	if _, err := e2eDecodeCompletionSnapshot(raw); err != nil {
		return err
	}
	return nil
}

func e2eS18Operation(f e2eS18Fixture, workspace string, files map[string]string, name string) e2eS18OperationSpec {
	queryText := files[f.queryFile]
	queryLine, queryChar := e2ePositionOf(queryText, f.queryToken, 1)
	if queryChar == math.MaxUint32 {
		queryLine, queryChar = e2ePositionOf(queryText, f.queryToken, 0)
	}
	docURI := uri.FromPath(filepath.Join(workspace, filepath.FromSlash(f.queryFile))).String()
	params := map[string]any{"textDocument": map[string]string{"uri": docURI}, "position": map[string]uint32{"line": queryLine, "character": queryChar}}
	docParams := map[string]any{"textDocument": map[string]string{"uri": docURI}}
	threshold := e2eS18Thresholds()[name]
	switch name {
	case "hot_hover":
		return e2eS18OperationSpec{
			method:    "textDocument/hover",
			params:    params,
			threshold: threshold,
			validate:  func(raw json.RawMessage) error { return validateE2EHoverFor(raw, f.queryToken) },
			compare:   e2eCompareHoverSemantics,
			presentationDifference: func(candidate, baseline json.RawMessage) (bool, error) {
				candidatePresentation, err := e2eHoverPresentation(candidate)
				if err != nil {
					return false, err
				}
				baselinePresentation, err := e2eHoverPresentation(baseline)
				if err != nil {
					return false, err
				}
				return candidatePresentation != baselinePresentation, nil
			},
		}
	case "hot_definition":
		targetText := files[f.targetFile]
		targetLine, targetChar, declarationErr := e2eDefinitionPosition(f, targetText)
		targetURI := uri.FromPath(filepath.Join(workspace, filepath.FromSlash(f.targetFile))).Canonical()
		validator := func(raw json.RawMessage) error {
			if declarationErr != nil {
				return fmt.Errorf("locate fixture declaration: %w", declarationErr)
			}
			locations, err := decodeE2ELocations(raw)
			if err != nil {
				return err
			}
			end := targetChar + uint32(len(f.queryToken))
			if !e2EContainsLocation(locations, targetURI, targetLine, targetChar, targetLine, end) {
				return fmt.Errorf("normalized definition did not include %s at %d:%d-%d; got %+v", targetURI, targetLine, targetChar, end, locations)
			}
			return nil
		}
		return e2eS18OperationSpec{method: "textDocument/definition", params: params, threshold: threshold, validate: validator, compare: func(candidate, baseline json.RawMessage) error {
			if err := validator(candidate); err != nil {
				return err
			}
			if err := validator(baseline); err != nil {
				return err
			}
			candidateLocations, err := decodeE2ELocations(candidate)
			if err != nil {
				return err
			}
			baselineLocations, err := decodeE2ELocations(baseline)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(e2eSortedLocations(candidateLocations), e2eSortedLocations(baselineLocations)) {
				return fmt.Errorf("normalized definition differs from upstream: candidate=%+v upstream=%+v", candidateLocations, baselineLocations)
			}
			return nil
		}}
	case "completion_first_usable":
		compare := func(candidate, baseline json.RawMessage) error {
			if err := validateE2ECompletionFor(candidate, f.queryToken); err != nil {
				return err
			}
			if err := validateE2ECompletionFor(baseline, f.queryToken); err != nil {
				return err
			}
			if f.id == "c" || f.id == "cpp" {
				return e2eCompareS18CompletionTargetSemantics(candidate, baseline, f.queryToken)
			}
			candidateLabels, err := e2eNormalizedCompletionLabels(candidate, f.queryToken)
			if err != nil {
				return err
			}
			baselineLabels, err := e2eNormalizedCompletionLabels(baseline, f.queryToken)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(candidateLabels, baselineLabels) {
				return fmt.Errorf("normalized expected completion labels differ from pinned upstream: candidate=%v upstream=%v", candidateLabels, baselineLabels)
			}
			return nil
		}
		var compareSample func(json.RawMessage, json.RawMessage) error
		var presentationDifference func(json.RawMessage, json.RawMessage) (bool, error)
		if f.id == "c" || f.id == "cpp" {
			compare = func(candidate, baseline json.RawMessage) error {
				return e2eCompareS18CompletionTargetSemantics(candidate, baseline, f.queryToken)
			}
			compareSample = func(candidate, baseline json.RawMessage) error {
				return e2eCompareS18CompletionTargetSemantics(candidate, baseline, f.queryToken)
			}
			presentationDifference = e2eS18CompletionPresentationDifference
		}
		return e2eS18OperationSpec{
			method: "textDocument/completion", params: params, threshold: threshold,
			validate: func(raw json.RawMessage) error { return validateE2ECompletionFor(raw, f.queryToken) },
			compare:  compare, compareSample: compareSample, presentationDifference: presentationDifference,
		}
	default:
		return e2eS18OperationSpec{method: "textDocument/documentSymbol", params: docParams, threshold: threshold, validate: func(raw json.RawMessage) error { return validateE2ESymbolsFor(raw, f.expectedSymbol) }, compare: func(candidate, baseline json.RawMessage) error {
			return e2eCompareS18DocumentSymbols(candidate, baseline, f, f.expectedSymbol)
		}}
	}
}

func e2eNewS18SemanticEvidence(f e2eS18Fixture, method string) *e2eS18SemanticEvidence {
	return &e2eS18SemanticEvidence{
		Method: method, ExpectedSymbol: e2eExpectedS18Symbol(f, method),
		CandidateObservationStatus: "not_verified", CandidateValidationStatus: "not_verified",
		UpstreamObservationStatus: "not_verified", UpstreamValidationStatus: "not_verified",
		DifferentialStatus: "not_verified",
	}
}

func e2eExpectedS18Symbol(f e2eS18Fixture, method string) string {
	switch method {
	case "textDocument/hover", "textDocument/definition", "textDocument/completion":
		return f.queryToken
	default:
		return f.expectedSymbol
	}
}

func e2eSemanticEvidenceStatus(err error) string {
	if err == nil {
		return "passed"
	}
	if e2eErrorStatus(err) == "not_verified" {
		return "not_verified"
	}
	return "failed"
}

func e2eRecordS18CompletionTargetSampleFailure(sampleSet *e2eSampleSet, err error) {
	if sampleSet == nil || sampleSet.SemanticEvidence == nil {
		return
	}
	status := e2eSemanticEvidenceStatus(err)
	sampleSet.SemanticEvidence.TargetSampleStatus = status
	sampleSet.SemanticEvidence.DifferentialMatched = false
	sampleSet.SemanticEvidence.DifferentialStatus = status
}

func e2eS18EditUpdateObserved(method string, update map[string]any) map[string]any {
	empty := []string{}
	if method != "textDocument/documentSymbol" {
		return map[string]any{
			"status": "not_applicable", "changed": false, "freshness_verified": false,
			"old_symbol": "", "new_symbol": "", "before_symbols": empty,
			"after_symbols": empty, "upstream_after_symbols": empty,
		}
	}
	if update == nil {
		return map[string]any{
			"status": "not_verified", "changed": false, "freshness_verified": false,
			"old_symbol": "", "new_symbol": "", "before_symbols": empty,
			"after_symbols": empty, "upstream_after_symbols": empty,
		}
	}
	changed, _ := update["changed"].(bool)
	freshnessVerified, _ := update["freshnessVerified"].(bool)
	oldSymbol, _ := update["oldSymbol"].(string)
	newSymbol, _ := update["newSymbol"].(string)
	beforeSymbols := e2eStringSlice(update["beforeSymbols"])
	afterSymbols := e2eStringSlice(update["afterSymbols"])
	upstreamAfterSymbols := e2eStringSlice(update["upstreamAfterSymbols"])
	status := "not_verified"
	if changed && freshnessVerified && oldSymbol != "" && newSymbol != "" && oldSymbol != newSymbol &&
		containsE2EString(beforeSymbols, oldSymbol) && !containsE2EString(afterSymbols, oldSymbol) &&
		containsE2EString(afterSymbols, newSymbol) && reflect.DeepEqual(afterSymbols, upstreamAfterSymbols) {
		status = "passed"
	}
	return map[string]any{
		"status": status, "changed": changed, "freshness_verified": freshnessVerified,
		"old_symbol": oldSymbol, "new_symbol": newSymbol,
		"before_symbols": beforeSymbols, "after_symbols": afterSymbols,
		"upstream_after_symbols": upstreamAfterSymbols,
	}
}

func e2eStringSlice(value any) []string {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return []string{}
			}
			result = append(result, text)
		}
		return result
	default:
		return []string{}
	}
}

func containsE2EString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func e2eNormalizeS18Observation(f e2eS18Fixture, method, expectedSymbol string, raw json.RawMessage) (any, error) {
	switch method {
	case "textDocument/hover":
		return e2eHoverSemanticContent(raw)
	case "textDocument/definition":
		locations, err := decodeE2ELocations(raw)
		if err != nil {
			return nil, err
		}
		return e2eSortedLocations(locations), nil
	case "textDocument/completion":
		return e2eNormalizedCompletionLabels(raw, expectedSymbol)
	case "textDocument/documentSymbol":
		return e2eS18SymbolShapes(raw, f, expectedSymbol)
	default:
		return nil, fmt.Errorf("%w: no normalized S18 observation for %q", errE2ENotVerified, method)
	}
}

func e2eBuildS18SemanticEvidence(f e2eS18Fixture, check e2eS18OperationSpec, expectedSymbol string, candidate, upstreamResult json.RawMessage, compareErr error) (*e2eS18SemanticEvidence, error) {
	evidence := e2eNewS18SemanticEvidence(f, check.method)
	evidence.ExpectedSymbol = expectedSymbol
	completionTargetOnly := check.method == "textDocument/completion" && (f.id == "c" || f.id == "cpp")
	if completionTargetOnly {
		evidence.FullListSnapshotStatus = "not_verified"
		evidence.FullListSampleStatus = "not_verified"
		evidence.TargetSampleStatus = "not_verified"
	}
	candidateValidationErr := check.validate(candidate)
	evidence.CandidateValidationStatus = e2eSemanticEvidenceStatus(candidateValidationErr)
	upstreamValidationErr := check.validate(upstreamResult)
	evidence.UpstreamValidationStatus = e2eSemanticEvidenceStatus(upstreamValidationErr)
	if normalized, err := e2eNormalizeS18Observation(f, check.method, evidence.ExpectedSymbol, candidate); err == nil {
		evidence.CandidateNormalized = normalized
		evidence.CandidateObservationStatus = "passed"
	} else {
		evidence.CandidateObservationStatus = e2eSemanticEvidenceStatus(err)
	}
	if normalized, err := e2eNormalizeS18Observation(f, check.method, evidence.ExpectedSymbol, upstreamResult); err == nil {
		evidence.UpstreamNormalized = normalized
		evidence.UpstreamObservationStatus = "passed"
	} else {
		evidence.UpstreamObservationStatus = e2eSemanticEvidenceStatus(err)
	}
	if evidence.CandidateValidationStatus != "passed" || evidence.UpstreamValidationStatus != "passed" ||
		evidence.CandidateObservationStatus != "passed" || evidence.UpstreamObservationStatus != "passed" {
		evidence.DifferentialStatus = "not_verified"
		return evidence, nil
	}
	if compareErr != nil {
		evidence.DifferentialStatus = e2eSemanticEvidenceStatus(compareErr)
		if completionTargetOnly {
			evidence.TargetSampleStatus = evidence.DifferentialStatus
		}
		return evidence, nil
	}
	if !reflect.DeepEqual(evidence.CandidateNormalized, evidence.UpstreamNormalized) {
		evidence.DifferentialStatus = "failed"
		return evidence, nil
	}
	evidence.DifferentialMatched = true
	evidence.DifferentialStatus = "passed"
	return evidence, nil
}

func e2eRunS18Samples(t *testing.T, session *lspdriver.Session, upstreamSession *upstream.Session, f e2eS18Fixture, files map[string]string, workspace string, check e2eS18OperationSpec, phaseTimingEnabled bool) e2eSampleSet {
	t.Helper()
	fullListCheck := e2eS18RequiresCompletionFullList(f.id, check.method)
	sampleSet := e2eSampleSet{
		Language: f.name, Status: "running", Method: check.method, Thresholds: check.threshold,
		Validation: "not checked", Differential: "not checked", SemanticEvidence: e2eNewS18SemanticEvidence(f, check.method),
	}
	if fullListCheck {
		sampleSet.SemanticEvidence.FullListSnapshotStatus = "not_verified"
		sampleSet.SemanticEvidence.FullListSampleStatus = "not_verified"
		sampleSet.SemanticEvidence.TargetSampleStatus = "not_verified"
	}
	validateCandidateResponse := check.validate
	if fullListCheck {
		validateCandidateResponse = func(raw json.RawMessage) error {
			return e2eS18ValidateFullCompletionResponse(check, raw)
		}
	}
	recordFullListFailure := func(label string, err error) {
		sampleSet.SemanticEvidence.FullListSnapshotStatus = "failed"
		sampleSet.SemanticEvidence.FullListSampleStatus = "failed"
		e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, fmt.Errorf("%s: %w", label, err))
	}
	if exceptionThreshold, ok := e2eS18ExceptionThreshold(f.id, "syntax_update_after_edit"); ok && check.method == "textDocument/documentSymbol" {
		sampleSet.ExceptionThresholds = &exceptionThreshold
	}
	oracleClosed := false
	closeOracle := func() error {
		if upstreamSession == nil || oracleClosed {
			return nil
		}
		oracleClosed = true
		return upstreamSession.Close()
	}
	defer func() {
		if err := closeOracle(); err != nil {
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureProcess, fmt.Errorf("pinned upstream process shutdown: %w", err))
		}
	}()
	queryText := files[f.queryFile]
	docURI := uri.FromPath(filepath.Join(workspace, filepath.FromSlash(f.queryFile))).String()
	versions := int64(1)
	marker := byte('A')
	markerOffset := e2eEditMarkerOffset(queryText)
	if markerOffset < 0 || markerOffset >= len(queryText) {
		e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, errors.New("fixture is missing a safe comment/string edit marker"))
		return sampleSet
	}
	request := func() (json.RawMessage, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return session.RequestContext(ctx, check.method, check.params)
	}
	baselineCtx, cancelBaseline := context.WithTimeout(context.Background(), 30*time.Second)
	baseline, baselineErr := upstreamSession.RequestContext(baselineCtx, check.method, check.params)
	cancelBaseline()
	if baselineErr != nil {
		sampleSet.SemanticEvidence.UpstreamValidationStatus = e2eSemanticEvidenceStatus(baselineErr)
		sampleSet.Validation = "pinned upstream request could not be completed"
		e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureKindOf(baselineErr), fmt.Errorf("blocked: upstream %s failed: %w", check.method, baselineErr))
		return sampleSet
	}
	if err := check.validate(baseline); err != nil {
		sampleSet.SemanticEvidence.UpstreamValidationStatus = e2eSemanticEvidenceStatus(err)
		sampleSet.Validation = "pinned upstream did not provide the required semantic observation"
		e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureSemantic, fmt.Errorf("blocked: upstream baseline: %w", err))
		return sampleSet
	}
	sampleSet.SemanticEvidence.UpstreamValidationStatus = "passed"
	sampleSet.Differential = "candidate and pinned upstream both satisfy the same normalized semantic assertion"
	if check.method == "textDocument/completion" && (f.id == "c" || f.id == "cpp") {
		sampleSet.Differential = "each candidate completion response is validated as a complete list; only expected-symbol target semantics are compared with the pinned upstream response, so evolving full lists are not compared across requests"
	}
	if check.presentationDifference != nil {
		sampleSet.Presentation = "display differences are classified separately from semantic equality"
	}
	var syntaxNames []string
	currentDocumentSymbol := f.expectedSymbol
	if check.method == "textDocument/documentSymbol" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		candidateBaseline, err := session.RequestContext(ctx, check.method, check.params)
		cancel()
		if err != nil {
			sampleSet.SemanticEvidence.CandidateValidationStatus = e2eSemanticEvidenceStatus(err)
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureKindOf(err), fmt.Errorf("blocked: candidate document-symbol baseline failed: %w", err))
			return sampleSet
		}
		syntaxNames, err = e2eS18SymbolNames(candidateBaseline, f, currentDocumentSymbol)
		if err != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureSemantic, fmt.Errorf("blocked: candidate document-symbol baseline: %w", err))
			return sampleSet
		}
		upstreamNames, err := e2eS18SymbolNames(baseline, f, currentDocumentSymbol)
		if err != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureSemantic, fmt.Errorf("blocked: pinned upstream document-symbol baseline: %w", err))
			return sampleSet
		}
		baselineDiffErr := check.compare(candidateBaseline, baseline)
		if evidence, evidenceErr := e2eBuildS18SemanticEvidence(f, check, sampleSet.SemanticEvidence.ExpectedSymbol, candidateBaseline, baseline, baselineDiffErr); evidenceErr == nil {
			sampleSet.SemanticEvidence = evidence
		}
		if !reflect.DeepEqual(syntaxNames, upstreamNames) {
			sampleSet.SemanticEvidence.CandidateValidationStatus = "passed"
			sampleSet.SemanticEvidence.DifferentialStatus = "failed"
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, fmt.Errorf("normalized document-symbol baseline differs: candidate=%v upstream=%v", syntaxNames, upstreamNames))
			return sampleSet
		}
		sampleSet.SemanticEvidence.CandidateValidationStatus = "passed"
		symbolsBefore := append([]string(nil), syntaxNames...)
		updatedText, updatedSymbol, renamed := e2eRenameFunctionSymbol(queryText, f.expectedSymbol)
		if !renamed {
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, fmt.Errorf("fixture has no function declaration for edit target %q", f.expectedSymbol))
			return sampleSet
		}
		versions++
		session.Notify(t, "textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": docURI, "version": versions},
			"contentChanges": []map[string]string{{"text": updatedText}},
		})
		if err := upstreamSession.Notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": docURI, "version": versions},
			"contentChanges": []map[string]string{{"text": updatedText}},
		}); err != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureKindOf(err), fmt.Errorf("blocked: pinned upstream didChange failed: %w", err))
			return sampleSet
		}
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		candidateUpdated, err := session.RequestContext(ctx, check.method, check.params)
		cancel()
		if err != nil {
			e2eRecordS18Failure(&sampleSet, e2eErrorStatus(err), e2eFailureKindOf(err), fmt.Errorf("candidate didChange freshness query: %w", err))
			return sampleSet
		}
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		upstreamUpdated, err := upstreamSession.RequestContext(ctx, check.method, check.params)
		cancel()
		if err != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureKindOf(err), fmt.Errorf("blocked: pinned upstream didChange freshness query: %w", err))
			return sampleSet
		}
		updatedNames, err := e2eS18SymbolNames(candidateUpdated, f, updatedSymbol)
		if err != nil {
			e2eRecordS18Failure(&sampleSet, e2eErrorStatus(err), e2eFailureSemantic, fmt.Errorf("candidate didChange symbol normalization: %w", err))
			return sampleSet
		}
		upstreamUpdatedNames, err := e2eS18SymbolNames(upstreamUpdated, f, updatedSymbol)
		if err != nil {
			e2eRecordS18Failure(&sampleSet, "not_verified", e2eFailureSemantic, fmt.Errorf("blocked: pinned upstream didChange symbol normalization: %w", err))
			return sampleSet
		}
		expectedUpdatedNames := append([]string(nil), syntaxNames...)
		renamedExpected := false
		for i, name := range expectedUpdatedNames {
			if name == f.expectedSymbol {
				expectedUpdatedNames[i] = updatedSymbol
				renamedExpected = true
			}
		}
		updatedCheck := check
		updatedCheck.validate = func(raw json.RawMessage) error { return validateE2ESymbolsFor(raw, updatedSymbol) }
		updatedCheck.compare = func(candidate, pinned json.RawMessage) error {
			return e2eCompareS18DocumentSymbols(candidate, pinned, f, updatedSymbol)
		}
		updatedDiffErr := updatedCheck.compare(candidateUpdated, upstreamUpdated)
		if evidence, evidenceErr := e2eBuildS18SemanticEvidence(f, updatedCheck, updatedSymbol, candidateUpdated, upstreamUpdated, updatedDiffErr); evidenceErr == nil {
			sampleSet.SemanticEvidence = evidence
		}
		if !renamedExpected || !reflect.DeepEqual(updatedNames, expectedUpdatedNames) || !reflect.DeepEqual(updatedNames, upstreamUpdatedNames) {
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, fmt.Errorf("document-symbol response was stale or differs after unsaved rename: candidate=%v upstream=%v expected=%v", updatedNames, upstreamUpdatedNames, expectedUpdatedNames))
			return sampleSet
		}
		check.validate = func(raw json.RawMessage) error { return validateE2ESymbolsFor(raw, updatedSymbol) }
		check.compare = func(candidate, pinned json.RawMessage) error {
			return e2eCompareS18DocumentSymbols(candidate, pinned, f, updatedSymbol)
		}
		syntaxNames = updatedNames
		currentDocumentSymbol = updatedSymbol
		baseline = upstreamUpdated
		queryText = updatedText
		markerOffset = e2eEditMarkerOffset(queryText)
		if markerOffset < 0 || markerOffset >= len(queryText) {
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSemantic, errors.New("renamed document-symbol fixture lost its safe edit marker"))
			return sampleSet
		}
		sampleSet.EditUpdate = map[string]any{
			"changed": true, "oldSymbol": f.expectedSymbol, "newSymbol": updatedSymbol,
			"beforeSymbols": symbolsBefore,
			"afterSymbols":  updatedNames, "upstreamAfterSymbols": upstreamUpdatedNames,
			"freshnessVerified": true,
		}
		sampleSet.Differential = "normalized document symbols match pinned upstream before and after an unsaved function rename"
		sampleSet.SemanticEvidence.ExpectedSymbol = updatedSymbol
		sampleSet.SemanticEvidence.UpstreamValidationStatus = "passed"
	}
	// Retain the pinned oracle's normalized response as evidence, then fully
	// stop its process before candidate warm-up and the 1000 timed requests.
	if err := closeOracle(); err != nil {
		e2eRecordS18Failure(&sampleSet, "failed", e2eFailureProcess, fmt.Errorf("pinned upstream process shutdown before candidate timing: %w", err))
		return sampleSet
	}
	operation := func() (json.RawMessage, error) {
		if check.method == "textDocument/documentSymbol" {
			versions++
			if marker == 'A' {
				marker = 'B'
			} else {
				marker = 'A'
			}
			updated := []byte(queryText)
			updated[markerOffset] = marker
			session.Notify(t, "textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": docURI, "version": versions},
				"contentChanges": []map[string]string{{"text": string(updated)}},
			})
		}
		return request()
	}
	measurePhaseTiming := phaseTimingEnabled && (f.id == "c" || f.id == "cpp") && check.method == "textDocument/documentSymbol"
	operationWithPhaseTiming := func() (json.RawMessage, error, int64, int64) {
		var didChangeNotifyNS int64
		if check.method == "textDocument/documentSymbol" {
			versions++
			if marker == 'A' {
				marker = 'B'
			} else {
				marker = 'A'
			}
			updated := []byte(queryText)
			updated[markerOffset] = marker
			notifyStarted := time.Now()
			session.Notify(t, "textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": docURI, "version": versions},
				"contentChanges": []map[string]string{{"text": string(updated)}},
			})
			didChangeNotifyNS = time.Since(notifyStarted).Nanoseconds()
		}
		requestStarted := time.Now()
		raw, err := request()
		return raw, err, didChangeNotifyNS, time.Since(requestStarted).Nanoseconds()
	}
	for i := 0; i < 32; i++ {
		raw, err := operation()
		failureKind := e2eFailureKindOf(err)
		if err == nil {
			err = validateCandidateResponse(raw)
			if err != nil {
				failureKind = e2eFailureSemantic
				err = e2eWrapFailure(failureKind, err)
				if fullListCheck {
					recordFullListFailure(fmt.Sprintf("warm-up %d candidate full-list validation", i), err)
				} else if i > 0 && check.compareSample != nil {
					e2eRecordS18CompletionTargetSampleFailure(&sampleSet, err)
				}
			}
		}
		if err == nil {
			compare := check.compare
			if i > 0 && check.compareSample != nil {
				compare = check.compareSample
			}
			err = compare(raw, baseline)
			if err != nil && i > 0 && check.compareSample != nil {
				e2eRecordS18CompletionTargetSampleFailure(&sampleSet, err)
			}
			if err != nil {
				failureKind = e2eFailureSemantic
				err = e2eWrapFailure(failureKind, err)
			}
		}
		if i == 0 {
			evidence, evidenceErr := e2eBuildS18SemanticEvidence(f, check, sampleSet.SemanticEvidence.ExpectedSymbol, raw, baseline, err)
			sampleSet.SemanticEvidence = evidence
			if evidenceErr != nil && err == nil {
				err = evidenceErr
				failureKind = e2eFailureSemantic
				err = e2eWrapFailure(failureKind, err)
			}
			if err == nil && evidence.DifferentialStatus == "not_verified" {
				err = fmt.Errorf("%w: structured normalized semantic evidence is incomplete", errE2ENotVerified)
				failureKind = e2eFailureSemantic
				err = e2eWrapFailure(failureKind, err)
			} else if err == nil && evidence.DifferentialStatus == "failed" {
				err = errors.New("structured normalized semantic observations differ")
				failureKind = e2eFailureSemantic
				err = e2eWrapFailure(failureKind, err)
			}
		}
		if err == nil && i == 0 && check.presentationDifference != nil {
			displayDiffers, displayErr := check.presentationDifference(raw, baseline)
			switch {
			case displayErr != nil:
				sampleSet.Presentation = "semantic comparison passed; display-only classification unavailable: " + displayErr.Error()
			case displayDiffers:
				sampleSet.Presentation = "display-only Markdown difference observed; normalized semantic content matched"
			default:
				sampleSet.Presentation = "no display-only difference observed; normalized semantic content matched"
			}
		}
		if err == nil && check.method == "textDocument/documentSymbol" {
			err = e2eSameS18SymbolNames(raw, f, currentDocumentSymbol, syntaxNames)
			if err != nil {
				failureKind = e2eFailureSemantic
				err = e2eWrapFailure(failureKind, err)
			}
		}
		if err != nil {
			sampleSet.Validation = "failed"
			e2eRecordS18Failure(&sampleSet, e2eErrorStatus(err), failureKind, fmt.Errorf("warm-up %d: %w", i, err))
			return sampleSet
		}
		sampleSet.Validation = "normalized semantic result valid"
	}
	sampleSet.SamplesNS = make([]int64, 0, performanceSamples)
	for i := 0; i < performanceSamples; i++ {
		var raw json.RawMessage
		var err error
		var didChangeNotifyNS, documentSymbolRequestNS int64
		var duration int64
		if measurePhaseTiming {
			start := time.Now()
			raw, err, didChangeNotifyNS, documentSymbolRequestNS = operationWithPhaseTiming()
			duration = time.Since(start).Nanoseconds()
		} else {
			start := time.Now()
			raw, err = operation()
			duration = time.Since(start).Nanoseconds()
		}
		failureKind := e2eFailureKindOf(err)
		if err == nil {
			err = validateCandidateResponse(raw)
			if err != nil {
				failureKind = e2eFailureSemantic
				err = e2eWrapFailure(failureKind, err)
				if fullListCheck {
					recordFullListFailure(fmt.Sprintf("sample %d candidate full-list validation", i), err)
				} else if check.compareSample != nil {
					e2eRecordS18CompletionTargetSampleFailure(&sampleSet, err)
				}
			}
		}
		if err == nil {
			compare := check.compare
			if check.compareSample != nil {
				compare = check.compareSample
			}
			err = compare(raw, baseline)
			if err != nil && check.compareSample != nil {
				e2eRecordS18CompletionTargetSampleFailure(&sampleSet, err)
			}
			if err != nil {
				failureKind = e2eFailureSemantic
				err = e2eWrapFailure(failureKind, err)
			}
		}
		if err == nil && check.method == "textDocument/documentSymbol" {
			err = e2eSameS18SymbolNames(raw, f, currentDocumentSymbol, syntaxNames)
			if err != nil {
				failureKind = e2eFailureSemantic
				err = e2eWrapFailure(failureKind, err)
			}
		}
		if err != nil {
			e2eRecordS18Failure(&sampleSet, e2eErrorStatus(err), failureKind, fmt.Errorf("sample %d: %w", i, err))
			break
		}
		sampleSet.SamplesNS = append(sampleSet.SamplesNS, duration)
		if measurePhaseTiming {
			if sampleSet.PhaseTiming == nil {
				sampleSet.PhaseTiming = &e2eS18PhaseTiming{}
			}
			sampleSet.PhaseTiming.DidChangeNotifyNS = append(sampleSet.PhaseTiming.DidChangeNotifyNS, didChangeNotifyNS)
			sampleSet.PhaseTiming.DocumentSymbolRequestNS = append(sampleSet.PhaseTiming.DocumentSymbolRequestNS, documentSymbolRequestNS)
		}
	}
	sampleSet.SampleCount = len(sampleSet.SamplesNS)
	if sampleSet.SampleCount > 0 {
		sampleSet.P50NS = e2ePercentileNS(sampleSet.SamplesNS, .50)
		sampleSet.P95NS = e2ePercentileNS(sampleSet.SamplesNS, .95)
		sampleSet.P99NS = e2ePercentileNS(sampleSet.SamplesNS, .99)
	}
	if sampleSet.SampleCount < performanceSamples && sampleSet.Status != "running" {
		e2eRecordS18Failure(&sampleSet, sampleSet.Status, e2eFailureSampleMissing, nil)
	}
	if sampleSet.Status == "running" {
		if sampleSet.SampleCount < performanceSamples {
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureSampleMissing, fmt.Errorf("collected %d of %d required individual request samples", sampleSet.SampleCount, performanceSamples))
		} else if e2eS18StrictLatencyExceeded(sampleSet) {
			e2eRecordS18Failure(&sampleSet, "failed", e2eFailureLatency, errors.New("one or more P50/P95/P99 values exceed the strict goal.md §S18 threshold"))
		} else {
			sampleSet.Status = "pass"
		}
	}
	if fullListCheck && sampleSet.SampleCount == performanceSamples && len(sampleSet.SamplesNS) == performanceSamples {
		sampleSet.SemanticEvidence.FullListSnapshotStatus = "pending"
		sampleSet.SemanticEvidence.FullListSampleStatus = "pending"
		sampleSet.SemanticEvidence.TargetSampleStatus = "pending"
	}
	return sampleSet
}

func runE2ES19(t *testing.T, binary string) e2eS19Report {
	t.Helper()
	result := e2eS19Report{
		Status:       "running",
		Cancellation: e2eCancellationOutcome{Status: "not_verified"},
		Progress:     e2eProgressOutcome{Status: "not_verified"},
		NoStarvation: e2eNoStarvationOutcome{BudgetMillis: 2000, CompletionStatus: "not_verified", HoverStatus: "not_verified", Status: "not_verified"},
	}
	workspace := t.TempDir()
	content, tokenPositions, corpusErr := buildE2ES19Corpus()
	if corpusErr != nil {
		result.Status = "failed"
		result.Errors = append(result.Errors, corpusErr.Error())
		t.Errorf("prepare S19 corpus: %v", corpusErr)
		return result
	}
	if err := writeE2EFiles(workspace, map[string]string{
		"go.mod":        "module acceptance\n\ngo 1.26\n",
		"references.go": content,
	}); err != nil {
		result.Status = "failed"
		result.Errors = append(result.Errors, err.Error())
		t.Errorf("prepare S19 corpus: %v", err)
		return result
	}
	session := lspdriver.Start(t, binary, workspace, []string{
		"OMNILSP_TRUST=trusted", "OMNILSP_LOG_LEVEL=error", "OMNILSP_MAX_CONCURRENT=8",
		"OMNILSP_MAX_QUEUE=64", "GOMAXPROCS=8",
		"OMNILSP_INDEX_DIR=" + filepath.Join(workspace, ".omnilsp-index"),
		"OMNILSP_S18_COMPLETION_PHASE_TRACE=",
	})
	defer session.Close(t)
	session.Initialize(t)
	result.Resources = append(result.Resources, e2eProcessTreeResources(os.Getpid(), "initialized"))
	docPath := filepath.Join(workspace, "references.go")
	docURI := uri.FromPath(docPath).String()
	session.Notify(t, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": docURI, "languageId": "go", "version": 1, "text": content},
	})
	version := int64(1)
	comment := 0
	changeSnapshot := func() {
		version++
		comment++
		changed := strings.Replace(content, "// revision=0", fmt.Sprintf("// revision=%d", comment), 1)
		session.Notify(t, "textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": docURI, "version": version},
			"contentChanges": []map[string]string{{"text": changed}},
		})
	}
	requestReferences := func(ctx context.Context, symbol string, token string) (int64, json.RawMessage, error) {
		line, char := tokenPositions[symbol][0], tokenPositions[symbol][1]
		return session.RequestIDContext(ctx, "textDocument/references", map[string]any{
			"textDocument":  map[string]string{"uri": docURI},
			"position":      map[string]uint32{"line": line, "character": char},
			"context":       map[string]bool{"includeDeclaration": true},
			"workDoneToken": token,
		})
	}
	var lastRequestID int64
	for _, locationTotal := range e2eS19ReturnedLocationScales {
		symbol := fmt.Sprintf("target%d", locationTotal)
		scale := e2eReferenceScale{
			ReturnedLocationTotalIncludingDeclaration:          locationTotal,
			ExpectedReturnedLocationCountIncludingDeclaration:  locationTotal,
			ObservedReturnedLocationCountsIncludingDeclaration: []int{}, SamplesNS: []int64{}, Status: "running",
		}
		// Invalidate the snapshot between samples so memoized LSP query results
		// cannot flatten the 200/800/3200 scaling curve.
		for sample := 0; sample < 3; sample++ {
			changeSnapshot()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			started := time.Now()
			requestID, raw, err := requestReferences(ctx, symbol, "")
			elapsed := time.Since(started).Nanoseconds()
			cancel()
			lastRequestID = requestID
			if err != nil {
				scale.Status = "failed"
				scale.Error = fmt.Sprintf("sample %d request failed: %v", sample, err)
				break
			}
			locations, decodeErr := decodeE2ELocations(raw)
			if decodeErr != nil {
				scale.Status = "failed"
				scale.Error = fmt.Sprintf("sample %d decode failed: %v", sample, decodeErr)
				break
			}
			scale.ObservedReturnedLocationCountsIncludingDeclaration = append(scale.ObservedReturnedLocationCountsIncludingDeclaration, len(locations))
			scale.SamplesNS = append(scale.SamplesNS, elapsed)
			if len(locations) != scale.ExpectedReturnedLocationCountIncludingDeclaration {
				scale.Status = "failed"
				scale.Error = fmt.Sprintf("sample %d returned %d locations, expected exactly %d returned locations including declaration", sample, len(locations), scale.ExpectedReturnedLocationCountIncludingDeclaration)
				break
			}
		}
		if len(scale.SamplesNS) > 0 {
			scale.P50NS = e2ePercentileNS(scale.SamplesNS, .50)
			scale.P95NS = e2ePercentileNS(scale.SamplesNS, .95)
			scale.P99NS = e2ePercentileNS(scale.SamplesNS, .99)
		}
		if scale.Status == "running" {
			scale.Status = "pass"
		}
		result.Scaling = append(result.Scaling, scale)
		result.Resources = append(result.Resources, e2eProcessTreeResources(os.Getpid(), fmt.Sprintf("returned-locations-%d", locationTotal)))
	}

	// Active cancellation: use the progress begin as evidence the server has
	// admitted the request, cancel it, then require its correlated terminal
	// JSON-RPC response to be RequestCancelled (-32800).
	changeSnapshot()
	const cancelToken = "omnilsp-s19-cancel"
	result.Cancellation.ProgressToken = cancelToken
	cancelCtx, cancelRequest := context.WithCancel(context.Background())
	type response struct {
		id  int64
		raw json.RawMessage
		err error
	}
	responseCh := make(chan response, 1)
	go func() {
		id, raw, err := requestReferences(cancelCtx, "target3200", cancelToken)
		responseCh <- response{id: id, raw: raw, err: err}
	}()
	progressStarted := waitE2EProgress(session, cancelToken, "begin", 5*time.Second)
	result.Cancellation.Attempted = true
	result.Cancellation.ProgressBegan = progressStarted
	if !progressStarted {
		result.Cancellation.Status = "not_verified"
		result.Cancellation.CallerOutcome = "cancelled because no progress begin was observed within 5s"
		result.NotVerified = append(result.NotVerified, "active cancellation not verified because request progress begin was not observed")
		cancelRequest()
		out := <-responseCh
		result.Cancellation.RequestID = out.id
		lastRequestID = out.id
	} else {
		cancelRequest()
		select {
		case out := <-responseCh:
			result.Cancellation.RequestID = out.id
			lastRequestID = out.id
			result.Cancellation.CallerOutcome = e2EErrorText(out.err)
			terminal := waitE2EResponse(session, out.id, 5*time.Second)
			if terminal == nil {
				result.Cancellation.Status = "failed"
				result.Cancellation.ServerTerminal = "no correlated server response observed"
			} else if terminal.Error != nil {
				result.Cancellation.ServerErrorCode = terminal.Error.Code
				result.Cancellation.ServerTerminal = terminal.Error.Error()
				if terminal.Error.Code == jsonrpc.RequestCancelled {
					result.Cancellation.Status = "pass"
				} else {
					result.Cancellation.Status = "failed"
				}
			} else {
				result.Cancellation.ServerTerminal = "successful result completed before cancellation"
				result.Cancellation.Status = "partial"
			}
		case <-time.After(10 * time.Second):
			cancelRequest()
			result.Cancellation.Status = "failed"
			result.Cancellation.ServerTerminal = "request did not leave the client call within 10s"
		}
	}
	if result.Cancellation.RequestID > lastRequestID {
		lastRequestID = result.Cancellation.RequestID
	}
	result.Progress.BeginObserved = waitE2EProgress(session, cancelToken, "begin", time.Second)
	result.Progress.EndObserved = waitE2EProgress(session, cancelToken, "end", 5*time.Second)
	if result.Progress.BeginObserved && result.Progress.EndObserved {
		result.Progress.Status = "pass"
	} else {
		result.Progress.Status = "not_verified"
		result.NotVerified = append(result.NotVerified, "references progress begin/end not observed")
	}

	// P0 completion and P1 hover are issued while the 3200-location request
	// is outstanding. If the work finishes before either request is issued,
	// the no-starvation requirement remains unverified and is reported as such.
	changeSnapshot()
	const starveToken = "omnilsp-s19-starve"
	starveCtx, cancelStarve := context.WithTimeout(context.Background(), 20*time.Second)
	starveCh := make(chan response, 1)
	go func() {
		id, raw, err := requestReferences(starveCtx, "target3200", starveToken)
		starveCh <- response{id: id, raw: raw, err: err}
	}()
	starveBegan := waitE2EProgress(session, starveToken, "begin", 5*time.Second)
	result.NoStarvation.ReferenceProgressBegan = starveBegan
	result.NoStarvation.ReferenceProgressToken = starveToken
	expectedStarveID := lastRequestID + 1
	line, char := tokenPositions["marker"][0], tokenPositions["marker"][1]
	type interactiveResponse struct {
		id      int64
		raw     json.RawMessage
		err     error
		elapsed int64
	}
	completionCh := make(chan interactiveResponse, 1)
	hoverCh := make(chan interactiveResponse, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		started := time.Now()
		id, raw, err := session.RequestIDContext(ctx, "textDocument/completion", map[string]any{
			"textDocument": map[string]string{"uri": docURI},
			"position":     map[string]uint32{"line": line, "character": char},
		})
		completionCh <- interactiveResponse{id: id, raw: raw, err: err, elapsed: time.Since(started).Nanoseconds()}
	}()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		started := time.Now()
		id, raw, err := session.RequestIDContext(ctx, "textDocument/hover", map[string]any{
			"textDocument": map[string]string{"uri": docURI},
			"position":     map[string]uint32{"line": line, "character": char},
		})
		hoverCh <- interactiveResponse{id: id, raw: raw, err: err, elapsed: time.Since(started).Nanoseconds()}
	}()
	var completion, hover interactiveResponse
	select {
	case completion = <-completionCh:
	case <-time.After(6 * time.Second):
		completion.err = context.DeadlineExceeded
	}
	select {
	case hover = <-hoverCh:
	case <-time.After(6 * time.Second):
		hover.err = context.DeadlineExceeded
	}
	completionRequestID, completionRaw, completionErr := completion.id, completion.raw, completion.err
	result.NoStarvation.CompletionNS = completion.elapsed
	completionCheckErr := completionErr
	if completionCheckErr == nil {
		completionCheckErr = validateE2ECompletionFor(completionRaw, "marker")
	}
	if completionCheckErr == nil && result.NoStarvation.CompletionNS <= result.NoStarvation.BudgetMillis*int64(time.Millisecond) {
		result.NoStarvation.CompletionStatus = "pass"
	} else if completionCheckErr != nil {
		result.NoStarvation.CompletionStatus = e2eErrorStatus(completionCheckErr)
	} else {
		result.NoStarvation.CompletionStatus = "failed"
	}
	hoverRequestID, hoverRaw, hoverErr := hover.id, hover.raw, hover.err
	result.NoStarvation.HoverNS = hover.elapsed
	hoverCheckErr := hoverErr
	if hoverCheckErr == nil {
		hoverCheckErr = validateE2EHoverFor(hoverRaw, "marker")
	}
	if hoverCheckErr == nil && result.NoStarvation.HoverNS <= result.NoStarvation.BudgetMillis*int64(time.Millisecond) {
		result.NoStarvation.HoverStatus = "pass"
	} else if hoverCheckErr != nil {
		result.NoStarvation.HoverStatus = e2eErrorStatus(hoverCheckErr)
	} else {
		result.NoStarvation.HoverStatus = "failed"
	}
	var completedStarve response
	starveResponseReceived := false
	starveCompletedNaturally := false
	select {
	case completedStarve = <-starveCh:
		starveResponseReceived = true
		starveCompletedNaturally = completedStarve.err == nil
	case <-time.After(20 * time.Second):
		cancelStarve()
		select {
		case completedStarve = <-starveCh:
			starveResponseReceived = true
			starveCompletedNaturally = completedStarve.err == nil
		case <-time.After(5 * time.Second):
			result.NoStarvation.Status = "not_verified"
			result.NoStarvation.ReferenceTerminal = "caller timed out; no correlated terminal response was received"
		}
	}
	cancelStarve()
	result.NoStarvation.ReferenceRequestID = completedStarve.id
	result.NoStarvation.CompletionRequestID = completionRequestID
	result.NoStarvation.HoverRequestID = hoverRequestID
	result.NoStarvation.ExpectedReferenceResultCount = e2eS19ReturnedLocationScales[len(e2eS19ReturnedLocationScales)-1]
	result.NoStarvation.ReferenceCompletedNaturally = starveCompletedNaturally
	if starveResponseReceived {
		result.NoStarvation.ReferenceCallerOutcome = e2EErrorText(completedStarve.err)
	} else {
		result.NoStarvation.ReferenceCallerOutcome = "caller did not return a result before the cancellation grace period"
	}
	var starveResultCount int
	var starveResultErr error
	if starveResponseReceived && completedStarve.err == nil {
		locations, decodeErr := decodeE2ELocations(completedStarve.raw)
		starveResultCount = len(locations)
		starveResultErr = decodeErr
	}
	result.NoStarvation.ReferenceResultCount = starveResultCount
	if starveResultErr != nil {
		result.NoStarvation.ReferenceTerminal = "caller received an undecodable references result: " + starveResultErr.Error()
	}
	if completedStarve.id > 0 {
		terminal := waitE2EResponse(session, completedStarve.id, 5*time.Second)
		if terminal != nil {
			if terminal.Error != nil {
				result.NoStarvation.ReferenceTerminal = terminal.Error.Error()
				result.NoStarvation.ReferenceTerminalErrorCode = terminal.Error.Code
			} else {
				terminalLocations, decodeErr := decodeE2ELocations(terminal.Result)
				if decodeErr != nil {
					result.NoStarvation.ReferenceTerminal = "correlated terminal result could not be decoded: " + decodeErr.Error()
				} else {
					result.NoStarvation.ReferenceTerminalResultCount = len(terminalLocations)
					result.NoStarvation.ReferenceTerminalSuccess = len(terminalLocations) == result.NoStarvation.ExpectedReferenceResultCount
					if result.NoStarvation.ReferenceTerminalSuccess {
						result.NoStarvation.ReferenceTerminal = "completed successfully"
					} else {
						result.NoStarvation.ReferenceTerminal = fmt.Sprintf("successful JSON-RPC result returned %d locations; expected %d", len(terminalLocations), result.NoStarvation.ExpectedReferenceResultCount)
					}
				}
			}
		} else {
			result.NoStarvation.ReferenceTerminal = "no correlated server terminal response observed"
		}
	}
	events := session.Events()
	result.NoStarvation.ReferenceBeginIndex = e2eProgressEventOrder(events, starveToken, "begin")
	result.NoStarvation.ReferenceEndIndex = e2eProgressEventOrder(events, starveToken, "end")
	result.NoStarvation.CompletionResponseIndex = e2eResponseEventOrder(events, completionRequestID)
	result.NoStarvation.HoverResponseIndex = e2eResponseEventOrder(events, hoverRequestID)
	result.NoStarvation.ReferenceResponseIndex = e2eResponseEventOrder(events, completedStarve.id)
	interactiveIDs := []int64{completionRequestID, hoverRequestID}
	sort.Slice(interactiveIDs, func(i, j int) bool { return interactiveIDs[i] < interactiveIDs[j] })
	activeAtDispatch := expectedStarveID > 0 &&
		completedStarve.id == expectedStarveID &&
		interactiveIDs[0] == expectedStarveID+1 &&
		interactiveIDs[1] == expectedStarveID+2 &&
		result.NoStarvation.CompletionResponseIndex >= 0 &&
		result.NoStarvation.HoverResponseIndex >= 0 &&
		result.NoStarvation.ReferenceResponseIndex > result.NoStarvation.CompletionResponseIndex &&
		result.NoStarvation.ReferenceResponseIndex > result.NoStarvation.HoverResponseIndex &&
		result.NoStarvation.ReferenceBeginIndex >= 0 &&
		result.NoStarvation.ReferenceEndIndex >= 0 &&
		result.NoStarvation.ReferenceBeginIndex < result.NoStarvation.CompletionResponseIndex &&
		result.NoStarvation.ReferenceBeginIndex < result.NoStarvation.HoverResponseIndex &&
		result.NoStarvation.ReferenceEndIndex > result.NoStarvation.CompletionResponseIndex &&
		result.NoStarvation.ReferenceEndIndex > result.NoStarvation.HoverResponseIndex
	result.NoStarvation.ReferenceWasActive = activeAtDispatch
	referenceTimedOut := errors.Is(completedStarve.err, context.Canceled) || errors.Is(completedStarve.err, context.DeadlineExceeded)
	if !starveResponseReceived {
		result.NoStarvation.Status = "not_verified"
		result.NotVerified = append(result.NotVerified, "3200-location caller did not return after cancellation grace period")
	} else if completedStarve.err != nil && referenceTimedOut {
		result.NoStarvation.Status = "not_verified"
		result.NoStarvation.ReferenceTerminalSuccess = false
		result.NotVerified = append(result.NotVerified, "3200-location query did not complete naturally before cancellation/deadline")
	} else if completedStarve.err != nil {
		result.NoStarvation.Status = "failed"
		result.Errors = append(result.Errors, "3200-location request failed: "+completedStarve.err.Error())
	} else if starveResultErr != nil || starveResultCount != result.NoStarvation.ExpectedReferenceResultCount {
		result.NoStarvation.Status = "failed"
		result.Errors = append(result.Errors, fmt.Sprintf("3200-location request returned %d locations, expected %d including declaration: %v", starveResultCount, result.NoStarvation.ExpectedReferenceResultCount, starveResultErr))
	} else if !result.NoStarvation.ReferenceTerminalSuccess {
		if result.NoStarvation.ReferenceResponseIndex < 0 {
			result.NoStarvation.Status = "not_verified"
			result.NotVerified = append(result.NotVerified, "correlated successful terminal response for the 3200-location query was not observed")
		} else {
			result.NoStarvation.Status = "failed"
			result.Errors = append(result.Errors, fmt.Sprintf("correlated terminal result for the 3200 returned-location query did not contain the expected %d locations including declaration", result.NoStarvation.ExpectedReferenceResultCount))
		}
	} else if starveCompletedNaturally && activeAtDispatch && result.NoStarvation.ReferenceTerminalSuccess &&
		result.NoStarvation.CompletionStatus == "pass" && result.NoStarvation.HoverStatus == "pass" {
		result.NoStarvation.Status = "pass"
	} else if !activeAtDispatch {
		result.NoStarvation.Status = "partial"
		result.NotVerified = append(result.NotVerified, "no-starvation not verified: correlated completion/hover responses did not both precede the same reference terminal response")
	} else if result.NoStarvation.CompletionStatus == "not_verified" || result.NoStarvation.HoverStatus == "not_verified" {
		result.NoStarvation.Status = "not_verified"
		result.NotVerified = append(result.NotVerified, "high-priority completion/hover unsupported or unavailable while a reference query was active")
	} else {
		result.NoStarvation.Status = "failed"
		result.Errors = append(result.Errors, "P0 completion or P1 hover failed semantic validation or exceeded the 2s response budget")
	}
	result.Resources = append(result.Resources, e2eProcessTreeResources(os.Getpid(), "completed"))

	allPass := len(result.Scaling) == 3 && result.Cancellation.Status == "pass" && result.Progress.Status == "pass" && result.NoStarvation.Status == "pass"
	resourcesVerified := len(result.Resources) == 5
	resourcesFailed := false
	for _, snapshot := range result.Resources {
		resourcesVerified = resourcesVerified && snapshot.Status == "observed"
		resourcesFailed = resourcesFailed || snapshot.Status == "failed"
	}
	allPass = allPass && resourcesVerified
	for _, scale := range result.Scaling {
		allPass = allPass && scale.Status == "pass"
	}
	if allPass {
		result.Status = "pass"
	} else {
		result.Status = "failed"
		for _, scale := range result.Scaling {
			if scale.Status == "failed" {
				result.Status = "failed"
			}
		}
		if result.Cancellation.Status == "not_verified" || result.Cancellation.Status == "partial" || result.Progress.Status == "not_verified" || result.NoStarvation.Status == "partial" || result.NoStarvation.Status == "not_verified" || !resourcesVerified {
			result.Status = "partial"
		}
		for _, scale := range result.Scaling {
			if scale.Status == "failed" {
				result.Status = "failed"
			}
		}
		if result.Cancellation.Status == "failed" {
			result.Errors = append(result.Errors, "active cancellation did not produce a verified RequestCancelled terminal response")
		}
		if result.Progress.Status == "failed" {
			result.Errors = append(result.Errors, "references progress begin/end was not observed")
		}
		if result.NoStarvation.Status == "failed" {
			result.Errors = append(result.Errors, "no-starvation behavior was not fully verified")
		}
		if !resourcesVerified && !resourcesFailed {
			result.NotVerified = append(result.NotVerified, "process-tree memory/resource measurement unavailable")
		}
		if resourcesFailed {
			result.Status = "failed"
			for _, snapshot := range result.Resources {
				if snapshot.Status == "failed" {
					result.Errors = append(result.Errors, snapshot.Error)
				}
			}
		}
		for _, scale := range result.Scaling {
			if scale.Status == "failed" {
				result.Errors = append(result.Errors, fmt.Sprintf("references@%d: %s", scale.ReturnedLocationTotalIncludingDeclaration, scale.Error))
			} else if scale.Status != "pass" {
				result.NotVerified = append(result.NotVerified, fmt.Sprintf("references@%d: %s", scale.ReturnedLocationTotalIncludingDeclaration, scale.Error))
			}
		}
		if len(result.Errors) > 0 || resourcesFailed {
			result.Status = "failed"
		}
	}
	return result
}

func e2eProcessTreeResources(rootPID int, stage string) e2eResourceSnapshot {
	rootKind := "Go test process (os.Getpid) and measurable descendants"
	snapshot := e2eResourceSnapshot{Stage: stage, SampledAt: time.Now().UTC(), RootPID: rootPID, RootKind: rootKind, Status: "not_verified"}
	if rootPID <= 0 {
		snapshot.Error = "Go test process PID unavailable"
		return snapshot
	}
	if runtime.GOOS == "windows" {
		first := e2eWindowsProcessTreeResources(rootPID, stage)
		first.RootKind = rootKind
		first.Error = strings.ReplaceAll(first.Error, "candidate", "Go test process")
		if first.Status != "observed" {
			return first
		}
		second := e2eWindowsProcessTreeResources(rootPID, stage)
		second.RootKind = rootKind
		second.Error = strings.ReplaceAll(second.Error, "candidate", "Go test process")
		if second.Status != "observed" {
			return second
		}
		if first.ProcessCount != second.ProcessCount {
			second.Status = "not_verified"
			second.Error = fmt.Sprintf("Windows Go test process tree changed during paired Toolhelp samples (%d then %d processes)", first.ProcessCount, second.ProcessCount)
			return second
		}
		second.PrivateMetric = "PROCESS_MEMORY_COUNTERS_EX.PrivateUsage"
		return second
	}
	if runtime.GOOS == "linux" {
		pids := map[int]bool{}
		incomplete := []string{}
		var visit func(int)
		visit = func(pid int) {
			if pids[pid] {
				return
			}
			pids[pid] = true
			taskRoot := filepath.Join("/proc", strconv.Itoa(pid), "task")
			tasks, err := os.ReadDir(taskRoot)
			if err != nil {
				incomplete = append(incomplete, fmt.Sprintf("enumerate process %d threads: %v", pid, err))
				return
			}
			for _, task := range tasks {
				if !task.IsDir() {
					continue
				}
				childrenPath := filepath.Join(taskRoot, task.Name(), "children")
				children, err := os.ReadFile(childrenPath)
				if err != nil {
					incomplete = append(incomplete, fmt.Sprintf("read child list %s: %v", childrenPath, err))
					continue
				}
				for _, token := range strings.Fields(string(children)) {
					child, parseErr := strconv.Atoi(token)
					if parseErr != nil {
						incomplete = append(incomplete, fmt.Sprintf("parse child PID %q for process %d: %v", token, pid, parseErr))
						continue
					}
					visit(child)
				}
			}
		}
		visit(rootPID)
		orderedPIDs := make([]int, 0, len(pids))
		for pid := range pids {
			orderedPIDs = append(orderedPIDs, pid)
		}
		sort.Ints(orderedPIDs)
		snapshot.ProcessCount = len(orderedPIDs)
		for _, pid := range orderedPIDs {
			procRoot := filepath.Join("/proc", strconv.Itoa(pid))
			data, err := os.ReadFile(filepath.Join(procRoot, "status"))
			if err != nil {
				incomplete = append(incomplete, fmt.Sprintf("read process %d status: %v", pid, err))
				continue
			}
			var hasRSS, hasThreads bool
			for _, line := range strings.Split(string(data), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 2 {
					continue
				}
				value, parseErr := strconv.ParseUint(fields[1], 10, 64)
				if parseErr != nil {
					continue
				}
				switch fields[0] {
				case "VmRSS:":
					hasRSS = true
					snapshot.WorkingSetBytes += value * 1024
				case "Threads:":
					hasThreads = true
					snapshot.ThreadCount += value
				}
			}
			if !hasRSS || !hasThreads {
				incomplete = append(incomplete, fmt.Sprintf("process %d status omitted VmRSS or Threads", pid))
			}
			privateBytes, err := e2eLinuxPrivateBytes(procRoot)
			if err != nil {
				incomplete = append(incomplete, fmt.Sprintf("read process %d private resident memory: %v", pid, err))
			} else {
				snapshot.PrivateBytes += privateBytes
			}
			if entries, err := os.ReadDir(filepath.Join(procRoot, "fd")); err == nil {
				snapshot.HandleCount += uint64(len(entries))
			} else {
				incomplete = append(incomplete, fmt.Sprintf("read process %d file descriptors: %v", pid, err))
			}
		}
		if len(incomplete) > 0 {
			snapshot.Error = "incomplete /proc Go test process-tree sample: " + strings.Join(incomplete, "; ")
			return snapshot
		}
		if snapshot.ProcessCount > 0 {
			snapshot.PrivateMetric = "Linux smaps_rollup Private_Clean + Private_Dirty + Private_Hugetlb (private resident bytes)"
			if snapshot.PrivateBytes > e2eS19MaxPrivateMemoryBytes {
				snapshot.Status = "failed"
				snapshot.Error = fmt.Sprintf("Go test process-tree private memory %d bytes exceeded %d byte acceptance limit", snapshot.PrivateBytes, e2eS19MaxPrivateMemoryBytes)
			} else {
				snapshot.Status = "observed"
			}
		} else {
			snapshot.Error = "Go test process tree is absent under /proc"
		}
		return snapshot
	}
	snapshot.Error = "Go test process-tree metrics are not implemented for " + runtime.GOOS
	return snapshot
}

func e2eLinuxPrivateBytes(procRoot string) (uint64, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, "smaps_rollup"))
	if err != nil {
		return 0, err
	}
	var privateKB uint64
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if fields[0] != "Private_Clean:" && fields[0] != "Private_Dirty:" && fields[0] != "Private_Hugetlb:" {
			continue
		}
		value, parseErr := strconv.ParseUint(fields[1], 10, 64)
		if parseErr != nil || fields[2] != "kB" {
			return 0, fmt.Errorf("invalid %s field %q", fields[0], strings.TrimSpace(line))
		}
		seen[fields[0]] = true
		privateKB += value
	}
	for _, field := range []string{"Private_Clean:", "Private_Dirty:", "Private_Hugetlb:"} {
		if !seen[field] {
			return 0, fmt.Errorf("smaps_rollup omitted %s", field)
		}
	}
	return privateKB * 1024, nil
}

func e2eS18Thresholds() map[string]e2eThresholds {
	return map[string]e2eThresholds{
		"hot_hover":                {20, 75, 150},
		"hot_definition":           {25, 100, 200},
		"completion_first_usable":  {40, 120, 250},
		"syntax_update_after_edit": {15, 50, 100},
	}
}

func buildE2ES18Corpus() map[string]string {
	files := map[string]string{"go.mod": "module acceptance\n\ngo 1.26\n"}
	for shard := 0; shard < 8; shard++ {
		var b strings.Builder
		b.WriteString("package perf\n\n")
		for i := shard * 39; i < (shard+1)*39; i++ {
			fmt.Fprintf(&b, "func sym%03d(x int) int {\n\ty := x*%d + %d\n\treturn y + sym%03d(y)\n}\n\n", i, i, i+1, (i+1)%312)
		}
		files[fmt.Sprintf("src%02d.go", shard)] = b.String()
	}
	files["use.go"] = "package perf\nfunc use(value int) int { return sym050(value) }\nvar editMarker = \"A\"\n"
	files["c-perf.c"] = "int targetPerf(int x) { return x + 1; }\nint usePerf(int x) { return targetPerf(x); }\n// editMarker=A\n"
	files["cpp-perf.cpp"] = "int targetPerf(int x) { return x + 1; }\nint usePerf(int x) { return targetPerf(x); }\n// editMarker=A\n"
	files["Cargo.toml"] = "[workspace]\nmembers = [\"rust-perf\"]\nresolver = \"2\"\n"
	files["rust-perf/Cargo.toml"] = "[package]\nname = \"acceptance_perf\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"
	files["Cargo.lock"] = "# This file is automatically @generated by Cargo.\n# It is not intended for manual editing.\nversion = 3\n\n[[package]]\nname = \"acceptance_perf\"\nversion = \"0.1.0\"\n"
	files["rust-perf/src/lib.rs"] = "pub fn target_perf(x: i32) -> i32 { x + 1 }\npub fn use_perf(x: i32) -> i32 { target_perf(x) }\n// editMarker=A\n"
	files["pyrightconfig.json"] = "{\"include\":[\"python-perf.py\"],\"pythonVersion\":\"3.11\"}\n"
	files["python-perf.py"] = "def target_perf(x: int) -> int:\n    return x + 1\ndef use_perf(x: int) -> int:\n    return target_perf(x)\n# editMarker=A\n"
	files["tsconfig.json"] = "{\"compilerOptions\":{\"target\":\"ES2020\",\"module\":\"commonjs\",\"strict\":true},\"include\":[\"typescript-perf.ts\"]}\n"
	files["typescript-perf.ts"] = "function targetPerf(x: number): number { return x + 1; }\nfunction usePerf(x: number): number { return targetPerf(x); }\n// editMarker=A\n"
	files["jsconfig.json"] = "{\"compilerOptions\":{\"target\":\"ES2020\",\"module\":\"commonjs\",\"checkJs\":true},\"include\":[\"javascript-perf.js\"]}\n"
	files["javascript-perf.js"] = "function targetPerf(x) { return x + 1; }\nfunction usePerf(x) { return targetPerf(x); }\n// editMarker=A\n"
	return files
}

func buildE2ES19Corpus() (string, map[string][2]uint32, error) {
	var b strings.Builder
	b.WriteString("package perf\n")
	positions := make(map[string][2]uint32, 4)
	for _, locationTotal := range e2eS19ReturnedLocationScales {
		name := fmt.Sprintf("target%d", locationTotal)
		fmt.Fprintf(&b, "var %s int\n", name)
		// Position is recalculated after the declaration is appended.
		line, char := e2ePositionOf(b.String(), name, 0)
		positions[name] = [2]uint32{line, char}
	}
	b.WriteString("// revision=0\nfunc marker() int { return 42 }\nfunc useMarker() int { return marker() }\nfunc useReferences() {\n")
	for _, locationTotal := range e2eS19ReturnedLocationScales {
		name := fmt.Sprintf("target%d", locationTotal)
		referenceUseCount, err := e2eS19ReferenceUseCount(locationTotal, true)
		if err != nil {
			return "", nil, err
		}
		for i := 0; i < referenceUseCount; i++ {
			fmt.Fprintf(&b, "_ = %s\n", name)
		}
	}
	b.WriteString("}\n")
	// The high-priority request position uses the declaration identifier. The
	// server resolves completion/hover from this point in the generated file.
	line, char := e2ePositionOf(b.String(), "marker", 1)
	positions["marker"] = [2]uint32{line, char}
	return b.String(), positions, nil
}

func e2eS19ReferenceUseCount(returnedLocationTotal int, includeDeclaration bool) (int, error) {
	if returnedLocationTotal < 0 || (includeDeclaration && returnedLocationTotal == 0) {
		return 0, fmt.Errorf("invalid returned-location total %d with includeDeclaration=%t", returnedLocationTotal, includeDeclaration)
	}
	if includeDeclaration {
		return returnedLocationTotal - 1, nil
	}
	return returnedLocationTotal, nil
}

func e2eResolveBinary(t testing.TB) (string, error) {
	t.Helper()
	if configured := os.Getenv(acceptanceBinEnv); configured != "" {
		if !filepath.IsAbs(configured) {
			root, err := e2eRepoRoot()
			if err != nil {
				return "", err
			}
			configured = filepath.Join(root, configured)
		}
		abs, err := filepath.Abs(configured)
		if err != nil {
			return "", fmt.Errorf("resolve OMNILSP_BIN: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("blocked: OMNILSP_BIN %q: %w", abs, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("blocked: OMNILSP_BIN %q is a directory", abs)
		}
		return abs, nil
	}
	if _, err := exec.LookPath("go"); err != nil {
		return "", fmt.Errorf("blocked: no OMNILSP_BIN and Go compiler unavailable: %w", err)
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("blocked: cannot locate repository root for candidate build")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	name := "omnilsp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", out, "./cmd/omnilsp")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("blocked: build candidate: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return out, nil
}

func e2eGitRevision(t testing.TB) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "unavailable"
	}
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	output, err := cmd.Output()
	if err != nil {
		return "unavailable"
	}
	return strings.TrimSpace(string(output))
}

func e2eRepoRoot() (string, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot locate repository root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..")), nil
}

func e2eToolVersions() map[string]string {
	versions := map[string]string{
		"go":                         e2eCommandVersion("go", "version"),
		"gopls":                      e2eCommandVersion("gopls", "version"),
		"omnilsp":                    "candidate binary; SHA-256 in candidateSha256",
		"go-backend":                 "native; golang.org/x/tools v0.49.0",
		"clangd":                     e2eCommandVersion("clangd", "--version"),
		"clang":                      e2eCommandVersion("clang", "--version"),
		"clang++":                    e2eCommandVersion("clang++", "--version"),
		"rust-analyzer":              e2eCommandVersion("rust-analyzer", "--version"),
		"rustc":                      e2eCommandVersion("rustc", "--version"),
		"cargo":                      e2eCommandVersion("cargo", "--version"),
		"pyright":                    e2eLocalPackageVersion("pyright"),
		"python":                     e2eFirstCommandVersion([]string{"python", "python3"}, "--version"),
		"typescript-language-server": e2eLocalPackageVersion("typescript-language-server"),
		"typescript":                 e2eLocalPackageVersion("typescript"),
		"node":                       e2eCommandVersion("node", "--version"),
		"npm":                        e2eCommandVersion("npm", "--version"),
	}
	if root, err := e2eRepoRoot(); err == nil {
		if data, err := os.ReadFile(filepath.Join(root, "test", "acceptance", "tools", "tools.lock.json")); err == nil {
			digest := sha256.Sum256(data)
			versions["tools.lock.json"] = "sha256:" + hex.EncodeToString(digest[:])
		} else {
			versions["tools.lock.json"] = "missing: " + err.Error()
		}
	}
	return versions
}

func e2eAcceptanceToolPath() string {
	root, err := e2eRepoRoot()
	if err != nil {
		return os.Getenv("PATH")
	}
	localBin := filepath.Join(root, "test", "acceptance", "tools", "bin")
	if current := os.Getenv("PATH"); current != "" {
		return localBin + string(os.PathListSeparator) + current
	}
	return localBin
}

func e2eMissingFixtureTools(names []string) []string {
	missing := []string{}
	root, _ := e2eRepoRoot()
	localBin := filepath.Join(root, "test", "acceptance", "tools", "bin")
	for _, name := range names {
		if name == "pyright-langserver" || name == "typescript-language-server" || name == "tsc" {
			binaryName := name
			if runtime.GOOS == "windows" {
				binaryName += ".exe"
			}
			if _, err := os.Stat(filepath.Join(localBin, binaryName)); err != nil {
				missing = append(missing, name+" (pinned local wrapper missing)")
				continue
			}
			if _, err := exec.LookPath("node"); err != nil {
				missing = append(missing, "node")
			}
			continue
		}
		if _, err := exec.LookPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	return uniqueE2EStrings(missing)
}

func e2eLockedToolVersionProblems(names []string, versions map[string]string) []string {
	root, err := e2eRepoRoot()
	if err != nil {
		return []string{"tools.lock.json unavailable: " + err.Error()}
	}
	data, err := os.ReadFile(filepath.Join(root, "test", "acceptance", "tools", "tools.lock.json"))
	if err != nil {
		return []string{"tools.lock.json unavailable: " + err.Error()}
	}
	var lock struct {
		Observed        map[string]string `json:"observed"`
		AcceptanceTools map[string]struct {
			Version string `json:"version"`
		} `json:"acceptanceTools"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return []string{"tools.lock.json invalid: " + err.Error()}
	}
	observedKey := map[string]string{
		"go": "go", "gopls": "gopls", "clangd": "clangd", "clang": "clang", "clang++": "clang++",
		"rust-analyzer": "rustAnalyzer", "rustc": "rustc", "cargo": "cargo", "python": "python", "python3": "python",
		"node": "node", "npm": "npm",
	}
	acceptanceKey := map[string]string{
		"pyright-langserver": "pyright", "typescript-language-server": "typescriptLanguageServer", "tsc": "typescript",
	}
	problems := []string{}
	for _, name := range names {
		var expected string
		actualName := name
		if key := observedKey[name]; key != "" {
			expected = lock.Observed[key]
		} else if key := acceptanceKey[name]; key != "" {
			expected = lock.AcceptanceTools[key].Version
			switch name {
			case "pyright-langserver":
				actualName = "pyright"
			case "tsc":
				actualName = "typescript"
			}
		} else {
			continue
		}
		actual := versions[actualName]
		if expected == "" {
			problems = append(problems, name+": no exact locked version is recorded")
			continue
		}
		if actual == "" || actual == "missing" || strings.Contains(actual, "version probe failed") || !toolversion.Matches(actual, expected) {
			problems = append(problems, fmt.Sprintf("%s version does not match lock: expected %q, observed %q", name, expected, actual))
		}
	}
	return problems
}

func e2eLookPath(name string) (string, error) {
	if name == "pyright-langserver" || name == "typescript-language-server" || name == "tsc" {
		root, err := e2eRepoRoot()
		if err != nil {
			return "", err
		}
		localName := name
		if runtime.GOOS == "windows" {
			localName += ".exe"
		}
		localPath := filepath.Join(root, "test", "acceptance", "tools", "bin", localName)
		if _, err := os.Stat(localPath); err == nil {
			return localPath, nil
		}
		return "", fmt.Errorf("pinned local wrapper is missing: %s", localPath)
	}
	return exec.LookPath(name)
}

func e2eLocalPackageVersion(name string) string {
	root, err := e2eRepoRoot()
	if err != nil {
		return "missing: repository root unavailable"
	}
	path := filepath.Join(root, "test", "acceptance", "tools", "node_modules", name, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "missing: " + path
	}
	var metadata struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil || metadata.Version == "" {
		return "invalid package metadata: " + path
	}
	return metadata.Version + " (locked local package; " + path + ")"
}

func uniqueE2EStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func e2eFilesystemType(path string) string {
	if runtime.GOOS == "windows" {
		volume := filepath.VolumeName(path)
		if volume == "" {
			return "unknown: temporary directory has no drive volume"
		}
		cmd := exec.Command("fsutil", "fsinfo", "volumeinfo", volume)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return e2eWindowsDriveFormat(volume, "fsutil volume query failed: "+strings.TrimSpace(string(output)))
		}
		for _, line := range strings.Split(string(output), "\n") {
			if strings.Contains(strings.ToLower(line), "file system name") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1]) + " on " + volume
				}
			}
		}
		return e2eWindowsDriveFormat(volume, "fsutil fsinfo volumeinfo returned no filesystem name")
	}
	cmd := exec.Command("df", "-T", path)
	output, err := cmd.Output()
	if err != nil {
		return "unknown: df -T could not identify temporary filesystem"
	}
	lines := strings.Fields(string(output))
	if len(lines) >= 2 {
		return lines[1] + " (df -T; " + path + ")"
	}
	return "unknown: df -T returned no filesystem type"
}

func e2eWindowsDriveFormat(volume, primaryReason string) string {
	driveRoot := strings.TrimRight(volume, `\\`) + `\`
	powershellString := strings.ReplaceAll(driveRoot, "'", "''")
	script := "$ErrorActionPreference = 'Stop'; $drive = [System.IO.DriveInfo]::new('" + powershellString + "'); if (-not $drive.IsReady) { throw 'drive is not ready' }; [Console]::Out.WriteLine($drive.DriveFormat)"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("unknown: %s; PowerShell DriveInfo fallback failed: %v: %s", primaryReason, err, strings.TrimSpace(string(output)))
	}
	filesystem := strings.TrimSpace(string(output))
	if filesystem == "" || strings.ContainsAny(filesystem, "\r\n") {
		return fmt.Sprintf("unknown: %s; PowerShell DriveInfo fallback returned invalid filesystem name %q", primaryReason, filesystem)
	}
	hasLetter := false
	for _, r := range filesystem {
		if unicode.IsLetter(r) {
			hasLetter = true
			continue
		}
		if !unicode.IsDigit(r) && !strings.ContainsRune("._-+", r) {
			return fmt.Sprintf("unknown: %s; PowerShell DriveInfo fallback returned invalid filesystem name %q", primaryReason, filesystem)
		}
	}
	if !hasLetter {
		return fmt.Sprintf("unknown: %s; PowerShell DriveInfo fallback returned invalid filesystem name %q", primaryReason, filesystem)
	}
	return fmt.Sprintf("%s on %s (PowerShell DriveInfo fallback; %s)", filesystem, volume, primaryReason)
}

func e2eOSVersion() string {
	if runtime.GOOS == "windows" {
		output, err := exec.Command("cmd.exe", "/c", "ver").CombinedOutput()
		if err == nil {
			return strings.TrimSpace(string(output))
		}
		return "unknown: cmd ver failed: " + strings.TrimSpace(string(output))
	}
	output, err := exec.Command("uname", "-a").CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(output))
	}
	return "unknown: uname failed: " + strings.TrimSpace(string(output))
}

func e2eCommandVersion(name string, args ...string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return "missing"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return fmt.Sprintf("installed at %s; version probe failed: %s", path, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output))
}

func e2eFirstCommandVersion(names []string, args ...string) string {
	for _, name := range names {
		if _, err := exec.LookPath(name); err == nil {
			return e2eCommandVersion(name, args...)
		}
	}
	return "missing"
}

func writeE2EFiles(root string, files map[string]string) error {
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func digestE2ECorpus(files map[string]string) (string, int) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(files[name]))
		_, _ = h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), len(names)
}

func e2ePositionOf(text, token string, occurrence int) (uint32, uint32) {
	search := 0
	index := -1
	for n := 0; n <= occurrence; n++ {
		rel := strings.Index(text[search:], token)
		if rel < 0 {
			return math.MaxUint32, math.MaxUint32
		}
		index = search + rel
		search = index + len(token)
	}
	prefix := text[:index]
	line := uint32(strings.Count(prefix, "\n"))
	lastNewline := strings.LastIndex(prefix, "\n")
	columnText := prefix[lastNewline+1:]
	return line, uint32(len([]rune(columnText)))
}

func e2eDefinitionPosition(f e2eS18Fixture, text string) (uint32, uint32, error) {
	declarationPrefix := map[string]string{
		"go": "func ", "c": "int ", "cpp": "int ", "rust": "fn ",
		"python": "def ", "typescript": "function ", "javascript": "function ",
	}[f.id]
	if declarationPrefix == "" {
		return 0, 0, fmt.Errorf("no declaration prefix registered for fixture %q", f.id)
	}
	declaration := declarationPrefix + f.queryToken
	index := strings.Index(text, declaration)
	if index < 0 {
		return 0, 0, fmt.Errorf("fixture %q has no declaration %q", f.id, declaration)
	}
	occurrence := strings.Count(text[:index+len(declarationPrefix)], f.queryToken)
	line, character := e2ePositionOf(text, f.queryToken, occurrence)
	return line, character, nil
}

func e2eRenameFunctionSymbol(text, name string) (updated, newName string, ok bool) {
	needle := name + "("
	index := strings.Index(text, needle)
	if index < 0 {
		return text, "", false
	}
	newName = name + "Updated"
	updated = text[:index] + newName + text[index+len(name):]
	return updated, newName, true
}

func e2eEditMarkerOffset(text string) int {
	if index := strings.LastIndex(text, "editMarker=A"); index >= 0 {
		return index + len("editMarker=")
	}
	if index := strings.LastIndex(text, `"A"`); index >= 0 {
		return index + 1
	}
	return -1
}

func e2ePercentileNS(samples []int64, q float64) int64 {
	if len(samples) == 0 {
		return 0
	}
	ordered := append([]int64(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := int(math.Ceil(q*float64(len(ordered)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}

func validateE2EHover(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("%w: hover result is null", errE2ENotVerified)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("decode hover: %w", err)
	}
	contents := value["contents"]
	if len(contents) == 0 || string(contents) == "null" || string(contents) == `""` {
		return fmt.Errorf("%w: hover has no usable contents", errE2ENotVerified)
	}
	return nil
}

func validateE2EHoverFor(raw json.RawMessage, symbol string) error {
	if err := validateE2EHover(raw); err != nil {
		return err
	}
	if !strings.Contains(string(raw), symbol) {
		return fmt.Errorf("%w: hover did not identify expected symbol %q", errE2ENotVerified, symbol)
	}
	return nil
}

type e2eHoverSemanticSegment struct {
	Kind     string `json:"kind"`
	Language string `json:"language,omitempty"`
	Text     string `json:"text"`
}

func e2eCompareHoverSemantics(candidate, baseline json.RawMessage) error {
	candidateMeaning, err := e2eHoverSemanticContent(candidate)
	if err != nil {
		return err
	}
	baselineMeaning, err := e2eHoverSemanticContent(baseline)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(candidateMeaning, baselineMeaning) {
		return fmt.Errorf("semantic hover differs from pinned upstream: candidate=%+v upstream=%+v", candidateMeaning, baselineMeaning)
	}
	return nil
}

func e2eHoverSemanticContent(raw json.RawMessage) ([]e2eHoverSemanticSegment, error) {
	var hover struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &hover); err != nil {
		return nil, fmt.Errorf("decode hover contents: %w", err)
	}
	if len(hover.Contents) == 0 || string(hover.Contents) == "null" {
		return nil, fmt.Errorf("%w: hover has no semantic contents", errE2ENotVerified)
	}
	var contents any
	if err := json.Unmarshal(hover.Contents, &contents); err != nil {
		return nil, fmt.Errorf("decode hover content representation: %w", err)
	}
	segments := []e2eHoverSemanticSegment{}
	appendText := func(kind, text string) {
		if kind == "prose" {
			text = e2eStripInlineHoverListMarker(text)
		}
		text = strings.Join(strings.Fields(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")), " ")
		if text == "" {
			return
		}
		if kind == "prose" && len(segments) > 0 && segments[len(segments)-1].Kind == kind {
			segments[len(segments)-1].Text += " " + text
			return
		}
		segments = append(segments, e2eHoverSemanticSegment{Kind: kind, Text: text})
	}
	appendCode := func(language, code string) {
		code = strings.ReplaceAll(strings.ReplaceAll(code, "\r\n", "\n"), "\r", "\n")
		if strings.HasPrefix(code, "\n") {
			code = strings.TrimPrefix(code, "\n")
		}
		if strings.HasSuffix(code, "\n") {
			code = strings.TrimSuffix(code, "\n")
		}
		segments = append(segments, e2eHoverSemanticSegment{Kind: "code", Language: language, Text: code})
	}
	var visit func(any) error
	visit = func(value any) error {
		switch typed := value.(type) {
		case string:
			e2eAppendHoverMarkdown(typed, appendText, appendCode)
		case []any:
			for _, item := range typed {
				if err := visit(item); err != nil {
					return err
				}
			}
		case map[string]any:
			text, ok := typed["value"].(string)
			if !ok {
				return fmt.Errorf("%w: unsupported hover content object", errE2ENotVerified)
			}
			if language, isMarkedString := typed["language"].(string); isMarkedString {
				appendCode(language, text)
				return nil
			}
			if kind, _ := typed["kind"].(string); kind == "plaintext" {
				appendText("prose", text)
				return nil
			}
			e2eAppendHoverMarkdown(text, appendText, appendCode)
		default:
			return fmt.Errorf("%w: unsupported hover content type %T", errE2ENotVerified, value)
		}
		return nil
	}
	if err := visit(contents); err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return nil, fmt.Errorf("%w: hover has no normalizable semantic content", errE2ENotVerified)
	}
	return segments, nil
}

func e2eHoverPresentation(raw json.RawMessage) (string, error) {
	var hover struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &hover); err != nil {
		return "", fmt.Errorf("decode hover presentation: %w", err)
	}
	var contents any
	if err := json.Unmarshal(hover.Contents, &contents); err != nil {
		return "", fmt.Errorf("decode hover presentation contents: %w", err)
	}
	canonical, err := json.Marshal(contents)
	if err != nil {
		return "", fmt.Errorf("canonicalize hover presentation: %w", err)
	}
	return string(canonical), nil
}

func e2eAppendHoverMarkdown(markdown string, appendText func(string, string), appendCode func(string, string)) {
	markdown = strings.ReplaceAll(strings.ReplaceAll(markdown, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(markdown, "\n")
	var prose []string
	flushProse := func() {
		if len(prose) == 0 {
			return
		}
		appendText("prose", e2eNormalizeHoverMarkdown(strings.Join(prose, " ")))
		prose = nil
	}
	for i := 0; i < len(lines); {
		marker, fenceLen, language, isFence := e2eHoverFenceOpen(lines[i])
		if !isFence {
			prose = append(prose, e2eStripHoverBlockPrefix(lines[i]))
			i++
			continue
		}
		flushProse()
		i++
		var codeLines []string
		for i < len(lines) && !e2eHoverFenceClose(lines[i], marker, fenceLen) {
			codeLines = append(codeLines, lines[i])
			i++
		}
		appendCode(strings.TrimSpace(language), strings.Join(codeLines, "\n"))
		if i < len(lines) {
			i++
		}
	}
	flushProse()
}

func e2eHoverFenceOpen(line string) (byte, int, string, bool) {
	leading := len(line) - len(strings.TrimLeft(line, " "))
	if leading > 3 || leading == len(line) {
		return 0, 0, "", false
	}
	line = line[leading:]
	marker := line[0]
	if marker != '`' && marker != '~' {
		return 0, 0, "", false
	}
	n := 0
	for n < len(line) && line[n] == marker {
		n++
	}
	if n < 3 {
		return 0, 0, "", false
	}
	info := strings.TrimSpace(line[n:])
	if marker == '`' && strings.ContainsRune(info, '`') {
		return 0, 0, "", false
	}
	return marker, n, info, true
}

func e2eHoverFenceClose(line string, marker byte, minimum int) bool {
	leading := len(line) - len(strings.TrimLeft(line, " "))
	if leading > 3 || leading == len(line) {
		return false
	}
	line = line[leading:]
	n := 0
	for n < len(line) && line[n] == marker {
		n++
	}
	return n >= minimum && strings.TrimSpace(line[n:]) == ""
}

func e2eStripHoverBlockPrefix(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	indent := len(line) - len(trimmed)
	if indent > 3 || trimmed == "" {
		return line
	}
	if trimmed[0] == '>' {
		return strings.TrimSpace(trimmed[1:])
	}
	if trimmed[0] == '#' {
		n := 0
		for n < len(trimmed) && trimmed[n] == '#' {
			n++
		}
		if n <= 6 && n < len(trimmed) && (trimmed[n] == ' ' || trimmed[n] == '\t') {
			return strings.TrimSpace(trimmed[n:])
		}
	}
	if (trimmed[0] == '-' || trimmed[0] == '+' || trimmed[0] == '*') && len(trimmed) > 1 && (trimmed[1] == ' ' || trimmed[1] == '\t') {
		return strings.TrimSpace(trimmed[1:])
	}
	return line
}

func e2eNormalizeHoverMarkdown(text string) string {
	text = e2eProtectInlineHoverCode(text)
	text = e2eStripHoverLinks(text)
	for _, delimiter := range []string{"**", "__", "~~", "*", "_"} {
		text = e2eStripHoverDelimiter(text, delimiter)
	}
	text = e2eStripInlineHoverListMarker(text)
	text = strings.ReplaceAll(text, "\x00", "")
	return strings.Join(strings.Fields(text), " ")
}

// Some servers serialize a Markdown list item inline after a section label
// (for example, "Parameters: - int x"). Treat that marker as presentation
// syntax, while preserving minus signs without following whitespace.
func e2eStripInlineHoverListMarker(text string) string {
	text = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) && r != ' ' && r != '\t' {
			return ' '
		}
		return r
	}, text)
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] != ':' {
			out.WriteByte(text[i])
			i++
			continue
		}
		out.WriteByte(text[i])
		i++
		spaceStart := i
		for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
			i++
		}
		if i+1 < len(text) && (text[i] == '-' || text[i] == '+' || text[i] == '*') &&
			(text[i+1] == ' ' || text[i+1] == '\t') {
			out.WriteByte(' ')
			i += 2
			for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
				i++
			}
			continue
		}
		out.WriteString(text[spaceStart:i])
	}
	return out.String()
}

func e2eProtectInlineHoverCode(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] != '`' {
			out.WriteByte(text[i])
			i++
			continue
		}
		start := i
		for i < len(text) && text[i] == '`' {
			i++
		}
		fence := text[start:i]
		end := strings.Index(text[i:], fence)
		if end < 0 {
			out.WriteString(fence)
			continue
		}
		out.WriteByte('\x00')
		out.WriteString(text[i : i+end])
		out.WriteByte('\x00')
		i += end + len(fence)
	}
	return out.String()
}

func e2eStripHoverLinks(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '\x00' {
			end := strings.IndexByte(text[i+1:], '\x00')
			if end >= 0 {
				end += i + 2
				out.WriteString(text[i:end])
				i = end
				continue
			}
		}
		if text[i] != '[' {
			out.WriteByte(text[i])
			i++
			continue
		}
		labelEnd := strings.IndexByte(text[i+1:], ']')
		if labelEnd < 0 {
			out.WriteByte(text[i])
			i++
			continue
		}
		labelEnd += i + 1
		if labelEnd+1 >= len(text) || text[labelEnd+1] != '(' {
			out.WriteByte(text[i])
			i++
			continue
		}
		urlEnd := strings.IndexByte(text[labelEnd+2:], ')')
		if urlEnd < 0 {
			out.WriteByte(text[i])
			i++
			continue
		}
		urlEnd += labelEnd + 2
		out.WriteString(text[i+1 : labelEnd])
		i = urlEnd + 1
	}
	return out.String()
}

func e2eStripHoverDelimiter(text, delimiter string) string {
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] == '\x00' {
			end := strings.IndexByte(text[i+1:], '\x00')
			if end >= 0 {
				end += i + 2
				out.WriteString(text[i:end])
				i = end
				continue
			}
		}
		if !strings.HasPrefix(text[i:], delimiter) || !e2eHoverMarkdownBoundaryBefore(text, i) {
			r, size := utf8.DecodeRuneInString(text[i:])
			out.WriteRune(r)
			i += size
			continue
		}
		contentStart := i + len(delimiter)
		if contentStart >= len(text) || unicode.IsSpace(rune(text[contentStart])) {
			out.WriteString(delimiter)
			i = contentStart
			continue
		}
		searchAt := contentStart
		matched := false
		for searchAt < len(text) {
			rel := strings.Index(text[searchAt:], delimiter)
			if rel < 0 {
				break
			}
			closeAt := searchAt + rel
			contentEnd := closeAt
			after := closeAt + len(delimiter)
			if contentEnd > contentStart && !unicode.IsSpace(rune(text[contentEnd-1])) && e2eHoverMarkdownBoundaryAfter(text, after) {
				out.WriteString(text[contentStart:contentEnd])
				i = after
				matched = true
				break
			}
			searchAt = closeAt + len(delimiter)
		}
		if !matched {
			out.WriteString(delimiter)
			i = contentStart
		}
	}
	return out.String()
}

func e2eHoverMarkdownBoundaryBefore(text string, index int) bool {
	if index == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:index])
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
}

func e2eHoverMarkdownBoundaryAfter(text string, index int) bool {
	if index >= len(text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[index:])
	return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
}

func TestE2EHoverSemanticComparison(t *testing.T) {
	hover := func(value string) json.RawMessage {
		raw, err := json.Marshal(map[string]any{"contents": map[string]string{"kind": "markdown", "value": value}})
		if err != nil {
			t.Fatalf("marshal hover fixture: %v", err)
		}
		return raw
	}
	upstream := hover("**FindUser** returns `*User*`.\n\n```go\nfunc FindUser(id int) *User\n```")
	presentationOnly := hover("### `FindUser` returns **`*User*`**.\n~~~go\nfunc FindUser(id int) *User\n~~~")
	if err := e2eCompareHoverSemantics(presentationOnly, upstream); err != nil {
		t.Fatalf("presentation-only Markdown differences should compare semantically equal: %v", err)
	}
	displayDiffers, err := func() (bool, error) {
		candidate, err := e2eHoverPresentation(presentationOnly)
		if err != nil {
			return false, err
		}
		baseline, err := e2eHoverPresentation(upstream)
		if err != nil {
			return false, err
		}
		return candidate != baseline, nil
	}()
	if err != nil || !displayDiffers {
		t.Fatalf("expected equivalent semantic hover content to retain a display-only difference, differs=%v err=%v", displayDiffers, err)
	}

	semanticMismatch := hover("**FindUser** returns `*Admin`.\n\n```go\nfunc FindUser(id int) *Admin\n```")
	if err := e2eCompareHoverSemantics(semanticMismatch, upstream); err == nil {
		t.Fatal("a changed return type must fail the semantic hover comparison")
	}

	candidateList := hover("function targetPerf → int\n\nParameters: int x\n\n```c\nint targetPerf(int x)\n```")
	upstreamList := hover("function targetPerf → int\n\nParameters: - int x\n\n```c\nint targetPerf(int x)\n```")
	if err := e2eCompareHoverSemantics(candidateList, upstreamList); err != nil {
		t.Fatalf("inline Markdown list marker should be classified as presentation: %v", err)
	}
	plainHover := json.RawMessage(`{"contents":{"kind":"plaintext","value":"function targetPerf → int Parameters: int x"}}`)
	markdownHover := hover("function targetPerf → int Parameters: - int x")
	if err := e2eCompareHoverSemantics(plainHover, markdownHover); err != nil {
		t.Fatalf("plain-text and Markdown parameter-list presentation should compare semantically: %v", err)
	}
	nonbreakingSpaceHover := json.RawMessage(`{"contents":{"kind":"plaintext","value":"function targetPerf → int Parameters:\u00a0-\u00a0int x"}}`)
	if err := e2eCompareHoverSemantics(nonbreakingSpaceHover, markdownHover); err != nil {
		t.Fatalf("Unicode whitespace around an inline Markdown list marker is presentation: %v", err)
	}
}

func TestE2ES18DocumentSymbolsCompareSymbolSpanAcrossResponseShapes(t *testing.T) {
	fixture := e2eS18Fixture{queryFile: "use.c", targetFile: "target.c", queryToken: "callTarget"}
	candidate := json.RawMessage(`[{"name":"targetPerf","kind":12,"location":{"uri":"file:///target.c","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":39}}}}]`)
	baseline := json.RawMessage(`[{"name":"targetPerf","kind":12,"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":39}},"selectionRange":{"start":{"line":0,"character":4},"end":{"line":0,"character":14}},"children":[]}]`)
	if err := e2eCompareS18DocumentSymbols(candidate, baseline, fixture, "targetPerf"); err != nil {
		t.Fatalf("equivalent SymbolInformation and DocumentSymbol spans should match: %v", err)
	}

	baseline = json.RawMessage(`[{"name":"targetPerf","kind":12,"range":{"start":{"line":0,"character":1},"end":{"line":0,"character":39}},"selectionRange":{"start":{"line":0,"character":4},"end":{"line":0,"character":14}},"children":[]}]`)
	if err := e2eCompareS18DocumentSymbols(candidate, baseline, fixture, "targetPerf"); err == nil {
		t.Fatal("different symbol spans must fail the differential comparison")
	}
}

func TestE2ES18ProgressStateTracksActiveWork(t *testing.T) {
	notifications := []upstream.Notification{
		{Method: "$/progress", Params: json.RawMessage(`{"token":"index","value":{"kind":"begin","title":"Indexing"}}`)},
		{Method: "$/progress", Params: json.RawMessage(`{"token":"index","value":{"kind":"report","message":"half"}}`)},
		{Method: "$/progress", Params: json.RawMessage(`{"token":"index","value":{"kind":"end"}}`)},
	}
	state, err := e2eS18ProgressStateFromNotifications(notifications)
	if err != nil {
		t.Fatalf("parse progress events: %v", err)
	}
	if state.begin != 1 || state.end != 1 || len(state.active) != 0 {
		t.Fatalf("completed progress event sequence left unexpected state: %+v", state)
	}
	active, err := e2eS18ProgressStateFromNotifications(notifications[:1])
	if err != nil {
		t.Fatalf("parse active progress event: %v", err)
	}
	if _, ok := active.active[`"index"`]; !ok {
		t.Fatalf("begin event did not retain active token: %+v", active)
	}
	if _, err := e2eS18ProgressStateFromNotifications([]upstream.Notification{{
		Method: "$/progress", Params: json.RawMessage(`{"value":{"kind":"begin"}}`),
	}}); err == nil {
		t.Fatal("progress event without token must not permit readiness")
	}
}

func TestE2ES18ReadinessHelpersRequireStableSemanticObservations(t *testing.T) {
	previous := []string{"symbol", "file:///workspace/lib.rs"}
	count := e2eS18NextStableObservationCount(nil, previous, 0)
	count = e2eS18NextStableObservationCount(previous, []string{"symbol", "file:///workspace/lib.rs"}, count)
	if count != 2 {
		t.Fatalf("equal normalized observations should accumulate stability, got %d", count)
	}
	count = e2eS18NextStableObservationCount(previous, []string{"other", "file:///workspace/lib.rs"}, count)
	if count != 1 {
		t.Fatalf("changed semantic observation should reset stability, got %d", count)
	}

	original := map[string]any{"position": map[string]uint32{"line": 1, "character": 2}}
	params, ok := e2eS18ReadinessParams(original, "ready-token").(map[string]any)
	if !ok || params["workDoneToken"] != "ready-token" {
		t.Fatalf("readiness request omitted workDoneToken: %#v", params)
	}
	if _, exists := original["workDoneToken"]; exists {
		t.Fatal("readiness params mutated the operation's semantic request")
	}
}

func validateE2ECompletion(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("%w: completion result is null", errE2ENotVerified)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if listErr := json.Unmarshal(raw, &list); listErr != nil {
			return fmt.Errorf("decode completion: %w", err)
		}
		items = list.Items
	}
	if len(items) == 0 {
		return fmt.Errorf("%w: completion returned no usable candidates", errE2ENotVerified)
	}
	return nil
}

func validateE2ECompletionFor(raw json.RawMessage, symbol string) error {
	if err := validateE2ECompletion(raw); err != nil {
		return err
	}
	type item struct {
		Label string `json:"label"`
	}
	var items []item
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
		if err := json.Unmarshal(raw, &items); err != nil {
			return fmt.Errorf("decode completion labels: %w", err)
		}
	} else {
		var result struct {
			Items []item `json:"items"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("decode completion labels: %w", err)
		}
		items = result.Items
	}
	for _, item := range items {
		if strings.Contains(item.Label, symbol) {
			return nil
		}
	}
	return fmt.Errorf("completion omitted expected symbol %q", symbol)
}

func e2eNormalizedCompletionLabels(raw json.RawMessage, symbol string) ([]string, error) {
	type item struct {
		Label string `json:"label"`
	}
	items := []item{}
	if len(raw) > 0 && raw[0] == '[' {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("decode completion items: %w", err)
		}
	} else {
		var list struct {
			Items []item `json:"items"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("decode completion list: %w", err)
		}
		items = list.Items
	}
	labels := []string{}
	seen := map[string]bool{}
	for _, item := range items {
		label := strings.Join(strings.Fields(item.Label), " ")
		if label != "" && strings.Contains(label, symbol) && !seen[label] {
			seen[label] = true
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	if len(labels) == 0 {
		return nil, fmt.Errorf("%w: completion has no normalized label for %q", errE2ENotVerified, symbol)
	}
	return labels, nil
}

type e2eCompletionSemanticSnapshot struct {
	IsIncomplete bool
	Items        []e2eCompletionItemSemantics
}

type e2eCompletionSnapshot struct {
	Semantics     e2eCompletionSemanticSnapshot
	Presentations []e2eCompletionPresentation
}

type e2eCompletionItemSemantics struct {
	Label               string
	Kind                *int
	InsertText          string
	InsertTextFormat    int
	SortText            string
	FilterText          string
	TextEdit            *e2eCompletionTextEdit
	AdditionalTextEdits []e2eCompletionTextEdit
}

type e2eCompletionPresentation struct {
	Detail        any
	Documentation any
}

type e2eCompletionPosition struct {
	Line      uint32
	Character uint32
}

type e2eCompletionRange struct {
	Start e2eCompletionPosition
	End   e2eCompletionPosition
}

type e2eCompletionTextEdit struct {
	Range   *e2eCompletionRange
	Insert  *e2eCompletionRange
	Replace *e2eCompletionRange
	NewText string
}

type e2eCompletionRawItem struct {
	Label               *string         `json:"label"`
	Kind                *int            `json:"kind"`
	Detail              json.RawMessage `json:"detail"`
	Documentation       json.RawMessage `json:"documentation"`
	InsertText          *string         `json:"insertText"`
	InsertTextFormat    *int            `json:"insertTextFormat"`
	SortText            *string         `json:"sortText"`
	FilterText          *string         `json:"filterText"`
	TextEdit            json.RawMessage `json:"textEdit"`
	AdditionalTextEdits json.RawMessage `json:"additionalTextEdits"`
}

type e2eCompletionDefaults struct {
	editRange        *e2eCompletionEditShape
	insertTextFormat *int
}

type e2eCompletionEditShape struct {
	rangeValue   *e2eCompletionRange
	insertValue  *e2eCompletionRange
	replaceValue *e2eCompletionRange
}

func e2eDecodeCompletionSnapshot(raw json.RawMessage) (e2eCompletionSnapshot, error) {
	var snapshot e2eCompletionSnapshot
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return snapshot, fmt.Errorf("%w: completion result is null", errE2ENotVerified)
	}
	var itemRaws []json.RawMessage
	var defaults e2eCompletionDefaults
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(raw, &itemRaws); err != nil {
			return snapshot, fmt.Errorf("decode completion item array: %w", err)
		}
	} else {
		var list struct {
			IsIncomplete json.RawMessage   `json:"isIncomplete"`
			Items        []json.RawMessage `json:"items"`
			ItemDefaults json.RawMessage   `json:"itemDefaults"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			return snapshot, fmt.Errorf("decode completion list: %w", err)
		}
		if len(list.IsIncomplete) != 0 && strings.TrimSpace(string(list.IsIncomplete)) != "null" {
			if err := json.Unmarshal(list.IsIncomplete, &snapshot.Semantics.IsIncomplete); err != nil {
				return snapshot, fmt.Errorf("decode completion isIncomplete: %w", err)
			}
		}
		var err error
		defaults, err = e2eDecodeCompletionDefaults(list.ItemDefaults)
		if err != nil {
			return snapshot, err
		}
		itemRaws = list.Items
	}
	if len(itemRaws) == 0 {
		return snapshot, fmt.Errorf("%w: completion returned no usable candidates", errE2ENotVerified)
	}
	snapshot.Semantics.Items = make([]e2eCompletionItemSemantics, 0, len(itemRaws))
	snapshot.Presentations = make([]e2eCompletionPresentation, 0, len(itemRaws))
	for index, itemRaw := range itemRaws {
		var item e2eCompletionRawItem
		if err := json.Unmarshal(itemRaw, &item); err != nil {
			return e2eCompletionSnapshot{}, fmt.Errorf("decode completion item %d: %w", index, err)
		}
		if item.Label == nil || strings.TrimSpace(*item.Label) == "" {
			return e2eCompletionSnapshot{}, fmt.Errorf("%w: completion item %d has no label", errE2ENotVerified, index)
		}
		insertText := *item.Label
		if item.InsertText != nil {
			insertText = *item.InsertText
		}
		sortText := *item.Label
		if item.SortText != nil {
			sortText = *item.SortText
		}
		filterText := *item.Label
		if item.FilterText != nil {
			filterText = *item.FilterText
		}
		insertTextFormat := 1 // LSP defaults to InsertTextFormat.PlainText.
		if defaults.insertTextFormat != nil {
			insertTextFormat = *defaults.insertTextFormat
		}
		if item.InsertTextFormat != nil {
			insertTextFormat = *item.InsertTextFormat
		}
		textEdit, err := e2eDecodeCompletionTextEdit(item.TextEdit)
		if err != nil {
			return e2eCompletionSnapshot{}, fmt.Errorf("completion item %d textEdit: %w", index, err)
		}
		if textEdit == nil && defaults.editRange != nil {
			textEdit = &e2eCompletionTextEdit{
				Range: defaults.editRange.rangeValue, Insert: defaults.editRange.insertValue,
				Replace: defaults.editRange.replaceValue, NewText: insertText,
			}
		}
		additionalTextEdits, err := e2eDecodeCompletionAdditionalTextEdits(item.AdditionalTextEdits)
		if err != nil {
			return e2eCompletionSnapshot{}, fmt.Errorf("completion item %d additionalTextEdits: %w", index, err)
		}
		detail, err := e2eDecodeCompletionPresentation(item.Detail)
		if err != nil {
			return e2eCompletionSnapshot{}, fmt.Errorf("completion item %d detail: %w", index, err)
		}
		documentation, err := e2eDecodeCompletionPresentation(item.Documentation)
		if err != nil {
			return e2eCompletionSnapshot{}, fmt.Errorf("completion item %d documentation: %w", index, err)
		}
		snapshot.Semantics.Items = append(snapshot.Semantics.Items, e2eCompletionItemSemantics{
			Label: *item.Label, Kind: item.Kind, InsertText: insertText, InsertTextFormat: insertTextFormat,
			SortText: sortText, FilterText: filterText, TextEdit: textEdit, AdditionalTextEdits: additionalTextEdits,
		})
		snapshot.Presentations = append(snapshot.Presentations, e2eCompletionPresentation{Detail: detail, Documentation: documentation})
	}
	return snapshot, nil
}

func e2eDecodeCompletionDefaults(raw json.RawMessage) (e2eCompletionDefaults, error) {
	var defaults e2eCompletionDefaults
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return defaults, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return defaults, fmt.Errorf("decode completion itemDefaults: %w", err)
	}
	for name, value := range values {
		if strings.TrimSpace(string(value)) == "null" {
			continue
		}
		switch name {
		case "insertTextFormat":
			var format int
			if err := json.Unmarshal(value, &format); err != nil {
				return defaults, fmt.Errorf("decode completion itemDefaults.insertTextFormat: %w", err)
			}
			defaults.insertTextFormat = &format
		case "editRange":
			shape, err := e2eDecodeCompletionEditShape(value)
			if err != nil {
				return defaults, fmt.Errorf("decode completion itemDefaults.editRange: %w", err)
			}
			defaults.editRange = shape
		default:
			return defaults, fmt.Errorf("%w: completion itemDefaults.%s is not projected for semantic comparison", errE2ENotVerified, name)
		}
	}
	return defaults, nil
}

func e2eDecodeCompletionEditShape(raw json.RawMessage) (*e2eCompletionEditShape, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		if err == nil {
			err = errors.New("edit is not an object")
		}
		return nil, err
	}
	get := func(name string) json.RawMessage {
		value := fields[name]
		if strings.TrimSpace(string(value)) == "null" {
			return nil
		}
		return value
	}
	rangeRaw, insertRaw, replaceRaw := get("range"), get("insert"), get("replace")
	switch {
	case len(rangeRaw) != 0 && len(insertRaw) == 0 && len(replaceRaw) == 0:
		rng, err := e2eDecodeCompletionRange(rangeRaw)
		if err != nil {
			return nil, err
		}
		return &e2eCompletionEditShape{rangeValue: rng}, nil
	case len(rangeRaw) == 0 && len(insertRaw) != 0 && len(replaceRaw) != 0:
		insert, err := e2eDecodeCompletionRange(insertRaw)
		if err != nil {
			return nil, fmt.Errorf("insert range: %w", err)
		}
		replace, err := e2eDecodeCompletionRange(replaceRaw)
		if err != nil {
			return nil, fmt.Errorf("replace range: %w", err)
		}
		shape := &e2eCompletionEditShape{insertValue: insert, replaceValue: replace}
		if reflect.DeepEqual(insert, replace) {
			shape.rangeValue = insert
			shape.insertValue, shape.replaceValue = nil, nil
		}
		return shape, nil
	default:
		return nil, errors.New("edit must use exactly one range form or a complete insert/replace form")
	}
}

func e2eDecodeCompletionTextEdit(raw json.RawMessage) (*e2eCompletionTextEdit, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		if err == nil {
			err = errors.New("textEdit is not an object")
		}
		return nil, err
	}
	newTextRaw, exists := fields["newText"]
	if !exists || strings.TrimSpace(string(newTextRaw)) == "null" {
		return nil, errors.New("textEdit has no newText")
	}
	var newText string
	if err := json.Unmarshal(newTextRaw, &newText); err != nil {
		return nil, fmt.Errorf("decode newText: %w", err)
	}
	shape, err := e2eDecodeCompletionEditShape(raw)
	if err != nil {
		return nil, err
	}
	return &e2eCompletionTextEdit{
		Range: shape.rangeValue, Insert: shape.insertValue, Replace: shape.replaceValue, NewText: newText,
	}, nil
}

func e2eDecodeCompletionRange(raw json.RawMessage) (*e2eCompletionRange, error) {
	var value struct {
		Start json.RawMessage `json:"start"`
		End   json.RawMessage `json:"end"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	start, err := e2eDecodeCompletionPosition(value.Start)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	end, err := e2eDecodeCompletionPosition(value.End)
	if err != nil {
		return nil, fmt.Errorf("end: %w", err)
	}
	return &e2eCompletionRange{Start: *start, End: *end}, nil
}

func e2eDecodeCompletionPosition(raw json.RawMessage) (*e2eCompletionPosition, error) {
	var value struct {
		Line      *uint32 `json:"line"`
		Character *uint32 `json:"character"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	if value.Line == nil || value.Character == nil {
		return nil, errors.New("position must include line and character")
	}
	return &e2eCompletionPosition{Line: *value.Line, Character: *value.Character}, nil
}

func e2eDecodeCompletionAdditionalTextEdits(raw json.RawMessage) ([]e2eCompletionTextEdit, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return []e2eCompletionTextEdit{}, nil
	}
	var editRaws []json.RawMessage
	if err := json.Unmarshal(raw, &editRaws); err != nil {
		return nil, err
	}
	edits := make([]e2eCompletionTextEdit, 0, len(editRaws))
	for index, editRaw := range editRaws {
		edit, err := e2eDecodeCompletionTextEdit(editRaw)
		if err != nil {
			return nil, fmt.Errorf("edit %d: %w", index, err)
		}
		if edit == nil || edit.Range == nil {
			return nil, fmt.Errorf("edit %d must use range/newText form", index)
		}
		edits = append(edits, *edit)
	}
	// Additional edits are applied as one non-overlapping edit set; their JSON
	// array order does not change the edit semantics.
	sort.Slice(edits, func(i, j int) bool {
		left, _ := json.Marshal(edits[i])
		right, _ := json.Marshal(edits[j])
		return string(left) < string(right)
	})
	return edits, nil
}

func e2eDecodeCompletionPresentation(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func e2eCompareCompletionItemSequences(candidate, upstream []e2eCompletionItemSemantics) error {
	if len(candidate) != len(upstream) {
		return fmt.Errorf("completion target item count differs: candidate=%d upstream=%d", len(candidate), len(upstream))
	}
	for index := range candidate {
		candidateItem, upstreamItem := candidate[index], upstream[index]
		if fields := e2eCompletionSemanticDifference(candidateItem, upstreamItem); len(fields) > 0 {
			return fmt.Errorf("completion target item at ordered index %d differs (candidate label %q; upstream label %q): %s", index, candidateItem.Label, upstreamItem.Label, strings.Join(fields, ", "))
		}
	}
	return nil
}

func e2eCompletionSemanticDifference(candidate, upstream e2eCompletionItemSemantics) []string {
	different := []string{}
	if candidate.Label != upstream.Label {
		different = append(different, "label")
	}
	if !reflect.DeepEqual(candidate.Kind, upstream.Kind) {
		different = append(different, "kind")
	}
	if candidate.InsertText != upstream.InsertText {
		different = append(different, "insertText")
	}
	if candidate.InsertTextFormat != upstream.InsertTextFormat {
		different = append(different, "insertTextFormat")
	}
	if candidate.SortText != upstream.SortText {
		different = append(different, "sortText")
	}
	if candidate.FilterText != upstream.FilterText {
		different = append(different, "filterText")
	}
	if !reflect.DeepEqual(candidate.TextEdit, upstream.TextEdit) {
		different = append(different, "textEdit")
	}
	if !reflect.DeepEqual(candidate.AdditionalTextEdits, upstream.AdditionalTextEdits) {
		different = append(different, "additionalTextEdits")
	}
	return different
}

func e2eCompareS18CompletionTargetSemantics(candidate, upstream json.RawMessage, expectedSymbol string) error {
	candidateSnapshot, err := e2eDecodeCompletionSnapshot(candidate)
	if err != nil {
		return err
	}
	upstreamSnapshot, err := e2eDecodeCompletionSnapshot(upstream)
	if err != nil {
		return err
	}
	candidateTargets := e2eCompletionTargets(candidateSnapshot.Semantics.Items, expectedSymbol)
	upstreamTargets := e2eCompletionTargets(upstreamSnapshot.Semantics.Items, expectedSymbol)
	if len(candidateTargets) == 0 || len(upstreamTargets) == 0 {
		return fmt.Errorf("%w: target completion candidates for %q are missing (candidate=%d upstream=%d)", errE2ENotVerified, expectedSymbol, len(candidateTargets), len(upstreamTargets))
	}
	return e2eCompareCompletionItemSequences(candidateTargets, upstreamTargets)
}

func e2eCompletionTargets(items []e2eCompletionItemSemantics, expectedSymbol string) []e2eCompletionItemSemantics {
	targets := make([]e2eCompletionItemSemantics, 0)
	for _, item := range items {
		if strings.Contains(item.Label, expectedSymbol) {
			targets = append(targets, item)
		}
	}
	return targets
}

func e2eS18CompletionPresentationDifference(candidate, upstream json.RawMessage) (bool, error) {
	candidateSnapshot, err := e2eDecodeCompletionSnapshot(candidate)
	if err != nil {
		return false, err
	}
	upstreamSnapshot, err := e2eDecodeCompletionSnapshot(upstream)
	if err != nil {
		return false, err
	}
	if len(candidateSnapshot.Presentations) != len(upstreamSnapshot.Presentations) {
		return false, errors.New("completion item count differs before presentation classification")
	}
	return !reflect.DeepEqual(candidateSnapshot.Presentations, upstreamSnapshot.Presentations), nil
}

func TestE2ES18CompletionSemanticComparisonAcceptsEquivalentWireShapes(t *testing.T) {
	const candidate = `[{"label":"target","kind":3,"insertText":"target","sortText":"target","filterText":"target","textEdit":{"range":{"start":{"line":0,"character":2},"end":{"line":0,"character":8}},"newText":"target"},"additionalTextEdits":[],"detail":"candidate detail","documentation":"candidate docs"}]`
	const upstreamResult = `{"isIncomplete":false,"itemDefaults":{"insertTextFormat":1,"editRange":{"insert":{"start":{"line":0,"character":2},"end":{"line":0,"character":8}},"replace":{"start":{"line":0,"character":2},"end":{"line":0,"character":8}}}},"items":[{"label":"target","kind":3,"insertText":"target","sortText":"target","filterText":"target","detail":"upstream detail","documentation":{"kind":"markdown","value":"upstream docs"}}]}`
	if err := e2eCompareS18CompletionTargetSemantics(json.RawMessage(candidate), json.RawMessage(upstreamResult), "target"); err != nil {
		t.Fatalf("equivalent completion array/list shapes should compare equal: %v", err)
	}
	differs, err := e2eS18CompletionPresentationDifference(json.RawMessage(candidate), json.RawMessage(upstreamResult))
	if err != nil {
		t.Fatalf("classify presentation fields: %v", err)
	}
	if !differs {
		t.Fatal("detail/documentation-only changes should be reported as presentation differences")
	}
}

func TestE2ES18CompletionTargetComparisonRejectsMissingAndReorderedItems(t *testing.T) {
	tests := []struct {
		name      string
		candidate string
		upstream  string
	}{
		{
			name:      "missing target overload",
			candidate: `[{"label":"target(int)","kind":3}]`,
			upstream:  `{"items":[{"label":"other","kind":6}]}`,
		},
		{
			name:      "target order",
			candidate: `[{"label":"target(int)","kind":3},{"label":"target(float)","kind":3}]`,
			upstream:  `{"items":[{"label":"target(float)","kind":3},{"label":"target(int)","kind":3}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := e2eCompareS18CompletionTargetSemantics(json.RawMessage(test.candidate), json.RawMessage(test.upstream), "target"); err == nil {
				t.Fatal("missing/reordered target candidates must not pass")
			}
		})
	}
}

func TestE2ES18CompletionTargetComparisonRejectsEditChanges(t *testing.T) {
	const base = `{"isIncomplete":false,"items":[{"label":"target","kind":3,"textEdit":{"range":{"start":{"line":0,"character":1},"end":{"line":0,"character":7}},"newText":"target"},"additionalTextEdits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"#include <x>"}]}]}`
	tests := []struct {
		name      string
		candidate string
		upstream  string
	}{
		{
			name:      "structured edits",
			candidate: base,
			upstream:  `{"isIncomplete":false,"items":[{"label":"target","kind":3,"textEdit":{"range":{"start":{"line":0,"character":1},"end":{"line":0,"character":8}},"newText":"targetNew"},"additionalTextEdits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"#include <y>"}]}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := e2eCompareS18CompletionTargetSemantics(json.RawMessage(test.candidate), json.RawMessage(test.upstream), "target"); err == nil {
				t.Fatal("semantic completion change must be rejected")
			}
		})
	}
}

func TestE2ES18CompletionFullListValidationAcceptsPerRequestUpdates(t *testing.T) {
	first := json.RawMessage(`{"isIncomplete":false,"items":[{"label":"target(int)","kind":3,"textEdit":{"range":{"start":{"line":0,"character":3},"end":{"line":0,"character":8}},"newText":"target(int)"},"additionalTextEdits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"#include <x>"}]}]}`)
	updated := json.RawMessage(`{"isIncomplete":true,"items":[{"label":"helper(float)","kind":3},{"label":"target(int)","kind":3,"textEdit":{"range":{"start":{"line":0,"character":3},"end":{"line":0,"character":8}},"newText":"target(int)"},"additionalTextEdits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"#include <x>"}]}]}`)
	check := e2eS18OperationSpec{
		method:   "textDocument/completion",
		validate: func(raw json.RawMessage) error { return validateE2ECompletionFor(raw, "target") },
	}
	for name, raw := range map[string]json.RawMessage{"first": first, "updated": updated} {
		t.Run(name, func(t *testing.T) {
			if err := e2eS18ValidateFullCompletionResponse(check, raw); err != nil {
				t.Fatalf("validate complete response independently: %v", err)
			}
			snapshot, err := e2eDecodeCompletionSnapshot(raw)
			if err != nil {
				t.Fatalf("decode complete response: %v", err)
			}
			if name == "updated" && (!snapshot.Semantics.IsIncomplete || len(snapshot.Semantics.Items) != 2 || snapshot.Semantics.Items[0].Label != "helper(float)") {
				t.Fatalf("response-local list semantics were lost: %+v", snapshot.Semantics)
			}
		})
	}
	if err := e2eCompareS18CompletionTargetSemantics(first, updated, "target"); err != nil {
		t.Fatalf("changed full lists with matching target semantics should compare as a valid response pair: %v", err)
	}
	malformed := json.RawMessage(`{"isIncomplete":true,"items":[{"label":"target(int)","kind":3},{"kind":3}]}`)
	if err := e2eS18ValidateFullCompletionResponse(check, malformed); err == nil {
		t.Fatal("malformed non-target item was accepted in a full completion response")
	}
}

func TestE2ES18CompletionEligibilityRequiresCompleteFullListEvidence(t *testing.T) {
	sample, _ := e2eS18ABBAExceptionTestSample("full-list-eligibility", "c", "completion_first_usable", 40, 80, 100, "pass")
	if !e2eS18CompletionSemanticEligible(sample) {
		t.Fatal("complete target and full-list evidence was rejected")
	}
	for _, test := range []struct {
		name   string
		mutate func(*e2eS18SemanticEvidence)
	}{
		{name: "snapshot pending", mutate: func(e *e2eS18SemanticEvidence) { e.FullListSnapshotStatus = "pending" }},
		{name: "sample missing", mutate: func(e *e2eS18SemanticEvidence) { e.FullListSampleStatus = "not_verified" }},
		{name: "target missing", mutate: func(e *e2eS18SemanticEvidence) { e.TargetSampleStatus = "not_verified" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := sample
			mutated.SemanticEvidence = new(e2eS18SemanticEvidence)
			*mutated.SemanticEvidence = *sample.SemanticEvidence
			test.mutate(mutated.SemanticEvidence)
			if e2eS18CompletionSemanticEligible(mutated) {
				t.Fatal("incomplete completion evidence was accepted")
			}
		})
	}
}

func TestE2ES18ExecutionRequiresFullListEvidenceForCAndCppCompletion(t *testing.T) {
	t.Setenv(e2eS18PolicyEnv, "stable")
	const runID = "execution-full-list-gate"
	for _, fixtureID := range []string{"c", "cpp"} {
		t.Run(fixtureID, func(t *testing.T) {
			report := e2eCompleteSyntheticPerformanceReport(runID)
			if !e2eS18ExecutionComplete(report) {
				t.Fatal("complete C/C++ completion evidence did not complete the execution")
			}
			id := runID + "/S18/" + fixtureID + "/completion_first_usable"
			sample := report.S18.Operations[id]
			sample.SemanticEvidence.FullListSampleStatus = "not_verified"
			report.S18.Operations[id] = sample
			if e2eS18ExecutionComplete(report) {
				t.Fatal("execution completed without per-sample full-list validation")
			}
		})
	}
}

func TestE2ES18CandidateEnvForwardsTraceProvenanceOnlyWithTrace(t *testing.T) {
	t.Setenv(e2eS18CompletionPhaseTraceEnv, `C:\phase-trace.json`)
	t.Setenv(acceptanceRunIDEnv, "formal-run-id")
	t.Setenv(candidateSHAEnv, "sha256:formal-candidate")
	env := e2eS18CandidateEnv("", "", "")
	want := []string{
		e2eS18CompletionPhaseTraceEnv + `=C:\phase-trace.json`,
		acceptanceRunIDEnv + "=formal-run-id",
		candidateSHAEnv + "=sha256:formal-candidate",
	}
	for _, expected := range want {
		found := false
		for _, entry := range env {
			if entry == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("candidate environment is missing %q: %v", expected, env)
		}
	}

	t.Setenv(e2eS18CompletionPhaseTraceEnv, "")
	env = e2eS18CandidateEnv("", "", "")
	for _, name := range []string{acceptanceRunIDEnv, candidateSHAEnv} {
		for _, entry := range env {
			if strings.HasPrefix(entry, name+"=") {
				t.Errorf("candidate environment forwarded %s without phase tracing", name)
			}
		}
	}
}

func TestE2ES18CompletionTargetComparisonRejectsStaleResult(t *testing.T) {
	const stale = `[{"label":"targetPerf","kind":3,"insertText":"targetPerf","textEdit":{"range":{"start":{"line":0,"character":3},"end":{"line":0,"character":8}},"newText":"targetPerf"}}]`
	const current = `{"isIncomplete":true,"items":[{"label":"targetPerf","kind":3,"insertText":"targetPerf","textEdit":{"range":{"start":{"line":0,"character":3},"end":{"line":0,"character":12}},"newText":"targetPerf"}}]}`
	if err := validateE2ECompletionFor(json.RawMessage(stale), "targetPerf"); err != nil {
		t.Fatalf("stale fixture should still pass the old label-presence assertion: %v", err)
	}
	if err := e2eCompareS18CompletionTargetSemantics(json.RawMessage(stale), json.RawMessage(current), "targetPerf"); err == nil {
		t.Fatal("target completion with an old edit range must not pass as fresh")
	}
}

func TestE2ES18CompletionReadinessTracksFullSnapshotGrowth(t *testing.T) {
	fixture := e2eS18Fixture{id: "cpp"}
	first, err := e2eS18ReadinessObservation(fixture, "textDocument/completion", "target", json.RawMessage(`{"isIncomplete":false,"items":[{"label":"target","kind":3}]}`))
	if err != nil {
		t.Fatalf("decode first readiness snapshot: %v", err)
	}
	count := e2eS18NextStableObservationCount(nil, first, 0)
	grown, err := e2eS18ReadinessObservation(fixture, "textDocument/completion", "target", json.RawMessage(`{"isIncomplete":true,"items":[{"label":"target","kind":3},{"label":"targetExtra","kind":3}]}`))
	if err != nil {
		t.Fatalf("decode grown readiness snapshot: %v", err)
	}
	count = e2eS18NextStableObservationCount(first, grown, count)
	if count != 1 {
		t.Fatalf("list growth/incomplete change must reset readiness stability to one observation, got %d", count)
	}
	count = e2eS18NextStableObservationCount(grown, grown, count)
	if count != 2 {
		t.Fatalf("matching full snapshots should accumulate stability, got %d", count)
	}
	incompleteOnly, err := e2eS18ReadinessObservation(fixture, "textDocument/completion", "target", json.RawMessage(`{"isIncomplete":false,"items":[{"label":"target","kind":3},{"label":"targetExtra","kind":3}]}`))
	if err != nil {
		t.Fatalf("decode same-list isIncomplete change: %v", err)
	}
	if reflect.DeepEqual(grown, incompleteOnly) {
		t.Fatal("isIncomplete must be part of full-snapshot readiness stability")
	}
	count = e2eS18NextStableObservationCount(grown, incompleteOnly, count)
	if count != 1 {
		t.Fatalf("isIncomplete change must reset full-snapshot stability, got %d", count)
	}
}

func validateE2ESymbols(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("%w: document symbols result is null", errE2ENotVerified)
	}
	var symbols []json.RawMessage
	if err := json.Unmarshal(raw, &symbols); err != nil {
		return fmt.Errorf("decode document symbols: %w", err)
	}
	if len(symbols) == 0 {
		return fmt.Errorf("%w: document symbols result is empty", errE2ENotVerified)
	}
	return nil
}

func validateE2ESymbolsFor(raw json.RawMessage, symbol string) error {
	if err := validateE2ESymbols(raw); err != nil {
		return err
	}
	names, err := e2eSymbolNames(raw)
	if err != nil {
		return err
	}
	for _, name := range names {
		if strings.Contains(name, symbol) {
			return nil
		}
	}
	return fmt.Errorf("%w: document symbols omitted expected declaration %q; got %v", errE2ENotVerified, symbol, names)
}

type e2eSymbolShape struct {
	Name           string `json:"name"`
	Kind           uint32 `json:"kind,omitempty"`
	StartLine      uint32 `json:"startLine,omitempty"`
	StartCharacter uint32 `json:"startCharacter,omitempty"`
	EndLine        uint32 `json:"endLine,omitempty"`
	EndCharacter   uint32 `json:"endCharacter,omitempty"`
}

func e2eNormalizedSymbolShapes(raw json.RawMessage) ([]e2eSymbolShape, error) {
	var symbols []struct {
		Name  string `json:"name"`
		Kind  uint32 `json:"kind"`
		Range *struct {
			Start struct {
				Line      uint32 `json:"line"`
				Character uint32 `json:"character"`
			} `json:"start"`
			End struct {
				Line      uint32 `json:"line"`
				Character uint32 `json:"character"`
			} `json:"end"`
		} `json:"range"`
		SelectionRange *struct {
			Start struct {
				Line      uint32 `json:"line"`
				Character uint32 `json:"character"`
			} `json:"start"`
			End struct {
				Line      uint32 `json:"line"`
				Character uint32 `json:"character"`
			} `json:"end"`
		} `json:"selectionRange"`
		Location *struct {
			Range struct {
				Start struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"start"`
				End struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"end"`
			} `json:"range"`
		} `json:"location"`
		Children []json.RawMessage `json:"children"`
	}
	if err := json.Unmarshal(raw, &symbols); err != nil {
		return nil, fmt.Errorf("decode document symbols: %w", err)
	}
	shapes := []e2eSymbolShape{}
	var appendSymbol func(json.RawMessage, string) error
	appendSymbol = func(encoded json.RawMessage, parent string) error {
		var symbol struct {
			Name  string `json:"name"`
			Kind  uint32 `json:"kind"`
			Range *struct {
				Start struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"start"`
				End struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"end"`
			} `json:"range"`
			SelectionRange *struct {
				Start struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"start"`
				End struct {
					Line      uint32 `json:"line"`
					Character uint32 `json:"character"`
				} `json:"end"`
			} `json:"selectionRange"`
			Location *struct {
				Range struct {
					Start struct {
						Line      uint32 `json:"line"`
						Character uint32 `json:"character"`
					} `json:"start"`
					End struct {
						Line      uint32 `json:"line"`
						Character uint32 `json:"character"`
					} `json:"end"`
				} `json:"range"`
			} `json:"location"`
			Children []json.RawMessage `json:"children"`
		}
		if err := json.Unmarshal(encoded, &symbol); err != nil {
			return fmt.Errorf("decode document-symbol entry: %w", err)
		}
		name := symbol.Name
		if parent != "" {
			name = parent + "/" + name
		}
		shape := e2eSymbolShape{Name: name, Kind: symbol.Kind}
		// Compare the symbol span, represented by DocumentSymbol.range or
		// SymbolInformation.location.range. selectionRange is a separate
		// client-selection hint and may be narrower or absent on either server.
		rng := symbol.Range
		if rng != nil {
			shape.StartLine, shape.StartCharacter = rng.Start.Line, rng.Start.Character
			shape.EndLine, shape.EndCharacter = rng.End.Line, rng.End.Character
		} else if symbol.Location != nil {
			shape.StartLine, shape.StartCharacter = symbol.Location.Range.Start.Line, symbol.Location.Range.Start.Character
			shape.EndLine, shape.EndCharacter = symbol.Location.Range.End.Line, symbol.Location.Range.End.Character
		}
		shapes = append(shapes, shape)
		for _, child := range symbol.Children {
			if err := appendSymbol(child, name); err != nil {
				return err
			}
		}
		return nil
	}
	for _, symbol := range symbols {
		encoded, err := json.Marshal(symbol)
		if err != nil {
			return nil, err
		}
		if err := appendSymbol(encoded, ""); err != nil {
			return nil, err
		}
	}
	sort.Slice(shapes, func(i, j int) bool {
		if shapes[i].Name != shapes[j].Name {
			return shapes[i].Name < shapes[j].Name
		}
		if shapes[i].Kind != shapes[j].Kind {
			return shapes[i].Kind < shapes[j].Kind
		}
		if shapes[i].StartLine != shapes[j].StartLine {
			return shapes[i].StartLine < shapes[j].StartLine
		}
		if shapes[i].StartCharacter != shapes[j].StartCharacter {
			return shapes[i].StartCharacter < shapes[j].StartCharacter
		}
		if shapes[i].EndLine != shapes[j].EndLine {
			return shapes[i].EndLine < shapes[j].EndLine
		}
		return shapes[i].EndCharacter < shapes[j].EndCharacter
	})
	if len(shapes) == 0 {
		return nil, fmt.Errorf("%w: document symbols result is empty", errE2ENotVerified)
	}
	return shapes, nil
}

func e2eS18SymbolShapes(raw json.RawMessage, f e2eS18Fixture, expectedSymbol string) ([]e2eSymbolShape, error) {
	all, err := e2eNormalizedSymbolShapes(raw)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{expectedSymbol: true}
	if f.queryFile == f.targetFile {
		wanted[f.queryToken] = true
	}
	filtered := make([]e2eSymbolShape, 0, len(wanted))
	for _, shape := range all {
		if wanted[shape.Name] {
			filtered = append(filtered, shape)
		}
	}
	for name := range wanted {
		found := false
		for _, shape := range filtered {
			if shape.Name == name {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: document symbols omitted required fixture declaration %q", errE2ENotVerified, name)
		}
	}
	return filtered, nil
}

func e2eCompareS18DocumentSymbols(candidate, baseline json.RawMessage, f e2eS18Fixture, expectedSymbol string) error {
	candidateShapes, err := e2eS18SymbolShapes(candidate, f, expectedSymbol)
	if err != nil {
		return err
	}
	baselineShapes, err := e2eS18SymbolShapes(baseline, f, expectedSymbol)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(candidateShapes, baselineShapes) {
		return fmt.Errorf("normalized top-level symbol navigation locations differ from pinned upstream: candidate=%v upstream=%v", candidateShapes, baselineShapes)
	}
	return nil
}

func e2eS18SymbolNames(raw json.RawMessage, f e2eS18Fixture, expectedSymbol string) ([]string, error) {
	shapes, err := e2eS18SymbolShapes(raw, f, expectedSymbol)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(shapes))
	for _, shape := range shapes {
		names = append(names, shape.Name)
	}
	return names, nil
}

func e2eSameS18SymbolNames(raw json.RawMessage, f e2eS18Fixture, expectedSymbol string, expected []string) error {
	got, err := e2eS18SymbolNames(raw, f, expectedSymbol)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, expected) {
		return fmt.Errorf("normalized top-level document symbols changed after the edit: before=%v after=%v", expected, got)
	}
	return nil
}

func e2eSymbolNames(raw json.RawMessage) ([]string, error) {
	shapes, err := e2eNormalizedSymbolShapes(raw)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(shapes))
	for _, shape := range shapes {
		names = append(names, shape.Name)
	}
	return names, nil
}

func e2eSameSymbolNames(raw json.RawMessage, expected []string) error {
	got, err := e2eSymbolNames(raw)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(got, expected) {
		return fmt.Errorf("normalized document symbols changed after comment-only edit: before=%v after=%v", expected, got)
	}
	return nil
}

func e2eSortedLocations(locations []e2eWireLocation) []e2eWireLocation {
	sorted := append([]e2eWireLocation(nil), locations...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		return fmt.Sprintf("%s:%d:%d:%d:%d", a.URI, a.Range.Start.Line, a.Range.Start.Character, a.Range.End.Line, a.Range.End.Character) <
			fmt.Sprintf("%s:%d:%d:%d:%d", b.URI, b.Range.Start.Line, b.Range.Start.Character, b.Range.End.Line, b.Range.End.Character)
	})
	return sorted
}

func decodeE2ELocations(raw json.RawMessage) ([]e2eWireLocation, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("%w: location result is null", errE2ENotVerified)
	}
	var many []e2eWireLocation
	if err := json.Unmarshal(raw, &many); err == nil {
		return e2eCanonicalizeLocationURIs(many), nil
	}
	var one e2eWireLocation
	if err := json.Unmarshal(raw, &one); err == nil && one.URI != "" {
		return e2eCanonicalizeLocationURIs([]e2eWireLocation{one}), nil
	}
	return nil, fmt.Errorf("decode locations: %s", string(raw))
}

func e2eCanonicalizeLocationURIs(locations []e2eWireLocation) []e2eWireLocation {
	canonical := append([]e2eWireLocation(nil), locations...)
	for i := range canonical {
		if parsed, err := uri.Parse(canonical[i].URI); err == nil {
			canonical[i].URI = parsed.Canonical()
		}
	}
	return canonical
}

func e2EContainsLocation(locations []e2eWireLocation, uri string, startLine, startChar, endLine, endChar uint32) bool {
	for _, location := range locations {
		if location.URI == uri && location.Range.Start.Line == startLine && location.Range.Start.Character == startChar &&
			location.Range.End.Line == endLine && location.Range.End.Character == endChar {
			return true
		}
	}
	return false
}

func waitE2EProgress(session *lspdriver.Session, token, kind string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, event := range session.Events() {
			if event.Method != "$/progress" {
				continue
			}
			var params struct {
				Token string `json:"token"`
				Value struct {
					Kind string `json:"kind"`
				} `json:"value"`
			}
			if json.Unmarshal(event.Params, &params) == nil && params.Token == token && params.Value.Kind == kind {
				return true
			}
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func waitE2EResponse(session *lspdriver.Session, id int64, timeout time.Duration) *jsonrpc.Message {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, event := range session.Events() {
			if event.IsResponse() && event.ID != nil && !event.ID.IsStr && event.ID.Num == id {
				return event
			}
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

func e2eResponseEventOrder(events []*jsonrpc.Message, id int64) int {
	if id <= 0 {
		return -1
	}
	for index, event := range events {
		if event.IsResponse() && event.ID != nil && !event.ID.IsStr && event.ID.Num == id {
			return index
		}
	}
	return -1
}

func e2eProgressEventOrder(events []*jsonrpc.Message, token, kind string) int {
	for index, event := range events {
		if event.Method != "$/progress" {
			continue
		}
		var params struct {
			Token string `json:"token"`
			Value struct {
				Kind string `json:"kind"`
			} `json:"value"`
		}
		if json.Unmarshal(event.Params, &params) == nil && params.Token == token && params.Value.Kind == kind {
			return index
		}
	}
	return -1
}

func e2EErrorText(err error) string {
	if err == nil {
		return "completed without caller error"
	}
	return err.Error()
}

func e2eErrorStatus(err error) string {
	if errors.Is(err, errE2ENotVerified) {
		return "not_verified"
	}
	var responseErr *jsonrpc.ResponseError
	if errors.As(err, &responseErr) {
		switch responseErr.Code {
		case jsonrpc.MethodNotFound, jsonrpc.RequestFailed, jsonrpc.RequestCancelled, jsonrpc.ContentModified:
			return "not_verified"
		}
	}
	return "failed"
}

func e2eOverallDecision(statuses ...string) string {
	for _, status := range statuses {
		if status == "failed" || status == "blocked" {
			return status
		}
	}
	for _, status := range statuses {
		if status != "pass" {
			return "not_verified"
		}
	}
	return "pass"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func writeE2EPerformanceReport(report e2ePerformanceReport) (string, error) {
	path := os.Getenv(performanceOutEnv)
	if path == "" {
		path = filepath.Join(os.TempDir(), "omnilsp-performance-"+report.RunID+".json")
	}
	if !filepath.IsAbs(path) {
		root, err := e2eRepoRoot()
		if err != nil {
			return "", err
		}
		path = filepath.Join(root, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if report.ReleaseAssessment == nil {
		report.ReleaseAssessment = e2eReleaseAssessment(report)
	}
	shared := acceptreport.New(report.RunID)
	shared.ReleaseAssessment = report.ReleaseAssessment
	shared.ABBAEvidence = report.S18.ABBAEvidence
	shared.Candidate = acceptreport.Candidate{Binary: report.CandidateBin, SHA256: report.CandidateSHA, Revision: report.GitRevision}
	shared.Corpus = acceptreport.Corpus{Name: "generated multi-language corpus namespaced by acceptance run ID " + report.RunID, SHA256: report.CorpusSHA}
	shared.Environment = report.Environment
	shared.Environment["corpusContentSHA256"] = report.CorpusContentSHA
	shared.Environment["corpusFiles"] = fmt.Sprintf("%d", report.CorpusFiles)
	shared.Environment["toolVersion.omnilsp"] = report.ToolVersions["omnilsp"]
	for name, version := range report.ToolVersions {
		if name != "omnilsp" {
			shared.Environment["toolVersion."+name] = version
		}
	}
	metadataMissing := []string{}
	for _, name := range []string{"filesystem", "osVersion", "cpu", "client"} {
		value := strings.TrimSpace(shared.Environment[name])
		if value == "" || strings.HasPrefix(strings.ToLower(value), "unknown") || value == "unavailable" {
			metadataMissing = append(metadataMissing, name+"="+firstNonEmpty(value, "missing"))
		}
	}
	metadataStatus := acceptreport.Passed
	if len(metadataMissing) > 0 {
		metadataStatus = acceptreport.NotVerified
	}
	shared.Checks = append(shared.Checks, acceptreport.Check{
		ID: report.RunID + "/environment/required-metadata", Status: metadataStatus,
		Summary:  strings.Join(metadataMissing, "; "),
		Observed: map[string]any{"goos": shared.Environment["goos"], "goarch": shared.Environment["goarch"], "os_version": shared.Environment["osVersion"], "filesystem": shared.Environment["filesystem"], "cpu": shared.Environment["cpu"], "client": shared.Environment["client"]},
	})
	shared.Limits["S18"] = report.S18.Thresholds
	shared.Limits["S18ReleasePolicy"] = firstNonEmpty(shared.Environment["releasePolicyID"], e2eS18PolicyID())
	shared.Limits["S18EvidenceQualifiedV2"] = map[string]any{
		"completionAbsoluteCeilingMillis": e2eS18StableCompletionThreshold,
		"syntaxAbsoluteCeilingMillis":     e2eS18StableSyntaxThreshold,
		"abbaOrder":                       []string{"candidate", "upstream", "upstream", "candidate"},
		"rounds":                          e2eS18ABBARounds,
		"samplesPerLeg":                   e2eS18ABBASamplesPerLeg,
		"overheadCeilingMillis":           map[string]int64{"p50": 5, "p95": 10, "p99": 20},
	}
	shared.Limits["S19"] = map[string]any{"process_tree_private_memory_bytes": e2eS19MaxPrivateMemoryBytes}
	shared.Limits["candidateProcess"] = map[string]any{"maxConcurrent": 8, "maxQueue": 64, "GOMAXPROCS": 8}
	s18Names := make([]string, 0, len(report.S18.Operations))
	for name := range report.S18.Operations {
		s18Names = append(s18Names, name)
	}
	sort.Strings(s18Names)
	exceptionCheckIDs := make(map[string]struct{})
	if report.ReleaseAssessment != nil && report.ReleaseAssessment.Decision == acceptreport.ReleasePassedWithPerformanceException {
		for _, id := range report.ReleaseAssessment.ExceptionCheckIDs {
			exceptionCheckIDs[id] = struct{}{}
		}
	}
	latencyOnlyException := len(exceptionCheckIDs) > 0 && len(e2eS18ExceptionSet(report)) > 0
	for _, name := range s18Names {
		samples := report.S18.Operations[name]
		editUpdateEvidence := e2eS18EditUpdateObserved(samples.Method, samples.EditUpdate)
		checkStatus := e2eReportStatus(samples.Status)
		if samples.Method == "textDocument/documentSymbol" && editUpdateEvidence["status"] != "passed" && checkStatus == acceptreport.Passed {
			checkStatus = acceptreport.NotVerified
		}
		semanticEvidence := samples.SemanticEvidence
		if semanticEvidence == nil {
			semanticEvidence = &e2eS18SemanticEvidence{
				Method: samples.Method, CandidateObservationStatus: "not_verified", CandidateValidationStatus: "not_verified",
				UpstreamObservationStatus: "not_verified", UpstreamValidationStatus: "not_verified", DifferentialStatus: "not_verified",
			}
		}
		observed := map[string]any{
			"language": samples.Language, "sample_count": samples.SampleCount,
			"p50_ns": samples.P50NS, "p95_ns": samples.P95NS, "p99_ns": samples.P99NS,
			"failure_type": e2eSampleFailureType(samples), "failure_types": append([]string{}, samples.FailureTypes...),
			"validation": map[string]any{
				"expected_symbol":                semanticEvidence.ExpectedSymbol,
				"candidate_status":               semanticEvidence.CandidateValidationStatus,
				"candidate_normalization_status": semanticEvidence.CandidateObservationStatus,
				"candidate_normalized":           semanticEvidence.CandidateNormalized,
				"upstream_status":                semanticEvidence.UpstreamValidationStatus,
				"upstream_normalization_status":  semanticEvidence.UpstreamObservationStatus,
				"upstream_normalized":            semanticEvidence.UpstreamNormalized,
			},
			"upstream_differential": map[string]any{
				"method": semanticEvidence.Method, "expected_symbol": semanticEvidence.ExpectedSymbol,
				"status": semanticEvidence.DifferentialStatus, "matched": semanticEvidence.DifferentialMatched,
				"full_list_snapshot_status":      semanticEvidence.FullListSnapshotStatus,
				"full_list_sample_status":        semanticEvidence.FullListSampleStatus,
				"target_candidate_sample_status": semanticEvidence.TargetSampleStatus,
			},
			"edit_update":             editUpdateEvidence,
			"presentation_comparison": samples.Presentation,
		}
		if samples.ExceptionThresholds != nil {
			observed["performance_exception_threshold"] = map[string]any{
				"p50_ms": samples.ExceptionThresholds.P50Millis, "p95_ms": samples.ExceptionThresholds.P95Millis, "p99_ms": samples.ExceptionThresholds.P99Millis,
			}
		}
		if samples.PhaseTiming != nil {
			observed["phase_timing"] = map[string]any{
				"sample_count":                      len(samples.PhaseTiming.DidChangeNotifyNS),
				"did_change_notify_sample_id":       name + "/phase/didChangeNotify",
				"document_symbol_request_sample_id": name + "/phase/documentSymbolRequest",
			}
		}
		shared.Checks = append(shared.Checks, acceptreport.Check{
			ID: name, Status: checkStatus,
			Summary:   strings.Join(samples.Errors, "; ") + "; " + samples.Differential,
			Threshold: map[string]any{"p50_ms": samples.Thresholds.P50Millis, "p95_ms": samples.Thresholds.P95Millis, "p99_ms": samples.Thresholds.P99Millis},
			Observed:  observed,
		})
		shared.Samples = append(shared.Samples, acceptreport.SampleSet{
			ID: name, Unit: "ns/op", Raw: e2eFloatSamples(samples.SamplesNS),
			P50: float64(samples.P50NS), P95: float64(samples.P95NS), P99: float64(samples.P99NS),
			Warmup: 32, SampledAt: report.CreatedUTC,
		})
		if samples.PhaseTiming != nil {
			didChangeNotify := samples.PhaseTiming.DidChangeNotifyNS
			documentSymbolRequest := samples.PhaseTiming.DocumentSymbolRequestNS
			shared.Samples = append(shared.Samples,
				acceptreport.SampleSet{
					ID: name + "/phase/didChangeNotify", Unit: "ns/notify", Raw: e2eFloatSamples(didChangeNotify),
					P50: float64(e2ePercentileNS(didChangeNotify, .50)), P95: float64(e2ePercentileNS(didChangeNotify, .95)), P99: float64(e2ePercentileNS(didChangeNotify, .99)),
					SampledAt: report.CreatedUTC,
				},
				acceptreport.SampleSet{
					ID: name + "/phase/documentSymbolRequest", Unit: "ns/request", Raw: e2eFloatSamples(documentSymbolRequest),
					P50: float64(e2ePercentileNS(documentSymbolRequest, .50)), P95: float64(e2ePercentileNS(documentSymbolRequest, .95)), P99: float64(e2ePercentileNS(documentSymbolRequest, .99)),
					SampledAt: report.CreatedUTC,
				},
			)
		}
		if samples.Status == "failed" {
			if latencyOnlyException {
				evidence, hasEvidence := report.S18.ABBAEvidence[name]
				if _, accepted := exceptionCheckIDs[name]; accepted && hasEvidence && e2eS18LatencyExceptionEligible(report.RunID, name, samples, evidence) {
					continue
				}
			}
			shared.Errors = append(shared.Errors, samples.Errors...)
		}
	}
	for _, scale := range report.S19.Scaling {
		summary := fmt.Sprintf("exact returned-location total including declaration; target=%d", scale.ReturnedLocationTotalIncludingDeclaration)
		if scale.Error != "" {
			summary += "; " + scale.Error
		}
		shared.Checks = append(shared.Checks, acceptreport.Check{
			ID: fmt.Sprintf("%s/S19/references@%d", report.RunID, scale.ReturnedLocationTotalIncludingDeclaration), Status: e2eReportStatus(scale.Status), Summary: summary,
			Observed: map[string]any{
				"returned_location_total_including_declaration":           scale.ReturnedLocationTotalIncludingDeclaration,
				"expected_returned_location_count_including_declaration":  scale.ExpectedReturnedLocationCountIncludingDeclaration,
				"observed_returned_location_counts_including_declaration": scale.ObservedReturnedLocationCountsIncludingDeclaration,
				"p50_ns": scale.P50NS, "p95_ns": scale.P95NS, "p99_ns": scale.P99NS,
			},
		})
		shared.Samples = append(shared.Samples, acceptreport.SampleSet{
			ID: fmt.Sprintf("%s/S19/references@%d", report.RunID, scale.ReturnedLocationTotalIncludingDeclaration), Unit: "ns/query", Raw: e2eFloatSamples(scale.SamplesNS),
			P50: float64(scale.P50NS), P95: float64(scale.P95NS), P99: float64(scale.P99NS), SampledAt: report.CreatedUTC,
		})
		if scale.Error != "" && scale.Status == "failed" {
			shared.Errors = append(shared.Errors, scale.Error)
		}
	}
	shared.Checks = append(shared.Checks,
		acceptreport.Check{ID: report.RunID + "/S19/active-cancellation", Status: e2eReportStatus(report.S19.Cancellation.Status), Summary: report.S19.Cancellation.ServerTerminal, Observed: map[string]any{
			"attempted": report.S19.Cancellation.Attempted, "request_id": report.S19.Cancellation.RequestID,
			"progress_token": report.S19.Cancellation.ProgressToken,
			"progress_began": report.S19.Cancellation.ProgressBegan, "caller_outcome": report.S19.Cancellation.CallerOutcome,
			"server_terminal": report.S19.Cancellation.ServerTerminal, "server_error_code": report.S19.Cancellation.ServerErrorCode,
		}},
		acceptreport.Check{ID: report.RunID + "/S19/progress", Status: e2eReportStatus(report.S19.Progress.Status), Observed: map[string]any{"begin_observed": report.S19.Progress.BeginObserved, "end_observed": report.S19.Progress.EndObserved}},
		acceptreport.Check{
			ID: report.RunID + "/S19/no-starvation", Status: e2eReportStatus(report.S19.NoStarvation.Status),
			Summary: fmt.Sprintf("status=%s; reference_terminal=%s; %s", report.S19.NoStarvation.Status, report.S19.NoStarvation.ReferenceTerminal, strings.Join(report.S19.NotVerified, "; ")),
			Observed: map[string]any{
				"reference_was_active":                    report.S19.NoStarvation.ReferenceWasActive,
				"reference_progress_token":                report.S19.NoStarvation.ReferenceProgressToken,
				"reference_progress_began":                report.S19.NoStarvation.ReferenceProgressBegan,
				"reference_begin_event_index":             report.S19.NoStarvation.ReferenceBeginIndex,
				"reference_end_event_index":               report.S19.NoStarvation.ReferenceEndIndex,
				"reference_request_id":                    report.S19.NoStarvation.ReferenceRequestID,
				"completion_request_id":                   report.S19.NoStarvation.CompletionRequestID,
				"hover_request_id":                        report.S19.NoStarvation.HoverRequestID,
				"completion_response_event_index":         report.S19.NoStarvation.CompletionResponseIndex,
				"hover_response_event_index":              report.S19.NoStarvation.HoverResponseIndex,
				"reference_terminal_response_event_index": report.S19.NoStarvation.ReferenceResponseIndex,
				"reference_terminal":                      report.S19.NoStarvation.ReferenceTerminal,
				"reference_terminal_success":              report.S19.NoStarvation.ReferenceTerminalSuccess,
				"reference_terminal_error_code":           report.S19.NoStarvation.ReferenceTerminalErrorCode,
				"reference_terminal_result_count":         report.S19.NoStarvation.ReferenceTerminalResultCount,
				"reference_completed_naturally":           report.S19.NoStarvation.ReferenceCompletedNaturally,
				"reference_caller_outcome":                report.S19.NoStarvation.ReferenceCallerOutcome,
				"reference_result_count":                  report.S19.NoStarvation.ReferenceResultCount,
				"expected_reference_result_count":         report.S19.NoStarvation.ExpectedReferenceResultCount,
				"completion_ns":                           report.S19.NoStarvation.CompletionNS,
				"completion_status":                       report.S19.NoStarvation.CompletionStatus,
				"hover_ns":                                report.S19.NoStarvation.HoverNS,
				"hover_status":                            report.S19.NoStarvation.HoverStatus,
				"budget_ms":                               report.S19.NoStarvation.BudgetMillis,
			},
		},
	)
	resourceStatus := acceptreport.Passed
	resourceSummary := "Go test process-tree memory, handles, and threads were observed at all five S19 stages"
	if len(report.S19.Resources) == 0 {
		resourceStatus = acceptreport.NotVerified
		resourceSummary = "no Go test process-tree resource snapshots were recorded"
	} else if len(report.S19.Resources) != 5 {
		resourceStatus = acceptreport.NotVerified
		resourceSummary = fmt.Sprintf("recorded %d of 5 required Go test process-tree snapshots", len(report.S19.Resources))
	}
	for _, snapshot := range report.S19.Resources {
		if snapshot.Status == "failed" {
			resourceStatus = acceptreport.Failed
			resourceSummary = snapshot.Error
			break
		}
		if snapshot.Status != "observed" && resourceStatus != acceptreport.Failed {
			resourceStatus = acceptreport.NotVerified
			resourceSummary = snapshot.Error
		}
	}
	shared.Checks = append(shared.Checks, acceptreport.Check{
		ID: report.RunID + "/S19/process-tree-resources", Status: resourceStatus, Summary: resourceSummary,
		Observed: map[string]any{"root_kind": "Go test process (os.Getpid) and measurable descendants", "expected_snapshot_count": 5, "snapshots": report.S19.Resources, "private_memory_limit_bytes": e2eS19MaxPrivateMemoryBytes},
	})
	if report.S18.Status == "blocked" || report.S18.Status == "partial" {
		reason := strings.Join(report.S18.NotVerified, "; ")
		if reason == "" {
			reason = strings.Join(report.S18.Errors, "; ")
		}
		if reason == "" {
			reason = "S18 acceptance evidence is incomplete"
		}
		shared.Skips = append(shared.Skips, acceptreport.Skip{ID: report.RunID + "/S18", Reason: reason})
	} else {
		if !latencyOnlyException {
			shared.Errors = append(shared.Errors, report.S18.Errors...)
		}
	}
	if report.S19.Status == "blocked" || report.S19.Status == "partial" {
		shared.Skips = append(shared.Skips, acceptreport.Skip{ID: report.RunID + "/S19", Reason: strings.Join(report.S19.NotVerified, "; ") + "; " + strings.Join(report.S19.Errors, "; ")})
	} else if report.S19.Status == "failed" {
		shared.Errors = append(shared.Errors, report.S19.Errors...)
	}
	shared.Finalize(time.Now())
	if err := acceptreport.Write(abs, shared); err != nil {
		return "", err
	}
	return abs, nil
}

func e2eReportStatus(status string) acceptreport.Status {
	switch status {
	case "pass":
		return acceptreport.Passed
	case "failed", "fail":
		return acceptreport.Failed
	case "running":
		return acceptreport.Running
	default:
		return acceptreport.NotVerified
	}
}

func e2eFloatSamples(samples []int64) []float64 {
	out := make([]float64, len(samples))
	for i, sample := range samples {
		out[i] = float64(sample)
	}
	return out
}

func TestE2EPerformanceReportWriterUsesSharedSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "performance.json")
	t.Setenv(performanceOutEnv, path)
	const runID = "performance-report-contract-test"
	manifest, err := json.Marshal(e2eBuildS18AuditManifest(buildE2ES18Corpus()))
	if err != nil {
		t.Fatalf("encode S18 fixture manifest: %v", err)
	}
	manifestDigest := sha256.Sum256(manifest)
	files := buildE2ES18Corpus()
	fixture := e2eS18Fixtures("", files)[0]
	hoverOperation := e2eS18Operation(fixture, t.TempDir(), files, "hot_hover")
	makeHover := func(value string) json.RawMessage {
		raw, err := json.Marshal(map[string]any{"contents": map[string]string{"kind": "markdown", "value": value}})
		if err != nil {
			t.Fatalf("marshal hover evidence fixture: %v", err)
		}
		return raw
	}
	candidateHover := makeHover("**sym050** has type `int`.\n```go\nfunc sym050(x int) int\n```")
	upstreamHover := makeHover("### `sym050` has type **int**.\n~~~go\nfunc sym050(x int) int\n~~~")
	hoverDiffErr := hoverOperation.compare(candidateHover, upstreamHover)
	hoverEvidence, err := e2eBuildS18SemanticEvidence(fixture, hoverOperation, fixture.queryToken, candidateHover, upstreamHover, hoverDiffErr)
	if err != nil {
		t.Fatalf("build normalized S18 hover evidence: %v", err)
	}
	input := e2ePerformanceReport{
		RunID: runID, CreatedUTC: time.Now().UTC(), CandidateBin: "candidate", CandidateSHA: "sha256:candidate", GitRevision: "revision",
		CorpusSHA: "sha256:corpus", CorpusContentSHA: "sha256:content", CorpusFiles: 1,
		ToolVersions: map[string]string{"omnilsp": "candidate", "go": "go test"},
		Environment: map[string]string{
			"goos": runtime.GOOS, "goarch": runtime.GOARCH, "filesystem": "testfs", "osVersion": "test-os", "cpu": "test-cpu", "client": "test-client",
			"s18FixtureManifest": string(manifest), "s18FixtureManifestSHA256": "sha256:" + hex.EncodeToString(manifestDigest[:]),
			"s18CoverageScope": "representative Tier S performance fixtures; not exhaustive language or tool conformance",
		},
		S18: e2eS18Report{Status: "partial", Thresholds: e2eS18Thresholds(), Operations: map[string]e2eSampleSet{
			runID + "/S18/Go/hot_hover": {
				Language: "Go", Status: "pass", Method: "textDocument/hover", SampleCount: 1, SamplesNS: []int64{9},
				Thresholds: e2eThresholds{20, 75, 150}, Presentation: "display-only Markdown difference observed; normalized semantic content matched", SemanticEvidence: hoverEvidence,
			},
			runID + "/S18/Go/syntax_update_after_edit": {
				Language: "Go", Status: "pass", Method: "textDocument/documentSymbol", SampleCount: 1, SamplesNS: []int64{11},
				Thresholds: e2eThresholds{15, 50, 100},
				SemanticEvidence: &e2eS18SemanticEvidence{
					Method: "textDocument/documentSymbol", ExpectedSymbol: "useRenamed",
					CandidateValidationStatus: "passed", CandidateObservationStatus: "passed", CandidateNormalized: []e2eSymbolShape{{Name: "useRenamed"}},
					UpstreamValidationStatus: "passed", UpstreamObservationStatus: "passed", UpstreamNormalized: []e2eSymbolShape{{Name: "useRenamed"}},
					DifferentialMatched: true, DifferentialStatus: "passed",
				},
				EditUpdate: map[string]any{
					"changed": true, "freshnessVerified": true, "oldSymbol": "use", "newSymbol": "useRenamed",
					"beforeSymbols": []string{"use"}, "afterSymbols": []string{"useRenamed"}, "upstreamAfterSymbols": []string{"useRenamed"},
				},
			},
			runID + "/S18/Python/syntax_update_after_edit": {
				Language: "Python", Status: "pass", Method: "textDocument/documentSymbol", SampleCount: 1, SamplesNS: []int64{12},
				Thresholds:       e2eThresholds{15, 50, 100},
				SemanticEvidence: &e2eS18SemanticEvidence{Method: "textDocument/documentSymbol", ExpectedSymbol: "use_renamed"},
			},
		}},
		S19: e2eS19Report{
			Status: "partial",
			Scaling: []e2eReferenceScale{{
				ReturnedLocationTotalIncludingDeclaration: 200, ExpectedReturnedLocationCountIncludingDeclaration: 200,
				ObservedReturnedLocationCountsIncludingDeclaration: []int{200, 200, 200}, SamplesNS: []int64{100, 110, 120},
				Status: "pass",
			}},
			Cancellation: e2eCancellationOutcome{
				Status: "pass", Attempted: true, RequestID: 5, ProgressToken: "omnilsp-s19-cancel", ProgressBegan: true,
				CallerOutcome: "caller received RequestCancelled", ServerTerminal: "server returned RequestCancelled",
				ServerErrorCode: jsonrpc.RequestCancelled,
			},
			Progress: e2eProgressOutcome{Status: "pass", BeginObserved: true, EndObserved: true},
			NoStarvation: e2eNoStarvationOutcome{
				Status: "pass", ReferenceWasActive: true, ReferenceProgressBegan: true, ReferenceProgressToken: "reference-token",
				ReferenceRequestID: 10, CompletionRequestID: 11, HoverRequestID: 12,
				ReferenceBeginIndex: 20, CompletionResponseIndex: 30, HoverResponseIndex: 40, ReferenceEndIndex: 50, ReferenceResponseIndex: 60,
				ReferenceTerminal: "completed successfully", ReferenceTerminalSuccess: true, ReferenceTerminalResultCount: 3200,
				ReferenceResultCount: 3200, ExpectedReferenceResultCount: 3200, ReferenceCompletedNaturally: true,
				CompletionStatus: "pass", HoverStatus: "pass", BudgetMillis: 2000,
			},
		},
	}
	written, err := writeE2EPerformanceReport(input)
	if err != nil {
		t.Fatalf("write shared performance report: %v", err)
	}
	data, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("read shared performance report: %v", err)
	}
	var document struct {
		SchemaVersion int                      `json:"schema_version"`
		RunID         string                   `json:"run_id"`
		Environment   map[string]string        `json:"environment"`
		Checks        []acceptreport.Check     `json:"checks"`
		Samples       []acceptreport.SampleSet `json:"samples"`
		PrivateS19    json.RawMessage          `json:"s19"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode shared report: %v", err)
	}
	if document.SchemaVersion != 1 || document.RunID != runID || len(document.Checks) == 0 || len(document.Samples) == 0 {
		t.Fatalf("shared report envelope incomplete: schema=%d run_id=%q checks=%d samples=%d", document.SchemaVersion, document.RunID, len(document.Checks), len(document.Samples))
	}
	if document.Environment["s18FixtureManifestSHA256"] != "sha256:"+hex.EncodeToString(manifestDigest[:]) || !strings.Contains(document.Environment["s18CoverageScope"], "not exhaustive") {
		t.Fatalf("shared report lost auditable, representative S18 fixture metadata: %+v", document.Environment)
	}
	if len(document.PrivateS19) != 0 {
		t.Fatalf("writer emitted a second private S19 report contract: %s", document.PrivateS19)
	}
	var locationScaleCheck *acceptreport.Check
	for i := range document.Checks {
		if document.Checks[i].ID == runID+"/S19/references@200" {
			locationScaleCheck = &document.Checks[i]
			break
		}
	}
	if locationScaleCheck == nil || locationScaleCheck.Status != acceptreport.Passed ||
		!strings.Contains(locationScaleCheck.Summary, "exact returned-location total including declaration; target=200") ||
		locationScaleCheck.Observed["returned_location_total_including_declaration"] != float64(200) ||
		locationScaleCheck.Observed["expected_returned_location_count_including_declaration"] != float64(200) ||
		!reflect.DeepEqual(locationScaleCheck.Observed["observed_returned_location_counts_including_declaration"], []any{float64(200), float64(200), float64(200)}) {
		t.Fatalf("shared report lost exact returned-location-scale semantics: %+v", locationScaleCheck)
	}
	var fairness *acceptreport.Check
	for i := range document.Checks {
		if document.Checks[i].ID == runID+"/S19/no-starvation" {
			fairness = &document.Checks[i]
			break
		}
	}
	if fairness == nil {
		t.Fatal("shared report is missing the S19 no-starvation check")
	}
	var hoverCheck *acceptreport.Check
	for i := range document.Checks {
		if document.Checks[i].ID == runID+"/S18/Go/hot_hover" {
			hoverCheck = &document.Checks[i]
			break
		}
	}
	if hoverCheck == nil || hoverCheck.Observed["presentation_comparison"] != "display-only Markdown difference observed; normalized semantic content matched" {
		t.Fatalf("shared report did not record display-only hover comparison separately: %+v", hoverCheck)
	}
	validationEvidence, ok := hoverCheck.Observed["validation"].(map[string]any)
	if !ok || validationEvidence["expected_symbol"] != "sym050" || validationEvidence["candidate_status"] != "passed" ||
		validationEvidence["candidate_normalization_status"] != "passed" || validationEvidence["upstream_status"] != "passed" ||
		validationEvidence["upstream_normalization_status"] != "passed" {
		t.Fatalf("shared report lost structured candidate/upstream validation evidence: %+v", hoverCheck.Observed["validation"])
	}
	candidateNormalized := validationEvidence["candidate_normalized"]
	upstreamNormalized := validationEvidence["upstream_normalized"]
	if candidateNormalized == nil || !reflect.DeepEqual(candidateNormalized, upstreamNormalized) {
		t.Fatalf("shared report did not preserve comparable normalized candidate/upstream observations: candidate=%+v upstream=%+v", candidateNormalized, upstreamNormalized)
	}
	segments, ok := candidateNormalized.([]any)
	if !ok || len(segments) < 2 {
		t.Fatalf("normalized hover evidence should include prose and code segments: %+v", candidateNormalized)
	}
	codeSegment, ok := segments[1].(map[string]any)
	if !ok || codeSegment["kind"] != "code" || codeSegment["language"] != "go" || codeSegment["text"] != "func sym050(x int) int" {
		t.Fatalf("normalized hover evidence lost code/language payload: %+v", segments[1])
	}
	differentialEvidence, ok := hoverCheck.Observed["upstream_differential"].(map[string]any)
	if !ok || differentialEvidence["status"] != "passed" || differentialEvidence["matched"] != true || differentialEvidence["expected_symbol"] != "sym050" {
		t.Fatalf("shared report lost structured upstream differential status: %+v", hoverCheck.Observed["upstream_differential"])
	}
	var editCheck, missingEditCheck *acceptreport.Check
	for i := range document.Checks {
		switch document.Checks[i].ID {
		case runID + "/S18/Go/syntax_update_after_edit":
			editCheck = &document.Checks[i]
		case runID + "/S18/Python/syntax_update_after_edit":
			missingEditCheck = &document.Checks[i]
		}
	}
	if editCheck == nil || editCheck.Status != acceptreport.Passed {
		t.Fatalf("verified edit-update operation did not retain Passed status: %+v", editCheck)
	}
	editEvidence, ok := editCheck.Observed["edit_update"].(map[string]any)
	if !ok || editEvidence["status"] != "passed" || editEvidence["changed"] != true || editEvidence["freshness_verified"] != true ||
		editEvidence["old_symbol"] != "use" || editEvidence["new_symbol"] != "useRenamed" ||
		!reflect.DeepEqual(e2eStringSlice(editEvidence["before_symbols"]), []string{"use"}) ||
		!reflect.DeepEqual(e2eStringSlice(editEvidence["after_symbols"]), []string{"useRenamed"}) ||
		!reflect.DeepEqual(e2eStringSlice(editEvidence["upstream_after_symbols"]), []string{"useRenamed"}) {
		t.Fatalf("shared report lost verified edit freshness/symbol evidence: %+v", editCheck.Observed["edit_update"])
	}
	if missingEditCheck == nil || missingEditCheck.Status != acceptreport.NotVerified {
		t.Fatalf("missing edit-update evidence must not pass validation: %+v", missingEditCheck)
	}
	missingEditEvidence, ok := missingEditCheck.Observed["edit_update"].(map[string]any)
	if !ok || missingEditEvidence["status"] != "not_verified" || missingEditEvidence["freshness_verified"] != false ||
		missingEditEvidence["old_symbol"] != "" || missingEditEvidence["new_symbol"] != "" ||
		len(e2eStringSlice(missingEditEvidence["before_symbols"])) != 0 || len(e2eStringSlice(missingEditEvidence["after_symbols"])) != 0 ||
		len(e2eStringSlice(missingEditEvidence["upstream_after_symbols"])) != 0 {
		t.Fatalf("missing edit evidence must remain not_verified and empty: %+v", missingEditCheck.Observed["edit_update"])
	}
	var cancellationCheck *acceptreport.Check
	for i := range document.Checks {
		if document.Checks[i].ID == runID+"/S19/active-cancellation" {
			cancellationCheck = &document.Checks[i]
			break
		}
	}
	if cancellationCheck == nil {
		t.Fatal("shared report is missing the S19 active-cancellation check")
	}
	if cancellationCheck.Status != acceptreport.Passed || cancellationCheck.Summary != "server returned RequestCancelled" ||
		cancellationCheck.Observed["attempted"] != true || cancellationCheck.Observed["progress_began"] != true ||
		cancellationCheck.Observed["request_id"] != float64(5) || cancellationCheck.Observed["progress_token"] != "omnilsp-s19-cancel" ||
		cancellationCheck.Observed["caller_outcome"] != "caller received RequestCancelled" ||
		cancellationCheck.Observed["server_terminal"] != cancellationCheck.Summary ||
		cancellationCheck.Observed["server_error_code"] != float64(jsonrpc.RequestCancelled) {
		t.Fatalf("active-cancellation evidence lost its correlated request/progress/terminal chain: %+v", cancellationCheck)
	}
	for _, key := range []string{
		"reference_progress_token", "reference_request_id", "completion_request_id", "hover_request_id",
		"reference_begin_event_index", "reference_end_event_index", "completion_response_event_index",
		"hover_response_event_index", "reference_terminal_response_event_index", "reference_terminal_success",
		"reference_terminal_result_count", "reference_completed_naturally",
	} {
		if _, ok := fairness.Observed[key]; !ok {
			t.Errorf("S19 no-starvation evidence is missing Observed.%s", key)
		}
	}
	if len(document.Samples[0].Raw) != 1 {
		t.Errorf("shared report did not preserve raw latency samples: %+v", document.Samples[0])
	}
}

func TestE2EPhaseTimingReportPreservesCAndCppRawSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phase-timing-report.json")
	t.Setenv(performanceOutEnv, path)
	const runID = "phase-timing-report-contract-test"
	phaseTiming := &e2eS18PhaseTiming{
		DidChangeNotifyNS:       []int64{11, 12},
		DocumentSymbolRequestNS: []int64{21, 22},
	}
	report := e2ePerformanceReport{
		RunID:        runID,
		CreatedUTC:   time.Unix(1, 0).UTC(),
		ToolVersions: map[string]string{},
		Environment: map[string]string{
			e2eS18PhaseTimingEnv: "1", "s18PhaseTimingEnabled": "true",
		},
		S18: e2eS18Report{Operations: map[string]e2eSampleSet{
			runID + "/S18/C/syntax_update_after_edit": {
				Language: "C", Status: "pass", Method: "textDocument/documentSymbol", SampleCount: 2,
				SamplesNS: []int64{31, 34}, PhaseTiming: phaseTiming,
			},
			runID + "/S18/C++/syntax_update_after_edit": {
				Language: "C++", Status: "pass", Method: "textDocument/documentSymbol", SampleCount: 2,
				SamplesNS: []int64{33, 35}, PhaseTiming: phaseTiming,
			},
		}},
	}
	written, err := writeE2EPerformanceReport(report)
	if err != nil {
		t.Fatalf("write phase timing report: %v", err)
	}
	data, err := os.ReadFile(written)
	if err != nil {
		t.Fatalf("read phase timing report: %v", err)
	}
	var document struct {
		Environment map[string]string        `json:"environment"`
		Checks      []acceptreport.Check     `json:"checks"`
		Samples     []acceptreport.SampleSet `json:"samples"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode phase timing report: %v", err)
	}
	if document.Environment[e2eS18PhaseTimingEnv] != "1" || document.Environment["s18PhaseTimingEnabled"] != "true" {
		t.Fatalf("phase timing flag/value missing from report environment: %+v", document.Environment)
	}
	samplesByID := make(map[string]acceptreport.SampleSet, len(document.Samples))
	for _, samples := range document.Samples {
		samplesByID[samples.ID] = samples
	}
	for _, language := range []string{"C", "C++"} {
		operationID := runID + "/S18/" + language + "/syntax_update_after_edit"
		for _, phase := range []struct {
			name string
			want []float64
		}{
			{name: "didChangeNotify", want: []float64{11, 12}},
			{name: "documentSymbolRequest", want: []float64{21, 22}},
		} {
			id := operationID + "/phase/" + phase.name
			sample, ok := samplesByID[id]
			if !ok || !reflect.DeepEqual(sample.Raw, phase.want) {
				t.Errorf("raw %s samples missing or changed: got %+v, want %v", id, sample.Raw, phase.want)
			}
		}
		var operationCheck *acceptreport.Check
		for i := range document.Checks {
			if document.Checks[i].ID == operationID {
				operationCheck = &document.Checks[i]
				break
			}
		}
		if operationCheck == nil {
			t.Errorf("report is missing %s", operationID)
			continue
		}
		phaseEvidence, ok := operationCheck.Observed["phase_timing"].(map[string]any)
		if !ok || phaseEvidence["sample_count"] != float64(2) {
			t.Errorf("phase sample references missing for %s: %+v", operationID, operationCheck.Observed["phase_timing"])
		}
	}
}

func TestE2ES18AuditManifestDescribesRepresentativeScope(t *testing.T) {
	manifest := e2eBuildS18AuditManifest(buildE2ES18Corpus())
	if !strings.Contains(manifest.CoverageScope, "representative") || !strings.Contains(manifest.CoverageScope, "not exhaustive") {
		t.Fatalf("S18 manifest must state its representative scope explicitly: %q", manifest.CoverageScope)
	}
	if len(manifest.Fixtures) != 7 || len(manifest.CorpusFiles) == 0 {
		t.Fatalf("S18 manifest does not describe all current language fixtures and corpus files: fixtures=%d files=%d", len(manifest.Fixtures), len(manifest.CorpusFiles))
	}
	seen := map[string]bool{}
	for _, fixture := range manifest.Fixtures {
		if fixture.ID == "" || fixture.Language == "" || fixture.QueryFile == "" || fixture.QueryToken == "" || fixture.UpstreamBinary == "" || len(fixture.ToolNames) == 0 {
			t.Errorf("fixture evidence is missing identity, query, or pinned-upstream metadata: %+v", fixture)
		}
		seen[fixture.ID] = true
	}
	for _, id := range []string{"go", "c", "cpp", "rust", "python", "typescript", "javascript"} {
		if !seen[id] {
			t.Errorf("S18 fixture manifest is missing representative fixture %q", id)
		}
	}
}

func TestE2ES18RustFixtureLocksCargoAndIsolatesOracleBuilds(t *testing.T) {
	files := buildE2ES18Corpus()
	lock, ok := files["Cargo.lock"]
	if !ok || !strings.Contains(lock, "name = \"acceptance_perf\"") || !strings.Contains(lock, "version = 3") {
		t.Fatalf("Rust S18 fixture Cargo.lock is missing or does not lock the local package: %q", lock)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	candidateTarget, upstreamTarget := e2eS18CargoTargetDirs(workspace)
	if candidateTarget == upstreamTarget {
		t.Fatalf("candidate and upstream share Cargo target directory %q", candidateTarget)
	}
	for _, target := range []string{candidateTarget, upstreamTarget} {
		rel, err := filepath.Rel(workspace, target)
		if err != nil || rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			t.Fatalf("Cargo target %q must be outside workspace %q (rel=%q, err=%v)", target, workspace, rel, err)
		}
	}
	fixture := e2eS18Fixture{id: "rust"}
	if envValue(e2eS18CandidateEnv("tools", "", workspace), "CARGO_TARGET_DIR") != candidateTarget {
		t.Fatal("candidate Rust analyzer does not use its isolated Cargo target directory")
	}
	if envValue(e2eS18UpstreamEnv(fixture, "tools", workspace), "CARGO_TARGET_DIR") != upstreamTarget {
		t.Fatal("upstream Rust analyzer does not use its isolated Cargo target directory")
	}
	t.Setenv(e2eS18CompletionPhaseTraceEnv, "")
	if envValue(e2eS18CandidateEnv("tools", "", workspace), e2eS18CompletionPhaseTraceEnv) != "" {
		t.Fatal("completion phase trace must stay disabled by default")
	}
	tracePath := filepath.Join(t.TempDir(), "phase-trace.json")
	t.Setenv(e2eS18CompletionPhaseTraceEnv, tracePath)
	if got := envValue(e2eS18CandidateEnv("tools", "", workspace), e2eS18CompletionPhaseTraceEnv); got != tracePath {
		t.Fatalf("candidate trace path = %q, want %q", got, tracePath)
	}
}

func envValue(env []string, name string) string {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func TestE2ES18CAndCppUpstreamMatchProductionClangdContract(t *testing.T) {
	baseArgs := []string{"--log=error", "--pch-storage=memory"}
	if got := cclsbackend.CommandArgs(""); !reflect.DeepEqual(got, baseArgs) {
		t.Fatalf("production clangd args for an empty audit workspace = %v, want %v", got, baseArgs)
	}
	tests := []struct {
		name    string
		rootDB  bool
		buildDB bool
		wantDir string
	}{
		{name: "no compile database"},
		{name: "workspace compile database", rootDB: true, wantDir: "root"},
		{name: "build compile database", buildDB: true, wantDir: "build"},
		{name: "workspace database takes precedence", rootDB: true, buildDB: true, wantDir: "root"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workspace := t.TempDir()
			writeDB := func(dir string) {
				t.Helper()
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "compile_commands.json"), []byte("[]"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.rootDB {
				writeDB(workspace)
			}
			if test.buildDB {
				writeDB(filepath.Join(workspace, "build"))
			}

			wantArgs := append([]string(nil), baseArgs...)
			if test.wantDir != "" {
				dbDir := workspace
				if test.wantDir == "build" {
					dbDir = filepath.Join(workspace, "build")
				}
				wantArgs = append(wantArgs, "--compile-commands-dir="+dbDir)
			}
			productionArgs := cclsbackend.CommandArgs(workspace)
			if !reflect.DeepEqual(productionArgs, wantArgs) {
				t.Fatalf("production clangd args = %v, want %v", productionArgs, wantArgs)
			}

			fixtures := e2eS18Fixtures(workspace, buildE2ES18Corpus())
			for _, fixture := range fixtures {
				if fixture.id != "c" && fixture.id != "cpp" {
					continue
				}
				if !reflect.DeepEqual(fixture.upstreamArgs, productionArgs) {
					t.Errorf("%s upstream clangd args = %v, want production args %v", fixture.id, fixture.upstreamArgs, productionArgs)
				}
				capabilities, ok := e2eS18UpstreamInitializeCapabilities(fixture)
				if !ok {
					t.Errorf("%s upstream did not select production clangd capabilities", fixture.id)
					continue
				}
				encoded, err := json.Marshal(capabilities)
				if err != nil || string(encoded) != "{}" {
					t.Errorf("%s upstream capabilities = %s, %v; want empty JSON object", fixture.id, encoded, err)
				}
			}
		})
	}
}

func e2eS18TestRawAtPercentiles(p50, p95, p99 int64) []int64 {
	return e2eS18TestRawAtPercentilesN(e2eS18ABBARequiredSamples, p50, p95, p99)
}

func e2eS18TestRawAtPercentilesN(sampleCount int, p50, p95, p99 int64) []int64 {
	raw := make([]int64, sampleCount)
	p50Index := int(math.Ceil(.50*float64(len(raw)))) - 1
	p95Index := int(math.Ceil(.95*float64(len(raw)))) - 1
	p99Index := int(math.Ceil(.99*float64(len(raw)))) - 1
	for index := range raw {
		switch {
		case index < p50Index:
			raw[index] = int64(time.Millisecond)
		case index < p95Index:
			raw[index] = p50 * int64(time.Millisecond)
		case index < p99Index:
			raw[index] = p95 * int64(time.Millisecond)
		default:
			raw[index] = p99 * int64(time.Millisecond)
		}
	}
	return raw
}

func e2eS18TestUpstreamPercentile(value int64) int64 {
	if value <= 1 {
		return 1
	}
	return value - 1
}

func e2eS18ABBAExceptionTestSample(runID, fixtureID, operation string, p50, p95, p99 int64, status string) (e2eSampleSet, acceptreport.ABBAEvidence) {
	method := "textDocument/completion"
	strict := e2eS18Thresholds()["completion_first_usable"]
	if operation == "syntax_update_after_edit" {
		method = "textDocument/documentSymbol"
		strict = e2eS18Thresholds()[operation]
	}
	stable, _ := e2eS18ExceptionThreshold(fixtureID, operation)
	fixture := e2eS18Fixture{id: fixtureID}
	checkID := runID + "/S18/" + fixtureID + "/" + operation
	raw := e2eS18TestRawAtPercentiles(p50, p95, p99)
	sample := e2eSampleSet{
		Language: fixtureID, Status: status, Method: method, SampleCount: len(raw), SamplesNS: raw,
		P50NS: e2ePercentileNS(raw, .50), P95NS: e2ePercentileNS(raw, .95), P99NS: e2ePercentileNS(raw, .99),
		Thresholds: strict, ExceptionThresholds: &stable, Validation: "passed",
		Differential: "candidate target semantics match the paired upstream response",
		SemanticEvidence: &e2eS18SemanticEvidence{
			Method: method, ExpectedSymbol: "targetPerf",
			CandidateObservationStatus: "passed", CandidateValidationStatus: "passed",
			UpstreamObservationStatus: "passed", UpstreamValidationStatus: "passed",
			DifferentialMatched: true, DifferentialStatus: "passed",
		},
	}
	if method == "textDocument/completion" {
		sample.SemanticEvidence.FullListSnapshotStatus = "passed"
		sample.SemanticEvidence.FullListSampleStatus = "passed"
		sample.SemanticEvidence.TargetSampleStatus = "passed"
	} else {
		sample.EditUpdate = map[string]any{
			"changed": true, "freshnessVerified": true, "oldSymbol": "usePerf", "newSymbol": "usePerfRenamed",
			"beforeSymbols": []string{"usePerf"}, "afterSymbols": []string{"usePerfRenamed"}, "upstreamAfterSymbols": []string{"usePerfRenamed"},
		}
	}
	if status == "failed" {
		sample.FailureType, sample.FailureTypes = string(e2eFailureLatency), []string{string(e2eFailureLatency)}
	}
	evidence := e2eNewS18ABBAEvidence(checkID, fixture, operation, method)
	candidateStart := 0
	requestID := int64(1)
	candidateRaw := raw
	upstreamRaw := e2eS18TestRawAtPercentilesN(performanceSamples,
		e2eS18TestUpstreamPercentile(p50), e2eS18TestUpstreamPercentile(p95), e2eS18TestUpstreamPercentile(p99))
	for roundIndex := 0; roundIndex < e2eS18ABBARounds; roundIndex++ {
		round := acceptreport.ABBARound{Index: roundIndex, Order: []string{"candidate", "upstream", "upstream", "candidate"}, Legs: make([]acceptreport.ABBALeg, 0, e2eS18ABBALegsPerRound)}
		for position, role := range round.Order {
			leg := acceptreport.ABBALeg{Position: position, Role: role}
			if role == "candidate" {
				leg.SampleStart = candidateStart
				leg.SampleCount = e2eS18ABBASamplesPerLeg
				leg.RequestIDs = make([]string, e2eS18ABBASamplesPerLeg)
				leg.WriteRawNS = make([]float64, e2eS18ABBASamplesPerLeg)
				leg.WaitRawNS = make([]float64, e2eS18ABBASamplesPerLeg)
				for i := 0; i < leg.SampleCount; i++ {
					outer := candidateRaw[candidateStart+i]
					leg.RequestIDs[i] = strconv.FormatInt(requestID, 10)
					requestID++
					leg.WriteRawNS[i] = float64(outer / 2)
					leg.WaitRawNS[i] = float64(outer - outer/2)
				}
				candidateStart += leg.SampleCount
			} else {
				leg.RawNS = e2eFloatSamples(upstreamRaw)
			}
			round.Legs = append(round.Legs, leg)
		}
		evidence.Rounds = append(evidence.Rounds, round)
	}
	return sample, evidence
}

func e2eCompleteSyntheticPerformanceReport(runID string) e2ePerformanceReport {
	report := e2ePerformanceReport{
		Schema: "omnilsp-performance-acceptance/v1", RunID: runID, CreatedUTC: time.Unix(1, 0).UTC(),
		ToolVersions: map[string]string{}, Environment: map[string]string{
			"filesystem": "NTFS", "osVersion": "Windows", "cpu": "test-cpu", "client": "test-client",
		},
		S18: e2eS18Report{Status: "failed", Thresholds: e2eS18Thresholds(), Operations: map[string]e2eSampleSet{}, ABBAEvidence: map[string]acceptreport.ABBAEvidence{}},
		S19: e2eS19Report{
			Status: "pass", Scaling: []e2eReferenceScale{
				{ReturnedLocationTotalIncludingDeclaration: 200, ExpectedReturnedLocationCountIncludingDeclaration: 200, Status: "pass", SamplesNS: []int64{1, 1, 1}},
				{ReturnedLocationTotalIncludingDeclaration: 800, ExpectedReturnedLocationCountIncludingDeclaration: 800, Status: "pass", SamplesNS: []int64{1, 1, 1}},
				{ReturnedLocationTotalIncludingDeclaration: 3200, ExpectedReturnedLocationCountIncludingDeclaration: 3200, Status: "pass", SamplesNS: []int64{1, 1, 1}},
			},
			Cancellation: e2eCancellationOutcome{Attempted: true, Status: "pass"},
			Progress:     e2eProgressOutcome{Status: "pass", BeginObserved: true, EndObserved: true},
			NoStarvation: e2eNoStarvationOutcome{Status: "pass", CompletionStatus: "pass", HoverStatus: "pass"},
			Resources:    []e2eResourceSnapshot{{Status: "observed"}, {Status: "observed"}, {Status: "observed"}, {Status: "observed"}, {Status: "observed"}},
		},
		Decision: "failed",
	}
	for _, fixtureID := range []string{"go", "c", "cpp", "rust", "python", "typescript", "javascript"} {
		for _, operation := range []string{"hot_hover", "hot_definition", "completion_first_usable", "syntax_update_after_edit"} {
			id := runID + "/S18/" + fixtureID + "/" + operation
			method := map[string]string{
				"hot_hover": "textDocument/hover", "hot_definition": "textDocument/definition",
				"completion_first_usable": "textDocument/completion", "syntax_update_after_edit": "textDocument/documentSymbol",
			}[operation]
			threshold := e2eS18Thresholds()[operation]
			raw := make([]int64, performanceSamples)
			for i := range raw {
				raw[i] = int64(time.Millisecond)
			}
			sample := e2eSampleSet{
				Language: fixtureID, Status: "pass", Method: method, SampleCount: len(raw), SamplesNS: raw,
				P50NS: int64(time.Millisecond), P95NS: int64(time.Millisecond), P99NS: int64(time.Millisecond),
				Thresholds: threshold, Validation: "passed", Differential: "candidate and upstream semantics match",
				SemanticEvidence: &e2eS18SemanticEvidence{
					Method: method, CandidateObservationStatus: "passed", CandidateValidationStatus: "passed",
					UpstreamObservationStatus: "passed", UpstreamValidationStatus: "passed",
					DifferentialMatched: true, DifferentialStatus: "passed",
				},
			}
			if method == "textDocument/completion" && (fixtureID == "c" || fixtureID == "cpp") {
				sample.SemanticEvidence.FullListSnapshotStatus = "passed"
				sample.SemanticEvidence.FullListSampleStatus = "passed"
				sample.SemanticEvidence.TargetSampleStatus = "passed"
			}
			if method == "textDocument/documentSymbol" {
				sample.EditUpdate = map[string]any{
					"changed": true, "freshnessVerified": true, "oldSymbol": "usePerf", "newSymbol": "usePerfRenamed",
					"beforeSymbols": []string{"usePerf"}, "afterSymbols": []string{"usePerfRenamed"}, "upstreamAfterSymbols": []string{"usePerfRenamed"},
				}
			}
			if stable, ok := e2eS18ExceptionThreshold(fixtureID, operation); ok {
				sample.ExceptionThresholds = &stable
				p50, p95, p99 := int64(80), int64(100), int64(125)
				status := "failed"
				if operation == "syntax_update_after_edit" {
					p50, p95, p99 = 60, 100, 150
				}
				sample, report.S18.ABBAEvidence[id] = e2eS18ABBAExceptionTestSample(runID, fixtureID, operation, p50, p95, p99, status)
			}
			report.S18.Operations[id] = sample
		}
	}
	report.S18.FailureTypes = []string{string(e2eFailureLatency)}
	return report
}

func TestE2ES18LatencyExceptionBoundaries(t *testing.T) {
	t.Setenv(e2eS18PolicyEnv, "stable")
	const runID = "boundary-run"
	for _, test := range []struct {
		name          string
		p50, p95, p99 int64
		want          bool
	}{
		{name: "exact ceilings", p50: 60, p95: 100, p99: 150, want: true},
		{name: "p50 just over", p50: 61, p95: 100, p99: 150},
		{name: "p95 just over", p50: 60, p95: 101, p99: 150},
		{name: "p99 just over", p50: 60, p95: 100, p99: 151},
	} {
		t.Run(test.name, func(t *testing.T) {
			sample, evidence := e2eS18ABBAExceptionTestSample(runID, "c", "syntax_update_after_edit", test.p50, test.p95, test.p99, "failed")
			got := e2eS18LatencyExceptionEligible(runID, runID+"/S18/c/syntax_update_after_edit", sample, evidence)
			if got != test.want {
				t.Fatalf("eligible=%t, want %t for p50/p95/p99=%d/%d/%d", got, test.want, test.p50, test.p95, test.p99)
			}
		})
	}
}

func TestE2ES18ABBAEvidenceRejectsTamperingAndPartialCorrelations(t *testing.T) {
	t.Setenv(e2eS18PolicyEnv, "stable")
	const runID = "abba-tamper-run"
	id := runID + "/S18/c/syntax_update_after_edit"
	tests := []struct {
		name   string
		mutate func(*e2eSampleSet, *acceptreport.ABBAEvidence)
	}{
		{name: "leg order", mutate: func(_ *e2eSampleSet, evidence *acceptreport.ABBAEvidence) { evidence.Rounds[0].Order[0] = "upstream" }},
		{name: "candidate sample range", mutate: func(_ *e2eSampleSet, evidence *acceptreport.ABBAEvidence) { evidence.Rounds[0].Legs[0].SampleStart++ }},
		{name: "duplicate request ID", mutate: func(_ *e2eSampleSet, evidence *acceptreport.ABBAEvidence) {
			evidence.Rounds[0].Legs[0].RequestIDs[1] = evidence.Rounds[0].Legs[0].RequestIDs[0]
		}},
		{name: "missing correlated phase", mutate: func(_ *e2eSampleSet, evidence *acceptreport.ABBAEvidence) {
			evidence.Rounds[0].Legs[0].WriteRawNS = nil
		}},
		{name: "negative same request overhead", mutate: func(sample *e2eSampleSet, evidence *acceptreport.ABBAEvidence) {
			leg := &evidence.Rounds[0].Legs[0]
			leg.WriteRawNS[0] = float64(sample.SamplesNS[leg.SampleStart])
			leg.WaitRawNS[0] = 1
		}},
		{name: "merged overhead p50 above budget", mutate: func(sample *e2eSampleSet, evidence *acceptreport.ABBAEvidence) {
			for roundIndex := range evidence.Rounds {
				for legIndex := range evidence.Rounds[roundIndex].Legs {
					leg := &evidence.Rounds[roundIndex].Legs[legIndex]
					if leg.Role != "candidate" {
						continue
					}
					for i := range leg.WriteRawNS {
						outer := sample.SamplesNS[leg.SampleStart+i]
						gap := int64(6 * time.Millisecond)
						if outer < gap {
							gap = outer
						}
						leg.WriteRawNS[i], leg.WaitRawNS[i] = float64(outer-gap), 0
					}
				}
			}
		}},
		{name: "stale edit", mutate: func(sample *e2eSampleSet, _ *acceptreport.ABBAEvidence) {
			sample.EditUpdate["freshnessVerified"] = false
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sample, evidence := e2eS18ABBAExceptionTestSample(runID, "c", "syntax_update_after_edit", 60, 100, 150, "failed")
			test.mutate(&sample, &evidence)
			if e2eS18ABBAEvidenceComplete(id, sample, evidence) {
				t.Fatal("tampered or partial ABBA evidence was accepted")
			}
		})
	}
}

func TestE2ES18ABBATraceCorrelationKeepsPartialMatchedPhases(t *testing.T) {
	workspace := t.TempDir()
	fixture := e2eS18Fixture{id: "c", queryFile: "c-perf.c"}
	tracePath := filepath.Join(t.TempDir(), "nested-rpc-timing.json")
	trace := e2eNestedTimingReport{
		Version: 1, Language: "cpp", SampleLimit: 16384,
		Samples: []e2eNestedTimingSample{{
			Method: "textDocument/completion", URI: uri.FromPath(filepath.Join(workspace, fixture.queryFile)).String(),
			ParentRequestID: json.RawMessage(`"1"`), DurationNS: 900, WriteDurationNS: 300, WaitDurationNS: 500, Outcome: "response",
		}},
	}
	data, err := json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	evidence := acceptreport.ABBAEvidence{
		CheckID: "partial-run/S18/c/completion_first_usable", FixtureID: "c",
		Operation: "completion_first_usable", Method: "textDocument/completion",
		Rounds: []acceptreport.ABBARound{{Index: 0, Order: []string{"candidate", "upstream", "upstream", "candidate"}, Legs: []acceptreport.ABBALeg{{
			Position: 0, Role: "candidate", SampleStart: 0, SampleCount: 2, RequestIDs: []string{"1", "2"},
		}}}},
	}
	sampleSet := e2eSampleSet{SamplesNS: []int64{1000, 2000}}
	err = e2eCompleteS18ABBATiming(&evidence, sampleSet, tracePath, fixture, workspace)
	if err == nil || !strings.Contains(err.Error(), "omitted candidate parent request ID 2") {
		t.Fatalf("missing trace request did not fail closed: %v", err)
	}
	leg := evidence.Rounds[0].Legs[0]
	if !reflect.DeepEqual(leg.WriteRawNS, []float64{300}) || !reflect.DeepEqual(leg.WaitRawNS, []float64{500}) {
		t.Fatalf("matched nested phases were not retained without placeholder values: write=%v wait=%v", leg.WriteRawNS, leg.WaitRawNS)
	}

	trace.Samples[0].URI = uri.FromPath(filepath.Join(workspace, "cpp-perf.cpp")).String()
	data, err = json.Marshal(trace)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tracePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	wrongFixtureEvidence := acceptreport.ABBAEvidence{
		CheckID: "partial-run/S18/c/completion_first_usable", FixtureID: "c",
		Operation: "completion_first_usable", Method: "textDocument/completion",
		Rounds: []acceptreport.ABBARound{{Index: 0, Order: []string{"candidate", "upstream", "upstream", "candidate"}, Legs: []acceptreport.ABBALeg{{
			Position: 0, Role: "candidate", SampleStart: 0, SampleCount: 1, RequestIDs: []string{"1"},
		}}}},
	}
	wrongFixtureSample := e2eSampleSet{SamplesNS: []int64{1000}}
	err = e2eCompleteS18ABBATiming(&wrongFixtureEvidence, wrongFixtureSample, tracePath, fixture, workspace)
	if err == nil || !strings.Contains(err.Error(), "method/URI/outcome mismatch") {
		t.Fatalf("C++ URI was accepted for the C fixture: %v", err)
	}
	if len(wrongFixtureEvidence.Rounds[0].Legs[0].WriteRawNS) != 0 || len(wrongFixtureEvidence.Rounds[0].Legs[0].WaitRawNS) != 0 {
		t.Fatal("phases from a different fixture URI were retained")
	}
}

func TestE2ES18StrictPassAndNonOverThresholdQuantilesIgnoreExceptionCaps(t *testing.T) {
	t.Setenv(e2eS18PolicyEnv, "stable")
	const runID = "strict-pass-ceiling-run"
	completionID := runID + "/S18/c/completion_first_usable"
	sample, evidence := e2eS18ABBAExceptionTestSample(runID, "c", "completion_first_usable", 40, 110, 124, "pass")
	report := e2eCompleteSyntheticPerformanceReport(runID)
	report.S18.Operations[completionID] = sample
	report.S18.ABBAEvidence[completionID] = evidence
	if !e2eS18ExecutionComplete(report) {
		t.Fatal("strict-passing completion sample above a non-over threshold's exception cap made execution incomplete")
	}
	if e2eS18LatencyExceptionEligible(runID, completionID, sample, evidence) {
		t.Fatal("a strict-passing row was accepted as a performance exception")
	}

	failed, failedEvidence := e2eS18ABBAExceptionTestSample(runID, "c", "completion_first_usable", 80, 110, 124, "failed")
	if !e2eS18LatencyExceptionEligible(runID, completionID, failed, failedEvidence) {
		t.Fatal("non-strict-over quantiles were incorrectly subjected to exception ceilings")
	}
}

func TestE2ES18LatencyExceptionRejectsWrongIdentityAndSemanticFailure(t *testing.T) {
	t.Setenv(e2eS18PolicyEnv, "stable")
	const runID = "identity-run"
	sample, evidence := e2eS18ABBAExceptionTestSample(runID, "c", "syntax_update_after_edit", 60, 100, 150, "failed")
	for _, id := range []string{
		runID + "/S18/c/hot_hover",
		runID + "/S18/go/syntax_update_after_edit",
		"other-run/S18/c/syntax_update_after_edit",
	} {
		if e2eS18LatencyExceptionEligible(runID, id, sample, evidence) {
			t.Errorf("wrong operation/language/run was accepted: %s", id)
		}
	}
	semanticFailure := sample
	semanticFailure.SemanticEvidence = &e2eS18SemanticEvidence{
		CandidateObservationStatus: "failed", CandidateValidationStatus: "passed",
		UpstreamObservationStatus: "passed", UpstreamValidationStatus: "passed",
		DifferentialMatched: false, DifferentialStatus: "failed",
	}
	if e2eS18LatencyExceptionEligible(runID, runID+"/S18/c/syntax_update_after_edit", semanticFailure, evidence) {
		t.Fatal("semantic failure was accepted as a latency exception")
	}
}

func TestE2ES18LatencyExceptionRejectsIncompleteOrFailedExecution(t *testing.T) {
	t.Setenv(e2eS18PolicyEnv, "stable")
	const runID = "execution-run"
	complete := e2eCompleteSyntheticPerformanceReport(runID)
	incomplete := complete
	incomplete.S19.Status = "partial"
	assessment := e2eReleaseAssessment(incomplete)
	if assessment.ExecutionStatus != "incomplete" || assessment.Decision != acceptreport.ReleaseNotVerified || len(assessment.ExceptionCheckIDs) != 0 {
		t.Fatalf("incomplete execution was accepted: %+v", assessment)
	}
	failed := complete
	failedSample := complete.S18.Operations[runID+"/S18/go/hot_hover"]
	failedSample.Status = "failed"
	failedSample.FailureType = string(e2eFailureSemantic)
	failedSample.FailureTypes = []string{string(e2eFailureSemantic)}
	failed.S18.Operations[runID+"/S18/go/hot_hover"] = failedSample
	assessment = e2eReleaseAssessment(failed)
	if assessment.ExecutionStatus != "completed" || assessment.Decision != acceptreport.ReleaseFailed || len(assessment.ExceptionCheckIDs) != 0 {
		t.Fatalf("non-latency failure was accepted: %+v", assessment)
	}
}

func TestE2ES18BoundedCandidatesRemainVisibleAlongsideOtherStrictFailure(t *testing.T) {
	t.Setenv(e2eS18PolicyEnv, "stable")
	const runID = "bounded-with-other-failure-run"
	report := e2eCompleteSyntheticPerformanceReport(runID)
	completionID := runID + "/S18/c/completion_first_usable"
	completionFailure, completionEvidence := e2eS18ABBAExceptionTestSample(runID, "c", "completion_first_usable", 80, 100, 125, "failed")
	report.S18.Operations[completionID] = completionFailure
	report.S18.ABBAEvidence[completionID] = completionEvidence
	otherFailureID := runID + "/S18/go/hot_hover"
	otherFailure := report.S18.Operations[otherFailureID]
	otherFailure.Status = "failed"
	otherFailure.FailureType = string(e2eFailureSemantic)
	otherFailure.FailureTypes = []string{string(e2eFailureSemantic)}
	report.S18.Operations[otherFailureID] = otherFailure
	report.S18.FailureTypes = []string{string(e2eFailureLatency), string(e2eFailureSemantic)}

	assessment := e2eReleaseAssessment(report)
	if assessment.ExecutionStatus != "completed" || assessment.Decision != acceptreport.ReleaseFailed {
		t.Fatalf("other strict failure did not remain release-blocking: %+v", assessment)
	}
	wantBounded := e2eS18ExceptionCheckIDs(runID)
	if !reflect.DeepEqual(assessment.BoundedExceptionCheckIDs, wantBounded) {
		t.Fatalf("bounded candidate IDs = %v, want %v", assessment.BoundedExceptionCheckIDs, wantBounded)
	}
	if len(assessment.ExceptionCheckIDs) != 0 {
		t.Fatalf("overall failed assessment must not claim accepted exception IDs: %v", assessment.ExceptionCheckIDs)
	}
}

func TestE2EPerformanceExceptionReportKeepsStrictFailureWithoutErrorsOrSkips(t *testing.T) {
	t.Setenv(e2eS18PolicyEnv, "stable")
	report := e2eCompleteSyntheticPerformanceReport("writer-exception-run")
	report.ReleaseAssessment = e2eReleaseAssessment(report)
	if report.ReleaseAssessment.Decision != acceptreport.ReleasePassedWithPerformanceException {
		t.Fatalf("synthetic report did not qualify for exception: %+v", report.ReleaseAssessment)
	}
	if !reflect.DeepEqual(report.ReleaseAssessment.BoundedExceptionCheckIDs, report.ReleaseAssessment.ExceptionCheckIDs) {
		t.Fatalf("accepted exception must retain the bounded candidate IDs: bounded=%v exception=%v", report.ReleaseAssessment.BoundedExceptionCheckIDs, report.ReleaseAssessment.ExceptionCheckIDs)
	}
	path := filepath.Join(t.TempDir(), "performance.json")
	t.Setenv(performanceOutEnv, path)
	if _, err := writeE2EPerformanceReport(report); err != nil {
		t.Fatalf("write exception performance report: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got acceptreport.Report
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Decision != acceptreport.Failed || got.ReleaseAssessment == nil || got.ReleaseAssessment.Decision != acceptreport.ReleasePassedWithPerformanceException {
		t.Fatalf("strict and release decisions were conflated: decision=%q assessment=%+v", got.Decision, got.ReleaseAssessment)
	}
	if len(got.Errors) != 0 || len(got.Skips) != 0 {
		t.Fatalf("accepted latency-only exception must not carry report errors/skips: errors=%v skips=%v", got.Errors, got.Skips)
	}
	if len(got.ABBAEvidence) != 4 || got.ABBAEvidence["writer-exception-run/S18/c/syntax_update_after_edit"].CheckID == "" {
		t.Fatalf("ABBA raw evidence was not retained in the shared report: %#v", got.ABBAEvidence)
	}
	var exceptionCheck *acceptreport.Check
	for i := range got.Checks {
		if got.Checks[i].ID == "writer-exception-run/S18/c/syntax_update_after_edit" {
			exceptionCheck = &got.Checks[i]
			break
		}
	}
	if exceptionCheck == nil || exceptionCheck.Status != acceptreport.Failed || exceptionCheck.Observed["failure_type"] != string(e2eFailureLatency) {
		t.Fatalf("strict failed latency row or structured classification was lost: %+v", exceptionCheck)
	}
}

func TestE2ES18FilterSelectsOnlyRequestedOperations(t *testing.T) {
	if got := e2eParseS18Filter(""); got != nil && len(got) != 0 {
		t.Fatalf("empty filter = %v, want full-matrix selection", got)
	}
	filter := e2eParseS18Filter(" C/syntax_update_after_edit, cpp ")
	if !e2eS18FilterSelects(filter, "c", "syntax_update_after_edit") ||
		e2eS18FilterSelects(filter, "c", "hot_hover") ||
		!e2eS18FilterSelects(filter, "cpp", "hot_definition") ||
		e2eS18FilterSelects(filter, "rust", "syntax_update_after_edit") {
		t.Fatalf("diagnostic S18 filter selected the wrong cases: %v", filter)
	}
}

func TestE2ES19CorpusScalesByExactReturnedLocationTotal(t *testing.T) {
	content, tokenPositions, err := buildE2ES19Corpus()
	if err != nil {
		t.Fatalf("build S19 corpus: %v", err)
	}
	lines := strings.Split(content, "\n")
	for _, returnedLocationTotal := range e2eS19ReturnedLocationScales {
		symbol := fmt.Sprintf("target%d", returnedLocationTotal)
		wantUses, err := e2eS19ReferenceUseCount(returnedLocationTotal, true)
		if err != nil {
			t.Fatalf("reference use count for %d returned locations: %v", returnedLocationTotal, err)
		}
		if got := strings.Count(content, "var "+symbol+" int\n"); got != 1 {
			t.Errorf("%s declaration count = %d, want 1", symbol, got)
		}
		if got := strings.Count(content, "_ = "+symbol+"\n"); got != wantUses {
			t.Errorf("%s reference-use count = %d, want %d for %d returned locations including declaration", symbol, got, wantUses, returnedLocationTotal)
		}
		if got := strings.Count(content, symbol); got != returnedLocationTotal {
			t.Errorf("%s total identifier occurrences = %d, want exactly %d returned locations", symbol, got, returnedLocationTotal)
		}
		position, ok := tokenPositions[symbol]
		if !ok || int(position[0]) >= len(lines) || int(position[1]) > len(lines[position[0]]) || !strings.HasPrefix(lines[position[0]][position[1]:], symbol) {
			t.Errorf("%s query position does not identify its declaration: position=%v", symbol, position)
		}
	}
	for _, invalidTotal := range []int{0, -1} {
		if _, err := e2eS19ReferenceUseCount(invalidTotal, true); err == nil {
			t.Errorf("includeDeclaration accepted invalid returned-location total %d", invalidTotal)
		}
	}

	scale := e2eReferenceScale{
		ReturnedLocationTotalIncludingDeclaration: 200, ExpectedReturnedLocationCountIncludingDeclaration: 200,
		ObservedReturnedLocationCountsIncludingDeclaration: []int{200, 200, 200}, Status: "pass",
	}
	encoded, err := json.Marshal(scale)
	if err != nil {
		t.Fatalf("encode S19 scale report: %v", err)
	}
	var reportFields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &reportFields); err != nil {
		t.Fatalf("decode S19 scale report: %v", err)
	}
	for _, field := range []string{"returnedLocationTotalIncludingDeclaration", "expectedReturnedLocationCountIncludingDeclaration", "observedReturnedLocationCountsIncludingDeclaration"} {
		if _, ok := reportFields[field]; !ok {
			t.Errorf("S19 scale report is missing unambiguous field %q: %s", field, encoded)
		}
	}
	for _, ambiguousField := range []string{"requestedReferences", "expectedResultCount", "resultCounts", "returnedLocationScale", "expectedReturnedLocationCount", "observedReturnedLocationCounts"} {
		if _, ok := reportFields[ambiguousField]; ok {
			t.Errorf("S19 scale report retained ambiguous legacy field %q", ambiguousField)
		}
	}
}

func TestE2EResourceSnapshotRootsAtGoTestProcess(t *testing.T) {
	snapshot := e2eProcessTreeResources(os.Getpid(), "unit-root-check")
	t.Logf("Go test process-tree sampler: root_pid=%d processes=%d status=%s metric=%s error=%s", snapshot.RootPID, snapshot.ProcessCount, snapshot.Status, snapshot.PrivateMetric, snapshot.Error)
	if snapshot.RootPID != os.Getpid() || snapshot.RootKind != "Go test process (os.Getpid) and measurable descendants" {
		t.Fatalf("resource snapshot did not identify the Go test process root: %+v", snapshot)
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && snapshot.Status != "not_verified" {
		t.Fatalf("unsupported host %s must leave process-tree measurement not_verified, got %+v", runtime.GOOS, snapshot)
	}
}
