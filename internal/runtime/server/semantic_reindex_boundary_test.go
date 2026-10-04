package server

import (
	"context"
	"testing"

	"github.com/omnilsp/omni/internal/index/model"
)

type mismatchedWorkspacePlanner struct{ fixtureSemanticProvider }

func (p mismatchedWorkspacePlanner) BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	request, err := p.fixtureSemanticProvider.BuildIndexRequest(ctx, view, rootURI)
	request.WorkspaceRootURI = rootURI + "/unrelated"
	return request, err
}

func TestSemanticReindexRejectsMismatchedWorkspaceBoundary(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	s, _, _ := newSemanticQueryFixture(t, 1)
	before := indexStats(t, s)
	provider := fixtureSemanticProvider{}
	s.RegisterSemanticIndexProvider("go", provider, mismatchedWorkspacePlanner{provider})
	response := indexRequest(s, context.Background(), "omnilsp/reindex")
	if response == nil || response.Error == nil {
		t.Fatalf("mismatched captured workspace was accepted: %+v", response)
	}
	after := indexStats(t, s)
	if after.Generation != before.Generation || !after.Fresh {
		t.Fatalf("rejected request changed the valid generation: before=%+v after=%+v", before, after)
	}
}
