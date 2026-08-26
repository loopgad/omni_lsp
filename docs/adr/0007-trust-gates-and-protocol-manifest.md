# ADR-0007: Workspace Trust Gates and Protocol Manifest Fingerprint

Date: 2026-08-24
Status: Accepted
Clauses: §N1-N4, §U4/§C17 (Y5-4/X9-2)

## Context

goal.md requires workspace trust states (§N1) with Untrusted default, an
operation capability model (§N2), and a prohibition list for untrusted
workspaces (§N3). It also requires "Generated files reproducible" (§Y5) and a
protocol reproducibility check (§C17), while the protocol surface is
hand-written by design.

## Decision

### 1. Trust is a policy input at three fixed gates

`internal/trust` implements the §N1 states verbatim. Enforcement lives at the
three semantic choke points where dangerous operations converge:

| Gate | Location | Capability class |
|---|---|---|
| Backend process start | `supervisor.Start()` via `nested.StartSupervised()` | process.execute |
| Plugin enable | `plugin.Manager.Enable` TrustGate hook | plugin.load |
| Direct checks | `trust.Policy.Gate()` | all eleven §N2 caps |

We deliberately do not sprinkle Gate calls through handlers: every backend
spawn already routes through one function, so one guard there covers pyright,
typescript-language-server, and rust-analyzer alike.

### 2. Product-policy elasticity on the Untrusted default

§N1 says new workspaces SHOULD default to Untrusted "unless product UX/policy
says otherwise". An LSP server only ever starts because an editor explicitly
launched it against a directory; we treat that launch as the user's trust
signal. Therefore:

- `OMNILSP_TRUST` unset → Trusted (`source=implicit-local-launch`)
- any explicit value → parsed and enforced strictly

This keeps every existing workflow green while giving CI/sandbox deployments a
single env var to lock down.

### 3. Restricted is a whitelist, not a blacklist

Restricted mode grants exactly the capabilities listed in
`OMNILSP_TRUST_ALLOW`; everything else is refused (same deny-by-default shape
as plugin grants, ADR-consistent with §O2).

### 4. Protocol reproducibility via canonical manifest, not codegen

The protocol types are hand-written by architecture decision (declarations
only, mirrored against LSP 3.17). Introducing metamodel-driven codegen would
invert that decision for marginal benefit. Instead,
`scripts/gen-protocol.go -check` fingerprints every exported declaration of
`internal/protocol/lsp/types.go` into `protocol.manifest` (byte-deterministic:
sorted lines, no timestamps, no map iteration order). Drift fails CI; updates
are deliberate diffs. The fingerprint check is the reproducibility guarantee;
true codegen stays post-X9+ if ever needed.

## Consequences

- Y3-1 (trust gates), Y1-2 (bounded caches), Y5-4/X9-2 (reproducibility)
  score mechanically via conformance probes.
- Untrusted mode refuses backend starts loudly (supervisor stays Disabled,
  reason surfaces via state-change callback) — silent degradation is avoided.
- The manifest adds a maintenance step to protocol edits: regenerate after
  intentional type changes (`go run scripts/gen-protocol.go`).
