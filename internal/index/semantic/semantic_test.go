package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
)

type testWorkspaceView struct {
	id    model.Identity
	files []model.File
}

func (v testWorkspaceView) Identity() model.Identity { return v.id }

func (v testWorkspaceView) Walk(ctx context.Context, root string, visit func(model.File) error) error {
	for _, file := range v.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if uriWithinRoot(root, file.URI) {
			if err := visit(file); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v testWorkspaceView) Read(ctx context.Context, uri string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, file := range v.files {
		if file.URI == uri {
			return io.NopCloser(strings.NewReader("source")), nil
		}
	}
	return nil, errors.New("file missing")
}

func TestRoundTripAfterReopen(t *testing.T) {
	ctx := context.Background()
	store, root, req, report, sourceHash := fixture(t)
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = build.Abort()
		}
	}()
	sink, err := NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	// References and edges arrive before their symbols, as they may in a
	// multi-provider extraction stream.
	occurrence := model.Occurrence{
		SymbolID: "go:pkg.Func", ScopeID: req.Scopes[0].ID, URI: "file:///repo/main.go",
		Range: model.Position{StartLine: 1, StartChar: 0, EndLine: 1, EndChar: 4}, Role: "reference",
		SourceHash: sourceHash, BuildContext: req.Scopes[0].BuildContext,
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{occurrence}); err != nil {
		t.Fatal(err)
	}
	edge := model.Edge{
		From: "go:pkg.Func", To: "go:pkg.Type", ScopeID: req.Scopes[0].ID, Kind: model.EdgeTypeRelation,
		SourceURI: "file:///repo/main.go", Range: occurrence.Range, SourceHash: sourceHash,
		BuildContext: req.Scopes[0].BuildContext,
	}
	if err := sink.WriteEdges(ctx, []model.Edge{edge}); err != nil {
		t.Fatal(err)
	}
	symbols := []model.Symbol{
		{ID: "go:pkg.Func", ScopeID: req.Scopes[0].ID, Name: "Func", Kind: "function"},
		{ID: "go:pkg.Type", ScopeID: req.Scopes[0].ID, Name: "Type", Kind: "type"},
	}
	if err := sink.WriteSymbols(ctx, symbols); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatal(err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	committed = true

	reopened, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	view, err := reopened.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(ctx, view)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	metadata := reader.Metadata()
	if metadata.Identity.Workspace != req.View.Identity().Workspace || metadata.Identity.DiskDigest != req.View.Identity().DiskDigest {
		t.Fatalf("metadata identity mismatch: %+v", metadata.Identity)
	}
	if metadata.Identity.SnapshotRev != 0 {
		t.Fatalf("persisted snapshot revision must be zero, got %d", metadata.Identity.SnapshotRev)
	}
	if metadata.Provenance[req.Scopes[0].ID].Identity.SnapshotRev != 0 {
		t.Fatal("persisted provenance retained process-local snapshot revision")
	}
	seen := map[BatchKind]int{}
	for {
		batch, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[batch.Kind]++
		if batch.Kind == BatchOccurrences && (len(batch.Occurrences) != 1 || batch.Occurrences[0].SymbolID != occurrence.SymbolID) {
			t.Fatalf("unexpected occurrence batch: %+v", batch)
		}
	}
	if seen[BatchSymbols] != 1 || seen[BatchOccurrences] != 1 || seen[BatchEdges] != 1 {
		t.Fatalf("unexpected batch counts: %#v", seen)
	}
	if err := reader.Rewind(); err != nil {
		t.Fatalf("rewind validated reader: %v", err)
	}
	seenAgain := map[BatchKind]int{}
	for {
		batch, err := reader.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seenAgain[batch.Kind]++
	}
	if seenAgain[BatchSymbols] != seen[BatchSymbols] || seenAgain[BatchOccurrences] != seen[BatchOccurrences] || seenAgain[BatchEdges] != seen[BatchEdges] {
		t.Fatalf("rewound batch counts = %#v, first pass = %#v", seenAgain, seen)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Rewind(); err == nil {
		t.Fatal("rewind closed reader succeeded")
	}
}

func TestUnauditedScopeWithoutFactsRoundTrips(t *testing.T) {
	for _, state := range []model.Completeness{model.Unknown, model.Unavailable} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			store, root, req, report, _ := fixture(t)
			addPinnedTool(&req)
			setCoverageState(&report, state)

			build, err := store.BeginBuild(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer build.Abort()
			sink, err := NewSink(ctx, build, req)
			if err != nil {
				t.Fatal(err)
			}
			defer sink.Close()
			if err := sink.Finalize(ctx, report); err != nil {
				t.Fatalf("Finalize empty %s scope: %v", state, err)
			}
			if err := build.Commit(ctx); err != nil {
				t.Fatal(err)
			}

			reopened, err := persistent.NewFileStore(root, persistent.Config{})
			if err != nil {
				t.Fatal(err)
			}
			view, err := reopened.OpenSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			reader, err := OpenReader(ctx, view)
			if err != nil {
				t.Fatalf("open persisted empty %s scope: %v", state, err)
			}
			defer reader.Close()
			for _, item := range reader.Metadata().Coverage {
				if item.State != state || item.Reason == "" {
					t.Fatalf("persisted coverage = %+v, want %s with reason", item, state)
				}
			}
			if _, err := reader.Next(ctx); !errors.Is(err, io.EOF) {
				t.Fatalf("Next on empty scope error = %v, want EOF", err)
			}
		})
	}
}

func TestUnauditedFactsRequirePinnedToolUsage(t *testing.T) {
	for _, state := range []model.Completeness{model.Unknown, model.Unavailable} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			store, _, req, report, _ := fixture(t)
			addPinnedTool(&req)
			setCoverageState(&report, state)
			build, err := store.BeginBuild(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer build.Abort()
			sink, err := NewSink(ctx, build, req)
			if err != nil {
				t.Fatal(err)
			}
			defer sink.Close()
			if err := sink.WriteSymbols(ctx, []model.Symbol{{ID: "emitted", ScopeID: req.Scopes[0].ID, Name: "Emitted"}}); err != nil {
				t.Fatal(err)
			}
			if err := sink.Finalize(ctx, report); !errors.Is(err, model.ErrInvalidProvenance) {
				t.Fatalf("Finalize with facts and no pinned tool usage = %v, want provenance error", err)
			}
		})
	}
}

func TestReaderRejectsUnauditedFactsWithoutPinnedToolUsage(t *testing.T) {
	for _, state := range []model.Completeness{model.Unknown, model.Unavailable} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			store, _, req, report, _ := fixture(t)
			addPinnedTool(&req)
			setCoverageState(&report, state)

			build, err := store.BeginBuild(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer build.Abort()
			record, err := json.Marshal(model.Symbol{ID: "emitted", ScopeID: req.Scopes[0].ID, Name: "Emitted"})
			if err != nil {
				t.Fatal(err)
			}
			batch, err := encodeBatch(BatchSymbols, []json.RawMessage{record})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := build.WriteSegment(batch); err != nil {
				t.Fatal(err)
			}
			meta := Metadata{
				SchemaVersion: SchemaVersion,
				Identity:      durableIdentity(req.View.Identity()),
				DiskDigest:    req.View.Identity().DiskDigest,
				Scopes:        cloneScopes(req.Scopes),
				Provenance:    cloneProvenance(req.Provenance),
				Coverage:      append([]model.Coverage(nil), report.Coverage...),
			}
			for scopeID, provenance := range meta.Provenance {
				provenance.Identity = durableIdentity(provenance.Identity)
				meta.Provenance[scopeID] = provenance
			}
			metadata, err := encodeMetadata(meta)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := build.WriteSegment(metadata); err != nil {
				t.Fatal(err)
			}
			if err := build.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			view, err := store.OpenSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenReader(ctx, view); !errors.Is(err, ErrMalformedPayload) || !strings.Contains(err.Error(), model.ErrInvalidProvenance.Error()) {
				t.Fatalf("OpenReader with unaudited facts = %v, want malformed payload with provenance cause", err)
			}
		})
	}
}

func TestBatchAndRecordBounds(t *testing.T) {
	ctx := context.Background()
	store, _, req, report, _ := fixture(t)
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	tooLarge := model.Symbol{ID: "large", ScopeID: req.Scopes[0].ID, Name: strings.Repeat("x", MaxRecordBytes)}
	if err := sink.WriteSymbols(ctx, []model.Symbol{tooLarge}); !errors.Is(err, ErrBounds) {
		t.Fatalf("oversized record error = %v, want ErrBounds", err)
	}
	_ = sink.Close()
	_ = build.Abort()

	build, err = store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err = NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	const symbolCount = 1800
	name := strings.Repeat("x", 5<<10)
	symbols := make([]model.Symbol, symbolCount)
	for i := range symbols {
		symbols[i] = model.Symbol{ID: identity.SymbolID(fmt.Sprintf("symbol-%05d", i)), ScopeID: req.Scopes[0].ID, Name: name}
	}
	if err := sink.WriteSymbols(ctx, symbols); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatal(err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := store.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dataSegments := 0
	for _, segment := range view.Segments {
		payload, err := view.ReadSegment(segment.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(payload) > MaxBatchBytes {
			t.Fatalf("payload length %d exceeds batch limit %d", len(payload), MaxBatchBytes)
		}
		wire, err := decodeWire(payload, segment.ID)
		if err != nil {
			t.Fatal(err)
		}
		if wire.Kind != "metadata" {
			dataSegments++
		}
	}
	if dataSegments < 2 {
		t.Fatalf("expected caller slice to split into multiple encoded batches, got %d", dataSegments)
	}
}

func TestSourceAndBuildContextRejection(t *testing.T) {
	ctx := context.Background()
	store, _, req, _, sourceHash := fixture(t)
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	firstSink, firstBuild := sink, build
	defer func() {
		_ = firstSink.Close()
		_ = firstBuild.Abort()
	}()
	base := model.Occurrence{SymbolID: "external", ScopeID: req.Scopes[0].ID, URI: "file:///repo/main.go", Role: "reference", SourceHash: sourceHash, BuildContext: req.Scopes[0].BuildContext}
	wrongHash := base
	wrongHash.SourceHash = identity.ContentHash("sha256:" + strings.Repeat("0", 64))
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{wrongHash}); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("wrong source hash error = %v, want ErrInvalidSource", err)
	}
	_ = sink.Close()
	_ = build.Abort()

	build, err = store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err = NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	secondSink, secondBuild := sink, build
	defer func() {
		_ = secondSink.Close()
		_ = secondBuild.Abort()
	}()
	wrongContext := base
	wrongContext.BuildContext = "unknown-context"
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{wrongContext}); !errors.Is(err, ErrBuildContext) {
		t.Fatalf("wrong BuildContext error = %v, want ErrBuildContext", err)
	}
}

func TestUnknownScopeRejected(t *testing.T) {
	ctx := context.Background()
	store, _, req, _, _ := fixture(t)
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sink.Close()
		_ = build.Abort()
	}()
	if err := sink.WriteSymbols(ctx, []model.Symbol{{ID: "symbol", ScopeID: "unknown-scope"}}); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("unknown scope error = %v, want ErrInvalidScope", err)
	}
}

func TestUnknownIDsFollowCoverageAndCrossScopeResolution(t *testing.T) {
	ctx := context.Background()
	store, _, req, report, sourceHash := fixture(t)
	external := model.Occurrence{SymbolID: "external:compiler-known", ScopeID: req.Scopes[0].ID,
		URI: "file:///repo/main.go", Role: "reference", SourceHash: sourceHash, BuildContext: req.Scopes[0].BuildContext}
	build, _ := store.BeginBuild(ctx)
	sink, err := NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{external}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(ctx, report); !errors.Is(err, ErrUnknownSymbol) {
		t.Fatalf("unknown ID under complete coverage error = %v", err)
	}
	_ = sink.Close()
	_ = build.Abort()

	store, _, req, report, sourceHash = fixture(t)
	secondScope := req.Scopes[0]
	secondScope.ID = "go:module:other-context"
	secondScope.Build.Options = map[string]string{"variant": "test"}
	secondScope.BuildContext = model.ComputeBuildContextID(secondScope, "test-extractor", "1", "test-toolchain", nil)
	req.Scopes = append(req.Scopes, secondScope)
	secondProvenance := req.Provenance[req.Scopes[0].ID]
	secondProvenance.Scope = secondScope
	secondProvenance.Backend = identity.BackendID{Language: secondScope.Language, Name: "test"}
	secondProvenance.Identity = req.View.Identity()
	req.Provenance[secondScope.ID] = secondProvenance
	view := req.View.(testWorkspaceView)
	view.files = append(view.files, view.files[0])
	req.View = view
	// Keep the local occurrence context in scope one and resolve it to a global
	// symbol declared only by scope two.
	report.Identity = req.View.Identity()
	report.Coverage = completeCoverage(req.Scopes)
	build, err = store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err = NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sink.Close()
		_ = build.Abort()
	}()
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{external}); err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteSymbols(ctx, []model.Symbol{{ID: external.SymbolID, ScopeID: secondScope.ID, Name: "External"}}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatalf("cross-scope symbol did not resolve: %v", err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIncompleteCoverageRetainsUnresolvedIdentity(t *testing.T) {
	ctx := context.Background()
	store, _, req, report, sourceHash := fixture(t)
	report.Coverage = completeCoverage(req.Scopes)
	for i := range report.Coverage {
		if report.Coverage[i].ScopeID == req.Scopes[0].ID && report.Coverage[i].Fact == model.FactReference {
			report.Coverage[i].State = model.IncompleteKnownSubset
			report.Coverage[i].Reason = "external references may be unresolved"
		}
	}
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	occurrence := model.Occurrence{SymbolID: "external:unresolved", ScopeID: req.Scopes[0].ID, URI: "file:///repo/main.go",
		Role: "reference", SourceHash: sourceHash, BuildContext: req.Scopes[0].BuildContext}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{occurrence}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatal(err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := store.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(ctx, view)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	batch, err := reader.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Occurrences) != 1 || batch.Occurrences[0].SymbolID != occurrence.SymbolID {
		t.Fatalf("unresolved identity changed or disappeared: %+v", batch)
	}
}

func TestLegacyMalformedAndUnknownSegments(t *testing.T) {
	ctx := context.Background()
	legacyStore, _ := persistent.NewFileStore(t.TempDir(), persistent.Config{})
	legacyBuild, _ := legacyStore.BeginBuild(ctx)
	if _, err := legacyBuild.WriteSegment([]byte(`[{"Path":"main.go","Size":1,"SHA256":"abc"}]`)); err != nil {
		t.Fatal(err)
	}
	if err := legacyBuild.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	legacyView, err := legacyStore.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenReader(ctx, legacyView)
	var notSemantic *NotSemanticError
	if !errors.As(err, &notSemantic) || !errors.Is(err, ErrNotSemantic) {
		t.Fatalf("legacy inventory error = %T %v, want typed ErrNotSemantic", err, err)
	}
	sealedStore, _ := persistent.NewFileStore(t.TempDir(), persistent.Config{})
	sealedBuild, _ := sealedStore.BeginBuild(ctx)
	sealed := persistent.SealPayload([]byte(`[{"Path":"main.go","Size":1,"SHA256":"abc"}]`), persistent.FreshnessTuple{SourceHash: "legacy", BackendVer: "inventory"})
	if _, err := sealedBuild.WriteSegment(sealed); err != nil {
		t.Fatal(err)
	}
	if err := sealedBuild.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	sealedView, err := sealedStore.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = OpenReader(ctx, sealedView)
	if !errors.As(err, &notSemantic) || !errors.Is(err, ErrNotSemantic) {
		t.Fatalf("sealed legacy inventory error = %T %v, want typed ErrNotSemantic", err, err)
	}
	lowercaseLegacy := persistent.SealPayload([]byte(`[{"path":"main.go","size":1,"sha256":"abc"}]`), persistent.FreshnessTuple{SourceHash: "legacy", BackendVer: "inventory"})
	_, err = decodeWire(lowercaseLegacy, "1-00000000")
	if !errors.As(err, &notSemantic) || !errors.Is(err, ErrNotSemantic) {
		t.Fatalf("lowercase sealed legacy inventory error = %T %v, want typed ErrNotSemantic", err, err)
	}

	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "malformed", payload: []byte(`{"format":`)},
		{name: "unknown kind", payload: []byte(`{"format":"omnilsp-semantic-segment","schemaVersion":1,"kind":"future-kind","records":[]}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, req, report, _ := fixture(t)
			build, err := store.BeginBuild(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := build.WriteSegment(test.payload); err != nil {
				t.Fatal(err)
			}
			sink, err := NewSink(ctx, build, req)
			if err != nil {
				t.Fatal(err)
			}
			if err := sink.Finalize(ctx, report); err != nil {
				t.Fatal(err)
			}
			if err := build.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			view, err := store.OpenSnapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = OpenReader(ctx, view)
			if !errors.Is(err, ErrMalformedPayload) {
				t.Fatalf("reader error = %v, want ErrMalformedPayload", err)
			}
		})
	}
}

func TestContextCancellation(t *testing.T) {
	ctx := context.Background()
	store, _, req, report, _ := fixture(t)
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	firstSink, firstBuild := sink, build
	defer func() {
		_ = firstSink.Close()
		_ = firstBuild.Abort()
	}()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := sink.WriteSymbols(canceled, []model.Symbol{{ID: "cancelled", ScopeID: req.Scopes[0].ID}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteSymbols error = %v, want context.Canceled", err)
	}
	_ = sink.Close()
	_ = build.Abort()

	store, _, req, report, _ = fixture(t)
	build, err = store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sink, err = NewSink(ctx, build, req)
	if err != nil {
		t.Fatal(err)
	}
	secondSink, secondBuild := sink, build
	defer func() {
		_ = secondSink.Close()
		_ = secondBuild.Abort()
	}()
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatal(err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := store.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(ctx, view)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := reader.Next(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next error = %v, want context.Canceled", err)
	}
}

func TestExternalIDLedgerSortResolvesAcrossScopes(t *testing.T) {
	ctx := context.Background()
	ledger, err := newIDLedger()
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	const count = 1500
	ids := make([]string, count)
	for i := range ids {
		ids[i] = fmt.Sprintf("symbol-%05d-%s", i, strings.Repeat("x", 1000))
		if err := ledger.Add("target-scope", ids[i], idDeclared, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		if err := ledger.Add("source-scope", id, idReferenced, model.FactReference); err != nil {
			t.Fatal(err)
		}
	}
	coverage := []model.Coverage{{ScopeID: "source-scope", Fact: model.FactReference, State: model.Complete}}
	if err := ledger.Validate(ctx, coverage); err != nil {
		t.Fatalf("bounded external sort did not resolve cross-scope IDs: %v", err)
	}
}

func addPinnedTool(req *model.Request) {
	scope := req.Scopes[0]
	provenance := req.Provenance[scope.ID]
	tool := model.ToolIdentity{
		Name: "extractor", Path: "C:/tools/extractor.exe", Version: "1",
		SHA256: strings.Repeat("a", 64),
	}
	provenance.Tools = []model.ToolIdentity{tool}
	scope.BuildContext = model.ComputeBuildContextID(scope, provenance.Extractor, provenance.ExtractorVer, provenance.Toolchain, provenance.Tools)
	provenance.Scope = scope
	req.Scopes[0] = scope
	req.Provenance[scope.ID] = provenance
}

func setCoverageState(report *model.Report, state model.Completeness) {
	for i := range report.Coverage {
		report.Coverage[i].State = state
		report.Coverage[i].Reason = "extractor did not attest this scope"
	}
}

func fixture(t *testing.T) (*persistent.FileStore, string, model.Request, model.Report, identity.ContentHash) {
	t.Helper()
	root := t.TempDir()
	store, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	contentHash := sha256.Sum256([]byte("source"))
	sourceHash := identity.ContentHash("sha256:" + hex.EncodeToString(contentHash[:]))
	diskHash := sha256.Sum256([]byte("workspace"))
	id := model.Identity{Workspace: "workspace-1", DiskDigest: identity.ContentHash("sha256:" + hex.EncodeToString(diskHash[:])), SnapshotRev: 17}
	view := testWorkspaceView{id: id, files: []model.File{{URI: "file:///repo/main.go", LanguageID: "go", SHA256: sourceHash}}}
	scope := model.Scope{ID: "go:module:repo", Language: "go", RootURI: "file:///repo", Build: model.BuildInputs{Options: map[string]string{}}}
	scope.BuildContext = model.ComputeBuildContextID(scope, "test-extractor", "1", "test-toolchain", nil)
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion,
		Identity:      id,
		Scope:         scope,
		Extractor:     "test-extractor",
		ExtractorVer:  "1",
		Backend:       identity.BackendID{Language: "go", Name: "test"},
		Toolchain:     "test-toolchain",
	}
	req := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	return store, root, req, model.Report{Identity: id, Coverage: completeCoverage(req.Scopes), UsedTools: map[string][]model.ToolIdentity{}}, sourceHash
}

func completeCoverage(scopes []model.Scope) []model.Coverage {
	coverage := make([]model.Coverage, 0, len(scopes)*len(model.RequiredFactKinds))
	for _, scope := range scopes {
		for _, fact := range model.RequiredFactKinds {
			coverage = append(coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: model.Complete})
		}
	}
	return coverage
}
