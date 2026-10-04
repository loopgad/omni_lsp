package corpus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/test/acceptance/lspdriver"
)

// s21CandidateBinary is deliberately environment-only. S21 evidence is a
// release claim about the frozen candidate process; falling back to a package
// backend here would make a report look attributable while bypassing the
// candidate and its transport, routing, snapshot, and response projection.
func s21CandidateBinary() (string, error) {
	configured := strings.TrimSpace(os.Getenv("OMNILSP_BIN"))
	if configured == "" {
		return "", errors.New("OMNILSP_BIN must name the frozen candidate binary")
	}
	abs, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("resolve OMNILSP_BIN: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("candidate binary is unavailable: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("candidate binary is a directory: %s", abs)
	}
	claimed := strings.TrimSpace(os.Getenv("OMNILSP_CANDIDATE_SHA256"))
	if claimed != "" {
		actual, hashErr := s21FileSHA256(abs)
		if hashErr != nil {
			return "", fmt.Errorf("hash candidate binary: %w", hashErr)
		}
		if !strings.EqualFold(actual, claimed) {
			return "", fmt.Errorf("candidate hash %s does not match OMNILSP_CANDIDATE_SHA256 %s", actual, claimed)
		}
	}
	return abs, nil
}

func s21FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// TestS21CandidateBinaryRequired makes the no-fallback rule executable. It
// also protects future refactors from reintroducing direct adapter execution
// in the report-producing path.
func TestS21CandidateBinaryRequired(t *testing.T) {
	t.Setenv("OMNILSP_BIN", "")
	if _, err := s21CandidateBinary(); err == nil {
		t.Fatal("S21 accepted an empty candidate path")
	}
}

func TestS21WireParsersValidateCandidatePayloads(t *testing.T) {
	t.Run("hover range", func(t *testing.T) {
		ranges, err := parseS21Hover(json.RawMessage(`{"contents":"fn greet()","range":{"start":{"line":1,"character":4},"end":{"line":1,"character":9}}}`))
		if err != nil || len(ranges) != 1 || ranges[0].StartCharacter != 4 || ranges[0].EndCharacter != 9 {
			t.Fatalf("hover projection = %+v, err=%v", ranges, err)
		}
	})
	t.Run("definition singleton and location link", func(t *testing.T) {
		for _, raw := range []json.RawMessage{
			json.RawMessage(`{"uri":"file:///workspace/main.rs","range":{"start":{"line":1,"character":0},"end":{"line":1,"character":4}}}`),
			json.RawMessage(`[{"targetUri":"file:///workspace/main.rs","targetRange":{"start":{"line":1,"character":0},"end":{"line":1,"character":4}},"targetSelectionRange":{"start":{"line":1,"character":0},"end":{"line":1,"character":4}}}]`),
		} {
			uris, ranges, err := parseS21Locations(raw)
			if err != nil || len(uris) != 1 || len(ranges) != 1 {
				t.Fatalf("locations = (%v, %v), err=%v", uris, ranges, err)
			}
		}
	})
	t.Run("workspace edit rejects missing new text", func(t *testing.T) {
		_, _, _, err := parseS21WorkspaceEdit(json.RawMessage(`{"changes":{"file:///workspace/main.rs":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}]}}`))
		if err == nil {
			t.Fatal("malformed workspace edit was accepted")
		}
	})
}

func TestS21PositiveResponseValidationRejectsLegalEmptyResults(t *testing.T) {
	tests := []struct {
		name     string
		feature  string
		raw      string
		response LSPResponse
		wantErr  bool
	}{
		{
			name:     "hover markup content",
			feature:  "hover",
			raw:      `{"contents":{"kind":"markdown","value":"func greet()"}}`,
			response: LSPResponse{Feature: "hover", Status: identity.ResultExact},
		},
		{
			name:     "hover empty content",
			feature:  "hover",
			raw:      `{"contents":"  "}`,
			response: LSPResponse{Feature: "hover", Status: identity.ResultExact},
			wantErr:  true,
		},
		{
			name:     "definition empty array",
			feature:  "definition",
			raw:      `[]`,
			response: LSPResponse{Feature: "definition", Status: identity.ResultExact},
			wantErr:  true,
		},
		{
			name:    "references with a location",
			feature: "references",
			raw:     `[{"uri":"file:///workspace/main.go","range":{"start":{"line":1,"character":5},"end":{"line":1,"character":10}}}]`,
			response: LSPResponse{
				Feature: "references", Status: identity.ResultExact,
				URIs:   []string{"file:///workspace/main.go"},
				Ranges: []languages.Range{{StartLine: 1, StartCharacter: 5, EndLine: 1, EndCharacter: 10}},
			},
		},
		{
			name:     "successful rename without edits",
			feature:  "rename",
			raw:      `{"changes":{}}`,
			response: LSPResponse{Feature: "rename", Status: identity.ResultExact},
			wantErr:  true,
		},
		{
			name:    "successful rename with edit",
			feature: "rename",
			raw:     `{"changes":{"file:///workspace/main.go":[{"range":{"start":{"line":1,"character":5},"end":{"line":1,"character":10}},"newText":"greet2"}]}}`,
			response: LSPResponse{
				Feature: "rename", Status: identity.ResultExact,
				URIs:        []string{"file:///workspace/main.go"},
				Ranges:      []languages.Range{{StartLine: 1, StartCharacter: 5, EndLine: 1, EndCharacter: 10}},
				EditNewText: []string{"greet2"},
			},
		},
		{
			name:     "null is not positive evidence",
			feature:  "hover",
			raw:      `null`,
			response: LSPResponse{Feature: "hover", Status: identity.ResultUnknown},
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateS21PositiveResponse(tt.feature, json.RawMessage(tt.raw), tt.response)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateS21PositiveResponse() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestS21PositivePollingRetriesEmptyResultsAndFailsOnDeadline(t *testing.T) {
	t.Run("empty definition retries until a location arrives", func(t *testing.T) {
		attempt := 0
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result := pollS21PositiveResponse(ctx, "definition", time.Millisecond, func(context.Context) (json.RawMessage, LSPResponse) {
			attempt++
			switch attempt {
			case 1:
				return json.RawMessage(`null`), LSPResponse{Feature: "definition", Status: identity.ResultUnknown}
			case 2:
				return json.RawMessage(`[]`), LSPResponse{Feature: "definition", Status: identity.ResultExact}
			default:
				return json.RawMessage(`[{"uri":"file:///workspace/main.go","range":{"start":{"line":1,"character":5},"end":{"line":1,"character":10}}}]`), LSPResponse{
					Feature: "definition", Status: identity.ResultExact,
					URIs:   []string{"file:///workspace/main.go"},
					Ranges: []languages.Range{{StartLine: 1, StartCharacter: 5, EndLine: 1, EndCharacter: 10}},
				}
			}
		})
		if result.ValidationErr != nil || result.Attempts != 3 || result.EmptyResponses != 2 || len(result.Response.URIs) != 1 {
			t.Fatalf("poll result = %+v, want final positive result after 2 empty responses", result)
		}
	})

	t.Run("empty hover remains a failure at context deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
		defer cancel()
		result := pollS21PositiveResponse(ctx, "hover", time.Millisecond, func(context.Context) (json.RawMessage, LSPResponse) {
			return json.RawMessage(`null`), LSPResponse{Feature: "hover", Status: identity.ResultUnknown}
		})
		if result.ValidationErr == nil || result.Attempts == 0 || result.EmptyResponses == 0 || ctx.Err() == nil {
			t.Fatalf("deadline poll result = %+v, context err=%v; want a failed timed-out poll", result, ctx.Err())
		}
	})
}

// validateS21PositiveResponse is separate from the six error buckets: those
// buckets classify incorrect responses, while this check prevents clean null
// and empty payloads from proving useful semantic coverage.
func validateS21PositiveResponse(feature string, raw json.RawMessage, response LSPResponse) error {
	if response.Err != "" {
		return fmt.Errorf("request failed: %s", response.Err)
	}
	if response.Status != identity.ResultExact {
		return fmt.Errorf("result status is %d, want exact positive evidence", response.Status)
	}
	switch feature {
	case "hover":
		text, err := parseS21HoverContentText(raw)
		if err != nil {
			return err
		}
		if strings.TrimSpace(text) == "" {
			return errors.New("hover contents are empty")
		}
	case "definition", "references":
		if len(response.URIs) == 0 || len(response.Ranges) == 0 {
			return fmt.Errorf("%s returned no locations", feature)
		}
	case "rename":
		if len(response.Ranges) == 0 || len(response.EditNewText) != len(response.Ranges) {
			return errors.New("rename returned no text edits")
		}
	default:
		return fmt.Errorf("unsupported positive S21 feature %q", feature)
	}
	return nil
}

type s21PositivePollResult struct {
	Raw            json.RawMessage
	Response       LSPResponse
	Attempts       int
	EmptyResponses int
	Elapsed        time.Duration
	ValidationErr  error
}

// pollS21PositiveResponse retries only legal empty semantic results. Invalid
// payloads and transport errors return immediately for normal bucket
// classification; a deadline with only empty results remains a test failure.
func pollS21PositiveResponse(ctx context.Context, feature string, interval time.Duration,
	request func(context.Context) (json.RawMessage, LSPResponse)) (result s21PositivePollResult) {
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	started := time.Now()
	defer func() { result.Elapsed = time.Since(started) }()
	for {
		result.Raw, result.Response = request(ctx)
		result.Attempts++
		result.ValidationErr = validateS21PositiveResponse(feature, result.Raw, result.Response)
		if result.ValidationErr == nil || !s21PositiveResponseIsEmpty(feature, result.Raw, result.Response) {
			return result
		}
		result.EmptyResponses++
		if ctx.Err() != nil {
			return result
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return result
		case <-timer.C:
		}
	}
}

func s21PositiveResponseIsEmpty(feature string, raw json.RawMessage, response LSPResponse) bool {
	if response.Err != "" {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return response.Status == identity.ResultUnknown || response.Status == identity.ResultUnavailable
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	switch feature {
	case "hover":
		text, err := parseS21HoverContentText(trimmed)
		return err == nil && strings.TrimSpace(text) == ""
	case "definition", "references":
		return len(response.URIs) == 0 && len(response.Ranges) == 0
	default:
		return false
	}
}

func parseS21HoverContentText(raw json.RawMessage) (string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("hover result is not an object")
		}
		return "", fmt.Errorf("invalid hover result: %w", err)
	}
	contents, ok := object["contents"]
	if !ok {
		return "", errors.New("invalid hover result: missing contents")
	}
	var parts []string
	var collect func(json.RawMessage) error
	collect = func(value json.RawMessage) error {
		var text string
		if err := json.Unmarshal(value, &text); err == nil {
			parts = append(parts, text)
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(value, &items); err == nil {
			for _, item := range items {
				if err := collect(item); err != nil {
					return err
				}
			}
			return nil
		}
		var marked map[string]json.RawMessage
		if err := json.Unmarshal(value, &marked); err != nil || marked == nil {
			if err == nil {
				err = errors.New("hover content is not text")
			}
			return err
		}
		if _, ok := marked["value"]; !ok {
			return errors.New("hover content object is missing value")
		}
		return collect(marked["value"])
	}
	if err := collect(contents); err != nil {
		return "", fmt.Errorf("invalid hover contents: %w", err)
	}
	return strings.Join(parts, "\n"), nil
}

func s21CandidateBackendAvailable(t *testing.T, session *lspdriver.Session, language string) bool {
	t.Helper()
	if language == "javascript" {
		// JavaScript and TypeScript share the TypeScript backend; the
		// request's .js URI still exercises the JavaScript routing branch.
		language = "typescript"
	}
	raw := session.Request(t, "omnilsp/backendStatus", map[string]string{"language": language})
	var status struct {
		Found bool `json:"Found"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatalf("decode candidate backend status for %s: %v", language, err)
	}
	return status.Found
}

// writeS21WorkspaceScaffold creates only the project metadata needed by the
// candidate backend. The source file itself remains the checked-in corpus
// fixture, so the report can be traced to a stable two-case-per-language set.
func writeS21WorkspaceScaffold(t *testing.T, dir, language, fileName string) {
	t.Helper()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write S21 %s scaffold: %v", name, err)
		}
	}
	switch language {
	case "go":
		write("go.mod", "module s21.local/corpus\n\ngo 1.26\n")
	case "c", "cpp":
		compiler := "clang"
		if language == "cpp" {
			compiler = "clang++"
		}
		file := filepath.Join(dir, fileName)
		data, err := json.Marshal([]map[string]any{{
			"directory": dir,
			"file":      file,
			"arguments": []string{compiler, "-c", file},
		}})
		if err != nil {
			t.Fatalf("encode S21 compile database: %v", err)
		}
		write("compile_commands.json", string(data)+"\n")
	case "rust":
		write("Cargo.toml", "[package]\nname = \"corpus\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[[bin]]\nname = \"corpus\"\npath = \""+fileName+"\"\n")
	case "python":
		write("pyrightconfig.json", fmt.Sprintf("{\"include\":[%q],\"pythonVersion\":\"3.11\"}\n", fileName))
		// OmniLSP's safe-rename gate requires a project marker before it
		// will accept a project-wide edit from pyright.
		write("pyproject.toml", "[tool.pyright]\n")
	case "typescript":
		write("tsconfig.json", fmt.Sprintf("{\"compilerOptions\":{\"target\":\"ES2020\",\"module\":\"commonjs\",\"strict\":true},\"include\":[%q]}\n", fileName))
	case "javascript":
		write("jsconfig.json", fmt.Sprintf("{\"compilerOptions\":{\"target\":\"ES2020\",\"module\":\"commonjs\",\"checkJs\":true},\"include\":[%q]}\n", fileName))
	}
}

type s21WirePosition struct {
	Line      uint32 `json:"line"`
	Character uint32 `json:"character"`
}

type s21WireRange struct {
	Start s21WirePosition `json:"start"`
	End   s21WirePosition `json:"end"`
}

type s21WireTextEdit struct {
	Range   s21WireRange `json:"range"`
	NewText string       `json:"newText"`
}

type s21ExplainEvidence struct {
	Method       string  `json:"method"`
	URI          string  `json:"uri"`
	SnapshotRev  uint64  `json:"snapshotRev"`
	Backend      string  `json:"backend"`
	BackendEpoch *uint64 `json:"backendEpoch"`
}

type s21ExplainResponse struct {
	Evidence []s21ExplainEvidence `json:"evidence"`
}

// candidateLSPResponse projects one public LSP response onto the classifier
// shape. The wire response has no internal status, revision, or backend epoch;
// the candidate's run-qualified explain record supplies those identity facts.
func candidateLSPResponse(ctx context.Context, session *lspdriver.Session, feature, requestURI string, raw json.RawMessage, requestErr error) LSPResponse {
	response := LSPResponse{Feature: feature, Status: identity.ResultExact}
	if requestErr != nil {
		var responseError *jsonrpc.ResponseError
		if errors.As(requestErr, &responseError) && responseError.Code == jsonrpc.RequestFailed {
			// OmniLSP uses RequestFailed for a safe semantic refusal, such as
			// rename without a project-wide proof or no symbol at a broken
			// source position. That is the wire form of ResultUnavailable,
			// not a malformed response.
			response.Status = identity.ResultUnavailable
		} else {
			response.Err = requestErr.Error()
			response.Status = identity.ResultUnavailable
		}
	} else if len(bytes.TrimSpace(raw)) == 0 {
		response.Err = "candidate returned an empty JSON-RPC result"
		response.Status = identity.ResultUnavailable
	} else if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		// null is a legal LSP result for an unresolved symbol or a broken
		// source file. It is still a protocol-clean response.
		response.Status = identity.ResultUnknown
	} else {
		var err error
		switch feature {
		case "hover":
			response.Ranges, err = parseS21Hover(raw)
		case "definition", "references":
			response.URIs, response.Ranges, err = parseS21Locations(raw)
		case "rename":
			response.URIs, response.Ranges, response.EditNewText, err = parseS21WorkspaceEdit(raw)
		default:
			err = fmt.Errorf("unsupported S21 feature %q", feature)
		}
		if err != nil {
			response.Err = err.Error()
			response.Status = identity.ResultUnavailable
		}
	}

	explainRaw, explainErr := session.RequestContext(ctx, "omnilsp/explain", map[string]string{"uri": requestURI})
	if explainErr != nil {
		response.Err = joinS21Error(response.Err, "candidate explain request failed: "+explainErr.Error())
		return response
	}
	var explained s21ExplainResponse
	if err := json.Unmarshal(explainRaw, &explained); err != nil {
		response.Err = joinS21Error(response.Err, "decode candidate explain response: "+err.Error())
		return response
	}
	var match *s21ExplainEvidence
	for i := len(explained.Evidence) - 1; i >= 0; i-- {
		candidate := explained.Evidence[i]
		if candidate.Method == "textDocument/"+feature && sameURIIdentity(candidate.URI, requestURI) {
			match = &candidate
			break
		}
	}
	if match == nil {
		response.Err = joinS21Error(response.Err, fmt.Sprintf("candidate explain response has no %s evidence for %s", feature, requestURI))
		return response
	}
	if err := applyS21ExplainIdentity(&response, *match); err != nil {
		response.Err = joinS21Error(response.Err, err.Error())
		return response
	}
	return response
}

func applyS21ExplainIdentity(response *LSPResponse, evidence s21ExplainEvidence) error {
	if evidence.SnapshotRev == 0 || strings.TrimSpace(evidence.Backend) == "" || evidence.BackendEpoch == nil {
		return errors.New("candidate explain evidence has no snapshot revision, backend identity, or backend epoch")
	}
	response.Rev = evidence.SnapshotRev
	response.Epoch = *evidence.BackendEpoch
	return nil
}

func TestS21ExplainBackendEpochIsRequiredAndClassified(t *testing.T) {
	var missingEpoch s21ExplainEvidence
	if err := json.Unmarshal([]byte(`{"method":"textDocument/hover","uri":"file:///workspace/main.go","snapshotRev":12,"backend":"go/gopls"}`), &missingEpoch); err != nil {
		t.Fatal(err)
	}
	var missingResponse LSPResponse
	if err := applyS21ExplainIdentity(&missingResponse, missingEpoch); err == nil {
		t.Fatal("missing backendEpoch was accepted")
	}

	var first, restarted LSPResponse
	for _, test := range []struct {
		response *LSPResponse
		payload  string
	}{
		{&first, `{"snapshotRev":12,"backend":"go/gopls","backendEpoch":31}`},
		{&restarted, `{"snapshotRev":12,"backend":"go/gopls","backendEpoch":32}`},
	} {
		var evidence s21ExplainEvidence
		if err := json.Unmarshal([]byte(test.payload), &evidence); err != nil {
			t.Fatal(err)
		}
		if err := applyS21ExplainIdentity(test.response, evidence); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := first.Epoch, uint64(31); got != want {
		t.Fatalf("first response epoch = %d, want %d", got, want)
	}
	if got, want := restarted.Epoch, uint64(32); got != want {
		t.Fatalf("restarted response epoch = %d, want %d", got, want)
	}
	got := ClassifyResponse(ClassifyRequest{Rev: first.Rev, Epoch: first.Epoch}, restarted)
	if !reflect.DeepEqual(got, []string{"staleEdit", "snapshotMixing"}) {
		t.Fatalf("same-backend epoch change buckets = %v, want [staleEdit snapshotMixing]", got)
	}
}

func joinS21Error(existing, additional string) string {
	if existing == "" {
		return additional
	}
	return existing + "; " + additional
}

func parseS21Hover(raw json.RawMessage) ([]languages.Range, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("hover result is not an object")
		}
		return nil, fmt.Errorf("invalid hover result: %w", err)
	}
	if _, ok := object["contents"]; !ok {
		return nil, errors.New("invalid hover result: missing contents")
	}
	rawRange, ok := object["range"]
	if !ok || bytes.Equal(bytes.TrimSpace(rawRange), []byte("null")) {
		return nil, nil
	}
	r, err := parseS21Range(rawRange)
	if err != nil {
		return nil, fmt.Errorf("invalid hover range: %w", err)
	}
	return []languages.Range{r}, nil
}

func parseS21Locations(raw json.RawMessage) ([]string, []languages.Range, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		// textDocument/definition permits one Location as well as an array;
		// normalize that legal singleton form before validating the payload.
		var object map[string]json.RawMessage
		if objectErr := json.Unmarshal(raw, &object); objectErr != nil || object == nil {
			return nil, nil, fmt.Errorf("invalid location array: %w", err)
		}
		items = []json.RawMessage{raw}
	}
	var uris []string
	var ranges []languages.Range
	for i, item := range items {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(item, &object); err != nil || object == nil {
			if err == nil {
				err = errors.New("location is not an object")
			}
			return nil, nil, fmt.Errorf("invalid location %d: %w", i, err)
		}
		uriRaw, ok := object["uri"]
		if !ok {
			uriRaw, ok = object["targetUri"]
		}
		var locationURI string
		if !ok || json.Unmarshal(uriRaw, &locationURI) != nil || strings.TrimSpace(locationURI) == "" {
			return nil, nil, fmt.Errorf("invalid location %d: missing URI", i)
		}
		rangeRaw, ok := object["range"]
		if !ok {
			rangeRaw, ok = object["targetSelectionRange"]
		}
		if !ok {
			rangeRaw, ok = object["targetRange"]
		}
		if !ok {
			return nil, nil, fmt.Errorf("invalid location %d: missing range", i)
		}
		r, err := parseS21Range(rangeRaw)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid location %d range: %w", i, err)
		}
		uris = append(uris, locationURI)
		ranges = append(ranges, r)
	}
	return uris, ranges, nil
}

func parseS21WorkspaceEdit(raw json.RawMessage) ([]string, []languages.Range, []string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = errors.New("workspace edit is not an object")
		}
		return nil, nil, nil, fmt.Errorf("invalid workspace edit: %w", err)
	}
	var uris []string
	var ranges []languages.Range
	var newTexts []string
	if changesRaw, ok := object["changes"]; ok && !bytes.Equal(bytes.TrimSpace(changesRaw), []byte("null")) {
		var changes map[string]json.RawMessage
		if err := json.Unmarshal(changesRaw, &changes); err != nil {
			return nil, nil, nil, fmt.Errorf("invalid workspace changes: %w", err)
		}
		for locationURI, editsRaw := range changes {
			if strings.TrimSpace(locationURI) == "" {
				return nil, nil, nil, errors.New("workspace edit has an empty URI")
			}
			edits, err := parseS21TextEdits(editsRaw)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("invalid edits for %s: %w", locationURI, err)
			}
			for _, edit := range edits {
				uris = append(uris, locationURI)
				ranges = append(ranges, languages.Range{
					StartLine: edit.Range.Start.Line, StartCharacter: edit.Range.Start.Character,
					EndLine: edit.Range.End.Line, EndCharacter: edit.Range.End.Character,
				})
				newTexts = append(newTexts, edit.NewText)
			}
		}
	}
	if documentChangesRaw, ok := object["documentChanges"]; ok && !bytes.Equal(bytes.TrimSpace(documentChangesRaw), []byte("null")) {
		var documents []json.RawMessage
		if err := json.Unmarshal(documentChangesRaw, &documents); err != nil {
			return nil, nil, nil, fmt.Errorf("invalid workspace documentChanges: %w", err)
		}
		for i, documentRaw := range documents {
			var document map[string]json.RawMessage
			if err := json.Unmarshal(documentRaw, &document); err != nil || document == nil {
				if err == nil {
					err = errors.New("document change is not an object")
				}
				return nil, nil, nil, fmt.Errorf("invalid document change %d: %w", i, err)
			}
			textDocumentRaw, ok := document["textDocument"]
			if !ok {
				// Resource operations are valid WorkspaceEdit members but
				// carry no text ranges for this classifier.
				continue
			}
			var textDocument struct {
				URI string `json:"uri"`
			}
			if err := json.Unmarshal(textDocumentRaw, &textDocument); err != nil || strings.TrimSpace(textDocument.URI) == "" {
				return nil, nil, nil, fmt.Errorf("invalid document change %d text document", i)
			}
			editsRaw, ok := document["edits"]
			if !ok {
				return nil, nil, nil, fmt.Errorf("invalid document change %d edits", i)
			}
			edits, err := parseS21TextEdits(editsRaw)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("invalid document change %d edits: %w", i, err)
			}
			for _, edit := range edits {
				uris = append(uris, textDocument.URI)
				ranges = append(ranges, languages.Range{
					StartLine: edit.Range.Start.Line, StartCharacter: edit.Range.Start.Character,
					EndLine: edit.Range.End.Line, EndCharacter: edit.Range.End.Character,
				})
				newTexts = append(newTexts, edit.NewText)
			}
		}
	}
	return uris, ranges, newTexts, nil
}

func parseS21TextEdits(raw json.RawMessage) ([]s21WireTextEdit, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	edits := make([]s21WireTextEdit, 0, len(items))
	for i, item := range items {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(item, &object); err != nil || object == nil {
			if err == nil {
				err = errors.New("text edit is not an object")
			}
			return nil, fmt.Errorf("edit %d: %w", i, err)
		}
		rawRange, ok := object["range"]
		if !ok {
			return nil, fmt.Errorf("edit %d: missing range", i)
		}
		var wireRange s21WireRange
		err := json.Unmarshal(rawRange, &wireRange)
		if err != nil {
			return nil, fmt.Errorf("edit %d range: %w", i, err)
		}
		rawText, ok := object["newText"]
		if !ok {
			return nil, fmt.Errorf("edit %d: missing newText", i)
		}
		var text string
		if err := json.Unmarshal(rawText, &text); err != nil {
			return nil, fmt.Errorf("edit %d newText: %w", i, err)
		}
		edits = append(edits, s21WireTextEdit{Range: wireRange, NewText: text})
	}
	return edits, nil
}

func parseS21Range(raw json.RawMessage) (languages.Range, error) {
	var wire s21WireRange
	if err := json.Unmarshal(raw, &wire); err != nil {
		return languages.Range{}, err
	}
	return languages.Range{
		StartLine: wire.Start.Line, StartCharacter: wire.Start.Character,
		EndLine: wire.End.Line, EndCharacter: wire.End.Character,
	}, nil
}
