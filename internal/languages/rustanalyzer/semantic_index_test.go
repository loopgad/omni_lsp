package rustanalyzer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/omnilsp/omni/internal/languages/nested"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

var rustTestCanonicalRootURI = workspaceuri.FromPath(filepath.Join(os.TempDir(), "omnilsp-captured-rust")).Canonical()
var rustTestRootURI = rustTestCanonicalRootURI + "/"

func TestRustAnalyzerExportImportsGroundedFactsAndPreservesScope(t *testing.T) {
	view := newRustTestView(t, map[string][]byte{
		rustTestRootURI + "src/lib.rs": []byte("// 🫠\npub fn café() {}\npub fn use_it() { café(); }\n"),
	})
	scope := rustTestScope("crate-a", rustTestRootURI, []string{"serde"}, "x86_64-unknown-linux-gnu")
	tools := rustTestTools(t, false)
	request := rustTestRequest(view, scope, tools)
	scope = request.Scopes[0]

	var exports int
	runner := func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(invocation.Args) == 1 && invocation.Args[0] == "--version" {
			return []byte(invocation.Tool.Version + "\n"), nil
		}
		if len(invocation.Args) == 0 || invocation.Args[0] != "scip" {
			return nil, fmt.Errorf("unexpected invocation: %#v", invocation.Args)
		}
		exports++
		if invocation.Tool.Name != rustAnalyzerToolName {
			t.Fatalf("without an optional semantic helper SCIP export used %q, want the live rust-analyzer identity", invocation.Tool.Name)
		}
		if invocation.Dir == "" || !filepath.IsAbs(invocation.Dir) {
			t.Fatalf("export did not receive a materialized absolute workspace: %q", invocation.Dir)
		}
		if !containsEnv(invocation.Env, "CARGO_TARGET_DIR=") {
			t.Fatalf("isolated Cargo target directory missing from child environment: %v", invocation.Env)
		}
		var config struct {
			Cargo struct {
				Features  []string `json:"features"`
				Target    string   `json:"target"`
				TargetDir string   `json:"targetDir"`
				ExtraArgs []string `json:"extraArgs"`
			} `json:"cargo"`
			ProcMacro struct {
				Enable bool `json:"enable"`
			} `json:"procMacro"`
		}
		configPath := invocation.Args[5]
		configData, err := os.ReadFile(configPath)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(configData, &config); err != nil {
			return nil, err
		}
		if !equalStrings(config.Cargo.Features, []string{"serde"}) || config.Cargo.Target != "x86_64-unknown-linux-gnu" || config.ProcMacro.Enable || !containsArg(config.Cargo.ExtraArgs, "--locked") {
			t.Fatalf("rust-analyzer config lost scope inputs: %+v", config)
		}
		target := filepath.Clean(filepath.Join(invocation.Dir, filepath.FromSlash(config.Cargo.TargetDir)))
		if pathWithin(invocation.Dir, target) {
			t.Fatalf("Cargo target output is inside the source snapshot: %s", target)
		}
		return rustSCIPFixtureAtRoot(t, workspaceuri.FromPath(invocation.Dir).Canonical(), "src/lib.rs"), nil
	}

	sink := &rustTestSink{}
	report, err := exportRustIndex(context.Background(), request, sink, runner)
	if err != nil {
		t.Fatalf("export Rust index: %v", err)
	}
	if exports != 1 {
		t.Fatalf("SCIP export count = %d, want 1", exports)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("invalid report: %v", err)
	}
	if len(sink.symbols) == 0 || len(sink.occurrences) != 2 {
		t.Fatalf("facts were not imported: %d symbols, %d occurrences", len(sink.symbols), len(sink.occurrences))
	}
	if len(sink.edges) != 2 || sink.edges[0].ScopeID != scope.ID || sink.edges[1].ScopeID != scope.ID {
		t.Fatalf("SCIP implementation/type edges were not scoped: %+v", sink.edges)
	}
	var sawDefinition, sawReference bool
	for _, occurrence := range sink.occurrences {
		if occurrence.ScopeID != scope.ID || occurrence.BuildContext != scope.BuildContext || occurrence.SourceHash == "" {
			t.Fatalf("occurrence lost scope/source provenance: %+v", occurrence)
		}
		if occurrence.Role == "definition" {
			sawDefinition = occurrence.Range.StartLine == 1 && occurrence.Range.StartChar == 7 && occurrence.Range.EndChar == 11
		}
		if occurrence.Role == "reference" {
			sawReference = occurrence.Range.StartLine == 2
		}
	}
	if !sawDefinition || !sawReference {
		t.Fatalf("UTF-8 SCIP ranges were not converted to UTF-16: %+v", sink.occurrences)
	}
	for _, fact := range []model.FactKind{model.FactCall, model.FactImport, model.FactInclude, model.FactModule, model.FactGenerated} {
		coverage := coverageFor(report, scope.ID, fact)
		if coverage.State != model.Unknown {
			t.Fatalf("unsupported %s coverage must remain unknown, got %+v", fact, coverage)
		}
	}
	for _, fact := range []model.FactKind{model.FactImplementation, model.FactTypeRelation} {
		coverage := coverageFor(report, scope.ID, fact)
		if coverage.State != model.IncompleteKnownSubset || !strings.Contains(coverage.Reason, "does not prove exhaustive relationship coverage") {
			t.Fatalf("SCIP %s coverage must remain an explicitly incomplete known subset, got %+v", fact, coverage)
		}
	}
	if !rustSameToolSet(report.UsedTools[scope.ID], tools) {
		t.Fatalf("verified tool provenance differs: got %v want %v", report.UsedTools[scope.ID], tools)
	}
}

func TestRustSCIPRelationCoverageCannotBeUpgradedToComplete(t *testing.T) {
	report := model.Report{Coverage: []model.Coverage{
		{ScopeID: "rust", Fact: model.FactImplementation, State: model.Complete},
		{ScopeID: "rust", Fact: model.FactTypeRelation, State: model.IncompleteKnownSubset},
		{ScopeID: "rust", Fact: model.FactReference, State: model.Complete},
		{ScopeID: "other", Fact: model.FactImplementation, State: model.Complete},
	}}

	constrainRustSCIPRelationCoverage(&report, "rust")
	for _, fact := range []model.FactKind{model.FactImplementation, model.FactTypeRelation} {
		coverage := coverageFor(report, "rust", fact)
		if coverage.State != model.IncompleteKnownSubset || !strings.Contains(coverage.Reason, "does not prove exhaustive relationship coverage") {
			t.Fatalf("Rust SCIP %s coverage = %+v, want conservative relation subset", fact, coverage)
		}
	}
	if got := coverageFor(report, "rust", model.FactReference).State; got != model.Complete {
		t.Fatalf("non-relation coverage was changed: %s", got)
	}
	if got := coverageFor(report, "other", model.FactImplementation).State; got != model.Complete {
		t.Fatalf("another scope's coverage was changed: %s", got)
	}
}

func TestRustCapturedSourceHashUsesCanonicalIdentityAndRejectsMutation(t *testing.T) {
	uri := rustTestRootURI + "src/lib.rs"
	view := newRustTestView(t, map[string][]byte{uri: []byte("pub fn captured() {}\n")})
	files, err := rustScopeFiles(context.Background(), view, rustTestRootURI)
	if err != nil {
		t.Fatalf("capture Rust source manifest with canonical hashes: %v", err)
	}
	file, ok := files[uri]
	if !ok || !strings.HasPrefix(string(file.SHA256), "sha256:") || !validRustContentHash(string(file.SHA256)) {
		t.Fatalf("captured Rust file hash is not a canonical model identity: %+v", file)
	}
	for _, malformed := range []string{
		strings.TrimPrefix(string(file.SHA256), "sha256:"),
		"sha256:not-a-digest",
		"sha256:" + strings.Repeat("0", 63),
	} {
		if validRustContentHash(malformed) {
			t.Fatalf("accepted noncanonical Rust source hash %q", malformed)
		}
	}

	file.SHA256 = identity.ContentHash("sha256:" + strings.Repeat("0", 64))
	if _, err := readRustCapturedFile(context.Background(), view, file); err == nil {
		t.Fatal("accepted captured Rust bytes whose digest differs from the immutable file manifest")
	}
}

func TestRustSemanticHelperImportsCallAndModuleEdgesWithUTF16Sources(t *testing.T) {
	contents := "// 🫠\npub fn caller() { let emoji = \"🫠\"; target(); }\npub fn target() {}\npub mod child;\n"
	view := newRustTestView(t, map[string][]byte{rustTestRootURI + "src/lib.rs": []byte(contents)})
	helper := rustTestSemanticHelper(t)
	tools := append(rustTestTools(t, false), helper)
	request := rustTestRequest(view, rustTestScope("crate", rustTestRootURI, nil, ""), tools)
	scope := request.Scopes[0]
	lines := strings.Split(contents, "\n")
	callStart := uint32(len(utf16.Encode([]rune(lines[1][:strings.Index(lines[1], "target")]))))
	moduleStart := uint32(strings.Index(lines[3], "child"))
	sidecar := rustRelationSidecar{SchemaVersion: 1, Relations: []rustSidecarRelation{
		rustTestSidecarRelation("call", "src/lib.rs", "rust . crate . caller().", "rust . crate . target().", 1, callStart, 1, callStart+6),
		rustTestSidecarRelation("module", "src/lib.rs", "rust . crate .", "rust . crate . child/", 3, moduleStart, 3, moduleStart+5),
	}}
	encodedSidecar, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	sink := &rustTestSink{}
	report, err := exportRustIndex(context.Background(), request, sink, func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
		if len(invocation.Args) == 1 && invocation.Args[0] == "--version" {
			return []byte(invocation.Tool.Version + "\n"), nil
		}
		if invocation.Tool != helper || !containsArg(invocation.Args, "--output") || invocation.OutputPath == "" {
			return nil, fmt.Errorf("semantic helper invocation is incomplete: %+v", invocation)
		}
		if err := os.WriteFile(invocation.OutputPath+".relations.json", encodedSidecar, 0o600); err != nil {
			return nil, err
		}
		return rustSCIPRelationFixtureAtRoot(t, workspaceuri.FromPath(invocation.Dir).Canonical(), "src/lib.rs", contents), nil
	})
	if err != nil {
		t.Fatalf("export helper relations: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("invalid helper report: %v", err)
	}
	if len(sink.edges) != 2 {
		t.Fatalf("helper emitted %d edges, want call and module: %+v", len(sink.edges), sink.edges)
	}
	wantKinds := map[model.EdgeKind]bool{model.EdgeCall: false, model.EdgeModule: false}
	for _, edge := range sink.edges {
		if _, ok := wantKinds[edge.Kind]; !ok || edge.ScopeID != scope.ID || edge.BuildContext != scope.BuildContext ||
			edge.SourceURI != rustTestRootURI+"src/lib.rs" || edge.SourceHash != viewFileHash(t, view, edge.SourceURI) {
			t.Fatalf("helper edge lost its immutable source provenance: %+v", edge)
		}
		wantKinds[edge.Kind] = true
		if edge.Kind == model.EdgeCall && (edge.From != "rust . crate . caller()." || edge.To != "rust . crate . target()." || edge.Range.StartLine != 1 || edge.Range.StartChar != callStart || edge.Range.EndChar != callStart+6) {
			t.Fatalf("call edge has the wrong symbols or UTF-16 range: %+v", edge)
		}
		if edge.Kind == model.EdgeModule && (edge.From != "rust . crate ." || edge.To != "rust . crate . child/" || edge.Range.StartLine != 3 || edge.Range.StartChar != moduleStart || edge.Range.EndChar != moduleStart+5) {
			t.Fatalf("module edge has the wrong symbols or range: %+v", edge)
		}
	}
	if !wantKinds[model.EdgeCall] || !wantKinds[model.EdgeModule] {
		t.Fatalf("helper omitted call or module edge: %+v", sink.edges)
	}
}

func TestRustSemanticHelperRejectsMissingOrCorruptSidecarBeforeImport(t *testing.T) {
	contents := "// 🫠\npub fn caller() { target(); }\npub fn target() {}\n"
	for _, test := range []struct {
		name    string
		sidecar []byte
		write   bool
	}{
		{name: "missing"},
		{name: "corrupt", sidecar: []byte("{not json"), write: true},
		{name: "unsupported schema", sidecar: []byte(`{"schema_version":3,"relations":[]}`), write: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			view := newRustTestView(t, map[string][]byte{rustTestRootURI + "src/lib.rs": []byte(contents)})
			helper := rustTestSemanticHelper(t)
			tools := append(rustTestTools(t, false), helper)
			request := rustTestRequest(view, rustTestScope("crate", rustTestRootURI, nil, ""), tools)
			sink := &rustTestSink{}
			report, err := exportRustIndex(context.Background(), request, sink, func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
				if len(invocation.Args) == 1 && invocation.Args[0] == "--version" {
					return []byte(invocation.Tool.Version + "\n"), nil
				}
				if test.write {
					if err := os.WriteFile(invocation.OutputPath+".relations.json", test.sidecar, 0o600); err != nil {
						return nil, err
					}
				}
				return rustSCIPRelationFixtureAtRoot(t, workspaceuri.FromPath(invocation.Dir).Canonical(), "src/lib.rs", contents), nil
			})
			if err != nil {
				t.Fatalf("export failure should be represented by unavailable coverage: %v", err)
			}
			if len(sink.symbols) != 0 || len(sink.occurrences) != 0 || len(sink.edges) != 0 {
				t.Fatalf("invalid helper sidecar reached the fact sink: symbols=%d occurrences=%d edges=%d", len(sink.symbols), len(sink.occurrences), len(sink.edges))
			}
			for _, coverage := range report.Coverage {
				if coverage.State != model.Unavailable || !strings.Contains(coverage.Reason, "sidecar") && !strings.Contains(coverage.Reason, "semantic relation") {
					t.Fatalf("invalid helper sidecar did not fail closed: %+v", coverage)
				}
			}
		})
	}
}

func TestRustSemanticHelperPromotesOnlyVerifiedWitnessFacts(t *testing.T) {
	contents := "// 🫠\npub fn café() {}\npub fn use_it() { café(); }\n"
	view := newRustTestView(t, map[string][]byte{rustTestRootURI + "src/lib.rs": []byte(contents)})
	helper := rustTestSemanticHelper(t)
	tools := append(rustTestTools(t, false), helper)
	request := rustTestRequest(view, rustTestScope("crate", rustTestRootURI, nil, ""), tools)
	scope := request.Scopes[0]
	sink := &rustTestSink{}
	report, err := exportRustIndex(context.Background(), request, sink, func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
		if len(invocation.Args) == 1 && invocation.Args[0] == "--version" {
			return []byte(invocation.Tool.Version + "\n"), nil
		}
		for i, arg := range invocation.Args {
			if arg == "--coverage-root" && i+1 < len(invocation.Args) && filepath.ToSlash(invocation.Args[i+1]) == "src/lib.rs" {
				data := rustSCIPFixtureAtRoot(t, workspaceuri.FromPath(invocation.Dir).Canonical(), "src/lib.rs")
				sidecar := rustTestCompletenessSidecar(t, data, contents, nil)
				encoded, marshalErr := marshalRustTestCompletenessSidecar(sidecar)
				if marshalErr != nil {
					return nil, marshalErr
				}
				if writeErr := os.WriteFile(invocation.OutputPath+".relations.json", encoded, 0o600); writeErr != nil {
					return nil, writeErr
				}
				return data, nil
			}
		}
		return nil, fmt.Errorf("helper invocation omitted the selected Cargo target root: %v", invocation.Args)
	})
	if err != nil {
		t.Fatalf("export verified Rust helper facts: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("invalid witness-backed report: %v", err)
	}
	for _, fact := range []model.FactKind{model.FactSymbol, model.FactDefinition, model.FactReference} {
		if got := coverageFor(report, scope.ID, fact); got.State != model.Complete || !strings.Contains(got.Reason, "same-response") {
			t.Fatalf("verified Rust %s coverage = %+v, want witness-backed complete", fact, got)
		}
	}
	if got := coverageFor(report, scope.ID, model.FactDeclaration); got.State != model.Unknown {
		t.Fatalf("Rust witness incorrectly promoted declaration coverage: %+v", got)
	}
	if len(sink.symbols) == 0 || len(sink.occurrences) == 0 {
		t.Fatalf("witness integration did not import SCIP facts: %d symbols, %d occurrences", len(sink.symbols), len(sink.occurrences))
	}
}

func TestRustCompletenessWitnessRejectsMismatchedBindings(t *testing.T) {
	contents := "// 🫠\npub fn café() {}\npub fn use_it() { café(); }\n"
	view := newRustTestView(t, map[string][]byte{rustTestRootURI + "src/lib.rs": []byte(contents)})
	scope := rustTestScope("crate", rustTestRootURI, nil, "")
	files, err := rustScopeFiles(context.Background(), view, scope.RootURI)
	if err != nil {
		t.Fatal(err)
	}
	indexData := rustSCIPFixtureAtRoot(t, rustTestCanonicalRootURI, "src/lib.rs")
	rawDigest := sha256.Sum256(indexData)
	valid := rustTestCompletenessSidecar(t, indexData, contents, nil)
	validate := func(sidecar rustRelationSidecar) (rustCompletenessValidation, error) {
		t.Helper()
		data, marshalErr := marshalRustTestCompletenessSidecar(sidecar)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return validateRustCompletenessWitness(context.Background(), indexData, data, rawDigest, scope, files, view)
	}
	if got, err := validate(valid); err != nil || !got.Complete {
		t.Fatalf("valid same-response witness was rejected: %+v, %v", got, err)
	}

	badSCIP := valid
	badSCIP.SCIPSHA256 = strings.Repeat("0", sha256.Size*2)
	if _, err := validate(badSCIP); err == nil || !strings.Contains(err.Error(), "exact SCIP response") {
		t.Fatalf("mismatched SCIP response digest was accepted: %v", err)
	}
	badSource := valid
	badSource.SourceManifest = append([]rustWitnessSource(nil), valid.SourceManifest...)
	badSource.SourceManifest[0].SHA256 = strings.Repeat("0", sha256.Size*2)
	if _, err := validate(badSource); err == nil || !strings.Contains(err.Error(), "source hash") {
		t.Fatalf("mismatched captured-source digest was accepted: %v", err)
	}
	badRoot := valid
	badRoot.SelectedCrateRoot = "src/other.rs"
	if _, err := validate(badRoot); err == nil || !strings.Contains(err.Error(), "does not match Cargo target") {
		t.Fatalf("witness for another Cargo target was accepted: %v", err)
	}
	missingRoot := valid
	missingRoot.SourceManifest = nil
	if _, err := validate(missingRoot); err == nil || !strings.Contains(err.Error(), "source_manifest") {
		t.Fatalf("witness with no source manifest was accepted: %v", err)
	}
	badCount := valid
	badCount.WitnessCounts = cloneRustWitnessCounts(valid.WitnessCounts)
	badCount.WitnessCounts.SCIPOccurrences++
	if _, err := validate(badCount); err == nil || !strings.Contains(err.Error(), "counts differ") {
		t.Fatalf("witness with a false SCIP occurrence count was accepted: %v", err)
	}
	blocked := valid
	blocked.Blockers = []string{"macro-syntax"}
	blocked.WitnessCounts = cloneRustWitnessCounts(valid.WitnessCounts)
	blocked.WitnessCounts.MacroSites = 1
	if got, err := validate(blocked); err != nil || got.Complete {
		t.Fatalf("macro-blocked witness established complete coverage: %+v, %v", got, err)
	}
}

func TestRustSemanticHelperMapsGeneratedSourceEdgesThroughCapturedSourceMap(t *testing.T) {
	contents := "// 🫠\npub fn caller() { let emoji = \"🫠\"; target(); }\npub fn target() {}\n"
	generatedURI := rustTestRootURI + ".generated/expanded.rs"
	sourceURI := rustTestRootURI + "src/lib.rs"
	base := newRustTestView(t, map[string][]byte{
		generatedURI: []byte(contents),
		sourceURI:    []byte(contents),
	})
	lines := strings.Split(contents, "\n")
	lineText := lines[1]
	callByteColumn := strings.Index(lineText, "target")
	callStart := uint32(len(utf16.Encode([]rune(lineText[:callByteColumn]))))
	spanEnd := uint32(len(utf16.Encode([]rune(lineText))))
	view := rustTestGeneratedView{
		rustTestView: base, generatedURI: generatedURI, sourceURI: sourceURI,
		sourceMap: []model.SourceMapSpan{{
			Generated: model.Position{StartLine: 1, StartChar: 0, EndLine: 1, EndChar: spanEnd},
			SourceURI: sourceURI,
			Source:    model.Position{StartLine: 1, StartChar: 0, EndLine: 1, EndChar: spanEnd},
		}},
	}
	helper := rustTestSemanticHelper(t)
	tools := append(rustTestTools(t, false), helper)
	request := rustTestRequest(view, rustTestScope("crate", rustTestRootURI, nil, ""), tools)
	sink := &rustTestSink{}
	report, err := exportRustIndex(context.Background(), request, sink, func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
		if len(invocation.Args) == 1 && invocation.Args[0] == "--version" {
			return []byte(invocation.Tool.Version + "\n"), nil
		}
		if err := os.WriteFile(invocation.OutputPath+".relations.json", mustMarshalRustSidecar(t, []rustSidecarRelation{
			rustTestSidecarRelation("call", ".generated/expanded.rs", "rust . crate . caller().", "rust . crate . target().", 1, callStart, 1, callStart+6),
		}), 0o600); err != nil {
			return nil, err
		}
		return rustSCIPRelationFixtureAtRoot(t, workspaceuri.FromPath(invocation.Dir).Canonical(), ".generated/expanded.rs", contents), nil
	})
	if err != nil {
		t.Fatalf("export generated-source relation: %v", err)
	}
	if len(sink.edges) != 1 {
		t.Fatalf("generated helper emitted %d edges, want one: %+v", len(sink.edges), sink.edges)
	}
	edge := sink.edges[0]
	if edge.Kind != model.EdgeCall || edge.SourceURI != sourceURI || edge.SourceHash != viewFileHash(t, base, sourceURI) ||
		edge.Range.StartLine != 1 || edge.Range.StartChar != callStart || edge.Range.EndChar != callStart+6 {
		t.Fatalf("generated-source edge was not mapped to its captured origin: %+v", edge)
	}
	for _, coverage := range report.Coverage {
		if coverage.State == model.Complete && (coverage.Fact == model.FactCall || coverage.Fact == model.FactGenerated) {
			t.Fatalf("generated call sidecar upgraded unsupported coverage: %+v", coverage)
		}
	}
}

func TestBindRustSCIPProjectRootRequiresVerifiedMaterializedDirectory(t *testing.T) {
	materializedRoot := t.TempDir()
	otherRoot := t.TempDir()
	data := rustSCIPFixtureAtRoot(t, workspaceuri.FromPath(otherRoot).Canonical(), "src/lib.rs")
	if _, err := bindRustSCIPProjectRoot(data, materializedRoot, rustTestCanonicalRootURI, ""); err == nil || !strings.Contains(err.Error(), "does not identify the verified materialized workspace") {
		t.Fatalf("SCIP project root outside the materialized workspace was accepted: %v", err)
	}
	data = rustSCIPFixtureAtRoot(t, workspaceuri.FromPath(materializedRoot).Canonical(), "src/lib.rs")
	normalized, err := bindRustSCIPProjectRoot(data, materializedRoot, rustTestCanonicalRootURI, "")
	if err != nil {
		t.Fatalf("bind matching materialized project root: %v", err)
	}
	var index scip.Index
	if err := proto.Unmarshal(normalized, &index); err != nil {
		t.Fatal(err)
	}
	if got := index.GetMetadata().GetProjectRoot(); got != rustTestCanonicalRootURI {
		t.Fatalf("SCIP project root was not rebound to captured URI: %q", got)
	}
	data = rustSCIPFixtureAtRoot(t, "", "src/lib.rs")
	if _, err := bindRustSCIPProjectRoot(data, materializedRoot, rustTestCanonicalRootURI, ""); err == nil || !strings.Contains(err.Error(), "omitted project root") {
		t.Fatalf("missing SCIP project root was accepted: %v", err)
	}
	canonical, err := canonicalRustWorkspaceRoot(rustTestRootURI)
	if err != nil || canonical != rustTestCanonicalRootURI {
		t.Fatalf("trailing slash workspace root did not normalize canonically: %q, %v", canonical, err)
	}
}

func TestRustAnalyzerBuildContextsDoNotCollapseFeatureAndTargetVariants(t *testing.T) {
	base := rustTestScope("crate", rustTestRootURI, []string{"default"}, "x86_64-unknown-linux-gnu")
	other := rustTestScope("crate", rustTestRootURI, []string{"default", "serde"}, "aarch64-unknown-linux-gnu")
	tools := rustTestTools(t, false)
	base.BuildContext = model.ComputeBuildContextID(base, "rust-analyzer-scip", "1.0", "rustc-test", tools)
	other.BuildContext = model.ComputeBuildContextID(other, "rust-analyzer-scip", "1.0", "rustc-test", tools)
	if base.BuildContext == other.BuildContext {
		t.Fatalf("feature/target variants share a BuildContext: %s", base.BuildContext)
	}
	baseConfig, err := rustAnalyzerConfig(base, rustTestToolMap(tools), "../target-a")
	if err != nil {
		t.Fatal(err)
	}
	otherConfig, err := rustAnalyzerConfig(other, rustTestToolMap(tools), "../target-b")
	if err != nil {
		t.Fatal(err)
	}
	if equalJSON(t, baseConfig, otherConfig) {
		t.Fatal("rust-analyzer configurations collapsed distinct Cargo feature/target inputs")
	}
}

func TestRustAnalyzerLockedSCIPDifferential(t *testing.T) {
	lockData, err := os.ReadFile(filepath.Join("..", "..", "..", "test", "acceptance", "tools", "tools.lock.json"))
	if err != nil {
		t.Skipf("locked acceptance tool manifest unavailable: %v", err)
	}
	var lock struct {
		Observed struct {
			RustAnalyzer               string `json:"rustAnalyzer"`
			RustAnalyzerSemanticHelper string `json:"rustAnalyzerSemanticHelper"`
			Cargo                      string `json:"cargo"`
			Rustc                      string `json:"rustc"`
		} `json:"observed"`
		ResolvedBinaries map[string]struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"resolvedBinaries"`
		AcceptanceTools struct {
			SemanticHelper struct {
				Version            string `json:"version"`
				SourceCommit       string `json:"sourceCommit"`
				SourceSHA256       string `json:"sourceSha256"`
				PatchSHA256        string `json:"patchSha256"`
				ModifiedTreeSHA256 string `json:"modifiedTreeSha256"`
				Patch              string `json:"patch"`
			} `json:"rustAnalyzerSemanticHelper"`
			RustAnalyzerSource struct {
				RepositoryCommit string `json:"repositoryCommit"`
				SHA256           string `json:"sha256"`
			} `json:"rustAnalyzerSource"`
		} `json:"acceptanceTools"`
	}
	if err := json.Unmarshal(lockData, &lock); err != nil {
		t.Fatalf("decode locked Rust tool versions: %v", err)
	}
	tools := make([]model.ToolIdentity, 0, 4)
	for _, pinned := range []struct{ name, command, version string }{
		{rustAnalyzerToolName, rustAnalyzerToolName, lock.Observed.RustAnalyzer},
		{cargoToolName, cargoToolName, lock.Observed.Cargo},
		{rustcToolName, rustcToolName, lock.Observed.Rustc},
	} {
		tool, resolveErr := lockedHostTool(t, pinned.name, pinned.command, pinned.version)
		if resolveErr != nil {
			t.Skipf("locked %s tool unavailable: %v", pinned.name, resolveErr)
		}
		tools = append(tools, tool)
	}
	helperVersion := strings.TrimSpace(lock.Observed.RustAnalyzerSemanticHelper)
	helperBinary := lock.ResolvedBinaries[rustSemanticHelperToolName]
	helperMetadata := lock.AcceptanceTools.SemanticHelper
	sourceMetadata := lock.AcceptanceTools.RustAnalyzerSource
	if helperVersion == "" || helperBinary.Path == "" || helperBinary.SHA256 == "" ||
		helperMetadata.Version == "" || helperMetadata.SourceCommit == "" || helperMetadata.SourceSHA256 == "" ||
		helperMetadata.PatchSHA256 == "" || helperMetadata.ModifiedTreeSHA256 == "" || helperMetadata.Patch == "" ||
		sourceMetadata.RepositoryCommit == "" || sourceMetadata.SHA256 == "" {
		t.Fatal("tools.lock.json does not pin the same-source Rust semantic helper and its source/patch provenance")
	}
	if len(helperMetadata.SourceCommit) < 8 {
		t.Fatalf("locked semantic helper source commit is too short: %q", helperMetadata.SourceCommit)
	}
	if helperMetadata.Version == "" || !strings.Contains(helperVersion, helperMetadata.Version) ||
		helperMetadata.SourceCommit != sourceMetadata.RepositoryCommit || helperMetadata.SourceSHA256 != sourceMetadata.SHA256 ||
		!strings.Contains(helperVersion, helperMetadata.SourceCommit[:8]) ||
		!strings.Contains(helperVersion, helperMetadata.PatchSHA256) {
		t.Fatalf("locked semantic helper version/source/patch identities do not agree: observed=%q metadata=%+v source=%+v", helperVersion, helperMetadata, sourceMetadata)
	}
	lockDir, err := filepath.Abs(filepath.Join("..", "..", "..", "test", "acceptance", "tools"))
	if err != nil {
		t.Fatal(err)
	}
	helperPath := helperBinary.Path
	if !filepath.IsAbs(helperPath) {
		helperPath = filepath.Join(lockDir, filepath.FromSlash(helperPath))
	}
	_, helperDigest, err := nested.ExecutableIdentity(helperPath)
	if err != nil {
		t.Skipf("locked semantic helper unavailable: %v", err)
	}
	if !strings.EqualFold(helperDigest, helperBinary.SHA256) {
		t.Fatalf("locked semantic helper binary digest mismatch: got %s want %s", helperDigest, helperBinary.SHA256)
	}
	helperVersionOutput, err := exec.Command(helperPath, "--version").Output()
	if err != nil {
		t.Skipf("locked semantic helper cannot run: %v", err)
	}
	helperReportedVersion := strings.TrimSpace(string(helperVersionOutput))
	if !strings.Contains(helperReportedVersion, helperVersion) || !strings.Contains(helperReportedVersion, helperMetadata.SourceCommit[:8]) {
		t.Fatalf("locked helper executable version %q does not match lock %q and source commit %s", helperReportedVersion, helperVersion, helperMetadata.SourceCommit)
	}
	patchPath := filepath.Clean(filepath.Join(lockDir, filepath.FromSlash(helperMetadata.Patch)))
	patchDigest, _, err := hashFile(context.Background(), patchPath)
	if err != nil {
		t.Fatalf("read locked semantic helper patch %q: %v", patchPath, err)
	}
	if !strings.EqualFold(patchDigest, helperMetadata.PatchSHA256) {
		t.Fatalf("locked semantic helper patch digest mismatch: got %s want %s", patchDigest, helperMetadata.PatchSHA256)
	}
	helper := model.ToolIdentity{Name: rustSemanticHelperToolName, Path: helperPath, Version: helperReportedVersion, SHA256: helperDigest}
	tools = append(tools, helper)
	files := map[string][]byte{
		rustTestRootURI + "Cargo.toml":    []byte("[package]\nname = \"rust-semantic-differential\"\nversion = \"0.1.0\"\nedition = \"2021\"\n\n[features]\nextra = []\n"),
		rustTestRootURI + "Cargo.lock":    []byte("version = 4\n\n[[package]]\nname = \"rust-semantic-differential\"\nversion = \"0.1.0\"\n"),
		rustTestRootURI + "src/lib.rs":    []byte("pub trait Measure { fn measure(&self) -> usize; }\npub struct Thing;\nimpl Measure for Thing { fn measure(&self) -> usize { 1 } }\npub fn café() -> usize { 2 }\npub fn takes(item: Thing) {}\npub fn target() -> usize { café() }\npub fn caller() -> usize { target() }\npub mod nested;\npub use nested::Nested;\nmacro_rules! make_function { ($name:ident) => { pub fn $name() -> usize { 3 } }; }\nmake_function!(generated);\n#[cfg(feature = \"extra\")] pub fn feature_only() {}\n"),
		rustTestRootURI + "src/nested.rs": []byte("pub struct Nested;\npub fn nested_function() -> usize { 4 }\n"),
	}
	view := newRustTestView(t, files)
	scope := rustTestScope("rust-semantic-differential", rustTestRootURI, []string{"extra"}, "")
	scope.Build.Tests = false
	scope.Build.PackagePatterns = []string{"rust-semantic-differential@0.1.0"}
	scope.Build.Arguments = []string{"--lib"}
	request := rustTestRequest(view, scope, tools)
	scope = request.Scopes[0]
	sink := &rustTestSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var rawImplementations, rawTypeRelations int
	rawRelationshipSet := make(map[string]bool)
	var rawHelperRelations []rustSidecarRelation
	var rawSymbolNames map[string]string
	runDifferential := func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
		data, err := runRustAnalyzerSCIP(ctx, invocation)
		if err == nil && len(invocation.Args) > 0 && invocation.Args[0] == "scip" {
			sidecarData, sidecarErr := os.ReadFile(invocation.OutputPath + ".relations.json")
			if sidecarErr != nil {
				return nil, fmt.Errorf("read relation sidecar from the same locked helper response: %w", sidecarErr)
			}
			sidecar, decodeErr := decodeRustRelationSidecar(sidecarData)
			if decodeErr != nil {
				return nil, fmt.Errorf("decode relation sidecar from the same locked helper response: %w", decodeErr)
			}
			rawHelperRelations = append([]rustSidecarRelation(nil), sidecar.Relations...)
			var index scip.Index
			if decodeErr := proto.Unmarshal(data, &index); decodeErr == nil {
				rawSymbolNames = make(map[string]string)
				for _, document := range index.Documents {
					for _, symbol := range document.Symbols {
						rawSymbolNames[symbol.GetSymbol()] = symbol.GetDisplayName()
					}
				}
				for _, symbol := range index.ExternalSymbols {
					rawSymbolNames[symbol.GetSymbol()] = symbol.GetDisplayName()
				}
				paths := make([]string, 0, len(index.Documents))
				for _, document := range index.Documents {
					paths = append(paths, fmt.Sprintf("%s: %d occurrences/%d symbols", document.RelativePath, len(document.Occurrences), len(document.Symbols)))
					for _, symbol := range document.Symbols {
						for _, relationship := range symbol.Relationships {
							if relationship.GetSymbol() == "" || symbol.GetSymbol() == "" {
								continue
							}
							if relationship.GetIsImplementation() {
								rawImplementations++
								rawRelationshipSet[symbol.GetDisplayName()+"|implementation|"+rawSymbolNames[relationship.GetSymbol()]] = true
							}
							if relationship.GetIsTypeDefinition() {
								rawTypeRelations++
								rawRelationshipSet[symbol.GetDisplayName()+"|type|"+rawSymbolNames[relationship.GetSymbol()]] = true
							}
						}
					}
				}
				t.Logf("locked rust-analyzer SCIP produced %d documents (%v), %d external symbols, and relationships: %d implementations/%d type relations", len(index.Documents), paths, len(index.ExternalSymbols), rawImplementations, rawTypeRelations)
			}
		}
		return data, err
	}
	report, err := exportRustIndex(ctx, request, sink, runDifferential)
	if err != nil {
		t.Fatalf("locked rust-analyzer SCIP differential export: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("locked rust-analyzer report failed validation: %v", err)
	}
	if rawSymbolNames == nil {
		t.Fatal("locked helper SCIP response did not expose its raw symbol names")
	}
	if len(rawHelperRelations) == 0 {
		t.Fatal("locked helper emitted no call/module relations in its same-response sidecar")
	}
	relationKey := func(kind model.EdgeKind, source, target identity.SymbolID, sourceURI string, position model.Position) string {
		return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d:%d:%d:%d", kind, source, target, sourceURI,
			position.StartLine, position.StartChar, position.EndLine, position.EndChar)
	}
	relationNameKey := func(kind, source, target, sourceDocument string, position model.Position) string {
		return fmt.Sprintf("%s|%s|%s|%s|%d:%d-%d:%d", kind, source, target, sourceDocument,
			position.StartLine, position.StartChar, position.EndLine, position.EndChar)
	}
	rawRelationSet := make(map[string]bool, len(rawHelperRelations))
	rawRelationNameSet := make(map[string]bool, len(rawHelperRelations))
	rawCallCount, rawModuleCount := 0, 0
	for _, relation := range rawHelperRelations {
		rawRange, rangeErr := rustRangeCoordinates(relation.Range)
		if rangeErr != nil {
			t.Fatalf("locked helper emitted an invalid same-response relation range: %v", rangeErr)
		}
		position := model.Position{
			StartLine: rawRange.startLine, StartChar: rawRange.startChar,
			EndLine: rawRange.endLine, EndChar: rawRange.endChar,
		}
		sourceURI, uriErr := rustSCIPDocumentURI(scope.RootURI, relation.SourceDocument)
		if uriErr != nil {
			t.Fatalf("locked helper emitted an invalid same-response source document %q: %v", relation.SourceDocument, uriErr)
		}
		kind := model.EdgeKind(relation.Kind)
		key := relationKey(kind, identity.SymbolID(relation.SourceSymbol), identity.SymbolID(relation.TargetSymbol), sourceURI, position)
		if rawRelationSet[key] {
			t.Fatalf("locked helper repeated same-response relation %q", key)
		}
		rawRelationSet[key] = true
		sourceName, sourceOK := rawSymbolNames[relation.SourceSymbol]
		targetName, targetOK := rawSymbolNames[relation.TargetSymbol]
		if !sourceOK || !targetOK {
			t.Fatalf("locked helper sidecar symbols are absent from its paired SCIP response: source=%q target=%q", relation.SourceSymbol, relation.TargetSymbol)
		}
		rawRelationNameSet[relationNameKey(relation.Kind, sourceName, targetName, relation.SourceDocument, position)] = true
		switch kind {
		case model.EdgeCall:
			rawCallCount++
		case model.EdgeModule:
			rawModuleCount++
		default:
			t.Fatalf("locked helper emitted unexpected relation kind %q", relation.Kind)
		}
	}
	if rawCallCount != 2 || rawModuleCount != 1 {
		t.Fatalf("locked helper relation fixture differs from its expected call/module facts: calls=%d modules=%d raw=%v symbols=%v", rawCallCount, rawModuleCount, rawRelationNameSet, rawSymbolNames)
	}
	for _, expected := range []string{
		"call|caller|target|src/lib.rs|6:27-6:33",
		"call|target|café|src/lib.rs|5:27-5:31",
	} {
		if !rawRelationNameSet[expected] {
			t.Fatalf("locked helper same-response sidecar omitted expected call relation %q (observed %v)", expected, rawRelationNameSet)
		}
	}
	moduleRelationFound := false
	for relation := range rawRelationNameSet {
		if strings.HasPrefix(relation, "module|") && strings.HasSuffix(relation, "|nested|src/lib.rs|7:8-7:14") {
			moduleRelationFound = true
			break
		}
	}
	if !moduleRelationFound {
		t.Fatalf("locked helper same-response sidecar omitted the nested module declaration at its captured UTF-16 range: %v", rawRelationNameSet)
	}
	importedRelationSet := make(map[string]bool, len(rawHelperRelations))
	importedRelationNameSet := make(map[string]bool, len(rawHelperRelations))
	importedCallCount, importedModuleCount := 0, 0
	for _, edge := range sink.edges {
		if edge.Kind != model.EdgeCall && edge.Kind != model.EdgeModule {
			continue
		}
		key := relationKey(edge.Kind, edge.From, edge.To, edge.SourceURI, edge.Range)
		if importedRelationSet[key] {
			t.Fatalf("locked helper relation was imported more than once: %q", key)
		}
		importedRelationSet[key] = true
		sourceBytes, sourceOK := files[edge.SourceURI]
		if !sourceOK {
			t.Fatalf("locked helper relation points outside the immutable fixture source map: %q", edge.SourceURI)
		}
		sourceDigest := sha256.Sum256(sourceBytes)
		if !strings.EqualFold(string(edge.SourceHash), "sha256:"+hex.EncodeToString(sourceDigest[:])) {
			t.Fatalf("locked helper relation source hash does not match the immutable fixture: edge=%+v", edge)
		}
		if !strings.HasPrefix(edge.SourceURI, scope.RootURI) {
			t.Fatalf("locked helper relation source URI escaped the captured fixture root: %q", edge.SourceURI)
		}
		sourceName, sourceOK := rawSymbolNames[string(edge.From)]
		targetName, targetOK := rawSymbolNames[string(edge.To)]
		if !sourceOK || !targetOK {
			t.Fatalf("imported call/module edge lost its raw SCIP symbol identity: from=%q to=%q", edge.From, edge.To)
		}
		documentPath := strings.TrimPrefix(edge.SourceURI, scope.RootURI)
		documentPath = strings.TrimPrefix(documentPath, "/")
		importedRelationNameSet[relationNameKey(string(edge.Kind), sourceName, targetName, documentPath, edge.Range)] = true
		switch edge.Kind {
		case model.EdgeCall:
			importedCallCount++
		case model.EdgeModule:
			importedModuleCount++
		}
	}
	if !reflect.DeepEqual(importedRelationSet, rawRelationSet) || !reflect.DeepEqual(importedRelationNameSet, rawRelationNameSet) ||
		importedCallCount != rawCallCount || importedModuleCount != rawModuleCount {
		t.Fatalf("imported call/module edges differ from the same-response raw helper sidecar: raw=%v imported=%v rawNames=%v importedNames=%v", rawRelationSet, importedRelationSet, rawRelationNameSet, importedRelationNameSet)
	}
	var definitions, references, implementations, typeRelations int
	var unicodeDefinition, generatedCallsite bool
	for _, occurrence := range sink.occurrences {
		switch occurrence.Role {
		case "definition":
			definitions++
			for _, symbol := range sink.symbols {
				if symbol.ID == occurrence.SymbolID && symbol.Name == "café" && occurrence.Range.StartLine == 3 && occurrence.Range.StartChar == 7 && occurrence.Range.EndChar == 11 {
					unicodeDefinition = true
				}
				if symbol.ID == occurrence.SymbolID && symbol.Name == "generated" && occurrence.URI == rustTestRootURI+"src/lib.rs" {
					generatedCallsite = true
				}
			}
		case "reference":
			references++
		}
	}
	for _, edge := range sink.edges {
		switch edge.Kind {
		case model.EdgeImplementation:
			implementations++
		case model.EdgeTypeRelation:
			typeRelations++
		}
	}
	if definitions == 0 || references == 0 {
		t.Fatalf("locked SCIP facts differ from Rust fixture expectations: definitions=%d refs=%d implementations=%d types=%d symbols=%d occurrences=%d coverage=%+v", definitions, references, implementations, typeRelations, len(sink.symbols), len(sink.occurrences), report.Coverage)
	}
	if rawImplementations == 0 || rawTypeRelations == 0 {
		t.Fatalf("locked rust-analyzer SCIP omitted required fixture relationships: implementations=%d type relations=%d", rawImplementations, rawTypeRelations)
	}
	expectedRelations := []string{"Thing|implementation|Measure", "item|type|Thing"}
	for _, expected := range expectedRelations {
		if !rawRelationshipSet[expected] {
			t.Fatalf("locked rust-analyzer SCIP omitted expected semantic relation %q (observed %v)", expected, rawRelationshipSet)
		}
	}
	if len(rawRelationshipSet) != len(expectedRelations) {
		t.Fatalf("locked rust-analyzer SCIP relation set differs from the fixture expectation: got %v want %v", rawRelationshipSet, expectedRelations)
	}
	if implementations != rawImplementations || typeRelations != rawTypeRelations {
		t.Fatalf("locked SCIP edges were not imported exactly from proved relationships: implementations=%d raw=%d types=%d raw=%d", implementations, rawImplementations, typeRelations, rawTypeRelations)
	}
	symbolNamesByID := make(map[identity.SymbolID]string, len(sink.symbols))
	for _, symbol := range sink.symbols {
		symbolNamesByID[symbol.ID] = symbol.Name
	}
	modelRelationSet := make(map[string]bool, len(sink.edges))
	for _, edge := range sink.edges {
		kind := ""
		switch edge.Kind {
		case model.EdgeImplementation:
			kind = "implementation"
		case model.EdgeTypeRelation:
			kind = "type"
		default:
			continue
		}
		modelRelationSet[symbolNamesByID[edge.From]+"|"+kind+"|"+symbolNamesByID[edge.To]] = true
	}
	for _, expected := range expectedRelations {
		if !modelRelationSet[expected] {
			t.Fatalf("SCIP importer reversed or lost semantic edge %q (observed %v)", expected, modelRelationSet)
		}
	}
	if len(modelRelationSet) != len(expectedRelations) {
		t.Fatalf("imported model relation set differs from the fixture expectation: got %v want %v", modelRelationSet, expectedRelations)
	}
	if !unicodeDefinition {
		t.Fatalf("locked SCIP Unicode definition range was not normalized to UTF-16: %+v", sink.occurrences)
	}
	if !generatedCallsite {
		t.Fatalf("locked SCIP macro expansion did not remain attached to the captured invocation source")
	}
	for _, fact := range []model.FactKind{model.FactCall, model.FactImport, model.FactInclude, model.FactModule, model.FactGenerated} {
		if coverageFor(report, scope.ID, fact).State != model.Unknown {
			t.Fatalf("SCIP cannot prove complete %s coverage", fact)
		}
	}
	for fact, want := range map[model.FactKind]model.Completeness{
		model.FactSymbol:         model.IncompleteKnownSubset,
		model.FactDefinition:     model.IncompleteKnownSubset,
		model.FactReference:      model.IncompleteKnownSubset,
		model.FactDeclaration:    model.Unknown,
		model.FactImplementation: model.IncompleteKnownSubset,
		model.FactTypeRelation:   model.IncompleteKnownSubset,
	} {
		if got := coverageFor(report, scope.ID, fact).State; got != want {
			t.Fatalf("locked SCIP %s coverage = %s, want conservative %s", fact, got, want)
		}
	}

	// The fixture's original test identity is intentionally lightweight. Give
	// the same immutable file set a content-derived digest before exercising the
	// production semantic store, then preserve the extractor's report verbatim.
	storeRoot := t.TempDir()
	persistedIdentity := request.View.Identity()
	persistedIdentity.DiskDigest = rustTestSnapshotDigest(files)
	persistedView := rustTestIdentityView{rustTestView: view, identity: persistedIdentity}
	persistedRequest := request
	persistedRequest.View = persistedView
	persistedProvenance := request.Provenance[scope.ID]
	persistedProvenance.Identity = persistedIdentity
	persistedRequest.Provenance = map[string]model.Provenance{scope.ID: persistedProvenance}
	persistedReport := report
	persistedReport.Identity = persistedIdentity

	store, err := persistent.NewFileStore(storeRoot, persistent.Config{})
	if err != nil {
		t.Fatalf("open Rust semantic store: %v", err)
	}
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatalf("begin Rust semantic generation: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	generationID := build.GenerationID()
	persistentSink, err := semantic.NewSink(ctx, build, persistedRequest)
	if err != nil {
		t.Fatalf("create Rust semantic sink: %v", err)
	}
	defer persistentSink.Close()
	if err := persistentSink.WriteSymbols(ctx, sink.symbols); err != nil {
		t.Fatalf("persist locked Rust symbols: %v", err)
	}
	if err := persistentSink.WriteOccurrences(ctx, sink.occurrences); err != nil {
		t.Fatalf("persist locked Rust occurrences: %v", err)
	}
	if err := persistentSink.WriteEdges(ctx, sink.edges); err != nil {
		t.Fatalf("persist locked Rust edges: %v", err)
	}
	if err := persistentSink.Finalize(ctx, persistedReport); err != nil {
		t.Fatalf("finalize locked Rust semantic generation: %v", err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatalf("commit locked Rust semantic generation: %v", err)
	}
	committed = true

	// Reconstruct the store and reader as a new process would. Keep this focused
	// on durable facts and the unchanged conservative coverage contract; it does
	// not imply that persistent location queries may treat SCIP as exhaustive.
	reopened, err := persistent.NewFileStore(storeRoot, persistent.Config{})
	if err != nil {
		t.Fatalf("reopen Rust semantic store: %v", err)
	}
	generation, err := reopened.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("open committed Rust generation: %v", err)
	}
	if generation.ID != generationID {
		t.Fatalf("reopened Rust generation ID = %d, want committed %d", generation.ID, generationID)
	}
	reader, err := semantic.OpenReader(ctx, generation)
	if err != nil {
		t.Fatalf("open Rust semantic reader: %v", err)
	}
	defer reader.Close()
	metadata := reader.Metadata()
	if metadata.Identity.Workspace != persistedIdentity.Workspace || metadata.Identity.DiskDigest != persistedIdentity.DiskDigest || metadata.Identity.SnapshotRev != 0 {
		t.Fatalf("reopened Rust generation identity = %+v, want durable identity %+v", metadata.Identity, persistedIdentity)
	}
	if !reflect.DeepEqual(metadata.Coverage, report.Coverage) {
		t.Fatalf("reopened Rust coverage changed: got %+v want %+v", metadata.Coverage, report.Coverage)
	}
	if !reflect.DeepEqual(metadata.UsedTools[scope.ID], report.UsedTools[scope.ID]) || metadata.Provenance[scope.ID].Scope.BuildContext != scope.BuildContext {
		t.Fatalf("reopened Rust provenance changed: tools=%v provenance=%+v", metadata.UsedTools[scope.ID], metadata.Provenance[scope.ID])
	}

	var persistedSymbols []model.Symbol
	var persistedOccurrences []model.Occurrence
	var persistedEdges []model.Edge
	for {
		batch, nextErr := reader.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("read committed Rust semantic batch: %v", nextErr)
		}
		persistedSymbols = append(persistedSymbols, batch.Symbols...)
		persistedOccurrences = append(persistedOccurrences, batch.Occurrences...)
		persistedEdges = append(persistedEdges, batch.Edges...)
	}
	if !equalRustFactMultiset(sink.symbols, persistedSymbols) ||
		!equalRustFactMultiset(sink.occurrences, persistedOccurrences) ||
		!equalRustFactMultiset(sink.edges, persistedEdges) {
		t.Fatalf("reopened Rust facts differ from locked extractor output: symbols=%d/%d occurrences=%d/%d edges=%d/%d",
			len(persistedSymbols), len(sink.symbols), len(persistedOccurrences), len(sink.occurrences), len(persistedEdges), len(sink.edges))
	}
	for _, kind := range []model.EdgeKind{model.EdgeCall, model.EdgeModule, model.EdgeImplementation, model.EdgeTypeRelation} {
		count := 0
		for _, edge := range persistedEdges {
			if edge.Kind == kind {
				count++
			}
		}
		if count == 0 {
			t.Fatalf("reopened locked Rust generation has no %s edges", kind)
		}
	}
	secondOpen, err := persistent.NewFileStore(storeRoot, persistent.Config{})
	if err != nil {
		t.Fatalf("open Rust store a second time: %v", err)
	}
	secondGeneration, err := secondOpen.OpenSnapshot(ctx)
	if err != nil || secondGeneration.ID != generationID {
		t.Fatalf("second reopen generation = %d, err %v; want fixed generation %d", secondGeneration.ID, err, generationID)
	}
}

func lockedHostTool(t *testing.T, name, command, lockedVersion string) (model.ToolIdentity, error) {
	t.Helper()
	if strings.TrimSpace(lockedVersion) == "" {
		return model.ToolIdentity{}, errors.New("version is absent from the tool lock")
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return model.ToolIdentity{}, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return model.ToolIdentity{}, err
	}
	_, digest, err := nested.ExecutableIdentity(path)
	if err != nil {
		return model.ToolIdentity{}, err
	}
	output, err := exec.Command(path, "--version").Output()
	if err != nil {
		return model.ToolIdentity{}, err
	}
	version := strings.TrimSpace(string(output))
	if !strings.Contains(version, lockedVersion) {
		return model.ToolIdentity{}, fmt.Errorf("installed %s version %q does not match locked version %q", name, version, lockedVersion)
	}
	return model.ToolIdentity{Name: name, Path: path, Version: version, SHA256: digest}, nil
}

func TestRustAnalyzerMissingToolReportsUnavailableCoverage(t *testing.T) {
	view := newRustTestView(t, map[string][]byte{rustTestRootURI + "src/lib.rs": []byte("pub fn f() {}\n")})
	scope := rustTestScope("crate", rustTestRootURI, nil, "")
	request := rustTestRequest(view, scope, nil)
	calls := 0
	report, err := exportRustIndex(context.Background(), request, &rustTestSink{}, func(context.Context, scipInvocation) ([]byte, error) {
		calls++
		return nil, errors.New("must not run without verified tools")
	})
	if err != nil {
		t.Fatalf("missing tools should produce an unavailable report, got %v", err)
	}
	if calls != 0 || len(report.UsedTools) != 0 {
		t.Fatalf("missing tool path was invoked or reported as used: calls=%d tools=%v", calls, report.UsedTools)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("unavailable report is not complete: %v", err)
	}
	for _, item := range report.Coverage {
		if item.State != model.Unavailable || !strings.Contains(item.Reason, rustAnalyzerToolName) {
			t.Fatalf("missing rust-analyzer did not fail closed: %+v", item)
		}
	}
}

func TestRustAnalyzerCancellationReportsRequiredCoverage(t *testing.T) {
	view := newRustTestView(t, map[string][]byte{rustTestRootURI + "src/lib.rs": []byte("pub fn f() {}\n")})
	scope := rustTestScope("crate", rustTestRootURI, nil, "")
	request := rustTestRequest(view, scope, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := exportRustIndex(ctx, request, &rustTestSink{}, func(context.Context, scipInvocation) ([]byte, error) {
		t.Fatal("cancelled export invoked a semantic tool")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("cancelled report omitted required coverage: %v", err)
	}
	for _, item := range report.Coverage {
		if item.State != model.Unavailable || !strings.Contains(item.Reason, "cancel") {
			t.Fatalf("cancellation was not recorded as unavailable: %+v", item)
		}
	}
}

func TestRustAnalyzerCancellationDuringSCIPDoesNotClaimCoverage(t *testing.T) {
	view := newRustTestView(t, map[string][]byte{rustTestRootURI + "src/lib.rs": []byte("pub fn f() {}\n")})
	scope := rustTestScope("crate", rustTestRootURI, nil, "")
	tools := rustTestTools(t, false)
	request := rustTestRequest(view, scope, tools)
	ctx, cancel := context.WithCancel(context.Background())
	report, err := exportRustIndex(ctx, request, &rustTestSink{}, func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
		if len(invocation.Args) == 1 && invocation.Args[0] == "--version" {
			return []byte(invocation.Tool.Version), nil
		}
		cancel()
		return rustSCIPFixture(t), nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during SCIP export error = %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("cancelled report omitted required coverage: %v", err)
	}
	for _, item := range report.Coverage {
		if item.State != model.Unavailable || !strings.Contains(item.Reason, "cancel") {
			t.Fatalf("cancellation was not recorded as unavailable: %+v", item)
		}
	}
}

func TestRustAnalyzerRejectsMaterializedSourceMutation(t *testing.T) {
	view := newRustTestView(t, map[string][]byte{rustTestRootURI + "src/lib.rs": []byte("pub fn f() {}\n")})
	scope := rustTestScope("crate", rustTestRootURI, nil, "")
	tools := rustTestTools(t, false)
	request := rustTestRequest(view, scope, tools)
	runner := func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
		if len(invocation.Args) == 1 && invocation.Args[0] == "--version" {
			return []byte(invocation.Tool.Version), nil
		}
		path := filepath.Join(invocation.Dir, "src", "lib.rs")
		if err := os.WriteFile(path, []byte("pub fn altered() {}\n"), 0o600); err != nil {
			return nil, err
		}
		return rustSCIPFixture(t), nil
	}
	report, err := exportRustIndex(context.Background(), request, &rustTestSink{}, runner)
	if err != nil {
		t.Fatalf("changed isolated input should be reported unavailable, got %v", err)
	}
	for _, item := range report.Coverage {
		if item.State != model.Unavailable || !strings.Contains(item.Reason, "differs from immutable workspace view") {
			t.Fatalf("mutation was not detected: %+v", item)
		}
	}
}

func TestRustAnalyzerDoesNotInventMappingForGeneratedSCIPDocument(t *testing.T) {
	view := newRustTestView(t, map[string][]byte{
		rustTestRootURI + "src/lib.rs": []byte("// 🫠\npub fn café() {}\npub fn use_it() { café(); }\n"),
	})
	scope := rustTestScope("crate", rustTestRootURI, nil, "")
	tools := rustTestTools(t, false)
	request := rustTestRequest(view, scope, tools)
	runner := func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
		if len(invocation.Args) == 1 && invocation.Args[0] == "--version" {
			return []byte(invocation.Tool.Version), nil
		}
		return rustSCIPFixtureAtRoot(t, workspaceuri.FromPath(invocation.Dir).Canonical(), "target/generated_macro.rs"), nil
	}
	sink := &rustTestSink{}
	report, err := exportRustIndex(context.Background(), request, sink, runner)
	if err != nil {
		t.Fatalf("out-of-view generated SCIP document should fail closed: %v", err)
	}
	for _, item := range report.Coverage {
		if item.State != model.Unavailable || !strings.Contains(item.Reason, "outside the captured snapshot") {
			t.Fatalf("generated document without a source mapping was not rejected: %+v", item)
		}
	}
	for _, occurrence := range sink.occurrences {
		if strings.Contains(occurrence.URI, "generated_macro") || occurrence.URI == rustTestRootURI+"src/lib.rs" {
			t.Fatalf("provider fabricated a mapping from generated document to source: %+v", occurrence)
		}
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("unavailable report is not complete: %v", err)
	}
}

func TestRustRequestBuilderUsesExplicitToolsAndCapturedCargoMetadata(t *testing.T) {
	files := map[string][]byte{
		rustTestRootURI + "Cargo.toml":         []byte("[workspace]\nmembers = [\"crate-a\"]\nresolver = \"2\"\n"),
		rustTestRootURI + "crate-a/Cargo.toml": []byte("[package]\nname = \"crate-a\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"),
		rustTestRootURI + "crate-a/src/lib.rs": []byte("// 🫠\npub fn café() {}\npub fn use_it() { café(); }\n"),
	}
	view := newRustTestView(t, files)
	tools := rustTestTools(t, false)
	byName := rustTestToolMap(tools)
	helperBytes := []byte("test same-source semantic helper executable")
	helperPath := filepath.Join(t.TempDir(), "semantic-helper.exe")
	if err := os.WriteFile(helperPath, helperBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	helperDigest := sha256.Sum256(helperBytes)
	helper := model.ToolIdentity{
		Name: rustSemanticHelperToolName, Path: helperPath, Version: "rust-analyzer 1.97.0+omnilsp-semantic.test (source test)",
		SHA256: hex.EncodeToString(helperDigest[:]),
	}
	metadataRunner := func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
		if invocation.Tool != byName[cargoToolName] {
			return nil, errors.New("Cargo metadata did not use the explicitly pinned cargo executable")
		}
		if len(invocation.Args) == 0 || invocation.Args[0] != "metadata" || !containsArg(invocation.Args, "--locked") || !containsArg(invocation.Args, "--no-deps") {
			return nil, fmt.Errorf("unsafe/incomplete Cargo metadata command: %v", invocation.Args)
		}
		manifest := filepath.Join(invocation.Dir, "crate-a", "Cargo.toml")
		source := filepath.Join(invocation.Dir, "crate-a", "src", "lib.rs")
		if _, err := os.Stat(manifest); err != nil {
			return nil, fmt.Errorf("Cargo metadata did not observe the captured manifest: %w", err)
		}
		if _, err := os.Stat(source); err != nil {
			return nil, fmt.Errorf("Cargo metadata root is not materialized: %w", err)
		}
		return []byte(fmt.Sprintf(`{"packages":[{"id":"path+file:///captured-rust/crate-a#crate-a@0.1.0","name":"crate-a","version":"0.1.0","manifest_path":%q,"targets":[{"name":"crate_a","src_path":%q,"kind":["lib"],"crate_types":["lib"]}]}],"workspace_members":["path+file:///captured-rust/crate-a#crate-a@0.1.0"]}`, manifest, source)), nil
	}
	builder := &SemanticIndexRequestBuilder{
		backend: &Backend{}, config: RustIndexToolConfig{
			Tools: tools, SemanticHelper: helper, Toolchain: "rustc-test", Build: model.BuildInputs{
				Features: []string{"serde"}, Options: map[string]string{"procMacros": "false", "buildScripts": "false", "target": "x86_64-unknown-linux-gnu"},
			},
		},
		run: metadataRunner, activeRAPath: func() string { return byName[rustAnalyzerToolName].Path },
	}
	request, err := builder.BuildIndexRequest(context.Background(), view, rustTestRootURI)
	if err != nil {
		t.Fatalf("build immutable Rust request: %v", err)
	}
	if len(request.Scopes) != 1 {
		t.Fatalf("scope count = %d, want one crate target", len(request.Scopes))
	}
	scope := request.Scopes[0]
	if scope.Language != langID || scope.RootURI != rustTestCanonicalRootURI || scope.Build.PackagePatterns[0] != "crate-a@0.1.0" || !containsArg(scope.Build.Arguments, "--lib") {
		t.Fatalf("request builder lost Cargo crate/target identity: %+v", scope)
	}
	provenance := request.Provenance[scope.ID]
	if scope.BuildContext != model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools) {
		t.Fatal("request builder did not key scope by auditable Cargo/toolchain inputs")
	}
	legacyScope := cloneRustScope(scope)
	legacyScope.BuildContext = model.ComputeBuildContextID(legacyScope, provenance.Extractor, byName[rustAnalyzerToolName].Version, provenance.Toolchain, provenance.Tools)
	if legacyScope.BuildContext == scope.BuildContext {
		t.Fatal("Rust semantic model version did not change the build context")
	}
	legacyProvenance := provenance
	legacyProvenance.ExtractorVer = byName[rustAnalyzerToolName].Version
	legacyProvenance.Scope = legacyScope
	legacyRequest := request
	legacyRequest.Scopes = []model.Scope{legacyScope}
	legacyRequest.Provenance = map[string]model.Provenance{legacyScope.ID: legacyProvenance}
	if _, err := exportRustIndex(context.Background(), legacyRequest, &rustTestSink{}, func(context.Context, scipInvocation) ([]byte, error) {
		return nil, errors.New("legacy Rust semantic provenance must not launch an extractor")
	}); !errors.Is(err, model.ErrInvalidProvenance) {
		t.Fatalf("legacy extractor version export error = %v, want invalid provenance", err)
	}
	if !rustSameToolSet(provenance.Tools, append(append([]model.ToolIdentity(nil), tools...), helper)) {
		t.Fatalf("request provenance did not bind the live RA and separate semantic helper: %+v", provenance.Tools)
	}
	if err := model.ValidateReport(request, model.Report{
		Identity: view.Identity(), Coverage: unavailableCoverage(scope.ID, "test validation"),
	}); err != nil {
		t.Fatalf("request provenance is invalid: %v", err)
	}
	sink := &rustTestSink{}
	var helperExports int
	report, err := exportRustIndex(context.Background(), request, sink, func(ctx context.Context, invocation scipInvocation) ([]byte, error) {
		if len(invocation.Args) == 1 && invocation.Args[0] == "--version" {
			return []byte(invocation.Tool.Version + "\n"), nil
		}
		if len(invocation.Args) == 0 || invocation.Args[0] != "scip" {
			return nil, fmt.Errorf("unexpected helper invocation: %v", invocation.Args)
		}
		if invocation.Tool != helper {
			return nil, fmt.Errorf("SCIP export selected %q, want semantic helper", invocation.Tool.Name)
		}
		if err := os.WriteFile(invocation.OutputPath+".relations.json", mustMarshalRustSidecar(t, []rustSidecarRelation{}), 0o600); err != nil {
			return nil, err
		}
		helperExports++
		return rustSCIPFixtureAtRoot(t, workspaceuri.FromPath(invocation.Dir).Canonical(), "crate-a/src/lib.rs"), nil
	})
	if err != nil {
		t.Fatalf("export through separately pinned semantic helper: %v", err)
	}
	if helperExports != 1 || !rustSameToolSet(report.UsedTools[scope.ID], append(append([]model.ToolIdentity(nil), tools...), helper)) {
		t.Fatalf("helper export/provenance mismatch: calls=%d used=%+v", helperExports, report.UsedTools[scope.ID])
	}
}

func TestRustRebuildVerifiedPlannerRequestUsesPinnedCargoOnly(t *testing.T) {
	cargoPath, err := exec.LookPath("cargo")
	if err != nil {
		t.Skip("installed Cargo is required for the real metadata planner test")
	}
	rustcPath, err := exec.LookPath("rustc")
	if err != nil {
		t.Skip("installed rustc is required for the real metadata planner test")
	}
	files := map[string][]byte{
		rustTestRootURI + "Cargo.toml": []byte("[package]\nname = \"planner_fixture\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"),
		rustTestRootURI + "Cargo.lock": []byte("version = 4\n\n[[package]]\nname = \"planner_fixture\"\nversion = \"0.1.0\"\n"),
		rustTestRootURI + "src/lib.rs": []byte("pub fn fixture() {}\n"),
	}
	view := newRustTestView(t, files)
	tools := rustTestTools(t, false)
	ra := tools[0]
	cargo := rustPlannerTestTool(t, cargoToolName, cargoPath)
	rustc := rustPlannerTestTool(t, rustcToolName, rustcPath)
	tools = []model.ToolIdentity{ra, cargo, rustc}
	helperPath := filepath.Join(t.TempDir(), "disabled-helper.txt")
	helperData := []byte("must remain non-executable during planner restoration")
	if err := os.WriteFile(helperPath, helperData, 0o600); err != nil {
		t.Fatal(err)
	}
	helperHash := sha256.Sum256(helperData)
	helper := model.ToolIdentity{
		Name: rustSemanticHelperToolName, Path: helperPath, Version: "rust-analyzer 1.97.0+omnilsp-semantic.fixture",
		SHA256: hex.EncodeToString(helperHash[:]),
	}
	builder := &SemanticIndexRequestBuilder{
		backend: &Backend{},
		config: RustIndexToolConfig{
			Tools: tools, SemanticHelper: helper, Toolchain: rustc.Version,
			Build: model.BuildInputs{Options: map[string]string{"procMacros": "false", "buildScripts": "false"}},
		},
		activeRAPath: func() string { return ra.Path },
		run:          runCargoMetadata,
	}
	request, err := builder.BuildIndexRequest(context.Background(), view, rustTestRootURI)
	if err != nil {
		t.Fatalf("build request with installed Cargo metadata: %v", err)
	}
	if len(request.Scopes) != 1 {
		t.Fatalf("Cargo discovered %d scopes, want one", len(request.Scopes))
	}
	if _, err := os.Stat(filepath.Join(view.rootPath, "target")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Cargo metadata polluted the captured source with target output: stat err=%v", err)
	}
	currentIdentity := view.Identity()
	currentIdentity.SnapshotRev++
	newView := rustTestIdentityView{rustTestView: view, identity: currentIdentity}
	prior := request.Provenance[request.Scopes[0].ID]
	restored, err := RebuildVerifiedPlannerRequest(context.Background(), newView, rustTestRootURI, []model.Provenance{prior})
	if err != nil {
		t.Fatalf("restore request without a live backend: %v", err)
	}
	if len(restored.Scopes) != 1 || restored.View != newView || restored.Provenance[restored.Scopes[0].ID].Identity != currentIdentity {
		t.Fatalf("restored request does not match current view: %+v", restored)
	}
	if _, err := os.Stat(filepath.Join(view.rootPath, "target")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("planner restoration polluted the captured source with target output: stat err=%v", err)
	}

	legacy := prior
	legacy.Scope = cloneRustScope(prior.Scope)
	delete(legacy.Scope.Build.Options, rustPlannerInputsOption)
	legacy.Scope.BuildContext = model.ComputeBuildContextID(legacy.Scope, legacy.Extractor, legacy.ExtractorVer, legacy.Toolchain, legacy.Tools)
	if _, err := RebuildVerifiedPlannerRequest(context.Background(), newView, rustTestRootURI, []model.Provenance{legacy}); !errors.Is(err, model.ErrInvalidProvenance) {
		t.Fatalf("legacy marker-less provenance error = %v, want invalid provenance", err)
	}
}

func TestRustPlannerInputMarkerRejectsNonCanonicalAndOversizedValues(t *testing.T) {
	build := model.BuildInputs{Options: map[string]string{"procMacros": "false"}}
	encoded, err := encodeRustPlannerInputs(build)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRustPlannerInputs(encoded)
	if err != nil || !reflect.DeepEqual(decoded, build) {
		t.Fatalf("canonical marker round trip = %+v, %v", decoded, err)
	}
	if _, err := decodeRustPlannerInputs(" {" + encoded[1:]); err == nil {
		t.Fatal("accepted non-canonical Rust planner marker")
	}
	if _, err := decodeRustPlannerInputs(strings.Repeat("x", maxRustPlannerInputsBytes+1)); err == nil {
		t.Fatal("accepted oversized Rust planner marker")
	}
	if _, err := encodeRustPlannerInputs(model.BuildInputs{Options: map[string]string{rustPlannerInputsOption: "user"}}); err == nil {
		t.Fatal("accepted user-supplied reserved planner marker")
	}
}

func rustPlannerTestTool(t *testing.T, name, path string) model.ToolIdentity {
	t.Helper()
	absPath, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("identify installed %s: %v", name, err)
	}
	file, err := os.Open(absPath)
	if err != nil {
		t.Fatalf("open installed %s: %v", name, err)
	}
	digest, _, err := hashReader(context.Background(), file)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("hash installed %s: %v", name, errors.Join(err, closeErr))
	}
	version, err := probePinnedToolVersion(context.Background(), absPath)
	if err != nil {
		t.Fatalf("query installed %s version: %v", name, err)
	}
	return model.ToolIdentity{Name: name, Path: absPath, Version: version, SHA256: digest}
}

func TestRustRequestBuilderRejectsUnpinnedCargoAndRustc(t *testing.T) {
	view := newRustTestView(t, map[string][]byte{rustTestRootURI + "Cargo.toml": []byte("[package]\nname=\"a\"\nversion=\"0.1.0\"\n")})
	ra := rustTestTools(t, false)[0]
	builder := &SemanticIndexRequestBuilder{
		backend: &Backend{}, config: RustIndexToolConfig{Tools: []model.ToolIdentity{ra}, Toolchain: "rustc-test"},
		run: func(context.Context, scipInvocation) ([]byte, error) {
			t.Fatal("unconfigured Cargo was invoked")
			return nil, nil
		},
		activeRAPath: func() string { return ra.Path },
	}
	if _, err := builder.BuildIndexRequest(context.Background(), view, rustTestRootURI); err == nil || !strings.Contains(err.Error(), cargoToolName) {
		t.Fatalf("missing explicit Cargo identity error = %v", err)
	}
}

func rustTestScope(id, rootURI string, features []string, target string) model.Scope {
	rootURI = strings.TrimSuffix(rootURI, "/")
	options := map[string]string{
		"procMacros": "false", "buildScripts": "false", "cargo.targetSrcPath": "src/lib.rs",
	}
	if target != "" {
		options["target"] = target
	}
	scope := model.Scope{
		ID: id, Language: langID, RootURI: rootURI,
		Build: model.BuildInputs{Features: append([]string(nil), features...), Options: options, Tests: true},
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, "rust-analyzer-scip", "test", "rustc-test", nil)
	return scope
}

func rustTestRequest(view model.WorkspaceView, scope model.Scope, tools []model.ToolIdentity) model.Request {
	rustAnalyzerVersion := "test"
	for _, tool := range tools {
		if tool.Name == rustAnalyzerToolName {
			rustAnalyzerVersion = tool.Version
			break
		}
	}
	extractorVersion := rustSemanticExtractorVersion(rustAnalyzerVersion)
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion, Identity: view.Identity(), Scope: scope,
		Extractor: "rust-analyzer-scip", ExtractorVer: extractorVersion, Toolchain: "rustc-test",
		Backend: identity.BackendID{Language: langID, Name: rustAnalyzerToolName}, Tools: append([]model.ToolIdentity(nil), tools...),
	}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	return model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
}

func rustTestTools(t *testing.T, withProcMacro bool) []model.ToolIdentity {
	t.Helper()
	names := []string{rustAnalyzerToolName, cargoToolName, rustcToolName}
	if withProcMacro {
		names = append(names, procMacroToolName)
	}
	tools := make([]model.ToolIdentity, 0, len(names))
	for _, name := range names {
		path := filepath.Join(t.TempDir(), name+".exe")
		data := []byte("test pinned executable: " + name)
		if err := os.WriteFile(path, data, 0o700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		tools = append(tools, model.ToolIdentity{Name: name, Path: path, Version: "test-" + name, SHA256: hex.EncodeToString(sum[:])})
	}
	return tools
}

func rustTestToolMap(tools []model.ToolIdentity) map[string]model.ToolIdentity {
	result := make(map[string]model.ToolIdentity, len(tools))
	for _, tool := range tools {
		result[tool.Name] = tool
	}
	return result
}

type rustTestView struct {
	identity identity.WorkspaceID
	files    map[string][]byte
	rootPath string
}

type rustTestIdentityView struct {
	*rustTestView
	identity model.Identity
}

func (v rustTestIdentityView) Identity() model.Identity { return v.identity }

func rustTestSnapshotDigest(files map[string][]byte) identity.ContentHash {
	uris := make([]string, 0, len(files))
	for uri := range files {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	h := sha256.New()
	for _, uri := range uris {
		_, _ = fmt.Fprintf(h, "%d:", len(uri))
		_, _ = h.Write([]byte(uri))
		_, _ = fmt.Fprintf(h, "%d:", len(files[uri]))
		_, _ = h.Write(files[uri])
	}
	return identity.ContentHash("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

func equalRustFactMultiset[T comparable](want, got []T) bool {
	if len(want) != len(got) {
		return false
	}
	counts := make(map[T]int, len(want))
	for _, fact := range want {
		counts[fact]++
	}
	for _, fact := range got {
		if counts[fact] == 0 {
			return false
		}
		counts[fact]--
	}
	return true
}

func newRustTestView(t *testing.T, files map[string][]byte) *rustTestView {
	t.Helper()
	copyFiles := make(map[string][]byte, len(files))
	rootPath := t.TempDir()
	rootURL, err := url.Parse(rustTestRootURI)
	if err != nil {
		t.Fatal(err)
	}
	for uri, contents := range files {
		copyFiles[uri] = append([]byte(nil), contents...)
		fileURL, parseErr := url.Parse(uri)
		if parseErr != nil || !strings.HasPrefix(fileURL.Path, rootURL.Path) {
			t.Fatalf("invalid test view URI %q", uri)
		}
		relative := strings.TrimPrefix(fileURL.Path, rootURL.Path)
		path := filepath.Join(rootPath, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &rustTestView{identity: identity.WorkspaceID("rust-test-workspace"), files: copyFiles, rootPath: rootPath}
}

func (v *rustTestView) Identity() model.Identity {
	return model.Identity{Workspace: v.identity, DiskDigest: identity.ContentHash("sha256:test"), SnapshotRev: 7}
}

func (v *rustTestView) Walk(ctx context.Context, rootURI string, visit func(model.File) error) error {
	for uri, content := range v.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasPrefix(uri, rootURI) {
			continue
		}
		sum := sha256.Sum256(content)
		if err := visit(model.File{URI: uri, LanguageID: langID, Size: int64(len(content)), SHA256: identity.ContentHash("sha256:" + hex.EncodeToString(sum[:]))}); err != nil {
			return err
		}
	}
	return nil
}

func (v *rustTestView) Read(ctx context.Context, uri string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	contents, ok := v.files[uri]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(contents)), nil
}

func (v *rustTestView) Materialize(ctx context.Context, rootURI, destination string) (model.MaterializedView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonical, err := canonicalRustWorkspaceRoot(rootURI)
	if err != nil || canonical != rustTestCanonicalRootURI {
		return nil, fmt.Errorf("unexpected test materialization root %q", rootURI)
	}
	rootPath := v.rootPath
	rootPath, err = filepath.Abs(rootPath)
	if err != nil {
		return nil, err
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	if pathWithin(rootPath, destination) || pathWithin(destination, rootPath) {
		return nil, errors.New("test materialization destination overlaps captured source")
	}
	return rustTestMaterializedView{rootURI: rootURI, rootPath: rootPath}, nil
}

type rustTestMaterializedView struct {
	rootURI  string
	rootPath string
}

func (v rustTestMaterializedView) RootURI() string  { return v.rootURI }
func (v rustTestMaterializedView) RootPath() string { return v.rootPath }
func (v rustTestMaterializedView) Close() error     { return nil }
func (v rustTestMaterializedView) PathForURI(uri string) (string, error) {
	rootURL, err := url.Parse(v.rootURI)
	if err != nil {
		return "", err
	}
	fileURL, err := url.Parse(uri)
	if err != nil || fileURL.Scheme != rootURL.Scheme || !strings.HasPrefix(fileURL.Path, rootURL.Path) {
		return "", errors.New("URI outside test materialization")
	}
	return filepath.Join(v.rootPath, filepath.FromSlash(strings.TrimPrefix(fileURL.Path, rootURL.Path))), nil
}

type rustTestSink struct {
	symbols     []model.Symbol
	occurrences []model.Occurrence
	edges       []model.Edge
}

type rustTestGeneratedView struct {
	*rustTestView
	generatedURI string
	sourceURI    string
	sourceMap    []model.SourceMapSpan
}

func (v rustTestGeneratedView) Walk(ctx context.Context, rootURI string, visit func(model.File) error) error {
	return v.rustTestView.Walk(ctx, rootURI, func(file model.File) error {
		if file.URI == v.generatedURI {
			file.Generated = true
			file.SourceURI = v.sourceURI
			file.SourceMap = append([]model.SourceMapSpan(nil), v.sourceMap...)
		}
		return visit(file)
	})
}

func rustTestSemanticHelper(t *testing.T) model.ToolIdentity {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rust-analyzer-semantic-helper.exe")
	data := []byte("test pinned same-source Rust semantic helper")
	if err := os.WriteFile(path, data, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return model.ToolIdentity{
		Name: rustSemanticHelperToolName, Path: path,
		// The witness marker advertises the helper generation that implements
		// --coverage-root and the schema-2 completeness sidecar (see
		// rustSemanticHelperWitnessMarker); the exporter sends the flag only
		// to this generation.
		Version: "rust-analyzer 1.97.0+omnilsp-semantic-witness.test",
		SHA256:  hex.EncodeToString(sum[:]),
	}
}

func rustTestSidecarRelation(kind, document, source, target string, startLine, startChar, endLine, endChar uint32) rustSidecarRelation {
	return rustSidecarRelation{
		Kind: kind, SourceDocument: document, SourceSymbol: source, TargetSymbol: target,
		Range: rustSidecarRange{
			Start: &rustSidecarPosition{Line: &startLine, Character: &startChar},
			End:   &rustSidecarPosition{Line: &endLine, Character: &endChar},
		},
	}
}

func rustTestCompletenessSidecar(t *testing.T, indexData []byte, source string, blockers []string) rustRelationSidecar {
	t.Helper()
	var index scip.Index
	if err := proto.Unmarshal(indexData, &index); err != nil {
		t.Fatalf("decode test SCIP for witness: %v", err)
	}
	symbols := make(map[string]struct{})
	for _, symbol := range index.ExternalSymbols {
		if symbol != nil {
			symbols[symbol.GetSymbol()] = struct{}{}
		}
	}
	var occurrences uint64
	for _, document := range index.Documents {
		if document == nil {
			continue
		}
		for _, symbol := range document.Symbols {
			if symbol != nil {
				symbols[symbol.GetSymbol()] = struct{}{}
			}
		}
		for _, occurrence := range document.Occurrences {
			if occurrence != nil {
				symbols[occurrence.GetSymbol()] = struct{}{}
				occurrences++
			}
		}
	}
	sourceDigest := sha256.Sum256([]byte(source))
	scipDigest := sha256.Sum256(indexData)
	blockers = append([]string{}, blockers...)
	sort.Strings(blockers)
	counts := &rustWitnessCounts{
		SourceFiles: 1, SCIPDocuments: uint64(len(index.Documents)), SCIPSymbols: uint64(len(symbols)), SCIPOccurrences: occurrences,
		CandidateTokens: 3, ClassifiedCandidateTokens: 3,
	}
	for _, blocker := range blockers {
		if blocker == "macro-syntax" {
			counts.MacroSites = 1
		}
	}
	return rustRelationSidecar{
		SchemaVersion: 2, SCIPSHA256: hex.EncodeToString(scipDigest[:]), SelectedCrateRoot: "src/lib.rs",
		SourceManifest: []rustWitnessSource{{Document: "src/lib.rs", SHA256: hex.EncodeToString(sourceDigest[:])}},
		WitnessCounts:  counts, Blockers: blockers, Relations: []rustSidecarRelation{},
	}
}

func cloneRustWitnessCounts(counts *rustWitnessCounts) *rustWitnessCounts {
	if counts == nil {
		return nil
	}
	copy := *counts
	return &copy
}

func marshalRustTestCompletenessSidecar(sidecar rustRelationSidecar) ([]byte, error) {
	return json.Marshal(struct {
		SchemaVersion     int                   `json:"schema_version"`
		SCIPSHA256        string                `json:"scip_sha256"`
		SelectedCrateRoot string                `json:"selected_crate_root"`
		SourceManifest    []rustWitnessSource   `json:"source_manifest"`
		WitnessCounts     *rustWitnessCounts    `json:"witness_counts"`
		Blockers          []string              `json:"blockers"`
		Relations         []rustSidecarRelation `json:"relations"`
	}{
		SchemaVersion: sidecar.SchemaVersion, SCIPSHA256: sidecar.SCIPSHA256,
		SelectedCrateRoot: sidecar.SelectedCrateRoot, SourceManifest: sidecar.SourceManifest,
		WitnessCounts: sidecar.WitnessCounts, Blockers: sidecar.Blockers, Relations: sidecar.Relations,
	})
}

func mustMarshalRustSidecar(t *testing.T, relations []rustSidecarRelation) []byte {
	t.Helper()
	encoded, err := json.Marshal(rustRelationSidecar{SchemaVersion: 1, Relations: relations})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func rustSCIPRelationFixtureAtRoot(t *testing.T, projectRoot, relativePath, content string) []byte {
	t.Helper()
	lines := strings.Split(content, "\n")
	if len(lines) < 3 {
		t.Fatalf("Rust relation fixture requires at least three lines: %q", content)
	}
	const callerSymbol = "rust . crate . caller()."
	const targetSymbol = "rust . crate . target()."
	const crateSymbol = "rust . crate ."
	const childSymbol = "rust . crate . child/"
	lineStart := func(line int, token string) int32 {
		index := strings.Index(lines[line], token)
		if index < 0 {
			t.Fatalf("Rust relation fixture line %d lacks %q: %q", line, token, lines[line])
		}
		return int32(index)
	}
	definition := func(symbol string, kind scip.SymbolInformation_Kind, displayName string) *scip.SymbolInformation {
		return &scip.SymbolInformation{Symbol: symbol, Kind: kind, DisplayName: displayName}
	}
	doc := &scip.Document{
		Language: "rust", RelativePath: relativePath,
		Symbols: []*scip.SymbolInformation{
			definition(callerSymbol, scip.SymbolInformation_Function, "caller"),
			definition(targetSymbol, scip.SymbolInformation_Function, "target"),
		},
		Occurrences: []*scip.Occurrence{
			{Symbol: callerSymbol, SymbolRoles: int32(scip.SymbolRole_Definition), Range: []int32{1, lineStart(1, "caller"), 1, lineStart(1, "caller") + int32(len("caller"))}},
			{Symbol: targetSymbol, SymbolRoles: int32(scip.SymbolRole_Definition), Range: []int32{2, lineStart(2, "target"), 2, lineStart(2, "target") + int32(len("target"))}},
			{Symbol: targetSymbol, Range: []int32{1, lineStart(1, "target"), 1, lineStart(1, "target") + int32(len("target"))}},
		},
	}
	index := &scip.Index{
		Metadata: &scip.Metadata{
			ProjectRoot: projectRoot, TextDocumentEncoding: scip.TextEncoding_UTF8,
			ToolInfo: &scip.ToolInfo{Name: "rust-analyzer", Version: "test"},
		},
		Documents: []*scip.Document{doc},
		ExternalSymbols: []*scip.SymbolInformation{{
			Symbol: crateSymbol, Kind: scip.SymbolInformation_Namespace, DisplayName: "crate",
		}},
	}
	if strings.Contains(content, "pub mod child") {
		start := lineStart(3, "child")
		doc.Symbols = append(doc.Symbols, definition(childSymbol, scip.SymbolInformation_Module, "child"))
		doc.Occurrences = append(doc.Occurrences, &scip.Occurrence{
			Symbol: childSymbol, SymbolRoles: int32(scip.SymbolRole_Definition), Range: []int32{3, start, 3, start + int32(len("child"))},
		})
	}
	data, err := proto.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func viewFileHash(t *testing.T, view *rustTestView, uri string) identity.ContentHash {
	t.Helper()
	data, ok := view.files[uri]
	if !ok {
		t.Fatalf("test view has no file %s", uri)
	}
	sum := sha256.Sum256(data)
	return identity.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}

func (s *rustTestSink) WriteSymbols(ctx context.Context, values []model.Symbol) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.symbols = append(s.symbols, values...)
	return nil
}
func (s *rustTestSink) WriteOccurrences(ctx context.Context, values []model.Occurrence) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.occurrences = append(s.occurrences, values...)
	return nil
}
func (s *rustTestSink) WriteEdges(ctx context.Context, values []model.Edge) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.edges = append(s.edges, values...)
	return nil
}

func rustSCIPFixture(t *testing.T) []byte {
	return rustSCIPFixtureAt(t, "src/lib.rs")
}

func rustSCIPFixtureAt(t *testing.T, relativePath string) []byte {
	return rustSCIPFixtureAtRoot(t, rustTestCanonicalRootURI, relativePath)
}

func rustSCIPFixtureAtRoot(t *testing.T, projectRoot, relativePath string) []byte {
	t.Helper()
	contents := "// 🫠\npub fn café() {}\npub fn use_it() { café(); }\n"
	lines := strings.Split(contents, "\n")
	definitionStart := int32(strings.Index(lines[1], "café"))
	referenceLine := lines[2]
	referenceStart := int32(strings.Index(referenceLine, "café"))
	index := &scip.Index{
		Metadata: &scip.Metadata{ProjectRoot: projectRoot, TextDocumentEncoding: scip.TextEncoding_UTF8, ToolInfo: &scip.ToolInfo{Name: "rust-analyzer", Version: "test"}},
		Documents: []*scip.Document{{
			Language: "rust", RelativePath: relativePath,
			Symbols: []*scip.SymbolInformation{{
				Symbol: "rust . crate . café().", DisplayName: "café", Kind: scip.SymbolInformation_Function,
				Relationships: []*scip.Relationship{
					{Symbol: "rust . crate . Trait#", IsImplementation: true},
					{Symbol: "rust . crate . Type#", IsTypeDefinition: true},
				},
			}},
			Occurrences: []*scip.Occurrence{
				{Symbol: "rust . crate . café().", SymbolRoles: int32(scip.SymbolRole_Definition), Range: []int32{1, definitionStart, 1, definitionStart + 5}},
				{Symbol: "rust . crate . café().", Range: []int32{2, referenceStart, 2, referenceStart + 5}},
			},
		}},
	}
	data, err := proto.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func containsArg(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}

func coverageFor(report model.Report, scopeID string, fact model.FactKind) model.Coverage {
	for _, item := range report.Coverage {
		if item.ScopeID == scopeID && item.Fact == fact {
			return item
		}
	}
	return model.Coverage{}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsEnv(env []string, prefix string) bool {
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}

func rustSameToolSet(a, b []model.ToolIdentity) bool {
	if len(a) != len(b) {
		return false
	}
	left := append([]model.ToolIdentity(nil), a...)
	right := append([]model.ToolIdentity(nil), b...)
	sort.Slice(left, func(i, j int) bool { return left[i].Name < left[j].Name })
	sort.Slice(right, func(i, j int) bool { return right[i].Name < right[j].Name })
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()
	left, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(left, right)
}
