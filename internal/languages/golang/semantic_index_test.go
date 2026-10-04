package golang

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	semanticindex "github.com/omnilsp/omni/internal/index/semantic"
	"golang.org/x/tools/go/packages"
)

type goIndexTestView struct {
	id        model.Identity
	root      string
	rootURI   string
	files     []model.File
	content   map[string][]byte
	closeCall int
}

func newGoIndexTestView(t *testing.T) *goIndexTestView {
	t.Helper()
	root := t.TempDir()
	view := &goIndexTestView{
		id:      model.Identity{Workspace: "go-semantic-fixture", DiskDigest: "sha256:fixture", SnapshotRev: 41},
		root:    root,
		rootURI: pathToUri(root),
		content: make(map[string][]byte),
	}
	add := func(relative, contents string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		data := []byte(contents)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		fileURI := pathToUri(path)
		digest := sha256.Sum256(data)
		view.files = append(view.files, model.File{
			URI: fileURI, LanguageID: "go", Size: int64(len(data)),
			SHA256: identity.ContentHash("sha256:" + hex.EncodeToString(digest[:])),
		})
		view.content[fileURI] = append([]byte(nil), data...)
	}
	add("go.work", "go 1.26.1\n\nuse (\n\t./app\n\t./lib\n)\n")
	add("app/go.mod", "module example.com/app\n\ngo 1.26.1\n\nrequire example.com/lib v0.0.0\n")
	add("lib/go.mod", "module example.com/lib\n\ngo 1.26.1\n")
	add("app/api.go", `package app

import "example.com/lib"

type Reader interface {
	Read() string
}

type Item struct{}

func (Item) Read() string { return lib.Value() }
`)
	add("app/generic.go", `package app

type Box[T any] struct {
	Value T
}

func NewBox[T any](value T) Box[T] { return Box[T]{Value: value} }
`)
	add("app/use.go", `package app

import "example.com/lib"

func Use() string {
	box := NewBox(lib.Value())
	var reader Reader = Item{}
	return box.Value + reader.Read()
}

func Target() BuildTarget { return BuildTarget{} }
`)
	add("app/target_windows.go", `//go:build windows && client

package app

type BuildTarget struct{ WindowsOnly int }
`)
	add("app/target_linux.go", `//go:build linux && server

package app

type BuildTarget struct{ LinuxOnly int }
`)
	add("lib/value.go", `package lib

func Value() string { return "value" }
`)
	sort.Slice(view.files, func(i, j int) bool { return view.files[i].URI < view.files[j].URI })
	return view
}

func (v *goIndexTestView) Identity() model.Identity { return v.id }

func (v *goIndexTestView) Walk(ctx context.Context, rootURI string, visit func(model.File) error) error {
	prefix := strings.TrimSuffix(rootURI, "/") + "/"
	for _, file := range v.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.URI != rootURI && !strings.HasPrefix(file.URI, prefix) {
			continue
		}
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func (v *goIndexTestView) Read(ctx context.Context, uri string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, ok := v.content[uri]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (v *goIndexTestView) Materialize(ctx context.Context, rootURI, destination string) (model.MaterializedView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dest, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	rel, relErr := filepath.Rel(v.root, dest)
	if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("materializer destination is inside the immutable fixture")
	}
	rootPath := uriToPath(rootURI)
	if rootPath == "" {
		return nil, fmt.Errorf("cannot resolve test root URI %q", rootURI)
	}
	return &goIndexTestMaterialized{view: v, rootURI: rootURI, rootPath: rootPath}, nil
}

type goIndexTestMaterialized struct {
	view              *goIndexTestView
	rootURI, rootPath string
	closed            bool
}

func (v *goIndexTestMaterialized) RootURI() string  { return v.rootURI }
func (v *goIndexTestMaterialized) RootPath() string { return v.rootPath }
func (v *goIndexTestMaterialized) Close() error {
	if !v.closed {
		v.closed = true
		v.view.closeCall++
	}
	return nil
}
func (v *goIndexTestMaterialized) PathForURI(value string) (string, error) {
	if v.closed {
		return "", os.ErrClosed
	}
	root, err := filepath.Abs(v.rootPath)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(uriToPath(value))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("URI outside test materialized root: %s", value)
	}
	if _, ok := v.view.content[value]; !ok {
		info, statErr := os.Stat(path)
		if statErr != nil || !info.IsDir() {
			return "", os.ErrNotExist
		}
	}
	return path, nil
}

type goIndexTestSink struct {
	symbols     []model.Symbol
	occurrences []model.Occurrence
	edges       []model.Edge
	maxBatch    int
}

func (s *goIndexTestSink) WriteSymbols(_ context.Context, values []model.Symbol) error {
	s.maxBatch = max(s.maxBatch, len(values))
	s.symbols = append(s.symbols, values...)
	return nil
}
func (s *goIndexTestSink) WriteOccurrences(_ context.Context, values []model.Occurrence) error {
	s.maxBatch = max(s.maxBatch, len(values))
	s.occurrences = append(s.occurrences, values...)
	return nil
}
func (s *goIndexTestSink) WriteEdges(_ context.Context, values []model.Edge) error {
	s.maxBatch = max(s.maxBatch, len(values))
	s.edges = append(s.edges, values...)
	return nil
}

func TestGoSemanticIndexDifferentialWithLockedGoplsAcrossBuildContexts(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "auto")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOENV", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	view := newGoIndexTestView(t)
	backend := New(view.root)
	baseRequest, err := backend.BuildIndexRequest(context.Background(), view, view.rootURI)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	if len(baseRequest.Scopes) != 1 {
		t.Fatalf("workspace scopes = %d, want one go.work scope", len(baseRequest.Scopes))
	}
	base := baseRequest.Scopes[0]
	if base.Build.Environment["GOOS"] == "" || base.Build.Environment["GOARCH"] == "" || base.Build.Options["go.mod"] != "readonly" {
		t.Fatalf("request omitted explicit Go build inputs: %+v", base.Build)
	}
	if got := baseRequest.Provenance[base.ID].Tools; len(got) != 1 || got[0].Name != "go" || got[0].SHA256 == "" {
		t.Fatalf("Go executable identity was not pinned: %+v", got)
	}

	contexts := []struct{ suffix, goos, tag string }{
		{suffix: "windows-client", goos: "windows", tag: "client"},
		{suffix: "linux-server", goos: "linux", tag: "server"},
	}
	request := model.Request{View: view, Scopes: make([]model.Scope, 0, len(contexts)), Provenance: make(map[string]model.Provenance, len(contexts))}
	for _, context := range contexts {
		scope := base
		scope.ID += ":" + context.suffix
		scope.Build.Environment = cloneStringMap(base.Build.Environment)
		scope.Build.Environment["GOOS"] = context.goos
		scope.Build.Environment["GOARCH"] = "amd64"
		scope.Build.Features = []string{context.tag}
		provenance := baseRequest.Provenance[base.ID]
		scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
		provenance.Scope = scope
		request.Scopes = append(request.Scopes, scope)
		request.Provenance[scope.ID] = provenance
	}
	if request.Scopes[0].BuildContext == request.Scopes[1].BuildContext {
		t.Fatal("different Go build contexts shared a cache identity")
	}

	sink := &goIndexTestSink{}
	report, err := backend.ExportIndex(context.Background(), request, sink)
	if err != nil {
		t.Fatalf("ExportIndex: %v", err)
	}
	if err := model.ValidateReport(request, report); err != nil {
		t.Fatalf("ValidateReport: %v", err)
	}
	if sink.maxBatch > semanticIndexBatchSize {
		t.Fatalf("sink received batch of %d, limit is %d", sink.maxBatch, semanticIndexBatchSize)
	}
	if view.closeCall != 3 {
		t.Fatalf("MaterializedView.Close called %d times, want once for discovery and once per scope", view.closeCall)
	}
	if len(report.Coverage) != len(request.Scopes)*len(model.RequiredFactKinds) {
		t.Fatalf("coverage entries = %d, want %d", len(report.Coverage), len(request.Scopes)*len(model.RequiredFactKinds))
	}
	assertUniqueGoSymbolIDs(t, sink.symbols)

	goplsPath, goplsAvailable := lookupLockedGopls(t)
	definitionFile := filepath.Join(view.root, "app", "use.go")
	definitionContents := view.content[pathToUri(definitionFile)]
	refsFile := filepath.Join(view.root, "lib", "value.go")
	refsContents := view.content[pathToUri(refsFile)]
	implementationFile := filepath.Join(view.root, "app", "api.go")
	implementationContents := view.content[pathToUri(implementationFile)]

	for _, scope := range request.Scopes {
		if !hasGoEdgeNamed(t, sink, scope.ID, "Use", "NewBox", model.EdgeCall) ||
			!hasGoEdgeNamed(t, sink, scope.ID, "Use", "Value", model.EdgeCall) {
			t.Errorf("scope %s is missing typed generic/cross-module call edges", scope.ID)
		}
		if !hasGoEdgeSuffix(sink.edges, scope.ID, "go:package:example.com/app", "go:package:example.com/lib", model.EdgeImport) {
			t.Errorf("scope %s is missing the cross-module import edge", scope.ID)
		}
		if !hasGoEdgeNamed(t, sink, scope.ID, "NewBox", "Box", model.EdgeTypeRelation) ||
			!hasGoEdgeTargetSuffix(sink.edges, scope.ID, "go:predeclared:string", model.EdgeTypeRelation) {
			t.Errorf("scope %s is missing typed generic/type-argument relations", scope.ID)
		}
		if !hasGoEdgeSuffix(sink.edges, scope.ID, "go:module:example.com/app", "go:module:example.com/lib", model.EdgeModule) {
			var moduleEdges []model.Edge
			for _, edge := range sink.edges {
				if edge.ScopeID == scope.ID && edge.Kind == model.EdgeModule {
					moduleEdges = append(moduleEdges, edge)
				}
			}
			t.Errorf("scope %s is missing module graph edges: coverage=%+v edges=%+v", scope.ID, coverageFor(report.Coverage, scope.ID, model.FactModule), moduleEdges)
		}
		if got := coverageFor(report.Coverage, scope.ID, model.FactInclude); got.State != model.Unavailable || got.Reason == "" {
			t.Errorf("Go include coverage = %+v, want precise unavailable reason", got)
		}
	}
	if !goplsAvailable {
		t.Skip("locked gopls is not installed; Go compiler-index fact checks passed")
	}

	for index, scope := range request.Scopes {
		targetFile := filepath.Join(view.root, "app", "target_"+map[string]string{"windows-client": "windows", "linux-server": "linux"}[contexts[index].suffix]+".go")
		queries := []struct {
			command string
			file    string
			content []byte
			needle  string
			which   int
			role    string
		}{
			{command: "definition", file: definitionFile, content: definitionContents, needle: "NewBox", which: 0, role: "definition"},
			{command: "references", file: refsFile, content: refsContents, needle: "Value", which: 0, role: "references"},
			{command: "implementation", file: implementationFile, content: implementationContents, needle: "Read", which: 0, role: "implementation"},
			{command: "definition", file: definitionFile, content: definitionContents, needle: "BuildTarget", which: 0, role: "build-target"},
		}
		for _, query := range queries {
			output, locations, err := runLockedGoplsQuery(t, goplsPath, view.root, scope, query.command, query.file, query.content, query.needle, query.which)
			if err != nil {
				t.Fatalf("gopls %s (%s): %v\n%s", query.command, query.role, err, output)
			}
			switch query.role {
			case "definition":
				id := findGoObjectID(t, sink.symbols, scope.ID, "NewBox", "function")
				definitionFile := filepath.Join(view.root, "app", "generic.go")
				indexed := occurrencesForIDAtPath(sink.occurrences, id, "definition", definitionFile)
				assertGoplsLocationsMatch(t, locations, output, indexed, definitionFile)
			case "references":
				id := findGoObjectID(t, sink.symbols, scope.ID, "Value", "function")
				indexed := occurrencesForID(sink.occurrences, id, "")
				assertGoplsLocationsMatch(t, locations, output, indexed, "")
			case "implementation":
				readerID := findGoObjectID(t, sink.symbols, scope.ID, "Reader", "type")
				itemID := findGoObjectID(t, sink.symbols, scope.ID, "Item", "type")
				if !hasGoEdge(sink.edges, scope.ID, itemID, readerID, model.EdgeImplementation) {
					t.Fatalf("compiler index missed Item -> Reader implementation in scope %s", scope.ID)
				}
				indexed := occurrencesForIDAtPath(sink.occurrences, itemID, "definition", implementationFile)
				assertGoplsLocationsMatch(t, locations, output, indexed, implementationFile)
			case "build-target":
				id := findGoObjectID(t, sink.symbols, scope.ID, "BuildTarget", "type")
				indexed := occurrencesForIDAtPath(sink.occurrences, id, "definition", targetFile)
				assertGoplsLocationsMatch(t, locations, output, indexed, targetFile)
			}
		}

	}
}

func TestGoSemanticIndexPersistsThroughValidatedSemanticSink(t *testing.T) {
	for key, value := range map[string]string{
		"GOFLAGS": "", "GOWORK": "auto", "GOPROXY": "off", "GOSUMDB": "off",
		"GOENV": "off", "GOTOOLCHAIN": "local", "GOTELEMETRY": "off",
	} {
		t.Setenv(key, value)
	}
	view := newGoIndexTestView(t)
	workspaceHash := sha256.New()
	for _, file := range view.files {
		_, _ = io.WriteString(workspaceHash, file.URI)
		_, _ = workspaceHash.Write([]byte{0})
		_, _ = workspaceHash.Write(view.content[file.URI])
	}
	view.id.DiskDigest = identity.ContentHash("sha256:" + hex.EncodeToString(workspaceHash.Sum(nil)))

	ctx := context.Background()
	backend := New(view.root)
	request, err := backend.BuildIndexRequest(ctx, view, view.rootURI)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	storeRoot := filepath.Join(t.TempDir(), "semantic-index")
	store, err := persistent.NewFileStore(storeRoot, persistent.Config{})
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatalf("BeginBuild: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	sink, err := semanticindex.NewSink(ctx, build, request)
	if err != nil {
		t.Fatalf("NewSink: %v", err)
	}
	defer sink.Close()
	report, err := backend.ExportIndex(ctx, request, sink)
	if err != nil {
		t.Fatalf("ExportIndex through semantic.Sink: %v", err)
	}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatalf("Finalize semantic sink: %v", err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatalf("Commit semantic generation: %v", err)
	}
	committed = true

	reopened, err := persistent.NewFileStore(storeRoot, persistent.Config{})
	if err != nil {
		t.Fatalf("reopen semantic store: %v", err)
	}
	generation, err := reopened.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	reader, err := semanticindex.OpenReader(ctx, generation)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer reader.Close()

	expectedHashes := make(map[string]identity.ContentHash, len(view.files))
	for _, file := range view.files {
		expectedHashes[file.URI] = file.SHA256
	}
	var symbols, occurrences, edges, moduleEdges int
	for {
		batch, err := reader.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read persisted semantic facts: %v", err)
		}
		switch batch.Kind {
		case semanticindex.BatchSymbols:
			symbols += len(batch.Symbols)
		case semanticindex.BatchOccurrences:
			occurrences += len(batch.Occurrences)
		case semanticindex.BatchEdges:
			edges += len(batch.Edges)
			for _, edge := range batch.Edges {
				if edge.Kind != model.EdgeModule {
					continue
				}
				moduleEdges++
				if edge.SourceURI == "" || edge.SourceHash == "" || expectedHashes[edge.SourceURI] != edge.SourceHash {
					t.Fatalf("persisted module edge lost its immutable source identity: %+v", edge)
				}
			}
		}
	}
	if symbols == 0 || occurrences == 0 || edges == 0 || moduleEdges == 0 {
		t.Fatalf("semantic round trip lost required facts: symbols=%d occurrences=%d edges=%d moduleEdges=%d", symbols, occurrences, edges, moduleEdges)
	}
}

func TestGoPackagesExtractorPinMatchesModuleManifest(t *testing.T) {
	root := findGoRepositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	want := ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "golang.org/x/tools" {
			want = fields[1]
			break
		}
	}
	if want == "" {
		t.Fatal("go.mod does not pin golang.org/x/tools")
	}
	if pinnedGoPackagesVersion != want {
		t.Fatalf("go/packages extractor pin = %q, go.mod has %q", pinnedGoPackagesVersion, want)
	}
}

func TestGoSemanticIndexRejectsExternalModfile(t *testing.T) {
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "auto")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOENV", "off")
	t.Setenv("GOTOOLCHAIN", "local")

	view := newGoIndexTestView(t)
	backend := New(view.root)
	base, err := backend.BuildIndexRequest(context.Background(), view, view.rootURI)
	if err != nil {
		t.Fatalf("BuildIndexRequest: %v", err)
	}
	if len(base.Scopes) != 1 {
		t.Fatalf("workspace scopes = %d, want one", len(base.Scopes))
	}
	scope := base.Scopes[0]
	provenance := base.Provenance[scope.ID]
	modfile := filepath.Join(t.TempDir(), "alternate.mod")

	for _, contents := range []string{
		"module example.com/alternate\n\ngo 1.26.1\n",
		"module example.com/alternate\n\ngo 1.26.1\n\nrequire example.com/changed v1.2.3\n",
	} {
		if err := os.WriteFile(modfile, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		candidateScope := scope
		candidateScope.Build.Arguments = []string{"-modfile=" + modfile}
		candidateScope.BuildContext = model.ComputeBuildContextID(candidateScope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
		candidateProvenance := provenance
		candidateProvenance.Scope = candidateScope
		request := model.Request{
			View:       base.View,
			Scopes:     []model.Scope{candidateScope},
			Provenance: map[string]model.Provenance{candidateScope.ID: candidateProvenance},
		}

		report, err := backend.ExportIndex(context.Background(), request, &goIndexTestSink{})
		if err != nil {
			t.Fatalf("ExportIndex with external modfile: %v", err)
		}
		moduleCoverage := coverageFor(report.Coverage, candidateScope.ID, model.FactModule)
		if moduleCoverage.State != model.Unavailable {
			t.Fatalf("FactModule coverage for external modfile = %+v, want unavailable", moduleCoverage)
		}
		if !strings.Contains(moduleCoverage.Reason, "unsupported external-path or execution build argument") {
			t.Fatalf("external modfile was unavailable for an unexpected reason: %+v", moduleCoverage)
		}
		if err := model.ValidateReport(request, report); err != nil {
			t.Fatalf("ValidateReport for rejected external modfile: %v", err)
		}
	}
}

func TestRebuildVerifiedPlannerRequestIsPureAndRejectsDrift(t *testing.T) {
	t.Run("non-executable pinned tool", func(t *testing.T) {
		view, attestations := goVerifiedPlannerFixture(t)
		current := *view
		current.id.SnapshotRev++
		materializations := current.closeCall
		request, err := RebuildVerifiedPlannerRequest(context.Background(), &current, view.rootURI, attestations)
		if err != nil {
			t.Fatalf("RebuildVerifiedPlannerRequest: %v", err)
		}
		if current.closeCall != materializations {
			t.Fatalf("pure planner materialized the workspace %d times", current.closeCall-materializations)
		}
		if len(request.Scopes) != len(attestations) || len(request.Provenance) != len(attestations) {
			t.Fatalf("rebuilt planner request has %d scopes and %d provenances; want %d each", len(request.Scopes), len(request.Provenance), len(attestations))
		}
		for _, scope := range request.Scopes {
			if !sameGoScope(scope, request.Provenance[scope.ID].Scope) {
				t.Fatalf("rebuilt scope and provenance differ: %+v %+v", scope, request.Provenance[scope.ID].Scope)
			}
			if request.Provenance[scope.ID].Identity != current.Identity() {
				t.Fatalf("rebuilt identity = %+v, want %+v", request.Provenance[scope.ID].Identity, current.Identity())
			}
		}
	})

	t.Run("source identity drift", func(t *testing.T) {
		view, attestations := goVerifiedPlannerFixture(t)
		changed := *view
		changed.id.DiskDigest = "sha256:changed-source"
		if _, err := RebuildVerifiedPlannerRequest(context.Background(), &changed, view.rootURI, attestations); err == nil {
			t.Fatal("source identity drift was accepted")
		}
	})

	t.Run("module config closure drift", func(t *testing.T) {
		view, attestations := goVerifiedPlannerFixture(t)
		changed := *view
		changed.files = append([]model.File(nil), view.files...)
		changed.content = make(map[string][]byte, len(view.content))
		for uri, contents := range view.content {
			changed.content[uri] = append([]byte(nil), contents...)
		}
		modURI := pathToUri(filepath.Join(view.root, "app", "go.mod"))
		modBytes := bytes.Replace(changed.content[modURI], []byte("example.com/app"), []byte("example.com/zpp"), 1)
		if bytes.Equal(modBytes, changed.content[modURI]) {
			t.Fatal("test fixture did not change go.mod")
		}
		changed.content[modURI] = modBytes
		for i := range changed.files {
			if changed.files[i].URI != modURI {
				continue
			}
			digest := sha256.Sum256(modBytes)
			changed.files[i].Size = int64(len(modBytes))
			changed.files[i].SHA256 = identity.ContentHash("sha256:" + hex.EncodeToString(digest[:]))
		}
		if _, err := RebuildVerifiedPlannerRequest(context.Background(), &changed, view.rootURI, attestations); err == nil {
			t.Fatal("current go.mod drift was accepted")
		}
	})

	t.Run("environment drift", func(t *testing.T) {
		view, attestations := goVerifiedPlannerFixture(t)
		t.Setenv("GOFLAGS", "-tags=changed")
		if _, err := RebuildVerifiedPlannerRequest(context.Background(), view, view.rootURI, attestations); err == nil {
			t.Fatal("current Go environment drift was accepted")
		}
	})

	t.Run("pinned tool bytes drift", func(t *testing.T) {
		view, attestations := goVerifiedPlannerFixture(t)
		if err := os.WriteFile(attestations[0].Tools[0].Path, []byte("changed non-executable tool bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := RebuildVerifiedPlannerRequest(context.Background(), view, view.rootURI, attestations); err == nil {
			t.Fatal("pinned Go tool hash drift was accepted")
		}
	})
}

func goVerifiedPlannerFixture(t *testing.T) (*goIndexTestView, []model.Provenance) {
	t.Helper()
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOWORK", "auto")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	t.Setenv("GOENV", "off")
	t.Setenv("GOTOOLCHAIN", "local")
	view := newGoIndexTestView(t)
	request, err := New(view.root).BuildIndexRequest(context.Background(), view, view.rootURI)
	if err != nil {
		t.Fatalf("BuildIndexRequest fixture: %v", err)
	}
	toolPath := filepath.Join(t.TempDir(), "not-an-executable-go.exe")
	toolBytes := []byte("fixture is deliberately not an executable Go command")
	if err := os.WriteFile(toolPath, toolBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(toolBytes)
	tool := request.Provenance[request.Scopes[0].ID].Tools[0]
	tool.Path = toolPath
	tool.SHA256 = hex.EncodeToString(digest[:])
	attestations := make([]model.Provenance, 0, len(request.Scopes))
	for _, originalScope := range request.Scopes {
		scope := originalScope
		provenance := request.Provenance[scope.ID]
		provenance.Tools = []model.ToolIdentity{tool}
		scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
		provenance.Scope = scope
		attestations = append(attestations, provenance)
	}
	return view, attestations
}

func TestGoCgoCompilerFactsAreNotReportedComplete(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "cgo.go", `package sample
/* #include "abi.h" */
import "C"
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	result := &loadResult{
		scope:   model.Scope{Build: model.BuildInputs{Environment: map[string]string{"CGO_ENABLED": "1"}}},
		partial: make(map[model.FactKind]string),
	}
	result.markUnpinnedCgoInputs(&packages.Package{Syntax: []*ast.File{file}})

	for _, fact := range []model.FactKind{
		model.FactSymbol, model.FactDeclaration, model.FactDefinition, model.FactReference,
		model.FactImplementation, model.FactTypeRelation, model.FactCall, model.FactImport,
		model.FactGenerated,
	} {
		if got := result.coverage()[fact]; got.state != model.IncompleteKnownSubset || got.reason == "" {
			t.Errorf("cgo fact %s coverage = %+v, want incomplete with a reason", fact, got)
		}
	}
	if got := result.coverage()[model.FactInclude]; got.state != model.Unavailable {
		t.Errorf("cgo include coverage = %+v, want unavailable", got)
	}
	if got := result.coverage()[model.FactModule]; got.state != model.Complete {
		t.Errorf("module coverage = %+v, want complete because cgo does not change module graph", got)
	}
}

func assertUniqueGoSymbolIDs(t *testing.T, symbols []model.Symbol) {
	t.Helper()
	seen := make(map[identity.SymbolID]string)
	for _, symbol := range symbols {
		if prior, exists := seen[symbol.ID]; exists {
			t.Fatalf("symbol ID %q is shared across scopes %q and %q", symbol.ID, prior, symbol.ScopeID)
		}
		seen[symbol.ID] = symbol.ScopeID
	}
}

func findGoObjectID(t *testing.T, symbols []model.Symbol, scopeID, name, kind string) identity.SymbolID {
	t.Helper()
	for _, symbol := range symbols {
		if symbol.ScopeID == scopeID && symbol.Name == name && symbol.Kind == kind {
			return symbol.ID
		}
	}
	t.Fatalf("no %s symbol named %q in scope %q", kind, name, scopeID)
	return ""
}

func occurrencesForID(occurrences []model.Occurrence, id identity.SymbolID, role string) []model.Occurrence {
	var result []model.Occurrence
	for _, occurrence := range occurrences {
		if occurrence.SymbolID == id && (role == "" || occurrence.Role == role) {
			result = append(result, occurrence)
		}
	}
	return result
}

func occurrencesForIDAtPath(occurrences []model.Occurrence, id identity.SymbolID, role, path string) []model.Occurrence {
	result := occurrencesForID(occurrences, id, role)
	uri := pathToUri(path)
	filtered := result[:0]
	for _, occurrence := range result {
		if occurrence.URI == uri {
			filtered = append(filtered, occurrence)
		}
	}
	return filtered
}

func hasGoEdge(edges []model.Edge, scopeID string, from, to identity.SymbolID, kind model.EdgeKind) bool {
	for _, edge := range edges {
		if edge.ScopeID == scopeID && edge.From == from && edge.To == to && edge.Kind == kind {
			return true
		}
	}
	return false
}

func hasGoEdgeNamed(t *testing.T, sink *goIndexTestSink, scopeID, fromName, toName string, kind model.EdgeKind) bool {
	t.Helper()
	fromID := findGoObjectID(t, sink.symbols, scopeID, fromName, "function")
	for _, edge := range sink.edges {
		if edge.ScopeID != scopeID || edge.From != fromID || edge.Kind != kind {
			continue
		}
		for _, symbol := range sink.symbols {
			if symbol.ScopeID == scopeID && symbol.ID == edge.To && symbol.Name == toName {
				return true
			}
		}
	}
	return false
}

func hasGoEdgeSuffix(edges []model.Edge, scopeID, fromSuffix, toSuffix string, kind model.EdgeKind) bool {
	for _, edge := range edges {
		if edge.ScopeID != scopeID || edge.Kind != kind || !strings.Contains(string(edge.From), fromSuffix) {
			continue
		}
		if toSuffix == "" || strings.Contains(string(edge.To), toSuffix) {
			return true
		}
	}
	return false
}

func hasGoEdgeTargetSuffix(edges []model.Edge, scopeID, toSuffix string, kind model.EdgeKind) bool {
	for _, edge := range edges {
		if edge.ScopeID == scopeID && edge.Kind == kind && strings.Contains(string(edge.To), toSuffix) {
			return true
		}
	}
	return false
}

func coverageFor(coverage []model.Coverage, scopeID string, fact model.FactKind) model.Coverage {
	for _, item := range coverage {
		if item.ScopeID == scopeID && item.Fact == fact {
			return item
		}
	}
	return model.Coverage{ScopeID: scopeID, Fact: fact}
}

type goLockedTool struct {
	Observed map[string]string `json:"observed"`
}

type goplsLocation struct {
	URI   string
	Range model.Position
}

func lookupLockedGopls(t *testing.T) (string, bool) {
	t.Helper()
	path, err := exec.LookPath("gopls")
	if err != nil {
		return "", false
	}
	root := findGoRepositoryRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "test", "acceptance", "tools", "tools.lock.json"))
	if err != nil {
		t.Fatalf("read locked tool manifest: %v", err)
	}
	var lock goLockedTool
	if err := json.Unmarshal(data, &lock); err != nil {
		t.Fatalf("decode locked tool manifest: %v", err)
	}
	locked := lock.Observed["gopls"]
	if locked == "" {
		t.Fatal("tools.lock.json has no gopls pin")
	}
	output, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("query gopls version: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), locked) {
		t.Fatalf("installed gopls does not match tools.lock.json: got %q, want %q", strings.TrimSpace(string(output)), locked)
	}
	return path, true
}

func findGoRepositoryRoot(t *testing.T) string {
	t.Helper()
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatal("could not locate repository go.mod")
		}
		current = parent
	}
}

func runLockedGoplsQuery(t *testing.T, goplsPath, root string, scope model.Scope, command, file string, content []byte, needle string, which int) (string, []goplsLocation, error) {
	t.Helper()
	position, err := goCLIPosition(file, content, needle, which)
	if err != nil {
		return "", nil, err
	}
	args := []string{command}
	if command == "definition" {
		args = append(args, "-json")
	}
	if command == "references" {
		args = append(args, "-d")
	}
	args = append(args, position)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cacheRoot, err := os.MkdirTemp("", "omnilsp-gopls-cache-*")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(cacheRoot)
	process := exec.CommandContext(ctx, goplsPath, args...)
	process.Dir = root
	process.Env = goplsEnvironment(scope, root, cacheRoot)
	output, runErr := process.CombinedOutput()
	return string(output), parseGoplsLocations(output), runErr
}

func goCLIPosition(file string, content []byte, needle string, occurrence int) (string, error) {
	searchFrom := 0
	var offset int
	for i := 0; i <= occurrence; i++ {
		relative := bytes.Index(content[searchFrom:], []byte(needle))
		if relative < 0 {
			return "", fmt.Errorf("%q occurrence %d was not found in %s", needle, occurrence, file)
		}
		offset = searchFrom + relative
		searchFrom = offset + len(needle)
	}
	line := bytes.Count(content[:offset], []byte("\n")) + 1
	lineStart := bytes.LastIndexByte(content[:offset], '\n') + 1
	column := offset - lineStart + 1
	return fmt.Sprintf("%s:%d:%d", file, line, column), nil
}

func goplsEnvironment(scope model.Scope, root, cacheRoot string) []string {
	environment := append([]string(nil), os.Environ()...)
	values := cloneStringMap(scope.Build.Environment)
	values["GOWORK"] = filepath.Join(root, "go.work")
	values["GOENV"] = "off"
	values["GOTOOLCHAIN"] = "local"
	values["GOPROXY"] = "off"
	values["GOSUMDB"] = "off"
	values["GOTELEMETRY"] = "off"
	values["GOCACHE"] = filepath.Join(cacheRoot, "gocache")
	values["LOCALAPPDATA"] = filepath.Join(cacheRoot, "local")
	values["APPDATA"] = filepath.Join(cacheRoot, "roaming")
	values["USERPROFILE"] = cacheRoot
	values["HOME"] = cacheRoot
	values["XDG_CACHE_HOME"] = filepath.Join(cacheRoot, "xdg-cache")
	goFlags := strings.TrimSpace(values["GOFLAGS"])
	if len(scope.Build.Features) > 0 {
		goFlags += " -tags=" + strings.Join(scope.Build.Features, ",")
	}
	goFlags += " -mod=" + scope.Build.Options["go.mod"]
	values["GOFLAGS"] = strings.TrimSpace(goFlags)
	for key, value := range values {
		prefix := strings.ToUpper(key) + "="
		filtered := environment[:0]
		for _, entry := range environment {
			if !strings.HasPrefix(strings.ToUpper(entry), prefix) {
				filtered = append(filtered, entry)
			}
		}
		environment = append(filtered, key+"="+value)
	}
	return environment
}

func parseGoplsLocations(data []byte) []goplsLocation {
	var root any
	if json.Unmarshal(data, &root) != nil {
		return parseGoplsTextLocations(data)
	}
	var locations []goplsLocation
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case []any:
			for _, child := range item {
				walk(child)
			}
		case map[string]any:
			uri := stringField(item, "uri", "URI", "targetUri", "targetURI")
			span, _ := field(item, "span", "Span")
			if spanObject, ok := span.(map[string]any); ok {
				if parsed, ok := parseGoplsCLISpan(spanObject); ok {
					locations = append(locations, parsed)
				}
			}
			rangeValue, _ := field(item, "range", "Range", "targetSelectionRange", "targetRange")
			if uri != "" {
				if parsed, ok := parseGoplsRange(rangeValue); ok {
					locations = append(locations, goplsLocation{URI: uri, Range: parsed})
				}
			}
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(root)
	return locations
}

func parseGoplsCLISpan(span map[string]any) (goplsLocation, bool) {
	uri := stringField(span, "uri", "URI")
	start, _ := field(span, "start", "Start")
	end, _ := field(span, "end", "End")
	startObject, startOK := start.(map[string]any)
	endObject, endOK := end.(map[string]any)
	if uri == "" || !startOK || !endOK {
		return goplsLocation{}, false
	}
	line0, okLine0 := numberField(startObject, "line", "Line")
	column0, okColumn0 := numberField(startObject, "column", "Column")
	line1, okLine1 := numberField(endObject, "line", "Line")
	column1, okColumn1 := numberField(endObject, "column", "Column")
	if !okLine0 || !okColumn0 || !okLine1 || !okColumn1 || line0 == 0 || column0 == 0 || line1 == 0 || column1 == 0 {
		return goplsLocation{}, false
	}
	path := uriToPath(uri)
	if path == "" {
		return goplsLocation{}, false
	}
	return goplsLocation{URI: pathToUri(path), Range: model.Position{
		StartLine: line0 - 1, StartChar: column0 - 1,
		EndLine: line1 - 1, EndChar: column1 - 1,
	}}, true
}

func parseGoplsTextLocations(data []byte) []goplsLocation {
	var locations []goplsLocation
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		last := strings.LastIndexByte(line, ':')
		if last < 0 {
			continue
		}
		columnText := line[last+1:]
		beforeColumn := line[:last]
		lineSep := strings.LastIndexByte(beforeColumn, ':')
		if lineSep < 0 {
			continue
		}
		lineNumber, err := strconv.ParseUint(beforeColumn[lineSep+1:], 10, 32)
		if err != nil || lineNumber == 0 {
			continue
		}
		path := beforeColumn[:lineSep]
		if filepath.Ext(uriToPath(path)) != ".go" {
			continue
		}
		columnStartText, columnEndText, hasRange := strings.Cut(columnText, "-")
		columnStart, err := strconv.ParseUint(columnStartText, 10, 32)
		if err != nil || columnStart == 0 {
			continue
		}
		endLine := lineNumber
		endColumnText := columnEndText
		if hasRange && strings.Contains(columnEndText, ":") {
			lineText, columnText, ok := strings.Cut(columnEndText, ":")
			if !ok {
				continue
			}
			parsedLine, parseErr := strconv.ParseUint(lineText, 10, 32)
			if parseErr != nil || parsedLine == 0 {
				continue
			}
			endLine = parsedLine
			endColumnText = columnText
		}
		columnEnd := columnStart
		if hasRange {
			columnEnd, err = strconv.ParseUint(endColumnText, 10, 32)
			if err != nil || columnEnd == 0 {
				continue
			}
		}
		locations = append(locations, goplsLocation{
			URI: pathToUri(path),
			Range: model.Position{
				StartLine: uint32(lineNumber - 1), StartChar: uint32(columnStart - 1),
				EndLine: uint32(endLine - 1), EndChar: uint32(columnEnd - 1),
			},
		})
	}
	return locations
}

func field(object map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		if value, ok := object[key]; ok {
			return value, true
		}
	}
	return nil, false
}

func stringField(object map[string]any, keys ...string) string {
	value, _ := field(object, keys...)
	text, _ := value.(string)
	return text
}

func parseGoplsRange(value any) (model.Position, bool) {
	rangeObject, ok := value.(map[string]any)
	if !ok {
		return model.Position{}, false
	}
	start, _ := field(rangeObject, "start", "Start")
	end, _ := field(rangeObject, "end", "End")
	startObject, startOK := start.(map[string]any)
	endObject, endOK := end.(map[string]any)
	if !startOK || !endOK {
		return model.Position{}, false
	}
	line0, okLine0 := numberField(startObject, "line", "Line")
	char0, okChar0 := numberField(startObject, "character", "Character")
	line1, okLine1 := numberField(endObject, "line", "Line")
	char1, okChar1 := numberField(endObject, "character", "Character")
	return model.Position{StartLine: line0, StartChar: char0, EndLine: line1, EndChar: char1}, okLine0 && okChar0 && okLine1 && okChar1
}

func numberField(object map[string]any, keys ...string) (uint32, bool) {
	value, _ := field(object, keys...)
	number, ok := value.(float64)
	if !ok || number < 0 {
		return 0, false
	}
	return uint32(number), true
}

func assertGoplsLocationsMatch(t *testing.T, locations []goplsLocation, output string, indexed []model.Occurrence, expectedPath string) {
	t.Helper()
	if len(indexed) == 0 {
		t.Fatal("compiler index returned no locations for differential comparison")
	}
	if len(locations) == 0 {
		t.Fatalf("gopls output did not expose locations matching compiler facts:\n%s", output)
	}
	indexedSet := make(map[string]struct{}, len(indexed))
	for _, occurrence := range indexed {
		indexedSet[occurrence.URI+"\x00"+fmt.Sprintf("%d:%d:%d:%d", occurrence.Range.StartLine, occurrence.Range.StartChar, occurrence.Range.EndLine, occurrence.Range.EndChar)] = struct{}{}
	}
	foundExpected := false
	matched := 0
	for _, location := range locations {
		path := uriToPath(location.URI)
		key := pathToUri(path) + "\x00" + fmt.Sprintf("%d:%d:%d:%d", location.Range.StartLine, location.Range.StartChar, location.Range.EndLine, location.Range.EndChar)
		if _, ok := indexedSet[key]; ok {
			matched++
			if expectedPath == "" || sameExecutablePath(path, expectedPath) {
				foundExpected = true
			}
		}
	}
	if !foundExpected {
		t.Fatalf("gopls locations did not match indexed URI/ranges for %s:\noutput=%s\nlocations=%+v\nindexed=%+v", expectedPath, output, locations, indexed)
	}
	if matched != len(locations) || matched != len(indexed) {
		t.Fatalf("gopls location set differs from compiler-index set: matched=%d gopls=%d indexed=%d\noutput=%s\nlocations=%+v\nindexed=%+v", matched, len(locations), len(indexed), output, locations, indexed)
	}
}
