package semantic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/index/model"
)

// Pin for verifierB-r23b entry 3: WriteEdges' record closure only ran the
// generic string checks — validString does not know edge-kind semantics — so
// an unsupported edge kind passed the pre-validation loop, was marshalled and
// handed to build.WriteSegment, landing as persistent bytes in a staging
// segment, and only recordIDs' edgeFact rejected it afterwards. That violated
// writeRecords' own "validate the complete caller batch before publishing any
// of its pieces" invariant (reader.validateBatch checks edgeFact on the read
// side; the write side did not), and sink.fail leaves the staged bytes in
// place. The kind-semantics check must run in the pre-validation closure —
// the same edgeFact the reader uses — so an invalid kind is rejected before
// anything touches the staging area.
func TestWriteEdgesRejectsUnknownKindBeforeStaging(t *testing.T) {
	ctx := context.Background()
	store, root, req, _, sourceHash := fixture(t)
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
	scope := req.Scopes[0]
	edge := func(kind model.EdgeKind) model.Edge {
		return model.Edge{
			From: "go repo main#Foo.", To: "go repo main#Bar.",
			ScopeID: scope.ID, Kind: kind,
			SourceURI: "file:///repo/main.go", Range: model.Position{StartLine: 1, StartChar: 0, EndLine: 1, EndChar: 4},
			SourceHash: sourceHash, BuildContext: scope.BuildContext,
		}
	}
	err = sink.WriteEdges(ctx, []model.Edge{edge(model.EdgeImplementation), edge(model.EdgeKind("definitely-not-a-kind"))})
	if !errors.Is(err, ErrMalformedPayload) || !strings.Contains(err.Error(), `unsupported edge kind "definitely-not-a-kind"`) {
		t.Fatalf("WriteEdges error = %v, want ErrMalformedPayload with the unsupported-kind text", err)
	}

	// Nothing may have been staged: the invalid kind must be rejected in the
	// pre-validation pass, before the first WriteSegment call. sink.fail does
	// not clean the staging area, so any staged byte here is a real violation.
	staging := filepath.Join(root, fmt.Sprintf("staging-%d", build.GenerationID()))
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	var staged []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "seg-") && strings.HasSuffix(name, ".bin") {
			staged = append(staged, name)
		}
	}
	if len(staged) != 0 {
		t.Fatalf("rejected edge batch left staged segment bytes on disk: %v", staged)
	}
	if sink.result == nil {
		t.Fatal("failed sink must record its failure result")
	}
}
