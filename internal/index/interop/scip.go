package interop

import (
	"fmt"
	"strings"

	"github.com/scip-code/scip/bindings/go/scip"
)

// SCIP protobuf 导入导出（goal.md §L14：SCIP 是互操作格式，非内存模型）。
// 本文件是模块内唯一允许 import github.com/scip-code/scip/bindings/go/scip 的位置。

const (
	scipToolName      = "omnilsp"
	scipProvenance    = "scip"
	scipFreshnessNote = "静态索引快照：数据可能已过时，不反映工作区当前状态（SCIP 导入）"

	commitArgPrefix       = "omnilsp.commit="
	scipRepositoryArgPrefix = "omnilsp.repository="
)

var kindToSCIP = map[Kind]scip.SymbolInformation_Kind{
	KindInterface: scip.SymbolInformation_Interface,
	KindClass:     scip.SymbolInformation_Class,
	KindFunction:  scip.SymbolInformation_Function,
	KindMethod:    scip.SymbolInformation_Method,
	KindVariable:  scip.SymbolInformation_Variable,
	KindConstant:  scip.SymbolInformation_Constant,
}

var kindFromSCIP = func() map[scip.SymbolInformation_Kind]Kind {
	m := make(map[scip.SymbolInformation_Kind]Kind, len(kindToSCIP))
	for k, v := range kindToSCIP {
		m[v] = k
	}
	return m
}()

// ExportSCIP 将 Canonical Index 映射为 SCIP Index protobuf 结构。
// CommitID 无标准字段，借 ToolInfo.Arguments 的 omnilsp.commit= 键值扩展传递。
func ExportSCIP(idx Index) (*scip.Index, error) {
	args := []string{}
	if c := idx.Metadata.CommitID; c != "" {
		args = append(args, commitArgPrefix+c)
	}
	out := &scip.Index{
		Metadata: &scip.Metadata{
			ToolInfo:             &scip.ToolInfo{Name: scipToolName, Version: idx.Metadata.ToolVersion, Arguments: args},
			ProjectRoot:          idx.Metadata.RepoIdentity,
			TextDocumentEncoding: scip.TextEncoding_UTF8,
		},
	}

	defDoc := map[string]string{} // symbol name -> defining document path
	for _, d := range idx.Documents {
		for _, occ := range d.Occurrences {
			if occ.Role == RoleDefinition {
				if _, ok := defDoc[occ.SymbolName]; !ok {
					defDoc[occ.SymbolName] = d.Path
				}
			}
		}
	}

	for _, doc := range idx.Documents {
		sd := &scip.Document{
			Language:     doc.LanguageID,
			RelativePath: doc.Path,
		}
		for _, sym := range idx.Symbols {
			if defDoc[sym.Name] != doc.Path {
				continue
			}
			si := &scip.SymbolInformation{
				Symbol:      sym.Name,
				DisplayName: sym.Name,
				Kind:        kindToSCIP[sym.Kind],
			}
			if sym.Signature != "" {
				si.Documentation = []string{sym.Signature}
			}
			sd.Symbols = append(sd.Symbols, si)
		}
		for _, occ := range doc.Occurrences {
			so := &scip.Occurrence{
				Symbol: occ.SymbolName,
				Range: []int32{
					occ.Range.StartLine, occ.Range.StartChar,
					occ.Range.EndLine, occ.Range.EndChar,
				},
			}
			if occ.Role == RoleDefinition {
				so.SymbolRoles = int32(scip.SymbolRole_Definition)
			}
			sd.Occurrences = append(sd.Occurrences, so)
		}
		out.Documents = append(out.Documents, sd)
	}
	return out, nil
}

// ImportSCIP 将 SCIP Index 映射回 Canonical IR，并标记 provenance/freshness。
func ImportSCIP(in *scip.Index) (Index, error) {
	if in == nil {
		return Index{}, fmt.Errorf("scip 导入：输入为 nil")
	}
	idx := Index{Metadata: Metadata{
		Provenance:     scipProvenance,
		FreshnessLimit: scipFreshnessNote,
	}}
	sigs := map[string]string{}
	kinds := map[string]Kind{}
	if in.Metadata != nil {
		idx.Metadata.RepoIdentity = in.Metadata.ProjectRoot
		if in.Metadata.ToolInfo != nil {
			idx.Metadata.ToolVersion = in.Metadata.ToolInfo.Version
			for _, a := range in.Metadata.ToolInfo.Arguments {
				if v, ok := strings.CutPrefix(a, commitArgPrefix); ok {
					idx.Metadata.CommitID = v
				}
				if v, ok := strings.CutPrefix(a, scipRepositoryArgPrefix); ok {
					idx.Metadata.RepoIdentity = v
				}
			}
		}
	}
	for _, d := range in.GetDocuments() {
		for _, si := range d.GetSymbols() {
			name := si.GetSymbol()
			if len(si.GetDocumentation()) > 0 {
				sigs[name] = si.GetDocumentation()[0]
			}
			if k, ok := kindFromSCIP[si.Kind]; ok && k != KindUnspecified {
				kinds[name] = k
			} else if _, seen := kinds[name]; !seen {
				kinds[name] = KindUnspecified
			}
		}
	}
	for _, d := range in.GetDocuments() {
		cd := Document{Path: d.GetRelativePath(), LanguageID: d.GetLanguage()}
		for _, occ := range d.GetOccurrences() {
			role := RoleReference
			if occ.GetSymbolRoles()&int32(scip.SymbolRole_Definition) != 0 {
				role = RoleDefinition
			}
			r := occ.GetRange()
			co := Occurrence{DocPath: cd.Path, SymbolName: occ.GetSymbol(), Role: role}
			switch len(r) {
			case 4:
				co.Range = Range{StartLine: r[0], StartChar: r[1], EndLine: r[2], EndChar: r[3]}
			case 3:
				co.Range = Range{StartLine: r[0], StartChar: r[1], EndLine: r[0], EndChar: r[2]}
			default:
				continue
			}
			cd.Occurrences = append(cd.Occurrences, co)
		}
		for _, occ := range cd.Occurrences {
			name := occ.SymbolName
			if !symbolSeen(idx.Symbols, name) {
				idx.Symbols = append(idx.Symbols, Symbol{Name: name, Kind: kinds[name], Signature: sigs[name]})
			}
		}
		idx.Documents = append(idx.Documents, cd)
	}
	return idx, nil
}

func symbolSeen(syms []Symbol, name string) bool {
	for _, s := range syms {
		if s.Name == name {
			return true
		}
	}
	return false
}
