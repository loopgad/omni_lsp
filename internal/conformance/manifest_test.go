package conformance

import (
	"testing"

	"github.com/omnilsp/omni/internal/protocol/lsp"
)

// TestY54_ProtocolManifestFresh 是 §Y5-4/§X9-2 的机械化探针：手写类型文件
// internal/protocol/lsp/types.go 与仓库内 protocol.manifest 必须保持同步，
// 协议面任何声明变动都应显式重写指纹而不是悄悄漂移。
func TestY54_ProtocolManifestFresh(t *testing.T) {
	if err := lsp.CheckManifest(moduleRoot()); err != nil {
		t.Errorf("protocol manifest 漂移；更新命令：go run scripts/gen-protocol.go\n%v", err)
	}
}
