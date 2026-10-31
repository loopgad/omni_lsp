package server

import (
	"encoding/json"
	"testing"

	"github.com/omnilsp/omni/internal/plugin"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

// 复证圈 7 sweep 发现 1 的守卫：emit 语言范围门必须用 canonical 后的 uri
// 判定。旧实现把插件可控的原始 ep.URI 原样传给 pluginWithinLanguageScope，
// 而 injectPluginDiagnostics 注入前才 canonical 化——两形态不一致可被
// percent-encoding 变体绕过：languageId 为空的已打开 prog.py，声明
// languages:[go] 的插件以 "file:///C:/work/prog.%70%79" emit，原始文本不
// 以 ".py" 结尾 → 后缀分支 miss → 语言不可判定 fail-open 放行 → 注入命中
// canonical 键，跨语言诊断上线。
func TestPluginEmit_LanguageScopeGateUsesCanonicalURI(t *testing.T) {
	s := New(DefaultConfig())
	attachRecordingTransport(t, s)
	pyURI := "file:///C:/work/prog.py"
	// languageId 为空（bypass 形态的前提：languageId 非空时 vfs 分支已拒绝）。
	s.vfs.Open(pyURI, "", 1, []byte("print('hello')\n"), vfs.SourceEditor)
	s.RegisterBackend("python", &mockBackend{langID: "python", exts: []string{".py"}})
	// pluginLangs 由 mountPlugins 初始化——白盒播种需自建 map。
	s.pluginMu.Lock()
	s.pluginLangs = map[string][]string{"p1": {"go"}, "p2": {"python"}}
	s.pluginMu.Unlock()
	grant := plugin.NewGrant(plugin.CapDiagnosticsEmit)

	emit := func(pluginID, uri string) {
		t.Helper()
		params, err := json.Marshal(map[string]any{
			"uri":         uri,
			"diagnostics": []map[string]any{{"startLine": 0, "startChar": 0, "endLine": 0, "endChar": 1, "severity": 1, "code": "E", "message": "x"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		s.onPluginNotify(pluginID, grant, "diagnostics/emit", params)
	}
	injected := func(uri string) bool {
		s.diag.mu.Lock()
		defer s.diag.mu.Unlock()
		_, ok := s.diag.pluginItems[uri]
		return ok
	}

	// go 声明的插件对编码变体 uri emit：必须被语言门拒绝（canonical 化后
	// 后缀分支命中 .py，与 go 声明无交集）。
	emit("p1", "file:///C:/work/prog.%70%79")
	if n := s.pluginEmitRejected.Load(); n != 1 {
		t.Fatalf("编码变体 emit 未被语言门拒绝，pluginEmitRejected=%d（旧实现对原始文本 HasSuffix，miss 后 fail-open 放行）", n)
	}
	if injected("file:///c:/work/prog.py") {
		t.Fatal("跨语言诊断经编码变体绕过语言门上线")
	}

	// 同一 uri 的直接拼写同样拒绝（门对 canonical 形态一致生效）。
	emit("p1", pyURI)
	if n := s.pluginEmitRejected.Load(); n != 2 {
		t.Fatalf("直接拼写的跨语言 emit 未被拒绝，pluginEmitRejected=%d", n)
	}

	// python 声明的插件对编码变体 emit：语言判定命中声明，照常注入——
	// 钉住修复不误伤合法投放。
	emit("p2", "file:///C:/work/prog.%70%79")
	if !injected("file:///c:/work/prog.py") {
		t.Fatal("语言匹配的插件经编码变体 emit 被误拒")
	}
}
