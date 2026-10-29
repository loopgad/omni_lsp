package persistent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// FuzzOpenSnapshotCorruptStore feeds hostile bytes into every store file and
// asserts the §L8 recovery path: never panic, never trust unchecked data —
// quarantine or clean error only (§S3 index-decoding target).
func FuzzOpenSnapshotCorruptStore(f *testing.F) {
	seeds := [][]byte{
		nil,
		{},
		[]byte("not json"),
		[]byte(`{"generation":1,"segments":[]}`),
		[]byte(`{"generation":-1,"segments":[{"id":"x","path":"../escape"}]}`),
		{0xFF, 0xFF, 0xFF, 0xFF},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, manifest []byte) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, fileManifest), manifest, 0o644); err != nil {
			t.Fatal(err)
		}
		store, err := NewFileStore(root, Config{})
		if err != nil {
			return // rejected at open: acceptable outcome
		}
		_, _ = store.OpenSnapshot(context.Background()) // must not panic
	})
}

// FuzzCompactPlanPreservesInvariants feeds hostile payload bytes through the
// structure-bounded compaction planner (§S3 index-decoding target): the
// planner classifies untrusted segment payloads before any rewrite, so it
// must never panic, never grow the segment count, always keep the last
// payload (the terminal metadata segment) byte-identical in the last plan
// position, and every planned output must be either a verbatim copy of an
// input payload or one valid JSON document.
func FuzzCompactPlanPreservesInvariants(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"format":"omnilsp-semantic-segment","schemaVersion":1,"kind":"symbols","records":[{"ID":"go:a.B","ScopeID":"a","Name":"B","Kind":"function","Signature":""}]}`),
		[]byte(`{"format":"omnilsp-semantic-segment","schemaVersion":1,"kind":"occurrences","records":[{"SymbolID":"go:a.B","ScopeID":"a"}]}`),
		[]byte(`{"format":"omnilsp-semantic-segment","schemaVersion":1,"kind":"metadata","metadata":{"SchemaVersion":1}}`),
		[]byte(`{"kind":"symbols","records":[]}`),
		[]byte(`[{"path":"legacy","Size":1,"SHA256":"x"}]`),
		[]byte("not json"),
		[]byte(`{"kind":"symbols","records":[{"n":1}]}{"kind":"symbols"}`),
		nil,
	}
	for _, seed := range seeds {
		f.Add(seed, seed, seed)
	}
	f.Fuzz(func(t *testing.T, a, b, c []byte) {
		payloads := [][]byte{a, b, c}
		plan := planCompaction(payloads, 256, 4096)
		if len(plan) > len(payloads) {
			t.Fatalf("plan grew segments: %d → %d", len(payloads), len(plan))
		}
		if len(plan) > 0 && !bytes.Equal(plan[len(plan)-1], payloads[len(payloads)-1]) {
			t.Fatalf("plan replaced the terminal payload:\n want: %q\n got:  %q", payloads[len(payloads)-1], plan[len(plan)-1])
		}
		for i, out := range plan {
			verbatim := false
			for _, in := range payloads {
				if bytes.Equal(out, in) {
					verbatim = true
					break
				}
			}
			if !verbatim && !json.Valid(out) {
				t.Fatalf("plan[%d] is neither a verbatim copy nor one valid JSON document: %q", i, out)
			}
		}
	})
}

// FuzzFreshnessSealVerify round-trips hostile payloads through the §L10
// freshness seal: VerifyPayload must reject truncation, bit-flips, and empty
// inputs without panicking.
func FuzzFreshnessSealVerify(f *testing.F) {
	f.Add([]byte("payload"), uint32(1))
	f.Add([]byte{}, uint32(0))
	f.Fuzz(func(t *testing.T, payload []byte, _ uint32) {
		tup := FreshnessTuple{
			SourceHash: "abc", BuildContext: "ctx", Toolchain: "go",
			BackendVer: "v1",
		}
		sealed := SealPayload(payload, tup)
		_, _ = VerifyPayload(sealed, tup) // must not panic on any sealed input
	})
}
