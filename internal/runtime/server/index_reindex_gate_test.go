package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 复证圈 2 审计发现 5 的守卫：omnilsp/reindex legacy 路径的 F13 阶梯门必须
// 在内存物化之前布防。旧实现先 inventory 全量走完（Files 计满）→
// inventoryPayload 一次性 json.Marshal 全部 records → 才 CanAllocate——
// 门防不住它要门控的那次分配。修复：inventory 流式按累计估算超限即中止
// （fail-closed 可见拒绝），CanAllocate 前移到 inventoryPayload 之前。
func TestF13_ReindexRefusesBeforeMaterialization(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	t.Setenv("OMNILSP_INDEX_MEMORY_BUDGET_BYTES", "8192")
	root := t.TempDir()
	const total = 200
	for i := 0; i < total; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("f%03d.go", i)), []byte("package p\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := initializedIndexServer(t, root)

	resp := indexRequest(s, context.Background(), "omnilsp/reindex")
	if resp == nil || resp.Error == nil {
		t.Fatal("超预算的 reindex 必须被可见拒绝")
	}
	stats := indexStats(t, s)
	if stats.LastRebuild.Files >= total {
		t.Fatalf("拒绝发生在 inventory 全量完成之后（LastRebuild.Files=%d/%d）：F13 门防不住它要门控的那次物化", stats.LastRebuild.Files, total)
	}
	if stats.LastRebuild.Error == "" {
		t.Fatal("拒绝必须可见（LastRebuild.Error）")
	}
}
