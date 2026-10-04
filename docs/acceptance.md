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
