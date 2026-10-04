package languages

import (
	"context"
	"fmt"

	workspaceuri "github.com/omnilsp/omni/internal/workspace/uri"
)

// SourceChangeKind matches the LSP FileChangeType values.
type SourceChangeKind uint8

const (
	SourceChangeCreated SourceChangeKind = 1
	SourceChangeChanged SourceChangeKind = 2
	SourceChangeDeleted SourceChangeKind = 3
)

// SourceChange identifies a filesystem change visible to a backend.
type SourceChange struct {
	URI  string
	Kind SourceChangeKind
}

// NormalizeSourceChanges validates file URIs and replaces their display
// spellings with canonical identities. Event order and duplicates are kept:
// delete/create pairs may represent atomic replacement or rename.
func NormalizeSourceChanges(changes []SourceChange) ([]SourceChange, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	normalized := make([]SourceChange, len(changes))
	for i, change := range changes {
		if change.Kind < SourceChangeCreated || change.Kind > SourceChangeDeleted {
			return nil, fmt.Errorf("invalid source change kind %d", change.Kind)
		}
		parsed, err := workspaceuri.Parse(change.URI)
		if err != nil || !parsed.IsFile() {
			if err != nil {
				return nil, fmt.Errorf("invalid source change URI %q: %w", change.URI, err)
			}
			return nil, fmt.Errorf("source change URI %q is not a file URI", change.URI)
		}
		normalized[i] = SourceChange{URI: parsed.Canonical(), Kind: change.Kind}
	}
	return normalized, nil
}

// SourceChangeSynchronizer is an optional capability for backends that can
// forward external filesystem changes to their semantic process.
// Cancellation before delivery leaves the batch unsent; successful delivery
// wins over late cancellation. Transport failures must retain their own error.
type SourceChangeSynchronizer interface {
	NotifySourceChanges(ctx context.Context, changes []SourceChange) error
}

// SourceChangeRecoverer can replace a child whose bounded invalidation queue
// lost events. Success proves the old semantic state was retired and a fresh
// supervised epoch completed initialization (or no child has ever existed).
// An in-progress recovery must not repeatedly restart the same epoch.
type SourceChangeRecoverer interface {
	RecoverSourceChanges(ctx context.Context) error
}
