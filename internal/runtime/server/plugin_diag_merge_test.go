package server

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/plugin"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// 第 18 轮治理开口 3 守卫：publishDiagnostics 是 uri 级全量替换语义——插件
// 诊断车道与后端 compute 车道对同一 uri 各自发布独立 payload 时，后到的
// 一帧把先到的一帧整体顶掉（客户端只看得见一个来源的诊断）。修复形态是
// 「发布前按 uri 合并」：两个车道的上线点都合并对方当前集（后端缓存集 +
// 全部插件 emit 集），线协议上同一 uri 只有一份合并后的全量报告。

// TestPluginAndBackendDiagnosticsPublishMergedForSameURI：同一 uri 上后端
// 诊断（go 后端，"undefined: foo"）与插件 emit 诊断（宿主强制来源
// plugin:merger）各自触发推送后，最终上线的 publishDiagnostics 必须同时
// 携带两个来源——任何一帧只带其一即合并缺失（红：最后一帧只剩单来源）。
func TestPluginAndBackendDiagnosticsPublishMergedForSameURI(t *testing.T) {
	t.Setenv("OMNILSP_SERVER_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "diag-emit")
	t.Setenv("GO_PLUGIN_EMIT_MESSAGE", "plugin-marker")
	t.Setenv("OMNILSP_TRUST", "trusted")

	const uri = "file:///w/merge.go"
	t.Setenv("GO_PLUGIN_EMIT_URI", uri)

	root := t.TempDir()
	writePluginDir(t, root, "merger", []plugin.Capability{plugin.CapDiagnosticsEmit}, nil)

	s := New(Config{PluginsDir: root, IndexDir: filepath.Join(t.TempDir(), "idx")})
	defer s.ClosePlugins()
	be := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	rt := attachRecordingTransport(t, s)

	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	s.InitializeWorkspace(t.TempDir()) // 策略就绪 → 装配期待命的插件 Launch

	proc := mountedProc(t, s, "merger")
	if proc == nil {
		t.Fatal("diagnostics.emit 插件应正常装配运行")
	}

	// 同一 uri、两个来源、各自一次去抖推送：后端 compute 车道 + 插件 emit 车道。
	s.diag.request(uri)
	if _, err := proc.Call("emit/now", nil); err != nil {
		t.Fatalf("Call: %v", err)
	}

	waitUntil(t, func() bool {
		return len(rt.filterMethod("textDocument/publishDiagnostics")) >= 2
	}, "两个来源的 publishDiagnostics 未都到达")
	time.Sleep(diagDebounce + 150*time.Millisecond) // 让最后的去抖全部落地

	pubs := rt.filterMethod("textDocument/publishDiagnostics")
	last := string(pubs[len(pubs)-1].Params)
	if !strings.Contains(last, "undefined: foo") {
		t.Errorf("最终推送缺少后端诊断（被插件车道全量替换顶掉）: %s", last)
	}
	if !strings.Contains(last, "plugin-marker") || !strings.Contains(last, `"source":"plugin:merger"`) {
		t.Errorf("最终推送缺少插件诊断（被后端车道全量替换顶掉）: %s", last)
	}
	if !strings.Contains(last, `"uri":"`+uri+`"`) {
		t.Errorf("最终推送 uri 缺失: %s", last)
	}
}
