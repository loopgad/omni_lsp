# ADR-0005: `omnilsp verify` as a W0 superset command

## Status

Accepted

## Context

The closed-loop conformance scorecard (internal/conformance) needs a user-facing
exit point beyond the test tree and `doctor --json`. goal.md §W0 lists canonical
commands but predates the scoring system; `verify` is not among them.

## Decision

1. Add `omnilsp verify [--json] [--full] [--min N]` reporting the machine-scored
   acceptance state of the working tree (schema omnilsp.verify.v1).
2. Exit codes extend §W1: 0 = score at/above floor, 1 = below floor, 2 = invalid
   flags. This category is provisional until first stable release, per W1.
3. docs/conformance.md becomes a generated artifact of internal/conformance;
   hand edits fail `test/conformance` (TestGenerateDocs).
4. Deferred milestone domains (x5/x7/x8/x9) keep independent denominators and
   are never merged into the gated core score — no cross-domain coupling.
5. Dependency policy (U5) is enforced mechanically against an explicit whitelist
   (golang.org/x/tools for the Go bridge semantic load, plus its chain), replacing
   the earlier inaccurate "stdlib-only" claim.

## Consequences

- Score regressions are loud in three surfaces: go test, doctor --json, verify.
- The floor constant (defaultScoreFloor) must track the committed baseline file
  when baselines are explicitly raised via OMNISP_UPDATE_CONFORMANCE=1.
