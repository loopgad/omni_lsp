# ADR 0004: Replay Recording Format v1 — JSONL with Meta Header

- **Status:** Accepted
- **Date:** 2026-08-23
- **Deciders:** OmniLSP core maintainers
- **Relates to:** goal.md §P9 (replay), §P10 (reproduction bundle), §U9 (ADRs)

## Context

goal.md §P9 requires protocol replay: a recorded session must be able to
reconstruct the full lifecycle — initialize, workspace attach, open, change,
request, cancel, save, close, shutdown — and deterministic replay SHOULD use
logical ordering rather than relying on original wall-clock timing. §P9
further recommends that a replay record include the protocol messages,
timestamps or logical sequence, environment/toolchain digests, config hash,
backend versions, and snapshot transitions.

The replay subsystem (`internal/replay/`) needs an on-disk format that:

1. streams naturally — sessions are captured live, one message at a time;
2. carries everything needed to reconstruct context without the original
   machine (environment digests per §P9/P10);
3. supports byte-exact round-trip verification of a recording against a
   fresh server's responses;
4. can evolve as fields are added.

Alternatives considered: a single JSON array (no streaming; a crash mid-
session loses everything), a binary container (opaque, not diffable or
line-greppable), SQLite (heavier than the problem). Line-delimited JSON
wins on every axis for append-only capture.

## Decision

Replay recordings are **JSONL files** (`internal/replay/session.go`,
`internal/replay/recorder.go`), format version **1**
(`FormatVersion = 1`):

> **Amendment (format version, added later).** The decision above stands — the
> JSONL line framing, the meta header on line 1, and the add-only field evolution
> rule are all unchanged. Only the version literal moved: `FormatVersion` is now
> **2** (`internal/replay/session.go`), which adds response-identity evidence to
> the meta payload. v1 recordings stay readable but cannot carry that evidence —
> replaying one returns `ErrSemanticResponseBindingUnverified`. The live
> version surface is `docs/versions.md`; this ADR records the original decision.

- **Line 1 is a meta entry** — `{"seq":0,"dir":"meta","payload":{...}}`
  whose payload carries:
  - `formatVersion` (schema version, currently 2 — see the amendment above);
  - `configHash` (SHA-256-derived digest of the deterministic config, per
    §P9 "config hash");
  - `backends` (language ID → engine version, per §P9 "backend versions");
  - `toolchain` (tool → version digests, per §P9 "environment/toolchain
    digests");
  - `workspaceDir` (optional context).
- **Every following line is one protocol message**: an `Entry` with
  - `seq` — monotonically increasing logical sequence number;
  - `dir` — `"in"` (client→server), `"out"` (server→client), or `"meta"`;
  - `snapRev` — snapshot revision sampled at capture time (§P9 "snapshot
    transitions");
  - `payload` — the raw JSON-RPC message.

**Replay semantics:** the player feeds `"in"` entries in logical sequence
to a fresh server and compares the resulting `"out"` stream against the
recording. Wall-clock timing never participates — ordering comes solely
from `seq`, satisfying §P9's logical-ordering requirement. Round-trip
consistency is guaranteed by `TestP9_RecordReplayRoundTrip`
(`internal/replay/replay_test.go`).

**Format evolution rules:** new fields are additive only — never removed,
never repurposed. Readers MUST ignore unknown payload fields (standard Go
JSON behavior). When a change breaks old readers' ability to interpret a
file correctly, `Meta.FormatVersion` increments and loaders reject
unsupported versions with an explicit error (already implemented in
`LoadSession`).

## Consequences

### Positive

- Streaming-friendly capture: each message is appended atomically as one
  line; a crashed session yields a usable prefix.
- Human-inspectable and tool-friendly: lines sort by `seq`, grep cleanly,
  and diff well between two recordings of the same scenario.
- Self-describing: the meta header alone answers "what environment produced
  this?" without external manifests (complementing, not replacing, the
  fuller §P10 reproduction bundle).
- Deterministic replay is testable end-to-end via the round-trip golden
  test.

### Negative / Risks

- One line per message means large `didChange` payloads dominate file size;
  the loader raises its scanner buffer accordingly (8 MiB). Very large
  workspace syncs could still exceed it — if that bites, chunking is a v2
  concern, not a v1 field.
- No checksums or compression at v1: truncated tails are detectable only as
  parse errors at the cut point. Add integrity framing in a future version
  if corrupted-session reports appear in practice.
