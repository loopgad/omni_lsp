package corpus

// §S21 accuracy verification (goal.md 行 4474-4487): every response produced
// while serving the qualified corpus is inspected and sorted into exactly the
// six release-blocking error buckets. The qualified-corpus goal is that all
// six counters stay zero:
//
//	Wrong Edit Rate            = 0
//	Stale Edit Applied         = 0
//	Wrong-file Location        = 0
//	Position Mapping Error     = 0
//	Protocol-invalid Response  = 0
//	Snapshot Mixing            = 0
//
// The acceptance harness supplies the frozen OmniLSP candidate through
// OMNILSP_BIN. A missing candidate is an environmental skip for form-only
// invocations; a run that explicitly opts in through OMNILSP_S21_GATE=required
// (the OMNILSP_SOAK_GATE=required contract of docs/soak-nightly.md) treats a
// missing candidate as a hard failure instead of a silent not_verified. Direct
// backend adapters are intentionally not used by this release evidence.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/omnilsp/omni/test/acceptance/lspdriver"
)

// --- negative fixtures: prove every bucket can fire ---

const fixtureSrc = "package m\n\nfunc greet() {}\n"

// fixtureBase builds a clean rename response over fixtureSrc: one edit whose
// range covers "greet" (line 2, UTF-16 cols 5..10) and renames it greet2.
func fixtureBase() (ClassifyRequest, LSPResponse) {
	req := ClassifyRequest{
		URI:     "file:///m.go",
		Content: []byte(fixtureSrc),
		Symbol:  "greet",
		NewName: "greet2",
		Rev:     7,
		Epoch:   3,
	}
	resp := LSPResponse{
		Feature:     "rename",
		Status:      identity.ResultExact,
		URIs:        []string{req.URI},
		Ranges:      []languages.Range{{StartLine: 2, StartCharacter: 5, EndLine: 2, EndCharacter: 10}},
		EditNewText: []string{"greet2"},
		Rev:         7,
		Epoch:       3,
	}
	return req, resp
}

func TestS21_WrongFileLocationUsesCanonicalURIIdentity(t *testing.T) {
	tests := []struct {
		name        string
		requestURI  string
		responseURI string
		want        []string
	}{
		{
			name:        "equivalent windows drive and escaping spellings",
			requestURI:  "file:///C:/Users/A%20B/%7Espace.py",
			responseURI: "file:///c:/Users/A%20B/~space.py",
		},
		{
			name:        "encoded drive colon matches literal drive colon",
			requestURI:  "file:///C:/Users/A%20B/main.py",
			responseURI: "file:///c%3A/Users/A%20B/main.py",
		},
		{
			name:        "different target",
			requestURI:  "file:///C:/Users/A%20B/main.py",
			responseURI: "file:///C:/Users/A%20B/other.py",
			want:        []string{"wrongFileLocation"},
		},
		{
			name:        "malformed response URI",
			requestURI:  "file:///C:/Users/A%20B/main.py",
			responseURI: "file:///C:/Users/A%20B/bad%.py",
			want:        []string{"wrongFileLocation"},
		},
		{
			name:        "malformed request URI",
			requestURI:  "file:///C:/Users/A%20B/bad%.py",
			responseURI: "file:///C:/Users/A%20B/bad%.py",
			want:        []string{"wrongFileLocation"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyResponse(
				ClassifyRequest{URI: tt.requestURI},
				LSPResponse{URIs: []string{tt.responseURI}},
			)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("buckets = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestS21_ZeroErrorClassification runs hover/definition/references/rename
// through one frozen OmniLSP candidate process per corpus case. It sorts the
// candidate's wire responses into the six §S21 buckets, prints the table, and
// fails if any bucket is non-zero. A subtest first proves the classifier itself
// fires on known-bad responses — otherwise all-zero would prove nothing.
func TestS21_ZeroErrorClassification(t *testing.T) {
	reportPath := strings.TrimSpace(os.Getenv(s21ReportPathEnv))
	evidenceEnabled := reportPath != ""
	// S21 is the candidate-process accuracy gate for the complete release
	// corpus. Every language below executes the same four public LSP methods;
	// a missing candidate backend is recorded as an explicit skip and therefore
	// makes the structured report not_verified.
	requiredLanguages := []string{"go", "c", "cpp", "rust", "python", "typescript", "javascript"}
	evidence := newS21Evidence(requiredLanguages,
		strings.TrimSpace(os.Getenv("OMNILSP_RUN_ID")),
		strings.TrimSpace(os.Getenv("OMNILSP_CANDIDATE_SHA256")))
	var buckets ErrorBuckets
	defer func() {
		if !evidenceEnabled {
			return
		}
		evidence.buckets = buckets
		report := evidence.report()
		if err := writeS21Report(reportPath, report); err != nil {
			t.Errorf("write S21 evidence report: %v", err)
			return
		}
		if report.Decision != s21Passed {
			t.Errorf("S21 acceptance evidence decision is %q: incomplete or failing attributable corpus coverage", report.Decision)
		}
	}()
	candidate, candidateErr := s21CandidateBinary()
	if candidateErr != nil {
		for _, language := range requiredLanguages {
			evidence.skippedLanguages[language] = candidateErr.Error()
		}
		if s21GateRequired() {
			t.Fatalf("OMNILSP_S21_GATE=required opted in but no frozen candidate is available: %v", candidateErr)
		}
		t.Skip(candidateErr.Error())
	}
	if testing.Short() {
		if evidenceEnabled {
			for _, lang := range requiredLanguages {
				evidence.skippedLanguages[lang] = "testing.Short() skips language toolchains"
			}
		}
		if s21GateRequired() {
			t.Fatalf("OMNILSP_S21_GATE=required opted in but -short skips the language toolchains; rerun without -short")
		}
		t.Skip("requires language toolchains")
	}

	if !t.Run("classifier_fires_on_known_bad_responses", func(t *testing.T) {
		// Astral content: col 6 lands inside the 🦀 surrogate pair, so the
		// engine must reject the mapping (and skip text extraction).
		astralContent := "package m\n\nfunc \U0001f980greet() {}\n"

		fixtures := []struct {
			name string
			mut  func(*ClassifyRequest, *LSPResponse)
			want []string
		}{
			{"clean_response_fires_nothing", func(*ClassifyRequest, *LSPResponse) {}, nil},
			{"transport_error", func(_ *ClassifyRequest, r *LSPResponse) { r.Err = "dial failed" },
				[]string{"protocolInvalidResponse"}},
			{"illegal_status_shape", func(_ *ClassifyRequest, r *LSPResponse) { r.Status = 99 },
				[]string{"protocolInvalidResponse"}},
			{"foreign_uri", func(_ *ClassifyRequest, r *LSPResponse) { r.URIs[0] = "file:///elsewhere.go" },
				[]string{"wrongFileLocation"}},
			{"edit_covers_wrong_text", func(_ *ClassifyRequest, r *LSPResponse) {
				r.Ranges[0].StartCharacter = 6
			}, []string{"wrongEdit"}},
			{"edit_renames_to_wrong_name", func(_ *ClassifyRequest, r *LSPResponse) {
				r.EditNewText[0] = "totally_other"
			}, []string{"wrongEdit"}},
			{"surrogate_midpoint_offset", func(rq *ClassifyRequest, r *LSPResponse) {
				rq.Content = []byte(astralContent)
				r.Ranges[0] = languages.Range{StartLine: 2, StartCharacter: 6, EndLine: 2, EndCharacter: 12}
				r.EditNewText = nil
			}, []string{"positionMappingError"}},
			{"stale_revision", func(_ *ClassifyRequest, r *LSPResponse) { r.Rev = 99 },
				[]string{"staleEdit", "snapshotMixing"}},
			{"epoch_mismatch", func(_ *ClassifyRequest, r *LSPResponse) { r.Epoch = 999 },
				[]string{"staleEdit", "snapshotMixing"}},
		}
		for _, fx := range fixtures {
			t.Run(fx.name, func(t *testing.T) {
				req, resp := fixtureBase()
				fx.mut(&req, &resp)
				got := ClassifyResponse(req, resp)
				sort.Strings(got)
				want := append([]string(nil), fx.want...)
				sort.Strings(want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("buckets = %v, want %v", got, want)
				}
			})
		}
	}) {
		evidence.classifierFailed = true
	}

	langs := []struct {
		name     string
		ext      string
		keyword  string
		language string
	}{
		{name: "go", ext: ".go", keyword: "func ", language: "go"},
		{name: "c", ext: ".c", keyword: "int ", language: "c"},
		{name: "cpp", ext: ".cpp", keyword: "int ", language: "cpp"},
		{name: "rust", ext: ".rs", keyword: "fn ", language: "rust"},
		{name: "python", ext: ".py", keyword: "def ", language: "python"},
		{name: "typescript", ext: ".ts", keyword: "function ", language: "typescript"},
		{name: "javascript", ext: ".js", keyword: "function ", language: "javascript"},
	}

	for _, lang := range langs {
		languagePassed := t.Run(lang.name, func(t *testing.T) {
			files, err := filepath.Glob(filepath.Join("testdata", lang.name, "*"+lang.ext))
			if err != nil || len(files) < 2 {
				t.Fatalf("corpus too thin for %s: %v (%d files)", lang.name, err, len(files))
			}

			for _, f := range files {
				src, rerr := os.ReadFile(f)
				if rerr != nil {
					t.Fatalf("%s: %v", f, rerr)
				}
				caseName := filepath.Base(f)
				if !t.Run(caseName, func(t *testing.T) {
					line, col, sym, ok := findDeclSymbol(src, lang.keyword)
					if !ok {
						evidence.skipCase(lang.name, caseName, "no declaration for configured language keyword")
						t.Skipf("no %q declaration in corpus file", lang.keyword)
					}
					dir := t.TempDir()
					writeS21WorkspaceScaffold(t, dir, lang.name, filepath.Base(f))
					furi := writeCorpusFile(t, dir, filepath.Base(f), src)
					session := lspdriver.Start(t, candidate, dir, nil)
					session.Initialize(t)
					defer session.Close(t)
					if !s21CandidateBackendAvailable(t, session, lang.language) {
						evidence.skippedLanguages[lang.name] = "candidate backend unavailable"
						t.Skip("candidate backend unavailable")
					}
					session.Notify(t, "textDocument/didOpen", map[string]any{
						"textDocument": map[string]any{
							"uri": furi, "languageId": lang.language, "version": 1, "text": string(src),
						},
					})
					evidence.testedLanguage[lang.name] = true
					caseEvidence := evidence.beginCase(lang.name, caseName)
					expectPositiveResults := !strings.HasPrefix(caseName, "broken.")
					runStdioQueriesAndClassify(t, session, furi, src, sym, line, col, &buckets, caseEvidence, expectPositiveResults)
				}) {
					evidence.failedLanguages[lang.name] = "one or more corpus cases failed"
				}
			}
		})
		if !languagePassed && evidence.failedLanguages[lang.name] == "" && evidence.skippedLanguages[lang.name] == "" {
			evidence.failedLanguages[lang.name] = "language subtest failed"
		}
	}

	t.Logf("\n%sall six buckets must be zero (§S21)", &buckets)
	if buckets.Total() != 0 {
		t.Fatalf("§S21 violated: %d error-bucket hits across the qualified corpus", buckets.Total())
	}
}

// runStdioQueriesAndClassify drives hover → definition → references → rename
// through the candidate's LSP transport and records every response. The
// candidate's private explain method supplies the snapshot/backend evidence
// that the public LSP payload intentionally omits. A missing evidence record
// is a protocol failure, so a direct adapter call cannot produce acceptance
// evidence.
func runStdioQueriesAndClassify(t *testing.T, session *lspdriver.Session,
	furi string, src []byte, sym string, line, col int, buckets *ErrorBuckets, evidenceCase *s21TestedCase,
	expectPositiveResults bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	position := map[string]uint32{"line": uint32(line), "character": uint32(col)}
	definitionPosition := position
	referencesPosition := position
	if useLine, useColumn, found := findSymbolUsePosition(src, sym, line); found {
		usePosition := map[string]uint32{"line": uint32(useLine), "character": uint32(useColumn)}
		definitionPosition = usePosition
		referencesPosition = usePosition
	}
	textDocument := map[string]string{"uri": furi}
	queries := []struct {
		name   string
		params map[string]any
		newReq func(ClassifyRequest) ClassifyRequest
	}{
		{name: "hover", params: map[string]any{"textDocument": textDocument, "position": position}},
		{name: "definition", params: map[string]any{"textDocument": textDocument, "position": definitionPosition}},
		{name: "references", params: map[string]any{
			"textDocument": textDocument, "position": referencesPosition,
			"context": map[string]bool{"includeDeclaration": true},
		}},
		{name: "rename", params: map[string]any{
			"textDocument": textDocument, "position": position, "newName": sym + "_renamed",
		}, newReq: func(req ClassifyRequest) ClassifyRequest {
			req.NewName = sym + "_renamed"
			return req
		}},
	}
	request := ClassifyRequest{URI: furi, Content: src, Symbol: sym}
	for _, query := range queries {
		var raw json.RawMessage
		var response LSPResponse
		if expectPositiveResults && query.name != "rename" {
			poll := pollS21PositiveResponse(ctx, query.name, 200*time.Millisecond, func(requestCtx context.Context) (json.RawMessage, LSPResponse) {
				responseRaw, requestErr := session.RequestContext(requestCtx, "textDocument/"+query.name, query.params)
				response := candidateLSPResponse(requestCtx, session, query.name, furi, responseRaw, requestErr)
				return responseRaw, response
			})
			raw, response = poll.Raw, poll.Response
			if poll.Attempts > 1 {
				t.Logf("%s positive-result polling: attempts=%d empty_responses=%d elapsed=%s", query.name, poll.Attempts, poll.EmptyResponses, poll.Elapsed)
			}
			if poll.ValidationErr != nil {
				t.Errorf("%s did not produce a positive result for known symbol %q after %d attempt(s), %d empty response(s), elapsed=%s: %v",
					query.name, sym, poll.Attempts, poll.EmptyResponses, poll.Elapsed, poll.ValidationErr)
				if ctx.Err() != nil {
					// The final attempt no longer has a live context from which
					// to obtain attributable snapshot/backend evidence. Record
					// the failed operation and stop this case instead of turning
					// the exhausted context into artificial stale/mixing hits on
					// every remaining method.
					evidenceCase.OperationCount.add(query.name)
					evidenceCase.Operations++
					return
				}
			}
		} else {
			var requestErr error
			raw, requestErr = session.RequestContext(ctx, "textDocument/"+query.name, query.params)
			response = candidateLSPResponse(ctx, session, query.name, furi, raw, requestErr)
		}
		if query.name == "hover" {
			request.Rev = response.Rev
			request.Epoch = response.Epoch
		}
		queryRequest := request
		if query.newReq != nil {
			queryRequest = query.newReq(queryRequest)
		}
		evidenceCase.OperationCount.add(query.name)
		evidenceCase.Operations++
		logResponseHits(t, query.name, request.URI, buckets.Record(queryRequest, response), response)
		if ctx.Err() != nil {
			return
		}
		if expectPositiveResults && query.name == "rename" && response.Status == identity.ResultExact {
			// A successful rename on a known symbol must carry a real edit.
			// A null or RequestFailed response remains a legal fail-closed
			// refusal when the backend cannot prove a safe workspace edit.
			if err := validateS21PositiveResponse(query.name, raw, response); err != nil {
				t.Errorf("rename returned an empty or invalid successful edit for known symbol %q: %v", sym, err)
			}
		}
	}
}

func logResponseHits(t *testing.T, feature, requestURI string, hits []string, response LSPResponse) {
	t.Helper()
	if len(hits) > 0 {
		t.Errorf("%s: hit buckets %v, requestURI=%q responseURIs=%q ranges=%+v revision=%d epoch=%d status=%d error=%q",
			feature, hits, requestURI, response.URIs, response.Ranges, response.Rev, response.Epoch, response.Status, response.Err)
	}
}

// writeCorpusFile materializes a corpus source inside the backend workspace
// and returns its canonical file URI (§D2).
func writeCorpusFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	fpath := filepath.Join(dir, name)
	if err := os.WriteFile(fpath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return uri.FromPath(fpath).Canonical()
}

// findDeclSymbol locates the identifier after a top-level declaration keyword
// ("fn ", "def ", "function ") and returns its last-char position plus the
// identifier text itself (needed to verify ranges cover the right symbol).
func findDeclSymbol(src []byte, kw string) (line, col int, symbol string, ok bool) {
	for i, raw := range strings.Split(string(src), "\n") {
		l := strings.TrimSpace(raw)
		if strings.HasPrefix(l, kw) {
			rest := strings.TrimPrefix(l, kw)
			if idx := strings.IndexFunc(rest, func(r rune) bool { return r == '(' }); idx > 0 {
				symbol := strings.TrimSpace(rest[:idx])
				nameStart := strings.Index(rest[:idx], symbol)
				if nameStart < 0 || symbol == "" {
					continue
				}
				return i, utf16Column(kw) + utf16Column(rest[:nameStart]) + utf16Column(symbol) - 1, symbol, true
			}
		}
	}
	return 0, 0, "", false
}

// findSymbolUsePosition finds the first whole-identifier occurrence after a
// declaration. Definition and references queries should exercise resolution
// from a use site, not depend on server-specific behavior when the cursor is
// already on the declaration name.
func findSymbolUsePosition(src []byte, symbol string, declarationLine int) (line, col int, ok bool) {
	if symbol == "" {
		return 0, 0, false
	}
	lines := strings.Split(string(src), "\n")
	for lineIndex := declarationLine + 1; lineIndex < len(lines); lineIndex++ {
		text := lines[lineIndex]
		for offset := 0; offset < len(text); {
			relative := strings.Index(text[offset:], symbol)
			if relative < 0 {
				break
			}
			start := offset + relative
			end := start + len(symbol)
			leftOK := start == 0 || !isS21IdentifierRune(lastRune(text[:start]))
			rightOK := end == len(text) || !isS21IdentifierRune(firstRune(text[end:]))
			if leftOK && rightOK {
				return lineIndex, utf16Column(text[:start]) + utf16Column(symbol) - 1, true
			}
			offset = start + 1
		}
	}
	return 0, 0, false
}

func isS21IdentifierRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func firstRune(text string) rune {
	r, _ := utf8.DecodeRuneInString(text)
	return r
}

func lastRune(text string) rune {
	r, _ := utf8.DecodeLastRuneInString(text)
	return r
}

func TestFindSymbolUsePositionUsesWholeIdentifierAndUTF16(t *testing.T) {
	source := []byte("fn greet() {}\nfn use_greet() { let emoji = \"🦀\"; greet(); }\n")
	line, column, ok := findSymbolUsePosition(source, "greet", 0)
	if !ok {
		t.Fatal("findSymbolUsePosition did not find the call after the declaration")
	}
	if line != 1 || column != 39 {
		t.Fatalf("use position = %d:%d, want 1:39 (skip use_greet suffix and count crab as two UTF-16 units)", line, column)
	}
}

func utf16Column(value string) int {
	column := 0
	for _, r := range value {
		if r > 0xffff {
			column += 2
		} else {
			column++
		}
	}
	return column
}
