// External test package: it drives typed multi-language semantic generations
// through the persistent store. The typed facts are produced by the interop
// SCIP import adapter (rust/cpp/python documents), and both interop and
// semantic import persistent, so only a _test package may exercise the full
// write/read path of the §L5 storage boundary at once.
package persistent_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/interop"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
	"github.com/scip-code/scip/bindings/go/scip"
	"google.golang.org/protobuf/proto"
)

const multiLangWorkspace = "file:///x5-multilang"

// multiLangView is an immutable model.WorkspaceView over three per-language
// source files. Walk honours the requested scope root so every language scope
// captures only its own files, exactly like the server's disk view.
type multiLangView struct {
	identity model.Identity
	files    []model.File
	contents map[string][]byte
}

func (v *multiLangView) Identity() model.Identity { return v.identity }

func (v *multiLangView) Walk(_ context.Context, root string, visit func(model.File) error) error {
	for _, file := range v.files {
		if !strings.HasPrefix(file.URI, root+"/") {
			continue
		}
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func (v *multiLangView) Read(_ context.Context, uri string) (io.ReadCloser, error) {
	if data, ok := v.contents[uri]; ok {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return nil, fmt.Errorf("multilang view: no file %q", uri)
}

// multiLangExpectation carries the per-scope facts a committed generation must
// serve back, keyed by scope ID.
type multiLangExpectation struct {
	names     map[string]string // scopeID → symbol display name
	symbolIDs map[string]string // scopeID → SCIP symbol identity
	edgeFrom  string            // rust implementation edge endpoints
	edgeTo    string
}

// multiLangLanguage describes one non-Go language in the fixture.
type multiLangLanguage struct {
	scopeID     string
	language    string
	root        string
	fileName    string
	toolchain   string
	tool        model.ToolIdentity
	suffix      string // type name suffix, e.g. "Widget"
	traitID     string // SCIP identity of the rust trait ("" elsewhere)
	traitName   string // display name of the trait symbol ("" elsewhere)
	occurrences int    // occurrences the SCIP fixture carries for the scope
}

func multiLangLanguages() []multiLangLanguage {
	return []multiLangLanguage{
		{
			scopeID: "x5-multilang-rust", language: "rust", root: multiLangWorkspace + "/rust",
			fileName: "lib.rs", toolchain: "rustc-1.90",
			tool:        model.ToolIdentity{Name: "rust-analyzer", Path: "C:/tools/rust-analyzer.exe", Version: "2026.09", SHA256: strings.Repeat("a", 64)},
			suffix:      "Widget",
			traitID:     "rust x5-multilang Render.",
			traitName:   "Render",
			occurrences: 3, // definition + reference + trait reference
		},
		{
			scopeID: "x5-multilang-cpp", language: "cpp", root: multiLangWorkspace + "/cpp",
			fileName: "kernel.cpp", toolchain: "clang-21",
			tool:        model.ToolIdentity{Name: "clangd", Path: "C:/tools/clangd.exe", Version: "21.1", SHA256: strings.Repeat("b", 64)},
			suffix:      "Kernel",
			occurrences: 2, // definition + reference
		},
		{
			scopeID: "x5-multilang-python", language: "python", root: multiLangWorkspace + "/python",
			fileName: "app.py", toolchain: "cpython-3.14",
			tool:        model.ToolIdentity{Name: "pyright", Path: "C:/tools/pyright.exe", Version: "1.1.410", SHA256: strings.Repeat("c", 64)},
			suffix:      "Cache",
			occurrences: 2, // definition + reference
		},
	}
}

// multiLangSource renders the fixture source for one language and marker.
func multiLangSource(lang multiLangLanguage, marker string) string {
	typeName := marker + lang.suffix
	switch lang.language {
	case "rust":
		return "trait Render {}\n\nstruct " + typeName + ";\n\nimpl Render for " + typeName + " {}\n"
	case "cpp":
		return "class " + typeName + " {};\n\nint boot(" + typeName + " kernel) {\n    return 0;\n}\n"
	default: // python
		return "class " + typeName + ":\n    pass\n\n\ndef warm():\n    item = " + typeName + "()\n    return item\n"
	}
}

func multiLangSymbolID(lang multiLangLanguage, marker string) string {
	return lang.language + " x5-multilang " + marker + lang.suffix + "."
}

// nthOccurrence locates the (skip+1)-th occurrence of needle and returns its
// line and column. Every fixture source is ASCII, so byte columns equal UTF-16
// columns and the SCIP UTF-16 ranges below need no conversion.
func nthOccurrence(content, needle string, skip int) (line, col int) {
	offset := -1
	for i := 0; i <= skip; i++ {
		next := strings.Index(content[offset+1:], needle)
		if next < 0 {
			panic("multilang fixture: occurrence missing from source")
		}
		offset += next + 1
	}
	prefix := content[:offset]
	line = strings.Count(prefix, "\n")
	col = offset - strings.LastIndex(prefix, "\n") - 1
	return line, col
}

// multiLangSCIPIndex builds one SCIP index for a language scope, following the
// interop test model construction: a protobuf scip.Index with UTF-16 positions
// that interop.ImportSCIPToModel converts into the typed semantic model.
func multiLangSCIPIndex(t *testing.T, lang multiLangLanguage, marker string) []byte {
	t.Helper()
	typeName := marker + lang.suffix
	content := multiLangSource(lang, marker)
	symbolID := multiLangSymbolID(lang, marker)

	type occurrenceSpec struct {
		symbol     string
		needle     string
		definition bool
		instance   int
	}
	occurrences := []occurrenceSpec{
		{symbol: symbolID, needle: typeName, definition: true, instance: 0},
		{symbol: symbolID, needle: typeName, instance: 1},
	}
	if lang.traitID != "" {
		occurrences = append(occurrences, occurrenceSpec{symbol: lang.traitID, needle: "Render", instance: 1})
	}

	doc := &scip.Document{Language: lang.language, RelativePath: lang.fileName}
	for _, spec := range occurrences {
		line, col := nthOccurrence(content, spec.needle, spec.instance)
		role := int32(0)
		if spec.definition {
			role = int32(scip.SymbolRole_Definition)
		}
		doc.Occurrences = append(doc.Occurrences, &scip.Occurrence{
			Symbol:      spec.symbol,
			SymbolRoles: role,
			Range:       []int32{int32(line), int32(col), int32(col + len(spec.needle))},
		})
	}
	defined := &scip.SymbolInformation{Symbol: symbolID, DisplayName: typeName, Kind: scip.SymbolInformation_Class}
	if lang.traitID != "" {
		defined.Relationships = []*scip.Relationship{{Symbol: lang.traitID, IsImplementation: true}}
		doc.Symbols = append(doc.Symbols, defined,
			&scip.SymbolInformation{Symbol: lang.traitID, DisplayName: "Render", Kind: scip.SymbolInformation_Interface})
	} else {
		doc.Symbols = append(doc.Symbols, defined)
	}
	index := &scip.Index{
		Metadata: &scip.Metadata{
			ProjectRoot:          lang.root,
			TextDocumentEncoding: scip.TextEncoding_UTF16,
			ToolInfo:             &scip.ToolInfo{Name: "x5-multilang-fixture", Version: "1"},
		},
		Documents: []*scip.Document{doc},
	}
	data, err := proto.Marshal(index)
	if err != nil {
		t.Fatalf("marshal %s SCIP fixture: %v", lang.language, err)
	}
	return data
}

// newMultiLangFixture builds the shared workspace identity, view, scopes and
// provenance for one generation marker (one per language scope).
func newMultiLangFixture(marker string) (*multiLangView, []model.Scope, map[string]model.Provenance) {
	workspaceIdentity := model.Identity{
		Workspace:  identity.WorkspaceID(multiLangWorkspace),
		DiskDigest: sha256ContentHash([]byte("x5-multilang-disk-" + marker)),
	}
	var files []model.File
	contents := make(map[string][]byte)
	var scopes []model.Scope
	provenance := make(map[string]model.Provenance)
	for _, lang := range multiLangLanguages() {
		content := multiLangSource(lang, marker)
		uri := lang.root + "/" + lang.fileName
		contents[uri] = []byte(content)
		files = append(files, model.File{URI: uri, LanguageID: lang.language, Size: int64(len(content)), SHA256: sha256ContentHash([]byte(content))})
		scope := model.Scope{ID: lang.scopeID, Language: lang.language, RootURI: lang.root, Build: model.BuildInputs{Options: map[string]string{}}}
		scope.BuildContext = model.ComputeBuildContextID(scope, "x5-multilang-extractor", "1", lang.toolchain, []model.ToolIdentity{lang.tool})
		provenance[lang.scopeID] = model.Provenance{
			SchemaVersion: model.SchemaVersion,
			Identity:      workspaceIdentity,
			Scope:         scope,
			Extractor:     "x5-multilang-extractor",
			ExtractorVer:  "1",
			Backend:       identity.BackendID{Language: lang.language, Name: "x5-multilang-backend"},
			Toolchain:     lang.toolchain,
			Tools:         []model.ToolIdentity{lang.tool},
		}
		scopes = append(scopes, scope)
	}
	return &multiLangView{identity: workspaceIdentity, files: files, contents: contents}, scopes, provenance
}

// buildMultiLanguageGeneration commits one typed semantic generation holding
// one scope per language. Facts enter through interop.ImportSCIPToModel — the
// same SCIP→typed-model path the interop tests exercise — so non-Go payloads
// cross the entire persistent boundary unmodified.
func buildMultiLanguageGeneration(t *testing.T, store *persistent.FileStore, marker string) (uint64, multiLangExpectation) {
	t.Helper()
	ctx := context.Background()
	view, scopes, provenance := newMultiLangFixture(marker)
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatalf("begin multilang build: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	sink, err := semantic.NewSink(ctx, build, model.Request{View: view, Scopes: scopes, Provenance: provenance})
	if err != nil {
		t.Fatalf("open multilang semantic sink: %v", err)
	}
	defer func() { _ = sink.Close() }()

	expectation := multiLangExpectation{names: map[string]string{}, symbolIDs: map[string]string{}}
	coverage := make([]model.Coverage, 0, len(scopes)*len(model.RequiredFactKinds))
	usedTools := make(map[string][]model.ToolIdentity, len(scopes))
	for _, lang := range multiLangLanguages() {
		scope := provenance[lang.scopeID].Scope
		data := multiLangSCIPIndex(t, lang, marker)
		report, err := interop.ImportSCIPToModel(ctx, data, model.Request{
			View:       view,
			Scopes:     []model.Scope{scope},
			Provenance: map[string]model.Provenance{lang.scopeID: provenance[lang.scopeID]},
		}, sink)
		if err != nil {
			t.Fatalf("import %s SCIP model: %v", lang.language, err)
		}
		coverage = append(coverage, report.Coverage...)
		usedTools[lang.scopeID] = report.UsedTools[lang.scopeID]
		expectation.names[lang.scopeID] = marker + lang.suffix
		expectation.symbolIDs[lang.scopeID] = multiLangSymbolID(lang, marker)
		if lang.traitID != "" {
			expectation.edgeFrom = multiLangSymbolID(lang, marker)
			expectation.edgeTo = lang.traitID
		}
	}
	report := model.Report{Identity: view.Identity(), Coverage: coverage, UsedTools: usedTools}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatalf("finalize multilang generation: %v", err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatalf("commit multilang generation: %v", err)
	}
	committed = true
	return build.GenerationID(), expectation
}

// expectedSymbolNames returns the sorted display names a committed scope must
// serve back for the given generation marker.
func expectedSymbolNames(lang multiLangLanguage, marker string) []string {
	names := []string{marker + lang.suffix}
	if lang.traitName != "" {
		names = append(names, lang.traitName)
	}
	sort.Strings(names)
	return names
}

// readMultiLangFacts drains a validated semantic reader and groups the typed
// facts by scope ID.
func readMultiLangFacts(t *testing.T, ctx context.Context, view persistent.GenerationView) (names map[string][]string, occurrenceCount map[string]int, definitionCount map[string]int, edges []model.Edge, metadata semantic.Metadata) {
	t.Helper()
	reader, err := semantic.OpenReader(ctx, view)
	if err != nil {
		t.Fatalf("open semantic reader over generation %d: %v", view.ID, err)
	}
	defer reader.Close()
	metadata = reader.Metadata()
	names = map[string][]string{}
	occurrenceCount = map[string]int{}
	definitionCount = map[string]int{}
	for {
		batch, readErr := reader.Next(ctx)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatalf("read generation %d: %v", view.ID, readErr)
		}
		switch batch.Kind {
		case semantic.BatchSymbols:
			for _, symbol := range batch.Symbols {
				names[symbol.ScopeID] = append(names[symbol.ScopeID], symbol.Name)
			}
		case semantic.BatchOccurrences:
			for _, occurrence := range batch.Occurrences {
				occurrenceCount[occurrence.ScopeID]++
				if occurrence.Role == "definition" {
					definitionCount[occurrence.ScopeID]++
				}
			}
		case semantic.BatchEdges:
			edges = append(edges, batch.Edges...)
		}
	}
	for scopeID := range names {
		sort.Strings(names[scopeID])
	}
	return names, occurrenceCount, definitionCount, edges, metadata
}

// TestMultiLanguageTypedSemanticGenerationIsolationAcrossRecommit closes the
// non-Go half of X5-7's multi-language gap: typed semantic generations built
// from rust/cpp/python SCIP payloads persist through the store, round-trip
// intact across a fresh-process reopen, and after a recommit fresh readers
// serve only the new generation's facts while a lease pinned to the old
// generation keeps reading exactly its own facts — never a mix.
func TestMultiLanguageTypedSemanticGenerationIsolationAcrossRecommit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	gen1, first := buildMultiLanguageGeneration(t, store, "Alpha")

	// Fresh-process reopen: a second store instance over the same directory
	// must recover the committed multi-language typed generation.
	reopened, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	recovered, err := reopened.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("reopened store lost multi-language generation %d: %v", gen1, err)
	}
	if recovered.ID != gen1 {
		t.Fatalf("reopened store serves generation %d, want the committed %d", recovered.ID, gen1)
	}
	names, occurrences, definitions, edges, metadata := readMultiLangFacts(t, ctx, recovered)
	if len(metadata.Scopes) != 3 {
		t.Fatalf("recovered generation carries %d scopes, want one per language", len(metadata.Scopes))
	}
	languages := make(map[string]string, len(metadata.Scopes))
	for _, scope := range metadata.Scopes {
		languages[scope.ID] = scope.Language
	}
	for _, lang := range multiLangLanguages() {
		if languages[lang.scopeID] != lang.language {
			t.Fatalf("scope %s language = %q, want %q", lang.scopeID, languages[lang.scopeID], lang.language)
		}
		if got := names[lang.scopeID]; !slices.Equal(got, expectedSymbolNames(lang, "Alpha")) {
			t.Fatalf("recovered %s symbols = %v, want %v", lang.language, got, expectedSymbolNames(lang, "Alpha"))
		}
		if occurrences[lang.scopeID] != lang.occurrences || definitions[lang.scopeID] != 1 {
			t.Fatalf("recovered %s occurrences = %d (%d definitions), want %d with one definition",
				lang.language, occurrences[lang.scopeID], definitions[lang.scopeID], lang.occurrences)
		}
	}
	if len(edges) != 1 || edges[0].Kind != model.EdgeImplementation ||
		edges[0].From != identity.SymbolID(first.edgeFrom) || edges[0].To != identity.SymbolID(first.edgeTo) ||
		edges[0].ScopeID != "x5-multilang-rust" {
		t.Fatalf("recovered rust implementation edge = %+v, want %s -> %s", edges, first.edgeFrom, first.edgeTo)
	}

	// Pin the recovered generation with a lease, then commit a second
	// multi-language generation from yet another store instance.
	lease, err := reopened.OpenSnapshotLease(ctx)
	if err != nil {
		t.Fatalf("pin generation %d: %v", gen1, err)
	}
	defer lease.Close()
	pinned := lease.Snapshot()
	if pinned.ID != gen1 {
		t.Fatalf("lease pinned generation %d, want %d", pinned.ID, gen1)
	}
	committer, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatalf("open committing store: %v", err)
	}
	gen2, second := buildMultiLanguageGeneration(t, committer, "Beta")
	if gen2 <= gen1 {
		t.Fatalf("generation IDs did not advance: gen1=%d gen2=%d", gen1, gen2)
	}

	// Fresh readers must see only the new generation's typed facts for every
	// language (fresh-only), never a mix with the superseded generation.
	current, err := reopened.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("open current generation: %v", err)
	}
	if current.ID != gen2 {
		t.Fatalf("fresh reader serves generation %d, want the committed %d", current.ID, gen2)
	}
	currentNames, currentOccurrences, _, currentEdges, _ := readMultiLangFacts(t, ctx, current)
	for _, lang := range multiLangLanguages() {
		if got := currentNames[lang.scopeID]; !slices.Equal(got, expectedSymbolNames(lang, "Beta")) {
			t.Fatalf("fresh reader %s facts after recommit = %v, want only %v", lang.language, got, expectedSymbolNames(lang, "Beta"))
		}
		if currentOccurrences[lang.scopeID] != lang.occurrences {
			t.Fatalf("fresh reader %s occurrence count = %d, want %d", lang.language, currentOccurrences[lang.scopeID], lang.occurrences)
		}
	}
	if len(currentEdges) != 1 || currentEdges[0].From != identity.SymbolID(second.edgeFrom) {
		t.Fatalf("fresh reader edges after recommit = %+v, want only the %s implementation edge", currentEdges, second.edgeFrom)
	}

	// The pinned reader must keep serving exactly its own generation's facts.
	pinnedNames, _, _, pinnedEdges, _ := readMultiLangFacts(t, ctx, pinned)
	for _, lang := range multiLangLanguages() {
		if got := pinnedNames[lang.scopeID]; !slices.Equal(got, expectedSymbolNames(lang, "Alpha")) {
			t.Fatalf("pinned reader %s facts after recommit = %v, want unchanged %v", lang.language, got, expectedSymbolNames(lang, "Alpha"))
		}
	}
	if len(pinnedEdges) != 1 || pinnedEdges[0].From != identity.SymbolID(first.edgeFrom) {
		t.Fatalf("pinned reader edges after recommit = %+v, want unchanged %s edge", pinnedEdges, first.edgeFrom)
	}
}
