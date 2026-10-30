package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// 审计批 3 发现 2 守卫（诊断车道合并遗漏面）：开口 3 的合并修复只覆盖两个
// push 上线点（publishPluginDiagnostics / publishComputed），pull 车道
// （textDocument/diagnostic 全量应答）直出纯后端集——pull 应答按 LSP 语义被
// 客户端整集替换，会把已上线的插件诊断整体顶掉，直到该插件下一次去抖推送
// 才恢复（插件静默期则无限期）。同一全量替换语义也命中缓存缺席残余面：
// publishPluginDiagnostics 在 d.cache 无该 uri 后端条目（后端从未算过/FIFO
// 驱逐）时只带插件集上线，把客户端当前显示的后端诊断一次性顶掉。

// pullResultShape 是 textDocument/diagnostic full 应答的解码形状（items 按
// projectDiagnostics 的线协议形状取用， languages.Diagnostic 无线协议
// unmarshal，此处只取 message/source 两个判别字段）。
type pullResultShape struct {
	Kind     string           `json:"kind"`
	ResultID string           `json:"resultId"`
	Items    []map[string]any `json:"items"`
}

func itemString(t *testing.T, item map[string]any, key string) string {
	t.Helper()
	v, _ := item[key].(string)
	return v
}

// TestPullDiagnosticsMergesPluginEmitSet：同一 uri 上插件 emit 集已注入协调
// 器后，pull 全量应答必须同时携带后端集与插件集，且 resultId 覆盖合并集
// （§I18 稳定 ID 不随车道遗漏漂移）。红：pull 应答只带后端集（插件诊断被
// pull 语义整集替换顶掉）。
func TestPullDiagnosticsMergesPluginEmitSet(t *testing.T) {
	const uri = "file:///w/pullmerge.go"
	s := New(DefaultConfig())
	be := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	attachRecordingTransport(t, s)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()

	// 插件 emit 集直接注入协调器（授权门与语言门的缝在此无关；Source 取
	// 宿主强制后的形态——真实链路的强制点在 onPluginNotify）。
	pluginItems := []languages.Diagnostic{{
		StartLine: 5, StartChar: 0, EndLine: 5, EndChar: 2,
		Severity: 2, Code: "P1", Source: "plugin:pullmerge", Message: "plugin-marker",
	}}
	s.diag.injectPluginDiagnostics("pullmerge", uri, pluginItems)

	resp := s.Dispatcher().Dispatch(context.Background(), jsonrpc.NewRequest(
		jsonrpc.RequestID{Num: 1}, "textDocument/diagnostic",
		json.RawMessage(`{"textDocument":{"uri":"`+uri+`"}}`)))
	if resp == nil || resp.Error != nil {
		t.Fatalf("pull failed: %+v", resp)
	}
	var result pullResultShape
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		t.Fatalf("decode pull result: %v", err)
	}
	if result.Kind != "full" {
		t.Fatalf("pull kind = %q, want full", result.Kind)
	}
	var hasBackend, hasPlugin bool
	for _, item := range result.Items {
		switch {
		case strings.Contains(itemString(t, item, "message"), "undefined: foo"):
			hasBackend = true
		case itemString(t, item, "source") == "plugin:pullmerge" &&
			strings.Contains(itemString(t, item, "message"), "plugin-marker"):
			hasPlugin = true
		}
	}
	if !hasBackend {
		t.Fatalf("pull 应答缺少后端诊断: %s", resp.Result)
	}
	if !hasPlugin {
		t.Fatalf("pull 应答缺少插件诊断（被 pull 全量替换语义顶掉）: %s", resp.Result)
	}
	// resultId 必须覆盖合并集（后端集 + 插件集），保证 §I18 稳定 ID 不漂移。
	expected := append([]languages.Diagnostic{
		{StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 3, Severity: 1, Code: "E1", Source: "go", Message: "undefined: foo"},
	}, pluginItems...)
	if result.ResultID != diagnosticsResultID(expected) {
		t.Fatalf("pull resultId 未覆盖合并集: got %q, want %q", result.ResultID, diagnosticsResultID(expected))
	}
}

// TestPluginLanePublishRecomputesMissingBackendCache：后端缓存无该 uri 条目
// （本测试不触发任何后端 compute，复现「后端从未算过/FIFO 驱逐」态）时，插件
// 车道的上线帧不得只带插件集——必须补一次后端重算并把后端集并入同一帧，
// 否则客户端当前显示的后端诊断被一次性顶掉。红：插件帧独占上线、后端零调用。
func TestPluginLanePublishRecomputesMissingBackendCache(t *testing.T) {
	const uri = "file:///w/cacheabsent.go"
	s := New(DefaultConfig())
	be := &diagBackend{mockBackend: mockBackend{langID: "go", exts: []string{".go"}}}
	s.RegisterBackend("go", be)
	rt := attachRecordingTransport(t, s)
	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()

	// 关键：不调用 s.diag.request——d.cache 保持无该 uri 条目。
	s.diag.injectPluginDiagnostics("cacheabsent", uri, []languages.Diagnostic{{
		StartLine: 1, StartChar: 0, EndLine: 1, EndChar: 1,
		Severity: 2, Code: "P1", Source: "plugin:cacheabsent", Message: "plugin-marker",
	}})

	waitUntil(t, func() bool {
		for _, m := range rt.filterMethod("textDocument/publishDiagnostics") {
			if strings.Contains(string(m.Params), "plugin-marker") {
				return true
			}
		}
		return false
	}, "插件车道 publishDiagnostics 未到达")

	for _, m := range rt.filterMethod("textDocument/publishDiagnostics") {
		body := string(m.Params)
		if !strings.Contains(body, "plugin-marker") {
			continue
		}
		if !strings.Contains(body, "undefined: foo") {
			t.Fatalf("缓存缺席帧只带插件集（客户端已显示的后端诊断被一次性顶掉）: %s", body)
		}
	}
	if calls := be.calls.Load(); calls < 1 {
		t.Fatalf("缓存缺席发布未触发后端重算（be.calls=%d）——后端集不可能并入该帧", calls)
	}
}
