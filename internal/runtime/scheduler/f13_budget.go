package scheduler

import "sync"

// BudgetCategory identifies a memory usage bucket per goal.md §F13.
type BudgetCategory int

const (
	CatDocumentText BudgetCategory = iota
	CatLineIndex
	CatSyntaxTrees
	CatQueryCache
	CatDynamicIndex
	CatSnapshots
	CatBackendRSS
	CatRPCBuffers

	// CatTotal is a sentinel bound, not a bucket: it must stay outside the
	// iota sequence or it collides with CatRPCBuffers (historical bug).
	CatTotal BudgetCategory = CatRPCBuffers + 1
)

// BudgetThresholds defines the pressure levels per goal.md §F13.
type BudgetThresholds struct {
	Soft     float64
	Medium   float64
	High     float64
	Critical float64
}

func DefaultBudgetThresholds() BudgetThresholds {
	return BudgetThresholds{Soft: 0.70, Medium: 0.80, High: 0.90, Critical: 1.0}
}

// BudgetManager tracks per-category memory and enforces thresholds (F13).
type BudgetManager struct {
	mu         sync.Mutex
	limits     map[BudgetCategory]int64
	usage      map[BudgetCategory]int64
	thresholds BudgetThresholds
	totalUsed  int64
	totalLimit int64
}

func NewBudgetManager(thresholds BudgetThresholds) *BudgetManager {
	if thresholds.Soft == 0 {
		thresholds = DefaultBudgetThresholds()
	}
	bm := &BudgetManager{
		limits:     make(map[BudgetCategory]int64),
		usage:      make(map[BudgetCategory]int64),
		thresholds: thresholds,
	}
	for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
		bm.limits[cat] = 1 << 30
	}
	bm.totalLimit = 0
	for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
		bm.totalLimit += bm.limits[cat]
	}
	return bm
}

func (bm *BudgetManager) RecordUsage(cat BudgetCategory, bytes int64) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.usage[cat] += bytes
	bm.totalUsed += bytes
}

func (bm *BudgetManager) ReleaseUsage(cat BudgetCategory, bytes int64) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	if bm.usage[cat] >= bytes {
		bm.usage[cat] -= bytes
		bm.totalUsed -= bytes
	}
}

func (bm *BudgetManager) Pressure() float64 {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	if bm.totalLimit == 0 {
		return 0
	}
	return float64(bm.totalUsed) / float64(bm.totalLimit)
}

func (bm *BudgetManager) CategoryPressure(cat BudgetCategory) float64 {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	limit, ok := bm.limits[cat]
	if !ok || limit == 0 {
		return 0
	}
	return float64(bm.usage[cat]) / float64(limit)
}

// State maps total pressure onto the goal.md §F13 ladder:
//
//	< Soft (0.70)          normal
//	>= Soft (0.70)         evict    — evict cold caches
//	>= Medium (0.80)       throttle — pause/throttle background work
//	>= High (0.90)         reject   — reject expensive work / emergency eviction
//	>= Critical (1.0)      critical — budget exhausted
//
// The band boundaries were shifted one tier up when this manager was written
// (80% -> evict, 90% -> throttle, refusal only at 100%) while the Soft
// threshold was consumed by no branch at all; TestBudgetManagerPressureLadder
// pins the clause mapping above.
func (bm *BudgetManager) State() string {
	p := bm.Pressure()
	switch {
	case p >= bm.thresholds.Critical:
		return "critical"
	case p >= bm.thresholds.High:
		return "reject"
	case p >= bm.thresholds.Medium:
		return "throttle"
	case p >= bm.thresholds.Soft:
		return "evict"
	default:
		return "normal"
	}
}

// CanAllocate reports whether an allocation of n bytes is admissible. Per
// goal.md §F13 the >= 90% band (High) rejects expensive work, so any
// allocation that would land total pressure at or above High is refused —
// rejection deferred to Critical(1.0) left the whole 90-100% band admissible
// and contradicted the clause.
func (bm *BudgetManager) CanAllocate(n int64) bool {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	if bm.totalLimit == 0 {
		return true
	}
	return float64(bm.totalUsed+n)/float64(bm.totalLimit) < bm.thresholds.High
}

func (bm *BudgetManager) SetLimit(cat BudgetCategory, bytes int64) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.limits[cat] = bytes
	bm.totalLimit = 0
	for cat := CatDocumentText; cat <= CatRPCBuffers; cat++ {
		if v, ok := bm.limits[cat]; ok {
			bm.totalLimit += v
		}
	}
}

// UsageSnapshot returns a copy of current per-category usage for telemetry (P2).
func (bm *BudgetManager) UsageSnapshot() map[BudgetCategory]int64 {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	out := make(map[BudgetCategory]int64, len(bm.usage))
	for k, v := range bm.usage {
		out[k] = v
	}
	return out
}
