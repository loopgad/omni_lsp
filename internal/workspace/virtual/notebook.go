package virtual

import (
	ierrors "github.com/omnilsp/omni/internal/errors"
)

// Cell is one notebook cell identity (goal.md §D13): per-cell URI, language,
// and version.
type Cell struct {
	URI        string
	LanguageID string
	Version    uint32
}

// Notebook is a notebook document's ordered cell list. Generic VFS/registry
// code stores the order as given; semantic ordering is language/backend-
// specific and MUST NOT be guessed here (§D13). Execution metadata is
// deliberately absent — optional and backend-owned.
type Notebook struct {
	OrderedCells []Cell
}

// RegisterNotebook stores nb under id, replacing any previous version.
func (r *Registry) RegisterNotebook(id string, nb Notebook) error {
	if id == "" {
		return ierrors.New(ierrors.ErrInvalidArgument, "virtual.RegisterNotebook", "empty notebook ID")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cells := make([]Cell, len(nb.OrderedCells))
	copy(cells, nb.OrderedCells)
	r.notebooks[id] = Notebook{OrderedCells: cells}
	return nil
}

// Notebook returns the stored notebook for id.
func (r *Registry) Notebook(id string) (Notebook, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	nb, ok := r.notebooks[id]
	return nb, ok
}

// UnregisterNotebook removes the notebook stored under id.
func (r *Registry) UnregisterNotebook(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.notebooks, id)
}
