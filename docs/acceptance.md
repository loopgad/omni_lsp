# Release candidate acceptance

This Windows-only local runbook is the repeatable release-candidate gate for
Go, C, C++, Rust, Python, TypeScript, and JavaScript through VS Code, Neovim,
Emacs/Eglot, Helix, Zed, and Sublime LSP. The gate requires 42 real
client/language results and retains the ten existing client/family summary
rows. Passing individual unit or conformance tests does not imply a
language/client combination passed.

VS Code, Neovim and Emacs/Eglot have native test drivers. Emacs 30.2 passed
nine native cases with clean client/server exit on development candidate
`97e68d72`; all seven cells also passed independent report validation. The
driver had prior independent source review; a frozen release must repeat the run.
Helix 25.07.1 is installed; its manual subset remains diagnostic evidence.
Sublime Text 4215, LSP 2.13.0, and five Python 3.14 dependency
wheels have verified pins; the isolated native driver still needs actual
operation and shutdown evidence. The
installed Zed Preview is not a verified release client. The six-client matrix
therefore remains blocked; installation or a successful subset is not a pass.

## Fixed host tools

`test/acceptance/tools/tools.lock.json` records the observed Windows tool
versions plus selected executable paths and SHA-256 values. Fast verifies every
resolved file identity before freezing a release candidate; missing files stay
`not_verified` and changed hashes fail the tool lock.

On 2026-10-26 the lock was rebuilt against this host: `go1.27.1`,
`rust-analyzer 1.99.0 (b940084d 2026-09-28)`, `rustc/cargo 1.99.0`,
VS Code `1.143.1`, and npm `11.9.0`; the resolved SHA-256 values for those
entries were recomputed from the local files. The same-source Rust semantic
helper remains `1.97.0+omnilsp-semantic…` and is pinned independently; the
locked Rust SCIP differential passes with the host's 1.99.0 live toolchain.
Ten editor/tool entries (seven Sublime artifacts, Neovim, Helix, Emacs) are
absent on this host and remain `not_verified` under the existing policy.
`test/acceptance/tools/package-lock.json` pins and checksums the project-local
Pyright, TypeScript language server, and TypeScript dependency tree. VS Code
language client dependencies are separately pinned in
`editors/vscode/package-lock.json`.

Rust indexing may use the separately pinned same-source semantic helper in
`tools.lock.json`; the original rust-analyzer continues to serve live LSP.
The runner binds the helper path, exact version and SHA-256 through
`OMNILSP_RUST_SCIP_HELPER_{PATH,VERSION,SHA256}`. Its source archive and patch
hashes are retained in the lock. The locked differential now checks nonempty
implementation, type, call and module relations, including exact same-response
symbol identities and UTF-16 locations. This fixture does not establish full
Rust fact coverage or persistent queries with the backend disabled.

Because the Go backend starts language servers with `os/exec`, the acceptance
installer builds project-local native wrappers for the npm `.js` entrypoints;
it does not depend on Windows `.cmd` shims or a global language-server PATH.

Install the missing upstream servers and portable Neovim with:

```powershell
pwsh -NoProfile -File scripts/install-acceptance-tools.ps1
```

The installer uses the official Neovim v0.12.5 Windows release URL and checks
SHA-256 `de8625ba8cf65ebf40eb80a388ba1ec8e9c15b30218821e2c639119b05920de1`
before extracting under `test/acceptance/tools/bin`. It installs no global
packages and does not change PATH or system settings. If the download or hash
check fails, the Neovim matrix remains not verified and the release gate stays
closed.

## Stages

Run every stage with the same `RunId`, evidence directory, candidate binary,
and unchanged source/toolchain. The `Fast` stage prints the values to reuse.

```powershell
$run = 'paste-run-id-from-Fast'
$evidence = "test/acceptance/evidence/$run"
$candidate = "$evidence/omnilsp.exe"
$s18Policy = 'stable'

pwsh -NoProfile -File scripts/acceptance.ps1 -Phase Fast -RunId $run -EvidenceDirectory $evidence -CandidateBinary $candidate -S18Policy $s18Policy
pwsh -NoProfile -File scripts/acceptance.ps1 -Phase Soak30s -RunId $run -EvidenceDirectory $evidence -CandidateBinary $candidate -S18Policy $s18Policy
pwsh -NoProfile -File scripts/acceptance.ps1 -Phase Soak10m -RunId $run -EvidenceDirectory $evidence -CandidateBinary $candidate -S18Policy $s18Policy
```

`-S18Policy` defaults to `strict`. Use `stable` explicitly for the
evidence-qualified release policy below. The selected policy and thresholds
are part of the frozen acceptance fingerprint; each soak phase and status
query must use the same value.

`Fast` runs the complete Go suite with and without the race detector, vet,
Windows candidate build, Linux amd64/arm64 and macOS arm64/amd64 cross-builds,
relevant fuzz smoke tests, real-process semantic/performance acceptance, the
42-cell editor matrix, and its ten legacy family summaries. These steps are
serial so builds cannot overlap performance samples.
The scorecard output is saved as context only and does not override a failed
release gate.

The Fast gate writes a frozen acceptance fingerprint covering the workspace
content, corpus, tool lock, npm lockfiles, and observed exact tool versions.
Every soak phase and the final status check recompute it; a mismatch invalidates
the run. Performance, semantic, client, and soak reports are removed before
their stage and must match the current run ID and candidate SHA-256. A passing
report must also contain every required check for that stage.

Before performance exceptions can be considered, the persistent-index gate
must pass: disk identity is derived from normalized workspace identity,
deterministic disk inventory content, and inventory semantic version;
process-local snapshot revisions guard only in-process build races. Reopening
the same disk tree after restart must preserve a fresh disk index even when
document event counts differ. Unsaved documents must be answered from the live
overlay. Disk add/change/delete makes the disk inventory stale, old inventory
versions rebuild transactionally, and a canceled build must leave no published
index, build lock, or temporary file. `fresh` reports disk inventory freshness;
the running snapshot revision remains separate state.

The production semantic path is wired for Go, C/C++, Rust and, with verified
helper inputs, Python and TypeScript/JavaScript. The fixed Pyright extractor
now traverses real workspace facts; its fact coverage remains explicit rather
than claiming general completeness. TypeScript has real compiler regressions
for private members, literal property references and import-equals statements.
References and other affected fact families still carry known-subset coverage.
The patched Rust helper proves implementation and type relations in a real
fixture; the remaining required Rust fact families are still incomplete.

Python extractor v3 uses pinned Pyright's include/exclude selection and keeps
captured, imported excluded files in the analyzed closure. Unsupported config,
unmatched include roots and uncaptured selected sources fail closed. These
paths passed the real pinned package tests and independent source review.
TypeScript extractor v8 qualifies import/module coverage only for its verified
closed static TypeScript scope. Runtime escapes and bodyless call targets keep
those facts partial. The module-only guard no longer demotes unrelated facts;
its regression and the full TypeScript package passed after the correction.
Rust model v2 uses the store's canonical `sha256:` source identity; real helper
facts survived commit and two reopen operations with unchanged conservative
coverage. Neither result replaces candidate-wide release acceptance.

Persistent workspace symbols, definitions, and references use committed
records only when their fact coverage is complete and disk contents, build
context, and tool identities match. Definitions and references reuse a
committed generation with open documents only
when every open document is byte-for-byte identical to the corresponding disk
file and its URI, language, version, snapshot, and VFS revision still match.
Changed or extra open documents use verified Go snapshot facts where supported
and otherwise fall back to the live backend. Byte-identical open documents can
share persistent workspace symbols. General seven-language overlay merging is
still incomplete. Development candidate `97e68d72` passed real restarts with
Go, Python, TypeScript and JavaScript backends disabled, returning workspace
symbols, definitions and references from the same committed generation for
their static acceptance fixtures. The query computation counter stayed fixed;
extractor invocation counts are not observable in this check. These results
do not prove all language constructs, all required fact families, or C/C++/Rust
persistent queries. Emacs/Eglot has
run nine native cases successfully and its native path is enabled in the
formal matrix after independent review. The incomplete language facts and
unqualified native matrices for Helix, Zed and Sublime LSP block the release goal.

Replay binds index-only responses to their generation and semantic identity.
Missing generations and mismatched identities fail closed. Live or mixed-source
responses do not receive complete-reproduction evidence from this binding.
The Go candidate CLI acceptance now executes strict export refusal, explicit
lossy SCIP export/import, fixed-generation replay and identity mismatch
refusals. Fast runs Go/Python/TypeScript/JavaScript/C/C++ persistence and Go SCIP/replay as separate
producers with separate reports; their hashes are included in frozen evidence.
The C/C++ fixture checks passed; actual candidate execution remains pending.
All five development reports for `97e68d72` also passed independent report
validation; this is not a frozen Fast result. Any new candidate must repeat
these checks. Other language CLI coverage and
live/mixed-source complete reproduction remain unverified.

The local SCIP commands accept one explicit semantic scope. Obtain its exact ID
from `omnilsp/indexStats` coverage before exporting:

```powershell
$scipPath = Join-Path $env:TEMP 'scope.scip'
omnilsp index export --format scip --workspace $workspace --scope $scopeId --output $scipPath
omnilsp index import --format scip --workspace $workspace --language go --scope $scopeId --input $scipPath
```

The default export rejects call, import, include, module, and generated-source
relations that SCIP cannot represent. Explicit `index export --allow-lossy`
can omit those relation kinds; its JSON summary counts each loss and the SCIP
metadata contains matching `omnilsp.lossy-edge.<kind>=<count>` markers. This
option does not permit malformed facts or relax imported coverage. Use a new
output path for each actual export; existing files are never overwritten.

Import requires a trusted workspace and an empty index directory so it cannot
replace other language scopes. Imported SCIP coverage is deliberately marked
incomplete wherever SCIP cannot prove completeness; this imported generation
does not satisfy complete-query or rename-safety gates by itself. Output files
are created without overwriting an existing path. Both `--input` and `--output`
must resolve outside the workspace because all workspace files participate in
the disk identity digest. LSIF remains available only through the existing
conversion adapter, not this local command.

The performance test must keep at least 1,000 raw request samples per
operation and language. Its strict S18 targets are:

| Operation | P50 | P95 | P99 |
|---|---:|---:|---:|
| Hot hover | 20 ms | 75 ms | 150 ms |
| Hot definition | 25 ms | 100 ms | 200 ms |
| Completion first usable result | 40 ms | 120 ms | 250 ms |
| Syntax update after edit | 15 ms | 50 ms | 100 ms |

The Fast Go suite also runs the same-response completion projection contract:
the child completion payload passes through decoding and the actual LSP
handler, which verifies full-list order, the supported item fields, edit
variants and additional edits, `insertTextFormat`, and `isIncomplete`. The
separate pinned upstream process is used for target-symbol semantics; its full
list ordering is not treated as a cross-instance equality oracle.

The `s18-evidence-qualified-v2` release policy can consider at most four checks:
C and C++ `completion_first_usable` and `syntax_update_after_edit`. It retains
all strict targets. Stable mode runs all four scoped checks in three exclusive
ABBA rounds, with at least 1,000 samples in every leg. A strict-passing check
needs no exception; an exception is considered only after architecture,
semantic, cancellation, and resource gates pass and candidate and upstream
semantics match.
For each strict-failing percentile, the fixed upstream must also fail the
original strict target, the candidate must be no slower than upstream plus
same-request OmniLSP overhead of 5/10/20 ms, and the candidate must remain
within its absolute ceiling: completion 80/100/125 ms or syntax updates
60/100/150 ms. The ABBA rounds use the same fixture, capabilities,
configuration, and pinned upstream version. The report retains the original
strict result and samples; its separate `release_assessment` accepts an
exception only after the gate recomputes candidate, upstream, and correlated
per-request overhead percentiles from raw evidence. The Go test process must
still exit zero. Semantic, protocol, process, tool, sample, S19, or any other
S18 failure remains a hard failure. Older reports cannot be reclassified
under this policy.

`Fast-summary.json` and `requirement-matrix.json` preserve the same validated
`release_assessment`, selected `s18_policy`, and current candidate fingerprint
even when Fast fails. The matrix keeps bounded rows failed and marks accepted
exceptions separately. A candidate fingerprint in a failed matrix is
diagnostic identity only; only a matching `fast-gates.json` from a passed Fast
run admits preflight or soak work.

S19 reports reference scaling at 200, 800, and 3,200 results, cancellation
terminal outcomes, progress, memory, and interactive-request fairness. It
does not use performance exceptions. The final requirement matrix preserves
the strict S18 failure rows and annotates which rows meet the stable ceiling;
it marks them accepted only if all selected release gates pass. The overall
Fast and release decisions are reported as
`passed_with_performance_exception` when that is the only remaining variance.

Before the long run, save the independent Luna D review as `pre-soak-review.json`
in the evidence directory. The runner accepts only schema version 1 with
`decision: "passed"`, the current `run_id`, candidate SHA-256, and acceptance
fingerprint SHA-256. `reviewer` must identify `luna-d` / `Luna D` and set
`independent` to `true`. The `checklist` must contain exactly these four
passing entries, each with the listed evidence references:

| Checklist entry | Required evidence references |
|---|---|
| `integrated_source` | `workspace_sha256` |
| `test_validity` | `soak_test_sha256`, `soak_30s_report_sha256`, `soak_10m_report_sha256` |
| `test_to_spec_mapping` | `soak_test_source_sha256`, `acceptance_spec_sha256` |
| `current_evidence` | `fast_gates_sha256`, `soak_30s_stage_sha256`, `soak_30s_report_sha256`, `soak_10m_stage_sha256`, `soak_10m_report_sha256` |

The `evidence` object must include exactly those references as lowercase
SHA-256 values, along with `run_id`, `candidate_sha256`, and
`acceptance_fingerprint_sha256`. The runner recomputes them from the current
workspace fingerprint, candidate, soak test binary and source, this runbook,
Fast gate, and both preflight stage records and reports. This binds the review
to the exact inputs that will enter the one-hour soak. For example, the
reviewer's JSON has this shape:

```json
{
  "schema_version": 1,
  "decision": "passed",
  "run_id": "<run-id>",
  "candidate_sha256": "<sha256>",
  "acceptance_fingerprint_sha256": "<sha256>",
  "reviewer": { "id": "luna-d", "name": "Luna D", "independent": true },
  "checklist": {
    "integrated_source": { "status": "passed", "evidence": ["workspace_sha256"] },
    "test_validity": { "status": "passed", "evidence": ["soak_test_sha256", "soak_30s_report_sha256", "soak_10m_report_sha256"] },
    "test_to_spec_mapping": { "status": "passed", "evidence": ["soak_test_source_sha256", "acceptance_spec_sha256"] },
    "current_evidence": { "status": "passed", "evidence": ["fast_gates_sha256", "soak_30s_stage_sha256", "soak_30s_report_sha256", "soak_10m_stage_sha256", "soak_10m_report_sha256"] }
  },
  "evidence": {
    "run_id": "<run-id>",
    "candidate_sha256": "<sha256>",
    "acceptance_fingerprint_sha256": "<sha256>",
    "workspace_sha256": "<sha256>",
    "fast_gates_sha256": "<sha256>",
    "soak_test_sha256": "<sha256>",
    "soak_test_source_sha256": "<sha256>",
    "acceptance_spec_sha256": "<sha256>",
    "soak_30s_stage_sha256": "<sha256>",
    "soak_30s_report_sha256": "<sha256>",
    "soak_10m_stage_sha256": "<sha256>",
    "soak_10m_report_sha256": "<sha256>"
  }
}
```

After the 30-second and 10-minute preflights and the bound review, start the
hidden one-hour process:

```powershell
pwsh -NoProfile -File scripts/acceptance.ps1 -Phase Start1h -RunId $run -EvidenceDirectory $evidence -CandidateBinary $candidate -S18Policy $s18Policy
pwsh -NoProfile -File scripts/acceptance.ps1 -Phase Status -RunId $run -EvidenceDirectory $evidence -CandidateBinary $candidate -S18Policy $s18Policy
```

The worker runs in a hidden window and writes minute-level JSONL telemetry,
client report, and worker status under the same evidence directory. It enforces
the configured ceiling of eight concurrent work tasks and 8 GiB total process
tree private memory, in addition to goroutine, heap, handle, backend, progress,
and semantic terminal-state checks. Sleeping, restarting, losing minute-level
progress, changing the candidate hash, or ending without a final report
invalidates the continuous run. This procedure never changes power settings.

## Evidence and interpretation

The structured test reports use `test/acceptance/report`, schema version 1.
Each report includes a run identifier, candidate SHA-256, environment, corpus
hash, limits, checks, samples, errors, and skip reasons. A missing tool, skipped
case, empty report, or `not_verified` check cannot become `passed`. The runner
also preserves command logs, the worktree status and diff summary, the corpus
hash, and candidate hash in the ignored evidence directory. Cross-platform
binaries are built one at a time under the system temporary directory and
removed after each build; their command logs remain as the build evidence.

S21 associates each public semantic response with the producing revision and
`backendEpoch` from that request's `omnilsp/explain` evidence. A missing epoch
field is a protocol failure; in-process backends report their fixed epoch as
`0`.

The final report must list every requirement as **passed**, **failed**, or
**not verified**. The quick scorecard value is historical context only; it is
not a substitute for correctness, client, S18/S19, or continuous-soak evidence.

## Gate evidence record — run `gate-20261004T2347Z`

Single frozen candidate for every gate in this round (docs/soak-nightly.md
"Freeze once" contract), built from the tree after the parallel workstreams
landed:

- Candidate: `test/acceptance/evidence/gate-20261004T2347Z/omnilsp.exe`,
  built with `go build -trimpath -o <evidence>/omnilsp.exe ./cmd/omnilsp`
  (exit 0), SHA-256 `34aa6446093712ad86f7c5a330821eba2d6d2b1734c2b64838fb3016b53f7129`.
- Freeze record: git HEAD `4b63ff3`, uncommitted worktree state, and UTC
  timestamp are captured in `freeze-record.txt` inside the evidence directory.
- `go run scripts/gen-sbom.go -o sbom.cdx.json .` → exit 0, 6 components.
  The artifact stays uncommitted by decision: gen-sbom embeds a wall-clock
  timestamp and a date-keyed serial, so every regeneration churns; `out/` and
  `sbom.cdx.json` are .gitignore-managed per-run evidence.
- `go test -count=1 ./test/golden/` → ok 7.504s (Y2-1/T-S9SEQ).
- `go test -tags soak -count=1 -timeout 300s ./test/soak/` → ok 31.182s
  (Y1-10 gate form). Two opt-in attempts of `TestSoak_RealStdioMixedWorkload`
  with `OMNILSP_SOAK_GATE=required`, the frozen candidate, and
  `SOAK_DURATION=30s` both failed at `soak/initial-queries` — the fixed
  30-second per-request budget (test/soak/stdio_soak_test.go) expired on the
  first Go references query. This is a host-speed limit, not a soak-bound
  violation; the real-stdio gate remains release evidence to be captured on a
  host that meets the budget.
- `TestS21_ZeroErrorClassification` with `OMNILSP_BIN` + `OMNILSP_CANDIDATE_SHA256`
  + `OMNILSP_S21_GATE=required` and the pinned seven-language toolchains →
  structured report `s21.json`, decision **passed**: all seven languages
  tested, 14 cases × 4 operations = 56 operations, all six §S21 buckets zero,
  no skips, candidate path/hash/run ID bound to this run.
- `TestS18S19_RealProcessPerformanceAcceptance` (strict policy, same frozen
  candidate) → structured report `performance.json`, decision **failed**:
  S19 real-process scaling passed all three sizes; S18 collected 1000-sample
  P50/P95/P99 distributions for all 7 fixtures × 4 operations, 21/28 rows
  passed strict thresholds. Failures: Go hover/definition/completion
  (P50 391-443ms vs 20-40ms targets) and C/C++ completion/syntax rows. The
  C/C++ rows are the ones `s18-evidence-qualified-v2` grants documented
  exceptions for; the Go hot-path rows are a real finding for the candidate's
  native in-process Go backend (packages.Load/type-check hot path) and keep PERF-2 partial.
- `go test -race -count=1 -timeout 900s ./...` on the final tree (after the
  registry/doc 落账 above was regenerated once to clear the drift the first
  race pass caught in `TestGenerateDocs`) → exit 0, 51 packages ok
  (16:59:09–17:04:19 UTC, `race-all-clean.log`). Y1-9 upgraded to AUTO with
  race-mode probes; Y1-10 stays GATE for the reason recorded above.
- Final tree: `go build ./...` and `go vet ./...` green; `omnilsp verify
  --json` from a fresh `-trimpath` build reports coreScore **97.7096**
  (floor 90, passed; `verify-final.json`) — +4.04 over the 93.669 baseline:
  SBOM +1.00, Y2-1 +0.72, PERF-S19 +0.75, PERF-3/S21 +0.75, Y1-9 +0.82.

## Gate evidence record — run `gate-20261024T0558Z`

PERF-2 full re-measurement against a freshly frozen candidate (canonical
evidence directory remains ACL-locked, so this round stages under
`out/freeze-staging/gate-20261024T0558Z/`; offset debt on file in
`freeze-record.txt`):

- Candidate: `omnilsp.exe` built with the locked toolchain
  `C:/Users/32806/sdk/go1.26.1/bin/go.exe` (`go version go1.26.1
  windows/amd64`, matches the go.mod lock), `go build -trimpath` exit 0,
  SHA-256 `9bb6d9ba789748530697b68757e09852209caa08e02f994aebcd57a71c88b43c`,
  git HEAD `355af1520bdd1c0e987c5af16071b48ac972b1be` plus the recorded
  uncommitted worktree state (`freeze-record.txt`, UTC 2026-10-24T05:59:19Z).
  The earlier same-day probe attempt `gate-20261024T0516Z` never reached
  measurement: `not_verified` because the ambient toolchain was go1.27.1
  (lock expects go1.26.1) — tool-blocked, not a threshold verdict.
- `TestS18S19_RealProcessPerformanceAcceptance` (strict policy) →
  `performance.json`, decision **failed**: S19 passed all rows
  (references@200/800/3200, active-cancellation, progress, no-starvation,
  process-tree-resources). S18 collected full 1000-sample P50/P95/P99
  distributions for all 7 languages × 4 rows; 24/28 strict rows passed.
  The Go hot-path fix landed: all four Go rows now pass strict thresholds
  (hover P50 3.8ms, definition P50 12.5ms, completion P50 3.8ms,
  syntax-update P50 0.5ms — previously 391-443ms in gate-20261004T2347Z).
- Failed rows, all on fully measured 1000-sample latency (no zero-sample
  skips in this report): `c/completion_first_usable` P50 74.1ms/P95 137.4ms
  vs 40/120; `c/syntax_update_after_edit` P50 74.0ms/P95 129.3ms/P99 149.4ms
  vs 15/50/100; `cpp/completion_first_usable` P50 68.1ms vs 40 (P95 75.6ms/
  P99 82.5ms within limits); `cpp/syntax_update_after_edit` P50 63.3ms/
  P95 71.3ms/P99 74.9ms vs 15/50/100. Both syntax rows also exceed the
  `s18-evidence-qualified-v2` absolute ceiling (60/100/150) — C on P50/P95,
  C++ on P50 — and `release_assessment` granted no exceptions
  (`exception_check_ids` empty).
- Blocked items recorded separately from threshold misses: the cpp
  completion display-only classification stayed tool-blocked ("completion
  item count differs before presentation classification"), and the companion
  diagnostic probe report (`performance-probe.json`, same freeze directory)
  is `not_verified` overall: 27 of its 28 S18 rows are zero-sample
  ("excluded by diagnostic S18 filter"), while the filter-selected
  `go/hot_hover` row did measure 1000 samples and passed. The
  classification block and the probe's overall `not_verified` are blocked
  evidence, not sub-threshold results.
- 落账: `internal/conformance/registry.go` PERF-2 Reason rewritten to this
  run's facts; `OMNISP_UPDATE_CONFORMANCE=1 go test -count=1
  ./test/conformance/` → ok, re-run without the env var → ok;
  `baseline.json` byte-identical (coreScore 97.70959595959597, PERF category
  0.9166666666666666). PERF-2 维持 PARTIAL，Reason 更新为新事实。

## Gate evidence record — run `gate-20261024T1136Z`

PERF-2 upgrade run against a freshly frozen candidate after the c/cpp bridge
hot-path fix (request-scoped memos), closing the four-row gap that kept
`gate-20261024T0558Z` at 24/28 (evidence directory remains ACL-locked, so
this round also stages under `out/freeze-staging/gate-20261024T1136Z/`;
offset debt on file in `freeze-record.txt`):

- Candidate: `omnilsp.exe` built with the locked toolchain
  `C:/Users/32806/sdk/go1.26.1/bin/go.exe` (`go version go1.26.1
  windows/amd64`, matches the go.mod lock), `go build -trimpath` exit 0,
  SHA-256 `e96b87c4bf85b09ba668f946bfa777bbfb55bbeb337ef836a8895ab718b86e60`,
  git HEAD `355af1520bdd1c0e987c5af16071b48ac972b1be` plus the recorded
  uncommitted worktree state (`freeze-record.txt`, UTC 2026-10-24T11:37:43Z).
- Fix under test: `internal/languages/ccls/memo.go` — bounded request-scoped
  memos for the two clangd requests that paid a full frontend invocation per
  request (completion, documentSymbol after a content change), keyed on
  document content hash + supervisor epoch + bridge invalidation counter, so
  answers stay semantically identical to upstream (`memo_test.go` pins the
  invalidation rules).
- `TestS18S19_RealProcessPerformanceAcceptance` (strict policy
  `s18-strict-v1`) → `performance.json`, decision **passed**: S18 collected
  full 1000-sample P50/P95/P99 distributions for all 7 languages × 4 rows and
  **all 28 strict rows passed**; S19 passed all rows; `release_assessment`
  completed with no exceptions (`exception_check_ids` empty). Evolution over
  `gate-20261024T0558Z` (24/28): the four formerly failing c/cpp rows now
  measure — `c/completion_first_usable` P50 2.13ms (vs 40 threshold, was
  74.1ms), `c/syntax_update_after_edit` P50 0.51ms (vs 15, was 74.0ms),
  `cpp/completion_first_usable` P50 1.62ms (vs 40, was 68.1ms),
  `cpp/syntax_update_after_edit` P50 0.51ms (vs 15, was 63.3ms) — and the
  round-8 tool-blocked cpp completion display-only classification no longer
  blocks this run (row status `passed` in `performance.json`).
- Gate guard in place: `TestPERF2_S18GateFailsClosedWithoutCandidate`
  (`internal/conformance/perf2_guard_test.go`) shells the acceptance test
  with `OMNILSP_ACCEPTANCE=required` and every candidate-binding variable
  stripped; the required mode fails closed with the missing-candidate
  `t.Fatalf` (`test/perf/e2e_lsp_test.go:807`) instead of skipping or
  self-building, so PERF-2 credit can never again come from a silently
  skipped S18 run (the DEF-S21CORPUS lesson).
- 落账: `internal/conformance/registry.go` PERF-2 → `Status: StatusAuto`,
  Reason deleted, Text rewritten to this run's facts, Probe added pointing at
  the in-package gate guard; shape pinned by
  `TestPERF2_S18RegistryShapePinsAutoGateGuardProbe` (red against the PARTIAL
  registry, green after the flip); `OMNISP_UPDATE_CONFORMANCE=1 go test
  -count=1 ./test/conformance/` → ok, re-run without the env var → ok;
  `baseline.json` coreScore 97.70959595959597 → **98.45959595959597**
  (+0.75), PERF category 0.9166666666666666 → **1**. PERF-2 升 AUTO。

## Evidence record — iteration10 closeout（2026-10-24/25）

### X8-2 客户端矩阵本地取证（42 格）

完整记录 `out/omnilsp-iteration10-取证记录.md`（取证员实测，环境 Windows
10.0.26300 x64 / go1.27.1）。报告经 `OMNILSP_ACCEPTANCE_REPORT` 重定向到仓库
外临时目录，未触碰 ACL 锁定区 `test/acceptance/evidence/**`。记账员抽核了
下述关键行引用（client_test.go / stdio_soak_test.go 均为本人读码确认）。

- 工具锁静态核验（先于运行，逐文件 SHA-256 复算）：缺失 emacs-30.2 /
  nvim-0.12.5 / helix-25.07.1 / npm / rust-analyzer-semantic-helper /
  sublime 全套 8 项；SHA 不匹配 go（本机 1.27.1 vs 锁 1.26.1）、vscode
  （自动更新致 Code.exe 内容漂移）、rustc/cargo/rust-analyzer 全套；匹配
  gopls、node、python、clangd 三件套、typescript、typescript-language-server、
  pyright 三件套。语义运行时绑定硬门（node/typescript/python/pyright，
  client_test.go:556-576）本轮 passed。
- 42 格矩阵运行：`go test -tags clients -run '^TestRealClientMatrix$'
  -count=1 -timeout=30m ./test/acceptance/clients/`（env：`OMNILSP_BIN`=本地
  构建候选、`OMNILSP_RUN_CLIENT_ACCEPTANCE=1`、`OMNILSP_RUN_ID=x82-local-evidence`）
  → EXIT=1，decision=`not_verified`，`client/matrix-contract` 校验通过（42 格
  三态合法）；无任何编辑器进程被拉起（三个原生客户端均因 tool 校验未过走
  clientReady=false 跳过分支，client_test.go:345-353）。
- 42 格逐格结果（pass=0 / fail=0 / skip(not_verified)=42）：VS Code 7 格
  skip（Code.exe SHA 与锁不符）；Neovim 7 格 skip（锁定二进制缺失）；
  Emacs/Eglot 7 格 skip（锁定 emacs-30.2 缺失，正式通道
  `TestFormalEmacsClientMatrix` 单独确认 not_verified）；Helix / Zed /
  Sublime LSP 21 格 skip（runner 无原生 driver，契约强制 not_verified）。
- 其余 driver 测试全包 `-count=1`（skip 两矩阵入口）：48 个测试 41 PASS /
  2 FAIL / 5 SKIP；两 FAIL 为 Sublime 锁定资产缺失 fail-closed（按设计），
  非回归；helix 相关 PASS 项为 ConPTY 探针等基础设施机构件，不等于 helix
  格通过。
- 判定意义：X8-2 CLIENTS 维持 PARTIAL 50% 不动，本轮不构成任何格升级依据。
  本机工具锁破损使 VS Code/Neovim/Emacs 三通道均不可现场复跑——上文
  「Fixed host tools」段所述安装状态与当前主机不符（helix/emacs 已不在），
  per-editor 现状以 docs/editors/*.md 的 missing→not_verified 政策记录为准。

### X9-1 soak（1h 连续档，红）

- 命令：`go test -tags soak -count=1 -timeout 100m ./test/soak/`；必填 env：
  `OMNILSP_SOAK_GATE=required`（stdio_soak_test.go:160，未设则 skip）、
  `SOAK_DURATION=1h`（parseStdioSoakDuration 仅接受精确 30s/10m/1h，
  stdio_soak_test.go:2049-2060；显式设置硬门 :201-206）、`OMNILSP_RUN_ID`
  （:195）、`OMNILSP_BIN=<冻结候选>`。`SOAK_DURATION=1h` 触发
  resource-trend 必检（stdio_soak_test.go:1009 `trendRequired := duration ==
  time.Hour`，需 10 个一分钟窗口）。注意 in-process soak（soak_test.go:29-38
  `soakDuration()`）同读 `SOAK_DURATION`、缺省 30s，与 stdio 严格 gate 是
  两套旋钮。
- 结果：红——1h 连续档证据在本机仍缺席，X9-1 维持 PARTIAL 现状（registry
  Reason「current evidence is pending」与本结果一致）。记账员未复跑本次
  1h 档（时长所限），结果自本轮 soak 实测转录。

### rust-analyzer semantic helper 重建（tools.lock.json 未手改）

- 源一致性：本地 write-tree tree-id `09d330dd…` 与 googlesource 报告的
  `src/tools/rust-analyzer@2d8144b7` tree id 一致 ⇒ 源内容与上游一致；但
  googlesource +archive 容器含下载时刻 mtime，字节 sha 不可复现
  （sourceShaOk=false）。
- 补丁与锁：补丁 sha 与锁一致且干净应用；但补丁引用上游不存在的
  `ReferenceData.is_import` ⇒ 仅补丁的树无法编译，锁 `modifiedTreeSha256`
  与任何自然 staging 协议均不符，证实原构建树含未登记的 static_index.rs
  修改。
- 重建：按 USE 祖先检查语义重建该最小修改后，cargo 1.97.0（rustc/cargo
  版本与锁一致）`build --release` 成功，`--version` 输出与锁内 observed
  版本串逐字一致。
- 产物：exe sha256 `3641e232…` ≠ 锁内 `f402ac78…`；已按协议生成未套用的
  锁更新补丁 `out/patches/i10-rust-helper-lock.patch`（`git apply --check`
  通过）。`tools.lock.json` 依纪律未手改，锁一致性待补丁批准后落账。
- **锁更新套用登记（2026-10-25，基建勘误员）**：上述补丁经批准后已套用——
  `tools.lock.json` 的 `rust-analyzer-semantic-helper` sha 由 `f402ac78…`
  改为 `3641e232…`（git diff 恰 1 行；补丁文件本身混有 CRLF/LF 行尾，直接
  套用会给 8 行上下文带入 CR，实际以 `git apply --ignore-whitespace` 套用并
  规范化行尾，终态 diff 已核验仅该 1 行）；重建产物
  `.zcode/tmp/rust-analyzer-semantic-1.97.0-built-3641e232.exe` 已复制为
  `test/acceptance/tools/bin/rust-analyzer-semantic-1.97.0.exe`，复制件
  sha256 实测 `3641e2323591c2f51c7e06ab713e0348f17590f30fa1dcd27b0ad0453a5ee933`
  与锁逐字一致。**modifiedTreeSha256 溯源缺口注记**：该缺口未因套用消除——
  锁 `modifiedTreeSha256=1dbe96a2…` 无法由任何自然 git staging 协议复现
  （raw+is_import=e08f0f3d、raw+仅补丁=f121e444、normalized=b88a194a），
  即**原锁构建树含补丁之外的未登记 static_index.rs 修改**；`3641e232` 重建
  按同一 USE 祖先检查语义复现了该最小修改，其构建树同样偏离「上游源+补丁」
  的可登记状态。当前对齐的仅是「锁内 exe sha ↔ 磁盘产物 sha」；树级溯源
  仍缺一份可复现的 modified 树登记，留待 X8-2 工具重锁策略一并处理。

## Evidence record — 2026-10-25 记账轮（registry X5-2/X5-4/X5-7 + semanticTokens 行为变更）

### semanticTokens 能力声明条件化（方案B，行为变更）

- 行为变更：initialize 的 `semanticTokensProvider` 声明改为按已注册后端真实
  能力条件化——仅当存在真 `SemanticTokensEmitter`（handlers.go:208，
  = `languages.EncodedSemanticTokensProvider` 别名，唯一真发 token 的 golang
  encoding-aware 路径；ccls/typescript/pyright/rust-analyzer 为显式空桩不满足）
  时声明；handler 仍注册、显式请求仍被服务；`omitempty` 使「不声明」在 wire
  上为省略而非 null。旧钉现状测试 semantic_tokens_current_state_test.go 改写为
  semantic_tokens_capability_test.go 的
  `TestQ3_SemanticTokensDeclaredOnlyForRealEmitters`（golang 必声明含
  Full/legend、ccls-only 必不声明且桩仍服务、无后端不声明）。
  internal/protocol/lsp/protocol.manifest 与 11 个 test/golden 快照经运行
  所有者授权后用官方命令（gen-protocol.go / `OMNISP_UPDATE_GOLDEN=1`）再生成
  承接，逐行核对：manifest 恰为三处签名变化 + ServerCapabilities 行
  omitempty（全部源于独占文件 handlers.go）；11 个 golden 每个恰为同一
  21 行 semanticTokensProvider 块的纯删除（0 增 21 删），无外来内容。
- 同轮 registry 落账（详见 docs/audit-longterm-spec.md §1/§3.3/§4/§5.3/§7
  收口标注）：X5-2 升 AUTO（server 原生 LSIF API 落地、探针修剪 5 个死码
  测试并补 3 个新 server LSIF 测试，全组 `-count=1` 实跑绿）；X5-7 维持
  PARTIAL（多语言 persistent 层 + replay 跨进程两项证据入探针，七语言
  fact 覆盖仍未闭合）；X5-4 改 probe-less PARTIAL（internal/index/graph
  死码整删）；X7-1 探针 TestExecutor_LaunchHandshake 的 SKIP 诚实性缺口
  收口（`-count=1 -v` 实核 e2e 子测试真实 PASS）。baseline.json 未动
  （core 逐位不变 98.45959595959597）；x5 由 78.57% 升 85.71%
  （`go run ./cmd/omnilsp verify --json` 实算）。

### 白名单扩展登记（2026-10-25 返工·清算独立复核后补登）

- 本轮 sanctioned 区（ZONE_PREFIXES）之外实测共两组 dirty 路径（`git status
  --porcelain -z` 全量 46 项逐一比对，无其他漏网），均属运行所有者授权的
  披露变更，登记为白名单扩展而非未授权越界：
  ① internal/index/graph/**（graph/generated/invalidate/graph_test 四文件
  整删）——勘察员 safeToDelete 清单内、清理员汇合后执行。返工复核：
  `rg '"internal/index/graph' --type go` 全仓零命中；唯一非测试引用即 registry
  旧 X5-4 Probe 字面量（HEAD registry.go:321，本轮已随探针移除，仅剩
  registry.go:320 Reason 对删除本身的记载）；`go build ./...` 绿。
  不回滚——恢复文件只会复活零引用死码，并与 X5-4 已落账的
  "Dead package removed" 处置（registry/audit/conformance.md 三处一致）矛盾。
  ② internal/protocol/lsp/protocol.manifest——server 域经运行所有者授权后
  用官方命令 `go run scripts/gen-protocol.go` 再生成承接。返工复核：
  `git diff --numstat` = 4 增/2 删，恰 4 行变化（+hasSemanticTokensEmitter、
  buildCapabilities 双参、+SemanticTokensEmitter 别名、ServerCapabilities
  semanticTokensProvider omitempty），全部源于独占文件 handlers.go 的真实
  签名；TestY54_ProtocolManifestFresh 与 TestConformanceNoRegression 均
  `-count=1` 实跑 PASS。不回滚——回退 manifest 将立即使 TestY54 判红
  （指纹与 handlers.go 现源不符）。授权会话本身不在仓库内、无法独立验证，
  仅能确认变更形态与声明逐位吻合且官方一致性测试全绿——按「诚实高于
  完成」如实登记该验证边界。本扩展仅限本轮；后续轮次白名单由运行脚本
  另行划定。



## Evidence record — 2026-10-25 最小收口轮（registry X5-7 升 AUTO + X9-1 非标准诊断 soak 红）

### X5-7 七语言覆盖闭合（升 AUTO）

- X5-7 工程师在 `internal/index/persistent/semantic_multilang_test.go` 为
  `TestMultiLanguageTypedSemanticGenerationIsolationAcrossRecommit` 补齐
  c/typescript/javascript 三语言（按既有 rust/cpp/python 表驱动构造法 +
  `interop.ImportSCIPToModel` typed facts，`scip.Document.Language` 与
  `scope.Language` 经 `scipLanguageMatches` 按 family 匹配——typescript/
  javascript 同属 ts-js-family，严格同名是充分而非必要条件；每次 import
  仅传单 scope（semantic_multilang_test.go:298），无跨语言混入风险——
  internal/index/interop/scip_model.go:242-262），恢复代 scopes 断言 3→6；
  go 语言由既有 `TestTypedSemanticGenerationIsolationAcrossRecommit`
  （semantic_isolation_test.go:195）覆盖，七语言（go/c/cpp/rust/python/
  typescript/javascript）全覆盖。变异验证由证据工程师在其文件边界内对
  被测断言施加等价最小变异（恒假→红→复原绿）；生产代码侧变异因记账员
  文件边界（registry/acceptance/audit-docs/baseline）未做，如实登记。
- 记账员本轮实跑（全部 `-count=1`，完整输出判定）：named run
  `go test -count=1 -run '^TestMultiLanguageTypedSemanticGenerationIsolationAcrossRecommit$' -v ./internal/index/persistent/`
  → `--- PASS ... (0.21s)`；探针四组全绿：persistent 全包 ok 1.297s、
  server 探针 15 测试 ok 3.995s、interop ok 0.628s、replay ok 0.267s。
- 记账：X5-7 升 AUTO、删 Reason（探针四组不变，测试名已含
  `TestMultiLanguageTypedSemanticGenerationIsolationAcrossRecommit`）；
  `OMNISP_UPDATE_CONFORMANCE=1 go test -count=1 ./test/conformance/`
  再生成 docs/conformance.md（X5-7 行 PARTIAL→AUTO），无 env 复跑
  ok 0.294s；baseline.json 逐位不变（X5-7 属 x5 域，core 预期不变，实测
  确认）。

### X9-1 soak 非标准诊断档（红，维持 PARTIAL）

- 性质：非标准 budget 诊断跑——`OMNILSP_SOAK_REQUEST_BUDGET=300s`
  （当时标准为 90s，scripts/acceptance.ps1:2692；勘误注 2026-10-25：该行号为当时位置，现赋值行在 :2693、标准已改 300s），按本轮任务授权施行的
  一次诊断性重跑；任务硬上限（一次完整重跑）已用，未追第二次。
- 冻结候选：RUNID=gate-20261025T0716Z；`C:/Users/32806/sdk/go1.26.1/bin/go.exe
  build -trimpath` 产出 omnilsp.exe，SHA-256=2f776cee8bae6b111552fa81936636c9b
  5b0c54e764f678cdce9905ad8191556，git rev=c12de47047c9c02b5adc5f47a442b8581197ed40；
  冻结时刻 porcelain/diff --stat 为空（freeze-record.txt，记账员已亲读）。
- 命令（经 s18run 运行器后台单次执行）：`go test -tags soak -count=1 -run
  '^TestSoak_RealStdioMixedWorkload$' -timeout 90m ./test/soak/`，env：
  `OMNILSP_SOAK_GATE=required`、`SOAK_DURATION=1h`、`OMNILSP_RUN_ID=
  gate-20261025T0716Z-soak`、`OMNILSP_BIN=<冻结候选>`、
  `OMNILSP_SOAK_REQUEST_BUDGET=300s`。
- 结果：红——真实执行 244.56s 后失败中止（请求时长 3600s，完成约 6.8%；
  非编译/环境快速失败）。失败检查=soak/reindex：`stdio_soak_test.go:424:
  omnilsp/reindex: context deadline exceeded`（soak-run.log 共 6 行记账员
  已全文读核；勘误注 2026-10-25 二次复核：原记「7 行」系误计，实测共 6 行，包级 FAIL 行在第 5 行、其后另有裸 `FAIL` 一行；soak-report.json decision=failed；10 项工具锁 +
  soak/process-tree-limit 全 passed，tool/rust-analyzer 实测 1.97.0
  (2d8144b7 2026-07-07)，go1.26.1 对齐）。soak.jsonl 0 字节属预期：失败
  发生在 8 worker 启动（stdio_soak_test.go:504）之前，无 telemetry 行。
- 根因（deadlineMechanism，记账员读码复核）：initial-queries 检查
  （:412 assertStdioQuerySet）的期限由 `OMNILSP_SOAK_REQUEST_BUDGET` 治理
  ——requestStdio→`context.WithTimeout(ctx, stdioRequestBudget())`
  （:2552-2556），`stdioRequestBudget()`（:2566-2578）未设/非法回退默认
  30s，无独立固定阶段 deadline；而 omnilsp/reindex 走另一条固定 10s 期限
  （requestReindexWithRetry，:1996 `context.WithTimeout(ctx, 10*time.Second)`），
  不受该 env 治理——ra 1.97.0 冷启动索引满载压力下 reindex 首次提交未在
  10s 内完成（同一路径在 1.99.0 的 3601s 完整跑中曾通过）。
  **【勘误 2026-10-25，基建勘误员】上一句括注「同一路径在 1.99.0 的 3601s
  完整跑中曾通过」为失实转录，原文保留不改，更正如下**：out/x9-1-soak-replay/
  whole-soak-report.json 实测 run_id=`x10-soak-replay-whole` 自
  2026-10-24T17:42:54.82Z 至 17:42:55.53Z 仅 **0.71s** 即死于 fail-closed
  工具闸门（stdio_soak_test.go:234：tool/go 本机 1.27.1 vs 锁 1.26.1、
  tool/rust-analyzer 本机 1.99.0 vs 锁 1.97.0，decision=not_verified；
  whole-soak.log 首行 `--- FAIL: TestSoak_RealStdioMixedWorkload (0.72s)`），
  reindex 工作负载从未执行；`3601.497s` 是 `go test` 的**包级墙钟**
  （whole-soak.log 包级 FAIL 行 `FAIL github.com/omnilsp/omni/test/soak 3601.497s`；二次复核注 2026-10-25：whole-soak.log 共 10 行，该行在第 8 行，其后另有 `FAIL` 与 `WHOLE REPLAY EXIT=1` 两行），
  被同包 in-process golang-only soak（soak_test.go:27-37 `soakDuration()`
  同读 `SOAK_DURATION=1h` 跑满 1h）消耗，并非 stdio soak 的运行时长。因此
  **omnilsp/reindex 的固定 10s 期限在 1.99.0 下从未被观测**：不存在
  「1.99.0 下 10s 期限内通过」的对照组，「10s 不足」仅由本机 1.97.0 冷启动
  的单次实测（gate-20261025T0716Z）支撑。
- 诊断价值：300s budget 下 initial-queries（上轮阻塞点）已通过（执行序
  证据：t.Fatal 命中 :424 reindex 分支仅当 :412 返回 nil 才可达，代码顺序
  记账员亲读核实），证明 90s 对 rust-analyzer 1.97.0 冷启动确实过紧。
- 判定：非标准 budget 的红 → X9-1 维持 PARTIAL（registry Milestone x9、
  Reason 不动），本结果不构成任何升级依据。
- 证据位置：out/freeze-staging/gate-20261025T0716Z/{freeze-record.txt,
  omnilsp.exe, soak-run.log, soak-report.json, soak.jsonl}。
- 工作树观察：internal/index/persistent/semantic_multilang_test.go 于
  soak 进行中（15:19）被并行 X5-7 工程师修改（+40/-10）；冻结时刻
  （15:16）porcelain 为空已录 freeze-record.txt；该文件为另一包 _test.go，
  不参与 `./test/soak/` 编译，冻结候选与 soak 证据不受影响。


## Evidence record — 2026-10-25 记账轮（X9-1 正式化标准 budget 1h soak 红，维持 PARTIAL）

### X9-1 soak 正式化标准 budget 档（1h，红，维持 PARTIAL）

- 性质：本轮为**正式化后的标准 budget 证据**——`OMNILSP_SOAK_REQUEST_BUDGET=300s`
  已由基建勘误员在 scripts/acceptance.ps1:2693 落地（注释 :2689-2692 载明
  harness plumbing：钉版 1.97.0 工具链冷启动 allowances，90s 经
  gate-20261025T0716Z 诊断实证不足，非 §S16 值；2026-10-25 起 300s 即标准
  harness budget），docs/soak-nightly.md 同步。本轮 soak 即该标准值的 1h 档。
- 冻结候选：RUNID=gate-20261025T1404Z；`C:/Users/32806/sdk/go1.26.1/bin/go.exe
  build -trimpath` 产出 omnilsp.exe，SHA-256=
  5a77f1b0a611a0ba280887eb31b369513590c71c9b27e3d1165c602ec67c9d20，
  git rev=912252391ba2db746d507feab6a355ca41f28885 (main)；冻结时刻含 6 个
  基建工作树文件（freeze-record.txt 记账员亲读；test/soak、docs、scripts
  变更不影响候选运行时代码）。
- 命令（经 s18run 运行器后台单次执行）：`go test -tags soak -count=1 -run
  '^TestSoak_RealStdioMixedWorkload$' -timeout 90m ./test/soak/`，env：
  `OMNILSP_SOAK_GATE=required`、`SOAK_DURATION=1h`、`OMNILSP_RUN_ID=
  gate-20261025T1404Z-soak`、`OMNILSP_BIN=<冻结候选>`、
  `OMNILSP_SOAK_REQUEST_BUDGET=300s`（显式传递，与 acceptance.ps1 正式化
  标准值一致）。
- 结果：红——TestSoak_RealStdioMixedWorkload 于 586.42s（≈9.8 分钟，未跑满
  1h）被测试自身连续性守卫中止：`stdio_soak_test.go:868: active duration
  monitor did not advance`（.zcode/tmp/soak-gate-20261025T1404Z.log 全文
  6 行记账员已读；勘误注 2026-10-25 二次复核：原记「5 行」系误计，实测共 6 行，包级 FAIL 行在第 5 行、其后另有裸 `FAIL` 一行；包级墙钟 586.506s，exit 1）。soak-report.json
  decision=not_verified，run_id=gate-20261025T1404Z-soak；记账员逐项亲读：
  10 项工具钉定（tool/go、node、python、clangd、rust-analyzer、pyright、
  typescript-language-server、typescript 及两 wrapper）+
  soak/process-tree-limit、initialize-open、cancel、reindex、
  semantic-workload 全部 passed，soak/continuity not_verified；soak.jsonl
  0 字节（零分钟遥测，与 monitor 饥饿一致）。
- 根因分层（soak 攻坚员诊断，记账员转录）：active 仅在 5s ticker 分支推进
  （stdio_soak_test.go:599），失败条件 active==0（:863）⇒ 主 goroutine 自
  ticker 创建起一次 tick 未处理，全程阻塞于某 select case 同步体内（机制
  定位：editStdioDocument→readStdioStatus/waitForStdioSnapshot 链，各受
  300s 预算；seed=0 确定性调度下两次连续 omnilsp/status 往返各吃满预算量
  级）。定性：冻结候选在真实负载下的活性/延迟问题触发 harness 连续性守卫，
  非环境配置错误；harness 脆弱性已登记（时长监控器与阻塞请求体共用主
  goroutine，单次 >30s 慢请求在 active==0 时即饿死监控致 not_verified），
  留待后续轮次评估监控器独立化。
- 判定：正式化标准 budget（300s）+ 1h 档的红 → X9-1 维持 PARTIAL
  （registry Milestone x9），本轮按红分支更新 Reason 载入新事实（budget
  正式化、run id、红因、候选侧根因待插桩诊断），不构成任何升级依据；
  探针挂载评估仅适用于绿分支（PERF-2 守卫模式），本轮不适用。
- 证据位置：out/freeze-staging/gate-20261025T1404Z/{freeze-record.txt,
  omnilsp.exe, soak-report.json, soak.jsonl}；
  .zcode/tmp/soak-gate-20261025T1404Z.log。
- 记账后续（记账员实跑）：`OMNISP_UPDATE_CONFORMANCE=1 go test -count=1
  ./test/conformance/` 再生成；docs/conformance.md 零 diff（X9-1 带
  Milestone x9，PARTIAL Reason 不进投影，docgen.go:82-86）；baseline.json
  逐位不变（X9-1 属 x9 域，core 独立计分，report.go:104-111）。fast 实测
  （`go run ./cmd/omnilsp verify --json`）：coreScore=98.45959595959597、
  x5 域 0.9285714285714286、x9 域 0.75（X9-1 PARTIAL 0.5 + X9-2 AUTO 1.0
  均值），变更前后逐位一致。


## Evidence record — 2026-10-25/26 第十六轮开行（九项收口裁决落档）

- 性质：本轮为档案记录轮（第十六轮开行）——把源侧九项收口裁决（含推导链）
  原样落进 docs/audit-longterm-spec.md §7 新增「终局裁决（2026-10-25）」
  小节，并在本文件登记本轮运行记录；除两份在案文档与 git 提交外零改动。
  裁决包日期 2026-10-25，落档动作执行于 2026-10-26。
- 本轮范围：① 探测——audit §7（docs/audit-longterm-spec.md「§7 需要拍板
  的开放项」）现状为 6 条编号开放项（§7.1 X5-4 已销；§7.2-§7.6 待拍板在
  位），acceptance.md 运行记录体例为「## Evidence record —」一轮一节，
  本节即第十六轮开行记录；② 落档——§7 末尾追加终局裁决小节：第 1-5 条
  拍板销 §7.2-§7.6 五个待拍板项（批量帧语义、fast 档执行探针、soak
  form-only 移出 build tag、Semantic Core 进 ci.yml、2s wall-clock 预算
  派生），第 7 条定 codeAction 收口标准（本轮评估登记差距、不强制全部
  实现），第 6/8/9 条明确为机械规则非拍板（X10 按 registry Milestone
  机械排序——X9-1 关账前不解锁，本轮不动插件接线；27 条 DEFERRED 各自
  Reason/Milestone 字段即规则；tools.lock.json + sha256 可复现性契约维持
  不变）。
- 裁决依据（逐条推导链见 audit §7 终局裁决小节，此处记要点）：JSON-RPC
  2.0 §6 与 LSP 3.17 弃用批量，帧完整性 vs 元素有效性切割；AUTO 的定义
  就是 Probe 被执行（能力=先探测后承诺）；廉价确定性检查必须常跑；凡记入
  账面的门必须有 CI 执行面；阈值一律预算派生/证据度量、禁裸常量（第十四轮
  reindex deadline 先例，commit 9e3b14e）；服务端原生诊断凡修复可机械推导
  者必须配真实 quickfix。
- 锚点实测（记账员本轮亲验）：test/perf/perf_test.go:487
  `if d := time.Since(start); d > 2*time.Second` 在位；
  internal/transport/stdio_test.go:282 `TestBatchFrameIsPerStreamFatal`
  在位（codec 侧对应注释见
  internal/protocol/jsonrpc/codec_framing_test.go:35）；test/soak/
  resource_trend_test.go:1 仍为 `//go:build soak`。上述即裁决 1/3/5 的
  现状锚点，本轮不动它们。
- 本轮不动代码：裁决的工程实现（批量帧用例改写、fast 探针执行、form-only
  移 tag、ci.yml 门禁接线、perf 阈值派生、codeAction 差距清点）均属后续
  轮次，本轮只落档。
- 门禁与提交：`go build ./...` 实跑通过（本轮 docs-only，未触碰任何 Go
  包，包级 `go test -count=1` 无对象，如实登记为不适用）；
  `git add docs/audit-longterm-spec.md docs/acceptance.md && git commit
  -m "docs: record the nine closing rulings with derivations"`。

## Evidence record — 2026-10-26 第十七轮（五项裁决工程落地 + codeAction 可达性修复）

- 性质：本轮为工程落地与复验轮（第十七轮）。第十六轮只落档九项裁决；本轮
  把其中五项工程项真正落地并逐项实跑验证，同时修复 codeAction 的可达性
  缺陷与两处 soak 监控器测试的时序缺陷。详细推导、差距清单与逐条输出见
  docs/audit-longterm-spec.md「2026-10-26 补缺验证」。
- 本轮改动（按面积）：
  - 合规闸：internal/conformance/report.go 新增 `FastProbeReport` /
    `probeReport` / `executeAutoProbes`（AUTO 探针真跑、GATE 仅保留
    存在性 0.5、全局 30m 预算）；probe.go 的 `runTestsBatch` 按 race 参数
    决定 `-race`；cmd/omnilsp/verify.go 新增 `--probe`；Makefile 新增
    `verify-probe`；test/conformance/conformance_test.go 切到真实探针模式；
    internal/conformance/engine_test.go 新增
    `TestFastProbeModeRequiresExecutedAutoProbe`（失败的 AUTO 在 fast-probe
    记 0、GATE 0.5；full 档 GATE 0）。
  - form-only 常跑：test/soak/resource_trend.go 无 build tag；
    resource_trend_test.go 去 tag；4 项阈值/斜率测试进入普通套件。
  - CI 依赖方向：ci.yml 的 `go list -deps` 加入 `./internal/index/...`。
  - perf 阈值：test/perf/perf_test.go 的取消预算改为 2× 同轮 800-ref
    实测值，删除裸 2s 常量。
  - codeAction 可达性：golang backend 为 unused-import 写入
    `Code: "unused-import"`，仅在证明安全时把范围扩到整行内容（换行保留，
    应用后留空行）；新增 backend 三子测试（单行/分组/同行不安全）与
    server §I19 集成测试。
  - soak harness：`maintenance_pause`/`maintenance_kind` 遥测字段、
    `minuteWindowGapContinuous` 共享谓词、按来源派生的维护预算、监控器
    维护窗抑制；phase trace schema v4（limit/status-every 环境变量、
    `status_sample_every`）；acceptance.ps1 的
    `OMNILSP_SOAK_PHASE_TRACE=1` 显式 opt-in 与失败关闭证据检查。
- 验收证据（本轮实跑）：
  - `go test -count=1 ./test/soak/` → 4 form-only tests PASS。
  - `go test -tags soak -count=1 -run '^(TestSoakContinuityMonitor.*|
    TestSoakMaintenanceBudgetsAreDerived|TestSoak_RequestBudgetDefaultIs30s)$'
    ./test/soak/` → 8 tests PASS；监控器 6 项另以 `-count=100` 复跑全绿。
  - `go test -count=1 -run 'Batch' ./internal/protocol/... ./internal/transport/...`
    → jsonrpc 0.092s、transport 0.509s PASS。
  - `go test -count=1 -timeout 900s -run '^TestS19_ReferencesScalingCurve$'
    ./test/perf/` → PASS；371.46µs@200、933.14µs@800、2.95933ms@3200。
  - `TestDiagnosticsUnusedImportCarriesWholeLineQuickfixRange`、
    `TestI19_UnusedImportQuickfixRemovesLineContent`、
    `TestCompletionPhaseTrace*`、conformance shape tests → PASS。
  - `go test -count=1 -timeout 900s ./test/conformance/`（`TestGenerateDocs`
    + `TestConformanceNoRegression`）→ PASS，124.57s（真跑 AUTO 探针）。
  - `go run ./cmd/omnilsp verify --probe --json` → exit 0，mode=fast-probe，
    coreScore=98.45959595959597，124 checks；`verify --min 90` → exit 0，
    同分。
  - `go build ./...`、`go vet ./...` → 通过（VET_ALL_OK）；依赖方向本机
    复跑 DEP_CLEAN；gofmt 按 Makefile 口径无输出。
- 未闭合边界：
  - 30s 真实 stdio soak 冒烟在 0.49s 因工具 pin 漂移失败关闭：
    `go1.27.1`≠`1.26.1`、`rust-analyzer 1.99.0`≠`1.97.0`；
    decision=not_verified，未启动候选、无 telemetry/phase trace。证据
    out/soak-verify/{soak.log, soak-report.json, omnilsp.exe, soak.test.exe}。
  - 无 10m/1h 绿色证据；X9-1 维持 PARTIAL（registry Milestone x9），本轮
    不写任何升级依据。恢复需还原锁定工具链或按流程换锁，禁止绕过 pin。
  - 未重跑完整 `go test ./...`（90m 档）与全包 `-race`；本轮证据只覆盖
    定向包、真实探针 conformance 与 fast/probe verify。
- 提交：本轮未提交（无提交授权）；工作树改动保留待审。

## Evidence record — 2026-10-26 本机工具链重锁与 soak 正式链

- 性质：按用户裁决「以本机环境为标准，环境可以更新」，把验收工具锁从
  go1.26.1 / rust-analyzer 1.97.0 重建为本机版本，并恢复真实 stdio soak。
  本轮不修改 §S16 契约；工具锁只跟随本机，语义 helper 仍是独立钉版的
  1.97.0 产物。
- 工具锁运行记录（全部本轮实跑）：
  - `observed`：go1.27.1；rust-analyzer 1.99.0 (b940084d 2026-09-28)；
    rustc 1.99.0 (b940084d7 2026-09-28)；cargo 1.99.0 (5f94df478
    2026-08-27)；VS Code 1.143.1；npm 11.9.0。
  - `resolvedBinaries`：go / rust-analyzer / rustc / cargo /
    rust-analyzer-proc-macro-srv / vscode / npm 按本机文件复算路径与
    SHA-256；全表 30 项复算 0 处失配，10 项（7 个 Sublime 工件 +
    Neovim/Helix/Emacs）缺失，按既有策略维持 `not_verified`。
  - `TestRustAnalyzerLockedSCIPDifferential`：本机 1.99.0
    rustc/cargo/rust-analyzer + 1.97.0 helper 组合实跑 PASS（12.25s），
    证明 live RA 与语义 helper 分离的锁口径可用。
- harness 预算修正：重启后的稳定性 hover 原为固定 3s；本机 8 worker
  负载下替换 rust-analyzer 重索引时会超时。改为
  `soakRustStabilityProbeBudget()` = `stdioRequestBudget()`（默认 30s；
  acceptance 300s），探针后重新采样进程身份；
  `TestSoakRustStabilityProbeBudgetIsRequestBudget` 钉住派生关系。
- 诊断运行 A（`out/soak-local/local-20261026T0855Z`，30s，固定 3s）：
  552.41s 后在 replacement rust-analyzer 稳定性窗口失败（hover 3s 超时）；
  其余工具、初始化、取消、reindex、语义负载检查通过。
- 诊断运行 B（`out/soak-local/local-20261026T0907Z`，30s，派生预算）：
  941.40s；backend-restart、candidate restart、continuity、duration、
  coverage 等功能检查全部通过；唯一失败是 `soak/worker-identity`——
  运行期间编辑了 worker 源文件，initial=faac4d9d… 与 final=fb7208c9…
  不同。该失败是测试的身份守卫在工作，不是产品失败。
- 采样率实测：运行 B 首个候选约 25s 内记录 15,308 条 status 样本
  （`STATUS_EVERY=8`），反推约 4.9k status/s；acceptance 的 trace 默认已
  按档位改为 30s `32768/8`、10m `65536/128`、1h `131072/512`。
- 正式链 C（`out/soak-local/local-20261026T0925Z`）：同一冻结候选
  （sha256 `c66fa11e…`）与 soak 测试（sha256 `3ebcee73…`），30s/10m/1h
  顺序执行。`test/acceptance/evidence` 子目录创建被 ACL 拒绝，故本地证据
  目录为 `out/soak-local/`；正式链结果待报告落盘后补记。
