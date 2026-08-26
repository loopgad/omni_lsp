package budget

import (
	"errors"
	"testing"

	ierrors "github.com/omnilsp/omni/internal/errors"
)

func isOverloaded(err error) bool {
	var e *ierrors.Error
	return errors.As(err, &e) && e.Kind == ierrors.ErrOverloaded
}

// TestF13_SoftLimitWarnsHardLimitRefuses pins §F13: the soft limit only
// marks; the hard limit refuses with ErrOverloaded.
func TestF13_SoftLimitWarnsHardLimitRefuses(t *testing.T) {
	m := New(100, 200)
	if err := m.Reserve("server", 80); err != nil {
		t.Fatalf("below soft limit refused: %v", err)
	}
	if _, exceeded := m.Usage(); exceeded {
		t.Error("soft limit flagged before crossing")
	}
	if err := m.Reserve("server", 30); err != nil {
		t.Fatalf("between soft and hard refused: %v", err)
	}
	if _, exceeded := m.Usage(); !exceeded {
		t.Error("soft limit not flagged after crossing")
	}
	err := m.Reserve("server", 200) // would blow the 200-byte hard cap
	if !isOverloaded(err) {
		t.Errorf("hard-limit refusal = %v, want ErrOverloaded", err)
	}

	m.Release("server", 110)
	if err := m.Reserve("server", 50); err != nil {
		t.Errorf("reserve after release failed: %v", err)
	}
}

// TestF14_PerBackendQuotaIsolated pins §F14: one owner exhausting its quota
// never blocks another.
func TestF14_PerBackendQuotaIsolated(t *testing.T) {
	m := New(1000, 1000)
	m.SetQuota("gopls", 100)
	m.SetQuota("pyright", 100)

	if err := m.Reserve("gopls", 100); err != nil {
		t.Fatalf("gopls within quota refused: %v", err)
	}
	if err := m.Reserve("gopls", 1); !isOverloaded(err) {
		t.Errorf("gopls over quota = %v, want refusal", err)
	}
	if err := m.Reserve("pyright", 100); err != nil {
		t.Errorf("pyright blocked by gopls exhaustion: %v", err)
	}
	if err := m.Reserve("unowned-backend", 500); err != nil {
		t.Errorf("owner without quota should only face global limits: %v", err)
	}
}

// TestF13_ReleaseRestoresBudget verifies release semantics including clamping.
func TestF13_ReleaseRestoresBudget(t *testing.T) {
	m := New(50, 50)
	if err := m.Reserve("a", 50); err != nil {
		t.Fatal(err)
	}
	if err := m.Reserve("b", 1); !isOverloaded(err) {
		t.Fatal("full budget accepted more reservations")
	}
	m.Release("a", 25)
	if err := m.Reserve("b", 20); err != nil {
		t.Errorf("post-release reserve refused: %v", err)
	}
	m.Release("a", 999) // over-release clamps, never goes negative
	total, _ := m.Usage()
	if total < 0 {
		t.Errorf("negative total %d after over-release", total)
	}
}
