package server

// Threshold-derivation guards (round-16 ruling 5): the shutdown-drain and
// external-sync windows must come from named budgets — an explicit Config
// knob, else defaultDrainWindow/defaultExternalSyncWindow capped by the §F9
// CostTiny deadline — never from bare integer literals scattered through the
// production files. The AST guard below retires the bare-literal shape
// mechanically so the six retired call sites cannot quietly grow back.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/runtime/scheduler"
)

// TestShutdownWindows_DefaultConfigDerivesBudget pins that under
// DefaultConfig (CostTiny deadline 30s ⇒ min(2s, 30s) = 2s) both windows
// answer exactly the named budget constants, not something wider.
func TestShutdownWindows_DefaultConfigDerivesBudget(t *testing.T) {
	s := New(DefaultConfig())
	if got := s.drainWindow(); got != defaultDrainWindow {
		t.Fatalf("drainWindow() = %s, want the named default %s", got, defaultDrainWindow)
	}
	if got := s.externalSyncWindow(); got != defaultExternalSyncWindow {
		t.Fatalf("externalSyncWindow() = %s, want the named default %s", got, defaultExternalSyncWindow)
	}
}

// TestShutdownWindows_ExplicitKnobsWin pins the override half of the
// derivation: an explicit Config knob always beats the derived budget.
func TestShutdownWindows_ExplicitKnobsWin(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ShutdownDrainWindow = 7 * time.Second
	cfg.ExternalSyncWindow = 3 * time.Second
	s := New(cfg)
	if got := s.drainWindow(); got != 7*time.Second {
		t.Fatalf("drainWindow() = %s, want the explicit 7s", got)
	}
	if got := s.externalSyncWindow(); got != 3*time.Second {
		t.Fatalf("externalSyncWindow() = %s, want the explicit 3s", got)
	}
}

// TestShutdownWindows_CappedByTinyBudget is the budget upper-bound hard
// evidence: with RequestDeadlines[CostTiny] = 300ms and the knobs unset, both
// windows must shrink to exactly that budget — a future bare literal can
// never outlive the request budget it serves.
func TestShutdownWindows_CappedByTinyBudget(t *testing.T) {
	const tiny = 300 * time.Millisecond
	cfg := DefaultConfig()
	cfg.RequestDeadlines = map[scheduler.CostClass]time.Duration{
		scheduler.CostTiny: tiny,
	}
	s := New(cfg)
	if got := s.drainWindow(); got != tiny {
		t.Fatalf("drainWindow() = %s, want it capped at the CostTiny budget %s", got, tiny)
	}
	if got := s.externalSyncWindow(); got != tiny {
		t.Fatalf("externalSyncWindow() = %s, want it capped at the CostTiny budget %s", got, tiny)
	}
}

// intTimesSecond reports whether "a × b" is the shape <integer literal> ×
// time.Second (either operand order).
func intTimesSecond(a, b ast.Expr) bool {
	lit, ok := a.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return false
	}
	sel, ok := b.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "time" && sel.Sel.Name == "Second"
}

// bareSecondMultiplications parses the named production files and reports
// every "integer × time.Second" literal whose expression is NOT part of a
// top-level const/var initializer. The named-budget definitions themselves
// (the §F8 ladder table, the default window constants) live at file scope, so
// they are whitelisted by construction; anything inside a function body is a
// bare call-site literal — exactly the shape round-16 ruling 5 retires.
func bareSecondMultiplications(t *testing.T, files ...string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var hits []string
	for _, name := range files {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		whitelisted := map[ast.Node]bool{}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, val := range vs.Values {
					ast.Inspect(val, func(n ast.Node) bool {
						whitelisted[n] = true
						return true
					})
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || bin.Op != token.MUL || whitelisted[bin] {
				return true
			}
			if intTimesSecond(bin.X, bin.Y) || intTimesSecond(bin.Y, bin.X) {
				hits = append(hits, fmt.Sprintf("%s:%d", name, fset.Position(bin.Pos()).Line))
			}
			return true
		})
	}
	return hits
}

// TestNoBareSecondLiteralsInServerProductionFiles is the AST guard: the
// server's production files must not carry bare "integer × time.Second"
// literals outside the named-budget definitions. Failures list every site at
// once so a regression batch is visible in one run.
func TestNoBareSecondLiteralsInServerProductionFiles(t *testing.T) {
	hits := bareSecondMultiplications(t, "server.go", "handlers.go", "source_changes.go")
	if len(hits) > 0 {
		t.Fatalf("%d bare integer×time.Second literal(s) in server production files; derive from named budgets (drainWindow/externalSyncWindow or the default* constants) instead:\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}
