package soak

// This file is deliberately untagged: the stdio soak's pure gate predicates,
// budget derivations, and query fixtures are cheap deterministic checks, so
// they and their form tests run in the default suite per the 2026-10-25
// ruling 3. The real-stdio workload that drives them over a live server stays
// behind the soak build tag.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

const (
	stdioGeneratedFiles = 4096

	// The candidate-restart phase's explicit waits. They are named here because
	// the maintenance-pause budget is derived from the same values; the budget
	// must not drift from the work it is supposed to cover.
	soakSnapshotOpenBudget      = 10 * time.Second
	soakSnapshotReopenBudget    = 30 * time.Second
	soakProgressLifecycleBudget = 5 * time.Second

	// stdioRequestBudgetEnv widens the per-request stdio budget for local
	// diagnostic runs (see stdioRequestBudget). It is not a soak-gate
	// precondition: strict gate semantics do not depend on it.
	stdioRequestBudgetEnv = "OMNILSP_SOAK_REQUEST_BUDGET"
)

// processTreeMember and processTreeSample are the platform-independent shapes
// of the process-tree resource sampler. The samplers themselves (Windows Job
// Object accounting and the refusing stub) stay in the tagged platform files.
type processTreeMember struct {
	PID          uint32 `json:"pid"`
	ParentPID    uint32 `json:"parent_pid"`
	Role         string `json:"role"`
	ImagePath    string `json:"image_path"`
	PrivateBytes uint64 `json:"private_bytes"`
	Handles      uint32 `json:"handles"`
}

type processTreeSample struct {
	PrivateBytes       uint64              `json:"private_bytes"`
	PeakJobCommitBytes uint64              `json:"peak_job_commit_bytes"`
	JobMemoryLimit     uint64              `json:"job_memory_limit_bytes"`
	ActiveProcesses    uint32              `json:"active_processes"`
	HandleCounts       map[string]uint64   `json:"handles_by_role"`
	Members            []processTreeMember `json:"members"`
}

type stdioQuery struct {
	method string
	params map[string]any
}

type progressLifecycleCounts struct {
	Begin  uint64 `json:"begin"`
	Report uint64 `json:"report"`
	End    uint64 `json:"end"`
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

func stdioGoSource() string {
	return "package soak\n\nfunc soakTarget(value int) int { return value + 1 }\nfunc useTarget() int { return soakTarget(1) }\n"
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
	// The retry window is the same per-request allowance requestStdio uses.
	// The fixed 10s this line previously carried was proven insufficient for
	// the pinned rust-analyzer 1.97.0 cold start under full-corpus load
	// (gate-20261025T0716Z: soak/reindex died on it), so it must not survive
	// as a floor: max(budget, 10s) would keep a proven-bad bound and defeat
	// deliberately small diagnostic budgets. stdioRequestBudget() widens the
	// window with OMNILSP_SOAK_REQUEST_BUDGET (default 30s).
	retryCtx, cancel := context.WithTimeout(ctx, stdioRequestBudget())
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

func validateStdioIndexContent(result json.RawMessage, docURI, text string) error {
	return assertStdioQuery("textDocument/references", result, docURI, text)
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

// minuteCadenceBudget is the unexplained part of a minute window: one minute
// plus two poll periods of scheduling slack.
func minuteCadenceBudget() time.Duration { return time.Minute + 2*soakPollPeriod }

// minuteWindowGapContinuous separates the nominal cadence from recorded
// maintenance. The raw active gap must still reach a minute (a row is due one
// minute after the previous one), while a bounded maintenance interval is
// subtracted from both raw gaps before the unexplained-stall ceiling applies.
// A pause larger than either gap, or a negative pause, is rejected.
func minuteWindowGapContinuous(wallGap, activeGap, maintenancePause time.Duration) bool {
	if wallGap <= 0 || activeGap < time.Minute {
		return false
	}
	if maintenancePause < 0 || maintenancePause > wallGap || maintenancePause > activeGap {
		return false
	}
	budget := minuteCadenceBudget()
	return wallGap-maintenancePause <= budget && activeGap-maintenancePause <= budget
}

// soakMaintenanceBudget bounds the known main-loop pauses. The reindex budget
// is the request budget it runs under; the restart budget is the rust
// cold-start allowance plus the phase's own explicit waits. Each pause is
// checked at record time, so a recorded pause can never be unbounded.
func soakMaintenanceBudget(kind string) time.Duration {
	switch kind {
	case "periodic-reindex":
		return stdioRequestBudget() + 2*soakPollPeriod
	case "candidate-restart":
		return stdioColdStartAllowance(90*time.Second) + soakSnapshotOpenBudget + soakSnapshotReopenBudget + soakProgressLifecycleBudget + 2*soakPollPeriod
	default:
		return 0
	}
}

// soakRustStabilityProbeBudget uses the same per-request stdio budget as every
// other request. A fixed 3s probe was too tight while the replacement
// re-indexes under the eight-worker load; identity is re-sampled after the
// probe, so a longer budget cannot hide a backend restart.
func soakRustStabilityProbeBudget() time.Duration {
	return stdioRequestBudget()
}

// stdioRequestBudget is the wall-clock budget every requestStdio caller gets.
// The default stays at the historical hard-coded 30s; §S16 fixes no
// per-request timeout, so this is harness plumbing, not a soak-gate value.
// OMNILSP_SOAK_REQUEST_BUDGET exists so a diagnostic run can widen it — the
// Y1-10 GATE (internal/conformance/registry.go) records this host's 30s
// preflights dying on the fixed budget — without touching the strict gates.
// A malformed or non-positive value must not silently mimic a widened run: it
// falls back to the default, announced on stderr.
func stdioRequestBudget() time.Duration {
	const defaultBudget = 30 * time.Second
	text := strings.TrimSpace(os.Getenv(stdioRequestBudgetEnv))
	if text == "" {
		return defaultBudget
	}
	budget, err := time.ParseDuration(text)
	if err != nil || budget <= 0 {
		fmt.Fprintf(os.Stderr, "soak: ignoring invalid %s=%q (want a positive duration like 90s); using default %s\n", stdioRequestBudgetEnv, text, defaultBudget)
		return defaultBudget
	}
	return budget
}

// stdioColdStartAllowance sizes a soak phase that must outlast the locked
// rust-analyzer cold start (e.g. the first semantic rust hover). The 300s
// standard budget was established against rust-analyzer 1.97.0 and remains a
// floor if the host lock moves.
// OMNILSP_SOAK_REQUEST_BUDGET may widen the phase but never tightens it below
// the historical sizing: a small diagnostic budget must not shrink an
// allowance whose fit at smaller values has never been observed. Phases whose
// window does not span cold start (e.g. cancellation acknowledgments) stay
// fixed constants instead.
func stdioColdStartAllowance(historical time.Duration) time.Duration {
	if budget := stdioRequestBudget(); budget > historical {
		return budget
	}
	return historical
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

func sameExecutablePath(left, right string) bool {
	clean := func(value string) string {
		value = strings.TrimPrefix(filepath.Clean(value), `\\?\`)
		return strings.ToLower(value)
	}
	return clean(left) == clean(right)
}
