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
}
