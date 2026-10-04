package typescript

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
)

func TestDirectCompilerRunnerUsesPinnedProgramAndUTF16Ranges(t *testing.T) {
	tools := localDirectTools(t)

	root := "file:///repo/direct"
	configURI := root + "/tsconfig.json"
	baseURI := root + "/base.ts"
	useURI := root + "/use.ts"
	config := []byte(`{"compilerOptions":{"target":"ES2020","strict":true},"include":["*.ts"]}`)
	base := []byte("export interface Base { run(): void; }\r\n")
	use := []byte("const emoji = \"😀\";\r\nimport { Base } from './base';\r\nclass Child implements Base { run() {} }\r\nconst value: Base = new Child(); value.run();\r\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "direct-typescript", DiskDigest: "sha256:direct", SnapshotRev: 1},
		files: map[string][]model.File{root: {
			semanticFile(configURI, "json", config), semanticFile(baseURI, "typescript", base), semanticFile(useURI, "typescript", use),
		}},
		content: map[string][]byte{configURI: config, baseURI: base, useURI: use},
	}
	configDigest := sha256.Sum256(config)
	scope := model.Scope{
		ID: "direct", Language: "typescript", RootURI: root,
		Build: model.BuildInputs{Options: map[string]string{
			"tsconfig": "tsconfig.json", "projectConfigDigest": "sha256:" + hex.EncodeToString(configDigest[:]),
			"compilerOptions": string(`{"target":"ES2020","strict":true}`), "projectReferences": "[]", "plugins": "[]",
		}},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: "typescript", Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope

	sink := &semanticTestSink{}
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), model.Request{
		View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, sink)
	if err != nil {
		t.Fatalf("direct ExportIndex: %v", err)
	}
	if err := model.ValidateReport(model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	if len(sink.symbols) == 0 || len(sink.occurrences) == 0 {
		t.Fatalf("compiler exporter emitted no symbols/occurrences: %d/%d", len(sink.symbols), len(sink.occurrences))
	}
	if sink.maxBatch > semanticIndexBatchLimit {
		t.Fatalf("compiler exporter exceeded bounded batch size: %d", sink.maxBatch)
	}
	var emojiReference bool
	for _, occurrence := range sink.occurrences {
		if occurrence.URI == useURI && occurrence.Role == "reference" && occurrence.Range.StartLine == 3 && occurrence.Range.StartChar > 20 {
			emojiReference = true
		}
	}
	if !emojiReference {
		t.Fatalf("UTF-16-aware reference occurrence missing: %+v", sink.occurrences)
	}
	var baseID identity.SymbolID
	for _, occurrence := range sink.occurrences {
		if occurrence.URI == baseURI && occurrence.Role == "declaration" {
			for _, symbol := range sink.symbols {
				if symbol.ID == occurrence.SymbolID && symbol.Name == "Base" {
					baseID = symbol.ID
				}
			}
		}
	}
	if baseID == "" {
		t.Fatalf("compiler declaration/reference identity for Base was not emitted: symbols=%+v occurrences=%+v", sink.symbols, sink.occurrences)
	}
	var baseDefinition bool
	var childDefinition bool
	for _, occurrence := range sink.occurrences {
		if occurrence.SymbolID == baseID && occurrence.Role == "definition" {
			baseDefinition = true
		}
		if occurrence.URI == useURI && occurrence.Role == "definition" {
			for _, symbol := range sink.symbols {
				if symbol.ID == occurrence.SymbolID && symbol.Name == "Child" {
					childDefinition = true
				}
			}
		}
	}
	if baseDefinition || !childDefinition {
		t.Fatalf("declaration/definition roles were not distinguished (interface definition=%v, class definition=%v): %+v", baseDefinition, childDefinition, sink.occurrences)
	}
	var baseReference bool
	for _, occurrence := range sink.occurrences {
		if occurrence.URI == useURI && occurrence.SymbolID == baseID && occurrence.Role == "reference" {
			baseReference = true
		}
	}
	if !baseReference {
		t.Fatalf("Base reference did not resolve to the declaration's compiler symbol: %+v", sink.occurrences)
	}
	var implementationEdge bool
	var importEdge bool
	var typeRelationEdge bool
	for _, edge := range sink.edges {
		if edge.Kind == model.EdgeImplementation && edge.To == baseID {
			implementationEdge = true
		}
		if edge.Kind == model.EdgeImport {
			importEdge = true
		}
		if edge.Kind == model.EdgeTypeRelation && edge.To == baseID {
			typeRelationEdge = true
		}
	}
	if !implementationEdge {
		t.Fatalf("compiler heritage relationship was not emitted: %+v", sink.edges)
	}
	if !importEdge {
		t.Fatalf("compiler import relationship was not emitted: %+v", sink.edges)
	}
	if !typeRelationEdge {
		t.Fatalf("compiler type annotation relationship was not emitted: %+v", sink.edges)
	}
	if got := coverageState(report.Coverage, scope.ID, model.FactInclude); got != model.Unavailable {
		t.Fatalf("include coverage = %s, want unavailable", got)
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactImplementation, model.FactTypeRelation} {
		if got := coverageState(report.Coverage, scope.ID, fact); got != model.Complete {
			t.Fatalf("TypeScript coverage %s = %s, want complete (all files are in one Program): %s", fact, got, strings.Join(report.Errors, "; "))
		}
	}
	for _, fact := range []model.FactKind{model.FactImport, model.FactModule} {
		if got := coverageState(report.Coverage, scope.ID, fact); got != model.IncompleteKnownSubset {
			t.Fatalf("TypeScript coverage %s = %s, want module-only subset after an unimplemented interface call: %s", fact, got, coverageReason(report.Coverage, scope.ID, fact))
		}
	}
}

func TestDirectCompilerRunnerKeepsStableModuleGraphIdentityAndInterfaceDeclarationOnly(t *testing.T) {
	tools := localDirectTools(t)
	root := "file:///repo/direct-module-identity"
	configURI := root + "/tsconfig.json"
	contractURI := root + "/contract.ts"
	consumerURI := root + "/consumer.ts"
	config := []byte(`{"compilerOptions":{"target":"ES2020","strict":true},"include":["*.ts"]}`)
	contract := []byte("export interface Contract { run(): void; }\n")
	consumer := []byte("import { Contract } from './contract';\nexport class Client implements Contract { run(): void {} }\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "direct-module-identity", DiskDigest: "sha256:direct-module-identity", SnapshotRev: 1},
		files: map[string][]model.File{root: {
			semanticFile(configURI, "json", config), semanticFile(contractURI, "typescript", contract), semanticFile(consumerURI, "typescript", consumer),
		}},
		content: map[string][]byte{configURI: config, contractURI: contract, consumerURI: consumer},
	}
	configDigest := sha256.Sum256(config)
	scope := model.Scope{
		ID: "direct-module-identity", Language: "typescript", RootURI: root,
		Build: model.BuildInputs{Options: map[string]string{
			"tsconfig": "tsconfig.json", "projectConfigDigest": "sha256:" + hex.EncodeToString(configDigest[:]),
			"compilerOptions": `{"target":"ES2020","strict":true}`, "projectReferences": "[]", "plugins": "[]",
		}},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: "typescript", Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	request := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	provider := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools})

	export := func() (*semanticTestSink, model.Report) {
		t.Helper()
		sink := &semanticTestSink{}
		report, err := provider.ExportIndex(context.Background(), request, sink)
		if err != nil {
			t.Fatalf("direct ExportIndex: %v", err)
		}
		if err := model.ValidateReport(request, report); err != nil {
			t.Fatalf("ValidateReport: %v", err)
		}
		return sink, report
	}
	first, _ := export()
	second, _ := export()

	type moduleNode struct {
		id   identity.SymbolID
		name string
	}
	moduleNodes := func(sink *semanticTestSink) map[string]moduleNode {
		t.Helper()
		got := make(map[string]moduleNode)
		for _, symbol := range sink.symbols {
			if symbol.Kind != "module_graph_node" {
				continue
			}
			if symbol.Name != contractURI && symbol.Name != consumerURI {
				t.Errorf("module graph name is not a stable logical URI: %+v", symbol)
			}
			if _, duplicate := got[symbol.Name]; duplicate {
				t.Errorf("duplicate module graph node for %q", symbol.Name)
			}
			got[symbol.Name] = moduleNode{id: symbol.ID, name: symbol.Name}
		}
		if len(got) != 2 {
			t.Errorf("module graph nodes = %+v, want both logical source files", got)
		}
		return got
	}
	firstModules := moduleNodes(first)
	secondModules := moduleNodes(second)
	for _, uri := range []string{contractURI, consumerURI} {
		if firstModules[uri].id == "" || firstModules[uri].id != secondModules[uri].id {
			t.Errorf("module ID for %q changed across fresh materializations: first=%q second=%q", uri, firstModules[uri].id, secondModules[uri].id)
		}
	}

	var contractID identity.SymbolID
	for _, symbol := range first.symbols {
		if symbol.Name == "Contract" && symbol.Kind == "interface" {
			contractID = symbol.ID
		}
	}
	if contractID == "" {
		t.Fatalf("real compiler did not emit the interface symbol: %+v", first.symbols)
	}
	var contractDeclaration, contractDefinition bool
	for _, occurrence := range first.occurrences {
		if occurrence.SymbolID != contractID || occurrence.URI != contractURI {
			continue
		}
		contractDeclaration = contractDeclaration || occurrence.Role == "declaration"
		contractDefinition = contractDefinition || occurrence.Role == "definition"
	}
	if !contractDeclaration || contractDefinition {
		t.Fatalf("interface must retain declaration-only occurrence roles: declaration=%v definition=%v occurrences=%+v", contractDeclaration, contractDefinition, first.occurrences)
	}

	moduleEdges := map[model.EdgeKind]bool{}
	for _, edge := range first.edges {
		if edge.SourceURI == consumerURI && edge.From == firstModules[consumerURI].id && edge.To == firstModules[contractURI].id {
			moduleEdges[edge.Kind] = true
		}
	}
	if !moduleEdges[model.EdgeModule] || !moduleEdges[model.EdgeImport] {
		t.Fatalf("module graph relationships were not preserved: module/import=%v/%v edges=%+v", moduleEdges[model.EdgeModule], moduleEdges[model.EdgeImport], first.edges)
	}
}

func TestDirectCompilerRunnerQualifiesClosedStaticTypeScriptModuleFacts(t *testing.T) {
	sources := map[string][]byte{
		"math.ts":   []byte("export type Value = number;\nexport function increment(value: Value): Value { return value + 1; }\n"),
		"barrel.ts": []byte("export { increment } from './math';\nexport type { Value } from './math';\n"),
		"main.ts":   []byte("import { increment } from './barrel';\nimport type { Value } from './math';\nexport const result: Value = increment(1);\n"),
	}
	report, sink := exportDirectTypeScriptFixture(t, sources)
	for _, fact := range []model.FactKind{model.FactImport, model.FactModule} {
		if got := coverageState(report.Coverage, "direct-reference-negative", fact); got != model.Complete {
			t.Fatalf("closed static TypeScript coverage %s = %s, want complete: %+v", fact, got, report.Coverage)
		}
		reason := coverageReason(report.Coverage, "direct-reference-negative", fact)
		for _, proof := range []string{directTypeScriptStaticModuleProof, "typescript=6.0.3", "closedScopes=1", "projectReferences=0", "sourceFiles=3"} {
			if !strings.Contains(reason, proof) {
				t.Fatalf("static TypeScript coverage %s provenance %q omits %q", fact, reason, proof)
			}
		}
	}
	for _, fact := range []model.FactKind{model.FactCall, model.FactImplementation, model.FactTypeRelation} {
		if fact == model.FactCall && coverageState(report.Coverage, "direct-reference-negative", fact) == model.Complete {
			t.Fatalf("module proof promoted unrelated fact %s: %+v", fact, report.Coverage)
		}
	}
	moduleEdges, importEdges := 0, 0
	for _, edge := range sink.edges {
		if edge.Kind == model.EdgeModule {
			moduleEdges++
		}
		if edge.Kind == model.EdgeImport {
			importEdges++
		}
	}
	if moduleEdges < 3 || importEdges < 3 {
		t.Fatalf("pinned compiler omitted static import/re-export edges: module=%d import=%d edges=%+v", moduleEdges, importEdges, sink.edges)
	}
}

func TestDirectCompilerRunnerKeepsDynamicTypeScriptModuleLoadsPartial(t *testing.T) {
	cases := []struct {
		name            string
		source          string
		wantReason      string
		wantKnownSubset bool
		compilerOptions string
	}{
		{
			name:       "Function constructor",
			source:     "export const load = Function('return import(\"./hidden\")');\n",
			wantReason: "Function constructors",
		},
		{
			name:       "indirect Function constructor",
			source:     "export const make = (function () {}).constructor;\n",
			wantReason: "constructor access",
		},
		{
			name:       "Function constructor invocation",
			source:     "export const load = (() => {}).constructor('return import(\"./hidden\")')();\n",
			wantReason: "Function constructors",
		},
		{
			name:       "bracket Function constructor invocation",
			source:     "const obj = () => {};\nconst make = obj['constructor'];\nexport const load = make('return import(\"./hidden\")')();\n",
			wantReason: "Function constructors",
		},
		{
			name:       "aliased require",
			source:     "declare function require(path: string): void;\ndeclare const suffix: string;\nconst load = require;\nload('./' + suffix);\n",
			wantReason: "require and eval",
		},
		{
			name:            "aliased createRequire with nonliteral path",
			source:          "declare function createRequire(url: string): (path: string) => void;\ndeclare const suffix: string;\nconst create = createRequire(import.meta.url);\nconst load = create;\nload('./' + suffix);\n",
			wantReason:      "runtime module loaders and code-generation APIs",
			compilerOptions: `{"target":"ES2020","module":"ESNext","moduleResolution":"Bundler","strict":true}`,
		},
		{
			name:       "eval string code",
			source:     "eval(\"import('./hidden')\");\n",
			wantReason: "require and eval",
		},
		{
			name:       "string timer code",
			source:     "setTimeout(\"import('./hidden')\", 0);\n",
			wantReason: "string callbacks passed to timer APIs",
		},
		{
			name:       "worker module loader",
			source:     "new Worker('./worker.js', { type: 'module' });\n",
			wantReason: "a call target without a body in the captured TypeScript project",
		},
		{
			name:       "service worker registration",
			source:     "navigator.serviceWorker.register('./worker.js', { type: 'module' });\n",
			wantReason: "a call target without a body in the captured TypeScript project",
		},
		{
			name:       "WebAssembly runtime compilation",
			source:     "WebAssembly.compile(new ArrayBuffer(0));\n",
			wantReason: "a call target without a body in the captured TypeScript project",
		},
		{
			name:            "unimplemented callable declaration",
			source:          "declare const load: (path: string) => void;\nload('./hidden');\n",
			wantReason:      "call target or owner could not be linked to a captured TypeScript implementation",
			wantKnownSubset: true,
		},
		{
			name:   "nonliteral import",
			source: "declare const path: string;\nimport(path);\n",
		},
		{
			name:   "any call target",
			source: "declare const load: any;\nload('./hidden');\n",
		},
		{
			name:   "unknown cast call target",
			source: "declare const load: unknown;\n(load as (path: string) => void)('./hidden');\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := exportDirectTypeScriptFixture
			if tc.compilerOptions != "" {
				fixture = func(t *testing.T, sources map[string][]byte) (model.Report, *semanticTestSink) {
					return exportDirectTypeScriptFixtureWithOptions(t, sources, tc.compilerOptions)
				}
			}
			report, _ := fixture(t, map[string][]byte{
				"main.ts":   []byte(tc.source),
				"hidden.ts": []byte("export const hidden = 1;\n"),
			})
			for _, fact := range []model.FactKind{model.FactImport, model.FactModule} {
				got := coverageState(report.Coverage, "direct-reference-negative", fact)
				if got == model.Complete {
					t.Fatalf("dynamic module case %s unexpectedly has complete %s coverage: %+v", tc.name, fact, report.Coverage)
				}
				if tc.wantKnownSubset && got != model.IncompleteKnownSubset {
					t.Fatalf("dynamic module case %s %s coverage = %s, want known subset: %+v", tc.name, fact, got, report.Coverage)
				}
				if tc.wantReason != "" && !strings.Contains(coverageReason(report.Coverage, "direct-reference-negative", fact), tc.wantReason) {
					t.Fatalf("dynamic module case %s %s reason = %q, want substring %q", tc.name, fact, coverageReason(report.Coverage, "direct-reference-negative", fact), tc.wantReason)
				}
			}
		})
	}
}

func TestDirectCompilerRunnerExportsPrivateElementAccessAndImportEqualsReferences(t *testing.T) {
	tools := localDirectTools(t)
	root := "file:///repo/direct-static-reference-forms"
	configURI := root + "/tsconfig.json"
	moduleURI := root + "/module.ts"
	consumerURI := root + "/consumer.ts"
	config := []byte(`{"compilerOptions":{"target":"ES2020","module":"CommonJS","strict":true},"include":["*.ts"]}`)
	module := []byte("export = { value: 2 };\n")
	consumer := []byte(strings.Join([]string{
		"import moduleValue = require('./module');",
		"const dynamicModule = import('./module');",
		"export class Box {",
		"  #secret = 1;",
		"  field = 2;",
		"  read(other: Box) { return other.#secret + other[\"field\"] + other[`field`] + moduleValue.value; }",
		"}",
		"type NumericBox = { 0: number };",
		"export function readNumeric(other: NumericBox) { return other[0]; }",
		"const shorthandTarget = 1;",
		"const shorthandUse = { shorthandTarget };",
		"interface Shape { prop: string; }",
		"const shape: Shape = { prop: 'x' };",
		"function readShape({ prop }: Shape) { return prop; }",
		"shape.prop; shape[\"prop\"];",
		"export { shorthandTarget as publishedTarget };",
	}, "\n") + "\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "direct-static-reference-forms", DiskDigest: "sha256:direct-static-reference-forms", SnapshotRev: 1},
		files: map[string][]model.File{root: {
			semanticFile(configURI, "json", config), semanticFile(moduleURI, "typescript", module), semanticFile(consumerURI, "typescript", consumer),
		}},
		content: map[string][]byte{configURI: config, moduleURI: module, consumerURI: consumer},
	}
	configDigest := sha256.Sum256(config)
	scope := model.Scope{
		ID: "direct-static-reference-forms", Language: "typescript", RootURI: root,
		Build: model.BuildInputs{Options: map[string]string{
			"tsconfig": "tsconfig.json", "projectConfigDigest": "sha256:" + hex.EncodeToString(configDigest[:]),
			"compilerOptions": `{"target":"ES2020","module":"CommonJS","strict":true}`, "projectReferences": "[]", "plugins": "[]",
		}},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: "typescript", Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	request := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	sink := &semanticTestSink{}
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("direct ExportIndex: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}

	var secretID, fieldID, moduleGraphID identity.SymbolID
	for _, symbol := range sink.symbols {
		switch {
		case symbol.Name == "#secret":
			secretID = symbol.ID
		case symbol.Name == "field":
			fieldID = symbol.ID
		case symbol.Kind == "module_graph_node" && symbol.Name == moduleURI:
			moduleGraphID = symbol.ID
		}
	}
	if secretID == "" || fieldID == "" || moduleGraphID == "" {
		t.Fatalf("real compiler identities missing private/property/module symbols: secret=%q field=%q module=%q symbols=%+v", secretID, fieldID, moduleGraphID, sink.symbols)
	}
	lineRange := func(lineNumber int, sourceLine, text string) model.Position {
		t.Helper()
		start := strings.Index(sourceLine, text)
		if start < 0 {
			t.Fatalf("fixture line %q does not contain %q", sourceLine, text)
		}
		return model.Position{StartLine: uint32(lineNumber), StartChar: uint32(start), EndLine: uint32(lineNumber), EndChar: uint32(start + len(text))}
	}
	consumerLines := strings.Split(string(consumer), "\n")
	lineRangeAt := func(lineNumber int, start int, text string) model.Position {
		t.Helper()
		if start < 0 || start+len(text) > len(consumerLines[lineNumber]) || consumerLines[lineNumber][start:start+len(text)] != text {
			t.Fatalf("fixture line %q does not contain %q at byte %d", consumerLines[lineNumber], text, start)
		}
		return model.Position{StartLine: uint32(lineNumber), StartChar: uint32(start), EndLine: uint32(lineNumber), EndChar: uint32(start + len(text))}
	}
	privateDeclarationRange := lineRange(3, consumerLines[3], "#secret")
	privateReferenceRange := lineRange(5, consumerLines[5], "#secret")
	elementAccessRange := lineRangeAt(5, strings.Index(consumerLines[5], "field"), "field")
	templateAccessRange := lineRangeAt(5, strings.LastIndex(consumerLines[5], "field"), "field")
	importEqualsRange := lineRange(0, consumerLines[0], "./module")
	dynamicImportRange := lineRange(1, consumerLines[1], "./module")
	numericReferenceRange := lineRange(8, consumerLines[8], "0")
	shorthandReferenceRange := lineRange(10, consumerLines[10], "shorthandTarget")
	propertyDeclarationRange := lineRange(11, consumerLines[11], "prop")
	propertyBindingRange := lineRange(13, consumerLines[13], "prop")

	privateDeclaration, privateDefinition, privateReference := false, false, false
	elementAccessReference, templateAccessReference, importEqualsReference, dynamicImportReference, numericReference := false, false, false, false, false
	var numericID, shorthandTargetID, shapePropertyID identity.SymbolID
	for _, symbol := range sink.symbols {
		if symbol.Name == "0" {
			numericID = symbol.ID
		}
	}
	for _, occurrence := range sink.occurrences {
		if occurrence.URI == consumerURI && occurrence.Range.StartLine == 9 && occurrence.Role == "declaration" {
			shorthandTargetID = occurrence.SymbolID
		}
		if occurrence.URI == consumerURI && occurrence.Range == propertyDeclarationRange && occurrence.Role == "declaration" {
			shapePropertyID = occurrence.SymbolID
		}
		if occurrence.URI != consumerURI {
			continue
		}
		if occurrence.SymbolID == secretID {
			switch occurrence.Role {
			case "declaration":
				privateDeclaration = occurrence.Range == privateDeclarationRange
			case "definition":
				privateDefinition = occurrence.Range == privateDeclarationRange
			case "reference":
				privateReference = occurrence.Range == privateReferenceRange
			}
		}
		if occurrence.SymbolID == fieldID && occurrence.Role == "reference" && occurrence.Range == elementAccessRange {
			elementAccessReference = true
		}
		if occurrence.SymbolID == fieldID && occurrence.Role == "reference" && occurrence.Range == templateAccessRange {
			templateAccessReference = true
		}
		if occurrence.SymbolID == moduleGraphID && occurrence.Role == "reference" && occurrence.Range == importEqualsRange {
			importEqualsReference = true
		}
		if occurrence.SymbolID == moduleGraphID && occurrence.Role == "reference" && occurrence.Range == dynamicImportRange {
			dynamicImportReference = true
		}
		if occurrence.SymbolID == numericID {
			numericReference = numericReference || occurrence.Role == "reference" && occurrence.Range == numericReferenceRange
		}
	}
	if shorthandTargetID == "" || shapePropertyID == "" {
		t.Fatalf("real compiler identities missing shorthand/property symbols: shorthand=%q property=%q symbols=%+v occurrences=%+v", shorthandTargetID, shapePropertyID, sink.symbols, sink.occurrences)
	}
	shorthandReference, bindingPropertyReference := false, false
	for _, occurrence := range sink.occurrences {
		if occurrence.URI != consumerURI || occurrence.Role != "reference" {
			continue
		}
		if occurrence.SymbolID == shorthandTargetID && occurrence.Range == shorthandReferenceRange {
			shorthandReference = true
		}
		if occurrence.SymbolID == shapePropertyID && occurrence.Range == propertyBindingRange {
			bindingPropertyReference = true
		}
	}
	if !privateDeclaration || !privateDefinition || !privateReference || !elementAccessReference || !templateAccessReference || !importEqualsReference || !dynamicImportReference || !numericReference || !shorthandReference || !bindingPropertyReference {
		t.Fatalf("static references are incomplete: private declaration/definition/reference=%v/%v/%v string/template elementAccess=%v/%v importEquals/dynamic=%v/%v numeric=%v shorthand=%v binding-property=%v occurrences=%+v", privateDeclaration, privateDefinition, privateReference, elementAccessReference, templateAccessReference, importEqualsReference, dynamicImportReference, numericReference, shorthandReference, bindingPropertyReference, sink.occurrences)
	}

	asserted := map[string][]model.Position{
		"field":     referenceRanges(sink.occurrences, consumerURI, fieldID),
		"module":    referenceRanges(sink.occurrences, consumerURI, moduleGraphID),
		"numeric":   referenceRanges(sink.occurrences, consumerURI, numericID),
		"shorthand": referenceRanges(sink.occurrences, consumerURI, shorthandTargetID),
		"property":  referenceRanges(sink.occurrences, consumerURI, shapePropertyID),
	}
	queries := map[string]int{
		"field":     strings.Index(string(consumer), "field =") + 1,
		"module":    strings.Index(string(consumer), "./module") + 1,
		"numeric":   strings.Index(string(consumer), "0: number"),
		"shorthand": strings.Index(string(consumer), "const shorthandTarget") + len("const "),
		"property":  strings.Index(string(consumer), "interface Shape { prop") + len("interface Shape { "),
	}
	oracle := pinnedLanguageServiceReferences(t, tools, consumer, module, queries)
	for label, expected := range asserted {
		got := oracle[label]
		if !sameRanges(expected, got) {
			t.Errorf("native exporter %s reference ranges differ from TypeScript 6.0.3 LanguageService: exporter=%v languageService=%v", label, expected, got)
		}
	}

	moduleEdge, importEdge := false, false
	for _, edge := range sink.edges {
		if edge.SourceURI == consumerURI && edge.To == moduleGraphID && edge.Range.StartLine == 0 {
			moduleEdge = moduleEdge || edge.Kind == model.EdgeModule
			importEdge = importEdge || edge.Kind == model.EdgeImport
		}
	}
	if !moduleEdge || !importEdge {
		t.Fatalf("static ImportEquals module/import edges missing: module=%v import=%v edges=%+v", moduleEdge, importEdge, sink.edges)
	}
	if got := coverageState(report.Coverage, scope.ID, model.FactReference); got == model.Complete {
		t.Fatalf("reference coverage = %s for a dynamic-import project; want a non-complete state", got)
	}
	for _, fact := range []model.FactKind{model.FactImport, model.FactModule} {
		if got := coverageState(report.Coverage, scope.ID, fact); got == model.Complete {
			t.Fatalf("require-based ImportEquals/dynamic-import project has complete %s coverage", fact)
		}
	}
}

func TestDirectCompilerRunnerMatchesLanguageServiceForAliasAndBindingReferences(t *testing.T) {
	tools := localDirectTools(t)
	root := "file:///repo/direct-reference-aliases"
	configURI := root + "/tsconfig.json"
	moduleURI := root + "/module.ts"
	consumerURI := root + "/consumer.ts"
	config := []byte(`{"compilerOptions":{"target":"ES2020","module":"CommonJS","strict":true},"include":["*.ts"]}`)
	module := []byte("export const value = 1;\nexport type Item = { label: string; };\n")
	consumer := []byte(strings.Join([]string{
		"import { value as renamedValue } from './module';",
		"type ImportedItem = import('./module').Item;",
		"interface Shape { prop: string; }",
		"const shorthand = { renamedValue };",
		"const aliasUse = renamedValue;",
		"function read({ prop }: Shape) { return prop; }",
		"const shape: Shape = { prop: 'x' };",
		"shape.prop; shape['prop'];",
		"export { renamedValue as reexported };",
	}, "\n") + "\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "direct-reference-aliases", DiskDigest: "sha256:direct-reference-aliases", SnapshotRev: 1},
		files: map[string][]model.File{root: {
			semanticFile(configURI, "json", config), semanticFile(moduleURI, "typescript", module), semanticFile(consumerURI, "typescript", consumer),
		}},
		content: map[string][]byte{configURI: config, moduleURI: module, consumerURI: consumer},
	}
	configDigest := sha256.Sum256(config)
	scope := model.Scope{
		ID: "direct-reference-aliases", Language: "typescript", RootURI: root,
		Build: model.BuildInputs{Options: map[string]string{
			"tsconfig": "tsconfig.json", "projectConfigDigest": "sha256:" + hex.EncodeToString(configDigest[:]),
			"compilerOptions": `{"target":"ES2020","module":"CommonJS","strict":true}`, "projectReferences": "[]", "plugins": "[]",
		}},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: "typescript", Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	request := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	sink := &semanticTestSink{}
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("direct ExportIndex: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}

	var valueID, propertyID, moduleID identity.SymbolID
	moduleLines := strings.Split(string(module), "\n")
	consumerLines := strings.Split(string(consumer), "\n")
	for _, occurrence := range sink.occurrences {
		if occurrence.Role != "declaration" {
			continue
		}
		if occurrence.URI == moduleURI && occurrence.Range.StartLine == 0 && occurrence.Range.StartChar == uint32(strings.Index(moduleLines[0], "value")) {
			valueID = occurrence.SymbolID
		}
		if occurrence.URI == consumerURI && occurrence.Range.StartLine == 2 && occurrence.Range.StartChar == uint32(strings.Index(consumerLines[2], "prop")) {
			propertyID = occurrence.SymbolID
		}
	}
	for _, symbol := range sink.symbols {
		if symbol.Kind == "module_graph_node" && symbol.Name == moduleURI {
			moduleID = symbol.ID
		}
	}
	if valueID == "" || propertyID == "" || moduleID == "" {
		t.Fatalf("compiler identities missing exported value/property/module: value=%q property=%q module=%q symbols=%+v occurrences=%+v", valueID, propertyID, moduleID, sink.symbols, sink.occurrences)
	}

	asserted := map[string][]model.Position{
		"value":    referenceRanges(sink.occurrences, consumerURI, valueID),
		"property": referenceRanges(sink.occurrences, consumerURI, propertyID),
		"module":   referenceRanges(sink.occurrences, consumerURI, moduleID),
	}
	queries := map[string]int{
		// Querying the imported name asks the pinned service for the target's
		// references, including the local alias declaration and shorthand use.
		"value":    strings.Index(string(consumer), "value as renamedValue"),
		"property": strings.Index(string(consumer), "interface Shape { prop") + len("interface Shape { "),
		"module":   strings.Index(string(consumer), "./module") + 1,
	}
	oracle := pinnedLanguageServiceReferences(t, tools, consumer, module, queries)
	for label, expected := range asserted {
		if got := oracle[label]; !sameRanges(expected, got) {
			t.Errorf("native exporter %s references differ from TypeScript 6.0.3 LanguageService: exporter=%v languageService=%v", label, expected, got)
		}
	}
	if got := coverageState(report.Coverage, scope.ID, model.FactReference); got != model.Complete {
		t.Fatalf("closed static TypeScript reference coverage = %s, want complete: %+v", got, report.Coverage)
	}

	// Compare every emitted project symbol's complete reference group against
	// LanguageService. This exercises alias, shorthand value/property, binding,
	// contextual property, module-string, and import-type references together.
	allQueries := make(map[string]languageServiceQuery)
	ids := make([]identity.SymbolID, 0)
	seenIDs := make(map[identity.SymbolID]struct{})
	type occurrenceRangeKey struct {
		URI   string
		Range model.Position
	}
	idsAtRange := make(map[occurrenceRangeKey]map[identity.SymbolID]struct{})
	for _, occurrence := range sink.occurrences {
		if occurrence.Role == "declaration" || occurrence.Role == "reference" {
			key := occurrenceRangeKey{URI: occurrence.URI, Range: occurrence.Range}
			if idsAtRange[key] == nil {
				idsAtRange[key] = make(map[identity.SymbolID]struct{})
			}
			idsAtRange[key][occurrence.SymbolID] = struct{}{}
		}
		if _, seen := seenIDs[occurrence.SymbolID]; seen {
			continue
		}
		seenIDs[occurrence.SymbolID] = struct{}{}
		ids = append(ids, occurrence.SymbolID)
	}
	for _, id := range ids {
		var declarationAnchor, referenceAnchor *model.Occurrence
		for i := range sink.occurrences {
			occurrence := &sink.occurrences[i]
			if occurrence.SymbolID != id || (occurrence.Role != "declaration" && occurrence.Role != "reference") {
				continue
			}
			if len(idsAtRange[occurrenceRangeKey{URI: occurrence.URI, Range: occurrence.Range}]) != 1 {
				continue
			}
			if occurrence.Role == "declaration" && declarationAnchor == nil {
				declarationAnchor = occurrence
			} else if occurrence.Role == "reference" && referenceAnchor == nil {
				referenceAnchor = occurrence
			}
		}
		anchor := declarationAnchor
		if anchor == nil {
			anchor = referenceAnchor
		}
		if anchor == nil {
			t.Errorf("symbol %q has no unambiguous language-service query anchor", id)
			continue
		}
		sources := map[string][]byte{"consumer.ts": consumer, "module.ts": module}
		allQueries[string(id)] = languageServiceQuery{
			File: uriSourceName(anchor.URI, sources), Offset: languageServiceOffset(anchor.URI, anchor.Range.StartLine, anchor.Range.StartChar, sources),
		}
	}
	sources := map[string][]byte{
		"consumer.ts": consumer,
		"module.ts":   module,
	}
	oracleSets := pinnedLanguageServiceReferenceSets(t, tools, sources, allQueries)
	for _, id := range ids {
		var expected []languageServiceReference
		var declarationAnchors []languageServiceReference
		for _, occurrence := range sink.occurrences {
			if occurrence.SymbolID == id && occurrence.Role == "reference" {
				expected = append(expected, languageServiceReference{File: uriSourceName(occurrence.URI, sources), Range: occurrence.Range})
			}
			if occurrence.SymbolID == id && occurrence.Role == "declaration" {
				declarationAnchors = append(declarationAnchors, languageServiceReference{File: uriSourceName(occurrence.URI, sources), Range: occurrence.Range})
			}
		}
		var got []languageServiceReference
		for _, reference := range oracleSets[string(id)] {
			candidate := languageServiceReference{File: reference.File, Range: reference.Range}
			// At a destructuring binding, TypeScript may return the same text span
			// as both this symbol's definition and another symbol's reference. Keep
			// the reference for the other symbol, but don't count the overlapping
			// token as a use of this symbol when the exporter classifies it only as
			// a declaration here.
			if containsLanguageServiceReference(declarationAnchors, candidate) && !containsLanguageServiceReference(expected, candidate) {
				continue
			}
			if len(declarationAnchors) != 0 {
				definition := languageServiceReference{File: reference.DefinitionFile, Range: reference.DefinitionRange}
				if !reference.HasDefinition || !containsLanguageServiceReference(declarationAnchors, definition) {
					continue
				}
			}
			got = append(got, candidate)
		}
		if !sameLanguageServiceReferences(expected, got) {
			t.Errorf("symbol %q full references differ from TypeScript 6.0.3 LanguageService: exporter=%v languageService=%v", id, expected, got)
		}
	}
}

func TestDirectCompilerRunnerKeepsUnprovenReferenceScopesIncomplete(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		files      map[string][]byte
		wantState  model.Completeness
		wantReason string
		wantLSRef  bool
	}{
		{
			name:       "reflection target outside project",
			source:     "const item = { name: 1 }; Reflect.get(item, 'name');\n",
			wantState:  model.IncompleteKnownSubset,
			wantReason: "standard-library declarations",
		},
		{
			name:      "compiler error",
			source:    "const item: number = 'not a number'; item;\n",
			wantState: model.IncompleteKnownSubset,
		},
		{
			name:      "unresolved identifier",
			source:    "missingName;\n",
			wantState: model.Unknown,
		},
		{
			name:      "unresolved dependency",
			source:    "import { missing } from './missing'; missing;\n",
			wantState: model.Unknown,
		},
		{
			name:       "aliased require loader",
			source:     "declare function require(path: string): void;\nconst load = require;\nload('./module');\n",
			wantState:  model.IncompleteKnownSubset,
			wantReason: "dynamic module or runtime behavior",
		},
		{
			name:       "aliased require property",
			source:     "interface Runtime { require(path: string): void; }\ndeclare const runtime: Runtime;\nconst load = runtime['require'];\nload('./module');\n",
			wantState:  model.IncompleteKnownSubset,
			wantReason: "dynamic module or runtime behavior",
		},
		{
			name:       "aliased eval property",
			source:     "interface Runtime { eval(source: string): void; }\ndeclare const runtime: Runtime;\nconst compile = runtime.eval;\ncompile('hidden = 1');\n",
			wantState:  model.IncompleteKnownSubset,
			wantReason: "dynamic module or runtime behavior",
		},
		{
			name:      "computed property",
			source:    "declare const item: { value: number }; declare const key: string; item[key];\n",
			wantState: model.IncompleteKnownSubset,
		},
		{
			name:      "JSDoc type reference",
			source:    "export class Thing {}\n/** @type {Thing} */\nconst value = new Thing();\n",
			wantState: model.Complete,
			wantLSRef: true,
		},
		{
			name:       "JSDoc template declaration",
			source:     "/** @template U */\nexport function identity<T>(value: T): T { return value; }\n",
			wantState:  model.IncompleteKnownSubset,
			wantReason: "JSDoc declaration and import tags",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := make(map[string][]byte, len(tc.files)+1)
			files["main.ts"] = []byte(tc.source)
			for name, content := range tc.files {
				files[name] = content
			}
			report, sink := exportDirectTypeScriptFixture(t, files)
			got := coverageState(report.Coverage, "direct-reference-negative", model.FactReference)
			if got != tc.wantState {
				t.Fatalf("reference coverage = %s, want %s: %+v", got, tc.wantState, report.Coverage)
			}
			if tc.wantReason != "" && !strings.Contains(coverageReason(report.Coverage, "direct-reference-negative", model.FactReference), tc.wantReason) {
				t.Fatalf("reference reason = %q, want substring %q", coverageReason(report.Coverage, "direct-reference-negative", model.FactReference), tc.wantReason)
			}
			if tc.wantLSRef {
				source := files["main.ts"]
				sources := map[string][]byte{"main.ts": source}
				queries := map[string]languageServiceQuery{"Thing": {
					File: "main.ts", Offset: strings.Index(string(source), "Thing"),
				}}
				references := pinnedLanguageServiceReferenceSets(t, localDirectTools(t), sources, queries)["Thing"]
				commentLine := strings.Split(string(source), "\n")[1]
				start := strings.Index(commentLine, "Thing")
				want := languageServiceReference{File: "main.ts", Range: model.Position{
					StartLine: 1, StartChar: uint32(start), EndLine: 1, EndChar: uint32(start + len("Thing")),
				}}
				foundJSDocReference := false
				for _, reference := range references {
					if reference.File == want.File && reference.Range == want.Range {
						foundJSDocReference = true
						break
					}
				}
				if !foundJSDocReference {
					t.Fatalf("pinned LanguageService references omit JSDoc type span %+v: %v", want, references)
				}
				assertAllSymbolReferencesMatchLanguageService(t, localDirectTools(t), sources, sink)
			}
		})
	}
}

func exportDirectTypeScriptFixture(t *testing.T, sources map[string][]byte) (model.Report, *semanticTestSink) {
	t.Helper()
	return exportDirectTypeScriptFixtureWithOptions(t, sources, `{"target":"ES2020","module":"CommonJS","strict":true}`)
}

func exportDirectTypeScriptFixtureWithOptions(t *testing.T, sources map[string][]byte, compilerOptions string) (model.Report, *semanticTestSink) {
	t.Helper()
	tools := localDirectTools(t)
	root := "file:///repo/direct-reference-negative"
	configURI := root + "/tsconfig.json"
	config := []byte(`{"compilerOptions":` + compilerOptions + `,"include":["*.ts"]}`)
	files := []model.File{semanticFile(configURI, "json", config)}
	content := map[string][]byte{configURI: config}
	for name, source := range sources {
		uri := root + "/" + name
		files = append(files, semanticFile(uri, "typescript", source))
		content[uri] = source
	}
	view := &semanticTestView{
		id:      model.Identity{Workspace: "direct-reference-negative", DiskDigest: "sha256:direct-reference-negative", SnapshotRev: 1},
		files:   map[string][]model.File{root: files},
		content: content,
	}
	digest := sha256.Sum256(config)
	scope := model.Scope{
		ID: "direct-reference-negative", Language: "typescript", RootURI: root,
		Build: model.BuildInputs{Options: map[string]string{
			"tsconfig": "tsconfig.json", "projectConfigDigest": "sha256:" + hex.EncodeToString(digest[:]),
			"compilerOptions": compilerOptions, "projectReferences": "[]", "plugins": "[]",
		}},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: "typescript", Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	sink := &semanticTestSink{}
	request := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("direct TypeScript ExportIndex: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	return report, sink
}

func TestDirectCompilerRunnerIsolatesSymbolsAcrossBuildContexts(t *testing.T) {
	tools := localDirectTools(t)
	root := "file:///repo/direct-context-isolation"
	sourceURI := root + "/main.ts"
	configAURI := root + "/tsconfig.strict.json"
	configBURI := root + "/tsconfig.loose.json"
	configA := []byte(`{"compilerOptions":{"target":"ES2020","strict":true},"include":["*.ts"]}`)
	configB := []byte(`{"compilerOptions":{"target":"ES2020","strict":false},"include":["*.ts"]}`)
	source := []byte("export interface Item { value: string }\nexport function read(item: Item): string { return item.value; }\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "direct-context-isolation", DiskDigest: "sha256:direct-context-isolation", SnapshotRev: 7},
		files: map[string][]model.File{root: {
			semanticFile(configAURI, "json", configA), semanticFile(configBURI, "json", configB), semanticFile(sourceURI, "typescript", source),
		}},
		content: map[string][]byte{configAURI: configA, configBURI: configB, sourceURI: source},
	}

	makeScope := func(id, configName string, config []byte, compilerOptions string) (model.Scope, model.Provenance) {
		digest := sha256.Sum256(config)
		scope := model.Scope{
			ID: id, Language: "typescript", RootURI: root,
			Build: model.BuildInputs{Options: map[string]string{
				"tsconfig": configName, "projectConfigDigest": "sha256:" + hex.EncodeToString(digest[:]),
				"compilerOptions": compilerOptions, "projectReferences": "[]", "plugins": "[]",
			}},
		}
		provenance := model.Provenance{
			SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
			Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
			Backend:   identity.BackendID{Language: scope.Language, Name: "typescript-direct-compiler"},
			Toolchain: semanticIndexToolchain, Tools: tools,
		}
		scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
		provenance.Scope = scope
		return scope, provenance
	}
	scopeA, provenanceA := makeScope("direct-strict", filepath.Base(configAURI), configA, `{"target":"ES2020","strict":true}`)
	scopeB, provenanceB := makeScope("direct-loose", filepath.Base(configBURI), configB, `{"target":"ES2020","strict":false}`)
	if scopeA.BuildContext == scopeB.BuildContext {
		t.Fatal("different TypeScript compiler options shared a build context")
	}
	request := model.Request{
		View: view, Scopes: []model.Scope{scopeA, scopeB},
		Provenance: map[string]model.Provenance{scopeA.ID: provenanceA, scopeB.ID: provenanceB},
	}
	sink := &semanticTestSink{}
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("direct multi-context ExportIndex: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	idsByScope := map[string]map[identity.SymbolID]struct{}{scopeA.ID: {}, scopeB.ID: {}}
	for _, symbol := range sink.symbols {
		if _, ok := idsByScope[symbol.ScopeID]; !ok {
			t.Fatalf("symbol has an unexpected scope: %+v", symbol)
		}
		idsByScope[symbol.ScopeID][symbol.ID] = struct{}{}
	}
	if len(idsByScope[scopeA.ID]) == 0 || len(idsByScope[scopeB.ID]) == 0 {
		t.Fatalf("direct compiler emitted no scoped symbols: %+v", idsByScope)
	}
	for id := range idsByScope[scopeA.ID] {
		if _, exists := idsByScope[scopeB.ID][id]; exists {
			t.Fatalf("same declaration identity collided across build contexts: %q", id)
		}
	}
	for _, occurrence := range sink.occurrences {
		if _, ok := idsByScope[occurrence.ScopeID][occurrence.SymbolID]; !ok {
			t.Fatalf("occurrence identity escaped its TypeScript build context: %+v", occurrence)
		}
	}
}

func TestDirectCompilerRunnerRecognizesJSConfigAndReportsDynamicCoverage(t *testing.T) {
	tools := localDirectTools(t)
	root := "file:///repo/direct-js"
	configURI := root + "/jsconfig.json"
	fileURI := root + "/main.js"
	config := []byte(`{"compilerOptions":{"target":"ES2020","allowJs":true,"checkJs":true},"include":["*.js"]}`)
	source := []byte("export class Box {}\r\n/** @param {Box} item\r\n * @returns {Box}\r\n */\r\nexport function identity(item) { return item; }\r\nconst emoji = \"😀\";\r\nfunction add(value) { return value + 1; }\r\nfunction run() { return add(1); }\r\nrun();\r\n")
	view := &semanticTestView{
		id:      model.Identity{Workspace: "direct-javascript", DiskDigest: "sha256:direct-js", SnapshotRev: 2},
		files:   map[string][]model.File{root: {semanticFile(configURI, "json", config), semanticFile(fileURI, "javascript", source)}},
		content: map[string][]byte{configURI: config, fileURI: source},
	}
	configDigest := sha256.Sum256(config)
	scope := model.Scope{
		ID: "direct-js", Language: "javascript", RootURI: root,
		Build: model.BuildInputs{Options: map[string]string{
			"jsconfig": "jsconfig.json", "jsconfigDigest": "sha256:" + hex.EncodeToString(configDigest[:]),
			"compilerOptions": string(`{"target":"ES2020","allowJs":true,"checkJs":true}`), "projectReferences": "[]", "plugins": "[]",
		}},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: "javascript", Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	sink := &semanticTestSink{}
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), model.Request{
		View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, sink)
	if err != nil {
		t.Fatalf("direct JavaScript ExportIndex: %v", err)
	}
	if err := model.ValidateReport(model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	if sink.maxBatch > semanticIndexBatchLimit {
		t.Fatalf("compiler exporter exceeded bounded batch size: %d", sink.maxBatch)
	}
	var addID identity.SymbolID
	for _, symbol := range sink.symbols {
		if symbol.Name == "add" {
			addID = symbol.ID
		}
	}
	if addID == "" {
		t.Fatalf("pinned TypeScript compiler emitted no JavaScript function symbol: %+v", sink.symbols)
	}
	var addReference, callEdge bool
	for _, occurrence := range sink.occurrences {
		if occurrence.URI == fileURI && occurrence.SymbolID == addID && occurrence.Role == "reference" {
			addReference = true
		}
	}
	for _, edge := range sink.edges {
		if edge.Kind == model.EdgeCall && edge.To == addID {
			callEdge = true
		}
	}
	if !addReference || !callEdge {
		t.Fatalf("pinned TypeScript compiler emitted no non-empty JavaScript reference/call facts: reference=%v call=%v occurrences=%+v edges=%+v", addReference, callEdge, sink.occurrences, sink.edges)
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference, model.FactImplementation, model.FactTypeRelation, model.FactCall, model.FactImport, model.FactModule} {
		if got := coverageState(report.Coverage, scope.ID, fact); got != model.IncompleteKnownSubset {
			t.Fatalf("JavaScript coverage %s = %s, want incomplete_known_subset", fact, got)
		}
	}
	assertAllSymbolReferencesMatchLanguageService(t, tools, map[string][]byte{"main.js": source}, sink)
}

func TestDirectCompilerRunnerQualifiesClosedStaticCheckJSESMProject(t *testing.T) {
	options := `{"target":"ES2020","module":"ESNext","moduleResolution":"Bundler","allowJs":true,"checkJs":true,"strict":true}`
	sources := map[string][]byte{
		"lib/math.js": []byte("/**\n * @param {number} left\n * @param {number} right\n * @returns {number}\n */\nexport function add(left, right) { return left + right; }\nexport const scale = 2;\n"),
		"main.js":     []byte("import { add, scale } from './lib/math.js';\n/**\n * @param {number} amount\n * @returns {number}\n */\nexport function total(amount) { return add(amount, scale); }\nexport const result = total(10);\n"),
	}
	report, sink, tools := exportDirectJavaScriptFixture(t, sources, options)
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference} {
		if got := coverageState(report.Coverage, "direct-static-js", fact); got != model.Complete {
			t.Fatalf("static checkJs coverage %s = %s, want complete: %+v", fact, got, report.Coverage)
		}
		reason := coverageReason(report.Coverage, "direct-static-js", fact)
		for _, proof := range []string{directJavaScriptCheckJSProof, "typescript=6.0.3", "allowJs=true", "checkJs=true", "closedScopes=1", "sourceFiles=2"} {
			if !strings.Contains(reason, proof) {
				t.Fatalf("static checkJs coverage %s provenance %q omits %q", fact, reason, proof)
			}
		}
	}
	for _, fact := range []model.FactKind{model.FactCall, model.FactImport, model.FactModule, model.FactImplementation, model.FactTypeRelation} {
		if got := coverageState(report.Coverage, "direct-static-js", fact); got == model.Complete {
			t.Fatalf("static checkJs coverage promoted unrelated fact %s: %+v", fact, report.Coverage)
		}
	}
	assertAllSymbolReferencesMatchLanguageService(t, tools, sources, sink)
}

func TestDirectCompilerRunnerKeepsCheckJSRuntimeAndTypeEscapesPartial(t *testing.T) {
	options := `{"target":"ES2020","module":"ESNext","moduleResolution":"Bundler","allowJs":true,"checkJs":true,"strict":true}`
	cases := []struct {
		name          string
		source        string
		query         string
		wantLSResults bool
		wantReason    string
	}{
		{
			name:          "any member access",
			source:        "/** @type {*} */\nconst value = { secret: 1 };\nexport const result = value.secret;\n",
			query:         "value",
			wantLSResults: true,
		},
		{
			name:          "any object spread",
			source:        "/**\n * @param {*} input\n * @returns {object}\n */\nexport function copy(input) { return { ...input }; }\n",
			query:         "input",
			wantLSResults: true,
			wantReason:    "any or unknown type reaches a JavaScript spread",
		},
		{
			name:          "any array spread",
			source:        "/**\n * @param {*} input\n */\nexport function values(input) { return [...input]; }\n",
			query:         "input",
			wantLSResults: true,
			wantReason:    "any or unknown type reaches a JavaScript spread",
		},
		{
			name:          "unknown object spread under suppression",
			source:        "/**\n * @param {unknown} input\n * @returns {object}\n */\nexport function copy(input) {\n  // @ts-ignore\n  return { ...input };\n}\n",
			query:         "input",
			wantLSResults: true,
			wantReason:    "suppression directives",
		},
		{
			name:          "unknown under suppression",
			source:        "/** @type {unknown} */\nconst value = { secret: 1 };\n// @ts-ignore\nexport const result = value.secret;\n",
			query:         "value",
			wantLSResults: true,
			wantReason:    "suppression directives",
		},
		{
			name:          "Function constructor alias",
			source:        "const compile = Function;\nconst generated = compile('return hiddenName');\nexport const result = generated;\n",
			query:         "compile",
			wantLSResults: true,
		},
		{
			name:          "prototype mutation",
			source:        "class Box {}\nBox.prototype.extra = 1;\nconst box = new Box();\nexport const result = box.extra;\n",
			query:         "extra",
			wantLSResults: true,
		},
		{
			name:          "Object defineProperty string key",
			source:        "const value = {};\nObject.defineProperty(value, 'secret', { value: 1 });\nexport const result = value.secret;\n",
			query:         "secret",
			wantLSResults: true,
		},
		{
			name:          "Proxy trap",
			source:        "const value = new Proxy({ secret: 1 }, { get(target, key) { return target.secret; } });\nexport const result = value.secret;\n",
			query:         "secret",
			wantLSResults: true,
		},
		{
			name:          "unanchored identifier",
			source:        "export const result = missingValue;\n",
			query:         "missingValue",
			wantLSResults: false,
		},
	}
	tools := localDirectTools(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sources := map[string][]byte{"main.js": []byte(tc.source)}
			report, sink, _ := exportDirectJavaScriptFixture(t, sources, options)
			for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference} {
				if got := coverageState(report.Coverage, "direct-static-js", fact); got != model.IncompleteKnownSubset {
					t.Fatalf("unsafe checkJs coverage %s = %s, want incomplete_known_subset: %+v", fact, got, report.Coverage)
				}
			}
			if tc.wantReason != "" && !strings.Contains(coverageReason(report.Coverage, "direct-static-js", model.FactReference), tc.wantReason) {
				t.Fatalf("unsafe checkJs reason = %q, want substring %q", coverageReason(report.Coverage, "direct-static-js", model.FactReference), tc.wantReason)
			}
			offset := strings.Index(tc.source, tc.query)
			if offset < 0 {
				t.Fatalf("query token %q is absent from test source", tc.query)
			}
			oracle := pinnedLanguageServiceReferenceSets(t, tools, sources, map[string]languageServiceQuery{
				"query": {File: "main.js", Offset: offset},
			})["query"]
			if tc.wantLSResults && len(oracle) == 0 {
				t.Fatalf("pinned TypeScript 6.0.3 LanguageService returned no references for %q", tc.query)
			}
			if !tc.wantLSResults && len(oracle) != 0 {
				t.Fatalf("pinned LanguageService unexpectedly resolved unanchored %q: %v", tc.query, oracle)
			}
			if tc.wantLSResults && len(sink.occurrences) == 0 {
				t.Fatal("direct compiler emitted no project occurrences for the pinned LanguageService query")
			}
		})
	}
}

func exportDirectJavaScriptFixture(t *testing.T, sources map[string][]byte, compilerOptions string) (model.Report, *semanticTestSink, []model.ToolIdentity) {
	t.Helper()
	tools := localDirectTools(t)
	root := "file:///repo/direct-static-js"
	configURI := root + "/jsconfig.json"
	config := []byte(`{"compilerOptions":` + compilerOptions + `,"include":["**/*.js"]}`)
	files := []model.File{semanticFile(configURI, "json", config)}
	content := map[string][]byte{configURI: config}
	for name, source := range sources {
		uri := root + "/" + filepath.ToSlash(name)
		files = append(files, semanticFile(uri, "javascript", source))
		content[uri] = source
	}
	view := &semanticTestView{
		id:      model.Identity{Workspace: "direct-static-js", DiskDigest: "sha256:direct-static-js", SnapshotRev: 1},
		files:   map[string][]model.File{root: files},
		content: content,
	}
	configDigest := sha256.Sum256(config)
	scope := model.Scope{
		ID: "direct-static-js", Language: "javascript", RootURI: root,
		Build: model.BuildInputs{Options: map[string]string{
			"jsconfig": "jsconfig.json", "jsconfigDigest": "sha256:" + hex.EncodeToString(configDigest[:]),
			"compilerOptions": compilerOptions, "projectReferences": "[]", "plugins": "[]",
		}},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: "javascript", Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	request := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	sink := &semanticTestSink{}
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("direct JavaScript ExportIndex: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	return report, sink, tools
}

func TestProductionBuildRequestQualifiesPureJSTSConfigButKeepsMixedProjectPartial(t *testing.T) {
	tools := localDirectTools(t)
	options := `{"target":"ES2020","module":"CommonJS","strict":true,"allowJs":true,"checkJs":true,"noEmit":true}`
	config := []byte(`{"compilerOptions":` + options + `,"include":["*.js","*.ts"]}`)
	javascriptSources := map[string][]byte{
		"target.js": []byte("export function target() { return 42; }\n"),
		"use.js":    []byte("import { target } from './target.js';\nexport function use() { return target(); }\n"),
	}

	buildAndExport := func(t *testing.T, root string, mixed bool) (model.Report, *semanticTestSink, model.Request) {
		t.Helper()
		sources := make(map[string][]byte, len(javascriptSources)+1)
		for name, source := range javascriptSources {
			sources[name] = source
		}
		if mixed {
			sources["typed.ts"] = []byte("export const typed: number = 1;\n")
		}
		configURI := root + "/tsconfig.json"
		files := []model.File{semanticFile(configURI, "json", config)}
		content := map[string][]byte{configURI: config}
		for name, source := range sources {
			uri := root + "/" + name
			language := "javascript"
			if strings.HasSuffix(name, ".ts") {
				language = "typescript"
			}
			files = append(files, semanticFile(uri, language, source))
			content[uri] = source
		}
		view := &semanticTestView{
			id:      model.Identity{Workspace: identity.WorkspaceID(filepath.Base(root)), DiskDigest: identity.ContentHash("sha256:" + filepath.Base(root)), SnapshotRev: 1},
			files:   map[string][]model.File{root: files},
			content: content,
		}
		provider := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools})
		request, err := provider.BuildIndexRequest(context.Background(), view, root)
		if err != nil {
			t.Fatalf("BuildIndexRequest: %v", err)
		}
		if len(request.Scopes) != 1 {
			t.Fatalf("discovered scopes = %d, want one tsconfig scope: %+v", len(request.Scopes), request.Scopes)
		}
		scope := request.Scopes[0]
		if scope.Language != "typescript" || scope.Build.Options["tsconfig"] != "tsconfig.json" {
			t.Fatalf("tsconfig project identity = %+v, want TypeScript config scope", scope)
		}
		sink := &semanticTestSink{}
		report, err := provider.ExportIndex(context.Background(), request, sink)
		if err != nil {
			t.Fatalf("production-path ExportIndex: %v", err)
		}
		if err := model.ValidateReport(request, report); err != nil {
			t.Fatalf("ValidateReport: %v", err)
		}
		return report, sink, request
	}

	t.Run("pure JavaScript tsconfig", func(t *testing.T) {
		root := "file:///repo/production-js-tsconfig"
		report, sink, request := buildAndExport(t, root, false)
		scopeID := request.Scopes[0].ID
		for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference} {
			if got := coverageState(report.Coverage, scopeID, fact); got != model.Complete {
				t.Fatalf("production pure-JS tsconfig coverage %s = %s, want complete: %+v", fact, got, report.Coverage)
			}
			for _, provenance := range []string{directJavaScriptCheckJSProof, "typescript=6.0.3", "allowJs=true", "checkJs=true", "closedScopes=1", "sourceFiles=2"} {
				if !strings.Contains(coverageReason(report.Coverage, scopeID, fact), provenance) {
					t.Fatalf("coverage %s provenance %q omits %q", fact, coverageReason(report.Coverage, scopeID, fact), provenance)
				}
			}
		}
		for _, fact := range []model.FactKind{model.FactCall, model.FactImport, model.FactModule, model.FactImplementation, model.FactTypeRelation} {
			if got := coverageState(report.Coverage, scopeID, fact); got == model.Complete {
				t.Fatalf("production pure-JS tsconfig promoted unrelated fact %s: %+v", fact, report.Coverage)
			}
		}
		assertAllSymbolReferencesMatchLanguageService(t, tools, map[string][]byte{
			"target.js": javascriptSources["target.js"], "use.js": javascriptSources["use.js"],
		}, sink)
	})

	t.Run("mixed JavaScript and TypeScript tsconfig", func(t *testing.T) {
		root := "file:///repo/production-mixed-tsconfig"
		report, _, request := buildAndExport(t, root, true)
		scopeID := request.Scopes[0].ID
		for _, fact := range []model.FactKind{model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference} {
			if got := coverageState(report.Coverage, scopeID, fact); got != model.IncompleteKnownSubset {
				t.Fatalf("mixed project coverage %s = %s, want incomplete_known_subset: %+v", fact, got, report.Coverage)
			}
		}
	})
}

func TestDirectCompilerRunnerBindsTopLevelJSCallToCapturedScriptScope(t *testing.T) {
	tools := localDirectTools(t)
	root := "file:///repo/direct-js-top-level"
	configURI := root + "/jsconfig.json"
	fileURI := root + "/main.js"
	config := []byte(`{"compilerOptions":{"target":"ES2020","allowJs":true,"checkJs":true},"include":["*.js"]}`)
	source := []byte("function add(value) { return value + 1; }\nfunction run() { return add(1); }\nrun();\nadd(2);\nmissingCall(3);\n")
	sourceFile := semanticFile(fileURI, "javascript", source)
	view := &semanticTestView{
		id:      model.Identity{Workspace: "direct-javascript-top-level", DiskDigest: "sha256:direct-js-top-level", SnapshotRev: 1},
		files:   map[string][]model.File{root: {semanticFile(configURI, "json", config), sourceFile}},
		content: map[string][]byte{configURI: config, fileURI: source},
	}
	configDigest := sha256.Sum256(config)
	scope := model.Scope{
		ID: "direct-js-top-level", Language: "javascript", RootURI: root,
		Build: model.BuildInputs{Options: map[string]string{
			"jsconfig": "jsconfig.json", "jsconfigDigest": "sha256:" + hex.EncodeToString(configDigest[:]),
			"compilerOptions": string(`{"target":"ES2020","allowJs":true,"checkJs":true}`), "projectReferences": "[]", "plugins": "[]",
		}},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: "javascript", Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	sink := &semanticTestSink{}
	request := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("direct JavaScript ExportIndex: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}

	var addID, runID identity.SymbolID
	var scriptScopeID identity.SymbolID
	for _, symbol := range sink.symbols {
		switch symbol.Name {
		case "add":
			addID = symbol.ID
		case "run":
			runID = symbol.ID
		}
		if symbol.Kind == "synthetic_script_scope" {
			scriptScopeID = symbol.ID
		}
	}
	if addID == "" || runID == "" {
		t.Fatalf("pinned TypeScript compiler emitted no JavaScript call targets: add=%q run=%q symbols=%+v", addID, runID, sink.symbols)
	}
	if wantPrefix := "typescript/6.0.3/" + string(scope.BuildContext) + "/script-scope:sha256:"; scriptScopeID == "" || !strings.HasPrefix(string(scriptScopeID), wantPrefix) || !strings.HasSuffix(string(scriptScopeID), ":revision:"+string(sourceFile.SHA256)) {
		t.Fatalf("script scope identity is not bound to its build context and captured source revision: got=%q", scriptScopeID)
	}
	functionCall, topLevelCall, topLevelRunCall := false, false, false
	callEdgeCount := 0
	for _, edge := range sink.edges {
		if edge.Kind != model.EdgeCall {
			continue
		}
		callEdgeCount++
		switch {
		case edge.From == runID && edge.To == addID:
			functionCall = true
		case edge.From == scriptScopeID && edge.To == addID:
			topLevelCall = true
		case edge.From == scriptScopeID && edge.To == runID:
			topLevelRunCall = true
		}
	}
	if !functionCall || !topLevelCall || !topLevelRunCall || callEdgeCount != 3 {
		t.Fatalf("expected function and statically resolved top-level calls only; function=%v topLevel=%v topLevelRun=%v callEdges=%d edges=%+v", functionCall, topLevelCall, topLevelRunCall, callEdgeCount, sink.edges)
	}
	var callCoverage model.Coverage
	for _, coverage := range report.Coverage {
		if coverage.ScopeID == scope.ID && coverage.Fact == model.FactCall {
			callCoverage = coverage
			break
		}
	}
	if callCoverage.State != model.Unknown || !strings.Contains(callCoverage.Reason, "missingCall(3)") {
		t.Fatalf("unresolved top-level call must be omitted and honestly mark call coverage unknown: %+v", callCoverage)
	}
}

func TestDiscoverScopesRecognizesJSONCTypeScriptAndJavaScriptProjects(t *testing.T) {
	root := "file:///repo/discovery"
	tsConfigURI := root + "/tsconfig.json"
	jsConfigURI := root + "/web/jsconfig.json"
	tsConfig := []byte("{\n // project options\n \"compilerOptions\": {\"strict\": true,},\n \"references\": [],\n}")
	jsConfig := []byte("{\"compilerOptions\": {\"allowJs\": true, \"checkJs\": true}, \"include\": [\"**/*.js\"]}")
	view := &semanticTestView{
		id: model.Identity{Workspace: "scope-discovery", DiskDigest: "sha256:scope-discovery", SnapshotRev: 3},
		files: map[string][]model.File{root: {
			semanticFile(tsConfigURI, "json", tsConfig), semanticFile(jsConfigURI, "json", jsConfig),
		}},
		content: map[string][]byte{tsConfigURI: tsConfig, jsConfigURI: jsConfig},
	}
	scopes, err := DiscoverScopes(context.Background(), view, root)
	if err != nil {
		t.Fatalf("DiscoverScopes: %v", err)
	}
	if len(scopes) != 2 {
		t.Fatalf("discovered scopes = %d, want 2: %+v", len(scopes), scopes)
	}
	if scopes[0].Language != "typescript" || scopes[0].RootURI != root || scopes[0].Build.Options["tsconfig"] != "tsconfig.json" {
		t.Fatalf("TypeScript scope = %+v", scopes[0])
	}
	var closure compilerOptionsClosure
	if err := json.Unmarshal([]byte(scopes[0].Build.Options["compilerOptions"]), &closure); err != nil {
		t.Fatalf("decode TypeScript compiler options closure: %v", err)
	}
	if !closure.Complete || len(closure.Configs) != 1 || string(closure.Configs[0].CompilerOptions) != `{"strict":true}` || scopes[0].Build.Options["projectReferences"] != `[]` {
		t.Fatalf("TypeScript options were not canonicalized: %+v", scopes[0].Build.Options)
	}
	if scopes[1].Language != "javascript" || scopes[1].RootURI != root+"/web" || scopes[1].Build.Options["jsconfig"] != "jsconfig.json" {
		t.Fatalf("JavaScript scope = %+v", scopes[1])
	}
}

func TestDiscoverScopesBindsRecursiveExtendsClosure(t *testing.T) {
	workspace := "file:///repo/extends"
	project := workspace + "/apps/web"
	configURI := project + "/tsconfig.json"
	baseURI := workspace + "/tsconfig.base.json"
	strictURI := workspace + "/configs/strict.json"
	sourceURI := project + "/src/main.ts"
	config := []byte(`{"extends":"../../tsconfig.base.json","compilerOptions":{"noEmit":true},"include":["src/**/*.ts"]}`)
	base := []byte(`{"extends":"./configs/strict.json","compilerOptions":{"target":"ES2020"}}`)
	strict := []byte(`{"compilerOptions":{"strict":true,"baseUrl":"."}}`)
	source := []byte("export const value: string = 'ok';\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "extends", DiskDigest: "sha256:extends", SnapshotRev: 4},
		files: map[string][]model.File{
			workspace: {
				semanticFile(configURI, "json", config), semanticFile(baseURI, "json", base), semanticFile(strictURI, "json", strict), semanticFile(sourceURI, "typescript", source),
			},
			project: {semanticFile(configURI, "json", config), semanticFile(sourceURI, "typescript", source)},
		},
		content: map[string][]byte{configURI: config, baseURI: base, strictURI: strict, sourceURI: source},
	}
	scopes, err := DiscoverScopes(context.Background(), view, workspace)
	if err != nil {
		t.Fatalf("DiscoverScopes: %v", err)
	}
	if len(scopes) != 1 {
		t.Fatalf("discovered scopes = %d, want 1: %+v", len(scopes), scopes)
	}
	var closure compilerOptionsClosure
	if err := json.Unmarshal([]byte(scopes[0].Build.Options["compilerOptions"]), &closure); err != nil {
		t.Fatalf("decode TypeScript compiler options closure: %v", err)
	}
	if !closure.Complete || len(closure.Configs) != 3 {
		t.Fatalf("recursive TypeScript extends closure is incomplete: %+v", closure)
	}
	if closure.Configs[0].Path != "../../configs/strict.json" || closure.Configs[1].Path != "../../tsconfig.base.json" || closure.Configs[2].Path != "tsconfig.json" {
		t.Fatalf("extends closure is not parent-first: %+v", closure.Configs)
	}
	if closure.Configs[0].Content != string(strict) {
		t.Fatalf("extended config bytes are not bound into the scope build inputs: %+v", scopes[0].Build.Options["compilerOptions"])
	}
	tools := localDirectTools(t)
	scope := scopes[0]
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: scope.Language, Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), model.Request{
		WorkspaceRootURI: workspace, View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, &semanticTestSink{})
	if err != nil {
		t.Fatalf("direct exporter did not validate recursively extended effective compilerOptions: %v", err)
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactTypeRelation} {
		if got := coverageState(report.Coverage, scope.ID, fact); got != model.Complete {
			t.Fatalf("recursive extends config coverage %s = %s, want complete: %+v", fact, got, report.Coverage)
		}
	}

	duplicateClosure := closure
	duplicateClosure.Configs = append(append([]compilerConfigInput(nil), closure.Configs...), closure.Configs[0])
	duplicateBytes, err := json.Marshal(duplicateClosure)
	if err != nil {
		t.Fatalf("encode duplicated TypeScript config closure: %v", err)
	}
	badScope := cloneScope(scope)
	badScope.Build.Options["compilerOptions"] = string(duplicateBytes)
	badScope.BuildContext = model.ComputeBuildContextID(badScope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	badProvenance := provenance
	badProvenance.Scope = badScope
	badRequest := model.Request{WorkspaceRootURI: workspace, View: view, Scopes: []model.Scope{badScope}, Provenance: map[string]model.Provenance{badScope.ID: badProvenance}}
	badReport, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), badRequest, &semanticTestSink{})
	if err != nil {
		t.Fatalf("duplicate config closure should produce conservative coverage: %v", err)
	}
	badCoverage := coverageState(badReport.Coverage, badScope.ID, model.FactSymbol)
	if badCoverage != model.Unknown || !strings.Contains(coverageReason(badReport.Coverage, badScope.ID, model.FactSymbol), "repeats or escapes") {
		t.Fatalf("duplicate config closure was not rejected as unknown: %+v", badReport.Coverage)
	}
}

func TestDiscoverScopesBindsPackageBasedExtendsClosure(t *testing.T) {
	tools := localDirectTools(t)
	root := "file:///repo/package-extends"
	configURI := root + "/tsconfig.json"
	baseURI := root + "/node_modules/@omni/strict/tsconfig.json"
	sourceURI := root + "/src/main.ts"
	config := []byte(`{"extends":"@omni/strict/tsconfig.json","compilerOptions":{"noEmit":true},"include":["src/**/*.ts"]}`)
	base := []byte(`{"compilerOptions":{"target":"ES2020","strict":true}}`)
	source := []byte("export interface Base {}\nexport type Value = Base;\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "package-extends", DiskDigest: "sha256:package-extends", SnapshotRev: 8},
		files: map[string][]model.File{root: {
			semanticFile(configURI, "json", config), semanticFile(baseURI, "json", base), semanticFile(sourceURI, "typescript", source),
		}},
		content: map[string][]byte{configURI: config, baseURI: base, sourceURI: source},
	}
	scopes, err := DiscoverScopes(context.Background(), view, root)
	if err != nil {
		t.Fatalf("DiscoverScopes: %v", err)
	}
	if len(scopes) != 1 {
		t.Fatalf("discovered scopes = %d, want 1: %+v", len(scopes), scopes)
	}
	scope := scopes[0]
	var closure compilerOptionsClosure
	if err := json.Unmarshal([]byte(scope.Build.Options["compilerOptions"]), &closure); err != nil {
		t.Fatalf("decode TypeScript compiler options closure: %v", err)
	}
	if !closure.Complete || len(closure.Configs) != 2 || closure.Configs[0].Path != "node_modules/@omni/strict/tsconfig.json" {
		t.Fatalf("package-based extends closure is incomplete: %+v", closure)
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: scope.Language, Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), model.Request{
		View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, &semanticTestSink{})
	if err != nil {
		t.Fatalf("direct exporter did not validate package-based effective compilerOptions: %v", err)
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactTypeRelation} {
		if got := coverageState(report.Coverage, scope.ID, fact); got != model.Complete {
			t.Fatalf("package-based extends coverage %s = %s, want complete: %+v", fact, got, report.Coverage)
		}
	}
}

func TestDirectCompilerRunnerHonorsProjectFilesAndTypeCoverage(t *testing.T) {
	tools := localDirectTools(t)
	root := "file:///repo/direct-project-set"
	configURI := root + "/tsconfig.json"
	selectedURI := root + "/src/selected.ts"
	excludedURI := root + "/src/generated/excluded.ts"
	outsideIncludeURI := root + "/other.ts"
	config := []byte(`{"compilerOptions":{"target":"ES2020","strict":true},"include":["src/**/*.ts"],"exclude":["src/generated/**"]}`)
	selected := []byte("export interface Selected {}\nexport type Local = Selected;\nexport type Builtin = Promise<string>;\n")
	excluded := []byte("export interface Excluded {}\n")
	outsideInclude := []byte("export interface OutsideInclude {}\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "project-file-set", DiskDigest: "sha256:project-file-set", SnapshotRev: 5},
		files: map[string][]model.File{root: {
			semanticFile(configURI, "json", config), semanticFile(selectedURI, "typescript", selected),
			semanticFile(excludedURI, "typescript", excluded), semanticFile(outsideIncludeURI, "typescript", outsideInclude),
		}},
		content: map[string][]byte{configURI: config, selectedURI: selected, excludedURI: excluded, outsideIncludeURI: outsideInclude},
	}
	configDigest := sha256.Sum256(config)
	scope := model.Scope{
		ID: "direct-project-set", Language: "typescript", RootURI: root,
		Build: model.BuildInputs{Options: map[string]string{
			"tsconfig": "tsconfig.json", "projectConfigDigest": "sha256:" + hex.EncodeToString(configDigest[:]),
			"compilerOptions": string(`{"target":"ES2020","strict":true}`), "projectReferences": "[]", "plugins": "[]",
		}},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: "typescript", Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	sink := &semanticTestSink{}
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: NewDirectCompilerRunner("node"), Tools: tools}).ExportIndex(context.Background(), model.Request{
		View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, sink)
	if err != nil {
		t.Fatalf("direct ExportIndex: %v", err)
	}
	for _, symbol := range sink.symbols {
		if symbol.Name == "Excluded" || symbol.Name == "OutsideInclude" {
			t.Fatalf("symbol outside the TypeScript project file set was emitted: %+v", symbol)
		}
		if symbol.Name == "Promise" {
			t.Fatalf("unattested standard library declaration was emitted into the project manifest: %+v", symbol)
		}
	}
	if got := coverageState(report.Coverage, scope.ID, model.FactSymbol); got != model.Complete {
		t.Fatalf("selected TypeScript project file set coverage = %s, want complete: %+v", got, report.Coverage)
	}
	if got := coverageState(report.Coverage, scope.ID, model.FactTypeRelation); got != model.IncompleteKnownSubset || !strings.Contains(coverageReason(report.Coverage, scope.ID, model.FactTypeRelation), "standard-library declarations") {
		t.Fatalf("unanchored library type relation coverage = %s, want an explicit incomplete standard-library subset: %+v", got, report.Coverage)
	}
	if got := coverageState(report.Coverage, scope.ID, model.FactReference); got != model.IncompleteKnownSubset || !strings.Contains(coverageReason(report.Coverage, scope.ID, model.FactReference), "standard-library declarations") {
		t.Fatalf("unanchored library reference coverage = %s, want an explicit incomplete standard-library subset: %+v", got, report.Coverage)
	}
}

func TestDirectCompilerRunnerRequiresWorkspaceBoundaryForProjectReferences(t *testing.T) {
	tools := localDirectTools(t)
	root := "file:///repo/direct-reference"
	configURI := root + "/tsconfig.json"
	mainURI := root + "/src/main.ts"
	libConfigURI := root + "/lib/tsconfig.json"
	libSourceURI := root + "/lib/index.ts"
	config := []byte(`{"compilerOptions":{"target":"ES2020","strict":true},"include":["src/**/*.ts"],"references":[{"path":"./lib"}]}`)
	main := []byte("export const result: string = 'ok';\n")
	libConfig := []byte(`{"compilerOptions":{"composite":true},"include":["*.ts"]}`)
	libSource := []byte("export interface LibType {}\n")
	view := &semanticTestView{
		id: model.Identity{Workspace: "project-reference", DiskDigest: "sha256:project-reference", SnapshotRev: 6},
		files: map[string][]model.File{root: {
			semanticFile(configURI, "json", config), semanticFile(mainURI, "typescript", main),
			semanticFile(libConfigURI, "json", libConfig), semanticFile(libSourceURI, "typescript", libSource),
		}},
		content: map[string][]byte{configURI: config, mainURI: main, libConfigURI: libConfig, libSourceURI: libSource},
	}
	configDigest := sha256.Sum256(config)
	scope := model.Scope{
		ID: "direct-reference", Language: "typescript", RootURI: root,
		Build: model.BuildInputs{Options: map[string]string{
			"tsconfig": "tsconfig.json", "projectConfigDigest": "sha256:" + hex.EncodeToString(configDigest[:]),
			"compilerOptions":   string(`{"target":"ES2020","strict":true}`),
			"projectReferences": string(`[ {"path":"./lib"} ]`), "plugins": "[]",
		}},
	}
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.id, Scope: scope,
		Extractor: semanticIndexExtractor, ExtractorVer: semanticIndexExtractorVersion,
		Backend:   identity.BackendID{Language: "typescript", Name: "typescript-direct-compiler"},
		Toolchain: semanticIndexToolchain, Tools: tools,
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	runner := &semanticTestRunner{}
	report, err := NewSemanticIndexProvider(SemanticIndexConfig{Runner: runner, Tools: tools}).ExportIndex(context.Background(), model.Request{
		View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance},
	}, &semanticTestSink{})
	if err != nil {
		t.Fatalf("direct project-reference ExportIndex: %v", err)
	}
	if runner.verifyCall != 0 || runner.exportCall != 0 {
		t.Fatalf("project reference without an explicit workspace boundary reached compiler: verify=%d export=%d", runner.verifyCall, runner.exportCall)
	}
	for _, fact := range model.RequiredFactKinds {
		coverage := coverageState(report.Coverage, scope.ID, fact)
		if fact == model.FactInclude {
			if coverage != model.Unavailable {
				t.Fatalf("include coverage = %s, want unavailable", coverage)
			}
			continue
		}
		if coverage != model.Unavailable {
			t.Fatalf("project reference coverage %s = %s, want unavailable without an explicit captured workspace boundary", fact, coverage)
		}
	}
}

func TestDirectCompilerRunnerClosesProjectReferencesFromWorkspaceSnapshot(t *testing.T) {
	tools := localDirectTools(t)
	workspaceRoot := "file:///repo/project-reference-closure"
	appRoot, libRoot, baseRoot := workspaceRoot+"/app", workspaceRoot+"/lib", workspaceRoot+"/base"
	appConfigURI, appMainURI := appRoot+"/tsconfig.json", appRoot+"/src/main.ts"
	libConfigURI, libSourceURI := libRoot+"/tsconfig.json", libRoot+"/src/index.ts"
	baseConfigURI, baseSourceURI := baseRoot+"/tsconfig.json", baseRoot+"/src/base.ts"
	appConfig := []byte(`{"compilerOptions":{"target":"ES2020","strict":true},"include":["src/**/*.ts"],"references":[{"path":"../lib"}]}`)
	appMain := []byte("import { LibType } from '../../lib/src/index';\nexport const result: LibType = { name: 'ok' };\n")
	libConfig := []byte(`{"compilerOptions":{"composite":true,"target":"ES2020","strict":true},"include":["src/**/*.ts"],"references":[{"path":"../base"}]}`)
	libSource := []byte("import { Base } from '../../base/src/base';\nexport interface LibType extends Base {}\n")
	baseConfig := []byte(`{"compilerOptions":{"composite":true,"target":"ES2020","strict":true},"include":["src/**/*.ts"]}`)
	baseSource := []byte("export interface Base { name: string; }\n")
	appFiles := []model.File{semanticFile(appConfigURI, "json", appConfig), semanticFile(appMainURI, "typescript", appMain)}
	libFiles := []model.File{semanticFile(libConfigURI, "json", libConfig), semanticFile(libSourceURI, "typescript", libSource)}
	baseFiles := []model.File{semanticFile(baseConfigURI, "json", baseConfig), semanticFile(baseSourceURI, "typescript", baseSource)}
	workspaceFiles := append(append(append([]model.File(nil), appFiles...), libFiles...), baseFiles...)
	view := &semanticTestView{
		id: model.Identity{Workspace: "project-reference-closure", DiskDigest: "sha256:project-reference-closure", SnapshotRev: 7},
		files: map[string][]model.File{
			workspaceRoot: workspaceFiles,
			appRoot:       appFiles,
			libRoot:       libFiles,
			baseRoot:      baseFiles,
		},
		content: map[string][]byte{
			appConfigURI: appConfig, appMainURI: appMain,
			libConfigURI: libConfig, libSourceURI: libSource,
			baseConfigURI: baseConfig, baseSourceURI: baseSource,
		},
	}
	provider := NewSemanticIndexProvider(SemanticIndexConfig{
		Runner: NewDirectCompilerRunner("node"), Tools: tools,
	})
	request, err := provider.BuildIndexRequest(context.Background(), view, workspaceRoot)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	if request.WorkspaceRootURI != workspaceRoot {
		t.Fatalf("builder workspace root = %q, want %q", request.WorkspaceRootURI, workspaceRoot)
	}
	if len(request.Scopes) != 3 {
		t.Fatalf("discovered project scopes = %d, want app, lib, and transitive base: %+v", len(request.Scopes), request.Scopes)
	}
	scopesByRoot := make(map[string]model.Scope, len(request.Scopes))
	for _, scope := range request.Scopes {
		scopesByRoot[scope.RootURI] = scope
	}
	appScope, libScope, baseScope := scopesByRoot[appRoot], scopesByRoot[libRoot], scopesByRoot[baseRoot]
	if appScope.ID == "" || libScope.ID == "" || baseScope.ID == "" {
		t.Fatalf("discovered project roots do not include the whole closure: %+v", scopesByRoot)
	}
	if appScope.BuildContext == libScope.BuildContext || libScope.BuildContext == baseScope.BuildContext || appScope.BuildContext == baseScope.BuildContext {
		t.Fatal("projects in the reference closure unexpectedly share a build context")
	}
	sink := &semanticTestSink{}
	report, err := provider.ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("project-reference ExportIndex: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	if coverageState(report.Coverage, appScope.ID, model.FactSymbol) == model.Unknown {
		t.Fatalf("captured transitive project-reference symbol coverage remained unknown: %+v", report.Coverage)
	}
	var libraryID, baseID identity.SymbolID
	for _, symbol := range sink.symbols {
		if symbol.ScopeID == libScope.ID && symbol.Name == "LibType" {
			libraryID = symbol.ID
		}
		if symbol.ScopeID == baseScope.ID && symbol.Name == "Base" {
			baseID = symbol.ID
		}
	}
	if libraryID == "" || baseID == "" {
		t.Fatalf("referenced declaration symbols are missing from their owning scopes: %+v", sink.symbols)
	}
	if !strings.Contains(string(libraryID), string(libScope.BuildContext)) || strings.Contains(string(libraryID), string(appScope.BuildContext)) {
		t.Fatalf("library semantic ID was not isolated by its own build context: %q", libraryID)
	}
	if !strings.Contains(string(baseID), string(baseScope.BuildContext)) || strings.Contains(string(baseID), string(libScope.BuildContext)) {
		t.Fatalf("transitive base semantic ID was not isolated by its own build context: %q", baseID)
	}
	var linkedImport bool
	var linkedTransitiveReference bool
	for _, occurrence := range sink.occurrences {
		if occurrence.ScopeID == appScope.ID && occurrence.URI == appMainURI && occurrence.Role == "reference" && occurrence.SymbolID == libraryID {
			linkedImport = true
		}
		if occurrence.ScopeID == libScope.ID && occurrence.URI == libSourceURI && occurrence.Role == "reference" && occurrence.SymbolID == baseID {
			linkedTransitiveReference = true
		}
		if occurrence.ScopeID == appScope.ID && occurrence.URI == libSourceURI {
			t.Fatalf("consumer scope emitted a sibling project's source occurrence: %+v", occurrence)
		}
		if occurrence.ScopeID == appScope.ID && occurrence.URI == baseSourceURI {
			t.Fatalf("consumer scope emitted a transitive sibling project's source occurrence: %+v", occurrence)
		}
	}
	if !linkedImport || !linkedTransitiveReference {
		t.Fatalf("project-reference occurrences did not link to target declaration IDs: direct=%v transitive=%v, occurrences=%+v", linkedImport, linkedTransitiveReference, sink.occurrences)
	}
	for _, fact := range []model.FactKind{model.FactImport, model.FactModule} {
		if got := coverageState(report.Coverage, appScope.ID, fact); got == model.Complete {
			t.Fatalf("project-reference scope has complete %s coverage outside the bounded module proof", fact)
		}
	}
	for _, edge := range sink.edges {
		if edge.ScopeID == appScope.ID && (edge.SourceURI == libSourceURI || edge.SourceURI == baseSourceURI) {
			t.Fatalf("consumer scope emitted a sibling project's source edge: %+v", edge)
		}
	}
}

func coverageReason(coverage []model.Coverage, scope string, fact model.FactKind) string {
	for _, item := range coverage {
		if item.ScopeID == scope && item.Fact == fact {
			return item.Reason
		}
	}
	return ""
}

func referenceRanges(occurrences []model.Occurrence, uri string, symbolID identity.SymbolID) []model.Position {
	var ranges []model.Position
	for _, occurrence := range occurrences {
		if occurrence.URI == uri && occurrence.SymbolID == symbolID && occurrence.Role == "reference" {
			ranges = append(ranges, occurrence.Range)
		}
	}
	return sortedRanges(ranges)
}

func sameRanges(left, right []model.Position) bool {
	left = sortedRanges(left)
	right = sortedRanges(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func sortedRanges(ranges []model.Position) []model.Position {
	result := append([]model.Position(nil), ranges...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].StartLine != result[j].StartLine {
			return result[i].StartLine < result[j].StartLine
		}
		if result[i].StartChar != result[j].StartChar {
			return result[i].StartChar < result[j].StartChar
		}
		if result[i].EndLine != result[j].EndLine {
			return result[i].EndLine < result[j].EndLine
		}
		return result[i].EndChar < result[j].EndChar
	})
	return result
}

// pinnedLanguageServiceReferences asks the same pinned TypeScript compiler used
// by the direct exporter for its own reference spans. This protects our LSP
// ranges from drifting into full-token spans when the LanguageService reports
// only a string's content or applies a different literal-boundary rule.
func pinnedLanguageServiceReferences(t *testing.T, tools []model.ToolIdentity, consumer, module []byte, queries map[string]int) map[string][]model.Position {
	t.Helper()
	var nodePath, compilerPath string
	for _, tool := range tools {
		switch tool.Name {
		case semanticIndexNodeName:
			nodePath = tool.Path
		case semanticIndexCompilerName:
			compilerPath = tool.Path
		}
	}
	if nodePath == "" || compilerPath == "" {
		t.Fatal("pinned TypeScript LanguageService tools are missing")
	}
	root := t.TempDir()
	consumerPath := filepath.Join(root, "consumer.ts")
	modulePath := filepath.Join(root, "module.ts")
	if err := os.WriteFile(consumerPath, consumer, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(modulePath, module, 0o600); err != nil {
		t.Fatal(err)
	}
	const script = `
const ts = require(process.argv[1]);
const fs = require('fs');
const consumerFile = process.argv[2];
const options = { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.CommonJS, strict: true };
const host = {
  getScriptFileNames: () => [consumerFile, process.argv[3]],
  getScriptVersion: () => '0',
  getScriptSnapshot(fileName) {
    const source = ts.sys.readFile(fileName);
    return source === undefined ? undefined : ts.ScriptSnapshot.fromString(source);
  },
  getCurrentDirectory: () => process.cwd(),
  getCompilationSettings: () => options,
  getDefaultLibFileName: settings => ts.getDefaultLibFilePath(settings),
  fileExists: ts.sys.fileExists,
  readFile: ts.sys.readFile,
  readDirectory: ts.sys.readDirectory,
  directoryExists: ts.sys.directoryExists,
  getDirectories: ts.sys.getDirectories,
  useCaseSensitiveFileNames: () => ts.sys.useCaseSensitiveFileNames,
  getNewLine: () => ts.sys.newLine,
};
const service = ts.createLanguageService(host);
const source = JSON.parse(fs.readFileSync(0, 'utf8'));
const program = service.getProgram();
const result = {};
for (const [label, position] of Object.entries(source)) {
  const groups = service.findReferences(consumerFile, position) || [];
  result[label] = groups.flatMap(group => group.references || []).map(reference => {
    const file = program && program.getSourceFile(reference.fileName);
    if (!file) return null;
    const start = file.getLineAndCharacterOfPosition(reference.textSpan.start);
    const end = file.getLineAndCharacterOfPosition(reference.textSpan.start + reference.textSpan.length);
    return {
      file: reference.fileName,
      isDefinition: !!reference.isDefinition,
      range: { StartLine: start.line, StartChar: start.character, EndLine: end.line, EndChar: end.character },
    };
  }).filter(Boolean);
}
process.stdout.write(JSON.stringify(result));
`
	input, err := json.Marshal(queries)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(nodePath, "-e", script, compilerPath, consumerPath, modulePath)
	command.Stdin = strings.NewReader(string(input))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned TypeScript LanguageService findReferences: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	var response map[string][]struct {
		File         string
		IsDefinition bool
		Range        model.Position
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("decode TypeScript LanguageService references: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	ranges := make(map[string][]model.Position, len(response))
	for label, references := range response {
		for _, reference := range references {
			if filepath.Base(reference.File) == "consumer.ts" && !reference.IsDefinition {
				ranges[label] = append(ranges[label], reference.Range)
			}
		}
		ranges[label] = sortedRanges(ranges[label])
	}
	return ranges
}

type languageServiceQuery struct {
	File   string `json:"file"`
	Offset int    `json:"offset"`
}

type languageServiceReference struct {
	File  string
	Range model.Position
}

type languageServiceReferenceResult struct {
	File            string
	Range           model.Position
	DefinitionFile  string
	DefinitionRange model.Position
	HasDefinition   bool
}

func pinnedLanguageServiceReferenceSets(t *testing.T, tools []model.ToolIdentity, sources map[string][]byte, queries map[string]languageServiceQuery) map[string][]languageServiceReferenceResult {
	t.Helper()
	var nodePath, compilerPath string
	for _, tool := range tools {
		switch tool.Name {
		case semanticIndexNodeName:
			nodePath = tool.Path
		case semanticIndexCompilerName:
			compilerPath = tool.Path
		}
	}
	if nodePath == "" || compilerPath == "" {
		t.Fatal("pinned TypeScript LanguageService tools are missing")
	}
	root := t.TempDir()
	files := make([]string, 0, len(sources))
	for name, content := range sources {
		files = append(files, name)
		filePath := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filePath, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(files)
	const script = `
const ts = require(process.argv[1]);
const fs = require('fs');
const path = require('path');
const root = process.argv[2];
const source = JSON.parse(fs.readFileSync(0, 'utf8'));
const javascript = source.files.some(file => /\.(?:js|jsx|mjs|cjs)$/i.test(file));
const options = { target: ts.ScriptTarget.ES2020, module: javascript ? ts.ModuleKind.ESNext : ts.ModuleKind.CommonJS, moduleResolution: javascript ? ts.ModuleResolutionKind.Bundler : undefined, strict: true, allowJs: javascript, checkJs: javascript };
const host = {
  getScriptFileNames: () => source.files.map(file => path.join(root, file)),
  getScriptVersion: () => '0',
  getScriptSnapshot(fileName) {
    const text = ts.sys.readFile(fileName);
    return text === undefined ? undefined : ts.ScriptSnapshot.fromString(text);
  },
  getCurrentDirectory: () => root,
  getCompilationSettings: () => options,
  getDefaultLibFileName: settings => ts.getDefaultLibFilePath(settings),
  fileExists: ts.sys.fileExists,
  readFile: ts.sys.readFile,
  readDirectory: ts.sys.readDirectory,
  directoryExists: ts.sys.directoryExists,
  getDirectories: ts.sys.getDirectories,
  useCaseSensitiveFileNames: () => ts.sys.useCaseSensitiveFileNames,
  getNewLine: () => ts.sys.newLine,
};
const service = ts.createLanguageService(host);
const program = service.getProgram();
const relativeName = fileName => path.relative(root, fileName).split(path.sep).join('/');
const positionOf = (fileName, span) => {
  const file = program && program.getSourceFile(fileName);
  if (!file || !span) return null;
  const start = file.getLineAndCharacterOfPosition(span.start);
  const end = file.getLineAndCharacterOfPosition(span.start + span.length);
  return { file: relativeName(fileName), range: { StartLine: start.line, StartChar: start.character, EndLine: end.line, EndChar: end.character } };
};
const result = {};
for (const [label, query] of Object.entries(source.queries)) {
  const fileName = path.join(root, query.file);
  const groups = service.findReferences(fileName, query.offset) || [];
  result[label] = groups.flatMap(group => (group.references || []).map(reference => {
    const value = positionOf(reference.fileName, reference.textSpan);
    if (!value) return null;
    const definition = group.definition && positionOf(group.definition.fileName, group.definition.textSpan);
    return {
      file: value.file,
      range: value.range,
      isDefinition: !!reference.isDefinition,
      definitionFile: definition ? definition.file : '',
      definitionRange: definition ? definition.range : null,
      hasDefinition: !!definition,
    };
  })).filter(Boolean);
}
process.stdout.write(JSON.stringify(result));
`
	input, err := json.Marshal(struct {
		Files   []string                        `json:"files"`
		Queries map[string]languageServiceQuery `json:"queries"`
	}{Files: files, Queries: queries})
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(nodePath, "-e", script, compilerPath, root)
	command.Stdin = strings.NewReader(string(input))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("pinned TypeScript LanguageService full findReferences sets: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	var response map[string][]struct {
		File            string
		IsDefinition    bool
		Range           model.Position
		DefinitionFile  string
		DefinitionRange model.Position
		HasDefinition   bool
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("decode TypeScript LanguageService full reference sets: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	result := make(map[string][]languageServiceReferenceResult, len(response))
	for label, references := range response {
		for _, reference := range references {
			if !reference.IsDefinition {
				result[label] = append(result[label], languageServiceReferenceResult{
					File: reference.File, Range: reference.Range, DefinitionFile: reference.DefinitionFile,
					DefinitionRange: reference.DefinitionRange, HasDefinition: reference.HasDefinition,
				})
			}
		}
		sort.Slice(result[label], func(i, j int) bool {
			left, right := result[label][i], result[label][j]
			if left.DefinitionFile != right.DefinitionFile {
				return left.DefinitionFile < right.DefinitionFile
			}
			if left.DefinitionRange.StartLine != right.DefinitionRange.StartLine {
				return left.DefinitionRange.StartLine < right.DefinitionRange.StartLine
			}
			if left.DefinitionRange.StartChar != right.DefinitionRange.StartChar {
				return left.DefinitionRange.StartChar < right.DefinitionRange.StartChar
			}
			if left.File != right.File {
				return left.File < right.File
			}
			if left.Range.StartLine != right.Range.StartLine {
				return left.Range.StartLine < right.Range.StartLine
			}
			return left.Range.StartChar < right.Range.StartChar
		})
	}
	return result
}

func containsLanguageServiceReference(references []languageServiceReference, candidate languageServiceReference) bool {
	for _, reference := range references {
		if reference == candidate {
			return true
		}
	}
	return false
}

func sortedLanguageServiceReferences(references []languageServiceReference) []languageServiceReference {
	result := append([]languageServiceReference(nil), references...)
	sort.Slice(result, func(i, j int) bool {
		if result[i].File != result[j].File {
			return result[i].File < result[j].File
		}
		if result[i].Range.StartLine != result[j].Range.StartLine {
			return result[i].Range.StartLine < result[j].Range.StartLine
		}
		if result[i].Range.StartChar != result[j].Range.StartChar {
			return result[i].Range.StartChar < result[j].Range.StartChar
		}
		if result[i].Range.EndLine != result[j].Range.EndLine {
			return result[i].Range.EndLine < result[j].Range.EndLine
		}
		return result[i].Range.EndChar < result[j].Range.EndChar
	})
	return result
}

func sameLanguageServiceReferences(left, right []languageServiceReference) bool {
	left = sortedLanguageServiceReferences(left)
	right = sortedLanguageServiceReferences(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func assertAllSymbolReferencesMatchLanguageService(t *testing.T, tools []model.ToolIdentity, sources map[string][]byte, sink *semanticTestSink) {
	t.Helper()
	type occurrenceRangeKey struct {
		URI   string
		Range model.Position
	}
	idsAtRange := make(map[occurrenceRangeKey]map[identity.SymbolID]struct{})
	seenIDs := make(map[identity.SymbolID]struct{})
	var ids []identity.SymbolID
	for _, occurrence := range sink.occurrences {
		if occurrence.Role != "declaration" && occurrence.Role != "reference" {
			continue
		}
		key := occurrenceRangeKey{URI: occurrence.URI, Range: occurrence.Range}
		if idsAtRange[key] == nil {
			idsAtRange[key] = make(map[identity.SymbolID]struct{})
		}
		idsAtRange[key][occurrence.SymbolID] = struct{}{}
		if _, seen := seenIDs[occurrence.SymbolID]; !seen {
			seenIDs[occurrence.SymbolID] = struct{}{}
			ids = append(ids, occurrence.SymbolID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	queries := make(map[string]languageServiceQuery, len(ids))
	for _, id := range ids {
		var anchor *model.Occurrence
		for i := range sink.occurrences {
			occurrence := &sink.occurrences[i]
			if occurrence.SymbolID != id || (occurrence.Role != "declaration" && occurrence.Role != "reference") {
				continue
			}
			if len(idsAtRange[occurrenceRangeKey{URI: occurrence.URI, Range: occurrence.Range}]) == 1 {
				anchor = occurrence
				if occurrence.Role == "declaration" {
					break
				}
			}
		}
		if anchor == nil {
			t.Errorf("symbol %q has no unambiguous LanguageService query anchor", id)
			continue
		}
		queries[string(id)] = languageServiceQuery{
			File: uriSourceName(anchor.URI, sources), Offset: languageServiceOffset(anchor.URI, anchor.Range.StartLine, anchor.Range.StartChar, sources),
		}
	}
	oracle := pinnedLanguageServiceReferenceSets(t, tools, sources, queries)
	for _, id := range ids {
		if _, ok := queries[string(id)]; !ok {
			continue
		}
		var expected []languageServiceReference
		var declarations []languageServiceReference
		for _, occurrence := range sink.occurrences {
			if occurrence.SymbolID != id {
				continue
			}
			candidate := languageServiceReference{File: uriSourceName(occurrence.URI, sources), Range: occurrence.Range}
			switch occurrence.Role {
			case "reference":
				expected = append(expected, candidate)
			case "declaration":
				declarations = append(declarations, candidate)
			}
		}
		var got []languageServiceReference
		for _, reference := range oracle[string(id)] {
			candidate := languageServiceReference{File: reference.File, Range: reference.Range}
			if containsLanguageServiceReference(declarations, candidate) && !containsLanguageServiceReference(expected, candidate) {
				continue
			}
			if len(declarations) != 0 {
				definition := languageServiceReference{File: reference.DefinitionFile, Range: reference.DefinitionRange}
				if !reference.HasDefinition || !containsLanguageServiceReference(declarations, definition) {
					continue
				}
			}
			got = append(got, candidate)
		}
		if !sameLanguageServiceReferences(expected, got) {
			t.Errorf("symbol %q full references differ from TypeScript 6.0.3 LanguageService: exporter=%v languageService=%v", id, expected, got)
		}
	}
}

func uriBaseName(uri string) string {
	if slash := strings.LastIndex(uri, "/"); slash >= 0 {
		return uri[slash+1:]
	}
	return uri
}

func uriSourceName(uri string, sources map[string][]byte) string {
	normalizedURI := strings.ReplaceAll(uri, "\\", "/")
	bestName := ""
	for name := range sources {
		normalizedName := strings.ReplaceAll(name, "\\", "/")
		if (normalizedURI == normalizedName || strings.HasSuffix(normalizedURI, "/"+normalizedName)) && len(normalizedName) > len(bestName) {
			bestName = name
		}
	}
	if bestName != "" {
		return bestName
	}
	return uriBaseName(uri)
}

func languageServiceOffset(uri string, line uint32, character uint32, sources map[string][]byte) int {
	content, ok := sources[uriSourceName(uri, sources)]
	if !ok {
		return 0
	}
	lines := strings.Split(string(content), "\n")
	if int(line) >= len(lines) {
		return len(content)
	}
	offset := 0
	for index := 0; index < int(line); index++ {
		offset += len(lines[index]) + 1
	}
	currentUnits := uint32(0)
	for byteIndex, value := range lines[line] {
		if currentUnits >= character {
			return offset + byteIndex
		}
		currentUnits++
		if value > 0xFFFF {
			currentUnits++
		}
	}
	return offset + len(lines[line])
}

func localDirectTools(t *testing.T) []model.ToolIdentity {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("local Node fixture unavailable: %v", err)
	}
	nodePath, err = filepath.Abs(nodePath)
	if err != nil {
		t.Fatal(err)
	}
	nodeBytes, err := os.ReadFile(nodePath)
	if err != nil {
		t.Skipf("local Node executable unavailable: %v", err)
	}
	nodeDigest := sha256.Sum256(nodeBytes)
	nodeVersionOutput, err := exec.Command(nodePath, "--version").CombinedOutput()
	if err != nil {
		t.Skipf("local Node executable cannot be probed: %v (%s)", err, strings.TrimSpace(string(nodeVersionOutput)))
	}
	nodeVersion := normalizeRuntimeVersion(string(nodeVersionOutput))
	if nodeVersion == "" {
		t.Skip("local Node executable returned an empty version")
	}
	compilerPath := filepath.Join("..", "..", "..", "test", "acceptance", "tools", "node_modules", "typescript", "lib", "typescript.js")
	compilerPath, err = filepath.Abs(compilerPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(compilerPath); err != nil {
		t.Skipf("local TypeScript 6.0.3 fixture unavailable: %v", err)
	}
	compilerBytes, err := os.ReadFile(compilerPath)
	if err != nil {
		t.Fatal(err)
	}
	compilerDigest := sha256.Sum256(compilerBytes)
	exporterPath, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test candidate executable: %v", err)
	}
	exporterPath, err = filepath.Abs(exporterPath)
	if err != nil {
		t.Fatalf("normalize test candidate executable: %v", err)
	}
	exporterBytes, err := os.ReadFile(exporterPath)
	if err != nil {
		t.Fatalf("read test candidate executable: %v", err)
	}
	exporterDigest := sha256.Sum256(exporterBytes)
	return []model.ToolIdentity{
		{Name: semanticIndexCompilerName, Version: semanticIndexCompilerVersion, Path: compilerPath, SHA256: hex.EncodeToString(compilerDigest[:])},
		{Name: semanticIndexNodeName, Version: nodeVersion, Path: nodePath, SHA256: hex.EncodeToString(nodeDigest[:])},
		{Name: semanticIndexDirectExporterName, Version: semanticIndexExtractorVersion, Path: exporterPath, SHA256: hex.EncodeToString(exporterDigest[:])},
	}
}
