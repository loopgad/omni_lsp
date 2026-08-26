package trust

import (
	"errors"
	"strings"
	"testing"

	ierrors "github.com/omnilsp/omni/internal/errors"
)

func isUntrusted(err error) bool {
	var e *ierrors.Error
	return errors.As(err, &e) && e.Kind == ierrors.ErrUntrustedOperation
}

// TestN1_DefaultUntrusted pins §N1: a newly opened arbitrary repository is
// Untrusted unless policy says otherwise.
func TestN1_DefaultUntrusted(t *testing.T) {
	p := NewPolicy("D:/some/random/repo")
	if p.State() != StateUntrusted {
		t.Fatalf("default state = %v, want untrusted", p.State())
	}
	if p.Source() != "default" {
		t.Fatalf("source = %q, want default", p.Source())
	}
	for _, c := range []Capability{CapProcessExecute, CapPluginLoad, CapNetworkAccess,
		CapCompilerExecute, CapFormatterExecute, CapBuildScriptExecute, CapProcMacroExecute,
		CapRemoteIndexRead, CapTelemetryExport} {
		if err := p.Gate(c); !isUntrusted(err) {
			t.Errorf("Gate(%s) = %v, want ErrUntrustedOperation (N3)", c, err)
		}
	}
}

// TestN3_UntrustedBlocksExecution walks the eight §N3 prohibitions and their
// capability mapping.
func TestN3_UntrustedBlocksExecution(t *testing.T) {
	p := NewPolicy("/repo")
	cases := []struct {
		name string
		cap  Capability
	}{
		{"workspace scripts", CapProcessExecute},
		{"shell from repo files", CapProcessExecute},
		{"compiler plugins", CapPluginLoad},
		{"typescript plugins", CapPluginLoad},
		{"arbitrary formatters", CapFormatterExecute},
		{"download binaries", CapNetworkAccess},
		{"remote source upload", CapRemoteIndexRead},
		{"install plugins", CapPluginLoad},
		{"build scripts", CapBuildScriptExecute},
		{"proc macros", CapProcMacroExecute},
		{"telemetry", CapTelemetryExport},
	}
	for _, tc := range cases {
		err := p.Gate(tc.cap)
		if !isUntrusted(err) {
			t.Errorf("%s: Gate(%s) = %v, want refusal", tc.name, tc.cap, err)
		}
	}
}

// TestN1_ExplicitEscalation verifies state changes only through explicit calls
// and that de-escalation is refused.
func TestN1_ExplicitEscalation(t *testing.T) {
	p := NewPolicy("/repo")
	if err := p.Escalate(StateTrusted, "user-consent"); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if p.State() != StateTrusted || p.Source() != "user-consent" {
		t.Fatalf("state=%v source=%q", p.State(), p.Source())
	}
	for _, c := range []Capability{CapProcessExecute, CapNetworkAccess, CapPluginLoad} {
		if err := p.Gate(c); err != nil {
			t.Errorf("trusted Gate(%s) = %v, want nil", c, err)
		}
	}
	if err := p.Escalate(StateUntrusted, "oops"); err == nil {
		t.Error("de-escalation must be refused")
	}
}

// TestN1_RestrictedWhitelist covers the Restricted middle state: only listed
// capabilities pass.
func TestN1_RestrictedWhitelist(t *testing.T) {
	p := &Policy{root: "/repo", state: StateRestricted, source: "admin"}
	p.Allow(CapWorkspaceRead, CapWorkspaceWrite)
	if err := p.Gate(CapWorkspaceRead); err != nil {
		t.Errorf("whitelisted read refused: %v", err)
	}
	if err := p.Gate(CapProcessExecute); !isUntrusted(err) {
		t.Errorf("unlisted process.execute = %v, want refusal", err)
	}
}

// TestN1_FromEnv covers the env policy surface including bad values.
func TestN1_FromEnv(t *testing.T) {
	t.Setenv("OMNILSP_TRUST", "restricted")
	t.Setenv("OMNILSP_TRUST_ALLOW", "workspace.read, formatter.execute")
	p, err := FromEnv("/w")
	if err != nil {
		t.Fatal(err)
	}
	if p.State() != StateRestricted || p.Source() != "env" {
		t.Fatalf("state=%v source=%q", p.State(), p.Source())
	}
	if err := p.Gate(CapFormatterExecute); err != nil {
		t.Errorf("allowed formatter refused: %v", err)
	}
	if err := p.Gate(CapPluginLoad); !isUntrusted(err) {
		t.Errorf("plugin.load = %v, want refusal", err)
	}

	t.Setenv("OMNILSP_TRUST", "bogus")
	if _, err := FromEnv("/w"); err == nil || !strings.Contains(err.Error(), "unknown trust state") {
		t.Errorf("bad env err = %v", err)
	}
}

// TestN4_CompileDbNoShell documents the compile_commands.json policy: OmniLSP
// never replays the original shell string; it extracts semantic flags and
// invokes a trusted compiler directly. The gate refuses the shell-execution
// capability class entirely under untrusted workspaces.
func TestN4_CompileDbNoShell(t *testing.T) {
	p := NewPolicy("/repo")
	if err := CanReplayCompileCommand(p); !isUntrusted(err) {
		t.Errorf("shell replay = %v, want refusal", err)
	}
	if err := ExtractSemanticFlags(p); err != nil {
		t.Errorf("semantic flag extraction = %v, want allowed", err)
	}
}

func CanReplayCompileCommand(p *Policy) error { return p.Gate(CapProcessExecute) }

func ExtractSemanticFlags(p *Policy) error { return p.Gate(CapWorkspaceRead) }
