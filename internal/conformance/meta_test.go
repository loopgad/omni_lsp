package conformance

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestU2_InvariantHeadersPresent enforces §U2/Y5-1 at PACKAGE granularity:
// every internal package documents its invariants in at least one primary
// source file's doc comment.
func TestU2_InvariantHeadersPresent(t *testing.T) {
	root := moduleRoot()
	fset := token.NewFileSet()
	documented := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(filepath.Join(root, "internal"), path)
		if rerr != nil {
			return rerr
		}
		pkg := filepath.ToSlash(filepath.Dir(rel))
		if pkg == "." || documented[pkg] {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil || f.Doc == nil {
			return nil
		}
		if strings.Contains(f.Doc.Text(), "Invariant") {
			documented[pkg] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Enumerate only directories that actually contain non-test Go sources.
	pkgs := map[string]bool{}
	filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if d.Name() == "testdata" { // fixture tree, not a package
			return filepath.SkipDir
		}
		rel, rerr := filepath.Rel(filepath.Join(root, "internal"), path)
		if rerr != nil || rel == "." {
			return nil
		}
		entries, derr := os.ReadDir(path)
		if derr != nil {
			return derr
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
				pkgs[filepath.ToSlash(rel)] = true
				break
			}
		}
		return nil
	})
	for pkg := range pkgs {
		if !documented[pkg] && !isLeafHelper(pkg) {
			t.Errorf("internal/%s: no package file documents Invariants (§U2)", pkg)
		}
	}
	if len(documented) < 15 {
		t.Fatalf("suspiciously few documented packages (%d); walker broken?", len(documented))
	}
}

// isLeafHelper exempts pure-leaf utility dirs whose contracts are fully
// carried by their exported signatures.
func isLeafHelper(pkg string) bool {
	switch pkg {
	case "internal/conformance":
		return false // this package must document itself
	default:
		return false
	}
}

// coreDirs must never import protocol surfaces (INV-ARCH-002).
var coreDirs = []string{
	"internal/semantic",
	"internal/identity",
	"internal/runtime/scheduler",
	"internal/workspace",
}

// TestARCH002_NoProtocolImportsInCore enforces Y5-2 mechanically instead of
// by claim.
func TestARCH002_NoProtocolImportsInCore(t *testing.T) {
	root := moduleRoot()
	banned := []string{"/internal/protocol/", "/internal/transport/", "/internal/languages/"}
	for _, dir := range coreDirs {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, b := range banned {
				if strings.Contains(string(src), b) {
					t.Errorf("%s imports %s — INV-ARCH-002 violation", path, strings.Trim(b, "/"))
				}
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// allowedDeps is the U5 whitelist: golang.org/x/tools powers the Go bridge's
// semantic loading (gopls-grade); the rest are its transitive chain. Anything
// outside the list fails the build's dependency policy.
var allowedDeps = map[string]bool{
	"golang.org/x/tools": true,
	"golang.org/x/mod":   true,
	"golang.org/x/sync":  true,
	// SCIP 互操作适配器（ADR-0006）：
	"github.com/scip-code/scip/bindings/go/scip": true, // SCIP protobuf 绑定本体（T5 DoD，§L14）
	"google.golang.org/protobuf":                 true, // scip 绑定的运行时依赖（官方 protobuf-go）
	"github.com/sourcegraph/beaut":               true, // scip 绑定的传递依赖（测试辅助链）
}

// TestU5_NoThirdPartyRuntimeDeps enforces Y5-5 mechanically: every require
// in go.mod must be inside the reviewed whitelist.
func TestU5_NoThirdPartyRuntimeDeps(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(moduleRoot(), "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "require (") {
			inBlock = true
			continue
		}
		if inBlock && line == ")" {
			inBlock = false
			continue
		}
		var mod string
		switch {
		case strings.HasPrefix(line, "require "):
			mod = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		case inBlock && line != "":
			mod = line
		default:
			continue
		}
		name := strings.Fields(mod)[0]
		if !allowedDeps[name] {
			t.Errorf("dependency %q outside U5 whitelist %v", name, allowedDeps)
		}
	}
}

// TestX8_ClientDocsPresent enforces Y2-5/X8-2 mechanically: every supported
// client profile ships its configuration document.
func TestX8_ClientDocsPresent(t *testing.T) {
	docs := []string{
		"docs/editors/neovim.md",
		"docs/editors/emacs.md",
		"docs/editors/helix.md",
		"docs/editors/zed.md",
	}
	for _, d := range docs {
		fi, err := os.Stat(filepath.Join(moduleRoot(), filepath.FromSlash(d)))
		if err != nil {
			t.Errorf("missing client profile %s", d)
			continue
		}
		if fi.Size() < 200 {
			t.Errorf("%s suspiciously small (%d bytes)", d, fi.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(moduleRoot(), "editors", "vscode")); err != nil {
		t.Error("editors/vscode extension missing from client matrix")
	}
}
