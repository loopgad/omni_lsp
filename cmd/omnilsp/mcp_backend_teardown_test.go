package main

// 审计批 2 发现 2 行为守卫：serveMCP 的收尾此前只覆盖插件腿（ClosePlugins）。
// 嵌套后端唯一的拆除点是 server.Run defer 链里的 closeBackends（优雅
// shutdown→exit 握手 + kill 兜底），而 MCP 分支不经过 Run——四个构造期
// StartSupervised 拉起的语言后端在 MCP 会话结束时收不到拆除，子进程靠宿主
// 退出后的管道 EOF 收场。diag.Close、scheduler.Shutdown 同样缺席。
//
// 守卫以真实 Server + 可观测 fake backend 直呼 serveMCP：stdin 立即 EOF，
// Serve 返回后 fake backend 的 Close 必须已被调用。红：旧实现只调
// ClosePlugins，Close 永不发生。

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/runtime/server"
)

type mcpTeardownBackend struct {
	languages.Backend
	closeCalls atomic.Int32
}

func (b *mcpTeardownBackend) Close() error {
	b.closeCalls.Add(1)
	return nil
}

func TestMCPTransportTearsRegisteredBackendsDown(t *testing.T) {
	cfg := config.Default()
	cfg.WorkspaceDir = t.TempDir()
	srv := server.New(serveConfig(cfg, ""))

	be := &mcpTeardownBackend{}
	srv.RegisterBackend("testlang", be)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// stdin 立即 EOF：MCP 会话即刻结束，随后必须执行会话收尾。
	if err := serveMCP(ctx, srv, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("serveMCP: %v", err)
	}

	if got := be.closeCalls.Load(); got == 0 {
		t.Fatal("MCP 会话结束后注册后端未被拆除（Close 未被调用）——serveMCP 收尾缺 closeBackends 腿（审计批 2 发现 2）")
	}
}
