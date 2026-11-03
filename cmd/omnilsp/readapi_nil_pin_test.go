package main

import (
	"context"
	"testing"
)

// TestHTTPCoreNilServerFailsClosed pin：sharedCore 无活跃会话（进程启动后
// 首个 TCP 会话之前、以及会话终局 Store(nil) 之后的空窗）时，HTTP read API
// 必须 fail-closed——/ready 报告未就绪（httpserver 层映射 503）、查询返回
// 错误（httpserver writeEnvelope 对 Core 错误映射 5xx）、Status 带错误键。
// 修复前 nil 会话核心在 Ready/Status/Hover 上直接 nil 解引用 panic。
func TestHTTPCoreNilServerFailsClosed(t *testing.T) {
	var core httpCore // srv 从未 Store：等价于会话终局 Store(nil) 之后的空窗

	if core.Ready() {
		t.Fatal("nil 会话核心不得报告 Ready（断连后 /ready 必须 fail-closed，不得假 Running）")
	}
	st := core.Status()
	if _, ok := st["error"]; !ok {
		t.Fatalf("nil 会话核心的 Status 必须带 error 键，got %v", st)
	}
	if _, err := core.Hover(context.Background(), "file:///a.go", 0, 0); err == nil {
		t.Fatal("nil 会话核心的 Hover 必须报错（fail-closed）")
	}
	if _, err := core.Definition(context.Background(), "file:///a.go", 0, 0); err == nil {
		t.Fatal("nil 会话核心的 Definition 必须报错（fail-closed）")
	}
	if _, err := core.References(context.Background(), "file:///a.go", 0, 0, false); err == nil {
		t.Fatal("nil 会话核心的 References 必须报错（fail-closed）")
	}
}
