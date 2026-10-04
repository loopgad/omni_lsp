package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/interop"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/omnilsp/omni/internal/replay"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/test/acceptance/report"
	"github.com/omnilsp/omni/test/acceptance/upstream"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

type persistentAcceptanceIndexStats struct {
	Enabled          bool   `json:"enabled"`
	Generation       uint64 `json:"generation"`
	Fresh            bool   `json:"fresh"`
	SemanticStatus   string `json:"semanticStatus"`
	SemanticCoverage []struct {
		ScopeID string `json:"scopeId"`
		Fact    string `json:"fact"`
		State   string `json:"state"`
	} `json:"semanticCoverage"`
}

type persistentAcceptanceBackendStatus struct {
	Language string `json:"Language"`
	Found    bool   `json:"Found"`
}

type persistentAcceptanceMeta struct {
	Method   string `json:"method"`
	Evidence []struct {
		Kind         uint8  `json:"Kind"`
		IndexGen     uint64 `json:"IndexGen"`
		BuildContext string `json:"BuildContext"`
	} `json:"evidence"`
}

type persistentAcceptanceWorkspaceSymbol struct {
	Name     string `json:"name"`
	Location struct {
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
	} `json:"location"`
}

// TestS21CandidatePersistentQueriesSurviveDisabledBackend keeps the original
// TypeScript candidate regression.
func TestS21CandidatePersistentQueriesSurviveDisabledBackend(t *testing.T) {
	runPersistentSemanticAcceptance(t, "typescript")
}

func TestS21GoCandidatePersistentQueriesSurviveDisabledBackend(t *testing.T) {
	runPersistentSemanticAcceptance(t, "go")
}

func TestS21PythonCandidatePersistentQueriesSurviveDisabledBackend(t *testing.T) {
	runPersistentSemanticAcceptance(t, "python")
}

func TestS21JavaScriptCandidatePersistentQueriesSurviveDisabledBackend(t *testing.T) {
	runPersistentSemanticAcceptance(t, "javascript")
}

// runPersistentSemanticAcceptance exercises one language candidate across two
// processes. The second server must answer from the committed generation with
// that language's live backend disabled, under the same tool environment.
func runPersistentSemanticAcceptance(t *testing.T, language string) {
	if os.Getenv(semanticAcceptanceModeEnv) != "1" {
		t.Skip("not run: set OMNILSP_ACCEPTANCE=1 to enable real-process persistent-index acceptance")
	}

	root, err := semanticRepoRoot()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	t.Setenv("GOCACHE", filepath.Join(root, ".tmp-gocache"))
	t.Setenv("GOTELEMETRY", "off")

	runID := strings.TrimSpace(os.Getenv(semanticRunIDEnv))
	if runID == "" {
		runID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	evidence := report.New(runID)
	evidence.Environment["acceptanceMode"] = "explicit"
	evidence.Environment["candidateProcess"] = "real stdio serve; reindex then restart with disabled backend"
	evidence.Environment["fixtureLanguage"] = language
	if language == "go" {
		evidence.Environment["phase2ToolEnvironment"] = "same Go tool path, GOCACHE, and GOENV=off as phase 1; backend disabled by serve config"
	} else if language == "python" {
		evidence.Environment["phase2ToolEnvironment"] = "same PATH and pinned Node/Pyright/Python configuration as phase 1; backend disabled by serve config"
	} else if language == "c" || language == "cpp" {
		evidence.Environment["phase2ToolEnvironment"] = "same pinned compiler, libclang and compilation database as phase 1; backend disabled by serve config"
	} else {
		evidence.Environment["phase2ToolEnvironment"] = "same PATH and pinned Node/TypeScript configuration as phase 1; backend disabled by serve config"
	}
	evidence.Limits["persistentQueries"] = "workspace/symbol, textDocument/definition, textDocument/references must carry the committed IndexGen"
	checkID := runID + "/S21/persistent-index-only-queries"
	if language == "go" {
		checkID = runID + "/S21/go-persistent-index-only-queries"
	} else if language != "typescript" {
		checkID = runID + "/S21/" + language + "-persistent-index-only-queries"
	}
	check := report.Check{ID: checkID, Status: report.NotVerified}
	defer func() {
		if check.ID != "" {
			evidence.Checks = append(evidence.Checks, check)
		}
		evidence.Finalize(time.Now())
		path := strings.TrimSpace(os.Getenv(semanticReportPathEnv))
		if path == "" {
			path = filepath.Join(os.TempDir(), "omnilsp-persistent-"+strings.ReplaceAll(runID, "/", "-")+".json")
			if language != "typescript" {
				path = strings.TrimSuffix(path, ".json") + "-" + language + ".json"
			}
		} else if language != "typescript" {
			extension := filepath.Ext(path)
			path = strings.TrimSuffix(path, extension) + "-" + language + extension
		}
		if err := report.Write(path, evidence); err != nil {
			t.Errorf("write persistent semantic acceptance report: %v", err)
			return
		}
		t.Logf("persistent semantic acceptance JSON report: %s (decision=%s)", path, evidence.Decision)
	}()

	// Missing prerequisites are unverified; an executed candidate failure is failed.
	executing := false
	block := func(reason string, observed map[string]any) {
		check.Status = report.NotVerified
		if executing {
			check.Status = report.Failed
		}
		check.Summary = reason
		check.Observed = observed
		t.Errorf("S21 persistent semantic acceptance %s: %s", check.Status, reason)
	}

	binary, err := semanticResolveBinary(t)
	if err != nil {
		block("candidate unavailable: "+err.Error(), nil)
		return
	}
	binaryData, err := os.ReadFile(binary)
	if err != nil {
		block("candidate unreadable: "+err.Error(), map[string]any{"binary": binary})
		return
	}
	digest := sha256.Sum256(binaryData)
	evidence.Candidate = report.Candidate{
		Binary: binary, SHA256: "sha256:" + hex.EncodeToString(digest[:]),
		Revision: semanticGitRevision(t),
	}

	workspace := t.TempDir()
	indexDir := filepath.Join(t.TempDir(), "verified-index")
	fixture, targetURI, useURI, callLine, callCharacter := newPersistentSemanticFixture(t, workspace, language)
	setPersistentFixtureEvidence(&evidence, fixture)
	wantDefinition := fixture.definition
	wantCall := semanticLocation{
		URI: useURI, StartLine: callLine, StartChar: callCharacter,
		EndLine: callLine, EndChar: callCharacter + uint32(len(fixture.symbol)),
	}

	cfg := config.Default()
	cfg.LogLevel = "error"
	cfg.Transport = "stdio"
	cfg.WorkspaceDir = workspace
	cfg.IndexDir = indexDir
	cfg.MaxConcurrentRequests = 8
	cfg.MaxQueueSize = 64
	backendLanguage := fixture.language
	if backendLanguage == "c" {
		backendLanguage = "cpp"
	} else if backendLanguage == "javascript" {
		backendLanguage = "typescript"
	}
	evidence.Environment["backendLanguage"] = backendLanguage
	cfg.Backends = persistentSemanticBackendConfigs(backendLanguage, true)
	phase1ConfigPath := filepath.Join(t.TempDir(), fixture.language+"-only.json")
	phase1ConfigBytes, err := json.Marshal(cfg)
	if err != nil {
		block("encode phase 1 config: "+err.Error(), nil)
		return
	}
	if err := os.WriteFile(phase1ConfigPath, append(phase1ConfigBytes, '\n'), 0o600); err != nil {
		block("write phase 1 config: "+err.Error(), nil)
		return
	}
	baseEnv := []string{
		"OMNILSP_TRUST=trusted", "OMNILSP_LOG_LEVEL=error", "OMNILSP_MAX_CONCURRENT=8",
		"OMNILSP_MAX_QUEUE=64", "GOMAXPROCS=8", "GOCACHE=" + filepath.Join(root, ".tmp-gocache"),
		"GOTELEMETRY=off", "GOTOOLCHAIN=local", "OMNILSP_INDEX_DIR=" + indexDir,
		"PATH=" + semanticAcceptanceToolPath(),
	}
	if fixture.language == "go" {
		baseEnv = append(baseEnv, "GOENV=off", "GOFLAGS=", "GOWORK=off", "GOPROXY=off", "GOSUMDB=off")
	} else {
		lockedEnv, err := persistentAcceptanceToolEnvFor(root, fixture.language)
		if err != nil {
			block("pinned extraction inputs unavailable: "+err.Error(), nil)
			return
		}
		baseEnv = append(baseEnv, lockedEnv...)
	}
	executing = true
	first, err := upstream.Start(binary, []string{"serve", "--config", phase1ConfigPath, "--transport", "stdio", "--workspace", workspace}, workspace, baseEnv)
	if err != nil {
		block("phase 1 server start failed: "+err.Error(), nil)
		return
	}
	firstCtx, firstCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := first.Initialize(firstCtx, workspace); err != nil {
		firstCancel()
		closeErr := first.Close()
		block("phase 1 initialize failed: "+err.Error()+"; process close: "+errorString(closeErr), nil)
		return
	}
	phase1BackendRaw, phase1BackendErr := first.RequestContext(firstCtx, "omnilsp/backendStatus", map[string]any{"language": backendLanguage})
	var phase1Backend persistentAcceptanceBackendStatus
	if phase1BackendErr == nil {
		phase1BackendErr = json.Unmarshal(phase1BackendRaw, &phase1Backend)
	}
	if phase1BackendErr != nil || !phase1Backend.Found {
		firstCancel()
		closeErr := first.Close()
		block("phase 1 "+fixture.language+" backend is not confirmed enabled: "+errorString(phase1BackendErr)+"; process close: "+errorString(closeErr), map[string]any{"backendStatus": string(phase1BackendRaw)})
		return
	}
	reindexRaw, reindexErr := first.RequestContext(firstCtx, "omnilsp/reindex", map[string]any{})
	phase1StatsRaw, phase1StatsErr := first.RequestContext(firstCtx, "omnilsp/indexStats", map[string]any{})
	firstCancel()
	firstCloseErr := first.Close()
	if firstCloseErr != nil {
		block("phase 1 server exited unsuccessfully: "+firstCloseErr.Error(), map[string]any{"reindexResponse": string(reindexRaw), "indexStats": string(phase1StatsRaw)})
		return
	}
	if reindexErr != nil {
		block("phase 1 real candidate reindex failed: "+reindexErr.Error(), map[string]any{"reindexResponse": string(reindexRaw)})
		return
	}
	var indexed struct {
		Generation uint64 `json:"generation"`
	}
	if err := json.Unmarshal(reindexRaw, &indexed); err != nil || indexed.Generation == 0 {
		block("phase 1 reindex did not return a committed generation", map[string]any{"reindexResponse": string(reindexRaw), "decodeError": errorString(err)})
		return
	}
	phase1Stats, phase1StatsRaw, err := persistentAcceptanceIndexStatsFrom(phase1StatsRaw, phase1StatsErr)
	if err != nil {
		block("phase 1 indexStats unavailable: "+err.Error(), map[string]any{"raw": string(phase1StatsRaw)})
		return
	}
	evidence.Environment["phase1Generation"] = fmt.Sprint(indexed.Generation)
	if phase1Stats.Generation != indexed.Generation || !phase1Stats.Fresh || !phase1Stats.Enabled {
		block("phase 1 generation is not fresh/enabled or differs from reindex response", map[string]any{
			"reindexGeneration": indexed.Generation, "indexStats": phase1Stats,
		})
		return
	}
	var semanticScopeIDs []string
	seenSemanticScopes := make(map[string]struct{})
	for _, coverage := range phase1Stats.SemanticCoverage {
		if strings.HasPrefix(coverage.ScopeID, fixture.scopeIDPrefix) {
			if _, exists := seenSemanticScopes[coverage.ScopeID]; !exists {
				seenSemanticScopes[coverage.ScopeID] = struct{}{}
				semanticScopeIDs = append(semanticScopeIDs, coverage.ScopeID)
			}
		}
	}
	if len(semanticScopeIDs) != 1 {
		block("phase 1 fresh generation does not have exactly one "+fixture.language+" semantic scope", map[string]any{"indexStats": phase1Stats, "semanticScopeIDs": semanticScopeIDs, "startupDiagnostics": first.Stderr()})
		return
	}
	evidence.Environment["phase1SemanticScopes"] = strings.Join(semanticScopeIDs, ",")
	if fixture.language == "typescript" {
		evidence.Environment["phase1TypeScriptScopes"] = strings.Join(semanticScopeIDs, ",")
	} else if fixture.language == "go" {
		evidence.Environment["phase1GoScopes"] = strings.Join(semanticScopeIDs, ",")
	}

	cfg.Backends = persistentSemanticBackendConfigs(backendLanguage, false)
	configPath := filepath.Join(t.TempDir(), "disabled-backends.json")
	configBytes, err := json.Marshal(cfg)
	if err != nil {
		block("encode phase 2 config: "+err.Error(), nil)
		return
	}
	if err := os.WriteFile(configPath, append(configBytes, '\n'), 0o600); err != nil {
		block("write phase 2 config: "+err.Error(), nil)
		return
	}
	second, err := upstream.Start(binary, []string{"serve", "--config", configPath, "--transport", "stdio", "--workspace", workspace}, workspace, baseEnv)
	if err != nil {
		block("phase 2 server start failed: "+err.Error(), nil)
		return
	}
	secondClosed := false
	defer func() {
		if secondClosed {
			return
		}
		if closeErr := second.Close(); closeErr != nil {
			block("phase 2 server exited unsuccessfully: "+closeErr.Error(), nil)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := second.Initialize(ctx, workspace); err != nil {
		block("phase 2 initialize failed: "+err.Error(), nil)
		return
	}
	backendRaw, backendErr := second.RequestContext(ctx, "omnilsp/backendStatus", map[string]any{"language": backendLanguage})
	var backend persistentAcceptanceBackendStatus
	if backendErr == nil {
		backendErr = json.Unmarshal(backendRaw, &backend)
	}
	if backendErr != nil || backend.Found {
		block("phase 2 "+fixture.language+" backend is not confirmed disabled", map[string]any{"backendStatus": string(backendRaw), "error": errorString(backendErr)})
		return
	}
	beforeRaw, beforeErr := second.RequestContext(ctx, "omnilsp/indexStats", map[string]any{})
	var before persistentAcceptanceIndexStats
	if beforeErr == nil {
		beforeErr = json.Unmarshal(beforeRaw, &before)
	}
	if beforeErr != nil || !before.Fresh || before.Generation != indexed.Generation {
		block("phase 2 did not reopen the fresh generation from phase 1", map[string]any{"indexStats": string(beforeRaw), "error": errorString(beforeErr), "expectedGeneration": indexed.Generation})
		return
	}
	traceBeforeRaw, traceBeforeErr := second.RequestContext(ctx, "omnilsp/queryTrace", map[string]any{})
	var traceBefore semanticQueryTrace
	if traceBeforeErr == nil {
		traceBeforeErr = json.Unmarshal(traceBeforeRaw, &traceBefore)
	}
	if traceBeforeErr != nil {
		block("phase 2 queryTrace unavailable before lookups: "+traceBeforeErr.Error(), map[string]any{"queryTrace": string(traceBeforeRaw)})
		return
	}

	observed := map[string]any{
		"generation":        indexed.Generation,
		"backendFound":      backend.Found,
		"plannerProbeCount": "not exposed by candidate; recorded as unobservable",
		"queryTraceBefore":  traceBefore,
	}
	var issues []string
	checkMeta := func(method, documentURI string) {
		params := map[string]any{"method": method}
		if documentURI != "" {
			params["uri"] = documentURI
		}
		raw, err := second.RequestContext(ctx, "omnilsp/resultMeta", params)
		if err != nil {
			issues = append(issues, method+" resultMeta: "+err.Error())
			return
		}
		var entries []persistentAcceptanceMeta
		if err := json.Unmarshal(raw, &entries); err != nil {
			issues = append(issues, method+" resultMeta decode: "+err.Error())
			return
		}
		valid := false
		for _, entry := range entries {
			if entry.Method != method {
				continue
			}
			for _, ev := range entry.Evidence {
				if ev.Kind == uint8(identity.EvidenceIndex) && ev.IndexGen == indexed.Generation && ev.BuildContext != "" {
					valid = true
				}
			}
		}
		observed["resultMeta."+method] = rawMessageAsAny(raw)
		if !valid {
			issues = append(issues, method+" did not carry index evidence for the phase 1 generation")
		}
	}

	workspaceRaw, workspaceErr := second.RequestContext(ctx, "workspace/symbol", map[string]any{"query": fixture.symbol})
	if workspaceErr != nil {
		issues = append(issues, "workspace/symbol: "+workspaceErr.Error())
	} else {
		var symbols []persistentAcceptanceWorkspaceSymbol
		if err := json.Unmarshal(workspaceRaw, &symbols); err != nil {
			issues = append(issues, "workspace/symbol decode: "+err.Error())
		} else {
			found := false
			for _, symbol := range symbols {
				if symbol.Name == fixture.symbol && semanticCanonicalURI(symbol.Location.URI) == targetURI &&
					symbol.Location.Range.Start.Line == wantDefinition.StartLine && symbol.Location.Range.Start.Character == wantDefinition.StartChar {
					found = true
				}
			}
			observed["workspaceSymbols"] = symbols
			if !found {
				issues = append(issues, "workspace/symbol omitted the indexed target definition")
			}
		}
	}
	checkMeta("workspace/symbol", "")

	definitionParams := map[string]any{
		"textDocument": map[string]any{"uri": useURI},
		"position":     map[string]any{"line": callLine, "character": callCharacter},
	}
	definitionRaw, definitionErr := second.RequestContext(ctx, "textDocument/definition", definitionParams)
	if definitionErr != nil {
		issues = append(issues, "textDocument/definition: "+definitionErr.Error())
	} else {
		locations, err := normalizeSemanticLocations(definitionRaw)
		if err != nil {
			issues = append(issues, "textDocument/definition decode: "+err.Error())
		} else if !containsSemanticLocation(locations, wantDefinition) {
			issues = append(issues, "textDocument/definition omitted the indexed target location")
		}
		observed["definition"] = locations
	}
	checkMeta("textDocument/definition", useURI)

	referencesParams := map[string]any{
		"textDocument": map[string]any{"uri": useURI},
		"position":     map[string]any{"line": callLine, "character": callCharacter},
		"context":      map[string]any{"includeDeclaration": true},
	}
	referencesRaw, referencesErr := second.RequestContext(ctx, "textDocument/references", referencesParams)
	if referencesErr != nil {
		issues = append(issues, "textDocument/references: "+referencesErr.Error())
	} else {
		locations, err := normalizeSemanticLocations(referencesRaw)
		if err != nil {
			issues = append(issues, "textDocument/references decode: "+err.Error())
		} else if !containsSemanticLocation(locations, wantDefinition) || !containsSemanticLocation(locations, wantCall) {
			issues = append(issues, "textDocument/references omitted the indexed declaration or call")
		}
		observed["references"] = locations
	}
	checkMeta("textDocument/references", useURI)

	traceAfterRaw, traceAfterErr := second.RequestContext(ctx, "omnilsp/queryTrace", map[string]any{})
	var traceAfter semanticQueryTrace
	if traceAfterErr == nil {
		traceAfterErr = json.Unmarshal(traceAfterRaw, &traceAfter)
	}
	observed["queryTraceAfter"] = traceAfter
	if traceAfterErr != nil {
		issues = append(issues, "post-query queryTrace: "+traceAfterErr.Error())
	} else if traceAfter.Computations != traceBefore.Computations {
		issues = append(issues, fmt.Sprintf("query engine computations changed from %d to %d", traceBefore.Computations, traceAfter.Computations))
	}
	afterRaw, afterErr := second.RequestContext(ctx, "omnilsp/indexStats", map[string]any{})
	var after persistentAcceptanceIndexStats
	if afterErr == nil {
		afterErr = json.Unmarshal(afterRaw, &after)
	}
	observed["indexStatsAfter"] = string(afterRaw)
	if afterErr != nil {
		issues = append(issues, "post-query indexStats: "+afterErr.Error())
	} else if after.Generation != indexed.Generation || !after.Fresh {
		issues = append(issues, "persistent generation changed or became stale during read-only queries")
	}
	observed["workspaceSymbolError"] = errorString(workspaceErr)
	observed["definitionError"] = errorString(definitionErr)
	observed["referencesError"] = errorString(referencesErr)
	observed["plannerProbeCount"] = "unobservable: production candidate exposes no external planner-probe counter"
	if len(issues) > 0 {
		block(strings.Join(issues, "; "), observed)
		return
	}
	if closeErr := second.Close(); closeErr != nil {
		secondClosed = true
		block("phase 2 server exited unsuccessfully: "+closeErr.Error(), map[string]any{"observed": observed})
		return
	}
	secondClosed = true
	check.Status = report.Passed
	check.Summary = "persistent " + fixture.language + " generation served workspace symbols, definition, and references after restart with the live backend disabled"
	check.Observed = observed
}

// Bind both processes to the same verified compiler inputs rather than relying
// on the invoking shell's optional semantic-tool environment.
func persistentAcceptanceToolEnv(root string) ([]string, error) {
	return persistentAcceptanceToolEnvFor(root, "typescript")
}

func persistentAcceptanceToolEnvFor(root, language string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, "test", "acceptance", "tools", "tools.lock.json"))
	if err != nil {
		return nil, err
	}
	var lock struct {
		Binaries map[string]struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"resolvedBinaries"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, err
	}
	var env []string
	inputs := []struct{ name, variable string }{}
	switch language {
	case "typescript", "javascript":
		inputs = append(inputs, []struct{ name, variable string }{
			{"node", "OMNILSP_SEMANTIC_NODE_PATH"},
			{"typescript", "OMNILSP_SEMANTIC_TYPESCRIPT_PATH"},
		}...)
	case "python":
		inputs = append(inputs, []struct{ name, variable string }{
			{"node", "OMNILSP_SEMANTIC_NODE_PATH"},
			{"python", "OMNILSP_SEMANTIC_PYTHON_PATH"},
			{"pyright-internal", "OMNILSP_SEMANTIC_PYRIGHT_INTERNAL_PATH"},
			{"pyright-vendor", "OMNILSP_SEMANTIC_PYRIGHT_VENDOR_PATH"},
		}...)
	case "c", "cpp":
		compiler := "clang"
		if language == "cpp" {
			compiler = "clang++"
		}
		// The compilation database names the pinned compiler; libclang is
		// resolved beside it by the production planner, not by an override.
		inputs = append(inputs, []struct{ name, variable string }{
			{compiler, ""}, {"libclang", ""},
		}...)
	default:
		return nil, fmt.Errorf("unsupported extraction tool language %q", language)
	}
	for _, input := range inputs {
		tool := lock.Binaries[input.name]
		if !filepath.IsAbs(tool.Path) || len(tool.SHA256) != 64 {
			return nil, fmt.Errorf("%s has no pinned absolute path and SHA-256", input.name)
		}
		file, err := os.Open(tool.Path)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		_, readErr := io.Copy(hash, file)
		closeErr := file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), tool.SHA256) {
			return nil, fmt.Errorf("%s SHA-256 differs from tool lock", input.name)
		}
		if input.variable != "" {
			env = append(env, input.variable+"="+tool.Path)
		}
	}
	return env, nil
}

func TestPersistentAcceptanceToolEnvRejectsChangedPinnedInput(t *testing.T) {
	root := t.TempDir()
	lockDir := filepath.Join(root, "test", "acceptance", "tools")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "pinned-tool")
	content := []byte("non-executable pinned input")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	input := map[string]string{"path": path, "sha256": hex.EncodeToString(digest[:])}
	data, err := json.Marshal(map[string]any{"resolvedBinaries": map[string]any{
		"node": input, "typescript": input, "python": input,
		"pyright-internal": input, "pyright-vendor": input,
		"clang": input, "clang++": input, "libclang": input,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "tools.lock.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if env, err := persistentAcceptanceToolEnv(root); err != nil || len(env) != 2 {
		t.Fatalf("verified inputs: env=%v err=%v", env, err)
	}
	if env, err := persistentAcceptanceToolEnvFor(root, "python"); err != nil || len(env) != 4 {
		t.Fatalf("verified Python inputs: env=%v err=%v", env, err)
	}
	for _, language := range []string{"c", "cpp"} {
		if env, err := persistentAcceptanceToolEnvFor(root, language); err != nil || len(env) != 0 {
			t.Fatalf("verified %s inputs must preserve production tool discovery: env=%v err=%v", language, env, err)
		}
	}
	var brokenLock map[string]map[string]map[string]string
	if err := json.Unmarshal(data, &brokenLock); err != nil {
		t.Fatal(err)
	}
	brokenLock["resolvedBinaries"]["libclang"]["sha256"] = strings.Repeat("0", 64)
	clangBrokenData, err := json.Marshal(brokenLock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "tools.lock.json"), clangBrokenData, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, language := range []string{"c", "cpp"} {
		if _, err := persistentAcceptanceToolEnvFor(root, language); err == nil {
			t.Fatalf("changed libclang identity must block %s extraction", language)
		}
	}
	if err := json.Unmarshal(data, &brokenLock); err != nil {
		t.Fatal(err)
	}
	brokenLock["resolvedBinaries"]["pyright-vendor"]["sha256"] = strings.Repeat("0", 64)
	brokenData, err := json.Marshal(brokenLock)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(lockDir, "tools.lock.json")
	if err := os.WriteFile(lockPath, brokenData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := persistentAcceptanceToolEnvFor(root, "python"); err == nil {
		t.Fatal("changed Pyright vendor identity must block the real-process test")
	}
	if err := os.WriteFile(lockPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed pinned input"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := persistentAcceptanceToolEnv(root); err == nil {
		t.Fatal("changed compiler input must block the real-process test")
	}
}

type persistentSemanticFixture struct {
	language      string
	symbol        string
	files         map[string]string
	targetFile    string
	useFile       string
	callToken     string
	scopeIDPrefix string
	definition    semanticLocation
}

func setPersistentFixtureEvidence(evidence *report.Report, fixture persistentSemanticFixture) {
	// JSON orders map keys; hash the logical files rather than temporary paths.
	manifest, _ := json.Marshal(fixture.files)
	digest := sha256.Sum256(manifest)
	evidence.Corpus = report.Corpus{Name: "persistent semantic fixture: " + fixture.language, SHA256: "sha256:" + hex.EncodeToString(digest[:])}
	evidence.Environment["fixtureManifest"] = string(manifest)
	evidence.Environment["goos"] = runtime.GOOS
	evidence.Environment["goarch"] = runtime.GOARCH
	evidence.Environment["goVersion"] = runtime.Version()
}

func newPersistentSemanticFixture(t *testing.T, workspace, language string) (persistentSemanticFixture, string, string, uint32, uint32) {
	t.Helper()
	var fixture persistentSemanticFixture
	switch language {
	case "c", "cpp":
		return newPersistentClangFixture(t, workspace, language)
	case "typescript":
		fixture = persistentSemanticFixture{
			language: language, symbol: "target", targetFile: "target.ts", useFile: "use.ts",
			callToken: "target()", scopeIDPrefix: "typescript-config:",
			files: map[string]string{
				"tsconfig.json": `{"compilerOptions":{"target":"ES2020","module":"commonjs","strict":true},"include":["*.ts"]}` + "\n",
				"target.ts":     "export function target(): number { return 7; }\n",
				"use.ts":        "import { target } from \"./target\";\nexport function run(): number { return target(); }\n",
			},
			definition: semanticLocation{StartLine: 0, StartChar: 16, EndLine: 0, EndChar: 22},
		}
	case "go":
		fixture = persistentSemanticFixture{
			language: language, symbol: "Target", targetFile: "target.go", useFile: "use.go",
			callToken: "Target", scopeIDPrefix: "go:scope:",
			files: map[string]string{
				"go.mod":    "module acceptance.local/persistent\n\ngo 1.26\n",
				"target.go": "package fixture\n\nfunc Target() int { return 7 }\n",
				"use.go":    "package fixture\n\nfunc Run() int { return Target() }\n",
			},
			definition: semanticLocation{StartLine: 2, StartChar: 5, EndLine: 2, EndChar: 11},
		}
	case "python":
		fixture = persistentSemanticFixture{
			language: language, symbol: "target", targetFile: "target.py", useFile: "use.py",
			callToken: "target()", scopeIDPrefix: "python-config:",
			files: map[string]string{
				"pyrightconfig.json": "{\"typeCheckingMode\":\"strict\"}\n",
				"target.py":          "def target() -> int:\n    return 7\n",
				"use.py":             "from target import target\n\ndef run() -> int:\n    return target()\n",
			},
			definition: semanticLocation{StartLine: 0, StartChar: 4, EndLine: 0, EndChar: 10},
		}
	case "javascript":
		fixture = persistentSemanticFixture{
			language: language, symbol: "target", targetFile: "target.js", useFile: "use.js",
			callToken: "target()", scopeIDPrefix: "typescript-config:",
			files: map[string]string{
				"tsconfig.json": `{"compilerOptions":{"target":"ES2020","module":"commonjs","allowJs":true,"checkJs":true,"strict":true,"noEmit":true},"include":["*.js"]}` + "\n",
				"target.js":     "export function target() { return 7; }\n",
				"use.js":        "import { target } from \"./target\";\nexport function run() { return target(); }\n",
			},
			definition: semanticLocation{StartLine: 0, StartChar: 16, EndLine: 0, EndChar: 22},
		}
	default:
		t.Fatalf("unsupported persistent semantic fixture language %q", language)
	}
	if err := semanticWriteFiles(workspace, fixture.files); err != nil {
		t.Fatalf("write %s workspace: %v", language, err)
	}
	targetURI := semanticCanonicalURI(uri.FromPath(filepath.Join(workspace, fixture.targetFile)).String())
	useURI := semanticCanonicalURI(uri.FromPath(filepath.Join(workspace, fixture.useFile)).String())
	fixture.definition.URI = targetURI
	callLine, callCharacter := semanticPositionOf(fixture.files[fixture.useFile], fixture.callToken, 0)
	return fixture, targetURI, useURI, callLine, callCharacter
}

func persistentSemanticBackendConfigs(enabledLanguage string, enabled bool) []config.BackendConfig {
	languages := []string{"go", "c", "cpp", "rust", "python", "typescript"}
	backends := make([]config.BackendConfig, 0, len(languages))
	for _, language := range languages {
		backends = append(backends, config.BackendConfig{LanguageID: language, Enabled: language == enabledLanguage && enabled})
	}
	return backends
}

func TestPersistentSemanticFixturesHaveCorrectQueryPositions(t *testing.T) {
	for _, language := range []string{"typescript", "go", "python", "javascript"} {
		t.Run(language, func(t *testing.T) {
			fixture, targetURI, useURI, callLine, callCharacter := newPersistentSemanticFixture(t, t.TempDir(), language)
			if fixture.definition.URI != targetURI || useURI == "" || fixture.symbol == "" {
				t.Fatalf("fixture has incomplete target identity: %+v target=%q use=%q", fixture, targetURI, useURI)
			}
			if callLine == ^uint32(0) || callCharacter == ^uint32(0) {
				t.Fatalf("fixture call token %q was not found in %s", fixture.callToken, fixture.useFile)
			}
			if language == "go" && (callLine != 2 || callCharacter != 24 || fixture.definition.StartLine != 2 || fixture.definition.StartChar != 5) {
				t.Fatalf("Go semantic positions: call=%d:%d definition=%+v", callLine, callCharacter, fixture.definition)
			}
			if language == "python" && (callLine != 3 || callCharacter != 11 || fixture.definition.StartLine != 0 || fixture.definition.StartChar != 4) {
				t.Fatalf("Python semantic positions: call=%d:%d definition=%+v", callLine, callCharacter, fixture.definition)
			}
		})
	}
}

func persistentAcceptanceIndexStatsFrom(raw json.RawMessage, err error) (persistentAcceptanceIndexStats, json.RawMessage, error) {
	if err != nil {
		return persistentAcceptanceIndexStats{}, raw, err
	}
	var stats persistentAcceptanceIndexStats
	if err := json.Unmarshal(raw, &stats); err != nil {
		return persistentAcceptanceIndexStats{}, raw, err
	}
	return stats, raw, nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func rawMessageAsAny(raw json.RawMessage) any {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	return value
}

// TestCandidateGoCLIInteropAndSemanticReplayPersistence exercises the real
// candidate's Go reindex, explicitly lossy SCIP export/import, and generation-
// bound replay. It requires OMNILSP_BIN so this acceptance test never races a
// separately controlled candidate build.
func TestCandidateGoCLIInteropAndSemanticReplayPersistence(t *testing.T) {
	if os.Getenv(semanticAcceptanceModeEnv) != "1" {
		t.Skip("not run: set OMNILSP_ACCEPTANCE=1 to enable real-candidate CLI acceptance")
	}
	if strings.TrimSpace(os.Getenv(semanticAcceptanceBinEnv)) == "" {
		t.Skip("not run: set OMNILSP_BIN to the controlled candidate binary; this test does not build candidates")
	}
	root, err := semanticRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"GOCACHE": filepath.Join(root, ".tmp-gocache"), "GOTELEMETRY": "off",
		"GOTOOLCHAIN": "local", "GOENV": "off", "GOWORK": "off", "GOFLAGS": "",
		"GOPROXY": "off", "GOSUMDB": "off", "GOMAXPROCS": "8",
		"OMNILSP_TRUST": "trusted", "OMNILSP_LOG_LEVEL": "error",
		"OMNILSP_MAX_CONCURRENT": "8", "OMNILSP_MAX_QUEUE": "64",
		"PATH": semanticAcceptanceToolPath(),
	} {
		t.Setenv(name, value)
	}
	binary, err := semanticResolveBinary(t)
	if err != nil {
		t.Fatalf("resolve controlled candidate: %v", err)
	}
	binaryData, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("read candidate binary: %v", err)
	}
	binaryDigest := sha256.Sum256(binaryData)

	runID := strings.TrimSpace(os.Getenv(semanticRunIDEnv))
	if runID == "" {
		runID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	evidence := report.New(runID)
	evidence.Candidate = report.Candidate{
		Binary: binary, SHA256: "sha256:" + hex.EncodeToString(binaryDigest[:]), Revision: semanticGitRevision(t),
	}
	evidence.Environment["acceptanceMode"] = "explicit"
	evidence.Environment["candidateProcess"] = "controlled Go candidate reindex, SCIP CLI roundtrip, disabled-backend query and replay"
	evidence.Environment["fixtureLanguage"] = "go"
	evidence.Limits["lossyExport"] = "only --allow-lossy may omit SCIP-unrepresentable semantic edge kinds; counts are checked in CLI JSON and SCIP ToolInfo"
	evidence.Limits["replay"] = "original complete Go generation is replayed; config, tool identity, content identity, and generation mismatches must fail"
	check := report.Check{ID: runID + "/S22/go-cli-scip-replay-persistence", Status: report.NotVerified}
	observed := make(map[string]any)
	defer func() {
		check.Observed = observed
		evidence.Checks = append(evidence.Checks, check)
		evidence.Finalize(time.Now())
		path := persistentCLIReportPath(runID)
		if err := report.Write(path, evidence); err != nil {
			t.Errorf("write Go SCIP/replay acceptance report: %v", err)
			return
		}
		t.Logf("Go SCIP/replay acceptance JSON report: %s (decision=%s)", path, evidence.Decision)
	}()
	fail := func(summary string) {
		check.Status = report.Failed
		check.Summary = summary
		t.Fatalf("Go CLI SCIP/replay acceptance failed: %s", summary)
	}
	artifacts := t.TempDir()
	workspace := filepath.Join(artifacts, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		fail("create isolated fixture workspace: " + err.Error())
	}
	fixture, targetURI, _, _, _ := newPersistentSemanticFixture(t, workspace, "go")
	setPersistentFixtureEvidence(&evidence, fixture)
	rootURI := semanticCanonicalURI(uri.FromPath(workspace).String())
	sourceIndexDir := filepath.Join(artifacts, "source-index")
	importIndexDir := filepath.Join(artifacts, "import-index")
	wrongSourceIndexDir := filepath.Join(artifacts, "wrong-source-index")
	artifactDir := filepath.Join(artifacts, "interop-artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		fail("create external interop artifact directory: " + err.Error())
	}
	goConfig := config.Default()
	goConfig.LogLevel = "error"
	goConfig.Transport = "stdio"
	goConfig.WorkspaceDir = workspace
	goConfig.IndexDir = sourceIndexDir
	goConfig.MaxConcurrentRequests = 8
	goConfig.MaxQueueSize = 64
	goConfig.Backends = persistentSemanticBackendConfigs("go", true)
	goConfigPath := writePersistentAcceptanceConfig(t, artifactDir, "go-enabled.json", goConfig)

	phase1, err := upstream.Start(binary,
		[]string{"serve", "--config", goConfigPath, "--transport", "stdio", "--workspace", workspace}, workspace, nil)
	if err != nil {
		fail("start candidate for real Go reindex: " + err.Error())
	}
	phase1Closed := false
	defer func() {
		if !phase1Closed {
			_ = phase1.Close()
		}
	}()
	phase1Ctx, phase1Cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := phase1.Initialize(phase1Ctx, workspace); err != nil {
		phase1Cancel()
		fail("initialize Go index candidate: " + err.Error())
	}
	backendRaw, err := phase1.RequestContext(phase1Ctx, "omnilsp/backendStatus", map[string]any{"language": "go"})
	var backend persistentAcceptanceBackendStatus
	if err == nil {
		err = json.Unmarshal(backendRaw, &backend)
	}
	if err != nil || !backend.Found {
		phase1Cancel()
		fail("candidate Go backend not confirmed enabled: " + errorString(err) + "; response=" + string(backendRaw))
	}
	reindexRaw, err := phase1.RequestContext(phase1Ctx, "omnilsp/reindex", map[string]any{})
	statsRaw, statsErr := phase1.RequestContext(phase1Ctx, "omnilsp/indexStats", map[string]any{})
	phase1Cancel()
	if err != nil {
		fail("real Go candidate reindex failed: " + err.Error() + "; response=" + string(reindexRaw))
	}
	if closeErr := phase1.Close(); closeErr != nil {
		phase1Closed = true
		fail("Go indexing candidate exited unsuccessfully: " + closeErr.Error())
	}
	phase1Closed = true
	var reindexed struct {
		Generation uint64 `json:"generation"`
	}
	if err := json.Unmarshal(reindexRaw, &reindexed); err != nil || reindexed.Generation == 0 {
		fail("candidate did not commit a real Go generation: " + errorString(err) + "; response=" + string(reindexRaw))
	}
	stats, _, err := persistentAcceptanceIndexStatsFrom(statsRaw, statsErr)
	if err != nil || !stats.Fresh || !stats.Enabled || stats.Generation != reindexed.Generation {
		fail("candidate Go generation is not fresh and enabled: " + errorString(err) + "; stats=" + string(statsRaw))
	}
	var scopeID string
	for _, coverage := range stats.SemanticCoverage {
		if strings.HasPrefix(coverage.ScopeID, fixture.scopeIDPrefix) {
			if scopeID != "" && scopeID != coverage.ScopeID {
				fail("candidate produced more than one Go scope in the isolated fixture")
			}
			scopeID = coverage.ScopeID
		}
	}
	if scopeID == "" {
		fail("candidate Go generation has no semantic coverage scope")
	}
	evidence.Environment["sourceGeneration"] = fmt.Sprint(reindexed.Generation)
	evidence.Environment["sourceScope"] = scopeID
	observed["sourceGeneration"] = reindexed.Generation
	observed["sourceScope"] = scopeID

	currentHashes, err := persistentFixtureSourceHashes(workspace, fixture.files)
	if err != nil {
		fail("hash immutable fixture sources: " + err.Error())
	}
	sourceStore, err := persistent.NewFileStore(sourceIndexDir, persistent.Config{})
	if err != nil {
		fail("open Go candidate's actual persistent store: " + err.Error())
	}
	sourceView, err := sourceStore.OpenSnapshot(context.Background())
	if err != nil {
		fail("open Go candidate's committed generation: " + err.Error())
	}
	if sourceView.ID != reindexed.Generation {
		fail(fmt.Sprintf("persistent store generation %d differs from reindex result %d", sourceView.ID, reindexed.Generation))
	}
	sourceReader, err := semantic.OpenReader(context.Background(), sourceView)
	if err != nil {
		fail("validate Go candidate's persisted semantic generation: " + err.Error())
	}
	sourceMeta := sourceReader.Metadata()
	if len(sourceMeta.Scopes) != 1 || sourceMeta.Scopes[0].ID != scopeID ||
		sourceMeta.Identity.Workspace == "" || sourceMeta.Identity.DiskDigest == "" ||
		len(sourceMeta.UsedTools[scopeID]) == 0 {
		_ = sourceReader.Close()
		fail("real Go generation lacks a complete scope/source/tool identity")
	}
	var sourceSymbols, sourceOccurrences, sourceEdges int
	var sourceModuleEdges, sourceCallEdges int
	for {
		batch, err := sourceReader.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = sourceReader.Close()
			fail("read Go candidate's semantic facts: " + err.Error())
		}
		switch batch.Kind {
		case semantic.BatchSymbols:
			sourceSymbols += len(batch.Symbols)
		case semantic.BatchOccurrences:
			sourceOccurrences += len(batch.Occurrences)
			for _, occurrence := range batch.Occurrences {
				if currentHashes[occurrence.URI] == "" || currentHashes[occurrence.URI] != string(occurrence.SourceHash) {
					_ = sourceReader.Close()
					fail("Go producer occurrence is not bound to the current source file: " + occurrence.URI)
				}
			}
		case semantic.BatchEdges:
			sourceEdges += len(batch.Edges)
			for _, edge := range batch.Edges {
				if edge.Kind == model.EdgeModule {
					sourceModuleEdges++
				}
				if edge.Kind == model.EdgeCall {
					sourceCallEdges++
				}
				if currentHashes[edge.SourceURI] == "" || currentHashes[edge.SourceURI] != string(edge.SourceHash) {
					_ = sourceReader.Close()
					fail("Go producer edge is not bound to the current source file: " + edge.SourceURI)
				}
			}
		}
	}
	if err := sourceReader.Close(); err != nil {
		fail("close Go candidate semantic reader: " + err.Error())
	}
	if sourceSymbols == 0 || sourceOccurrences == 0 || sourceEdges == 0 || sourceModuleEdges == 0 {
		fail(fmt.Sprintf("real Go generation facts are incomplete for SCIP export: symbols=%d occurrences=%d edges=%d moduleEdges=%d", sourceSymbols, sourceOccurrences, sourceEdges, sourceModuleEdges))
	}
	observed["sourceFacts"] = map[string]int{"symbols": sourceSymbols, "occurrences": sourceOccurrences, "edges": sourceEdges, "moduleEdges": sourceModuleEdges, "callEdges": sourceCallEdges}

	strictExportPath := filepath.Join(artifactDir, "strict-go-export.scip")
	strictResult := runPersistentCandidateCLI(binary, workspace,
		"index", "export", "--format", "scip", "--scope", scopeID, "--output", strictExportPath,
		"--config", goConfigPath, "--workspace", workspace)
	if strictResult.Err == nil || !strictResult.contains("cannot represent semantic edge kind") {
		fail("default CLI export did not fail closed on the Go module edge: " + strictResult.diagnostic())
	}
	if _, err := os.Stat(strictExportPath); !errors.Is(err, os.ErrNotExist) {
		fail("strict failed export unexpectedly published an artifact")
	}
	observed["strictExport"] = persistentCLIResultObservation(strictResult)

	exportPath := filepath.Join(artifactDir, "go-generation.scip")
	exportResult := runPersistentCandidateCLI(binary, workspace,
		"index", "export", "--format", "scip", "--scope", scopeID, "--allow-lossy", "--output", exportPath,
		"--config", goConfigPath, "--workspace", workspace)
	if exportResult.Err != nil {
		fail("explicit lossy CLI export of real Go generation failed: " + exportResult.diagnostic())
	}
	var exportSummary struct {
		Operation string                           `json:"operation"`
		Scope     string                           `json:"scope"`
		Bytes     int                              `json:"bytes"`
		Losses    []interop.SemanticSCIPExportLoss `json:"losses"`
	}
	if err := json.Unmarshal([]byte(exportResult.Stdout), &exportSummary); err != nil {
		fail("decode explicit export loss summary from stdout: " + err.Error() + "; " + exportResult.diagnostic())
	}
	if exportSummary.Operation != "export" || exportSummary.Scope != scopeID || exportSummary.Bytes <= 0 || len(exportSummary.Losses) == 0 {
		fail("CLI export summary lacks scope, byte count, or structured losses: " + exportResult.diagnostic())
	}
	moduleLoss := uint64(0)
	for _, loss := range exportSummary.Losses {
		if loss.Kind == model.EdgeModule {
			moduleLoss += loss.Count
		}
	}
	if moduleLoss == 0 {
		fail("explicit lossy export did not report the producer's unrepresentable module edges")
	}
	outputBytes, err := os.ReadFile(exportPath)
	if err != nil {
		fail("read exported SCIP artifact: " + err.Error())
	}
	if len(outputBytes) != exportSummary.Bytes {
		fail(fmt.Sprintf("CLI export byte count %d differs from artifact size %d", exportSummary.Bytes, len(outputBytes)))
	}
	var exported scip.Index
	if err := proto.Unmarshal(outputBytes, &exported); err != nil {
		fail("decode real Go SCIP artifact: " + err.Error())
	}
	if exported.GetMetadata().GetProjectRoot() != rootURI {
		fail("real Go SCIP artifact lost the captured workspace root")
	}
	exportedOccurrences := 0
	for _, document := range exported.GetDocuments() {
		exportedOccurrences += len(document.GetOccurrences())
	}
	if exportedOccurrences == 0 {
		fail("real Go SCIP artifact contains no semantic occurrences")
	}
	for _, loss := range exportSummary.Losses {
		marker := fmt.Sprintf("omnilsp.lossy-edge.%s=%d", loss.Kind, loss.Count)
		if !persistentAcceptanceHasArgument(exported.GetMetadata().GetToolInfo().GetArguments(), marker) {
			fail("SCIP artifact omitted loss marker " + marker)
		}
	}
	observed["exportSummary"] = persistentCLIResultObservation(exportResult)
	observed["losses"] = exportSummary.Losses
	observed["exportedDocuments"] = len(exported.GetDocuments())
	observed["exportedOccurrences"] = exportedOccurrences

	importConfig := goConfig
	importConfig.IndexDir = importIndexDir
	importConfigPath := writePersistentAcceptanceConfig(t, artifactDir, "go-import.json", importConfig)
	importResult := runPersistentCandidateCLI(binary, workspace,
		"index", "import", "--format", "scip", "--scope", scopeID, "--language", "go", "--input", exportPath,
		"--config", importConfigPath, "--workspace", workspace)
	if importResult.Err != nil {
		fail("candidate CLI could not import its exported SCIP artifact into a fresh store: " + importResult.diagnostic())
	}
	observed["importOutput"] = persistentCLIResultObservation(importResult)
	importStore, err := persistent.NewFileStore(importIndexDir, persistent.Config{})
	if err != nil {
		fail("open imported SCIP FileStore: " + err.Error())
	}
	importView, err := importStore.OpenSnapshot(context.Background())
	if err != nil {
		fail("open imported SCIP generation: " + err.Error())
	}
	importReader, err := semantic.OpenReader(context.Background(), importView)
	if err != nil {
		fail("validate imported SCIP generation: " + err.Error())
	}
	importMeta := importReader.Metadata()
	if importView.ID == 0 || len(importMeta.Scopes) != 1 || importMeta.Scopes[0].ID != scopeID ||
		importMeta.Identity.DiskDigest == "" || importMeta.Identity.Workspace == "" {
		_ = importReader.Close()
		fail("imported generation lost scope or captured source identity metadata")
	}
	coverage := make(map[model.FactKind]model.Completeness, len(importMeta.Coverage))
	for _, item := range importMeta.Coverage {
		if item.ScopeID != scopeID {
			_ = importReader.Close()
			fail("imported coverage is attached to a different semantic scope")
		}
		coverage[item.Fact] = item.State
		if item.State == model.Complete {
			_ = importReader.Close()
			fail("SCIP import overstated complete coverage for " + string(item.Fact))
		}
	}
	if coverage[model.FactCall] != model.Unknown || coverage[model.FactModule] != model.Unknown {
		_ = importReader.Close()
		fail(fmt.Sprintf("SCIP import overstated unrepresented call/module coverage: call=%s module=%s", coverage[model.FactCall], coverage[model.FactModule]))
	}
	importedSymbols, importedOccurrences, importedEdges := 0, 0, 0
	for {
		batch, err := importReader.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = importReader.Close()
			fail("read imported SCIP facts: " + err.Error())
		}
		switch batch.Kind {
		case semantic.BatchSymbols:
			importedSymbols += len(batch.Symbols)
		case semantic.BatchOccurrences:
			importedOccurrences += len(batch.Occurrences)
			for _, occurrence := range batch.Occurrences {
				if currentHashes[occurrence.URI] == "" || string(occurrence.SourceHash) != currentHashes[occurrence.URI] {
					_ = importReader.Close()
					fail("SCIP import did not rebind occurrence to current source hash: " + occurrence.URI)
				}
			}
		case semantic.BatchEdges:
			importedEdges += len(batch.Edges)
			for _, edge := range batch.Edges {
				if currentHashes[edge.SourceURI] == "" || string(edge.SourceHash) != currentHashes[edge.SourceURI] {
					_ = importReader.Close()
					fail("SCIP import did not rebind relationship to current source hash: " + edge.SourceURI)
				}
			}
		}
	}
	if err := importReader.Close(); err != nil {
		fail("close imported SCIP semantic reader: " + err.Error())
	}
	if importedSymbols == 0 || importedOccurrences == 0 {
		fail(fmt.Sprintf("imported generation lacks real SCIP facts: symbols=%d occurrences=%d", importedSymbols, importedOccurrences))
	}
	observed["importedGeneration"] = importView.ID
	observed["importedFacts"] = map[string]int{"symbols": importedSymbols, "occurrences": importedOccurrences, "edges": importedEdges}
	observed["importedCoverage"] = coverage

	wrongRootIndex := proto.Clone(&exported).(*scip.Index)
	wrongRootIndex.Metadata.ProjectRoot = "file:///different-source-root"
	wrongRootData, err := (proto.MarshalOptions{Deterministic: true}).Marshal(wrongRootIndex)
	if err != nil {
		fail("encode wrong-root SCIP fixture: " + err.Error())
	}
	wrongRootPath := filepath.Join(artifactDir, "wrong-root.scip")
	if err := os.WriteFile(wrongRootPath, wrongRootData, 0o600); err != nil {
		fail("write wrong-root SCIP fixture: " + err.Error())
	}
	wrongRootConfig := goConfig
	wrongRootConfig.IndexDir = wrongSourceIndexDir
	wrongRootConfigPath := writePersistentAcceptanceConfig(t, artifactDir, "wrong-root-import.json", wrongRootConfig)
	wrongRootResult := runPersistentCandidateCLI(binary, workspace,
		"index", "import", "--format", "scip", "--scope", scopeID, "--language", "go", "--input", wrongRootPath,
		"--config", wrongRootConfigPath, "--workspace", workspace)
	if wrongRootResult.Err == nil || !wrongRootResult.contains("SCIP project root does not match") {
		fail("CLI import accepted a SCIP artifact from another source root: " + wrongRootResult.diagnostic())
	}
	observed["wrongSourceRootRejected"] = persistentCLIResultObservation(wrongRootResult)
	wrongRootStore, err := persistent.NewFileStore(wrongSourceIndexDir, persistent.Config{})
	if err != nil {
		fail("open rejected source-root store: " + err.Error())
	}
	if _, err := wrongRootStore.OpenSnapshot(context.Background()); !errors.Is(err, persistent.ErrNoGeneration) {
		fail("rejected source-root import published a persistent generation: " + errorString(err))
	}

	disabledConfig := goConfig
	disabledConfig.Backends = persistentSemanticBackendConfigs("go", false)
	disabledConfigPath := writePersistentAcceptanceConfig(t, artifactDir, "go-disabled.json", disabledConfig)
	recordPath := filepath.Join(artifactDir, "go-semantic-session.jsonl")
	recorder, err := upstream.Start(binary,
		[]string{"serve", "--record", recordPath, "--config", disabledConfigPath, "--transport", "stdio", "--workspace", workspace}, workspace, nil)
	if err != nil {
		fail("start disabled-backend recorder for the original Go generation: " + err.Error())
	}
	recorderClosed := false
	defer func() {
		if !recorderClosed {
			_ = recorder.Close()
		}
	}()
	replayCtx, replayCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := recorder.Initialize(replayCtx, workspace); err != nil {
		replayCancel()
		fail("initialize disabled-backend recorder: " + err.Error())
	}
	backendRaw, backendErr := recorder.RequestContext(replayCtx, "omnilsp/backendStatus", map[string]any{"language": "go"})
	if backendErr == nil {
		backendErr = json.Unmarshal(backendRaw, &backend)
	}
	if backendErr != nil || backend.Found {
		replayCancel()
		fail("Go backend was not disabled during persistent query recording: " + errorString(backendErr) + "; status=" + string(backendRaw))
	}
	statsRaw, statsErr = recorder.RequestContext(replayCtx, "omnilsp/indexStats", map[string]any{})
	phase2Stats, _, statsErr := persistentAcceptanceIndexStatsFrom(statsRaw, statsErr)
	if statsErr != nil || !phase2Stats.Fresh || phase2Stats.Generation != reindexed.Generation {
		replayCancel()
		fail("disabled backend did not reopen the original complete generation: " + errorString(statsErr) + "; stats=" + string(statsRaw))
	}
	symbolRaw, symbolErr := recorder.RequestContext(replayCtx, "workspace/symbol", map[string]any{"query": fixture.symbol})
	if symbolErr != nil {
		replayCancel()
		fail("persistent Go workspace/symbol query failed: " + symbolErr.Error())
	}
	var workspaceSymbols []persistentAcceptanceWorkspaceSymbol
	if err := json.Unmarshal(symbolRaw, &workspaceSymbols); err != nil {
		replayCancel()
		fail("decode persistent workspace/symbol result: " + err.Error())
	}
	foundTarget := false
	for _, symbol := range workspaceSymbols {
		if symbol.Name == fixture.symbol && semanticCanonicalURI(symbol.Location.URI) == targetURI {
			foundTarget = true
		}
	}
	if !foundTarget {
		replayCancel()
		fail("disabled-backend query did not return the Go target definition")
	}
	metaRaw, metaErr := recorder.RequestContext(replayCtx, "omnilsp/resultMeta", map[string]any{"method": "workspace/symbol"})
	if metaErr != nil {
		replayCancel()
		fail("read recorded persistent query evidence: " + metaErr.Error())
	}
	var queryMeta []persistentAcceptanceMeta
	if err := json.Unmarshal(metaRaw, &queryMeta); err != nil {
		replayCancel()
		fail("decode persistent query evidence: " + err.Error())
	}
	queryEvidenceFound := false
	for _, item := range queryMeta {
		if item.Method != "workspace/symbol" {
			continue
		}
		for _, fact := range item.Evidence {
			if fact.Kind == uint8(identity.EvidenceIndex) && fact.IndexGen == reindexed.Generation && fact.BuildContext != "" {
				queryEvidenceFound = true
			}
		}
	}
	if !queryEvidenceFound {
		replayCancel()
		fail("workspace/symbol response was not bound to the original generation")
	}
	replayCancel()
	if err := recorder.Close(); err != nil {
		recorderClosed = true
		fail("close semantic session recorder: " + err.Error())
	}
	recorderClosed = true
	session, err := replay.LoadSession(recordPath)
	if err != nil {
		fail("load actual candidate replay recording: " + err.Error())
	}
	var sawGenerationBinding bool
	for _, entry := range session.Entries {
		if entry.Dir == "out" && entry.SemanticBinding != nil &&
			entry.SemanticBinding.Kind == replay.SemanticBindingGeneration && entry.SemanticBinding.Generation == reindexed.Generation {
			sawGenerationBinding = true
		}
	}
	if !sawGenerationBinding {
		fail("candidate recorder did not persist a generation-bound semantic response")
	}
	observed["recordedIdentity"] = sawGenerationBinding

	replayResult := runPersistentCandidateCLI(binary, workspace,
		"replay", "--input", recordPath, "--config", disabledConfigPath, "--workspace", workspace)
	if replayResult.Err != nil || !replayResult.contains("REPLAY-OK") {
		fail("candidate replay rejected its fixed-generation recording: " + replayResult.diagnostic())
	}
	observed["fixedGenerationReplay"] = persistentCLIResultObservation(replayResult)

	wrongConfig := disabledConfig
	wrongConfig.MaxQueueSize++
	wrongConfigPath := writePersistentAcceptanceConfig(t, artifactDir, "go-wrong-config.json", wrongConfig)
	wrongConfigResult := runPersistentCandidateCLI(binary, workspace,
		"replay", "--input", recordPath, "--config", wrongConfigPath, "--workspace", workspace)
	if wrongConfigResult.Err == nil || !wrongConfigResult.contains("config hash mismatch") {
		fail("candidate replay accepted a mismatched config identity: " + wrongConfigResult.diagnostic())
	}
	observed["wrongConfigRejected"] = persistentCLIResultObservation(wrongConfigResult)

	wrongToolPath, err := mutatePersistentReplaySession(t, artifactDir, recordPath, "wrong-tool.jsonl", func(session *replay.Session) error {
		return mutatePersistentReplayIdentity(session, func(value *replay.SemanticIdentity) error {
			if len(value.Tools) == 0 || value.Tools[0].SHA256 == "" {
				return errors.New("recorded identity has no tool hash")
			}
			value.Tools[0].SHA256 = flipSHA256(value.Tools[0].SHA256)
			return nil
		})
	})
	if err != nil {
		fail("prepare wrong-tool replay case: " + err.Error())
	}
	wrongToolResult := runPersistentCandidateCLI(binary, workspace,
		"replay", "--input", wrongToolPath, "--config", disabledConfigPath, "--workspace", workspace)
	if wrongToolResult.Err == nil || !wrongToolResult.contains("semantic identity differs from recording") {
		fail("candidate replay accepted a changed tool identity: " + wrongToolResult.diagnostic())
	}
	observed["wrongToolIdentityRejected"] = persistentCLIResultObservation(wrongToolResult)

	wrongContentPath, err := mutatePersistentReplaySession(t, artifactDir, recordPath, "wrong-content-identity.jsonl", func(session *replay.Session) error {
		var oldGeneration uint64
		var oldDigest, newDigest string
		err := mutatePersistentReplayIdentity(session, func(value *replay.SemanticIdentity) error {
			oldGeneration = value.Generation
			oldDigest = value.IndexContentDigest
			newDigest = "sha256:" + flipSHA256(strings.TrimPrefix(oldDigest, "sha256:"))
			value.IndexContentDigest = newDigest
			return nil
		})
		if err != nil {
			return err
		}
		if newDigest == "" || oldGeneration == 0 {
			return errors.New("recorded generation identity is incomplete")
		}
		updated := false
		for i := range session.Entries {
			binding := session.Entries[i].SemanticBinding
			if session.Entries[i].Dir == "out" && binding != nil && binding.Kind == replay.SemanticBindingGeneration &&
				binding.Generation == oldGeneration && binding.IndexContentDigest == oldDigest {
				binding.IndexContentDigest = newDigest
				updated = true
			}
		}
		if !updated {
			return errors.New("recorded session has no matching semantic response binding")
		}
		return nil
	})
	if err != nil {
		fail("prepare wrong-content replay case: " + err.Error())
	}
	wrongContentResult := runPersistentCandidateCLI(binary, workspace,
		"replay", "--input", wrongContentPath, "--config", disabledConfigPath, "--workspace", workspace)
	if wrongContentResult.Err == nil || !wrongContentResult.contains("semantic identity differs from recording") {
		fail("candidate replay accepted a changed source/index content identity: " + wrongContentResult.diagnostic())
	}
	observed["wrongContentIdentityRejected"] = persistentCLIResultObservation(wrongContentResult)

	missingGenerationPath, err := mutatePersistentReplaySession(t, artifactDir, recordPath, "missing-generation.jsonl", func(session *replay.Session) error {
		var oldGeneration uint64
		var missingGeneration uint64
		err := mutatePersistentReplayIdentity(session, func(value *replay.SemanticIdentity) error {
			oldGeneration = value.Generation
			missingGeneration = oldGeneration + 1000
			value.Generation = missingGeneration
			return nil
		})
		if err != nil {
			return err
		}
		updated := false
		for i := range session.Entries {
			binding := session.Entries[i].SemanticBinding
			if session.Entries[i].Dir == "out" && binding != nil && binding.Kind == replay.SemanticBindingGeneration && binding.Generation == oldGeneration {
				binding.Generation = missingGeneration
				updated = true
			}
		}
		if !updated {
			return errors.New("recorded session has no matching semantic response binding")
		}
		return nil
	})
	if err != nil {
		fail("prepare missing-generation replay case: " + err.Error())
	}
	missingGenerationResult := runPersistentCandidateCLI(binary, workspace,
		"replay", "--input", missingGenerationPath, "--config", disabledConfigPath, "--workspace", workspace)
	if missingGenerationResult.Err == nil || !missingGenerationResult.contains("required semantic generation is unavailable") {
		fail("candidate replay accepted an unavailable semantic generation: " + missingGenerationResult.diagnostic())
	}
	observed["missingGenerationRejected"] = persistentCLIResultObservation(missingGenerationResult)

	check.Status = report.Passed
	check.Summary = "real Go candidate facts were explicitly exported/imported with truthful losses and source hashes, and replay enforced the original semantic generation identity"
}

func persistentCLIReportPath(runID string) string {
	path := strings.TrimSpace(os.Getenv(semanticReportPathEnv))
	if path == "" {
		return filepath.Join(os.TempDir(), "omnilsp-go-cli-"+strings.ReplaceAll(runID, "/", "-")+".json")
	}
	extension := filepath.Ext(path)
	return strings.TrimSuffix(path, extension) + "-go-cli" + extension
}

func writePersistentAcceptanceConfig(t testing.TB, directory, name string, cfg config.Config) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("encode acceptance config %s: %v", name, err)
	}
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write acceptance config %s: %v", name, err)
	}
	return path
}

type persistentCandidateCLIResult struct {
	Stdout              string
	Stderr              string
	Err                 error
	ExitCode            int
	OutputLimitExceeded bool
	Completed           bool
}

const persistentCandidateCLIOutputLimit = 8192

func runPersistentCandidateCLI(binary, directory string, args ...string) persistentCandidateCLIResult {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = directory
	stop := func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}
	stdout := persistentCLIOutput{stop: stop}
	stderr := persistentCLIOutput{stop: stop}
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if stdout.exceeded || stderr.exceeded {
		err = errors.Join(err, errors.New("candidate CLI diagnostic output exceeded 8 KiB per stream"))
	}
	exitCode := -1
	if command.ProcessState != nil {
		exitCode = command.ProcessState.ExitCode()
	}
	return persistentCandidateCLIResult{
		Stdout:              strings.TrimSpace(stdout.text.String()),
		Stderr:              strings.TrimSpace(stderr.text.String()),
		Err:                 err,
		ExitCode:            exitCode,
		OutputLimitExceeded: stdout.exceeded || stderr.exceeded,
		Completed:           ctx.Err() == nil && command.ProcessState != nil && command.ProcessState.Exited(),
	}
}

type persistentCLIOutput struct {
	text     strings.Builder
	exceeded bool
	stop     func()
}

func (output *persistentCLIOutput) Write(data []byte) (int, error) {
	remaining := persistentCandidateCLIOutputLimit - output.text.Len()
	if len(data) <= remaining {
		return output.text.Write(data)
	}
	_, _ = output.text.Write(data[:remaining])
	if !output.exceeded {
		output.exceeded = true
		output.stop()
	}
	return len(data), nil
}

func TestPersistentCLIOutputLimitStopsProducer(t *testing.T) {
	stops := 0
	output := persistentCLIOutput{stop: func() { stops++ }}
	for i := 0; i < 3; i++ {
		data := []byte(strings.Repeat("x", persistentCandidateCLIOutputLimit))
		if n, err := output.Write(data); err != nil || n != len(data) {
			t.Fatalf("bounded write: n=%d err=%v", n, err)
		}
	}
	if output.text.Len() != persistentCandidateCLIOutputLimit || !output.exceeded || stops != 1 {
		t.Fatalf("output length=%d exceeded=%t stops=%d", output.text.Len(), output.exceeded, stops)
	}
	if (persistentCandidateCLIResult{Stdout: "expected refusal", OutputLimitExceeded: true}).contains("expected refusal") {
		t.Fatal("an output-limit failure was accepted as the intended refusal")
	}
}

func (result persistentCandidateCLIResult) contains(value string) bool {
	return result.Completed && !result.OutputLimitExceeded && (strings.Contains(result.Stdout, value) || strings.Contains(result.Stderr, value))
}

func (result persistentCandidateCLIResult) diagnostic() string {
	return fmt.Sprintf("exitError=%q; stdout=%q; stderr=%q", errorString(result.Err),
		persistentCandidateCLITruncate(result.Stdout), persistentCandidateCLITruncate(result.Stderr))
}

func persistentCandidateCLITruncate(value string) string {
	if len(value) <= persistentCandidateCLIOutputLimit {
		return value
	}
	return strings.ToValidUTF8(value[:persistentCandidateCLIOutputLimit], "�") + "…[truncated]"
}

func persistentCLIResultObservation(result persistentCandidateCLIResult) map[string]any {
	return map[string]any{
		"exitCode":            result.ExitCode,
		"outputLimitExceeded": result.OutputLimitExceeded,
		"completed":           result.Completed,
		"exitError":           errorString(result.Err),
		"stdout":              persistentCandidateCLITruncate(result.Stdout),
		"stderr":              persistentCandidateCLITruncate(result.Stderr),
	}
}

func persistentFixtureSourceHashes(workspace string, files map[string]string) (map[string]string, error) {
	hashes := make(map[string]string, len(files))
	for name := range files {
		path := filepath.Join(workspace, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(data)
		fileURI := semanticCanonicalURI(uri.FromPath(path).String())
		hashes[fileURI] = "sha256:" + hex.EncodeToString(digest[:])
	}
	return hashes, nil
}

func mutatePersistentReplaySession(t testing.TB, directory, sourcePath, name string, mutate func(*replay.Session) error) (string, error) {
	t.Helper()
	session, err := replay.LoadSession(sourcePath)
	if err != nil {
		return "", err
	}
	if err := mutate(session); err != nil {
		return "", err
	}
	path := filepath.Join(directory, name)
	if err := replay.Save(path, session); err != nil {
		return "", err
	}
	return path, nil
}

func mutatePersistentReplayIdentity(session *replay.Session, mutate func(*replay.SemanticIdentity) error) error {
	if session == nil {
		return errors.New("nil replay session")
	}
	found := false
	for i := range session.Entries {
		entry := &session.Entries[i]
		if entry.Dir != "identity" {
			continue
		}
		var identityValue replay.SemanticIdentity
		if err := json.Unmarshal(entry.Payload, &identityValue); err != nil {
			return err
		}
		if mutate != nil {
			if err := mutate(&identityValue); err != nil {
				return err
			}
		}
		encoded, err := json.Marshal(identityValue)
		if err != nil {
			return err
		}
		entry.Payload = encoded
		found = true
	}
	if !found {
		return errors.New("recorded session has no semantic generation identity event")
	}
	return nil
}

func flipSHA256(value string) string {
	if value == "" {
		return "0"
	}
	if value[0] == '0' {
		return "1" + value[1:]
	}
	return "0" + value[1:]
}

func persistentAcceptanceHasArgument(arguments []string, want string) bool {
	for _, argument := range arguments {
		if argument == want {
			return true
		}
	}
	return false
}
