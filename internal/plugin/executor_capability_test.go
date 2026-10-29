package plugin

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestCall_MethodCapabilityGate pins the sunk capability gate (§O2): the
// host's fixed method→capability mapping is enforced per Call against the
// manifest's declared capabilities — a mapped method whose capability the
// manifest never declared is refused, a mapped method with the declaration
// passes, and methods outside the mapping (protocol plumbing) carry no gate.
// The pre-spawn Launch gate stays as-is: declared ⊆ grant.
func TestCall_MethodCapabilityGate(t *testing.T) {
	t.Run("映射内_未声明_拒绝", func(t *testing.T) {
		t.Setenv("OMNISP_PLUGIN_HELPER", "1")
		t.Setenv("GO_PLUGIN_MODE", "roundtrip")
		m := newValidManifest([]byte("x"))
		m.Capabilities = []Capability{CapDiagnosticsEmit} // 未声明 index.query
		p := spawnHelper(t, "roundtrip", m, NewGrant(CapDiagnosticsEmit))

		_, err := p.Call("index/query", map[string]any{"uri": "file:///denied.go"})
		if !errors.Is(err, ErrCapabilityDenied) {
			t.Fatalf("err = %v, want ErrCapabilityDenied", err)
		}
	})

	t.Run("映射内_已声明_放行", func(t *testing.T) {
		t.Setenv("OMNISP_PLUGIN_HELPER", "1")
		t.Setenv("GO_PLUGIN_MODE", "roundtrip")
		m := newValidManifest([]byte("x"))
		m.Capabilities = []Capability{CapIndexQuery}
		p := spawnHelper(t, "roundtrip", m, NewGrant(CapIndexQuery))

		res, err := p.Call("index/query", map[string]any{"uri": "file:///allowed.go"})
		if err != nil {
			t.Fatalf("声明内的映射方法被拒: %v", err)
		}
		var got struct {
			Pong struct {
				URI string `json:"uri"`
			} `json:"pong"`
		}
		if err := json.Unmarshal(res, &got); err != nil {
			t.Fatalf("result unparsable: %v (%s)", err, res)
		}
		if got.Pong.URI != "file:///allowed.go" {
			t.Fatalf("params mangled: %+v", got.Pong)
		}
	})

	t.Run("映射外_无能力门", func(t *testing.T) {
		t.Setenv("OMNISP_PLUGIN_HELPER", "1")
		t.Setenv("GO_PLUGIN_MODE", "roundtrip")
		// 声明与 index.query 无关：映射外方法不受能力门约束（协议管线）。
		m := newValidManifest([]byte("x"))
		m.Capabilities = []Capability{CapDiagnosticsEmit}
		p := spawnHelper(t, "roundtrip", m, NewGrant(CapDiagnosticsEmit))

		if _, err := p.Call("probe/free-form", map[string]any{"uri": "file:///free.go"}); err != nil {
			t.Fatalf("映射外方法不应设能力门: %v", err)
		}
	})

	// 映射登记的非循环验证半边（审计批 3）：四个映射方法的字面名逐一打
	// deny 路径——映射表任何 key 打错字，对应方法就会落回「映射外_无能力门」
	// 的放行路径，本子测试立即红。TestMethodCapabilityCompleteness 读的是
	// 同一张表，抓不住 key 笔误（值的完备性照样成立）；这里以独立字面量
	// 闭环每个方法名的登记。
	t.Run("映射内_四方法_逐一拒绝", func(t *testing.T) {
		t.Setenv("OMNISP_PLUGIN_HELPER", "1")
		t.Setenv("GO_PLUGIN_MODE", "roundtrip")
		// manifest 只声明 document.read：四个映射方法所需的能力均未声明，
		// 门必须逐一拒绝（拒绝发生在写管道之前，helper 不会看到这些请求）。
		m := newValidManifest([]byte("x"))
		m.Capabilities = []Capability{CapDocumentRead}
		p := spawnHelper(t, "roundtrip", m, NewGrant(CapDocumentRead))

		for _, method := range []string{"index/query", "index/write", "diagnostics/emit", "code_action/emit"} {
			if _, err := p.Call(method, nil); !errors.Is(err, ErrCapabilityDenied) {
				t.Fatalf("Call(%q) err = %v, want ErrCapabilityDenied", method, err)
			}
		}
	})
}

// TestMethodCapabilityCompleteness 是方法→能力映射的双向完备性守卫（审计批 3，
// §O2）：methodCapability 对映射外方法 allow-by-default（fail-open，协议管线
// 的取舍，被 TestCall_MethodCapabilityGate 钉住），故登记必须被机制强制——
//  1. 前向（方法→能力）：映射每个条目的值必须是九能力白名单内的真实能力，
//     拦截能力名笔误与死条目；
//  2. 反向（能力→登记）：AllCapabilities 的每一项能力要么是某个映射条目的值
//     （有方法触达面），要么显式列入 envOnlyCapabilities（环境型，无方法面）。
//     两头都不沾 = 漏登记 = 红灯：向协议新增能力（或为能力新增方法触达面）
//     而不同步登记表，包级测试直接失败，而不是静默零检查直发。
func TestMethodCapabilityCompleteness(t *testing.T) {
	white := make(map[Capability]bool, 9)
	for _, c := range AllCapabilities() {
		white[c] = true
	}

	gated := make(map[Capability]bool, len(methodCapability))
	for method, c := range methodCapability {
		if method == "" {
			t.Error("methodCapability 存在空方法名条目")
			continue
		}
		if !white[c] {
			t.Errorf("methodCapability[%q] = %q 不在九能力白名单内（能力名笔误或死条目）", method, c)
			continue
		}
		gated[c] = true
	}

	envOnly := make(map[Capability]bool, len(envOnlyCapabilities))
	for _, c := range envOnlyCapabilities {
		if envOnly[c] {
			t.Errorf("envOnlyCapabilities 重复登记 %q", c)
			continue
		}
		if gated[c] {
			t.Errorf("能力 %q 同时登记为方法触达与环境型：两表互斥，必须二选一", c)
		}
		envOnly[c] = true
	}

	for _, c := range AllCapabilities() {
		if !gated[c] && !envOnly[c] {
			t.Errorf("能力 %q 未登记：既无 methodCapability 方法触达条目，也未列入 envOnlyCapabilities（环境型）——新能力必须二选一登记，否则经映射外路径零检查直发", c)
		}
	}
}
