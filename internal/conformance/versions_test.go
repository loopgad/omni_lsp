package conformance

// TestY56_VersionSurfacesDocumented makes the "documented" half of registry
// check Y5-6 machine-checkable. The check's other half is the protocol.manifest
// fingerprint (TestY54_ProtocolManifestFresh); that one never opened
// docs/versions.md, so a stale version table scored full credit while the page
// claimed to be audited. ADR-0009 D3 requires probe text to match what the probe
// actually asserts.
//
// Mechanism: every row's Pin mechanism column names a `path:Ident` Go constant.
// This test resolves that constant with go/parser and compares it to the
// Version column, so a change on either side is caught. Standard library only —
// the same go/parser approach probe.go:41 already uses.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// versionRow is one line of the docs/versions.md table.
type versionRow struct {
	surface string
	version string
	pin     string
}

// TestY56_VersionSurfacesDocumented checks each row's Version cell against the
// constant its Pin mechanism names.
func TestY56_VersionSurfacesDocumented(t *testing.T) {
	rows := parseVersionsTable(t)
	// A row can vanish without any other check noticing, so hold the floor at
	// the surfaces §R4 requires (LSP, MCP, replay, plugin, HTTP, Go module).
	// Adding rows stays legal; deleting one is not.
	const minRows = 6
	if len(rows) < minRows {
		t.Fatalf("docs/versions.md has %d data rows, want at least %d; dropping a version surface would leave it unaudited",
			len(rows), minRows)
	}
	for _, r := range rows {
		path, ident, ok := parsePin(r.pin)
		if !ok {
			// go.mod carries no Go constant — the module path is a file-level
			// directive, so it gets its own comparison rather than exemption.
			if hasModulePin(r.pin) {
				if got := resolveModulePath(t); r.version != got {
					t.Errorf("%s: docs say %q but go.mod declares module %q", r.surface, r.version, got)
				}
				continue
			}
			t.Errorf("%s: Pin mechanism %q must name a `path:Ident` Go constant so the row is machine-auditable", r.surface, r.pin)
			continue
		}
		got, ok := resolveConst(t, path, ident)
		if !ok {
			t.Errorf("%s: pinned constant %s:%s not found", r.surface, path, ident)
			continue
		}
		if want := normalizeVersion(got); r.version != want {
			t.Errorf("%s: docs say %q but %s:%s = %q (rendered %q)",
				r.surface, r.version, path, ident, got, want)
		}
	}
}

// hasModulePin reports whether the pin cell anchors on go.mod.
func hasModulePin(cell string) bool {
	for _, part := range strings.Fields(cell) {
		if strings.Trim(part, "`") == "go.mod" {
			return true
		}
	}
	return false
}

// resolveModulePath reads the module path from go.mod.
func resolveModulePath(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot(), "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("go.mod has no module directive")
	return ""
}

// parseVersionsTable reads docs/versions.md and returns its data rows. Row
// count is deliberately not pinned: adding a surface is legitimate, forgetting
// its pin constant is what must fail.
func parseVersionsTable(t *testing.T) []versionRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot(), "docs", "versions.md"))
	if err != nil {
		t.Fatalf("read docs/versions.md: %v", err)
	}
	var rows []versionRow
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 {
			continue
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		// Skip the header and its --- separator row.
		if cells[0] == "Surface" || strings.HasPrefix(cells[1], "---") {
			continue
		}
		rows = append(rows, versionRow{surface: cells[0], version: cells[1], pin: cells[2]})
	}
	return rows
}

// parsePin extracts path and identifier from a `path:Ident` pin cell.
func parsePin(cell string) (path, ident string, ok bool) {
	for _, part := range strings.Fields(cell) {
		part = strings.Trim(part, "`")
		if !strings.Contains(part, ":") {
			continue
		}
		p, id, found := strings.Cut(part, ":")
		if found && p != "" && id != "" && !strings.Contains(id, "/") {
			return p, id, true
		}
	}
	return "", "", false
}

// resolveConst reads a string or integer constant declaration without importing
// the package, so this test adds no dependency edge.
func resolveConst(t *testing.T, path, ident string) (value string, ok bool) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(moduleRoot(), path), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		vs, isValue := n.(*ast.ValueSpec)
		if !isValue || len(vs.Names) != 1 || vs.Names[0].Name != ident || len(vs.Values) != 1 {
			return true
		}
		lit, isLit := vs.Values[0].(*ast.BasicLit)
		if !isLit {
			return true
		}
		switch lit.Kind {
		case token.STRING:
			if s, err := strconv.Unquote(lit.Value); err == nil {
				value, ok = s, true
			}
		case token.INT:
			value, ok = lit.Value, true
		}
		return false
	})
	return value, ok
}

// normalizeVersion renders an integer constant as "v<n>" and leaves a string
// constant as-is, so "omnilsp.plugin.v1" keeps its literal spelling.
func normalizeVersion(value string) string {
	if _, err := strconv.Atoi(value); err == nil {
		return "v" + value
	}
	return value
}
