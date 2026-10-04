package languages

import (
	"context"

	"github.com/omnilsp/omni/internal/index/model"
)

// SemanticIndexProvider is an optional capability. It does not change the
// frozen Backend method set. Implementations must report coverage separately
// for every requested scope and fact kind.
type SemanticIndexProvider interface {
	ExportIndex(ctx context.Context, request model.Request, sink model.Sink) (model.Report, error)
}

// SemanticIndexRequestBuilder discovers the language's project/build scopes and
// pins its extractor inputs against an immutable workspace view. It is a
// separate optional capability so provider implementations remain testable
// with explicitly constructed requests.
type SemanticIndexRequestBuilder interface {
	BuildIndexRequest(ctx context.Context, view model.WorkspaceView, rootURI string) (model.Request, error)
}
