# Nightly and release soak

The real stdio gate drives the frozen OmniLSP binary through the shared LSP
transport and exercises eight concurrent query workers, document edits,
cancellation, reindexing, close/reopen, and process restart. It requires
Windows amd64 so the test can place the Go test runner, candidate, and backend
children in one Windows Job Object. The Job enforces an 8 GiB commit ceiling;
the gate also sums `PROCESS_MEMORY_COUNTERS_EX.PrivateUsage` and handles over
all enumerated Job members. Missing tools or incomplete process accounting
produce `not_verified` and a nonzero test result.

The workload uses Go's `math/rand` with seed `0` to deterministically shuffle
the eight query workers and select edit intervals of 48–80 completed queries
and canceled-reindex intervals of 192–320 queries. It starts a controlled
`rust-analyzer` child restart at one third of the run while the Go query workers
remain active, rebuilds the persistent index each minute, and restarts the
candidate halfway through. The Rust probe must return a symbol-specific hover
before and after the supervised child replacement. Each explicit reindex must
publish a newer generation that is readable and fresh. The final check requires
the current generation to equal the last successfully committed generation and
the committed content to remain fresh; it must not manufacture a generation
for an interval with no scheduled reindex. Candidate restart checks verify
recovered query semantics directly. The long references query must
return the fixture declaration and at least 4,096 use sites; its unique progress
token must receive ordered `begin` and `end` events before the terminal query
response. Report events are optional. Each minute records first/last/minimum/
maximum/median PrivateUsage and aggregate process-tree handles. Each rolling
set of ten one-minute medians is checked with a Theil-Sen slope, so isolated
local dips are allowed while sustained growth still fails when estimated
private-memory growth reaches 512 MiB or handle growth reaches 1,024.

For the local Windows release candidate, run the same mixed workload through
30-second and 10-minute preflights, then one uninterrupted one-hour worker.
The preflights do not count toward the final hour. Freeze the candidate once;
all evidence shares the same run ID and directory:

```powershell
$runId = '<runId>'
$evidence = "test/acceptance/evidence/$runId"
$env:OMNILSP_RUN_ID = $runId
$env:OMNILSP_BIN = (Resolve-Path "$evidence/omnilsp.exe").Path
$env:SOAK_JSONL = "$evidence/soak.jsonl"
$env:OMNILSP_ACCEPTANCE_REPORT = "$evidence/soak-report.json"
$env:SOAK_DURATION = '1h'
go test -tags soak -run '^TestSoak_RealStdioMixedWorkload$' -count=1 -timeout 90m ./test/soak/
```

The release path uses `Start1h` and `Status` in `scripts/acceptance.ps1`, which
run the worker in a hidden window and bind it to the passed Fast fingerprint.
The selected local gate does not require a 24-hour run and makes no claim of
24-hour coverage. Durations below the five-second sampling interval are not
valid soak runs.

The monitor samples every five seconds and writes one flushed JSONL row per
minute with a monotonic sequence, UTC time, active elapsed time, bounded
per-method latency samples and P50/P95/P99, worker liveness, Go heap/goroutines,
server status, index state, backend registration, process-tree private bytes,
commit high-water mark, and process handles. Missing minute windows, clock
discontinuities, or sleep/suspension gaps invalidate the run; elapsed wall time
is never used to fill missed intervals. A final row records candidate and
worker source hashes, verifies their identity did not change during the run,
and verifies that candidate/backend processes and handles are gone after
graceful LSP shutdown. `KILL_ON_JOB_CLOSE` cleans up any
remaining descendants when the Go test process exits unexpectedly.

The real stdio gate also retains the post-GC growth bounds from the accelerated
soak: at most 50 additional Go goroutines and less than 256 MiB additional
heap. `TestSoak_SustainedMixedLoadBounded` remains available as a short
in-process test; it is not a substitute for the real stdio process-tree gate.
