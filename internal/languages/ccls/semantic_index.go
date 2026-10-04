package ccls

import (
	"context"

	"github.com/omnilsp/omni/internal/identity"
	"github.com/omnilsp/omni/internal/index/extractors/clang"
	"github.com/omnilsp/omni/internal/index/model"
	"github.com/omnilsp/omni/internal/languages"
)

var _ languages.SemanticIndexProvider = (*Backend)(nil)
var _ languages.SemanticIndexRequestBuilder = (*Backend)(nil)

// BuildIndexRequest derives pinned compilation scopes from compile_commands.json
// in the immutable view. It does not consult the mutable backend working tree
// or search PATH for compiler binaries.
func (b *Backend) BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (model.Request, error) {
	return clang.BuildIndexRequest(ctx, view, rootURI, identity.BackendID{Language: "cpp", Name: "ccls"}, b.currentBackendEpoch())
}

// RebuildVerifiedPlannerRequest reconstructs a C/C++ planner request from a
// captured compile database and previously pinned tool/header identities. The
// verified clang restore path hashes its inputs without launching clang.
func RebuildVerifiedPlannerRequest(ctx context.Context, view model.WorkspaceView, rootURI string, attestations []model.Provenance) (model.Request, error) {
	return clang.RebuildVerifiedPlannerRequest(ctx, view, rootURI, attestations)
}

// ExportIndex streams libclang-derived facts from the materialized immutable
// view through the canonical bounded sink.
func (b *Backend) ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error) {
	return clang.Extract(ctx, request, sink)
}
