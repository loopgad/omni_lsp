package conformance

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
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

// arch002Forbidden is INV-ARCH-002's forbidden set, exactly as ADR-0002 and
// internal/security/doc.go state it. The previous version of this test used a
// broader list that both contradicted the ADR -- it banned all of
// internal/protocol, including the jsonrpc framing package the ADR explicitly
// sanctions -- and missed runtime/server, which the ADR does forbid, leaving
// semantic/identity free to depend on the server adapter with nothing to
// catch it. CI's go list -deps check (ci.yml) covers a different set of
// packages than this test does; neither alone is complete, so both must match
// the ADR.
var arch002Forbidden = []string{
	"/internal/protocol/lsp",
	"/internal/protocol/mcp",
	"/internal/protocol/dap",
	"/internal/transport",
	"/internal/runtime/server",
}

// TestARCH002_NoProtocolImportsInCore enforces Y5-2 mechanically instead of
// by claim. It reads the parsed import list rather than scanning the file text:
// a substring scan cannot tell an import from a comment that merely mentions
// the path, and doc comments in these packages do discuss the boundary.
func TestARCH002_NoProtocolImportsInCore(t *testing.T) {
	root := moduleRoot()
	for _, dir := range coreDirs {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if perr != nil {
				return perr
			}
			for _, spec := range file.Imports {
				if spec.Path == nil {
					continue
				}
				imported := strings.Trim(spec.Path.Value, `"`)
				for _, forbidden := range arch002Forbidden {
					if strings.Contains(imported, forbidden) {
						t.Errorf("%s imports %s — INV-ARCH-002 violation", path, strings.TrimPrefix(forbidden, "/"))
					}
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
		"docs/editors/sublime.md",
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

// TestRegistry_ProbeSymbolsExist is the root-cause lock for probe drift.
// checkResult used to verify probe symbols only for AUTO checks, so a PARTIAL
// check could name a test that had been renamed or deleted and keep its entry
// looking live while proving nothing. Two such names had already drifted. This
// walks every check that declares a probe, whatever its Status, and fails with
// the whole drift list at once.
func TestRegistry_ProbeSymbolsExist(t *testing.T) {
	sym, err := testSymbols(moduleRoot())
	if err != nil {
		t.Fatalf("index test symbols: %v", err)
	}
	var drift []string
	for i := range Registry.Checks {
		c := &Registry.Checks[i]
		if c.Probe == nil {
			continue
		}
		for _, missing := range hasAllGroups(sym, c.Probe.Groups) {
			drift = append(drift, c.ID+": "+missing)
		}
	}
	if len(drift) > 0 {
		sort.Strings(drift)
		t.Errorf("%d registry probe(s) name tests that no longer exist:\n  %s",
			len(drift), strings.Join(drift, "\n  "))
	}
}

// TestPartialDetailReportsDriftWithoutRescoring locks the PARTIAL branch of
// checkResult. Its contract has two halves that pull in opposite directions:
// the credit stays at 0.5 whatever the probe does (the credit stands for the
// acknowledged partial work, not for the probe), while the Detail must say
// exactly why the probe is unsound. Both halves drifted silently before —
// a renamed test kept a live-looking PARTIAL entry with no signal at all.
func TestPartialDetailReportsDriftWithoutRescoring(t *testing.T) {
	const pkg = "example/pkg"
	probe := &Probe{Groups: []ProbeGroup{{Pkg: pkg, Tests: []string{"TestPresent", "TestAbsent"}}}}

	tests := []struct {
		name    string
		check   Check
		sym     map[string][]string
		exec    map[string]error
		mode    string
		wantSub string
	}{
		{
			name:    "no probe keeps Reason verbatim",
			check:   Check{Status: StatusPartial, Reason: "acknowledged partial work"},
			mode:    "fast",
			wantSub: "acknowledged partial work",
		},
		{
			name:    "missing probe symbol is named",
			check:   Check{Status: StatusPartial, Reason: "ack", Probe: probe},
			sym:     map[string][]string{pkg: {"TestPresent"}},
			mode:    "fast",
			wantSub: "missing probe symbols: " + pkg + "/TestAbsent",
		},
		{
			name:  "probe failure is reported in full mode",
			check: Check{Status: StatusPartial, Reason: "ack", Probe: probe},
			sym:   map[string][]string{pkg: {"TestPresent"}},
			exec:  map[string]error{pkg + "\x00" + strings.Join(probe.Groups[0].Tests, "\x00"): errors.New("boom")},
			mode:  "full",
			// A missing symbol outranks the failure text, so name the symbol
			// rather than pretending the probe ran clean.
			wantSub: "missing probe symbols: " + pkg + "/TestAbsent",
		},
		{
			name:  "probe failure is reported when every symbol exists",
			check: Check{Status: StatusPartial, Reason: "ack", Probe: &Probe{Groups: []ProbeGroup{{Pkg: pkg, Tests: []string{"TestPresent"}}}}},
			sym:   map[string][]string{pkg: {"TestPresent"}},
			// checkResult keys the exec map on pkg\x00join(Tests,\x00), so the
			// key must name exactly this group's tests.
			exec:    map[string]error{pkg + "\x00TestPresent": errors.New("boom")},
			mode:    "full",
			wantSub: "probe failed: boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkResult(&tt.check, tt.sym, tt.exec, tt.mode)
			if got.Result != creditPartial {
				t.Errorf("Result = %v, want %v; PARTIAL credit must not move with the probe",
					got.Result, creditPartial)
			}
			if !strings.Contains(got.Detail, tt.wantSub) {
				t.Errorf("Detail = %q, want it to contain %q", got.Detail, tt.wantSub)
			}
		})
	}
}

// TestPartialDetailSkipsProbeRunOutsideFullMode pins that fast mode reports
// drift from the symbol index alone. Fast mode never spawns a subprocess, so a
// probe that exists but fails must not be reported as failed there; doing so
// would make the fast score depend on the machine.
func TestPartialDetailSkipsProbeRunOutsideFullMode(t *testing.T) {
	const pkg = "example/pkg"
	groups := []ProbeGroup{{Pkg: pkg, Tests: []string{"TestPresent"}}}
	c := &Check{
		Status: StatusPartial,
		Reason: "ack",
		Probe:  &Probe{Groups: groups},
	}
	sym := map[string][]string{pkg: {"TestPresent"}}
	exec := map[string]error{pkg + "\x00TestPresent": errors.New("boom")}

	for _, mode := range []string{"fast", ""} {
		got := checkResult(c, sym, exec, mode)
		if strings.Contains(got.Detail, "probe failed") {
			t.Errorf("mode %q reported a probe failure it never observed: Detail = %q", mode, got.Detail)
		}
	}
	if got := checkResult(c, sym, exec, "full"); !strings.Contains(got.Detail, "probe failed: boom") {
		t.Errorf("full mode Detail = %q, want it to contain %q", got.Detail, "probe failed: boom")
	}
}
