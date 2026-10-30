package server

// 审计批 2 发现 2 守卫：MCP 分支不经过 Run，Run 的 defer 链（scheduler →
// ClosePlugins → closeBackends → diag.Close）在该路径不执行——closeBackends
// 是嵌套后端唯一的拆除点（优雅 shutdown→exit 握手 + kill 兜底），四个语言
// 后端构造期即 StartSupervised 拉起，MCP 会话结束时缺了这条腿后端子进程只
// 能靠宿主退出的管道 EOF 收场。修复形态：Run 的 defer 尾链提取为统一收尾
// CloseSession，Run 与 MCP 分支共用同一条链、同一顺序、同一幂等性。
//
// 本文件钉 CloseSession 本身：注册后端必须被 reap、二次调用幂等
// （backendCloseOnce 保证不重复拆）。

import (
	"sync/atomic"
	"testing"
)

type closeTrackingBackend struct {
	mockBackend
	closeCalls atomic.Int32
}

func (b *closeTrackingBackend) Close() error {
	b.closeCalls.Add(1)
	return nil
}

func TestCloseSessionReapsBackendsAndIsIdempotent(t *testing.T) {
	s := New(DefaultConfig())
	be := &closeTrackingBackend{mockBackend: mockBackend{langID: "testlang", exts: []string{".tl"}}}
	s.RegisterBackend("testlang", be)

	if err := s.CloseSession(); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if got := be.closeCalls.Load(); got != 1 {
		t.Fatalf("backend Close calls after first CloseSession = %d, want 1 (session teardown must reap registered backends)", got)
	}

	// 幂等：二次收尾不得重复拆除（backendCloseOnce 语义，与 Run defer 链
	// 上 closeBackends 的既有行为一致）。
	if err := s.CloseSession(); err != nil {
		t.Fatalf("second CloseSession: %v", err)
	}
	if got := be.closeCalls.Load(); got != 1 {
		t.Fatalf("backend Close calls after second CloseSession = %d, want 1 (teardown must be idempotent)", got)
	}
}
