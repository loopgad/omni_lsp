package interop

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/workspace/position"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

var errInvalidSCIP = errors.New("interop: invalid SCIP semantic index")

// ImportSCIPToModel converts a SCIP protobuf stream into the format-neutral
// semantic model. It deliberately reports unsupported SCIP fact families as
// incomplete/unknown rather than treating the format's omissions as negatives.
func ImportSCIPToModel(ctx context.Context, data []byte, req model.Request, sink model.Sink) (model.Report, error) {
	if sink == nil || req.View == nil || len(req.Scopes) != 1 {
		return model.Report{}, fmt.Errorf("%w: expected one scope, immutable view, and sink", errInvalidSCIP)
	}
	if len(data) == 0 {
		return model.Report{}, fmt.Errorf("%w: empty input", errInvalidSCIP)
	}
	var index scip.Index
	if err := proto.Unmarshal(data, &index); err != nil {
		return model.Report{}, fmt.Errorf("%w: decode: %v", errInvalidSCIP, err)
	}
	if index.Metadata == nil || index.Metadata.TextDocumentEncoding == scip.TextEncoding_UnspecifiedTextEncoding {
		return model.Report{}, fmt.Errorf("%w: missing text document encoding", errInvalidSCIP)
	}
	if index.Metadata.TextDocumentEncoding != scip.TextEncoding_UTF8 && index.Metadata.TextDocumentEncoding != scip.TextEncoding_UTF16 {
		return model.Report{}, fmt.Errorf("%w: unsupported text document encoding %s", errInvalidSCIP, index.Metadata.TextDocumentEncoding)
	}
	scope := req.Scopes[0]
	if err := validateSCIPSourceIdentity(index.Metadata, req.View.Identity(), scope.RootURI); err != nil {
		return model.Report{}, err
	}
	prov, ok := req.Provenance[scope.ID]
	if !ok || !reflect.DeepEqual(prov.Scope, scope) || prov.Identity != req.View.Identity() {
		return model.Report{}, fmt.Errorf("%w: request provenance mismatch", errInvalidSCIP)
	}
	if err := ctx.Err(); err != nil {
		return model.Report{}, err
	}
	files := make(map[string]model.File)
	if err := req.View.Walk(ctx, scope.RootURI, func(file model.File) error {
		if file.URI == "" || file.SHA256 == "" {
			return fmt.Errorf("%w: snapshot file missing URI or content hash", errInvalidSCIP)
		}
		if _, exists := files[file.URI]; exists {
			return fmt.Errorf("%w: duplicate snapshot URI %s", errInvalidSCIP, file.URI)
		}
		files[file.URI] = file
		return nil
	}); err != nil {
		return model.Report{}, err
	}
	if err := ctx.Err(); err != nil {
		return model.Report{}, err
	}

	symbols := make(map[identity.SymbolID]model.Symbol)
	for _, info := range index.ExternalSymbols {
		if err := addSCIPSymbol(symbols, scope.ID, info); err != nil {
			return model.Report{}, err
		}
	}
	edges := make([]model.Edge, 0, 2048)
	for _, doc := range index.Documents {
		for _, info := range doc.Symbols {
			if err := addSCIPSymbol(symbols, scope.ID, info); err != nil {
				return model.Report{}, err
			}
		}
	}

	report := model.Report{Identity: req.View.Identity(), UsedTools: map[string][]model.ToolIdentity{scope.ID: append([]model.ToolIdentity(nil), prov.Tools...)}}
	var emittedSymbols []model.Symbol
	symbolIDs := make([]string, 0, len(symbols))
	for id := range symbols {
		symbolIDs = append(symbolIDs, string(id))
	}
	sort.Strings(symbolIDs)
	for _, id := range symbolIDs {
		emittedSymbols = append(emittedSymbols, symbols[identity.SymbolID(id)])
		if len(emittedSymbols) == 2048 {
			if err := sink.WriteSymbols(ctx, emittedSymbols); err != nil {
				return model.Report{}, err
			}
			emittedSymbols = emittedSymbols[:0]
		}
	}
	if len(emittedSymbols) > 0 {
		if err := sink.WriteSymbols(ctx, emittedSymbols); err != nil {
			return model.Report{}, err
		}
	}

	for _, doc := range index.Documents {
		if err := ctx.Err(); err != nil {
			return model.Report{}, err
		}
		uri, err := scipDocumentURI(scope.RootURI, doc.RelativePath)
		if err != nil {
			return model.Report{}, err
		}
		file, exists := files[uri]
		if !exists {
			return model.Report{}, fmt.Errorf("%w: SCIP document is outside the captured snapshot: %s", errInvalidSCIP, uri)
		}
		if !scipLanguageMatches(scope.Language, doc.GetLanguage()) {
			return model.Report{}, fmt.Errorf("%w: SCIP document language %q does not match scope language %q", errInvalidSCIP, doc.GetLanguage(), scope.Language)
		}
		ranges := make([]model.Position, len(doc.Occurrences))
		for i, occurrence := range doc.Occurrences {
			ranges[i], err = scipRange(occurrence)
			if err != nil {
				return model.Report{}, err
			}
		}
		if index.Metadata.TextDocumentEncoding == scip.TextEncoding_UTF8 {
			reader, readErr := req.View.Read(ctx, uri)
			if readErr != nil {
				return model.Report{}, readErr
			}
			ranges, err = utf8RangesToUTF16(ctx, reader, ranges)
			closeErr := reader.Close()
			if err != nil {
				return model.Report{}, err
			}
			if closeErr != nil {
				return model.Report{}, closeErr
			}
		} else {
			reader, readErr := req.View.Read(ctx, uri)
			if readErr != nil {
				return model.Report{}, readErr
			}
			validateErr := validateUTF16Ranges(ctx, reader, ranges)
			closeErr := reader.Close()
			if validateErr != nil {
				return model.Report{}, validateErr
			}
			if closeErr != nil {
				return model.Report{}, closeErr
			}
		}
		batch := make([]model.Occurrence, 0, 2048)
		for i, occurrence := range doc.Occurrences {
			if occurrence.Symbol == "" {
				return model.Report{}, fmt.Errorf("%w: occurrence without symbol", errInvalidSCIP)
			}
			id := identity.SymbolID(occurrence.Symbol)
			if _, ok := symbols[id]; !ok {
				// Preserve the authoritative SCIP identity without fabricating a
				// display name or symbol kind for external/unresolved symbols.
				symbols[id] = model.Symbol{ID: id, ScopeID: scope.ID}
				if err := sink.WriteSymbols(ctx, []model.Symbol{symbols[id]}); err != nil {
					return model.Report{}, err
				}
			}
			role := "reference"
			if occurrence.SymbolRoles&int32(scip.SymbolRole_Definition) != 0 {
				role = "definition"
			}
			batch = append(batch, model.Occurrence{
				SymbolID: id, ScopeID: scope.ID, URI: uri, Range: ranges[i], Role: role,
				SourceHash: file.SHA256, BuildContext: scope.BuildContext,
			})
			if len(batch) == cap(batch) {
				if err := sink.WriteOccurrences(ctx, batch); err != nil {
					return model.Report{}, err
				}
				batch = make([]model.Occurrence, 0, 2048)
			}
		}
		if len(batch) > 0 {
			if err := sink.WriteOccurrences(ctx, batch); err != nil {
				return model.Report{}, err
			}
		}
		for _, info := range doc.Symbols {
			for _, relationship := range info.Relationships {
				kind := model.EdgeKind("")
				switch {
				case relationship.GetIsImplementation():
					kind = model.EdgeImplementation
				case relationship.GetIsTypeDefinition():
					kind = model.EdgeTypeRelation
				}
				if kind == "" || relationship.GetSymbol() == "" || info.GetSymbol() == "" {
					continue
				}
				edge := model.Edge{
					From: identity.SymbolID(info.GetSymbol()), To: identity.SymbolID(relationship.GetSymbol()),
					ScopeID: scope.ID, Kind: kind, SourceURI: uri, SourceHash: file.SHA256,
					BuildContext: scope.BuildContext,
				}
				edges = append(edges, edge)
				if len(edges) == cap(edges) {
					if err := sink.WriteEdges(ctx, edges); err != nil {
						return model.Report{}, err
					}
					edges = make([]model.Edge, 0, 2048)
				}
			}
		}
	}
	if len(edges) > 0 {
		if err := sink.WriteEdges(ctx, edges); err != nil {
			return model.Report{}, err
		}
	}

	for _, fact := range model.RequiredFactKinds {
		state, reason := model.Unknown, "SCIP does not encode this fact family with sufficient completeness evidence"
		switch fact {
		case model.FactSymbol, model.FactDefinition, model.FactReference, model.FactImplementation, model.FactTypeRelation:
			state, reason = model.IncompleteKnownSubset, "SCIP facts were imported, but format coverage does not prove completeness for the requested scope"
		case model.FactDeclaration:
			reason = "SCIP occurrence roles do not distinguish declaration completeness from definitions"
		}
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: state, Reason: reason})
	}
	if err := model.ValidateReport(req, report); err != nil {
		return model.Report{}, err
	}
	return report, nil
}

func scipLanguageMatches(scopeLanguage, documentLanguage string) bool {
	if scopeLanguage == "" || documentLanguage == "" {
		return false
	}
	scopeFamily := scipLanguageFamily(scopeLanguage)
	documentFamily := scipLanguageFamily(documentLanguage)
	return scopeFamily != "" && scopeFamily == documentFamily
}

func scipLanguageFamily(language string) string {
	switch strings.ToLower(strings.TrimSpace(language)) {
	case "c", "cpp", "c++":
		return "c-family"
	case "javascript", "javascriptreact", "typescript", "typescriptreact":
		return "ts-js-family"
	case "go", "python", "rust":
		return strings.ToLower(strings.TrimSpace(language))
	default:
		return ""
	}
}

func addSCIPSymbol(dst map[identity.SymbolID]model.Symbol, scopeID string, info *scip.SymbolInformation) error {
	if info == nil || info.GetSymbol() == "" {
		return fmt.Errorf("%w: symbol information without identity", errInvalidSCIP)
	}
	id := identity.SymbolID(info.GetSymbol())
	signature := ""
	if info.GetSignatureDocumentation() != nil {
		signature = info.GetSignatureDocumentation().GetText()
	} else if documentation := info.GetDocumentation(); len(documentation) > 0 {
		signature = documentation[0]
	}
	kind := modelKindFromSCIP(info.GetKind())
	candidate := model.Symbol{ID: id, ScopeID: scopeID, Name: info.GetDisplayName(), Kind: kind, Signature: signature}
	if previous, exists := dst[id]; exists {
		if previous.Name != candidate.Name || previous.Kind != candidate.Kind || previous.Signature != candidate.Signature {
			return fmt.Errorf("%w: conflicting symbol records for %q", errInvalidSCIP, id)
		}
		return nil
	}
	dst[id] = candidate
	return nil
}

func modelKindFromSCIP(kind scip.SymbolInformation_Kind) string {
	switch kind {
	case scip.SymbolInformation_Interface:
		return "interface"
	case scip.SymbolInformation_Class:
		return "class"
	case scip.SymbolInformation_Function:
		return "function"
	case scip.SymbolInformation_Method:
		return "method"
	case scip.SymbolInformation_Variable:
		return "variable"
	case scip.SymbolInformation_Constant:
		return "constant"
	default:
		return ""
	}
}

func scipDocumentURI(rootURI, relative string) (string, error) {
	relative = strings.ReplaceAll(relative, "\\", "/")
	if relative == "" || len(relative) > maxSCIPRelativePathBytes || path.IsAbs(relative) || path.Clean(relative) != relative || relative == "." || strings.HasPrefix(relative, "../") || strings.Contains(relative, ":") {
		return "", fmt.Errorf("%w: unsafe SCIP relative path %q", errInvalidSCIP, relative)
	}
	root, err := url.Parse(rootURI)
	if err != nil || root.Scheme != "file" || root.Opaque != "" || root.RawQuery != "" || root.Fragment != "" || root.User != nil || root.RawPath != "" || path.Clean(root.Path) != root.Path {
		return "", fmt.Errorf("%w: invalid workspace URI %q", errInvalidSCIP, rootURI)
	}
	root.Path = path.Join(root.Path, relative)
	root.RawPath = ""
	return root.String(), nil
}

func validateSCIPSourceIdentity(metadata *scip.Metadata, current model.Identity, rootURI string) error {
	if metadata == nil {
		return fmt.Errorf("%w: missing SCIP metadata", errInvalidSCIP)
	}
	projectRoot := metadata.GetProjectRoot()
	if projectRoot == "" {
		return fmt.Errorf("%w: SCIP project root is required to bind documents to the captured workspace", errInvalidSCIP)
	}
	if projectRoot != rootURI && (current.Repository == "" || projectRoot != string(current.Repository)) {
		return fmt.Errorf("%w: SCIP project root does not match the captured repository", errInvalidSCIP)
	}
	var repository, revision string
	for _, argument := range metadata.GetToolInfo().GetArguments() {
		if value, ok := strings.CutPrefix(argument, scipRepositoryArgPrefix); ok {
			if repository != "" && repository != value {
				return fmt.Errorf("%w: conflicting SCIP repository identity arguments", errInvalidSCIP)
			}
			repository = value
		}
		if value, ok := strings.CutPrefix(argument, commitArgPrefix); ok {
			if revision != "" && revision != value {
				return fmt.Errorf("%w: conflicting SCIP revision arguments", errInvalidSCIP)
			}
			revision = value
		}
	}
	if repository != "" && current.Repository != "" && repository != string(current.Repository) {
		return fmt.Errorf("%w: SCIP repository identity does not match the captured snapshot", errInvalidSCIP)
	}
	if revision != "" && current.Revision != "" && revision != current.Revision {
		return fmt.Errorf("%w: SCIP revision does not match the captured snapshot", errInvalidSCIP)
	}
	return nil
}

func scipRange(occurrence *scip.Occurrence) (model.Position, error) {
	if occurrence == nil {
		return model.Position{}, fmt.Errorf("%w: nil occurrence", errInvalidSCIP)
	}
	r := occurrence.GetRange()
	if typed := occurrence.GetSingleLineRange(); typed != nil {
		r = []int32{typed.GetLine(), typed.GetStartCharacter(), typed.GetEndCharacter()}
	} else if typed := occurrence.GetMultiLineRange(); typed != nil {
		r = []int32{typed.GetStartLine(), typed.GetStartCharacter(), typed.GetEndLine(), typed.GetEndCharacter()}
	}
	var startLine, startChar, endLine, endChar int32
	switch len(r) {
	case 3:
		startLine, startChar, endLine, endChar = r[0], r[1], r[0], r[2]
	case 4:
		startLine, startChar, endLine, endChar = r[0], r[1], r[2], r[3]
	default:
		return model.Position{}, fmt.Errorf("%w: invalid occurrence range length %d", errInvalidSCIP, len(r))
	}
	if startLine < 0 || startChar < 0 || endLine < startLine || endChar < 0 || (endLine == startLine && endChar < startChar) {
		return model.Position{}, fmt.Errorf("%w: invalid occurrence range", errInvalidSCIP)
	}
	return model.Position{StartLine: uint32(startLine), StartChar: uint32(startChar), EndLine: uint32(endLine), EndChar: uint32(endChar)}, nil
}

func utf8RangesToUTF16(ctx context.Context, reader io.Reader, ranges []model.Position) ([]model.Position, error) {
	wanted := make(map[uint32]map[uint32]struct{})
	for _, r := range ranges {
		for _, p := range []struct{ line, column uint32 }{{r.StartLine, r.StartChar}, {r.EndLine, r.EndChar}} {
			if wanted[p.line] == nil {
				wanted[p.line] = make(map[uint32]struct{})
			}
			wanted[p.line][p.column] = struct{}{}
		}
	}
	lines := make([]uint32, 0, len(wanted))
	for line := range wanted {
		lines = append(lines, line)
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i] < lines[j] })
	converted := make(map[uint32]map[uint32]uint32, len(wanted))
	br := bufio.NewReader(reader)
	var lineNo uint32
	lastLineTerminated := false
	seenLine := false
	for _, target := range lines {
		for lineNo <= target {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			line, lineTerminated, err := readSemanticSourceLine(br)
			if errors.Is(err, io.EOF) && len(line) == 0 {
				if target == lineNo && onlyZeroColumn(wanted[target]) &&
					((target == 0 && !seenLine) || (target > 0 && lastLineTerminated)) {
					converted[target] = map[uint32]uint32{0: 0}
					lineNo++
					break
				}
				return nil, fmt.Errorf("%w: source line %d unavailable for UTF-8 range conversion", errInvalidSCIP, target)
			}
			if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
				return nil, fmt.Errorf("%w: source line %d unavailable for UTF-8 range conversion", errInvalidSCIP, target)
			}
			if lineNo == target {
				converted[target] = make(map[uint32]uint32, len(wanted[target]))
				columns := make([]uint32, 0, len(wanted[target]))
				for column := range wanted[target] {
					columns = append(columns, column)
				}
				sort.Slice(columns, func(i, j int) bool { return columns[i] < columns[j] })
				for _, column := range columns {
					value, err := utf8ByteColumnToUTF16(line, column)
					if err != nil {
						return nil, fmt.Errorf("%w: line %d column %d: %v", errInvalidSCIP, target, column, err)
					}
					converted[target][column] = value
				}
			}
			seenLine = true
			lastLineTerminated = lineTerminated
			lineNo++
		}
	}
	out := append([]model.Position(nil), ranges...)
	for i := range out {
		out[i].StartChar = converted[out[i].StartLine][out[i].StartChar]
		out[i].EndChar = converted[out[i].EndLine][out[i].EndChar]
	}
	return out, nil
}

func validateUTF16Ranges(ctx context.Context, reader io.Reader, ranges []model.Position) error {
	wanted := make(map[uint32]map[uint32]struct{})
	for _, r := range ranges {
		for _, p := range []struct{ line, column uint32 }{{r.StartLine, r.StartChar}, {r.EndLine, r.EndChar}} {
			if wanted[p.line] == nil {
				wanted[p.line] = make(map[uint32]struct{})
			}
			wanted[p.line][p.column] = struct{}{}
		}
	}
	lines := make([]uint32, 0, len(wanted))
	for line := range wanted {
		lines = append(lines, line)
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i] < lines[j] })
	br := bufio.NewReader(reader)
	var lineNo uint32
	lastLineTerminated := false
	seenLine := false
	for _, target := range lines {
		for lineNo <= target {
			if err := ctx.Err(); err != nil {
				return err
			}
			line, lineTerminated, err := readSemanticSourceLine(br)
			if errors.Is(err, io.EOF) && len(line) == 0 {
				if target == lineNo && onlyZeroColumn(wanted[target]) &&
					((target == 0 && !seenLine) || (target > 0 && lastLineTerminated)) {
					lineNo++
					break
				}
				return fmt.Errorf("%w: source line %d unavailable for UTF-16 range validation", errInvalidSCIP, target)
			}
			if err != nil && !(errors.Is(err, io.EOF) && len(line) > 0) {
				return fmt.Errorf("%w: source line %d unavailable for UTF-16 range validation", errInvalidSCIP, target)
			}
			if lineNo == target {
				columns := make([]uint32, 0, len(wanted[target]))
				for column := range wanted[target] {
					columns = append(columns, column)
				}
				sort.Slice(columns, func(i, j int) bool { return columns[i] < columns[j] })
				for _, column := range columns {
					if _, err := position.OffsetOfLineCharEncoding(line, 0, column, position.UTF16); err != nil {
						return fmt.Errorf("%w: line %d UTF-16 column %d: %v", errInvalidSCIP, target, column, err)
					}
				}
			}
			seenLine = true
			lastLineTerminated = lineTerminated
			lineNo++
		}
	}
	return nil
}

func readSemanticSourceLine(reader *bufio.Reader) ([]byte, bool, error) {
	line := make([]byte, 0, 128)
	for {
		value, err := reader.ReadByte()
		if err != nil {
			return line, false, err
		}
		switch value {
		case '\n':
			return line, true, nil
		case '\r':
			if next, peekErr := reader.Peek(1); peekErr == nil && next[0] == '\n' {
				_, _ = reader.ReadByte()
			}
			return line, true, nil
		default:
			line = append(line, value)
		}
	}
}

func onlyZeroColumn(columns map[uint32]struct{}) bool {
	if len(columns) != 1 {
		return false
	}
	_, ok := columns[0]
	return ok
}

func utf8ByteColumnToUTF16(line []byte, byteColumn uint32) (uint32, error) {
	if uint64(byteColumn) > uint64(len(line)) {
		return 0, fmt.Errorf("byte column exceeds line length")
	}
	end := int(byteColumn)
	if !utf8.Valid(line[:end]) {
		return 0, fmt.Errorf("byte column splits a UTF-8 code point")
	}
	units := uint32(0)
	for len(line[:end]) > 0 {
		r, size := utf8.DecodeRune(line[:end])
		if r == utf8.RuneError && size == 1 {
			units++
		} else if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
		line = line[size:]
		end -= size
	}
	return units, nil
}
