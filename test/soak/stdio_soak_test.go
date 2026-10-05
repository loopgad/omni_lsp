//go:build soak

package soak

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/test/acceptance/lspdriver"
	"github.com/omnilsp/omni/test/acceptance/report"
)

const (
	soakWorkers                = 8
	stdioGeneratedFiles        = 4096
	soakMemoryCap              = uint64(8 << 30)
	soakPollPeriod             = 5 * time.Second
	soakSeed                   = int64(0)
	resourceTrendWindows       = 10
	resourcePrivateGrowthLimit = uint64(512 << 20)
	resourceHandleGrowthLimit  = uint64(1024)
)

type stdioQuery struct {
	method string
	params map[string]any
}

type stdioWorkerResult struct {
	worker  int
	method  string
	latency time.Duration
	err     error
}

type latencySummary struct {
	Count       uint64    `json:"count"`
	SampleCount int       `json:"sample_count"`
	Truncated   bool      `json:"truncated"`
	RawMS       []float64 `json:"raw_ms"`
	P50MS       float64   `json:"p50_ms"`
	P95MS       float64   `json:"p95_ms"`
	P99MS       float64   `json:"p99_ms"`
}

type latencyBucket struct {
	count  uint64
	values []float64
}

type resourceWindow struct {
	Minute                           int64   `json:"minute"`
	SampleCount                      int     `json:"sample_count"`
	FirstPrivate                     uint64  `json:"first_private_bytes"`
	LastPrivate                      uint64  `json:"last_private_bytes"`
	MedianPrivate                    uint64  `json:"median_private_bytes"`
	MinPrivate                       uint64  `json:"min_private_bytes"`
	MaxPrivate                       uint64  `json:"max_private_bytes"`
	FirstHandles                     uint64  `json:"first_handles"`
	LastHandles                      uint64  `json:"last_handles"`
	MedianHandles                    uint64  `json:"median_handles"`
	MinHandles                       uint64  `json:"min_handles"`
	MaxHandles                       uint64  `json:"max_handles"`
	TrendWindowCount                 int     `json:"trend_window_count,omitempty"`
	TrendFirstPrivate                uint64  `json:"trend_first_private_bytes,omitempty"`
	TrendLastPrivate                 uint64  `json:"trend_last_private_bytes,omitempty"`
	TrendPrivateDelta                int64   `json:"trend_private_delta_bytes,omitempty"`
	TrendFirstHandles                uint64  `json:"trend_first_handles,omitempty"`
	TrendLastHandles                 uint64  `json:"trend_last_handles,omitempty"`
	TrendHandlesDelta                int64   `json:"trend_handles_delta"`
	TrendPrivateSlopeBytesPerMinute  float64 `json:"trend_private_slope_bytes_per_minute"`
	TrendHandlesSlopePerMinute       float64 `json:"trend_handles_slope_per_minute"`
	TrendPrivateEstimatedGrowthBytes float64 `json:"trend_private_estimated_growth_bytes"`
	TrendHandlesEstimatedGrowth      float64 `json:"trend_handles_estimated_growth"`
	PrivateTrendPositive             bool    `json:"private_trend_positive"`
	HandlesTrendPositive             bool    `json:"handles_trend_positive"`
}

type latencyCollector struct {
	mu      sync.Mutex
	buckets map[int64]map[string]*latencyBucket
}

type stdioTelemetry struct {
	Sequence             int64                     `json:"sequence"`
	Kind                 string                    `json:"kind"`
	UTC                  time.Time                 `json:"utc"`
	ActiveElapsed        time.Duration             `json:"active_elapsed"`
	Requests             uint64                    `json:"completed_requests"`
	Edits                uint64                    `json:"applied_edits"`
	WorkersAlive         []bool                    `json:"workers_alive"`
	Tree                 processTreeSample         `json:"process_tree"`
	Server               map[string]any            `json:"server_status"`
	Index                map[string]any            `json:"index_stats"`
	Backends             map[string]bool           `json:"backends"`
	ProgressEvents       int                       `json:"progress_events"`
	ProgressKinds        map[string]uint64         `json:"progress_kinds"`
	BackendRestarts      uint64                    `json:"backend_restarts"`
	SnapshotEpochs       int                       `json:"snapshot_epochs"`
	IndexGeneration      uint64                    `json:"index_generation"`
	WorkerHash           string                    `json:"worker_hash"`
	CandidateHash        string                    `json:"candidate_sha256"`
	Seed                 int64                     `json:"seed"`
	GoGoroutines         int                       `json:"go_goroutines"`
	GoHeapAlloc          uint64                    `json:"go_heap_alloc_bytes"`
	LatencyByMethod      map[string]latencySummary `json:"latency_by_method"`
	CanceledRequests     uint64                    `json:"canceled_reindex_requests"`
	CancellationOutcomes map[string]uint64         `json:"cancellation_outcomes"`
	CanceledQueries      uint64                    `json:"canceled_query_requests"`
	QueryCancelOutcomes  map[string]uint64         `json:"query_cancellation_outcomes"`
	Resource             *resourceWindow           `json:"resource_window,omitempty"`
}

type progressLifecycleCounts struct {
	Begin  uint64 `json:"begin"`
	Report uint64 `json:"report"`
	End    uint64 `json:"end"`
}

type controlledBackendRestartResult struct {
	oldPID uint32
	newPID uint32
	err    error
}

func TestSoak_RealStdioMixedWorkload(t *testing.T) {
	// §S16 contract (docs/soak-nightly.md): the real stdio soak is the
	// release-candidate evidence gate and runs only when the acceptance
	// runner explicitly opts in via OMNILSP_SOAK_GATE=required, which
	// scripts/acceptance.ps1 sets. Every other invocation — the nightly
	// form-only regression job, full-package sweeps, plain `go test` — skips
	// instead of failing on preconditions a form-only environment cannot
	// satisfy. The strict gates below (run ID, SOAK_DURATION, windows/amd64
	// Job Object accounting, frozen candidate, pinned tools) keep their full
	// force under opt-in.
	if os.Getenv("OMNILSP_SOAK_GATE") != "required" {
		t.Skip("real stdio soak gate not opted in: set OMNILSP_SOAK_GATE=required (scripts/acceptance.ps1 does this); nightly/CI invocations are form-only regression signals, not release-candidate evidence — see docs/soak-nightly.md")
	}
	runID := os.Getenv("OMNILSP_RUN_ID")
	if runID == "" {
		runID = fmt.Sprintf("missing-run-id-%d", os.Getpid())
	}
	evidence := report.New(runID)
	evidence.Environment["goos"] = runtime.GOOS
	evidence.Environment["goarch"] = runtime.GOARCH
	evidence.Environment["measurement_source"] = "JobObjectBasicProcessIdList + PSAPI PROCESS_MEMORY_COUNTERS_EX.PrivateUsage summed over every Job Object member; GetProcessHandleCount per member; JobMemoryLimit hard commit ceiling and PeakJobMemoryUsed high-water mark"
	evidence.Environment["worker_count"] = fmt.Sprint(soakWorkers)
	evidence.Environment["workload_seed"] = fmt.Sprint(soakSeed)
	evidence.Environment["workload_schedule"] = "Go math/rand.NewSource(0) deterministically shuffles the eight query workers and selects each edit interval from 48..80 completed queries and each paired reindex/reference-query cancellation interval from 192..320; controlled rust-analyzer restart at one third, rebuild index every minute, restart candidate halfway"
	evidence.Limits["worker_count"] = soakWorkers
	evidence.Limits["gomaxprocs"] = soakWorkers
	evidence.Limits["scheduler_max_concurrent"] = soakWorkers
	evidence.Limits["process_tree_private_bytes"] = soakMemoryCap
	evidence.Limits["job_commit_bytes"] = soakMemoryCap
	evidence.Corpus.Name = "real-stdio-mixed-workload-v1"
	root := stdioSoakRepoRoot(t)
	reportPath, jsonlPath := stdioSoakPaths(root, runID)
	if value := os.Getenv("OMNILSP_ACCEPTANCE_REPORT"); value != "" {
		reportPath = value
	}
	if value := os.Getenv("SOAK_JSONL"); value != "" {
		jsonlPath = value
	}
	defer func() {
		evidence.Finalize(time.Now())
		if err := report.Write(reportPath, evidence); err != nil {
			t.Errorf("write soak report %q: %v", reportPath, err)
		}
		t.Logf("soak report: %s (decision: %s); telemetry: %s", reportPath, evidence.Decision, jsonlPath)
	}()
	if os.Getenv("OMNILSP_RUN_ID") == "" {
		message := "OMNILSP_RUN_ID must be supplied by the acceptance runner"
		soakNotVerified(&evidence, "soak/run-id", message)
		t.Fatal(message)
	}

	durationText := os.Getenv("SOAK_DURATION")
	if durationText == "" {
		soakNotVerified(&evidence, "soak/gate", "SOAK_DURATION must be explicitly set for the real stdio soak gate")
		t.Fatal("real stdio soak is not verified; set SOAK_DURATION (for example 1h)")
	}
	duration, err := parseStdioSoakDuration(durationText)
	if err != nil {
		message := err.Error()
		soakFailed(&evidence, "soak/duration", message)
		t.Fatal(message)
	}
	evidence.Limits["duration"] = duration.String()
	evidence.Limits["edit_interval_completed_queries"] = map[string]any{"min": 48, "max": 80, "selection": "seeded PRNG"}
	evidence.Limits["cancel_interval_completed_queries"] = map[string]any{"min": 192, "max": 320, "selection": "seeded PRNG"}
	evidence.Limits["query_cancel_method"] = "textDocument/references with includeDeclaration=false and a unique workDoneToken"
	evidence.Limits["reindex_interval"] = "1m"
	evidence.Limits["workload_seed"] = soakSeed
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		message := fmt.Sprintf("Windows amd64 Job Object accounting is required; got %s/%s", runtime.GOOS, runtime.GOARCH)
		soakNotVerified(&evidence, "soak/host", message)
		t.Fatal(message)
	}
	previousGOMAXPROCS := runtime.GOMAXPROCS(soakWorkers)
	defer runtime.GOMAXPROCS(previousGOMAXPROCS)
	evidence.Environment["gomaxprocs"] = fmt.Sprint(runtime.GOMAXPROCS(0))

	if strings.TrimSpace(os.Getenv("OMNILSP_BIN")) == "" {
		message := "OMNILSP_BIN must name the frozen candidate binary for the real stdio soak"
		soakNotVerified(&evidence, "soak/candidate", message)
		t.Fatal(message)
	}
	if err := verifyStdioTools(root, &evidence); err != nil {
		soakNotVerified(&evidence, "soak/tools", err.Error())
		t.Fatalf("pinned real backend tools are not verified: %v", err)
	}
	rustAnalyzerPath, err := exec.LookPath("rust-analyzer")
	if err != nil {
		soakNotVerified(&evidence, "soak/backend-restart", "pinned rust-analyzer path is unavailable to the runner")
		t.Fatalf("controlled backend restart unavailable: %v", err)
	}
	rustAnalyzerPath, err = filepath.Abs(rustAnalyzerPath)
	if err != nil {
		soakFailed(&evidence, "soak/backend-restart", err.Error())
		t.Fatal(err)
	}
	serverBin, err := frozenStdioCandidate()
	if err != nil {
		soakFailed(&evidence, "soak/candidate", err.Error())
		t.Fatalf("frozen candidate unavailable: %v", err)
	}
	candidateHash, err := sha256File(serverBin)
	if err != nil {
		soakFailed(&evidence, "soak/candidate-hash", err.Error())
		t.Fatal(err)
	}
	evidence.Candidate.Binary, evidence.Candidate.SHA256 = serverBin, candidateHash
	if rev, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output(); err == nil {
		evidence.Candidate.Revision = strings.TrimSpace(string(rev))
	}
	workspace := t.TempDir()
	corpusHash, docURI, text, cancelQueryURI, cancelQueryText, err := prepareStdioCorpus(workspace)
	if err != nil {
		soakFailed(&evidence, "soak/corpus", err.Error())
		t.Fatal(err)
	}
	// Keep one editor-only declaration absent from the disk corpus. The
	// persistent index must remain fresh for unchanged disk inventory while
	// document queries continue to observe this dirty overlay across restart.
	text += "\nfunc soakOverlayOnly() int { return 7 }\n"
	evidence.Corpus.SHA256 = corpusHash
	workerHash, err := hashSoakWorkers(root)
	if err != nil {
		soakFailed(&evidence, "soak/worker-hash", err.Error())
		t.Fatal(err)
	}
	evidence.Environment["worker_hash"] = workerHash

	logFile, err := openStdioJSONL(jsonlPath)
	if err != nil {
		soakFailed(&evidence, "soak/jsonl", err.Error())
		t.Fatal(err)
	}
	defer logFile.Close()
	writer := bufio.NewWriterSize(logFile, 32*1024)
	var sequence int64
	var writtenSequence int64
	var activeStart time.Time
	var lastWrittenAt time.Time
	var lastWrittenActive time.Duration
	defer func() {
		if err := writer.Flush(); err != nil {
			t.Errorf("flush telemetry: %v", err)
		}
	}()
	writeRow := func(row stdioTelemetry) {
		if row.Sequence != writtenSequence+1 {
			message := fmt.Sprintf("telemetry sequence discontinuity: got %d after %d", row.Sequence, writtenSequence)
			soakNotVerified(&evidence, "soak/telemetry-sequence", message)
			t.Fatal(message)
		}
		if row.Kind == "minute" {
			referenceTime, referenceActive := lastWrittenAt, lastWrittenActive
			if referenceTime.IsZero() {
				referenceTime = activeStart.UTC()
			}
			wallGap := row.UTC.Sub(referenceTime)
			activeGap := row.ActiveElapsed - referenceActive
			if wallGap <= 0 || wallGap > time.Minute+2*soakPollPeriod || activeGap < time.Minute || activeGap > time.Minute+2*soakPollPeriod {
				message := fmt.Sprintf("minute JSONL window discontinuity: wall=%s active=%s", wallGap, activeGap)
				soakNotVerified(&evidence, "soak/telemetry-continuity", message)
				t.Fatal(message)
			}
		}
		if !lastWrittenAt.IsZero() && (row.UTC.Before(lastWrittenAt) || row.ActiveElapsed < lastWrittenActive) {
			message := "telemetry UTC or active elapsed time moved backwards"
			soakNotVerified(&evidence, "soak/telemetry-time", message)
			t.Fatal(message)
		}
		if err := json.NewEncoder(writer).Encode(row); err != nil {
			soakFailed(&evidence, "soak/jsonl-write", err.Error())
			t.Fatalf("write telemetry: %v", err)
		}
		if err := writer.Flush(); err != nil {
			soakFailed(&evidence, "soak/jsonl-flush", err.Error())
			t.Fatalf("flush telemetry: %v", err)
		}
		if err := logFile.Sync(); err != nil {
			soakFailed(&evidence, "soak/jsonl-sync", err.Error())
			t.Fatalf("sync telemetry: %v", err)
		}
		writtenSequence = row.Sequence
		lastWrittenAt, lastWrittenActive = row.UTC, row.ActiveElapsed
	}

	job, err := newProcessTreeBudget(os.Getpid(), soakMemoryCap)
	if err != nil {
		soakNotVerified(&evidence, "soak/process-tree-limit", err.Error())
		t.Fatalf("8 GiB process-tree memory accounting is not verified: %v", err)
	}
	if _, err := job.sample(os.Getpid(), 0); err != nil {
		soakNotVerified(&evidence, "soak/process-tree-accounting", err.Error())
		t.Fatalf("Job Object accounting unavailable: %v", err)
	}
	evidence.Checks = append(evidence.Checks, report.Check{ID: "soak/process-tree-limit", Status: report.Passed, Summary: "the test runner is in a Job Object with an 8 GiB commit ceiling and member PrivateUsage is enumerated per process"})

	env := stdioServerEnv(root, workspace, t.TempDir())
	var candidatePID atomic.Int64
	startSession := func() *lspdriver.Session {
		return lspdriver.StartWithProcessHook(t, serverBin, workspace, env, func(pid int) error {
			if err := job.verifyMember(pid); err != nil {
				return fmt.Errorf("candidate process did not inherit Job Object: %w", err)
			}
			candidatePID.Store(int64(pid))
			return nil
		})
	}
	session := startSession()
	defer func() { session.Close(t) }()
	session.Initialize(t)
	session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": docURI, "languageId": "go", "version": 1, "text": text,
	}})
	if err := waitForStdioOpen(session, docURI); err != nil {
		soakFailed(&evidence, "soak/initialize-open", err.Error())
		t.Fatal(err)
	}
	beforeCancelQueryOpen, err := readStdioStatus(session)
	if err != nil {
		soakFailed(&evidence, "soak/cancel-query-open", err.Error())
		t.Fatal(err)
	}
	session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": cancelQueryURI, "languageId": "go", "version": 1, "text": cancelQueryText,
	}})
	if err := waitForStdioSnapshot(session, beforeCancelQueryOpen, 30*time.Second); err != nil {
		soakFailed(&evidence, "soak/cancel-query-open", err.Error())
		t.Fatal(err)
	}
	rustURI := uri.FromPath(filepath.Join(workspace, "src", "lib.rs")).String()
	rustText := rustSoakSource()
	beforeRustOpen, err := readStdioStatus(session)
	if err != nil {
		soakFailed(&evidence, "soak/rust-open", err.Error())
		t.Fatal(err)
	}
	session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": rustURI, "languageId": "rust", "version": 1, "text": rustText,
	}})
	if err := waitForStdioSnapshot(session, beforeRustOpen, 30*time.Second); err != nil {
		soakFailed(&evidence, "soak/rust-open", err.Error())
		t.Fatal(err)
	}
	if err := verifyStdioBackends(session); err != nil {
		soakFailed(&evidence, "soak/backends", err.Error())
		t.Fatal(err)
	}
	if err := verifyStdioIndex(session); err != nil {
		soakFailed(&evidence, "soak/index-init", err.Error())
		t.Fatal(err)
	}
	if err := waitForRustSoakHover(session, rustURI, rustText, 90*time.Second); err != nil {
		soakFailed(&evidence, "soak/rust-semantic", err.Error())
		t.Fatal(err)
	}
	queries := makeStdioQueries(docURI, text)
	if len(queries) != soakWorkers {
		soakFailed(&evidence, "soak/workers", fmt.Sprintf("query list has %d items, want %d", len(queries), soakWorkers))
		t.Fatal("invalid worker count")
	}
	rng := rand.New(rand.NewSource(soakSeed))
	rng.Shuffle(len(queries), func(i, j int) { queries[i], queries[j] = queries[j], queries[i] })
	if err := assertStdioQuerySet(session, queries); err != nil {
		soakFailed(&evidence, "soak/initial-queries", err.Error())
		t.Fatal(err)
	}
	progressCounts := countProgressLifecycle(session.Events(), "soak-references")
	workerQueries := withoutProgressTokens(queries)
	if err := cancelStdioReindex(session, time.Time{}, nil); err != nil {
		soakFailed(&evidence, "soak/cancel", err.Error())
		t.Fatal(err)
	}
	if _, err := rebuildStdioIndex(session); err != nil {
		soakFailed(&evidence, "soak/reindex", err.Error())
		t.Fatal(err)
	}
	runtime.GC()
	var baselineMem runtime.MemStats
	runtime.ReadMemStats(&baselineMem)
	baselineGoroutines := runtime.NumGoroutine()
	evidence.Environment["baseline_goroutines"] = fmt.Sprint(baselineGoroutines)
	evidence.Environment["baseline_heap_alloc_bytes"] = fmt.Sprint(baselineMem.HeapAlloc)
	evidence.Checks = append(evidence.Checks,
		report.Check{ID: "soak/initialize-open", Status: report.Passed, Summary: "real stdio initialize/initialized and didOpen advanced the server snapshot"},
		report.Check{ID: "soak/cancel", Status: report.Passed, Summary: "canceled reindex received a terminal JSON-RPC RequestCancelled response by ID"},
		report.Check{ID: "soak/reindex", Status: report.Passed, Summary: "reindex published a fresh persistent generation, then indexStats confirmed it"},
		report.Check{ID: "soak/semantic-workload", Status: report.Passed, Summary: "all eight concurrent query classes returned terminal results; hover, definition, references, symbols, status and index had semantic assertions"},
	)

	var completed atomic.Uint64
	var edits atomic.Uint64
	var peakInFlight atomic.Int64
	var canceledReindexes uint64 = 1 // The checked startup cancellation above is included in the run totals.
	var canceledQueries uint64
	cancellationOutcomes := map[string]uint64{"request_cancelled": 1}
	queryCancellationOutcomes := map[string]uint64{}
	continuityGuard, err := newSuspendResumeGuard()
	if err != nil {
		soakNotVerified(&evidence, "soak/continuity-monitor", err.Error())
		t.Fatalf("uninterrupted-run monitoring is unavailable: %v", err)
	}
	evidence.Environment["continuity_signal"] = "Windows PowerRegisterSuspendResumeNotification callback; every suspend/resume event invalidates the continuous run"
	continuityRecorded := false
	continuityCloseAttempted := false
	recordContinuityFailure := func(message string) {
		if continuityRecorded {
			return
		}
		soakNotVerified(&evidence, "soak/continuity", message)
		continuityRecorded = true
	}
	defer func() {
		if !continuityCloseAttempted {
			continuityCloseAttempted = true
			closeErr, checkErr := finalizeSuspendResumeGuard(continuityGuard)
			if closeErr != nil {
				soakNotVerified(&evidence, "soak/continuity-monitor-close", closeErr.Error())
				t.Errorf("close suspend/resume monitor: %v", closeErr)
			}
			if checkErr != nil {
				recordContinuityFailure(checkErr.Error())
			}
		}
		if !continuityRecorded {
			recordContinuityFailure("soak ended before the suspend/resume monitor verified the full requested duration")
		}
	}()
	if err := continuityGuard.Check(); err != nil {
		recordContinuityFailure(err.Error())
		t.Fatal(err)
	}
	startAt := time.Now()
	activeStart = startAt
	latencies := &latencyCollector{buckets: make(map[int64]map[string]*latencyBucket)}
	if err := cancelStdioReindex(session, startAt, latencies); err != nil {
		soakFailed(&evidence, "soak/cancel-repeated", err.Error())
		t.Fatalf("second startup reindex cancellation: %v", err)
	}
	canceledReindexes++
	cancellationOutcomes["request_cancelled"]++
	lastCommittedIndexGeneration, err := rebuildStdioIndex(session)
	if err != nil {
		soakFailed(&evidence, "soak/reindex", err.Error())
		t.Fatal(err)
	}
	for ordinal := uint64(1); ordinal <= 2; ordinal++ {
		if err := cancelStdioReferenceQuery(session, cancelQueryURI, cancelQueryText, ordinal, startAt, latencies); err != nil {
			soakFailed(&evidence, "soak/cancel-repeated", err.Error())
			t.Fatalf("startup canceled references query %d: %v", ordinal, err)
		}
		canceledQueries++
		queryCancellationOutcomes["request_cancelled"]++
	}
	workerCtx, stopWorkers := context.WithCancel(context.Background())
	results, workerDone, alive := startStdioWorkers(workerCtx, session, workerQueries, &completed, latencies, startAt)
	active := time.Duration(0)
	lastMono := startAt
	lastWall := lastMono.UTC()
	lastTelemetry := time.Duration(0)
	lastIndexGeneration := uint64(0)
	lastSnapshotEpochs := 0
	nextEdit := uint64(48 + rng.Intn(33))
	nextCancellation := uint64(192 + rng.Intn(129))
	peakPrivate := uint64(0)
	privateSamples, commitPeakSamples := []float64{}, []float64{}
	handleSamples := []float64{}
	minutePrivate, minuteHandles := []uint64{}, []uint64{}
	resourceWindows := make([]resourceWindow, 0)
	heapSamples, goroutineSamples := []float64{}, []float64{}
	restarted := false
	backendRestarts := uint64(0)
	backendRestarted := false
	backendRestartStarted := false
	backendRestartDone := make(chan controlledBackendRestartResult, 1)
	var backendRestartPending <-chan controlledBackendRestartResult
	lastRequestProgressAt := startAt
	lastRequestProgress := completed.Load()
	var ticker = time.NewTicker(soakPollPeriod)
	defer ticker.Stop()
	recordBackendRestart := func(result controlledBackendRestartResult) error {
		if result.err != nil {
			return result.err
		}
		backendRestarts++
		backendRestarted = true
		sequence++
		now := time.Now()
		writeRow(stdioTelemetry{Sequence: sequence, Kind: "backend-restart", UTC: now.UTC(), ActiveElapsed: active, Requests: completed.Load(), Edits: edits.Load(), Seed: soakSeed, BackendRestarts: backendRestarts, CandidateHash: candidateHash, WorkerHash: workerHash, Server: map[string]any{"backend": "rust/rust-analyzer", "old_pid": result.oldPID, "new_pid": result.newPID}})
		evidence.Checks = append(evidence.Checks, report.Check{ID: "soak/backend-restart", Status: report.Passed, Summary: "terminated the supervised rust-analyzer leaf under active queries; the replacement returned semantic hover and kept one process identity through a stability window", Observed: map[string]any{"backend": "rust/rust-analyzer", "old_pid": result.oldPID, "new_pid": result.newPID, "candidate_pid": candidatePID.Load()}})
		return nil
	}
	for active < duration {
		select {
		case result := <-results:
			if result.err != nil {
				stopWorkers()
				soakFailed(&evidence, "soak/query/"+result.method, result.err.Error())
				t.Fatalf("worker %d %s: %v", result.worker, result.method, result.err)
			}
			if completed.Load() >= nextCancellation {
				nextCancellation = completed.Load() + uint64(192+rng.Intn(129))
				if err := cancelStdioReindex(session, startAt, latencies); err != nil {
					stopWorkers()
					soakFailed(&evidence, "soak/cancel-repeated", err.Error())
					t.Fatal(err)
				}
				canceledReindexes++
				cancellationOutcomes["request_cancelled"]++
				if err := cancelStdioReferenceQuery(session, cancelQueryURI, cancelQueryText, canceledQueries+1, startAt, latencies); err != nil {
					stopWorkers()
					soakFailed(&evidence, "soak/cancel-repeated", err.Error())
					t.Fatalf("cancel references query: %v", err)
				}
				canceledQueries++
				queryCancellationOutcomes["request_cancelled"]++
			}
			if completed.Load() >= nextEdit {
				nextEdit = completed.Load() + uint64(48+rng.Intn(33))
				text = fmt.Sprintf("package soak\n\nfunc soakTarget(value int) int { return value + 1 }\nfunc useTarget() int { return soakTarget(1) }\nfunc soakOverlayOnly() int { return 7 }\n// edit %d\n", edits.Load()+1)
				if err := editStdioDocument(t, session, docURI, text, int(edits.Add(1))+1); err != nil {
					stopWorkers()
					soakFailed(&evidence, "soak/edit", err.Error())
					t.Fatal(err)
				}
			}
			lastRequestProgressAt = time.Now()
			lastRequestProgress = completed.Load()
		case restartedBackend := <-backendRestartPending:
			backendRestartPending = nil
			if err := recordBackendRestart(restartedBackend); err != nil {
				stopWorkers()
				soakFailed(&evidence, "soak/backend-restart", err.Error())
				t.Fatalf("controlled rust-analyzer restart: %v", err)
			}
		case <-ticker.C:
			if err := continuityGuard.Check(); err != nil {
				stopWorkers()
				recordContinuityFailure(err.Error())
				t.Fatal(err)
			}
			now := time.Now()
			wall := now.UTC()
			monoDelta, wallDelta := now.Sub(lastMono), wall.Sub(lastWall)
			if wallDelta < 0 || wallDelta > 2*soakPollPeriod || wallDelta-monoDelta > 20*time.Second {
				stopWorkers()
				message := fmt.Sprintf("sleep/interruption invalidated the soak: wall sample gap=%s monotonic gap=%s", wallDelta, monoDelta)
				recordContinuityFailure(message)
				t.Fatal(message)
			}
			active += monoDelta
			lastMono, lastWall = now, wall
			if completed.Load() > lastRequestProgress {
				lastRequestProgress = completed.Load()
				lastRequestProgressAt = now
			}
			if active >= 2*time.Minute && now.Sub(lastRequestProgressAt) > 2*time.Minute {
				stopWorkers()
				message := fmt.Sprintf("query progress stalled for %s with %d completed requests", now.Sub(lastRequestProgressAt), lastRequestProgress)
				soakFailed(&evidence, "soak/query-progress", message)
				t.Fatal(message)
			}
			tree, err := job.sample(os.Getpid(), int(candidatePID.Load()))
			if err != nil {
				stopWorkers()
				soakNotVerified(&evidence, "soak/process-tree-accounting", err.Error())
				t.Fatalf("process-tree accounting failed: %v", err)
			}
			if tree.PrivateBytes > soakMemoryCap || tree.PeakJobCommitBytes > soakMemoryCap {
				stopWorkers()
				message := fmt.Sprintf("8 GiB memory cap exceeded: sampled PrivateUsage=%d PeakJobMemoryUsed=%d", tree.PrivateBytes, tree.PeakJobCommitBytes)
				soakFailed(&evidence, "soak/process-tree-memory", message)
				t.Fatal(message)
			}
			if err := verifyStdioWorkersAlive(alive); err != nil {
				stopWorkers()
				soakFailed(&evidence, "soak/workers", err.Error())
				t.Fatal(err)
			}
			statusSample, statusErr := readStdioStatus(session)
			if statusErr != nil {
				stopWorkers()
				soakFailed(&evidence, "soak/scheduler-limit", statusErr.Error())
				t.Fatalf("sample scheduler concurrency: %v", statusErr)
			}
			if _, ok := statusSample["InFlight"]; !ok {
				stopWorkers()
				message := "omnilsp/status did not expose InFlight for the scheduler limit check"
				soakNotVerified(&evidence, "soak/scheduler-limit", message)
				t.Fatal(message)
			}
			inFlight := int64(number(statusSample["InFlight"]))
			if inFlight > peakInFlight.Load() {
				peakInFlight.Store(inFlight)
			}
			if inFlight > soakWorkers {
				stopWorkers()
				message := fmt.Sprintf("scheduler in-flight count %d exceeds configured maximum %d", inFlight, soakWorkers)
				soakFailed(&evidence, "soak/scheduler-limit", message)
				t.Fatal(message)
			}
			if tree.PrivateBytes > peakPrivate {
				peakPrivate = tree.PrivateBytes
			}
			privateSamples = append(privateSamples, float64(tree.PrivateBytes))
			commitPeakSamples = append(commitPeakSamples, float64(tree.PeakJobCommitBytes))
			totalHandles := processTreeHandleTotal(tree)
			handleSamples = append(handleSamples, float64(totalHandles))
			minutePrivate = append(minutePrivate, tree.PrivateBytes)
			minuteHandles = append(minuteHandles, totalHandles)
			var currentMem runtime.MemStats
			runtime.ReadMemStats(&currentMem)
			heapSamples = append(heapSamples, float64(currentMem.HeapAlloc))
			goroutineSamples = append(goroutineSamples, float64(runtime.NumGoroutine()))
			if active-lastTelemetry >= time.Minute {
				if active-lastTelemetry > time.Minute+2*soakPollPeriod {
					stopWorkers()
					message := fmt.Sprintf("missed minute telemetry interval: previous=%s active=%s", lastTelemetry, active)
					soakNotVerified(&evidence, "soak/telemetry-continuity", message)
					t.Fatal(message)
				}
				row, err := collectStdioTelemetry(session, job, int(candidatePID.Load()), alive, completed.Load(), edits.Load(), active, wall, candidateHash, workerHash)
				if err != nil {
					stopWorkers()
					soakFailed(&evidence, "soak/telemetry", err.Error())
					t.Fatalf("telemetry: %v", err)
				}
				sequence++
				row.Sequence, row.Kind = sequence, "minute"
				if row.IndexGeneration == 0 || row.SnapshotEpochs == 0 {
					stopWorkers()
					message := fmt.Sprintf("minute server progress did not report an index generation and snapshot epoch: index=%d snapshot=%d", row.IndexGeneration, row.SnapshotEpochs)
					soakFailed(&evidence, "soak/server-progress", message)
					t.Fatal(message)
				}
				if lastIndexGeneration != 0 && row.IndexGeneration <= lastIndexGeneration {
					stopWorkers()
					message := fmt.Sprintf("persistent index generation did not advance between telemetry rows: %d -> %d", lastIndexGeneration, row.IndexGeneration)
					soakFailed(&evidence, "soak/server-progress", message)
					t.Fatal(message)
				}
				if lastSnapshotEpochs != 0 && row.SnapshotEpochs < lastSnapshotEpochs {
					stopWorkers()
					message := fmt.Sprintf("document snapshot epoch regressed between telemetry rows: %d -> %d", lastSnapshotEpochs, row.SnapshotEpochs)
					soakFailed(&evidence, "soak/server-progress", message)
					t.Fatal(message)
				}
				lastIndexGeneration, lastSnapshotEpochs = row.IndexGeneration, row.SnapshotEpochs
				row.Seed = soakSeed
				row.BackendRestarts = backendRestarts
				row.LatencyByMethod = latencies.take(int64(active/time.Minute) - 1)
				row.CanceledRequests = canceledReindexes
				row.CancellationOutcomes = cloneCounts(cancellationOutcomes)
				row.CanceledQueries = canceledQueries
				row.QueryCancelOutcomes = cloneCounts(queryCancellationOutcomes)
				window := summarizeResourceWindow(int64(active/time.Minute)-1, minutePrivate, minuteHandles)
				if len(minutePrivate) == 0 || len(minuteHandles) == 0 {
					stopWorkers()
					message := "minute resource telemetry has no process-tree samples"
					soakNotVerified(&evidence, "soak/resource-trend", message)
					t.Fatal(message)
				}
				if len(resourceWindows) >= resourceTrendWindows-1 {
					trendWindows := append(append([]resourceWindow(nil), resourceWindows[len(resourceWindows)-(resourceTrendWindows-1):]...), window)
					trend := resourceTrend(trendWindows)
					window.TrendWindowCount = len(trendWindows)
					window.TrendFirstPrivate, window.TrendLastPrivate = trend.firstPrivate, trend.lastPrivate
					window.TrendPrivateDelta = signedDelta(trend.lastPrivate, trend.firstPrivate)
					window.TrendFirstHandles, window.TrendLastHandles = trend.firstHandles, trend.lastHandles
					window.TrendHandlesDelta = signedDelta(trend.lastHandles, trend.firstHandles)
					window.TrendPrivateSlopeBytesPerMinute = trend.privateSlopeBytesPerMinute
					window.TrendHandlesSlopePerMinute = trend.handlesSlopePerMinute
					window.TrendPrivateEstimatedGrowthBytes = trend.privateEstimatedGrowth
					window.TrendHandlesEstimatedGrowth = trend.handlesEstimatedGrowth
					window.PrivateTrendPositive, window.HandlesTrendPositive = trend.privateSlopeBytesPerMinute > 0, trend.handlesSlopePerMinute > 0
					if resourceTrendExceeded(trend) {
						stopWorkers()
						message := fmt.Sprintf("sustained resource growth over %d one-minute windows: private=%d -> %d bytes (Theil-Sen slope=%.0f bytes/min, estimated growth=%.0f bytes), handles=%d -> %d (Theil-Sen slope=%.1f handles/min, estimated growth=%.1f)", len(trendWindows), trend.firstPrivate, trend.lastPrivate, trend.privateSlopeBytesPerMinute, trend.privateEstimatedGrowth, trend.firstHandles, trend.lastHandles, trend.handlesSlopePerMinute, trend.handlesEstimatedGrowth)
						soakFailed(&evidence, "soak/resource-trend", message)
						t.Fatal(message)
					}
				}
				resourceWindows = append(resourceWindows, window)
				row.Resource = &window
				writeRow(row)
				lastTelemetry = active
				minutePrivate, minuteHandles = minutePrivate[:0], minuteHandles[:0]
				generation, rebuildErr := rebuildStdioIndex(session)
				if rebuildErr != nil {
					stopWorkers()
					soakFailed(&evidence, "soak/reindex-periodic", rebuildErr.Error())
					t.Fatal(rebuildErr)
				}
				lastCommittedIndexGeneration = generation
			}
			if !backendRestartStarted && active >= duration/3 {
				backendRestartStarted = true
				backendRestartPending = backendRestartDone
				go func() {
					oldPID, newPID, restartErr := restartControlledRustBackend(session, job, os.Getpid(), int(candidatePID.Load()), rustAnalyzerPath, rustURI, rustText)
					backendRestartDone <- controlledBackendRestartResult{oldPID: oldPID, newPID: newPID, err: restartErr}
				}()
			}
			if !restarted && backendRestarted && active >= duration/2 {
				stopWorkers()
				waitStdioWorkers(workerDone)
				persistedGeneration, indexErr := freshPersistentStdioIndexGeneration(session)
				if indexErr != nil {
					soakFailed(&evidence, "soak/restart-index", indexErr.Error())
					t.Fatal(indexErr)
				}
				if persistedGeneration != lastCommittedIndexGeneration {
					message := fmt.Sprintf("pre-restart persistent generation disagrees with last successful reindex: stats=%d committed=%d", persistedGeneration, lastCommittedIndexGeneration)
					soakFailed(&evidence, "soak/restart-index", message)
					t.Fatal(message)
				}
				session.Notify(t, "textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": docURI}})
				session.Notify(t, "textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": cancelQueryURI}})
				session.Close(t)
				tree, err = job.sample(os.Getpid(), 0)
				if err != nil || tree.ActiveProcesses != 1 || tree.HandleCounts["candidate"] != 0 || tree.HandleCounts["backend"] != 0 {
					message := fmt.Sprintf("backend processes/handles survived session shutdown: sample=%+v err=%v", tree, err)
					soakFailed(&evidence, "soak/backend-close", message)
					t.Fatal(message)
				}
				session = startSession()
				session.Initialize(t)
				before, err := readStdioStatus(session)
				if err != nil {
					soakFailed(&evidence, "soak/restart", err.Error())
					t.Fatal(err)
				}
				session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
					"uri": docURI, "languageId": "go", "version": 1, "text": text,
				}})
				if err := waitForStdioSnapshot(session, before, 10*time.Second); err != nil {
					soakFailed(&evidence, "soak/restart-open", err.Error())
					t.Fatal(err)
				}
				beforeCancelQueryReopen, err := readStdioStatus(session)
				if err != nil {
					soakFailed(&evidence, "soak/restart-cancel-query-open", err.Error())
					t.Fatal(err)
				}
				session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
					"uri": cancelQueryURI, "languageId": "go", "version": 1, "text": cancelQueryText,
				}})
				if err := waitForStdioSnapshot(session, beforeCancelQueryReopen, 30*time.Second); err != nil {
					soakFailed(&evidence, "soak/restart-cancel-query-open", err.Error())
					t.Fatal(err)
				}
				reopenedStatus, err := readStdioStatus(session)
				if err != nil || number(reopenedStatus["SnapshotEpochs"]) <= number(before["SnapshotEpochs"]) {
					message := fmt.Sprintf("reopened document snapshot did not advance: before=%v after=%v err=%v", before["SnapshotEpochs"], reopenedStatus["SnapshotEpochs"], err)
					soakFailed(&evidence, "soak/restart-open", message)
					t.Fatal(message)
				}
				if err := verifyStdioBackends(session); err != nil {
					soakFailed(&evidence, "soak/restart-backends", err.Error())
					t.Fatal(err)
				}
				if err := verifyPersistentStdioIndex(session, persistedGeneration, docURI, text); err != nil {
					soakFailed(&evidence, "soak/restart-index", err.Error())
					t.Fatal(err)
				}
				if err := assertStdioOverlaySymbol(session, docURI); err != nil {
					soakFailed(&evidence, "soak/restart-overlay", err.Error())
					t.Fatal(err)
				}
				beforeRustReopen, err := readStdioStatus(session)
				if err != nil {
					soakFailed(&evidence, "soak/restart-rust-open", err.Error())
					t.Fatal(err)
				}
				session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
					"uri": rustURI, "languageId": "rust", "version": 1, "text": rustText,
				}})
				if err := waitForStdioSnapshot(session, beforeRustReopen, 30*time.Second); err != nil {
					soakFailed(&evidence, "soak/restart-rust-open", err.Error())
					t.Fatal(err)
				}
				if err := waitForRustSoakHover(session, rustURI, rustText, 90*time.Second); err != nil {
					soakFailed(&evidence, "soak/restart-rust-semantic", err.Error())
					t.Fatal(err)
				}
				if err := assertStdioQuerySet(session, queries); err != nil {
					soakFailed(&evidence, "soak/restart-queries", err.Error())
					t.Fatal(err)
				}
				restartedProgress, progressErr := waitForProgressLifecycle(session, "soak-references", 5*time.Second)
				if progressErr != nil {
					soakFailed(&evidence, "soak/restart-progress", progressErr.Error())
					t.Fatal(progressErr)
				}
				progressCounts.Begin += restartedProgress.Begin
				progressCounts.Report += restartedProgress.Report
				progressCounts.End += restartedProgress.End
				workerCtx, stopWorkers = context.WithCancel(context.Background())
				results, workerDone, alive = startStdioWorkers(workerCtx, session, workerQueries, &completed, latencies, startAt)
				lastSnapshotEpochs = number(reopenedStatus["SnapshotEpochs"])
				restarted = true
				evidence.Checks = append(evidence.Checks,
					report.Check{ID: "soak/backend-close", Status: report.Passed, Summary: "candidate and backend process-tree members and handles returned to zero after graceful LSP shutdown"},
					report.Check{ID: "soak/restart", Status: report.Passed, Summary: "second real stdio process reopened the unchanged disk index at the same fresh generation, returned the expected cross-file references, preserved the dirty editor overlay, and restarted all eight workers"},
				)
			}
		case worker := <-workerDone:
			if workerCtx.Err() == nil {
				stopWorkers()
				message := fmt.Sprintf("semantic worker %d exited before the soak was stopped", worker)
				soakFailed(&evidence, "soak/workers", message)
				t.Fatal(message)
			}
		}
		if active == 0 && time.Since(startAt) > 30*time.Second {
			stopWorkers()
			message := "active duration monitor did not advance"
			soakNotVerified(&evidence, "soak/continuity", message)
			continuityRecorded = true
			t.Fatal(message)
		}
	}
	continuityCloseAttempted = true
	closeErr, finalCheckErr := finalizeSuspendResumeGuard(continuityGuard)
	if closeErr != nil {
		soakNotVerified(&evidence, "soak/continuity-monitor-close", closeErr.Error())
		recordContinuityFailure(fmt.Sprintf("could not finalize Windows suspend/resume monitoring: %v", closeErr))
	}
	if finalCheckErr != nil {
		recordContinuityFailure(finalCheckErr.Error())
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if finalCheckErr != nil {
		t.Fatal(finalCheckErr)
	}
	continuityRecorded = true
	evidence.Checks = append(evidence.Checks, report.Check{ID: "soak/continuity", Status: report.Passed, Summary: "no Windows suspend/resume event, sample gap over 10 seconds, minute telemetry gap over 70 seconds, or clock discontinuity was observed", Threshold: map[string]any{"sample_gap_max": (2 * soakPollPeriod).String(), "minute_telemetry_gap_max": (time.Minute + 2*soakPollPeriod).String()}, Observed: map[string]any{"active_elapsed": active.String(), "requested_duration": duration.String(), "suspend_resume_events": 0, "signal": "PowerRegisterSuspendResumeNotification"}})
	stopWorkers()
	waitStdioWorkers(workerDone)
	for index, state := range alive {
		if state == nil || state.Load() {
			message := fmt.Sprintf("semantic worker %d remained alive after cancellation", index)
			soakFailed(&evidence, "soak/workers-stop", message)
			t.Fatal(message)
		}
	}
	finalStatus, err := readStdioStatus(session)
	if err != nil {
		soakFailed(&evidence, "soak/final-server-progress", err.Error())
		t.Fatal(err)
	}
	finalIndexRaw, err := requestStdio(session, "omnilsp/indexStats", map[string]any{})
	if err != nil {
		soakFailed(&evidence, "soak/final-server-progress", err.Error())
		t.Fatal(err)
	}
	var finalIndex map[string]any
	if err := json.Unmarshal(finalIndexRaw, &finalIndex); err != nil {
		soakFailed(&evidence, "soak/final-server-progress", err.Error())
		t.Fatal(err)
	}
	if err := verifyStdioIndexContent(session, docURI, text); err != nil {
		soakFailed(&evidence, "soak/final-index-content", err.Error())
		t.Fatal(err)
	}
	finalGeneration := uint64(number(finalIndex["generation"]))
	finalSnapshotEpochs := number(finalStatus["SnapshotEpochs"])
	if err := validateFinalStdioProgress(finalGeneration, lastCommittedIndexGeneration, finalSnapshotEpochs, lastSnapshotEpochs, finalIndex["enabled"] == true, finalIndex["fresh"] == true); err != nil {
		message := fmt.Sprintf("final server progress is not readable and fresh: generation=%d last_committed=%d snapshot=%d priorSnapshot=%d fresh=%v: %v", finalGeneration, lastCommittedIndexGeneration, finalSnapshotEpochs, lastSnapshotEpochs, finalIndex["fresh"], err)
		soakFailed(&evidence, "soak/final-server-progress", message)
		t.Fatal(message)
	}
	lastIndexGeneration, lastSnapshotEpochs = finalGeneration, finalSnapshotEpochs
	session.Notify(t, "textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": docURI}})
	session.Notify(t, "textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": cancelQueryURI}})
	session.Close(t)
	tree, err := job.sample(os.Getpid(), 0)
	if err != nil || tree.ActiveProcesses != 1 || tree.HandleCounts["candidate"] != 0 || tree.HandleCounts["backend"] != 0 {
		message := fmt.Sprintf("final backend process tree did not close: sample=%+v err=%v", tree, err)
		soakFailed(&evidence, "soak/final-close", message)
		t.Fatal(message)
	}
	evidence.Checks = append(evidence.Checks, report.Check{
		ID: "soak/final-close", Status: report.Passed,
		Summary:   "candidate and all supervised backend processes exited before final process-tree sampling",
		Threshold: map[string]any{"active_processes": 1, "candidate_handles": 0, "backend_handles": 0},
		Observed:  map[string]any{"active_processes": tree.ActiveProcesses, "candidate_handles": tree.HandleCounts["candidate"], "backend_handles": tree.HandleCounts["backend"]},
	})
	finalLatency := latencies.take(int64(active / time.Minute))
	if completed.Load() < uint64(soakWorkers) || edits.Load() == 0 || peakPrivate == 0 {
		message := fmt.Sprintf("insufficient workload coverage: requests=%d edits=%d peakPrivate=%d", completed.Load(), edits.Load(), peakPrivate)
		soakFailed(&evidence, "soak/coverage", message)
		t.Fatal(message)
	}
	evidence.Checks = append(evidence.Checks, report.Check{
		ID: "soak/coverage", Status: report.Passed,
		Summary:   "all eight workers completed semantic requests with observed edits and process-tree memory samples",
		Threshold: map[string]any{"minimum_completed_requests": soakWorkers, "minimum_edits": 1, "minimum_peak_private_bytes": 1},
		Observed:  map[string]any{"completed_requests": completed.Load(), "edits": edits.Load(), "peak_private_bytes": peakPrivate, "worker_count": soakWorkers},
	})
	if active < duration {
		message := fmt.Sprintf("continuous active elapsed %s is shorter than requested %s", active, duration)
		soakNotVerified(&evidence, "soak/duration", message)
		t.Fatal(message)
	}
	evidence.Checks = append(evidence.Checks, report.Check{
		ID: "soak/duration", Status: report.Passed,
		Summary:   "continuous active monitoring covered the requested soak duration",
		Threshold: map[string]any{"requested_duration": duration.String()},
		Observed:  map[string]any{"active_elapsed": active.String(), "requested_duration": duration.String()},
	})
	if canceledReindexes < 2 || canceledQueries < 2 {
		message := fmt.Sprintf("repeated cancellation coverage is incomplete: reindex=%d query=%d; at least two of each are required", canceledReindexes, canceledQueries)
		soakNotVerified(&evidence, "soak/cancel-repeated", message)
		t.Fatal(message)
	}
	if !restarted {
		message := "server close/reopen/restart workload did not run before the soak duration ended"
		soakNotVerified(&evidence, "soak/restart", message)
		t.Fatal(message)
	}
	if !backendRestarted || backendRestarts != 1 {
		message := fmt.Sprintf("controlled supervised backend restart did not complete: started=%t completed=%t count=%d", backendRestartStarted, backendRestarted, backendRestarts)
		soakNotVerified(&evidence, "soak/backend-restart", message)
		t.Fatal(message)
	}
	finalCandidateHash, hashErr := sha256File(serverBin)
	if hashErr != nil || finalCandidateHash != candidateHash {
		message := fmt.Sprintf("frozen candidate changed during soak: initial=%s final=%s err=%v", candidateHash, finalCandidateHash, hashErr)
		soakFailed(&evidence, "soak/candidate-identity", message)
		t.Fatal(message)
	}
	finalWorkerHash, hashErr := hashSoakWorkers(root)
	if hashErr != nil || finalWorkerHash != workerHash {
		message := fmt.Sprintf("soak worker sources changed during run: initial=%s final=%s err=%v", workerHash, finalWorkerHash, hashErr)
		soakFailed(&evidence, "soak/worker-identity", message)
		t.Fatal(message)
	}
	finalGoroutines, finalHeap := waitForStdioRuntimeBounds(baselineGoroutines, baselineMem.HeapAlloc)
	evidence.Environment["final_goroutines"] = fmt.Sprint(finalGoroutines)
	evidence.Environment["final_heap_alloc_bytes"] = fmt.Sprint(finalHeap)
	sequence++
	finalResource := summarizeResourceWindow(int64(active/time.Minute), minutePrivate, minuteHandles)
	writeRow(stdioTelemetry{Sequence: sequence, Kind: "final", UTC: time.Now().UTC(), ActiveElapsed: active, Requests: completed.Load(), Edits: edits.Load(), Tree: tree, Server: finalStatus, Index: finalIndex, SnapshotEpochs: lastSnapshotEpochs, IndexGeneration: lastIndexGeneration, CandidateHash: candidateHash, WorkerHash: workerHash, Seed: soakSeed, BackendRestarts: backendRestarts, Resource: &finalResource,
		LatencyByMethod: finalLatency, CanceledRequests: canceledReindexes, CancellationOutcomes: cloneCounts(cancellationOutcomes), CanceledQueries: canceledQueries, QueryCancelOutcomes: cloneCounts(queryCancellationOutcomes), GoGoroutines: finalGoroutines, GoHeapAlloc: finalHeap})
	if finalGoroutines-baselineGoroutines > 50 || finalHeap >= baselineMem.HeapAlloc+(256<<20) {
		message := fmt.Sprintf("post-GC Go runtime growth exceeded bounds: goroutines %d -> %d (limit +50), heap %d -> %d (limit +256 MiB)", baselineGoroutines, finalGoroutines, baselineMem.HeapAlloc, finalHeap)
		soakFailed(&evidence, "soak/go-runtime-bounds", message)
		t.Fatal(message)
	}
	evidence.Samples = append(evidence.Samples,
		sampleSet("process_tree_private_bytes", privateSamples),
		sampleSetWithUnit("process_tree_total_handles", "handles", handleSamples),
		sampleSet("job_peak_commit_bytes", commitPeakSamples),
		sampleSet("go_heap_alloc_bytes", heapSamples),
		sampleSet("go_goroutines", goroutineSamples),
	)
	evidence.Checks = append(evidence.Checks, report.Check{ID: "soak/resource-cap", Status: report.Passed, Summary: "sampled aggregate process-tree PrivateUsage and Job commit peak stayed within the 8 GiB cap", Threshold: map[string]any{"max_bytes": soakMemoryCap}, Observed: map[string]any{"peak_sampled_private_bytes": peakPrivate, "peak_job_commit_bytes": lastPeak(commitPeakSamples), "source": "PSAPI PrivateUsage summed over Job Object members and Job Object PeakJobMemoryUsed"}})
	trendRequired := duration == time.Hour
	selectedLongGate := "selected_long_gate=none"
	if trendRequired {
		selectedLongGate = "selected_long_gate=1h"
	}
	evidence.Environment["resource_trend_gate"] = fmt.Sprintf("required=%t; %s; required_duration=1h for ten one-minute windows; completed_windows=%d", trendRequired, selectedLongGate, len(resourceWindows))
	if trendRequired {
		resourceStatus := report.Passed
		resourceSummary := "rolling sets of ten one-minute medians were recorded; Theil-Sen slopes estimate robust trend growth across the first-to-last window span"
		if len(resourceWindows) < 10 {
			resourceStatus = report.NotVerified
			resourceSummary = fmt.Sprintf("only %d completed one-minute windows; at least 10 are required for this long-duration stage", len(resourceWindows))
			evidence.Skips = append(evidence.Skips, report.Skip{ID: "soak/resource-trend", Reason: resourceSummary})
		}
		evidence.Checks = append(evidence.Checks, report.Check{ID: "soak/resource-trend", Status: resourceStatus, Summary: resourceSummary, Threshold: map[string]any{"one_minute_windows": resourceTrendWindows, "private_estimated_growth_bytes": resourcePrivateGrowthLimit, "handle_estimated_growth": resourceHandleGrowthLimit, "slope_method": "Theil-Sen median of pairwise slopes against elapsed minute indexes"}, Observed: map[string]any{"completed_minute_windows": len(resourceWindows), "trend_rule": "positive Theil-Sen slope and estimated growth across first-to-last minute span meets the existing threshold", "first_window": firstResourceWindow(resourceWindows), "last_window": lastResourceWindow(resourceWindows), "total_handles_source": "sum of GetProcessHandleCount across stable Job Object PID list"}})
	}
	evidence.Checks = append(evidence.Checks,
		report.Check{ID: "soak/server-progress", Status: report.Passed, Summary: "document edits advanced snapshots, each periodic reindex committed a strictly newer fresh generation, minute telemetry reported both values, and final cross-file references validated index content", Observed: map[string]any{"last_snapshot_epochs": lastSnapshotEpochs, "last_index_generation": lastIndexGeneration, "indexed_reference_minimum": stdioGeneratedFiles + 2, "progress_events": "recorded with each minute row; generation/snapshot changes are the advancement assertions"}},
		report.Check{ID: "soak/progress-lifecycle", Status: report.Passed, Summary: "the references query produced a correlated same-token begin/end lifecycle before its terminal response; index and result checks prove useful work advanced; report events are optional", Observed: map[string]any{"token": "soak-references", "counts": progressCounts, "report_events_optional": true}},
		report.Check{ID: "soak/workers", Status: report.Passed, Summary: "eight concurrent workers completed validated requests throughout both server sessions with GOMAXPROCS fixed at eight", Observed: map[string]any{"workers": soakWorkers, "requests": completed.Load(), "edits": edits.Load(), "gomaxprocs": runtime.GOMAXPROCS(0), "peak_scheduler_inflight": peakInFlight.Load(), "scheduler_inflight_max": soakWorkers}},
		report.Check{ID: "soak/candidate-identity", Status: report.Passed, Summary: "both server sessions used the same frozen candidate binary", Observed: map[string]any{"binary": serverBin, "sha256": candidateHash}},
		report.Check{ID: "soak/worker-identity", Status: report.Passed, Summary: "worker and resource harness source hash recorded in the report and JSONL", Observed: map[string]any{"sha256": workerHash}},
		report.Check{ID: "soak/cancel-repeated", Status: report.Passed, Summary: "seeded-PRNG-cadence reindex and references-query cancellations each received correlated terminal RequestCancelled responses", Observed: map[string]any{"reindex_count": canceledReindexes, "reindex_outcomes": cancellationOutcomes, "query_count": canceledQueries, "query_outcomes": queryCancellationOutcomes, "seed": soakSeed, "interval_min_completed_queries": 192, "interval_max_completed_queries": 320}},
		report.Check{ID: "soak/go-runtime-bounds", Status: report.Passed, Summary: "post-GC Go test-runner growth stayed within the existing goroutine and heap acceptance bounds", Threshold: map[string]any{"goroutine_growth_max": 50, "heap_growth_max_bytes": 256 << 20}, Observed: map[string]any{"baseline_goroutines": baselineGoroutines, "final_goroutines": finalGoroutines, "baseline_heap_alloc_bytes": baselineMem.HeapAlloc, "final_heap_alloc_bytes": finalHeap}},
	)
	t.Logf("completed %s with %d requests, %d edits; peak private bytes=%d", active, completed.Load(), edits.Load(), peakPrivate)
}

func stdioSoakRepoRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve stdio soak source path")
	}
	root, err := filepath.Abs(filepath.Join(filepath.Dir(source), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func stdioSoakPaths(root, runID string) (string, string) {
	evidenceDir := filepath.Join(root, "test", "acceptance", "evidence", runID)
	return filepath.Join(evidenceDir, "soak-report.json"), filepath.Join(evidenceDir, "soak.jsonl")
}

func openStdioJSONL(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

func frozenStdioCandidate() (string, error) {
	path := strings.TrimSpace(os.Getenv("OMNILSP_BIN"))
	if path == "" {
		return "", errors.New("OMNILSP_BIN must name the frozen candidate binary")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("frozen OMNILSP_BIN is unavailable: %s", absolute)
	}
	return absolute, nil
}

func stdioServerEnv(root, workspace, cargoTarget string) []string {
	toolBin := filepath.Join(root, "test", "acceptance", "tools", "bin")
	path := toolBin
	if current := os.Getenv("PATH"); current != "" {
		path += string(os.PathListSeparator) + current
	}
	return []string{
		"OMNILSP_TRUST=trusted",
		"OMNILSP_MAX_CONCURRENT=8",
		"OMNILSP_MAX_QUEUE=64",
		"OMNILSP_INDEX_DIR=" + filepath.Join(workspace, ".omnilsp-index"),
		"CARGO_TARGET_DIR=" + cargoTarget,
		"GOMAXPROCS=8",
		"PATH=" + path,
	}
}

func prepareStdioCorpus(root string) (string, string, string, string, string, error) {
	const generatedFiles = stdioGeneratedFiles
	module := []byte("module soak.local/stdio\n\ngo 1.26\n")
	cargo := []byte("[package]\nname = \"soak\"\nversion = \"0.1.0\"\nedition = \"2024\"\n")
	cargoLock := []byte("# This file is automatically @generated by Cargo.\n# It is not intended for manual editing.\nversion = 4\n\n[[package]]\nname = \"soak\"\nversion = \"0.1.0\"\n")
	rustSource := []byte(rustSoakSource())
	mainText := "package soak\n\nfunc soakTarget(value int) int { return value + 1 }\nfunc useTarget() int { return soakTarget(1) }\n"
	if err := os.WriteFile(filepath.Join(root, "go.mod"), module, 0o644); err != nil {
		return "", "", "", "", "", err
	}
	if err := os.WriteFile(filepath.Join(root, "soak.go"), []byte(mainText), 0o644); err != nil {
		return "", "", "", "", "", err
	}
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), cargo, 0o644); err != nil {
		return "", "", "", "", "", err
	}
	if err := os.WriteFile(filepath.Join(root, "Cargo.lock"), cargoLock, 0o644); err != nil {
		return "", "", "", "", "", err
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		return "", "", "", "", "", err
	}
	if err := os.WriteFile(filepath.Join(root, "src", "lib.rs"), rustSource, 0o644); err != nil {
		return "", "", "", "", "", err
	}
	for i := 0; i < generatedFiles; i++ {
		name := fmt.Sprintf("corpus_%04d.go", i)
		content := fmt.Sprintf("package soak\n\nfunc corpusUse%04d() int { return soakTarget(%d) }\n", i, i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			return "", "", "", "", "", err
		}
	}
	docPath := filepath.Join(root, "soak.go")
	var corpus bytes.Buffer
	corpus.WriteString("go.mod\x00")
	corpus.Write(module)
	corpus.WriteString("soak.go\x00")
	corpus.WriteString(mainText)
	corpus.WriteString("Cargo.toml\x00")
	corpus.Write(cargo)
	corpus.WriteString("Cargo.lock\x00")
	corpus.Write(cargoLock)
	corpus.WriteString("src/lib.rs\x00")
	corpus.Write(rustSource)
	for i := 0; i < generatedFiles; i++ {
		name := fmt.Sprintf("corpus_%04d.go", i)
		content := fmt.Sprintf("package soak\n\nfunc corpusUse%04d() int { return soakTarget(%d) }\n", i, i)
		corpus.WriteString(name)
		corpus.WriteByte(0)
		corpus.WriteString(content)
	}
	hash := sha256.Sum256(corpus.Bytes())
	queryPath := filepath.Join(root, "corpus_0000.go")
	queryText := fmt.Sprintf("package soak\n\nfunc corpusUse%04d() int { return soakTarget(%d) }\n", 0, 0)
	return hex.EncodeToString(hash[:]), uri.FromPath(docPath).String(), mainText, uri.FromPath(queryPath).String(), queryText, nil
}

func rustSoakSource() string {
	return "pub fn rustSoakTarget(value: i64) -> i64 { value + 1 }\npub fn rustSoakUse() -> i64 { rustSoakTarget(1) }\n"
}

func hashSoakWorkers(root string) (string, error) {
	paths := []string{
		"test/soak/stdio_soak_test.go",
		"test/soak/process_tree_windows_test.go",
		"test/soak/process_tree_other_test.go",
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, name := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			return "", fmt.Errorf("read worker source %s: %w", name, err)
		}
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sha256File(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func verifyStdioTools(root string, evidence *report.Report) error {
	lockPath := filepath.Join(root, "test", "acceptance", "tools", "tools.lock.json")
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("read pinned tools lock: %w", err)
	}
	var lock struct {
		Observed struct {
			Go           string `json:"go"`
			Clangd       string `json:"clangd"`
			RustAnalyzer string `json:"rustAnalyzer"`
			Python       string `json:"python"`
			Node         string `json:"node"`
		} `json:"observed"`
		AcceptanceTools struct {
			Pyright struct {
				Version string `json:"version"`
			} `json:"pyright"`
			TypeScriptLanguageServer struct {
				Version string `json:"version"`
			} `json:"typescriptLanguageServer"`
			TypeScript struct {
				Version string `json:"version"`
			} `json:"typescript"`
		} `json:"acceptanceTools"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return fmt.Errorf("parse pinned tools lock: %w", err)
	}
	checks := make([]report.Check, 0, 9)
	probe := func(id, executable string, args []string, expected string) {
		path, lookErr := exec.LookPath(executable)
		if lookErr != nil {
			checks = append(checks, report.Check{ID: "tool/" + id, Status: report.NotVerified, Summary: executable + " is not available on the process PATH"})
			return
		}
		out, runErr := exec.Command(path, args...).CombinedOutput()
		version := strings.TrimSpace(string(out))
		matches := runErr == nil && exactSoakToolVersion(id, version, expected)
		status := report.Passed
		if !matches {
			status = report.NotVerified
		}
		checks = append(checks, report.Check{ID: "tool/" + id, Status: status, Summary: fmt.Sprintf("%s reported %q; pinned version is %q", executable, version, expected), Observed: map[string]any{"path": path, "version": version}, Threshold: map[string]any{"version": expected}})
	}
	probe("go", "go", []string{"version"}, lock.Observed.Go)
	probe("node", "node", []string{"--version"}, lock.Observed.Node)
	probe("python", "python", []string{"--version"}, lock.Observed.Python)
	probe("clangd", "clangd", []string{"--version"}, lock.Observed.Clangd)
	probe("rust-analyzer", "rust-analyzer", []string{"--version"}, lock.Observed.RustAnalyzer)
	toolsRoot := filepath.Join(root, "test", "acceptance", "tools")
	for _, item := range []struct{ id, rel, want string }{
		{"pyright", filepath.Join("node_modules", "pyright", "package.json"), lock.AcceptanceTools.Pyright.Version},
		{"typescript-language-server", filepath.Join("node_modules", "typescript-language-server", "package.json"), lock.AcceptanceTools.TypeScriptLanguageServer.Version},
		{"typescript", filepath.Join("node_modules", "typescript", "package.json"), lock.AcceptanceTools.TypeScript.Version},
	} {
		packagePath := filepath.Join(toolsRoot, item.rel)
		var pkg struct {
			Version string `json:"version"`
		}
		packageData, readErr := os.ReadFile(packagePath)
		if readErr == nil {
			readErr = json.Unmarshal(packageData, &pkg)
		}
		status := report.Passed
		if readErr != nil || item.want == "" || pkg.Version != item.want {
			status = report.NotVerified
		}
		checks = append(checks, report.Check{ID: "tool/" + item.id, Status: status, Summary: fmt.Sprintf("local package version is %q; lock requires %q", pkg.Version, item.want), Observed: map[string]any{"path": packagePath, "version": pkg.Version}, Threshold: map[string]any{"version": item.want}})
	}
	for _, name := range []string{"pyright-langserver", "typescript-language-server"} {
		path := filepath.Join(toolsRoot, "bin", name+".exe")
		info, statErr := os.Stat(path)
		status := report.Passed
		if statErr != nil || info.IsDir() {
			status = report.NotVerified
		}
		checks = append(checks, report.Check{ID: "tool/" + name + "-wrapper", Status: status, Summary: "project-local pinned language-server wrapper", Observed: map[string]any{"path": path}})
	}
	evidence.Checks = append(evidence.Checks, checks...)
	var missing []string
	for _, check := range checks {
		if check.Status != report.Passed {
			evidence.Skips = append(evidence.Skips, report.Skip{ID: check.ID, Reason: check.Summary})
			missing = append(missing, check.ID+": "+check.Summary)
		}
	}
	if len(missing) != 0 {
		return errors.New(strings.Join(missing, "; "))
	}
	return nil
}

func exactSoakToolVersion(id, output, expected string) bool {
	line := strings.TrimSpace(output)
	if index := strings.IndexAny(line, "\r\n"); index >= 0 {
		line = line[:index]
	}
	fields := strings.Fields(line)
	switch id {
	case "go":
		return len(fields) >= 3 && fields[0] == "go" && fields[1] == "version" && fields[2] == expected
	case "node":
		return line == expected
	case "python":
		return len(fields) == 2 && fields[0] == "Python" && fields[1] == expected
	case "clangd":
		return len(fields) >= 3 && fields[0] == "clangd" && fields[1] == "version" && fields[2] == expected
	case "rust-analyzer":
		return len(fields) >= 2 && fields[0] == "rust-analyzer" && strings.Join(fields[1:], " ") == expected
	default:
		return false
	}
}

func TestExactSoakToolVersionRejectsSuffixAndPrefixMatches(t *testing.T) {
	tests := []struct {
		id, output, expected string
		want                 bool
	}{
		{"go", "go version go1.26.1 windows/amd64", "go1.26.1", true},
		{"go", "go version go1.26.10 windows/amd64", "go1.26.1", false},
		{"node", "v24.14.0", "v24.14.0", true},
		{"node", "v24.14.0-extra", "v24.14.0", false},
		{"python", "Python 3.13.13", "3.13.13", true},
		{"clangd", "clangd version 22.1.5", "22.1.5", true},
		{"clangd", "clangd version 22.1.50", "22.1.5", false},
		{"rust-analyzer", "rust-analyzer 1.97.0 (2d8144b7 2026-07-07)", "1.97.0 (2d8144b7 2026-07-07)", true},
		{"rust-analyzer", "rust-analyzer 1.97.0-extra (2d8144b7 2026-07-07)", "1.97.0 (2d8144b7 2026-07-07)", false},
	}
	for _, test := range tests {
		t.Run(test.id+"/"+test.output, func(t *testing.T) {
			if got := exactSoakToolVersion(test.id, test.output, test.expected); got != test.want {
				t.Fatalf("exactSoakToolVersion(%q, %q, %q) = %t, want %t", test.id, test.output, test.expected, got, test.want)
			}
		})
	}
}

func TestMakeStdioQueriesKeepSymbolPositionsIndependent(t *testing.T) {
	text := stdioGoSource()
	queries := makeStdioQueries("file:///soak.go", text)
	base := positionOf(text, "soakTarget", 2)
	wantOffsets := map[string]int{
		"textDocument/definition":    0,
		"textDocument/references":    0,
		"textDocument/completion":    5,
		"textDocument/prepareRename": len("soakTarget"),
	}
	for _, query := range queries {
		offset, ok := wantOffsets[query.method]
		if !ok {
			continue
		}
		position := query.params["position"].(map[string]any)
		if got, want := number(position["character"]), number(base["character"])+offset; got != want {
			t.Errorf("%s character = %d, want %d", query.method, got, want)
		}
	}
}

func makeStdioQueries(docURI, text string) []stdioQuery {
	definition := positionOf(text, "soakTarget", 1)
	call := positionOf(text, "soakTarget", 2)
	completion := positionOf(text, "soakTarget", 2)
	completion["character"] = number(completion["character"]) + 5
	prepareRename := positionOf(text, "soakTarget", 2)
	prepareRename["character"] = number(prepareRename["character"]) + len("soakTarget")
	textDoc := map[string]any{"uri": docURI}
	return []stdioQuery{
		{method: "textDocument/hover", params: map[string]any{"textDocument": textDoc, "position": definition}},
		{method: "textDocument/definition", params: map[string]any{"textDocument": textDoc, "position": call}},
		{method: "textDocument/completion", params: map[string]any{"textDocument": textDoc, "position": completion}},
		{method: "textDocument/references", params: map[string]any{"textDocument": textDoc, "position": call, "context": map[string]any{"includeDeclaration": true}, "workDoneToken": "soak-references"}},
		{method: "textDocument/documentSymbol", params: map[string]any{"textDocument": textDoc}},
		{method: "textDocument/prepareRename", params: map[string]any{"textDocument": textDoc, "position": prepareRename}},
		{method: "omnilsp/status", params: map[string]any{}},
		{method: "omnilsp/indexStats", params: map[string]any{}},
	}
}

func withoutProgressTokens(queries []stdioQuery) []stdioQuery {
	result := make([]stdioQuery, len(queries))
	for i, query := range queries {
		params := make(map[string]any, len(query.params))
		for key, value := range query.params {
			params[key] = value
		}
		delete(params, "workDoneToken")
		result[i] = stdioQuery{method: query.method, params: params}
	}
	return result
}

func positionOf(text, symbol string, occurrence int) map[string]any {
	index := -1
	start := 0
	for i := 0; i < occurrence; i++ {
		next := strings.Index(text[start:], symbol)
		if next < 0 {
			return map[string]any{"line": -1, "character": -1}
		}
		index = start + next
		start = index + len(symbol)
	}
	line := strings.Count(text[:index], "\n")
	lastNewline := strings.LastIndex(text[:index], "\n")
	return map[string]any{"line": line, "character": index - lastNewline - 1}
}

func assertStdioQuerySet(session *lspdriver.Session, queries []stdioQuery) error {
	for _, query := range queries {
		eventStart := len(session.Events())
		result, err := requestStdio(session, query.method, query.params)
		if err != nil {
			return fmt.Errorf("%s terminal response: %w", query.method, err)
		}
		if err := assertStdioQuery(query.method, result, queryURI(query), stdioGoSource()); err != nil {
			return fmt.Errorf("%s semantic assertion: %w", query.method, err)
		}
		if query.method == "textDocument/references" {
			counts, progressErr := progressLifecycleBeforeResponse(session.Events()[eventStart:], "soak-references")
			if progressErr != nil {
				return fmt.Errorf("references work-done lifecycle: %w", progressErr)
			}
			if counts.Begin == 0 || counts.End == 0 {
				return fmt.Errorf("references request did not produce correlated begin/end progress: %+v", counts)
			}
		}
	}
	textDocument, _ := queries[0].params["textDocument"].(map[string]any)
	docURI, _ := textDocument["uri"].(string)
	return assertUnsafeStdioRename(session, docURI)
}

func queryURI(query stdioQuery) string {
	document, _ := query.params["textDocument"].(map[string]any)
	value, _ := document["uri"].(string)
	return value
}

func assertStdioQuery(method string, raw json.RawMessage, docURI, text string) error {
	switch method {
	case "textDocument/hover":
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil || value == nil || value["contents"] == nil {
			return fmt.Errorf("hover has no contents: %s", raw)
		}
		content, _ := json.Marshal(value["contents"])
		if !bytes.Contains(content, []byte("soakTarget")) {
			return fmt.Errorf("hover did not identify soakTarget: %s", raw)
		}
	case "textDocument/definition", "textDocument/references":
		locations, err := decodeStdioLocations(raw)
		if err != nil || len(locations) == 0 {
			return fmt.Errorf("location result is empty or invalid: %s", raw)
		}
		declaration := positionOf(text, "soakTarget", 1)
		if method == "textDocument/definition" {
			if !hasLocationAt(locations, docURI, declaration) {
				return fmt.Errorf("definition did not target soakTarget at %s:%v: %s", docURI, declaration, raw)
			}
		} else {
			use := positionOf(text, "soakTarget", 2)
			if len(locations) < stdioGeneratedFiles+2 {
				return fmt.Errorf("long references query returned only %d locations; want at least %d", len(locations), stdioGeneratedFiles+2)
			}
			if !hasLocationAt(locations, docURI, declaration) || !hasLocationAt(locations, docURI, use) {
				return fmt.Errorf("references did not include the exact declaration and use ranges in %s: %s", docURI, raw)
			}
		}
	case "textDocument/completion":
		var list struct {
			Items []struct {
				Label string `json:"label"`
			} `json:"items"`
		}
		if err := json.Unmarshal(raw, &list.Items); err != nil || len(list.Items) == 0 {
			if err := json.Unmarshal(raw, &list); err != nil || len(list.Items) == 0 {
				return fmt.Errorf("completion list is empty or invalid: %s", raw)
			}
		}
		found := false
		for _, item := range list.Items {
			if item.Label == "soakTarget" {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("completion did not include the soakTarget symbol: %s", raw)
		}
	case "textDocument/documentSymbol":
		var value []struct {
			Name     string `json:"name"`
			Children []struct {
				Name string `json:"name"`
			} `json:"children"`
		}
		if err := json.Unmarshal(raw, &value); err != nil || len(value) == 0 {
			return fmt.Errorf("document symbols are empty or invalid: %s", raw)
		}
		found := false
		for _, symbol := range value {
			found = found || symbol.Name == "soakTarget"
			for _, child := range symbol.Children {
				found = found || child.Name == "soakTarget"
			}
		}
		if !found {
			return fmt.Errorf("document symbols did not include soakTarget: %s", raw)
		}
	case "textDocument/prepareRename":
		var value struct {
			Placeholder string `json:"placeholder"`
		}
		if err := json.Unmarshal(raw, &value); err != nil || value.Placeholder != "soakTarget" {
			return fmt.Errorf("prepareRename did not identify soakTarget: %s", raw)
		}
	case "omnilsp/status":
		var value struct{ SnapshotEpochs int64 }
		if err := json.Unmarshal(raw, &value); err != nil || value.SnapshotEpochs < 1 {
			return fmt.Errorf("status snapshot epochs are not positive: %s", raw)
		}
	case "omnilsp/indexStats":
		var value struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &value); err != nil || !value.Enabled {
			return fmt.Errorf("persistent index is not enabled: %s", raw)
		}
	default:
		return fmt.Errorf("unrecognized query method %q", method)
	}
	return nil
}

type stdioPosition struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

type stdioRange struct {
	Start stdioPosition `json:"start"`
}

type stdioLocation struct {
	URI                  string     `json:"uri"`
	Range                stdioRange `json:"range"`
	TargetURI            string     `json:"targetUri"`
	TargetRange          stdioRange `json:"targetRange"`
	TargetSelectionRange stdioRange `json:"targetSelectionRange"`
}

func decodeStdioLocations(raw []byte) ([]stdioLocation, error) {
	var locations []stdioLocation
	if err := json.Unmarshal(raw, &locations); err == nil {
		return locations, nil
	}
	var location stdioLocation
	if err := json.Unmarshal(raw, &location); err != nil {
		return nil, err
	}
	return []stdioLocation{location}, nil
}

func hasLocationAt(locations []stdioLocation, uri string, position map[string]any) bool {
	wantLine, wantCharacter := uint32(number(position["line"])), uint32(number(position["character"]))
	for _, location := range locations {
		gotURI, gotPosition := location.URI, location.Range.Start
		if location.TargetURI != "" {
			gotURI, gotPosition = location.TargetURI, location.TargetSelectionRange.Start
		}
		if gotURI == uri && gotPosition.Line == wantLine && gotPosition.Character == wantCharacter {
			return true
		}
	}
	return false
}

func stdioGoSource() string {
	return "package soak\n\nfunc soakTarget(value int) int { return value + 1 }\nfunc useTarget() int { return soakTarget(1) }\n"
}

func assertUnsafeStdioRename(session *lspdriver.Session, docURI string) error {
	text := "package soak\n\nfunc soakTarget(value int) int { return value + 1 }\nfunc useTarget() int { return soakTarget(1) }\n"
	position := positionOf(text, "soakTarget", 1)
	beforeStatus, err := readStdioStatus(session)
	if err != nil {
		return fmt.Errorf("status before unsafe rename: %w", err)
	}
	_, err = requestStdio(session, "textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": docURI},
		"position":     position,
		"newName":      "useTarget",
	})
	var responseError *jsonrpc.ResponseError
	if !errors.As(err, &responseError) || responseError.Code != jsonrpc.RequestFailed {
		return fmt.Errorf("collision rename did not return JSON-RPC RequestFailed: %w", err)
	}
	if !strings.Contains(strings.ToLower(responseError.Message), "collision") {
		return fmt.Errorf("unsafe rename rejection did not explain the collision: %w", responseError)
	}
	afterStatus, err := readStdioStatus(session)
	if err != nil {
		return fmt.Errorf("status after unsafe rename: %w", err)
	}
	if number(beforeStatus["SnapshotEpochs"]) != number(afterStatus["SnapshotEpochs"]) {
		return fmt.Errorf("document snapshot changed after refused collision rename: before=%v after=%v", beforeStatus["SnapshotEpochs"], afterStatus["SnapshotEpochs"])
	}
	return nil
}

func waitForStdioOpen(session *lspdriver.Session, _ string) error {
	return waitForStdioSnapshot(session, map[string]any{"SnapshotEpochs": float64(0)}, 30*time.Second)
}

func waitForStdioSnapshot(session *lspdriver.Session, before map[string]any, timeout time.Duration) error {
	previous := number(before["SnapshotEpochs"])
	if previous == 0 {
		previous = number(before["snapshotEpochs"])
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := readStdioStatus(session)
		if err == nil && number(status["SnapshotEpochs"]) > previous {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("snapshot epoch did not advance beyond %v", previous)
}

func readStdioStatus(session *lspdriver.Session) (map[string]any, error) {
	result, err := requestStdio(session, "omnilsp/status", map[string]any{})
	if err != nil {
		return nil, err
	}
	var status map[string]any
	if err := json.Unmarshal(result, &status); err != nil {
		return nil, fmt.Errorf("decode omnilsp/status: %w", err)
	}
	return status, nil
}

func verifyStdioBackends(session *lspdriver.Session) error {
	for _, language := range []string{"go", "cpp", "rust", "python", "typescript"} {
		result, err := requestStdio(session, "omnilsp/backendStatus", map[string]any{"language": language})
		if err != nil {
			return fmt.Errorf("backendStatus %s: %w", language, err)
		}
		var status struct{ Found bool }
		if err := json.Unmarshal(result, &status); err != nil || !status.Found {
			return fmt.Errorf("backend %s is not registered: %s", language, result)
		}
	}
	return nil
}

func restartControlledRustBackend(session *lspdriver.Session, job *processTreeBudget, runnerPID, candidatePID int, executable, rustURI, rustText string) (uint32, uint32, error) {
	expected, err := filepath.Abs(executable)
	if err != nil {
		return 0, 0, err
	}
	initial, err := job.sample(runnerPID, candidatePID)
	if err != nil {
		return 0, 0, fmt.Errorf("sample before backend restart: %w", err)
	}
	oldPID, err := supervisedRustAnalyzerPID(initial, uint32(candidatePID), expected)
	if err != nil {
		return 0, 0, fmt.Errorf("find supervised rust-analyzer process before restart: %w", err)
	}
	if err := waitForRustSoakHover(session, rustURI, rustText, 90*time.Second); err != nil {
		return 0, 0, fmt.Errorf("Rust backend semantic probe before restart: %w", err)
	}
	if err := terminateJobPID(oldPID); err != nil {
		sample, sampleErr := job.sample(runnerPID, candidatePID)
		if sampleErr != nil || containsProcessPID(sample, oldPID) {
			return oldPID, 0, fmt.Errorf("terminate supervised rust-analyzer PID %d: %w (resample: %v)", oldPID, err, sampleErr)
		}
	}
	restartAt := time.Now()
	deadline := time.Now().Add(60 * time.Second)
	var newPID uint32
	var lastSampleErr error
	for time.Now().Before(deadline) {
		sample, sampleErr := job.sample(runnerPID, candidatePID)
		if sampleErr != nil {
			lastSampleErr = sampleErr
			time.Sleep(100 * time.Millisecond)
			continue
		}
		pid, identityErr := supervisedRustAnalyzerPID(sample, uint32(candidatePID), expected)
		if identityErr == nil && pid != oldPID {
			// A process image appearing is not enough: wait for a semantic
			// response, then resolve identity again so a transient rustup shim
			// cannot be mistaken for the supervised language server.
			if hoverErr := waitForRustSoakHoverAfter(session, rustURI, rustText, 2*time.Second, restartAt); hoverErr == nil {
				confirmed, confirmErr := job.sample(runnerPID, candidatePID)
				if confirmErr == nil {
					confirmedPID, confirmedErr := supervisedRustAnalyzerPID(confirmed, uint32(candidatePID), expected)
					if confirmedErr == nil && confirmedPID == pid && !containsProcessPID(confirmed, oldPID) {
						newPID = pid
						break
					}
				}
			}
		} else if identityErr != nil {
			lastSampleErr = identityErr
		}
		time.Sleep(250 * time.Millisecond)
	}
	if newPID == 0 {
		return oldPID, 0, fmt.Errorf("supervisor did not replace supervised rust-analyzer PID %d with a semantically ready process within 60s (last identity/sample error: %v)", oldPID, lastSampleErr)
	}
	if err := verifyRustBackendStable(session, job, runnerPID, candidatePID, expected, rustURI, rustText, oldPID, newPID, restartAt, 12*time.Second); err != nil {
		return oldPID, newPID, err
	}
	return oldPID, newPID, nil
}

func verifyRustBackendStable(session *lspdriver.Session, job *processTreeBudget, runnerPID, candidatePID int, executable, rustURI, rustText string, oldPID, newPID uint32, recoveredAfter time.Time, duration time.Duration) error {
	deadline := time.Now().Add(duration)
	lastSemanticProbe := time.Now()
	for time.Now().Before(deadline) {
		sample, err := job.sample(runnerPID, candidatePID)
		if err != nil {
			return fmt.Errorf("sample replacement backend during stability window: %w", err)
		}
		pid, err := supervisedRustAnalyzerPID(sample, uint32(candidatePID), executable)
		if err != nil || pid != newPID || containsProcessPID(sample, oldPID) {
			return fmt.Errorf("rust-analyzer did not remain stable for %s: expected replacement PID %d, observed PID %d (identity error %v, old PID present %t)", duration, newPID, pid, err, containsProcessPID(sample, oldPID))
		}
		if time.Since(lastSemanticProbe) >= 3*time.Second {
			if err := waitForRustSoakHoverAfter(session, rustURI, rustText, 3*time.Second, recoveredAfter); err != nil {
				return fmt.Errorf("replacement rust-analyzer lost semantic recovery during stability window: %w", err)
			}
			lastSemanticProbe = time.Now()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func supervisedRustAnalyzerPID(sample processTreeSample, candidatePID uint32, executable string) (uint32, error) {
	members := make(map[uint32]processTreeMember, len(sample.Members))
	for _, member := range sample.Members {
		members[member.PID] = member
	}
	if _, ok := members[candidatePID]; !ok {
		return 0, fmt.Errorf("candidate PID %d is absent from the measured process tree", candidatePID)
	}
	candidates := make(map[uint32]struct{})
	for _, member := range sample.Members {
		if isRustAnalyzerImage(member.ImagePath, executable) && processDescendsFrom(member.PID, candidatePID, members) {
			candidates[member.PID] = struct{}{}
		}
	}
	if len(candidates) == 0 {
		return 0, fmt.Errorf("no rust-analyzer process descends from candidate PID %d", candidatePID)
	}
	var leaves []uint32
	for pid := range candidates {
		hasCandidateDescendant := false
		for other := range candidates {
			if other != pid && processDescendsFrom(other, pid, members) {
				hasCandidateDescendant = true
				break
			}
		}
		if !hasCandidateDescendant {
			leaves = append(leaves, pid)
		}
	}
	if len(leaves) != 1 {
		sort.Slice(leaves, func(i, j int) bool { return leaves[i] < leaves[j] })
		return 0, fmt.Errorf("expected one supervised rust-analyzer leaf process, got %v from candidates %v", leaves, sortedProcessPIDs(candidates))
	}
	return leaves[0], nil
}

func processDescendsFrom(pid, ancestor uint32, members map[uint32]processTreeMember) bool {
	seen := make(map[uint32]struct{}, len(members))
	for steps := 0; steps < len(members); steps++ {
		member, ok := members[pid]
		if !ok || member.ParentPID == 0 {
			return false
		}
		if member.ParentPID == ancestor {
			return true
		}
		if _, duplicate := seen[member.ParentPID]; duplicate {
			return false
		}
		seen[member.ParentPID] = struct{}{}
		pid = member.ParentPID
	}
	return false
}

func sortedProcessPIDs(values map[uint32]struct{}) []uint32 {
	pids := make([]uint32, 0, len(values))
	for pid := range values {
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	return pids
}

func isRustAnalyzerImage(image, expected string) bool {
	if sameExecutablePath(image, expected) {
		return true
	}
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(filepath.Clean(image))), strings.ToLower(filepath.Ext(image)))
	return base == "rust-analyzer"
}

func containsProcessPID(sample processTreeSample, pid uint32) bool {
	for _, member := range sample.Members {
		if member.PID == pid {
			return true
		}
	}
	return false
}

func sameExecutablePath(left, right string) bool {
	clean := func(value string) string {
		value = strings.TrimPrefix(filepath.Clean(value), `\\?\`)
		return strings.ToLower(value)
	}
	return clean(left) == clean(right)
}

func waitForRustSoakHover(session *lspdriver.Session, rustURI, rustText string, timeout time.Duration) error {
	return waitForRustSoakHoverAfter(session, rustURI, rustText, timeout, time.Time{})
}

func waitForRustSoakHoverAfter(session *lspdriver.Session, rustURI, rustText string, timeout time.Duration, after time.Time) error {
	deadline := time.Now().Add(timeout)
	position := positionOf(rustText, "rustSoakTarget", 1)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		raw, err := session.RequestContext(ctx, "textDocument/hover", map[string]any{
			"textDocument": map[string]any{"uri": rustURI},
			"position":     position,
		})
		cancel()
		if err == nil {
			var hover map[string]any
			if json.Unmarshal(raw, &hover) == nil && hover != nil {
				contents, _ := json.Marshal(hover["contents"])
				if bytes.Contains(contents, []byte("rustSoakTarget")) {
					if err := verifyRustHoverEvidenceAfter(session, rustURI, after); err == nil {
						return nil
					} else {
						lastErr = err
					}
				} else {
					lastErr = fmt.Errorf("Rust hover did not identify rustSoakTarget: %s", raw)
				}
			} else {
				lastErr = fmt.Errorf("invalid Rust hover response: %s", raw)
			}
		} else {
			lastErr = err
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("no semantic Rust hover from rust-analyzer within %s: %w", timeout, lastErr)
}

func verifyRustHoverEvidenceAfter(session *lspdriver.Session, rustURI string, after time.Time) error {
	raw, err := requestStdio(session, "omnilsp/explain", map[string]any{"uri": rustURI})
	if err != nil {
		return err
	}
	var explanation struct {
		Evidence []struct {
			Method  string `json:"method"`
			URI     string `json:"uri"`
			Backend string `json:"backend"`
			At      string `json:"at"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(raw, &explanation); err != nil {
		return err
	}
	for i := len(explanation.Evidence) - 1; i >= 0; i-- {
		evidence := explanation.Evidence[i]
		if evidence.Method != "textDocument/hover" || evidence.URI != rustURI {
			continue
		}
		if evidence.Backend != "rust/rust-analyzer" {
			return fmt.Errorf("latest Rust hover used backend %q: %s", evidence.Backend, raw)
		}
		if !after.IsZero() {
			at, parseErr := time.Parse(time.RFC3339Nano, evidence.At)
			if parseErr != nil || at.Before(after) {
				return fmt.Errorf("latest Rust hover evidence is not newer than the backend restart: at=%q restart=%s err=%v", evidence.At, after.Format(time.RFC3339Nano), parseErr)
			}
		}
		return nil
	}
	return fmt.Errorf("omnilsp/explain has no post-request rust/rust-analyzer hover evidence for %s: %s", rustURI, raw)
}

func verifyStdioIndex(session *lspdriver.Session) error {
	result, err := requestStdio(session, "omnilsp/indexStats", map[string]any{})
	if err != nil {
		return err
	}
	var stats struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(result, &stats); err != nil || !stats.Enabled {
		return fmt.Errorf("index is not enabled: stats=%s reason=%s", result, stats.Reason)
	}
	return nil
}

func rebuildStdioIndex(session *lspdriver.Session) (uint64, error) {
	beforeRaw, err := requestStdio(session, "omnilsp/indexStats", map[string]any{})
	if err != nil {
		return 0, fmt.Errorf("indexStats before reindex: %w", err)
	}
	var before struct {
		Enabled    bool   `json:"enabled"`
		Generation uint64 `json:"generation"`
	}
	if err := json.Unmarshal(beforeRaw, &before); err != nil || !before.Enabled {
		return 0, fmt.Errorf("index is not enabled before reindex: %s", beforeRaw)
	}
	result, err := requestReindexWithRetry(context.Background(), func(ctx context.Context) (json.RawMessage, error) {
		return session.RequestContext(ctx, "omnilsp/reindex", map[string]any{})
	})
	if err != nil {
		return 0, fmt.Errorf("omnilsp/reindex: %w", err)
	}
	var rebuilt struct {
		Generation uint64 `json:"generation"`
		Files      int    `json:"files"`
	}
	if err := json.Unmarshal(result, &rebuilt); err != nil {
		return 0, fmt.Errorf("decode reindex response: %w (%s)", err, result)
	}
	if err := validateReindexGeneration(before.Generation, rebuilt.Generation, rebuilt.Files); err != nil {
		return 0, fmt.Errorf("reindex returned no meaningful generation/files: %s: %w", result, err)
	}
	afterRaw, err := requestStdio(session, "omnilsp/indexStats", map[string]any{})
	if err != nil {
		return 0, fmt.Errorf("indexStats after reindex: %w", err)
	}
	var after struct {
		Enabled    bool   `json:"enabled"`
		Fresh      bool   `json:"fresh"`
		Generation uint64 `json:"generation"`
	}
	if err := json.Unmarshal(afterRaw, &after); err != nil || !after.Enabled || !after.Fresh || after.Generation != rebuilt.Generation {
		return 0, fmt.Errorf("reindex generation did not become a fresh readable index: response=%s stats=%s", result, afterRaw)
	}
	return rebuilt.Generation, nil
}

func freshPersistentStdioIndexGeneration(session *lspdriver.Session) (uint64, error) {
	result, err := requestStdio(session, "omnilsp/indexStats", map[string]any{})
	if err != nil {
		return 0, err
	}
	var stats struct {
		Enabled    bool   `json:"enabled"`
		Fresh      bool   `json:"fresh"`
		Generation uint64 `json:"generation"`
	}
	if err := json.Unmarshal(result, &stats); err != nil {
		return 0, fmt.Errorf("decode persistent index stats: %w (%s)", err, result)
	}
	if err := validatePersistentIndexRecovery(stats.Generation, 0, stats.Enabled, stats.Fresh); err != nil {
		return 0, fmt.Errorf("persistent index is not enabled with a fresh readable generation: %s: %w", result, err)
	}
	return stats.Generation, nil
}

func verifyPersistentStdioIndex(session *lspdriver.Session, expectedGeneration uint64, docURI, text string) error {
	generation, err := freshPersistentStdioIndexGeneration(session)
	if err != nil {
		return err
	}
	if expectedGeneration == 0 {
		return fmt.Errorf("cannot verify restart recovery against an empty prior generation")
	}
	if err := validatePersistentIndexRecovery(generation, expectedGeneration, true, true); err != nil {
		return err
	}
	return verifyStdioIndexContent(session, docURI, text)
}

func validatePersistentIndexRecovery(generation, expectedGeneration uint64, enabled, fresh bool) error {
	if !enabled || !fresh || generation == 0 {
		return fmt.Errorf("index is not enabled with a nonzero fresh generation: enabled=%t fresh=%t generation=%d", enabled, fresh, generation)
	}
	if expectedGeneration != 0 && generation != expectedGeneration {
		return fmt.Errorf("persistent index recovery changed generation: got %d want the previously committed fresh generation %d", generation, expectedGeneration)
	}
	return nil
}

func requestReindexWithRetry(ctx context.Context, request func(context.Context) (json.RawMessage, error)) (json.RawMessage, error) {
	retryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var lastBusy error
	for {
		if err := retryCtx.Err(); err != nil {
			if lastBusy != nil {
				return nil, fmt.Errorf("reindex remained busy until retry deadline: %w (last response: %v)", err, lastBusy)
			}
			return nil, err
		}
		result, err := request(retryCtx)
		if err == nil || !isReindexAlreadyRunning(err) {
			return result, err
		}
		lastBusy = err
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-retryCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("reindex remained busy until retry deadline: %w (last response: %v)", retryCtx.Err(), lastBusy)
		case <-timer.C:
		}
	}
}

func isReindexAlreadyRunning(err error) bool {
	var responseErr *jsonrpc.ResponseError
	return errors.As(err, &responseErr) && responseErr.Code == jsonrpc.RequestFailed && responseErr.Message == "reindex already running"
}

func validateReindexGeneration(before, after uint64, files int) error {
	if after == 0 || after <= before {
		return fmt.Errorf("generation did not advance: before=%d after=%d", before, after)
	}
	if files < 100 {
		return fmt.Errorf("indexed only %d files, want at least 100", files)
	}
	return nil
}

func validateFinalStdioProgress(generation, lastCommitted uint64, snapshot, lastSnapshot int, enabled, fresh bool) error {
	if lastCommitted == 0 || generation != lastCommitted {
		return fmt.Errorf("final generation does not match the last successful commit: generation=%d last_committed=%d", generation, lastCommitted)
	}
	if !enabled || !fresh {
		return fmt.Errorf("last committed generation is not enabled and fresh: enabled=%t fresh=%t", enabled, fresh)
	}
	if snapshot == 0 || snapshot < lastSnapshot {
		return fmt.Errorf("document snapshot is missing or regressed: snapshot=%d last_snapshot=%d", snapshot, lastSnapshot)
	}
	return nil
}

func parseStdioSoakDuration(value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid SOAK_DURATION %q: %w", value, err)
	}
	switch duration {
	case 30 * time.Second, 10 * time.Minute, time.Hour:
		return duration, nil
	default:
		return 0, fmt.Errorf("SOAK_DURATION must select the 30s preflight, 10m preflight, or continuous 1h gate; got %q", value)
	}
}

func verifyStdioIndexContent(session *lspdriver.Session, docURI, text string) error {
	position := positionOf(text, "soakTarget", 2)
	result, err := requestStdio(session, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": docURI},
		"position":     position,
		"context":      map[string]any{"includeDeclaration": true},
	})
	if err != nil {
		return fmt.Errorf("persistent index references request: %w", err)
	}
	if err := validateStdioIndexContent(result, docURI, text); err != nil {
		return fmt.Errorf("persistent index content: %w", err)
	}
	return nil
}

func validateStdioIndexContent(result json.RawMessage, docURI, text string) error {
	return assertStdioQuery("textDocument/references", result, docURI, text)
}

func assertStdioOverlaySymbol(session *lspdriver.Session, docURI string) error {
	result, err := requestStdio(session, "textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": docURI}})
	if err != nil {
		return fmt.Errorf("documentSymbol for reopened dirty overlay: %w", err)
	}
	var symbols []struct {
		Name     string `json:"name"`
		Children []struct {
			Name string `json:"name"`
		} `json:"children"`
	}
	if err := json.Unmarshal(result, &symbols); err != nil {
		return fmt.Errorf("decode reopened overlay symbols: %w", err)
	}
	for _, symbol := range symbols {
		if symbol.Name == "soakOverlayOnly" {
			return nil
		}
		for _, child := range symbol.Children {
			if child.Name == "soakOverlayOnly" {
				return nil
			}
		}
	}
	return fmt.Errorf("reopened document no longer contains dirty overlay symbol soakOverlayOnly: %s", result)
}

func cancelStdioReindex(session *lspdriver.Session, startAt time.Time, latencies *latencyCollector) error {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	id, _, err := session.RequestIDContext(ctx, "omnilsp/reindex", map[string]any{})
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("reindex request did not enter cancellation path: id=%d err=%v", id, err)
	}
	terminalCtx, terminalCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer terminalCancel()
	terminal, err := session.WaitForResponse(terminalCtx, id)
	if err != nil {
		return fmt.Errorf("wait for terminal canceled reindex response %d: %w", id, err)
	}
	if terminal.Error == nil || terminal.Error.Code != jsonrpc.RequestCancelled {
		return fmt.Errorf("canceled reindex returned non-cancellation terminal response: %+v", terminal)
	}
	if latencies != nil {
		latencies.record(startAt, "omnilsp/reindex(cancel)", time.Since(started))
	}
	return nil
}

func cancelStdioReferenceQuery(session *lspdriver.Session, docURI, text string, ordinal uint64, startAt time.Time, latencies *latencyCollector) error {
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	workToken := fmt.Sprintf("soak-cancel-references-%d", ordinal)
	params := map[string]any{
		"textDocument":       map[string]any{"uri": docURI},
		"position":           positionOf(text, "soakTarget", 1),
		"context":            map[string]any{"includeDeclaration": false},
		"workDoneToken":      workToken,
		"partialResultToken": fmt.Sprintf("soak-cancel-references-partial-%d", ordinal),
	}
	id, _, err := session.RequestIDContext(ctx, "textDocument/references", params)
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("references request did not enter cancellation path: id=%d err=%v", id, err)
	}
	terminalCtx, terminalCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer terminalCancel()
	terminal, err := session.WaitForResponse(terminalCtx, id)
	if err != nil {
		return fmt.Errorf("wait for terminal canceled references response %d: %w", id, err)
	}
	if terminal.Error == nil || terminal.Error.Code != jsonrpc.RequestCancelled {
		return fmt.Errorf("canceled references request returned non-cancellation terminal response: %+v", terminal)
	}
	if latencies != nil {
		latencies.record(startAt, "textDocument/references(cancel)", time.Since(started))
	}
	return nil
}

func editStdioDocument(t testing.TB, session *lspdriver.Session, docURI, text string, version int) error {
	before, err := readStdioStatus(session)
	if err != nil {
		return err
	}
	session.Notify(t, "textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": docURI, "version": version},
		"contentChanges": []map[string]any{{"text": text}},
	})
	return waitForStdioSnapshot(session, before, 30*time.Second)
}

func startStdioWorkers(ctx context.Context, session *lspdriver.Session, queries []stdioQuery, completed *atomic.Uint64, latencies *latencyCollector, startAt time.Time) (<-chan stdioWorkerResult, <-chan int, []*atomic.Bool) {
	results := make(chan stdioWorkerResult, soakWorkers*2)
	done := make(chan int, soakWorkers)
	alive := make([]*atomic.Bool, soakWorkers)
	for worker := 0; worker < soakWorkers; worker++ {
		state := &atomic.Bool{}
		state.Store(true)
		alive[worker] = state
		query := queries[worker]
		go func(index int, aliveState *atomic.Bool, query stdioQuery) {
			defer aliveState.Store(false)
			defer func() { done <- index }()
			for ctx.Err() == nil {
				requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				requestStarted := time.Now()
				raw, err := session.RequestContext(requestCtx, query.method, query.params)
				elapsed := time.Since(requestStarted)
				cancel()
				if ctx.Err() != nil {
					return
				}
				latencies.record(startAt, query.method, elapsed)
				if err == nil {
					err = assertStdioQuery(query.method, raw, queryURI(query), stdioGoSource())
				}
				if err != nil {
					select {
					case results <- stdioWorkerResult{worker: index, method: query.method, latency: elapsed, err: err}:
					case <-ctx.Done():
					}
					return
				}
				completed.Add(1)
				select {
				case results <- stdioWorkerResult{worker: index, method: query.method, latency: elapsed}:
				case <-ctx.Done():
					return
				}
			}
		}(worker, state, query)
	}
	return results, done, alive
}

func waitStdioWorkers(done <-chan int) {
	for i := 0; i < soakWorkers; i++ {
		<-done
	}
}

func verifyStdioWorkersAlive(alive []*atomic.Bool) error {
	if len(alive) != soakWorkers {
		return fmt.Errorf("tracked %d workers, want %d", len(alive), soakWorkers)
	}
	for i, state := range alive {
		if state == nil || !state.Load() {
			return fmt.Errorf("semantic worker %d is not alive", i)
		}
	}
	return nil
}

func collectStdioTelemetry(session *lspdriver.Session, job *processTreeBudget, candidatePID int, alive []*atomic.Bool, requests, edits uint64, active time.Duration, now time.Time, candidateHash, workerHash string) (stdioTelemetry, error) {
	status, err := readStdioStatus(session)
	if err != nil {
		return stdioTelemetry{}, err
	}
	indexRaw, err := requestStdio(session, "omnilsp/indexStats", map[string]any{})
	if err != nil {
		return stdioTelemetry{}, err
	}
	var index map[string]any
	if err := json.Unmarshal(indexRaw, &index); err != nil {
		return stdioTelemetry{}, err
	}
	backends := make(map[string]bool)
	for _, language := range []string{"go", "cpp", "rust", "python", "typescript"} {
		raw, requestErr := requestStdio(session, "omnilsp/backendStatus", map[string]any{"language": language})
		if requestErr != nil {
			return stdioTelemetry{}, fmt.Errorf("backendStatus %s: %w", language, requestErr)
		}
		var backend struct{ Found bool }
		if err := json.Unmarshal(raw, &backend); err != nil {
			return stdioTelemetry{}, err
		}
		backends[language] = backend.Found
	}
	tree, err := job.sample(os.Getpid(), candidatePID)
	if err != nil {
		return stdioTelemetry{}, err
	}
	workerSnapshot := make([]bool, len(alive))
	for i, state := range alive {
		workerSnapshot[i] = state != nil && state.Load()
	}
	progressKinds := progressEventCounts(session)
	progress := 0
	for _, count := range progressKinds {
		progress += int(count)
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return stdioTelemetry{UTC: now.UTC(), ActiveElapsed: active, Requests: requests, Edits: edits, WorkersAlive: workerSnapshot, Tree: tree, Server: status, Index: index, Backends: backends, ProgressEvents: progress, ProgressKinds: progressKinds, SnapshotEpochs: number(status["SnapshotEpochs"]), IndexGeneration: uint64(number(index["generation"])), CandidateHash: candidateHash, WorkerHash: workerHash, Seed: soakSeed,
		GoGoroutines: runtime.NumGoroutine(), GoHeapAlloc: mem.HeapAlloc}, nil
}

func progressEventCounts(session *lspdriver.Session) map[string]uint64 {
	counts := map[string]uint64{"begin": 0, "report": 0, "end": 0, "unknown": 0}
	for _, event := range session.Events() {
		if event.Method != "$/progress" {
			continue
		}
		var params struct {
			Value struct {
				Kind string `json:"kind"`
			} `json:"value"`
		}
		if err := json.Unmarshal(event.Params, &params); err != nil {
			counts["unknown"]++
			continue
		}
		if _, ok := counts[params.Value.Kind]; ok {
			counts[params.Value.Kind]++
		} else {
			counts["unknown"]++
		}
	}
	return counts
}

func waitForProgressLifecycle(session *lspdriver.Session, token string, timeout time.Duration) (progressLifecycleCounts, error) {
	deadline := time.Now().Add(timeout)
	counts := progressLifecycleCounts{}
	for time.Now().Before(deadline) {
		counts = countProgressLifecycle(session.Events(), token)
		if counts.Begin > 0 && counts.End > 0 {
			return counts, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return counts, fmt.Errorf("progress token %q did not produce both begin/end events: %+v", token, counts)
}

func countProgressLifecycle(events []*jsonrpc.Message, token string) progressLifecycleCounts {
	counts, _ := progressLifecycleInEvents(events, token)
	return counts
}

func progressLifecycleInEvents(events []*jsonrpc.Message, token string) (progressLifecycleCounts, error) {
	counts := progressLifecycleCounts{}
	beginAt, endAt := -1, -1
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
		if json.Unmarshal(event.Params, &params) != nil || params.Token != token {
			continue
		}
		switch params.Value.Kind {
		case "begin":
			counts.Begin++
			if beginAt < 0 {
				beginAt = index
			}
		case "report":
			counts.Report++
		case "end":
			counts.End++
			endAt = index
		}
	}
	if beginAt < 0 || endAt < 0 || endAt <= beginAt {
		return counts, fmt.Errorf("progress token %q did not produce an ordered begin/end lifecycle: %+v", token, counts)
	}
	return counts, nil
}

func progressLifecycleBeforeResponse(events []*jsonrpc.Message, token string) (progressLifecycleCounts, error) {
	counts, err := progressLifecycleInEvents(events, token)
	if err != nil {
		return counts, err
	}
	endAt, responseAt := -1, -1
	for index, event := range events {
		if event.Method == "$/progress" {
			var params struct {
				Token string `json:"token"`
				Value struct {
					Kind string `json:"kind"`
				} `json:"value"`
			}
			if json.Unmarshal(event.Params, &params) == nil && params.Token == token && params.Value.Kind == "end" {
				endAt = index
			}
		}
		// The initial semantic query set is sequential, so the first terminal
		// response after event capture belongs to the request under observation.
		if responseAt < 0 && event.IsResponse() {
			responseAt = index
		}
	}
	if responseAt < 0 {
		return counts, fmt.Errorf("request for progress token %q had no terminal response in the event stream", token)
	}
	if endAt < 0 || endAt >= responseAt {
		return counts, fmt.Errorf("progress token %q did not end before its terminal response (end index=%d response index=%d)", token, endAt, responseAt)
	}
	return counts, nil
}

func TestProgressLifecycleMustEndBeforeTerminalResponse(t *testing.T) {
	progress := func(kind string) *jsonrpc.Message {
		params, err := json.Marshal(map[string]any{"token": "test-token", "value": map[string]any{"kind": kind}})
		if err != nil {
			t.Fatal(err)
		}
		return jsonrpc.NewNotification("$/progress", params)
	}
	response := jsonrpc.NewResponse(jsonrpc.RequestID{Num: 1}, json.RawMessage(`[]`))
	valid := []*jsonrpc.Message{progress("begin"), progress("report"), progress("end"), response}
	if _, err := progressLifecycleBeforeResponse(valid, "test-token"); err != nil {
		t.Fatalf("ordered lifecycle rejected: %v", err)
	}
	lateEnd := []*jsonrpc.Message{progress("begin"), response, progress("end")}
	if _, err := progressLifecycleBeforeResponse(lateEnd, "test-token"); err == nil {
		t.Fatal("lifecycle ending after terminal response was accepted")
	}
}

func processTreeHandleTotal(sample processTreeSample) uint64 {
	var total uint64
	for _, count := range sample.HandleCounts {
		total += count
	}
	return total
}

func medianUint(values []uint64) uint64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]uint64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted[len(sorted)/2]
}

func summarizeResourceWindow(minute int64, private, handles []uint64) resourceWindow {
	window := resourceWindow{Minute: minute, SampleCount: len(private)}
	if len(private) == 0 || len(handles) == 0 {
		return window
	}
	window.FirstPrivate, window.LastPrivate = private[0], private[len(private)-1]
	window.MedianPrivate = medianUint(private)
	window.MinPrivate, window.MaxPrivate = private[0], private[0]
	for _, value := range private[1:] {
		if value < window.MinPrivate {
			window.MinPrivate = value
		}
		if value > window.MaxPrivate {
			window.MaxPrivate = value
		}
	}
	window.FirstHandles, window.LastHandles = handles[0], handles[len(handles)-1]
	window.MedianHandles = medianUint(handles)
	window.MinHandles, window.MaxHandles = handles[0], handles[0]
	for _, value := range handles[1:] {
		if value < window.MinHandles {
			window.MinHandles = value
		}
		if value > window.MaxHandles {
			window.MaxHandles = value
		}
	}
	return window
}

type resourceTrendSummary struct {
	firstPrivate, lastPrivate  uint64
	firstHandles, lastHandles  uint64
	privateSlopeBytesPerMinute float64
	handlesSlopePerMinute      float64
	privateEstimatedGrowth     float64
	handlesEstimatedGrowth     float64
}

func resourceTrend(windows []resourceWindow) resourceTrendSummary {
	trend := resourceTrendSummary{}
	if len(windows) == 0 {
		return trend
	}
	trend.firstPrivate, trend.lastPrivate = windows[0].MedianPrivate, windows[len(windows)-1].MedianPrivate
	trend.firstHandles, trend.lastHandles = windows[0].MedianHandles, windows[len(windows)-1].MedianHandles
	trend.privateSlopeBytesPerMinute = theilSenSlope(windows, func(window resourceWindow) uint64 { return window.MedianPrivate })
	trend.handlesSlopePerMinute = theilSenSlope(windows, func(window resourceWindow) uint64 { return window.MedianHandles })
	spanMinutes := float64(windows[len(windows)-1].Minute - windows[0].Minute)
	if spanMinutes > 0 {
		trend.privateEstimatedGrowth = trend.privateSlopeBytesPerMinute * spanMinutes
		trend.handlesEstimatedGrowth = trend.handlesSlopePerMinute * spanMinutes
	}
	return trend
}

func theilSenSlope(windows []resourceWindow, value func(resourceWindow) uint64) float64 {
	slopes := make([]float64, 0, len(windows)*(len(windows)-1)/2)
	for i := 0; i < len(windows); i++ {
		for j := i + 1; j < len(windows); j++ {
			elapsed := windows[j].Minute - windows[i].Minute
			if elapsed <= 0 {
				continue
			}
			delta := float64(value(windows[j])) - float64(value(windows[i]))
			slopes = append(slopes, delta/float64(elapsed))
		}
	}
	if len(slopes) == 0 {
		return 0
	}
	sort.Float64s(slopes)
	middle := len(slopes) / 2
	if len(slopes)%2 == 1 {
		return slopes[middle]
	}
	return (slopes[middle-1] + slopes[middle]) / 2
}

func resourceTrendExceeded(trend resourceTrendSummary) bool {
	return trend.privateEstimatedGrowth >= float64(resourcePrivateGrowthLimit) ||
		trend.handlesEstimatedGrowth >= float64(resourceHandleGrowthLimit)
}

func signedDelta(last, first uint64) int64 {
	if last >= first {
		return int64(last - first)
	}
	return -int64(first - last)
}

func lastResourceWindow(windows []resourceWindow) any {
	if len(windows) == 0 {
		return nil
	}
	return windows[len(windows)-1]
}

func firstResourceWindow(windows []resourceWindow) any {
	if len(windows) == 0 {
		return nil
	}
	return windows[0]
}

func sampleSet(id string, values []float64) report.SampleSet {
	return sampleSetWithUnit(id, "bytes", values)
}

func lastPeak(values []float64) float64 {
	var peak float64
	for _, value := range values {
		if value > peak {
			peak = value
		}
	}
	return peak
}

func sampleSetWithUnit(id, unit string, values []float64) report.SampleSet {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return report.SampleSet{ID: id, Unit: unit, Raw: append([]float64(nil), values...), P50: percentile(sorted, 0.50), P95: percentile(sorted, 0.95), P99: percentile(sorted, 0.99), SampledAt: time.Now().UTC()}
}

func requestStdio(session *lspdriver.Session, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return session.RequestContext(ctx, method, params)
}

func waitForStdioRuntimeBounds(baselineGoroutines int, baselineHeap uint64) (int, uint64) {
	deadline := time.Now().Add(10 * time.Second)
	var goroutines int
	var heap uint64
	for {
		runtime.GC()
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		goroutines, heap = runtime.NumGoroutine(), mem.HeapAlloc
		if goroutines-baselineGoroutines <= 50 && heap < baselineHeap+(256<<20) {
			return goroutines, heap
		}
		if time.Now().After(deadline) {
			return goroutines, heap
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted)-1) * q)
	return sorted[index]
}

func (c *latencyCollector) record(startAt time.Time, method string, elapsed time.Duration) {
	window := int64(time.Since(startAt) / time.Minute)
	if window < 0 {
		window = 0
	}
	value := float64(elapsed) / float64(time.Millisecond)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.buckets[window] == nil {
		c.buckets[window] = make(map[string]*latencyBucket)
	}
	bucket := c.buckets[window][method]
	if bucket == nil {
		bucket = &latencyBucket{}
		c.buckets[window][method] = bucket
	}
	bucket.count++
	const sampleLimit = 256
	if len(bucket.values) < sampleLimit {
		bucket.values = append(bucket.values, value)
		return
	}
	// Deterministic reservoir replacement keeps per-minute raw data bounded
	// while allowing late-window requests to contribute to the percentiles.
	index := (bucket.count*11400714819323198485 + 14029467366897019727) % bucket.count
	if index < sampleLimit {
		bucket.values[index] = value
	}
}

func (c *latencyCollector) take(window int64) map[string]latencySummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	byMethod := c.buckets[window]
	delete(c.buckets, window)
	result := make(map[string]latencySummary, len(byMethod))
	for method, bucket := range byMethod {
		sorted := append([]float64(nil), bucket.values...)
		sort.Float64s(sorted)
		result[method] = latencySummary{
			Count: bucket.count, SampleCount: len(bucket.values), Truncated: bucket.count > uint64(len(bucket.values)),
			RawMS: append([]float64(nil), bucket.values...),
			P50MS: percentile(sorted, 0.50), P95MS: percentile(sorted, 0.95), P99MS: percentile(sorted, 0.99),
		}
	}
	return result
}

func cloneCounts(source map[string]uint64) map[string]uint64 {
	copy := make(map[string]uint64, len(source))
	for name, count := range source {
		copy[name] = count
	}
	return copy
}

func number(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case uint64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func soakFailed(evidence *report.Report, id, message string) {
	evidence.Checks = append(evidence.Checks, report.Check{ID: id, Status: report.Failed, Summary: message})
	evidence.Errors = append(evidence.Errors, message)
}

func soakNotVerified(evidence *report.Report, id, message string) {
	evidence.Checks = append(evidence.Checks, report.Check{ID: id, Status: report.NotVerified, Summary: message})
	evidence.Skips = append(evidence.Skips, report.Skip{ID: id, Reason: message})
}
