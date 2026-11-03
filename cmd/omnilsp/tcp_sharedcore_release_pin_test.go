package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/config"
	"github.com/omnilsp/omni/internal/runtime/server"
)

// TestTCPSessionEndReleasesSharedCore pin：TCP 会话以非 exit 握手收尾
// （此处为 ctx 取消，与生产上的 EOF/传输故障同属 Run 返回路径）后，
// sharedCore 必须统一摘到 nil。否则 HTTP read API 在空窗期继续指向死会话：
// /ready 假 Running、hover/definition/references 打到死核心报后端错，直到
// 下一次 accept 的 Store 覆盖。exit 握手路径已由 StateExited 让 Ready=false，
// Store(nil) 对其幂等；非 exit 收尾只有这里一条摘除路径。TCP 一次一会话
// （v1 语义），摘除与下一次 Store 之间无并发竞争。
func TestTCPSessionEndReleasesSharedCore(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	serverConn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	sessionSrv := server.New(server.DefaultConfig())
	sharedCore.srv.Store(sessionSrv)
	defer sharedCore.srv.Store(nil) // 测试自身的全局状态复位，不依赖被测行为

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTCPSession(ctx, 0, serverConn, sessionSrv, config.Config{}, "")
	}()

	// Let the session settle into its read loop, then cancel the context:
	// Run returns ctx.Err() — a non-exit session end (与
	// TestTCPSessionClosesConnWhenRunReturns 同一驱动方式)。
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runTCPSession did not return after cancellation")
	}

	if got := sharedCore.srv.Load(); got != nil {
		t.Fatal("非 exit 收尾后 sharedCore 仍指向死会话：/ready 假 Running、查询打后端错")
	}
}
