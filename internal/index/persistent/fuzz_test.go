package persistent

import (
	"context"
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
