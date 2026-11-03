package interop

import (
	"fmt"
	"sort"
	"strings"

	"github.com/omnilsp/omni/internal/index/model"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

// ConvertSCIPToLSIF 将已验证的 SCIP protobuf 流转码为 LSIF JSONL。
// provenance（projectRoot、repository/revision 扩展、tool 版本、
// omnilsp.lossy-edge.* 计数）原样转录进 metaData；出现点（符号/位置/角色）
// 全保留，SCIP SymbolInformation（显示名/kind/signature）在 LSIF 子集中
// 无编码位（见 LossyNotes）。
func ConvertSCIPToLSIF(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty input", errInvalidSCIP)
	}
	var index scip.Index
	if err := proto.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", errInvalidSCIP, err)
	}
	metadata := index.GetMetadata()
	root := metadata.GetProjectRoot()
	if root == "" {
		return nil, fmt.Errorf("%w: SCIP project root is required to derive LSIF document URIs", errInvalidSCIP)
	}
	repository, revision := "", ""
	var lossArgs []string
	if toolInfo := metadata.GetToolInfo(); toolInfo != nil {
		for _, argument := range toolInfo.GetArguments() {
			if value, ok := strings.CutPrefix(argument, scipRepositoryArgPrefix); ok {
				if repository != "" && repository != value {
					return nil, fmt.Errorf("%w: conflicting SCIP repository identity arguments", errInvalidSCIP)
				}
				repository = value
			} else if value, ok := strings.CutPrefix(argument, commitArgPrefix); ok {
				if revision != "" && revision != value {
					return nil, fmt.Errorf("%w: conflicting SCIP revision arguments", errInvalidSCIP)
				}
				revision = value
			} else if strings.HasPrefix(argument, scipLossyEdgeArgPrefix) {
				lossArgs = append(lossArgs, argument)
			}
		}
	}
	sort.Strings(lossArgs)

	input := lsifExportInput{
		projectRoot: root,
		repository:  repository,
		revision:    revision,
		toolVersion: metadata.GetToolInfo().GetVersion(),
		lossArgs:    lossArgs,
	}
	for _, doc := range index.GetDocuments() {
		uri, err := scipDocumentURI(root, doc.GetRelativePath())
		if err != nil {
			return nil, err
		}
		lsifDoc := lsifExportDoc{uri: uri, language: doc.GetLanguage()}
		for _, occurrence := range doc.GetOccurrences() {
			position, err := scipRange(occurrence)
			if err != nil {
				return nil, err
			}
			if occurrence.GetSymbol() == "" {
				return nil, fmt.Errorf("%w: occurrence without symbol", errInvalidSCIP)
			}
			lsifDoc.occs = append(lsifDoc.occs, lsifExportOcc{
				symbol:  occurrence.GetSymbol(),
				r:       positionToLSIFRange(position),
				defined: occurrence.GetSymbolRoles()&int32(scip.SymbolRole_Definition) != 0,
			})
		}
		input.docs = append(input.docs, lsifDoc)
	}
	return emitLSIF(input)
}

// ConvertLSIFToSCIP 将 LSIF JSONL 流转码为 SCIP protobuf 流（UTF-16 编码声明，
// 与 LSIF 位置语义一致）。文档 URI 经 scope root（metaData.projectRoot）
// 校验并转为相对路径；身份扩展转录为 ToolInfo.Arguments。
func ConvertLSIFToSCIP(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty input", errInvalidLSIF)
	}
	parsed, err := parseLSIFStream(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidLSIF, err)
	}
	meta, ok := parsed.lsifMetaData()
	if !ok {
		return nil, fmt.Errorf("%w: LSIF metaData vertex is required", errInvalidLSIF)
	}
	root := meta.Root
	if root == "" {
		return nil, fmt.Errorf("%w: LSIF projectRoot is required to derive SCIP relative paths", errInvalidLSIF)
	}
	repository, revision := "", ""
	if meta.Tool != nil {
		for _, argument := range meta.Tool.Args {
			if value, ok := strings.CutPrefix(argument, scipRepositoryArgPrefix); ok {
				if repository != "" && repository != value {
					return nil, fmt.Errorf("%w: conflicting LSIF repository identity arguments", errInvalidLSIF)
				}
				repository = value
			}
			if value, ok := strings.CutPrefix(argument, commitArgPrefix); ok {
				if revision != "" && revision != value {
					return nil, fmt.Errorf("%w: conflicting LSIF revision arguments", errInvalidLSIF)
				}
				revision = value
			}
		}
	}
	if meta.Commit != "" {
		if revision != "" && revision != meta.Commit {
			return nil, fmt.Errorf("%w: conflicting LSIF revision between commitId and toolInfo arguments", errInvalidLSIF)
		}
		revision = meta.Commit
	}

	// The converted index must declare the encoding its columns actually use.
	// The conversion carries columns verbatim (it has no source access to
	// re-encode them), so the declaration has to follow the stream's
	// positionEncoding instead of the old hard-coded UTF-16: a utf-8 stream
	// previously converted with a UTF-16 declaration, silently mis-decoding
	// every position after a multi-byte character downstream. UTF-8 streams
	// re-import through ImportSCIPToModel, which converts UTF-8 columns on
	// the way in. A utf-32 stream can neither keep an honest declaration
	// (the SCIP TextEncoding enum has no UTF-32) nor be re-encoded without
	// source content, so it is rejected; the model import path
	// (ImportLSIFToModel) does have source access and converts utf-32 there.
	// An undeclared or unknown encoding is rejected as well.
	var encoding scip.TextEncoding
	switch meta.PositionEncoding {
	case "utf-16":
		encoding = scip.TextEncoding_UTF16
	case "utf-8":
		encoding = scip.TextEncoding_UTF8
	default:
		return nil, fmt.Errorf("%w: LSIF positionEncoding %q cannot be carried into SCIP without source access", errInvalidLSIF, meta.PositionEncoding)
	}
	index := &scip.Index{Metadata: &scip.Metadata{
		ProjectRoot:          root,
		TextDocumentEncoding: encoding,
		ToolInfo:             &scip.ToolInfo{Name: scipToolName, Version: toolVersion(meta)},
	}}
	if repository != "" {
		index.Metadata.ToolInfo.Arguments = append(index.Metadata.ToolInfo.Arguments, scipRepositoryArgPrefix+repository)
	}
	if revision != "" {
		index.Metadata.ToolInfo.Arguments = append(index.Metadata.ToolInfo.Arguments, commitArgPrefix+revision)
	}

	rangeRole := parsed.lsifRangeRoles()
	for _, id := range parsed.lsifVertexIDsAscending() {
		vertex := parsed.vertices[id]
		if vertex.Label != "document" {
			continue
		}
		relative, err := modelURIToSCIPPath(root, vertex.URI)
		if err != nil {
			return nil, err
		}
		sd := &scip.Document{Language: vertex.Lang, RelativePath: relative}
		for _, rid := range parsed.docRangeIDs[id] {
			rv, ok := parsed.vertices[rid]
			if !ok || rv.Label != "range" || rv.Start == nil || rv.End == nil {
				return nil, fmt.Errorf("%w: document %q references missing or malformed range %d", errInvalidLSIF, vertex.URI, rid)
			}
			symbol := ""
			if rsID, ok := parsed.nextRangeToRS[rid]; ok {
				symbol = parsed.vertices[rsID].Symbol
			}
			if symbol == "" {
				return nil, fmt.Errorf("%w: occurrence without symbol (range %d)", errInvalidLSIF, rid)
			}
			role, ok := rangeRole[rid]
			if !ok {
				return nil, fmt.Errorf("%w: range %d has no item edge", errInvalidLSIF, rid)
			}
			if rv.Start.Line < 0 || rv.Start.Character < 0 || rv.End.Line < rv.Start.Line ||
				rv.End.Character < 0 || (rv.End.Line == rv.Start.Line && rv.End.Character < rv.Start.Character) {
				return nil, fmt.Errorf("%w: invalid occurrence range", errInvalidLSIF)
			}
			wireOccurrence := &scip.Occurrence{
				Symbol: symbol,
				Range:  []int32{rv.Start.Line, rv.Start.Character, rv.End.Line, rv.End.Character},
			}
			if role == RoleDefinition {
				wireOccurrence.SymbolRoles = int32(scip.SymbolRole_Definition)
			}
			sd.Occurrences = append(sd.Occurrences, wireOccurrence)
		}
		sort.Slice(sd.Occurrences, func(i, j int) bool {
			a, b := sd.Occurrences[i], sd.Occurrences[j]
			if a.GetRange()[0] != b.GetRange()[0] {
				return a.GetRange()[0] < b.GetRange()[0]
			}
			if a.GetRange()[1] != b.GetRange()[1] {
				return a.GetRange()[1] < b.GetRange()[1]
			}
			return a.GetSymbol() < b.GetSymbol()
		})
		index.Documents = append(index.Documents, sd)
	}
	sort.Slice(index.Documents, func(i, j int) bool {
		return index.Documents[i].GetRelativePath() < index.Documents[j].GetRelativePath()
	})
	data, err = (proto.MarshalOptions{Deterministic: true}).Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("%w: encode SCIP: %v", errInvalidLSIF, err)
	}
	return data, nil
}

func toolVersion(meta lsifNodeRaw) string {
	if meta.Tool != nil {
		return meta.Tool.Version
	}
	return ""
}

// positionToLSIFRange 把已校验的模型位置转为 Canonical Range（纯字段搬运）。
func positionToLSIFRange(position model.Position) Range {
	return Range{
		StartLine: int32(position.StartLine),
		StartChar: int32(position.StartChar),
		EndLine:   int32(position.EndLine),
		EndChar:   int32(position.EndChar),
	}
}
