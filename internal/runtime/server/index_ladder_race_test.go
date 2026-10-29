package server

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/runtime/scheduler"
)

// 复证圈 1 审计发现 6/9 的守卫。

// TestF13_LadderRefresh_BoundedUnderHeldWriterLock（发现 9）：refreshBudgetLadder
// 以无 deadline 的 ctx 打开快照 lease 时不得无限期等待——writer.lock 被占
// （另一进程 reindex/Compact 的长临界区）时必须在有界时间内返回并走既有
// fail-open 分支（保留上次用量），否则 opt-in 选入后的 initialize 握手被
// 无限期阻塞（挂起而非泄漏）。修复形态：函数内部对无 deadline 的 ctx 包
// 一层 indexLadderRefreshTimeout（与审计 fixSketch 的「调用点改传带超时
// ctx」目标一致，落在可测的函数边界上——调用点上游 NewFileStore 构造同样
// 以 Background 抢 writer.lock，属点名文件之外的第二个等待点，见交付说明）。
func TestF13_LadderRefresh_BoundedUnderHeldWriterLock(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	t.Setenv("OMNILSP_INDEX_MEMORY_BUDGET_BYTES", "1048576")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package p"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := initializedIndexServer(t, root)
	if s.idx == nil || s.idx.ladder == nil {
		t.Fatal("ladder must be constructed when OMNILSP_INDEX_MEMORY_BUDGET_BYTES is set")
	}
	if resp := indexRequest(s, context.Background(), "omnilsp/reindex"); resp == nil || resp.Error != nil {
		t.Fatalf("reindex: %+v", resp)
	}
	live := s.idx.ladder.UsageSnapshot()[scheduler.CatDynamicIndex]
	if live <= 0 {
		t.Fatalf("基线用量必须为正, got %d", live)
	}

	// 以与 persistent 相同的锁定原语占住 writer.lock（模拟另一进程持锁）。
	release, err := holdExclusiveFileLock(filepath.Join(s.idx.dir, "writer.lock"))
	if err != nil {
		t.Skipf("测试环境无法制造 writer.lock 竞争（锁原语不可用）: %v", err)
	}
	defer release()

	old := indexLadderRefreshTimeout
	indexLadderRefreshTimeout = 400 * time.Millisecond
	t.Cleanup(func() { indexLadderRefreshTimeout = old })

	started := time.Now()
	done := make(chan struct{})
	go func() {
		s.idx.refreshBudgetLadder(context.Background()) // 无 deadline 的调用形态（修复前的调用点语义）
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("refreshBudgetLadder(context.Background()) 在 writer.lock 被占时 %v 内未返回（10ms 轮询仅响应 ctx.Done()，Background 永不 Done——initialize 握手被无限期阻塞）", time.Since(started))
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("refresh 返回耗时 %v，超出有界窗口", elapsed)
	}
	// fail-open 语义闭合：等待失败保留上次用量，不产生漂移。
	if got := s.idx.ladder.UsageSnapshot()[scheduler.CatDynamicIndex]; got != live {
		t.Fatalf("锁竞争超时后 usage = %d, want 保留上次用量 %d（fail-open）", got, live)
	}
	// 锁释放后恢复记账能力。
	release()
	s.idx.refreshBudgetLadder(context.Background())
	if got := s.idx.ladder.UsageSnapshot()[scheduler.CatDynamicIndex]; got != live {
		t.Fatalf("锁释放后 usage = %d, want %d（记账未恢复）", got, live)
	}
}

// TestF13_LadderRefresh_ConcurrentWriterKeepsAccountingExact（发现 6）：refresh
// 与第二类 CatDynamicIndex 记账方并发之后，记账必须在静止点严格一致——
// usage == live 且 Pressure 的分子（totalUsed）与 usage 一致。
//
// 诚实注记：三步读-改-写（UsageSnapshot→ReleaseUsage→RecordUsage）的 TOCTOU
// 窗口在本测试中只能以并发压力逼近、无法确定性命中（窗口在两次持锁之间，
// 无生产侧钩子）；确定性红证据由 scheduler 包的 TestReplaceUsage_RebaseIsAtomicAndExact
// 承担（新原语缺失 = 编译红）。本测试承担重构后的记账精确性回归钉。
func TestF13_LadderRefresh_ConcurrentWriterKeepsAccountingExact(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	t.Setenv("OMNILSP_INDEX_MEMORY_BUDGET_BYTES", "1048576")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source.go"), []byte("package p"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := initializedIndexServer(t, root)
	if s.idx == nil || s.idx.ladder == nil {
		t.Fatal("ladder must be constructed when OMNILSP_INDEX_MEMORY_BUDGET_BYTES is set")
	}
	if resp := indexRequest(s, context.Background(), "omnilsp/reindex"); resp == nil || resp.Error != nil {
		t.Fatalf("reindex: %+v", resp)
	}
	bm := s.idx.ladder
	live := bm.UsageSnapshot()[scheduler.CatDynamicIndex]
	if live <= 0 {
		t.Fatalf("基线用量必须为正, got %d", live)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // 第二类 CatDynamicIndex 记账方（净贡献 0 的配对记账）
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			bm.RecordUsage(scheduler.CatDynamicIndex, 1)
			bm.ReleaseUsage(scheduler.CatDynamicIndex, 1)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			s.idx.refreshBudgetLadder(context.Background())
		}
	}()
	wg.Wait()

	s.idx.refreshBudgetLadder(context.Background())
	usage := bm.UsageSnapshot()[scheduler.CatDynamicIndex]
	if usage != live {
		t.Fatalf("并发静止后 usage = %d, want live=%d（记账漂移）", usage, live)
	}
	if got := int64(bm.Pressure() * float64(1048576)); got != usage {
		t.Fatalf("并发静止后 totalUsed（Pressure 分子）= %d, usage = %d——读-改-写撕裂", got, usage)
	}
}
