package server

import (
	"testing"

	"github.com/omnilsp/omni/internal/workspace/virtual"
)

// TestNotebooksDoNotSurviveCloseSession pins bug round 23 A / BUG-2 at the
// session level: CloseSession's teardown backstop (UnregisterAll, bug round 20
// A#11 圈 4 — "nothing survives into the next session") swept only the
// host↔virtual table; a notebook an embedder registered via
// VirtualRegistry().RegisterNotebook stayed queryable after CloseSession.
// The round-20 pin (virtual_cleanup_test.go leg 3) covered the virtual table
// only — this closes the notebook leg.
func TestNotebooksDoNotSurviveCloseSession(t *testing.T) {
	s := New(DefaultConfig())
	reg := s.VirtualRegistry()
	if err := reg.RegisterNotebook("nb:///probe.ipynb", notebookFixture()); err != nil {
		t.Fatal(err)
	}

	if err := s.CloseSession(); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	// CloseSession is idempotent (audit batch 2, finding 2); a second call
	// must not resurrect or mask anything either.
	if err := s.CloseSession(); err != nil {
		t.Fatalf("second CloseSession: %v", err)
	}

	if _, ok := reg.Notebook("nb:///probe.ipynb"); ok {
		t.Error("notebook SURVIVED CloseSession — teardown backstop missed the notebook table")
	}
}

func notebookFixture() (nb virtual.Notebook) {
	nb.OrderedCells = []virtual.Cell{{URI: "nb:///probe.ipynb#cell-1", LanguageID: "python", Version: 1}}
	return
}
