// External test package: these guards pin DEF-COMPACT structure-bounded
// compaction against the live semantic format. semantic imports persistent,
// so only a _test package may exercise the store, the compactor and
// semantic.OpenReader at once (§L5 storage boundary).
package persistent_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
	"github.com/omnilsp/omni/internal/index/semantic"
)

// commitTypedGenerationWithExtraBatches writes one complete typed semantic
// generation through semantic.NewSink, then injects extra hand-crafted
// symbols batches through the same build session BEFORE the sink finalizes,
// so the committed layout is exactly the live format: data batches first,
// terminal metadata segment last, with multiple batches of the same kind.
// Layout: [symbols(sink), occurrences(sink), symbols(extra...), metadata].
func commitTypedGenerationWithExtraBatches(t *testing.T, store *persistent.FileStore, symbolName string, extraNames []string) uint64 {
	t.Helper()
	ctx := context.Background()
	view := newIsolationView(symbolName)
	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatalf("begin build: %v", err)
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
		t.Fatalf("open semantic sink: %v", err)
	}
	defer func() { _ = sink.Close() }()
	symbolID := identity.SymbolID("go:x5-isolation." + symbolName)
	mainURI := isolationRootURI + "/main.go"
	useURI := isolationRootURI + "/use.go"
	mainLine, mainCol := lineCharOf(view.contents[mainURI], symbolName)
	useLine, useCol := lineCharOf(view.contents[useURI], symbolName)
	if err := sink.WriteSymbols(ctx, []model.Symbol{{ID: symbolID, ScopeID: scope.ID, Name: symbolName, Kind: "function"}}); err != nil {
		t.Fatalf("write symbols: %v", err)
	}
	if err := sink.WriteOccurrences(ctx, []model.Occurrence{
		{SymbolID: symbolID, ScopeID: scope.ID, URI: mainURI,
			Range: model.Position{StartLine: uint32(mainLine), StartChar: uint32(mainCol), EndLine: uint32(mainLine), EndChar: uint32(mainCol + len(symbolName))},
			Role:  "definition", SourceHash: view.files[0].SHA256, BuildContext: scope.BuildContext},
		{SymbolID: symbolID, ScopeID: scope.ID, URI: useURI,
			Range: model.Position{StartLine: uint32(useLine), StartChar: uint32(useCol), EndLine: uint32(useLine), EndChar: uint32(useCol + len(symbolName))},
			Role:  "reference", SourceHash: view.files[1].SHA256, BuildContext: scope.BuildContext},
	}); err != nil {
		t.Fatalf("write occurrences: %v", err)
	}
	// Extra same-kind batches: each is one legal wire envelope whose records
	// decodeStrict cleanly into model.Symbol within the sink's scope.
	for _, extra := range extraNames {
		payload := symbolsWireEnvelope("go:x5-isolation."+extra, scope.ID, extra)
		if _, err := build.WriteSegment(payload); err != nil {
			t.Fatalf("write extra symbols batch %q: %v", extra, err)
		}
	}
	report := model.Report{Identity: view.Identity(), UsedTools: map[string][]model.ToolIdentity{}}
	for _, fact := range model.RequiredFactKinds {
		report.Coverage = append(report.Coverage, model.Coverage{ScopeID: scope.ID, Fact: fact, State: model.Complete})
	}
	if err := sink.Finalize(ctx, report); err != nil {
		t.Fatalf("finalize generation: %v", err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatalf("commit generation: %v", err)
	}
	committed = true
	return build.GenerationID()
}

// symbolsWireEnvelope crafts one symbols batch in the exact live wire format
// (semantic.wireSegment): a single JSON document, one kind, records array.
func symbolsWireEnvelope(symbolID, scopeID, name string) []byte {
	return []byte(fmt.Sprintf(`{"format":%q,"schemaVersion":%d,"kind":"symbols","records":[{"ID":%q,"ScopeID":%q,"Name":%q,"Kind":"function","Signature":""}]}`,
		"omnilsp-semantic-segment", semantic.SchemaVersion, symbolID, scopeID, name))
}

// generationBytes captures the verified payload bytes and per-kind raw record
// bytes of one generation, in segment order, through GenerationView only.
type generationBytes struct {
	genID     uint64
	order     []persistent.SegmentID
	payloads  map[persistent.SegmentID][]byte
	metaID    persistent.SegmentID
	metaBytes []byte
	records   map[string][][]byte // kind → raw record bytes in order
}

func captureGenerationBytes(t *testing.T, view persistent.GenerationView) *generationBytes {
	t.Helper()
	captured := &generationBytes{
		genID:    view.ID,
		payloads: make(map[persistent.SegmentID][]byte),
		records:  make(map[string][][]byte),
	}
	for _, ref := range view.Segments {
		payload, err := view.ReadSegment(ref.ID)
		if err != nil {
			t.Fatalf("read segment %s of generation %d: %v", ref.ID, view.ID, err)
		}
		captured.order = append(captured.order, ref.ID)
		captured.payloads[ref.ID] = payload
		var probe struct {
			Kind    string          `json:"kind"`
			Records json.RawMessage `json:"records"`
		}
		if err := json.Unmarshal(payload, &probe); err != nil {
			t.Fatalf("segment %s of generation %d is not the live JSON envelope: %v", ref.ID, view.ID, err)
		}
		if probe.Kind == "metadata" {
			captured.metaID = ref.ID
			captured.metaBytes = payload
			continue
		}
		var records []json.RawMessage
		if err := json.Unmarshal(probe.Records, &records); err != nil {
			t.Fatalf("segment %s records: %v", ref.ID, err)
		}
		for _, record := range records {
			captured.records[probe.Kind] = append(captured.records[probe.Kind], []byte(record))
		}
	}
	if captured.metaID == "" {
		t.Fatalf("generation %d has no terminal metadata segment", view.ID)
	}
	return captured
}

// assertSameFacts requires the compaction roundtrip to be bit-identical where
// the format pins it: the terminal metadata payload byte-for-byte, and every
// record of every kind byte-for-byte in its original order (re-partitioning
// may regroup records into different segments, never rewrite or reorder them).
func assertSameFacts(t *testing.T, before, after *generationBytes) {
	t.Helper()
	if !bytes.Equal(before.metaBytes, after.metaBytes) {
		t.Errorf("terminal metadata payload changed across compaction:\n before: %q\n after:  %q", before.metaBytes, after.metaBytes)
	}
	for kind, want := range before.records {
		got := after.records[kind]
		if len(got) != len(want) {
			t.Errorf("kind %s: %d records after compaction, want %d", kind, len(got), len(want))
			continue
		}
		for i := range want {
			if !bytes.Equal(got[i], want[i]) {
				t.Errorf("kind %s record[%d] changed across compaction:\n before: %s\n after:  %s", kind, i, want[i], got[i])
			}
		}
	}
	for kind := range after.records {
		if _, ok := before.records[kind]; !ok {
			t.Errorf("kind %s appeared after compaction but was not in the source generation", kind)
		}
	}
}

// TestCompact_StructuredSemanticGenerationRoundTripIsBitIdentical is the
// DEF-COMPACT guard: compaction of a live-format generation (multiple data
// batches of the same kind + terminal metadata) must produce re-partitioned
// batch segments plus the terminal metadata segment, and semantic.OpenReader
// must serve the full fact set with every record preserved bit-for-bit.
// The pre-DEF-COMPACT compactor merged every payload into one physical
// segment, which OpenReader rejects (decodeStrict refuses the length-prefixed
// trailing JSON) — this test is red against that implementation by design.
func TestCompact_StructuredSemanticGenerationRoundTripIsBitIdentical(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	genID := commitTypedGenerationWithExtraBatches(t, store, "CompactedOne", []string{"ExtraSymA", "ExtraSymB"})

	before, err := store.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before.ID != genID {
		t.Fatalf("open generation %d, want %d", before.ID, genID)
	}
	capturedBefore := captureGenerationBytes(t, before)
	if len(capturedBefore.order) != 5 {
		t.Fatalf("fixture layout: %d segments, want 5 (2 sink batches + 2 extra batches + metadata)", len(capturedBefore.order))
	}

	if err := store.Compact(ctx); err != nil {
		t.Fatalf("compact: %v", err)
	}
	after, err := store.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("open post-compact generation: %v", err)
	}
	if after.ID <= before.ID {
		t.Fatalf("post-compact generation %d did not advance past %d", after.ID, before.ID)
	}
	if len(after.Segments) >= len(before.Segments) {
		t.Errorf("compaction did not reduce segment count: %d → %d", len(before.Segments), len(after.Segments))
	}

	// The post-compact generation must open as a live semantic generation and
	// serve the same facts; under the single-segment merger this is the step
	// that failed with ErrMalformedPayload.
	reader, err := semantic.OpenReader(ctx, after)
	if err != nil {
		t.Fatalf("semantic.OpenReader rejected the compacted generation: %v", err)
	}
	defer reader.Close()

	capturedAfter := captureGenerationBytes(t, after)
	assertSameFacts(t, capturedBefore, capturedAfter)

	// Structural layout: one merged segment per kind (first-appearance order)
	// plus the terminal metadata segment.
	var kinds []string
	for _, ref := range after.Segments {
		var probe struct {
			Kind string `json:"kind"`
		}
		payload, err := after.ReadSegment(ref.ID)
		if err != nil {
			t.Fatalf("read post-compact segment %s: %v", ref.ID, err)
		}
		if err := json.Unmarshal(payload, &probe); err != nil {
			t.Fatalf("post-compact segment %s is not a single JSON envelope: %v", ref.ID, err)
		}
		kinds = append(kinds, probe.Kind)
	}
	wantKinds := []string{"symbols", "occurrences", "metadata"}
	if fmt.Sprint(kinds) != fmt.Sprint(wantKinds) {
		t.Errorf("post-compact segment kinds = %v, want %v", kinds, wantKinds)
	}

	names, roles, metadata := readGenerationFacts(t, ctx, after)
	wantNames := []string{"CompactedOne", "ExtraSymA", "ExtraSymB"}
	if fmt.Sprint(names) != fmt.Sprint(wantNames) {
		t.Errorf("post-compact symbol facts = %v, want %v", names, wantNames)
	}
	if roles["definition"] != 1 || roles["reference"] != 1 {
		t.Errorf("post-compact occurrence roles = %v, want one definition and one reference", roles)
	}
	if metadata.SchemaVersion != semantic.SchemaVersion {
		t.Errorf("post-compact metadata schema = %d, want %d", metadata.SchemaVersion, semantic.SchemaVersion)
	}
}

// TestCompact_MidCompactionFailureRollsBackAndKeepsQuarantineClean pins the
// transaction semantics of structure-bounded compaction (TXN-002/L16): a
// refusal at a mid-compaction checkpoint — after every source segment was
// read and verified, while the staging build is publishing its first output
// segment — must roll the staging generation back completely and leave the
// committed generation, its quarantine state and the live semantic format
// exactly as they were; a retry with an adequate budget then compacts.
func TestCompact_MidCompactionFailureRollsBackAndKeepsQuarantineClean(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	genID := commitTypedGenerationWithExtraBatches(t, store, "RollbackKept", []string{"ExtraSymA", "ExtraSymB", "ExtraSymC"})
	before, err := store.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	capturedBefore := captureGenerationBytes(t, before)
	if len(capturedBefore.order) != 6 {
		t.Fatalf("fixture layout: %d segments, want 6 (2 sink batches + 3 extra batches + metadata)", len(capturedBefore.order))
	}
	if len(store.Quarantined()) != 0 {
		t.Fatalf("fixture precondition: quarantine holds %v", store.Quarantined())
	}

	// A budget that covers the current store but leaves no room for any new
	// segment: the first compaction output write is refused (§L17 fail-closed)
	// after the compactor already read and verified all source segments.
	budget := dirLiveBytes(t, root) + 64
	tight, err := persistent.NewFileStore(root, persistent.Config{DiskBudgetBytes: budget})
	if err != nil {
		t.Fatalf("open budgeted store: %v", err)
	}
	if err := tight.Compact(ctx); !errors.Is(err, persistent.ErrDiskBudgetExceeded) {
		t.Fatalf("mid-compaction write error = %v, want ErrDiskBudgetExceeded", err)
	}

	// Full rollback: no staging residue, manifest untouched, original
	// generation byte-identical and still a valid semantic generation.
	stagingLeftover, _ := filepath.Glob(filepath.Join(root, "staging-*"))
	if len(stagingLeftover) != 0 {
		t.Errorf("failed compaction left staging directories: %v", stagingLeftover)
	}
	if len(store.Quarantined()) != 0 {
		t.Errorf("failed compaction quarantined %v; quarantine must stay untouched", store.Quarantined())
	}
	recovered, err := store.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("open generation after failed compaction: %v", err)
	}
	if recovered.ID != genID {
		t.Errorf("generation after failed compaction = %d, want the untouched %d", recovered.ID, genID)
	}
	for _, ref := range recovered.Segments {
		payload, err := recovered.ReadSegment(ref.ID)
		if err != nil {
			t.Fatalf("original segment %s unreadable after failed compaction: %v", ref.ID, err)
		}
		if !bytes.Equal(payload, capturedBefore.payloads[ref.ID]) {
			t.Errorf("original segment %s payload changed after a failed compaction", ref.ID)
		}
	}
	if _, err := semantic.OpenReader(ctx, recovered); err != nil {
		t.Errorf("semantic.OpenReader rejected the original generation after a failed compaction: %v", err)
	}

	// Retry with an adequate budget: the same store content compacts, and the
	// post-compact roundtrip is bit-identical to the pre-failure generation.
	retry, err := persistent.NewFileStore(root, persistent.Config{})
	if err != nil {
		t.Fatalf("open retry store: %v", err)
	}
	if err := retry.Compact(ctx); err != nil {
		t.Fatalf("retry compaction after rollback: %v", err)
	}
	after, err := retry.OpenSnapshot(ctx)
	if err != nil {
		t.Fatalf("open post-retry generation: %v", err)
	}
	capturedAfter := captureGenerationBytes(t, after)
	assertSameFacts(t, capturedBefore, capturedAfter)
	if _, err := semantic.OpenReader(ctx, after); err != nil {
		t.Errorf("semantic.OpenReader rejected the compacted generation: %v", err)
	}
}

// dirLiveBytes measures the store's on-disk footprint the same way the §L17
// budget walk does (all regular files, quarantine excluded — there is none in
// this fixture), so the test can size a budget that admits no new segment.
func dirLiveBytes(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(root, func(_ string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk store root: %v", err)
	}
	return total
}

// TestSemanticReaderRejectsLegacySingleSegmentMergeOutput pins the failure
// mode that motivated DEF-COMPACT: the pre-structure-bounded compactor packed
// every payload of the pinned snapshot into ONE physical segment with 8-byte
// big-endian length prefixes (mergeAppend framing). The frame layer accepts
// such a segment (valid magic, length, crc), but the semantic reader must
// reject it — decodeStrict refuses the trailing JSON after the first
// document — so a compacted live generation was unreadable. If a future
// change reintroduces single-segment merging, this pin documents exactly what
// breaks, and captureGenerationBytes guards above fail first.
func TestSemanticReaderRejectsLegacySingleSegmentMergeOutput(t *testing.T) {
	ctx := context.Background()
	store, err := persistent.NewFileStore(t.TempDir(), persistent.Config{})
	if err != nil {
		t.Fatal(err)
	}
	// The live-format payloads the legacy merger would have packed: one data
	// batch and the terminal metadata envelope, merged into one segment.
	data := symbolsWireEnvelope("go:x5-isolation.LegacyMerged", "x5-isolation-go", "LegacyMerged")
	metadata := []byte(`{"format":"omnilsp-semantic-segment","schemaVersion":1,"kind":"metadata","metadata":{"SchemaVersion":1}}`)
	merged := legacyMergeAppend(data, metadata)

	build, err := store.BeginBuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := build.WriteSegment(merged); err != nil {
		t.Fatal(err)
	}
	if err := build.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	view, err := store.OpenSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Segments) != 1 {
		t.Fatalf("legacy layout: %d segments, want 1", len(view.Segments))
	}
	payload, err := view.ReadSegment(view.Segments[0].ID)
	if err != nil {
		t.Fatalf("frame layer must accept the merged segment (magic+schemaVer+crc are valid): %v", err)
	}
	if !bytes.Equal(payload, merged) {
		t.Fatal("frame roundtrip of the merged segment is not byte-identical")
	}
	if _, err := semantic.OpenReader(ctx, view); !errors.Is(err, semantic.ErrMalformedPayload) {
		t.Fatalf("semantic.OpenReader error = %v, want ErrMalformedPayload (decodeStrict must reject the legacy merged output)", err)
	}
}

// legacyMergeAppend reproduces the pre-DEF-COMPACT mergeAppend framing
// (recovery.go): 8-byte big-endian payload length prefix, payload, repeat.
func legacyMergeAppend(payloads ...[]byte) []byte {
	var out []byte
	for _, payload := range payloads {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
		out = append(out, length[:]...)
		out = append(out, payload...)
	}
	return out
}
