# Soak Nightly Form (§S16 / X9-1)

The accelerated soak (`go test -tags soak -timeout 300s ./test/soak/`, 30s
default) is the commit gate. The nightly/release form reuses the same test
with a longer duration and matching harness timeout:

```bash
# Nightly (1h): duration must stay well under the go-test timeout.
SOAK_DURATION=55m go test -tags soak -timeout 70m ./test/soak/

# Release soak (24h+ per T7): CI job with artifact retention.
SOAK_DURATION=24h go test -tags soak -timeout 26h ./test/soak/
```

Pass criteria are enforced by the test itself and do not change with
duration:

- goroutine count within ±50 of the post-warmup baseline;
- heap growth below 256 MiB over the run;
- no panic, no scheduler starvation, mixed edit/query workload continuous.

Duration is a knob (`soakDuration()` reads `SOAK_DURATION`); bounds are
invariants. A release soak that fails either bound blocks the release —
see `docs/conformance.md` Y1-10.
