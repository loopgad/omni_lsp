package clang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

type verifiedPlannerFixture struct {
	view      *testView
	rootURI   string
	request   model.Request
	compiler  string
	header    string
	compileDB string
}

func newVerifiedPlannerFixture(t *testing.T) verifiedPlannerFixture {
	t.Helper()
	logicalRoot, snapshotRoot := t.TempDir(), t.TempDir()
	rootURI := workspaceuri.FromPath(logicalRoot).Canonical()
	toolDir := t.TempDir()
	compilerPath := filepath.Join(toolDir, "clang++.exe")
	libclangPath := filepath.Join(toolDir, "libclang.dll")
	for path, contents := range map[string]string{
		compilerPath: "intentionally not an executable: clang++",
		libclangPath: "intentionally not a library: libclang",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	headerPath := filepath.Join(toolDir, "helper.hpp")
	if err := os.WriteFile(headerPath, []byte("#define HELPER_VALUE 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, manifestDigest, err := helperDependencyContentManifest(context.Background(), []string{headerPath})
	if err != nil {
		t.Fatal(err)
	}
	manifestJSON, err := encodeHelperHeaderManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	const compilerVersion = "22.1.5"
	compiler := testToolIdentity(t, "clang++", compilerPath, compilerVersion)
	libclang := testToolIdentity(t, "libclang.dll", libclangPath, compilerVersion)
	sourcePath := filepath.Join(logicalRoot, "main.cpp")
	arguments := []string{
		compilerPath, "-std=c++17", "-I", "include", "-DVALUE=1", "-c", sourcePath,
		"-o", filepath.Join(logicalRoot, "main.obj"),
	}
	database, err := json.Marshal([]rawCompileCommand{{
		Directory: logicalRoot,
		File:      sourcePath,
		Arguments: arguments,
	}})
	if err != nil {
		t.Fatal(err)
	}
	view := newTestView(t, rootURI, logicalRoot, snapshotRoot, map[string][]byte{
		"main.cpp":              []byte("int main() { return VALUE; }\n"),
		"compile_commands.json": database,
	})
	canonicalArgs := normalizeBuildArgs(arguments, sourcePath, logicalRoot)
	group := &compileContext{
		key:              compileContextDigest(compilerPath, "", canonicalArgs),
		compiler:         compiler,
		libclang:         libclang,
		args:             canonicalArgs,
		files:            map[string]struct{}{"main.cpp": {}},
		extractorVersion: ExtractorVersionPinned() + "+headers-sha256:" + manifestDigest,
		headerManifest:   manifestJSON,
	}
	request, err := requestForContexts(context.Background(), view, rootURI,
		identity.BackendID{Language: "cpp", Name: "ccls-test"}, 3, []*compileContext{group})
	if err != nil {
		t.Fatal(err)
	}
	return verifiedPlannerFixture{
		view: view, rootURI: rootURI, request: request, compiler: compilerPath,
		header:    headerPath,
		compileDB: workspaceuri.FromPath(filepath.Join(logicalRoot, "compile_commands.json")).Canonical(),
	}
}

func testToolIdentity(t *testing.T, name, path, version string) model.ToolIdentity {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	return model.ToolIdentity{Name: name, Path: path, Version: version, SHA256: hex.EncodeToString(digest[:])}
}

func TestRebuildVerifiedPlannerRequestDoesNotExecutePinnedTools(t *testing.T) {
	fixture := newVerifiedPlannerFixture(t)
	got, err := RebuildVerifiedPlannerRequest(context.Background(), fixture.view, fixture.rootURI,
		[]model.Provenance{fixture.request.Provenance[fixture.request.Scopes[0].ID]})
	if err != nil {
		t.Fatalf("rebuild verified planner request: %v", err)
	}
	if !reflect.DeepEqual(got.Scopes, fixture.request.Scopes) || !reflect.DeepEqual(got.Provenance, fixture.request.Provenance) {
		t.Fatalf("rebuilt planner request differs from its captured inputs:\n got: %#v\nwant: %#v", got, fixture.request)
	}
}

func TestRebuildVerifiedPlannerRequestRejectsCompileConfigurationDrift(t *testing.T) {
	fixture := newVerifiedPlannerFixture(t)
	data := strings.Replace(string(fixture.view.data[fixture.compileDB]), `"-DVALUE=1"`, `"-DVALUE=2"`, 1)
	if data == string(fixture.view.data[fixture.compileDB]) {
		t.Fatal("test compile database did not contain the expected configuration flag")
	}
	fixture.view.data[fixture.compileDB] = []byte(data)
	if _, err := RebuildVerifiedPlannerRequest(context.Background(), fixture.view, fixture.rootURI,
		[]model.Provenance{fixture.request.Provenance[fixture.request.Scopes[0].ID]}); err == nil {
		t.Fatal("compile configuration drift reused the captured planner attestation")
	}
}

func TestRebuildVerifiedPlannerRequestRejectsCompilerEnvironmentDrift(t *testing.T) {
	t.Setenv("CPATH", "before")
	fixture := newVerifiedPlannerFixture(t)
	t.Setenv("CPATH", "after")
	if _, err := RebuildVerifiedPlannerRequest(context.Background(), fixture.view, fixture.rootURI,
		[]model.Provenance{fixture.request.Provenance[fixture.request.Scopes[0].ID]}); err == nil {
		t.Fatal("compiler environment drift reused the captured planner attestation")
	}
}

func TestRebuildVerifiedPlannerRequestRejectsHeaderAndToolDrift(t *testing.T) {
	t.Run("helper header", func(t *testing.T) {
		fixture := newVerifiedPlannerFixture(t)
		if err := os.WriteFile(fixture.header, []byte("#define HELPER_VALUE 2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := RebuildVerifiedPlannerRequest(context.Background(), fixture.view, fixture.rootURI,
			[]model.Provenance{fixture.request.Provenance[fixture.request.Scopes[0].ID]}); err == nil {
			t.Fatal("helper header drift reused the captured planner attestation")
		}
	})
	t.Run("compiler binary", func(t *testing.T) {
		fixture := newVerifiedPlannerFixture(t)
		if err := os.WriteFile(fixture.compiler, []byte("changed compiler bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := RebuildVerifiedPlannerRequest(context.Background(), fixture.view, fixture.rootURI,
			[]model.Provenance{fixture.request.Provenance[fixture.request.Scopes[0].ID]}); err == nil {
			t.Fatal("compiler hash drift reused the captured planner attestation")
		}
	})
}

func TestRebuildVerifiedPlannerRequestRejectsLegacyManifestlessAttestation(t *testing.T) {
	fixture := newVerifiedPlannerFixture(t)
	scope := fixture.request.Scopes[0]
	provenance := fixture.request.Provenance[scope.ID]
	scope.Build.Options = map[string]string{contextOption: scope.Build.Options[contextOption]}
	provenance.ExtractorVer = ExtractorVersionPinned()
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	if _, err := RebuildVerifiedPlannerRequest(context.Background(), fixture.view, fixture.rootURI,
		[]model.Provenance{provenance}); err == nil {
		t.Fatal("legacy manifestless planner attestation was accepted")
	}
}

func TestClangPlannerInputBudgetsRejectOversizedInputs(t *testing.T) {
	t.Run("compile database", func(t *testing.T) {
		root := t.TempDir()
		rootURI := workspaceuri.FromPath(root).Canonical()
		view := newTestView(t, rootURI, root, t.TempDir(), map[string][]byte{
			"compile_commands.json": []byte("[]"),
		})
		fileURI := workspaceuri.FromPath(filepath.Join(root, "compile_commands.json")).Canonical()
		file := view.files[fileURI]
		file.Size = maxCompileDBBytes + 1
		if _, found, err := readRawCommands(context.Background(), view, []model.File{file}); err == nil || !found || !strings.Contains(err.Error(), "indexing limit") {
			t.Fatalf("oversized compile database result: found=%v err=%v", found, err)
		}
	})
	t.Run("helper header count", func(t *testing.T) {
		paths := make([]string, maxHelperHeaders+1)
		if _, _, err := helperDependencyContentManifest(context.Background(), paths); err == nil {
			t.Fatal("helper dependency manifest exceeded its header-count budget")
		}
	})
}
