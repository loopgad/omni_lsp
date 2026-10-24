// External test package: it round-trips typed semantic generations through
// the persistent store. semantic imports persistent, so only a _test package
// may exercise both sides of the §L5 storage boundary at once.
package persistent_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
)

const isolationRootURI = "file:///x5-isolation"

// isolationView is a minimal immutable model.WorkspaceView over two in-memory
// source files, shaped exactly like the views the server captures on disk.
type isolationView struct {
	identity model.Identity
	files    []model.File
	contents map[string][]byte
}

func (v *isolationView) Identity() model.Identity { return v.identity }

func (v *isolationView) Walk(_ context.Context, _ string, visit func(model.File) error) error {
	for _, file := range v.files {
		if err := visit(file); err != nil {
			return err
		}
	}
	return nil
}

func (v *isolationView) Read(_ context.Context, uri string) (io.ReadCloser, error) {
	if data, ok := v.contents[uri]; ok {
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return nil, fmt.Errorf("isolation view: no file %q", uri)
}

func sha256ContentHash(data []byte) identity.ContentHash {
	sum := sha256.Sum256(data)
	return identity.ContentHash("sha256:" + hex.EncodeToString(sum[:]))
}

// newIsolationView builds the per-generation fixture: main.go defines the
// named symbol, use.go references it. Only the symbol name changes between
// generations, so a reader that serves superseded facts is detectable by name.
func newIsolationView(symbolName string) *isolationView {
	mainContent := []byte("package iso\n\nfunc " + symbolName + "() {}\n")
	useContent := []byte("package iso\n\nfunc Caller() { " + symbolName + "() }\n")
	return &isolationView{
		identity: model.Identity{
			Workspace:  identity.WorkspaceID(isolationRootURI),
			DiskDigest: sha256ContentHash([]byte("x5-isolation-disk")),
		},
		files: []model.File{
			{URI: isolationRootURI + "/main.go", LanguageID: "go", Size: int64(len(mainContent)), SHA256: sha256ContentHash(mainContent)},
			{URI: isolationRootURI + "/use.go", LanguageID: "go", Size: int64(len(useContent)), SHA256: sha256ContentHash(useContent)},
		},
		contents: map[string][]byte{
			isolationRootURI + "/main.go": mainContent,
			isolationRootURI + "/use.go":  useContent,
		},
	}
}

// commitTypedGeneration writes one complete typed semantic generation
// (symbols + occurrences + metadata) through semantic.NewSink and commits it.
func commitTypedGeneration(t *testing.T, store *persistent.FileStore, symbolName string) (uint64, *isolationView) {
	t.Helper()
	ctx := context.Background()
	view := newIsolationView(symbolName)
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatalf("begin build for %s: %v", symbolName, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	scope := model.Scope{ID: "x5-isolation-go", Language: "go", RootURI: isolationRootURI, Build: model.BuildInputs{Options: map[string]string{}}}
	scope.BuildContext = model.ComputeBuildContextID(scope, "x5-isolation-extractor", "1", "x5-isolation-toolchain", nil)
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion,
		Identity:      view.Identity(),
		Scope:         scope,
		Extractor:     "x5-isolation-extractor",
		ExtractorVer:  "1",
		Backend:       identity.BackendID{Language: "go", Name: "x5-isolation-backend"},
		Toolchain:     "x5-isolation-toolchain",
	}
	request := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	sink, err := semantic.NewSink(ctx, build, request)
	if err != nil {
		t.Fatalf("open semantic sink for %s: %v", symbolName, err)
	}
	defer func() { _ = sink.Close() }()
	symbolID := identity.SymbolID("go:x5-isolation." + symbolName)
	mainURI := isolationRootURI + "/main.go"
	useURI := isolationRootURI + "/use.go"
	mainLine, mainCol := lineCharOf(view.contents[mainURI], symbolName)
	useLine, useCol := lineCharOf(view.contents[useURI], symbolName)
	if err := sink.WriteSymbols(ctx, []model.Symbol{{ID: symbolID, ScopeID: scope.ID, Name: symbolName, Kind: "function"}}); err != nil {
		t.Fatalf("write symbols for %s: %v", symbolName, err)
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{
		{SymbolID: symbolID, ScopeID: scope.ID, URI: mainURI,
			Range: model.Position{StartLine: uint32(mainLine), StartChar: uint32(mainCol), EndLine: uint32(mainLine), EndChar: uint32(mainCol + len(symbolName))},
			Role:  "definition", SourceHash: view.files[0].SHA256, BuildContext: scope.BuildContext},
		{SymbolID: symbolID, ScopeID: scope.ID, URI: useURI,
			Range: model.Position{StartLine: uint32(useLine), StartChar: uint32(useCol), EndLine: uint32(useLine), EndChar: uint32(useCol + len(symbolName))},
			Role:  "reference", SourceHash: view.files[1].SHA256, BuildContext: scope.BuildContext},
	}); err != nil {
		t.Fatalf("write occurrences for %s: %v", symbolName, err)
	}
	report := model.Report{Identity: view.Identity(), UsedTools: map[string][]model.ToolIdentity{}}
	for _, fact := range model.RequiredFactKinds {
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: model.Complete})
	}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatalf("finalize generation for %s: %v", symbolName, err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatalf("commit generation for %s: %v", symbolName, err)
	}
	committed = true
	return build.GenerationID(), view
}

func lineCharOf(content []byte, needle string) (line, col int) {
	offset := strings.Index(string(content), needle)
	if offset < 0 {
		panic("isolation fixture: symbol missing from source")
	}
	line = strings.Count(string(content[:offset]), "\n")
	col = offset - strings.LastIndex(string(content[:offset]), "\n") - 1
	return line, col
}

// readGenerationFacts opens a validated semantic reader over the view and
// collects every symbol name, occurrence role and the generation metadata.
func readGenerationFacts(t *testing.T, ctx context.Context, view persistent.GenerationView) ([]string, map[string]int, semantic.Metadata) {
	t.Helper()
	reader, err := semantic.OpenReader(ctx, view)
	if err != nil {
		t.Fatalf("open semantic reader over generation %d: %v", view.ID, err)
	}
	defer reader.Close()
	metadata := reader.Metadata()
	var names []string
	roles := make(map[string]int)
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
				names = append(names, symbol.Name)
			}
		case semantic.BatchOccurrences:
			for _, occurrence := range batch.Occurrences {
				roles[occurrence.Role]++
			}
		}
	}
	sort.Strings(names)
	return names, roles, metadata
}

// TestTypedSemanticGenerationIsolationAcrossRecommit closes the persistent
// half of X5-7: a typed semantic generation committed by one store instance
// round-trips intact through a fresh store instance (fresh-process reopen),
// and after a second generation is committed on top, fresh readers serve only
// the new generation's facts while a lease pinned to the old generation keeps
// reading exactly its own facts — never a mix.
func TestTypedSemanticGenerationIsolationAcrossRecommit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	gen1, _ := commitTypedGeneration(t, store, "IsolatedOne")

	// Fresh-process reopen: a second store instance over the same directory
	// must recover the committed typed generation, byte-for-byte facts.
	reopened, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	recovered, err := reopened.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("reopened store lost generation %d: %v", gen1, err)
	}
	if recovered.ID != gen1 {
		t.Fatalf("reopened store serves generation %d, want the committed %d", recovered.ID, gen1)
	}
	names, roles, metadata := readGenerationFacts(t, ctx, recovered)
	if len(names) != 1 || names[0] != "IsolatedOne" {
		t.Fatalf("recovered typed facts = %v, want the committed [IsolatedOne]", names)
	}
	if roles["definition"] != 1 || roles["reference"] != 1 {
		t.Fatalf("recovered occurrence roles = %v, want one definition and one reference", roles)
	}
	if metadata.SchemaVersion != semantic.SchemaVersion || metadata.Identity.Workspace != identity.WorkspaceID(isolationRootURI) {
		t.Fatalf("recovered metadata identity = %+v, want schema %d workspace %q", metadata.Identity, semantic.SchemaVersion, isolationRootURI)
	}

	// Pin the recovered generation with a lease, then commit a second
	// generation from yet another store instance (another process).
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
	gen2, _ := commitTypedGeneration(t, committer, "IsolatedTwo")
	if gen2 <= gen1 {
		t.Fatalf("generation IDs did not advance: gen1=%d gen2=%d", gen1, gen2)
	}

	// Fresh readers must see only the new generation's typed facts.
	current, err := reopened.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("open current generation: %v", err)
	}
	if current.ID != gen2 {
		t.Fatalf("fresh reader serves generation %d, want the committed %d", current.ID, gen2)
	}
	currentNames, _, _ := readGenerationFacts(t, ctx, current)
	if len(currentNames) != 1 || currentNames[0] != "IsolatedTwo" {
		t.Fatalf("fresh reader facts after recommit = %v, want only [IsolatedTwo]", currentNames)
	}

	// The pinned reader must keep serving exactly its own generation's facts.
	pinnedNames, pinnedRoles, _ := readGenerationFacts(t, ctx, pinned)
	if len(pinnedNames) != 1 || pinnedNames[0] != "IsolatedOne" {
		t.Fatalf("pinned reader facts after recommit = %v, want unchanged [IsolatedOne]", pinnedNames)
	}
	if pinnedRoles["definition"] != 1 || pinnedRoles["reference"] != 1 {
		t.Fatalf("pinned reader roles after recommit = %v, want one definition and one reference", pinnedRoles)
	}
}
