package clang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

func TestBuildAndExtractSemanticFactsAcrossCompileContexts(t *testing.T) {
	compiler, ok := pinnedLLVM22Compiler(t)
	if !ok {
		t.Skip("pinned LLVM 22.1.5 is not installed")
	}
	logicalRoot := t.TempDir()
	snapshotRoot := t.TempDir()
	rootURI := workspaceuri.FromPath(logicalRoot).Canonical()
	uriFor := func(relative string) string {
		return workspaceuri.FromPath(filepath.Join(logicalRoot, filepath.FromSlash(relative))).Canonical()
	}
	files := map[string][]byte{
		"api.hpp": []byte(`#pragma once
#define API_WRAP(x) ((x) + 1)
struct Base { virtual int value(int) const = 0; };
struct Widget : Base {
  int value(int) const override;
  int score(int) const;
  int score(double) const;
};
struct C { C(); };
int external(int);
`),
		"api.cpp": []byte(`#include "api.hpp"
#define IMPL_WRAP(x) ((x) + CTX)
int Widget::value(int x) const { return IMPL_WRAP(x); }
int Widget::score(int x) const { return x + CTX; }
int Widget::score(double x) const { return static_cast<int>(x) + CTX; }
int external(int x) { return x; }
int dispatch(Widget const& w) { return w.value(1) + w.score(2) + w.score(3.0) + external(4); }
`),
		"main.cpp": []byte(`#include "api.hpp"
int use(Widget& w) { return API_WRAP(w.value(5) + w.score(6) + w.score(7.0) + external(8)); }
int construct() {
  C c;
  return 0;
}
`),
		"generated.cpp": []byte(`int generated() { return CTX; }
`),
		"template.cpp": []byte(`int generated_template() { return 0; }
`),
	}
	args := func(source string, define string) []string {
		return []string{compiler, "-std=c++17", "-I", logicalRoot, "-DCTX=" + define, "-c", filepath.Join(logicalRoot, source), "-o", filepath.Join(logicalRoot, "obj", strings.TrimSuffix(source, ".cpp")+".obj")}
	}
	db, err := json.Marshal([]map[string]any{
		{"directory": logicalRoot, "file": filepath.Join(logicalRoot, "api.cpp"), "arguments": args("api.cpp", "1")},
		{"directory": logicalRoot, "file": filepath.Join(logicalRoot, "main.cpp"), "arguments": args("main.cpp", "2")},
		{"directory": logicalRoot, "file": filepath.Join(logicalRoot, "generated.cpp"), "arguments": args("generated.cpp", "2")},
	})
	if err != nil {
		t.Fatal(err)
	}
	files["uncompiled.cpp"] = []byte("int uncompiled_generated() { return 0; }\n")
	files["compile_commands.json"] = db
	view := newTestView(t, rootURI, logicalRoot, snapshotRoot, files)
	generatedURI, templateURI := uriFor("generated.cpp"), uriFor("template.cpp")
	generated := view.files[generatedURI]
	generated.Generated = true
	generated.SourceURI = templateURI
	generated.SourceMap = []model.SourceMapSpan{{
		Generated: model.Position{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 11},
		SourceURI: templateURI,
		Source:    model.Position{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 11},
	}}
	view.files[generatedURI] = generated
	uncompiledURI := uriFor("uncompiled.cpp")
	uncompiled := view.files[uncompiledURI]
	uncompiled.Generated = true
	uncompiled.SourceURI = templateURI
	uncompiled.SourceMap = []model.SourceMapSpan{{
		Generated: model.Position{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 11},
		SourceURI: templateURI,
		Source:    model.Position{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 11},
	}}
	view.files[uncompiledURI] = uncompiled
	request, err := BuildIndexRequest(context.Background(), view, rootURI, identity.BackendID{Language: "cpp", Name: "ccls-test"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Scopes) != 2 {
		t.Fatalf("got %d build contexts, want 2", len(request.Scopes))
	}
	for _, scope := range request.Scopes {
		pinned := request.Provenance[scope.ID].Tools
		if len(pinned) != 2 || pinned[0].Version != "22.1.5" && pinned[1].Version != "22.1.5" {
			t.Fatalf("scope %q does not pin the requested LLVM 22.1.5 toolchain: %+v", scope.ID, pinned)
		}
	}
	sink := &memorySink{}
	report, err := Extract(context.Background(), request, sink)
	if err != nil {
		t.Fatal(err)
	}
	for _, occurrence := range sink.occurrences {
		if occurrence.Role != "reference" {
			continue
		}
		if occurrence.Range.EndLine < occurrence.Range.StartLine ||
			(occurrence.Range.EndLine == occurrence.Range.StartLine &&
				occurrence.Range.EndChar <= occurrence.Range.StartChar) {
			t.Fatalf("reference occurrence has no non-empty identifier range: %+v", occurrence)
		}
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("report validation failed: %v", err)
	}
	for _, scope := range request.Scopes {
		helperPinned := false
		for _, tool := range report.UsedTools[scope.ID] {
			if tool.Name == "omnilsp-clang-helper" && filepath.IsAbs(tool.Path) && len(tool.SHA256) == 64 {
				helperPinned = true
			}
		}
		if !helperPinned {
			t.Errorf("scope %q did not report the generated helper executable identity", scope.ID)
		}
	}
	if view.materializeCount != 1 || view.closeCount != 1 {
		t.Fatalf("borrowed view lifecycle: materialize=%d close=%d, want 1 each", view.materializeCount, view.closeCount)
	}
	if _, err := os.Stat(filepath.Join(snapshotRoot, "api.hpp")); err != nil {
		t.Fatalf("extractor removed or modified the borrowed snapshot: %v", err)
	}

	var scoreIDs = make(map[identity.SymbolID]bool)
	var macroIDs = make(map[identity.SymbolID]bool)
	var valueIDs = make(map[identity.SymbolID]bool)
	var externalID identity.SymbolID
	for _, symbol := range sink.symbols {
		if symbol.Name == "score" {
			scoreIDs[symbol.ID] = true
		}
		if symbol.Name == "value" {
			valueIDs[symbol.ID] = true
		}
		if symbol.Name == "external" {
			externalID = symbol.ID
		}
		if symbol.Name == "API_WRAP" {
			macroIDs[symbol.ID] = true
		}
	}
	if len(scoreIDs) != 2 {
		var names []string
		for _, symbol := range sink.symbols {
			names = append(names, symbol.Name+"/"+symbol.Kind)
		}
		t.Fatalf("overload identities = %d, want two distinct score overloads; symbols=%v coverage=%+v", len(scoreIDs), names, report.Coverage)
	}
	var headerContexts = make(map[identity.BuildContextID]bool)
	matchedCrossFileReference := false
	matchedMacroReference := false
	var baseValueID, widgetValueID identity.SymbolID
	externalDefinition := false
	for _, occurrence := range sink.occurrences {
		if occurrence.URI == uriFor("api.hpp") && occurrence.Role == "declaration" && scoreIDs[occurrence.SymbolID] {
			headerContexts[occurrence.BuildContext] = true
		}
		if occurrence.URI == uriFor("api.hpp") && occurrence.Role == "declaration" && valueIDs[occurrence.SymbolID] {
			switch occurrence.Range.StartLine {
			case 2:
				baseValueID = occurrence.SymbolID
			case 4:
				widgetValueID = occurrence.SymbolID
			}
		}
		if occurrence.URI == uriFor("api.cpp") && occurrence.Role == "definition" && occurrence.SymbolID == externalID {
			externalDefinition = true
		}
		if occurrence.URI == uriFor("main.cpp") && occurrence.Role == "reference" && scoreIDs[occurrence.SymbolID] {
			matchedCrossFileReference = true
		}
		if occurrence.URI == uriFor("main.cpp") && occurrence.Role == "reference" && macroIDs[occurrence.SymbolID] {
			matchedMacroReference = true
		}
	}
	if len(headerContexts) != 2 {
		t.Fatalf("header declarations observed in %d build contexts, want 2", len(headerContexts))
	}
	if !matchedCrossFileReference {
		t.Fatal("no main.cpp reference resolved to an api.hpp overload declaration")
	}
	if externalID == "" || !externalDefinition {
		t.Fatalf("C++ external definition was not exported from api.cpp: symbol=%q definition=%v", externalID, externalDefinition)
	}
	constructorIDs := make(map[identity.SymbolID]bool)
	for _, symbol := range sink.symbols {
		if symbol.Name == "C" && strings.Contains(strings.ToLower(symbol.Kind), "constructor") {
			constructorIDs[symbol.ID] = true
		}
	}
	if len(constructorIDs) == 0 {
		t.Fatalf("constructor declaration was not exported; symbols=%+v", sink.symbols)
	}
	constructorReference := false
	constructorCallEdge := false
	for _, occurrence := range sink.occurrences {
		if occurrence.URI != uriFor("main.cpp") || occurrence.Role != "reference" || !constructorIDs[occurrence.SymbolID] {
			continue
		}
		if occurrence.Range != (model.Position{StartLine: 3, StartChar: 2, EndLine: 3, EndChar: 3}) {
			t.Fatalf("constructor reference did not map to the exact C token: %+v", occurrence)
		}
		constructorReference = true
	}
	for _, edge := range sink.edges {
		if edge.Kind != model.EdgeCall || edge.SourceURI != uriFor("main.cpp") || !constructorIDs[edge.To] {
			continue
		}
		if edge.Range != (model.Position{StartLine: 3, StartChar: 2, EndLine: 3, EndChar: 3}) {
			t.Fatalf("constructor call edge did not map to the exact C token: %+v", edge)
		}
		constructorCallEdge = true
	}
	if !constructorReference {
		var referenceCoverage []model.Coverage
		for _, item := range report.Coverage {
			if item.Fact == model.FactReference {
				referenceCoverage = append(referenceCoverage, item)
			}
		}
		t.Fatalf("construction expression did not export a reference to C::C; constructor IDs=%v reference coverage=%+v", constructorIDs, referenceCoverage)
	}
	if !constructorCallEdge {
		t.Fatal("construction expression did not export a range-aware call edge to C::C")
	}
	if len(macroIDs) != 1 || !matchedMacroReference {
		t.Fatalf("macro definition/reference did not map across header and expansion: IDs=%v matched=%v", macroIDs, matchedMacroReference)
	}
	partialMacroCoverage := false
	for _, coverage := range report.Coverage {
		if coverage.Fact == model.FactReference && coverage.State == model.IncompleteKnownSubset && strings.Contains(coverage.Reason, "macro") {
			partialMacroCoverage = true
		}
	}
	if !partialMacroCoverage {
		t.Fatal("macro expansion did not report explicit partial reference coverage")
	}
	if baseValueID == "" || widgetValueID == "" || baseValueID == widgetValueID {
		t.Fatalf("override declarations did not retain distinct Base and Widget identities: Base=%q Widget=%q", baseValueID, widgetValueID)
	}
	implementationOverride := false
	for _, edge := range sink.edges {
		if edge.Kind == model.EdgeImplementation && edge.SourceURI == uriFor("api.hpp") &&
			edge.From == widgetValueID && edge.To == baseValueID {
			implementationOverride = true
		}
	}
	if !implementationOverride {
		t.Fatalf("Widget::value override edge missing or reversed: Base=%q Widget=%q edges=%+v", baseValueID, widgetValueID, sink.edges)
	}
	for _, edgeKind := range []model.EdgeKind{model.EdgeCall, model.EdgeTypeRelation, model.EdgeImplementation, model.EdgeInclude, model.EdgeGenerated} {
		found := false
		for _, edge := range sink.edges {
			if edge.Kind == edgeKind {
				found = true
				break
			}
		}
		if !found {
			var kinds []model.EdgeKind
			for _, edge := range sink.edges {
				kinds = append(kinds, edge.Kind)
			}
			t.Errorf("no %q edge was exported; edges=%v", edgeKind, kinds)
		}
	}
	typeIdentityLossSeen := false
	for _, scope := range request.Scopes {
		for _, fact := range []model.FactKind{
			model.FactSymbol, model.FactDeclaration, model.FactDefinition,
			model.FactImplementation, model.FactTypeRelation, model.FactCall, model.FactInclude,
		} {
			item, ok := findCoverage(report.Coverage, scope.ID, fact)
			if !ok {
				t.Fatalf("C++ coverage omitted scope=%q fact=%q", scope.ID, fact)
			}
			lostTypeIdentity := fact == model.FactTypeRelation && strings.Contains(item.Reason, "type relation has no stable owner or target identity")
			if lostTypeIdentity {
				typeIdentityLossSeen = true
			}
			if item.State != model.Complete &&
				(item.State != model.IncompleteKnownSubset ||
					(!strings.Contains(item.Reason, "outside the captured workspace view") && !lostTypeIdentity)) {
				t.Fatalf("C++ %s coverage is not complete or explicitly partial for known extraction limits: %+v", fact, item)
			}
			if strings.Contains(item.Reason, "has no stable") && !lostTypeIdentity {
				t.Fatalf("C++ %s coverage reported an unexpected lost semantic identity: %+v", fact, item)
			}
		}
		importCoverage, ok := findCoverage(report.Coverage, scope.ID, model.FactImport)
		if !ok || (importCoverage.State != model.Unknown &&
			(importCoverage.State != model.IncompleteKnownSubset || !strings.Contains(importCoverage.Reason, "outside the captured workspace view"))) {
			t.Fatalf("C++ import coverage is not unknown or explicitly partial for external declarations: %+v", importCoverage)
		}
		assertCoverageState(t, report.Coverage, scope.ID, model.FactModule, model.Unknown, "")
		assertCoverageState(t, report.Coverage, scope.ID, model.FactGenerated, model.IncompleteKnownSubset, uncompiledURI)
		referenceCoverage, ok := findCoverage(report.Coverage, scope.ID, model.FactReference)
		if !ok || (referenceCoverage.State != model.Complete &&
			(referenceCoverage.State != model.IncompleteKnownSubset ||
				(!strings.Contains(referenceCoverage.Reason, "macro") && !strings.Contains(referenceCoverage.Reason, "outside the captured workspace view")))) {
			t.Fatalf("C++ reference coverage is neither complete nor explicitly partial for macros/external declarations: %+v", referenceCoverage)
		}
	}
	if !typeIdentityLossSeen {
		t.Fatal("C++ extraction did not report incomplete type-relation coverage after a type owner/target identity was lost")
	}
	generatedRangeMapped := false
	for _, edge := range sink.edges {
		if edge.Kind == model.EdgeGenerated && edge.SourceURI == generatedURI && edge.Range.EndChar == 11 {
			generatedRangeMapped = true
		}
	}
	if !generatedRangeMapped {
		t.Fatal("generated source mapping did not produce a range-aware generated edge")
	}
}

func TestBuildAndExtractCTranslationUnitsInCompileContext(t *testing.T) {
	pinnedCompiler, ok := pinnedLLVM22Compiler(t)
	if !ok {
		t.Skip("pinned LLVM 22.1.5 is not installed")
	}
	compiler := filepath.Join(filepath.Dir(pinnedCompiler), "clang.exe")
	if !fileExists(compiler) {
		t.Skip("pinned LLVM 22.1.5 C driver is not installed")
	}
	logicalRoot := t.TempDir()
	snapshotRoot := t.TempDir()
	rootURI := workspaceuri.FromPath(logicalRoot).Canonical()
	uriFor := func(relative string) string {
		return workspaceuri.FromPath(filepath.Join(logicalRoot, filepath.FromSlash(relative))).Canonical()
	}
	files := map[string][]byte{
		"api.h": []byte(`#ifndef API_H
#define API_H
struct Counter { int value; };
int increment(int);
#endif
`),
		"impl.c": []byte(`#include "api.h"
int increment(int value) { return value + 1; }
`),
		"main.c": []byte(`#include "api.h"
int run(void) { struct Counter counter = {0}; return increment(counter.value); }
`),
	}
	args := func(source string) []string {
		return []string{compiler, "-std=c11", "-I", logicalRoot, "-c",
			filepath.Join(logicalRoot, source), "-o",
			filepath.Join(logicalRoot, "obj", strings.TrimSuffix(source, ".c")+".obj")}
	}
	database, err := json.Marshal([]map[string]any{
		{"directory": logicalRoot, "file": filepath.Join(logicalRoot, "impl.c"), "arguments": args("impl.c")},
		{"directory": logicalRoot, "file": filepath.Join(logicalRoot, "main.c"), "arguments": args("main.c")},
	})
	if err != nil {
		t.Fatal(err)
	}
	files["compile_commands.json"] = database
	view := newTestView(t, rootURI, logicalRoot, snapshotRoot, files)
	request, err := BuildIndexRequest(context.Background(), view, rootURI,
		identity.BackendID{Language: "cpp", Name: "ccls-c-test"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Scopes) != 1 {
		t.Fatalf("C translation units produced %d compile contexts, want one: %+v", len(request.Scopes), request.Scopes)
	}
	scope := request.Scopes[0]
	if got := strings.Join(scope.Build.PackagePatterns, ","); got != "impl.c,main.c" {
		t.Fatalf("C translation-unit scope contains %q, want impl.c,main.c", got)
	}
	toolPinned := false
	for _, tool := range request.Provenance[scope.ID].Tools {
		if tool.Path == compiler && tool.Version == "22.1.5" {
			toolPinned = true
		}
	}
	if !toolPinned {
		t.Fatalf("C compile scope did not pin clang 22.1.5: %+v", request.Provenance[scope.ID].Tools)
	}

	sink := &memorySink{}
	report, err := Extract(context.Background(), request, sink)
	if err != nil {
		t.Fatal(err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("C report validation failed: %v", err)
	}
	if len(sink.symbols) == 0 || len(sink.occurrences) == 0 || len(sink.edges) == 0 {
		t.Fatalf("C extraction returned empty facts: symbols=%d occurrences=%d edges=%d coverage=%+v",
			len(sink.symbols), len(sink.occurrences), len(sink.edges), report.Coverage)
	}
	symbolIDs := make(map[string]identity.SymbolID)
	for _, symbol := range sink.symbols {
		if symbol.Name == "increment" || symbol.Name == "run" || symbol.Name == "Counter" {
			symbolIDs[symbol.Name] = symbol.ID
		}
	}
	for _, name := range []string{"increment", "run", "Counter"} {
		if symbolIDs[name] == "" {
			t.Fatalf("C declaration %q was not exported; symbols=%+v", name, sink.symbols)
		}
	}
	mainURI, implURI, headerURI := uriFor("main.c"), uriFor("impl.c"), uriFor("api.h")
	mainReference, headerDeclaration := false, false
	mainDefinition, implDefinition := false, false
	for _, occurrence := range sink.occurrences {
		if occurrence.ScopeID != scope.ID || occurrence.BuildContext != scope.BuildContext {
			t.Fatalf("C occurrence escaped its translation-unit compile context: %+v scope=%+v", occurrence, scope)
		}
		if occurrence.URI == mainURI && occurrence.Role == "reference" && occurrence.SymbolID == symbolIDs["increment"] {
			if occurrence.Range.EndLine != occurrence.Range.StartLine ||
				occurrence.Range.EndChar <= occurrence.Range.StartChar ||
				occurrence.Range.EndChar-occurrence.Range.StartChar != uint32(len("increment")) {
				t.Fatalf("C function reference lost its exact identifier range: %+v", occurrence)
			}
			mainReference = true
		}
		if occurrence.URI == headerURI && occurrence.Role == "declaration" && occurrence.SymbolID == symbolIDs["Counter"] {
			headerDeclaration = true
		}
		if occurrence.URI == mainURI && occurrence.Role == "definition" && occurrence.SymbolID == symbolIDs["run"] {
			mainDefinition = true
		}
		if occurrence.URI == implURI && occurrence.Role == "definition" && occurrence.SymbolID == symbolIDs["increment"] {
			implDefinition = true
		}
	}
	if !mainReference || !headerDeclaration || !mainDefinition || !implDefinition {
		t.Fatalf("C source facts missing: main reference=%v header declaration=%v main definition=%v impl definition=%v occurrences=%+v",
			mainReference, headerDeclaration, mainDefinition, implDefinition, sink.occurrences)
	}
	mainInclude, implInclude, mainCall, mainTypeRelation := false, false, false, false
	for _, edge := range sink.edges {
		if edge.ScopeID != scope.ID || edge.BuildContext != scope.BuildContext {
			t.Fatalf("C edge escaped its compile context: %+v scope=%+v", edge, scope)
		}
		switch {
		case edge.Kind == model.EdgeInclude && edge.SourceURI == mainURI && edge.To == fileSymbolID(headerURI):
			mainInclude = true
		case edge.Kind == model.EdgeInclude && edge.SourceURI == implURI && edge.To == fileSymbolID(headerURI):
			implInclude = true
		case edge.Kind == model.EdgeCall && edge.SourceURI == mainURI && edge.To == symbolIDs["increment"]:
			mainCall = true
		case edge.Kind == model.EdgeTypeRelation && edge.SourceURI == mainURI && edge.To == symbolIDs["Counter"]:
			mainTypeRelation = true
		}
	}
	if !mainInclude || !implInclude || !mainCall || !mainTypeRelation {
		t.Fatalf("C semantic relationships missing: includes=(%v,%v) call=%v type=%v edges=%+v",
			mainInclude, implInclude, mainCall, mainTypeRelation, sink.edges)
	}
	for _, fact := range []model.FactKind{
		model.FactSymbol, model.FactDeclaration, model.FactDefinition,
		model.FactReference, model.FactImplementation, model.FactTypeRelation,
		model.FactCall, model.FactInclude, model.FactGenerated,
	} {
		assertCoverageState(t, report.Coverage, scope.ID, fact, model.Complete, "")
	}
	assertCoverageState(t, report.Coverage, scope.ID, model.FactImport, model.Unknown, "")
	assertCoverageState(t, report.Coverage, scope.ID, model.FactModule, model.Unknown, "")
}

func TestExternalForcedIncludeChangeKeepsBuildContextIncomplete(t *testing.T) {
	compiler, ok := pinnedLLVM22Compiler(t)
	if !ok {
		t.Skip("pinned LLVM 22.1.5 is not installed")
	}
	logicalRoot := t.TempDir()
	snapshotRoot := t.TempDir()
	externalRoot := t.TempDir()
	cpathBefore := t.TempDir()
	t.Setenv("CPATH", cpathBefore)
	rootURI := workspaceuri.FromPath(logicalRoot).Canonical()
	externalHeader := filepath.Join(externalRoot, "semantic_config.hpp")
	writeHeader := func(enabled bool) {
		t.Helper()
		value := "0"
		if enabled {
			value = "1"
		}
		if err := os.WriteFile(externalHeader, []byte("#define EXTERNAL_GATE "+value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeHeader(true)
	mainFile := filepath.Join(logicalRoot, "main.cpp")
	args := []string{compiler, "-std=c++17", "-include", externalHeader, "-c", mainFile,
		"-o", filepath.Join(logicalRoot, "obj", "main.obj")}
	database, err := json.Marshal([]map[string]any{{
		"directory": logicalRoot,
		"file":      mainFile,
		"arguments": args,
	}})
	if err != nil {
		t.Fatal(err)
	}
	view := newTestView(t, rootURI, logicalRoot, snapshotRoot, map[string][]byte{
		"main.cpp": []byte(`#if EXTERNAL_GATE
int enabled_by_external_header() { return 1; }
#else
int disabled_by_external_header() { return 0; }
#endif
`),
		"compile_commands.json": database,
	})
	backend := identity.BackendID{Language: "cpp", Name: "ccls-external-input-test"}
	buildAndExtract := func() (model.Request, *memorySink, model.Report) {
		t.Helper()
		request, err := BuildIndexRequest(context.Background(), view, rootURI, backend, 1)
		if err != nil {
			t.Fatal(err)
		}
		sink := &memorySink{}
		report, err := Extract(context.Background(), request, sink)
		if err != nil {
			t.Fatal(err)
		}
		if err := model.ValidateReport(request, report); err != nil {
			t.Fatalf("external-input report validation failed: %v", err)
		}
		return request, sink, report
	}
	firstRequest, firstSink, firstReport := buildAndExtract()
	writeHeader(false)
	secondRequest, secondSink, secondReport := buildAndExtract()
	if len(firstRequest.Scopes) != 1 || len(secondRequest.Scopes) != 1 {
		t.Fatalf("forced-include fixture produced %d and %d contexts; want one each", len(firstRequest.Scopes), len(secondRequest.Scopes))
	}
	if firstRequest.Scopes[0].BuildContext != secondRequest.Scopes[0].BuildContext {
		t.Fatalf("external header contents unexpectedly changed build context: %q vs %q", firstRequest.Scopes[0].BuildContext, secondRequest.Scopes[0].BuildContext)
	}
	if got := firstRequest.Scopes[0].Build.Environment["CPATH"]; got != cpathBefore {
		t.Fatalf("build context did not capture CPATH: got %q want %q", got, cpathBefore)
	}
	cpathAfter := t.TempDir()
	t.Setenv("CPATH", cpathAfter)
	thirdRequest, err := BuildIndexRequest(context.Background(), view, rootURI, backend, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(thirdRequest.Scopes) != 1 || thirdRequest.Scopes[0].Build.Environment["CPATH"] != cpathAfter {
		t.Fatalf("new CPATH was not captured by the third request: %#v", thirdRequest.Scopes)
	}
	if thirdRequest.Scopes[0].BuildContext == firstRequest.Scopes[0].BuildContext {
		t.Fatal("changing captured CPATH did not change build context")
	}
	containsSymbol := func(sink *memorySink, name string) bool {
		for _, symbol := range sink.symbols {
			if symbol.Name == name {
				return true
			}
		}
		return false
	}
	if !containsSymbol(firstSink, "enabled_by_external_header") || !containsSymbol(secondSink, "disabled_by_external_header") {
		t.Fatalf("external header mutation did not change exported source facts: before=%+v after=%+v", firstSink.symbols, secondSink.symbols)
	}
	for label, report := range map[string]model.Report{"before": firstReport, "after": secondReport} {
		for _, fact := range []model.FactKind{
			model.FactSymbol, model.FactDeclaration, model.FactDefinition,
			model.FactReference, model.FactImplementation, model.FactTypeRelation,
			model.FactCall, model.FactInclude,
		} {
			found := false
			for _, coverage := range report.Coverage {
				if coverage.Fact != fact {
					continue
				}
				found = true
				if coverage.State != model.IncompleteKnownSubset || !strings.Contains(coverage.Reason, "outside the captured workspace view") {
					t.Fatalf("%s forced-include dependency left %s coverage complete: %+v", label, fact, coverage)
				}
				break
			}
			if !found {
				t.Fatalf("%s forced-include report omitted %s coverage", label, fact)
			}
		}
	}
}

func TestModuleImportWithoutAttestedIdentityExportsNoEdge(t *testing.T) {
	compiler, ok := pinnedLLVM22Compiler(t)
	if !ok {
		t.Skip("pinned LLVM 22.1.5 is not installed")
	}
	dir := t.TempDir()
	moduleArtifacts := t.TempDir()
	moduleSource := filepath.Join(moduleArtifacts, "mathmod.cppm")
	pcm := filepath.Join(moduleArtifacts, "mathmod.pcm")
	importer := filepath.Join(dir, "use.cpp")
	if err := os.WriteFile(moduleSource, []byte("export module mathmod;\nexport int answer() { return 42; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(importer, []byte("import mathmod;\nint use() { return answer(); }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	buildPCM := exec.Command(compiler, "-std=c++20", "--precompile", moduleSource, "-o", pcm)
	if output, err := buildPCM.CombinedOutput(); err != nil {
		t.Fatalf("build pinned C++20 module PCM: %v (%s)", err, output)
	}
	if !fileExists(pcm) {
		t.Fatal("pinned clang did not produce the requested module PCM")
	}

	libclang := filepath.Join(filepath.Dir(compiler), "libclang.dll")
	if !fileExists(libclang) {
		t.Skip("pinned LLVM 22.1.5 libclang.dll is not installed")
	}
	artifact, err := buildHelper(context.Background(), pinned{
		compiler: model.ToolIdentity{Path: compiler},
		libclang: model.ToolIdentity{Path: libclang},
	}, nil)
	if err != nil {
		t.Fatalf("build pinned libclang helper: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(artifact.dir) })

	command := exec.Command(artifact.path,
		"--root", dir,
		"--source", importer,
		"--",
		"-std=c++20",
		"-fmodule-file=mathmod="+pcm,
		"-fsyntax-only",
	)
	command.Dir = dir
	command.Env = withSearchPath(os.Environ(), artifact.libDir)
	stdout, err := command.Output()
	if err != nil {
		t.Fatalf("run pinned helper on real module import: %v", err)
	}
	var unresolved *rawEvent
	for _, line := range strings.Split(strings.TrimSpace(string(stdout)), "\n") {
		if line == "" {
			continue
		}
		var event rawEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode helper event %q: %v", line, err)
		}
		if event.Type == "diagnostic" && event.Severity >= 3 {
			t.Fatalf("pinned libclang rejected the C++20 module fixture: %+v", event)
		}
		if event.Type == "unresolved_module" {
			if unresolved != nil {
				t.Fatalf("module import produced multiple unresolved events: first=%+v next=%+v", *unresolved, event)
			}
			unresolved = &event
		}
		if event.Type == "edge" && (event.Target == "module:mathmod" || event.EdgeKind == "import" || event.EdgeKind == "module") {
			t.Fatalf("helper exported a module relationship without an attested source/PCM identity: %+v", event)
		}
	}
	if unresolved == nil || unresolved.Path != "use.cpp" || unresolved.Target != "" || !strings.Contains(unresolved.Message, "attested immutable source or PCM identity") {
		t.Fatalf("real CXCursor_ModuleImportDecl did not produce the explicit unresolved identity event: %+v; helper output=%s", unresolved, stdout)
	}

	coverage := newCoverage("module-import")
	analysis := analyzeState{coverage: coverage, workspace: &workspace{}}
	if err := analysis.addEvent(*unresolved, model.Scope{}, nil); err != nil {
		t.Fatalf("record unresolved module cursor: %v", err)
	}
	if err := analysis.addEvent(rawEvent{
		Type: "edge", USR: "file:use.cpp", Target: "module:mathmod", EdgeKind: "import",
	}, model.Scope{}, nil); err != nil {
		t.Fatalf("reject legacy name-only module edge: %v", err)
	}
	analysis.finish()
	assertCoverageState(t, coverage.results(), "module-import", model.FactImport, model.Unknown, "attested immutable source or PCM identity")
	assertCoverageState(t, coverage.results(), "module-import", model.FactModule, model.Unknown, "attested immutable source or PCM identity")
}

func TestHelperDependencyContentChangesExtractorBuildIdentity(t *testing.T) {
	dependency := filepath.Join(t.TempDir(), "clang_helper_input.hpp")
	if err := os.WriteFile(dependency, []byte("#define HELPER_INPUT 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	firstDigest, err := helperDependencyContentDigest([]string{dependency})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dependency, []byte("#define HELPER_INPUT 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondDigest, err := helperDependencyContentDigest([]string{dependency})
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("changing a helper compile dependency did not change its digest")
	}
	scope := model.Scope{ID: "clang-test", Language: "cpp", RootURI: "file:///workspace"}
	buildContext := func(digest string) string {
		version := ExtractorVersionPinned() + "+headers-sha256:" + digest
		return string(model.ComputeBuildContextID(scope, ExtractorName, version, "clang/test", nil))
	}
	if buildContext(firstDigest) == buildContext(secondDigest) {
		t.Fatal("changing a helper compile dependency did not change build context")
	}
}

func TestCompilerEnvironmentDisablesDriverConfigAndCapturesIncludePaths(t *testing.T) {
	got := compilerEnvironment(
		[]string{"CLANG_CONFIG_FILE=ambient.cfg", "CPATH=ambient"},
		map[string]string{"CLANG_CONFIG_FILE": "captured.cfg", "CPATH": "captured"},
		"",
	)
	configFound := false
	cpath := ""
	for _, item := range got {
		if strings.HasPrefix(strings.ToUpper(item), "CLANG_CONFIG_FILE=") {
			configFound = true
		}
		if strings.HasPrefix(strings.ToUpper(item), "CPATH=") {
			cpath = item[strings.IndexByte(item, '=')+1:]
		}
	}
	if configFound {
		t.Fatal("ambient clang driver config was not disabled")
	}
	if cpath != "captured" {
		t.Fatalf("captured include path did not replace the ambient value: %q", cpath)
	}
}

func TestMissingCompileDatabaseIsUnavailable(t *testing.T) {
	root := t.TempDir()
	rootURI := workspaceuri.FromPath(root).Canonical()
	view := newTestView(t, rootURI, root, t.TempDir(), map[string][]byte{"readme.txt": []byte("no build database")})
	request, err := BuildIndexRequest(context.Background(), view, rootURI, identity.BackendID{Language: "cpp", Name: "ccls-test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Scopes) != 1 || len(request.Provenance[request.Scopes[0].ID].Tools) != 0 {
		t.Fatalf("missing database request should have one unpinned unavailable scope: %#v", request.Scopes)
	}
	report, err := Extract(context.Background(), request, &memorySink{})
	if err != nil {
		t.Fatal(err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("missing database report invalid: %v", err)
	}
	for _, coverage := range report.Coverage {
		if coverage.State != model.Unavailable || !strings.Contains(coverage.Reason, "compile_commands.json") {
			t.Fatalf("unexpected missing database coverage: %+v", coverage)
		}
	}
	if view.materializeCount != 0 {
		t.Fatalf("missing database should not materialize snapshot; got %d calls", view.materializeCount)
	}
}

func TestBareCompilerCommandDoesNotResolveThroughPath(t *testing.T) {
	root := t.TempDir()
	rootURI := workspaceuri.FromPath(root).Canonical()
	compilerCommand, _ := json.Marshal([]map[string]any{{
		"directory": root,
		"file":      filepath.Join(root, "main.cpp"),
		"arguments": []string{"clang++", "-std=c++17", "-c", filepath.Join(root, "main.cpp")},
	}})
	view := newTestView(t, rootURI, root, t.TempDir(), map[string][]byte{
		"main.cpp":              []byte("int main() { return 0; }\n"),
		"compile_commands.json": compilerCommand,
	})
	request, err := BuildIndexRequest(context.Background(), view, rootURI, identity.BackendID{Language: "cpp", Name: "ccls-test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Extract(context.Background(), request, &memorySink{})
	if err != nil {
		t.Fatal(err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("bare compiler report invalid: %v", err)
	}
	for _, coverage := range report.Coverage {
		if coverage.State != model.Unknown || !strings.Contains(coverage.Reason, "PATH lookup is not allowed") {
			t.Fatalf("bare compiler should be explicitly unknown without PATH lookup: %+v", coverage)
		}
	}
	if view.materializeCount != 0 {
		t.Fatalf("unresolved compiler should not materialize a workspace; calls=%d", view.materializeCount)
	}
}

func TestUnmappedConstructorReferenceDowngradesCoverage(t *testing.T) {
	coverage := newCoverage("cpp-context")
	analysis := analyzeState{coverage: coverage}
	if err := analysis.addEvent(rawEvent{
		Type:    "incomplete_call",
		Message: "constructor reference has no exact source token mapping",
	}, model.Scope{}, nil); err != nil {
		t.Fatalf("record incomplete constructor reference: %v", err)
	}
	referenceIncomplete, callIncomplete := false, false
	for _, item := range coverage.results() {
		if item.Fact == model.FactReference {
			if item.State != model.IncompleteKnownSubset || !strings.Contains(item.Reason, "exact source token") {
				t.Fatalf("unmapped constructor reference did not block complete coverage: %+v", item)
			}
			referenceIncomplete = true
		}
		if item.Fact == model.FactCall {
			if item.State != model.IncompleteKnownSubset || !strings.Contains(item.Reason, "exact source token") {
				t.Fatalf("unmapped constructor call did not block complete coverage: %+v", item)
			}
			callIncomplete = true
		}
	}
	if !referenceIncomplete || !callIncomplete {
		t.Fatalf("unmapped constructor did not report partial reference/call coverage: reference=%v call=%v", referenceIncomplete, callIncomplete)
	}
}

func TestLostSemanticRelationIdentityDowngradesCoverage(t *testing.T) {
	tests := []struct {
		name  string
		event rawEvent
		facts []model.FactKind
	}{
		{
			name:  "constructor caller",
			event: rawEvent{Type: "incomplete_call", Message: "constructor call has no stable caller identity"},
			facts: []model.FactKind{model.FactReference, model.FactCall},
		},
		{
			name:  "type relation owner or target",
			event: rawEvent{Type: "incomplete_type_relation", Message: "type relation has no stable owner or target identity"},
			facts: []model.FactKind{model.FactTypeRelation},
		},
		{
			name:  "override source or target",
			event: rawEvent{Type: "incomplete_implementation", Message: "override relation has no stable source or target identity"},
			facts: []model.FactKind{model.FactImplementation},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coverage := newCoverage("lost-identity")
			analysis := analyzeState{coverage: coverage}
			if err := analysis.addEvent(test.event, model.Scope{}, nil); err != nil {
				t.Fatalf("record incomplete semantic relation: %v", err)
			}
			for _, fact := range test.facts {
				assertCoverageState(t, coverage.results(), "lost-identity", fact, model.IncompleteKnownSubset, test.event.Message)
			}
		})
	}
}

func TestTranslationUnitCancellationKillsAndWaitsForChild(t *testing.T) {
	compiler, ok := pinnedLLVM22Compiler(t)
	if !ok {
		t.Skip("pinned LLVM 22.1.5 is not installed")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "sleeper.cpp")
	executable := filepath.Join(dir, "sleeper.exe")
	if err := os.WriteFile(source, []byte(`#include <chrono>
#include <thread>
int main() { std::this_thread::sleep_for(std::chrono::seconds(5)); }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command(compiler, "-std=c++17", source, "-o", executable)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cancellation child: %v (%s)", err, output)
	}
	ctx, cancel := context.WithCancel(context.Background())
	artifact := &helperArtifact{path: executable, dir: dir, libDir: filepath.Dir(compiler)}
	scope := model.Scope{ID: "cancel", Language: "cpp", BuildContext: "cancel"}
	workspace := &workspace{root: dir}
	analysis := analyzeState{coverage: newCoverage(scope.ID), workspace: workspace, seenSource: make(map[string]bool)}
	writer := newFactWriter(ctx, &memorySink{}, scope)
	done := make(chan error, 1)
	go func() {
		done <- runTranslationUnit(ctx, artifact, scope, compileCommand{directory: dir, file: source}, workspace, &analysis, writer)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled child returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled helper did not terminate and get waited within 2 seconds")
	}
}

func pinnedLLVM22Compiler(t *testing.T) (string, bool) {
	t.Helper()
	path := os.Getenv("OMNILSP_LLVM22_HOME")
	if path == "" {
		path = `D:\Program Files\LLVM-22\clang+llvm-22.1.5-x86_64-pc-windows-msvc`
	}
	compiler := filepath.Join(path, "bin", "clang++.exe")
	if runtime.GOOS != "windows" || !fileExists(compiler) {
		return "", false
	}
	return compiler, true
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

type testView struct {
	rootURI          string
	logicalRoot      string
	snapshotRoot     string
	files            map[string]model.File
	data             map[string][]byte
	materializeCount int
	closeCount       int
}

func newTestView(t *testing.T, rootURI, logicalRoot, snapshotRoot string, data map[string][]byte) *testView {
	t.Helper()
	view := &testView{rootURI: rootURI, logicalRoot: logicalRoot, snapshotRoot: snapshotRoot, files: make(map[string]model.File), data: make(map[string][]byte)}
	for relative, contents := range data {
		fileURI := workspaceuri.FromPath(filepath.Join(logicalRoot, filepath.FromSlash(relative))).Canonical()
		copyContents := append([]byte(nil), contents...)
		sum := sha256.Sum256(copyContents)
		view.data[fileURI] = copyContents
		view.files[fileURI] = model.File{
			URI: fileURI, LanguageID: "cpp", Size: int64(len(copyContents)),
			SHA256: identity.ContentHash("sha256:" + hex.EncodeToString(sum[:])),
		}
		destination := filepath.Join(snapshotRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destination, copyContents, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	return view
}

func (v *testView) Identity() model.Identity {
	return model.Identity{Workspace: "clang-test", DiskDigest: "sha256:test-snapshot", SnapshotRev: 7}
}

func (v *testView) Walk(ctx context.Context, rootURI string, visit func(model.File) error) error {
	if rootURI != v.rootURI {
		return errors.New("unexpected root URI")
	}
	for _, file := range v.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func (v *testView) Read(ctx context.Context, fileURI string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := v.data[fileURI]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(string(data))), nil
}

func (v *testView) Materialize(ctx context.Context, rootURI, destination string) (model.MaterializedView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if rootURI != v.rootURI || pathWithin(v.logicalRoot, destination) {
		return nil, errors.New("materialize destination is invalid")
	}
	v.materializeCount++
	return &testMaterializedView{owner: v, rootURI: rootURI, rootPath: v.snapshotRoot}, nil
}

type testMaterializedView struct {
	owner    *testView
	rootURI  string
	rootPath string
}

func (v *testMaterializedView) RootURI() string  { return v.rootURI }
func (v *testMaterializedView) RootPath() string { return v.rootPath }
func (v *testMaterializedView) Close() error {
	v.owner.closeCount++
	return nil
}
func (v *testMaterializedView) PathForURI(uri string) (string, error) {
	file, ok := v.owner.files[uri]
	if !ok {
		return "", os.ErrNotExist
	}
	logical, err := workspaceuri.Parse(file.URI)
	if err != nil {
		return "", err
	}
	logicalPath, err := logical.Path()
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(v.owner.logicalRoot, logicalPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", os.ErrNotExist
	}
	return filepath.Join(v.rootPath, rel), nil
}

type memorySink struct {
	symbols     []model.Symbol
	occurrences []model.Occurrence
	edges       []model.Edge
}

func assertCoverageState(t *testing.T, coverage []model.Coverage, scopeID string, fact model.FactKind, want model.Completeness, reasonPart string) {
	t.Helper()
	item, ok := findCoverage(coverage, scopeID, fact)
	if !ok {
		t.Fatalf("coverage omitted scope=%q fact=%q", scopeID, fact)
	}
	if item.State != want || reasonPart != "" && !strings.Contains(item.Reason, reasonPart) {
		t.Fatalf("coverage scope=%q fact=%q = %+v, want state=%q reason containing %q", scopeID, fact, item, want, reasonPart)
	}
}

func findCoverage(coverage []model.Coverage, scopeID string, fact model.FactKind) (model.Coverage, bool) {
	for _, item := range coverage {
		if item.ScopeID == scopeID && item.Fact == fact {
			return item, true
		}
	}
	return model.Coverage{}, false
}

func (s *memorySink) WriteSymbols(ctx context.Context, values []model.Symbol) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.symbols = append(s.symbols, values...)
	return nil
}

func (s *memorySink) WriteOccurrences(ctx context.Context, values []model.Occurrence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.occurrences = append(s.occurrences, values...)
	return nil
}

func (s *memorySink) WriteEdges(ctx context.Context, values []model.Edge) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.edges = append(s.edges, values...)
	return nil
}
