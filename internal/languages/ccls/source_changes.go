package ccls

import (
	"context"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/languages/nested"
)

func (b *Backend) NotifySourceChanges(ctx context.Context, changes []languages.SourceChange) error {
	// External file events change clangd's compilation world (headers on
	// disk feed preambles and the index) before any nested bookkeeping
	// reflects them; drop memoized answers up front, conservatively even
	// if the notification below ends up rejected or empty.
	b.memo.invalidate()
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
	// Recovery replaces the worker process: every memoized answer belonged
	// to a predecessor clangd world.
	b.memo.invalidate()
	return b.conn.RecoverSourceChanges(ctx)
}
