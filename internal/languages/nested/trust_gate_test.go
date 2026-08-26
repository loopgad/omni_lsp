package nested

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestN3_UntrustedWorkspaceBlocksBackendStart pins the §N1/N3 integration:
// with OMNILSP_TRUST=untrusted, StartSupervised refuses to spawn any backend
// process and surfaces an untrusted_operation error.
func TestN3_UntrustedWorkspaceBlocksBackendStart(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "untrusted")
	c := New(Config{
		Name:    "pyright",
		Lang:    "python",
		WorkDir: t.TempDir(),
		Start: func(c *Conn) error {
			t.Error("Start factory must not run for untrusted workspace")
			return nil
		},
	})
	err := c.StartSupervised()
	if err == nil {
		t.Fatal("expected refusal, got nil")
	}
	if !strings.Contains(err.Error(), "workspace trust") {
		t.Errorf("err = %v, want workspace-trust refusal", err)
	}
}

// TestN1_ExplicitTrustedEnvAllowsStart verifies the explicit-escalation path
// end to end: OMNILSP_TRUST=trusted runs the start factory.
func TestN1_ExplicitTrustedEnvAllowsStart(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "trusted")
	started := make(chan struct{})
	c := New(Config{
		Name:           "testlang",
		Lang:           "test",
		WorkDir:        t.TempDir(),
		RequestTimeout: 2 * time.Second,
		Start: func(c *Conn) error {
			close(started)
			return nil
		},
	})
	if err := c.StartSupervised(); err != nil {
		t.Fatalf("trusted start refused: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("start factory never ran")
	}
	_ = context.Background()
}
