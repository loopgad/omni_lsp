package golang

import (
	"context"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strings"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/languages"
)

// Optional capabilities (§I16/§I20/§I22). Syntax-first with an honest
// Incomplete envelope: they upgrade only where the existing tooling already
// provides the answer for free (A3: never fabricate semantics).

// parseBest returns a (possibly partial) AST for the content. The parser
// returns a tree alongside its error list for most malformed inputs; both
// are used rather than refusing to answer.
func parseBest(fset *token.FileSet, path string, src []byte) *ast.File {
	f, _ := parser.ParseFile(fset, path, src, parser.AllErrors|parser.SkipObjectResolution)
	return f
}

// SignatureHelp answers the innermost call at the cursor from a syntax parse
// (§I16). Callee name and argument count are exact; parameter labels come
// from same-file declarations when present — hence Incomplete.
func (b *Backend) SignatureHelp(ctx context.Context, req languages.SignatureHelpRequest) (identity.SemanticResult[*languages.SignatureHelpResult], error) {
	fset := token.NewFileSet()
	file := parseBest(fset, uriToPath(req.URI), req.Content)
	if file == nil {
		return identity.SemanticResult[*languages.SignatureHelpResult]{
			Status: identity.ResultUnavailable,
		}, nil
	}
	tokFile := fset.File(file.Pos())
	if tokFile == nil || int(req.Line) >= tokFile.LineCount() {
		return identity.SemanticResult[*languages.SignatureHelpResult]{
			Status: identity.ResultUnavailable,
		}, nil
	}
	cursor := tokFile.LineStart(int(req.Line)+1) + token.Pos(req.Column)

	var call *ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		if ce, ok := n.(*ast.CallExpr); ok && ce.Lparen.IsValid() &&
			cursor >= ce.Lparen && cursor <= ce.Rparen {
			call = ce // innermost wins: DFS visits outer before inner
		}
		return true
	})
	if call == nil {
		return identity.SemanticResult[*languages.SignatureHelpResult]{
			Status: identity.ResultUnavailable,
		}, nil
	}

	sig := languages.SignatureInformation{
		Label:           fmt.Sprintf("%s(%d args)", exprName(call.Fun), len(call.Args)),
		Parameters:      sameFileParams(file, exprName(call.Fun), len(call.Args)),
		ActiveParameter: activeArg(fset, call, cursor),
	}
	return identity.SemanticResult[*languages.SignatureHelpResult]{
		Status:       identity.ResultPartial,
		Completeness: identity.IncompleteKnownSubset,
		Value:        &languages.SignatureHelpResult{Signatures: []languages.SignatureInformation{sig}},
		Evidence:     []identity.Evidence{{Kind: identity.EvidenceSyntax, Assurance: identity.AssuranceSyntax}},
	}, nil
}

// Formatting runs go/format over the whole document (§I20): one full-span
// TextEdit, empty when already canonical. Unparseable input refuses honestly.
func (b *Backend) Formatting(ctx context.Context, req languages.FormattingRequest) ([]languages.TextEdit, error) {
	src := string(req.Content)
	out, err := format.Source([]byte(src))
	if err != nil {
		return nil, fmt.Errorf("go/format: %w", err)
	}
	if string(out) == src {
		return nil, nil
	}
	lastNL := strings.LastIndexByte(src, '\n')
	endLine := uint32(strings.Count(src, "\n"))
	endCol := uint32(len(src))
	if lastNL >= 0 {
		endCol = uint32(len(src)-lastNL-1) - 1 // exclusive end at last char+1
		if endCol == 0 {
			endCol = 0
		}
	}
	return []languages.TextEdit{{
		URI:       req.URI,
		StartLine: 0, StartChar: 0,
		EndLine: endLine, EndChar: endCol,
		NewText: string(out),
	}}, nil
}

// InlayHints annotates anonymous function parameters with their positional
// index label (§I22, syntax tier).
func (b *Backend) InlayHints(ctx context.Context, req languages.InlayHintRequest) ([]languages.InlayHint, error) {
	fset := token.NewFileSet()
	file := parseBest(fset, uriToPath(req.URI), req.Content)
	if file == nil {
		return nil, nil
	}
	var hints []languages.InlayHint
	ast.Inspect(file, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Type == nil || fd.Type.Params == nil || fd.Body == nil {
			return true
		}
		pos := fset.Position(fd.Body.Pos())
		for i, p := range fd.Type.Params.List {
			if p.Names == nil {
				hints = append(hints, languages.InlayHint{
					Line:   uint32(pos.Line - 1),
					Column: uint32(pos.Column - 1),
					Label:  fmt.Sprintf("arg%d:", i),
					Kind:   "parameter",
				})
			}
		}
		return true
	})
	return hints, nil
}

// --- helpers -----------------------------------------------------------------

func exprName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return v.Sel.Name
	default:
		return "call"
	}
}

func sameFileParams(file *ast.File, name string, argc int) []string {
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name == nil || fd.Name.Name != name || fd.Type.Params == nil {
			continue
		}
		var out []string
		for _, p := range fd.Type.Params.List {
			label := "arg"
			if p.Type != nil {
				label = gofmtExpr(p.Type)
			}
			if len(p.Names) > 0 {
				label = p.Names[0].Name + " " + label
			}
			out = append(out, label)
		}
		return out
	}
	return nil // callee not declared in this file
}

func gofmtExpr(e ast.Expr) string {
	var b strings.Builder
	format.Node(&b, token.NewFileSet(), e)
	s := b.String()
	if s == "" {
		return "?"
	}
	return s
}

func activeArg(fset *token.FileSet, call *ast.CallExpr, cursor token.Pos) int {
	idx := 0
	for _, arg := range call.Args {
		if cursor > arg.End() {
			idx++
		}
	}
	return idx
}
