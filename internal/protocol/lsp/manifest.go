// manifest.go 提供 protocol.manifest（goal.md §U4 可复现检查，
// 对应的是本仓库 registry 条目 Y5-4，它不是 goal.md 的章节号）的
// 构建与校验：对手写类型文件 types.go 的全部顶层声明做规范化文本指纹，
// 供 scripts/gen-protocol.go 与 internal/conformance 探针共同调用。
// 不引入真实 codegen——指纹一致即视为协议面未被意外改动。
package lsp

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ManifestRelPath 是 manifest 相对模块根的斜杠路径，脚本与测试共用。
const ManifestRelPath = "internal/protocol/lsp/protocol.manifest"

// BuildManifest 解析与本文件同目录的 types.go，输出确定性 manifest 文本：
// 每条顶层声明一行、按整行字典序排序、无时间戳、无任何 map 迭代序，
// 因此同一份 types.go 字节级可复现。
// manifestSources 列入指纹的协议面文件（§C17 扩面）：除 protocol/lsp 的
// types.go 基线外，server 投影层的请求/响应结构同样是客户端可见的线协议，
// 意外漂移必须同样可复现地暴露。
var manifestSources = []string{"types.go"}

func BuildManifest() (string, error) {
	root, rerr := moduleRoot()
	if rerr != nil {
		return "", rerr
	}
	return buildManifestAt(filepath.Join(root, filepath.FromSlash("internal/protocol/lsp")), root)
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	for err == nil {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("module root not found")
}

func buildManifestAt(lspDir, root string) (string, error) {
	var lines []string
	for _, name := range manifestSources {
		src := filepath.Join(lspDir, name)
		fileLines, err := manifestLinesFor(src)
		if err != nil {
			return "", err
		}
		lines = append(lines, fileLines...)
	}
	serverLines, serr := BuildServerManifest(root)
	if serr != nil {
		return "", serr
	}
	lines = append(lines, splitLines(serverLines)...)
	methodLines, merr := BuildMethodManifest(root)
	if merr != nil {
		return "", merr
	}
	lines = append(lines, methodLines...)
	sort.Strings(lines)
	var b strings.Builder
	b.WriteString("# protocol.manifest —— 协议投影层（LSP 3.17 基线 + server 线类型）的规范声明指纹。\n")
	b.WriteString("# 由 scripts/gen-protocol.go 维护；漂移时运行 go run scripts/gen-protocol.go 重写。\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// methodManifestSources 相对模块根，只贡献「已注册的 LSP 方法名」行，不贡献
// 任何声明行。方法名是客户端可见的那一半（§C17 要求 pin 具体的协议修订），
// 而 manifestLinesFor 只看顶层签名、不进函数体，所以把 server.go 整体加进
// serverManifestSources 只能多抓到签名漂移，抓不到 Register 字面量的增删。
// 这里单独抽这一个字符串实参，缺口才是闭合的。
var methodManifestSources = []string{
	"internal/runtime/server/server.go",
}

// BuildMethodManifest 抽出 `.dispatcher.Register("<method>", …)` 的第一个
// 字符串实参。匹配条件极窄（选择器 X 为 dispatcher、方法名为 Register），
// 仓库内 registerHandlers 是唯一注册点，零误报。
func BuildMethodManifest(root string) ([]string, error) {
	var lines []string
	for _, rel := range methodManifestSources {
		src := filepath.Join(root, rel)
		fileLines, err := methodLinesFor(src)
		if err != nil {
			return nil, err
		}
		lines = append(lines, fileLines...)
	}
	sort.Strings(lines)
	return lines, nil
}

func methodLinesFor(src string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		return nil, err
	}
	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Register" {
			return true
		}
		recv, ok := sel.X.(*ast.SelectorExpr)
		if !ok || recv.Sel.Name != "dispatcher" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		out = append(out, "method "+lit.Value)
		return true
	})
	return out, nil
}

// serverManifestSources 相对模块根，由 BuildServerManifest 覆盖。
var serverManifestSources = []string{
	"internal/runtime/server/handlers.go",
	"internal/runtime/server/index.go",
}

// BuildServerManifest 把 server 投影层的导出类型并入同一指纹体系：
// InitializeParams、ServerCapabilities 等直接出现在 wire 上。
func BuildServerManifest(root string) (string, error) {
	var lines []string
	for _, rel := range serverManifestSources {
		src := filepath.Join(root, rel)
		fileLines, err := manifestLinesFor(src)
		if err != nil {
			return "", err
		}
		lines = append(lines, fileLines...)
	}
	sort.Strings(lines)
	var b strings.Builder
	b.WriteString("# server-wire-types —— internal/runtime/server 投影结构的声明指纹（§C17）。\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func manifestLinesFor(src string) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("解析 %s: %w", src, err)
	}
	var lines []string
	for _, d := range file.Decls {
		switch d := d.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch d.Tok {
				case token.TYPE:
					lines = append(lines, typeLine(spec.(*ast.TypeSpec)))
				case token.CONST:
					vs := spec.(*ast.ValueSpec)
					for _, n := range vs.Names {
						if n.IsExported() {
							lines = append(lines, "const "+n.Name)
						}
					}
				case token.VAR:
					vs := spec.(*ast.ValueSpec)
					for _, n := range vs.Names {
						if n.IsExported() {
							lines = append(lines, "var "+n.Name)
						}
					}
				}
			}
		case *ast.FuncDecl:
			lines = append(lines, funcLine(d))
		}
	}
	return lines, nil
}

// CheckManifest 用重新生成的指纹对比模块根下磁盘上的 protocol.manifest；
// 不一致时返回带逐行差异的错误（调用方负责转为非零退出码，gofmt -l 语义）。
func CheckManifest(root string) error {
	want, err := BuildManifest()
	if err != nil {
		return err
	}
	path := filepath.Join(root, filepath.FromSlash(ManifestRelPath))
	have, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(have) == want {
		return nil
	}
	oldSet := make(map[string]bool)
	var oldLines []string
	for _, l := range splitLines(string(have)) {
		if !oldSet[l] {
			oldSet[l] = true
			oldLines = append(oldLines, l)
		}
	}
	newSet := make(map[string]bool)
	var newLines []string
	for _, l := range splitLines(want) {
		if !newSet[l] {
			newSet[l] = true
			newLines = append(newLines, l)
		}
	}
	var missing, added []string
	for _, l := range oldLines {
		if !newSet[l] {
			missing = append(missing, "- "+l)
		}
	}
	for _, l := range newLines {
		if !oldSet[l] {
			added = append(added, "+ "+l)
		}
	}
	sort.Strings(missing)
	sort.Strings(added)
	diff := append(append([]string{}, missing...), added...)
	return fmt.Errorf("protocol.manifest 已过期（-%d/+%d 行）；更新命令：go run scripts/gen-protocol.go\n%s",
		len(missing), len(added), strings.Join(diff, "\n"))
}

func splitLines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func typeLine(ts *ast.TypeSpec) string {
	name := ts.Name.Name
	switch t := ts.Type.(type) {
	case *ast.StructType:
		parts := make([]string, 0, len(t.Fields.List))
		for _, fl := range t.Fields.List {
			typ := types.ExprString(fl.Type)
			tag := ""
			if fl.Tag != nil {
				tag, _ = strconv.Unquote(fl.Tag.Value)
			}
			if len(fl.Names) == 0 { // 内嵌字段只记类型
				parts = append(parts, typ)
				continue
			}
			for _, n := range fl.Names {
				p := n.Name + " " + typ
				if tag != "" {
					p += " " + tag
				}
				parts = append(parts, p)
			}
		}
		return "type " + name + " struct{" + strings.Join(parts, ", ") + "}"
	case *ast.InterfaceType:
		parts := make([]string, 0, len(t.Methods.List))
		for _, m := range t.Methods.List {
			if len(m.Names) == 0 { // 内嵌接口
				parts = append(parts, types.ExprString(m.Type))
				continue
			}
			for _, n := range m.Names {
				parts = append(parts, n.Name)
			}
		}
		return "type " + name + " interface{" + strings.Join(parts, ", ") + "}"
	default:
		if ts.Assign.IsValid() { // 别名（含 map/string 等任意底层类型）
			return "type " + name + " = " + types.ExprString(ts.Type)
		}
		return "type " + name + " " + types.ExprString(ts.Type)
	}
}

func funcLine(fd *ast.FuncDecl) string {
	line := "func "
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		line += "(" + fieldTypes(fd.Recv) + ") "
	}
	line += fd.Name.Name + "(" + fieldTypes(fd.Type.Params) + ")"
	res := fieldTypes(fd.Type.Results)
	switch {
	case res == "":
	case fd.Type.Results.NumFields() == 1 && len(fd.Type.Results.List[0].Names) == 0:
		line += " " + res
	default:
		line += " (" + res + ")"
	}
	return line
}

func fieldTypes(fl *ast.FieldList) string {
	if fl == nil {
		return ""
	}
	var parts []string
	for _, f := range fl.List {
		typ := types.ExprString(f.Type)
		if len(f.Names) == 0 {
			parts = append(parts, typ)
			continue
		}
		for _, n := range f.Names {
			parts = append(parts, n.Name+" "+typ)
		}
	}
	return strings.Join(parts, ", ")
}
