package server

// §C11 推送路径的代次绑定守卫。审计发现：computeAndPush 发布前的两道检查
// 都只比对「cache 中该 URI 条目的 key == currentKey」，从不校验 cached.key
// 等于本次 compute 所用的 key——旧 run 停顿越过检查点、新 run 先覆盖 cache
// 后，发布决策读到新代条目（version 取新代），投影的却是旧代 compute 的
// items，旧代 items 冒新代 version 发布。
//
// 修复语义（fixSketch）：compute 返回 (key, items) 绑定值；发布决策收敛进
// publishComputed，发布前后各校验一次「cache 中该 URI 条目的 key 仍等于本次
// key」且「currentKey 仍等于本次 key」，不等则放弃发布。
//
// 行为级时序注入说明：旧代码的 TOCTOU 窗口位于两段临界区之间的锁间隙（几条
// 指令，窗口内无 IO 点可供测试插桩；vfs/snapMgr 是具体类型非接口），集成时
// 序无法确定性命中，故本文件以 publishComputed 为被测面直接构造窗口两侧的
// 状态——红来自「publishComputed 不存在（编译失败）」加「旧结构无从表达本
// 次 key」；同代发布分支同时钉住正常路径不被校验误伤。

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/omnilsp/omni/internal/languages"
	"github.com/omnilsp/omni/internal/workspace/vfs"
)

type publishedDiags struct {
	Version     int64            `json:"version"`
	URI         string           `json:"uri"`
	Diagnostics []map[string]any `json:"diagnostics"`
}

func pushPayloads(t *testing.T, rt *recordingTransport) []publishedDiags {
	t.Helper()
	msgs := rt.filterMethod("textDocument/publishDiagnostics")
	out := make([]publishedDiags, 0, len(msgs))
	for _, m := range msgs {
		var p publishedDiags
		if err := json.Unmarshal(m.Params, &p); err != nil {
			t.Fatalf("decode publish payload: %v (%s)", err, m.Params)
		}
		out = append(out, p)
	}
	return out
}

func seedDiagCache(s *Server, uri string, key diagKey, items []languages.Diagnostic, version int64) {
	s.diag.mu.Lock()
	s.diag.cache[uri] = diagEntry{key: key, items: items, version: version}
	s.diag.mu.Unlock()
}

func newRecordingServer(t *testing.T, langID string) (*Server, *diagBackend, *recordingTransport) {
	t.Helper()
	s := New(DefaultConfig())
	be := &diagBackend{mockBackend: mockBackend{langID: langID, exts: []string{".go"}}}
	s.RegisterBackend(langID, be)
	rt := &recordingTransport{}
	s.mu.Lock()
	s.transport = rt
	s.mu.Unlock()
	return s, be, rt
}

func boundItems(message string) []languages.Diagnostic {
	return []languages.Diagnostic{{
		StartLine: 0, StartChar: 0, EndLine: 0, EndChar: 3,
		Severity: 1, Code: "E2", Source: "go", Message: message,
	}}
}

func TestDiagPush_StaleGenerationCannotPublish(t *testing.T) {
	const uri = "file:///w/main.go"
	s, be, rt := newRecordingServer(t, "go")

	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	current := s.diag.currentKey(uri, be)

	seedDiagCache(s, uri, current, boundItems("new-generation diagnostic"), 7)

	// 旧代 (key, items)：contentHash 已漂移的代次带着自己的 items 走到发布点，
	// 而 cache 中已是新代条目——修复前 version 取新代、items 用旧代，一起上线。
	stale := current
	stale.ContentHash = "0000000000000000"

	s.diag.publishComputed(uri, stale, boundItems("stale-generation diagnostic"), be)
	if pubs := pushPayloads(t, rt); len(pubs) != 0 {
		t.Fatalf("stale generation published %d time(s): %+v", len(pubs), pubs)
	}
}

func TestDiagPush_InputMovedOnCannotPublish(t *testing.T) {
	const uri = "file:///w/main.go"
	s, be, rt := newRecordingServer(t, "go")

	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	generation := s.diag.currentKey(uri, be)
	seedDiagCache(s, uri, generation, boundItems("diagnostic"), 7)

	// cache 条目与本次 key 一致，但输入此后又前进了一代：发布点必须放弃，
	// 把接力交给新代的 debounce run。
	s.vfs.Update(uri, 2, []byte("package main // edited\n"))
	s.publishSnapshot()

	s.diag.publishComputed(uri, generation, boundItems("diagnostic"), be)
	if pubs := pushPayloads(t, rt); len(pubs) != 0 {
		t.Fatalf("published although the input had moved on: %+v", pubs)
	}
}

func TestDiagPush_MissingCacheEntryCannotPublish(t *testing.T) {
	const uri = "file:///w/main.go"
	s, be, rt := newRecordingServer(t, "go")

	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	// compute 成功返回时 cache 必有本次条目；条目缺席即被 didClose/驱逐，
	// version 无从同代绑定，放弃发布。
	current := s.diag.currentKey(uri, be)

	s.diag.publishComputed(uri, current, boundItems("diagnostic"), be)
	if pubs := pushPayloads(t, rt); len(pubs) != 0 {
		t.Fatalf("published without a bound cache entry: %+v", pubs)
	}
}

func TestDiagPush_BoundGenerationPublishes(t *testing.T) {
	const uri = "file:///w/main.go"
	s, be, rt := newRecordingServer(t, "go")

	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	current := s.diag.currentKey(uri, be)
	seedDiagCache(s, uri, current, boundItems("bound diagnostic"), 7)

	// 同代绑定 (key, items)：正常路径必须照常发布，且 version/items 同源。
	s.diag.publishComputed(uri, current, boundItems("bound diagnostic"), be)
	pubs := pushPayloads(t, rt)
	if len(pubs) != 1 {
		t.Fatalf("bound generation published %d time(s), want 1: %+v", len(pubs), pubs)
	}
	if pubs[0].Version != 7 {
		t.Errorf("version = %d, want 7", pubs[0].Version)
	}
	if len(pubs[0].Diagnostics) != 1 || pubs[0].Diagnostics[0]["message"] != "bound diagnostic" {
		t.Errorf("payload items drifted from the bound items: %+v", pubs[0].Diagnostics)
	}
}

// 回归钉（绿钉）：真实调度路径下，compute 返回的 key 与 items 绑定流转到
// publishComputed，常规去抖推送一次到位。compute 返回值拆散即编译失败。
func TestDiagPush_DebouncedPathStillPublishes(t *testing.T) {
	const uri = "file:///w/main.go"
	s, be, rt := newRecordingServer(t, "go")

	s.vfs.Open(uri, "go", 1, []byte("package main\n"), vfs.SourceEditor)
	s.publishSnapshot()
	s.diag.request(uri)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(rt.filterMethod("textDocument/publishDiagnostics")) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	pubs := pushPayloads(t, rt)
	if len(pubs) != 1 {
		t.Fatalf("expected exactly 1 debounced publish, got %d", len(pubs))
	}
	if n := be.calls.Load(); n != 1 {
		t.Errorf("backend calls = %d, want 1", n)
	}
}
