package pyright

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/index/model"
)

func TestRuntimeSemanticProviderPinsTheAnalyzerUsedByExport(t *testing.T) {
	_, request, _ := directCompilerFixture(t, "from .base import Base\n")
	for _, tool := range request.Tools {
		switch tool.Name {
		case semanticIndexRuntimeName:
			t.Setenv(semanticPyrightNodeEnv, tool.Path)
		case semanticIndexCompilerName:
			t.Setenv(semanticPyrightInternalEnv, tool.Path)
		case semanticIndexVendorName:
			t.Setenv(semanticPyrightVendorEnv, tool.Path)
		case "python":
			t.Setenv(semanticPyrightPythonEnv, tool.Path)
		}
	}
	provider, err := NewRuntimeSemanticIndexProvider(context.Background(), &Backend{})
	if err != nil {
		t.Fatalf("pin production tools: %v", err)
	}
	if _, err := directPyrightTools(provider.config.Tools); err != nil {
		t.Fatalf("runner rejected production tool set: %v", err)
	}
	badBundle := filepath.Join(t.TempDir(), "pyright-internal.js")
	if err := os.WriteFile(badBundle, []byte("modified analyzer"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(semanticPyrightInternalEnv, badBundle)
	if _, err := NewRuntimeSemanticIndexProvider(context.Background(), &Backend{}); err == nil {
		t.Fatal("modified Pyright analyzer was accepted")
	}
}

func TestDirectPyrightRunnerAcceptsCaseInsensitivePythonExtensions(t *testing.T) {
	runner, request, _ := directCompilerFixture(t, "from .base import Base\n")
	originalURI := request.Files[1].URI
	uppercaseURI := strings.TrimSuffix(originalURI, ".py") + ".PY"
	request.Files[1].URI = uppercaseURI
	materialized, ok := request.Materialized.(semanticTestMaterialized)
	if !ok {
		t.Fatalf("unexpected materialized view %T", request.Materialized)
	}
	materialized.paths[uppercaseURI] = materialized.paths[originalURI]
	delete(materialized.paths, originalURI)
	request.Materialized = materialized

	var emittedBase bool
	_, err := runner.Export(context.Background(), request, func(batch PyrightSemanticBatch) error {
		for _, symbol := range batch.Symbols {
			if symbol.Name == "Base" {
				emittedBase = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("export uppercase .PY source: %v", err)
	}
	if !emittedBase {
		t.Fatal("Pyright did not export symbols from the uppercase .PY source")
	}
}

func TestDirectPyrightRunnerExportsVariableDefinitions(t *testing.T) {
	runner, request, _ := directCompilerFixture(t, "VALUE = 1\n\ndef read():\n    return VALUE\n")
	var valueID string
	var occurrences []model.Occurrence
	_, err := runner.Export(context.Background(), request, func(batch PyrightSemanticBatch) error {
		occurrences = append(occurrences, batch.Occurrences...)
		for _, symbol := range batch.Symbols {
			if symbol.Name == "VALUE" {
				valueID = string(symbol.ID)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("export Python variable definition: %v", err)
	}
	if valueID == "" {
		t.Fatal("Pyright did not emit the VALUE symbol")
	}
	want := model.Position{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 5}
	if !hasSemanticOccurrenceAt(occurrences, valueID, "file:///repo/direct/pkg/use.py", want, "definition") {
		t.Fatalf("VALUE definition occurrence missing at its assignment: %+v", occurrences)
	}
}

func TestDirectPyrightRequestRequiresPinnedInterpreterAndConfigDigest(t *testing.T) {
	_, request, _ := directCompilerFixture(t, "from .base import Base\n")
	tools, err := directPyrightTools(request.Tools)
	if err != nil {
		t.Fatalf("read pinned Pyright tools: %v", err)
	}
	wrongInterpreter := filepath.Join(t.TempDir(), "python.exe")
	if err := os.WriteFile(wrongInterpreter, []byte("another interpreter"), 0o600); err != nil {
		t.Fatal(err)
	}
	request.Scope.Build.Environment["pythonInterpreter"] = wrongInterpreter
	if _, err := makeDirectPyrightRequest(request, tools); err == nil {
		t.Fatal("BuildInputs accepted a different Python executable than the pinned interpreter")
	}
	request.Scope.Build.Environment["pythonInterpreter"] = tools.python.Path

	materialized := request.Materialized.(semanticTestMaterialized)
	configURI := request.Files[0].URI
	if err := os.WriteFile(materialized.paths[configURI], []byte(`{"typeCheckingMode":"basic"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := makeDirectPyrightRequest(request, tools); err == nil {
		t.Fatal("materialized Pyright config changed without invalidating its BuildInputs digest")
	}
}

func TestDirectPyrightRunnerMarksParseErrorsIncomplete(t *testing.T) {
	runner, request, _ := directCompilerFixture(t, "def broken(:\n    return 1\n")
	result, err := runner.Export(context.Background(), request, func(PyrightSemanticBatch) error { return nil })
	if err != nil {
		t.Fatalf("export source with parse errors: %v", err)
	}
	for _, fact := range []model.FactKind{
		model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference, model.FactCall,
	} {
		if got := coverageState(result.Coverage, request.Scope.ID, fact); got != model.IncompleteKnownSubset {
			t.Errorf("coverage for %s = %s, want incomplete_known_subset", fact, got)
		}
	}
}

func TestDirectPyrightRunnerMarksUnmodeledLambdaScopeIncomplete(t *testing.T) {
	runner, request, _ := directCompilerFixture(t, "unused = lambda argument: 1\n")
	result, err := runner.Export(context.Background(), request, func(PyrightSemanticBatch) error { return nil })
	if err != nil {
		t.Fatalf("export lambda scope: %v", err)
	}
	for _, fact := range []model.FactKind{
		model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference, model.FactCall,
	} {
		if got := coverageState(result.Coverage, request.Scope.ID, fact); got != model.IncompleteKnownSubset {
			t.Errorf("coverage for %s = %s, want incomplete_known_subset", fact, got)
		}
	}
}

func TestDirectPyrightRunnerMapsCarriageReturnOnlySourcePositions(t *testing.T) {
	useSource := strings.Join([]string{
		"# 😀", "from .base import Base", "", "class Child(Base):", "    pass", "",
	}, "\r")
	runner, request, _ := directCompilerFixture(t, useSource)
	var symbols []model.Symbol
	var occurrences []model.Occurrence
	_, err := runner.Export(context.Background(), request, func(batch PyrightSemanticBatch) error {
		symbols = append(symbols, batch.Symbols...)
		occurrences = append(occurrences, batch.Occurrences...)
		return nil
	})
	if err != nil {
		t.Fatalf("export CR-only source: %v", err)
	}
	baseID := ""
	for _, symbol := range symbols {
		if symbol.Name == "Base" {
			baseID = string(symbol.ID)
			break
		}
	}
	if baseID == "" {
		t.Fatal("Pyright did not emit the imported Base symbol")
	}
	want := model.Position{StartLine: 3, StartChar: 12, EndLine: 3, EndChar: 16}
	if !hasSemanticOccurrenceAt(occurrences, baseID, "file:///repo/direct/pkg/use.py", want, "reference") {
		t.Fatalf("CR-only source reference did not map to UTF-16 line 3, chars 12-16: %+v", occurrences)
	}
}

func TestDirectPyrightRunnerResolvesAliasedProjectImports(t *testing.T) {
	runner, request, _ := directCompilerFixture(t, "from .base import Base as Alias\n\nclass Child(Alias):\n    pass\n")
	var symbols []model.Symbol
	var edges []model.Edge
	_, err := runner.Export(context.Background(), request, func(batch PyrightSemanticBatch) error {
		symbols = append(symbols, batch.Symbols...)
		edges = append(edges, batch.Edges...)
		return nil
	})
	if err != nil {
		t.Fatalf("export aliased project import: %v", err)
	}
	baseID, childID := "", ""
	for _, symbol := range symbols {
		switch symbol.Name {
		case "Base":
			baseID = string(symbol.ID)
		case "Child":
			childID = string(symbol.ID)
		}
	}
	if baseID == "" || childID == "" {
		t.Fatalf("aliased import did not retain project symbol identities: %+v", symbols)
	}
	if !hasSemanticEdge(edges, childID, baseID, model.EdgeImplementation) {
		t.Fatalf("aliased class base did not resolve to Base: %+v", edges)
	}
}

func TestParseSupportedPyrightConfigHandlesJSONCAndRejectsTrailingData(t *testing.T) {
	content := []byte(`{
		// Pyright accepts comments and trailing commas in JSONC configs.
		"pythonVersion": "3.13",
		"extraPaths": ["src",],
		"strict": ["src",],
		"include": ["src/**/*.py",],
		"exclude": ["src/generated/**",],
	}`)
	unsupported, options, paths := parseSupportedPyrightConfig(content)
	if unsupported != "" {
		t.Fatalf("valid JSONC config was marked unsupported: %s", unsupported)
	}
	if options["pythonVersion"] != "3.13" || len(paths) != 1 || paths[0] != "src" {
		t.Fatalf("parsed options = %#v, extra paths = %#v", options, paths)
	}
	if options["pyrightInclude"] != `["src/**/*.py"]` || options["pyrightExclude"] != `["src/generated/**"]` {
		t.Fatalf("include/exclude file specs were not retained in build options: %#v", options)
	}
	unsupported, _, _ = parseSupportedPyrightConfig([]byte(`{"pythonVersion":"3.13"} {"pythonVersion":"3.12"}`))
	if unsupported == "" {
		t.Fatal("trailing JSON value was accepted as a Pyright config")
	}
	for _, invalid := range []string{
		`{"pythonVersion":"3.13.1"}`,
		`{"pythonPlatform":"FreeBSD"}`,
		`{"typeCheckingMode":"unknown"}`,
		`{"strict":true}`,
		`{"include":"src"}`,
		`{"exclude":null}`,
		`{"include":[null]}`,
		`{"include":["../outside"]}`,
		`{"exclude":["C:\\outside"]}`,
	} {
		unsupported, _, _ = parseSupportedPyrightConfig([]byte(invalid))
		if unsupported == "" {
			t.Errorf("invalid Pyright setting %s was accepted", invalid)
		}
	}
}
