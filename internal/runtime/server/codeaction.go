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

	items, err := s.diag.compute(params.TextDocument.URI)
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
		// Deleting a whole line containing only the offending import is safe:
		// go/types already proved the import unused. The edit itself is a
		// workspace/edit proposal — S3 gates still govern application.
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
	ident := identAt(src, params.Position.Line, params.Position.Character)
	if ident == "" {
		return json.RawMessage("null"), nil
	}
	if lang := be.LanguageID(); lang == "go" && isExportedIdent(ident) {
		return nil, &jsonrpc.ResponseError{
			Code:    jsonrpc.InvalidRequest,
			Message: "rename refused: symbol is exported; importer packages are not loaded (SEM-SAFE-001)",
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"range": map[string]any{
			"start": map[string]any{"line": params.Position.Line, "character": params.Position.Character - uint32(len(ident))},
			"end":   map[string]any{"line": params.Position.Line, "character": params.Position.Character},
		},
		"placeholder": ident,
	})
	return payload, nil
}

// identAt extracts the identifier overlapping the given line/character using
// simple Go-ish lexical rules sufficient for placeholder reporting.
func identAt(src []byte, line, char uint32) string {
	lines := strings.Split(string(src), "\n")
	if int(line) >= len(lines) {
		return ""
	}
	l := lines[line]
	if int(char) > len(l) {
		return ""
	}
	isIdent := func(c byte) bool {
		return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
	}
	start := int(char)
	for start > 0 && isIdent(l[start-1]) {
		start--
	}
	end := int(char)
	for end < len(l) && isIdent(l[end]) {
		end++
	}
	if start == end {
		return ""
	}
	c := l[start]
	if c >= '0' && c <= '9' {
		return "" // numbers are not identifiers
	}
	return l[start:end]
}

func isExportedIdent(ident string) bool {
	return len(ident) > 0 && ident[0] >= 'A' && ident[0] <= 'Z'
}
