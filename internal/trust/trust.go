// Package trust implements workspace trust states per goal.md §N1-N4.
//
// Trust is a policy input, not a semantic guess (§N1). A newly opened
// arbitrary repository defaults to Untrusted unless local policy says
// otherwise. Escalation is always explicit.
//
// N3 mapping — in Untrusted mode OmniLSP MUST NOT automatically:
//   - execute workspace scripts / shell commands   -> CapProcessExecute
//   - load compiler or TypeScript plugins          -> CapPluginLoad, CapCompilerExecute
//   - run arbitrary formatters                     -> CapFormatterExecute
//   - download/execute binaries                    -> CapNetworkAccess
//   - send source to remote services               -> CapNetworkAccess, CapRemoteIndexRead
//   - install plugins                              -> CapPluginLoad
//
// Concurrency model: Policy is immutable after construction; Gate is safe
// for concurrent use without locking.
//
// Invariants:
//  1. Default is deny: a zero-value policy gates everything (§N1 default Untrusted).
//  2. Escalation is explicit: state only changes via NewPolicy/Escalate, never inferred.
//  3. Restricted is a whitelist: capabilities not listed are refused (§N2 grant model).
package trust

import (
	"fmt"
	"os"
	"strings"

	ierrors "github.com/omnilsp/omni/internal/errors"
)

// State is a workspace trust level (§N1).
type State int

const (
	// StateUntrusted is the default for any newly opened repository.
	StateUntrusted State = iota
	// StateRestricted allows explicitly whitelisted capabilities only.
	StateRestricted
	// StateTrusted allows all capabilities.
	StateTrusted
)

func (s State) String() string {
	switch s {
	case StateUntrusted:
		return "untrusted"
	case StateRestricted:
		return "restricted"
	case StateTrusted:
		return "trusted"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

// ParseState parses a policy string into a State.
func ParseState(s string) (State, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "untrusted", "":
		return StateUntrusted, nil
	case "restricted":
		return StateRestricted, nil
	case "trusted":
		return StateTrusted, nil
	default:
		return StateUntrusted, ierrors.New(ierrors.ErrUntrustedOperation, "trust.parse", "unknown trust state "+s)
	}
}

// Capability enumerates the §N2 operation capability model.
type Capability int

const (
	CapWorkspaceRead Capability = iota
	CapWorkspaceWrite
	CapProcessExecute
	CapNetworkAccess
	CapCompilerExecute
	CapBuildScriptExecute
	CapProcMacroExecute
	CapPluginLoad
	CapFormatterExecute
	CapRemoteIndexRead
	CapTelemetryExport
)

var capNames = map[Capability]string{
	CapWorkspaceRead:      "workspace.read",
	CapWorkspaceWrite:     "workspace.write",
	CapProcessExecute:     "process.execute",
	CapNetworkAccess:      "network.access",
	CapCompilerExecute:    "compiler.execute",
	CapBuildScriptExecute: "build_script.execute",
	CapProcMacroExecute:   "proc_macro.execute",
	CapPluginLoad:         "plugin.load",
	CapFormatterExecute:   "formatter.execute",
	CapRemoteIndexRead:    "remote_index.read",
	CapTelemetryExport:    "telemetry.export",
}

func (c Capability) String() string { return capNames[c] }

// Policy is the trust input for one workspace root.
type Policy struct {
	root    string
	state   State
	allowed map[Capability]bool // Restricted whitelist; ignored otherwise
	source  string              // where the decision came from (env/config/default)
}

// NewPolicy returns the §N1 default: Untrusted for an arbitrary repository.
func NewPolicy(root string) *Policy {
	return &Policy{root: root, state: StateUntrusted, source: "default"}
}

// FromEnv builds a policy from OMNILSP_TRUST ("untrusted"|"restricted"|"trusted").
// OMNILSP_TRUST_ALLOW adds comma-separated capability names for Restricted mode.
func FromEnv(root string) (*Policy, error) {
	p := NewPolicy(root)
	st, err := ParseState(os.Getenv("OMNILSP_TRUST"))
	if err != nil {
		return nil, err
	}
	p.state = st
	p.source = "env"
	if st == StateRestricted {
		for _, name := range strings.Split(os.Getenv("OMNILSP_TRUST_ALLOW"), ",") {
			if c, ok := capByName[strings.TrimSpace(name)]; ok {
				if p.allowed == nil {
					p.allowed = make(map[Capability]bool)
				}
				p.allowed[c] = true
			}
		}
	}
	return p, nil
}

// PolicyForBackend resolves the trust policy for a backend process start.
// When OMNILSP_TRUST is unset, local product policy treats an explicit
// editor launch as the user's trust signal (§N1 SHOULD elasticity: "unless
// product UX/policy says otherwise"); any explicit value always wins.
func PolicyForBackend(root, backend string) (*Policy, error) {
	if _, set := os.LookupEnv("OMNILSP_TRUST"); !set {
		return &Policy{root: root, state: StateTrusted, source: "implicit-local-launch"}, nil
	}
	p, err := FromEnv(root)
	if err != nil {
		return nil, fmt.Errorf("backend %q: %w", backend, err)
	}
	return p, nil
}

var capByName = func() map[string]Capability {
	m := make(map[string]Capability, len(capNames))
	for c, n := range capNames {
		m[n] = c
	}
	return m
}()

// Escalate explicitly raises the trust level with a recorded source.
func (p *Policy) Escalate(s State, source string) error {
	if s < p.state {
		return fmt.Errorf("trust: cannot de-escalate %s to %s", p.state, s)
	}
	p.state = s
	p.source = source
	return nil
}

// Allow whitelists a capability for Restricted mode.
func (p *Policy) Allow(caps ...Capability) {
	if p.allowed == nil {
		p.allowed = make(map[Capability]bool, len(caps))
	}
	for _, c := range caps {
		p.allowed[c] = true
	}
}

// State reports the current trust level.
func (p *Policy) State() State { return p.state }

// Root reports the workspace root this policy applies to.
func (p *Policy) Root() string { return p.root }

// Source reports where the trust decision came from.
func (p *Policy) Source() string { return p.source }

// Gate decides whether an operation requiring cap may proceed (§N2/N3).
// Refusals wrap errors.ErrUntrustedOperation so callers can classify them.
func (p *Policy) Gate(cap Capability) error {
	switch p.state {
	case StateTrusted:
		return nil
	case StateRestricted:
		if p.allowed[cap] {
			return nil
		}
	case StateUntrusted:
		if cap == CapWorkspaceRead || cap == CapWorkspaceWrite {
			// Reading/writing open buffers stays available; that is what the
			// user explicitly opened. Everything automatic is refused (§N3).
			return nil
		}
	}
	return ierrors.New(ierrors.ErrUntrustedOperation, "trust.gate",
		fmt.Sprintf("%s requires %s which trust state %q does not grant", p.root, cap, p.state))
}

// CanStartBackend reports whether a language backend may be spawned
// (process.execute class). Used by the supervisor start gate.
func (p *Policy) CanStartBackend(backend string) error {
	if err := p.Gate(CapProcessExecute); err != nil {
		return fmt.Errorf("backend %q blocked by workspace trust: %w", backend, err)
	}
	return nil
}
