package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/telemetry"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/test/acceptance/lspdriver"
	"github.com/omnilsp/omni/test/acceptance/report"
	"github.com/omnilsp/omni/test/acceptance/toolversion"
	"github.com/omnilsp/omni/test/acceptance/upstream"
)

const (
	semanticAcceptanceModeEnv = "OMNILSP_ACCEPTANCE"
	semanticAcceptanceBinEnv  = "OMNILSP_BIN"
	semanticReportPathEnv     = "OMNILSP_ACCEPTANCE_REPORT"
	semanticRunIDEnv          = "OMNILSP_RUN_ID"
)

type semanticLocation struct {
	URI       string `json:"uri"`
	StartLine uint32 `json:"startLine"`
	StartChar uint32 `json:"startCharacter"`
	EndLine   uint32 `json:"endLine"`
	EndChar   uint32 `json:"endCharacter"`
}

type semanticWireLocation struct {
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

type semanticWorkspaceEditRange struct {
	Start struct {
		Line      uint32 `json:"line"`
		Character uint32 `json:"character"`
	} `json:"start"`
	End struct {
		Line      uint32 `json:"line"`
		Character uint32 `json:"character"`
	} `json:"end"`
}

type semanticWorkspaceTextEdit struct {
	Range   semanticWorkspaceEditRange `json:"range"`
	NewText string                     `json:"newText"`
}

type semanticWorkspaceTextDocumentEdit struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Edits []semanticWorkspaceTextEdit `json:"edits"`
}

type semanticEditObservation struct {
	URI       string `json:"uri"`
	StartLine uint32 `json:"startLine"`
	StartChar uint32 `json:"startCharacter"`
	EndLine   uint32 `json:"endLine"`
	EndChar   uint32 `json:"endCharacter"`
	NewText   string `json:"newText"`
}

type semanticDiagnostic struct {
	Severity *int            `json:"severity,omitempty"`
	Code     json.RawMessage `json:"code,omitempty"`
	Source   string          `json:"source,omitempty"`
	Message  string          `json:"message"`
	Range    struct {
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

type semanticPublishDiagnostics struct {
	URI         string               `json:"uri"`
	Version     *int                 `json:"version,omitempty"`
	Diagnostics []semanticDiagnostic `json:"diagnostics"`
}

type semanticLanguageCase struct {
	name                 string
	toolNames            []string
	queryFile            string
	queryToken           string
	definitionFile       string
	definitionURI        string
	definition           semanticLocation
	call                 semanticLocation
	upstreamBinary       string
	upstreamArgs         []string
	additionalReferences []semanticLocation
}

// TestS20_RealProcessSemanticAcceptance uses one real OmniLSP stdio process
// for Go, C, C++, Rust, Python, TypeScript, and JavaScript. Its report keeps
// normalized locations and explicit not-verified outcomes for blocked or
// incomplete behavior.
func TestS20_RealProcessSemanticAcceptance(t *testing.T) {
	if os.Getenv(semanticAcceptanceModeEnv) != "1" {
		t.Skip("not run: set OMNILSP_ACCEPTANCE=1 to enable real-process semantic acceptance")
	}

	runID := strings.TrimSpace(os.Getenv(semanticRunIDEnv))
	if runID == "" {
		runID = fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	}
	evidence := report.New(runID)
	evidence.Limits["definitionAndReferences"] = "normalized target URI/range and exact queried-call range"
	evidence.Limits["candidateProcess"] = map[string]any{"maxConcurrent": 8, "maxQueue": 64, "GOMAXPROCS": 8}
	evidence.Environment["goos"] = runtime.GOOS
	evidence.Environment["goarch"] = runtime.GOARCH
	evidence.Environment["acceptanceMode"] = "explicit"
	evidence.Environment["acceptanceRunID"] = runID
	evidence.Environment["client"] = "test/acceptance/lspdriver; LSP 3.17; UTF-16"
	evidence.Environment["filesystem"] = semanticFilesystemType(os.TempDir())
	evidence.Environment["osVersion"] = semanticOSVersion()
	evidence.Environment["candidateMaxConcurrent"] = "8"
	evidence.Environment["candidateMaxQueue"] = "64"
	evidence.Environment["candidateGOMAXPROCS"] = "8"
	evidence.Environment["cpu"] = strings.TrimSpace(os.Getenv("OMNILSP_CPU_MODEL"))
	if evidence.Environment["cpu"] == "" {
		evidence.Environment["cpu"] = strings.TrimSpace(os.Getenv("PROCESSOR_IDENTIFIER"))
	}
	if evidence.Environment["cpu"] == "" {
		evidence.Environment["cpu"] = "unknown: CPU model unavailable"
	}
	missingMetadata := []string{}
	for _, key := range []string{"goos", "goarch", "osVersion", "filesystem", "cpu", "client"} {
		value := strings.ToLower(strings.TrimSpace(evidence.Environment[key]))
		if value == "" || strings.HasPrefix(value, "unknown") || value == "unavailable" {
			missingMetadata = append(missingMetadata, key+"="+evidence.Environment[key])
		}
	}
	metadataCheck := report.Check{ID: runID + "/S20/environment-metadata", Status: report.Passed, Summary: "OS, filesystem, CPU, and protocol client metadata were observed", Observed: map[string]any{"goos": evidence.Environment["goos"], "goarch": evidence.Environment["goarch"], "osVersion": evidence.Environment["osVersion"], "filesystem": evidence.Environment["filesystem"], "cpu": evidence.Environment["cpu"], "client": evidence.Environment["client"]}}
	if len(missingMetadata) > 0 {
		metadataCheck.Status = report.NotVerified
		metadataCheck.Summary = "required environment metadata could not be observed: " + strings.Join(missingMetadata, "; ")
		evidence.Skips = append(evidence.Skips, report.Skip{ID: metadataCheck.ID, Reason: metadataCheck.Summary})
	}
	evidence.Checks = append(evidence.Checks, metadataCheck)
	defer func() {
		evidence.Checks = append(evidence.Checks, semanticAccuracyKPIReport(runID, evidence.Checks))
		evidence.Finalize(time.Now())
		path := os.Getenv(semanticReportPathEnv)
		if path == "" {
			path = filepath.Join(os.TempDir(), "omnilsp-semantic-"+strings.ReplaceAll(runID, "/", "-")+".json")
		}
		if err := report.Write(path, evidence); err != nil {
			t.Errorf("write semantic acceptance report: %v", err)
			return
		}
		t.Logf("semantic acceptance JSON report: %s (decision=%s)", path, evidence.Decision)
	}()

	binary, err := semanticResolveBinary(t)
	if err != nil {
		evidence.Checks = append(evidence.Checks, report.Check{ID: "S20/candidate", Status: report.NotVerified, Summary: "blocked: " + err.Error()})
		evidence.Skips = append(evidence.Skips, report.Skip{ID: "S20/candidate", Reason: err.Error()})
		t.Logf("candidate unavailable; structured semantic decision will remain not_verified: %v", err)
		return
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		evidence.Checks = append(evidence.Checks, report.Check{ID: "S20/candidate", Status: report.NotVerified, Summary: "blocked: cannot read candidate binary"})
		evidence.Skips = append(evidence.Skips, report.Skip{ID: "S20/candidate", Reason: err.Error()})
		t.Logf("candidate binary unreadable; structured semantic decision will remain not_verified: %v", err)
		return
	}
	digest := sha256.Sum256(data)
	evidence.Candidate = report.Candidate{Binary: binary, SHA256: "sha256:" + hex.EncodeToString(digest[:]), Revision: semanticGitRevision(t)}
	toolVersions := semanticToolVersions()
	for name, version := range toolVersions {
		evidence.Environment["toolVersion."+name] = version
	}

	workspace := t.TempDir()
	files, cases := semanticFixtureSet(workspace)
	if err := semanticWriteFiles(workspace, files); err != nil {
		evidence.Checks = append(evidence.Checks, report.Check{ID: "S20/fixture", Status: report.Failed, Summary: err.Error()})
		evidence.Errors = append(evidence.Errors, err.Error())
		t.Errorf("prepare semantic fixtures: %v", err)
		return
	}
	contentSHA := semanticDigestInWorkspace(files, workspace)
	corpusRunSHA := sha256.Sum256([]byte(contentSHA + "\nrun-id:" + runID))
	evidence.Corpus = report.Corpus{Name: "multi-language semantic fixtures namespaced by acceptance run ID " + runID, SHA256: "sha256:" + hex.EncodeToString(corpusRunSHA[:])}
	evidence.Environment["corpusContentSHA256"] = contentSHA

	session := lspdriver.Start(t, binary, workspace, []string{
		"OMNILSP_TRUST=trusted", "OMNILSP_LOG_LEVEL=error", "OMNILSP_MAX_CONCURRENT=8",
		"OMNILSP_MAX_QUEUE=64", "GOMAXPROCS=8", "PATH=" + semanticAcceptanceToolPath(),
		"OMNILSP_INDEX_DIR=" + filepath.Join(workspace, ".omnilsp-index"),
		"OMNILSP_S18_COMPLETION_PHASE_TRACE=",
	})
	defer session.Close(t)
	session.Initialize(t)

	for _, testCase := range cases {
		testCase := testCase
		var languageCheck report.Check
		t.Run(testCase.name, func(st *testing.T) {
			languageCheck = runSemanticLanguageCase(st, session, workspace, files, testCase, toolVersions)
			languageCheck.ID = runID + "/" + languageCheck.ID
			evidence.Checks = append(evidence.Checks, languageCheck)
			if languageCheck.Status == report.Failed {
				st.Errorf("%s semantic assertion failed: %s", testCase.name, languageCheck.Summary)
			}
		})
		for _, staleCheck := range semanticRunStaleFeatureProbes(t, session, workspace, files, testCase, languageCheck) {
			staleCheck.ID = runID + "/" + staleCheck.ID
			evidence.Checks = append(evidence.Checks, staleCheck)
			if staleCheck.Status == report.Failed {
				t.Errorf("%s failed: %s", staleCheck.ID, staleCheck.Summary)
			}
		}
	}
	buildContext := runSemanticCPPBuildContextCase(t, session, workspace, files, toolVersions)
	buildContext.ID = runID + "/" + buildContext.ID
	evidence.Checks = append(evidence.Checks, buildContext)
	if buildContext.Status == report.Failed {
		t.Errorf("%s failed: %s", buildContext.ID, buildContext.Summary)
	}

	// The source-safety cases run through the same child process after the
	// cross-language definitions/references have completed.
	for _, check := range runGoSafetyCases(t, session, workspace, files, toolVersions) {
		check.ID = runID + "/" + check.ID
		evidence.Checks = append(evidence.Checks, check)
		if check.Status == report.Failed {
			t.Errorf("%s failed: %s", check.ID, check.Summary)
		}
	}
	positionStaleCheck := semanticRunGoPositionStaleProbe(t, session, workspace)
	positionStaleCheck.ID = runID + "/" + positionStaleCheck.ID
	evidence.Checks = append(evidence.Checks, positionStaleCheck)
	if positionStaleCheck.Status == report.Failed {
		t.Errorf("%s failed: %s", positionStaleCheck.ID, positionStaleCheck.Summary)
	}
}

func semanticFixtureSet(root string) (map[string]string, []semanticLanguageCase) {
	files := map[string]string{
		"go-case/go.mod":                 "module acceptance/go\n\ngo 1.26\n",
		"go-case/target.go":              "package acceptance\nfunc target() int { return 7 }\nfunc Exported() int { return target() }\n",
		"go-case/use.go":                 "package acceptance\nfunc useTarget() int { return target() }\nfunc useExported() int { return Exported() }\n// missingProbe\n",
		"go-case/unicode_crlf.go":        "package acceptance\r\nvar 世界 = 7\r\nfunc useWorld() int { return 世界 }\r\n",
		"go-broken/go.mod":               "module acceptance/broken\n\ngo 1.26\n",
		"go-broken/broken.go":            "package broken\nvar wanted int\nfunc use() {\n    _ = wanted\n}\nfunc broken( {\n",
		"go-safety/go.mod":               "module acceptance/safety\n\ngo 1.26\n",
		"go-safety/target.go":            "package safety\nfunc target() int { return 7 }\nfunc Exported() int { return target() }\n",
		"go-safety/use.go":               "package safety\nfunc useTarget() int { return target() }\nfunc useExported() int { return Exported() }\nfunc useLocal() int { localScopeValue := 1; return localScopeValue }\n// missingProbe\n",
		"include/acceptance/c-api.h":     "int target(void);\n",
		"c-api/impl.c":                   "#include <acceptance/c-api.h>\nint target(void) { return 7; }\n",
		"c-api/use.c":                    "#include <acceptance/c-api.h>\nint use_target(void) { return target(); }\n// missingProbe\n",
		"include/acceptance/cpp-api.hpp": "consteval int cxx20_marker() { return 20; }\nint target(void);\n",
		"cpp-api/impl.cpp":               "#include <acceptance/cpp-api.hpp>\nstatic_assert(cxx20_marker() == 20);\nint target(void) { return 7; }\n",
		"cpp-api/use.cpp":                "#include <acceptance/cpp-api.hpp>\nstatic_assert(cxx20_marker() == 20);\nint use_target() { return target(); }\n// missingProbe\n",
		"Cargo.toml":                     "[workspace]\nmembers = [\"rust-case\"]\nresolver = \"2\"\n",
		"rust-case/Cargo.toml":           "[package]\nname = \"acceptance_rust\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"rust-case/src/lib.rs":           "pub fn target() -> i32 { 7 }\npub fn use_target() -> i32 { target() }\n// missingProbe\n",
		"pyrightconfig.json":             "{\"include\":[\"python-case\"],\"pythonVersion\":\"3.11\"}\n",
		"python-case/target_mod.py":      "def target() -> int:\n    return 7\n",
		"python-case/use_mod.py":         "from target_mod import target\ndef use_target() -> int:\n    return target()\n# missingProbe\n",
		"tsconfig.json":                  "{\"compilerOptions\":{\"target\":\"ES2020\",\"module\":\"commonjs\",\"strict\":true},\"include\":[\"typescript-case/**/*.ts\"]}\n",
		"typescript-case/target.ts":      "export function target(): number { return 7; }\n",
		"typescript-case/use.ts":         "import { target } from \"./target\";\nexport function useTarget(): number { return target(); }\n// missingProbe\n",
		"jsconfig.json":                  "{\"compilerOptions\":{\"target\":\"ES2020\",\"module\":\"commonjs\",\"checkJs\":true},\"include\":[\"javascript-case/**/*.js\"]}\n",
		"javascript-case/target.js":      "export function target() { return 7; }\n",
		"javascript-case/use.js":         "import { target } from \"./target.js\";\nexport function useTarget() { return target(); }\n// missingProbe\n",
	}

	compileDB := []map[string]any{}
	for _, item := range []struct{ file, compiler, standard string }{
		{"c-api/impl.c", "clang", "-std=c17"},
		{"c-api/use.c", "clang", "-std=c17"},
		{"cpp-api/impl.cpp", "clang++", "-std=c++20"},
		{"cpp-api/use.cpp", "clang++", "-std=c++20"},
	} {
		abs := filepath.Join(root, filepath.FromSlash(item.file))
		args := []string{item.compiler, item.standard, "-I", filepath.Join(root, "include"), "-c", abs}
		compileDB = append(compileDB, map[string]any{"directory": root, "file": abs, "arguments": args})
	}
	compileDBJSON, _ := json.Marshal(compileDB)
	files["compile_commands.json"] = string(compileDBJSON) + "\n"

	makeCase := func(name string, tools []string, queryFile, token, defFile string, defLine, defChar, defEnd uint32) semanticLanguageCase {
		queryText := files[queryFile]
		callLine, callChar := semanticPositionOf(queryText, token, 0)
		defURI := semanticCanonicalURI(uri.FromPath(filepath.Join(root, filepath.FromSlash(defFile))).String())
		return semanticLanguageCase{
			name: name, toolNames: tools, queryFile: queryFile, queryToken: token, definitionFile: defFile, definitionURI: defURI,
			definition: semanticLocation{URI: defURI, StartLine: defLine, StartChar: defChar, EndLine: defLine, EndChar: defEnd},
			call:       semanticLocation{URI: semanticCanonicalURI(uri.FromPath(filepath.Join(root, filepath.FromSlash(queryFile))).String()), StartLine: callLine, StartChar: callChar, EndLine: callLine, EndChar: callChar + uint32(len([]rune(strings.TrimSuffix(token, "()"))))},
		}
	}

	cases := []semanticLanguageCase{
		makeCase("Go", []string{"go", "gopls"}, "go-case/use.go", "target()", "go-case/target.go", 1, 5, 11),
		makeCase("C", []string{"clangd", "clang"}, "c-api/use.c", "target()", "c-api/impl.c", 1, 4, 10),
		makeCase("C++", []string{"clangd", "clang++"}, "cpp-api/use.cpp", "target()", "cpp-api/impl.cpp", 2, 4, 10),
		makeCase("Rust", []string{"rust-analyzer", "rustc", "cargo"}, "rust-case/src/lib.rs", "target()", "rust-case/src/lib.rs", 0, 7, 13),
		makeCase("Python", []string{"pyright-langserver"}, "python-case/use_mod.py", "target()", "python-case/target_mod.py", 0, 4, 10),
		makeCase("TypeScript", []string{"typescript-language-server", "node", "tsc"}, "typescript-case/use.ts", "target()", "typescript-case/target.ts", 0, 16, 22),
		makeCase("JavaScript", []string{"typescript-language-server", "node", "tsc"}, "javascript-case/use.js", "target()", "javascript-case/target.js", 0, 16, 22),
	}
	for i := range cases {
		cases[i].upstreamBinary, cases[i].upstreamArgs = semanticUpstreamSpec(cases[i].name)
		if cases[i].name == "C" || cases[i].name == "C++" {
			header := "include/acceptance/c-api.h"
			line := uint32(0)
			if cases[i].name == "C++" {
				header = "include/acceptance/cpp-api.hpp"
				line = 1
			}
			cases[i].additionalReferences = []semanticLocation{{
				URI:       semanticCanonicalURI(uri.FromPath(filepath.Join(root, filepath.FromSlash(header))).String()),
				StartLine: line, StartChar: 4, EndLine: line, EndChar: 10,
			}}
		}
	}
	return files, cases
}

func runSemanticLanguageCase(t *testing.T, session *lspdriver.Session, workspace string, files map[string]string, tc semanticLanguageCase, toolVersions map[string]string) report.Check {
	t.Helper()
	check := report.Check{ID: "S20/language/" + strings.ToLower(strings.ReplaceAll(tc.name, "+", "plus")), Status: report.NotVerified}
	missing := []string{}
	for _, tool := range tc.toolNames {
		if _, err := semanticLookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	missing = append(missing, semanticLockedToolVersionProblems(tc.toolNames, toolVersions)...)
	missing = uniqueStrings(missing...)
	if len(missing) > 0 {
		check.Summary = "blocked: required acceptance tools missing: " + strings.Join(missing, ", ")
		check.Observed = map[string]any{"missingTools": missing, "featureStatus": "not_verified"}
		return check
	}
	upstreamPath, upstreamErr := semanticLookPath(tc.upstreamBinary)
	if upstreamErr != nil {
		check.Summary = "blocked: pinned upstream unavailable: " + upstreamErr.Error()
		check.Observed = map[string]any{"featureStatus": "not_verified", "upstream": tc.upstreamBinary}
		return check
	}

	queryPath := filepath.Join(workspace, filepath.FromSlash(tc.queryFile))
	queryURI := uri.FromPath(queryPath).String()
	candidateProgressCursor := session.EventCursor()
	for _, rel := range uniqueStrings(tc.queryFile, tc.definitionFile) {
		if rel == "" {
			continue
		}
		fileText, ok := files[rel]
		if !ok {
			continue
		}
		session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
			"uri":        uri.FromPath(filepath.Join(workspace, filepath.FromSlash(rel))).String(),
			"languageId": languageIDForPath(rel), "version": 1, "text": fileText,
		}})
	}

	line, char := semanticPositionOf(files[tc.queryFile], tc.queryToken, 0)
	params := map[string]any{"textDocument": map[string]string{"uri": queryURI}, "position": map[string]uint32{"line": line, "character": char}}
	refsParams := map[string]any{
		"textDocument": map[string]string{"uri": queryURI},
		"position":     map[string]uint32{"line": line, "character": char},
		"context":      map[string]bool{"includeDeclaration": true},
	}
	candidateReadinessTimeout := 30 * time.Second
	if tc.name == "C" || tc.name == "C++" {
		// clangd's project index may still be warming after the full Fast suite;
		// preserve the same complete-result and three-snapshot requirements.
		candidateReadinessTimeout = 60 * time.Second
	}
	definitions, references, candidateReadiness, candidateReadyErr := semanticWaitCandidateLocations(
		session, candidateProgressCursor, params, refsParams, tc.definition, tc.call, candidateReadinessTimeout, tc.additionalReferences...,
	)
	if candidateReadyErr != nil {
		check.Status = report.NotVerified
		check.Summary = "not_verified: candidate semantic results did not become complete and stable: " + candidateReadyErr.Error()
		check.Observed = map[string]any{
			"definition": definitions, "references": references,
			"candidateReadiness": candidateReadiness, "featureStatus": "not_verified",
		}
		return check
	}
	if len(definitions) == 0 {
		check.Status = report.NotVerified
		check.Summary = "partial: definition returned no semantic locations"
		check.Observed = map[string]any{"definition": definitions, "expectedDefinition": tc.definition, "candidateReadiness": candidateReadiness, "featureStatus": "not_verified"}
		return check
	}
	if !containsSemanticLocation(definitions, tc.definition) {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("definition did not resolve to expected normalized target; got %+v want %+v", definitions, tc.definition)
		check.Observed = map[string]any{"definition": definitions, "expectedDefinition": tc.definition, "candidateReadiness": candidateReadiness}
		return check
	}
	if len(references) == 0 {
		check.Status = report.NotVerified
		check.Summary = "partial: references returned no semantic locations"
		check.Observed = map[string]any{"definition": definitions, "references": references, "expectedDefinition": tc.definition, "expectedCall": tc.call, "candidateReadiness": candidateReadiness, "featureStatus": "not_verified"}
		return check
	}
	if !containsSemanticLocation(references, tc.definition) || !containsSemanticLocation(references, tc.call) {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("references omitted the exact declaration or queried call; got %+v", references)
		check.Observed = map[string]any{"definition": definitions, "references": references, "expectedDefinition": tc.definition, "expectedCall": tc.call, "candidateReadiness": candidateReadiness}
		return check
	}
	upstreamArgs := append([]string(nil), tc.upstreamArgs...)
	if tc.name == "C" || tc.name == "C++" {
		upstreamArgs = append(upstreamArgs, "--compile-commands-dir="+workspace)
	}
	upstreamSession, err := upstream.Start(upstreamPath, upstreamArgs, workspace, []string{"PATH=" + semanticAcceptanceToolPath()})
	if err != nil {
		check.Status = report.NotVerified
		check.Summary = "blocked: pinned upstream failed to start: " + err.Error()
		check.Observed = map[string]any{"definition": definitions, "references": references, "upstream": tc.upstreamBinary, "featureStatus": "not_verified"}
		return check
	}
	defer upstreamSession.Close()
	upstreamProgressCursor := upstreamSession.NotificationCursor()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	upstreamInitErr := upstreamSession.Initialize(ctx, workspace)
	cancel()
	if upstreamInitErr != nil {
		check.Status = report.NotVerified
		check.Summary = "blocked: pinned upstream initialize failed: " + upstreamInitErr.Error()
		check.Observed = map[string]any{"definition": definitions, "references": references, "upstream": tc.upstreamBinary, "featureStatus": "not_verified"}
		return check
	}
	for _, rel := range uniqueStrings(tc.queryFile, tc.definitionFile) {
		if text, ok := files[rel]; ok {
			if err := upstreamSession.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
				"uri": uri.FromPath(filepath.Join(workspace, filepath.FromSlash(rel))).String(), "languageId": languageIDForPath(rel), "version": 1, "text": text,
			}}); err != nil {
				check.Status = report.NotVerified
				check.Summary = "blocked: pinned upstream didOpen failed: " + err.Error()
				return check
			}
		}
	}
	upstreamDefinitions, upstreamReferences, upstreamReadiness, upstreamReadyErr := semanticWaitUpstreamLocations(
		upstreamSession, upstreamProgressCursor, params, refsParams, tc.definition, tc.call, 45*time.Second, tc.additionalReferences...,
	)
	if upstreamReadyErr != nil {
		check.Status = report.NotVerified
		check.Summary = "blocked: pinned upstream semantics did not become complete and stable: " + upstreamReadyErr.Error()
		check.Observed = map[string]any{
			"definition": definitions, "references": references,
			"upstreamDefinition": upstreamDefinitions, "upstreamReferences": upstreamReferences,
			"upstreamReadiness": upstreamReadiness, "featureStatus": "not_verified",
		}
		return check
	}
	if !reflect.DeepEqual(definitions, upstreamDefinitions) || !reflect.DeepEqual(references, upstreamReferences) {
		check.Status = report.Failed
		check.Summary = "normalized OmniLSP definition/references differ from pinned upstream results"
		check.Observed = map[string]any{"definition": definitions, "references": references, "upstreamDefinition": upstreamDefinitions, "upstreamReferences": upstreamReferences}
		return check
	}
	negativeLine, negativeChar := semanticPositionOf(files[tc.queryFile], "missingProbe", 0)
	negativeParams := map[string]any{"textDocument": map[string]string{"uri": queryURI}, "position": map[string]uint32{"line": negativeLine, "character": negativeChar}}
	candidateNegativeDefinitions, candidateNegativeReferences, candidateNegativeErr := semanticGoPair(session, negativeParams)
	if candidateNegativeErr == nil {
		candidateNegativeErr = semanticVerifyNegativeEvidence(session, queryURI)
	}
	if candidateNegativeErr != nil {
		check.Status = report.NotVerified
		check.Summary = "not_verified: candidate negative definition/reference probe failed: " + candidateNegativeErr.Error()
		check.Observed = map[string]any{"definition": definitions, "references": references, "upstreamDefinition": upstreamDefinitions, "upstreamReferences": upstreamReferences, "negativeProbe": negativeParams, "candidateNegativeError": candidateNegativeErr.Error(), "featureStatus": "not_verified"}
		return check
	}
	upstreamNegativeDefinitions, upstreamNegativeReferences := []semanticLocation{}, []semanticLocation{}
	upstreamNegativeErrors := map[string]string{}
	upstreamNegativeOracleEmpty := false
	if tc.name == "Go" {
		negativeResult := semanticGoNegativeOraclePair(upstreamSession, negativeParams)
		upstreamNegativeDefinitions = negativeResult.Definition
		upstreamNegativeReferences = negativeResult.References
		upstreamNegativeErrors = map[string]string{}
		if negativeResult.DefinitionError != "" {
			upstreamNegativeErrors["definition"] = negativeResult.DefinitionError
		}
		if negativeResult.ReferencesError != "" {
			upstreamNegativeErrors["references"] = negativeResult.ReferencesError
		}
		if negativeResult.Err != nil {
			check.Status = report.NotVerified
			check.Summary = "not_verified: pinned upstream Go negative probe failed outside the exact no-identifier outcome: " + negativeResult.Err.Error()
			check.Observed = map[string]any{
				"definition": definitions, "references": references,
				"upstreamDefinition": upstreamDefinitions, "upstreamReferences": upstreamReferences,
				"upstreamReadiness": upstreamReadiness, "negativeProbe": negativeParams,
				"upstreamNegativeErrors": upstreamNegativeErrors, "featureStatus": "not_verified",
			}
			return check
		}
		upstreamNegativeOracleEmpty = negativeResult.DefinitionNoLocation && negativeResult.ReferencesNoLocation
	} else {
		var upstreamNegativeErr error
		upstreamNegativeDefinitions, upstreamNegativeReferences, upstreamNegativeErr = semanticGoPair(upstreamSession, negativeParams)
		if upstreamNegativeErr != nil {
			check.Status = report.NotVerified
			check.Summary = "not_verified: pinned upstream negative definition/reference probe failed: " + upstreamNegativeErr.Error()
			check.Observed = map[string]any{"definition": definitions, "references": references, "upstreamDefinition": upstreamDefinitions, "upstreamReferences": upstreamReferences, "upstreamReadiness": upstreamReadiness, "negativeProbe": negativeParams, "upstreamNegativeError": upstreamNegativeErr.Error(), "featureStatus": "not_verified"}
			return check
		}
		upstreamNegativeOracleEmpty = len(upstreamNegativeDefinitions) == 0 && len(upstreamNegativeReferences) == 0
	}
	if !upstreamNegativeOracleEmpty {
		check.Status = report.NotVerified
		check.Summary = "not_verified: pinned upstream resolved the negative probe or could not establish both empty outcomes"
		check.Observed = map[string]any{
			"definition": definitions, "references": references,
			"upstreamDefinition": upstreamDefinitions, "upstreamReferences": upstreamReferences,
			"upstreamReadiness": upstreamReadiness, "negativeProbe": negativeParams,
			"upstreamNegativeDefinitions": upstreamNegativeDefinitions,
			"upstreamNegativeReferences":  upstreamNegativeReferences,
			"upstreamNegativeErrors":      upstreamNegativeErrors, "featureStatus": "not_verified",
		}
		return check
	}
	kpiRecords := []telemetry.Record{
		semanticLocationKPI("definition", tc.name, tc.upstreamBinary, toolVersions, definitions, upstreamDefinitions, candidateNegativeDefinitions, upstreamNegativeOracleEmpty),
		semanticLocationKPI("references", tc.name, tc.upstreamBinary, toolVersions, references, upstreamReferences, candidateNegativeReferences, upstreamNegativeOracleEmpty),
	}
	if tc.name == "Go" {
		// The pinned gopls no-identifier refusal is retained as raw evidence and
		// counts only as a no-location response for this negative cursor query.
		kpiRecords = append(kpiRecords, semanticLocationKPI("position-mapping", tc.name, tc.upstreamBinary, toolVersions, definitions, upstreamDefinitions, candidateNegativeDefinitions, upstreamNegativeOracleEmpty))
	}
	check.Observed = map[string]any{
		"definition": definitions, "references": references,
		"upstreamDefinition": upstreamDefinitions, "upstreamReferences": upstreamReferences,
		"upstreamReadiness": upstreamReadiness,
		"negativeProbe": map[string]any{
			"position":            []uint32{negativeLine, negativeChar},
			"candidateDefinition": candidateNegativeDefinitions, "candidateReferences": candidateNegativeReferences,
			"upstreamDefinition": upstreamNegativeDefinitions, "upstreamReferences": upstreamNegativeReferences,
			"upstreamErrors": upstreamNegativeErrors,
			"expectedEmpty":  true,
		},
		"kpiObservations": kpiRecords,
	}
	if len(candidateNegativeDefinitions) != 0 || len(candidateNegativeReferences) != 0 {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("candidate returned locations for comment-only negative probes while pinned upstream returned none: definition=%+v references=%+v", candidateNegativeDefinitions, candidateNegativeReferences)
		return check
	}
	check.Status = report.Passed
	check.Summary = "normalized definition/references match the pinned upstream for positive symbols and return no locations for the negative probe"
	check.Observed["upstream"] = tc.upstreamBinary
	check.Observed["expectedDefinition"] = tc.definition
	check.Observed["expectedCall"] = tc.call
	return check
}

func semanticLocationKPI(feature, language, oracleBinary string, toolVersions map[string]string, candidate, pinned, candidateNegative []semanticLocation, negativeOracleEmpty bool) telemetry.Record {
	inputs := semanticLocationKPIInputs(candidate, pinned, candidateNegative, negativeOracleEmpty)
	inputs.Refusals = telemetry.Sampled(0)
	inputs.Requests = telemetry.Sampled(2) // candidate answered the positive and negative query.
	return telemetry.NewRecord(telemetry.Dimension{
		Feature: feature, Language: language, Backend: semanticCandidateBackend(language),
		Oracle: semanticOracleLabel(oracleBinary, toolVersions),
	}, inputs)
}

// semanticLocationKPIInputs measures one positive and one negative result-set
// probe against the pinned server. Confusion counts are probe outcomes: a
// positive probe can add FP and FN when it returns extras and misses expected
// locations; an empty candidate result on an upstream-empty negative probe is
// a true negative. File and position rates are based on returned positive
// locations, where upstream URI/range observations provide the comparison set.
func semanticLocationKPIInputs(candidate, pinned, candidateNegative []semanticLocation, negativeOracleEmpty bool) telemetry.Inputs {
	var tp, fp, fn int64
	pinnedSet := make(map[semanticLocation]struct{}, len(pinned))
	for _, location := range pinned {
		pinnedSet[location] = struct{}{}
	}
	candidateSet := make(map[semanticLocation]struct{}, len(candidate))
	for _, location := range candidate {
		candidateSet[location] = struct{}{}
	}
	if len(pinned) > 0 {
		for location := range candidateSet {
			if _, ok := pinnedSet[location]; ok {
				tp = 1
				break
			}
		}
		for location := range candidateSet {
			if _, ok := pinnedSet[location]; !ok {
				fp = 1
				break
			}
		}
		for location := range pinnedSet {
			if _, ok := candidateSet[location]; !ok {
				fn = 1
				break
			}
		}
	}
	var inputs telemetry.Inputs
	if len(pinned) > 0 {
		inputs.TruePositive = telemetry.Sampled(tp)
		inputs.FalsePositive = telemetry.Sampled(fp)
		inputs.FalseNegative = telemetry.Sampled(fn)
	}
	if negativeOracleEmpty {
		tn := int64(0)
		if len(candidateNegative) == 0 {
			tn = 1
		} else {
			fp++
		}
		inputs.TruePositive = telemetry.Sampled(tp)
		inputs.FalsePositive = telemetry.Sampled(fp)
		inputs.FalseNegative = telemetry.Sampled(fn)
		inputs.TrueNegative = telemetry.Sampled(tn)
	}

	// Position/file comparisons need candidate results and a pinned result set.
	// The positive probe always has a pinned result in the acceptance case.
	if len(pinned) > 0 {
		wrongFile, locationResults := int64(0), int64(len(candidate))
		pinnedURIs := make(map[string]struct{}, len(pinned))
		pinnedByURI := make(map[string]map[semanticLocation]struct{}, len(pinned))
		for _, location := range pinned {
			pinnedURIs[location.URI] = struct{}{}
			if pinnedByURI[location.URI] == nil {
				pinnedByURI[location.URI] = make(map[semanticLocation]struct{})
			}
			pinnedByURI[location.URI][location] = struct{}{}
		}
		positionFailures, positionSamples := int64(0), int64(0)
		for _, location := range candidate {
			if _, ok := pinnedURIs[location.URI]; !ok {
				wrongFile++
				continue
			}
			positionSamples++
			if _, ok := pinnedByURI[location.URI][location]; !ok {
				positionFailures++
			}
		}
		inputs.WrongFileLocations = telemetry.Sampled(wrongFile)
		inputs.LocationResults = telemetry.Sampled(locationResults)
		inputs.PositionMappingFailures = telemetry.Sampled(positionFailures)
		inputs.PositionMappingSamples = telemetry.Sampled(positionSamples)
	}
	return inputs
}

func semanticCandidateBackend(language string) string {
	switch language {
	case "Go":
		return "golang"
	case "C", "C++":
		return "ccls/clangd"
	case "Rust":
		return "rustanalyzer"
	case "Python":
		return "pyright"
	case "TypeScript", "JavaScript":
		return "typescript"
	default:
		return "unknown"
	}
}

func semanticOracleLabel(binary string, versions map[string]string) string {
	key := binary
	if binary == "pyright-langserver" {
		key = "pyright"
	}
	version := strings.TrimSpace(versions[key])
	if version == "" || version == "missing" {
		return binary
	}
	return binary + "@" + version
}

type semanticKPIMetricObservation struct {
	Dimension telemetry.Dimension `json:"dimension"`
	Name      string              `json:"name"`
	Metric    telemetry.Metric    `json:"metric"`
}

func semanticAccuracyKPIReport(runID string, checks []report.Check) report.Check {
	byDimension := make(map[string]telemetry.Record)
	duplicates := []string{}
	metricObservations := []semanticKPIMetricObservation{}
	metricConflicts := []string{}
	for _, check := range checks {
		observations, ok := check.Observed["kpiObservations"].([]telemetry.Record)
		if ok {
			for _, record := range observations {
				key := semanticKPIDimensionKey(record.Dimension)
				if semanticEditValidationNotApplicable(record.Dimension) {
					metric := record.Metrics.EditValidationFailureRate
					if metric.Numerator != nil || metric.Denominator != nil || metric.Value != nil ||
						(metric.Status != telemetry.MetricNotVerified && !(metric.Status == telemetry.MetricNotApplicable && metric.Reason != "")) {
						metricConflicts = append(metricConflicts, key+" (read-only dimension contains edit-validation samples)")
					}
				}
				if _, exists := byDimension[key]; exists {
					duplicates = append(duplicates, key)
					continue
				}
				byDimension[key] = record
			}
		}
		if updates, ok := check.Observed["kpiMetricObservations"].([]semanticKPIMetricObservation); ok {
			metricObservations = append(metricObservations, updates...)
		}
	}

	const oracleUnknown = "not observed"
	for _, language := range []string{"Go", "C", "C++", "Rust", "Python", "TypeScript", "JavaScript"} {
		for _, feature := range []string{"definition", "references"} {
			dimension := telemetry.Dimension{Feature: feature, Language: language, Backend: semanticCandidateBackend(language), Oracle: oracleUnknown}
			key := semanticKPIDimensionKey(dimension)
			if _, exists := byDimension[key]; !exists {
				byDimension[key] = telemetry.NewRecord(dimension, telemetry.Inputs{})
			}
		}
	}
	positionDimension := telemetry.Dimension{Feature: "position-mapping", Language: "Go", Backend: semanticCandidateBackend("Go"), Oracle: oracleUnknown}
	if _, exists := byDimension[semanticKPIDimensionKey(positionDimension)]; !exists {
		byDimension[semanticKPIDimensionKey(positionDimension)] = telemetry.NewRecord(positionDimension, telemetry.Inputs{})
	}
	renameDimension := telemetry.Dimension{Feature: "rename", Language: "Go", Backend: semanticCandidateBackend("Go"), Oracle: oracleUnknown}
	if _, exists := byDimension[semanticKPIDimensionKey(renameDimension)]; !exists {
		byDimension[semanticKPIDimensionKey(renameDimension)] = telemetry.NewRecord(renameDimension, telemetry.Inputs{})
	}
	seenMetricUpdates := map[string]telemetry.Metric{}
	for _, update := range metricObservations {
		dimensionKey := semanticKPIDimensionKey(update.Dimension)
		record, exists := byDimension[dimensionKey]
		if !exists {
			metricConflicts = append(metricConflicts, dimensionKey+" (metric update has no record)")
			continue
		}
		updateKey := dimensionKey + "\x00" + update.Name
		if earlier, found := seenMetricUpdates[updateKey]; found && !reflect.DeepEqual(earlier, update.Metric) {
			metricConflicts = append(metricConflicts, updateKey+" (conflicting metric updates)")
			continue
		}
		seenMetricUpdates[updateKey] = update.Metric
		current, known := semanticMetricByName(record.Metrics, update.Name)
		if !known {
			metricConflicts = append(metricConflicts, updateKey+" (unknown metric name)")
			continue
		}
		if update.Name == "editValidationFailureRate" && semanticEditValidationNotApplicable(record.Dimension) {
			metricConflicts = append(metricConflicts, updateKey+" (edit validation is not applicable to a read-only query dimension)")
			continue
		}
		if current.Status == telemetry.MetricObserved && update.Metric.Status != telemetry.MetricObserved {
			continue
		}
		if current.Status == telemetry.MetricObserved && !reflect.DeepEqual(current, update.Metric) {
			metricConflicts = append(metricConflicts, updateKey+" (observed metric conflicts with existing record)")
			continue
		}
		if !semanticSetMetric(&record.Metrics, update.Name, update.Metric) {
			metricConflicts = append(metricConflicts, updateKey+" (metric update could not be applied)")
			continue
		}
		byDimension[dimensionKey] = record
	}

	keys := make([]string, 0, len(byDimension))
	for key := range byDimension {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	records := make([]telemetry.Record, 0, len(keys))
	limitations := make([]map[string]string, 0)
	notApplicable := make([]map[string]string, 0)
	invalid, complete := false, true
	for _, key := range keys {
		record := byDimension[key]
		if semanticEditValidationNotApplicable(record.Dimension) {
			record.Metrics.EditValidationFailureRate = telemetry.NotApplicable(
				"edit_validation_failures/edit_validation_attempts",
				"the dimension returns read-only semantic locations; no WorkspaceEdit is produced or passed to edit validation",
			)
		}
		records = append(records, record)
		invalid = invalid || record.Invalid()
		complete = complete && record.Complete()
		for name, metric := range semanticKPIMetricMap(record.Metrics) {
			if metric.Status == telemetry.MetricNotVerified {
				limitations = append(limitations, map[string]string{
					"feature": record.Dimension.Feature, "language": record.Dimension.Language,
					"backend": record.Dimension.Backend, "metric": name,
					"reason": metric.Reason,
				})
			} else if metric.Status == telemetry.MetricNotApplicable {
				notApplicable = append(notApplicable, map[string]string{
					"feature": record.Dimension.Feature, "language": record.Dimension.Language,
					"backend": record.Dimension.Backend, "metric": name,
					"reason": metric.Reason,
				})
			}
		}
	}
	sort.Slice(limitations, func(i, j int) bool {
		left := limitations[i]["feature"] + "/" + limitations[i]["language"] + "/" + limitations[i]["metric"]
		right := limitations[j]["feature"] + "/" + limitations[j]["language"] + "/" + limitations[j]["metric"]
		return left < right
	})
	sort.Slice(notApplicable, func(i, j int) bool {
		left := notApplicable[i]["feature"] + "/" + notApplicable[i]["language"] + "/" + notApplicable[i]["metric"]
		right := notApplicable[j]["feature"] + "/" + notApplicable[j]["language"] + "/" + notApplicable[j]["metric"]
		return left < right
	})
	status, summary := report.NotVerified, "S20 KPI rows include uncollected or undefined applicable metrics; those dimensions remain not_verified"
	if invalid || len(duplicates) > 0 || len(metricConflicts) > 0 {
		status, summary = report.Failed, "S20 KPI evidence has impossible counts or duplicate feature/language/backend dimensions"
	} else if complete {
		status, summary = report.Passed, "all applicable S20 KPIs are observed for every feature/language/backend dimension; read-only dimensions are explicitly not_applicable for edit validation"
	}
	return report.Check{
		ID: runID + "/S20/accuracy-kpi", Status: status, Summary: summary,
		Threshold: map[string]any{
			"requiredLanguages": 7, "requiredFeatures": []string{"definition", "references"},
			"additionalDimensions": []map[string]string{
				{"feature": "position-mapping", "language": "Go", "backend": semanticCandidateBackend("Go")},
				{"feature": "rename", "language": "Go", "backend": semanticCandidateBackend("Go")},
			},
			"minimumRecords": 16,
		},
		Observed: map[string]any{
			"schema":              telemetry.KPIReportSchema,
			"records":             records,
			"recordCount":         len(records),
			"countingUnit":        "one semantic result-set probe; a positive probe contributes TP when it returns at least one pinned location, FP when it returns non-oracle locations, and FN when it omits pinned locations; a valid empty negative probe contributes TN when candidate output is empty, FP otherwise",
			"limitations":         limitations,
			"notApplicable":       notApplicable,
			"duplicateDimensions": duplicates,
			"metricConflicts":     metricConflicts,
		},
	}
}

func semanticEditValidationNotApplicable(dimension telemetry.Dimension) bool {
	switch dimension.Feature {
	case "definition", "references":
		switch dimension.Language {
		case "Go", "C", "C++", "Rust", "Python", "TypeScript", "JavaScript":
			return true
		}
	case "position-mapping":
		return dimension.Language == "Go"
	}
	return false
}

func semanticMetricByName(metrics telemetry.Metrics, name string) (telemetry.Metric, bool) {
	metric, ok := semanticKPIMetricMap(metrics)[name]
	return metric, ok
}

func semanticSetMetric(metrics *telemetry.Metrics, name string, metric telemetry.Metric) bool {
	switch name {
	case "precision":
		metrics.Precision = metric
	case "recall":
		metrics.Recall = metric
	case "falsePositiveRate":
		metrics.FalsePositiveRate = metric
	case "falseNegativeRate":
		metrics.FalseNegativeRate = metric
	case "refusalRate":
		metrics.RefusalRate = metric
	case "staleResultRejectionRate":
		metrics.StaleResultRejectionRate = metric
	case "wrongFileRate":
		metrics.WrongFileRate = metric
	case "positionMappingFailureRate":
		metrics.PositionMappingFailureRate = metric
	case "editValidationFailureRate":
		metrics.EditValidationFailureRate = metric
	default:
		return false
	}
	return true
}

func semanticKPIDimensionKey(d telemetry.Dimension) string {
	return d.Feature + "\x00" + d.Language + "\x00" + d.Backend
}

func semanticKPIMetricMap(metrics telemetry.Metrics) map[string]telemetry.Metric {
	return map[string]telemetry.Metric{
		"precision":                  metrics.Precision,
		"recall":                     metrics.Recall,
		"falsePositiveRate":          metrics.FalsePositiveRate,
		"falseNegativeRate":          metrics.FalseNegativeRate,
		"refusalRate":                metrics.RefusalRate,
		"staleResultRejectionRate":   metrics.StaleResultRejectionRate,
		"wrongFileRate":              metrics.WrongFileRate,
		"positionMappingFailureRate": metrics.PositionMappingFailureRate,
		"editValidationFailureRate":  metrics.EditValidationFailureRate,
	}
}

func runSemanticCPPBuildContextCase(t *testing.T, session *lspdriver.Session, workspace string, files map[string]string, toolVersions map[string]string) report.Check {
	t.Helper()
	check := report.Check{ID: "S20/cpp-build-context", Status: report.NotVerified, Summary: "C++ compile context was not observed"}
	if missing := semanticLockedToolVersionProblems([]string{"clangd", "clang++"}, toolVersions); len(missing) > 0 {
		check.Summary = "blocked: " + strings.Join(missing, "; ")
		return check
	}
	usePath := filepath.Join(workspace, "cpp-api", "use.cpp")
	implementationPath := filepath.Join(workspace, "cpp-api", "impl.cpp")
	headerPath := filepath.Join(workspace, "include", "acceptance", "cpp-api.hpp")
	compileDatabasePath := filepath.Join(workspace, "compile_commands.json")
	if !fileExistsSemantic(compileDatabasePath) {
		check.Status = report.Failed
		check.Summary = "workspace fixture omitted its configured root compile database"
		return check
	}
	queryText := files["cpp-api/use.cpp"]
	markerLine, markerChar := semanticPositionOf(queryText, "cxx20_marker()", 0)
	targetLine, targetChar := semanticPositionOf(queryText, "target()", 0)
	if markerLine == ^uint32(0) || targetLine == ^uint32(0) {
		check.Status = report.Failed
		check.Summary = "C++ fixture lacks an exact consteval or target call-site token"
		return check
	}
	queryURI := uri.FromPath(usePath).String()
	refreshText := queryText + "// trigger C++ build-context diagnostics\n"
	session.Notify(t, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": queryURI, "version": 2}, "contentChanges": []map[string]string{{"text": refreshText}}})
	diagnosticCtx, cancelDiagnostics := context.WithTimeout(context.Background(), 20*time.Second)
	diagnosticRaw, diagnosticErr := session.RequestContext(diagnosticCtx, "textDocument/diagnostic", map[string]any{
		"textDocument": map[string]string{"uri": queryURI},
	})
	cancelDiagnostics()
	var diagnosticReport struct {
		Kind     string               `json:"kind"`
		ResultID string               `json:"resultId"`
		Items    []semanticDiagnostic `json:"items"`
	}
	diagnosticDecodeErr := json.Unmarshal(diagnosticRaw, &diagnosticReport)
	if diagnosticErr != nil || diagnosticDecodeErr != nil || diagnosticReport.Kind != "full" {
		check.Summary = fmt.Sprintf("not_verified: candidate textDocument/diagnostic pull did not return a full report: request=%v decode=%v kind=%q", diagnosticErr, diagnosticDecodeErr, diagnosticReport.Kind)
		check.Observed = map[string]any{
			"fixtureBuildConfiguration": map[string]any{"compileDatabasePath": compileDatabasePath, "compileDatabaseSHA256": semanticSHA256([]byte(files["compile_commands.json"]))},
			"requiredStandard":          "-std=c++20", "requiredInclude": filepath.Join(workspace, "include"), "featureStatus": "not_verified",
			"candidateEvidence": map[string]any{
				"source":                 "real candidate stdio textDocument/diagnostic pull after document version 2",
				"diagnosticRequestError": semanticErrString(diagnosticErr), "diagnosticDecodeError": semanticErrString(diagnosticDecodeErr),
				"diagnosticResponse": json.RawMessage(diagnosticRaw), "diagnosticKind": diagnosticReport.Kind,
			},
		}
		return check
	}
	diagnostics := diagnosticReport.Items
	markerCtx, cancelMarker := context.WithTimeout(context.Background(), 20*time.Second)
	markerRaw, markerErr := session.RequestContext(markerCtx, "textDocument/definition", map[string]any{
		"textDocument": map[string]string{"uri": queryURI},
		"position":     map[string]uint32{"line": markerLine, "character": markerChar},
	})
	cancelMarker()
	targetCtx, cancelTarget := context.WithTimeout(context.Background(), 20*time.Second)
	targetRaw, targetErr := session.RequestContext(targetCtx, "textDocument/definition", map[string]any{
		"textDocument": map[string]string{"uri": queryURI},
		"position":     map[string]uint32{"line": targetLine, "character": targetChar},
	})
	cancelTarget()
	markerDefinitions, markerDecodeErr := normalizeSemanticLocations(markerRaw)
	targetDefinitions, targetDecodeErr := normalizeSemanticLocations(targetRaw)
	markerText := files["include/acceptance/cpp-api.hpp"]
	markerDefinitionLine, markerDefinitionChar := semanticPositionOf(markerText, "cxx20_marker()", 0)
	markerWidth := uint32(len(utf16.Encode([]rune("cxx20_marker"))))
	expectedMarker := semanticLocation{URI: semanticCanonicalURI(uri.FromPath(headerPath).String()), StartLine: markerDefinitionLine, StartChar: markerDefinitionChar, EndLine: markerDefinitionLine, EndChar: markerDefinitionChar + markerWidth}
	implementationText := files["cpp-api/impl.cpp"]
	implementationLine, implementationChar := semanticPositionOf(implementationText, "target(void)", 0)
	targetWidth := uint32(len(utf16.Encode([]rune("target"))))
	expectedImplementation := semanticLocation{URI: semanticCanonicalURI(uri.FromPath(implementationPath).String()), StartLine: implementationLine, StartChar: implementationChar, EndLine: implementationLine, EndChar: implementationChar + targetWidth}
	var errorDiagnostics []semanticDiagnostic
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity != nil && *diagnostic.Severity == 1 {
			errorDiagnostics = append(errorDiagnostics, diagnostic)
		}
	}
	check.Observed = map[string]any{
		"fixtureBuildConfiguration": map[string]any{
			"compileDatabasePath": compileDatabasePath, "compileDatabaseSHA256": semanticSHA256([]byte(files["compile_commands.json"])),
			"providedStandard": "-std=c++20", "providedIncludePath": filepath.Join(workspace, "include"),
		},
		"candidateEvidence": map[string]any{
			"source":           "real candidate stdio definition requests and textDocument/diagnostic pull after document version 2",
			"diagnosticMethod": "textDocument/diagnostic", "diagnosticRequestedAfterDocumentVersion": 2,
			"diagnosticRequestError": semanticErrString(diagnosticErr), "diagnosticKind": diagnosticReport.Kind, "diagnosticResultID": diagnosticReport.ResultID,
			"cxx20MarkerCallPosition": []uint32{markerLine, markerChar}, "cxx20MarkerDefinitions": markerDefinitions,
			"expectedCxx20MarkerDefinition": expectedMarker, "cxx20MarkerRequestError": semanticErrString(markerErr),
			"cxx20MarkerDecodeError": semanticErrString(markerDecodeErr),
			"targetCallPosition":     []uint32{targetLine, targetChar}, "targetDefinitions": targetDefinitions,
			"expectedImplementationDefinition": expectedImplementation, "targetRequestError": semanticErrString(targetErr),
			"targetDecodeError": semanticErrString(targetDecodeErr), "pullDiagnostics": diagnostics,
		},
		"errorDiagnostics": errorDiagnostics,
	}
	if markerErr != nil || targetErr != nil {
		check.Summary = fmt.Sprintf("not_verified: candidate C++ build-context definition query failed: marker=%v target=%v", markerErr, targetErr)
		check.Observed["featureStatus"] = "not_verified"
		return check
	}
	if markerDecodeErr != nil || targetDecodeErr != nil {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("candidate C++ build-context definition could not be normalized: marker=%v target=%v", markerDecodeErr, targetDecodeErr)
		return check
	}
	if len(errorDiagnostics) > 0 || !containsSemanticLocation(markerDefinitions, expectedMarker) || !containsSemanticLocation(targetDefinitions, expectedImplementation) {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("candidate did not resolve the C++20 consteval/header and implementation targets with no compiler errors: errors=%+v marker=%+v wantMarker=%+v target=%+v wantImplementation=%+v", errorDiagnostics, markerDefinitions, expectedMarker, targetDefinitions, expectedImplementation)
		return check
	}
	check.Status = report.Passed
	check.Summary = "candidate stdio resolved the consteval call to the configured header, resolved the target call to its implementation, and emitted no compile errors under the fixture build configuration"
	return check
}

func semanticWaitDiagnostics(session *lspdriver.Session, targetURI string, start int, expectedVersion int, timeout time.Duration) ([]semanticDiagnostic, int, bool) {
	deadline := time.Now().Add(timeout)
	canonical := semanticCanonicalURI(targetURI)
	for time.Now().Before(deadline) {
		events := session.Events()
		if start > len(events) {
			start = len(events)
		}
		for _, event := range events[start:] {
			if event.Method != "textDocument/publishDiagnostics" {
				continue
			}
			var params semanticPublishDiagnostics
			if json.Unmarshal(event.Params, &params) == nil && semanticCanonicalURI(params.URI) == canonical && params.Version != nil && *params.Version == expectedVersion {
				return params.Diagnostics, *params.Version, true
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil, 0, false
}

func semanticStartGoUpstream(workspace string) (*upstream.Session, error) {
	binary, args := semanticUpstreamSpec("Go")
	path, err := semanticLookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("pinned gopls unavailable: %w", err)
	}
	session, err := upstream.Start(path, args, workspace, []string{"PATH=" + semanticAcceptanceToolPath()})
	if err != nil {
		return nil, fmt.Errorf("start pinned gopls: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := session.Initialize(ctx, workspace); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("initialize pinned gopls: %w", err)
	}
	return session, nil
}

func semanticWaitUpstreamDiagnostics(session *upstream.Session, targetURI string, cursor uint64, expectedVersion int, timeout time.Duration) ([]semanticDiagnostic, int, bool, string) {
	if session == nil {
		return nil, 0, false, "pinned gopls session is unavailable"
	}
	deadline := time.Now().Add(timeout)
	canonical := semanticCanonicalURI(targetURI)
	observedVersions := map[int]bool{}
	for time.Now().Before(deadline) {
		notifications, overflow := session.NotificationsSince(cursor)
		if overflow {
			return nil, 0, false, "pinned gopls notification history overflowed before the requested diagnostics were observed"
		}
		for _, notification := range notifications {
			if notification.Method != "textDocument/publishDiagnostics" {
				continue
			}
			var params semanticPublishDiagnostics
			if json.Unmarshal(notification.Params, &params) != nil || semanticCanonicalURI(params.URI) != canonical || params.Version == nil {
				continue
			}
			observedVersions[*params.Version] = true
			if *params.Version == expectedVersion {
				return params.Diagnostics, *params.Version, true, ""
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	versions := make([]int, 0, len(observedVersions))
	for version := range observedVersions {
		versions = append(versions, version)
	}
	sort.Ints(versions)
	return nil, 0, false, fmt.Sprintf("timed out waiting for pinned gopls diagnostics at document version %d (observed versions %v)", expectedVersion, versions)
}

type semanticDiagnosticCore struct {
	HasSeverity bool   `json:"hasSeverity"`
	Severity    int    `json:"severity,omitempty"`
	StartLine   uint32 `json:"startLine"`
	StartChar   uint32 `json:"startCharacter"`
	EndLine     uint32 `json:"endLine"`
	EndChar     uint32 `json:"endCharacter"`
}

func semanticDiagnosticCoreLess(a, b semanticDiagnosticCore) bool {
	if a.HasSeverity != b.HasSeverity {
		return a.HasSeverity
	}
	if a.Severity != b.Severity {
		return a.Severity < b.Severity
	}
	if a.StartLine != b.StartLine {
		return a.StartLine < b.StartLine
	}
	if a.StartChar != b.StartChar {
		return a.StartChar < b.StartChar
	}
	if a.EndLine != b.EndLine {
		return a.EndLine < b.EndLine
	}
	return a.EndChar < b.EndChar
}

func semanticDiagnosticOrderedCores(left, right map[semanticDiagnosticCore][]string) []semanticDiagnosticCore {
	set := map[semanticDiagnosticCore]bool{}
	for core := range left {
		set[core] = true
	}
	for core := range right {
		set[core] = true
	}
	cores := make([]semanticDiagnosticCore, 0, len(set))
	for core := range set {
		cores = append(cores, core)
	}
	sort.Slice(cores, func(i, j int) bool { return semanticDiagnosticCoreLess(cores[i], cores[j]) })
	return cores
}

func semanticMalformedSourceErrors(diagnostics []semanticDiagnostic, syntaxLine uint32) []semanticDiagnostic {
	var errors []semanticDiagnostic
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity != nil && *diagnostic.Severity == 1 && diagnostic.Range.Start.Line >= syntaxLine {
			errors = append(errors, diagnostic)
		}
	}
	return errors
}

func semanticDiagnosticCoreOf(diagnostic semanticDiagnostic) semanticDiagnosticCore {
	core := semanticDiagnosticCore{
		StartLine: diagnostic.Range.Start.Line, StartChar: diagnostic.Range.Start.Character,
		EndLine: diagnostic.Range.End.Line, EndChar: diagnostic.Range.End.Character,
	}
	if diagnostic.Severity != nil {
		core.HasSeverity = true
		core.Severity = *diagnostic.Severity
	}
	return core
}

func semanticDiagnosticCores(diagnostics []semanticDiagnostic) []semanticDiagnosticCore {
	cores := make([]semanticDiagnosticCore, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		cores = append(cores, semanticDiagnosticCoreOf(diagnostic))
	}
	sort.Slice(cores, func(i, j int) bool { return semanticDiagnosticCoreLess(cores[i], cores[j]) })
	return cores
}

func semanticDiagnosticCodeValue(code json.RawMessage) string {
	if len(code) == 0 || string(code) == "null" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(code, &decoded); err != nil {
		return strings.TrimSpace(string(code))
	}
	normalized, err := json.Marshal(decoded)
	if err != nil {
		return strings.TrimSpace(string(code))
	}
	return string(normalized)
}

func semanticDiagnosticFieldValues(diagnostics []semanticDiagnostic, field func(semanticDiagnostic) string) map[semanticDiagnosticCore][]string {
	values := make(map[semanticDiagnosticCore][]string)
	for _, diagnostic := range diagnostics {
		value := field(diagnostic)
		if value != "" {
			core := semanticDiagnosticCoreOf(diagnostic)
			values[core] = append(values[core], value)
		}
	}
	for core := range values {
		sort.Strings(values[core])
	}
	return values
}

func semanticDiagnosticFieldDifferences(candidate, pinned []semanticDiagnostic) []map[string]any {
	candidateValues := semanticDiagnosticFieldValues(candidate, func(diagnostic semanticDiagnostic) string { return diagnostic.Message })
	pinnedValues := semanticDiagnosticFieldValues(pinned, func(diagnostic semanticDiagnostic) string { return diagnostic.Message })
	var differences []map[string]any
	for _, core := range semanticDiagnosticOrderedCores(candidateValues, pinnedValues) {
		candidateMessages := candidateValues[core]
		pinnedMessages := pinnedValues[core]
		if reflect.DeepEqual(candidateMessages, pinnedMessages) {
			continue
		}
		differences = append(differences, map[string]any{"core": core, "candidateMessages": candidateMessages, "pinnedMessages": pinnedMessages})
	}
	return differences
}

func semanticDiagnosticCodeDifferences(candidate, pinned []semanticDiagnostic) ([]map[string]any, []map[string]any, bool) {
	candidateValues := semanticDiagnosticFieldValues(candidate, func(diagnostic semanticDiagnostic) string { return semanticDiagnosticCodeValue(diagnostic.Code) })
	pinnedValues := semanticDiagnosticFieldValues(pinned, func(diagnostic semanticDiagnostic) string { return semanticDiagnosticCodeValue(diagnostic.Code) })
	var differences, notComparable []map[string]any
	mismatch := false
	for _, core := range semanticDiagnosticOrderedCores(candidateValues, pinnedValues) {
		left, right := candidateValues[core], pinnedValues[core]
		if len(left) == 0 || len(right) == 0 || len(left) != len(right) {
			notComparable = append(notComparable, map[string]any{"core": core, "candidateCodes": left, "pinnedCodes": right})
			continue
		}
		if !reflect.DeepEqual(left, right) {
			mismatch = true
			differences = append(differences, map[string]any{"core": core, "candidateCodes": left, "pinnedCodes": right})
		}
	}
	return differences, notComparable, mismatch
}

func semanticDiagnosticSourceDifferences(candidate, pinned []semanticDiagnostic) []map[string]any {
	candidateValues := semanticDiagnosticFieldValues(candidate, func(diagnostic semanticDiagnostic) string { return diagnostic.Source })
	pinnedValues := semanticDiagnosticFieldValues(pinned, func(diagnostic semanticDiagnostic) string { return diagnostic.Source })
	var differences []map[string]any
	for _, core := range semanticDiagnosticOrderedCores(candidateValues, pinnedValues) {
		left, right := candidateValues[core], pinnedValues[core]
		if !reflect.DeepEqual(left, right) {
			differences = append(differences, map[string]any{"core": core, "candidateSources": left, "pinnedSources": right})
		}
	}
	return differences
}

func semanticCompareDiagnostics(candidate, pinned []semanticDiagnostic) (bool, map[string]any) {
	// Severity and UTF-16 ranges are compared across servers. Diagnostic codes
	// are compared when both servers publish them; source labels and messages
	// remain separate provider/rendering evidence because those names vary.
	candidateCores := semanticDiagnosticCores(candidate)
	pinnedCores := semanticDiagnosticCores(pinned)
	codeDifferences, codeNotComparable, codeMismatch := semanticDiagnosticCodeDifferences(candidate, pinned)
	messageDifferences := semanticDiagnosticFieldDifferences(candidate, pinned)
	sourceDifferences := semanticDiagnosticSourceDifferences(candidate, pinned)
	coreMatch := reflect.DeepEqual(candidateCores, pinnedCores)
	return coreMatch && !codeMismatch, map[string]any{
		"candidateStableFields":  candidateCores,
		"pinnedStableFields":     pinnedCores,
		"stableFieldsMatch":      coreMatch,
		"codeDifferences":        codeDifferences,
		"codeNotComparable":      codeNotComparable,
		"messageDifferences":     messageDifferences,
		"sourceLabelDifferences": sourceDifferences,
	}
}

func normalizeSemanticWorkspaceEdit(raw json.RawMessage) ([]semanticEditObservation, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, errors.New("rename returned no WorkspaceEdit")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("decode WorkspaceEdit: %w", err)
	}
	hasChanges := len(fields["changes"]) > 0 && strings.TrimSpace(string(fields["changes"])) != "null" && strings.TrimSpace(string(fields["changes"])) != "{}"
	hasDocumentChanges := len(fields["documentChanges"]) > 0 && strings.TrimSpace(string(fields["documentChanges"])) != "null" && strings.TrimSpace(string(fields["documentChanges"])) != "[]"
	if hasChanges == hasDocumentChanges {
		return nil, errors.New("WorkspaceEdit must contain exactly one nonempty changes or documentChanges form")
	}

	var observations []semanticEditObservation
	appendEdit := func(fileURI string, edit semanticWorkspaceTextEdit) error {
		start, end := edit.Range.Start, edit.Range.End
		if fileURI == "" || edit.NewText == "" || end.Line < start.Line || (end.Line == start.Line && end.Character < start.Character) {
			return errors.New("WorkspaceEdit contains an incomplete or invalid text edit")
		}
		observations = append(observations, semanticEditObservation{
			URI: semanticCanonicalURI(fileURI), StartLine: start.Line, StartChar: start.Character,
			EndLine: end.Line, EndChar: end.Character, NewText: edit.NewText,
		})
		return nil
	}
	if hasChanges {
		var changes map[string][]semanticWorkspaceTextEdit
		if err := json.Unmarshal(fields["changes"], &changes); err != nil {
			return nil, fmt.Errorf("decode WorkspaceEdit.changes: %w", err)
		}
		for fileURI, edits := range changes {
			if len(edits) == 0 {
				return nil, fmt.Errorf("WorkspaceEdit.changes[%q] contains no text edits", fileURI)
			}
			for _, edit := range edits {
				if err := appendEdit(fileURI, edit); err != nil {
					return nil, err
				}
			}
		}
	} else {
		var documentChanges []semanticWorkspaceTextDocumentEdit
		if err := json.Unmarshal(fields["documentChanges"], &documentChanges); err != nil {
			return nil, fmt.Errorf("decode WorkspaceEdit.documentChanges: %w", err)
		}
		for _, document := range documentChanges {
			if document.TextDocument.URI == "" || len(document.Edits) == 0 {
				return nil, errors.New("WorkspaceEdit.documentChanges contains a resource operation or empty document edit")
			}
			for _, edit := range document.Edits {
				if err := appendEdit(document.TextDocument.URI, edit); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(observations) == 0 {
		return nil, errors.New("WorkspaceEdit contains no text edits")
	}
	sort.Slice(observations, func(i, j int) bool {
		left, right := observations[i], observations[j]
		if left.URI != right.URI {
			return left.URI < right.URI
		}
		if left.StartLine != right.StartLine {
			return left.StartLine < right.StartLine
		}
		if left.StartChar != right.StartChar {
			return left.StartChar < right.StartChar
		}
		if left.EndLine != right.EndLine {
			return left.EndLine < right.EndLine
		}
		if left.EndChar != right.EndChar {
			return left.EndChar < right.EndChar
		}
		return left.NewText < right.NewText
	})
	return observations, nil
}

func semanticValidatedEditKPI(raw json.RawMessage, requestErr error, dimension telemetry.Dimension) (telemetry.Record, []semanticEditObservation, bool, error) {
	if requestErr != nil {
		return telemetry.Record{}, nil, false, requestErr
	}
	edits, err := normalizeSemanticWorkspaceEdit(raw)
	if err != nil {
		return telemetry.Record{}, nil, false, err
	}
	return telemetry.NewRecord(dimension, telemetry.Inputs{
		EditValidationFailures: telemetry.Sampled(0), EditValidationAttempts: telemetry.Sampled(1),
	}), edits, true, nil
}

type semanticQueryTrace struct {
	Computations          uint64  `json:"Computations"`
	StalePublishRejected  uint64  `json:"StalePublishRejected"`
	RenameRequestsStarted *uint64 `json:"RenameRequestsStarted"`
	RenameStaleRejected   *uint64 `json:"RenameStaleRejected"`
}

func semanticReadQueryTrace(session *lspdriver.Session) (semanticQueryTrace, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := session.RequestContext(ctx, "omnilsp/queryTrace", map[string]any{})
	if err != nil {
		return semanticQueryTrace{}, err
	}
	var stats semanticQueryTrace
	if err := json.Unmarshal(raw, &stats); err != nil {
		return semanticQueryTrace{}, fmt.Errorf("decode omnilsp/queryTrace: %w", err)
	}
	return stats, nil
}

func semanticStaleMetric(before, after semanticQueryTrace) (telemetry.Metric, string) {
	unknown := telemetry.NewRecord(telemetry.Dimension{Feature: "definition", Language: "Go", Backend: "golang"}, telemetry.Inputs{}).Metrics.StaleResultRejectionRate
	if after.Computations < before.Computations || after.StalePublishRejected < before.StalePublishRejected {
		unknown.Reason = "queryTrace counters regressed; a stale-candidate denominator cannot be attributed"
		return unknown, unknown.Reason
	}
	computationDelta := after.Computations - before.Computations
	rejectionDelta := after.StalePublishRejected - before.StalePublishRejected
	if computationDelta != 1 || rejectionDelta > computationDelta {
		unknown.Reason = fmt.Sprintf("one unique request must produce exactly one computation and at most one stale rejection; observed computations=%d stale_rejections=%d", computationDelta, rejectionDelta)
		return unknown, unknown.Reason
	}
	metric := telemetry.NewRecord(telemetry.Dimension{Feature: "definition", Language: "Go", Backend: "golang"}, telemetry.Inputs{
		StaleRejected: telemetry.Sampled(int64(rejectionDelta)), StaleCandidates: telemetry.Sampled(int64(computationDelta)),
	}).Metrics.StaleResultRejectionRate
	return metric, ""
}

func semanticStaleRejectionSafety(metric telemetry.Metric) (bool, string) {
	if metric.Status != telemetry.MetricObserved || metric.Numerator == nil || metric.Denominator == nil {
		return false, "stale rejection safety could not be verified from an observed queryTrace ratio"
	}
	if *metric.Numerator != 1 || *metric.Denominator != 1 {
		return false, fmt.Sprintf("one stale result was observed but rejected=%d of candidates=%d; stale safety requires 1/1", *metric.Numerator, *metric.Denominator)
	}
	return true, "one stale candidate was rejected"
}

func semanticRenameStaleMetric(before, after semanticQueryTrace) (telemetry.Metric, string) {
	unknown := semanticNotVerifiedMetric("staleResultRejectionRate", "rename freshness counters are unavailable")
	if before.RenameRequestsStarted == nil || before.RenameStaleRejected == nil ||
		after.RenameRequestsStarted == nil || after.RenameStaleRejected == nil {
		unknown.Reason = "queryTrace did not expose both rename request-start and stale-rejection counters"
		return unknown, unknown.Reason
	}
	if *after.RenameRequestsStarted < *before.RenameRequestsStarted || *after.RenameStaleRejected < *before.RenameStaleRejected {
		unknown.Reason = "rename queryTrace counters regressed; no stale candidate denominator can be attributed"
		return unknown, unknown.Reason
	}
	started := *after.RenameRequestsStarted - *before.RenameRequestsStarted
	rejected := *after.RenameStaleRejected - *before.RenameStaleRejected
	if started != 1 || rejected > started {
		unknown.Reason = fmt.Sprintf("one unique backend rename must produce exactly one request start and at most one stale rejection; observed started=%d stale_rejections=%d", started, rejected)
		return unknown, unknown.Reason
	}
	metric := telemetry.NewRecord(telemetry.Dimension{Feature: "rename", Language: "Go", Backend: "golang"}, telemetry.Inputs{
		StaleRejected: telemetry.Sampled(int64(rejected)), StaleCandidates: telemetry.Sampled(int64(started)),
	}).Metrics.StaleResultRejectionRate
	return metric, ""
}

func semanticNotVerifiedMetric(name, reason string) telemetry.Metric {
	metric, ok := semanticMetricByName(telemetry.NewRecord(telemetry.Dimension{}, telemetry.Inputs{}).Metrics, name)
	if !ok {
		return telemetry.Metric{Formula: "unknown", Status: telemetry.MetricNotVerified, Reason: reason}
	}
	metric.Reason = reason
	return metric
}

func semanticKPIUpdate(dimension telemetry.Dimension, name string, metric telemetry.Metric) []semanticKPIMetricObservation {
	return []semanticKPIMetricObservation{{Dimension: dimension, Name: name, Metric: metric}}
}

func semanticRunStaleResultProbe(t *testing.T, session *lspdriver.Session, workspace string) report.Check {
	t.Helper()
	check := report.Check{ID: "S20/stale-result-rejection", Status: report.NotVerified}
	dimension := telemetry.Dimension{Feature: "definition", Language: "Go", Backend: "golang"}
	setUnverified := func(reason string) {
		metric := semanticNotVerifiedMetric("staleResultRejectionRate", reason)
		check.Observed["staleMetric"] = metric
		check.Observed["kpiMetricObservations"] = semanticKPIUpdate(dimension, "staleResultRejectionRate", metric)
		check.Summary = "not_verified: " + reason
	}
	filePath := filepath.Join(workspace, "go-safety", "stale_race.go")
	fileURI := uri.FromPath(filePath).String()
	var source strings.Builder
	source.Grow(450000)
	source.WriteString("package safety\nfunc staleTarget() int { return 1 }\nfunc staleUse() int { return staleTarget() }\n")
	for i := 0; i < 8000; i++ {
		fmt.Fprintf(&source, "func staleFiller%d() int { return %d }\n", i, i)
	}
	text := source.String()
	if err := os.WriteFile(filePath, []byte(text), 0o600); err != nil {
		check.Status = report.Failed
		check.Summary = "could not write bounded stale-result race fixture: " + err.Error()
		check.Observed = map[string]any{}
		setUnverified("could not exercise freshness gate because the real race fixture could not be written: " + err.Error())
		return check
	}
	session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": fileURI, "languageId": "go", "version": 1, "text": text,
	}})
	before, err := semanticReadQueryTrace(session)
	if err != nil {
		check.Observed = map[string]any{"queryTraceError": err.Error(), "fixtureSHA256": semanticSHA256([]byte(text))}
		setUnverified("existing omnilsp/queryTrace could not expose freshness counters: " + err.Error())
		return check
	}
	line, character := semanticPositionOf(text, "staleTarget()", 1)
	params := map[string]any{"textDocument": map[string]string{"uri": fileURI}, "position": map[string]uint32{"line": line, "character": character}}
	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 60*time.Second)
	type response struct {
		raw json.RawMessage
		err error
	}
	responseCh := make(chan response, 1)
	go func() {
		raw, requestErr := session.RequestContext(requestCtx, "textDocument/definition", params)
		responseCh <- response{raw: raw, err: requestErr}
	}()

	started := false
	var startStats semanticQueryTrace
	var traceErr error
	var result response
	responseReady := false
	startDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(startDeadline) {
		startStats, traceErr = semanticReadQueryTrace(session)
		if traceErr != nil {
			break
		}
		if startStats.Computations > before.Computations {
			started = startStats.Computations-before.Computations == 1 && startStats.StalePublishRejected == before.StalePublishRejected
			break
		}
		select {
		case result = <-responseCh:
			responseReady = true
			traceErr = errors.New("definition request completed before queryTrace exposed its unique computation")
			break
		default:
		}
		if traceErr != nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	revisionAdvanced := false
	if started {
		// A completed query is not a stale candidate. Advance only while its
		// response is still pending after one isolated queryTrace computation.
		select {
		case result = <-responseCh:
			responseReady = true
			started = false
			traceErr = errors.New("definition response completed before the revision could advance")
		default:
			session.Notify(t, "textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": fileURI, "version": 2},
				"contentChanges": []map[string]string{{"text": text + "// freshness revision advance\n"}},
			})
			revisionAdvanced = true
		}
	}
	if !revisionAdvanced {
		cancelRequest()
	}
	if !responseReady {
		select {
		case result = <-responseCh:
		case <-time.After(65 * time.Second):
			cancelRequest()
			result.err = errors.New("stale-race definition request timed out")
		}
	}
	cancelRequest()
	after, afterErr := semanticReadQueryTrace(session)
	metricReason := "freshness race was not uniquely attributed"
	metric := semanticNotVerifiedMetric("staleResultRejectionRate", metricReason)
	switch {
	case !started:
		metricReason = "queryTrace did not confirm exactly one isolated definition computation still in flight before the revision advance: " + semanticErrString(traceErr)
	case !revisionAdvanced:
		metricReason = "document revision did not advance while the real definition computation was in flight"
	case afterErr != nil:
		metricReason = "post-race omnilsp/queryTrace counters were unavailable: " + afterErr.Error()
	default:
		metric, metricReason = semanticStaleMetric(before, after)
	}
	check.Observed = map[string]any{
		"fixtureURI": fileURI, "fixtureSHA256": semanticSHA256([]byte(text)), "fillerFunctionCount": 8000,
		"queryPosition": []uint32{line, character}, "queryTraceBefore": before, "queryTraceAtStart": startStats,
		"queryTraceAfter": after, "afterTraceError": semanticErrString(afterErr),
		"queryRequestStarted": started, "queryTracePollError": semanticErrString(traceErr),
		"requestError": semanticErrString(result.err), "staleMetric": metric,
	}
	if metric.Status != telemetry.MetricObserved {
		metric.Reason = metricReason
		check.Observed["staleMetric"] = metric
		check.Observed["kpiMetricObservations"] = semanticKPIUpdate(dimension, "staleResultRejectionRate", metric)
		check.Summary = "not_verified: the real query did not produce a uniquely attributable stale candidate: " + metricReason
		return check
	}
	dimension.Oracle = "omnilsp/queryTrace single-request freshness probe"
	check.Observed["kpiMetricObservations"] = semanticKPIUpdate(dimension, "staleResultRejectionRate", metric)
	if safe, reason := semanticStaleRejectionSafety(metric); !safe {
		check.Status = report.Failed
		check.Summary = "stale-safety invariant failed: " + reason
		return check
	}
	if result.err != nil {
		var rpcErr *jsonrpc.ResponseError
		if errors.As(result.err, &rpcErr) && rpcErr.Code == jsonrpc.ContentModified {
			check.Status = report.Passed
			check.Summary = "the single stale semantic result was rejected by the freshness gate and returned as ContentModified"
			return check
		}
		check.Status = report.Failed
		check.Summary = "freshness gate rejection was observed, but the semantic request ended unexpectedly: " + result.err.Error()
		return check
	}
	locations, decodeErr := normalizeSemanticLocations(result.raw)
	want := semanticLocation{URI: semanticCanonicalURI(fileURI), StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 16}
	check.Observed["responseLocations"] = locations
	check.Observed["expectedDefinition"] = want
	check.Observed["decodeError"] = semanticErrString(decodeErr)
	if decodeErr != nil || !containsSemanticLocation(locations, want) {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("freshness gate rejected the stale publish, but captured-snapshot response was invalid: decode=%v locations=%+v expected=%+v", decodeErr, locations, want)
		return check
	}
	check.Status = report.Passed
	check.Summary = "one unique real definition computation crossed a document revision advance; queryTrace confirmed its stale publish rejection and the captured-snapshot result remained correct"
	return check
}

func semanticRunRenameStaleProbe(t *testing.T, session *lspdriver.Session, workspace string) report.Check {
	t.Helper()
	const id = "S20/stale-result-rejection/go/rename"
	dimension := telemetry.Dimension{Feature: "rename", Language: "Go", Backend: "golang", Oracle: "omnilsp/queryTrace single-request rename freshness probe"}
	check := report.Check{ID: id, Status: report.NotVerified, Summary: "not_verified: rename stale-result race was not uniquely attributed", Observed: map[string]any{}}
	setUnverified := func(reason string) report.Check {
		metric := semanticNotVerifiedMetric("staleResultRejectionRate", reason)
		check.Status = report.NotVerified
		check.Summary = "not_verified: " + reason
		check.Observed["staleMetric"] = metric
		check.Observed["kpiMetricObservations"] = semanticKPIUpdate(dimension, "staleResultRejectionRate", metric)
		return check
	}
	const fillers = 8000
	filePath := filepath.Join(workspace, "go-safety", "rename_stale_race.go")
	fileURI := uri.FromPath(filePath).String()
	var source strings.Builder
	source.Grow(600000)
	source.WriteString("package safety\nfunc renameRaceTarget() int { return 1 }\nfunc renameRaceUse() int { return renameRaceTarget() }\n")
	for i := 0; i < fillers; i++ {
		fmt.Fprintf(&source, "func renameRaceFiller%d() int { return renameRaceTarget() }\n", i)
	}
	text := source.String()
	if err := os.WriteFile(filePath, []byte(text), 0o600); err != nil {
		check.Status = report.Failed
		check.Summary = "could not write bounded rename freshness fixture: " + err.Error()
		check.Observed["fixtureSHA256"] = semanticSHA256([]byte(text))
		return setUnverified("no rename race was run because the fixture could not be written: " + err.Error())
	}
	session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": fileURI, "languageId": "go", "version": 1, "text": text,
	}})
	before, err := semanticReadQueryTrace(session)
	if err != nil {
		check.Observed["queryTraceError"] = err.Error()
		return setUnverified("queryTrace before rename freshness probe failed: " + err.Error())
	}
	if before.RenameRequestsStarted == nil || before.RenameStaleRejected == nil {
		check.Observed["queryTraceBefore"] = before
		return setUnverified("queryTrace does not expose the rename request-start and stale-rejection counters")
	}
	line, character := semanticPositionOf(text, "renameRaceTarget()", 1)
	if line == ^uint32(0) {
		return setUnverified("the real rename fixture did not contain a target use-site")
	}
	params := map[string]any{
		"textDocument": map[string]string{"uri": fileURI},
		"position":     map[string]uint32{"line": line, "character": character},
		"newName":      "renamedRaceTarget",
	}
	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 90*time.Second)
	type response struct {
		raw json.RawMessage
		err error
	}
	responseCh := make(chan response, 1)
	go func() {
		raw, requestErr := session.RequestContext(requestCtx, "textDocument/rename", params)
		responseCh <- response{raw: raw, err: requestErr}
	}()

	started, responseReady := false, false
	var atStart semanticQueryTrace
	var tracePollErr error
	var result response
	startDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(startDeadline) {
		atStart, tracePollErr = semanticReadQueryTrace(session)
		if tracePollErr != nil {
			break
		}
		if atStart.RenameRequestsStarted == nil || atStart.RenameStaleRejected == nil {
			tracePollErr = errors.New("queryTrace stopped exposing rename freshness counters")
			break
		}
		if *atStart.RenameRequestsStarted > *before.RenameRequestsStarted {
			started = *atStart.RenameRequestsStarted-*before.RenameRequestsStarted == 1 &&
				*atStart.RenameStaleRejected == *before.RenameStaleRejected
			break
		}
		select {
		case result = <-responseCh:
			responseReady = true
			tracePollErr = errors.New("rename response completed before queryTrace exposed its unique backend request")
		default:
		}
		if tracePollErr != nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	advanced := false
	if started {
		select {
		case result = <-responseCh:
			responseReady = true
			started = false
			tracePollErr = errors.New("rename response completed before the document revision could advance")
		default:
			session.Notify(t, "textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": fileURI, "version": 2},
				"contentChanges": []map[string]string{{"text": text + "// freshness revision advance\n"}},
			})
			advanced = true
		}
	}
	if !advanced {
		cancelRequest()
	}
	if !responseReady {
		select {
		case result = <-responseCh:
		case <-time.After(95 * time.Second):
			cancelRequest()
			result.err = errors.New("rename freshness request timed out")
		}
	}
	cancelRequest()
	after, afterErr := semanticReadQueryTrace(session)
	metricReason := "real rename did not produce a uniquely attributable stale candidate"
	metric := semanticNotVerifiedMetric("staleResultRejectionRate", metricReason)
	var normalizedEdits []semanticEditObservation
	var editDecodeErr error
	staleGateResponse := false
	backendResultProven := false
	if result.err != nil {
		var rpcErr *jsonrpc.ResponseError
		staleGateResponse = errors.As(result.err, &rpcErr) && rpcErr.Code == jsonrpc.ContentModified &&
			strings.Contains(strings.ToLower(rpcErr.Message), "workspace changed since request")
		backendResultProven = staleGateResponse
	} else {
		normalizedEdits, editDecodeErr = normalizeSemanticWorkspaceEdit(result.raw)
		backendResultProven = editDecodeErr == nil && len(normalizedEdits) > 0
	}
	if !backendResultProven && started && advanced && afterErr == nil {
		countedMetric, _ := semanticRenameStaleMetric(before, after)
		backendResultProven = countedMetric.Status == telemetry.MetricObserved && countedMetric.Numerator != nil && *countedMetric.Numerator == 1
	}
	switch {
	case !started:
		metricReason = "queryTrace did not confirm one unique backend rename request still in flight before the revision advance: " + semanticErrString(tracePollErr)
	case !advanced:
		metricReason = "document revision was not advanced while the real backend rename request was in flight"
	case afterErr != nil:
		metricReason = "queryTrace after the rename freshness race failed: " + afterErr.Error()
	case !backendResultProven:
		metricReason = "the rename ended without a stale-gate ContentModified response or a nonempty stale WorkspaceEdit, so a successful backend rename candidate was not proven: " + semanticErrString(result.err)
	default:
		metric, metricReason = semanticRenameStaleMetric(before, after)
	}
	check.Observed = map[string]any{
		"dimension": dimension, "method": "textDocument/rename", "fixtureURI": fileURI,
		"fixtureSHA256": semanticSHA256([]byte(text)), "fillerFunctionCount": fillers,
		"queryTraceBefore": before, "queryTraceAtStart": atStart, "queryTraceAfter": after,
		"queryTracePollError": semanticErrString(tracePollErr), "afterTraceError": semanticErrString(afterErr),
		"queryRequestStarted": started, "revisionAdvancedWhilePending": advanced,
		"requestError": semanticErrString(result.err), "staleGateContentModified": staleGateResponse,
		"workspaceEdit": result.raw, "normalizedEdits": normalizedEdits,
		"workspaceEditDecodeError": semanticErrString(editDecodeErr),
	}
	if metric.Status != telemetry.MetricObserved {
		metric.Reason = metricReason
		check.Status = report.NotVerified
		check.Summary = "not_verified: " + metricReason
		check.Observed["staleMetric"] = metric
		check.Observed["kpiMetricObservations"] = semanticKPIUpdate(dimension, "staleResultRejectionRate", metric)
		return check
	}
	check.Observed["staleMetric"] = metric
	check.Observed["kpiMetricObservations"] = semanticKPIUpdate(dimension, "staleResultRejectionRate", metric)
	if safe, safetyReason := semanticStaleRejectionSafety(metric); !safe {
		check.Status = report.Failed
		check.Summary = "rename stale-safety invariant failed: " + safetyReason
		return check
	}
	if !staleGateResponse {
		check.Status = report.Failed
		check.Summary = "queryTrace counted a stale rename rejection but the request did not return the expected ContentModified freshness-gate error"
		return check
	}
	check.Status = report.Passed
	check.Summary = "one unique real Go rename crossed a document revision advance; queryTrace confirmed the stale edit was rejected and the request returned ContentModified"
	return check
}

func semanticRunStaleFeatureProbes(t *testing.T, session *lspdriver.Session, workspace string, files map[string]string, tc semanticLanguageCase, semanticCheck report.Check) []report.Check {
	t.Helper()
	features := []string{"definition", "references"}
	if tc.name == "Go" {
		features = []string{"references"} // Go definition is covered by the dedicated safety fixture probe.
	}
	checks := make([]report.Check, 0, len(features))
	for _, feature := range features {
		dimension := telemetry.Dimension{Feature: feature, Language: tc.name, Backend: semanticCandidateBackend(tc.name), Oracle: "omnilsp/queryTrace unique " + feature + " probe"}
		observed := semanticCheck.Observed
		candidate, candidateOK := observed[feature].([]semanticLocation)
		pinnedKey := "upstream" + strings.ToUpper(feature[:1]) + feature[1:]
		pinned, pinnedOK := observed[pinnedKey].([]semanticLocation)
		if !candidateOK || !pinnedOK || len(candidate) == 0 || len(pinned) == 0 {
			reason := "positive candidate and pinned result sets were not both observed; no stale candidate denominator can be attributed"
			checks = append(checks, semanticUnverifiedStaleCheck("S20/stale-result-rejection/"+strings.ToLower(strings.ReplaceAll(tc.name, "+", "plus"))+"/"+feature, dimension, reason))
			continue
		}
		queryFileText, ok := files[tc.queryFile]
		if !ok || queryFileText == "" {
			reason := "semantic fixture text is unavailable for a real stale-result probe"
			checks = append(checks, semanticUnverifiedStaleCheck("S20/stale-result-rejection/"+strings.ToLower(strings.ReplaceAll(tc.name, "+", "plus"))+"/"+feature, dimension, reason))
			continue
		}
		line, character := semanticPositionOf(queryFileText, tc.queryToken, 0)
		method := "textDocument/" + feature
		params := map[string]any{
			"textDocument": map[string]string{"uri": uri.FromPath(filepath.Join(workspace, filepath.FromSlash(tc.queryFile))).String()},
			"position":     map[string]uint32{"line": line, "character": character},
		}
		if feature == "references" {
			params["context"] = map[string]bool{"includeDeclaration": true}
		}
		version := 2
		if feature == "references" {
			version = 4 // the definition probe on this URI advances it through version 3.
		}
		checks = append(checks, semanticRunAttributedStaleProbe(t, session, "S20/stale-result-rejection/"+strings.ToLower(strings.ReplaceAll(tc.name, "+", "plus"))+"/"+feature, dimension, params, method, queryFileText, version, pinned))
	}
	return checks
}

func semanticRunGoPositionStaleProbe(t *testing.T, session *lspdriver.Session, workspace string) report.Check {
	t.Helper()
	dimension := telemetry.Dimension{Feature: "position-mapping", Language: "Go", Backend: "golang", Oracle: "omnilsp/queryTrace Go UTF-16/CRLF position probe"}
	rel := "go-case/position_mapping_stale_probe.go"
	source := "package acceptance\r\nvar 世界 = 7\r\nfunc stalePositionUse() int { return 世界 }\r\n"
	filePath := filepath.Join(workspace, filepath.FromSlash(rel))
	if err := os.WriteFile(filePath, []byte(source), 0o600); err != nil {
		check := semanticUnverifiedStaleCheck("S20/stale-result-rejection/go/position-mapping", dimension, "could not write the isolated Unicode/CRLF fixture: "+err.Error())
		check.Status = report.Failed
		return check
	}
	line, character := semanticPositionOf(source, "世界", 1)
	if line == ^uint32(0) {
		return semanticUnverifiedStaleCheck("S20/stale-result-rejection/go/position-mapping", dimension, "Unicode use-site was not present in the Go fixture")
	}
	fileURI := uri.FromPath(filePath).String()
	session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": fileURI, "languageId": "go", "version": 1, "text": source,
	}})
	params := map[string]any{
		"textDocument": map[string]string{"uri": fileURI},
		"position":     map[string]uint32{"line": line, "character": character + 1},
	}
	expected := []semanticLocation{{URI: semanticCanonicalURI(fileURI), StartLine: 1, StartChar: 4, EndLine: 1, EndChar: 6}}
	return semanticRunAttributedStaleProbe(t, session, "S20/stale-result-rejection/go/position-mapping", dimension, params, "textDocument/definition", source, 2, expected)
}

func semanticUnverifiedStaleCheck(id string, dimension telemetry.Dimension, reason string) report.Check {
	metric := semanticNotVerifiedMetric("staleResultRejectionRate", reason)
	return report.Check{
		ID: id, Status: report.NotVerified, Summary: "not_verified: " + reason,
		Observed: map[string]any{
			"kpiMetricObservations": semanticKPIUpdate(dimension, "staleResultRejectionRate", metric),
			"staleMetric":           metric,
		},
	}
}

func semanticRunAttributedStaleProbe(t *testing.T, session *lspdriver.Session, id string, dimension telemetry.Dimension, params map[string]any, method, source string, warmVersion int, expected []semanticLocation) report.Check {
	t.Helper()
	fileURI := ""
	if document, ok := params["textDocument"].(map[string]string); ok {
		fileURI = document["uri"]
	}
	if fileURI == "" {
		return semanticUnverifiedStaleCheck(id, dimension, "real LSP document URI is unavailable")
	}
	largeText := semanticStaleExpansion(dimension.Language, dimension.Feature, source)
	version := warmVersion
	session.Notify(t, "textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": fileURI, "version": version},
		"contentChanges": []map[string]string{{"text": largeText}},
	})
	before, err := semanticReadQueryTrace(session)
	if err != nil {
		return semanticUnverifiedStaleCheck(id, dimension, "queryTrace before the unique semantic request failed: "+err.Error())
	}
	requestCtx, cancelRequest := context.WithTimeout(context.Background(), 60*time.Second)
	type response struct {
		raw json.RawMessage
		err error
	}
	responseCh := make(chan response, 1)
	go func() {
		raw, requestErr := session.RequestContext(requestCtx, method, params)
		responseCh <- response{raw: raw, err: requestErr}
	}()

	started := false
	var atStart semanticQueryTrace
	var tracePollErr error
	var result response
	responseReady := false
	startDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(startDeadline) {
		atStart, tracePollErr = semanticReadQueryTrace(session)
		if tracePollErr != nil {
			break
		}
		if atStart.Computations > before.Computations {
			started = atStart.Computations-before.Computations == 1 && atStart.StalePublishRejected == before.StalePublishRejected
			break
		}
		select {
		case result = <-responseCh:
			responseReady = true
			tracePollErr = errors.New("semantic request completed before queryTrace exposed a new computation")
			break
		default:
		}
		if tracePollErr != nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	advanced := false
	if started {
		// The queryTrace computation counter is observed first; a second full
		// change then advances the captured workspace revision while this request
		// still has no response available.
		select {
		case result = <-responseCh:
			responseReady = true
			started = false
			tracePollErr = errors.New("semantic response completed before the revision could advance")
		default:
			version++
			session.Notify(t, "textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": fileURI, "version": version},
				"contentChanges": []map[string]string{{"text": largeText + semanticStaleRevisionMarker(dimension.Language)}},
			})
			advanced = true
		}
	}
	if !advanced {
		cancelRequest()
	}
	if !responseReady {
		select {
		case result = <-responseCh:
		case <-time.After(65 * time.Second):
			cancelRequest()
			result.err = errors.New("stale-result semantic request timed out")
		}
	}
	cancelRequest()
	after, afterErr := semanticReadQueryTrace(session)
	metricReason := "real semantic request did not produce a uniquely attributable stale candidate"
	metric := semanticNotVerifiedMetric("staleResultRejectionRate", metricReason)
	switch {
	case !started:
		metricReason = "queryTrace did not confirm one unique computation still in flight before the revision advance: " + semanticErrString(tracePollErr)
	case !advanced:
		metricReason = "document revision was not advanced while the semantic request was in flight"
	case afterErr != nil:
		metricReason = "queryTrace after the real revision race failed: " + afterErr.Error()
	default:
		metric, metricReason = semanticStaleMetric(before, after)
	}
	check := report.Check{ID: id, Status: report.NotVerified}
	check.Observed = map[string]any{
		"dimension": dimension, "method": method, "fixtureURI": fileURI,
		"fixtureSHA256": semanticSHA256([]byte(largeText)), "fillerFunctionCount": 8000,
		"queryTraceBefore": before, "queryTraceAtStart": atStart, "queryTraceAfter": after,
		"queryTracePollError": semanticErrString(tracePollErr), "afterTraceError": semanticErrString(afterErr),
		"queryRequestStarted": started, "revisionAdvancedWhilePending": advanced,
		"requestError": semanticErrString(result.err), "expectedLocations": expected,
	}
	if metric.Status != telemetry.MetricObserved {
		metric.Reason = metricReason
		check.Observed["staleMetric"] = metric
		check.Observed["kpiMetricObservations"] = semanticKPIUpdate(dimension, "staleResultRejectionRate", metric)
		check.Summary = "not_verified: " + metricReason
		return check
	}
	check.Observed["staleMetric"] = metric
	check.Observed["kpiMetricObservations"] = semanticKPIUpdate(dimension, "staleResultRejectionRate", metric)
	if safe, safetyReason := semanticStaleRejectionSafety(metric); !safe {
		check.Status = report.Failed
		check.Summary = "stale-safety invariant failed: " + safetyReason
		return check
	}
	if result.err != nil {
		var rpcErr *jsonrpc.ResponseError
		if errors.As(result.err, &rpcErr) && rpcErr.Code == jsonrpc.ContentModified {
			check.Status = report.Passed
			check.Summary = "the stale semantic result was rejected with ContentModified after one queryTrace-confirmed rejection"
			return check
		}
		check.Status = report.Failed
		check.Summary = "queryTrace confirmed stale rejection, but the semantic request ended unexpectedly: " + result.err.Error()
		return check
	}
	locations, decodeErr := normalizeSemanticLocations(result.raw)
	check.Observed["responseLocations"] = locations
	check.Observed["decodeError"] = semanticErrString(decodeErr)
	if decodeErr != nil || !reflect.DeepEqual(locations, expected) {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("freshness gate rejected stale publication but captured-snapshot %s result differed from pinned baseline: decode=%v got=%+v pinned=%+v", method, decodeErr, locations, expected)
		return check
	}
	check.Status = report.Passed
	check.Summary = "one real " + method + " computation crossed a document revision advance, its stale publish was rejected, and the captured-snapshot result matched pinned output"
	return check
}

func semanticStaleExpansion(language, feature, source string) string {
	newline := "\n"
	if strings.Contains(source, "\r\n") {
		newline = "\r\n"
	}
	tag := strings.ReplaceAll(strings.ToLower(feature), "-", "_")
	var builder strings.Builder
	builder.Grow(len(source) + 8000*55)
	builder.WriteString(source)
	if !strings.HasSuffix(source, newline) {
		builder.WriteString(newline)
	}
	for i := 0; i < 8000; i++ {
		switch language {
		case "Go":
			fmt.Fprintf(&builder, "func omniStaleFill_%s_%d() int { return %d }%s", tag, i, i, newline)
		case "C":
			fmt.Fprintf(&builder, "static int omni_stale_fill_%s_%d(void) { return %d; }%s", tag, i, i, newline)
		case "C++":
			fmt.Fprintf(&builder, "static int omni_stale_fill_%s_%d() { return %d; }%s", tag, i, i, newline)
		case "Rust":
			fmt.Fprintf(&builder, "#[allow(dead_code)] fn omni_stale_fill_%s_%d() -> i32 { %d }%s", tag, i, i, newline)
		case "Python":
			fmt.Fprintf(&builder, "def omni_stale_fill_%s_%d():\n    return %d%s", tag, i, i, newline)
		case "TypeScript":
			fmt.Fprintf(&builder, "function omniStaleFill_%s_%d(): number { return %d; }%s", tag, i, i, newline)
		case "JavaScript":
			fmt.Fprintf(&builder, "function omniStaleFill_%s_%d() { return %d; }%s", tag, i, i, newline)
		}
	}
	return builder.String()
}

func semanticStaleRevisionMarker(language string) string {
	if language == "Python" {
		return "# freshness revision advance\n"
	}
	return "// freshness revision advance\n"
}

func fileExistsSemantic(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func runGoSafetyCases(t *testing.T, session *lspdriver.Session, workspace string, files map[string]string, toolVersions map[string]string) []report.Check {
	t.Helper()
	checks := []report.Check{}
	if missing := semanticLockedToolVersionProblems([]string{"go", "gopls"}, toolVersions); len(missing) > 0 {
		ids := []string{"unmodified-edit", "unsaved-edit", "unicode-crlf", "broken-source-diagnostics", "broken-source", "diagnostic-edit", "rename-safety", "rename-local-edit-validation", "stale-result-rejection"}
		for _, id := range ids {
			checks = append(checks, report.Check{ID: "S20/" + id, Status: report.NotVerified, Summary: "blocked: " + strings.Join(missing, "; ")})
		}
		return checks
	}
	goUpstream, goUpstreamErr := semanticStartGoUpstream(workspace)
	if goUpstream != nil {
		defer goUpstream.Close()
	}
	usePath := filepath.Join(workspace, "go-safety", "use.go")
	useURI := uri.FromPath(usePath).String()
	targetPath := filepath.Join(workspace, "go-safety", "target.go")
	targetURI := uri.FromPath(targetPath).String()
	useText := files["go-safety/use.go"]
	targetText := files["go-safety/target.go"]
	session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": targetURI, "languageId": "go", "version": 1, "text": targetText}})
	session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": useURI, "languageId": "go", "version": 1, "text": useText}})
	var renameOracle semanticRequestClient
	var renameOracleOpenError error
	if goUpstream != nil && goUpstreamErr == nil {
		if err := goUpstream.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": targetURI, "languageId": "go", "version": 1, "text": targetText}}); err != nil {
			renameOracleOpenError = fmt.Errorf("open target in pinned gopls: %w", err)
		} else if err := goUpstream.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": useURI, "languageId": "go", "version": 1, "text": useText}}); err != nil {
			renameOracleOpenError = fmt.Errorf("open use file in pinned gopls: %w", err)
		} else {
			renameOracle = goUpstream
		}
	}
	line, char := semanticPositionOf(useText, "target()", 0)
	params := map[string]any{"textDocument": map[string]string{"uri": useURI}, "position": map[string]uint32{"line": line, "character": char}}
	beforeDefinition, beforeReferences, err := semanticGoPair(session, params)
	if err != nil {
		check := report.Check{ID: "S20/unmodified-edit", Status: report.Failed, Summary: "baseline semantic query failed: " + err.Error()}
		return append(checks, check)
	}
	// An unchanged text edit still advances document version and snapshot. The
	// normalized semantic answer must remain identical after publication.
	session.Notify(t, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": useURI, "version": 2}, "contentChanges": []map[string]string{{"text": useText}}})
	afterDefinition, afterReferences, err := semanticGoPair(session, params)
	unmodified := report.Check{ID: "S20/unmodified-edit", Status: report.Passed, Summary: "semantic locations are stable after an identical full-document change", Observed: map[string]any{"beforeDefinition": beforeDefinition, "afterDefinition": afterDefinition, "beforeReferences": beforeReferences, "afterReferences": afterReferences}}
	if err != nil || !reflect.DeepEqual(beforeDefinition, afterDefinition) || !reflect.DeepEqual(beforeReferences, afterReferences) {
		unmodified.Status = report.Failed
		unmodified.Summary = fmt.Sprintf("normalized semantic result changed after unchanged edit: %v", err)
		if semanticUnsupportedError(err) {
			unmodified.Status = report.NotVerified
			unmodified.Summary = "not_verified: unchanged-edit semantic query is unsupported: " + err.Error()
		} else {
			t.Errorf("%s", unmodified.Summary)
		}
	}
	checks = append(checks, unmodified)

	changedText := useText + "func useUnsaved() int { return target() }\n"
	session.Notify(t, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": useURI, "version": 3}, "contentChanges": []map[string]string{{"text": changedText}}})
	newCallLine, newCallChar := semanticPositionOf(changedText, "target()", 1)
	newCallParams := map[string]any{"textDocument": map[string]string{"uri": useURI}, "position": map[string]uint32{"line": newCallLine, "character": newCallChar}}
	newDefinition, newReferences, newErr := semanticGoPair(session, newCallParams)
	wantNewCall := semanticLocation{URI: semanticCanonicalURI(useURI), StartLine: newCallLine, StartChar: newCallChar, EndLine: newCallLine, EndChar: newCallChar + 6}
	wantDefinition := semanticLocation{URI: semanticCanonicalURI(targetURI), StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 11}
	unsaved := report.Check{ID: "S20/unsaved-edit", Status: report.Passed, Summary: "an unsaved full-document edit adds a semantic reference visible to definition/references", Observed: map[string]any{
		"changedTextSentToServer": true, "diskWritePerformed": false, "newDefinition": newDefinition, "expectedDefinition": wantDefinition,
		"beforeReferences": beforeReferences, "afterReferences": newReferences, "expectedNewCall": wantNewCall,
	}}
	if newErr != nil {
		if semanticUnsupportedError(newErr) {
			unsaved.Status = report.NotVerified
			unsaved.Summary = "not_verified: candidate did not provide unsaved-edit semantic results: " + newErr.Error()
		} else {
			unsaved.Status = report.Failed
			unsaved.Summary = "unsaved-edit semantic request failed: " + newErr.Error()
			t.Errorf("%s", unsaved.Summary)
		}
	} else if !containsSemanticLocation(newDefinition, wantDefinition) ||
		!containsSemanticLocation(newReferences, wantNewCall) || len(newReferences) != len(beforeReferences)+1 {
		unsaved.Status = report.Failed
		unsaved.Summary = fmt.Sprintf("unsaved edit did not add exactly the expected reference: definition=%+v refs=%+v oldRefCount=%d expectedDefinition=%+v expectedCall=%+v", newDefinition, newReferences, len(beforeReferences), wantDefinition, wantNewCall)
		t.Errorf("%s", unsaved.Summary)
	}
	checks = append(checks, unsaved)

	unicodePath := filepath.Join(workspace, "go-case", "unicode_crlf.go")
	unicodeURI := uri.FromPath(unicodePath).String()
	unicodeText := files["go-case/unicode_crlf.go"]
	if unicodeText == "" {
		checks = append(checks, report.Check{ID: "S20/unicode-crlf", Status: report.Failed, Summary: "Unicode/CRLF fixture was not included in the corpus"})
	} else {
		session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": unicodeURI, "languageId": "go", "version": 1, "text": unicodeText}})
		queryLine, queryChar := semanticPositionOf(unicodeText, "世界", 1)
		// Put the UTF-16 cursor one code unit into a multi-byte identifier to
		// prove the server maps protocol columns back to UTF-8 source bytes.
		params := map[string]any{"textDocument": map[string]string{"uri": unicodeURI}, "position": map[string]uint32{"line": queryLine, "character": queryChar + 1}}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		raw, queryErr := session.RequestContext(ctx, "textDocument/definition", params)
		cancel()
		locations, decodeErr := normalizeSemanticLocations(raw)
		var upstreamLocations []semanticLocation
		upstreamUnicodeErr := goUpstreamErr
		if goUpstream == nil && upstreamUnicodeErr == nil {
			upstreamUnicodeErr = errors.New("pinned gopls session unavailable")
		}
		if goUpstream != nil {
			upstreamUnicodeErr = goUpstream.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": unicodeURI, "languageId": "go", "version": 1, "text": unicodeText}})
			if upstreamUnicodeErr == nil {
				ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
				upstreamRaw, upstreamQueryErr := goUpstream.RequestContext(ctx, "textDocument/definition", params)
				cancel()
				if upstreamQueryErr != nil {
					upstreamUnicodeErr = upstreamQueryErr
				} else {
					upstreamLocations, upstreamUnicodeErr = normalizeSemanticLocations(upstreamRaw)
				}
			}
		}
		want := semanticLocation{URI: semanticCanonicalURI(unicodeURI), StartLine: 1, StartChar: 4, EndLine: 1, EndChar: 6}
		check := report.Check{ID: "S20/unicode-crlf", Status: report.Passed, Summary: "candidate and pinned gopls resolve the CRLF/UTF-16 query to the same Unicode declaration", Observed: map[string]any{
			"locations": locations, "upstreamLocations": upstreamLocations, "expected": want,
			"queryUtf16": []uint32{queryLine, queryChar + 1}, "lineEnding": "CRLF",
			"upstream": semanticOracleLabel("gopls", toolVersions), "upstreamError": semanticErrString(upstreamUnicodeErr),
		}}
		if queryErr != nil || decodeErr != nil || !containsSemanticLocation(locations, want) {
			check.Status = report.Failed
			check.Summary = fmt.Sprintf("CRLF/Unicode definition mismatch: request=%v decode=%v locations=%+v expected=%+v", queryErr, decodeErr, locations, want)
			if semanticUnsupportedError(queryErr) {
				check.Status = report.NotVerified
				check.Summary = "not_verified: CRLF/Unicode definition is unsupported: " + queryErr.Error()
			} else {
				t.Errorf("%s", check.Summary)
			}
		} else if upstreamUnicodeErr != nil {
			check.Status = report.NotVerified
			check.Summary = "not_verified: pinned gopls Unicode/CRLF position observation failed: " + upstreamUnicodeErr.Error()
		} else {
			if !reflect.DeepEqual(locations, upstreamLocations) {
				check.Status = report.Failed
				check.Summary = fmt.Sprintf("candidate and pinned gopls Unicode/CRLF position results differ: candidate=%+v pinned=%+v", locations, upstreamLocations)
				t.Errorf("%s", check.Summary)
			}
		}
		checks = append(checks, check)
	}

	brokenPath := filepath.Join(workspace, "go-broken", "broken.go")
	brokenURI := uri.FromPath(brokenPath).String()
	brokenText := files["go-broken/broken.go"]
	brokenSyntaxLine, _ := semanticPositionOf(brokenText, "func broken", 0)
	brokenEventStart := len(session.Events())
	upstreamBrokenCursor := uint64(0)
	if goUpstream != nil {
		upstreamBrokenCursor = goUpstream.NotificationCursor()
	}
	session.Notify(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": brokenURI, "languageId": "go", "version": 1, "text": brokenText}})
	upstreamBrokenDiagnosticProblem := semanticErrString(goUpstreamErr)
	if goUpstream != nil {
		if err := goUpstream.Notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": brokenURI, "languageId": "go", "version": 1, "text": brokenText}}); err != nil {
			upstreamBrokenDiagnosticProblem = "send malformed-source didOpen to pinned gopls: " + err.Error()
		}
	}
	brokenDiagnostics, brokenDiagnosticsVersion, hasBrokenDiagnostics := semanticWaitDiagnostics(session, brokenURI, brokenEventStart, 1, 10*time.Second)
	var upstreamBrokenDiagnostics []semanticDiagnostic
	upstreamBrokenDiagnosticsVersion := 0
	upstreamHasBrokenDiagnostics := false
	if goUpstream != nil && upstreamBrokenDiagnosticProblem == "" {
		upstreamBrokenDiagnostics, upstreamBrokenDiagnosticsVersion, upstreamHasBrokenDiagnostics, upstreamBrokenDiagnosticProblem = semanticWaitUpstreamDiagnostics(goUpstream, brokenURI, upstreamBrokenCursor, 1, 10*time.Second)
	}
	brokenDiagnosticCheck := report.Check{
		ID:      "S20/broken-source-diagnostics",
		Status:  report.NotVerified,
		Summary: "not_verified: version-1 malformed-code diagnostics were not observed from candidate and pinned gopls",
		Observed: map[string]any{
			"candidateDiagnostics": brokenDiagnostics, "candidateDiagnosticsVersion": brokenDiagnosticsVersion,
			"upstreamDiagnostics": upstreamBrokenDiagnostics, "upstreamDiagnosticsVersion": upstreamBrokenDiagnosticsVersion,
			"upstreamProblem": upstreamBrokenDiagnosticProblem, "expectedSyntaxLine": brokenSyntaxLine,
		},
	}
	if hasBrokenDiagnostics && upstreamHasBrokenDiagnostics {
		candidateMalformedErrors := semanticMalformedSourceErrors(brokenDiagnostics, brokenSyntaxLine)
		upstreamMalformedErrors := semanticMalformedSourceErrors(upstreamBrokenDiagnostics, brokenSyntaxLine)
		matches, comparison := semanticCompareDiagnostics(brokenDiagnostics, upstreamBrokenDiagnostics)
		brokenDiagnosticCheck.Observed["candidateMalformedErrors"] = candidateMalformedErrors
		brokenDiagnosticCheck.Observed["upstreamMalformedErrors"] = upstreamMalformedErrors
		brokenDiagnosticCheck.Observed["differential"] = comparison
		if len(candidateMalformedErrors) == 0 || len(upstreamMalformedErrors) == 0 || !matches {
			brokenDiagnosticCheck.Status = report.Failed
			brokenDiagnosticCheck.Summary = "malformed-source diagnostics differ semantically from pinned gopls or omit the syntax error"
			t.Errorf("%s", brokenDiagnosticCheck.Summary)
		} else {
			brokenDiagnosticCheck.Status = report.Passed
			brokenDiagnosticCheck.Summary = "candidate malformed-source severity/range diagnostics match pinned gopls; message and source-label differences are reported separately"
		}
	}
	checks = append(checks, brokenDiagnosticCheck)
	brokenLine, brokenChar := semanticPositionOf(brokenText, "wanted", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	brokenRaw, brokenErr := session.RequestContext(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]string{"uri": brokenURI},
		"position":     map[string]uint32{"line": brokenLine, "character": brokenChar},
		"context":      map[string]bool{"includeDeclaration": true},
	})
	cancel()
	brokenLocations, brokenDecodeErr := normalizeSemanticLocations(brokenRaw)
	wantedDeclaration := semanticLocation{URI: semanticCanonicalURI(brokenURI), StartLine: 1, StartChar: 4, EndLine: 1, EndChar: 10}
	wantedUse := semanticLocation{URI: semanticCanonicalURI(brokenURI), StartLine: 3, StartChar: 8, EndLine: 3, EndChar: 14}
	broken := semanticBrokenSourceCheck(brokenErr, brokenDecodeErr, brokenLocations, []semanticLocation{wantedDeclaration, wantedUse})
	broken.Observed["queriedIdentifier"] = "wanted"
	broken.Observed["expectedLocations"] = []semanticLocation{wantedDeclaration, wantedUse}
	if broken.Status == report.Failed {
		t.Errorf("%s", broken.Summary)
	}
	checks = append(checks, broken)

	fixedBrokenText := strings.Replace(brokenText, "func broken( {\n", "", 1)
	fixedEventStart := len(session.Events())
	upstreamFixedCursor := uint64(0)
	if goUpstream != nil {
		upstreamFixedCursor = goUpstream.NotificationCursor()
	}
	repairApplied := fixedBrokenText != brokenText
	fixedDiagnostics := []semanticDiagnostic(nil)
	fixedDiagnosticsVersion := 0
	hasFixedDiagnostics := false
	var upstreamFixedDiagnostics []semanticDiagnostic
	upstreamFixedDiagnosticsVersion := 0
	upstreamHasFixedDiagnostics := false
	upstreamFixedDiagnosticProblem := semanticErrString(goUpstreamErr)
	if repairApplied {
		session.Notify(t, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": brokenURI, "version": 2}, "contentChanges": []map[string]string{{"text": fixedBrokenText}}})
		if goUpstream != nil {
			if err := goUpstream.Notify("textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": brokenURI, "version": 2}, "contentChanges": []map[string]string{{"text": fixedBrokenText}}}); err != nil {
				upstreamFixedDiagnosticProblem = "send repair didChange to pinned gopls: " + err.Error()
			}
		}
		fixedDiagnostics, fixedDiagnosticsVersion, hasFixedDiagnostics = semanticWaitDiagnostics(session, brokenURI, fixedEventStart, 2, 10*time.Second)
		if goUpstream != nil && upstreamFixedDiagnosticProblem == "" {
			upstreamFixedDiagnostics, upstreamFixedDiagnosticsVersion, upstreamHasFixedDiagnostics, upstreamFixedDiagnosticProblem = semanticWaitUpstreamDiagnostics(goUpstream, brokenURI, upstreamFixedCursor, 2, 10*time.Second)
		}
	}
	fixedDiagnosticCheck := report.Check{
		ID:      "S20/diagnostic-edit",
		Status:  report.NotVerified,
		Summary: "not_verified: fresh version-2 diagnostics after the syntax repair were not observed from candidate and pinned gopls",
		Observed: map[string]any{
			"candidateDiagnostics": fixedDiagnostics, "candidateDiagnosticsVersion": fixedDiagnosticsVersion,
			"upstreamDiagnostics": upstreamFixedDiagnostics, "upstreamDiagnosticsVersion": upstreamFixedDiagnosticsVersion,
			"upstreamProblem": upstreamFixedDiagnosticProblem, "malformedLineRemoved": repairApplied,
		},
	}
	if !repairApplied {
		fixedDiagnosticCheck.Status = report.Failed
		fixedDiagnosticCheck.Summary = "the malformed syntax fixture was not removed by the repair edit"
		t.Errorf("%s", fixedDiagnosticCheck.Summary)
	} else if hasFixedDiagnostics && upstreamHasFixedDiagnostics {
		remainingErrors := []semanticDiagnostic{}
		for _, diagnostic := range fixedDiagnostics {
			if diagnostic.Severity != nil && *diagnostic.Severity == 1 {
				remainingErrors = append(remainingErrors, diagnostic)
			}
		}
		upstreamRemainingErrors := []semanticDiagnostic{}
		for _, diagnostic := range upstreamFixedDiagnostics {
			if diagnostic.Severity != nil && *diagnostic.Severity == 1 {
				upstreamRemainingErrors = append(upstreamRemainingErrors, diagnostic)
			}
		}
		matches, comparison := semanticCompareDiagnostics(fixedDiagnostics, upstreamFixedDiagnostics)
		fixedDiagnosticCheck.Observed["candidateRemainingErrors"] = remainingErrors
		fixedDiagnosticCheck.Observed["upstreamRemainingErrors"] = upstreamRemainingErrors
		fixedDiagnosticCheck.Observed["differential"] = comparison
		if len(remainingErrors) == 0 && len(upstreamRemainingErrors) == 0 && matches {
			fixedDiagnosticCheck.Status = report.Passed
			fixedDiagnosticCheck.Summary = "fresh version-2 diagnostics after repair match pinned gopls with no remaining errors; message and source-label differences are reported separately"
		} else {
			fixedDiagnosticCheck.Status = report.Failed
			fixedDiagnosticCheck.Summary = "syntax repair left severity-1 diagnostics or differs semantically from pinned gopls"
			t.Errorf("%s", fixedDiagnosticCheck.Summary)
		}
	}
	checks = append(checks, fixedDiagnosticCheck)

	// Exported Go rename has unknown importer completeness and must fail closed.
	exportedLine, exportedChar := semanticPositionOf(targetText, "Exported", 0)
	renameParams := map[string]any{
		"textDocument": map[string]string{"uri": targetURI},
		"position":     map[string]uint32{"line": exportedLine, "character": exportedChar},
		"newName":      "RenamedExported",
	}
	beforeTargetDisk, _ := os.ReadFile(targetPath)
	beforeUseDisk, _ := os.ReadFile(usePath)
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	_, renameErr := session.RequestContext(ctx, "textDocument/rename", renameParams)
	cancel()
	var rpcErr *jsonrpc.ResponseError
	refused := errors.As(renameErr, &rpcErr) && rpcErr.Code == jsonrpc.RequestFailed &&
		(strings.Contains(strings.ToLower(rpcErr.Message), "exported") || strings.Contains(strings.ToLower(rpcErr.Message), "completeness"))
	afterTargetDisk, _ := os.ReadFile(targetPath)
	afterUseDisk, _ := os.ReadFile(usePath)
	filesUnchanged := string(beforeTargetDisk) == string(afterTargetDisk) && string(beforeUseDisk) == string(afterUseDisk)
	exportedRenameRefused := refused && filesUnchanged
	rename := report.Check{ID: "S20/rename-safety", Status: report.Passed, Summary: "unproven exported rename was refused without changing source files", Observed: map[string]any{"requestError": semanticErrString(renameErr), "errorCode": errorCode(rpcErr), "filesUnchanged": filesUnchanged}}
	if !exportedRenameRefused {
		rename.Status = report.Failed
		rename.Summary = "SEM-SAFE-001 violation: rename was not refused with RequestFailed or source files changed"
		if semanticUnsupportedError(renameErr) {
			rename.Status = report.NotVerified
			rename.Summary = "not_verified: candidate rename-safety behavior is unsupported: " + renameErr.Error()
		} else {
			t.Errorf("%s (error=%v code=%d)", rename.Summary, renameErr, errorCode(rpcErr))
		}
	}
	checks = append(checks, rename)
	localRename := semanticRunSafeLocalRenameProbe(t, session, renameOracle, semanticOracleLabel("gopls", toolVersions), renameOracleOpenError, useURI, useText)
	checks = append(checks, semanticObservedRenameRefusalKPI(localRename, exportedRenameRefused))
	checks = append(checks, semanticRunStaleResultProbe(t, session, workspace))
	checks = append(checks, semanticRunRenameStaleProbe(t, session, workspace))
	return checks
}

// semanticObservedRenameRefusalKPI combines the two Go rename requests that
// have directly observable outcomes: the exported rename is explicitly
// refused when importer completeness is unknown, and the safe local rename
// returns a nonempty WorkspaceEdit only after ValidateEditSet accepts it.
// If either outcome was not observed, the refusal rate remains unsampled.
func semanticObservedRenameRefusalKPI(localRename report.Check, exportedRenameRefused bool) report.Check {
	if !exportedRenameRefused {
		return localRename
	}
	observations, ok := localRename.Observed["kpiObservations"].([]telemetry.Record)
	if !ok {
		return localRename
	}
	validatedLocalRename := false
	for _, record := range observations {
		if record.Dimension.Feature != "rename" || record.Dimension.Language != "Go" || record.Dimension.Backend != "golang" {
			continue
		}
		metric := record.Metrics.EditValidationFailureRate
		if metric.Status == telemetry.MetricObserved && metric.Numerator != nil && *metric.Numerator == 0 && metric.Denominator != nil && *metric.Denominator == 1 {
			validatedLocalRename = true
			break
		}
	}
	if !validatedLocalRename {
		return localRename
	}

	dimension := telemetry.Dimension{Feature: "rename", Language: "Go", Backend: "golang", Oracle: "candidate.ValidateEditSet"}
	metric := telemetry.NewRecord(dimension, telemetry.Inputs{
		Refusals: telemetry.Sampled(1), Requests: telemetry.Sampled(2),
	}).Metrics.RefusalRate
	updates, _ := localRename.Observed["kpiMetricObservations"].([]semanticKPIMetricObservation)
	localRename.Observed["kpiMetricObservations"] = append(updates, semanticKPIUpdate(dimension, "refusalRate", metric)...)
	return localRename
}

func semanticRunSafeLocalRenameProbe(t *testing.T, session, oracle semanticRequestClient, oracleLabel string, oracleOpenError error, useURI, source string) report.Check {
	t.Helper()
	check := report.Check{ID: "S20/rename-local-edit-validation", Status: report.NotVerified}
	dimension := telemetry.Dimension{Feature: "rename", Language: "Go", Backend: "golang", Oracle: "candidate.ValidateEditSet"}
	setUnverified := func(reason string) {
		metric := semanticNotVerifiedMetric("editValidationFailureRate", reason)
		check.Observed["kpiMetricObservations"] = semanticKPIUpdate(dimension, "editValidationFailureRate", metric)
		check.Summary = "not_verified: " + reason
	}
	const symbol, newName = "localScopeValue", "renamedLocalScopeValue"
	line, character := semanticPositionOf(source, symbol, 0)
	if line == ^uint32(0) {
		check.Status = report.Failed
		check.Observed = map[string]any{}
		check.Summary = "the Go semantic fixture does not contain the safe local rename symbol"
		setUnverified("no validation attempt occurred because the safe local rename fixture is missing")
		return check
	}
	params := map[string]any{
		"textDocument": map[string]string{"uri": useURI},
		"position":     map[string]uint32{"line": line, "character": character},
		"newName":      newName,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	raw, requestErr := session.RequestContext(ctx, "textDocument/rename", params)
	cancel()
	_, edits, validationSampled, editEvidenceErr := semanticValidatedEditKPI(raw, requestErr, dimension)
	check.Observed = map[string]any{
		"uri": useURI, "oldName": symbol, "newName": newName,
		"position": []uint32{line, character}, "requestError": semanticErrString(requestErr),
	}
	if requestErr != nil {
		setUnverified("no validated WorkspaceEdit was returned; the request ended before an observable successful edit-validation result: " + requestErr.Error())
		if !semanticUnsupportedError(requestErr) {
			check.Status = report.Failed
			check.Summary = "safe local-variable rename request failed: " + requestErr.Error()
		}
		return check
	}
	check.Observed["workspaceEdit"] = raw
	check.Observed["normalizedEdits"] = edits
	check.Observed["workspaceEditDecodeError"] = semanticErrString(editEvidenceErr)
	if editEvidenceErr != nil || !validationSampled {
		reason := "candidate did not produce a validated WorkspaceEdit observation"
		if editEvidenceErr != nil {
			reason = editEvidenceErr.Error()
		}
		setUnverified("candidate returned no nonempty, normalizable WorkspaceEdit, so a successful edit-validation result was not observed: " + reason)
		check.Status = report.Failed
		check.Summary = "safe local-variable rename returned an invalid or empty WorkspaceEdit: " + reason
		return check
	}

	startLine, startChar := semanticPositionOf(source, symbol, 0)
	secondLine, secondChar := semanticPositionOf(source, symbol, 1)
	width := uint32(len(utf16.Encode([]rune(symbol))))
	want := []semanticEditObservation{
		{URI: semanticCanonicalURI(useURI), StartLine: startLine, StartChar: startChar, EndLine: startLine, EndChar: startChar + width, NewText: newName},
		{URI: semanticCanonicalURI(useURI), StartLine: secondLine, StartChar: secondChar, EndLine: secondLine, EndChar: secondChar + width, NewText: newName},
	}
	sort.Slice(want, func(i, j int) bool {
		return want[i].StartLine < want[j].StartLine || (want[i].StartLine == want[j].StartLine && want[i].StartChar < want[j].StartChar)
	})
	check.Observed["expectedEdits"] = want
	// A nonempty WorkspaceEdit returned by the candidate passed through the
	// server's ValidateEditSet gate. Refusals and malformed output never enter
	// the denominator.
	oraclePositiveEdits := []semanticEditObservation(nil)
	var oraclePositiveRaw json.RawMessage
	var oraclePositiveErr error
	var oraclePositiveDecodeError error
	if oracle != nil {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		oraclePositiveRaw, oraclePositiveErr = oracle.RequestContext(ctx, "textDocument/rename", params)
		cancel()
		if oraclePositiveErr == nil {
			oraclePositiveEdits, oraclePositiveDecodeError = normalizeSemanticWorkspaceEdit(oraclePositiveRaw)
		}
	} else if oracleOpenError != nil {
		oraclePositiveErr = oracleOpenError
	} else {
		oraclePositiveErr = errors.New("pinned gopls session is unavailable")
	}
	check.Observed["pinnedWorkspaceEdit"] = oraclePositiveRaw
	check.Observed["pinnedRequestError"] = semanticErrString(oraclePositiveErr)
	check.Observed["pinnedWorkspaceEditDecodeError"] = semanticErrString(oraclePositiveDecodeError)

	negativeLine, negativeChar := semanticPositionOf(source, "missingProbe", 0)
	negativeCandidateEdits, negativeCandidateObserved, negativeCandidateReason := []semanticEditObservation(nil), false, "missingProbe comment was not present in the local rename fixture"
	negativeOracleEdits, negativeOracleObserved, negativeOracleReason := []semanticEditObservation(nil), false, "pinned gopls session is unavailable"
	var negativeCandidateRaw, negativeOracleRaw json.RawMessage
	var negativeCandidateErr, negativeOracleErr error
	if negativeLine != ^uint32(0) {
		negativeParams := map[string]any{
			"textDocument": map[string]string{"uri": useURI},
			"position":     map[string]uint32{"line": negativeLine, "character": negativeChar},
			"newName":      "renamedMissingProbe",
		}
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		negativeCandidateRaw, negativeCandidateErr = session.RequestContext(ctx, "textDocument/rename", negativeParams)
		cancel()
		negativeCandidateEdits, negativeCandidateObserved, negativeCandidateReason = semanticRenameRequestOutcome(negativeCandidateRaw, negativeCandidateErr)
		if oracle != nil {
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			negativeOracleRaw, negativeOracleErr = oracle.RequestContext(ctx, "textDocument/rename", negativeParams)
			cancel()
			negativeOracleEdits, negativeOracleObserved, negativeOracleReason = semanticRenameRequestOutcome(negativeOracleRaw, negativeOracleErr)
		}
	}
	check.Observed["negativeRenameProbe"] = map[string]any{
		"position":     []uint32{negativeLine, negativeChar},
		"query":        "rename at a comment-only token must produce no WorkspaceEdit",
		"candidateRaw": negativeCandidateRaw, "candidateError": semanticErrString(negativeCandidateErr),
		"candidateObserved": negativeCandidateObserved, "candidateReason": negativeCandidateReason,
		"pinnedRaw": negativeOracleRaw, "pinnedError": semanticErrString(negativeOracleErr),
		"pinnedObserved": negativeOracleObserved, "pinnedReason": negativeOracleReason,
	}

	positivePairObserved := oraclePositiveErr == nil && oraclePositiveDecodeError == nil && len(oraclePositiveEdits) > 0
	kpiInputs := semanticRenameKPIInputs(edits, oraclePositiveEdits, positivePairObserved,
		negativeCandidateEdits, negativeCandidateObserved, negativeOracleObserved && len(negativeOracleEdits) == 0)
	kpiInputs.EditValidationFailures = telemetry.Sampled(0)
	kpiInputs.EditValidationAttempts = telemetry.Sampled(1)
	if strings.TrimSpace(oracleLabel) == "" {
		oracleLabel = "pinned gopls not observed"
	}
	kpiDimension := telemetry.Dimension{Feature: "rename", Language: "Go", Backend: "golang", Oracle: oracleLabel}
	kpiDimension.Oracle += " + candidate.ValidateEditSet"
	check.Observed["kpiObservations"] = []telemetry.Record{telemetry.NewRecord(kpiDimension, kpiInputs)}
	check.Observed["renameKPIEvidence"] = map[string]any{
		"positivePairObserved":   positivePairObserved,
		"positiveCandidateEdits": edits, "positivePinnedEdits": oraclePositiveEdits,
		"positiveOracleError":         semanticErrString(oraclePositiveErr),
		"positiveOracleDecodeError":   semanticErrString(oraclePositiveDecodeError),
		"verifiedNegativeOracleEmpty": negativeOracleObserved && len(negativeOracleEdits) == 0,
	}
	if !reflect.DeepEqual(edits, want) {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("validated safe local rename WorkspaceEdit differs from the exact declaration/use edits: got=%+v want=%+v", edits, want)
		return check
	}
	if positivePairObserved && !reflect.DeepEqual(edits, oraclePositiveEdits) {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("validated safe local rename differs from pinned gopls: candidate=%+v pinned=%+v", edits, oraclePositiveEdits)
		return check
	}
	if negativeOracleObserved && len(negativeOracleEdits) == 0 && negativeCandidateObserved && len(negativeCandidateEdits) > 0 {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("candidate returned a WorkspaceEdit for a comment-only rename probe that pinned gopls rejected: candidate=%+v", negativeCandidateEdits)
		return check
	}
	check.Status = report.Passed
	check.Summary = "safe local-variable rename returned the exact declaration/use WorkspaceEdit after server edit validation; the edit was compared with pinned gopls"
	return check
}

func semanticRenameRequestOutcome(raw json.RawMessage, requestErr error) ([]semanticEditObservation, bool, string) {
	if requestErr != nil {
		if semanticNoIdentifierError(requestErr) {
			return nil, true, "pinned semantic server reported no identifier at the comment-only cursor; no WorkspaceEdit was returned"
		}
		var rpcErr *jsonrpc.ResponseError
		message := ""
		if errors.As(requestErr, &rpcErr) {
			message = strings.ToLower(rpcErr.Message)
		}
		if errors.As(requestErr, &rpcErr) && (rpcErr.Code == jsonrpc.RequestFailed || rpcErr.Code == jsonrpc.InvalidParams) && semanticNoRenameableSymbolMessage(message) {
			return nil, true, "LSP request was explicitly refused at a location without a renameable symbol"
		}
		return nil, false, requestErr.Error()
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return nil, true, "server explicitly returned no rename result"
	}
	var fields map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, false, "rename response was not a JSON WorkspaceEdit or null"
	}
	hasChanges := len(fields["changes"]) > 0 && strings.TrimSpace(string(fields["changes"])) != "null" && strings.TrimSpace(string(fields["changes"])) != "{}"
	hasDocumentChanges := len(fields["documentChanges"]) > 0 && strings.TrimSpace(string(fields["documentChanges"])) != "null" && strings.TrimSpace(string(fields["documentChanges"])) != "[]"
	if !hasChanges && !hasDocumentChanges {
		return nil, true, "server returned an empty WorkspaceEdit"
	}
	edits, err := normalizeSemanticWorkspaceEdit(raw)
	if err != nil {
		return nil, false, err.Error()
	}
	return edits, true, "server returned a nonempty WorkspaceEdit"
}

func semanticNoRenameableSymbolMessage(message string) bool {
	for _, phrase := range []string{"no object", "no identifier", "no symbol", "not renameable", "no renameable", "cannot rename", "not a valid identifier"} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	return false
}

func semanticRenameKPIInputs(candidate, pinned []semanticEditObservation, positivePairObserved bool, negativeCandidate []semanticEditObservation, negativeCandidateObserved, negativePinnedEmpty bool) telemetry.Inputs {
	var inputs telemetry.Inputs
	var falsePositives int64
	falsePositiveObserved := false
	if positivePairObserved {
		candidateCounts := semanticEditCounts(candidate)
		pinnedCounts := semanticEditCounts(pinned)
		extra, missing := false, false
		for edit, count := range candidateCounts {
			if count > pinnedCounts[edit] {
				extra = true
			}
		}
		for edit, count := range pinnedCounts {
			if count > candidateCounts[edit] {
				missing = true
			}
		}
		truePositive, falseNegative := int64(0), int64(0)
		if !extra && !missing && len(candidate) > 0 {
			truePositive = 1
		} else {
			falseNegative = 1
		}
		if extra {
			falsePositives++
		}
		falsePositiveObserved = true
		inputs.TruePositive = telemetry.Sampled(truePositive)
		inputs.FalseNegative = telemetry.Sampled(falseNegative)

		pinnedFiles := make(map[string]struct{}, len(pinned))
		pinnedExact := make(map[semanticEditObservation]struct{}, len(pinned))
		for _, edit := range pinned {
			pinnedFiles[edit.URI] = struct{}{}
			pinnedExact[edit] = struct{}{}
		}
		wrongFiles, positionSamples, positionFailures := int64(0), int64(0), int64(0)
		for _, edit := range candidate {
			if _, ok := pinnedFiles[edit.URI]; !ok {
				wrongFiles++
				continue
			}
			positionSamples++
			if _, ok := pinnedExact[edit]; !ok {
				positionFailures++
			}
		}
		inputs.WrongFileLocations = telemetry.Sampled(wrongFiles)
		inputs.LocationResults = telemetry.Sampled(int64(len(candidate)))
		inputs.PositionMappingFailures = telemetry.Sampled(positionFailures)
		inputs.PositionMappingSamples = telemetry.Sampled(positionSamples)
	}
	if negativePinnedEmpty && negativeCandidateObserved {
		inputs.TrueNegative = telemetry.Sampled(0)
		if len(negativeCandidate) == 0 {
			inputs.TrueNegative = telemetry.Sampled(1)
		} else {
			falsePositives++
		}
		falsePositiveObserved = true
	}
	if falsePositiveObserved {
		inputs.FalsePositive = telemetry.Sampled(falsePositives)
	}
	return inputs
}

func semanticEditCounts(edits []semanticEditObservation) map[semanticEditObservation]int {
	counts := make(map[semanticEditObservation]int, len(edits))
	for _, edit := range edits {
		counts[edit]++
	}
	return counts
}

type semanticRequestClient interface {
	RequestContext(context.Context, string, any) (json.RawMessage, error)
}

type semanticProgressClient interface {
	semanticRequestClient
	NotificationsSinceAndCursor(uint64) ([]upstream.Notification, uint64, bool)
}

type semanticCandidateProgressClient interface {
	semanticRequestClient
	EventsSince(uint64) ([]*jsonrpc.Message, uint64, bool)
}

type semanticUpstreamReadiness struct {
	Attempts                   int                 `json:"attempts"`
	ElapsedMillis              int64               `json:"elapsedMillis"`
	StableCompleteSnapshots    int                 `json:"stableCompleteSnapshots"`
	RequiredDefinitionObserved bool                `json:"requiredDefinitionObserved"`
	RequiredCallObserved       bool                `json:"requiredCallObserved"`
	ActiveProgressTokensAtEnd  []string            `json:"activeProgressTokensAtEnd"`
	ProgressEvents             []map[string]string `json:"progressEvents"`
	LastProblem                string              `json:"lastProblem,omitempty"`
	LastDefinition             []semanticLocation  `json:"lastDefinition"`
	LastReferences             []semanticLocation  `json:"lastReferences"`
}

type semanticWorkDoneProgress struct {
	Token json.RawMessage `json:"token"`
	Value struct {
		Kind string `json:"kind"`
	} `json:"value"`
}

// semanticWaitUpstreamLocations waits for a complete positive upstream result
// and requires three identical snapshots before using it as the differential
// oracle. The candidate result is deliberately not part of the readiness
// predicate; the fixture's known declaration, call, and any required extra
// references are.
func semanticWaitUpstreamLocations(
	session semanticProgressClient,
	progressCursor uint64,
	definitionParams, referencesParams any,
	expectedDefinition, expectedCall semanticLocation,
	timeout time.Duration,
	additionalReferences ...semanticLocation,
) ([]semanticLocation, []semanticLocation, map[string]any, error) {
	started := time.Now()
	deadline := started.Add(timeout)
	readiness := semanticUpstreamReadiness{
		ProgressEvents: []map[string]string{},
		LastDefinition: []semanticLocation{},
		LastReferences: []semanticLocation{},
	}
	activeProgress := map[string]bool{}
	previousDefinition := []semanticLocation(nil)
	previousReferences := []semanticLocation(nil)
	stable := 0
	cursor := progressCursor
	var lastErr error

	for time.Now().Before(deadline) {
		readiness.Attempts++
		definitionRequest, err := semanticParamsWithWorkDoneToken(definitionParams, fmt.Sprintf("s20-semantic-%d-definition", readiness.Attempts))
		if err != nil {
			lastErr = err
			readiness.LastProblem = err.Error()
			break
		}
		referencesRequest, err := semanticParamsWithWorkDoneToken(referencesParams, fmt.Sprintf("s20-semantic-%d-references", readiness.Attempts))
		if err != nil {
			lastErr = err
			readiness.LastProblem = err.Error()
			break
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		definitionRaw, definitionErr := session.RequestContext(ctx, "textDocument/definition", definitionRequest)
		var referencesRaw json.RawMessage
		var referencesErr error
		if definitionErr == nil {
			referencesRaw, referencesErr = session.RequestContext(ctx, "textDocument/references", referencesRequest)
		} else {
			referencesErr = definitionErr
		}
		cancel()

		var definitions, references []semanticLocation
		if definitionErr == nil {
			definitions, err = normalizeSemanticLocations(definitionRaw)
			if err != nil {
				definitionErr = fmt.Errorf("normalize upstream definition: %w", err)
			}
		}
		if referencesErr == nil {
			references, err = normalizeSemanticLocations(referencesRaw)
			if err != nil {
				referencesErr = fmt.Errorf("normalize upstream references: %w", err)
			}
		}
		if definitions == nil {
			definitions = []semanticLocation{}
		}
		if references == nil {
			references = []semanticLocation{}
		}
		readiness.LastDefinition = definitions
		readiness.LastReferences = references
		if definitionErr != nil {
			lastErr = definitionErr
			readiness.LastProblem = "definition: " + definitionErr.Error()
		} else if referencesErr != nil {
			lastErr = referencesErr
			readiness.LastProblem = "references: " + referencesErr.Error()
		} else {
			lastErr = nil
			readiness.LastProblem = ""
		}

		notifications, nextCursor, overflow := session.NotificationsSinceAndCursor(cursor)
		if overflow {
			lastErr = errors.New("upstream progress notification history overflowed")
			readiness.LastProblem = lastErr.Error()
			break
		}
		semanticApplyWorkDoneProgress(notifications, activeProgress, &readiness)
		cursor = nextCursor

		complete := definitionErr == nil && referencesErr == nil && len(activeProgress) == 0 &&
			containsSemanticLocation(definitions, expectedDefinition) &&
			containsSemanticLocation(references, expectedDefinition) &&
			containsSemanticLocation(references, expectedCall)
		for _, required := range additionalReferences {
			complete = complete && containsSemanticLocation(references, required)
		}
		readiness.RequiredDefinitionObserved = containsSemanticLocation(definitions, expectedDefinition) && containsSemanticLocation(references, expectedDefinition)
		readiness.RequiredCallObserved = containsSemanticLocation(references, expectedCall)
		if complete {
			if stable > 0 && reflect.DeepEqual(previousDefinition, definitions) && reflect.DeepEqual(previousReferences, references) {
				stable++
			} else {
				stable = 1
			}
			previousDefinition = append([]semanticLocation(nil), definitions...)
			previousReferences = append([]semanticLocation(nil), references...)
		} else {
			stable = 0
		}
		readiness.StableCompleteSnapshots = stable
		if stable >= 3 {
			readiness.ElapsedMillis = time.Since(started).Milliseconds()
			readiness.ActiveProgressTokensAtEnd = semanticSortedProgressTokens(activeProgress)
			return definitions, references, semanticReadinessObserved(readiness), nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		pause := 250 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		time.Sleep(pause)
	}

	readiness.ElapsedMillis = time.Since(started).Milliseconds()
	readiness.ActiveProgressTokensAtEnd = semanticSortedProgressTokens(activeProgress)
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out after %d attempts before three identical complete positive upstream snapshots", readiness.Attempts)
	} else {
		lastErr = fmt.Errorf("timed out after %d attempts before three identical complete positive upstream snapshots; last upstream result: %w", readiness.Attempts, lastErr)
	}
	return readiness.LastDefinition, readiness.LastReferences, semanticReadinessObserved(readiness), lastErr
}

type semanticCandidateReadiness struct {
	Attempts                   int                 `json:"attempts"`
	ElapsedMillis              int64               `json:"elapsedMillis"`
	StableCompleteSnapshots    int                 `json:"stableCompleteSnapshots"`
	RequiredDefinitionObserved bool                `json:"requiredDefinitionObserved"`
	RequiredCallObserved       bool                `json:"requiredCallObserved"`
	ActiveProgressTokensAtEnd  []string            `json:"activeProgressTokensAtEnd"`
	ProgressEvents             []map[string]string `json:"progressEvents"`
	LastProblem                string              `json:"lastProblem,omitempty"`
	LastDefinition             []semanticLocation  `json:"lastDefinition"`
	LastReferences             []semanticLocation  `json:"lastReferences"`
}

// semanticWaitCandidateLocations waits until the candidate can resolve the
// fixture's known declaration, queried call, and required extra references,
// reports no active workDone
// progress, and returns three identical positive snapshots. Stable output is
// not treated as correct here; the caller still compares every normalized
// location against the pinned upstream result.
func semanticWaitCandidateLocations(
	session semanticCandidateProgressClient,
	progressCursor uint64,
	definitionParams, referencesParams any,
	expectedDefinition, expectedCall semanticLocation,
	timeout time.Duration,
	additionalReferences ...semanticLocation,
) ([]semanticLocation, []semanticLocation, map[string]any, error) {
	started := time.Now()
	deadline := started.Add(timeout)
	readiness := semanticCandidateReadiness{
		ProgressEvents: []map[string]string{},
		LastDefinition: []semanticLocation{},
		LastReferences: []semanticLocation{},
	}
	activeProgress := map[string]bool{}
	previousDefinition := []semanticLocation(nil)
	previousReferences := []semanticLocation(nil)
	stable := 0
	cursor := progressCursor
	var lastErr error

	for time.Now().Before(deadline) {
		readiness.Attempts++
		definitionRequest, err := semanticParamsWithWorkDoneToken(definitionParams, fmt.Sprintf("s20-candidate-%d-definition", readiness.Attempts))
		if err != nil {
			lastErr = err
			readiness.LastProblem = err.Error()
			break
		}
		referencesRequest, err := semanticParamsWithWorkDoneToken(referencesParams, fmt.Sprintf("s20-candidate-%d-references", readiness.Attempts))
		if err != nil {
			lastErr = err
			readiness.LastProblem = err.Error()
			break
		}
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		definitionRaw, definitionErr := session.RequestContext(ctx, "textDocument/definition", definitionRequest)
		var referencesRaw json.RawMessage
		var referencesErr error
		if definitionErr == nil {
			referencesRaw, referencesErr = session.RequestContext(ctx, "textDocument/references", referencesRequest)
		} else {
			referencesErr = definitionErr
		}
		cancel()

		var definitions, references []semanticLocation
		if definitionErr == nil {
			definitions, err = normalizeSemanticLocations(definitionRaw)
			if err != nil {
				definitionErr = fmt.Errorf("normalize candidate definition: %w", err)
			}
		}
		if referencesErr == nil {
			references, err = normalizeSemanticLocations(referencesRaw)
			if err != nil {
				referencesErr = fmt.Errorf("normalize candidate references: %w", err)
			}
		}
		if definitions == nil {
			definitions = []semanticLocation{}
		}
		if references == nil {
			references = []semanticLocation{}
		}
		readiness.LastDefinition = definitions
		readiness.LastReferences = references
		if definitionErr != nil {
			lastErr = definitionErr
			readiness.LastProblem = "definition: " + definitionErr.Error()
		} else if referencesErr != nil {
			lastErr = referencesErr
			readiness.LastProblem = "references: " + referencesErr.Error()
		} else {
			lastErr = nil
			readiness.LastProblem = ""
		}

		events, nextCursor, overflow := session.EventsSince(cursor)
		if overflow {
			lastErr = errors.New("candidate progress event history overflowed")
			readiness.LastProblem = lastErr.Error()
			break
		}
		notifications := make([]upstream.Notification, 0, len(events))
		for _, event := range events {
			if event != nil {
				notifications = append(notifications, upstream.Notification{Method: event.Method, Params: event.Params})
			}
		}
		semanticApplyCandidateWorkDoneProgress(notifications, activeProgress, &readiness)
		cursor = nextCursor

		complete := definitionErr == nil && referencesErr == nil && len(activeProgress) == 0 &&
			containsSemanticLocation(definitions, expectedDefinition) &&
			containsSemanticLocation(references, expectedDefinition) &&
			containsSemanticLocation(references, expectedCall)
		for _, required := range additionalReferences {
			complete = complete && containsSemanticLocation(references, required)
		}
		readiness.RequiredDefinitionObserved = containsSemanticLocation(definitions, expectedDefinition) && containsSemanticLocation(references, expectedDefinition)
		readiness.RequiredCallObserved = containsSemanticLocation(references, expectedCall)
		if complete {
			if stable > 0 && reflect.DeepEqual(previousDefinition, definitions) && reflect.DeepEqual(previousReferences, references) {
				stable++
			} else {
				stable = 1
			}
			previousDefinition = append([]semanticLocation(nil), definitions...)
			previousReferences = append([]semanticLocation(nil), references...)
		} else {
			stable = 0
		}
		readiness.StableCompleteSnapshots = stable
		if stable >= 3 {
			readiness.ElapsedMillis = time.Since(started).Milliseconds()
			readiness.ActiveProgressTokensAtEnd = semanticSortedProgressTokens(activeProgress)
			return definitions, references, semanticCandidateReadinessObserved(readiness), nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		pause := 250 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		time.Sleep(pause)
	}

	readiness.ElapsedMillis = time.Since(started).Milliseconds()
	readiness.ActiveProgressTokensAtEnd = semanticSortedProgressTokens(activeProgress)
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out after %d attempts before three identical complete positive candidate snapshots", readiness.Attempts)
	} else {
		lastErr = fmt.Errorf("timed out after %d attempts before three identical complete positive candidate snapshots; last candidate result: %w", readiness.Attempts, lastErr)
	}
	return readiness.LastDefinition, readiness.LastReferences, semanticCandidateReadinessObserved(readiness), lastErr
}

func semanticCandidateReadinessObserved(readiness semanticCandidateReadiness) map[string]any {
	if readiness.ActiveProgressTokensAtEnd == nil {
		readiness.ActiveProgressTokensAtEnd = []string{}
	}
	return map[string]any{
		"attempts": readiness.Attempts, "elapsedMillis": readiness.ElapsedMillis,
		"stableCompleteSnapshots":    readiness.StableCompleteSnapshots,
		"requiredDefinitionObserved": readiness.RequiredDefinitionObserved,
		"requiredCallObserved":       readiness.RequiredCallObserved,
		"activeProgressTokensAtEnd":  readiness.ActiveProgressTokensAtEnd,
		"progressEvents":             readiness.ProgressEvents,
		"lastProblem":                readiness.LastProblem,
		"lastDefinition":             readiness.LastDefinition,
		"lastReferences":             readiness.LastReferences,
	}
}

func semanticApplyCandidateWorkDoneProgress(notifications []upstream.Notification, active map[string]bool, readiness *semanticCandidateReadiness) {
	for _, notification := range notifications {
		if notification.Method != "$/progress" {
			continue
		}
		var progress semanticWorkDoneProgress
		if err := json.Unmarshal(notification.Params, &progress); err != nil || len(progress.Token) == 0 {
			continue
		}
		token := string(progress.Token)
		switch progress.Value.Kind {
		case "begin":
			active[token] = true
		case "end":
			delete(active, token)
		}
		if len(readiness.ProgressEvents) < 64 {
			readiness.ProgressEvents = append(readiness.ProgressEvents, map[string]string{"token": token, "kind": progress.Value.Kind})
		}
	}
}

func semanticParamsWithWorkDoneToken(params any, token string) (map[string]any, error) {
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	result["workDoneToken"] = token
	return result, nil
}

func semanticApplyWorkDoneProgress(notifications []upstream.Notification, active map[string]bool, readiness *semanticUpstreamReadiness) {
	for _, notification := range notifications {
		if notification.Method != "$/progress" {
			continue
		}
		var progress semanticWorkDoneProgress
		if err := json.Unmarshal(notification.Params, &progress); err != nil || len(progress.Token) == 0 {
			continue
		}
		token := string(progress.Token)
		switch progress.Value.Kind {
		case "begin":
			active[token] = true
		case "end":
			delete(active, token)
		}
		if len(readiness.ProgressEvents) < 64 {
			readiness.ProgressEvents = append(readiness.ProgressEvents, map[string]string{"token": token, "kind": progress.Value.Kind})
		}
	}
}

func semanticSortedProgressTokens(active map[string]bool) []string {
	tokens := make([]string, 0, len(active))
	for token := range active {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	return tokens
}

func semanticReadinessObserved(readiness semanticUpstreamReadiness) map[string]any {
	if readiness.ActiveProgressTokensAtEnd == nil {
		readiness.ActiveProgressTokensAtEnd = []string{}
	}
	return map[string]any{
		"attempts": readiness.Attempts, "elapsedMillis": readiness.ElapsedMillis,
		"stableCompleteSnapshots":    readiness.StableCompleteSnapshots,
		"requiredDefinitionObserved": readiness.RequiredDefinitionObserved,
		"requiredCallObserved":       readiness.RequiredCallObserved,
		"activeProgressTokensAtEnd":  readiness.ActiveProgressTokensAtEnd,
		"progressEvents":             readiness.ProgressEvents, "lastProblem": readiness.LastProblem,
		"lastDefinition": readiness.LastDefinition, "lastReferences": readiness.LastReferences,
	}
}

type semanticGoNegativeOracleResult struct {
	Definition           []semanticLocation
	References           []semanticLocation
	DefinitionError      string
	ReferencesError      string
	DefinitionNoLocation bool
	ReferencesNoLocation bool
	Err                  error
}

func semanticGoNegativeOraclePair(session semanticRequestClient, params any) semanticGoNegativeOracleResult {
	result := semanticGoNegativeOracleResult{Definition: []semanticLocation{}, References: []semanticLocation{}}
	definitionContext, definitionCancel := context.WithTimeout(context.Background(), 30*time.Second)
	definitionRaw, err := session.RequestContext(definitionContext, "textDocument/definition", params)
	definitionCancel()
	if err != nil {
		if !semanticNoIdentifierError(err) {
			result.Err = fmt.Errorf("definition request: %w", err)
			return result
		}
		result.DefinitionError = err.Error()
		result.DefinitionNoLocation = true
	} else {
		result.Definition, err = normalizeSemanticLocations(definitionRaw)
		if err != nil {
			result.Err = fmt.Errorf("normalize definition: %w", err)
			return result
		}
		result.DefinitionNoLocation = len(result.Definition) == 0
	}

	refsParams := map[string]any{}
	encoded, err := json.Marshal(params)
	if err != nil {
		result.Err = err
		return result
	}
	if err := json.Unmarshal(encoded, &refsParams); err != nil {
		result.Err = err
		return result
	}
	refsParams["context"] = map[string]bool{"includeDeclaration": true}
	referencesContext, referencesCancel := context.WithTimeout(context.Background(), 30*time.Second)
	referencesRaw, err := session.RequestContext(referencesContext, "textDocument/references", refsParams)
	referencesCancel()
	if err != nil {
		if !semanticNoIdentifierError(err) {
			result.Err = fmt.Errorf("references request: %w", err)
			return result
		}
		result.ReferencesError = err.Error()
		result.ReferencesNoLocation = true
		return result
	}
	result.References, err = normalizeSemanticLocations(referencesRaw)
	if err != nil {
		result.Err = fmt.Errorf("normalize references: %w", err)
		return result
	}
	result.ReferencesNoLocation = len(result.References) == 0
	return result
}

func semanticNoIdentifierError(err error) bool {
	return err != nil && strings.EqualFold(strings.TrimSpace(err.Error()), "jsonrpc error 0: no identifier found")
}

func semanticGoPair(session semanticRequestClient, params any) ([]semanticLocation, []semanticLocation, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	definitionRaw, err := session.RequestContext(ctx, "textDocument/definition", params)
	cancel()
	if err != nil {
		return nil, nil, err
	}
	definition, err := normalizeSemanticLocations(definitionRaw)
	if err != nil {
		return nil, nil, err
	}
	refsParams := map[string]any{}
	encoded, _ := json.Marshal(params)
	_ = json.Unmarshal(encoded, &refsParams)
	refsParams["context"] = map[string]bool{"includeDeclaration": true}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	refsRaw, err := session.RequestContext(ctx, "textDocument/references", refsParams)
	cancel()
	if err != nil {
		return definition, nil, err
	}
	references, err := normalizeSemanticLocations(refsRaw)
	return definition, references, err
}

// An empty projection is not proof of absence: Unknown and partial results can
// also project to null or []. Read the latest evidence for each completed probe.
func semanticVerifyNegativeEvidence(session semanticRequestClient, documentURI string) error {
	for _, method := range []string{"textDocument/definition", "textDocument/references"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		raw, err := session.RequestContext(ctx, "omnilsp/resultMeta", map[string]string{"uri": documentURI, "method": method})
		cancel()
		if err != nil {
			return fmt.Errorf("negative %s evidence: %w", method, err)
		}
		var entries []struct {
			Method       string `json:"method"`
			Status       string `json:"status"`
			Completeness string `json:"completeness"`
		}
		if err := json.Unmarshal(raw, &entries); err != nil {
			return fmt.Errorf("negative %s evidence decode: %w", method, err)
		}
		if len(entries) == 0 {
			return fmt.Errorf("negative %s has no current evidence", method)
		}
		latest := entries[len(entries)-1]
		if latest.Method != method || latest.Status != "exact" || latest.Completeness != "complete" {
			return fmt.Errorf("negative %s is not proven: status=%s completeness=%s", method, latest.Status, latest.Completeness)
		}
	}
	return nil
}

func normalizeSemanticLocations(raw json.RawMessage) ([]semanticLocation, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []semanticLocation{}, nil
	}
	var many []semanticWireLocation
	if err := json.Unmarshal(raw, &many); err != nil {
		var one semanticWireLocation
		if oneErr := json.Unmarshal(raw, &one); oneErr != nil || one.URI == "" {
			return nil, fmt.Errorf("decode locations %s: %w", string(raw), err)
		}
		many = []semanticWireLocation{one}
	}
	locations := make([]semanticLocation, 0, len(many))
	for _, location := range many {
		locations = append(locations, semanticLocation{URI: semanticCanonicalURI(location.URI), StartLine: location.Range.Start.Line, StartChar: location.Range.Start.Character, EndLine: location.Range.End.Line, EndChar: location.Range.End.Character})
	}
	sort.Slice(locations, func(i, j int) bool {
		a, b := locations[i], locations[j]
		if a.URI != b.URI {
			return a.URI < b.URI
		}
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}
		if a.StartChar != b.StartChar {
			return a.StartChar < b.StartChar
		}
		if a.EndLine != b.EndLine {
			return a.EndLine < b.EndLine
		}
		return a.EndChar < b.EndChar
	})
	unique := locations[:0]
	for _, location := range locations {
		if len(unique) == 0 || unique[len(unique)-1] != location {
			unique = append(unique, location)
		}
	}
	return unique, nil
}

func TestNormalizeSemanticLocationsDeduplicatesCanonicalAliases(t *testing.T) {
	raw := json.RawMessage(`[
		{"uri":"file:///C:/work/impl.cpp","range":{"start":{"line":2,"character":4},"end":{"line":2,"character":10}}},
		{"uri":"file:///c:/work/impl.cpp","range":{"start":{"line":2,"character":4},"end":{"line":2,"character":10}}},
		{"uri":"file:///C:/work/use.cpp","range":{"start":{"line":2,"character":26},"end":{"line":2,"character":32}}}
	]`)
	got, err := normalizeSemanticLocations(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("canonical location set = %+v, want two unique locations", got)
	}
}

func containsSemanticLocation(locations []semanticLocation, expected semanticLocation) bool {
	for _, location := range locations {
		if location == expected {
			return true
		}
	}
	return false
}

func semanticPositionOf(text, token string, occurrence int) (uint32, uint32) {
	if token == "" || occurrence < 0 {
		return ^uint32(0), ^uint32(0)
	}
	tokenStart, _ := utf8.DecodeRuneInString(token)
	tokenEnd, _ := utf8.DecodeLastRuneInString(token)
	search, index, found := 0, -1, 0
	for search <= len(text) {
		rel := strings.Index(text[search:], token)
		if rel < 0 {
			return ^uint32(0), ^uint32(0)
		}
		candidate := search + rel
		end := candidate + len(token)
		search = end
		if semanticIdentifierRune(tokenStart) && candidate > 0 {
			previous, _ := utf8.DecodeLastRuneInString(text[:candidate])
			if semanticIdentifierRune(previous) {
				continue
			}
		}
		if semanticIdentifierRune(tokenEnd) && end < len(text) {
			next, _ := utf8.DecodeRuneInString(text[end:])
			if semanticIdentifierRune(next) {
				continue
			}
		}
		if found == occurrence {
			index = candidate
			break
		}
		found++
	}
	if index < 0 {
		return ^uint32(0), ^uint32(0)
	}
	prefix := text[:index]
	line := uint32(strings.Count(prefix, "\n"))
	lastNewline := strings.LastIndex(prefix, "\n")
	column := uint32(0)
	for _, r := range prefix[lastNewline+1:] {
		column += uint32(len(utf16.Encode([]rune{r})))
	}
	return line, column
}

func semanticIdentifierRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r)
}

func semanticWriteFiles(root string, files map[string]string) error {
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

func semanticDigestInWorkspace(files map[string]string, workspace string) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	workspaceJSON, _ := json.Marshal(workspace)
	escapedWorkspace := ""
	if len(workspaceJSON) >= 2 {
		escapedWorkspace = string(workspaceJSON[1 : len(workspaceJSON)-1])
	}
	for _, name := range names {
		content := strings.ReplaceAll(files[name], workspace, "$WORKSPACE")
		content = strings.ReplaceAll(content, filepath.ToSlash(workspace), "$WORKSPACE")
		if escapedWorkspace != "" {
			content = strings.ReplaceAll(content, escapedWorkspace, "$WORKSPACE")
		}
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(content))
		_, _ = h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func semanticResolveBinary(t testing.TB) (string, error) {
	t.Helper()
	if configured := os.Getenv(semanticAcceptanceBinEnv); configured != "" {
		if !filepath.IsAbs(configured) {
			root, err := semanticRepoRoot()
			if err != nil {
				return "", err
			}
			configured = filepath.Join(root, configured)
		}
		abs, err := filepath.Abs(configured)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", fmt.Errorf("OMNILSP_BIN is a directory: %s", abs)
		}
		return abs, nil
	}
	if _, err := exec.LookPath("go"); err != nil {
		return "", fmt.Errorf("Go compiler unavailable and OMNILSP_BIN not set: %w", err)
	}
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot locate repository root for candidate build")
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
		return "", fmt.Errorf("build candidate: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return out, nil
}

func semanticGitRevision(t testing.TB) string {
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

func semanticRepoRoot() (string, error) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("cannot locate repository root")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..")), nil
}

func semanticToolVersions() map[string]string {
	tools := map[string][]string{
		"go": {"version"}, "gopls": {"version"}, "clangd": {"--version"}, "clang": {"--version"}, "clang++": {"--version"},
		"rust-analyzer": {"--version"}, "rustc": {"--version"}, "cargo": {"--version"},
		"python": {"--version"}, "python3": {"--version"}, "node": {"--version"}, "npm": {"--version"},
	}
	versions := map[string]string{}
	for name, args := range tools {
		path, err := exec.LookPath(name)
		if err != nil {
			versions[name] = "missing"
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		output, runErr := exec.CommandContext(ctx, path, args...).CombinedOutput()
		cancel()
		if runErr != nil {
			versions[name] = "installed at " + path + "; version probe failed: " + strings.TrimSpace(string(output))
		} else {
			versions[name] = strings.TrimSpace(string(output))
		}
	}
	versions["pyright"] = semanticLocalPackageVersion("pyright")
	versions["typescript-language-server"] = semanticLocalPackageVersion("typescript-language-server")
	versions["typescript"] = semanticLocalPackageVersion("typescript")
	if lockPath, err := semanticRepoRoot(); err == nil {
		data, err := os.ReadFile(filepath.Join(lockPath, "test", "acceptance", "tools", "tools.lock.json"))
		if err == nil {
			versions["tools.lock.json"] = "sha256:" + semanticSHA256(data)
		}
	}
	return versions
}

func semanticLockedToolVersionProblems(names []string, versions map[string]string) []string {
	root, err := semanticRepoRoot()
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
		actualName := name
		expected := ""
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

func TestS20_LockedTypeScriptLanguageServerUsesDashedVersionKey(t *testing.T) {
	root, err := semanticRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "test", "acceptance", "tools", "tools.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	var lock struct {
		AcceptanceTools map[string]struct {
			Version string `json:"version"`
		} `json:"acceptanceTools"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatal(err)
	}
	lockedVersion := lock.AcceptanceTools["typescriptLanguageServer"].Version
	if lockedVersion == "" {
		t.Fatal("tools.lock.json has no locked TypeScript language server version")
	}
	versions := map[string]string{
		"typescript-language-server": lockedVersion + " (local package)",
		"typescriptLanguageServer":   "deliberately-wrong-version",
	}
	if problems := semanticLockedToolVersionProblems([]string{"typescript-language-server"}, versions); len(problems) != 0 {
		t.Fatalf("dashed tool version key should match the locked version: %v", problems)
	}
	versions["typescript-language-server"] = "different-version"
	if problems := semanticLockedToolVersionProblems([]string{"typescript-language-server"}, versions); len(problems) != 1 {
		t.Fatalf("a nonmatching local package version must remain blocked by the exact lock check: %v", problems)
	}
}

func semanticUpstreamSpec(name string) (string, []string) {
	switch name {
	case "Go":
		return "gopls", []string{"serve"}
	case "C", "C++":
		return "clangd", []string{"--log=error", "--pch-storage=memory"}
	case "Rust":
		return "rust-analyzer", nil
	case "Python":
		return "pyright-langserver", []string{"--stdio"}
	case "TypeScript", "JavaScript":
		return "typescript-language-server", []string{"--stdio"}
	default:
		return "", nil
	}
}

func semanticAcceptanceToolPath() string {
	root, err := semanticRepoRoot()
	if err != nil {
		return os.Getenv("PATH")
	}
	localBin := filepath.Join(root, "test", "acceptance", "tools", "bin")
	if current := os.Getenv("PATH"); current != "" {
		return localBin + string(os.PathListSeparator) + current
	}
	return localBin
}

func semanticLookPath(name string) (string, error) {
	if name == "pyright-langserver" || name == "typescript-language-server" || name == "tsc" {
		root, err := semanticRepoRoot()
		if err != nil {
			return "", err
		}
		binary := name
		if runtime.GOOS == "windows" {
			binary += ".exe"
		}
		path := filepath.Join(root, "test", "acceptance", "tools", "bin", binary)
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("pinned local wrapper missing at %s: %w", path, err)
		}
		return path, nil
	}
	return exec.LookPath(name)
}

func semanticLocalPackageVersion(name string) string {
	root, err := semanticRepoRoot()
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
	return metadata.Version + " (local package; " + path + ")"
}

func semanticSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func semanticFilesystemType(path string) string {
	if runtime.GOOS == "windows" {
		volume := filepath.VolumeName(path)
		if volume == "" {
			return "unknown: temp directory has no drive volume"
		}
		output, err := exec.Command("fsutil", "fsinfo", "volumeinfo", volume).CombinedOutput()
		if err != nil {
			return semanticWindowsDriveFormat(volume, "fsutil fsinfo volumeinfo failed: "+strings.TrimSpace(string(output)))
		}
		for _, line := range strings.Split(string(output), "\n") {
			if strings.Contains(strings.ToLower(line), "file system name") {
				parts := strings.SplitN(line, ":", 2)
				if len(parts) == 2 {
					return strings.TrimSpace(parts[1]) + " on " + volume + " (fsutil fsinfo volumeinfo)"
				}
			}
		}
		return semanticWindowsDriveFormat(volume, "fsutil fsinfo volumeinfo returned no filesystem name")
	}
	output, err := exec.Command("df", "-T", path).Output()
	if err != nil {
		return "unknown: df -T failed"
	}
	fields := strings.Fields(string(output))
	if len(fields) >= 2 {
		return fields[1] + " (df -T; " + path + ")"
	}
	return "unknown: df -T returned no filesystem"
}

func semanticWindowsDriveFormat(volume, primaryReason string) string {
	driveRoot := strings.TrimRight(volume, `\`) + `\`
	powershellString := strings.ReplaceAll(driveRoot, "'", "''")
	script := "$ErrorActionPreference = 'Stop'; $drive = [System.IO.DriveInfo]::new('" + powershellString + "'); if (-not $drive.IsReady) { throw 'drive is not ready' }; [Console]::Out.WriteLine($drive.DriveFormat)"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("unknown: %s; PowerShell System.IO.DriveInfo.DriveFormat fallback failed: %v: %s", primaryReason, err, strings.TrimSpace(string(output)))
	}
	filesystem := strings.TrimSpace(string(output))
	if filesystem == "" || strings.ContainsAny(filesystem, "\r\n") {
		return fmt.Sprintf("unknown: %s; PowerShell System.IO.DriveInfo.DriveFormat fallback returned invalid filesystem name %q", primaryReason, filesystem)
	}
	hasLetter := false
	for _, r := range filesystem {
		if unicode.IsLetter(r) {
			hasLetter = true
			continue
		}
		if !unicode.IsDigit(r) && !strings.ContainsRune("._-+", r) {
			return fmt.Sprintf("unknown: %s; PowerShell System.IO.DriveInfo.DriveFormat fallback returned invalid filesystem name %q", primaryReason, filesystem)
		}
	}
	if !hasLetter {
		return fmt.Sprintf("unknown: %s; PowerShell System.IO.DriveInfo.DriveFormat fallback returned invalid filesystem name %q", primaryReason, filesystem)
	}
	return fmt.Sprintf("%s on %s (PowerShell [System.IO.DriveInfo].DriveFormat fallback; %s)", filesystem, volume, primaryReason)
}

func semanticOSVersion() string {
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

func semanticCanonicalURI(value string) string {
	parsed, err := uri.Parse(value)
	if err != nil {
		return value
	}
	return parsed.Canonical()
}

func uniqueStrings(values ...string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func languageIDForPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".c":
		return "c"
	case ".cpp", ".hpp":
		return "cpp"
	case ".rs":
		return "rust"
	case ".py":
		return "python"
	case ".ts":
		return "typescript"
	case ".js":
		return "javascript"
	default:
		return "plaintext"
	}
}

func semanticErrString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func semanticUnsupportedError(err error) bool {
	var responseErr *jsonrpc.ResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	switch responseErr.Code {
	case jsonrpc.MethodNotFound, jsonrpc.RequestFailed, jsonrpc.RequestCancelled, jsonrpc.ContentModified:
		return true
	default:
		return false
	}
}

func semanticKnownMalformedSourceRefusal(err error) bool {
	var responseErr *jsonrpc.ResponseError
	if !errors.As(err, &responseErr) || responseErr.Code != jsonrpc.RequestFailed {
		return false
	}
	message := strings.ToLower(strings.TrimSpace(responseErr.Message))
	if !strings.Contains(message, "reference") && !strings.Contains(message, "semantic") {
		return false
	}
	for _, marker := range []string{"malformed source", "syntax error", "syntax errors", "parse error", "parse errors", "invalid syntax"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func semanticBrokenSourceCheck(requestErr, decodeErr error, locations, expected []semanticLocation) report.Check {
	check := report.Check{
		ID:      "S20/broken-source",
		Status:  report.NotVerified,
		Summary: "not_verified: broken-source reference outcome is unknown",
		Observed: map[string]any{
			"requestError": semanticErrString(requestErr),
			"decodeError":  semanticErrString(decodeErr),
			"locations":    locations,
			"completeness": "not_asserted_for_malformed_source",
		},
	}
	if requestErr != nil {
		if semanticKnownMalformedSourceRefusal(requestErr) {
			check.Status = report.Passed
			check.Summary = "candidate explicitly refused references because the source contains syntax errors"
			check.Observed["outcome"] = "supported-malformed-source-refusal"
		}
		return check
	}
	if decodeErr != nil {
		check.Status = report.Failed
		check.Summary = "broken-source references returned an invalid location payload: " + decodeErr.Error()
		return check
	}
	if len(locations) == 0 {
		check.Summary = "not_verified: broken-source reference result was empty, so safe semantic locations were not demonstrated"
		return check
	}
	if len(locations) > len(expected) {
		check.Status = report.Failed
		check.Summary = fmt.Sprintf("broken-source references include extra locations: got=%+v expected=%+v", locations, expected)
		return check
	}
	seen := make(map[semanticLocation]bool, len(locations))
	for _, location := range locations {
		if seen[location] || !containsSemanticLocation(expected, location) {
			check.Status = report.Failed
			check.Summary = fmt.Sprintf("broken-source references include duplicate or unsafe locations: got=%+v expected=%+v", locations, expected)
			return check
		}
		seen[location] = true
	}
	var missing []semanticLocation
	for _, location := range expected {
		if !containsSemanticLocation(locations, location) {
			missing = append(missing, location)
		}
	}
	check.Status = report.Passed
	check.Observed["missingExpectedLocations"] = missing
	check.Observed["reportedLocationCount"] = len(locations)
	check.Observed["expectedLocationCount"] = len(expected)
	if len(missing) == 0 {
		check.Summary = "candidate returned the exact safe declaration and use locations despite malformed trailing syntax"
		check.Observed["outcome"] = "exact-safe-locations"
	} else {
		check.Summary = "candidate returned only safe locations for malformed source; reference completeness is not asserted"
		check.Observed["outcome"] = "safe-locations-completeness-not-asserted"
	}
	return check
}

func TestS20_BrokenSourceOutcomeClassification(t *testing.T) {
	file := "file:///workspace/broken.go"
	expected := []semanticLocation{
		{URI: file, StartLine: 1, StartChar: 4, EndLine: 1, EndChar: 10},
		{URI: file, StartLine: 3, StartChar: 8, EndLine: 3, EndChar: 14},
	}
	knownRefusal := &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "references unavailable because source has syntax errors"}
	methodMissing := &jsonrpc.ResponseError{Code: jsonrpc.MethodNotFound, Message: "syntax error"}
	unknownFailure := &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "index disabled: no workspace"}
	tests := []struct {
		name      string
		request   error
		decode    error
		locations []semanticLocation
		want      report.Status
	}{
		{name: "exact safe locations", locations: expected, want: report.Passed},
		{name: "known malformed-source refusal", request: knownRefusal, want: report.Passed},
		{name: "safe but incomplete subset", locations: expected[:1], want: report.Passed},
		{name: "empty result", locations: []semanticLocation{}, want: report.NotVerified},
		{name: "missing capability", request: methodMissing, want: report.NotVerified},
		{name: "unrecognized refusal", request: unknownFailure, want: report.NotVerified},
		{name: "unknown transport error", request: errors.New("connection closed"), want: report.NotVerified},
		{name: "invalid location payload", decode: errors.New("bad range"), want: report.Failed},
		{name: "wrong-file location", locations: []semanticLocation{{URI: "file:///workspace/other.go", StartLine: 1, StartChar: 4, EndLine: 1, EndChar: 10}}, want: report.Failed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			check := semanticBrokenSourceCheck(test.request, test.decode, test.locations, expected)
			if check.Status != test.want {
				t.Fatalf("status = %q, want %q; summary: %s", check.Status, test.want, check.Summary)
			}
		})
	}
}

func TestS20_DiagnosticSemanticComparison(t *testing.T) {
	severity := 1
	makeDiagnostic := func(source, message, code string, startLine uint32) semanticDiagnostic {
		diagnostic := semanticDiagnostic{Severity: &severity, Source: source, Message: message, Code: json.RawMessage(code)}
		diagnostic.Range.Start.Line = startLine
		diagnostic.Range.Start.Character = 7
		diagnostic.Range.End.Line = startLine
		diagnostic.Range.End.Character = 8
		return diagnostic
	}
	candidate := []semanticDiagnostic{makeDiagnostic("omnilsp-go", "unexpected token", `"syntax"`, 5)}
	pinned := []semanticDiagnostic{makeDiagnostic("compiler", "expected operand", `"syntax"`, 5)}
	match, evidence := semanticCompareDiagnostics(candidate, pinned)
	if !match {
		t.Fatalf("message/source-only differences changed semantic comparison: %+v", evidence)
	}
	if len(evidence["messageDifferences"].([]map[string]any)) == 0 || len(evidence["sourceLabelDifferences"].([]map[string]any)) == 0 {
		t.Fatalf("expected message and source-label differences to be recorded separately: %+v", evidence)
	}

	changedRange := []semanticDiagnostic{makeDiagnostic("compiler", "expected operand", `"syntax"`, 4)}
	match, _ = semanticCompareDiagnostics(candidate, changedRange)
	if match {
		t.Fatal("different diagnostic ranges were treated as semantically equal")
	}
	changedCode := []semanticDiagnostic{makeDiagnostic("compiler", "expected operand", `"other-code"`, 5)}
	match, _ = semanticCompareDiagnostics(candidate, changedCode)
	if match {
		t.Fatal("different comparable diagnostic codes were treated as semantically equal")
	}
	codeUnavailable := []semanticDiagnostic{makeDiagnostic("compiler", "expected operand", "", 5)}
	match, evidence = semanticCompareDiagnostics(candidate, codeUnavailable)
	if !match || len(evidence["codeNotComparable"].([]map[string]any)) == 0 {
		t.Fatalf("optional diagnostic code asymmetry should be recorded without a false semantic mismatch: %+v", evidence)
	}
}

func TestS20_FixturePositionFindsCallsNotIdentifierSuffixes(t *testing.T) {
	files, cases := semanticFixtureSet(t.TempDir())
	byName := make(map[string]semanticLanguageCase, len(cases))
	for _, testCase := range cases {
		byName[testCase.name] = testCase
	}
	for _, language := range []string{"C++", "Python"} {
		t.Run(language, func(t *testing.T) {
			testCase, ok := byName[language]
			if !ok {
				t.Fatalf("%s fixture case missing", language)
			}
			text := files[testCase.queryFile]
			line, character := semanticPositionOf(text, testCase.queryToken, 0)
			targetCall := strings.Index(text, "return target()")
			if targetCall < 0 {
				t.Fatal("fixture lacks an explicit return target() call")
			}
			callOffset := targetCall + len("return ")
			prefix := text[:callOffset]
			wantLine := uint32(strings.Count(prefix, "\n"))
			lastNewline := strings.LastIndex(prefix, "\n")
			wantCharacter := uint32(len(prefix[lastNewline+1:]))
			if line != wantLine || character != wantCharacter {
				t.Fatalf("semanticPositionOf selected %s identifier suffix instead of call: got [%d,%d], want [%d,%d]", language, line, character, wantLine, wantCharacter)
			}
			if testCase.call.StartLine != wantLine || testCase.call.StartChar != wantCharacter {
				t.Fatalf("%s expected call range does not use the actual invocation: got %+v, want start [%d,%d]", language, testCase.call, wantLine, wantCharacter)
			}
		})
	}

	for _, testCase := range []struct {
		language, definitionFile string
		line, start, end         uint32
	}{
		{language: "C", definitionFile: "c-api/impl.c", line: 1, start: 4, end: 10},
		{language: "C++", definitionFile: "cpp-api/impl.cpp", line: 2, start: 4, end: 10},
	} {
		got, ok := byName[testCase.language]
		if !ok {
			t.Fatalf("%s fixture case missing", testCase.language)
		}
		if got.definitionFile != testCase.definitionFile || got.definition.StartLine != testCase.line ||
			got.definition.StartChar != testCase.start || got.definition.EndChar != testCase.end {
			t.Errorf("%s fixture expected target = %s:%d:%d-%d; got file=%s location=%+v", testCase.language,
				testCase.definitionFile, testCase.line, testCase.start, testCase.end, got.definitionFile, got.definition)
		}
	}
}

func TestS20_KPIPositiveNegativeAndLocationRegression(t *testing.T) {
	target := semanticLocation{URI: "file:///workspace/target.go", StartLine: 1, StartChar: 5, EndLine: 1, EndChar: 11}
	call := semanticLocation{URI: "file:///workspace/use.go", StartLine: 2, StartChar: 20, EndLine: 2, EndChar: 26}
	for _, feature := range []struct {
		name      string
		candidate []semanticLocation
		pinned    []semanticLocation
	}{
		{name: "definition", candidate: []semanticLocation{target}, pinned: []semanticLocation{target}},
		{name: "references", candidate: []semanticLocation{target, call}, pinned: []semanticLocation{target, call}},
	} {
		t.Run(feature.name+" positive and negative", func(t *testing.T) {
			record := semanticLocationKPI(feature.name, "Go", "gopls", map[string]string{"gopls": "pinned-test-version"}, feature.candidate, feature.pinned, nil, true)
			assertKPIValue(t, "precision", record.Metrics.Precision, 1)
			assertKPIValue(t, "recall", record.Metrics.Recall, 1)
			assertKPIValue(t, "false positive rate", record.Metrics.FalsePositiveRate, 0)
			assertKPIValue(t, "false negative rate", record.Metrics.FalseNegativeRate, 0)
			assertKPIValue(t, "refusal rate", record.Metrics.RefusalRate, 0)
			assertKPIValue(t, "wrong file rate", record.Metrics.WrongFileRate, 0)
			assertKPIValue(t, "position mapping failure rate", record.Metrics.PositionMappingFailureRate, 0)
			if record.Metrics.StaleResultRejectionRate.Status != telemetry.MetricNotVerified || record.Metrics.EditValidationFailureRate.Status != telemetry.MetricNotVerified {
				t.Fatalf("stale/edit rates must remain not_verified without freshness/edit observations: %+v", record.Metrics)
			}
		})
	}

	falsePositive := semanticLocation{URI: "file:///workspace/target.go", StartLine: 5, StartChar: 3, EndLine: 5, EndChar: 15}
	negativeRecord := semanticLocationKPI("references", "Go", "gopls", nil, []semanticLocation{target, call}, []semanticLocation{target, call}, []semanticLocation{falsePositive}, true)
	assertKPIValue(t, "negative false-positive rate", negativeRecord.Metrics.FalsePositiveRate, 1)

	wrongRange := target
	wrongRange.StartChar++
	wrongRange.EndChar++
	positionRecord := semanticLocationKPI("definition", "Go", "gopls", nil, []semanticLocation{wrongRange}, []semanticLocation{target}, nil, true)
	assertKPIValue(t, "same-file position mapping failure", positionRecord.Metrics.PositionMappingFailureRate, 1)
	assertKPIValue(t, "same-file wrong-file rate", positionRecord.Metrics.WrongFileRate, 0)

	wrongFile := target
	wrongFile.URI = "file:///workspace/other.go"
	wrongFileRecord := semanticLocationKPI("definition", "Go", "gopls", nil, []semanticLocation{wrongFile}, []semanticLocation{target}, nil, true)
	assertKPIValue(t, "wrong-file rate", wrongFileRecord.Metrics.WrongFileRate, 1)
	if wrongFileRecord.Metrics.PositionMappingFailureRate.Status != telemetry.MetricNotVerified || wrongFileRecord.Metrics.PositionMappingFailureRate.Value != nil {
		t.Fatalf("position rate must not claim a zero when there is no same-file mapping sample: %+v", wrongFileRecord.Metrics.PositionMappingFailureRate)
	}
}

func TestS20_StaleMetricAttribution(t *testing.T) {
	base := semanticQueryTrace{Computations: 40, StalePublishRejected: 7}
	metric, reason := semanticStaleMetric(base, semanticQueryTrace{Computations: 41, StalePublishRejected: 8})
	if reason != "" || metric.Status != telemetry.MetricObserved || metric.Numerator == nil || *metric.Numerator != 1 || metric.Denominator == nil || *metric.Denominator != 1 || metric.Value == nil || *metric.Value != 1 {
		t.Fatalf("single attributable stale candidate = %+v, reason %q; want observed 1/1", metric, reason)
	}
	zeroRejected, reason := semanticStaleMetric(base, semanticQueryTrace{Computations: 41, StalePublishRejected: 7})
	if reason != "" || zeroRejected.Status != telemetry.MetricObserved || zeroRejected.Numerator == nil || *zeroRejected.Numerator != 0 || zeroRejected.Denominator == nil || *zeroRejected.Denominator != 1 || zeroRejected.Value == nil || *zeroRejected.Value != 0 {
		t.Fatalf("single attributable candidate with no rejection = %+v, reason %q; want observed 0/1", zeroRejected, reason)
	}
	if safe, safetyReason := semanticStaleRejectionSafety(zeroRejected); safe || safetyReason == "" {
		t.Fatalf("measured zero rejection must independently fail stale safety: safe=%t reason=%q", safe, safetyReason)
	}

	for name, after := range map[string]semanticQueryTrace{
		"multiple computations": {Computations: 42, StalePublishRejected: 8},
		"regressed counter":     {Computations: 39, StalePublishRejected: 7},
	} {
		t.Run(name, func(t *testing.T) {
			metric, reason := semanticStaleMetric(base, after)
			if reason == "" || metric.Status != telemetry.MetricNotVerified || metric.Value != nil {
				t.Fatalf("unattributable stale sample = %+v, reason %q; want not_verified without value", metric, reason)
			}
		})
	}
	if safe, reason := semanticStaleRejectionSafety(metric); !safe || reason == "" {
		t.Fatalf("1/1 stale rejection should meet the independent safety ratio: safe=%t reason=%q", safe, reason)
	}
}

func TestS20_RenameStaleMetricRequiresOneObservableBackendCandidate(t *testing.T) {
	counter := func(value uint64) *uint64 { return &value }
	base := semanticQueryTrace{RenameRequestsStarted: counter(12), RenameStaleRejected: counter(4)}
	for _, testCase := range []struct {
		name         string
		after        semanticQueryTrace
		wantRejected int64
		wantStatus   telemetry.MetricStatus
	}{
		{name: "rejected", after: semanticQueryTrace{RenameRequestsStarted: counter(13), RenameStaleRejected: counter(5)}, wantRejected: 1, wantStatus: telemetry.MetricObserved},
		{name: "proven zero rejection", after: semanticQueryTrace{RenameRequestsStarted: counter(13), RenameStaleRejected: counter(4)}, wantRejected: 0, wantStatus: telemetry.MetricObserved},
		{name: "multiple requests", after: semanticQueryTrace{RenameRequestsStarted: counter(14), RenameStaleRejected: counter(5)}, wantStatus: telemetry.MetricNotVerified},
		{name: "counters unavailable", after: semanticQueryTrace{RenameRequestsStarted: counter(13)}, wantStatus: telemetry.MetricNotVerified},
		{name: "counter regression", after: semanticQueryTrace{RenameRequestsStarted: counter(11), RenameStaleRejected: counter(4)}, wantStatus: telemetry.MetricNotVerified},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			metric, reason := semanticRenameStaleMetric(base, testCase.after)
			if metric.Status != testCase.wantStatus {
				t.Fatalf("rename stale metric status=%q reason=%q; want %q", metric.Status, reason, testCase.wantStatus)
			}
			if testCase.wantStatus != telemetry.MetricObserved {
				if reason == "" || metric.Value != nil || metric.Numerator != nil || metric.Denominator != nil {
					t.Fatalf("unattributable rename freshness counters produced a sample: metric=%+v reason=%q", metric, reason)
				}
				return
			}
			if reason != "" || metric.Numerator == nil || *metric.Numerator != testCase.wantRejected || metric.Denominator == nil || *metric.Denominator != 1 {
				t.Fatalf("rename stale metric=%+v reason=%q; want %d/1", metric, reason, testCase.wantRejected)
			}
			if testCase.wantRejected == 0 {
				if safe, safetyReason := semanticStaleRejectionSafety(metric); safe || safetyReason == "" {
					t.Fatalf("proven zero rename rejection must independently fail stale safety: safe=%t reason=%q", safe, safetyReason)
				}
			}
		})
	}
}

func TestS20_RenameKPIRequiresPinnedPositiveAndVerifiedNegative(t *testing.T) {
	first := semanticEditObservation{URI: "file:///workspace/use.go", StartLine: 1, StartChar: 4, EndLine: 1, EndChar: 19, NewText: "renamedLocalScopeValue"}
	second := semanticEditObservation{URI: "file:///workspace/use.go", StartLine: 2, StartChar: 11, EndLine: 2, EndChar: 26, NewText: "renamedLocalScopeValue"}
	inputs := semanticRenameKPIInputs([]semanticEditObservation{first, second}, []semanticEditObservation{first, second}, true, nil, true, true)
	record := telemetry.NewRecord(telemetry.Dimension{Feature: "rename", Language: "Go", Backend: "golang"}, inputs)
	assertKPIValue(t, "rename precision", record.Metrics.Precision, 1)
	assertKPIValue(t, "rename recall", record.Metrics.Recall, 1)
	assertKPIValue(t, "rename false-positive rate", record.Metrics.FalsePositiveRate, 0)
	assertKPIValue(t, "rename false-negative rate", record.Metrics.FalseNegativeRate, 0)
	assertKPIValue(t, "rename wrong-file rate", record.Metrics.WrongFileRate, 0)
	assertKPIValue(t, "rename position-mapping failure rate", record.Metrics.PositionMappingFailureRate, 0)
	if record.Metrics.StaleResultRejectionRate.Status != telemetry.MetricNotVerified {
		t.Fatalf("rename stale rate must remain unverified without a rename-stale probe: %+v", record.Metrics.StaleResultRejectionRate)
	}

	wrongFile := first
	wrongFile.URI = "file:///workspace/other.go"
	wrongRange := second
	wrongRange.StartChar++
	wrongRange.EndChar++
	failedInputs := semanticRenameKPIInputs([]semanticEditObservation{wrongFile, wrongRange}, []semanticEditObservation{first, second}, true, nil, true, true)
	failed := telemetry.NewRecord(telemetry.Dimension{Feature: "rename", Language: "Go", Backend: "golang"}, failedInputs)
	assertKPIValue(t, "mismatched rename precision", failed.Metrics.Precision, 0)
	assertKPIValue(t, "mismatched rename recall", failed.Metrics.Recall, 0)
	assertKPIValue(t, "mismatched rename false-positive rate", failed.Metrics.FalsePositiveRate, 0.5)
	assertKPIValue(t, "mismatched rename false-negative rate", failed.Metrics.FalseNegativeRate, 1)
	assertKPIValue(t, "mismatched rename wrong-file rate", failed.Metrics.WrongFileRate, 0.5)
	assertKPIValue(t, "mismatched rename position-mapping failure rate", failed.Metrics.PositionMappingFailureRate, 1)

	withoutPinnedNegative := semanticRenameKPIInputs([]semanticEditObservation{first, second}, []semanticEditObservation{first, second}, true, nil, true, false)
	withoutNegativeRecord := telemetry.NewRecord(telemetry.Dimension{Feature: "rename", Language: "Go", Backend: "golang"}, withoutPinnedNegative)
	if withoutNegativeRecord.Metrics.FalsePositiveRate.Status != telemetry.MetricNotVerified || withoutNegativeRecord.Metrics.FalsePositiveRate.Value != nil {
		t.Fatalf("unverified negative rename oracle must not synthesize TN/FPR: %+v", withoutNegativeRecord.Metrics.FalsePositiveRate)
	}
}

func TestS20_RenameNegativeProbeOnlyAcceptsSemanticRefusal(t *testing.T) {
	refusal := &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "no object found at position"}
	if edits, observed, reason := semanticRenameRequestOutcome(nil, refusal); !observed || len(edits) != 0 || reason == "" {
		t.Fatalf("domain refusal should verify an empty negative result: edits=%+v observed=%t reason=%q", edits, observed, reason)
	}
	if edits, observed, reason := semanticRenameRequestOutcome(nil, errors.New("jsonrpc error 0: no identifier found")); !observed || len(edits) != 0 || reason == "" {
		t.Fatalf("pinned no-identifier outcome should verify an empty negative result: edits=%+v observed=%t reason=%q", edits, observed, reason)
	}
	transient := &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "backend temporarily unavailable"}
	if _, observed, _ := semanticRenameRequestOutcome(nil, transient); observed {
		t.Fatal("transient RequestFailed was counted as a verified negative rename sample")
	}
	otherNoIdentifierError := errors.New("jsonrpc error -32603: no identifier found")
	if _, observed, _ := semanticRenameRequestOutcome(nil, otherNoIdentifierError); observed {
		t.Fatal("an unrecognized no-identifier error was counted as a verified negative rename sample")
	}
}

func TestS20_WorkspaceEditNormalization(t *testing.T) {
	legacy := json.RawMessage(`{"changes":{"file:///workspace/use.go":[{"range":{"start":{"line":2,"character":4},"end":{"line":2,"character":19}},"newText":"renamedLocalScopeValue"}]}}`)
	documentChanges := json.RawMessage(`{"documentChanges":[{"textDocument":{"uri":"file:///workspace/use.go","version":4},"edits":[{"range":{"start":{"line":2,"character":4},"end":{"line":2,"character":19}},"newText":"renamedLocalScopeValue"}]}]}`)
	want := []semanticEditObservation{{URI: "file:///workspace/use.go", StartLine: 2, StartChar: 4, EndLine: 2, EndChar: 19, NewText: "renamedLocalScopeValue"}}
	for name, raw := range map[string]json.RawMessage{"changes": legacy, "documentChanges": documentChanges} {
		t.Run(name, func(t *testing.T) {
			got, err := normalizeSemanticWorkspaceEdit(raw)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("normalized WorkspaceEdit = %+v, error %v; want %+v", got, err, want)
			}
		})
	}
	for name, raw := range map[string]json.RawMessage{
		"empty":              json.RawMessage(`{"changes":{}}`),
		"both forms":         json.RawMessage(`{"changes":{"file:///workspace/use.go":[]},"documentChanges":[{}]}`),
		"resource operation": json.RawMessage(`{"documentChanges":[{"kind":"create","uri":"file:///workspace/new.go"}]}`),
		"reversed range":     json.RawMessage(`{"changes":{"file:///workspace/use.go":[{"range":{"start":{"line":3,"character":2},"end":{"line":2,"character":5}},"newText":"x"}]}}`),
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := normalizeSemanticWorkspaceEdit(raw); err == nil {
				t.Fatalf("invalid WorkspaceEdit unexpectedly normalized: %+v", got)
			}
		})
	}
}

func TestS20_EditValidationSampleRequiresReturnedWorkspaceEdit(t *testing.T) {
	dimension := telemetry.Dimension{Feature: "rename", Language: "Go", Backend: "golang"}
	refusal := &jsonrpc.ResponseError{Code: jsonrpc.RequestFailed, Message: "rename refused because completeness is unknown"}
	if _, _, sampled, err := semanticValidatedEditKPI(nil, refusal, dimension); sampled || err == nil {
		t.Fatalf("rename refusal was counted as edit validation: sampled=%t err=%v", sampled, err)
	}
	if _, _, sampled, err := semanticValidatedEditKPI(json.RawMessage(`{"changes":{}}`), nil, dimension); sampled || err == nil {
		t.Fatalf("empty WorkspaceEdit was counted as edit validation: sampled=%t err=%v", sampled, err)
	}
	valid := json.RawMessage(`{"changes":{"file:///workspace/use.go":[{"range":{"start":{"line":1,"character":2},"end":{"line":1,"character":17}},"newText":"renamedLocalScopeValue"}]}}`)
	record, edits, sampled, err := semanticValidatedEditKPI(valid, nil, dimension)
	if err != nil || !sampled || len(edits) != 1 {
		t.Fatalf("valid returned WorkspaceEdit = edits %+v, sampled %t, err %v", edits, sampled, err)
	}
	metric := record.Metrics.EditValidationFailureRate
	if metric.Status != telemetry.MetricObserved || metric.Numerator == nil || *metric.Numerator != 0 || metric.Denominator == nil || *metric.Denominator != 1 || metric.Value == nil || *metric.Value != 0 {
		t.Fatalf("returned validated WorkspaceEdit KPI = %+v; want observed 0/1", metric)
	}
}

func TestS20_RenameRefusalRateRequiresBothObservedOutcomes(t *testing.T) {
	dimension := telemetry.Dimension{Feature: "rename", Language: "Go", Backend: "golang", Oracle: "candidate.ValidateEditSet"}
	editRecord := telemetry.NewRecord(dimension, telemetry.Inputs{
		EditValidationFailures: telemetry.Sampled(0), EditValidationAttempts: telemetry.Sampled(1),
	})
	newLocalRenameCheck := func() report.Check {
		return report.Check{Observed: map[string]any{
			"kpiObservations": []telemetry.Record{editRecord},
		}}
	}

	withBothOutcomes := semanticObservedRenameRefusalKPI(newLocalRenameCheck(), true)
	reportCheck := semanticAccuracyKPIReport("acceptance-run", []report.Check{withBothOutcomes})
	if reportCheck.Status != report.NotVerified {
		t.Fatalf("partial KPI report status = %q, want not_verified", reportCheck.Status)
	}
	records, ok := reportCheck.Observed["records"].([]telemetry.Record)
	if !ok {
		t.Fatalf("KPI records = %T, want []telemetry.Record", reportCheck.Observed["records"])
	}
	found := false
	for _, record := range records {
		if record.Dimension.Feature != "rename" || record.Dimension.Language != "Go" || record.Dimension.Backend != "golang" {
			continue
		}
		metric := record.Metrics.RefusalRate
		if metric.Status != telemetry.MetricObserved || metric.Numerator == nil || *metric.Numerator != 1 || metric.Denominator == nil || *metric.Denominator != 2 || metric.Value == nil || *metric.Value != 0.5 {
			t.Fatalf("observed exported refusal + validated local success = %+v; want 1/2", metric)
		}
		if record.Metrics.EditValidationFailureRate.Status != telemetry.MetricObserved {
			t.Fatalf("existing edit validation observation was lost while adding refusal evidence: %+v", record.Metrics.EditValidationFailureRate)
		}
		found = true
		break
	}
	if !found {
		t.Fatal("Go rename KPI dimension missing")
	}

	withoutExportedRefusal := semanticObservedRenameRefusalKPI(newLocalRenameCheck(), false)
	withoutRefusalReport := semanticAccuracyKPIReport("acceptance-run", []report.Check{withoutExportedRefusal})
	withoutRefusalRecords := withoutRefusalReport.Observed["records"].([]telemetry.Record)
	for _, record := range withoutRefusalRecords {
		if record.Dimension.Feature == "rename" && record.Dimension.Language == "Go" && record.Dimension.Backend == "golang" {
			if record.Metrics.RefusalRate.Status != telemetry.MetricNotVerified || record.Metrics.RefusalRate.Value != nil {
				t.Fatalf("unobserved exported refusal fabricated a rate: %+v", record.Metrics.RefusalRate)
			}
			return
		}
	}
	t.Fatal("Go rename KPI dimension missing in unobserved case")
}

func TestS20_ObservedStaleMetricCompletesKPIReport(t *testing.T) {
	checks := []report.Check{}
	languages := []string{"Go", "C", "C++", "Rust", "Python", "TypeScript", "JavaScript"}
	inputs := telemetry.Inputs{
		TruePositive: telemetry.Sampled(1), FalsePositive: telemetry.Sampled(0), FalseNegative: telemetry.Sampled(0), TrueNegative: telemetry.Sampled(1),
		Refusals: telemetry.Sampled(0), Requests: telemetry.Sampled(1),
		StaleRejected: telemetry.Sampled(0), StaleCandidates: telemetry.Sampled(1),
		WrongFileLocations: telemetry.Sampled(0), LocationResults: telemetry.Sampled(1),
		PositionMappingFailures: telemetry.Sampled(0), PositionMappingSamples: telemetry.Sampled(1),
	}
	for _, language := range languages {
		for _, feature := range []string{"definition", "references"} {
			dimension := telemetry.Dimension{Feature: feature, Language: language, Backend: semanticCandidateBackend(language), Oracle: "pinned test oracle"}
			recordInputs := inputs
			if language == "Go" && feature == "definition" {
				// The report must use the direct stdio queryTrace observation,
				// not this deliberately unsampled placeholder.
				recordInputs.StaleRejected = telemetry.Count{}
				recordInputs.StaleCandidates = telemetry.Count{}
			}
			checks = append(checks, report.Check{Observed: map[string]any{"kpiObservations": []telemetry.Record{telemetry.NewRecord(dimension, recordInputs)}}})
		}
	}
	for _, feature := range []string{"position-mapping", "rename"} {
		dimension := telemetry.Dimension{Feature: feature, Language: "Go", Backend: semanticCandidateBackend("Go"), Oracle: "pinned test oracle"}
		recordInputs := inputs
		if feature == "rename" {
			recordInputs.EditValidationFailures = telemetry.Sampled(0)
			recordInputs.EditValidationAttempts = telemetry.Sampled(1)
		}
		checks = append(checks, report.Check{Observed: map[string]any{"kpiObservations": []telemetry.Record{telemetry.NewRecord(dimension, recordInputs)}}})
	}
	dimension := telemetry.Dimension{Feature: "definition", Language: "Go", Backend: semanticCandidateBackend("Go")}
	metric, reason := semanticStaleMetric(semanticQueryTrace{Computations: 10, StalePublishRejected: 2}, semanticQueryTrace{Computations: 11, StalePublishRejected: 3})
	if reason != "" {
		t.Fatal(reason)
	}
	checks = append(checks, report.Check{Observed: map[string]any{"kpiMetricObservations": semanticKPIUpdate(dimension, "staleResultRejectionRate", metric)}})

	check := semanticAccuracyKPIReport("acceptance-run", checks)
	if check.Status != report.Passed {
		t.Fatalf("observed complete KPI evidence status = %q, summary %q, limitations %v", check.Status, check.Summary, check.Observed["limitations"])
	}
	records, ok := check.Observed["records"].([]telemetry.Record)
	if !ok || len(records) != 16 {
		t.Fatalf("KPI rows = %T/%d, want 16", check.Observed["records"], len(records))
	}
	for _, record := range records {
		if record.Dimension.Feature == "definition" && record.Dimension.Language == "Go" && record.Dimension.Backend == "golang" {
			got := record.Metrics.StaleResultRejectionRate
			if got.Status != telemetry.MetricObserved || got.Numerator == nil || *got.Numerator != 1 || got.Denominator == nil || *got.Denominator != 1 || got.Value == nil || *got.Value != 1 {
				t.Fatalf("Go definition stale metric was not merged from stdio evidence: %+v", got)
			}
			return
		}
	}
	t.Fatal("Go definition KPI row missing from report")
}

func TestS20_KPIReportHasSevenLanguagesAndExplicitGaps(t *testing.T) {
	check := semanticAccuracyKPIReport("acceptance-run", nil)
	if check.ID != "acceptance-run/S20/accuracy-kpi" || check.Status != report.NotVerified {
		t.Fatalf("KPI check id/status = %q/%q, want run-prefixed not_verified", check.ID, check.Status)
	}
	if check.Observed["schema"] != telemetry.KPIReportSchema {
		t.Fatalf("unexpected KPI schema: %+v", check.Observed["schema"])
	}
	records, ok := check.Observed["records"].([]telemetry.Record)
	if !ok || len(records) != 16 {
		t.Fatalf("KPI report records = %T/%d, want 16 structured rows", check.Observed["records"], len(records))
	}
	languages := map[string]map[string]bool{}
	dimensions := map[string]bool{}
	for _, record := range records {
		dimension := record.Dimension
		if dimension.Feature == "" || dimension.Language == "" || dimension.Backend == "" || dimension.Oracle == "" {
			t.Errorf("KPI row has an empty dimension: %+v", dimension)
		}
		key := semanticKPIDimensionKey(dimension)
		if dimensions[key] {
			t.Errorf("KPI report repeats dimension %q", key)
		}
		dimensions[key] = true
		if languages[record.Dimension.Language] == nil {
			languages[record.Dimension.Language] = map[string]bool{}
		}
		languages[record.Dimension.Language][record.Dimension.Feature] = true
		for name, metric := range semanticKPIMetricMap(record.Metrics) {
			if name == "editValidationFailureRate" && semanticEditValidationNotApplicable(dimension) {
				if metric.Formula == "" || metric.Status != telemetry.MetricNotApplicable || metric.Value != nil || metric.Numerator != nil || metric.Denominator != nil || metric.Reason == "" {
					t.Errorf("read-only %s/%s edit validation must be explicit not_applicable without samples: %+v", dimension.Feature, dimension.Language, metric)
				}
				continue
			}
			if metric.Formula == "" || metric.Status != telemetry.MetricNotVerified || metric.Value != nil {
				t.Errorf("unobserved %s metric must retain formula and not_verified status without a value: %+v", name, metric)
			}
		}
	}
	if len(languages) != 7 {
		t.Fatalf("KPI report languages = %v, want seven", languages)
	}
	for language, features := range languages {
		if !features["definition"] || !features["references"] {
			t.Errorf("language %s missing definition/references feature rows: %v", language, features)
		}
	}
	if !languages["Go"]["position-mapping"] || !languages["Go"]["rename"] {
		t.Fatalf("Go-specific KPI dimensions missing: %v", languages["Go"])
	}
	if got := len(check.Observed["notApplicable"].([]map[string]string)); got != 15 {
		t.Fatalf("not-applicable KPI cells = %d, want 15 read-only definition/reference/Go-position cells", got)
	}
	for _, record := range records {
		if record.Dimension.Feature == "rename" && record.Dimension.Language == "Go" && record.Metrics.EditValidationFailureRate.Status != telemetry.MetricNotVerified {
			t.Fatalf("edit validation must remain required for Go rename: %+v", record.Metrics.EditValidationFailureRate)
		}
	}
	limitations, ok := check.Observed["limitations"].([]map[string]string)
	if !ok || len(limitations) == 0 {
		t.Fatalf("uncollected KPI gaps are not structured: %T/%v", check.Observed["limitations"], check.Observed["limitations"])
	}
	for _, limitation := range limitations {
		for _, key := range []string{"feature", "language", "backend", "metric", "reason"} {
			if limitation[key] == "" {
				t.Errorf("limitation missing %q: %+v", key, limitation)
			}
		}
	}
}

func TestS20_EditValidationNotApplicableIsRestrictedToReadOnlyDimensions(t *testing.T) {
	for _, test := range []struct {
		dimension telemetry.Dimension
		want      bool
	}{
		{dimension: telemetry.Dimension{Feature: "definition", Language: "Go"}, want: true},
		{dimension: telemetry.Dimension{Feature: "references", Language: "TypeScript"}, want: true},
		{dimension: telemetry.Dimension{Feature: "position-mapping", Language: "Go"}, want: true},
		{dimension: telemetry.Dimension{Feature: "position-mapping", Language: "Python"}, want: false},
		{dimension: telemetry.Dimension{Feature: "rename", Language: "Go"}, want: false},
		{dimension: telemetry.Dimension{Feature: "definition", Language: "unknown"}, want: false},
	} {
		if got := semanticEditValidationNotApplicable(test.dimension); got != test.want {
			t.Errorf("not-applicable predicate for %+v = %t, want %t", test.dimension, got, test.want)
		}
	}

	dimension := telemetry.Dimension{Feature: "definition", Language: "Go", Backend: "golang"}
	forged := telemetry.NewRecord(dimension, telemetry.Inputs{
		EditValidationFailures: telemetry.Sampled(0), EditValidationAttempts: telemetry.Sampled(1),
	})
	check := semanticAccuracyKPIReport("acceptance-run", []report.Check{{Observed: map[string]any{"kpiObservations": []telemetry.Record{forged}}}})
	if check.Status != report.Failed || len(check.Observed["metricConflicts"].([]string)) == 0 {
		t.Fatalf("read-only dimension with fabricated edit samples must fail the KPI report: status=%s conflicts=%v", check.Status, check.Observed["metricConflicts"])
	}
}

func assertKPIValue(t *testing.T, name string, metric telemetry.Metric, want float64) {
	t.Helper()
	if metric.Status != telemetry.MetricObserved || metric.Value == nil || *metric.Value != want {
		t.Fatalf("%s = status %q value %v; want observed %v", name, metric.Status, metric.Value, want)
	}
}

type semanticScriptedReply struct {
	definition      json.RawMessage
	references      json.RawMessage
	definitionError error
	referencesError error
}

type semanticScriptedProgressClient struct {
	replies   []semanticScriptedReply
	completed int
	progress  []upstream.Notification
	tokens    []string
}

func (c *semanticScriptedProgressClient) RequestContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request, ok := params.(map[string]any); ok {
		if token, ok := request["workDoneToken"].(string); ok {
			c.tokens = append(c.tokens, token)
		}
	}
	index := c.completed
	if index >= len(c.replies) {
		index = len(c.replies) - 1
	}
	reply := c.replies[index]
	switch method {
	case "textDocument/definition":
		return reply.definition, reply.definitionError
	case "textDocument/references":
		c.completed++
		return reply.references, reply.referencesError
	default:
		return nil, fmt.Errorf("unexpected semantic method %q", method)
	}
}

func (c *semanticScriptedProgressClient) NotificationsSinceAndCursor(cursor uint64) ([]upstream.Notification, uint64, bool) {
	limit := uint64(c.completed)
	result := []upstream.Notification{}
	for _, notification := range c.progress {
		if notification.Sequence > cursor && notification.Sequence <= limit {
			result = append(result, notification)
		}
	}
	return result, limit, false
}

func (c *semanticScriptedProgressClient) NotificationCursor() uint64 {
	if uint64(c.completed) < uint64(len(c.progress)) {
		return uint64(c.completed)
	}
	return uint64(len(c.progress))
}

type semanticScriptedCandidateClient struct {
	replies   []semanticScriptedReply
	completed int
	events    []*jsonrpc.Message
	tokens    []string
}

func (c *semanticScriptedCandidateClient) RequestContext(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request, ok := params.(map[string]any); ok {
		if token, ok := request["workDoneToken"].(string); ok {
			c.tokens = append(c.tokens, token)
		}
	}
	index := c.completed
	if index >= len(c.replies) {
		index = len(c.replies) - 1
	}
	reply := c.replies[index]
	switch method {
	case "textDocument/definition":
		return reply.definition, reply.definitionError
	case "textDocument/references":
		c.completed++
		if c.completed == 1 {
			c.events = append(c.events, &jsonrpc.Message{Method: "$/progress", Params: json.RawMessage(`{"token":"candidate-index","value":{"kind":"begin"}}`)})
		} else if c.completed == 2 {
			c.events = append(c.events, &jsonrpc.Message{Method: "$/progress", Params: json.RawMessage(`{"token":"candidate-index","value":{"kind":"end"}}`)})
		}
		return reply.references, reply.referencesError
	default:
		return nil, fmt.Errorf("unexpected semantic method %q", method)
	}
}

func (c *semanticScriptedCandidateClient) EventsSince(cursor uint64) ([]*jsonrpc.Message, uint64, bool) {
	if cursor > uint64(len(c.events)) {
		return nil, uint64(len(c.events)), true
	}
	return append([]*jsonrpc.Message(nil), c.events[cursor:]...), uint64(len(c.events)), false
}

func TestS20_CandidateSemanticReadinessWaitsForCompleteStableResults(t *testing.T) {
	definition := semanticLocation{URI: "file:///workspace/target.go", StartLine: 0, StartChar: 4, EndLine: 0, EndChar: 10}
	call := semanticLocation{URI: "file:///workspace/use.go", StartLine: 1, StartChar: 8, EndLine: 1, EndChar: 14}
	header := semanticLocation{URI: "file:///workspace/target.h", StartLine: 0, StartChar: 4, EndLine: 0, EndChar: 10}
	locationJSON := func(location semanticLocation) json.RawMessage {
		value := []map[string]any{{
			"uri": location.URI,
			"range": map[string]any{
				"start": map[string]uint32{"line": location.StartLine, "character": location.StartChar},
				"end":   map[string]uint32{"line": location.EndLine, "character": location.EndChar},
			},
		}}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	empty := json.RawMessage("null")
	definitionRaw := locationJSON(definition)
	definitionItems, err := func() ([]json.RawMessage, error) {
		var values []json.RawMessage
		err := json.Unmarshal(definitionRaw, &values)
		return values, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	callItems, err := func() ([]json.RawMessage, error) {
		var values []json.RawMessage
		err := json.Unmarshal(locationJSON(call), &values)
		return values, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	headerItems, err := func() ([]json.RawMessage, error) {
		var values []json.RawMessage
		err := json.Unmarshal(locationJSON(header), &values)
		return values, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	referencesRaw, err := json.Marshal(append(definitionItems, callItems...))
	if err != nil {
		t.Fatal(err)
	}
	completeReferencesRaw, err := json.Marshal(append(append([]json.RawMessage(nil), definitionItems...), append(callItems, headerItems...)...))
	if err != nil {
		t.Fatal(err)
	}
	client := &semanticScriptedCandidateClient{replies: []semanticScriptedReply{
		{definition: empty, references: empty},
		{definition: definitionRaw, references: referencesRaw},
		{definition: definitionRaw, references: referencesRaw},
		{definition: definitionRaw, references: completeReferencesRaw},
		{definition: definitionRaw, references: completeReferencesRaw},
		{definition: definitionRaw, references: completeReferencesRaw},
	}}
	definitions, references, observed, err := semanticWaitCandidateLocations(
		client, 0,
		map[string]any{"textDocument": map[string]string{"uri": "file:///workspace/use.go"}},
		map[string]any{"textDocument": map[string]string{"uri": "file:///workspace/use.go"}, "context": map[string]bool{"includeDeclaration": true}},
		definition, call, 3*time.Second, header,
	)
	if err != nil {
		t.Fatalf("semanticWaitCandidateLocations: %v (readiness=%+v)", err, observed)
	}
	if !reflect.DeepEqual(definitions, []semanticLocation{definition}) || !containsSemanticLocation(references, definition) || !containsSemanticLocation(references, call) || !containsSemanticLocation(references, header) {
		t.Fatalf("candidate locations = definitions %+v references %+v; expected complete fixture locations", definitions, references)
	}
	if observed["attempts"] != 6 || observed["stableCompleteSnapshots"] != 3 || observed["requiredDefinitionObserved"] != true || observed["requiredCallObserved"] != true {
		t.Fatalf("candidate readiness = %+v; expected 6 attempts and 3 complete snapshots", observed)
	}
	if len(client.tokens) != 12 || client.tokens[0] == client.tokens[2] || observed["activeProgressTokensAtEnd"].([]string) != nil && len(observed["activeProgressTokensAtEnd"].([]string)) != 0 {
		t.Fatalf("candidate progress/token evidence is incomplete: tokens=%v readiness=%+v", client.tokens, observed)
	}
}

type semanticScriptedRequestClient struct {
	errors  map[string]error
	results map[string]json.RawMessage
	calls   []string
}

func (c *semanticScriptedRequestClient) RequestContext(ctx context.Context, method string, _ any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.calls = append(c.calls, method)
	return c.results[method], c.errors[method]
}

func TestS20_UpstreamSemanticReadinessWaitsForCompleteStableOracle(t *testing.T) {
	definition := semanticLocation{URI: "file:///workspace/target.rs", StartLine: 0, StartChar: 7, EndLine: 0, EndChar: 13}
	call := semanticLocation{URI: "file:///workspace/use.rs", StartLine: 1, StartChar: 30, EndLine: 1, EndChar: 36}
	header := semanticLocation{URI: "file:///workspace/target.h", StartLine: 0, StartChar: 4, EndLine: 0, EndChar: 10}
	locationJSON := func(location semanticLocation) json.RawMessage {
		value := []map[string]any{{
			"uri": location.URI,
			"range": map[string]any{
				"start": map[string]uint32{"line": location.StartLine, "character": location.StartChar},
				"end":   map[string]uint32{"line": location.EndLine, "character": location.EndChar},
			},
		}}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	empty := json.RawMessage("null")
	positiveDefinition := locationJSON(definition)
	positiveReferencesItems := []json.RawMessage{}
	if err := json.Unmarshal(positiveDefinition, &positiveReferencesItems); err != nil {
		t.Fatal(err)
	}
	callJSON := []json.RawMessage{}
	if err := json.Unmarshal(locationJSON(call), &callJSON); err != nil {
		t.Fatal(err)
	}
	headerJSON := []json.RawMessage{}
	if err := json.Unmarshal(locationJSON(header), &headerJSON); err != nil {
		t.Fatal(err)
	}
	partialReferences, err := json.Marshal(append(append([]json.RawMessage(nil), positiveReferencesItems...), callJSON...))
	if err != nil {
		t.Fatal(err)
	}
	positiveReferences, err := json.Marshal(append(append([]json.RawMessage(nil), positiveReferencesItems...), append(callJSON, headerJSON...)...))
	if err != nil {
		t.Fatal(err)
	}
	progress := []upstream.Notification{
		{Sequence: 1, Method: "$/progress", Params: json.RawMessage(`{"token":"workspace-index","value":{"kind":"begin","title":"indexing"}}`)},
		{Sequence: 2, Method: "$/progress", Params: json.RawMessage(`{"token":"workspace-index","value":{"kind":"end"}}`)},
	}
	client := &semanticScriptedProgressClient{
		replies: []semanticScriptedReply{
			{definition: empty, references: empty},
			{definition: positiveDefinition, references: partialReferences},
			{definition: positiveDefinition, references: partialReferences},
			{definition: positiveDefinition, references: positiveReferences},
			{definition: positiveDefinition, references: positiveReferences},
			{definition: positiveDefinition, references: positiveReferences},
		},
		progress: progress,
	}
	definitions, references, observed, err := semanticWaitUpstreamLocations(
		client, 0,
		map[string]any{"textDocument": map[string]string{"uri": "file:///workspace/use.rs"}},
		map[string]any{"textDocument": map[string]string{"uri": "file:///workspace/use.rs"}, "context": map[string]bool{"includeDeclaration": true}},
		definition, call, 3*time.Second, header,
	)
	if err != nil {
		t.Fatalf("semanticWaitUpstreamLocations: %v (observed %+v)", err, observed)
	}
	if len(definitions) != 1 || len(references) != 3 || observed["attempts"] != 6 || observed["stableCompleteSnapshots"] != 3 {
		t.Fatalf("readiness did not wait for complete, stable responses: defs=%+v refs=%+v observed=%+v", definitions, references, observed)
	}
	if len(observed["progressEvents"].([]map[string]string)) != 2 || len(observed["activeProgressTokensAtEnd"].([]string)) != 0 {
		t.Fatalf("work-done progress was not captured and drained: %+v", observed)
	}
	if !containsSemanticLocation(references, header) || len(client.tokens) != 12 || len(uniqueStrings(client.tokens...)) != len(client.tokens) {
		t.Fatalf("each upstream sample must carry unique work-done tokens: %v", client.tokens)
	}
}

func TestS20_UpstreamSemanticReadinessTimesOutOnStableEmptyOracle(t *testing.T) {
	empty := json.RawMessage("null")
	client := &semanticScriptedProgressClient{replies: []semanticScriptedReply{{definition: empty, references: empty}}}
	definition := semanticLocation{URI: "file:///workspace/target.rs", StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 6}
	call := semanticLocation{URI: "file:///workspace/use.rs", StartLine: 0, StartChar: 8, EndLine: 0, EndChar: 14}
	definitions, references, observed, err := semanticWaitUpstreamLocations(
		client, 0, map[string]any{}, map[string]any{}, definition, call, 25*time.Millisecond,
	)
	if err == nil || len(definitions) != 0 || len(references) != 0 || observed["stableCompleteSnapshots"] != 0 {
		t.Fatalf("stable empty oracle passed readiness: defs=%+v refs=%+v observed=%+v err=%v", definitions, references, observed, err)
	}
}

func TestS20_GoNegativeOracleKeepsExactNoIdentifierErrorAsEvidence(t *testing.T) {
	noIdentifier := errors.New("jsonrpc error 0: no identifier found")
	client := &semanticScriptedRequestClient{
		errors:  map[string]error{"textDocument/definition": noIdentifier, "textDocument/references": noIdentifier},
		results: map[string]json.RawMessage{},
	}
	result := semanticGoNegativeOraclePair(client, map[string]any{"position": map[string]uint32{"line": 3, "character": 3}})
	if result.Err != nil || !result.DefinitionNoLocation || !result.ReferencesNoLocation ||
		result.DefinitionError != noIdentifier.Error() || result.ReferencesError != noIdentifier.Error() ||
		!reflect.DeepEqual(client.calls, []string{"textDocument/definition", "textDocument/references"}) {
		t.Fatalf("exact no-identifier oracle was not retained as empty-location evidence: result=%+v calls=%v", result, client.calls)
	}
	client.errors["textDocument/definition"] = errors.New("jsonrpc error -32603: no identifier found")
	result = semanticGoNegativeOraclePair(client, nil)
	if result.Err == nil {
		t.Fatal("unrecognized JSON-RPC code was incorrectly treated as a verified no-location result")
	}
}

func TestS20_UpstreamNotificationCapture(t *testing.T) {
	const helperEnv = "OMNILSP_UPSTREAM_NOTIFICATION_HELPER"
	if os.Getenv(helperEnv) == "1" {
		codec := jsonrpc.NewCodec()
		for version := 1; version <= 257; version++ {
			params, err := json.Marshal(map[string]any{
				"uri": "file:///workspace/broken.go", "version": version, "diagnostics": []any{},
			})
			if err != nil {
				t.Fatal(err)
			}
			message := &jsonrpc.Message{JSONRPC: jsonrpc.Version, Method: "textDocument/publishDiagnostics", Params: params}
			if err := codec.WriteMessage(os.Stdout, message); err != nil {
				t.Fatal(err)
			}
		}
		for {
			message, err := codec.ReadMessage(os.Stdin)
			if err != nil {
				return
			}
			if message.IsRequest() {
				if err := codec.WriteMessage(os.Stdout, jsonrpc.NewResponse(*message.ID, json.RawMessage("null"))); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if message.IsNotification() && message.Method == "exit" {
				return
			}
		}
	}

	session, err := upstream.Start(os.Args[0], []string{"-test.run=^TestS20_UpstreamNotificationCapture$"}, t.TempDir(), []string{helperEnv + "=1"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	deadline := time.Now().Add(5 * time.Second)
	for session.NotificationCursor() < 257 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if session.NotificationCursor() != 257 {
		t.Fatalf("captured notification cursor = %d, want 257", session.NotificationCursor())
	}
	if notifications, overflow := session.NotificationsSince(0); !overflow || notifications != nil {
		t.Fatalf("bounded history query = (%d events, overflow=%v), want (0 events, true)", len(notifications), overflow)
	}
	notifications, overflow := session.NotificationsSince(256)
	if overflow || len(notifications) != 1 || notifications[0].Sequence != 257 || notifications[0].Method != "textDocument/publishDiagnostics" {
		t.Fatalf("latest bounded notification = (%+v, overflow=%v), want sequence 257 diagnostics", notifications, overflow)
	}
	var params semanticPublishDiagnostics
	if err := json.Unmarshal(notifications[0].Params, &params); err != nil || params.Version == nil || *params.Version != 257 {
		t.Fatalf("captured notification params = %+v, decode error %v; want version 257", params, err)
	}
	notifications[0].Params[0] = 'x'
	copyAgain, overflow := session.NotificationsSince(256)
	if overflow || len(copyAgain) != 1 || len(copyAgain[0].Params) == 0 || copyAgain[0].Params[0] != '{' {
		t.Fatal("notification snapshots share mutable parameter storage")
	}
}

func errorCode(err *jsonrpc.ResponseError) int {
	if err == nil {
		return 0
	}
	return err.Code
}
