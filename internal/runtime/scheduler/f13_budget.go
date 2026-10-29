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
	// totalBudget is an explicit §F13 total budget set via SetTotalBudget.
	// Zero (the zero value) keeps the historical behavior: the ladder runs
	// against totalLimit, the sum of the per-category limits.
	totalBudget int64
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

// ReplaceUsage 在同一次持锁内把 cat 的记账重置（rebase）为 bytes：扣旧值与
// 记新值原子完成（复证圈 1 审计发现 6）。旧实现的
// UsageSnapshot→ReleaseUsage→RecordUsage 三步读-改-写分离存在 TOCTOU 窗口——
// 任何第二类 CatDynamicIndex 记账方与 refresh 并发即产生用量漂移：并发方的
// 贡献被按过期快照值的释放一并抹掉，或 ReleaseUsage 的下限守卫误跳过使旧
// 用量重复计入（§F13 压力档误判）。
func (bm *BudgetManager) ReplaceUsage(cat BudgetCategory, bytes int64) {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	old := bm.usage[cat]
	bm.usage[cat] = bytes
	bm.totalUsed += bytes - old
}

func (bm *BudgetManager) Pressure() float64 {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	budget := bm.ladderBudget()
	if budget == 0 {
		return 0
	}
	return float64(bm.totalUsed) / float64(budget)
}

// ladderBudget returns the total the pressure ladder runs against: the
// explicit SetTotalBudget value when one was set, otherwise the sum of the
// per-category limits (caller holds bm.mu).
func (bm *BudgetManager) ladderBudget() int64 {
	if bm.totalBudget > 0 {
		return bm.totalBudget
	}
	return bm.totalLimit
}

// SetTotalBudget pins the §F13 total budget explicitly, overriding the sum
// of per-category limits as the ladder's denominator. Category limits keep
// bounding their own categories (CategoryPressure is unchanged); only the
// total pressure, State and CanAllocate read this budget. Zero restores the
// sum-of-limits denominator. This is the hook the server's index-budget
// wiring uses to run the goal.md §F13 ladder against a configured memory
// budget instead of the constructor's 1 GiB-per-category defaults.
func (bm *BudgetManager) SetTotalBudget(bytes int64) {
	if bytes < 0 {
		bytes = 0
	}
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.totalBudget = bytes
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
	budget := bm.ladderBudget()
	if budget == 0 {
		return true
	}
	return float64(bm.totalUsed+n)/float64(budget) < bm.thresholds.High
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
