package plugin

import (
	"context"
	"errors"
	"testing"
)

// TestLaunch_RejectsBeforeSpawn pins §O2: both pre-spawn gates fire without
// ever creating a child process — manifest integrity first, then the
// capability gate (default deny).
func TestLaunch_RejectsBeforeSpawn(t *testing.T) {
	// Gate 1: manifest integrity.
	bad := newValidManifest([]byte("#!/bin/demo\n"))
	bad.ID = ""
	if _, err := Launch(context.Background(), bad, NewGrant(AllCapabilities()...), nil); err == nil {
		t.Fatal("empty id must be rejected by Validate before spawn")
	}

	// Gate 2: capability denied — manifest asks for two, grant allows one.
	m := newValidManifest([]byte("#!/bin/demo\n"))
	m.Entrypoint = "whatever-the-gate-rejects-first"
	m.Capabilities = []Capability{CapNetworkAccess, CapFilesystemWrite}
	_, err := Launch(context.Background(), m, NewGrant(CapNetworkAccess), nil)
	if !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("err = %v, want ErrCapabilityDenied", err)
	}
}
