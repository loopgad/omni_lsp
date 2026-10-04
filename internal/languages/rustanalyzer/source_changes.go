package rustanalyzer

import (
	"context"

	ierrors "github.com/omnilsp/omni/internal/errors"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/nested"
)

var _ languages.SourceChangeSynchronizer = (*Backend)(nil)

// NotifySourceChanges forwards workspace-file lifecycle events to
// rust-analyzer through the nested connection's workspace lease.
func (b *Backend) NotifySourceChanges(ctx context.Context, changes []languages.SourceChange) error {
	if b == nil || b.conn == nil {
		return ierrors.New(ierrors.ErrBackendUnavailable, serverName, "backend is unavailable")
	}
	nestedChanges := make([]nested.SourceChange, len(changes))
	for i, change := range changes {
		nestedChanges[i] = nested.SourceChange{URI: change.URI, Kind: int(change.Kind)}
	}
	return b.conn.NotifySourceChanges(ctx, nestedChanges)
}

func (b *Backend) RecoverSourceChanges(ctx context.Context) error {
	if b == nil || b.conn == nil {
		return ierrors.New(ierrors.ErrBackendUnavailable, serverName, "backend is unavailable")
	}
	return b.conn.RecoverSourceChanges(ctx)
}
