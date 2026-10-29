package scheduler

import "testing"

// 复证圈 1 审计发现 6 的守卫：ReplaceUsage 在同一次持锁内完成类别级重置
// （rebase：扣旧值 → 记新值），消除 refreshBudgetLadder 旧实现
// UsageSnapshot→ReleaseUsage→RecordUsage 三步读-改-写分离的 TOCTOU 窗口——
// 任何第二类 CatDynamicIndex 记账方与 refresh 并发即产生用量漂移。
// 新原语缺失即编译红（本测试即发现 6 的确定性红证据）。
func TestReplaceUsage_RebaseIsAtomicAndExact(t *testing.T) {
	bm := NewBudgetManager(DefaultBudgetThresholds())
	const total = int64(1 << 20)
	bm.SetTotalBudget(total)

	bm.RecordUsage(CatDynamicIndex, 100)
	if got := bm.UsageSnapshot()[CatDynamicIndex]; got != 100 {
		t.Fatalf("RecordUsage(100) 后 usage = %d, want 100", got)
	}

	bm.ReplaceUsage(CatDynamicIndex, 40)
	if got := bm.UsageSnapshot()[CatDynamicIndex]; got != 40 {
		t.Fatalf("ReplaceUsage(40) 后 usage = %d, want 40", got)
	}
	if got := int64(bm.Pressure() * float64(total)); got != 40 {
		t.Fatalf("ReplaceUsage(40) 后 totalUsed（Pressure 分子）= %d, want 40（扣旧值与记新值必须同一持锁完成）", got)
	}

	// ErrNoGeneration 清零路径：ReplaceUsage(0) 不得留负漂移。
	bm.ReplaceUsage(CatDynamicIndex, 0)
	if got := bm.UsageSnapshot()[CatDynamicIndex]; got != 0 {
		t.Fatalf("ReplaceUsage(0) 后 usage = %d, want 0", got)
	}
	if got := int64(bm.Pressure() * float64(total)); got != 0 {
		t.Fatalf("清零后 totalUsed = %d, want 0（不得出现负漂移）", got)
	}

	// 重置后 Record 照常累积（原语面行为不变）。
	bm.RecordUsage(CatDynamicIndex, 7)
	if got := bm.UsageSnapshot()[CatDynamicIndex]; got != 7 {
		t.Fatalf("重置后 RecordUsage(7) usage = %d, want 7", got)
	}
}
