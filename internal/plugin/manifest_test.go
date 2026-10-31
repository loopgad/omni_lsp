package plugin

import "testing"

// TestValidate_UnverifiableSignatureDoesNotSilenceWarning（复证圈 1 sweep
// 发现 4）：全仓无任何签名验证实现，Signature 是咨询性字段。旧实现只对
// ""/"unsigned" 记 Warning——任何非空非 "unsigned" 的攻击者可控字符串
// （如 "forged-by-attacker"）都会使 Warning 为空：诚实未签名的清单带告警，
// 伪造签名的清单完全静音。修复后：非空 signature 在宿主具备验证实现之前
// 一律记「声明了但未验证」告警——未验证的声明值不得静音告警（真实信任
// 链是 sha256 checksum 门 + 句柄钉定，不受本告警影响）。
func TestValidate_UnverifiableSignatureDoesNotSilenceWarning(t *testing.T) {
	for _, sig := range []string{"forged-by-attacker", "ed25519:AAAA", "unsigned-but-fancy"} {
		m := newValidManifest([]byte("x"))
		m.Signature = sig
		if err := m.Validate(); err != nil {
			t.Fatalf("signature %q 不应报错（咨询字段）: %v", sig, err)
		}
		if m.Warning == "" {
			t.Fatalf("signature %q 无法被宿主验证，必须记录告警（旧实现静音）", sig)
		}
	}
	// 对照：unsigned 语义不变。
	m := newValidManifest([]byte("x"))
	m.Signature = "unsigned"
	if err := m.Validate(); err != nil {
		t.Fatalf("unsigned 不应报错: %v", err)
	}
	if m.Warning == "" {
		t.Fatal("unsigned 应记录 warning（既有钉住不回退）")
	}
}

func TestO5_APIVersionRangeNegotiation(t *testing.T) {
	if !APIVersionSupported(SupportedAPIVersion) {
		t.Fatal("preferred version must always be supported")
	}
	if APIVersionSupported("omnilsp.plugin.v99") {
		t.Fatal("unknown version must be refused")
	}
	// Simulate a transition window: host lists v1 and v2.
	old := SupportedAPIVersions
	SupportedAPIVersions = append(SupportedAPIVersions, "omnilsp.plugin.v2")
	defer func() { SupportedAPIVersions = old }()
	if !APIVersionSupported("omnilsp.plugin.v2") {
		t.Fatal("transition-window version should negotiate")
	}
}
