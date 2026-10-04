package ccls

import (
	"context"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/nested"
)

func (b *Backend) NotifySourceChanges(ctx context.Context, changes []languages.SourceChange) error {
	normalized, err := languages.NormalizeSourceChanges(changes)
	if err != nil || len(normalized) == 0 {
		return err
	}
	nestedChanges := make([]nested.SourceChange, len(normalized))
	for i, change := range normalized {
		nestedChanges[i] = nested.SourceChange{URI: change.URI, Kind: int(change.Kind)}
	}
	return b.conn.NotifySourceChanges(ctx, nestedChanges)
}

func (b *Backend) RecoverSourceChanges(ctx context.Context) error {
	return b.conn.RecoverSourceChanges(ctx)
}
