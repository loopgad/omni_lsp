package conformance

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// testSymbols walks the module and returns every top-level Test*/Benchmark*
// function name, mapped by the package dir that declares it.
func testSymbols(root string) (map[string][]string, error) {
	out := map[string][]string{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				return filepath.SkipDir // unreadable tree is not ours to judge
			}
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "testdata" || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "main_test.go") {
			return nil
		}
		// Build-tag-excluded files (e.g. soak) still count: their symbols
		// exist in source; execution is the gate's business.
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, rerr := filepath.Rel(root, filepath.Dir(path))
		if rerr != nil {
			return rerr
		}
		pkg := filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			name := fn.Name.Name
			if strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark") || strings.HasPrefix(name, "Fuzz") {
				out[pkg] = append(out[pkg], name)
			}
		}
		return nil
	})
	for k := range out {
		sort.Strings(out[k])
	}
	return out, err
}

func hasAllGroups(sym map[string][]string, groups []ProbeGroup) (missing []string) {
	for _, g := range groups {
		set := map[string]bool{}
		for _, s := range sym[g.Pkg] {
			set[s] = true
		}
		for _, t := range g.Tests {
			if !set[t] {
				missing = append(missing, g.Pkg+"/"+t)
			}
		}
	}
	return missing
}

// runTestsBatch executes one go test invocation covering every probe in a
// package. Returns the combined output tail on failure.
// probeRequiredEnv forces the environment a probe package needs before its
// tests will actually run. Without it a gated test skips, `go test` exits 0,
// and the probe collects full credit for measuring nothing.
var probeRequiredEnv = map[string][]string{
	"test/corpus": {"OMNILSP_S21_GATE=required"},
}

func runTestsBatch(pkg string, tests []string, timeout string) error {
	args := []string{"test", "-race", "-count=1"}
	if timeout != "" {
		args = append(args, "-timeout", timeout)
	}
	pattern := "^(" + strings.Join(quoted(tests), "|") + ")$"
	args = append(args, "-run", pattern, "./"+pkg)
	cmd := exec.Command("go", args...)
	cmd.Dir = moduleRoot()
	// A probe that skips because an env gate is unset still exits 0, which
	// checkResult would score as a pass. PERF-3's S21 gate is the one place
	// that happens: test/corpus skips without OMNILSP_BIN, so the probe must
	// require it rather than inherit the environment.
	if required, ok := probeRequiredEnv[pkg]; ok {
		cmd.Env = append(os.Environ(), required...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(out)
		if len(tail) > 2000 {
			tail = tail[len(tail)-2000:]
		}
		return fmt.Errorf("go test %s: %v\n%s", pkg, err, tail)
	}
	return nil
}

func quoted(xs []string) []string {
	cp := make([]string, len(xs))
	for i, x := range xs {
		cp[i] = strings.ReplaceAll(x, "'", "")
	}
	return cp
}

// moduleRoot resolves the enclosing module root once; the engine lives at
// internal/conformance, so the root is two directories up from this file.
var moduleRootOnce struct {
	once sync.Once
	val  string
	err  error
}

func moduleRoot() string {
	moduleRootOnce.once.Do(func() {
		moduleRootOnce.val, moduleRootOnce.err = findModuleRoot()
	})
	if moduleRootOnce.err != nil {
		return ""
	}
	return moduleRootOnce.val
}

// findModuleRoot walks up from the working directory to the go.mod holder.
func findModuleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// RequireModuleRoot returns the enclosing go.mod directory or an error.
func RequireModuleRoot() (string, error) {
	root, err := findModuleRoot()
	if err != nil || root == "" {
		wd, _ := os.Getwd()
		return "", fmt.Errorf("conformance: no go.mod found from %s; run inside the omnilsp module", wd)
	}
	return root, nil
}
