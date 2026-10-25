package plugin

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNotify_BroadcastDeliveredToPlugin pins the host→plugin broadcast
// contract: Notify sends an id-less JSON-RPC notification frame that arrives
// verbatim (method + params) and in write order, while consuming no request
// id — a notification must never be mistaken for a request by the child.
func TestNotify_BroadcastDeliveredToPlugin(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "collect-notify")

	m := newValidManifest([]byte("x"))
	m.Capabilities = []Capability{CapDiagnosticsEmit}
	p := spawnHelper(t, "collect-notify", m, NewGrant(m.Capabilities...))

	if err := p.Notify("plugin/documentDidOpen", map[string]any{"uri": "file:///a.go"}); err != nil {
		t.Fatalf("Notify #1: %v", err)
	}
	if err := p.Notify("plugin/documentDidChange", map[string]any{"uri": "file:///b.go", "n": 2}); err != nil {
		t.Fatalf("Notify #2: %v", err)
	}

	audit, err := p.Call("broadcast/audit", nil)
	if err != nil {
		t.Fatalf("audit Call: %v", err)
	}
	var got struct {
		Notifications []struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		} `json:"notifications"`
	}
	if err := json.Unmarshal(audit, &got); err != nil {
		t.Fatalf("audit result unparsable: %v (%s)", err, audit)
	}
	if len(got.Notifications) != 2 {
		t.Fatalf("child received %d notifications, want 2: %s", len(got.Notifications), audit)
	}
	if got.Notifications[0].Method != "plugin/documentDidOpen" {
		t.Errorf("notification[0].method = %q, want plugin/documentDidOpen", got.Notifications[0].Method)
	}
	if string(got.Notifications[0].Params) != `{"uri":"file:///a.go"}` {
		t.Errorf("notification[0].params mangled: %s", got.Notifications[0].Params)
	}
	if got.Notifications[1].Method != "plugin/documentDidChange" {
		t.Errorf("notification[1].method = %q, want plugin/documentDidChange", got.Notifications[1].Method)
	}
	if !strings.Contains(string(got.Notifications[1].Params), `"n":2`) {
		t.Errorf("notification[1].params mangled: %s", got.Notifications[1].Params)
	}
}

// TestNotify_RejectsEmptyMethod pins the input guard: a notification without
// a method is malformed JSON-RPC and must be refused host-side, before any
// bytes reach the pipe.
func TestNotify_RejectsEmptyMethod(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "collect-notify")

	m := newValidManifest([]byte("x"))
	p := spawnHelper(t, "collect-notify", m, NewGrant(m.Capabilities...))

	if err := p.Notify("", map[string]any{"x": 1}); err == nil {
		t.Fatal("empty method must be rejected")
	}
}

// TestFraming_HeaderBlockAndChunkedStream pins the framing upgrade over the
// line-delimited skeleton:
//   - extra-headers: a response carrying an additional header and a lowercase
//     content-length must parse, and a raw newline inside the framed body
//     must survive (line-based readers would split mid-JSON);
//   - chunked stream: two response frames concatenated into a single write
//     must be split exactly at Content-Length boundaries.
func TestFraming_HeaderBlockAndChunkedStream(t *testing.T) {
	t.Run("附加头_小写头名_负载内换行", func(t *testing.T) {
		t.Setenv("OMNISP_PLUGIN_HELPER", "1")
		t.Setenv("GO_PLUGIN_MODE", "extra-headers")
		m := newValidManifest([]byte("x"))
		m.Capabilities = []Capability{CapIndexQuery}
		p := spawnHelper(t, "extra-headers", m, NewGrant(CapIndexQuery))

		params := map[string]any{"uri": "file:///hdr.go", "line": 5}
		res, err := p.Call("index/query", params)
		if err != nil {
			t.Fatalf("Call with extra headers: %v", err)
		}
		var got struct {
			Pong struct {
				URI  string `json:"uri"`
				Line int    `json:"line"`
			} `json:"pong"`
		}
		if err := json.Unmarshal(res, &got); err != nil {
			t.Fatalf("result unparsable: %v (%s)", err, res)
		}
		if got.Pong.URI != "file:///hdr.go" || got.Pong.Line != 5 {
			t.Fatalf("params mangled across header block: %+v", got.Pong)
		}
	})

	t.Run("单次写入的连续两帧", func(t *testing.T) {
		t.Setenv("OMNISP_PLUGIN_HELPER", "1")
		t.Setenv("GO_PLUGIN_MODE", "batch-frames")
		m := newValidManifest([]byte("x"))
		m.Capabilities = []Capability{CapIndexQuery}
		p := spawnHelper(t, "batch-frames", m, NewGrant(CapIndexQuery))

		type echoed struct {
			Pong struct {
				URI  string `json:"uri"`
				Line int    `json:"line"`
			} `json:"pong"`
		}
		done := make(chan error, 2)
		for _, c := range []struct {
			uri  string
			line int
		}{{"file:///one.go", 1}, {"file:///two.go", 2}} {
			c := c
			go func() {
				res, err := p.Call("index/query", map[string]any{"uri": c.uri, "line": c.line})
				if err != nil {
					done <- err
					return
				}
				var got echoed
				if err := json.Unmarshal(res, &got); err != nil {
					done <- err
					return
				}
				if got.Pong.URI != c.uri || got.Pong.Line != c.line {
					t.Errorf("Call(%q) mismatched response: %+v", c.uri, got.Pong)
				}
				done <- nil
			}()
		}
		for i := 0; i < 2; i++ {
			if err := <-done; err != nil {
				t.Fatalf("chunked-frame call failed: %v", err)
			}
		}
	})
}

// TestFraming_RejectsOversizedFrame pins the single-frame size cap: a child
// declaring a Content-Length far beyond maxPluginFrameSize must be refused
// immediately with the cap error — not honored (unbounded allocation) and
// not surfacing as a bare EOF.
func TestFraming_RejectsOversizedFrame(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "oversized-frame")

	m := newValidManifest([]byte("x"))
	p := spawnHelper(t, "oversized-frame", m, NewGrant(m.Capabilities...))

	_, err := p.Call("anything", nil)
	if err == nil {
		t.Fatal("oversized frame must be rejected")
	}
	if !strings.Contains(err.Error(), "超出单帧上限") {
		t.Fatalf("err = %v, want 单帧上限 rejection", err)
	}
}

// TestFraming_SkipsUnparsableFrame pins single-frame resilience: one
// unparsable frame on the wire must be dropped while the connection and all
// other in-flight requests stay healthy — the pre-upgrade behavior aborted
// the whole read loop and failed every pending call.
func TestFraming_SkipsUnparsableFrame(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "garbage-then-reply")

	m := newValidManifest([]byte("x"))
	// index/query 走 Call 级能力门（下沉后），manifest 须声明 index.query。
	m.Capabilities = []Capability{CapIndexQuery}
	p := spawnHelper(t, "garbage-then-reply", m, NewGrant(m.Capabilities...))

	res, err := p.Call("index/query", map[string]any{"uri": "file:///ok.go"})
	if err != nil {
		t.Fatalf("call after garbage frame failed: %v", err)
	}
	var got struct {
		Pong struct {
			URI string `json:"uri"`
		} `json:"pong"`
	}
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatalf("result unparsable: %v (%s)", err, res)
	}
	if got.Pong.URI != "file:///ok.go" {
		t.Fatalf("uri mangled: %+v", got.Pong)
	}
}
