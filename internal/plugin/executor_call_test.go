package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// spawnHelper launches the test binary itself as a fake plugin subprocess.
func spawnHelper(t *testing.T, mode string, m Manifest, g Grant) *Process {
	t.Helper()
	m.Entrypoint = os.Args[0]
	p, err := Launch(context.Background(), m, g, nil)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })
	return p
}

// TestLaunch_HandshakeGrantsFiltered pins the handshake contract: the
// plugin/hello frame carries the host API version and exactly the manifest's
// declared capabilities — no more (a wider grant must NOT leak extra grants
// to the child), no less (the pre-spawn gate already forces full coverage).
// The child proves receipt by echoing hello.params back as its first result.
func TestLaunch_HandshakeGrantsFiltered(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "echo-hello")

	m := newValidManifest([]byte("ignored"))
	m.Capabilities = []Capability{CapDocumentRead, CapNetworkAccess}
	// Deliberately over-grant: the child must still see only the two
	// manifest-declared caps, not everything the host is willing to give.
	p := spawnHelper(t, "echo-hello", m,
		NewGrant(CapDocumentRead, CapNetworkAccess, CapFilesystemWrite, CapIndexQuery))

	res, err := p.Call("probe/handshake", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var hello struct {
		APIVersion string   `json:"apiVersion"`
		Grants     []string `json:"grants"`
	}
	if err := json.Unmarshal(res, &hello); err != nil {
		t.Fatalf("handshake echo unparsable: %v (%s)", err, res)
	}
	if hello.APIVersion != SupportedAPIVersion {
		t.Errorf("apiVersion = %q, want %q", hello.APIVersion, SupportedAPIVersion)
	}
	want := []string{string(CapDocumentRead), string(CapNetworkAccess)}
	if len(hello.Grants) != len(want) {
		t.Fatalf("grants = %v, want %v", hello.Grants, want)
	}
	for i := range want {
		if hello.Grants[i] != want[i] {
			t.Errorf("grants[%d] = %q, want %q (over-grant leaked or reordered)", i, hello.Grants[i], want[i])
		}
	}
}

// TestCall_RoundTripAndRPCError pins the line-JSON-RPC skeleton: params go
// out verbatim, results come back parsed, and plugin-side rpc errors surface
// as distinguishable Go errors.
func TestCall_RoundTripAndRPCError(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")

	t.Run("roundtrip", func(t *testing.T) {
		t.Setenv("GO_PLUGIN_MODE", "roundtrip")
		m := newValidManifest([]byte("x"))
		m.Capabilities = []Capability{CapIndexQuery}
		p := spawnHelper(t, "roundtrip", m, NewGrant(CapIndexQuery))

		params := map[string]any{"uri": "file:///x.go", "line": 3}
		res, err := p.Call("index/query", params)
		if err != nil {
			t.Fatalf("Call: %v", err)
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
		if got.Pong.URI != "file:///x.go" || got.Pong.Line != 3 {
			t.Errorf("params mangled in flight: %+v", got.Pong)
		}
	})

	t.Run("rpc-error", func(t *testing.T) {
		t.Setenv("GO_PLUGIN_MODE", "rpc-error")
		m := newValidManifest([]byte("x"))
		m.Capabilities = []Capability{CapDiagnosticsEmit}
		p := spawnHelper(t, "rpc-error", m, NewGrant(CapDiagnosticsEmit))

		_, err := p.Call("diagnostics/emit", nil)
		if err == nil || !strings.Contains(err.Error(), "rpc 错误 42") {
			t.Fatalf("err = %v, want rpc 错误 42", err)
		}
	})
}

// TestCall_PluginDies pins the EOF path (§O crash containment input): when
// the child dies mid-call the host must get an error — not a hang, not a
// partial parse. The 5s timeout branch is deliberately not exercised:
// callTimeout is a const and waiting it out tests the clock.
func TestCall_PluginDies(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "die")

	m := newValidManifest([]byte("x"))
	p := spawnHelper(t, "die", m, NewGrant(m.Capabilities...))

	done := make(chan error, 1)
	go func() {
		_, err := p.Call("anything", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dead plugin must yield an error, got nil")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Call hung on dead plugin past callTimeout")
	}
}

// TestOnExit_OnceAndCrashQuarantine closes the §O4 containment loop:
// (1) OnExit fires exactly once per dead child even though Wait and the
// host both observe the exit; (2) feeding those exits into
// Manager.RecordCrash quarantines the plugin at exactly
// MaxConsecutivePluginCrashes, after which Enable refuses with
// ErrQuarantined.
func TestOnExit_OnceAndCrashQuarantine(t *testing.T) {
	t.Setenv("OMNISP_PLUGIN_HELPER", "1")
	t.Setenv("GO_PLUGIN_MODE", "die")

	m := newValidManifest([]byte("x"))

	exits := make(chan error, 4)
	// OnExit wired at spawn time (the only race-free moment).
	m.Entrypoint = os.Args[0]
	p, err := Launch(context.Background(), m, NewGrant(m.Capabilities...), func(err error) { exits <- err })
	if err != nil {
		t.Fatalf("launch with OnExit: %v", err)
	}
	t.Cleanup(func() { _ = p.cmd.Process.Kill() })

	// Exactly-once: no further sends ever arrive.
	select {
	case err := <-exits:
		_ = err // die-mode exits 0; value irrelevant, delivery is the contract
	case <-time.After(3 * time.Second):
		t.Fatal("OnExit never fired")
	}
	select {
	case err := <-exits:
		t.Fatalf("OnExit fired twice: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	// Crash-loop quarantine via the manager.
	mgr := NewManager()
	if err := mgr.Install(Discovery{Dir: "d", Manifest: m}); err != nil {
		t.Fatalf("install: %v", err)
	}
	// First enable from Verified succeeds...
	if _, err := mgr.Enable(m.ID); err != nil {
		t.Fatalf("first enable refused: %v", err)
	}
	// ...then crashes accumulate regardless of lifecycle state, and the
	// threshold flips the plugin into quarantine.
	for i := 1; i <= MaxConsecutivePluginCrashes; i++ {
		mgr.RecordCrash(m.ID)
	}
	if _, err := mgr.Enable(m.ID); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("post-threshold enable err = %v, want ErrQuarantined", err)
	}
	st, err := mgr.State(m.ID)
	if err != nil || st != StateQuarantined {
		t.Errorf("state = %v (err %v), want quarantined", st, err)
	}
}
