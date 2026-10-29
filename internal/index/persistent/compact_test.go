package persistent

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func compactTestBatch(kind string, records ...string) []byte {
	return []byte(`{"format":"omnilsp-semantic-segment","schemaVersion":1,"kind":"` + kind + `","records":[` + strings.Join(records, ",") + `]}`)
}

func compactTestMetadata() []byte {
	return []byte(`{"format":"omnilsp-semantic-segment","schemaVersion":1,"kind":"metadata","metadata":{"workspace":"w"}}`)
}

func compactTestRecord(prefix string, n int) string {
	return `{"pad":"` + prefix + strings.Repeat("x", 11) + `","n":` + strconv.Itoa(n) + `}`
}

// planRecords concatenates one kind's raw record bytes across a plan, in
// order, so tests can assert record preservation without re-deriving kinds.
func planRecords(t *testing.T, plan [][]byte, kind string) [][]byte {
	t.Helper()
	var out [][]byte
	for _, payload := range plan {
		var probe struct {
			Kind    string          `json:"kind"`
			Records json.RawMessage `json:"records"`
		}
		if err := json.Unmarshal(payload, &probe); err != nil {
			t.Fatalf("planned payload is not a single JSON document: %v", err)
		}
		if probe.Kind != kind {
			continue
		}
		var records []json.RawMessage
		if err := json.Unmarshal(probe.Records, &records); err != nil {
			t.Fatalf("planned %s records: %v", kind, err)
		}
		for _, raw := range records {
			out = append(out, raw)
		}
	}
	return out
}

func assertRecordOrder(t *testing.T, plan [][]byte, kind string, want [][]byte) {
	t.Helper()
	got := planRecords(t, plan, kind)
	if len(got) != len(want) {
		t.Fatalf("kind %s: %d records in plan, want %d", kind, len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("kind %s record[%d] = %s, want %s", kind, i, got[i], want[i])
		}
	}
}

// TestPlanCompact_MergesSameKindBounded pins the structural bound of
// DEF-COMPACT re-partitioning at a scale the unit test can inspect directly:
// same-kind batches merge into envelope chunks that each respect the batch
// bound (a record is never split), the per-kind record order is preserved
// bit-for-bit across chunk boundaries, kinds keep their first-appearance
// order, the metadata payload is terminal and verbatim, and the plan never
// grows the segment count.
func TestPlanCompact_MergesSameKindBounded(t *testing.T) {
	const batchLimit = int64(400)
	symbols := make([][]byte, 40)
	for i := range symbols {
		symbols[i] = []byte(compactTestRecord("sym", i))
	}
	// Ten symbols source segments of four records each (well under the bound)
	// plus one occurrences segment: 40 records re-chunk into fewer, bounded
	// segments without splitting any record.
	payloads := [][]byte{}
	for source := 0; source < 10; source++ {
		records := make([]string, 4)
		for i := range records {
			records[i] = string(symbols[source*4+i])
		}
		payloads = append(payloads, compactTestBatch("symbols", records...))
	}
	payloads = append(payloads,
		compactTestBatch("occurrences", compactTestRecord("occ", 0), compactTestRecord("occ", 1)),
		compactTestBatch("edges", `{"e":1}`),
		compactTestMetadata(),
	)

	plan := planCompaction(payloads, batchLimit, compactSegmentBytes)
	if len(plan) >= len(payloads) {
		t.Fatalf("plan did not reduce segments: %d → %d", len(payloads), len(plan))
	}

	// Layout: symbol chunks first (first-appearance kind order), then
	// occurrences, edges, and the metadata segment last.
	var kinds []string
	for _, payload := range plan {
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(payload, &probe); err != nil {
			t.Fatalf("planned payload is not a single JSON document: %v", err)
		}
		kinds = append(kinds, probe.Kind)
	}
	if len(kinds) < 5 || kinds[0] != "symbols" || kinds[len(kinds)-3] != "occurrences" ||
		kinds[len(kinds)-2] != "edges" || kinds[len(kinds)-1] != "metadata" ||
		countKind(kinds, "occurrences") != 1 || countKind(kinds, "edges") != 1 ||
		countKind(kinds, "metadata") != 1 {
		t.Fatalf("plan kinds = %v, want bounded symbols chunks then occurrences, edges, metadata", kinds)
	}
	if countKind(kinds, "symbols") < 2 {
		t.Fatalf("40 bounded records re-chunked into %d segments; the bound is not being applied", countKind(kinds, "symbols"))
	}

	// Every merged payload respects the batch bound and re-emits the source
	// envelope fields verbatim.
	for _, payload := range plan[:len(plan)-1] {
		if int64(len(payload)) > batchLimit {
			t.Errorf("merged payload is %d bytes, over the %d byte bound", len(payload), batchLimit)
		}
		var probe struct {
			Format        string `json:"format"`
			SchemaVersion uint32 `json:"schemaVersion"`
		}
		if err := json.Unmarshal(payload, &probe); err != nil {
			t.Fatalf("planned payload: %v", err)
		}
		if probe.Format != "omnilsp-semantic-segment" || probe.SchemaVersion != 1 {
			t.Errorf("merged envelope lost its source fields: format=%q schema=%d", probe.Format, probe.SchemaVersion)
		}
	}

	// Records survive bit-for-bit in their original per-kind order, across
	// chunk boundaries.
	assertRecordOrder(t, plan, "symbols", symbols)
	assertRecordOrder(t, plan, "occurrences", [][]byte{[]byte(compactTestRecord("occ", 0)), []byte(compactTestRecord("occ", 1))})
	assertRecordOrder(t, plan, "edges", [][]byte{[]byte(`{"e":1}`)})

	if !bytes.Equal(plan[len(plan)-1], compactTestMetadata()) {
		t.Errorf("metadata payload did not pass through verbatim")
	}
}

func countKind(kinds []string, kind string) int {
	count := 0
	for _, value := range kinds {
		if value == kind {
			count++
		}
	}
	return count
}

// TestPlanCompact_PassThroughNonLiveLayouts: any generation that does not
// match the live batch layout is rewritten byte-for-byte — never merged,
// never reordered, never reinterpreted.
func TestPlanCompact_PassThroughNonLiveLayouts(t *testing.T) {
	legacyInventory := []byte(`[{"path":"a.go","Size":1,"SHA256":"x"}]`)
	opaque := []byte("not json at all")
	cases := map[string][][]byte{
		"opaque among conforming": {
			compactTestBatch("symbols", `{"n":1}`),
			opaque,
			compactTestMetadata(),
		},
		"legacy inventory": {legacyInventory},
		"metadata not terminal": {
			compactTestMetadata(),
			compactTestBatch("symbols", `{"n":1}`),
		},
		"two metadata segments": {
			compactTestBatch("symbols", `{"n":1}`),
			compactTestMetadata(),
			compactTestMetadata(),
		},
		"data only, no metadata": {
			compactTestBatch("symbols", `{"n":1}`),
		},
		"empty records array": {
			[]byte(`{"format":"omnilsp-semantic-segment","schemaVersion":1,"kind":"symbols","records":[]}`),
			compactTestMetadata(),
		},
		"data envelope carries metadata field": {
			[]byte(`{"format":"f","schemaVersion":1,"kind":"symbols","records":[{"n":1}],"metadata":{}}`),
			compactTestMetadata(),
		},
		"mismatched envelope templates": {
			compactTestBatch("symbols", `{"n":1}`),
			[]byte(`{"format":"omnilsp-semantic-segment","schemaVersion":2,"kind":"symbols","records":[{"n":2}]}`),
			compactTestMetadata(),
		},
		"trailing json in payload": {
			[]byte(`{"kind":"symbols","records":[{"n":1}]} {"kind":"symbols"}`),
			compactTestMetadata(),
		},
		"records not an array": {
			[]byte(`{"kind":"symbols","records":{"n":1}}`),
			compactTestMetadata(),
		},
		"kind is not a string": {
			[]byte(`{"kind":7,"records":[{"n":1}]}`),
			compactTestMetadata(),
		},
		"data segment larger than the batch bound": {
			compactTestBatch("symbols", compactTestRecord("big", 0), compactTestRecord("big", 1), compactTestRecord("big", 2), compactTestRecord("big", 3), compactTestRecord("big", 4), compactTestRecord("big", 5), compactTestRecord("big", 6), compactTestRecord("big", 7), compactTestRecord("big", 8), compactTestRecord("big", 9), compactTestRecord("big", 10), compactTestRecord("big", 11), compactTestRecord("big", 12), compactTestRecord("big", 13), compactTestRecord("big", 14)),
			compactTestMetadata(),
		},
	}
	for name, payloads := range cases {
		plan := planCompaction(payloads, 400, compactSegmentBytes)
		if len(plan) != len(payloads) {
			t.Errorf("%s: plan changed segment count %d → %d", name, len(payloads), len(plan))
			continue
		}
		for i := range payloads {
			if !bytes.Equal(plan[i], payloads[i]) {
				t.Errorf("%s: plan[%d] rewritten:\n before: %q\n after:  %q", name, i, payloads[i], plan[i])
			}
		}
	}
}

// TestPlanCompact_OversizedRecords: a record bigger than the batch bound but
// under the hard segment cap rides in its own bounded-by-necessity chunk; a
// payload that cannot fit the hard cap at all passes through verbatim (the
// reader could not have read it either).
func TestPlanCompact_OversizedRecords(t *testing.T) {
	big := `{"pad":"` + strings.Repeat("y", 300) + `"}`
	payloads := [][]byte{
		compactTestBatch("symbols", `{"n":1}`),
		compactTestBatch("symbols", big),
		compactTestMetadata(),
	}
	plan := planCompaction(payloads, 400, 4096)
	if len(plan) != len(payloads) {
		t.Fatalf("oversized-but-readable record forced a pass-through: %d → %d", len(payloads), len(plan))
	}
	assertRecordOrder(t, plan, "symbols", [][]byte{[]byte(`{"n":1}`), []byte(big)})

	huge := `{"pad":"` + strings.Repeat("z", 5000) + `"}`
	payloads = [][]byte{
		compactTestBatch("symbols", huge),
		compactTestMetadata(),
	}
	plan = planCompaction(payloads, 400, 4096)
	if len(plan) != len(payloads) {
		t.Fatalf("unreadable record changed the segment count: %d → %d", len(payloads), len(plan))
	}
	if !bytes.Equal(plan[0], payloads[0]) || !bytes.Equal(plan[1], payloads[1]) {
		t.Errorf("over-cap record was not passed through verbatim")
	}
}

// TestPlanCompact_MetadataOnlyAndEmpty: degenerate generations fall back to
// the verbatim rewrite instead of producing an empty or malformed plan.
func TestPlanCompact_MetadataOnlyAndEmpty(t *testing.T) {
	plan := planCompaction([][]byte{compactTestMetadata()}, compactBatchBytes, compactSegmentBytes)
	if len(plan) != 1 || !bytes.Equal(plan[0], compactTestMetadata()) {
		t.Fatalf("metadata-only generation: plan = %v", plan)
	}
	if plan := planCompaction(nil, compactBatchBytes, compactSegmentBytes); len(plan) != 0 {
		t.Fatalf("empty generation: plan = %v", plan)
	}
}
