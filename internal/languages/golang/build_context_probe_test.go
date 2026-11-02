package golang

// round20 第 3 圈：deriveBuildContext 的 `go env` 探针（backend.go）是请求
// 路径上唯一无 ctx、无超时的子进程调用（同文件 semantic_index.go 的探针均
// 用 CommandContext）。它藏在 buildCtxOnce 后：探针挂起（GOWORK 指向停摆
// 网络挂载、杀软扫描停顿）时，后续所有需要 build context 的请求无限期阻塞
// 在 Once 上。修复：CommandContext + buildContextProbeTimeout（默认 10s），
// 超时落回既有的 "go:sha256:unavailable" 兜底。
//
// 红由变异实验证明：把 CommandContext 退回 exec.Command 后本测试在假 go 的
// 20s 挂起上超距失败（elapsed 断言）。假 go 用测试二进制自拷贝：CommandContext
// 直接杀掉它（无 cmd.exe 包装孙进程残留），管道随之关闭、Output 立即返回。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/identity"
)

// TestMain 让同一测试二进制可以假扮挂起的 `go`：设置环境变量后它睡 20s 才
// 退出（探针在 200ms 处杀掉它）。
func TestMain(m *testing.M) {
	if os.Getenv("OMNILSP_FAKE_GO_HANG") != "" {
		time.Sleep(20 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestBuildContextProbeCannotHangRequestPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("fake hung `go` is exercised via a copied Windows executable on PATH")
	}
	fakeDir := t.TempDir()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	data, err := os.ReadFile(testBinary)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeDir, "go.exe"), data, 0o755); err != nil {
		t.Fatalf("stage fake go.exe: %v", err)
	}
	t.Setenv("OMNILSP_FAKE_GO_HANG", "1")
	t.Setenv("PATH", fakeDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	orig := buildContextProbeTimeout
	buildContextProbeTimeout = 200 * time.Millisecond
	defer func() { buildContextProbeTimeout = orig }()

	b := New(t.TempDir())
	started := time.Now()
	id := b.BuildContextID()
	elapsed := time.Since(started)
	if elapsed > 2*time.Second {
		t.Fatalf("BuildContextID returned after %v: the `go env` probe has no deadline and hung the request path on buildCtxOnce", elapsed)
	}
	if id != identity.BuildContextID("go:sha256:unavailable") {
		t.Fatalf("BuildContextID = %q after a timed-out probe, want the unavailable fallback", id)
	}
}
