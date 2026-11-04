package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/index/persistent"
)

// cpuOnlyBuild is a BuildSession that discards payloads without touching the
// filesystem, so the benchmark isolates the sink's CPU shape (validation and
// JSON encoding) from WriteSegment's fsync. The ledger's own temp-file writes
// stay inside the timed region and are identical before/after, so they cancel
// in the comparison.
type cpuOnlyBuild struct{ genID uint64 }

func (b *cpuOnlyBuild) GenerationID() uint64 { return b.genID }

func (b *cpuOnlyBuild) WriteSegment(payload []byte) (persistent.SegmentID, error) {
	return persistent.SegmentID(fmt.Sprintf("%x", len(payload))), nil
}

func (b *cpuOnlyBuild) Commit(context.Context) error { return nil }

func (b *cpuOnlyBuild) Abort() error { return nil }

func benchSinkRequest() (model.Request, identity.ContentHash) {
	contentHash := sha256.Sum256([]byte("source"))
	sourceHash := identity.ContentHash("sha256:" + hex.EncodeToString(contentHash[:]))
	diskHash := sha256.Sum256([]byte("workspace"))
	id := model.Identity{Workspace: "workspace-bench", DiskDigest: identity.ContentHash("sha256:" + hex.EncodeToString(diskHash[:])), SnapshotRev: 17}
	view := testWorkspaceView{id: id, files: []model.File{{URI: "file:///repo/main.go", LanguageID: "go", SHA256: sourceHash}}}
	scope := model.Scope{ID: "go:module:repo", Language: "go", RootURI: "file:///repo", Build: model.BuildInputs{Options: map[string]string{}}}
	scope.BuildContext = model.ComputeBuildContextID(scope, "bench-extractor", "1", "bench-toolchain", nil)
	provenance := model.Provenance{
		SchemaVersion: model.SchemaVersion,
		Identity:      id,
		Scope:         scope,
		Extractor:     "bench-extractor",
		ExtractorVer:  "1",
		Backend:       identity.BackendID{Language: "go", Name: "bench"},
		Toolchain:     "bench-toolchain",
	}
	req := model.Request{View: view, Scopes: []model.Scope{scope}, Provenance: map[string]model.Provenance{scope.ID: provenance}}
	return req, sourceHash
}

// BenchmarkSinkWriteOccurrencesCPUShape exercises the real
// sink.WriteOccurrences path for one large caller batch against a
// payload-discarding build session (verifierB-r23b entry 5: the write path
// used to run the record closure three times and json.Marshal twice per
// record, all under s.mu).
func BenchmarkSinkWriteOccurrencesCPUShape(b *testing.B) {
	const count = 50000
	ctx := context.Background()
	req, sourceHash := benchSinkRequest()
	scope := req.Scopes[0]
	values := make([]model.Occurrence, count)
	for i := range values {
		values[i] = model.Occurrence{
			SymbolID:     identity.SymbolID(fmt.Sprintf("go:sym.%06d", i%1000)),
			ScopeID:      scope.ID,
			URI:          "file:///repo/main.go",
			Range:        model.Position{StartLine: 1, StartChar: 0, EndLine: 1, EndChar: 8},
			Role:         "reference",
			SourceHash:   sourceHash,
			BuildContext: scope.BuildContext,
		}
	}
	if len(values[0].BuildContext) == 0 || !strings.HasPrefix(string(values[0].SourceHash), "sha256:") {
		b.Fatal("fixture precondition failed")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		sink, err := NewSink(ctx, &cpuOnlyBuild{genID: 1}, req)
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if err := sink.WriteOccurrences(ctx, values); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = sink.Close()
		b.StartTimer()
	}
}
