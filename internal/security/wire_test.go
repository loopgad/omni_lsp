package security

import (
	"strings"
	"testing"
)

// TestFprintfRedacted（复证圈 5 sweep 发现 4 的守卫）：internal 侧统一脱敏
// stderr 接线口——格式化结果先过 RedactString 再落通道，多行数组凭据形态
// （发现 1 的缺陷形态）同样被遮蔽。
func TestFprintfRedacted(t *testing.T) {
	var b strings.Builder
	if _, err := FprintfRedacted(&b, "omnilsp: readApiTokens=%q; token=%s\n", []string{"E2E-A", "E2E-B"}, "plain-secret"); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, secret := range []string{"E2E-A", "E2E-B", "plain-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("FprintfRedacted 输出仍含密文 %q:\n%s", secret, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("输出缺少 [REDACTED]:\n%s", out)
	}
	// 无凭据内容原样通过（键名结构保留）。
	b.Reset()
	if _, err := FprintfRedacted(&b, "omnilsp: plugin %s 未通过信任链\n", "demo"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "plugin demo 未通过信任链") {
		t.Fatalf("无凭据行被误改:\n%s", b.String())
	}
}
