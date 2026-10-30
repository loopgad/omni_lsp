package plugin

import (
	"errors"
	"testing"
)

// 复证圈 2 审计发现 1 的守卫：装配点对 manifest.ID 必须有唯一性约束。
// 旧实现的 Install 对已注册 ID 静默覆盖 m.manifests[ID]——同 ID 的两个插件
// 目录互相顶替，破坏有界拆除（pluginProcs 同键覆盖使先 Launch 的进程失去
// 登记句柄）、崩溃隔离（RecordCrash 按 ID 连坐）与 emit 来源治理。
func TestInstall_RejectsDuplicatePluginID(t *testing.T) {
	m := NewManager()
	first := Discovery{Dir: "dir-a", Manifest: newValidManifest([]byte("a"))}
	second := Discovery{Dir: "dir-b", Manifest: newValidManifest([]byte("b"))}
	second.Manifest.ID = first.Manifest.ID // 同 ID、不同目录来源

	if err := m.Install(first); err != nil {
		t.Fatalf("首次 Install: %v", err)
	}
	err := m.Install(second)
	if err == nil {
		t.Fatal("同 ID 的第二次 Install 必须被拒绝（旧实现静默顶替注册表条目）")
	}
	if !errors.Is(err, ErrDuplicatePluginID) {
		t.Fatalf("err = %v, want ErrDuplicatePluginID（errors.Is 可判别）", err)
	}
	// 注册表必须保留首个来源，不被顶替（白盒：同包读注册表）。
	got, ok := m.manifests[first.Manifest.ID]
	if !ok {
		t.Fatal("首个来源的 manifest 必须仍在注册表")
	}
	if got.Entrypoint != first.Manifest.Entrypoint || got.Checksum != first.Manifest.Checksum {
		t.Fatalf("注册表 manifest 被顶替: %+v", got)
	}
}
