package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// §I19 code actions: three kinds with distinct safety classes.
//   - non-mutating (explanation/navigation): always allowed
//   - mutating edits: S3 rename-grade gates apply at execution time; the
//     action only carries the title/kind here
//   - executable commands: §S4 trust — v1 exposes none, so a command action
//     can never hide execution behind an innocent title

type codeActionParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
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
	Context struct {
		Diagnostics []map[string]any `json:"diagnostics"`
	} `json:"context"`
}

// handleCodeAction serves textDocument/codeAction. The golang bridge's syntax
// tier offers quickfix actions derived from diagnostics in range; every
// emitted action is non-mutating-explanation or edit-carrying, never a
// disguised command.
func (s *Server) handleCodeAction(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params codeActionParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid codeAction params: %w", err)
	}

	items, err := s.diag.compute(ctx, params.TextDocument.URI)
	if err != nil {
		items = nil // degraded: answer empty rather than stale data (§Q4)
	}
	out := []map[string]any{}
	for _, it := range items {
		if !diagInRange(it, params.Range.Start.Line, params.Range.End.Line) {
			continue
		}
		switch fix := quickfixFor(it, params.TextDocument.URI); {
		case fix != nil:
			out = append(out, map[string]any{
				"title":       fix.Title,
				"kind":        "quickfix",
				"diagnostics": []map[string]any{projectDiagnostics([]languages.Diagnostic{it})[0]},
				"edit":        fix.Edit,
			})
		default:
			out = append(out, map[string]any{
				"title": fmt.Sprintf("About this diagnostic: %s", it.Message),
				"kind":  "quickfix",
				"data": map[string]any{
					"explanationOnly": true,
					"source":          it.Source,
					"code":            it.Code,
				},
			})
		}
	}
	return json.Marshal(out)
}

func diagInRange(it languages.Diagnostic, startLine, endLine uint32) bool {
	return it.EndLine >= startLine && it.StartLine <= endLine
}

// quickfix is a mutating edit proposal (S3 rules govern its application).
type quickfix struct {
	Title string
	Edit  map[string]any
}

// quickfixFor maps known diagnostic codes to concrete fixes. Only codes whose
// fix is provably safe at syntax tier get an edit; everything else falls back
// to an explanation-only action (never a fake one-click repair).
func quickfixFor(it languages.Diagnostic, uri string) *quickfix {
	switch it.Code {
	case "unused-import", "imported-and-not-used":
		// Removing the content of a line that carries only the offending
		// import is safe: go/types already proved the import unused. The line
		// terminator stays, so applying the edit leaves a blank line. The edit
		// is an edit proposal — S3 gates still govern application.
		return &quickfix{
			Title: "Remove unused import",
			Edit: map[string]any{
				"type":         "edit",
				"range":        map[string]any{"start": map[string]any{"line": it.StartLine, "character": it.StartChar}, "end": map[string]any{"line": it.EndLine, "character": it.EndChar}},
				"textDocument": map[string]any{"uri": uri},
				"newText":      "",
			},
		}
	default:
		return nil
	}
}

// handlePrepareRename serves textDocument/prepareRename (§I10 step one):
// report whether the position is on a renamable symbol and the range that
// would be replaced. v1 syntax-tier policy mirrors the bridge's own gate —
// exported Go identifiers are refused (SEM-SAFE-001), everything else gets a
// placeholder equal to the identifier at the position.
func (s *Server) handlePrepareRename(ctx context.Context, msg *jsonrpc.Message) (json.RawMessage, error) {
	var params HoverParams // same position-only shape
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, fmt.Errorf("invalid prepareRename params: %w", err)
	}
	be, err := s.resolveBackend(params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	src := s.vfs.Content(params.TextDocument.URI)
	startChar, endChar, ident := identSpanAt(src, params.Position.Line, params.Position.Character, s.negotiatedEncodingInt())
	if ident == "" {
		return json.RawMessage("null"), nil
	}
	if lang := be.LanguageID(); lang == "go" && isExportedIdent(ident) {
		return nil, &jsonrpc.ResponseError{
			Code:    jsonrpc.RequestFailed,
			Message: "rename refused: symbol is exported; importer packages are not loaded (SEM-SAFE-001)",
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"range": map[string]any{
			"start": map[string]any{"line": params.Position.Line, "character": startChar},
			"end":   map[string]any{"line": params.Position.Line, "character": endChar},
		},
		"placeholder": ident,
	})
	return payload, nil
}

// identSpanAt extracts the identifier overlapping the given line/character and
// reports its span projected into the negotiated encoding's columns: start
// (inclusive) and end (exclusive) on the identifier's line. The scan itself is
// byte-based (§I10 syntax tier is ASCII-identifier shaped); the byte span is
// then converted with encodingWidth, so the reported range always bounds the
// identifier in the encoding the client negotiated — never "cursor column
// minus byte length", which underflows uint32 whenever the cursor is not at
// the identifier's end and mismatches UTF-16 on non-ASCII lines.
func identSpanAt(src []byte, line, char uint32, encoding int) (start, end uint32, ident string) {
	lines := strings.Split(string(src), "\n")
	if int(line) >= len(lines) {
		return 0, 0, ""
	}
	l := lines[line]
	if int(char) > len(l) {
		return 0, 0, ""
	}
	isIdent := func(c byte) bool {
		return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
	}
	bStart := int(char)
	for bStart > 0 && isIdent(l[bStart-1]) {
		bStart--
	}
	bEnd := int(char)
	for bEnd < len(l) && isIdent(l[bEnd]) {
		bEnd++
	}
	if bStart == bEnd {
		return 0, 0, ""
	}
	if c := l[bStart]; c >= '0' && c <= '9' {
		return 0, 0, "" // numbers are not identifiers
	}
	start = encodingWidth(l[:bStart], encoding)
	end = start + encodingWidth(l[bStart:bEnd], encoding)
	return start, end, l[bStart:bEnd]
}

func isExportedIdent(ident string) bool {
	return len(ident) > 0 && ident[0] >= 'A' && ident[0] <= 'Z'
}
