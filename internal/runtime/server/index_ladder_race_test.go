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

// 复证圈 2 审计发现 8/2（并合并圈 2 发现 11 同一残留）的守卫。
//
// 与圈 1 守卫（TestF13_LadderRefresh_BoundedUnderHeldWriterLock，已随本轮
// 语义切割删除）的替换说明：圈 1 把 5s 有界包裹加在 refreshBudgetLadder 的
// OpenSnapshotLease 调用上；本轮发现该界「包错了段」——OpenSnapshotLease 为
// 求段长总和做全代读盘+CRC 且独占跨进程 writer.lock，校验循环对 deadline 盲。
// 修复改为 manifest 只读口径（FileStore.CurrentGenerationLenSum，s.mu 下对
// current 段长求和，不读盘、不校验、不碰锁），refreshBudgetLadder 不再等待
// 任何跨进程锁；跨进程等待点收敛到 NewFileStore 构造（发现 2，由
// TestInitIndex_BoundedUnderHeldWriterLock 钉住）。

// TestF13_LadderRefresh_UpdatesUnderHeldWriterLock：writer.lock 被另一进程
// 持有时，refreshBudgetLadder 仍必须能按 current 口径纠偏用量——旧实现为求
// 段长走 OpenSnapshotLease（抢锁+全代校验），锁下等满超时后 fail-open 保留
// 漂移用量（人为注入的 99999 不被纠正）。
func TestF13_LadderRefresh_UpdatesUnderHeldWriterLock(t *testing.T) {
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
	// 注入漂移用量：refresh 的职责就是把它纠偏回真实值。
	s.idx.ladder.RecordUsage(scheduler.CatDynamicIndex, 99999)

	release, err := holdExclusiveFileLock(filepath.Join(s.idx.dir, "writer.lock"))
	if err != nil {
		t.Skipf("测试环境无法制造 writer.lock 竞争（锁原语不可用）: %v", err)
	}
	defer release()

	old := indexLadderRefreshTimeout
	indexLadderRefreshTimeout = 300 * time.Millisecond
	t.Cleanup(func() { indexLadderRefreshTimeout = old })

	started := time.Now()
	s.idx.refreshBudgetLadder()
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("refreshBudgetLadder 在 writer.lock 被占时耗时 %v——manifest 只读口径不得等待跨进程锁/全代校验", elapsed)
	}
	if got := s.idx.ladder.UsageSnapshot()[scheduler.CatDynamicIndex]; got != live {
		t.Fatalf("锁被占时 refresh 后 usage = %d, want live=%d（旧实现等锁超时后 fail-open 保留漂移用量；新口径必须直接纠偏）", got, live)
	}
}

// TestF13_LadderRefresh_ConcurrentWriterKeepsAccountingExact（圈 1 守卫延续）：
// refresh 与第二类 CatDynamicIndex 记账方并发之后，记账必须在静止点严格一致
// ——usage == live 且 Pressure 的分子（totalUsed）与 usage 一致。
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
			s.idx.refreshBudgetLadder()
		}
	}()
	wg.Wait()

	s.idx.refreshBudgetLadder()
	usage := bm.UsageSnapshot()[scheduler.CatDynamicIndex]
	if usage != live {
		t.Fatalf("并发静止后 usage = %d, want live=%d（记账漂移）", usage, live)
	}
	if got := int64(bm.Pressure() * float64(1048576)); got != usage {
		t.Fatalf("并发静止后 totalUsed（Pressure 分子）= %d, usage = %d——读-改-写撕裂", got, usage)
	}
}

// TestInitIndex_BoundedUnderHeldWriterLock（发现 2）：initialize 路径上的
// NewFileStore 以 context.Background() 抢 writer.lock，另一进程持锁时 10ms
// 轮询仅响应 ctx.Done()——initialize 握手挂死。修复：构造走带界 ctx，超时
// 走既有 disableIndex fail-open 分支（索引禁用、原因可观测、进程活着）。
func TestInitIndex_BoundedUnderHeldWriterLock(t *testing.T) {
	t.Setenv("OMNILSP_INDEX_MEMORY_BUDGET_BYTES", "1048576")
	indexDir := filepath.Join(t.TempDir(), "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := holdExclusiveFileLock(filepath.Join(indexDir, "writer.lock"))
	if err != nil {
		t.Skipf("测试环境无法制造 writer.lock 竞争（锁原语不可用）: %v", err)
	}
	defer release()

	old := indexLadderRefreshTimeout
	indexLadderRefreshTimeout = 300 * time.Millisecond
	t.Cleanup(func() { indexLadderRefreshTimeout = old })

	cfg := DefaultConfig()
	cfg.IndexDir = indexDir
	s := New(cfg)
	done := make(chan struct{})
	go func() {
		s.InitializeWorkspace(t.TempDir())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("initialize 在 writer.lock 被占时 3s 内未完成——NewFileStore 以 Background 抢锁是无界跨进程等待点")
	}
	if s.idx != nil {
		t.Fatal("NewFileStore 超时必须走 disableIndex fail-open（索引禁用）")
	}
	s.mu.RLock()
	reason := s.idxReason
	s.mu.RUnlock()
	if reason == "" {
		t.Fatal("disableIndex 必须记录失败原因（idxReason 可观测约定）")
	}
}
