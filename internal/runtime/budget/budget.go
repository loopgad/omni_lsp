// Package budget implements the §F13 memory budget manager and §F14
// per-backend quotas: a global soft limit that warns (visible, non-fatal)
// and a hard limit plus per-owner quotas that refuse allocation with
// ErrOverloaded.
//
// Concurrency model: one mutex guards the counters; Reserve/Release are
// O(1). Owners are language backend names or "server".
//
// Invariants:
//  1. Hard limit is absolute: total usage never exceeds it through this API.
//  2. A quota refusal for one owner never blocks another (§F14 isolation).
//  3. Release below zero is a caller bug — clamped, never negative totals.
package budget

import (
	"fmt"
	"sync"

	ierrors "github.com/omnilsp/omni/internal/errors"
)

// Manager tracks byte budgets: global soft/hard limits plus per-owner quotas.
type Manager struct {
	mu         sync.Mutex
	soft, hard int64
	total      int64
	used       map[string]int64
	quota      map[string]int64
}

// New builds a manager with global soft and hard limits in bytes.
func New(globalSoft, globalHard int64) *Manager {
	return &Manager{
		soft:  globalSoft,
		hard:  globalHard,
		used:  map[string]int64{},
		quota: map[string]int64{},
	}
}

// SetQuota caps one owner independently of the global budget (§F14).
func (m *Manager) SetQuota(owner string, limit int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quota[owner] = limit
}

// Reserve reports whether owner may allocate n bytes. Refusals wrap
// errors.ErrOverloaded so callers classify them uniformly (§Q0).
func (m *Manager) Reserve(owner string, n int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q, ok := m.quota[owner]; ok && m.used[owner]+n > q {
		return ierrors.New(ierrors.ErrOverloaded, "budget.reserve",
			fmt.Sprintf("owner %q quota %d exceeded (%d used + %d requested)", owner, q, m.used[owner], n))
	}
	if m.total+n > m.hard {
		return ierrors.New(ierrors.ErrOverloaded, "budget.reserve",
			fmt.Sprintf("global hard limit %d exceeded (%d used + %d requested)", m.hard, m.total, n))
	}
	m.total += n
	m.used[owner] += n
	return nil
}

// Release returns n bytes previously reserved by owner.
func (m *Manager) Release(owner string, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n > m.used[owner] {
		n = m.used[owner] // clamp; under-release is a caller bug, not a panic
	}
	if n > m.total {
		n = m.total
	}
	m.total -= n
	m.used[owner] -= n
}

// Usage reports global consumption and whether the soft limit is crossed.
func (m *Manager) Usage() (total int64, softExceeded bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total, m.soft > 0 && m.total > m.soft
}
